import { useCallback, useEffect, useRef, useState } from "react";

/**
 * useCachedFetch — fetches data with sessionStorage caching and
 * silent background refresh.
 *
 * Pattern:
 *   1. On mount, read from sessionStorage (instant display, no
 *      loading flash when navigating back to the page).
 *   2. Immediately fetch fresh data in the background.
 *   3. Update both the UI and sessionStorage with the fresh data.
 *   4. Optionally poll on an interval (always silent after the
 *      first load — `loading` only stays true when there is no
 *      cached value to show).
 *
 * This is useful for widgets that:
 *   - Are expensive to fetch (weather, camera list)
 *   - Are re-mounted on page navigation (no flash of "loading…")
 *   - Have data that changes infrequently relative to the poll
 *     interval, so the cached value is "good enough" while the
 *     refresh is in flight.
 *
 * The cache is per-key (caller picks a stable key like
 * "home.dashboard.weather"). sessionStorage is used (not
 * localStorage) because the cached values are transient — a fresh
 * fetch will always replace them, and we don't want stale widgets
 * after the browser is closed and reopened.
 *
 * TTL: the cached value is shown regardless of age (better to show
 * stale data than a loading flash). The TTL is used only as a hint
 * to decide whether to show a "refreshing…" indicator — currently
 * unused, reserved for future UX.
 */
export function useCachedFetch<T>(
    key: string,
    fetcher: () => Promise<T>,
    options: {
        /** Polling interval in ms. 0 = no polling (one-shot refresh). */
        refetchMs?: number;
        /** Cache TTL in ms (reserved for future UX; currently unused). */
        ttlMs?: number;
        /** Skip the fetch entirely when false. */
        enabled?: boolean;
    } = {},
): {
    data: T | null;
    loading: boolean;
    error: Error | null;
    refetch: () => void;
} {
    const { refetchMs = 0, enabled = true } = options;

    // Read cached value synchronously on first render so the UI
    // can paint immediately without a loading flash.
    const [data, setData] = useState<T | null>(() => {
        if (!enabled) return null;
        try {
            const raw = sessionStorage.getItem(key);
            if (!raw) return null;
            const parsed = JSON.parse(raw) as { t: number; v: T };
            return parsed.v;
        } catch {
            return null;
        }
    });

    // Only show the loading state when there is no cached data
    // to display. Once we have any data (cached or fresh), the
    // background refresh is silent.
    const [loading, setLoading] = useState(() => {
        if (!enabled) return false;
        try {
            return sessionStorage.getItem(key) === null;
        } catch {
            return true;
        }
    });

    const [error, setError] = useState<Error | null>(null);

    // Keep a live ref to the fetcher so changes to the caller's
    // closure (e.g. new function identity each render) don't
    // trigger a refetch storm.
    const fetcherRef = useRef(fetcher);
    fetcherRef.current = fetcher;

    const fetchNow = useCallback(() => {
        let cancelled = false;
        (async () => {
            try {
                const v = await fetcherRef.current();
                if (cancelled) return;
                setData(v);
                setError(null);
                try {
                    sessionStorage.setItem(key, JSON.stringify({ t: Date.now(), v }));
                } catch {
                    // private browsing or quota exceeded — accept
                    // loss of persistence, the in-memory state is
                    // still updated.
                }
            } catch (e) {
                if (cancelled) return;
                setError(e instanceof Error ? e : new Error(String(e)));
            } finally {
                if (!cancelled) setLoading(false);
            }
        })();
    }, [key]);

    // Initial fetch + optional polling.
    useEffect(() => {
        if (!enabled) return;
        fetchNow();
        if (refetchMs > 0) {
            const id = window.setInterval(fetchNow, refetchMs);
            return () => window.clearInterval(id);
        }
    }, [fetchNow, refetchMs, enabled]);

    return { data, loading, error, refetch: fetchNow };
}
