import { useCallback, useEffect, useState } from "react";

/**
 * Theme = "light" | "dark". Persisted in localStorage as
 * `home.theme`. The first read happens synchronously during
 * hook init so the first paint already has the right
 * `data-theme` attribute set on <html> (no flash).
 *
 * The default is "dark" to match the dashboard's original
 * look. Once a user toggles, their choice sticks across
 * reloads. A future improvement would be to add a "system"
 * mode that follows `prefers-color-scheme`; left as a future
 * hook parameter for now.
 */
export type Theme = "light" | "dark";

const STORAGE_KEY = "home.theme";

function readInitial(): Theme {
    if (typeof window === "undefined") return "dark";
    try {
        const v = window.localStorage.getItem(STORAGE_KEY);
        if (v === "light" || v === "dark") return v;
    } catch {
        // localStorage may throw under private browsing in some
        // browsers — fall through to the default.
    }
    return "dark";
}

/**
 * useTheme — single source of truth for the active theme.
 *
 * Returns the current theme + a setter that persists the
 * choice. Setting either value is cheap (one DOM attribute
 * write + one localStorage write). The hook subscribes to
 * `storage` events so a toggle in one tab is reflected in
 * the other tabs of the same origin without a refresh.
 */
export function useTheme(): [Theme, (next: Theme) => void] {
    const [theme, setThemeState] = useState<Theme>(readInitial);

    useEffect(() => {
        // Apply the initial theme to <html>. This effect is
        // idempotent: setting the same attribute is a no-op.
        document.documentElement.setAttribute("data-theme", theme);
    }, [theme]);

    // Cross-tab sync: if the user toggles in another tab, the
    // `storage` event fires here and we re-apply the change.
    useEffect(() => {
        function onStorage(e: StorageEvent) {
            if (e.key !== STORAGE_KEY) return;
            const next = e.newValue;
            if (next === "light" || next === "dark") {
                setThemeState(next);
            }
        }
        window.addEventListener("storage", onStorage);
        return () => window.removeEventListener("storage", onStorage);
    }, []);

    const setTheme = useCallback((next: Theme) => {
        setThemeState(next);
        try {
            window.localStorage.setItem(STORAGE_KEY, next);
        } catch {
            // localStorage write failed (quota / private mode).
            // The in-memory state is still correct, so the UI
            // is consistent for this session.
        }
    }, []);

    return [theme, setTheme];
}

/**
 * applyThemeEarly — call this in main.tsx BEFORE React mounts
 * so the initial paint already has the right theme attribute
 * on <html>. Without it, the page would render with the
 * default dark theme for ~16ms before useTheme's first
 * effect runs, producing a visible flash on slow devices.
 */
export function applyThemeEarly(): void {
    if (typeof document === "undefined") return;
    document.documentElement.setAttribute("data-theme", readInitial());
}
