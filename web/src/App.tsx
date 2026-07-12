import { Navigate, Route, Routes } from "react-router-dom";
import { AuthProvider, useAuth } from "@/hooks/useAuth";
import { ProtectedRoute } from "@/components/ProtectedRoute";
import { Layout } from "@/components/Layout";
import Login from "@/pages/Login";
import Dashboard from "@/pages/Dashboard";
import Cameras from "@/pages/Cameras";
import Network from "@/pages/Network";
import EventsPage from "@/pages/Events";
import DeviceCreate from "@/pages/DeviceCreate";
import Devices from "@/pages/Devices";
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
 * - /devices          auth
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
                    path="/events"
                    element={
                        <ProtectedRoute>
                            <Layout>
                                <EventsPage />
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
        </AuthProvider>
    );
}

/** Named export so Suspense/lazy could wrap it later if needed. */
export function useAuthState() {
    return useAuth();
}
