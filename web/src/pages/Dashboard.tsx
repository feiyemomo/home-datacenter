import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
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
    ExternalLink,
    Sun,
    Cloud,
    CloudSun,
    CloudRain,
    CloudSnow,
    CloudFog,
    CloudLightning,
    CloudDrizzle,
    Droplets,
    Wind,
    MapPin,
    Network as NetworkIcon,
} from "lucide-react";
import { getSystemStatus } from "@/api/system";
import { getNetworkStatus, checkClientIPv6 } from "@/api/network";
import { listAlerts, alertSnapshotUrl, alertThumbnailUrl, type CameraAlert } from "@/api/camera";
import { getWeather, wmoToIcon, type WeatherResponse } from "@/api/weather";
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
 * WeatherCard — top-of-dashboard weather summary, mirrors the
 * Android DashboardFragment's weather card. Calls GET /api/v1/weather
 * (proxied wttr.in j1) and renders current temp, "feels like",
 * WMO-code icon, location label, humidity + wind.
 *
 * The card degrades gracefully: if wttr.in is unreachable (the
 * backend's 5-min cache also helps), we show a compact "weather
 * unavailable" badge instead of a blank card.
 */
function WeatherCard() {
    const [weather, setWeather] = useState<WeatherResponse | null>(null);
    const [error, setError] = useState<string | null>(null);
    const [loading, setLoading] = useState(true);

    useEffect(() => {
        let cancelled = false;
        let timer: number | null = null;

        async function load() {
            try {
                const w = await getWeather();
                if (cancelled) return;
                setWeather(w);
                setError(null);
            } catch (err) {
                if (cancelled) return;
                setError(
                    err instanceof ApiError
                        ? err.message
                        : err instanceof Error
                            ? err.message
                            : "weather unavailable",
                );
            } finally {
                if (!cancelled) setLoading(false);
                // wttr.in updates ~every 10 min; the backend caches
                // for 5 min. We re-fetch every 10 min to refresh the
                // card without hammering the proxy.
                if (!cancelled) {
                    timer = window.setTimeout(load, 10 * 60 * 1000);
                }
            }
        }
        load();
        return () => {
            cancelled = true;
            if (timer !== null) window.clearTimeout(timer);
        };
    }, []);

    const cond = weather?.current_condition?.[0];
    const area = weather?.nearest_area?.[0];

    const code = cond?.weatherCode ? parseInt(cond.weatherCode, 10) : NaN;
    // wttr.in's WMO codes match the open-meteo table for 0..99, but
    // they also emit 113/116/119/122/143/176/200/227/230/248/260/263/266/281/284/293/296/299/302/305/308/311/314/317/320/323/326/329/332/335/338/350/353/356/359/362/365/368/371/374/377/386/389/392/395
    // (legacy Codes). We normalize the common ones to the WMO table.
    const wmo = useMemo(() => {
        if (Number.isNaN(code)) return { icon: "cloud", label: "—" };
        // Map wttr.in's 1xx codes down to WMO equivalents
        const m: Record<number, number> = {
            113: 0, 116: 2, 119: 3, 122: 3, 143: 45, 176: 51,
            200: 95, 227: 71, 230: 75, 248: 45, 260: 45,
            263: 51, 266: 53, 281: 53, 284: 55, 293: 61, 296: 61,
            299: 63, 302: 63, 305: 65, 308: 67, 311: 65, 314: 67,
            317: 67, 320: 71, 323: 71, 326: 73, 329: 75, 332: 75,
            335: 75, 338: 75, 350: 51, 353: 61, 356: 65, 359: 67,
            362: 71, 365: 73, 368: 71, 371: 73, 374: 75, 377: 75,
            386: 95, 389: 95, 392: 95, 395: 95,
        };
        const normalized = m[code] ?? code;
        return wmoToIcon(normalized);
    }, [code]);

    // Map icon name → lucide component
    const Icon = ({
        sun: Sun,
        cloud: Cloud,
        "cloud-sun": CloudSun,
        "cloud-rain": CloudRain,
        "cloud-snow": CloudSnow,
        "cloud-fog": CloudFog,
        "cloud-lightning": CloudLightning,
        "cloud-drizzle": CloudDrizzle,
    } as Record<string, typeof Sun>)[wmo.icon] ?? Cloud;

    const tempC = cond?.temp_C ? parseInt(cond.temp_C, 10) : null;
    const feelsC = cond?.FeelsLikeC ? parseInt(cond.FeelsLikeC, 10) : null;
    const humidity = cond?.humidity ? parseInt(cond.humidity, 10) : null;
    const windKmph = cond?.windspeedKmph ? parseInt(cond.windspeedKmph, 10) : null;
    const windDir = cond?.winddir16Point;
    const areaName = area?.areaName?.[0]?.value;
    const region = area?.region?.[0]?.value;

    return (
        <Card className="animate-fade-in relative overflow-hidden">
            <div className="pointer-events-none absolute inset-0 bg-gradient-to-br from-[rgb(var(--accent-warm)/0.15)] via-[rgb(var(--accent-primary)/0.05)] to-transparent" />
            <CardHeader className="relative flex-row items-center justify-between pb-2">
                <CardTitle className="flex items-center gap-2 text-xs uppercase tracking-wider text-fg-muted">
                    <Icon size={16} className="text-[rgb(var(--accent-warm))]" /> 天气
                </CardTitle>
                {areaName && (
                    <Badge variant="outline" className="text-[10px] gap-1">
                        <MapPin size={10} />
                        {areaName}{region ? ` · ${region}` : ""}
                    </Badge>
                )}
            </CardHeader>
            <CardContent className="relative">
                {loading ? (
                    <div className="flex items-center gap-2 text-fg-muted">
                        <RefreshCw size={14} className="animate-spin" />
                        <span className="text-xs">加载中…</span>
                    </div>
                ) : error || !cond ? (
                    <div className="flex items-center gap-2 text-fg-muted">
                        <Cloud size={20} className="opacity-50" />
                        <span className="text-xs">
                            {error ?? "weather unavailable"}
                        </span>
                    </div>
                ) : (
                    <div className="flex items-center gap-4">
                        {/* Big icon + temp */}
                        <div className="flex items-center gap-3">
                            <div className="flex h-14 w-14 items-center justify-center rounded-2xl bg-[rgb(var(--accent-warm)/0.15)] ring-1 ring-inset ring-[rgb(var(--accent-warm)/0.3)]">
                                <Icon size={28} className="text-[rgb(var(--accent-warm))]" />
                            </div>
                            <div>
                                <div className="flex items-baseline gap-1">
                                    <span className="text-3xl font-semibold tracking-tight text-fg">
                                        {tempC ?? "—"}
                                    </span>
                                    <span className="text-sm text-fg-muted">°C</span>
                                </div>
                                <span className="text-xs text-fg-muted">{wmo.label}</span>
                            </div>
                        </div>
                        {/* Secondary stats */}
                        <div className="ml-auto grid grid-cols-3 gap-3 text-xs">
                            <div className="flex flex-col items-center">
                                <span className="text-fg-subtle">体感</span>
                                <span className="font-medium text-fg">
                                    {feelsC ?? "—"}°
                                </span>
                            </div>
                            <div className="flex flex-col items-center">
                                <Droplets size={12} className="text-fg-subtle" />
                                <span className="font-medium text-fg">
                                    {humidity ?? "—"}%
                                </span>
                            </div>
                            <div className="flex flex-col items-center">
                                <Wind size={12} className="text-fg-subtle" />
                                <span className="font-medium text-fg">
                                    {windKmph ?? "—"}
                                    <span className="text-fg-subtle"> km/h</span>
                                    {windDir ? ` ${windDir}` : ""}
                                </span>
                            </div>
                        </div>
                    </div>
                )}
            </CardContent>
        </Card>
    );
}

/**
 * Determine whether the current dashboard origin is the LAN path
 * (192.168.x.x or 10.x or 172.16-31.x) or the remote path
 * (Cloudflare Tunnel via api.feiyemomo.top).
 *
 * Used by the LAN/Remote chip on the Network Quality card. The
 * Android app uses BaseUrlResolver to actually probe both paths;
 * on web we're bound to the current origin so we just classify it.
 */
function detectApiPath(): "lan" | "remote" {
    if (typeof window === "undefined") return "remote";
    const h = window.location.hostname;
    if (h === "localhost" || h === "127.0.0.1") return "lan";
    if (/^192\.168\./.test(h)) return "lan";
    if (/^10\./.test(h)) return "lan";
    if (/^172\.(1[6-9]|2[0-9]|3[01])\./.test(h)) return "lan";
    return "remote";
}

/**
 * Dashboard: stat cards + live detection alerts.
 */
export default function Dashboard() {
    const navigate = useNavigate();
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
    const apiPath = useMemo(() => detectApiPath(), []);

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

            {/* Weather card — mirrors Android DashboardFragment's weather card.
             * Calls the existing /api/v1/weather proxy (5-min server cache). */}
            <WeatherCard />

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
                        {liveAlert.has_snapshot && (
                            <button
                                type="button"
                                onClick={() => setSelectedAlert(liveAlert)}
                                className="relative h-12 w-16 shrink-0 overflow-hidden rounded-lg bg-black/30 transition-transform hover:scale-105"
                                title="点击查看截图"
                            >
                                <img
                                    src={alertThumbnailUrl(liveAlert.id)}
                                    alt={liveAlert.label}
                                    className="h-full w-full object-cover"
                                />
                                <span className="absolute inset-0 flex items-center justify-center bg-black/0 opacity-0 transition-all hover:bg-black/30 hover:opacity-100">
                                    <Eye size={12} className="text-white" />
                                </span>
                            </button>
                        )}
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
                    <div className="flex items-center gap-2">
                        {/* LAN/Remote path chip — mirrors Android's
                         * network-quality card chip. Green dot = LAN
                         * (low latency, ~10ms TTFB), amber dot = remote
                         * (Cloudflare Tunnel, ~1.4s+ TTFB). */}
                        <Badge
                            variant="outline"
                            className="text-[10px] gap-1"
                            title={
                                apiPath === "lan"
                                    ? "Dashboard is loaded from the LAN path (low latency)"
                                    : "Dashboard is loaded via Cloudflare Tunnel (remote)"
                            }
                        >
                            <NetworkIcon size={10} />
                            <span
                                className={`inline-block h-1.5 w-1.5 rounded-full ${
                                    apiPath === "lan" ? "bg-emerald-400" : "bg-amber-400"
                                }`}
                            />
                            {apiPath === "lan" ? "LAN" : "Remote"}
                        </Badge>
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
                    </div>
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
                                    {/* Thumbnail / icon — click opens full-resolution modal */}
                                    {alert.has_snapshot ? (
                                        <button
                                            type="button"
                                            onClick={() => setSelectedAlert(alert)}
                                            className="relative h-16 w-24 shrink-0 overflow-hidden rounded-lg bg-black/30"
                                            title="点击查看大图"
                                        >
                                            <img
                                                src={alertThumbnailUrl(alert.id)}
                                                alt={alert.label}
                                                className="h-full w-full object-cover"
                                                loading="lazy"
                                            />
                                            <span className="absolute inset-0 flex items-center justify-center bg-black/0 opacity-0 transition-all group-hover:bg-black/30 group-hover:opacity-100">
                                                <Eye size={14} className="text-white" />
                                            </span>
                                        </button>
                                    ) : (
                                        <div className="flex h-16 w-24 shrink-0 items-center justify-center rounded-lg bg-[rgb(var(--accent-warm)/0.1)]">
                                            <AlertTriangle size={18} className="text-[rgb(var(--accent-warm)/0.5)]" />
                                        </div>
                                    )}

                                    {/* Info — click navigates to the camera page with
                                     * timestamp + mode=recording so the LiveVideo
                                     * auto-plays the matching 60s recording bucket
                                     * and seeks to the alert's timestamp. This
                                     * mirrors Android's "查看录像" → recording
                                     * playback with pendingAlertSeekMs flow. */}
                                    <button
                                        type="button"
                                        onClick={() => {
                                            const search = new URLSearchParams();
                                            if (alert.camera_id) {
                                                search.set("camera", String(alert.camera_id));
                                            }
                                            if (alert.start_time) {
                                                search.set("time", String(alert.start_time));
                                            }
                                            search.set("mode", "recording");
                                            navigate(`/cameras?${search.toString()}`);
                                        }}
                                        className="min-w-0 flex-1 cursor-pointer py-0.5 text-left"
                                        title="查看录像 — 跳转到对应时间点的录像回放"
                                    >
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
                                    </button>

                                    {/* Jump icon — visible on hover */}
                                    <div className="flex shrink-0 items-center self-center text-fg-subtle opacity-0 transition-opacity group-hover:opacity-100">
                                        <ExternalLink size={14} />
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
