import { useCallback, useEffect, useRef } from "react";

export interface PrefetchResult {
    prefetch: <T>(key: string, fetcher: () => Promise<T>) => Promise<void>;
    prefetchOnIdle: <T>(key: string, fetcher: () => Promise<T>, idleMs?: number) => void;
    prefetchOnVisible: <T>(
        ref: React.RefObject<HTMLElement | null>,
        key: string,
        fetcher: () => Promise<T>,
    ) => void;
}

/**
 * usePrefetch — proactively fetch and cache data for later use.
 *
 * Data is stored in sessionStorage with the same `{ t, v }` format
 * as useCachedFetch, so anything cached by usePrefetch is immediately
 * available to useCachedFetch on the same key.
 *
 * Three strategies:
 *   1. prefetch        — fetch immediately
 *   2. prefetchOnIdle  — defer until the browser is idle
 *   3. prefetchOnVisible — wait until a DOM element scrolls into view
 *
 * Cleanup is automatic: any pending callbacks or observers are
 * cancelled when the component unmounts.
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

    const prefetch = useCallback(
        async <T>(key: string, fetcher: () => Promise<T>): Promise<void> => {
            const v = await fetcher();
            try {
                sessionStorage.setItem(key, JSON.stringify({ t: Date.now(), v }));
            } catch {
                // private browsing or quota exceeded — silently ignore
            }
        },
        [],
    );

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

    const prefetchOnVisible = useCallback(
        <T>(
            ref: React.RefObject<HTMLElement | null>,
            key: string,
            fetcher: () => Promise<T>,
        ): void => {
            const el = ref.current;
            if (!el) return;

            let cancelled = false;

            const observer = new IntersectionObserver(
                (entries) => {
                    if (entries[0]?.isIntersecting && !cancelled) {
                        observer.disconnect();
                        (async () => {
                            try {
                                const v = await fetcher();
                                if (cancelled) return;
                                sessionStorage.setItem(
                                    key,
                                    JSON.stringify({ t: Date.now(), v }),
                                );
                            } catch {
                                // silently ignore
                            }
                        })();
                    }
                },
                { threshold: 0 },
            );

            observer.observe(el);

            cleanupsRef.current.push(() => {
                cancelled = true;
                observer.disconnect();
            });
        },
        [],
    );

    return { prefetch, prefetchOnIdle, prefetchOnVisible };
}