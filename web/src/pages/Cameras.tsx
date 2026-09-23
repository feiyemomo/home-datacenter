import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { Camera as CameraIcon, Plus, Trash2, RefreshCcw, Loader2, LayoutGrid, List, Maximize2, Sparkles } from "lucide-react";
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
import { Skeleton } from "@/components/Skeleton";
import FamilyFaceManager from "@/components/vision/FamilyFaceManager";
import { cn } from "@/lib/utils";

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
    const [viewMode, setViewMode] = useState<"list" | "grid4" | "vision">("list");

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
            <div className="animate-fade-in flex items-center justify-between pb-4 mb-2 border-b border-[rgb(var(--border)/0.3)]">
                <div className="flex items-center gap-3">
                    <div className="flex h-9 w-9 items-center justify-center rounded-xl bg-gradient-to-br from-[rgb(var(--accent-info)/0.15)] to-[rgb(var(--accent-primary)/0.08)] ring-1 ring-inset ring-[rgb(var(--accent-info)/0.15)] text-[rgb(var(--accent-info))]">
                        <CameraIcon size={18} />
                    </div>
                    <div>
                        <h2 className="text-base font-medium tracking-tight text-fg">摄像头</h2>
                        <p className="text-xs text-fg-muted">
                            {(cams ?? []).length} 个设备 · 实时监控与录像回放
                        </p>
                    </div>
                </div>
                <div className="flex items-center gap-2">
                    {/* View switcher */}
                    <div className="flex rounded-xl glass-subtle p-0.5 border border-[rgb(var(--border)/0.2)]">
                        <button
                            type="button"
                            onClick={() => setViewMode("list")}
                            className={cn(
                                "flex items-center gap-1 px-2.5 py-1 text-xs rounded-lg transition-all",
                                viewMode === "list"
                                    ? "bg-[rgb(var(--accent-primary)/0.2)] text-[rgb(var(--accent-primary))] font-medium shadow-sm"
                                    : "text-fg-muted hover:text-fg"
                            )}
                            title="卡片列表视图"
                        >
                            <List size={13} />
                            列表
                        </button>
                        <button
                            type="button"
                            onClick={() => setViewMode("grid4")}
                            className={cn(
                                "flex items-center gap-1 px-2.5 py-1 text-xs rounded-lg transition-all",
                                viewMode === "grid4"
                                    ? "bg-[rgb(var(--accent-primary)/0.2)] text-[rgb(var(--accent-primary))] font-medium shadow-sm"
                                    : "text-fg-muted hover:text-fg"
                            )}
                            title="四分屏同屏矩阵视图"
                        >
                            <LayoutGrid size={13} />
                            四分屏
                        </button>
                        <button
                            type="button"
                            onClick={() => setViewMode("vision")}
                            className={cn(
                                "flex items-center gap-1 px-2.5 py-1 text-xs rounded-lg transition-all",
                                viewMode === "vision"
                                    ? "bg-[rgb(var(--accent-primary)/0.2)] text-[rgb(var(--accent-primary))] font-medium shadow-sm"
                                    : "text-fg-muted hover:text-fg"
                            )}
                            title="家人识别与 AI 视觉感知管理"
                        >
                            <Sparkles size={13} />
                            家人与 AI 视觉
                        </button>
                    </div>

                    <Button
                        size="sm"
                        variant="outline"
                        onClick={refetch}
                        disabled={loading}
                    >
                        <RefreshCcw size={14} className={loading ? "animate-spin mr-1.5" : "mr-1.5"} />
                        刷新
                    </Button>
                    {isAdmin && (
                        <Button
                            size="sm"
                            onClick={() => nav("/cameras/new")}
                        >
                            <Plus size={14} className="mr-1.5" />
                            注册
                        </Button>
                    )}
                </div>
            </div>

            {fetchError && (
                <ErrorRetry message={fetchError.message} onRetry={refetch} />
            )}
            {error && (
                <div className="animate-fade-in rounded-2xl bg-[rgb(var(--accent-danger)/0.08)] px-4 py-3 text-sm text-[rgb(var(--accent-danger)/0.9)]">
                    {error}
                </div>
            )}

            <WsBridge>
                {(onMsg) => (
                    viewMode === "vision" ? (
                        <FamilyFaceManager />
                    ) : viewMode === "grid4" ? (
                        <div id="multi-cam-grid" className="space-y-4 animate-fade-in">
                            <div className="flex items-center justify-between px-1">
                                <span className="text-xs text-fg-muted font-medium">
                                    同屏实时监控（并发最多 4 路实时画面）
                                </span>
                                <Button
                                    size="sm"
                                    variant="ghost"
                                    className="h-7 px-2 text-xs text-fg-muted hover:text-fg"
                                    onClick={() => {
                                        const el = document.getElementById("multi-cam-grid");
                                        if (!document.fullscreenElement) {
                                            el?.requestFullscreen().catch(() => {});
                                        } else {
                                            document.exitFullscreen().catch(() => {});
                                        }
                                    }}
                                >
                                    <Maximize2 size={13} className="mr-1" />
                                    全屏矩阵
                                </Button>
                            </div>
                            <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
                                {(cams ?? []).slice(0, 4).map((cam) => (
                                    <div key={cam.id} className="relative rounded-2xl overflow-hidden glass border border-[rgb(var(--border)/0.3)] shadow-lg group">
                                        <div className="absolute top-0 inset-x-0 z-20 flex items-center justify-between px-3 py-1.5 bg-gradient-to-b from-black/80 via-black/40 to-transparent">
                                            <span className="text-xs font-medium text-white truncate drop-shadow">{cam.name}</span>
                                            <div className="flex items-center gap-1.5">
                                                <Badge variant={cam.status === "online" ? "success" : "danger"} className="text-[9px] py-0 px-1.5 h-4">
                                                    {cam.status === "online" ? "实时" : "离线"}
                                                </Badge>
                                                <button
                                                    type="button"
                                                    onClick={() => setViewMode("list")}
                                                    className="text-[10px] text-white/70 hover:text-white underline ml-1"
                                                    title="在单路详情中打开"
                                                >
                                                    详情
                                                </button>
                                            </div>
                                        </div>
                                        <div className="aspect-video w-full bg-black/90">
                                            <LiveVideo camera={cam} isAdmin={isAdmin} onWsMessage={onMsg} />
                                        </div>
                                    </div>
                                ))}
                                {(cams ?? []).length === 0 && !loading && (
                                    <div className="col-span-full rounded-2xl border border-[rgb(var(--border)/0.2)] p-10 text-center">
                                        <p className="text-sm font-medium text-fg">暂无在线摄像头</p>
                                    </div>
                                )}
                            </div>
                        </div>
                    ) : (
                        <div className="grid grid-cols-1 gap-5 md:grid-cols-2 xl:grid-cols-3 stagger-children">
                            {loading ? (
                                [0, 1, 2].map((i) => (
                                    <div key={i} className="flex flex-col overflow-hidden glass rounded-2xl">
                                        <div className="flex items-center justify-between gap-3 px-4 py-2.5">
                                            <div className="min-w-0 flex-1 space-y-2">
                                                <Skeleton className="h-4 w-32" />
                                                <Skeleton className="h-3 w-24" />
                                            </div>
                                            <Skeleton className="h-6 w-16 rounded-full" />
                                        </div>
                                        <Skeleton className="aspect-video w-full rounded-none" />
                                    </div>
                                ))
                            ) : (
                                <>
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
                                        <div className="col-span-full rounded-2xl border border-[rgb(var(--border)/0.2)] p-10 text-center animate-fade-in">
                                            <div className="mx-auto mb-4 flex h-16 w-16 items-center justify-center rounded-full bg-[rgb(var(--bg-subtle)/0.4)]">
                                                <CameraIcon size={28} className="text-fg-subtle" />
                                            </div>
                                            <p className="text-sm font-medium text-fg">暂无注册的摄像头</p>
                                            <p className="mt-1 text-xs text-fg-muted">
                                                {isAdmin ? "点击右上角「注册」按钮添加第一个摄像头。" : "请联系管理员添加摄像头设备。"}
                                            </p>
                                        </div>
                                    )}
                                </>
                            )}
                        </div>
                    )
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
        <div className="group flex flex-col overflow-hidden glass card-lift rounded-2xl animate-fade-in shadow-[0_0_20px_rgb(var(--accent-warm)/0.06)]">
            {/* Header */}
            <div className="flex items-center justify-between gap-3 px-4 py-2.5">
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
 * WsBridge — observer pattern wrapper for camera WS events.
 */
function WsBridge({ children }: { children: (onMsg: (h: (m: WsMessage) => void) => () => void) => React.ReactNode }) {
    const ws = useWebSocket(true);
    const listenersRef = useRef<Set<(m: WsMessage) => void>>(new Set());

    useEffect(() => {
        ws.subscribe("device");
        ws.subscribe("camera");
    }, [ws.subscribe]);

    useEffect(() => {
        if (!ws.lastMessage) return;
        listenersRef.current.forEach((handler) => {
            try {
                handler(ws.lastMessage!);
            } catch {
                // Ignore listener error
            }
        });
    }, [ws.lastMessage]);

    const onMsg = useCallback((h: (m: WsMessage) => void) => {
        listenersRef.current.add(h);
        return () => {
            listenersRef.current.delete(h);
        };
    }, []);

    return <>{children(onMsg)}</>;
}
