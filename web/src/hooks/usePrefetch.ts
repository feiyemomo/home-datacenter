import { useCallback, useEffect, useRef } from "react";
import { reportClientError } from "@/lib/errorReport";

export interface PrefetchResult {
    prefetchOnIdle: <T>(key: string, fetcher: () => Promise<T>, idleMs?: number) => void;
}

/**
 * usePrefetch — proactively fetch and cache data for later use.
 *
 * Data is stored in sessionStorage with the same `{ t, v }` format
 * as useCachedFetch, so anything cached by usePrefetch is immediately
 * available to useCachedFetch on the same key.
 *
 * Strategy:
 *   prefetchOnIdle — defer until the browser is idle
 *
 * v1.8.42 hardening:
 *   - Freshness guard: a prefetch will NOT overwrite an existing cache
 *     entry that is newer than PREFETCH_TTL_MS. This is the key fix for
 *     "screen going backwards" — with both usePrefetch and useCachedFetch
 *     writing the same sessionStorage key, an async prefetch could land a
 *     stale snapshot on top of live (WS-driven) data. Prefetch now only
 *     seeds missing or stale entries, never clobbers fresh ones.
 *   - Failure reporting: a failed prefetch no longer vanishes silently;
 *     it's reported to the backend (context "prefetch") so the operator
 *     can see when warm-up is consistently failing (e.g. bad network).
 *
 * Cleanup is automatic: any pending callbacks are cancelled when the
 * component unmounts.
 */
export function usePrefetch(): PrefetchResult {
    // Collect cleanup functions that should run on unmount.
    const cleanupsRef = useRef<Set<() => void>>(new Set());

    useEffect(() => {
        return () => {
            cleanupsRef.current.forEach((fn) => fn());
            cleanupsRef.current.clear();
        };
    }, []);

    // A cache entry younger than this counts as "fresh" and is never
    // overwritten by a prefetch. 30s is well within a page-hopping
    // session but short enough that a stale entry still gets refreshed.
    const PREFETCH_TTL_MS = 30_000;

    const prefetchOnIdle = useCallback(
        <T>(key: string, fetcher: () => Promise<T>, idleMs = 1000): void => {
            let cancelled = false;

            const doPrefetch = async () => {
                if (cancelled) return;

                // Freshness gate BEFORE issuing the request: if a cache
                // entry newer than TTL already exists, skip the network
                // hit entirely. Dashboard's 10s status poll re-arms
                // prefetchOnIdle every cycle; without this gate each
                // re-arm fired a real request that the post-fetch guard
                // then discarded (3 wasted round-trips per 30s).
                const readCache = () => {
                    try {
                        const raw = sessionStorage.getItem(key);
                        if (!raw) return null;
                        return JSON.parse(raw) as { t: number };
                    } catch {
                        // unreadable cache — treat as absent, safe to fetch.
                        return null;
                    }
                };
                const cache = readCache();
                if (cache && Date.now() - cache.t < PREFETCH_TTL_MS) {
                    return;
                }

                try {
                    const v = await fetcher();
                    if (cancelled) return;
                    // Second guard after the round-trip: live (WS-driven)
                    // data may have refreshed the cache while the request
                    // was in flight — never clobber it.
                    const fresh = readCache();
                    if (fresh && Date.now() - fresh.t < PREFETCH_TTL_MS) {
                        return;
                    }
                    sessionStorage.setItem(key, JSON.stringify({ t: Date.now(), v }));
                } catch (e) {
                    // v1.8.42: a silent prefetch failure is invisible to
                    // the operator. Report it so repeated warm-up failures
                    // surface in the log pane.
                    try {
                        reportClientError({
                            level: "info",
                            context: "prefetch",
                            message: `prefetch '${key}' failed: ${e instanceof Error ? e.message : String(e)}`,
                        });
                    } catch {
                        // reporting must never throw here
                    }
                }
            };

            const useRIC =
                typeof window !== "undefined" && "requestIdleCallback" in window;

            if (useRIC) {
                let cleanup: () => void;
                const id = requestIdleCallback(
                    () => {
                        cleanupsRef.current.delete(cleanup);
                        doPrefetch();
                    },
                    { timeout: idleMs },
                );
                cleanup = () => {
                    cancelled = true;
                    cancelIdleCallback(id);
                };
                cleanupsRef.current.add(cleanup);
            } else {
                let cleanup: () => void;
                const id = setTimeout(() => {
                    cleanupsRef.current.delete(cleanup);
                    doPrefetch();
                }, idleMs);
                cleanup = () => {
                    cancelled = true;
                    clearTimeout(id);
                };
                cleanupsRef.current.add(cleanup);
            }
        },
        [],
    );

    return { prefetchOnIdle };
}
