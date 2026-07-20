import { useCallback, useEffect, useState } from "react";

/**
 * Theme = "light" | "dark" | "system". Persisted in localStorage as
 * `home.theme`. The first read happens synchronously during hook
 * init so the first paint already has the right `data-theme`
 * attribute set on <html> (no flash).
 *
 *  - "light"  — force light theme
 *  - "dark"   — force dark theme (default, matches original look)
 *  - "system" — follow `prefers-color-scheme: dark` media query.
 *               We subscribe to changes so OS-level theme switches
 *               propagate to the dashboard live (no reload needed).
 *
 * For "system", the *resolved* theme (what's actually applied to
 * <html>) is one of "light" | "dark". The `theme` field stays
 * "system" so the toggle UI can show the user's choice; callers
 * that need the resolved value use the `resolved` field.
 */
export type Theme = "light" | "dark" | "system";
export type ResolvedTheme = "light" | "dark";

const STORAGE_KEY = "home.theme";

function systemPrefersDark(): boolean {
    if (typeof window === "undefined" || !window.matchMedia) return false;
    return window.matchMedia("(prefers-color-scheme: dark)").matches;
}

function resolveTheme(t: Theme): ResolvedTheme {
    if (t === "system") return systemPrefersDark() ? "dark" : "light";
    return t;
}

function readInitial(): Theme {
    if (typeof window === "undefined") return "dark";
    try {
        const v = window.localStorage.getItem(STORAGE_KEY);
        if (v === "light" || v === "dark" || v === "system") return v;
    } catch {
        // localStorage may throw under private browsing in some
        // browsers — fall through to the default.
    }
    return "dark";
}

/**
 * useTheme — single source of truth for the active theme.
 *
 * Returns the current theme choice + a setter that persists the
 * choice, plus the `resolved` theme (the actual "light" or "dark"
 * applied to <html> — only differs from `theme` when theme === "system").
 *
 * Setting any value is cheap (one DOM attribute write + one
 * localStorage write). The hook subscribes to `storage` events so
 * a toggle in one tab is reflected in other tabs of the same origin
 * without a refresh. When the user picks "system", we also subscribe
 * to `prefers-color-scheme` changes so an OS theme switch updates
 * the dashboard live.
 */
export function useTheme(): {
    theme: Theme;
    resolved: ResolvedTheme;
    setTheme: (next: Theme) => void;
} {
    const [theme, setThemeState] = useState<Theme>(readInitial);
    const [resolved, setResolved] = useState<ResolvedTheme>(() => resolveTheme(readInitial()));

    // Apply the resolved theme to <html>. Idempotent.
    useEffect(() => {
        const next = resolveTheme(theme);
        setResolved(next);
        document.documentElement.setAttribute("data-theme", next);
    }, [theme]);

    // When the user picks "system", listen for OS dark-mode changes
    // and re-resolve. When they pick an explicit theme, skip the
    // listener (no-op since resolveTheme returns the explicit value).
    useEffect(() => {
        if (theme !== "system") return;
        const mq = window.matchMedia("(prefers-color-scheme: dark)");
        const onChange = () => {
            const next: ResolvedTheme = mq.matches ? "dark" : "light";
            setResolved(next);
            document.documentElement.setAttribute("data-theme", next);
        };
        // addEventListener is the modern API; addListener is the
        // Safari < 14 fallback. We try both for safety.
        if (typeof mq.addEventListener === "function") {
            mq.addEventListener("change", onChange);
            return () => mq.removeEventListener("change", onChange);
        }
        const legacyMq = mq as unknown as {
            addListener?: (l: (e: MediaQueryListEvent) => void) => void;
            removeListener?: (l: (e: MediaQueryListEvent) => void) => void;
        };
        if (typeof legacyMq.addListener === "function") {
            legacyMq.addListener(onChange);
            return () => legacyMq.removeListener?.(onChange);
        }
        return () => undefined;
    }, [theme]);

    // Cross-tab sync: if the user toggles in another tab, the
    // `storage` event fires here and we re-apply the change.
    useEffect(() => {
        function onStorage(e: StorageEvent) {
            if (e.key !== STORAGE_KEY) return;
            const next = e.newValue;
            if (next === "light" || next === "dark" || next === "system") {
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

    return { theme, resolved, setTheme };
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
    const t = readInitial();
    document.documentElement.setAttribute("data-theme", resolveTheme(t));
}
