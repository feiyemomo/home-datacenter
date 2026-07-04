import { Navigate, Route, Routes } from "react-router-dom";
import { AuthProvider, useAuth } from "@/hooks/useAuth";
import { ProtectedRoute } from "@/components/ProtectedRoute";
import { Layout } from "@/components/Layout";
import Login from "@/pages/Login";
import Dashboard from "@/pages/Dashboard";
import Cameras from "@/pages/Cameras";
import Devices from "@/pages/Devices";
import MqttDebug from "@/pages/MqttDebug";
import Profile from "@/pages/Profile";

/**
 * Application routes.
 *
 * - /login           public
 * - /dashboard       auth
 * - /cameras         auth (admin for mutating)
 * - /devices         auth
 * - /mqtt            auth + admin
 * - /profile         auth
 *
 * A tiny full-screen splash is shown while AuthProvider resolves the
 * initial /user/me probe so route guards see an accurate `isAdmin`.
 */
export default function App() {
    return (
        <AuthProvider>
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
                    path="/devices"
                    element={
                        <ProtectedRoute>
                            <Layout>
                                <Devices />
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
        </AuthProvider>
    );
}

/** Named export so Suspense/lazy could wrap it later if needed. */
export function useAuthState() {
    return useAuth();
}
