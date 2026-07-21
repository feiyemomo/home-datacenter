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

export default function Login() {
    const { login, token } = useAuth();
    const navigate = useNavigate();
    const location = useLocation();
    const from = (location.state as LocationState | null)?.from ?? "/dashboard";

    const [userId, setUserId] = useState("");
    const [accessKey, setAccessKey] = useState("");
    const [error, setError] = useState<string | null>(null);
    const [submitting, setSubmitting] = useState(false);

    if (token) {
        return <Navigate to={from} replace />;
    }

    async function handleSubmit(e: FormEvent) {
        e.preventDefault();
        setError(null);

        const trimmedId = userId.trim();
        const trimmedKey = accessKey.trim();
        if (!trimmedId || !trimmedKey) {
            setError("用户 ID 和 Access Key 不能为空");
            return;
        }
        const idNum = Number(trimmedId);
        if (!Number.isInteger(idNum) || idNum <= 0) {
            setError("用户 ID 必须为正整数");
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
                        : "登录失败";
            setError(msg);
        } finally {
            setSubmitting(false);
        }
    }

    return (
        <div className="relative flex min-h-screen items-center justify-center overflow-hidden bg-surface px-4">
            {/* Ambient orbs */}
            <div className="orb orb-warm" style={{ width: 500, height: 500, top: -150, left: -150 }} />
            <div className="orb orb-cool" style={{ width: 400, height: 400, bottom: -100, right: -100 }} />
            <div className="orb orb-accent" style={{ width: 300, height: 300, top: "50%", left: "60%" }} />

            <Card className="w-full max-w-md animate-scale-in">
                <CardHeader className="items-center text-center">
                    <div className="mx-auto mb-3 flex h-14 w-14 items-center justify-center rounded-2xl bg-gradient-to-br from-[rgb(var(--accent-primary)/0.6)] to-[rgb(var(--accent-warm)/0.4)] text-white glass-glow">
                        <Server size={26} />
                    </div>
                    <CardTitle className="text-base">登录到家庭数据中心</CardTitle>
                    <p className="text-xs text-fg-muted">
                        将此浏览器绑定到已存在的设备凭据。
                    </p>
                </CardHeader>

                <CardContent>
                    <form onSubmit={handleSubmit} className="space-y-4">
                        <div className="space-y-2">
                            <Label htmlFor="user_id">用户 ID</Label>
                            <div className="relative">
                                <UserIcon
                                    size={16}
                                    className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
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
                                    className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-fg-subtle"
                                />
                                <Input
                                    id="access_key"
                                    type="password"
                                    placeholder="十六进制 Access Key"
                                    className="pl-9 font-mono"
                                    value={accessKey}
                                    onChange={(e) => setAccessKey(e.target.value)}
                                    autoComplete="current-password"
                                    disabled={submitting}
                                />
                            </div>
                        </div>

                        {error && (
                            <div className="rounded-xl glass bg-[rgb(var(--accent-danger)/0.1)] px-4 py-3 text-xs text-[rgb(var(--accent-danger))] animate-fade-in">
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
                                    绑定中…
                                </>
                            ) : (
                                "登录"
                            )}
                        </Button>
                    </form>

                    <p className="mt-4 text-center text-[11px] text-fg-subtle">
                        令牌有效期 365 天，存储在本设备本地。
                    </p>
                </CardContent>
            </Card>
        </div>
    );
}
