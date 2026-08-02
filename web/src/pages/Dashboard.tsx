import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
    Activity,
    Clock,
    Radio,
    Wifi,
    WifiOff,
    RefreshCw,
    Globe,
    Star,
    Network as NetworkIcon,
} from "lucide-react";
import { getSystemStatus, listSystemLogs } from "@/api/system";
import { getNetworkStatus, checkClientIPv6 } from "@/api/network";
import { listAlerts, listCameras, type CameraAlert } from "@/api/camera";
import { useAuth } from "@/hooks/useAuth";
import { useWebSocket } from "@/hooks/useWebSocket";
import { useCachedFetch } from "@/hooks/useCachedFetch";
import { usePrefetch } from "@/hooks/usePrefetch";
import { formatUptime } from "@/lib/utils";
import type { SystemStatus, NetworkStatus, SystemLog } from "@/types";
import {
    Card,
    CardContent,
    CardHeader,
    CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { StatCard } from "@/components/dashboard/StatCard";
import { WeatherCard } from "@/components/dashboard/WeatherCard";
import LiveAlertBanner from "@/components/dashboard/LiveAlertBanner";
import AlertItem from "@/components/dashboard/AlertItem";
import AlertSnapshotModal from "@/components/dashboard/AlertSnapshotModal";
import SystemSnapshot from "@/components/dashboard/SystemSnapshot";

/**
 * The NAS's DDNS domain — pure-AAAA record (no A record), so any
 * access via this hostname is an IPv6 direct connection. The ISP
 * DHCPv6-PD prefix rotations are tracked by the DDNS provider, so
 * this constant never needs updating.
 */
const NAS_DDNS_DOMAIN = "nas.feiyemomo.top";

/**
 * Determine the current dashboard connection path.
 *
 * Returns:
 *  - "lan"       — direct LAN access (192.168.x.x, 10.x, 172.16-31.x)
 *  - "ipv6"      — IPv6 direct connection (DDNS domain or IPv6 literal,
 *                  bypasses Cloudflare Tunnel)
 *  - "remote"    — Cloudflare Tunnel or other remote path
 *
 * Used by the Network Quality card to show the current path chip.
 * The Android app uses BaseUrlResolver to actually probe paths;
 * on web we classify by current origin.
 */
function detectApiPath(): "lan" | "ipv6" | "remote" {
    if (typeof window === "undefined") return "remote";
    const h = window.location.hostname;
    if (h === "localhost" || h === "127.0.0.1") return "lan";
    if (/^192\.168\./.test(h)) return "lan";
    if (/^10\./.test(h)) return "lan";
    if (/^172\.(1[6-9]|2[0-9]|3[01])\./.test(h)) return "lan";
    // DDNS domain (nas.feiyemomo.top) — pure-AAAA record, so access
    // via this hostname is an IPv6 direct connection.
    if (h === NAS_DDNS_DOMAIN) return "ipv6";
    // Detect IPv6 literal addresses (wrapped in brackets like [::1] or [2001:db8::1])
    if (/^\[[0-9a-f:]+\]$/i.test(h)) return "ipv6";
    return "remote";
}

/**
 * Dashboard: stat cards + live detection alerts.
 */
export default function Dashboard() {
    const navigate = useNavigate();
    const { isAdmin } = useAuth();
    const [clientIPv6, setClientIPv6] = useState<boolean | null>(null);
    const [liveAlert, setLiveAlert] = useState<CameraAlert | null>(null);
    // Alert selected for full-resolution snapshot viewing (modal).
    const [selectedAlert, setSelectedAlert] = useState<CameraAlert | null>(null);
    // System logs for the dashboard log card, with silent background refresh.
    const {
        data: systemLogs,
        loading: _logsLoading,
        mutate: mutateLogs,
    } = useCachedFetch<SystemLog[]>(
        "home.dashboard.logs",
        () => listSystemLogs(3, 0).then(res => res.logs ?? []),
        { refetchMs: 10000 },
    );

    // WebSocket for real-time events
    const ws = useWebSocket(true);

    // Use refs to track latest values without triggering effect re-creation
    const lastMessageRef = useRef(ws.lastMessage);
    lastMessageRef.current = ws.lastMessage;
    const systemLogsRef = useRef(systemLogs);
    systemLogsRef.current = systemLogs;
    const liveAlertRef = useRef(liveAlert);
    liveAlertRef.current = liveAlert;

    // Force a fresh backend network detection on the first load only.
    // The backend caches network status for 60s; without this, the
    // displayed quality rating may be up to 60s stale on initial load.
    // Subsequent polling refreshes use the cached backend response to
    // avoid hammering STUN servers.
    const forceRefreshRef = useRef(true);

    // System + network status: cached fetch with 10s silent background
    // refresh. The cache lets us paint the last-known values instantly
    // when the dashboard remounts (e.g. navigating back from another
    // page), instead of showing a loading spinner for the first 5s.
    const {
        data: statusData,
        loading: statusLoading,
        error: statusError,
    } = useCachedFetch<[SystemStatus, NetworkStatus]>(
        "home.dashboard.status",
        () => {
            const refresh = forceRefreshRef.current;
            forceRefreshRef.current = false;
            return Promise.all([getSystemStatus(), getNetworkStatus(refresh)]);
        },
        { refetchMs: 10000 },
    );
    const status = statusData?.[0] ?? null;
    const netStatus = statusData?.[1] ?? null;
    const error = statusError ? statusError.message : null;
    const loading = statusLoading && status === null;

    // Historical alerts: cached fetch with 30s silent background
    // refresh. The cache preserves the recent alert list across
    // remounts so the operator doesn't see an empty list flash
    // when navigating away and back.
    const {
        data: alertsData,
        loading: alertsLoading,
        refetch: refetchAlerts,
    } = useCachedFetch<{ alerts: CameraAlert[] }>(
        "home.dashboard.alerts",
        () => listAlerts(20),
        { refetchMs: 30000 },
    );
    const alerts = alertsData?.alerts ?? [];

    const { prefetchOnIdle } = usePrefetch();

    // Preload other pages' data after dashboard loads
    useEffect(() => {
        if (statusData) {
            // Preload cameras list using the prefetch hook
            prefetchOnIdle("home.cameras.list", () => listCameras(), 2000);

            // Preload network status
            prefetchOnIdle("home.network.status", () =>
                getNetworkStatus(false).then(res => [res, null] as any),
                3000
            );
        }
    }, [statusData, prefetchOnIdle]);

    // Listen for real-time events via WebSocket — effect depends only
    // on stable callbacks, avoiding re-creation on every lastMessage change.
    useEffect(() => {
        const msg = lastMessageRef.current;
        if (!msg) return;
        if (msg.type !== "event") return;

        // Handle system.log events for real-time log updates
        if (msg.topic === "system.log") {
            try {
                const p = msg.payload as Record<string, unknown>;
                if (p && p.event_type) {
                    const newLog: SystemLog = {
                        id: typeof p.id === "number" ? p.id : Date.now(),
                        ts: typeof p.ts === "number" ? p.ts : Math.floor(Date.now() / 1000),
                        event_type: String(p.event_type ?? ""),
                        level: String(p.level ?? "normal"),
                        message: String(p.message ?? ""),
                        payload: typeof p.payload === "string" ? p.payload : "",
                        created_at: "",
                    };
                    const current = systemLogsRef.current ?? [];
                    const updated = [newLog, ...current].slice(0, 3);
                    mutateLogs(updated);
                }
            } catch {
                // Ignore malformed events
            }
            return; // Don't fall through to camera.motion check
        }

        if (msg.topic !== "camera.motion") return;

        try {
            const p = msg.payload as Record<string, unknown>;
            if (p && p.type === "detection") {
                setLiveAlert({
                    id: typeof p.event_id === "string" ? p.event_id : String(p.ts ?? Date.now()),
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
                window.setTimeout(() => {
                    // Only dismiss if the alert hasn't been replaced
                    setLiveAlert((current) => {
                        if (current && current.id === (typeof p.event_id === "string" ? p.event_id : String(p.ts ?? Date.now()))) {
                            return null;
                        }
                        return current;
                    });
                }, 8000);
            }
        } catch {
            // Ignore malformed events
        }
    }, [mutateLogs]);

    // Client-side IPv6 check — runs once on mount. Client IPv6 doesn't
    // change frequently; re-checking every 5s would be wasteful and
    // could cause CORS noise in the console.
    useEffect(() => {
        checkClientIPv6().then((v) => setClientIPv6(v));
    }, []);

    const onlineCount = status?.online_device_count ?? 0;
    const uptime = status ? formatUptime(status.uptime_seconds) : "—";
    const apiPath = detectApiPath();

    // Star quality display for the network quality card
    const currentQuality = (() => {
        if (apiPath === "lan" || apiPath === "ipv6") return 5;
        if (netStatus?.strategy === "ipv6_direct" && clientIPv6 === false) return 3;
        if (apiPath === "remote") return Math.min(netStatus?.quality ?? 0, 3);
        return netStatus?.quality ?? 0;
    })();

    return (
        <div className="space-y-6">
            {/* Page header — liquid glass banner with warm accent glow.
             * The gradient strip on the left anchors the title
             * visually and ties into the ambient orb background. */}
            <div className="animate-fade-in glass-subtle relative overflow-hidden rounded-2xl px-5 py-4">
                <div className="pointer-events-none absolute inset-y-0 left-0 w-1 bg-gradient-to-b from-[rgb(var(--accent-warm)/0.8)] via-[rgb(var(--accent-primary)/0.5)] to-transparent" />
                <div className="pointer-events-none absolute -right-8 -top-8 h-24 w-24 rounded-full bg-[rgb(var(--accent-warm)/0.08)] blur-2xl" />
                <div className="relative flex items-center justify-between">
                    <div>
                        <h2 className="text-lg font-semibold tracking-tight text-fg">
                            仪表盘
                        </h2>
                        <p className="mt-0.5 text-xs text-fg-muted">
                            实时系统指标，每 10 秒刷新。
                        </p>
                    </div>
                    {loading ? (
                        <RefreshCw size={16} className="animate-spin text-fg-subtle" />
                    ) : (
                        <Badge variant={error ? "danger" : "success"}>
                            <span
                                className={`mr-1 inline-block h-1.5 w-1.5 rounded-full ${error ? "bg-[rgb(var(--accent-danger))]" : "bg-[rgb(var(--accent-success))]"}`}
                            />
                            {error ? "异常" : "实时"}
                        </Badge>
                    )}
                </div>
            </div>

            {error && (
                <div className="animate-fade-in glass rounded-2xl bg-[rgb(var(--accent-danger)/0.1)] px-4 py-3 text-sm text-[rgb(var(--accent-danger))]">
                    {error}
                </div>
            )}

            {/* Weather card — mirrors Android DashboardFragment's weather card. */}
            <WeatherCard />

            {/* Live detection alert banner */}
            {liveAlert && (
                <LiveAlertBanner
                    alert={liveAlert}
                    onViewSnapshot={setSelectedAlert}
                />
            )}

            <div className="animate-fade-in grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
                <StatCard
                    label="在线设备"
                    value={String(onlineCount)}
                    icon={<Activity size={18} />}
                    accent="emerald"
                    hint={
                        status ? (
                            <span>
                                {(status.online_device_ids?.length ?? 0)} 个 ID 在线
                            </span>
                        ) : undefined
                    }
                />
                <StatCard
                    label="MQTT 状态"
                    value={status ? (status.mqtt_connected ? "已连接" : "中断") : "—"}
                    icon={
                        status?.mqtt_connected ? <Wifi size={18} /> : <WifiOff size={18} />
                    }
                    accent={status?.mqtt_connected ? "emerald" : "amber"}
                    hint={
                        status ? (
                            <span className="inline-flex items-center gap-1.5">
                                <span
                                    className={`inline-block h-2 w-2 rounded-full ${status.mqtt_connected ? "bg-[rgb(var(--accent-success))]" : "bg-[rgb(var(--accent-danger))]"}`}
                                />
                                {status.mqtt_connected ? "代理可达" : "代理离线"}
                            </span>
                        ) : undefined
                    }
                />
                <StatCard
                    label="WS 客户端"
                    value={status ? String(status.ws_clients) : "—"}
                    icon={<Radio size={18} />}
                    accent="sky"
                    hint="已连接的应用客户端"
                />
                <StatCard
                    label="运行时长"
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

            <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
                {/* Network quality summary - compact */}
                <Card
                    className="animate-fade-in cursor-pointer transition-colors hover:bg-[rgb(var(--bg-subtle)/0.2)]"
                    onClick={() => navigate("/network")}
                >
                    <CardHeader className="flex-row items-center justify-between pb-2">
                        <CardTitle className="flex items-center gap-2 text-xs tracking-wider text-fg-muted">
                            <Globe size={16} /> 网络质量
                        </CardTitle>
                        <div className="flex items-center gap-2">
                            {/* Path chip */}
                            <Badge variant="outline" className="text-[10px] gap-1">
                                <NetworkIcon size={10} />
                                <span
                                    className={`inline-block h-1.5 w-1.5 rounded-full ${
                                        apiPath === "lan"
                                            ? "bg-[rgb(var(--accent-success))]"
                                            : apiPath === "ipv6"
                                                ? "bg-[rgb(var(--accent-info))]"
                                                : "bg-[rgb(var(--accent-warm))]"
                                    }`}
                                />
                                {apiPath === "lan" ? "局域网" : apiPath === "ipv6" ? "IPv6 直连" : "远程"}
                            </Badge>
                            {/* Stars */}
                            <div className="flex items-center gap-0.5">
                                {[1, 2, 3, 4, 5].map((n) => (
                                    <Star
                                        key={n}
                                        size={14}
                                        className={
                                            n <= currentQuality
                                                ? "fill-[rgb(var(--accent-warm))] text-[rgb(var(--accent-warm))]"
                                                : "fill-none text-fg-subtle"
                                        }
                                    />
                                ))}
                            </div>
                        </div>
                    </CardHeader>
                </Card>

                {isAdmin && (
                    <Card
                        className="animate-fade-in cursor-pointer transition-colors hover:bg-[rgb(var(--bg-subtle)/0.2)]"
                        onClick={() => navigate("/logs")}
                    >
                        <CardHeader className="flex-row items-center justify-between pb-2">
                            <CardTitle className="flex items-center gap-2 text-xs tracking-wider text-fg-muted">
                                <Activity size={16} /> 系统日志
                            </CardTitle>
                            <Badge variant="outline" className="text-[10px]">
                                {systemLogs?.length ?? 0} 条
                            </Badge>
                        </CardHeader>
                        <CardContent>
                            {!systemLogs || systemLogs.length === 0 ? (
                                <div className="text-xs text-fg-subtle">暂无日志</div>
                            ) : (
                                <ul className="space-y-1">
                                    {(systemLogs ?? []).slice(0, 3).map((log) => (
                                        <li key={log.id} className="flex items-center gap-2 text-xs">
                                            <Badge
                                                variant={
                                                    log.level === "critical" ? "danger"
                                                        : log.level === "normal" ? "info"
                                                            : "outline"
                                                }
                                                className="text-[9px] shrink-0"
                                            >
                                                {log.level === "critical" ? "严重"
                                                    : log.level === "normal" ? "普通"
                                                        : "信息"}
                                            </Badge>
                                            <span className="min-w-0 flex-1 truncate text-fg-muted">
                                                {log.message || log.event_type}
                                            </span>
                                        </li>
                                    ))}
                                </ul>
                            )}
                        </CardContent>
                    </Card>
                )}
            </div>

            {/* Detection alerts list */}
            <Card className="animate-fade-in">
                <CardHeader className="flex-row items-center justify-between pb-2">
                    <CardTitle className="flex items-center gap-2 text-xs tracking-wider text-fg-muted">
                        <Activity size={16} /> 检测报警
                    </CardTitle>
                    <div className="flex items-center gap-2">
                        <Badge variant="outline" className="text-[10px]">
                            {alerts.length} 条
                        </Badge>
                        <button
                            type="button"
                            onClick={refetchAlerts}
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
                                <AlertItem
                                    key={alert.id}
                                    alert={alert}
                                    onViewSnapshot={setSelectedAlert}
                                    onNavigateToCamera={() => {}}
                                />
                            ))}
                        </ul>
                    )}
                </CardContent>
            </Card>

            {/* Full-resolution snapshot modal */}
            {selectedAlert && (
                <AlertSnapshotModal
                    alert={selectedAlert}
                    onClose={() => setSelectedAlert(null)}
                />
            )}

            {/* Raw JSON snapshot — collapsed by default. */}
            <SystemSnapshot status={status} />
        </div>
    );
}