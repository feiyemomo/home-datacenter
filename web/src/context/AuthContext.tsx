import {
    createContext,
    useCallback,
    useContext,
    useEffect,
    useMemo,
    useState,
    type ReactNode,
} from "react";
import { bind as bindApi } from "@/api/auth";
import { getCurrentUser } from "@/api/system";
import { clearTokenAndRedirect, getToken, setToken } from "@/api/client";
import { listCameras, listAlerts, preheatCamera } from "@/api/camera";
import { getNetworkStatus } from "@/api/network";
import { getWeather } from "@/api/weather";
import { decodeJwtPayload } from "@/lib/utils";
import type { JwtClaims, User } from "@/types";

// Prefetch sessionStorage keys — must match useCachedFetch keys exactly
// so that cached values are immediately available to the consuming pages.
//   home.cameras.list       — Cameras.tsx
//   home.network.status     — Network.tsx (structure: { status, clientIPv6 })
//   home.dashboard.weather  — WeatherCard.tsx (rendered on Dashboard)
//   home.dashboard.alerts   — Dashboard.tsx
const PREFETCH_KEYS = {
    cameras: "home.cameras.list",
    network: "home.network.status",
    weather: "home.dashboard.weather",
    alerts: "home.dashboard.alerts",
} as const;

// Mirrors usePrefetch's PREFETCH_TTL_MS. A cache entry younger than this
// is "fresh" and is never overwritten by a prefetch — otherwise a slow
// prefetch response could clobber fresher WS-driven data with a stale
// snapshot (the "screen going backwards" regression).
const PREFETCH_TTL_MS = 30_000;

interface AuthContextValue {
    token: string | null;
    user: User | null;
    /** Decoded JWT claims (user_id, device_id, exp, iat). */
    claims: JwtClaims | null;
    /** True once we've finished the initial /user/me probe. */
    initialized: boolean;
    isAdmin: boolean;
    login: (userId: number, accessKey: string) => Promise<void>;
    logout: () => void;
}

const AuthContext = createContext<AuthContextValue | undefined>(undefined);

export function AuthProvider({ children }: { children: ReactNode }) {
    const [token, setTokenState] = useState<string | null>(() => getToken());
    const [user, setUser] = useState<User | null>(null);
    const [initialized, setInitialized] = useState(false);

    const claims = useMemo<JwtClaims | null>(() => {
        if (!token) return null;
        return decodeJwtPayload<JwtClaims>(token);
    }, [token]);

    // On mount (or when token changes), fetch the user identity AND
    // prefetch Dashboard first-screen data in parallel. The prefetch
    // runs alongside getCurrentUser() so the splash screen overlaps
    // with real work instead of idling. We wait for everything to
    // settle (or a 2000ms timeout, whichever comes first) before
    // flipping `initialized` so the splash doesn't become a blocker.
    useEffect(() => {
        if (!token) {
            setUser(null);
            setInitialized(true);
            return;
        }
        let cancelled = false;

        // Best-effort prefetch — each call settles independently and
        // rejected ones are silently skipped (not written to cache).
        const prefetchPromise = Promise.allSettled([
            listCameras(),
            getNetworkStatus(),
            getWeather(),
            listAlerts(),
        ]).then(([cameras, network, weather, alerts]) => {
            if (cancelled) return;
            const now = Date.now();
            const writeCache = (key: string, v: unknown) => {
                try {
                    // Freshness guard (mirrors usePrefetch): never
                    // overwrite an existing cache entry newer than 30s
                    // so a slow prefetch can't clobber fresher WS-driven
                    // data with a stale snapshot.
                    const raw = sessionStorage.getItem(key);
                    if (raw) {
                        const parsed = JSON.parse(raw) as { t: number };
                        if (parsed && now - parsed.t < PREFETCH_TTL_MS) {
                            return;
                        }
                    }
                    sessionStorage.setItem(key, JSON.stringify({ t: now, v }));
                } catch {
                    // private browsing or quota exceeded — silently ignore
                }
            };
            if (cameras.status === "fulfilled") {
                writeCache(PREFETCH_KEYS.cameras, cameras.value);
                // v1.9.x: warm the backend (go2rtc/RTSP) streams during
                // splash so the first WebRTC/HLS/MP4 request doesn't pay
                // the 1-10s cold-start. Fire-and-forget — never blocks the
                // splash gate. go2rtc keeps producers warm for 120s
                // (#stop=120, see Registry.StopTimeout), so a normal login
                // → live-view navigation hits a hot source without the
                // 1-2s cold-start; idle cameras still release after 120s
                // so they cost nothing long-term. Only online cameras are
                // warmed; offline ones can't connect and would just fail
                // silently.
                for (const cam of cameras.value) {
                    if (cam.status === "online") {
                        preheatCamera(cam.id).catch(() => {});
                    }
                }
            }
            if (network.status === "fulfilled") {
                // Match the consuming Network.tsx shape: { status, clientIPv6 }.
                // clientIPv6 is unknown during splash (it needs a separate
                // client-side probe), so it's seeded as null and the page
                // self-heals with "checking…" on mount.
                writeCache(PREFETCH_KEYS.network, {
                    status: network.value,
                    clientIPv6: null,
                });
            }
            if (weather.status === "fulfilled") {
                writeCache(PREFETCH_KEYS.weather, weather.value);
            }
            if (alerts.status === "fulfilled") {
                writeCache(PREFETCH_KEYS.alerts, alerts.value);
            }
        });

        // User identity — 401 is handled by the axios interceptor
        // (redirect to /login); here we just clear the user and
        // continue to `initialized` so the splash resolves.
        const userPromise = getCurrentUser()
            .then((u) => {
                if (!cancelled) setUser(u);
            })
            .catch(() => {
                if (!cancelled) setUser(null);
            });

        const timeoutPromise = new Promise<void>((resolve) =>
            setTimeout(resolve, 2000),
        );

        Promise.race([
            Promise.all([userPromise, prefetchPromise]),
            timeoutPromise,
        ]).then(() => {
            if (!cancelled) setInitialized(true);
        });

        return () => {
            cancelled = true;
        };
    }, [token]);

    const login = useCallback(async (userId: number, accessKey: string) => {
        const jwt = await bindApi(userId, accessKey);
        setToken(jwt);
        setTokenState(jwt);
        // Fetch the user identity immediately so isAdmin is available
        // before the first protected route renders.
        const u = await getCurrentUser();
        setUser(u);
    }, []);

    const logout = useCallback(() => {
        clearTokenAndRedirect();
        setTokenState(null);
        setUser(null);
    }, []);

    const value: AuthContextValue = {
        token,
        user,
        claims,
        initialized,
        isAdmin: user?.is_admin ?? false,
        login,
        logout,
    };

    return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

/** Consume the AuthContext. Throws if used outside <AuthProvider>. */
export function useAuth(): AuthContextValue {
    const ctx = useContext(AuthContext);
    if (!ctx) {
        throw new Error("useAuth must be used within an AuthProvider");
    }
    return ctx;
}

export default AuthContext;
