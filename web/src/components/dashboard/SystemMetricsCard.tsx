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
    Settings2,
    CloudUpload,
    X,
} from "lucide-react";
import {
    cleanTranscodeCache,
    getStorageConfig,
    updateStorageConfig,
    triggerArchiveSync,
    type CleanCacheResponse,
} from "@/api/system";
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
    const [showModal, setShowModal] = useState(false);
    const [cfgQuota, setCfgQuota] = useState(400);
    const [cfgHour, setCfgHour] = useState(3);
    const [cfgMinAge, setCfgMinAge] = useState(7);
    const [savingCfg, setSavingCfg] = useState(false);
    const [syncingArchive, setSyncingArchive] = useState(false);

    const openConfigModal = async () => {
        setShowModal(true);
        try {
            const cfg = await getStorageConfig();
            setCfgQuota(cfg.quota_gb || 400);
            setCfgHour(cfg.archive_schedule_hour ?? 3);
            setCfgMinAge(cfg.archive_min_age_days ?? 7);
        } catch (e) {
            console.error("Failed to load storage config:", e);
        }
    };

    const handleSaveConfig = async () => {
        setSavingCfg(true);
        try {
            await updateStorageConfig({
                quota_gb: cfgQuota,
                archive_schedule_hour: cfgHour,
                archive_min_age_days: cfgMinAge,
            });
            setShowModal(false);
            setCleanResult(`已更新配置：录像池上限 ${cfgQuota} GB，每日 ${String(cfgHour).padStart(2, "0")}:00 归档`);
            onCleaned?.();
        } catch (err: any) {
            alert(err?.message || "保存配置失败");
        } finally {
            setSavingCfg(false);
        }
    };

    const handleTriggerSync = async () => {
        setSyncingArchive(true);
        try {
            await triggerArchiveSync();
            setCleanResult("已成功触发蓝奏云录像归档任务！");
        } catch (err: any) {
            alert(err?.message || "触发归档失败");
        } finally {
            setSyncingArchive(false);
        }
    };

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
        <Card className="animate-fade-in border shadow-sm relative">
            <CardHeader className="flex flex-row items-center justify-between pb-3">
                <div className="space-y-0.5">
                    <CardTitle className="text-base font-semibold flex items-center gap-2 text-fg">
                        <Activity className="text-[rgb(var(--accent-primary))]" size={18} />
                        数据中心资源与存储看板
                    </CardTitle>
                    <p className="text-xs text-fg-muted">
                        宿主机硬件负载、{metrics.recordings?.quota_bytes ? Math.round(metrics.recordings.quota_bytes / (1024 * 1024 * 1024)) : 400} GiB 监控录像池配额与云端归档
                    </p>
                </div>
                <div className="flex items-center gap-2">
                    <Button
                        variant="outline"
                        size="sm"
                        onClick={openConfigModal}
                        className="h-8 gap-1.5 text-xs text-fg hover:text-[rgb(var(--accent-primary))]"
                        title="设置录像配额与云端同步计划"
                    >
                        <Settings2 size={13} />
                        <span>配额与归档</span>
                    </Button>
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
                </div>
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
                <div className="flex flex-wrap items-center justify-between gap-2 pt-2 border-t border-border/40 text-xs text-fg-muted">
                    <div className="flex flex-wrap items-center gap-4">
                        <div className="flex items-center gap-1.5">
                            <span className="text-fg-subtle">转码切片缓存：</span>
                            <span className="font-mono font-medium text-fg">
                                {formatBytes(metrics.transcode_cache?.size_bytes)} ({metrics.transcode_cache?.file_count ?? 0} 个文件)
                            </span>
                        </div>
                        <div className="flex items-center gap-1.5">
                            <span className="text-fg-subtle">蓝奏云归档调度：</span>
                            <span className="font-medium text-fg">
                                每日 {String(cfgHour).padStart(2, "0")}:00 (留存 ≥{cfgMinAge} 天)
                            </span>
                        </div>
                    </div>
                    {metrics.recordings?.quota_active && (
                        <Badge variant="warning" className="text-[11px] gap-1 px-2 py-0.5">
                            <AlertTriangle size={12} />
                            录像池已达阈值，已自动缩短历史录像保留天数以保障持续录制
                        </Badge>
                    )}
                </div>
            </CardContent>

            {/* Storage Quota & Archive Config Modal */}
            {showModal && (
                <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 backdrop-blur-sm p-4 animate-fade-in">
                    <div className="bg-card border border-border rounded-2xl shadow-2xl max-w-lg w-full p-6 space-y-5 text-fg animate-scale-in">
                        <div className="flex items-center justify-between pb-3 border-b border-border/50">
                            <div className="flex items-center gap-2">
                                <Settings2 className="text-[rgb(var(--accent-primary))]" size={20} />
                                <h3 className="font-semibold text-base">录像池配额与云端归档设置</h3>
                            </div>
                            <Button
                                variant="ghost"
                                size="sm"
                                onClick={() => setShowModal(false)}
                                className="h-8 w-8 p-0 rounded-full hover:bg-muted"
                            >
                                <X size={16} />
                            </Button>
                        </div>

                        <div className="space-y-4 text-xs">
                            {/* Quota Setting */}
                            <div className="space-y-1.5">
                                <label className="font-medium text-fg flex items-center justify-between">
                                    <span>本地监控录像池配额上限 (GB)</span>
                                    <span className="text-fg-muted font-mono font-bold text-sm text-[rgb(var(--accent-primary))]">
                                        {cfgQuota} GB
                                    </span>
                                </label>
                                <p className="text-[11px] text-fg-subtle">
                                    当录像总体积超过此配额时，系统将自动触发历史切片轮转淘汰，保障新录像顺畅写入。
                                </p>
                                <div className="flex items-center gap-2 pt-1">
                                    <input
                                        type="number"
                                        min="50"
                                        max="5000"
                                        step="50"
                                        value={cfgQuota}
                                        onChange={(e) => setCfgQuota(Math.max(50, parseInt(e.target.value) || 400))}
                                        className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                                    />
                                    <div className="flex gap-1">
                                        {[200, 400, 800, 1000].map((q) => (
                                            <Button
                                                key={q}
                                                type="button"
                                                variant={cfgQuota === q ? "primary" : "outline"}
                                                size="sm"
                                                className="h-9 px-2 text-xs font-mono"
                                                onClick={() => setCfgQuota(q)}
                                            >
                                                {q}G
                                            </Button>
                                        ))}
                                    </div>
                                </div>
                            </div>

                            {/* Archive Schedule Setting */}
                            <div className="space-y-1.5 pt-2 border-t border-border/40">
                                <label className="font-medium text-fg flex items-center justify-between">
                                    <span>每日自动归档执行时间</span>
                                    <span className="text-fg-muted font-mono font-bold text-sm">
                                        {String(cfgHour).padStart(2, "0")}:00
                                    </span>
                                </label>
                                <p className="text-[11px] text-fg-subtle">
                                    每日在此时间通过 Alist WebDAV 将本地积压的历史录像切片无损上传归档到蓝奏云网盘。
                                </p>
                                <select
                                    value={cfgHour}
                                    onChange={(e) => setCfgHour(parseInt(e.target.value) || 0)}
                                    className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
                                >
                                    {Array.from({ length: 24 }).map((_, i) => (
                                        <option key={i} value={i} className="bg-card text-fg">
                                            {String(i).padStart(2, "0")}:00 {i < 6 ? "(凌晨低峰)" : i < 12 ? "(上午)" : i < 18 ? "(下午)" : "(晚上)"}
                                        </option>
                                    ))}
                                </select>
                            </div>

                            {/* Min Age Setting */}
                            <div className="space-y-1.5 pt-2 border-t border-border/40">
                                <label className="font-medium text-fg flex items-center justify-between">
                                    <span>本地历史切片最小保留天数</span>
                                    <span className="text-fg-muted font-mono font-bold text-sm">
                                        {cfgMinAge} 天
                                    </span>
                                </label>
                                <p className="text-[11px] text-fg-subtle">
                                    仅将生成超过此天数的积压旧切片归档并由云端接管，保证近期关键录像在本地 NAS SSD 秒级秒开。
                                </p>
                                <div className="flex items-center gap-2">
                                    {[3, 5, 7, 14, 30].map((days) => (
                                        <Button
                                            key={days}
                                            type="button"
                                            variant={cfgMinAge === days ? "primary" : "outline"}
                                            size="sm"
                                            className="h-8 flex-1 text-xs"
                                            onClick={() => setCfgMinAge(days)}
                                        >
                                            {days} 天
                                        </Button>
                                    ))}
                                </div>
                            </div>

                            {/* Manual Trigger Option */}
                            <div className="pt-2 border-t border-border/40 flex items-center justify-between bg-muted/30 p-2.5 rounded-xl">
                                <div className="space-y-0.5">
                                    <div className="font-medium text-fg">手动触发全量同步</div>
                                    <div className="text-[11px] text-fg-subtle">无需等待每日计划，立即通知归档容器运行一次同步</div>
                                </div>
                                <Button
                                    type="button"
                                    variant="outline"
                                    size="sm"
                                    onClick={handleTriggerSync}
                                    disabled={syncingArchive}
                                    className="gap-1.5 h-8 text-xs text-sky-600 dark:text-sky-400 border-sky-500/30 hover:bg-sky-500/10"
                                >
                                    {syncingArchive ? (
                                        <Loader2 size={13} className="animate-spin" />
                                    ) : (
                                        <CloudUpload size={13} />
                                    )}
                                    <span>{syncingArchive ? "正在通知..." : "立即归档"}</span>
                                </Button>
                            </div>
                        </div>

                        <div className="flex items-center justify-end gap-2.5 pt-3 border-t border-border/50">
                            <Button
                                type="button"
                                variant="outline"
                                size="sm"
                                onClick={() => setShowModal(false)}
                            >
                                取消
                            </Button>
                            <Button
                                type="button"
                                size="sm"
                                onClick={handleSaveConfig}
                                disabled={savingCfg}
                                className="gap-1.5"
                            >
                                {savingCfg && <Loader2 size={13} className="animate-spin" />}
                                <span>{savingCfg ? "保存中..." : "保存配置"}</span>
                            </Button>
                        </div>
                    </div>
                </div>
            )}
        </Card>
    );
}
