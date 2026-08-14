import { Navigate, Route, Routes, useLocation } from "react-router-dom";
import { Loader2, Server } from "lucide-react";
import { useEffect } from "react";
import { AuthProvider, useAuth } from "@/hooks/useAuth";
import { ProtectedRoute } from "@/components/ProtectedRoute";
import { ErrorBoundary } from "@/components/ErrorBoundary";
import { usePrefetch } from "@/hooks/usePrefetch";
import { listCameras } from "@/api/camera";
import { Layout } from "@/components/Layout";
import OfflineBanner from "@/components/OfflineBanner";
import Login from "@/pages/Login";
import Dashboard from "@/pages/Dashboard";
import Cameras from "@/pages/Cameras";
import Network from "@/pages/Network";
import DeviceCreate from "@/pages/DeviceCreate";
import Logs from "@/pages/Logs";
import MqttDebug from "@/pages/MqttDebug";
import Profile from "@/pages/Profile";
import Users from "@/pages/Users";

/**
 * Application routes.
 *
 * - /login            public
 * - /dashboard        auth
 * - /cameras          auth (admin for mutating)
 * - /cameras/new      auth + admin (dedicated device-create page)
 * - /users            auth + admin
 * - /mqtt             auth + admin
 * - /profile          auth
 *
 * A tiny full-screen splash is shown while AuthProvider resolves the
 * initial /user/me probe so route guards see an accurate `isAdmin`.
 */
export default function App() {
    return (
        <AuthProvider>
            <AppRoutes />
        </AuthProvider>
    );
}

/**
 * Renders the route tree. Shows a full-screen splash while AuthProvider
 * resolves the initial /user/me probe (token present but not yet
 * initialized), then animates each route change with a fade/slide-up.
 */
function AppRoutes() {
    const { token, initialized } = useAuth();
    const location = useLocation();
    const { prefetchOnIdle } = usePrefetch();

    // v1.8.42: prefetch the camera list during the splash wait. The
    // splash is a pure spinner while /user/me resolves — that idle time
    // is otherwise wasted. Warming "home.cameras.list" here means the
    // Dashboard AND Cameras pages both render their first frame from
    // cache with no loading flash. The freshness guard in usePrefetch
    // ensures it never clobbers newer WS-driven data.
    useEffect(() => {
        if (token && !initialized) {
            prefetchOnIdle("home.cameras.list", () => listCameras(), 0);
        }
    }, [token, initialized, prefetchOnIdle]);

    // Full-screen splash while the /user/me probe is in flight.
    if (token && !initialized) {
        return <SplashScreen />;
    }

    return (
        <ErrorBoundary context="app">
            <div key={location.pathname} className="animate-slide-up">
                <OfflineBanner />
                <Routes>
                    <Route path="/login" element={<Login />} />

                    <Route
                        path="/dashboard"
                        element={
                            <ProtectedRoute>
                                <Layout>
                                    <Dashboard />
                                </Layout>
                            </ProtectedRoute>
                        }
                    />
                    <Route
                        path="/cameras"
                        element={
                            <ProtectedRoute>
                                <Layout>
                                    <Cameras />
                                </Layout>
                            </ProtectedRoute>
                        }
                    />
                    <Route
                        path="/network"
                        element={
                            <ProtectedRoute>
                                <Layout>
                                    <Network />
                                </Layout>
                            </ProtectedRoute>
                        }
                    />
                    <Route
                        path="/logs"
                        element={
                            <ProtectedRoute adminOnly>
                                <Layout>
                                    <Logs />
                                </Layout>
                            </ProtectedRoute>
                        }
                    />
                    <Route
                        path="/cameras/new"
                        element={
                            <ProtectedRoute adminOnly>
                                <Layout>
                                    <DeviceCreate />
                                </Layout>
                            </ProtectedRoute>
                        }
                    />
                    <Route
                        path="/users"
                        element={
                            <ProtectedRoute adminOnly>
                                <Layout>
                                    <Users />
                                </Layout>
                            </ProtectedRoute>
                        }
                    />
                    <Route
                        path="/mqtt"
                        element={
                            <ProtectedRoute adminOnly>
                                <Layout>
                                    <MqttDebug />
                                </Layout>
                            </ProtectedRoute>
                        }
                    />
                    <Route
                        path="/profile"
                        element={
                            <ProtectedRoute>
                                <Layout>
                                    <Profile />
                                </Layout>
                            </ProtectedRoute>
                        }
                    />

                    {/* Default redirects */}
                    <Route path="/" element={<Navigate to="/dashboard" replace />} />
                    <Route path="*" element={<Navigate to="/dashboard" replace />} />
                </Routes>
            </div>
        </ErrorBoundary>
    );
}

/**
 * Full-screen liquid-glass splash shown while AuthProvider resolves the
 * initial /user/me probe so route guards see an accurate `isAdmin`.
 */
function SplashScreen() {
    return (
        <div className="relative flex min-h-screen items-center justify-center overflow-hidden bg-surface">
            {/* Ambient orbs */}
            <div className="orb orb-warm" style={{ width: 550, height: 550, top: -180, left: -150 }} />
            <div className="orb orb-cool" style={{ width: 450, height: 450, bottom: -120, right: -120 }} />
            <div className="orb orb-accent" style={{ width: 250, height: 250, top: "50%", left: "55%" }} />

            <div className="relative z-10 flex flex-col items-center gap-5 animate-scale-in">
                <div className="relative flex h-16 w-16 items-center justify-center rounded-2xl bg-gradient-to-br from-[rgb(var(--accent-warm)/0.6)] to-[rgb(var(--accent-primary)/0.4)] text-white glass-glow">
                    <Server size={32} />
                    <div className="absolute inset-0 rounded-2xl bg-gradient-to-t from-transparent to-white/20" />
                </div>
                <div className="flex items-center gap-2 text-sm text-fg-muted">
                    <Loader2 size={16} className="animate-spin text-[rgb(var(--accent-warm))]" />
                    加载中…
                </div>
            </div>
        </div>
    );
}
