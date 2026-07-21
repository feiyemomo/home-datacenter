import { useCallback, useEffect, useMemo, useState } from "react";
import { HardDrive, RefreshCw, Trash2, Loader2 } from "lucide-react";
import { listDevices, revokeDevice } from "@/api/device";
import { getSystemStatus } from "@/api/system";
import { ApiError } from "@/api/client";
import { useWebSocket } from "@/hooks/useWebSocket";
import { cn, formatDateTime } from "@/lib/utils";
import type { Device, SystemStatus, WsMessage } from "@/types";
import {
    Card,
    CardContent,
    CardDescription,
    CardHeader,
    CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";

/**
 * Devices page.
 *
 * Combines:
 *   - GET /device/list (the rows)
 *   - GET /system/status (online_device_ids drives the status badge)
 *   - WebSocket device.* events to update status without a refresh
 *   - DELETE /device/:id to revoke
 */
export default function Devices() {
    const [devices, setDevices] = useState<Device[]>([]);
    const [status, setStatus] = useState<SystemStatus | null>(null);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState<string | null>(null);
    const [revokingId, setRevokingId] = useState<number | null>(null);
    const [confirmId, setConfirmId] = useState<number | null>(null);

    const { lastMessage, subscribe } = useWebSocket();

    const refreshAll = useCallback(async () => {
        setError(null);
        try {
            const [devs, sys] = await Promise.all([
                listDevices(),
                getSystemStatus().catch(() => null),
            ]);
            setDevices(devs);
            setStatus(sys);
        } catch (err) {
            setError(
                err instanceof ApiError
                    ? err.message
                    : err instanceof Error
                        ? err.message
                        : "加载设备失败",
            );
        } finally {
            setLoading(false);
        }
    }, []);

    useEffect(() => {
        refreshAll();
    }, [refreshAll]);

    // Subscribe to device status events for live updates.
    useEffect(() => {
        subscribe("device");
    }, [subscribe]);

    // Update the in-memory online set immediately from any
    // device.status event so the badge flips without a round-trip.
    // Also re-fetch /system/status in the background to keep the
    // server's view of mqtt_connected / ws_clients fresh.
    useEffect(() => {
        if (!lastMessage || lastMessage.type !== "event") return;
        const topic = lastMessage.topic ?? "";
        if (!topic.startsWith("device")) return;
        if (topic !== "device.status") {
            // telemetry/command — not interesting for online state.
            getSystemStatus().then(setStatus).catch(() => undefined);
            return;
        }
        const payload = (lastMessage.payload ?? {}) as {
            device_id?: number;
            status?: string;
        };
        const id = payload.device_id;
        if (typeof id !== "number") return;
        setStatus((prev) => {
            if (!prev) return prev;
            const ids = new Set(prev.online_device_ids ?? []);
            if (payload.status === "online" || payload.status === "heartbeat") {
                ids.add(id);
            } else if (payload.status === "offline") {
                ids.delete(id);
            }
            return {
                ...prev,
                online_device_ids: Array.from(ids),
                online_device_count: ids.size,
            };
        });
        // Best-effort reconcile with the server (handles edge cases
        // like the dashboard not having seen the latest sweep).
        getSystemStatus().then(setStatus).catch(() => undefined);
    }, [lastMessage]);

    // Maintain a Set of online device IDs, updated from both polling
    // and WebSocket events.
    const onlineIds = useMemo<Set<number>>(() => {
        return new Set(status?.online_device_ids ?? []);
    }, [status]);

    // Apply the initial online_list snapshot from the server (sent
    // once on WebSocket connect) and the canonical refresh for any
    // non-status device.* event. The device.status fast path lives
    // in the useEffect above to avoid an extra /system/status fetch
    // on every status flip.
    useEffect(() => {
        if (!lastMessage) return;
        if (
            lastMessage.type === "event" &&
            lastMessage.topic?.startsWith("device") &&
            lastMessage.topic !== "device.status"
        ) {
            getSystemStatus()
                .then(setStatus)
                .catch(() => undefined);
        } else if (lastMessage.type === "online_list") {
            // Server sends {"device_ids": [...], "count": N} as the payload.
            const payload = lastMessage.payload as
                | { device_ids?: number[]; count?: number }
                | null;
            const ids = payload?.device_ids;
            if (Array.isArray(ids)) {
                setStatus((prev) =>
                    prev
                        ? { ...prev, online_device_ids: ids, online_device_count: ids.length }
                        : prev,
                );
            } else {
                getSystemStatus()
                    .then(setStatus)
                    .catch(() => undefined);
            }
        }
    }, [lastMessage]);

    async function handleRevoke(device: Device) {
        setError(null);
        setRevokingId(device.id);
        try {
            await revokeDevice(device.id);
            // Optimistically mark as revoked locally.
            setDevices((prev) =>
                prev.map((d) =>
                    d.id === device.id
                        ? { ...d, revoked_at: new Date().toISOString() }
                        : d,
                ),
            );
            setConfirmId(null);
        } catch (err) {
            setError(
                err instanceof ApiError
                    ? err.message
                    : err instanceof Error
                        ? err.message
                        : "吊销设备失败",
            );
        } finally {
            setRevokingId(null);
        }
    }

    return (
        <div className="animate-fade-in space-y-6">
            <div className="flex flex-wrap items-center justify-between gap-3">
                <div>
                    <h2 className="text-lg font-semibold text-fg">设备</h2>
                    <p className="text-xs text-fg-subtle">
                        已绑定的设备凭证与实时在线状态。
                    </p>
                </div>
                <Button
                    variant="outline"
                    size="sm"
                    onClick={refreshAll}
                    disabled={loading}
                >
                    {loading ? (
                        <Loader2 size={14} className="animate-spin" />
                    ) : (
                        <RefreshCw size={14} />
                    )}
                    刷新
                </Button>
            </div>

            {error && (
                <div className="rounded-xl glass bg-[rgb(var(--accent-danger)/0.1)] px-4 py-3 text-xs text-[rgb(var(--accent-danger))]">
                    {error}
                </div>
            )}

            <Card>
                <CardHeader>
                    <CardTitle className="flex items-center gap-2">
                        <HardDrive size={16} /> 设备注册表
                    </CardTitle>
                    <CardDescription>
                        当前可见 {devices.length} 个设备。状态反映最新的 WebSocket 心跳。
                    </CardDescription>
                </CardHeader>
                <CardContent className="p-0">
                    <div className="overflow-x-auto">
                        <table className="w-full text-left text-sm">
                            <thead className="glass-subtle border-b border-[rgb(var(--border)/0.3)] text-xs uppercase tracking-wider text-fg-subtle">
                                <tr>
                                    <th className="px-4 py-3 font-medium">名称</th>
                                    <th className="px-4 py-3 font-medium">用户</th>
                                    <th className="px-4 py-3 font-medium">状态</th>
                                    <th className="px-4 py-3 font-medium">最后登录</th>
                                    <th className="px-4 py-3 font-medium">创建于</th>
                                    <th className="px-4 py-3 text-right font-medium">操作</th>
                                </tr>
                            </thead>
                            <tbody className="divide-[rgb(var(--border)/0.3)]">
                                {devices.length === 0 && !loading && (
                                    <tr>
                                        <td
                                            colSpan={6}
                                            className="px-4 py-10 text-center text-fg-subtle"
                                        >
                                            未找到设备。
                                        </td>
                                    </tr>
                                )}
                                {devices.map((d) => {
                                    const isOnline = onlineIds.has(d.id);
                                    const isRevoked = !!d.revoked_at;
                                    return (
                                        <tr
                                            key={d.id}
                                            className={cn(
                                                "transition-colors hover:bg-[rgb(var(--bg-subtle)/0.3)]",
                                                isRevoked && "opacity-60",
                                            )}
                                        >
                                            <td className="px-4 py-3">
                                                <div className="flex items-center gap-3">
                                                    <div
                                                        className={cn(
                                                            "flex h-8 w-8 items-center justify-center rounded-xl",
                                                            isRevoked
                                                                ? "bg-[rgb(var(--glass-base))] text-fg-subtle"
                                                                : "bg-gradient-to-br from-[rgb(var(--accent-primary)/0.3)] to-[rgb(var(--accent-warm)/0.2)] text-[rgb(var(--accent-info))]",
                                                        )}
                                                    >
                                                        <HardDrive size={15} />
                                                    </div>
                                                    <div>
                                                        <div className="font-medium text-fg">
                                                            {d.device_name}
                                                        </div>
                                                        <div className="text-[11px] text-fg-subtle">
                                                            id #{d.id}
                                                            {d.last_ip ? ` · ${d.last_ip}` : ""}
                                                        </div>
                                                    </div>
                                                </div>
                                            </td>
                                            <td className="px-4 py-3 text-fg-muted">
                                                #{d.user_id}
                                            </td>
                                            <td className="px-4 py-3">
                                                {isRevoked ? (
                                                    <Badge variant="danger">已吊销</Badge>
                                                ) : isOnline ? (
                                                    <Badge variant="success">
                                                        <span className="mr-1 inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-[rgb(var(--accent-success))]" />
                                                        在线
                                                    </Badge>
                                                ) : (
                                                    <Badge variant="outline">离线</Badge>
                                                )}
                                            </td>
                                            <td className="px-4 py-3 text-fg-muted">
                                                {formatDateTime(d.last_login_at)}
                                            </td>
                                            <td className="px-4 py-3 text-fg-muted">
                                                {formatDateTime(d.created_at)}
                                            </td>
                                            <td className="px-4 py-3 text-right">
                                                {confirmId === d.id ? (
                                                    <div className="flex items-center justify-end gap-2">
                                                        <Button
                                                            variant="ghost"
                                                            size="sm"
                                                            onClick={() => setConfirmId(null)}
                                                            disabled={revokingId === d.id}
                                                        >
                                                            取消
                                                        </Button>
                                                        <Button
                                                            variant="danger"
                                                            size="sm"
                                                            onClick={() => handleRevoke(d)}
                                                            disabled={revokingId === d.id || isRevoked}
                                                        >
                                                            {revokingId === d.id ? (
                                                                <Loader2 size={14} className="animate-spin" />
                                                            ) : (
                                                                <Trash2 size={14} />
                                                            )}
                                                            确认
                                                        </Button>
                                                    </div>
                                                ) : (
                                                    <Button
                                                        variant="ghost"
                                                        size="sm"
                                                        className="text-fg-muted hover:text-[rgb(var(--accent-danger))]"
                                                        onClick={() => setConfirmId(d.id)}
                                                        disabled={isRevoked}
                                                    >
                                                        <Trash2 size={14} />
                                                        吊销
                                                    </Button>
                                                )}
                                            </td>
                                        </tr>
                                    );
                                })}
                            </tbody>
                        </table>
                    </div>
                </CardContent>
            </Card>

            <Card>
                <CardHeader>
                    <CardTitle>实时事件</CardTitle>
                    <CardDescription>
                        <code>device.*</code> 主题上的最近 WebSocket 消息。
                    </CardDescription>
                </CardHeader>
                <CardContent>
                    <EventLog lastMessage={lastMessage} />
                </CardContent>
            </Card>
        </div>
    );
}

/** Compact tail of recent WS messages, newest first. */
function EventLog({ lastMessage }: { lastMessage: WsMessage | null }) {
    const [entries, setEntries] = useState<WsMessage[]>([]);

    useEffect(() => {
        if (!lastMessage) return;
        setEntries((prev) => [lastMessage, ...prev].slice(0, 8));
    }, [lastMessage]);

    if (entries.length === 0) {
        return (
            <p className="text-xs text-fg-subtle">
                等待事件中…（已订阅 <code>device</code>）
            </p>
        );
    }

    return (
        <ul className="space-y-1 font-mono text-xs">
            {entries.map((m, i) => (
                <li
                    key={`${m.ts}-${i}`}
                    className="glass-subtle rounded-lg flex items-start gap-2 px-3 py-2"
                >
                    <span className="text-fg-subtle">
                        {new Date(m.ts * 1000).toLocaleTimeString()}
                    </span>
                    <Badge variant="info" className="text-[10px]">
                        {m.type}
                    </Badge>
                    {m.topic && (
                        <span className="text-fg-muted">{m.topic}</span>
                    )}
                    <span className="ml-auto max-w-[60%] truncate text-fg-subtle">
                        {JSON.stringify(m.payload)}
                    </span>
                </li>
            ))}
        </ul>
    );
}