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
import { decodeJwtPayload } from "@/lib/utils";
import type { JwtClaims, User } from "@/types";

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

    // On mount (or when token changes), fetch the user identity.
    useEffect(() => {
        if (!token) {
            setUser(null);
            setInitialized(true);
            return;
        }
        let cancelled = false;
        getCurrentUser()
            .then((u) => {
                if (!cancelled) {
                    setUser(u);
                    setInitialized(true);
                }
            })
            .catch(() => {
                // 401 path is handled by the axios interceptor (redirect to /login).
                if (!cancelled) {
                    setUser(null);
                    setInitialized(true);
                }
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
