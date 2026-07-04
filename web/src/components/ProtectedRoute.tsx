import { Navigate, useLocation } from "react-router-dom";
import { useAuth } from "@/hooks/useAuth";
import { getToken } from "@/api/client";

interface ProtectedRouteProps {
    /** If true, only admins may render the children. */
    adminOnly?: boolean;
    children: React.ReactNode;
}

/**
 * Route guard.
 *
 * - No token in localStorage -> redirect to /login (preserving the
 *   intended destination via `state.from`).
 * - adminOnly + non-admin -> redirect to /dashboard.
 *
 * The AuthProvider also kicks off /user/me on mount; we don't block
 * on it here because the JWT itself is enough to attempt the route,
 * and a 401 from any sub-request will bounce the user to /login.
 */
export function ProtectedRoute({ adminOnly, children }: ProtectedRouteProps) {
    const location = useLocation();
    const { isAdmin, initialized } = useAuth();
    const token = getToken();

    if (!token) {
        return (
            <Navigate to="/login" state={{ from: location.pathname }} replace />
        );
    }

    // Wait for the first /user/me probe so isAdmin is accurate before
    // deciding an admin-only redirect.
    if (adminOnly && initialized && !isAdmin) {
        return <Navigate to="/dashboard" replace />;
    }

    return <>{children}</>;
}

export default ProtectedRoute;
