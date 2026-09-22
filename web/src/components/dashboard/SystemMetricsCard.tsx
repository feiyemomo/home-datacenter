import { useState } from "react";
import {
    Cpu,
    HardDrive,
    Database,
    Trash2,
    CheckCircle2,
    AlertTriangle,
    Loader2,
    Activity,
    Layers,
} from "lucide-react";
import { cleanTranscodeCache, type CleanCacheResponse } from "@/api/system";
import type { SystemMetrics } from "@/types";
import {
    Card,
    CardContent,
    CardHeader,
    CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

interface SystemMetricsCardProps {
    metrics?: SystemMetrics;
    onCleaned?: () => void;
}

function formatBytes(bytes?: number): string {
    if (!bytes || bytes <= 0) return "0 B";
    if (bytes >= 1024 * 1024 * 1024 * 1024) {
        return (bytes / (1024 * 1024 * 1024 * 1024)).toFixed(1) + " TiB";
    }
    if (bytes >= 1024 * 1024 * 1024) {
        return (bytes / (1024 * 1024 * 1024)).toFixed(1) + " GiB";
    }
    if (bytes >= 1024 * 1024) {
        return (bytes / (1024 * 1024)).toFixed(1) + " MiB";
    }
    if (bytes >= 1024) {
        return (bytes / 1024).toFixed(1) + " KiB";
    }
    return bytes + " B";
}

export default function SystemMetricsCard({ metrics, onCleaned }: SystemMetricsCardProps) {
    const [cleaning, setCleaning] = useState(false);
    const [cleanResult, setCleanResult] = useState<string | null>(null);
    const [cleanError, setCleanError] = useState<string | null>(null);

    const handleClean = async () => {
        setCleaning(true);
        setCleanResult(null);
        setCleanError(null);
        try {
            const res: CleanCacheResponse = await cleanTranscodeCache();
            setCleanResult(
                `清理成功：已释放 ${formatBytes(res.reclaimed_bytes)}，移除 ${res.deleted_files} 个转码切片`
            );
            onCleaned?.();
        } catch (err: any) {
            setCleanError(err?.message || "清理转码缓存失败");
        } finally {
            setCleaning(false);
        }
    };

    if (!metrics) {
        return null;
    }

    const cpuPct = metrics.cpu?.percent ?? 0;
    const memPct = metrics.memory?.used_percent ?? 0;
    const diskPct = metrics.disk?.used_percent ?? 0;
    const recPct = metrics.recordings?.quota_percent ?? 0;

    return (
        <Card className="animate-fade-in border shadow-sm">
            <CardHeader className="flex flex-row items-center justify-between pb-3">
                <div className="space-y-0.5">
                    <CardTitle className="text-base font-semibold flex items-center gap-2 text-fg">
                        <Activity className="text-[rgb(var(--accent-primary))]" size={18} />
                        数据中心资源与存储看板
                    </CardTitle>
                    <p className="text-xs text-fg-muted">
                        宿主机硬件负载、400 GiB 监控录像池配额与转码切片缓存
                    </p>
                </div>
                <Button
                    variant="outline"
                    size="sm"
                    onClick={handleClean}
                    disabled={cleaning}
                    className="h-8 gap-1.5 text-xs text-fg hover:text-[rgb(var(--accent-primary))]"
                    title="释放 /data/recordings/.transcode-cache 中的临时 MP4 切片"
                >
                    {cleaning ? (
                        <Loader2 size={13} className="animate-spin" />
                    ) : (
                        <Trash2 size={13} />
                    )}
                    <span>{cleaning ? "清理中..." : "清理转码缓存"}</span>
                </Button>
            </CardHeader>

            <CardContent className="space-y-4 pt-1">
                {/* Clean Result Alert */}
                {cleanResult && (
                    <div className="flex items-center gap-2 p-2.5 rounded-xl bg-emerald-500/10 border border-emerald-500/20 text-emerald-600 dark:text-emerald-400 text-xs animate-fade-in">
                        <CheckCircle2 size={15} />
                        <span>{cleanResult}</span>
                    </div>
                )}
                {cleanError && (
                    <div className="flex items-center gap-2 p-2.5 rounded-xl bg-red-500/10 border border-red-500/20 text-red-600 dark:text-red-400 text-xs animate-fade-in">
                        <AlertTriangle size={15} />
                        <span>{cleanError}</span>
                    </div>
                )}

                <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-3.5">
                    {/* CPU Box */}
                    <div className="rounded-xl border border-border/50 bg-[rgb(var(--bg-subtle)/0.25)] p-3 space-y-2">
                        <div className="flex items-center justify-between">
                            <div className="flex items-center gap-1.5 text-xs text-fg font-medium">
                                <Cpu size={15} className="text-sky-500" />
                                <span>CPU 负载</span>
                            </div>
                            <span className="font-mono text-xs font-bold text-fg">
                                {cpuPct}%
                            </span>
                        </div>
                        <div className="w-full bg-slate-200 dark:bg-slate-700 h-2 rounded-full overflow-hidden">
                            <div
                                className={cn(
                                    "h-full transition-all duration-500 rounded-full",
                                    cpuPct > 80 ? "bg-red-500" : cpuPct > 50 ? "bg-amber-500" : "bg-sky-500"
                                )}
                                style={{ width: `${Math.min(cpuPct, 100)}%` }}
                            />
                        </div>
                        <div className="text-[11px] text-fg-subtle flex justify-between">
                            <span>{metrics.cpu?.cores ?? 4} 核心并发</span>
                            <span>{cpuPct > 70 ? "高负荷" : "运行平稳"}</span>
                        </div>
                    </div>

                    {/* Memory Box */}
                    <div className="rounded-xl border border-border/50 bg-[rgb(var(--bg-subtle)/0.25)] p-3 space-y-2">
                        <div className="flex items-center justify-between">
                            <div className="flex items-center gap-1.5 text-xs text-fg font-medium">
                                <Layers size={15} className="text-indigo-500" />
                                <span>物理内存</span>
                            </div>
                            <span className="font-mono text-xs font-bold text-fg">
                                {memPct}%
                            </span>
                        </div>
                        <div className="w-full bg-slate-200 dark:bg-slate-700 h-2 rounded-full overflow-hidden">
                            <div
                                className={cn(
                                    "h-full transition-all duration-500 rounded-full",
                                    memPct > 85 ? "bg-red-500" : memPct > 65 ? "bg-amber-500" : "bg-indigo-500"
                                )}
                                style={{ width: `${Math.min(memPct, 100)}%` }}
                            />
                        </div>
                        <div className="text-[11px] text-fg-subtle flex justify-between font-mono">
                            <span>{formatBytes(metrics.memory?.used_bytes)}</span>
                            <span>/ {formatBytes(metrics.memory?.total_bytes)}</span>
                        </div>
                    </div>

                    {/* Disk Box */}
                    <div className="rounded-xl border border-border/50 bg-[rgb(var(--bg-subtle)/0.25)] p-3 space-y-2">
                        <div className="flex items-center justify-between">
                            <div className="flex items-center gap-1.5 text-xs text-fg font-medium">
                                <HardDrive size={15} className="text-emerald-500" />
                                <span>数据盘挂载 (/data)</span>
                            </div>
                            <span className="font-mono text-xs font-bold text-fg">
                                {diskPct}%
                            </span>
                        </div>
                        <div className="w-full bg-slate-200 dark:bg-slate-700 h-2 rounded-full overflow-hidden">
                            <div
                                className={cn(
                                    "h-full transition-all duration-500 rounded-full",
                                    diskPct > 90 ? "bg-red-500" : diskPct > 75 ? "bg-amber-500" : "bg-emerald-500"
                                )}
                                style={{ width: `${Math.min(diskPct, 100)}%` }}
                            />
                        </div>
                        <div className="text-[11px] text-fg-subtle flex justify-between font-mono">
                            <span>已用 {formatBytes(metrics.disk?.used_bytes)}</span>
                            <span>剩余 {formatBytes(metrics.disk?.free_bytes)}</span>
                        </div>
                    </div>

                    {/* 400G Recordings Quota Box */}
                    <div className="rounded-xl border border-border/50 bg-[rgb(var(--bg-subtle)/0.25)] p-3 space-y-2">
                        <div className="flex items-center justify-between">
                            <div className="flex items-center gap-1.5 text-xs text-fg font-medium">
                                <Database size={15} className="text-amber-500" />
                                <span>400G 监控录像池</span>
                            </div>
                            <span className="font-mono text-xs font-bold text-fg">
                                {recPct}%
                            </span>
                        </div>
                        <div className="w-full bg-slate-200 dark:bg-slate-700 h-2 rounded-full overflow-hidden">
                            <div
                                className={cn(
                                    "h-full transition-all duration-500 rounded-full",
                                    recPct > 95 ? "bg-red-500" : recPct > 80 ? "bg-amber-500" : "bg-amber-400"
                                )}
                                style={{ width: `${Math.min(recPct, 100)}%` }}
                            />
                        </div>
                        <div className="text-[11px] text-fg-subtle flex justify-between">
                            <span className="font-mono">{formatBytes(metrics.recordings?.size_bytes)}</span>
                            <span>/ {formatBytes(metrics.recordings?.quota_bytes || 429496729600)}</span>
                        </div>
                    </div>
                </div>

                {/* Sub-bar: Transcode cache & Quota status */}
                <div className="flex flex-wrap items-center justify-between gap-2 pt-1 border-t border-border/40 text-xs text-fg-muted">
                    <div className="flex items-center gap-2">
                        <span className="text-fg-subtle">转码切片缓存占用：</span>
                        <span className="font-mono font-medium text-fg">
                            {formatBytes(metrics.transcode_cache?.size_bytes)} ({metrics.transcode_cache?.file_count ?? 0} 个文件)
                        </span>
                    </div>
                    {metrics.recordings?.quota_active && (
                        <Badge variant="warning" className="text-[11px] gap-1 px-2 py-0.5">
                            <AlertTriangle size={12} />
                            录像池已达阈值，已自动缩短历史录像保留天数以保障持续录制
                        </Badge>
                    )}
                </div>
            </CardContent>
        </Card>
    );
}
