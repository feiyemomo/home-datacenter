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
                        : "failed to load network status",
            );
        } finally {
            setRefreshing(false);
        }
    }, []);

    useEffect(() => {
        fetchAll();
    }, [fetchAll]);

    const strategyLabel: Record<ConnectionStrategy, string> = {
        ipv6_direct: "IPv6 Direct",
        p2p: "P2P UDP",
        relay: "Relay (Tunnel)",
    };

    // Whether an upgrade from relay is available.
    const hasUpgrade = status != null && status.strategy !== status.initial;

    // IPv6 direct is possible only if BOTH server and client have IPv6.
    const ipv6DirectPossible =
        status?.ipv6?.reachable === true && clientIPv6 === true;

    return (
        <div className="space-y-6">
            {/* Header */}
            <div className="flex items-center justify-between">
                <div>
                    <h2 className="text-lg font-semibold text-fg">Network</h2>
                    <p className="text-xs text-fg-muted">
                        Server network capability and connection strategy.
                    </p>
                </div>
                <Button
                    variant="outline"
                    size="sm"
                    onClick={() => fetchAll(true)}
                    disabled={refreshing}
                >
                    <RefreshCw size={14} className={refreshing ? "animate-spin" : ""} />
                    Refresh
                </Button>
            </div>

            {error && (
                <div className="rounded-md border border-rose-500/40 bg-rose-500/10 px-4 py-3 text-sm text-rose-700 dark:text-rose-300">
                    {error}
                </div>
            )}

            {/* Connection model: Relay First, Then Upgrade */}
            <Card>
                <CardHeader className="flex-row items-center justify-between">
                    <div>
                        <CardTitle className="flex items-center gap-2">
                            <Wifi size={16} /> Connection Model
                        </CardTitle>
                        <CardDescription>
                            Relay-first: connect immediately, then probe and upgrade.
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
                                            ? "fill-amber-400 text-amber-400"
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
                            <div className="flex items-center gap-3 rounded-lg border border-emerald-500/30 bg-emerald-500/5 px-4 py-3">
                                <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-emerald-500 text-sm font-bold text-white">
                                    1
                                </div>
                                <div className="min-w-0 flex-1">
                                    <p className="text-sm font-medium text-fg">
                                        Connect via Relay
                                    </p>
                                    <p className="text-xs text-fg-muted">
                                        Cloudflare Tunnel — zero delay, always works
                                    </p>
                                </div>
                                <Badge variant="success">Active</Badge>
                            </div>

                            {/* Arrow down */}
                            <div className="flex justify-center">
                                <ArrowRight size={16} className="rotate-90 text-fg-subtle" />
                            </div>

                            {/* Step 2: Probe & Upgrade */}
                            <div
                                className={`flex items-center gap-3 rounded-lg border px-4 py-3 ${
                                    hasUpgrade
                                        ? "border-sky-500/30 bg-sky-500/5"
                                        : "border-surface-border bg-surface-subtle/30"
                                }`}
                            >
                                <div
                                    className={`flex h-8 w-8 shrink-0 items-center justify-center rounded-full text-sm font-bold ${
                                        hasUpgrade
                                            ? "bg-sky-500 text-white"
                                            : "bg-surface-subtle text-fg-muted"
                                    }`}
                                >
                                    2
                                </div>
                                <div className="min-w-0 flex-1">
                                    <p className="text-sm font-medium text-fg">
                                        {hasUpgrade
                                            ? `Probe & Upgrade to ${strategyLabel[status.strategy]}`
                                            : "No upgrade available"}
                                    </p>
                                    <p className="text-xs text-fg-muted">
                                        {hasUpgrade
                                            ? status.strategy === "ipv6_direct"
                                                ? "Background probe to server's public IPv6 — switch if reachable"
                                                : "Background UDP hole-punching via STUN — switch if established"
                                            : "Server has no IPv6 or P2P capability — relay is the only path"}
                                    </p>
                                </div>
                                {hasUpgrade ? (
                                    <Badge variant="info">
                                        <ArrowUp size={12} className="mr-1" />
                                        Upgrade
                                    </Badge>
                                ) : (
                                    <Badge variant="outline">N/A</Badge>
                                )}
                            </div>

                            <div className="text-xs text-fg-subtle">
                                Last checked: {new Date(status.checked_at).toLocaleString()}
                            </div>
                        </div>
                    ) : (
                        <div className="text-sm text-fg-muted">Loading...</div>
                    )}
                </CardContent>
            </Card>

            {/* IPv6 side-by-side: Server vs Client */}
            <Card>
                <CardHeader>
                    <CardTitle className="flex items-center gap-2">
                        <Globe size={16} /> IPv6 Connectivity
                    </CardTitle>
                    <CardDescription>
                        IPv6 direct requires BOTH server and client to have public IPv6.
                    </CardDescription>
                </CardHeader>
                <CardContent>
                    <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                        {/* Server side */}
                        <div className="rounded-lg border border-surface-border bg-surface-subtle/30 p-4">
                            <div className="mb-2 flex items-center gap-2">
                                <Server size={14} className="text-fg-muted" />
                                <span className="text-xs font-medium uppercase tracking-wider text-fg-muted">
                                    Server
                                </span>
                            </div>
                            {status?.ipv6 ? (
                                <div className="space-y-2">
                                    <div className="flex items-center gap-2">
                                        {status.ipv6.reachable ? (
                                            <CheckCircle2 size={16} className="text-emerald-500" />
                                        ) : (
                                            <XCircle size={16} className="text-rose-500" />
                                        )}
                                        <span className="text-sm text-fg">
                                            {status.ipv6.reachable
                                                ? "Public IPv6 reachable"
                                                : status.ipv6.enabled
                                                    ? "IPv6 enabled but not publicly reachable"
                                                    : "IPv6 unavailable"}
                                        </span>
                                    </div>
                                    {status.ipv6.address && (
                                        <div>
                                            <code className="font-mono text-xs text-fg-muted">
                                                {status.ipv6.address}
                                            </code>
                                        </div>
                                    )}
                                </div>
                            ) : (
                                <div className="text-sm text-fg-muted">Loading...</div>
                            )}
                        </div>

                        {/* Client side */}
                        <div className="rounded-lg border border-surface-border bg-surface-subtle/30 p-4">
                            <div className="mb-2 flex items-center gap-2">
                                <Smartphone size={14} className="text-fg-muted" />
                                <span className="text-xs font-medium uppercase tracking-wider text-fg-muted">
                                    Your Device
                                </span>
                            </div>
                            <div className="space-y-2">
                                <div className="flex items-center gap-2">
                                    {clientIPv6 === null ? (
                                        <RefreshCw size={16} className="animate-spin text-fg-subtle" />
                                    ) : clientIPv6 ? (
                                        <CheckCircle2 size={16} className="text-emerald-500" />
                                    ) : (
                                        <XCircle size={16} className="text-rose-500" />
                                    )}
                                    <span className="text-sm text-fg">
                                        {clientIPv6 === null
                                            ? "Checking..."
                                            : clientIPv6
                                                ? "IPv6 available"
                                                : "IPv6 unavailable"}
                                    </span>
                                </div>
                                <p className="text-xs text-fg-muted">
                                    {clientIPv6 === false
                                        ? "Your network lacks IPv6 — relay is the only option"
                                        : clientIPv6 === true && !status?.ipv6?.reachable
                                            ? "You have IPv6, but the server doesn't — IPv6 direct blocked by server"
                                            : ipv6DirectPossible
                                                ? "Both sides have IPv6 — direct connection possible"
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
                                        <span className="text-xs text-fg-muted">Public IP: </span>
                                        <code className="font-mono text-xs text-fg">
                                            {status.nat.public_ip}
                                        </code>
                                    </div>
                                )}
                                {status.nat.public_port != null && status.nat.public_port > 0 && (
                                    <div>
                                        <span className="text-xs text-fg-muted">Public port: </span>
                                        <code className="font-mono text-xs text-fg">
                                            {status.nat.public_port}
                                        </code>
                                    </div>
                                )}
                                {!status.nat.public_ip && (
                                    <span className="text-xs text-fg-muted">
                                        STUN unreachable — NAT type unknown.
                                    </span>
                                )}
                            </div>
                        ) : (
                            <div className="text-sm text-fg-muted">Loading...</div>
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
                                {status.p2p.supported ? "Supported" : "Unsupported"}
                            </Badge>
                        )}
                    </CardHeader>
                    <CardContent>
                        {status?.p2p ? (
                            <p className="text-xs text-fg-muted">{status.p2p.reason}</p>
                        ) : (
                            <div className="text-sm text-fg-muted">Loading...</div>
                        )}
                    </CardContent>
                </Card>
            </div>

            {/* Raw payload */}
            <Card>
                <CardHeader>
                    <CardTitle className="flex items-center gap-2">
                        <NetworkIcon size={16} /> Raw payload
                    </CardTitle>
                    <CardDescription>
                        Raw JSON from <code className="font-mono">/api/v1/network/status</code>.
                    </CardDescription>
                </CardHeader>
                <CardContent>
                    <pre className="overflow-x-auto rounded-lg border border-surface-border bg-surface-subtle/50 p-4 text-xs leading-relaxed text-fg">
                        {status ? JSON.stringify(status, null, 2) : "// no data yet"}
                    </pre>
                </CardContent>
            </Card>
        </div>
    );
}
