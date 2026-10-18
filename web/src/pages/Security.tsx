import { useState, useCallback, useEffect, useMemo } from "react";
import {
    ShieldAlert,
    ShieldCheck,
    Mail,
    Ban,
    Unlock,
    RefreshCw,
    Loader2,
    CheckCircle2,
    XCircle,
    Lock,
    Globe,
    Plus,
    X,
    Search,
    Copy,
    Download,
    Clock,
} from "lucide-react";
import { useAuth } from "@/hooks/useAuth";
import { useCachedFetch } from "@/hooks/useCachedFetch";
import {
    getDefenseOverview,
    manualBan,
    manualUnban,
    testAlertEmail,
} from "@/api/security";
import type { DefenseOverview, BanEntry } from "@/types";
import { cn, formatDateTime } from "@/lib/utils";
import {
    Card,
    CardContent,
    CardDescription,
    CardHeader,
    CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { ErrorRetry } from "@/components/ErrorRetry";

export default function Security() {
    const { isAdmin } = useAuth();

    // Feedback states
    const [actionMsg, setActionMsg] = useState<{
        type: "success" | "error";
        text: string;
    } | null>(null);

    // Modal state for manual ban
    const [showBanModal, setShowBanModal] = useState(false);
    const [banIP, setBanIP] = useState("");
    const [banDuration, setBanDuration] = useState(30);
    const [banReason, setBanReason] = useState("");
    const [submittingBan, setSubmittingBan] = useState(false);

    // Testing alert email state
    const [testingEmail, setTestingEmail] = useState(false);

    // Unbanning state per IP
    const [unbanningIP, setUnbanningIP] = useState<string | null>(null);

    // v1.15.0 UI state: live polling, search/sort, ticker for countdowns
    const [autoRefresh, setAutoRefresh] = useState(true);
    const [search, setSearch] = useState("");
    const [sortBy, setSortBy] = useState<"expiry" | "recent">("expiry");
    const [nowMs, setNowMs] = useState(() => Date.now());

    // Fetch overview data
    const {
        data: overview,
        loading,
        error: fetchError,
        refetch,
    } = useCachedFetch<DefenseOverview>(
        "home.security.defense.overview",
        useCallback(async () => {
            return await getDefenseOverview();
        }, []),
        { ttlMs: 10000, refetchMs: autoRefresh ? 30000 : 0 },
    );

    // Re-render every 15s so "剩余 X 分钟" stays truthful without
    // hammering the API.
    useEffect(() => {
        const t = setInterval(() => setNowMs(Date.now()), 15_000);
        return () => clearInterval(t);
    }, []);

    const activeBans: BanEntry[] = overview?.active_bans ?? [];

    const expiryValue = (ban: BanEntry) =>
        ban.permanent ? Number.MAX_SAFE_INTEGER : new Date(ban.expires_at).getTime() || Number.MAX_SAFE_INTEGER;

    const remainingText = (ban: BanEntry) => {
        if (ban.permanent) return "永久";
        const ms = new Date(ban.expires_at).getTime() - nowMs;
        if (!Number.isFinite(ms)) return "";
        if (ms <= 0) return "即将解封";
        const mins = Math.ceil(ms / 60000);
        return mins < 60 ? `剩余 ${mins} 分钟` : `剩余 ${Math.floor(mins / 60)} 小时 ${mins % 60} 分`;
    };

    const filteredBans = useMemo(() => {
        const q = search.trim().toLowerCase();
        const list = activeBans.filter(
            (b) =>
                !q ||
                b.ip.toLowerCase().includes(q) ||
                (b.reason || "").toLowerCase().includes(q),
        );
        return [...list].sort((a, b) =>
            sortBy === "expiry"
                ? expiryValue(a) - expiryValue(b)
                : new Date(b.banned_at).getTime() - new Date(a.banned_at).getTime(),
        );
    }, [activeBans, search, sortBy, nowMs]);

    const handleCopyIP = async (ip: string) => {
        try {
            await navigator.clipboard.writeText(ip);
            showToast("success", `已复制 IP ${ip}`);
        } catch {
            showToast("error", "复制失败，请手动选择文本");
        }
    };

    const handleExportCsv = () => {
        if (activeBans.length === 0) {
            showToast("error", "当前没有封禁记录可导出");
            return;
        }
        const header = "ip,reason,banned_at,expires_at,permanent\n";
        const rows = activeBans
            .map((b) =>
                [b.ip, `"${(b.reason || "").replace(/"/g, '""')}"`, b.banned_at, b.expires_at, b.permanent]
                    .join(","),
            )
            .join("\n");
        const blob = new Blob([header + rows], { type: "text/csv;charset=utf-8" });
        const url = URL.createObjectURL(blob);
        const a = document.createElement("a");
        a.href = url;
        a.download = `security-bans-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, "-")}.csv`;
        a.click();
        URL.revokeObjectURL(url);
        showToast("success", `已导出 ${activeBans.length} 条封禁记录`);
    };

    const showToast = (type: "success" | "error", text: string) => {
        setActionMsg({ type, text });
        setTimeout(() => setActionMsg(null), 5000);
    };

    const handleTestAlert = async () => {
        setTestingEmail(true);
        try {
            const res = await testAlertEmail();
            showToast(
                "success",
                `测试告警邮件已发送至 ${res.recipient || "管理员邮箱"}，请查收！`,
            );
        } catch (err: any) {
            showToast(
                "error",
                `发送测试邮件失败: ${err.message || "请检查 SMTP 配置"}`,
            );
        } finally {
            setTestingEmail(false);
        }
    };

    const isValidIp = (raw: string) => {
        if (!raw || /\s/.test(raw)) return false;
        if (raw.includes(":")) {
            return /^[0-9a-fA-F:]+$/.test(raw) && (raw.match(/:/g)?.length ?? 0) >= 2;
        }
        const parts = raw.split(".");
        return (
            parts.length === 4 &&
            parts.every(
                (p) => p.length > 0 && p.length <= 3 && /^\d+$/.test(p) && Number(p) <= 255,
            )
        );
    };

    const handleManualBan = async (e: React.FormEvent) => {
        e.preventDefault();
        const trimmedIP = banIP.trim();
        if (!trimmedIP) {
            showToast("error", "请输入要封禁的 IP 地址");
            return;
        }
        // v1.15.0: reject hostnames/typos before they reach the server —
        // a wrong entry can lock out the gateway.
        if (!isValidIp(trimmedIP)) {
            showToast("error", "IP 格式不合法，请输入 IPv4 或 IPv6 地址");
            return;
        }
        setSubmittingBan(true);
        try {
            await manualBan({
                ip: trimmedIP,
                duration_minutes: Number(banDuration),
                reason: banReason.trim() || "管理员手动封禁",
            });
            showToast("success", `已成功将 IP [${trimmedIP}] 加入拦截列表！`);
            setShowBanModal(false);
            setBanIP("");
            setBanReason("");
            setBanDuration(30);
            refetch();
        } catch (err: any) {
            showToast("error", `封禁操作失败: ${err.message || "请求异常"}`);
        } finally {
            setSubmittingBan(false);
        }
    };

    const handleUnban = async (ip: string) => {
        setUnbanningIP(ip);
        try {
            await manualUnban(ip);
            showToast("success", `已解除对 IP [${ip}] 的封禁`);
            refetch();
        } catch (err: any) {
            showToast("error", `解封失败: ${err.message || "请求异常"}`);
        } finally {
            setUnbanningIP(null);
        }
    };

    if (!isAdmin) {
        return (
            <div className="space-y-6">
                <div className="animate-fade-in pb-4 mb-2 border-b border-[rgb(var(--border)/0.3)]">
                    <h2 className="text-lg font-semibold tracking-tight text-fg">
                        安全防御管理
                    </h2>
                    <p className="mt-0.5 text-xs text-fg-muted">
                        仅系统管理员具备查看和管理主动入侵防御系统的权限。
                    </p>
                </div>
            </div>
        );
    }

    return (
        <div className="space-y-6 animate-fade-in">
            {/* Header */}
            <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between pb-4 border-b border-[rgb(var(--border)/0.3)]">
                <div>
                    <div className="flex items-center gap-2.5">
                        <div className="flex h-9 w-9 items-center justify-center rounded-xl bg-gradient-to-br from-rose-500/20 to-amber-500/20 text-rose-500 border border-rose-500/30 shadow-sm">
                            <ShieldAlert size={20} />
                        </div>
                        <h2 className="text-xl font-bold tracking-tight text-fg">
                            主动安全防御中心
                        </h2>
                    </div>
                    <p className="mt-1 text-xs text-fg-muted">
                        多层入侵检测与自动化熔断系统：实时拦截恶意爆破、非法路径扫描与未授权探测
                    </p>
                </div>

                <div className="flex flex-wrap items-center gap-2">
                    {/* v1.15.0: live polling toggle (30s) */}
                    <button
                        type="button"
                        onClick={() => setAutoRefresh((v) => !v)}
                        title={autoRefresh ? "每 30 秒自动同步，点击暂停" : "已暂停自动同步，点击开启"}
                        className={cn(
                            "inline-flex items-center gap-1.5 rounded-lg border px-2.5 py-1.5 text-[11px] font-medium transition-colors",
                            autoRefresh
                                ? "border-emerald-500/40 bg-emerald-500/10 text-emerald-600"
                                : "border-[rgb(var(--border)/0.4)] bg-transparent text-fg-muted",
                        )}
                    >
                        <span
                            className={cn(
                                "h-1.5 w-1.5 rounded-full",
                                autoRefresh ? "bg-emerald-500 animate-pulse" : "bg-fg-muted/50",
                            )}
                        />
                        {autoRefresh ? "自动同步中" : "自动同步已暂停"}
                    </button>

                    <Button
                        variant="outline"
                        size="sm"
                        onClick={handleTestAlert}
                        disabled={testingEmail || loading}
                        className="gap-2 glass text-xs"
                    >
                        {testingEmail ? (
                            <Loader2 size={14} className="animate-spin text-rose-500" />
                        ) : (
                            <Mail size={14} className="text-rose-500" />
                        )}
                        <span>测试告警邮件</span>
                    </Button>

                    <Button
                        size="sm"
                        onClick={() => setShowBanModal(true)}
                        className="gap-1.5 bg-gradient-to-r from-rose-500 to-amber-600 hover:from-rose-600 hover:to-amber-700 text-white text-xs shadow-md"
                    >
                        <Plus size={14} />
                        <span>手动封禁 IP</span>
                    </Button>

                    <Button
                        variant="ghost"
                        size="icon"
                        onClick={() => refetch()}
                        disabled={loading}
                        aria-label="刷新状态"
                        title="刷新安全状态"
                    >
                        <RefreshCw size={15} className={cn(loading && "animate-spin")} />
                    </Button>
                </div>
            </div>

            {/* Notification alert banner */}
            {actionMsg && (
                <div
                    className={cn(
                        "flex items-center gap-2.5 rounded-xl px-4 py-3 text-xs font-medium animate-slide-up shadow-sm border",
                        actionMsg.type === "success"
                            ? "bg-emerald-500/10 text-emerald-600 border-emerald-500/30"
                            : "bg-rose-500/10 text-rose-600 border-rose-500/30",
                    )}
                >
                    {actionMsg.type === "success" ? (
                        <CheckCircle2 size={16} className="shrink-0 text-emerald-500" />
                    ) : (
                        <XCircle size={16} className="shrink-0 text-rose-500" />
                    )}
                    <span className="flex-1">{actionMsg.text}</span>
                    <button
                        onClick={() => setActionMsg(null)}
                        className="opacity-70 hover:opacity-100 p-0.5"
                    >
                        <X size={14} />
                    </button>
                </div>
            )}

            {/* Error display if fetch fails */}
            {fetchError && (
                <ErrorRetry
                    message={`获取安全防御数据失败: ${fetchError.message}`}
                    onRetry={refetch}
                />
            )}

            {/* 4 Overview metric cards */}
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
                {/* Metric 1 */}
                <Card className="glass-subtle hover-glow border-[rgb(var(--border)/0.4)]">
                    <CardHeader className="flex flex-row items-center justify-between pb-2">
                        <span className="text-xs font-medium text-fg-muted">
                            防御运行状态
                        </span>
                        <div className="rounded-lg p-2 bg-emerald-500/10 text-emerald-500">
                            <ShieldCheck size={18} />
                        </div>
                    </CardHeader>
                    <CardContent>
                        <div className="text-xl font-bold tracking-tight text-emerald-500 flex items-center gap-1.5">
                            <span className="h-2.5 w-2.5 rounded-full bg-emerald-500 animate-pulse" />
                            主动巡检中
                        </div>
                        <p className="mt-1 text-[11px] text-fg-muted">
                            5次验证失败触发自动熔断
                        </p>
                    </CardContent>
                </Card>

                {/* Metric 2 */}
                <Card className="glass-subtle hover-glow border-[rgb(var(--border)/0.4)]">
                    <CardHeader className="flex flex-row items-center justify-between pb-2">
                        <span className="text-xs font-medium text-fg-muted">
                            当前封禁 IP 数
                        </span>
                        <div className="rounded-lg p-2 bg-rose-500/10 text-rose-500">
                            <Ban size={18} />
                        </div>
                    </CardHeader>
                    <CardContent>
                        <div className="text-2xl font-bold tracking-tight text-fg">
                            {overview?.total_banned_count ?? 0}
                        </div>
                        <p className="mt-1 text-[11px] text-fg-muted">
                            {overview?.total_banned_count === 0
                                ? "当前系统无攻击者滞留"
                                : "处于全系统 403 阻断拦截状态"}
                        </p>
                    </CardContent>
                </Card>

                {/* Metric 3 */}
                <Card className="glass-subtle hover-glow border-[rgb(var(--border)/0.4)]">
                    <CardHeader className="flex flex-row items-center justify-between pb-2">
                        <span className="text-xs font-medium text-fg-muted">
                            累计阻断攻击次数
                        </span>
                        <div className="rounded-lg p-2 bg-amber-500/10 text-amber-500">
                            <ShieldAlert size={18} />
                        </div>
                    </CardHeader>
                    <CardContent>
                        <div className="text-2xl font-bold tracking-tight text-fg">
                            {overview?.total_blocked_hits ?? 0}
                        </div>
                        <p className="mt-1 text-[11px] text-fg-muted">
                            阻断高危探测及非法穿透请求
                        </p>
                    </CardContent>
                </Card>

                {/* Metric 4 */}
                <Card className="glass-subtle hover-glow border-[rgb(var(--border)/0.4)]">
                    <CardHeader className="flex flex-row items-center justify-between pb-2">
                        <span className="text-xs font-medium text-fg-muted">
                            邮件紧急告警通道
                        </span>
                        <div className="rounded-lg p-2 bg-blue-500/10 text-blue-500">
                            <Mail size={18} />
                        </div>
                    </CardHeader>
                    <CardContent>
                        <div className="flex items-center gap-1.5 text-base font-semibold text-fg">
                            {overview?.smtp_enabled ? (
                                <Badge
                                    variant="outline"
                                    className="bg-emerald-500/10 text-emerald-600 border-emerald-500/30"
                                >
                                    QQ 邮箱 SSL 已连接
                                </Badge>
                            ) : (
                                <Badge variant="outline" className="text-fg-muted">
                                    未配置通知
                                </Badge>
                            )}
                        </div>
                        <p
                            className="mt-1.5 truncate text-[11px] text-fg-muted"
                            title={overview?.recipient || "3318647421@qq.com"}
                        >
                            接收端: {overview?.recipient || "3318647421@qq.com"}
                        </p>
                    </CardContent>
                </Card>
            </div>

            {/* Active Banned IPs Table */}
            <Card className="glass border-[rgb(var(--border)/0.4)]">
                <CardHeader className="pb-3 border-b border-[rgb(var(--border)/0.2)]">
                    <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
                        <div>
                            <CardTitle className="text-base font-semibold flex items-center gap-2">
                                <Ban size={18} className="text-rose-500" />
                                <span>实时黑名单与拦截列表</span>
                                <Badge variant="default" className="text-[11px]">
                                    {filteredBans.length === activeBans.length
                                        ? `${activeBans.length} 个 IP`
                                        : `${filteredBans.length} / ${activeBans.length} 个 IP`}
                                </Badge>
                            </CardTitle>
                            <CardDescription className="text-xs mt-0.5">
                                被拦截的 IP 在所有业务路由（Web、API、流媒体）中均被阻断并记录审计日志
                            </CardDescription>
                        </div>
                        <div className="flex flex-wrap items-center gap-2">
                            <div className="relative">
                                <Search
                                    size={13}
                                    className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-fg-muted"
                                />
                                <Input
                                    value={search}
                                    onChange={(e) => setSearch(e.target.value)}
                                    placeholder="搜索 IP / 原因"
                                    className="h-8 w-44 pl-7 text-xs"
                                />
                            </div>
                            <select
                                value={sortBy}
                                onChange={(e) => setSortBy(e.target.value as "expiry" | "recent")}
                                className="h-8 rounded-lg border border-[rgb(var(--border)/0.4)] bg-transparent px-2 text-xs text-fg"
                                aria-label="排序方式"
                            >
                                <option value="expiry">按剩余时长</option>
                                <option value="recent">按拦截时间</option>
                            </select>
                            <Button
                                variant="outline"
                                size="sm"
                                onClick={handleExportCsv}
                                className="h-8 gap-1.5 text-xs"
                                title="导出当前黑名单为 CSV"
                            >
                                <Download size={13} />
                                <span>导出</span>
                            </Button>
                        </div>
                    </div>
                </CardHeader>
                <CardContent className="p-0">
                    {loading && !overview ? (
                        <div className="flex items-center justify-center py-12 text-fg-muted text-xs gap-2">
                            <Loader2 size={16} className="animate-spin text-primary" />
                            正在载入防御黑名单...
                        </div>
                    ) : activeBans.length === 0 ? (
                        <div className="flex flex-col items-center justify-center py-12 text-center">
                            <div className="flex h-12 w-12 items-center justify-center rounded-full bg-emerald-500/10 text-emerald-500 mb-3">
                                <ShieldCheck size={24} />
                            </div>
                            <p className="text-sm font-medium text-fg">
                                当前无活跃被拦截 IP
                            </p>
                            <p className="text-xs text-fg-muted max-w-sm mt-1">
                                安全防御引擎正在后台持续监控流量，若外部发起撞库爆破或恶意渗透将自动封锁。
                            </p>
                        </div>
                    ) : filteredBans.length === 0 ? (
                        <div className="flex flex-col items-center justify-center py-10 text-center">
                            <p className="text-sm font-medium text-fg">没有匹配「{search}」的封禁记录</p>
                            <button
                                onClick={() => setSearch("")}
                                className="mt-2 text-xs text-primary hover:underline"
                            >
                                清除搜索条件
                            </button>
                        </div>
                    ) : (
                        <div className="overflow-x-auto">
                            <table className="w-full text-left text-xs">
                                <thead className="border-b border-[rgb(var(--border)/0.2)] bg-[rgb(var(--bg-subtle)/0.3)] text-fg-muted">
                                    <tr>
                                        <th className="py-3 px-4 font-medium">拦截 IP 地址</th>
                                        <th className="py-3 px-4 font-medium">触发原因 / 威胁类型</th>
                                        <th className="py-3 px-4 font-medium">拦截时间</th>
                                        <th className="py-3 px-4 font-medium">有效期限</th>
                                        <th className="py-3 px-4 font-medium text-right">操作</th>
                                    </tr>
                                </thead>
                                <tbody className="divide-y divide-[rgb(var(--border)/0.15)]">
                                    {filteredBans.map((ban) => {
                                        const isUnbanning = unbanningIP === ban.ip;
                                        return (
                                            <tr
                                                key={ban.ip}
                                                className="transition-colors hover:bg-[rgb(var(--bg-subtle)/0.25)]"
                                            >
                                                <td className="py-3 px-4 font-mono font-medium text-fg">
                                                    <div className="flex items-center gap-2">
                                                        <span className="h-2 w-2 rounded-full bg-rose-500" />
                                                        <span>{ban.ip}</span>
                                                        <button
                                                            type="button"
                                                            onClick={() => handleCopyIP(ban.ip)}
                                                            title="复制 IP"
                                                            className="text-fg-muted/70 hover:text-fg transition-colors"
                                                        >
                                                            <Copy size={12} />
                                                        </button>
                                                    </div>
                                                </td>
                                                <td className="py-3 px-4 text-fg-muted">
                                                    <span className="inline-flex items-center gap-1.5 rounded-md px-2 py-0.5 bg-rose-500/10 text-rose-600 font-medium text-[11px] border border-rose-500/20">
                                                        {ban.reason || "触发恶意访问策略"}
                                                    </span>
                                                </td>
                                                <td className="py-3 px-4 text-fg-muted">
                                                    {formatDateTime(ban.banned_at)}
                                                </td>
                                                <td className="py-3 px-4 text-fg-muted">
                                                    {ban.permanent ? (
                                                        <Badge variant="outline" className="border-rose-500/40 text-rose-500">
                                                            永久封禁
                                                        </Badge>
                                                    ) : (
                                                        <div className="flex flex-col gap-0.5">
                                                            <span className="inline-flex items-center gap-1 text-[11px] font-medium text-amber-600">
                                                                <Clock size={11} />
                                                                {remainingText(ban)}
                                                            </span>
                                                            <span className="text-[11px] text-fg-muted/80">
                                                                至 {formatDateTime(ban.expires_at)} 自动解封
                                                            </span>
                                                        </div>
                                                    )}
                                                </td>
                                                <td className="py-3 px-4 text-right">
                                                    <Button
                                                        variant="ghost"
                                                        size="sm"
                                                        onClick={() => handleUnban(ban.ip)}
                                                        disabled={isUnbanning}
                                                        className="text-xs gap-1.5 text-fg-muted hover:text-emerald-500 hover:bg-emerald-500/10"
                                                    >
                                                        {isUnbanning ? (
                                                            <Loader2 size={13} className="animate-spin" />
                                                        ) : (
                                                            <Unlock size={13} />
                                                        )}
                                                        <span>立即解封</span>
                                                    </Button>
                                                </td>
                                            </tr>
                                        );
                                    })}
                                </tbody>
                            </table>
                        </div>
                    )}
                </CardContent>
            </Card>

            {/* Defense Policies & Architecture Details */}
            <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
                <Card className="glass-subtle border-[rgb(var(--border)/0.3)]">
                    <CardHeader className="pb-2.5">
                        <CardTitle className="text-sm font-semibold flex items-center gap-2">
                            <Lock size={16} className="text-amber-500" />
                            <span>主动入侵防护策略</span>
                        </CardTitle>
                    </CardHeader>
                    <CardContent className="space-y-2 text-xs text-fg-muted leading-relaxed">
                        <div className="flex items-start gap-2">
                            <span className="font-semibold text-fg">1. 身份认证防爆破:</span>
                            <span>
                                5 分钟内连续 5 次设备绑定失败将立即熔断，默认封禁 30 分钟。
                            </span>
                        </div>
                        <div className="flex items-start gap-2">
                            <span className="font-semibold text-fg">2. 真实 IP 穿透识别:</span>
                            <span>
                                自动解析 Cloudflare `CF-Connecting-IP` 及受信任代理链路，防止代理掩护。
                            </span>
                        </div>
                        <div className="flex items-start gap-2">
                            <span className="font-semibold text-fg">3. 自动回收机制:</span>
                            <span>
                                后台定时清理过期临时封禁记录，无需人工频繁清理黑名单。
                            </span>
                        </div>
                    </CardContent>
                </Card>

                <Card className="glass-subtle border-[rgb(var(--border)/0.3)]">
                    <CardHeader className="pb-2.5">
                        <CardTitle className="text-sm font-semibold flex items-center gap-2">
                            <Globe size={16} className="text-blue-500" />
                            <span>白名单与告警通道</span>
                        </CardTitle>
                    </CardHeader>
                    <CardContent className="space-y-2 text-xs text-fg-muted leading-relaxed">
                        <div className="flex items-start gap-2">
                            <span className="font-semibold text-fg">安全白名单:</span>
                            <span>
                                内网 `192.168.31.0/24`、`127.0.0.1` 及本机环回具备豁免权，杜绝家庭内部被误杀。
                            </span>
                        </div>
                        <div className="flex items-start gap-2">
                            <span className="font-semibold text-fg">邮件即时通知:</span>
                            <span>
                                触发黑名单熔断时，通过 QQ 邮箱 SMTP 即时推送 HTML 预警至{" "}
                                <span className="font-mono text-fg">3318647421@qq.com</span>。
                            </span>
                        </div>
                        <div className="flex items-start gap-2">
                            <span className="font-semibold text-fg">事件总线联动:</span>
                            <span>
                                威胁事件同步发布至 EventBus，前端仪表盘及系统日志实时呈现告警流。
                            </span>
                        </div>
                    </CardContent>
                </Card>
            </div>

            {/* Manual Ban Modal */}
            {showBanModal && (
                <div
                    className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 backdrop-blur-sm p-4 animate-fade-in"
                    onClick={() => setShowBanModal(false)}
                >
                    <div
                        className="w-full max-w-md rounded-2xl glass p-6 shadow-2xl animate-scale-in border border-[rgb(var(--border)/0.4)]"
                        onClick={(e) => e.stopPropagation()}
                    >
                        <div className="flex items-center justify-between pb-3 border-b border-[rgb(var(--border)/0.2)]">
                            <div className="flex items-center gap-2">
                                <Ban size={18} className="text-rose-500" />
                                <h3 className="text-base font-semibold text-fg">
                                    手动拦截 / 封禁 IP
                                </h3>
                            </div>
                            <button
                                onClick={() => setShowBanModal(false)}
                                className="text-fg-muted hover:text-fg p-1"
                            >
                                <X size={18} />
                            </button>
                        </div>

                        <form onSubmit={handleManualBan} className="mt-4 space-y-4">
                            <div>
                                <label className="block text-xs font-medium text-fg mb-1">
                                    目标 IP 地址 <span className="text-rose-500">*</span>
                                </label>
                                <Input
                                    value={banIP}
                                    onChange={(e) => setBanIP(e.target.value)}
                                    placeholder="例如 185.220.101.5"
                                    className="font-mono text-xs"
                                    required
                                    autoFocus
                                />
                            </div>

                            <div>
                                <label className="block text-xs font-medium text-fg mb-1.5">
                                    封禁时长
                                </label>
                                <div className="flex flex-wrap gap-1.5 mb-2">
                                    {[
                                        { label: "30 分钟", value: 30 },
                                        { label: "6 小时", value: 360 },
                                        { label: "24 小时", value: 1440 },
                                        { label: "永久", value: 0 },
                                    ].map((opt) => (
                                        <button
                                            key={opt.value}
                                            type="button"
                                            onClick={() => setBanDuration(opt.value)}
                                            className={cn(
                                                "rounded-lg border px-2.5 py-1 text-[11px] font-medium transition-colors",
                                                banDuration === opt.value
                                                    ? "border-rose-500/50 bg-rose-500/10 text-rose-600"
                                                    : "border-[rgb(var(--border)/0.4)] text-fg-muted hover:text-fg",
                                            )}
                                        >
                                            {opt.label}
                                        </button>
                                    ))}
                                </div>
                                <Input
                                    type="number"
                                    min="0"
                                    value={banDuration}
                                    onChange={(e) => setBanDuration(Number(e.target.value))}
                                    className="text-xs"
                                    placeholder="自定义分钟数，0 表示永久"
                                />
                                <p className="mt-1 text-[11px] text-fg-muted">
                                    当前：{banDuration === 0 ? "永久封禁（需手动解封）" : `${banDuration} 分钟后自动解封`}
                                </p>
                            </div>

                            <div>
                                <label className="block text-xs font-medium text-fg mb-1">
                                    封禁原因说明
                                </label>
                                <Input
                                    value={banReason}
                                    onChange={(e) => setBanReason(e.target.value)}
                                    placeholder="例如：恶意扫描 /api/v1/debug"
                                    className="text-xs"
                                />
                            </div>

                            <div className="flex items-center justify-end gap-2 pt-3 border-t border-[rgb(var(--border)/0.2)]">
                                <Button
                                    type="button"
                                    variant="ghost"
                                    size="sm"
                                    onClick={() => setShowBanModal(false)}
                                    disabled={submittingBan}
                                    className="text-xs"
                                >
                                    取消
                                </Button>
                                <Button
                                    type="submit"
                                    size="sm"
                                    disabled={submittingBan}
                                    className="bg-rose-600 hover:bg-rose-700 text-white text-xs gap-1.5"
                                >
                                    {submittingBan && (
                                        <Loader2 size={13} className="animate-spin" />
                                    )}
                                    <span>确认封禁</span>
                                </Button>
                            </div>
                        </form>
                    </div>
                </div>
            )}
        </div>
    );
}
