import { useEffect, useState, useCallback } from "react";
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
} from "lucide-react";
import { getNetworkStatus, checkClientIPv6 } from "@/api/network";
import { ApiError } from "@/api/client";
import type { NetworkStatus, ConnectionStrategy } from "@/types";
import {
    Card,
    CardContent,
    CardDescription,
    CardHeader,
    CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";

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
    const [status, setStatus] = useState<NetworkStatus | null>(null);
    const [clientIPv6, setClientIPv6] = useState<boolean | null>(null);
    const [error, setError] = useState<string | null>(null);
    const [refreshing, setRefreshing] = useState(false);

    const fetchAll = useCallback(async (force = false) => {
        try {
            if (force) setRefreshing(true);
            const [s, c] = await Promise.all([
                getNetworkStatus(force),
                checkClientIPv6(),
            ]);
            setStatus(s);
            setClientIPv6(c);
            setError(null);
        } catch (err) {
            setError(
                err instanceof ApiError
                    ? err.message
                    : err instanceof Error
                        ? err.message
                        : "加载网络状态失败",
            );
        } finally {
            setRefreshing(false);
        }
    }, []);

    useEffect(() => {
        fetchAll();
    }, [fetchAll]);

    const strategyLabel: Record<ConnectionStrategy, string> = {
        ipv6_direct: "IPv6 直连",
        p2p: "P2P UDP",
        relay: "中继（隧道）",
    };

    // Whether an upgrade from relay is available.
    const hasUpgrade = status != null && status.strategy !== status.initial;

    // IPv6 direct is possible only if BOTH server and client have IPv6.
    const ipv6DirectPossible =
        status?.ipv6?.reachable === true && clientIPv6 === true;

    return (
        <div className="animate-fade-in space-y-6">
            {/* Header */}
            <div className="flex items-center justify-between">
                <div>
                    <h2 className="text-lg font-semibold text-fg">网络</h2>
                    <p className="text-xs text-fg-muted">
                        服务器网络能力与连接策略。
                    </p>
                </div>
                <Button
                    variant="outline"
                    size="sm"
                    onClick={() => fetchAll(true)}
                    disabled={refreshing}
                >
                    <RefreshCw size={14} className={refreshing ? "animate-spin" : ""} />
                    刷新
                </Button>
            </div>

            {error && (
                <div className="rounded-xl glass bg-[rgb(var(--accent-danger)/0.1)] px-4 py-3 text-xs text-[rgb(var(--accent-danger))]">
                    {error}
                </div>
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

            {/* IPv6 side-by-side: Server vs Client */}
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