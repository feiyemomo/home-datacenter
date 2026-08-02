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
import { useCachedFetch } from "@/hooks/useCachedFetch";
import { LiveVideo } from "@/components/LiveVideo";
import { ErrorRetry } from "@/components/ErrorRetry";

type CodecOption = "passthrough" | "h264" | "h265";

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
 * Cameras — list + live view + delete with enhanced liquid glass styling.
 */
export default function Cameras() {
    const { isAdmin } = useAuth();
    const nav = useNavigate();
    const [error, setError] = useState<string | null>(null);
    const [searchParams] = useSearchParams();

    const targetCameraId = searchParams.get("camera") ? Number(searchParams.get("camera")) : undefined;
    const targetTime = searchParams.get("time") ? Number(searchParams.get("time")) : undefined;

    const { data: cams, loading, error: fetchError, refetch, mutate } = useCachedFetch<Camera[]>(
        "home.cameras.list",
        listCameras,
        { refetchMs: 30000 }
    );

    async function remove(id: number) {
        if (!isAdmin) return;
        if (!confirm(`确认删除摄像头 ${id}？`)) return;
        try {
            await deleteCamera(id);
            const list = await listCameras();
            mutate(list);
        } catch (e) {
            setError(e instanceof Error ? e.message : String(e));
        }
    }

    return (
        <div className="space-y-5 animate-fade-in">
            {/* Page header */}
            <div className="glass-subtle relative overflow-hidden rounded-2xl px-5 py-4 card-lift">
                <div className="pointer-events-none absolute inset-y-0 left-0 w-1 bg-gradient-to-b from-[rgb(var(--accent-info)/0.8)] via-[rgb(var(--accent-primary)/0.5)] to-transparent" />
                <div className="pointer-events-none absolute -right-8 -top-8 h-28 w-28 rounded-full bg-[rgb(var(--accent-info)/0.1)] blur-3xl" />
                <div className="relative flex items-center justify-between">
                    <div className="flex items-center gap-3">
                        <div className="flex h-10 w-10 items-center justify-center rounded-xl bg-gradient-to-br from-[rgb(var(--accent-info)/0.2)] to-[rgb(var(--accent-primary)/0.1)] ring-1 ring-inset ring-[rgb(var(--accent-info)/0.2)] text-[rgb(var(--accent-info))]">
                            <CameraIcon size={20} />
                        </div>
                        <div>
                            <h2 className="text-lg font-semibold tracking-tight text-fg">摄像头</h2>
                            <p className="text-xs text-fg-muted">
                                {(cams ?? []).length} 个设备 · 实时监控与录像回放
                            </p>
                        </div>
                    </div>
                    <div className="flex gap-2">
                        <Button
                            size="sm"
                            variant="outline"
                            onClick={refetch}
                            disabled={loading}
                            className="glass-subtle hover:glass transition-all"
                        >
                            <RefreshCcw size={14} className={loading ? "animate-spin mr-1.5" : "mr-1.5"} />
                            刷新
                        </Button>
                        {isAdmin && (
                            <Button
                                size="sm"
                                onClick={() => nav("/cameras/new")}
                                className="shadow-lg shadow-[rgb(var(--accent-primary)/0.2)] transition-all hover:scale-105"
                            >
                                <Plus size={14} className="mr-1.5" />
                                注册
                            </Button>
                        )}
                    </div>
                </div>
            </div>

            {fetchError && (
                <ErrorRetry message={fetchError.message} onRetry={refetch} />
            )}
            {error && (
                <div className="glass rounded-2xl bg-gradient-to-r from-[rgb(var(--accent-danger)/0.12)] to-[rgb(var(--accent-danger)/0.04)] px-4 py-3 text-sm text-[rgb(var(--accent-danger))] border border-[rgb(var(--accent-danger)/0.2)]">
                    {error}
                </div>
            )}

            <WsBridge>
                {(onMsg) => (
                    <div className="grid grid-cols-1 gap-5 md:grid-cols-2 xl:grid-cols-3 stagger-children">
                        {(cams ?? []).map((cam) => (
                            <CamCard
                                key={cam.id}
                                cam={cam}
                                isAdmin={isAdmin}
                                onDelete={() => remove(cam.id)}
                                onRefresh={refetch}
                                onWsMessage={onMsg}
                                targetTime={(targetCameraId === cam.id) ? targetTime : undefined}
                            />
                        ))}
                        {(cams ?? []).length === 0 && !loading && (
                            <div className="col-span-full glass glass-glow rounded-2xl p-10 text-center animate-fade-in">
                                <div className="mx-auto mb-4 flex h-16 w-16 items-center justify-center rounded-2xl glass-subtle">
                                    <CameraIcon size={28} className="text-fg-subtle" />
                                </div>
                                <p className="text-sm font-medium text-fg">暂无注册的摄像头</p>
                                <p className="mt-1 text-xs text-fg-muted">
                                    {isAdmin ? "点击右上角「注册」按钮添加第一个摄像头。" : "请联系管理员添加摄像头设备。"}
                                </p>
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

    const currentCodec: CodecOption = cam.codec
        ? (cam.codec as CodecOption)
        : cam.transcode
            ? "h264"
            : "passthrough";

    async function onCodecChange(_: React.ChangeEvent<HTMLSelectElement>) {
        if (!isAdmin) return;
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
        <div className="group flex flex-col overflow-hidden glass glass-glow glass-hover-lift rounded-2xl animate-fade-in">
            {/* Header */}
            <div className="flex items-center justify-between gap-3 glass-subtle px-4 py-3 border-b border-[rgb(var(--border)/0.15)]">
                <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                        <h3 className="truncate text-sm font-semibold tracking-tight text-fg">
                            {cam.name}
                        </h3>
                        {badgeLabel && (
                            <Badge variant="info" className="shrink-0 text-[9px] shadow-sm">{badgeLabel}</Badge>
                        )}
                    </div>
                    <p className="truncate text-[11px] text-fg-muted mt-0.5">
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
                                className="glass-subtle rounded-lg h-7 w-[104px] px-2 py-0 text-[11px] transition-all hover:glass"
                            >
                                {currentCodec !== "h264" && (
                                    <option value={currentCodec} disabled>
                                        {codecBadgeLabel(cam)}（旧版）
                                    </option>
                                )}
                                <option value="h264">H.264</option>
                            </Select>
                            {codecLoading && (
                                <Loader2 size={11} className="pointer-events-none absolute right-2 top-1/2 -translate-y-1/2 animate-spin text-fg-muted" />
                            )}
                        </div>
                    )}
                    <Badge variant={statusVariant} className="text-[10px] shadow-sm gap-1">
                        <span className={`inline-block h-1.5 w-1.5 rounded-full ${cam.status === "online" ? "bg-current pulse-dot" : "bg-current"}`} />
                        {cam.status === "online" ? "在线" : cam.status === "offline" ? "离线" : "未知"}
                    </Badge>
                    {isAdmin && (
                        <button
                            className="rounded-lg p-1.5 text-fg-subtle transition-all hover:bg-[rgb(var(--accent-danger)/0.1)] hover:text-[rgb(var(--accent-danger))] hover:scale-110"
                            onClick={onDelete}
                            aria-label="删除摄像头"
                            title="删除摄像头"
                        >
                            <Trash2 size={14} />
                        </button>
                    )}
                </div>
            </div>

            {/* Video */}
            <div className="relative aspect-video bg-black/50">
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
 * WsBridge — child-as-render hook wrapper.
 */
function WsBridge({ children }: { children: (onMsg: (h: (m: WsMessage) => void) => () => void) => React.ReactNode }) {
    const ws = useWebSocket(true);
    const [last, setLast] = useState<WsMessage | null>(null);

    useEffect(() => {
        if (ws.lastMessage) setLast(ws.lastMessage);
    }, [ws.lastMessage]);

    const onMsg = useCallback(
        (h: (m: WsMessage) => void) => {
            void last;
            h(ws.lastMessage ?? { type: "noop", ts: 0 });
            return () => undefined;
        },
        [last, ws.lastMessage],
    );

    return <>{children(onMsg)}</>;
}
