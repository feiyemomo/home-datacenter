import { useCallback, useEffect, useState } from "react";
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
    AlertTriangle,
    Eye,
    X,
} from "lucide-react";
import { getSystemStatus } from "@/api/system";
import { getNetworkStatus, checkClientIPv6 } from "@/api/network";
import { listAlerts, alertSnapshotUrl, type CameraAlert } from "@/api/camera";
import { useWebSocket } from "@/hooks/useWebSocket";
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

/** Format a detection label for display. */
function formatLabel(label: string): string {
    const known: Record<string, string> = {
        person: "人员",
        car: "车辆",
        truck: "卡车",
        bus: "公交车",
        bicycle: "自行车",
        motorcycle: "摩托车",
        dog: "狗",
        cat: "猫",
        bird: "鸟",
    };
    return known[label] ?? label;
}

/** Format a confidence (0..1) as a percentage string. */
function formatConfidence(c: number): string {
    if (!Number.isFinite(c) || c < 0) return "—";
    return `${Math.round(c * 100)}%`;
}

/** Format a unix timestamp as a locale time string. */
function formatAlertTime(ts: number): string {
    if (!ts) return "—";
    return new Date(ts * 1000).toLocaleString();
}

/**
 * Dashboard: stat cards + live detection alerts.
 */
export default function Dashboard() {
    const [status, setStatus] = useState<SystemStatus | null>(null);
    const [netStatus, setNetStatus] = useState<NetworkStatus | null>(null);
    const [clientIPv6, setClientIPv6] = useState<boolean | null>(null);
    const [error, setError] = useState<string | null>(null);
    const [loading, setLoading] = useState(true);

    // Alerts state
    const [alerts, setAlerts] = useState<CameraAlert[]>([]);
    const [alertsLoading, setAlertsLoading] = useState(false);
    const [liveAlert, setLiveAlert] = useState<CameraAlert | null>(null);
    // Alert selected for full-resolution snapshot viewing (modal).
    const [selectedAlert, setSelectedAlert] = useState<CameraAlert | null>(null);

    // WebSocket for real-time alerts
    const ws = useWebSocket(true);

    // Load historical alerts
    const loadAlerts = useCallback(async () => {
        setAlertsLoading(true);
        try {
            const res = await listAlerts(20);
            setAlerts(res.alerts);
        } catch {
            // Non-fatal: alerts are supplementary info
        } finally {
            setAlertsLoading(false);
        }
    }, []);

    useEffect(() => {
        void loadAlerts();
        // Refresh alerts every 30s
        const id = window.setInterval(loadAlerts, 30000);
        return () => window.clearInterval(id);
    }, [loadAlerts]);

    // Listen for real-time camera.motion events via WebSocket
    useEffect(() => {
        if (!ws.lastMessage) return;
        if (ws.lastMessage.type !== "event") return;
        if (ws.lastMessage.topic !== "camera.motion") return;

        try {
            const p = ws.lastMessage.payload as Record<string, unknown>;
            if (p && p.type === "detection") {
                setLiveAlert({
                    id: String(p.ts ?? Date.now()),
                    camera_slug: String(p.camera_slug ?? ""),
                    camera_id: typeof p.camera_id === "number" ? p.camera_id : undefined,
                    camera_name: typeof p.camera_name === "string" ? p.camera_name : undefined,
                    label: String(p.label ?? "unknown"),
                    confidence: typeof p.confidence === "number" ? p.confidence : 0,
                    start_time: typeof p.ts === "number" ? p.ts : Date.now() / 1000,
                    end_time: 0,
                    zones: Array.isArray(p.zones) ? p.zones : [],
                    has_clip: typeof p.has_clip === "boolean" ? p.has_clip : false,
                    has_snapshot: typeof p.has_snapshot === "boolean" ? p.has_snapshot : false,
                });
                // Auto-dismiss after 8 seconds
                window.setTimeout(() => setLiveAlert(null), 8000);
            }
        } catch {
            // Ignore malformed events
        }
    }, [ws.lastMessage]);

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

    // Close the snapshot modal on Escape key.
    useEffect(() => {
        if (!selectedAlert) return;
        const onKey = (e: KeyboardEvent) => {
            if (e.key === "Escape") setSelectedAlert(null);
        };
        window.addEventListener("keydown", onKey);
        return () => window.removeEventListener("keydown", onKey);
    }, [selectedAlert]);

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

            {/* Live detection alert banner */}
            {liveAlert && (
                <div className="animate-fade-in glass rounded-2xl bg-[rgb(var(--accent-warm)/0.15)] border border-[rgb(var(--accent-warm)/0.3)] px-4 py-3">
                    <div className="flex items-center gap-3">
                        <div className="flex h-8 w-8 items-center justify-center rounded-full bg-[rgb(var(--accent-warm)/0.2)]">
                            <AlertTriangle size={16} className="text-[rgb(var(--accent-warm))]" />
                        </div>
                        <div className="min-w-0 flex-1">
                            <div className="flex items-center gap-2">
                                <span className="text-sm font-semibold text-fg">
                                    检测到 {formatLabel(liveAlert.label)}
                                </span>
                                <Badge variant="info" className="text-[9px]">
                                    {formatConfidence(liveAlert.confidence)}
                                </Badge>
                            </div>
                            <p className="text-xs text-fg-muted">
                                {liveAlert.camera_name ?? liveAlert.camera_slug}
                                {liveAlert.zones && liveAlert.zones.length > 0
                                    ? ` · ${liveAlert.zones.join(", ")}`
                                    : ""}
                            </p>
                        </div>
                        <span className="text-[10px] text-fg-subtle">实时</span>
                    </div>
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

            {/* Detection alerts list */}
            <Card className="animate-fade-in">
                <CardHeader className="flex-row items-center justify-between pb-2">
                    <CardTitle className="flex items-center gap-2 text-xs uppercase tracking-wider text-fg-muted">
                        <Eye size={16} /> 检测报警
                    </CardTitle>
                    <div className="flex items-center gap-2">
                        <Badge variant="outline" className="text-[10px]">
                            {alerts.length} 条
                        </Badge>
                        <button
                            type="button"
                            onClick={loadAlerts}
                            disabled={alertsLoading}
                            className="inline-flex items-center gap-1 text-[10px] text-fg-muted transition-colors hover:text-fg disabled:opacity-50"
                        >
                            <RefreshCw size={10} className={alertsLoading ? "animate-spin" : ""} />
                            刷新
                        </button>
                    </div>
                </CardHeader>
                <CardContent>
                    {alertsLoading && alerts.length === 0 ? (
                        <div className="flex items-center justify-center py-6 text-xs text-fg-muted">
                            <RefreshCw size={12} className="mr-1.5 animate-spin" />
                            加载中…
                        </div>
                    ) : alerts.length === 0 ? (
                        <div className="py-6 text-center text-xs text-fg-subtle">
                            暂无检测报警
                        </div>
                    ) : (
                        <ul className="max-h-80 space-y-2 overflow-y-auto pr-0.5">
                            {alerts.map((alert) => (
                                <li
                                    key={alert.id}
                                    className="group flex gap-3 glass-subtle rounded-xl p-2 transition-colors hover:bg-[rgb(var(--bg-subtle)/0.3)]"
                                >
                                    {/* Thumbnail / icon */}
                                    {alert.thumbnail ? (
                                        <button
                                            type="button"
                                            onClick={() => setSelectedAlert(alert)}
                                            className="relative h-16 w-24 shrink-0 overflow-hidden rounded-lg bg-black/30"
                                            title="点击查看大图"
                                        >
                                            <img
                                                src={`data:image/jpeg;base64,${alert.thumbnail}`}
                                                alt={alert.label}
                                                className="h-full w-full object-cover"
                                                loading="lazy"
                                            />
                                            <span className="absolute inset-0 flex items-center justify-center bg-black/0 opacity-0 transition-all group-hover:bg-black/30 group-hover:opacity-100">
                                                <Eye size={14} className="text-white" />
                                            </span>
                                        </button>
                                    ) : alert.has_snapshot ? (
                                        <button
                                            type="button"
                                            onClick={() => setSelectedAlert(alert)}
                                            className="flex h-16 w-24 shrink-0 items-center justify-center rounded-lg bg-[rgb(var(--accent-warm)/0.1)] text-[rgb(var(--accent-warm)/0.5)] transition-colors hover:bg-[rgb(var(--accent-warm)/0.2)]"
                                            title="点击查看快照"
                                        >
                                            <Eye size={18} />
                                        </button>
                                    ) : (
                                        <div className="flex h-16 w-24 shrink-0 items-center justify-center rounded-lg bg-[rgb(var(--accent-warm)/0.1)]">
                                            <AlertTriangle size={18} className="text-[rgb(var(--accent-warm)/0.5)]" />
                                        </div>
                                    )}

                                    {/* Info */}
                                    <div className="min-w-0 flex-1 py-0.5">
                                        <div className="flex flex-wrap items-center gap-1.5">
                                            <span className="text-sm font-medium text-fg">
                                                {formatLabel(alert.label)}
                                            </span>
                                            <Badge variant="info" className="text-[8px] px-1 py-0">
                                                {formatConfidence(alert.confidence)}
                                            </Badge>
                                            {alert.has_clip && (
                                                <Badge variant="success" className="text-[8px] px-1 py-0">
                                                    录像
                                                </Badge>
                                            )}
                                            {alert.has_snapshot && (
                                                <Badge variant="outline" className="text-[8px] px-1 py-0">
                                                    截图
                                                </Badge>
                                            )}
                                        </div>
                                        <p className="mt-1 truncate text-[11px] text-fg-muted">
                                            {alert.camera_name ?? alert.camera_slug}
                                            {alert.zones && alert.zones.length > 0
                                                ? ` · ${alert.zones.join(", ")}`
                                                : ""}
                                        </p>
                                        <p className="mt-0.5 text-[10px] text-fg-subtle">
                                            {formatAlertTime(alert.start_time)}
                                        </p>
                                    </div>
                                </li>
                            ))}
                        </ul>
                    )}
                </CardContent>
            </Card>

            {/* Full-resolution snapshot modal */}
            {selectedAlert && (
                <div
                    className="fixed inset-0 z-50 flex items-center justify-center bg-black/70 p-4 backdrop-blur-sm animate-fade-in"
                    onClick={() => setSelectedAlert(null)}
                >
                    <div
                        className="relative max-h-[90vh] max-w-3xl overflow-hidden rounded-2xl glass shadow-2xl"
                        onClick={(e) => e.stopPropagation()}
                    >
                        {/* Header */}
                        <div className="flex items-center justify-between gap-3 border-b border-[rgb(var(--border)/0.5)] px-4 py-3">
                            <div className="min-w-0">
                                <div className="flex items-center gap-2">
                                    <AlertTriangle size={14} className="text-[rgb(var(--accent-warm))]" />
                                    <span className="text-sm font-semibold text-fg">
                                        {formatLabel(selectedAlert.label)}
                                    </span>
                                    <Badge variant="info" className="text-[9px]">
                                        {formatConfidence(selectedAlert.confidence)}
                                    </Badge>
                                </div>
                                <p className="mt-0.5 truncate text-[11px] text-fg-muted">
                                    {selectedAlert.camera_name ?? selectedAlert.camera_slug}
                                    {selectedAlert.zones && selectedAlert.zones.length > 0
                                        ? ` · ${selectedAlert.zones.join(", ")}`
                                        : ""}
                                    {" · "}
                                    {formatAlertTime(selectedAlert.start_time)}
                                </p>
                            </div>
                            <button
                                type="button"
                                onClick={() => setSelectedAlert(null)}
                                className="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg text-fg-muted transition-colors hover:bg-[rgb(var(--bg-subtle)/0.5)] hover:text-fg"
                            >
                                <X size={16} />
                            </button>
                        </div>
                        {/* Image */}
                        <div className="flex max-h-[75vh] items-center justify-center bg-black/40 p-2">
                            <img
                                src={alertSnapshotUrl(selectedAlert.id)}
                                alt={`${selectedAlert.label} snapshot`}
                                className="max-h-[72vh] max-w-full rounded-lg object-contain"
                            />
                        </div>
                    </div>
                </div>
            )}

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
