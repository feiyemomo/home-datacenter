import { useState, type FormEvent } from "react";
import { Navigate, useLocation, useNavigate } from "react-router-dom";
import { Server, KeyRound, User as UserIcon, Loader2 } from "lucide-react";
import { useAuth } from "@/hooks/useAuth";
import { Button } from "@/components/ui/button";
import { Input, Label } from "@/components/ui/input";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { ApiError } from "@/api/client";

interface LocationState {
    from?: string;
}

/**
 * Login screen.
 *
 * Collects user_id (number) + access_key (string), calls /auth/bind,
 * then redirects to the originally requested route (or /dashboard).
 */
export default function Login() {
    const { login, token } = useAuth();
    const navigate = useNavigate();
    const location = useLocation();
    const from = (location.state as LocationState | null)?.from ?? "/dashboard";

    const [userId, setUserId] = useState("");
    const [accessKey, setAccessKey] = useState("");
    const [error, setError] = useState<string | null>(null);
    const [submitting, setSubmitting] = useState(false);

    // Already authenticated -> bounce to the app.
    if (token) {
        return <Navigate to={from} replace />;
    }

    async function handleSubmit(e: FormEvent) {
        e.preventDefault();
        setError(null);

        const trimmedId = userId.trim();
        const trimmedKey = accessKey.trim();
        if (!trimmedId || !trimmedKey) {
            setError("user_id and access_key are required");
            return;
        }
        const idNum = Number(trimmedId);
        if (!Number.isInteger(idNum) || idNum <= 0) {
            setError("user_id must be a positive integer");
            return;
        }

        setSubmitting(true);
        try {
            await login(idNum, trimmedKey);
            navigate(from, { replace: true });
        } catch (err) {
            const msg =
                err instanceof ApiError
                    ? err.message
                    : err instanceof Error
                        ? err.message
                        : "login failed";
            setError(msg);
        } finally {
            setSubmitting(false);
        }
    }

    return (
        <div className="relative flex min-h-screen items-center justify-center overflow-hidden bg-surface px-4">
            {/* Ambient background glow */}
            <div className="pointer-events-none absolute -top-32 -left-32 h-96 w-96 rounded-full bg-sky-500/10 blur-3xl" />
            <div className="pointer-events-none absolute -bottom-32 -right-32 h-96 w-96 rounded-full bg-indigo-500/10 blur-3xl" />

            <Card className="w-full max-w-md">
                <CardHeader className="items-center text-center">
                    <div className="mx-auto mb-3 flex h-12 w-12 items-center justify-center rounded-xl bg-gradient-to-br from-sky-500 to-indigo-600 text-white shadow-lg shadow-sky-500/30">
                        <Server size={24} />
                    </div>
                    <CardTitle className="text-base">Sign in to Home Datacenter</CardTitle>
                    <p className="text-xs text-slate-500">
                        Bind this browser to an existing device credential.
                    </p>
                </CardHeader>

                <CardContent>
                    <form onSubmit={handleSubmit} className="space-y-4">
                        <div className="space-y-2">
                            <Label htmlFor="user_id">User ID</Label>
                            <div className="relative">
                                <UserIcon
                                    size={16}
                                    className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-slate-500"
                                />
                                <Input
                                    id="user_id"
                                    type="number"
                                    inputMode="numeric"
                                    min={1}
                                    step={1}
                                    placeholder="1"
                                    className="pl-9"
                                    value={userId}
                                    onChange={(e) => setUserId(e.target.value)}
                                    autoComplete="username"
                                    disabled={submitting}
                                />
                            </div>
                        </div>

                        <div className="space-y-2">
                            <Label htmlFor="access_key">Access Key</Label>
                            <div className="relative">
                                <KeyRound
                                    size={16}
                                    className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-slate-500"
                                />
                                <Input
                                    id="access_key"
                                    type="password"
                                    placeholder="hex access key"
                                    className="pl-9 font-mono"
                                    value={accessKey}
                                    onChange={(e) => setAccessKey(e.target.value)}
                                    autoComplete="current-password"
                                    disabled={submitting}
                                />
                            </div>
                        </div>

                        {error && (
                            <div className="rounded-md border border-rose-500/40 bg-rose-500/10 px-3 py-2 text-xs text-rose-300">
                                {error}
                            </div>
                        )}

                        <Button
                            type="submit"
                            className="w-full"
                            disabled={submitting}
                        >
                            {submitting ? (
                                <>
                                    <Loader2 size={16} className="animate-spin" />
                                    Binding…
                                </>
                            ) : (
                                "Sign in"
                            )}
                        </Button>
                    </form>

                    <p className="mt-4 text-center text-[11px] text-slate-600">
                        Tokens are valid for 365 days. Stored locally on this device.
                    </p>
                </CardContent>
            </Card>
        </div>
    );
}
