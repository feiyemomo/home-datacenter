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
    Zap,
    Activity,
} from "lucide-react";
import { getNetworkStatus, checkClientIPv6, probeIPv6Direct } from "@/api/network";
import { ApiError } from "@/api/client";
import { cn } from "@/lib/utils";
import type { NetworkStatus, ConnectionStrategy, P2PSession } from "@/types";
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
    // IPv6 direct probe result: null = not probed, >=0 = RTT ms, -1 = failed, -2 = mixed content.
    const [probeRTT, setProbeRTT] = useState<number | null>(null);
    const [probing, setProbing] = useState(false);
    // Manual strategy override: "auto" follows server recommendation,
    // otherwise the user forces a specific path. Persisted in localStorage.
    const [manualStrategy, setManualStrategy] = useState<ConnectionStrategy | "auto">("auto");

    const fetchAll = useCallback(async (force = false) => {
        try {
            if (force) setRefreshing(true);
            setProbeRTT(null); // clear stale probe result
            const [s, c] = await Promise.all([
                getNetworkStatus(force),
                checkClientIPv6(),
            ]);
            setStatus(s);
            setClientIPv6(c);
            setError(null);

            // Auto-probe IPv6 direct in the background. checkClientIPv6()
            // test endpoints may be blocked by GFW; the probe is the
            // definitive test. If it succeeds, override clientIPv6=true.
            // On HTTPS pages this returns -2 (mixed content) instantly.
            if (s.direct_url) {
                setProbing(true);
                probeIPv6Direct(s.direct_url).then((rtt) => {
                    setProbeRTT(rtt);
                    if (rtt >= 0) {
                        setClientIPv6(true);
                    }
                }).finally(() => {
                    setProbing(false);
                });
            }
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

    // Load manual strategy override from localStorage on mount.
    useEffect(() => {
        try {
            const saved = localStorage.getItem("network.strategy.override");
            if (
                saved === "auto" ||
                saved === "ipv6_direct" ||
                saved === "p2p" ||
                saved === "relay"
            ) {
                setManualStrategy(saved);
            }
        } catch {
            // localStorage may be unavailable (private browsing) — ignore.
        }
    }, []);

    const handleStrategyChange = useCallback(
        (strategy: ConnectionStrategy | "auto") => {
            setManualStrategy(strategy);
            try {
                localStorage.setItem("network.strategy.override", strategy);
            } catch {
                // Ignore write failure in private browsing.
            }
        },
        [],
    );

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

    // Effective strategy: manual override takes precedence, otherwise
    // fall back to the server's recommended upgrade target.
    const effectiveStrategy: ConnectionStrategy =
        manualStrategy === "auto"
            ? status?.strategy ?? "relay"
            : manualStrategy;

    // Availability per strategy — unavailable options are disabled.
    const strategyAvailable: Record<ConnectionStrategy, boolean> = {
        ipv6_direct: ipv6DirectPossible,
        p2p: status?.p2p?.supported ?? false,
        relay: status?.relay?.available ?? true,
    };

    // Probe the server's IPv6 direct URL. Tests whether this client can
    // actually reach the server over IPv6 (firewall, routing, etc.).
    // If the probe succeeds, the client definitively has IPv6 — override
    // the checkClientIPv6() result which may fail due to GFW blocking
    // the test endpoints even though the client's network has IPv6.
    const handleProbe = useCallback(async () => {
        if (!status?.direct_url) return;
        setProbing(true);
        try {
            const rtt = await probeIPv6Direct(status.direct_url);
            setProbeRTT(rtt);
            if (rtt >= 0) {
                setClientIPv6(true);
            }
        } finally {
            setProbing(false);
        }
    }, [status?.direct_url]);

    const sessionStatusBadge = (s: P2PSession["status"]) => {
        switch (s) {
            case "established":
                return <Badge variant="success">Established</Badge>;
            case "punching":
                return <Badge variant="info">Punching</Badge>;
            default:
                return <Badge variant="danger">Failed</Badge>;
        }
    };

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
                                className={`flex items-center gap-3 rounded-lg border px-4 py-3 ${hasUpgrade
                                    ? "border-sky-500/30 bg-sky-500/5"
                                    : "border-surface-border bg-surface-subtle/30"
                                    }`}
                            >
                                <div
                                    className={`flex h-8 w-8 shrink-0 items-center justify-center rounded-full text-sm font-bold ${hasUpgrade
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

            {/* Manual Strategy Override */}
            <Card>
                <CardHeader>
                    <CardTitle className="flex items-center gap-2">
                        <Shield size={16} /> Manual Strategy Override
                    </CardTitle>
                    <CardDescription>
                        Force a specific connection path instead of the server's
                        recommendation. Preference is stored locally in this browser.
                    </CardDescription>
                </CardHeader>
                <CardContent>
                    <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
                        {(["auto", "ipv6_direct", "p2p", "relay"] as const).map((opt) => {
                            const isActive = manualStrategy === opt;
                            const isAvailable =
                                opt === "auto" || strategyAvailable[opt];
                            const label =
                                opt === "auto" ? "Auto" : strategyLabel[opt];
                            return (
                                <button
                                    key={opt}
                                    type="button"
                                    onClick={() => handleStrategyChange(opt)}
                                    disabled={!isAvailable}
                                    className={cn(
                                        "flex flex-col items-start gap-1 rounded-lg border px-4 py-3 text-left transition-colors",
                                        isActive
                                            ? "border-sky-500 bg-sky-500/10"
                                            : isAvailable
                                                ? "border-surface-border bg-surface-subtle/30 hover:border-sky-500/50 hover:bg-sky-500/5"
                                                : "cursor-not-allowed border-surface-border bg-surface-subtle/20 opacity-50",
                                    )}
                                >
                                    <div className="flex w-full items-center justify-between">
                                        <span className="text-sm font-medium text-fg">
                                            {label}
                                        </span>
                                        {isActive && (
                                            <CheckCircle2
                                                size={14}
                                                className="text-sky-500"
                                            />
                                        )}
                                    </div>
                                    <span className="text-xs text-fg-muted">
                                        {opt === "auto"
                                            ? `Server: ${status ? strategyLabel[status.strategy] : "..."}`
                                            : isAvailable
                                                ? "Available"
                                                : "Unavailable"}
                                    </span>
                                </button>
                            );
                        })}
                    </div>
                    <div className="mt-4 flex items-center gap-2 rounded-lg border border-surface-border bg-surface-subtle/30 px-4 py-3">
                        <span className="text-xs text-fg-muted">
                            Effective strategy:
                        </span>
                        <Badge variant="info">
                            {strategyLabel[effectiveStrategy]}
                        </Badge>
                        {manualStrategy !== "auto" && (
                            <span className="text-xs text-amber-600 dark:text-amber-400">
                                Manual override active
                            </span>
                        )}
                    </div>
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
                                        ? probeRTT !== null && probeRTT >= 0
                                            ? "IPv6 confirmed via direct probe (test endpoints may be blocked by GFW)"
                                            : "No IPv6 detected — try the direct probe below to confirm"
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

            {/* IPv6 Direct Probe — test actual reachability to server's IPv6 */}
            <Card>
                <CardHeader>
                    <CardTitle className="flex items-center gap-2">
                        <Zap size={16} /> IPv6 Direct Probe
                    </CardTitle>
                    <CardDescription>
                        Test whether this device can reach the server over public IPv6.
                    </CardDescription>
                </CardHeader>
                <CardContent>
                    {status?.direct_url ? (
                        <div className="space-y-3">
                            <div className="flex items-center gap-2">
                                <span className="text-xs text-fg-muted">Target:</span>
                                <code className="font-mono text-xs text-fg">
                                    {status.direct_url}
                                </code>
                            </div>
                            <div className="flex items-center gap-3">
                                <Button
                                    variant="outline"
                                    size="sm"
                                    onClick={handleProbe}
                                    disabled={probing}
                                >
                                    {probing ? (
                                        <RefreshCw size={14} className="animate-spin" />
                                    ) : (
                                        <Zap size={14} />
                                    )}
                                    {probing ? "Probing..." : "Probe"}
                                </Button>
                                {probeRTT !== null && (
                                    <div className="flex items-center gap-2">
                                        {probeRTT >= 0 ? (
                                            <>
                                                <CheckCircle2 size={16} className="text-emerald-500" />
                                                <span className="text-sm text-fg">
                                                    Reachable — <span className="font-mono">{probeRTT} ms</span> RTT
                                                </span>
                                            </>
                                        ) : probeRTT === -2 ? (
                                            <>
                                                <XCircle size={16} className="text-amber-500" />
                                                <span className="text-sm text-fg">
                                                    Mixed content blocked
                                                </span>
                                            </>
                                        ) : (
                                            <>
                                                <XCircle size={16} className="text-rose-500" />
                                                <span className="text-sm text-fg">
                                                    Unreachable from this device
                                                </span>
                                            </>
                                        )}
                                    </div>
                                )}
                            </div>
                            <p className="text-xs text-fg-muted">
                                {probeRTT === -2
                                    ? status?.ipv6?.address
                                        ? <>Dashboard is HTTPS but probe target is HTTP — browser blocks mixed content. Open <code className="font-mono">{`http://[${status.ipv6.address}]/`}</code> on this device to probe via HTTP over IPv6 direct.</>
                                        : "Dashboard is HTTPS but probe target is HTTP — browser blocks mixed content. Access the dashboard via HTTP or use the mobile app for probing."
                                    : ipv6DirectPossible
                                        ? "Both sides have IPv6 — probe tests the actual end-to-end path."
                                        : "Direct URL configured, but IPv6 may not be available on one side."}
                            </p>
                        </div>
                    ) : (
                        <div className="text-sm text-fg-muted">
                            IPv6 direct is not configured. Set{" "}
                            <code className="font-mono text-xs">network.direct_port</code>{" "}
                            on the server to enable.
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

            {/* P2P Sessions — active hole-punching sessions */}
            <Card>
                <CardHeader className="flex-row items-center justify-between">
                    <div>
                        <CardTitle className="flex items-center gap-2">
                            <Activity size={16} /> P2P Sessions
                        </CardTitle>
                        <CardDescription>
                            Active UDP hole-punching sessions on this server.
                        </CardDescription>
                    </div>
                    {status?.p2p_endpoint && (
                        <Badge variant="info">
                            <code className="font-mono text-xs">{status.p2p_endpoint}</code>
                        </Badge>
                    )}
                </CardHeader>
                <CardContent>
                    {!status?.p2p_endpoint ? (
                        <div className="text-sm text-fg-muted">
                            P2P hole punching is not enabled. Set{" "}
                            <code className="font-mono text-xs">network.p2p_port</code>{" "}
                            on the server to enable.
                        </div>
                    ) : status.p2p_sessions && status.p2p_sessions.length > 0 ? (
                        <div className="space-y-2">
                            {status.p2p_sessions.map((s) => (
                                <div
                                    key={s.peer_id}
                                    className="flex flex-wrap items-center gap-3 rounded-lg border border-surface-border bg-surface-subtle/30 px-4 py-3"
                                >
                                    <div className="min-w-0 flex-1">
                                        <div className="flex items-center gap-2">
                                            <span className="text-xs font-medium uppercase tracking-wider text-fg-muted">
                                                Peer
                                            </span>
                                            <code className="font-mono text-xs text-fg">
                                                {s.peer_id}
                                            </code>
                                        </div>
                                        <div className="mt-1 flex items-center gap-2">
                                            <span className="text-xs text-fg-muted">Remote:</span>
                                            <code className="font-mono text-xs text-fg-muted">
                                                {s.remote_addr}
                                            </code>
                                        </div>
                                        <div className="mt-1 flex flex-wrap gap-x-4 gap-y-0.5 text-xs text-fg-subtle">
                                            <span>Punches: {s.punch_count}</span>
                                            {s.established_at && (
                                                <span>
                                                    Established:{" "}
                                                    {new Date(s.established_at).toLocaleTimeString()}
                                                </span>
                                            )}
                                            {s.last_packet_at && (
                                                <span>
                                                    Last pkt:{" "}
                                                    {new Date(s.last_packet_at).toLocaleTimeString()}
                                                </span>
                                            )}
                                        </div>
                                    </div>
                                    {sessionStatusBadge(s.status)}
                                </div>
                            ))}
                        </div>
                    ) : (
                        <div className="text-sm text-fg-muted">
                            No active sessions. Peers will appear here once they start hole punching.
                        </div>
                    )}
                </CardContent>
            </Card>

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
