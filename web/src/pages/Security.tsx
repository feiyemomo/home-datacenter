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
    Activity,
    Sliders,
    Trash2,
    Shield,
    TrendingUp,
    Flame,
    PieChart,
    ExternalLink,
    Terminal,
    Server,
} from "lucide-react";
import { useAuth } from "@/hooks/useAuth";
import { useCachedFetch } from "@/hooks/useCachedFetch";
import { useWebSocket } from "@/hooks/useWebSocket";
import {
    getDefenseOverview,
    manualBan,
    manualUnban,
    updateBan,
    testAlertEmail,
    getSecurityEvents,
    pruneSecurityEvents,
    getSecurityWhitelist,
    addSecurityWhitelist,
    deleteSecurityWhitelist,
    updateSecurityConfig,
    getHostLogs,
} from "@/api/security";
import type {
    DefenseOverview,
    BanEntry,
    SecurityEvent,
    SecurityWhitelistItem,
    HostLogStatusResponse,
} from "@/types";
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

    // Active tab: 'bans' | 'events' | 'whitelist' | 'config' | 'host'
    const [activeTab, setActiveTab] = useState<"bans" | "events" | "whitelist" | "config" | "host">("bans");

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

    // Live polling, search/sort, ticker for countdowns
    const [autoRefresh, setAutoRefresh] = useState(true);
    const [search, setSearch] = useState("");
    const [sortBy, setSortBy] = useState<"expiry" | "recent" | "hits">("expiry");
    const [nowMs, setNowMs] = useState(() => Date.now());

    // Timeline events
    const [events, setEvents] = useState<SecurityEvent[]>([]);
    const [eventsLoading, setEventsLoading] = useState(false);
    const [eventFilterType, setEventFilterType] = useState<string>("all");
    const [pruningEvents, setPruningEvents] = useState(false);

    // Whitelist state
    const [whitelist, setWhitelist] = useState<SecurityWhitelistItem[]>([]);
    const [whitelistLoading, setWhitelistLoading] = useState(false);
    const [newWLEntry, setNewWLEntry] = useState("");
    const [newWLNote, setNewWLNote] = useState("");
    const [addingWL, setAddingWL] = useState(false);

    // Host OS log integration state
    const [hostLogData, setHostLogData] = useState<HostLogStatusResponse | null>(null);
    const [hostLogLoading, setHostLogLoading] = useState(false);

    // Config form state
    const [cfgFailures, setCfgFailures] = useState(5);
    const [cfgWindow, setCfgWindow] = useState(5);
    const [cfgBanDuration, setCfgBanDuration] = useState(30);
    const [savingCfg, setSavingCfg] = useState(false);

    // WebSocket real-time alerts
    const { lastMessage, subscribe } = useWebSocket(true);

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

    // Subscribe to EventBus security topics
    useEffect(() => {
        subscribe("security.alert");
        subscribe("security.guard_mode");
    }, [subscribe]);

    // Handle incoming real-time security events
    useEffect(() => {
        if (!lastMessage) return;
        if (lastMessage.topic === "security.alert") {
            const payload = lastMessage.payload as any;
            const msg = typeof payload === "object" && payload?.message ? payload.message : "检测到新的安全防护拦截事件";
            setActionMsg({ type: "error", text: `[实时安全拦截] ${msg}` });
            setTimeout(() => setActionMsg(null), 6000);
            refetch();
            if (activeTab === "events") fetchEventsData();
        } else if (lastMessage.topic === "security.guard_mode") {
            refetch();
        }
    }, [lastMessage, refetch, activeTab]);

    // Sync config state from overview
    useEffect(() => {
        if (overview?.config) {
            setCfgFailures(overview.config.max_auth_failures);
            setCfgWindow(overview.config.failure_window_mins);
            setCfgBanDuration(overview.config.ban_duration_mins);
        }
    }, [overview]);

    // Re-render ticker for countdowns
    useEffect(() => {
        const t = setInterval(() => setNowMs(Date.now()), 15_000);
        return () => clearInterval(t);
    }, []);

    const fetchEventsData = useCallback(async () => {
        setEventsLoading(true);
        try {
            const data = await getSecurityEvents(50);
            setEvents(data);
        } catch {
            // non-fatal
        } finally {
            setEventsLoading(false);
        }
    }, []);

    const fetchWhitelistData = useCallback(async () => {
        setWhitelistLoading(true);
        try {
            const data = await getSecurityWhitelist();
            setWhitelist(data);
        } catch {
            // non-fatal
        } finally {
            setWhitelistLoading(false);
        }
    }, []);

    const fetchHostLogData = useCallback(async () => {
        setHostLogLoading(true);
        try {
            const data = await getHostLogs();
            setHostLogData(data);
        } catch {
            // non-fatal
        } finally {
            setHostLogLoading(false);
        }
    }, []);

    // Load tab-specific data on switch
    useEffect(() => {
        if (activeTab === "events") {
            fetchEventsData();
        } else if (activeTab === "whitelist") {
            fetchWhitelistData();
        } else if (activeTab === "host") {
            fetchHostLogData();
        }
    }, [activeTab, fetchEventsData, fetchWhitelistData, fetchHostLogData]);

    const activeBans: BanEntry[] = overview?.active_bans ?? [];

    const expiryValue = (ban: BanEntry) =>
        ban.permanent ? Number.MAX_SAFE_INTEGER : new Date(ban.expires_at).getTime() || Number.MAX_SAFE_INTEGER;

    const remainingText = (ban: BanEntry) => {
        if (ban.permanent) return "永久封禁";
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
                (b.reason || "").toLowerCase().includes(q) ||
                (b.source || "").toLowerCase().includes(q),
        );
        return [...list].sort((a, b) => {
            if (sortBy === "hits") {
                return (b.hit_count || 0) - (a.hit_count || 0);
            }
            if (sortBy === "recent") {
                return new Date(b.banned_at).getTime() - new Date(a.banned_at).getTime();
            }
            return expiryValue(a) - expiryValue(b);
        });
    }, [activeBans, search, sortBy, nowMs]);

    const filteredEvents = useMemo(() => {
        if (eventFilterType === "all") return events;
        return events.filter((e) => e.type === eventFilterType);
    }, [events, eventFilterType]);

    const handleUpgradeToPermanent = async (ip: string) => {
        try {
            await updateBan(ip, 0, true);
            showToast("success", `已将 IP [${ip}] 升级为永久黑名单`);
            refetch();
        } catch (err: any) {
            showToast("error", `升级失败: ${err.message || "请求异常"}`);
        }
    };

    const handlePruneEvents = async (days = 30) => {
        if (!window.confirm(`确定要清理 ${days} 天前的历史安全事件日志吗？此操作不可逆。`)) return;
        setPruningEvents(true);
        try {
            const res = await pruneSecurityEvents(days);
            showToast("success", `已成功清理 ${res.deleted} 条历史审计日志`);
            fetchEventsData();
            refetch();
        } catch (err: any) {
            showToast("error", `清理日志失败: ${err.message || "请求异常"}`);
        } finally {
            setPruningEvents(false);
        }
    };

    const showToast = (type: "success" | "error", text: string) => {
        setActionMsg({ type, text });
        setTimeout(() => setActionMsg(null), 5000);
    };

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
        const header = "ip,reason,source,hit_count,banned_at,expires_at,permanent\n";
        const rows = activeBans
            .map((b) =>
                [
                    b.ip,
                    `"${(b.reason || "").replace(/"/g, '""')}"`,
                    b.source || "auto",
                    b.hit_count || 0,
                    b.banned_at,
                    b.expires_at,
                    b.permanent,
                ].join(","),
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
            showToast("error", `封禁操作被拦截: ${err.response?.data?.message || err.message || "请求异常"}`);
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

    const handleAddWhitelist = async (e: React.FormEvent) => {
        e.preventDefault();
        const entry = newWLEntry.trim();
        if (!entry) return;
        setAddingWL(true);
        try {
            await addSecurityWhitelist(entry, newWLNote.trim());
            showToast("success", `已添加白名单豁免: ${entry}`);
            setNewWLEntry("");
            setNewWLNote("");
            fetchWhitelistData();
            refetch();
        } catch (err: any) {
            showToast("error", `添加白名单失败: ${err.message || "格式错误"}`);
        } finally {
            setAddingWL(false);
        }
    };

    const handleDeleteWhitelist = async (entry: string) => {
        try {
            await deleteSecurityWhitelist(entry);
            showToast("success", `已移除白名单: ${entry}`);
            fetchWhitelistData();
            refetch();
        } catch (err: any) {
            showToast("error", `移除失败: ${err.message}`);
        }
    };

    const handleSaveConfig = async (e: React.FormEvent) => {
        e.preventDefault();
        setSavingCfg(true);
        try {
            await updateSecurityConfig({
                max_auth_failures: Number(cfgFailures),
                failure_window_mins: Number(cfgWindow),
                ban_duration_mins: Number(cfgBanDuration),
            });
            showToast("success", "防御中心策略参数已热更新生效！");
            refetch();
        } catch (err: any) {
            showToast("error", `保存策略失败: ${err.message}`);
        } finally {
            setSavingCfg(false);
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

    const pendingAttempts = overview?.pending_attempts ?? [];

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
                        多层入侵检测与自动化熔断系统：实时拦截恶意爆破、防自我锁死、持久化审计与白名单防护
                    </p>
                </div>

                <div className="flex flex-wrap items-center gap-2">
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
                        onClick={() => {
                            refetch();
                            if (activeTab === "events") fetchEventsData();
                            if (activeTab === "whitelist") fetchWhitelistData();
                        }}
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

            {/* Overview metric cards */}
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
                            持久化主动防护
                        </div>
                        <p className="mt-1 text-[11px] text-fg-muted">
                            阈值: {overview?.config?.max_auth_failures ?? 5}次 / 窗口: {overview?.config?.failure_window_mins ?? 5}分钟
                        </p>
                    </CardContent>
                </Card>

                {/* Metric 2 */}
                <Card className="glass-subtle hover-glow border-[rgb(var(--border)/0.4)]">
                    <CardHeader className="flex flex-row items-center justify-between pb-2">
                        <span className="text-xs font-medium text-fg-muted">
                            当前生效封禁
                        </span>
                        <div className="rounded-lg p-2 bg-rose-500/10 text-rose-500">
                            <Ban size={18} />
                        </div>
                    </CardHeader>
                    <CardContent>
                        <div className="text-2xl font-bold tracking-tight text-fg flex items-baseline gap-2">
                            <span>{overview?.total_banned_count ?? 0}</span>
                            <span className="text-[11px] font-normal text-fg-muted">
                                今日 +{overview?.bans_today ?? 0}
                            </span>
                        </div>
                        <p className="mt-1 text-[11px] text-fg-muted">
                            7天内累计新封禁: {overview?.bans_7d ?? 0} 个来源
                        </p>
                    </CardContent>
                </Card>

                {/* Metric 3 */}
                <Card className="glass-subtle hover-glow border-[rgb(var(--border)/0.4)]">
                    <CardHeader className="flex flex-row items-center justify-between pb-2">
                        <span className="text-xs font-medium text-fg-muted">
                            累计阻断拦截
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
                            重启/重建不丢失，数据库持久化存储
                        </p>
                    </CardContent>
                </Card>

                {/* Metric 4 */}
                <Card className="glass-subtle hover-glow border-[rgb(var(--border)/0.4)]">
                    <CardHeader className="flex flex-row items-center justify-between pb-2">
                        <span className="text-xs font-medium text-fg-muted">
                            观察中可疑尝试
                        </span>
                        <div className="rounded-lg p-2 bg-blue-500/10 text-blue-500">
                            <Activity size={18} />
                        </div>
                    </CardHeader>
                    <CardContent>
                        <div className="text-2xl font-bold tracking-tight text-fg flex items-baseline gap-2">
                            <span>{pendingAttempts.length}</span>
                            {pendingAttempts.length > 0 && (
                                <span className="text-[11px] font-medium text-amber-500">
                                    正在探测
                                </span>
                            )}
                        </div>
                        <p className="mt-1 text-[11px] text-fg-muted">
                            {pendingAttempts.length === 0 ? "暂无失败探测积累" : "窗口内失败正在累积"}
                        </p>
                    </CardContent>
                </Card>
            </div>

            {/* Pending attempts banner if any */}
            {pendingAttempts.length > 0 && (
                <div className="rounded-xl border border-amber-500/30 bg-amber-500/10 p-3 text-xs">
                    <div className="flex items-center gap-2 font-semibold text-amber-600 mb-1.5">
                        <Activity size={14} />
                        <span>观察窗口内可疑探测 IP ({pendingAttempts.length})</span>
                    </div>
                    <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-2">
                        {pendingAttempts.map((pa) => (
                            <div key={pa.ip} className="rounded-lg bg-bg-surface/60 p-2 border border-amber-500/20 flex justify-between items-center">
                                <div>
                                    <div className="flex items-center gap-1.5">
                                        <span className="font-mono font-medium text-fg">{pa.ip}</span>
                                        <a
                                            href={`https://ipinfo.io/${pa.ip}`}
                                            target="_blank"
                                            rel="noreferrer"
                                            className="text-fg-muted hover:text-primary transition-colors"
                                            title="在 ipinfo.io 查看情报"
                                        >
                                            <Globe size={11} />
                                        </a>
                                    </div>
                                    <div className="text-[10px] text-fg-muted mt-0.5">{pa.reason || "认证失败"}</div>
                                </div>
                                <div className="text-right">
                                    <Badge variant="outline" className="text-amber-600 border-amber-500/40 text-[10px]">
                                        {pa.count} 次失败
                                    </Badge>
                                    <div className="text-[10px] text-fg-muted mt-0.5">剩余 {pa.window_left_sec}s</div>
                                </div>
                            </div>
                        ))}
                    </div>
                </div>
            )}

            {/* 24h Attack Trends & Threat Analysis Grid */}
            <div className="grid grid-cols-1 lg:grid-cols-3 gap-4">
                {/* 24h Activity Mini-Chart (takes 2 columns on lg) */}
                <Card className="glass-subtle border-[rgb(var(--border)/0.4)] lg:col-span-2">
                    <CardHeader className="pb-2 flex flex-row items-center justify-between">
                        <div>
                            <CardTitle className="text-xs font-semibold flex items-center gap-1.5">
                                <TrendingUp size={15} className="text-primary" />
                                <span>24 小时阻断走势与攻击峰值</span>
                            </CardTitle>
                            <CardDescription className="text-[11px] mt-0.5">
                                过去 24 小时每个时间段的拦截阻断分布（近 24 小时活跃度）
                            </CardDescription>
                        </div>
                        <Badge variant="outline" className="text-[10px] text-fg-muted">
                            共计 24 小时数据
                        </Badge>
                    </CardHeader>
                    <CardContent className="pt-2">
                        {/* 24 bars chart */}
                        <div className="h-28 flex items-end gap-1.5 pt-4 pb-1 border-b border-[rgb(var(--border)/0.2)]">
                            {(overview?.blocked_hits_24h ?? Array(24).fill(0)).map((hits, idx) => {
                                const maxHits = Math.max(1, ...(overview?.blocked_hits_24h ?? [1]));
                                const heightPercent = Math.max(8, Math.round((hits / maxHits) * 100));
                                const hourLabel = `${(new Date().getHours() - 23 + idx + 24) % 24}:00`;
                                return (
                                    <div
                                        key={idx}
                                        className="flex-1 flex flex-col items-center group relative h-full justify-end"
                                    >
                                        {/* Tooltip on hover */}
                                        <div className="absolute -top-7 left-1/2 -translate-x-1/2 opacity-0 group-hover:opacity-100 transition-opacity bg-bg-surface/90 text-fg text-[10px] py-0.5 px-1.5 rounded shadow pointer-events-none whitespace-nowrap z-10 border border-[rgb(var(--border)/0.4)] font-mono">
                                            {hourLabel}: {hits} 次阻断
                                        </div>
                                        <div
                                            style={{ height: `${heightPercent}%` }}
                                            className={cn(
                                                "w-full rounded-t transition-all",
                                                hits === 0
                                                    ? "bg-[rgb(var(--bg-subtle)/0.5)]"
                                                    : hits > 10
                                                    ? "bg-rose-500 shadow-sm shadow-rose-500/30"
                                                    : "bg-primary/70 hover:bg-primary",
                                            )}
                                        />
                                    </div>
                                );
                            })}
                        </div>
                        <div className="flex justify-between items-center text-[10px] text-fg-muted mt-1.5 font-mono">
                            <span>24 小时前</span>
                            <span>12 小时前</span>
                            <span>当前</span>
                        </div>
                    </CardContent>
                </Card>

                {/* Threat Type Breakdown (1 column on lg) */}
                <Card className="glass-subtle border-[rgb(var(--border)/0.4)]">
                    <CardHeader className="pb-2">
                        <CardTitle className="text-xs font-semibold flex items-center gap-1.5">
                            <PieChart size={15} className="text-amber-500" />
                            <span>威胁分类分布 (近7天)</span>
                        </CardTitle>
                    </CardHeader>
                    <CardContent className="space-y-2.5 pt-1 text-xs">
                        {overview?.by_type && Object.keys(overview.by_type).length > 0 ? (
                            Object.entries(overview.by_type).map(([type, count]) => {
                                const total = Object.values(overview.by_type!).reduce((a, b) => a + b, 0) || 1;
                                const pct = Math.round((count / total) * 100);
                                const labelMap: Record<string, string> = {
                                    auth_fail: "暴力破解与认证失败",
                                    manual_ban: "管理员手动封禁",
                                    manual_unban: "管理员主动解封",
                                    config_change: "白名单及策略调整",
                                    gateway_warning: "网关穿透异常警报",
                                    test_alert: "测试警报邮件",
                                    audit_prune: "审计日志清理维护",
                                };
                                return (
                                    <div key={type} className="space-y-1">
                                        <div className="flex justify-between text-[11px]">
                                            <span className="text-fg font-medium">{labelMap[type] || type}</span>
                                            <span className="text-fg-muted font-mono">{count} 次 ({pct}%)</span>
                                        </div>
                                        <div className="h-1.5 w-full bg-[rgb(var(--bg-subtle)/0.5)] rounded-full overflow-hidden">
                                            <div
                                                style={{ width: `${pct}%` }}
                                                className={cn(
                                                    "h-full rounded-full",
                                                    type === "auth_fail"
                                                        ? "bg-rose-500"
                                                        : type.includes("ban")
                                                        ? "bg-amber-500"
                                                        : "bg-primary",
                                                )}
                                            />
                                        </div>
                                    </div>
                                );
                            })
                        ) : (
                            <div className="py-6 text-center text-fg-muted text-[11px]">
                                暂无威胁分类统计
                            </div>
                        )}
                    </CardContent>
                </Card>
            </div>

            {/* Top Attackers Highlight */}
            {overview?.top_attackers && overview.top_attackers.length > 0 && (
                <div className="rounded-xl border border-[rgb(var(--border)/0.4)] bg-bg-surface/50 p-3 text-xs">
                    <div className="flex items-center justify-between mb-2">
                        <div className="flex items-center gap-1.5 font-semibold text-rose-500 text-xs">
                            <Flame size={15} />
                            <span>Top 5 频繁侵扰攻击源 (高频阻断目标)</span>
                        </div>
                        <span className="text-[11px] text-fg-muted">已根据历史累计阻断次数降序排列</span>
                    </div>
                    <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-5 gap-2">
                        {overview.top_attackers.map((ta, idx) => (
                            <div
                                key={ta.ip}
                                className="rounded-lg bg-bg-surface/80 p-2.5 border border-rose-500/20 flex flex-col justify-between"
                            >
                                <div>
                                    <div className="flex items-center justify-between">
                                        <Badge variant="outline" className="text-[9px] px-1 py-0 h-4 border-rose-500/40 text-rose-600 bg-rose-500/10">
                                            TOP {idx + 1}
                                        </Badge>
                                        <a
                                            href={`https://ipinfo.io/${ta.ip}`}
                                            target="_blank"
                                            rel="noreferrer"
                                            className="text-fg-muted hover:text-primary transition-colors flex items-center gap-0.5 text-[10px]"
                                            title="查询威胁情报与归属地"
                                        >
                                            <Globe size={11} />
                                            <span>情报</span>
                                        </a>
                                    </div>
                                    <div className="font-mono font-medium text-fg mt-1 text-xs">{ta.ip}</div>
                                    <div className="text-[10px] text-fg-muted line-clamp-1 mt-0.5" title={ta.reason}>
                                        {ta.reason}
                                    </div>
                                </div>
                                <div className="mt-2 pt-1.5 border-t border-[rgb(var(--border)/0.2)] flex items-center justify-between">
                                    <span className="text-[10px] text-fg-muted font-mono">{ta.hits} 次阻断</span>
                                    <button
                                        type="button"
                                        onClick={() => handleUpgradeToPermanent(ta.ip)}
                                        className="text-[10px] text-amber-600 hover:text-amber-500 hover:underline"
                                        title="将该高频攻击源转为永久封禁"
                                    >
                                        永久封锁
                                    </button>
                                </div>
                            </div>
                        ))}
                    </div>
                </div>
            )}

            {/* Tabs Navigation */}
            <div className="flex border-b border-[rgb(var(--border)/0.3)] gap-2">
                <button
                    onClick={() => setActiveTab("bans")}
                    className={cn(
                        "py-2 px-3 text-xs font-medium border-b-2 transition-colors flex items-center gap-1.5",
                        activeTab === "bans"
                            ? "border-primary text-primary"
                            : "border-transparent text-fg-muted hover:text-fg",
                    )}
                >
                    <Ban size={14} />
                    <span>封禁拦截黑名单 ({activeBans.length})</span>
                </button>
                <button
                    onClick={() => setActiveTab("events")}
                    className={cn(
                        "py-2 px-3 text-xs font-medium border-b-2 transition-colors flex items-center gap-1.5",
                        activeTab === "events"
                            ? "border-primary text-primary"
                            : "border-transparent text-fg-muted hover:text-fg",
                    )}
                >
                    <Clock size={14} />
                    <span>安全事件审计日志</span>
                </button>
                <button
                    onClick={() => setActiveTab("whitelist")}
                    className={cn(
                        "py-2 px-3 text-xs font-medium border-b-2 transition-colors flex items-center gap-1.5",
                        activeTab === "whitelist"
                            ? "border-primary text-primary"
                            : "border-transparent text-fg-muted hover:text-fg",
                    )}
                >
                    <Shield size={14} />
                    <span>安全白名单</span>
                </button>
                <button
                    onClick={() => setActiveTab("config")}
                    className={cn(
                        "py-2 px-3 text-xs font-medium border-b-2 transition-colors flex items-center gap-1.5",
                        activeTab === "config"
                            ? "border-primary text-primary"
                            : "border-transparent text-fg-muted hover:text-fg",
                    )}
                >
                    <Sliders size={14} />
                    <span>防御策略配置</span>
                </button>
                <button
                    onClick={() => setActiveTab("host")}
                    className={cn(
                        "py-2 px-3 text-xs font-medium border-b-2 transition-colors flex items-center gap-1.5",
                        activeTab === "host"
                            ? "border-primary text-primary"
                            : "border-transparent text-fg-muted hover:text-fg",
                    )}
                >
                    <Terminal size={14} />
                    <span>NAS 宿主系统与 SSH 审计</span>
                </button>
            </div>

            {/* TAB 1: ACTIVE BANS */}
            {activeTab === "bans" && (
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
                                    黑名单在数据库持久化保存，被拦截 IP 在所有业务路由（Web、API、流媒体）中均被阻断
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
                                        placeholder="搜索 IP / 原因 / 来源"
                                        className="h-8 w-44 pl-7 text-xs"
                                    />
                                </div>
                                <select
                                    value={sortBy}
                                    onChange={(e) => setSortBy(e.target.value as "expiry" | "recent" | "hits")}
                                    className="h-8 rounded-lg border border-[rgb(var(--border)/0.4)] bg-transparent px-2 text-xs text-fg"
                                    aria-label="排序方式"
                                >
                                    <option value="expiry">按剩余时长</option>
                                    <option value="recent">按拦截时间</option>
                                    <option value="hits">按阻断次数</option>
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
                                            <th className="py-3 px-4 font-medium">来源</th>
                                            <th className="py-3 px-4 font-medium">触发原因 / 威胁类型</th>
                                            <th className="py-3 px-4 font-medium">阻断次数</th>
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
                                                            <a
                                                                href={`https://ipinfo.io/${ban.ip}`}
                                                                target="_blank"
                                                                rel="noreferrer"
                                                                title="在 ipinfo.io 查看威胁情报与地理位置"
                                                                className="text-fg-muted/70 hover:text-primary transition-colors flex items-center"
                                                            >
                                                                <ExternalLink size={11} />
                                                            </a>
                                                        </div>
                                                    </td>
                                                    <td className="py-3 px-4 text-fg-muted">
                                                        <Badge
                                                            variant="outline"
                                                            className={cn(
                                                                "text-[10px]",
                                                                ban.source === "manual"
                                                                    ? "border-amber-500/40 text-amber-600 bg-amber-500/10"
                                                                    : "border-rose-500/40 text-rose-600 bg-rose-500/10",
                                                            )}
                                                        >
                                                            {ban.source === "manual" ? "手动封禁" : "自动熔断"}
                                                        </Badge>
                                                    </td>
                                                    <td className="py-3 px-4 text-fg-muted">
                                                        <span className="inline-flex items-center gap-1.5 rounded-md px-2 py-0.5 bg-rose-500/10 text-rose-600 font-medium text-[11px] border border-rose-500/20">
                                                            {ban.reason || "触发恶意访问策略"}
                                                        </span>
                                                    </td>
                                                    <td className="py-3 px-4 font-mono font-medium text-fg">
                                                        {ban.hit_count ?? 0} 次
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
            )}

            {/* TAB 2: AUDIT EVENTS TIMELINE */}
            {activeTab === "events" && (
                <Card className="glass border-[rgb(var(--border)/0.4)]">
                    <CardHeader className="pb-3 border-b border-[rgb(var(--border)/0.2)]">
                        <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
                            <div>
                                <CardTitle className="text-base font-semibold flex items-center gap-2">
                                    <Clock size={18} className="text-blue-500" />
                                    <span>安全事件审计时间线 (最近 50 条)</span>
                                </CardTitle>
                                <CardDescription className="text-xs mt-0.5">
                                    详细记录每一次触发爆破、封禁、解封、策略热更新及测试操作
                                </CardDescription>
                            </div>
                            <div className="flex flex-wrap items-center gap-2">
                                <select
                                    value={eventFilterType}
                                    onChange={(e) => setEventFilterType(e.target.value)}
                                    className="h-8 rounded-lg border border-[rgb(var(--border)/0.4)] bg-transparent px-2 text-xs text-fg"
                                    aria-label="筛选事件类型"
                                >
                                    <option value="all">全部事件类型</option>
                                    <option value="auth_fail">认证失败与自动熔断</option>
                                    <option value="manual_ban">手动拦截封禁</option>
                                    <option value="manual_unban">解封操作</option>
                                    <option value="gateway_warning">网关穿透异常</option>
                                    <option value="config_change">策略与白名单变更</option>
                                    <option value="test_alert">测试警报</option>
                                    <option value="audit_prune">审计日志维护</option>
                                </select>
                                <Button
                                    variant="outline"
                                    size="sm"
                                    disabled={pruningEvents}
                                    onClick={() => handlePruneEvents(30)}
                                    className="h-8 gap-1.5 text-xs text-rose-500 hover:bg-rose-500/10"
                                    title="清理 30 天前的旧审计事件记录"
                                >
                                    <Trash2 size={13} />
                                    <span>清理 30 天前日志</span>
                                </Button>
                                <Button
                                    variant="outline"
                                    size="sm"
                                    onClick={fetchEventsData}
                                    disabled={eventsLoading}
                                    className="h-8 gap-1.5 text-xs"
                                >
                                    <RefreshCw size={13} className={cn(eventsLoading && "animate-spin")} />
                                    <span>刷新</span>
                                </Button>
                            </div>
                        </div>
                    </CardHeader>
                    <CardContent className="p-0">
                        {eventsLoading && events.length === 0 ? (
                            <div className="flex items-center justify-center py-12 text-fg-muted text-xs gap-2">
                                <Loader2 size={16} className="animate-spin text-primary" />
                                正在加载安全审计日志...
                            </div>
                        ) : filteredEvents.length === 0 ? (
                            <div className="flex flex-col items-center justify-center py-12 text-center text-fg-muted text-xs">
                                {events.length === 0 ? "暂无安全事件记录" : "没有符合当前筛选类型的事件"}
                            </div>
                        ) : (
                            <div className="overflow-x-auto">
                                <table className="w-full text-left text-xs">
                                    <thead className="border-b border-[rgb(var(--border)/0.2)] bg-[rgb(var(--bg-subtle)/0.3)] text-fg-muted">
                                        <tr>
                                            <th className="py-3 px-4 font-medium">时间</th>
                                            <th className="py-3 px-4 font-medium">相关来源</th>
                                            <th className="py-3 px-4 font-medium">事件类型</th>
                                            <th className="py-3 px-4 font-medium">执行处置</th>
                                            <th className="py-3 px-4 font-medium">详情说明</th>
                                        </tr>
                                    </thead>
                                    <tbody className="divide-y divide-[rgb(var(--border)/0.15)]">
                                        {filteredEvents.map((ev) => (
                                            <tr key={ev.id} className="transition-colors hover:bg-[rgb(var(--bg-subtle)/0.25)]">
                                                <td className="py-3 px-4 text-fg-muted whitespace-nowrap">
                                                    {formatDateTime(ev.created_at)}
                                                </td>
                                                <td className="py-3 px-4 font-mono font-medium text-fg whitespace-nowrap">
                                                    <div className="flex items-center gap-1.5">
                                                        <span>{ev.ip}</span>
                                                        {isValidIp(ev.ip) && (
                                                            <a
                                                                href={`https://ipinfo.io/${ev.ip}`}
                                                                target="_blank"
                                                                rel="noreferrer"
                                                                className="text-fg-muted hover:text-primary transition-colors"
                                                                title="查询情报"
                                                            >
                                                                <ExternalLink size={10} />
                                                            </a>
                                                        )}
                                                    </div>
                                                </td>
                                                <td className="py-3 px-4">
                                                    <Badge
                                                        variant="outline"
                                                        className={cn(
                                                            "text-[10px]",
                                                            ev.type.includes("ban")
                                                                ? "border-rose-500/40 text-rose-600 bg-rose-500/10"
                                                                : ev.type.includes("unban")
                                                                ? "border-emerald-500/40 text-emerald-600 bg-emerald-500/10"
                                                                : ev.type.includes("alert")
                                                                ? "border-amber-500/40 text-amber-600 bg-amber-500/10"
                                                                : "border-blue-500/40 text-blue-600 bg-blue-500/10",
                                                        )}
                                                    >
                                                        {ev.type}
                                                    </Badge>
                                                </td>
                                                <td className="py-3 px-4 font-medium text-fg whitespace-nowrap">
                                                    {ev.action}
                                                </td>
                                                <td className="py-3 px-4 text-fg-muted max-w-md break-all">
                                                    {ev.detail}
                                                </td>
                                            </tr>
                                        ))}
                                    </tbody>
                                </table>
                            </div>
                        )}
                    </CardContent>
                </Card>
            )}

            {/* TAB 3: DYNAMIC WHITELIST */}
            {activeTab === "whitelist" && (
                <div className="space-y-4">
                    <Card className="glass border-[rgb(var(--border)/0.4)]">
                        <CardHeader className="pb-3 border-b border-[rgb(var(--border)/0.2)]">
                            <CardTitle className="text-base font-semibold flex items-center gap-2">
                                <Plus size={16} className="text-emerald-500" />
                                <span>添加白名单豁免</span>
                            </CardTitle>
                            <CardDescription className="text-xs">
                                加入白名单的 IP 或 CIDR 网段不会被自动化入侵检测机制熔断封禁
                            </CardDescription>
                        </CardHeader>
                        <CardContent className="pt-4">
                            <form onSubmit={handleAddWhitelist} className="flex flex-col sm:flex-row gap-2">
                                <Input
                                    value={newWLEntry}
                                    onChange={(e) => setNewWLEntry(e.target.value)}
                                    placeholder="IP 或 CIDR，例如 192.168.1.100 或 10.0.0.0/8"
                                    className="font-mono text-xs flex-1"
                                    required
                                />
                                <Input
                                    value={newWLNote}
                                    onChange={(e) => setNewWLNote(e.target.value)}
                                    placeholder="备注说明，例如：开发测试机"
                                    className="text-xs sm:w-60"
                                />
                                <Button
                                    type="submit"
                                    disabled={addingWL}
                                    className="bg-emerald-600 hover:bg-emerald-700 text-white text-xs gap-1.5"
                                >
                                    {addingWL ? <Loader2 size={13} className="animate-spin" /> : <Plus size={14} />}
                                    <span>添加豁免</span>
                                </Button>
                            </form>
                        </CardContent>
                    </Card>

                    <Card className="glass border-[rgb(var(--border)/0.4)]">
                        <CardHeader className="pb-3 border-b border-[rgb(var(--border)/0.2)]">
                            <CardTitle className="text-base font-semibold flex items-center gap-2">
                                <Shield size={18} className="text-emerald-500" />
                                <span>当前生效的白名单列表</span>
                            </CardTitle>
                        </CardHeader>
                        <CardContent className="p-0">
                            {whitelistLoading && whitelist.length === 0 ? (
                                <div className="flex items-center justify-center py-10 text-fg-muted text-xs gap-2">
                                    <Loader2 size={16} className="animate-spin text-primary" />
                                    载入白名单列表中...
                                </div>
                            ) : (
                                <div className="overflow-x-auto">
                                    <table className="w-full text-left text-xs">
                                        <thead className="border-b border-[rgb(var(--border)/0.2)] bg-[rgb(var(--bg-subtle)/0.3)] text-fg-muted">
                                            <tr>
                                                <th className="py-3 px-4 font-medium">豁免地址 / 网段</th>
                                                <th className="py-3 px-4 font-medium">备注说明</th>
                                                <th className="py-3 px-4 font-medium">类型</th>
                                                <th className="py-3 px-4 font-medium">添加时间</th>
                                                <th className="py-3 px-4 font-medium text-right">操作</th>
                                            </tr>
                                        </thead>
                                        <tbody className="divide-y divide-[rgb(var(--border)/0.15)]">
                                            {/* Static config whitelists */}
                                            {overview?.config?.whitelist?.map((entry) => (
                                                <tr key={entry} className="transition-colors hover:bg-[rgb(var(--bg-subtle)/0.25)]">
                                                    <td className="py-3 px-4 font-mono font-medium text-fg">{entry}</td>
                                                    <td className="py-3 px-4 text-fg-muted">配置文件全局预设</td>
                                                    <td className="py-3 px-4">
                                                        <Badge variant="outline" className="text-[10px]">静态预设</Badge>
                                                    </td>
                                                    <td className="py-3 px-4 text-fg-muted">-</td>
                                                    <td className="py-3 px-4 text-right text-fg-muted text-[11px]">系统保护</td>
                                                </tr>
                                            ))}
                                            {/* Dynamic DB whitelists */}
                                            {whitelist.map((w) => (
                                                <tr key={w.entry} className="transition-colors hover:bg-[rgb(var(--bg-subtle)/0.25)]">
                                                    <td className="py-3 px-4 font-mono font-medium text-fg">{w.entry}</td>
                                                    <td className="py-3 px-4 text-fg-muted">{w.note || "-"}</td>
                                                    <td className="py-3 px-4">
                                                        <Badge variant="outline" className="border-emerald-500/40 text-emerald-600 text-[10px]">动态配置</Badge>
                                                    </td>
                                                    <td className="py-3 px-4 text-fg-muted">{formatDateTime(w.created_at)}</td>
                                                    <td className="py-3 px-4 text-right">
                                                        <Button
                                                            variant="ghost"
                                                            size="sm"
                                                            onClick={() => handleDeleteWhitelist(w.entry)}
                                                            className="text-xs text-rose-500 hover:bg-rose-500/10 h-7 px-2 gap-1"
                                                        >
                                                            <Trash2 size={12} />
                                                            <span>移除</span>
                                                        </Button>
                                                    </td>
                                                </tr>
                                            ))}
                                        </tbody>
                                    </table>
                                </div>
                            )}
                        </CardContent>
                    </Card>
                </div>
            )}

            {/* TAB 4: CONFIGURATION */}
            {activeTab === "config" && (
                <Card className="glass border-[rgb(var(--border)/0.4)] max-w-xl">
                    <CardHeader className="pb-3 border-b border-[rgb(var(--border)/0.2)]">
                        <CardTitle className="text-base font-semibold flex items-center gap-2">
                            <Sliders size={18} className="text-primary" />
                            <span>主动入侵防御策略配置</span>
                        </CardTitle>
                        <CardDescription className="text-xs">
                            参数修改后即时保存并热加载生效，无需重启服务
                        </CardDescription>
                    </CardHeader>
                    <CardContent className="pt-4">
                        <form onSubmit={handleSaveConfig} className="space-y-4 text-xs">
                            <div>
                                <label className="block font-medium text-fg mb-1">
                                    最大连续认证失败次数 (阈值)
                                </label>
                                <Input
                                    type="number"
                                    min="3"
                                    max="50"
                                    value={cfgFailures}
                                    onChange={(e) => setCfgFailures(Number(e.target.value))}
                                    className="text-xs"
                                    required
                                />
                                <p className="mt-1 text-[11px] text-fg-muted">
                                    安全保护下限：不得低于 3 次（防止手误导致自己被误封）
                                </p>
                            </div>

                            <div>
                                <label className="block font-medium text-fg mb-1">
                                    失败统计观察窗口 (分钟)
                                </label>
                                <Input
                                    type="number"
                                    min="1"
                                    max="120"
                                    value={cfgWindow}
                                    onChange={(e) => setCfgWindow(Number(e.target.value))}
                                    className="text-xs"
                                    required
                                />
                                <p className="mt-1 text-[11px] text-fg-muted">
                                    在此时间窗口内的连续失败将被累计计数，超时后自动重置。
                                </p>
                            </div>

                            <div>
                                <label className="block font-medium text-fg mb-1">
                                    自动触发熔断封禁时长 (分钟)
                                </label>
                                <Input
                                    type="number"
                                    min="1"
                                    max="10080"
                                    value={cfgBanDuration}
                                    onChange={(e) => setCfgBanDuration(Number(e.target.value))}
                                    className="text-xs"
                                    required
                                />
                                <p className="mt-1 text-[11px] text-fg-muted">
                                    达到阈值后自动封禁的时长，默认 30 分钟。
                                </p>
                            </div>

                            <div className="pt-3 border-t border-[rgb(var(--border)/0.2)]">
                                <Button
                                    type="submit"
                                    disabled={savingCfg}
                                    className="bg-primary hover:bg-primary/90 text-white text-xs gap-1.5"
                                >
                                    {savingCfg ? <Loader2 size={13} className="animate-spin" /> : <CheckCircle2 size={14} />}
                                    <span>保存并应用策略</span>
                                </Button>
                            </div>
                        </form>
                    </CardContent>
                </Card>
            )}

            {/* TAB 5: HOST OS LOGS & SSH AUDIT */}
            {activeTab === "host" && (
                <div className="space-y-4">
                    {/* Status & Sync Card */}
                    <Card className="glass border-[rgb(var(--border)/0.4)]">
                        <CardHeader className="pb-3 border-b border-[rgb(var(--border)/0.2)]">
                            <div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
                                <div>
                                    <CardTitle className="text-base font-semibold flex items-center gap-2">
                                        <Terminal size={18} className="text-purple-500" />
                                        <span>NAS 宿主系统认证与网络层防火墙联动</span>
                                    </CardTitle>
                                    <CardDescription className="text-xs mt-0.5">
                                        监听宿主机底层（Debian / fnOS）/var/log/auth.log 实时捕获 SSH 爆破，并导出规则供 iptables/ipset 消费
                                    </CardDescription>
                                </div>
                                <Button
                                    variant="outline"
                                    size="sm"
                                    onClick={fetchHostLogData}
                                    disabled={hostLogLoading}
                                    className="h-8 gap-1.5 text-xs"
                                >
                                    <RefreshCw size={13} className={cn(hostLogLoading && "animate-spin")} />
                                    <span>刷新宿主日志</span>
                                </Button>
                            </div>
                        </CardHeader>
                        <CardContent className="pt-4">
                            <div className="grid grid-cols-1 md:grid-cols-3 gap-3 text-xs">
                                <div className="rounded-xl border border-[rgb(var(--border)/0.3)] bg-[rgb(var(--bg-subtle)/0.3)] p-3">
                                    <span className="text-fg-muted font-medium text-[11px]">宿主日志监听状态</span>
                                    <div className="mt-1 flex items-center gap-1.5">
                                        <span className={cn("h-2 w-2 rounded-full", hostLogData?.enabled ? "bg-emerald-500 animate-pulse" : "bg-amber-500")} />
                                        <span className="font-semibold text-fg">
                                            {hostLogData?.enabled ? "实时监听中 (Tail Active)" : "未挂载 / 待机模式"}
                                        </span>
                                    </div>
                                    <p className="mt-1 text-[10px] text-fg-muted font-mono truncate" title={hostLogData?.source_path}>
                                        源路径: {hostLogData?.source_path || "/host/log/auth.log"}
                                    </p>
                                </div>

                                <div className="rounded-xl border border-[rgb(var(--border)/0.3)] bg-[rgb(var(--bg-subtle)/0.3)] p-3">
                                    <span className="text-fg-muted font-medium text-[11px]">宿主防火墙规则导出</span>
                                    <div className="mt-1 flex items-center gap-1.5">
                                        <span className="h-2 w-2 rounded-full bg-blue-500" />
                                        <span className="font-semibold text-fg">iptables / ipset 格式</span>
                                    </div>
                                    <p className="mt-1 text-[10px] text-fg-muted font-mono truncate" title={hostLogData?.export_path}>
                                        目标: {hostLogData?.export_path || "/data/security/blocked_ips.txt"}
                                    </p>
                                </div>

                                <div className="rounded-xl border border-[rgb(var(--border)/0.3)] bg-[rgb(var(--bg-subtle)/0.3)] p-3">
                                    <span className="text-fg-muted font-medium text-[11px]">宿主网络层 (L3/L4) 联动</span>
                                    <div className="mt-1 text-fg text-[11px]">
                                        黑名单更新时同步写入文件，宿主机可通过计划任务自动丢包（DROP 拦截 22/5666 等全部端口）。
                                    </div>
                                </div>
                            </div>
                        </CardContent>
                    </Card>

                    {/* SSH Authentication Audit Table */}
                    <Card className="glass border-[rgb(var(--border)/0.4)]">
                        <CardHeader className="pb-3 border-b border-[rgb(var(--border)/0.2)]">
                            <CardTitle className="text-base font-semibold flex items-center gap-2">
                                <Server size={18} className="text-purple-500" />
                                <span>宿主机 SSH 认证审计日志</span>
                                <Badge variant="default" className="text-[11px]">
                                    {hostLogData?.entries.length ?? 0} 条记录
                                </Badge>
                            </CardTitle>
                            <CardDescription className="text-xs">
                                实时捕获宿主机 SSH 登录成功与暴力破解尝试，失败多次将自动触发全系统阻断
                            </CardDescription>
                        </CardHeader>
                        <CardContent className="p-0">
                            {hostLogLoading && !hostLogData ? (
                                <div className="flex items-center justify-center py-12 text-fg-muted text-xs gap-2">
                                    <Loader2 size={16} className="animate-spin text-primary" />
                                    正在读取宿主操作系统认证记录...
                                </div>
                            ) : !hostLogData || hostLogData.entries.length === 0 ? (
                                <div className="flex flex-col items-center justify-center py-12 text-center text-fg-muted text-xs">
                                    <div className="flex h-10 w-10 items-center justify-center rounded-full bg-[rgb(var(--bg-subtle)/0.5)] text-fg-muted mb-2">
                                        <Terminal size={20} />
                                    </div>
                                    <p className="font-medium text-fg">暂无宿主机 SSH 认证记录</p>
                                    <p className="text-[11px] text-fg-muted mt-1 max-w-sm">
                                        {hostLogData?.enabled
                                            ? "宿主机 auth.log 已连接，当前尚未捕获到新的 SSH 登录或暴力破解记录。"
                                            : "宿主机 /var/log/auth.log 尚未挂载到容器，可在 compose.yaml 中启用宿主机认证日志挂载。"}
                                    </p>
                                </div>
                            ) : (
                                <div className="overflow-x-auto">
                                    <table className="w-full text-left text-xs">
                                        <thead className="border-b border-[rgb(var(--border)/0.2)] bg-[rgb(var(--bg-subtle)/0.3)] text-fg-muted">
                                            <tr>
                                                <th className="py-3 px-4 font-medium">捕获时间</th>
                                                <th className="py-3 px-4 font-medium">来源 IP</th>
                                                <th className="py-3 px-4 font-medium">尝试用户</th>
                                                <th className="py-3 px-4 font-medium">目标端口</th>
                                                <th className="py-3 px-4 font-medium">认证结果</th>
                                                <th className="py-3 px-4 font-medium">底层详情</th>
                                                <th className="py-3 px-4 font-medium text-right">处置</th>
                                            </tr>
                                        </thead>
                                        <tbody className="divide-y divide-[rgb(var(--border)/0.15)]">
                                            {hostLogData.entries.map((entry, idx) => (
                                                <tr key={idx} className="transition-colors hover:bg-[rgb(var(--bg-subtle)/0.25)]">
                                                    <td className="py-3 px-4 text-fg-muted whitespace-nowrap">
                                                        {formatDateTime(entry.timestamp)}
                                                    </td>
                                                    <td className="py-3 px-4 font-mono font-medium text-fg whitespace-nowrap">
                                                        <div className="flex items-center gap-1.5">
                                                            <span>{entry.ip}</span>
                                                            {isValidIp(entry.ip) && (
                                                                <a
                                                                    href={`https://ipinfo.io/${entry.ip}`}
                                                                    target="_blank"
                                                                    rel="noreferrer"
                                                                    className="text-fg-muted hover:text-primary transition-colors"
                                                                    title="查询情报"
                                                                >
                                                                    <ExternalLink size={10} />
                                                                </a>
                                                            )}
                                                        </div>
                                                    </td>
                                                    <td className="py-3 px-4 font-mono text-fg">
                                                        {entry.user}
                                                    </td>
                                                    <td className="py-3 px-4 font-mono text-fg-muted">
                                                        {entry.port}
                                                    </td>
                                                    <td className="py-3 px-4 whitespace-nowrap">
                                                        {entry.status === "accepted" ? (
                                                            <Badge variant="outline" className="border-emerald-500/40 text-emerald-600 bg-emerald-500/10 text-[10px]">
                                                                登录成功
                                                            </Badge>
                                                        ) : entry.status === "invalid_user" ? (
                                                            <Badge variant="outline" className="border-amber-500/40 text-amber-600 bg-amber-500/10 text-[10px]">
                                                                非法用户名
                                                            </Badge>
                                                        ) : (
                                                            <Badge variant="outline" className="border-rose-500/40 text-rose-600 bg-rose-500/10 text-[10px]">
                                                                密码错误
                                                            </Badge>
                                                        )}
                                                    </td>
                                                    <td className="py-3 px-4 text-fg-muted max-w-sm truncate" title={entry.raw_message}>
                                                        {entry.raw_message}
                                                    </td>
                                                    <td className="py-3 px-4 text-right whitespace-nowrap">
                                                        {entry.status !== "accepted" && (
                                                            <Button
                                                                variant="ghost"
                                                                size="sm"
                                                                onClick={() => {
                                                                    setBanIP(entry.ip);
                                                                    setBanReason(`宿主 SSH 恶意撞库 (尝试用户: ${entry.user})`);
                                                                    setShowBanModal(true);
                                                                }}
                                                                className="text-xs text-rose-500 hover:bg-rose-500/10 h-7 px-2 gap-1"
                                                            >
                                                                <Ban size={12} />
                                                                <span>封禁</span>
                                                            </Button>
                                                        )}
                                                    </td>
                                                </tr>
                                            ))}
                                        </tbody>
                                    </table>
                                </div>
                            )}
                        </CardContent>
                    </Card>
                </div>
            )}

            {/* Defense Policies & Architecture Details */}
            <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
                <Card className="glass-subtle border-[rgb(var(--border)/0.3)]">
                    <CardHeader className="pb-2.5">
                        <CardTitle className="text-sm font-semibold flex items-center gap-2">
                            <Lock size={16} className="text-amber-500" />
                            <span>主动入侵防护与自我保护</span>
                        </CardTitle>
                    </CardHeader>
                    <CardContent className="space-y-2 text-xs text-fg-muted leading-relaxed">
                        <div className="flex items-start gap-2">
                            <span className="font-semibold text-fg">1. 状态全量持久化:</span>
                            <span>
                                黑名单、拦截计数及审计事件均写入 SQLite 数据库，服务升级重启不丢状态。
                            </span>
                        </div>
                        <div className="flex items-start gap-2">
                            <span className="font-semibold text-fg">2. 管理员防自我锁死:</span>
                            <span>
                                严禁封禁回环地址、当前登录操作 IP 及白名单网关，杜绝误封失联。
                            </span>
                        </div>
                        <div className="flex items-start gap-2">
                            <span className="font-semibold text-fg">3. 告警去重与防风暴:</span>
                            <span>
                                同一 IP 告警邮件设有 10 分钟冷却期；白名单设备告警后自动清零失败计数。
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
                                静态配置与动态添加相结合，支持单 IP 或 CIDR 掩码段豁免。
                            </span>
                        </div>
                        <div className="flex items-start gap-2">
                            <span className="font-semibold text-fg">邮件即时通知:</span>
                            <span>
                                触发黑名单熔断时，通过 QQ 邮箱 SMTP 即时推送 HTML 预警至{" "}
                                <span className="font-mono text-fg">{overview?.recipient || "3318647421@qq.com"}</span>。
                            </span>
                        </div>
                        <div className="flex items-start gap-2">
                            <span className="font-semibold text-fg">事件总线联动:</span>
                            <span>
                                威胁事件同步发布至 EventBus，前端仪表盘及系统审计日志实时呈现告警流。
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
