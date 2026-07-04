import { useEffect, useState } from "react";
import {
    Activity,
    Clock,
    Radio,
    Server,
    Wifi,
    WifiOff,
    RefreshCw,
} from "lucide-react";
import { getSystemStatus } from "@/api/system";
import { ApiError } from "@/api/client";
import { formatUptime } from "@/lib/utils";
import type { SystemStatus } from "@/types";
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

const accentClasses: Record<StatCardProps["accent"], string> = {
    sky: "from-sky-500/20 to-sky-500/0 text-sky-300 ring-sky-500/30",
    emerald:
        "from-emerald-500/20 to-emerald-500/0 text-emerald-300 ring-emerald-500/30",
    amber: "from-amber-500/20 to-amber-500/0 text-amber-300 ring-amber-500/30",
    violet:
        "from-violet-500/20 to-violet-500/0 text-violet-300 ring-violet-500/30",
};

function StatCard({ label, value, icon, accent, hint }: StatCardProps) {
    return (
        <Card className="relative overflow-hidden">
            <div
                className={`pointer-events-none absolute inset-0 bg-gradient-to-br ${accentClasses[accent].split(" ")[0]} ${accentClasses[accent].split(" ")[1]}`}
            />
            <CardHeader className="relative flex-row items-center justify-between pb-2">
                <CardTitle className="text-xs uppercase tracking-wider text-slate-400">
                    {label}
                </CardTitle>
                <div
                    className={`flex h-9 w-9 items-center justify-center rounded-lg bg-slate-900/60 ring-1 ring-inset ${accentClasses[accent].split(" ").slice(2).join(" ")}`}
                >
                    {icon}
                </div>
            </CardHeader>
            <CardContent className="relative">
                <div className="text-3xl font-semibold tracking-tight text-slate-50">
                    {value}
                </div>
                {hint && <div className="mt-2 text-xs text-slate-400">{hint}</div>}
            </CardContent>
        </Card>
    );
}

/**
 * Dashboard: 4 stat cards refreshed every 5s via GET /system/status.
 */
export default function Dashboard() {
    const [status, setStatus] = useState<SystemStatus | null>(null);
    const [error, setError] = useState<string | null>(null);
    const [loading, setLoading] = useState(true);

    useEffect(() => {
        let cancelled = false;
        let timer: number | null = null;

        async function tick() {
            try {
                const s = await getSystemStatus();
                if (!cancelled) {
                    setStatus(s);
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

    const onlineCount = status?.online_device_count ?? 0;
    const uptime = status ? formatUptime(status.uptime_seconds) : "—";

    return (
        <div className="space-y-6">
            <div className="flex items-center justify-between">
                <div>
                    <h2 className="text-lg font-semibold text-slate-100">Dashboard</h2>
                    <p className="text-xs text-slate-500">
                        Live system metrics, refreshed every 5 seconds.
                    </p>
                </div>
                {loading ? (
                    <RefreshCw size={16} className="animate-spin text-slate-500" />
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
                <div className="rounded-md border border-rose-500/40 bg-rose-500/10 px-4 py-3 text-sm text-rose-300">
                    {error}
                </div>
            )}

            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
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
                    value={status ? (status.mqtt_connected ? "Connected" : "Down") : "—"}
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
                    value={status ? String(status.ws_clients) : "—"}
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

            <Card>
                <CardHeader>
                    <CardTitle className="flex items-center gap-2">
                        <Server size={16} /> System snapshot
                    </CardTitle>
                    <CardDescription>
                        Raw payload from <code className="font-mono">/api/v1/system/status</code>.
                    </CardDescription>
                </CardHeader>
                <CardContent>
                    <pre className="overflow-x-auto rounded-md bg-slate-950/80 p-4 text-xs leading-relaxed text-slate-300">
                        {status ? JSON.stringify(status, null, 2) : "// no data yet"}
                    </pre>
                </CardContent>
            </Card>
        </div>
    );
}
