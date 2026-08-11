import { useCallback, useEffect, useRef } from "react";

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
 * Cleanup is automatic: any pending callbacks are cancelled when the
 * component unmounts.
 */
export function usePrefetch(): PrefetchResult {
    // Collect cleanup functions that should run on unmount.
    const cleanupsRef = useRef<Array<() => void>>([]);

    useEffect(() => {
        return () => {
            const fns = cleanupsRef.current;
            for (let i = 0; i < fns.length; i++) {
                fns[i]();
            }
            cleanupsRef.current = [];
        };
    }, []);

    const prefetchOnIdle = useCallback(
        <T>(key: string, fetcher: () => Promise<T>, idleMs = 1000): void => {
            let cancelled = false;

            const doPrefetch = async () => {
                if (cancelled) return;
                try {
                    const v = await fetcher();
                    if (cancelled) return;
                    sessionStorage.setItem(key, JSON.stringify({ t: Date.now(), v }));
                } catch {
                    // silently ignore prefetch errors
                }
            };

            const useRIC =
                typeof window !== "undefined" && "requestIdleCallback" in window;

            if (useRIC) {
                const id = requestIdleCallback(
                    () => {
                        doPrefetch();
                    },
                    { timeout: idleMs },
                );
                cleanupsRef.current.push(() => {
                    cancelled = true;
                    cancelIdleCallback(id);
                });
            } else {
                const id = setTimeout(() => {
                    doPrefetch();
                }, idleMs);
                cleanupsRef.current.push(() => {
                    cancelled = true;
                    clearTimeout(id);
                });
            }
        },
        [],
    );

    return { prefetchOnIdle };
}
