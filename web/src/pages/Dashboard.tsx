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
    Zap,
    Gauge,
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
 * access via this hostname is an IPv6 direct connection.
 */
const NAS_DDNS_DOMAIN = "nas.feiyemomo.top";

/**
 * Determine the current dashboard connection path.
 */
function detectApiPath(): "lan" | "ipv6" | "remote" {
    if (typeof window === "undefined") return "remote";
    const h = window.location.hostname;
    if (h === "localhost" || h === "127.0.0.1") return "lan";
    if (/^192\.168\./.test(h)) return "lan";
    if (/^10\./.test(h)) return "lan";
    if (/^172\.(1[6-9]|2[0-9]|3[01])\./.test(h)) return "lan";
    if (h === NAS_DDNS_DOMAIN) return "ipv6";
    if (/^\[[0-9a-f:]+\]$/i.test(h)) return "ipv6";
    return "remote";
}

/**
 * Dashboard: stat cards + live detection alerts with enhanced UI.
 */
export default function Dashboard() {
    const navigate = useNavigate();
    const { isAdmin } = useAuth();
    const [clientIPv6, setClientIPv6] = useState<boolean | null>(null);
    const [liveAlert, setLiveAlert] = useState<CameraAlert | null>(null);
    const [selectedAlert, setSelectedAlert] = useState<CameraAlert | null>(null);
    const {
        data: systemLogs,
        loading: _logsLoading,
        mutate: mutateLogs,
    } = useCachedFetch<SystemLog[]>(
        "home.dashboard.logs",
        () => listSystemLogs(3, 0).then(res => res.logs ?? []),
        { refetchMs: 10000 },
    );

    const ws = useWebSocket(true);

    const lastMessageRef = useRef(ws.lastMessage);
    lastMessageRef.current = ws.lastMessage;
    const systemLogsRef = useRef(systemLogs);
    systemLogsRef.current = systemLogs;
    const liveAlertRef = useRef(liveAlert);
    liveAlertRef.current = liveAlert;

    const forceRefreshRef = useRef(true);

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

    useEffect(() => {
        if (statusData) {
            prefetchOnIdle("home.cameras.list", () => listCameras(), 2000);
            prefetchOnIdle("home.network.status", () =>
                getNetworkStatus(false).then(res => [res, null] as any),
                3000
            );
        }
    }, [statusData, prefetchOnIdle]);

    useEffect(() => {
        const msg = lastMessageRef.current;
        if (!msg) return;
        if (msg.type !== "event") return;

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
            return;
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
                window.setTimeout(() => {
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

    useEffect(() => {
        checkClientIPv6().then((v) => setClientIPv6(v));
    }, []);

    const onlineCount = status?.online_device_count ?? 0;
    const uptime = status ? formatUptime(status.uptime_seconds) : "—";
    const apiPath = detectApiPath();

    const currentQuality = (() => {
        if (apiPath === "lan" || apiPath === "ipv6") return 5;
        if (netStatus?.strategy === "ipv6_direct" && clientIPv6 === false) return 3;
        if (apiPath === "remote") return Math.min(netStatus?.quality ?? 0, 3);
        return netStatus?.quality ?? 0;
    })();

    const qualityLabel = currentQuality >= 5 ? "极佳" : currentQuality >= 4 ? "优秀" : currentQuality >= 3 ? "良好" : currentQuality >= 2 ? "一般" : "较差";
    const latencyHint = netStatus?.latency_ms ? `${netStatus.latency_ms}ms` : apiPath === "lan" ? "~10ms" : apiPath === "ipv6" ? "~30ms" : "—";

    return (
        <div className="space-y-6">
            {/* Page header */}
            <div className="animate-fade-in glass-subtle relative overflow-hidden rounded-2xl px-5 py-4 card-lift">
                <div className="pointer-events-none absolute inset-y-0 left-0 w-1 bg-gradient-to-b from-[rgb(var(--accent-warm)/0.9)] via-[rgb(var(--accent-primary)/0.6)] to-transparent" />
                <div className="pointer-events-none absolute -right-10 -top-10 h-32 w-32 rounded-full bg-[rgb(var(--accent-warm)/0.1)] blur-3xl" />
                <div className="pointer-events-none absolute -left-5 -bottom-5 h-24 w-24 rounded-full bg-[rgb(var(--accent-primary)/0.08)] blur-2xl" />
                <div className="relative flex items-center justify-between">
                    <div>
                        <h2 className="text-xl font-semibold tracking-tight text-fg">
                            仪表盘
                        </h2>
                        <p className="mt-0.5 text-xs text-fg-muted">
                            实时系统指标，每 10 秒自动刷新
                        </p>
                    </div>
                    {loading ? (
                        <RefreshCw size={18} className="animate-spin text-fg-subtle" />
                    ) : (
                        <Badge variant={error ? "danger" : "success"} className="gap-1.5 shadow-sm">
                            <span
                                className={`pulse-dot inline-block h-2 w-2 rounded-full ${error ? "bg-[rgb(var(--accent-danger))]" : "bg-[rgb(var(--accent-success))]"}`}
                            />
                            {error ? "连接异常" : "实时连接"}
                        </Badge>
                    )}
                </div>
            </div>

            {error && (
                <div className="animate-fade-in glass rounded-2xl bg-gradient-to-r from-[rgb(var(--accent-danger)/0.12)] to-[rgb(var(--accent-danger)/0.04)] px-4 py-3 text-sm text-[rgb(var(--accent-danger))] border border-[rgb(var(--accent-danger)/0.2)]">
                    {error}
                </div>
            )}

            {/* Weather card */}
            <WeatherCard />

            {/* Live detection alert banner */}
            {liveAlert && (
                <LiveAlertBanner
                    alert={liveAlert}
                    onViewSnapshot={setSelectedAlert}
                />
            )}

            {/* Stat cards grid */}
            <div className="animate-fade-in grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4 stagger-children">
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
                                    className={`pulse-dot inline-block h-2 w-2 rounded-full ${status.mqtt_connected ? "bg-[rgb(var(--accent-success))]" : "bg-[rgb(var(--accent-danger))]"}`}
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

            {/* Network + Logs row */}
            <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
                {/* Network quality card - enhanced */}
                <Card
                    className="animate-fade-in group relative overflow-hidden glass glass-glow card-lift cursor-pointer transition-all"
                    onClick={() => navigate("/network")}
                >
                    <div className="pointer-events-none absolute inset-0 bg-gradient-to-br from-[rgb(var(--accent-info)/0.08)] via-transparent to-[rgb(var(--accent-primary)/0.05)]" />
                    <div className="pointer-events-none absolute top-0 left-0 right-0 h-px bg-gradient-to-r from-transparent via-[rgb(var(--accent-info)/0.3)] to-transparent" />

                    <CardHeader className="relative flex-row items-center justify-between pb-3">
                        <CardTitle className="flex items-center gap-2 text-[11px] font-medium tracking-wider uppercase text-fg-muted">
                            <Globe size={15} /> 网络质量
                        </CardTitle>
                        <div className="flex items-center gap-2">
                            <Badge variant="outline" className="gap-1 text-[10px] glass-subtle">
                                <NetworkIcon size={10} />
                                <span
                                    className={`pulse-dot inline-block h-1.5 w-1.5 rounded-full ${
                                        apiPath === "lan"
                                            ? "bg-[rgb(var(--accent-success))]"
                                            : apiPath === "ipv6"
                                                ? "bg-[rgb(var(--accent-info))]"
                                                : "bg-[rgb(var(--accent-warm))]"
                                    }`}
                                />
                                {apiPath === "lan" ? "局域网" : apiPath === "ipv6" ? "IPv6 直连" : "远程隧道"}
                            </Badge>
                        </div>
                    </CardHeader>
                    <CardContent className="relative">
                        <div className="flex items-center justify-between">
                            <div className="flex items-center gap-4">
                                {/* Quality gauge */}
                                <div className="relative flex h-14 w-14 items-center justify-center">
                                    <div className="absolute inset-0 rounded-full bg-gradient-to-br from-[rgb(var(--accent-warm)/0.15)] to-[rgb(var(--accent-primary)/0.1)]" />
                                    <Gauge size={28} className={`${
                                        currentQuality >= 4 ? "text-[rgb(var(--accent-success))]" :
                                        currentQuality >= 3 ? "text-[rgb(var(--accent-warm))]" :
                                        "text-[rgb(var(--accent-danger))]"
                                    }`} />
                                </div>
                                <div>
                                    <div className="flex items-center gap-2">
                                        <span className={`text-2xl font-semibold ${
                                            currentQuality >= 4 ? "text-[rgb(var(--accent-success))]" :
                                            currentQuality >= 3 ? "text-[rgb(var(--accent-warm))]" :
                                            "text-[rgb(var(--accent-danger))]"
                                        }`}>
                                            {qualityLabel}
                                        </span>
                                        <Zap size={14} className="text-fg-subtle" />
                                    </div>
                                    <div className="mt-1 flex items-center gap-2">
                                        <div className="flex items-center gap-0.5">
                                            {[1, 2, 3, 4, 5].map((n) => (
                                                <Star
                                                    key={n}
                                                    size={13}
                                                    className={
                                                        n <= currentQuality
                                                            ? "fill-[rgb(var(--accent-warm))] text-[rgb(var(--accent-warm))] drop-shadow-[0_0_4px_rgb(var(--accent-warm)/0.4)]"
                                                            : "fill-none text-fg-subtle/40"
                                                    }
                                                />
                                            ))}
                                        </div>
                                        <span className="text-xs text-fg-muted">{latencyHint}</span>
                                    </div>
                                </div>
                            </div>
                            <div className="text-right text-xs text-fg-subtle group-hover:text-fg-muted transition-colors">
                                点击查看详情 →
                            </div>
                        </div>
                    </CardContent>
                </Card>

                {/* System logs card - enhanced */}
                {isAdmin && (
                    <Card
                        className="animate-fade-in group relative overflow-hidden glass glass-glow card-lift cursor-pointer transition-all"
                        onClick={() => navigate("/logs")}
                    >
                        <div className="pointer-events-none absolute inset-0 bg-gradient-to-br from-[rgb(var(--accent-warm)/0.06)] via-transparent to-[rgb(var(--accent-primary)/0.04)]" />
                        <div className="pointer-events-none absolute top-0 left-0 right-0 h-px bg-gradient-to-r from-transparent via-[rgb(var(--border)/0.4)] to-transparent" />

                        <CardHeader className="relative flex-row items-center justify-between pb-3">
                            <CardTitle className="flex items-center gap-2 text-[11px] font-medium tracking-wider uppercase text-fg-muted">
                                <Activity size={15} /> 系统日志
                            </CardTitle>
                            <Badge variant="outline" className="text-[10px] glass-subtle">
                                {systemLogs?.length ?? 0} 条最新
                            </Badge>
                        </CardHeader>
                        <CardContent className="relative">
                            {!systemLogs || systemLogs.length === 0 ? (
                                <div className="py-3 text-center text-xs text-fg-subtle">暂无日志记录</div>
                            ) : (
                                <ul className="space-y-2">
                                    {(systemLogs ?? []).slice(0, 3).map((log, idx) => (
                                        <li
                                            key={log.id}
                                            className="flex items-center gap-2 text-xs glass-subtle rounded-lg px-2.5 py-2 transition-all group-hover:bg-[rgb(var(--bg-subtle)/0.3)]"
                                            style={{ animationDelay: `${idx * 80}ms` }}
                                        >
                                            <Badge
                                                variant={
                                                    log.level === "critical" ? "danger"
                                                        : log.level === "normal" ? "info"
                                                            : "outline"
                                                }
                                                className="shrink-0 text-[9px] shadow-sm"
                                            >
                                                {log.level === "critical" ? "严重"
                                                    : log.level === "normal" ? "普通"
                                                        : "信息"}
                                            </Badge>
                                            <span className="min-w-0 flex-1 truncate text-fg-muted group-hover:text-fg transition-colors">
                                                {log.message || log.event_type}
                                            </span>
                                        </li>
                                    ))}
                                </ul>
                            )}
                            <div className="mt-2 text-right text-[10px] text-fg-subtle group-hover:text-fg-muted transition-colors">
                                查看全部日志 →
                            </div>
                        </CardContent>
                    </Card>
                )}
            </div>

            {/* Detection alerts list */}
            <Card className="animate-fade-in relative overflow-hidden glass glass-glow">
                <div className="pointer-events-none absolute top-0 left-0 right-0 h-px bg-gradient-to-r from-transparent via-[rgb(var(--border)/0.4)] to-transparent" />

                <CardHeader className="relative flex-row items-center justify-between pb-3">
                    <CardTitle className="flex items-center gap-2 text-[11px] font-medium tracking-wider uppercase text-fg-muted">
                        <Activity size={15} /> 检测报警
                    </CardTitle>
                    <div className="flex items-center gap-2">
                        <Badge variant="outline" className="text-[10px] glass-subtle">
                            {alerts.length} 条记录
                        </Badge>
                        <button
                            type="button"
                            onClick={refetchAlerts}
                            disabled={alertsLoading}
                            className="inline-flex items-center gap-1 rounded-lg px-2 py-1 text-[10px] text-fg-muted transition-all glass-subtle hover:glass hover:text-fg disabled:opacity-50"
                        >
                            <RefreshCw size={10} className={alertsLoading ? "animate-spin" : ""} />
                            刷新
                        </button>
                    </div>
                </CardHeader>
                <CardContent className="relative">
                    {alertsLoading && alerts.length === 0 ? (
                        <div className="flex items-center justify-center py-8 text-xs text-fg-muted">
                            <RefreshCw size={14} className="mr-2 animate-spin" />
                            加载报警记录中…
                        </div>
                    ) : alerts.length === 0 ? (
                        <div className="py-8 text-center">
                            <div className="mx-auto mb-3 flex h-12 w-12 items-center justify-center rounded-full glass-subtle">
                                <Activity size={20} className="text-fg-subtle" />
                            </div>
                            <p className="text-xs text-fg-subtle">暂无检测报警</p>
                            <p className="mt-1 text-[10px] text-fg-subtle/70">系统运行正常，未检测到异常活动</p>
                        </div>
                    ) : (
                        <ul className="max-h-80 space-y-2 overflow-y-auto pr-1 stagger-children">
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

            {/* Raw JSON snapshot */}
            <SystemSnapshot status={status} />
        </div>
    );
}
