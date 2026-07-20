import { useEffect, useMemo, useState } from "react";
import {
    KeyRound,
    HardDrive,
    ShieldCheck,
    User as UserIcon,
    Clock,
} from "lucide-react";
import { listDevices } from "@/api/device";
import { getCurrentUser } from "@/api/system";
import { ApiError } from "@/api/client";
import { useAuth } from "@/hooks/useAuth";
import { cn, formatCountdown, formatDateTime } from "@/lib/utils";
import type { Device, JwtClaims, User } from "@/types";
import {
    Card,
    CardContent,
    CardDescription,
    CardHeader,
    CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";

/**
 * Profile page.
 *
 * Shows the current user, decoded JWT claims, token expiry countdown,
 * and the list of devices bound to this user.
 */
export default function Profile() {
    const { user: ctxUser, claims, token } = useAuth();
    const [user, setUser] = useState<User | null>(ctxUser);
    const [devices, setDevices] = useState<Device[]>([]);
    const [error, setError] = useState<string | null>(null);
    const [now, setNow] = useState(() => Date.now());

    // Refresh user + bound devices on mount.
    useEffect(() => {
        let cancelled = false;
        (async () => {
            try {
                const [u, all] = await Promise.all([
                    getCurrentUser(),
                    listDevices(),
                ]);
                if (cancelled) return;
                setUser(u);
                setDevices(all.filter((d) => d.user_id === u.id));
            } catch (err) {
                if (cancelled) return;
                setError(
                    err instanceof ApiError
                        ? err.message
                        : err instanceof Error
                            ? err.message
                            : "failed to load profile",
                );
            }
        })();
        return () => {
            cancelled = true;
        };
    }, []);

    // 1-second ticker for the expiry countdown.
    useEffect(() => {
        const id = window.setInterval(() => setNow(Date.now()), 1000);
        return () => window.clearInterval(id);
    }, []);

    const expiry = useMemo<{ ms: number; expired: boolean }>(() => {
        const exp = claims?.exp;
        if (!exp) return { ms: 0, expired: true };
        const ms = exp * 1000;
        return { ms, expired: ms <= now };
    }, [claims, now]);

    return (
        <div className="animate-fade-in space-y-6">
            <div>
                <h2 className="text-lg font-semibold text-fg">Profile</h2>
                <p className="text-xs text-fg-subtle">
                    Your account, token claims, and bound devices.
                </p>
            </div>

            {error && (
                <div className="rounded-xl glass bg-[rgb(var(--accent-danger)/0.1)] px-4 py-3 text-xs text-[rgb(var(--accent-danger))]">
                    {error}
                </div>
            )}

            <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
                {/* User card */}
                <Card>
                    <CardHeader>
                        <CardTitle className="flex items-center gap-2">
                            <UserIcon size={16} /> Account
                        </CardTitle>
                        <CardDescription>From GET /api/v1/user/me</CardDescription>
                    </CardHeader>
                    <CardContent className="space-y-3">
                        <Row label="User ID" value={`#${user?.id ?? "—"}`} />
                        <Row label="Name" value={user?.name ?? "—"} />
                        <Row
                            label="Role"
                            value={
                                user?.is_admin ? (
                                    <Badge variant="success">
                                        <ShieldCheck size={11} /> administrator
                                    </Badge>
                                ) : (
                                    <Badge variant="outline">user</Badge>
                                )
                            }
                        />
                    </CardContent>
                </Card>

                {/* JWT card */}
                <Card>
                    <CardHeader>
                        <CardTitle className="flex items-center gap-2">
                            <KeyRound size={16} /> Token
                        </CardTitle>
                        <CardDescription>
                            Decoded locally from the HS256 JWT payload.
                        </CardDescription>
                    </CardHeader>
                    <CardContent className="space-y-3">
                        <Row label="User ID" value={`#${claims?.user_id ?? "—"}`} />
                        <Row label="Device ID" value={`#${claims?.device_id ?? "—"}`} />
                        <Row
                            label="Issued at"
                            value={
                                claims?.iat
                                    ? formatDateTime(new Date(claims.iat * 1000).toISOString())
                                    : "—"
                            }
                        />
                        <Row
                            label="Expires at"
                            value={
                                claims?.exp
                                    ? formatDateTime(new Date(claims.exp * 1000).toISOString())
                                    : "—"
                            }
                        />
                        <Row
                            label="Countdown"
                            value={
                                <span
                                    className={cn(
                                        "inline-flex items-center gap-1 font-mono",
                                        expiry.expired ? "text-[rgb(var(--accent-danger))]" : "text-[rgb(var(--accent-success))]",
                                    )}
                                >
                                    <Clock size={12} />
                                    {claims?.exp
                                        ? expiry.expired
                                            ? "expired"
                                            : formatCountdown(expiry.ms)
                                        : "—"}
                                </span>
                            }
                        />
                        {token && (
                            <div className="pt-1">
                                <div className="mb-1 text-xs uppercase tracking-wider text-fg-subtle">
                                    Raw token
                                </div>
                                <div className="glass-subtle rounded-xl max-h-24 overflow-y-auto p-2 font-mono text-[11px] break-all text-fg-subtle">
                                    {token}
                                </div>
                            </div>
                        )}
                    </CardContent>
                </Card>
            </div>

            {/* Bound devices */}
            <Card>
                <CardHeader>
                    <CardTitle className="flex items-center gap-2">
                        <HardDrive size={16} /> Bound devices
                    </CardTitle>
                    <CardDescription>
                        Devices owned by you ({devices.length}).
                    </CardDescription>
                </CardHeader>
                <CardContent className="p-0">
                    <div className="overflow-x-auto">
                        <table className="w-full text-left text-sm">
                            <thead className="glass-subtle border-b border-[rgb(var(--border)/0.3)] text-xs uppercase tracking-wider text-fg-subtle">
                                <tr>
                                    <th className="px-4 py-3 font-medium">Name</th>
                                    <th className="px-4 py-3 font-medium">ID</th>
                                    <th className="px-4 py-3 font-medium">Last login</th>
                                    <th className="px-4 py-3 font-medium">Created</th>
                                    <th className="px-4 py-3 font-medium">State</th>
                                </tr>
                            </thead>
                            <tbody className="divide-[rgb(var(--border)/0.3)]">
                                {devices.length === 0 && (
                                    <tr>
                                        <td
                                            colSpan={5}
                                            className="px-4 py-10 text-center text-fg-subtle"
                                        >
                                            No devices bound to your account.
                                        </td>
                                    </tr>
                                )}
                                {devices.map((d) => (
                                    <tr key={d.id} className="hover:bg-[rgb(var(--bg-subtle)/0.3)]">
                                        <td className="px-4 py-3 font-medium text-fg">
                                            {d.device_name}
                                        </td>
                                        <td className="px-4 py-3 text-fg-muted">#{d.id}</td>
                                        <td className="px-4 py-3 text-fg-muted">
                                            {formatDateTime(d.last_login_at)}
                                        </td>
                                        <td className="px-4 py-3 text-fg-muted">
                                            {formatDateTime(d.created_at)}
                                        </td>
                                        <td className="px-4 py-3">
                                            {d.revoked_at ? (
                                                <Badge variant="danger">revoked</Badge>
                                            ) : (
                                                <Badge variant="success">active</Badge>
                                            )}
                                        </td>
                                    </tr>
                                ))}
                            </tbody>
                        </table>
                    </div>
                </CardContent>
            </Card>
        </div>
    );
}

/** Label / value row used inside the profile cards. */
function Row({
    label,
    value,
}: {
    label: string;
    value: React.ReactNode;
}) {
    return (
        <div className="flex items-center justify-between gap-4 border-b border-[rgb(var(--border)/0.3)] pb-2 last:border-0 last:pb-0">
            <span className="text-xs uppercase tracking-wider text-fg-subtle">
                {label}
            </span>
            <span className="text-right text-sm text-fg">{value}</span>
        </div>
    );
}

// Re-export JwtClaims so consumers can import from this module if desired.
export type { JwtClaims };