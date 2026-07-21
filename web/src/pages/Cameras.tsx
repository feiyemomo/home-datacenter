import { useCallback, useEffect, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { Camera as CameraIcon, Plus, Trash2, RefreshCcw, Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Select } from "@/components/ui/input";
import { listCameras, deleteCamera, updateCodec } from "@/api/camera";
import type { Camera, WsMessage } from "@/types";
import { useAuth } from "@/hooks/useAuth";
import { useWebSocket } from "@/hooks/useWebSocket";
import { LiveVideo } from "@/components/LiveVideo";

// Only H.264 is offered in the dashboard codec selector. WebRTC's RTP
// codec registry mandates H.264 (plus VP8/VP9/AV1) but NOT H.265, so
// `passthrough` and `h265` always 502 on Chrome/Edge/Firefox WebRTC.
// Legacy cameras with `codec=passthrough`/`h265` (set before this
// restriction) still render correctly in the badge via
// `codecBadgeLabel`, and the dropdown shows a disabled "(legacy)"
// entry plus the selectable "H.264" so the operator can migrate.
type CodecOption = "passthrough" | "h264" | "h265";

/** Resolve the human-readable codec label for a camera badge. */
function codecBadgeLabel(cam: Camera): string | null {
    if (cam.codec) {
        if (cam.codec === "passthrough") return "直通";
        if (cam.codec === "h264") return "H.264";
        if (cam.codec === "h265") return "H.265";
        return cam.codec.toUpperCase();
    }
    if (cam.transcode) return "x264";
    return null;
}

/**
 * Cameras — list + live view + delete.
 *
 * Registration has moved to a dedicated page (/cameras/new,
 * DeviceCreate.tsx). The list page is now strictly for *browsing*
 * — the operator can refresh, watch live, and remove a camera, but
 * not stand up a new one inline. This keeps the cards above the
 * fold and gives the create flow its own URL to bookmark / share.
 *
 * Recording playback + recording toggle now live inside LiveVideo
 * itself (preview/live/playback mode switch). The page just routes
 * the URL params (camera, time) through to the per-card LiveVideo.
 *
 * Supports URL query params:
 *   - camera: camera ID to scroll to
 *   - time: unix timestamp to auto-play the corresponding recording
 */
export default function Cameras() {
    const { isAdmin } = useAuth();
    const nav = useNavigate();
    const [cams, setCams] = useState<Camera[]>([]);
    const [loading, setLoading] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const [searchParams] = useSearchParams();

    const targetCameraId = searchParams.get("camera") ? Number(searchParams.get("camera")) : undefined;
    const targetTime = searchParams.get("time") ? Number(searchParams.get("time")) : undefined;

    const refresh = useCallback(async () => {
        setLoading(true);
        setError(null);
        try {
            const list = await listCameras();
            setCams(list);
        } catch (e) {
            setError(e instanceof Error ? e.message : String(e));
        } finally {
            setLoading(false);
        }
    }, []);

    useEffect(() => {
        void refresh();
    }, [refresh]);

    async function remove(id: number) {
        if (!isAdmin) return;
        if (!confirm(`确认删除摄像头 ${id}？`)) return;
        try {
            await deleteCamera(id);
            await refresh();
        } catch (e) {
            setError(e instanceof Error ? e.message : String(e));
        }
    }

    return (
        <div className="space-y-4 animate-fade-in">
            <div className="flex items-center justify-between">
                <div className="flex items-center gap-2">
                    <CameraIcon className="h-5 w-5 text-[rgb(var(--accent-primary))]" />
                    <h2 className="text-lg font-semibold text-fg">摄像头</h2>
                    <Badge variant="outline">{cams.length}</Badge>
                </div>
                <div className="flex gap-2">
                    <Button size="sm" variant="outline" onClick={refresh} disabled={loading}>
                        <RefreshCcw size={14} className="mr-1" />
                        刷新
                    </Button>
                    {isAdmin && (
                        <Button size="sm" onClick={() => nav("/cameras/new")}>
                            <Plus size={14} className="mr-1" />
                            注册
                        </Button>
                    )}
                </div>
            </div>

            {error && (
                <div className="glass bg-[rgb(var(--accent-danger)/0.1)] text-[rgb(var(--accent-danger))] rounded-lg px-3 py-2 text-sm">
                    {error}
                </div>
            )}

            <WsBridge>
                {(onMsg) => (
                    <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
                        {cams.map((cam) => (
                            <CamCard
                                key={cam.id}
                                cam={cam}
                                isAdmin={isAdmin}
                                onDelete={() => remove(cam.id)}
                                onRefresh={refresh}
                                onWsMessage={onMsg}
                                targetTime={(targetCameraId === cam.id) ? targetTime : undefined}
                            />
                        ))}
                        {cams.length === 0 && !loading && (
                            <div className="col-span-full glass rounded-2xl p-8 text-center text-sm text-fg-muted animate-fade-in">
                                暂无注册的摄像头。{isAdmin ? "点击「注册」添加一个。" : "请联系管理员添加。"}
                            </div>
                        )}
                    </div>
                )}
            </WsBridge>
        </div>
    );
}

function CamCard({
    cam,
    isAdmin,
    onDelete,
    onRefresh,
    onWsMessage,
    targetTime,
}: {
    cam: Camera;
    isAdmin: boolean;
    onDelete: () => void;
    onRefresh: () => void | Promise<void>;
    onWsMessage: (handler: (m: WsMessage) => void) => () => void;
    targetTime?: number;
}) {
    const statusVariant =
        cam.status === "online"
            ? "success"
            : cam.status === "offline"
                ? "danger"
                : "warning";

    const [codecLoading, setCodecLoading] = useState(false);
    const [codecError, setCodecError] = useState<string | null>(null);

    // Current codec value for the <Select>. Prefer the explicit
    // `cam.codec`; fall back to legacy transcode bool (true ⇒ h264,
    // false ⇒ passthrough) so the dropdown reflects server state.
    const currentCodec: CodecOption = cam.codec
        ? (cam.codec as CodecOption)
        : cam.transcode
            ? "h264"
            : "passthrough";

    async function onCodecChange(_: React.ChangeEvent<HTMLSelectElement>) {
        if (!isAdmin) return;
        // Only "h264" is selectable now; the disabled "(legacy)" entry
        // for non-h264 cameras can't be re-selected, so any onChange
        // event means the operator chose "H.264" (migrate from legacy).
        const next = "h264" as const;
        if (next === currentCodec) return;
        setCodecLoading(true);
        setCodecError(null);
        try {
            await updateCodec(cam.id, next);
            await onRefresh();
        } catch (err) {
            setCodecError(err instanceof Error ? err.message : String(err));
        } finally {
            setCodecLoading(false);
        }
    }

    const badgeLabel = codecBadgeLabel(cam);

    return (
        <div className="flex flex-col overflow-hidden glass glass-glow glass-hover-lift rounded-2xl animate-fade-in">
            {/* Header */}
            <div className="flex items-center justify-between gap-3 glass-subtle rounded-t-2xl px-4 py-3">
                <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                        <h3 className="truncate text-sm font-semibold text-fg">
                            {cam.name}
                        </h3>
                        {badgeLabel && (
                            <Badge variant="info" className="shrink-0 text-[9px]">{badgeLabel}</Badge>
                        )}
                    </div>
                    <p className="truncate text-[11px] text-fg-muted">
                        {cam.vendor} · {cam.host}
                    </p>
                    {codecError && (
                        <p className="mt-0.5 truncate text-[10px] text-[rgb(var(--accent-danger))]" title={codecError}>
                            编码：{codecError}
                        </p>
                    )}
                </div>
                <div className="flex shrink-0 items-center gap-2">
                    {isAdmin && (
                        <div className="relative">
                            <Select
                                value={currentCodec}
                                onChange={onCodecChange}
                                disabled={codecLoading}
                                aria-label="输出编码"
                                title="输出编码（WebRTC 要求 H.264）"
                                className="glass-subtle rounded-lg h-7 w-[104px] px-1.5 py-0 text-[11px]"
                            >
                                {currentCodec !== "h264" && (
                                    <option value={currentCodec} disabled>
                                        {codecBadgeLabel(cam)}（旧版）
                                    </option>
                                )}
                                <option value="h264">H.264</option>
                            </Select>
                            {codecLoading && (
                                <Loader2 size={11} className="pointer-events-none absolute right-1.5 top-1/2 -translate-y-1/2 animate-spin text-fg-muted" />
                            )}
                        </div>
                    )}
                    <Badge variant={statusVariant} className="text-[10px]">
                        {cam.status === "online" ? "在线" : cam.status === "offline" ? "离线" : "未知"}
                    </Badge>
                    {isAdmin && (
                        <button
                            className="rounded-md p-1.5 text-fg-subtle transition-colors hover:bg-[rgb(var(--accent-danger)/0.1)] hover:text-[rgb(var(--accent-danger))]"
                            onClick={onDelete}
                            aria-label="删除摄像头"
                            title="删除摄像头"
                        >
                            <Trash2 size={14} />
                        </button>
                    )}
                </div>
            </div>

            {/* Video — LiveVideo now owns preview / live / playback
             * modes plus the recording list and the admin recording
             * toggle. The aspect-video wrapper is preserved so the
             * card keeps its frame shape before the LiveVideo Card
             * mounts its inner surface. */}
            <div className="relative aspect-video bg-black">
                <LiveVideo
                    camera={cam}
                    isAdmin={isAdmin}
                    onWsMessage={onWsMessage}
                    onRefresh={onRefresh}
                    targetTime={targetTime}
                />
            </div>
        </div>
    );
}

/**
 * WsBridge — child-as-render hook wrapper. The Cameras page owns
 * one WebSocket connection, and each CamCard subscribes to its own
 * "device.<id>" topic via the passed-down callback.
 */
function WsBridge({ children }: { children: (onMsg: (h: (m: WsMessage) => void) => () => void) => React.ReactNode }) {
    const ws = useWebSocket(true);
    const [last, setLast] = useState<WsMessage | null>(null);

    useEffect(() => {
        if (ws.lastMessage) setLast(ws.lastMessage);
    }, [ws.lastMessage]);

    // Provide each consumer a stable "give-me-the-latest-message"
    // hook. The actual message routing happens inside LiveVideo
    // (filters by device_id).
    const onMsg = useCallback(
        (h: (m: WsMessage) => void) => {
            // Force a re-render trigger by reading `last` so the
            // consumer re-subscribes only on identity change.
            void last;
            h(ws.lastMessage ?? { type: "noop", ts: 0 });
            // The page is the single WS owner; the consumer just
            // polls the latest message via the same channel. No
            // explicit unsubscribe is required.
            return () => undefined;
        },
        [last, ws.lastMessage],
    );

    return <>{children(onMsg)}</>;
}
