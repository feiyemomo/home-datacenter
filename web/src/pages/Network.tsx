import { useState, useCallback, useEffect } from "react";
import { useCachedFetch } from "@/hooks/useCachedFetch";
import {
    Globe,
    Network as NetworkIcon,
    Shield,
    Star,
    RefreshCw,
    Wifi,
    ArrowRight,
    ArrowUp,
    CheckCircle2,
    XCircle,
    Smartphone,
    Server,
    Zap,
    ExternalLink,
    Copy,
    Check,
    Clock,
} from "lucide-react";
import { getNetworkStatus, checkClientIPv6 } from "@/api/network";
import { getToken } from "@/api/client";
import type { ConnectionStrategy } from "@/types";
import {
    Card,
    CardContent,
    CardDescription,
    CardHeader,
    CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ErrorRetry } from "@/components/ErrorRetry";
import { isOnRelay } from "@/lib/network";

/**
 * Network page: displays the server's network capability report.
 *
 * Shows IPv6 availability, NAT type, P2P feasibility, relay status,
 * and the recommended connection strategy with a quality rating.
 *
 * The connection model is "Relay First, Then Upgrade":
 *   1. Client connects via Relay (Cloudflare Tunnel) immediately — zero delay.
 *   2. Client probes the `strategy` path (IPv6 direct or P2P) in background.
 *   3. If the probe succeeds, the client upgrades to the better path.
 */
export default function Network() {
    const [error, _setError] = useState<string | null>(null);

    const fetchNetworkData = useCallback(async () => {
        const [s, c] = await Promise.all([
            getNetworkStatus(false),
            checkClientIPv6(),
        ]);
        return { status: s, clientIPv6: c };
    }, []);

    const { data, loading, error: fetchError, refetch } = useCachedFetch(
        "home.network.status",
        fetchNetworkData,
        { refetchMs: 60000 },
    );

    const status = data?.status ?? null;
    const clientIPv6 = data?.clientIPv6 ?? null;

    const strategyLabel: Record<ConnectionStrategy, string> = {
        ipv6_direct: "IPv6 直连",
        p2p: "P2P UDP",
        relay: "中继（隧道）",
    };

    const [h3cData, setH3cData] = useState<{
        status: string;
        externalAddr?: string;
        tunnelId?: string;
        closeTime?: string;
        remainingMinutes?: number;
        lastChecked?: string;
    } | null>(null);
    const [h3cLoading, setH3cLoading] = useState(false);
    const [h3cError, setH3cError] = useState(false);
    const [copied, setCopied] = useState(false);

    const fetchH3c = useCallback(async () => {
        try {
            const controller = new AbortController();
            const timer = setTimeout(() => controller.abort(), 6000);
            const res = await fetch("/fast-status", { signal: controller.signal });
            clearTimeout(timer);
            if (res.ok) {
                const data = await res.json();
                setH3cData(data);
                setH3cError(false);
            } else {
                setH3cError(true);
            }
        } catch {
            setH3cError(true);
        }
    }, []);

    useEffect(() => {
        fetchH3c();
        const interval = setInterval(fetchH3c, 30000);
        return () => clearInterval(interval);
    }, [fetchH3c]);

    const handleRefreshH3c = async () => {
        setH3cLoading(true);
        try {
            const res = await fetch("/fast-refresh", { method: "POST" });
            if (res.ok) {
                const data = await res.json();
                setH3cData(data);
                setH3cError(false);
            }
        } catch (e) {
            console.error(e);
            setH3cError(true);
        } finally {
            setH3cLoading(false);
        }
    };

    const handleCopy = (text: string) => {
        navigator.clipboard.writeText(text);
        setCopied(true);
        setTimeout(() => setCopied(false), 2000);
    };

    // Whether an upgrade from relay is available.
    const hasUpgrade = status != null && status.strategy !== status.initial;

    // IPv6 feature flag: upstream optical modem has no IPv6 enabled currently.
    // Kept dormant for future re-enablement.
    const SHOW_IPV6 = false;

    // IPv6 direct is possible only if BOTH server and client have IPv6.
    const ipv6DirectPossible =
        SHOW_IPV6 && status?.ipv6?.reachable === true && clientIPv6 === true;

    // Whether to show the clickable "switch to IPv6 direct" link.
    // Requires: user is on relay, server has IPv6, client has IPv6.
    const canSwitchToIPv6Direct =
        SHOW_IPV6 && isOnRelay() && ipv6DirectPossible && status?.ipv6?.address != null;

    return (
        <div className="animate-fade-in space-y-6">
            {/* Header */}
            <div className="animate-fade-in flex items-center justify-between pb-4 mb-2 border-b border-[rgb(var(--border)/0.3)]">
                <div>
                    <h2 className="text-lg font-semibold text-fg">网络</h2>
                    <p className="text-xs text-fg-muted">
                        服务器网络能力与连接策略。
                    </p>
                </div>
                <Button
                    variant="outline"
                    size="sm"
                    onClick={() => refetch()}
                    disabled={loading}
                >
                    <RefreshCw size={14} className={loading ? "animate-spin" : ""} />
                    刷新
                </Button>
            </div>

            {(error || fetchError) && (
                <ErrorRetry message={error || fetchError?.message || ""} onRetry={refetch} />
            )}

            {/* Connection model: Relay First, Then Upgrade */}
            <Card>
                <CardHeader className="flex-row items-center justify-between">
                    <div>
                        <CardTitle className="flex items-center gap-2">
                            <Wifi size={16} /> 连接模型
                        </CardTitle>
                        <CardDescription>
                            中继优先：立即连接，然后探测并升级。
                        </CardDescription>
                    </div>
                    {status && (
                        <div className="flex items-center gap-1">
                            {[1, 2, 3, 4, 5].map((n) => (
                                <Star
                                    key={n}
                                    size={20}
                                    className={
                                        n <= status.quality
                                            ? "fill-[rgb(var(--accent-warm))] text-[rgb(var(--accent-warm))]"
                                            : "fill-none text-fg-subtle"
                                    }
                                />
                            ))}
                        </div>
                    )}
                </CardHeader>
                <CardContent>
                    {status ? (
                        <div className="space-y-3">
                            {/* Step 1: Relay (initial) */}
                            <div className="glass-subtle rounded-xl border-[rgb(var(--accent-success)/0.3)] bg-[rgb(var(--accent-success)/0.05)] flex items-center gap-3 px-4 py-3">
                                <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-[rgb(var(--accent-success))] text-sm font-bold text-white">
                                    1
                                </div>
                                <div className="min-w-0 flex-1">
                                    <p className="text-sm font-medium text-fg">
                                        通过中继连接
                                    </p>
                                    <p className="text-xs text-fg-muted">
                                        Cloudflare 隧道 — 零延迟，始终可用
                                    </p>
                                </div>
                                <Badge variant="success">使用中</Badge>
                            </div>

                            {/* Arrow down */}
                            <div className="flex justify-center">
                                <ArrowRight size={16} className="rotate-90 text-fg-subtle" />
                            </div>

                            {/* Step 2: Probe & Upgrade */}
                            <div
                                className={`glass-subtle rounded-xl flex items-center gap-3 border px-4 py-3 ${
                                    hasUpgrade
                                        ? "border-[rgb(var(--accent-primary)/0.3)] bg-[rgb(var(--accent-primary)/0.05)]"
                                        : "border-[rgb(var(--border)/0.3)] bg-[rgb(var(--bg-subtle)/0.1)]"
                                }`}
                            >
                                <div
                                    className={`flex h-8 w-8 shrink-0 items-center justify-center rounded-full text-sm font-bold ${
                                        hasUpgrade
                                            ? "bg-[rgb(var(--accent-info))] text-white"
                                            : "bg-[rgb(var(--glass-base))] text-fg-muted"
                                    }`}
                                >
                                    2
                                </div>
                                <div className="min-w-0 flex-1">
                                    <p className="text-sm font-medium text-fg">
                                        {hasUpgrade
                                            ? `探测并升级到 ${strategyLabel[status.strategy]}`
                                            : "无可用升级"}
                                    </p>
                                    <p className="text-xs text-fg-muted">
                                        {hasUpgrade
                                            ? status.strategy === "ipv6_direct"
                                                ? "后台探测服务器公网 IPv6 — 可达则切换"
                                                : "通过 STUN 后台 UDP 打洞 — 建立则切换"
                                            : "服务器无 IPv6 或 P2P 能力 — 中继是唯一路径"}
                                    </p>
                                    {canSwitchToIPv6Direct && (
                                        <a
                                            href="http://nas.feiyemomo.top:8088/"
                                            target="_blank"
                                            rel="noopener noreferrer"
                                            className="mt-1.5 inline-flex items-center gap-1 text-xs font-medium text-[rgb(var(--accent-info))] hover:underline"
                                        >
                                            切换到 IPv6 直连 →
                                        </a>
                                    )}
                                </div>
                                {hasUpgrade ? (
                                    <Badge variant="info">
                                        <ArrowUp size={12} className="mr-1" />
                                        升级
                                    </Badge>
                                ) : (
                                    <Badge variant="outline">不适用</Badge>
                                )}
                            </div>

                            <div className="text-xs text-fg-subtle">
                                最后检查：{new Date(status.checked_at).toLocaleString()}
                            </div>
                        </div>
                    ) : (
                        <div className="text-sm text-fg-muted">加载中...</div>
                    )}
                </CardContent>
            </Card>

            {/* IPv6 side-by-side: Server vs Client (Hidden while upstream IPv6 is unavailable) */}
            {SHOW_IPV6 && (
                <Card>
                    <CardHeader>
                        <CardTitle className="flex items-center gap-2">
                            <Globe size={16} /> IPv6 连通性
                        </CardTitle>
                        <CardDescription>
                            IPv6 直连要求服务器和客户端都具备公网 IPv6。
                        </CardDescription>
                    </CardHeader>
                    <CardContent>
                        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                            {/* Server side */}
                            <div className="glass-subtle rounded-xl p-4">
                                <div className="mb-2 flex items-center gap-2">
                                    <Server size={14} className="text-fg-muted" />
                                    <span className="text-xs font-medium uppercase tracking-wider text-fg-muted">
                                        服务器
                                    </span>
                                </div>
                                {status?.ipv6 ? (
                                    <div className="space-y-2">
                                        <div className="flex items-center gap-2">
                                            {status.ipv6.reachable ? (
                                                <CheckCircle2 size={16} className="text-[rgb(var(--accent-success))]" />
                                            ) : (
                                                <XCircle size={16} className="text-[rgb(var(--accent-danger))]" />
                                            )}
                                            <span className="text-sm text-fg">
                                                {status.ipv6.reachable
                                                    ? "公网 IPv6 可达"
                                                    : status.ipv6.enabled
                                                        ? "IPv6 已启用但公网不可达"
                                                        : "IPv6 不可用"}
                                            </span>
                                        </div>
                                        {status.ipv6.address && (
                                            <div className="space-y-1">
                                                <code className="font-mono text-xs text-fg-muted">
                                                    {status.ipv6.address}
                                                </code>
                                                <div className="rounded-lg bg-[rgb(var(--accent-success)/0.08)] px-2.5 py-1.5">
                                                    <p className="text-[11px] text-fg-muted">IPv6 直连地址：</p>
                                                    <code className="font-mono text-[11px] text-[rgb(var(--accent-success))] break-all">
                                                        http://[{status.ipv6.address}]:8088/
                                                    </code>
                                                </div>
                                            </div>
                                        )}
                                    </div>
                                ) : (
                                    <div className="text-sm text-fg-muted">加载中...</div>
                                )}
                            </div>

                            {/* Client side */}
                            <div className="glass-subtle rounded-xl p-4">
                                <div className="mb-2 flex items-center gap-2">
                                    <Smartphone size={14} className="text-fg-muted" />
                                    <span className="text-xs font-medium uppercase tracking-wider text-fg-muted">
                                        您的设备
                                    </span>
                                </div>
                                <div className="space-y-2">
                                    <div className="flex items-center gap-2">
                                        {clientIPv6 === null ? (
                                            <RefreshCw size={16} className="animate-spin text-fg-subtle" />
                                        ) : clientIPv6 ? (
                                            <CheckCircle2 size={16} className="text-[rgb(var(--accent-success))]" />
                                        ) : (
                                            <XCircle size={16} className="text-[rgb(var(--accent-danger))]" />
                                        )}
                                        <span className="text-sm text-fg">
                                            {clientIPv6 === null
                                                ? "检查中..."
                                                : clientIPv6
                                                    ? "IPv6 可用"
                                                    : "IPv6 不可用"}
                                        </span>
                                    </div>
                                    <p className="text-xs text-fg-muted">
                                        {clientIPv6 === false
                                            ? "您的网络无 IPv6 — 只能使用中继"
                                            : clientIPv6 === true && !status?.ipv6?.reachable
                                                ? "您具备 IPv6，但服务器没有 — IPv6 直连被服务器阻挡"
                                                : ipv6DirectPossible
                                                    ? "双方都具备 IPv6 — 可以直连"
                                                    : ""}
                                    </p>
                                </div>
                            </div>
                        </div>
                    </CardContent>
                </Card>
            )}

            {/* H3C Oasis Domestic High-Speed Gateway */}
            <Card className="border-[rgb(var(--accent-primary)/0.3)] bg-gradient-to-r from-[rgb(var(--accent-primary)/0.03)] to-transparent">
                <CardHeader className="flex-row items-center justify-between pb-3">
                    <div>
                        <CardTitle className="flex items-center gap-2">
                            <Zap size={18} className="text-[rgb(var(--accent-primary))]" /> H3C 简优云国内高速通道
                        </CardTitle>
                        <CardDescription>
                            基于 H3C 路由器的国内极速中继穿透，告别海外隧道高延迟，千兆全速直连。
                        </CardDescription>
                    </div>
                    {h3cData && (
                        <Badge variant={h3cData.status === "online" ? "success" : "danger"}>
                            {h3cData.status === "online" ? "在线运行" : "重连中"}
                        </Badge>
                    )}
                    {h3cError && !h3cData && (
                        <Badge variant="outline">稍有延迟</Badge>
                    )}
                </CardHeader>
                <CardContent>
                    {h3cData && h3cData.status === "online" ? (
                        <div className="space-y-4">
                            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                                <div className="glass-subtle rounded-xl p-3.5 space-y-1.5">
                                    <div className="flex items-center justify-between">
                                        <span className="text-xs text-fg-muted">当前动态外网直连地址</span>
                                        <div className="flex items-center gap-1">
                                            <Button
                                                variant="ghost"
                                                size="sm"
                                                className="h-6 px-2 text-[11px]"
                                                onClick={() => handleCopy(h3cData.externalAddr || "")}
                                            >
                                                {copied ? <Check size={12} className="text-[rgb(var(--accent-success))]" /> : <Copy size={12} />}
                                                {copied ? "已复制" : "复制"}
                                            </Button>
                                        </div>
                                    </div>
                                    <code className="block font-mono text-sm font-semibold text-[rgb(var(--accent-primary))] break-all">
                                        {h3cData.externalAddr}
                                    </code>
                                    <div className="flex items-center gap-1.5 text-[11px] text-fg-subtle pt-1">
                                        <Clock size={12} />
                                        <span>剩余有效期：<b className="text-fg font-medium">{h3cData.remainingMinutes} 分钟</b>（NAS后台自动化循环保活，永不中断）</span>
                                    </div>
                                </div>

                                <div className="glass-subtle rounded-xl p-3.5 space-y-2 flex flex-col justify-between">
                                    <div>
                                        <span className="text-xs text-fg-muted">永久固定重定向入口（推荐书签）</span>
                                        <code className="block font-mono text-xs text-fg mt-1">
                                            https://dashboard.feiyemomo.top/fast
                                        </code>
                                        <p className="text-[11px] text-fg-subtle mt-1">
                                            访问此固定链接将自动 302 秒级重定向至最新的高速端口。
                                        </p>
                                    </div>
                                    <div className="flex items-center gap-2 pt-1">
                                        <a
                                            href={h3cData?.externalAddr ? `${h3cData.externalAddr}/#token=${encodeURIComponent(getToken() || "")}` : `/fast`}
                                            target="_blank"
                                            rel="noopener noreferrer"
                                            className="inline-flex flex-1 items-center justify-center gap-1.5 rounded-lg bg-[rgb(var(--accent-primary))] px-3 py-1.5 text-xs font-semibold text-white shadow-sm hover:opacity-90 transition-opacity"
                                        >
                                            <Zap size={14} /> 立即进入高速通道 <ExternalLink size={12} />
                                        </a>
                                        <Button
                                            variant="outline"
                                            size="sm"
                                            className="h-8 text-xs"
                                            disabled={h3cLoading}
                                            onClick={handleRefreshH3c}
                                        >
                                            <RefreshCw size={12} className={h3cLoading ? "animate-spin" : ""} />
                                            刷新租期
                                        </Button>
                                    </div>
                                </div>
                            </div>
                        </div>
                    ) : h3cError ? (
                        <div className="glass-subtle rounded-xl p-4 flex flex-col sm:flex-row items-center justify-between gap-3 text-sm">
                            <span className="text-xs text-fg-muted">
                                海外中继查询穿透状态稍有延迟，后台保活仍常态运行中。您可直接点击进入高速通道：
                            </span>
                            <div className="flex items-center gap-2">
                                <a
                                    href="/fast"
                                    target="_blank"
                                    rel="noopener noreferrer"
                                    className="inline-flex items-center gap-1.5 rounded-lg bg-[rgb(var(--accent-primary))] px-3 py-1.5 text-xs font-semibold text-white hover:opacity-90 transition-opacity"
                                >
                                    <Zap size={13} /> 立即进入高速通道 <ExternalLink size={11} />
                                </a>
                                <Button
                                    variant="outline"
                                    size="sm"
                                    className="h-7 text-xs"
                                    onClick={() => { setH3cError(false); fetchH3c(); }}
                                >
                                    <RefreshCw size={12} className="mr-1" /> 重试
                                </Button>
                            </div>
                        </div>
                    ) : (
                        <div className="text-sm text-fg-muted flex items-center gap-2 py-2">
                            <RefreshCw size={14} className="animate-spin" /> 正在获取 H3C 高速穿透状态...
                        </div>
                    )}
                </CardContent>
            </Card>

            {/* Capability grid */}
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                {/* NAT */}
                <Card>
                    <CardHeader className="flex-row items-center justify-between pb-2">
                        <CardTitle className="flex items-center gap-2 text-sm">
                            <NetworkIcon size={16} /> NAT
                        </CardTitle>
                        {status?.nat && (
                            <Badge
                                variant={
                                    status.nat.type === "cone"
                                        ? "success"
                                        : status.nat.type === "symmetric"
                                            ? "danger"
                                            : "outline"
                                }
                            >
                                {status.nat.type}
                            </Badge>
                        )}
                    </CardHeader>
                    <CardContent>
                        {status?.nat ? (
                            <div className="space-y-1.5">
                                {status.nat.public_ip && (
                                    <div>
                                        <span className="text-xs text-fg-muted">公网 IP： </span>
                                        <code className="font-mono text-xs text-fg">
                                            {status.nat.public_ip}
                                        </code>
                                    </div>
                                )}
                                {status.nat.public_port != null && status.nat.public_port > 0 && (
                                    <div>
                                        <span className="text-xs text-fg-muted">公网端口： </span>
                                        <code className="font-mono text-xs text-fg">
                                            {status.nat.public_port}
                                        </code>
                                    </div>
                                )}
                                {!status.nat.public_ip && (
                                    <span className="text-xs text-fg-muted">
                                        STUN 不可达 — NAT 类型未知。
                                    </span>
                                )}
                            </div>
                        ) : (
                            <div className="text-sm text-fg-muted">加载中...</div>
                        )}
                    </CardContent>
                </Card>

                {/* P2P */}
                <Card>
                    <CardHeader className="flex-row items-center justify-between pb-2">
                        <CardTitle className="flex items-center gap-2 text-sm">
                            <Shield size={16} /> P2P
                        </CardTitle>
                        {status?.p2p && (
                            <Badge variant={status.p2p.supported ? "success" : "danger"}>
                                {status.p2p.supported ? "支持" : "不支持"}
                            </Badge>
                        )}
                    </CardHeader>
                    <CardContent>
                        {status?.p2p ? (
                            <p className="text-xs text-fg-muted">{status.p2p.reason}</p>
                        ) : (
                            <div className="text-sm text-fg-muted">加载中...</div>
                        )}
                    </CardContent>
                </Card>
            </div>

            {/* Raw payload */}
            <Card>
                <CardHeader>
                    <CardTitle className="flex items-center gap-2">
                        <NetworkIcon size={16} /> 原始负载
                    </CardTitle>
                    <CardDescription>
                        来自 <code className="font-mono">/api/v1/network/status</code> 的原始 JSON。
                    </CardDescription>
                </CardHeader>
                <CardContent>
                    <pre className="glass-subtle rounded-xl overflow-x-auto p-4 text-xs leading-relaxed text-fg">
                        {status ? JSON.stringify(status, null, 2) : "// 暂无数据"}
                    </pre>
                </CardContent>
            </Card>
        </div>
    );
}