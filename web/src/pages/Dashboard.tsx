import { useEffect, useState } from "react";
import {
    Activity,
    Clock,
    Radio,
    Server,
    Wifi,
    WifiOff,
    RefreshCw,
    Globe,
    Star,
    Smartphone,
    ArrowUp,
} from "lucide-react";
import { getSystemStatus } from "@/api/system";
import { getNetworkStatus, checkClientIPv6 } from "@/api/network";
import { ApiError } from "@/api/client";
import { formatUptime } from "@/lib/utils";
import type { SystemStatus, NetworkStatus } from "@/types";
import {
    Card,
    CardContent,
    CardDescription,
    CardHeader,
    CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";

interface StatCardProps {
    label: string;
    value: string;
    icon: React.ReactNode;
    accent: "sky" | "emerald" | "amber" | "violet";
    hint?: React.ReactNode;
}

const accentClasses: Record<StatCardProps["accent"], { gradient: string; iconBg: string; text: string }> = {
    sky: {
        gradient: "from-[rgb(var(--accent-info)/0.15)] to-[rgb(var(--accent-info)/0)]",
        iconBg: "bg-[rgb(var(--accent-info)/0.15)] ring-[rgb(var(--accent-info)/0.3)] text-[rgb(var(--accent-info))]",
        text: "text-[rgb(var(--accent-info))]",
    },
    emerald: {
        gradient: "from-[rgb(var(--accent-success)/0.15)] to-[rgb(var(--accent-success)/0)]",
        iconBg: "bg-[rgb(var(--accent-success)/0.15)] ring-[rgb(var(--accent-success)/0.3)] text-[rgb(var(--accent-success))]",
        text: "text-[rgb(var(--accent-success))]",
    },
    amber: {
        gradient: "from-[rgb(var(--accent-warm)/0.15)] to-[rgb(var(--accent-warm)/0)]",
        iconBg: "bg-[rgb(var(--accent-warm)/0.15)] ring-[rgb(var(--accent-warm)/0.3)] text-[rgb(var(--accent-warm))]",
        text: "text-[rgb(var(--accent-warm))]",
    },
    violet: {
        gradient: "from-[rgb(var(--accent-primary)/0.15)] to-[rgb(var(--accent-warm)/0)]",
        iconBg: "bg-[rgb(var(--accent-primary)/0.15)] ring-[rgb(var(--accent-primary)/0.3)] text-[rgb(var(--accent-primary))]",
        text: "text-[rgb(var(--accent-primary))]",
    },
};

function StatCard({ label, value, icon, accent, hint }: StatCardProps) {
    return (
        <Card className="relative overflow-hidden">
            <div
                className={`pointer-events-none absolute inset-0 bg-gradient-to-br ${accentClasses[accent].gradient}`}
            />
            <CardHeader className="relative flex-row items-center justify-between pb-2">
                <CardTitle className="text-xs uppercase tracking-wider text-fg-muted">
                    {label}
                </CardTitle>
                <div
                    className={`flex h-9 w-9 items-center justify-center rounded-xl ring-1 ring-inset ${accentClasses[accent].iconBg}`}
                >
                    {icon}
                </div>
            </CardHeader>
            <CardContent className="relative">
                <div className="text-3xl font-semibold tracking-tight text-fg">
                    {value}
                </div>
                {hint && <div className="mt-2 text-xs text-fg-muted">{hint}</div>}
            </CardContent>
        </Card>
    );
}

/**
 * Dashboard: 4 stat cards refreshed every 5s via GET /system/status.
 */
export default function Dashboard() {
    const [status, setStatus] = useState<SystemStatus | null>(null);
    const [netStatus, setNetStatus] = useState<NetworkStatus | null>(null);
    const [clientIPv6, setClientIPv6] = useState<boolean | null>(null);
    const [error, setError] = useState<string | null>(null);
    const [loading, setLoading] = useState(true);

    useEffect(() => {
        let cancelled = false;
        let timer: number | null = null;

        async function tick() {
            try {
                const [s, n] = await Promise.all([
                    getSystemStatus(),
                    getNetworkStatus(),
                ]);
                if (!cancelled) {
                    setStatus(s);
                    setNetStatus(n);
                    setError(null);
                }
            } catch (err) {
                if (!cancelled) {
                    setError(
                        err instanceof ApiError
                            ? err.message
                            : err instanceof Error
                                ? err.message
                                : "failed to load status",
                    );
                }
            } finally {
                if (!cancelled) setLoading(false);
                if (!cancelled) {
                    timer = window.setTimeout(tick, 5000);
                }
            }
        }

        tick();
        return () => {
            cancelled = true;
            if (timer !== null) window.clearTimeout(timer);
        };
    }, []);

    // Client-side IPv6 check — runs once on mount (client IPv6 doesn't
    // change frequently; re-checking every 5s would be wasteful and
    // could cause CORS noise in the console).
    useEffect(() => {
        checkClientIPv6().then((v) => setClientIPv6(v));
    }, []);

    const onlineCount = status?.online_device_count ?? 0;
    const uptime = status ? formatUptime(status.uptime_seconds) : "—";

    return (
        <div className="space-y-6">
            <div className="animate-fade-in flex items-center justify-between">
                <div>
                    <h2 className="text-lg font-semibold text-fg">
                        Dashboard
                    </h2>
                    <p className="text-xs text-fg-muted">
                        Live system metrics, refreshed every 5 seconds.
                    </p>
                </div>
                {loading ? (
                    <RefreshCw size={16} className="animate-spin text-fg-subtle" />
                ) : (
                    <Badge variant={error ? "danger" : "success"}>
                        <span
                            className={`mr-1 inline-block h-1.5 w-1.5 rounded-full ${error ? "bg-rose-400" : "bg-emerald-400"}`}
                        />
                        {error ? "error" : "live"}
                    </Badge>
                )}
            </div>

            {error && (
                <div className="animate-fade-in glass rounded-2xl bg-[rgb(var(--accent-danger)/0.1)] px-4 py-3 text-sm text-[rgb(var(--accent-danger))]">
                    {error}
                </div>
            )}

            <div className="animate-fade-in grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
                <StatCard
                    label="Online Devices"
                    value={String(onlineCount)}
                    icon={<Activity size={18} />}
                    accent="emerald"
                    hint={
                        status ? (
                            <span>
                                {(status.online_device_ids?.length ?? 0)} ID
                                {(status.online_device_ids?.length ?? 0) === 1 ? "" : "s"} online
                            </span>
                        ) : undefined
                    }
                />
                <StatCard
                    label="MQTT Status"
                    value={status ? (status.mqtt_connected ? "Connected" : "Down") : "—"
                    }
                    icon={
                        status?.mqtt_connected ? <Wifi size={18} /> : <WifiOff size={18} />
                    }
                    accent={status?.mqtt_connected ? "emerald" : "amber"}
                    hint={
                        status ? (
                            <span className="inline-flex items-center gap-1.5">
                                <span
                                    className={`inline-block h-2 w-2 rounded-full ${status.mqtt_connected ? "bg-emerald-400" : "bg-rose-400"}`}
                                />
                                {status.mqtt_connected ? "broker reachable" : "broker offline"}
                            </span>
                        ) : undefined
                    }
                />
                <StatCard
                    label="WS Clients"
                    value={status ? String(status.ws_clients) : "—"
                    }
                    icon={<Radio size={18} />}
                    accent="sky"
                    hint="connected app clients"
                />
                <StatCard
                    label="Uptime"
                    value={uptime}
                    icon={<Clock size={18} />}
                    accent="violet"
                    hint={
                        status ? (
                            <span className="font-mono text-[11px]">
                                {status.server_time}
                            </span>
                        ) : undefined
                    }
                />
            </div>

            {/* Network quality summary */}
            <Card className="animate-fade-in">
                <CardHeader className="flex-row items-center justify-between pb-2">
                    <CardTitle className="flex items-center gap-2 text-xs uppercase tracking-wider text-fg-muted">
                        <Globe size={16} /> Network Quality
                    </CardTitle>
                    {netStatus && (
                        <div className="flex items-center gap-0.5">
                            {[1, 2, 3, 4, 5].map((n) => (
                                <Star
                                    key={n}
                                    size={14}
                                    className={
                                        n <= netStatus.quality
                                            ? "fill-[rgb(var(--accent-warm))] text-[rgb(var(--accent-warm))]"
                                            : "fill-none text-fg-subtle"
                                    }
                                />
                            ))}
                        </div>
                    )}
                </CardHeader>
                <CardContent>
                    <div className="flex items-center gap-4">
                        <div className="min-w-0 flex-1">
                            {/* Connection model: Relay → Upgrade */}
                            <div className="flex items-center gap-2">
                                <span className="text-lg font-semibold text-fg">
                                    Relay
                                </span>
                                {netStatus && netStatus.strategy !== netStatus.initial && (
                                    <>
                                        <ArrowUp size={14} className="text-[rgb(var(--accent-info))]" />
                                        <span className="text-sm text-[rgb(var(--accent-info))]">
                                            upgradable to{" "}
                                            {netStatus.strategy === "ipv6_direct"
                                                ? "IPv6 Direct"
                                                : netStatus.strategy === "p2p"
                                                    ? "P2P"
                                                    : ""}
                                        </span>
                                    </>
                                )}
                            </div>
                            {/* Capability indicators: Server vs Client */}
                            <div className="mt-1.5 flex flex-wrap items-center gap-3 text-xs text-fg-muted">
                                <span className="inline-flex items-center gap-1" title="Server IPv6">
                                    <Server size={11} />
                                    <span
                                        className={`inline-block h-2 w-2 rounded-full ${netStatus?.ipv6?.reachable ? "bg-emerald-400" : "bg-rose-400"}`}
                                    />
                                    IPv6
                                </span>
                                <span className="inline-flex items-center gap-1" title="Your device IPv6">
                                    <Smartphone size={11} />
                                    <span
                                        className={`inline-block h-2 w-2 rounded-full ${clientIPv6 === null
                                                ? "bg-slate-400"
                                                : clientIPv6
                                                    ? "bg-emerald-400"
                                                    : "bg-rose-400"
                                            }`}
                                    />
                                    You
                                </span>
                                <span className="inline-flex items-center gap-1" title="Server P2P">
                                    <span
                                        className={`inline-block h-2 w-2 rounded-full ${netStatus?.p2p?.supported ? "bg-emerald-400" : "bg-rose-400"}`}
                                    />
                                    P2P
                                </span>
                                <span className="inline-flex items-center gap-1" title="Relay">
                                    <span
                                        className={`inline-block h-2 w-2 rounded-full ${netStatus?.relay?.available ? "bg-emerald-400" : "bg-rose-400"}`}
                                    />
                                    Relay
                                </span>
                            </div>
                        </div>
                    </div>
                </CardContent>
            </Card>

            <Card className="animate-fade-in">
                <CardHeader>
                    <CardTitle className="flex items-center gap-2">
                        <Server size={16} /> System snapshot
                    </CardTitle>
                    <CardDescription>
                        Raw payload from <code className="font-mono">/api/v1/system/status</code>.
                    </CardDescription>
                </CardHeader>
                <CardContent>
                    <pre className="glass-subtle overflow-x-auto rounded-2xl p-4 text-xs leading-relaxed text-fg">
                        {status ? JSON.stringify(status, null, 2) : "// no data yet"}
                    </pre>
                </CardContent>
            </Card>
        </div>
    );
}
