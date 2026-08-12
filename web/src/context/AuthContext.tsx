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
import { listCameras, listAlerts } from "@/api/camera";
import { getNetworkStatus } from "@/api/network";
import { getWeather } from "@/api/weather";
import { decodeJwtPayload } from "@/lib/utils";
import type { JwtClaims, User } from "@/types";

// Prefetch sessionStorage keys — must match useCachedFetch keys exactly
// so that cached values are immediately available to the consuming pages.
//   home.cameras.list        — Cameras.tsx
//   home.network.status      — Network.tsx
//   home.dashboard.weather   — WeatherCard.tsx (rendered on Dashboard)
//   home.dashboard.alerts    — Dashboard.tsx
const PREFETCH_KEYS = {
    cameras: "home.cameras.list",
    network: "home.network.status",
    weather: "home.dashboard.weather",
    alerts: "home.dashboard.alerts",
} as const;

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
                    sessionStorage.setItem(key, JSON.stringify({ t: now, v }));
                } catch {
                    // private browsing or quota exceeded — silently ignore
                }
            };
            if (cameras.status === "fulfilled") {
                writeCache(PREFETCH_KEYS.cameras, cameras.value);
            }
            if (network.status === "fulfilled") {
                writeCache(PREFETCH_KEYS.network, network.value);
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
