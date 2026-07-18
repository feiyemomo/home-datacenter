import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Camera as CameraIcon, Plus, Trash2, RefreshCcw, Loader2, ChevronDown, ChevronRight, Play, Square } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Select } from "@/components/ui/input";
import { listCameras, deleteCamera, updateCodec, setRecordingPlan, listRecordings, deleteRecording } from "@/api/camera";
import { authHeaderFor } from "@/api/client";
import type { Camera, CameraRecording, WsMessage } from "@/types";
import { useAuth } from "@/hooks/useAuth";
import { useWebSocket } from "@/hooks/useWebSocket";
import { LiveVideo } from "@/components/LiveVideo";

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

/** Format a recording duration (seconds) as h:mm:ss or m:ss. */
function formatDuration(sec: number): string {
    if (!Number.isFinite(sec) || sec < 0) return "--";
    const s = Math.floor(sec % 60);
    const m = Math.floor(sec / 60) % 60;
    const h = Math.floor(sec / 3600);
    const pad = (n: number) => n.toString().padStart(2, "0");
    return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`;
}

/**
 * Cameras — list + live view + delete.
 *
 * Registration has moved to a dedicated page (/cameras/new,
 * DeviceCreate.tsx). The list page is now strictly for *browsing*
 * — the operator can refresh, watch live, and remove a camera, but
 * not stand up a new one inline. This keeps the cards above the
 * fold and gives the create flow its own URL to bookmark / share.
 */
export default function Cameras() {
    const { isAdmin } = useAuth();
    const nav = useNavigate();
    const [cams, setCams] = useState<Camera[]>([]);
    const [loading, setLoading] = useState(false);
    const [error, setError] = useState<string | null>(null);

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
        if (!confirm(`Delete camera ${id}?`)) return;
        try {
            await deleteCamera(id);
            await refresh();
        } catch (e) {
            setError(e instanceof Error ? e.message : String(e));
        }
    }

    return (
        <div className="space-y-4">
            <div className="flex items-center justify-between">
                <div className="flex items-center gap-2">
                    <CameraIcon className="h-5 w-5 text-sky-600 dark:text-sky-300" />
                    <h2 className="text-lg font-semibold text-fg">Cameras</h2>
                    <Badge variant="outline">{cams.length}</Badge>
                </div>
                <div className="flex gap-2">
                    <Button size="sm" variant="outline" onClick={refresh} disabled={loading}>
                        <RefreshCcw size={14} className="mr-1" />
                        Refresh
                    </Button>
                    {isAdmin && (
                        <Button size="sm" onClick={() => nav("/cameras/new")}>
                            <Plus size={14} className="mr-1" />
                            Register
                        </Button>
                    )}
                </div>
            </div>

            {error && (
                <div className="rounded-md border border-rose-500/40 bg-rose-500/10 px-3 py-2 text-sm text-rose-700 dark:text-rose-200">
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
                            />
                        ))}
                        {cams.length === 0 && !loading && (
                            <div className="col-span-full rounded-md border border-dashed border-surface-border p-8 text-center text-sm text-fg-muted">
                                No cameras registered. {isAdmin ? "Click Register to add one." : "Ask an admin to register one."}
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
}: {
    cam: Camera;
    isAdmin: boolean;
    onDelete: () => void;
    onRefresh: () => void | Promise<void>;
    onWsMessage: (handler: (m: WsMessage) => void) => () => void;
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

    async function onCodecChange(e: React.ChangeEvent<HTMLSelectElement>) {
        if (!isAdmin) return;
        const next = e.target.value as CodecOption;
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
        <div className="flex flex-col overflow-hidden rounded-xl border border-surface-border bg-surface-raised shadow-sm shadow-black/5 transition-shadow hover:shadow-md hover:shadow-black/10 dark:bg-surface-raised/90 dark:shadow-black/20 dark:hover:shadow-black/30">
            {/* Header */}
            <div className="flex items-center justify-between gap-3 border-b border-surface-border bg-surface-subtle/40 px-4 py-3">
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
                        <p className="mt-0.5 truncate text-[10px] text-rose-600 dark:text-rose-300" title={codecError}>
                            codec: {codecError}
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
                                aria-label="Output codec"
                                title="Output codec"
                                className="h-7 w-[104px] rounded-md px-1.5 py-0 text-[11px]"
                            >
                                <option value="passthrough">直通</option>
                                <option value="h264">H.264</option>
                                <option value="h265">H.265</option>
                            </Select>
                            {codecLoading && (
                                <Loader2 size={11} className="pointer-events-none absolute right-1.5 top-1/2 -translate-y-1/2 animate-spin text-fg-muted" />
                            )}
                        </div>
                    )}
                    <Badge variant={statusVariant} className="text-[10px] capitalize">
                        {cam.status}
                    </Badge>
                    {isAdmin && (
                        <button
                            className="rounded-md p-1.5 text-fg-subtle transition-colors hover:bg-rose-500/10 hover:text-rose-600 dark:hover:text-rose-300"
                            onClick={onDelete}
                            aria-label="Delete camera"
                            title="Delete camera"
                        >
                            <Trash2 size={14} />
                        </button>
                    )}
                </div>
            </div>

            {/* Video */}
            <div className="relative aspect-video bg-black">
                <LiveVideo camera={cam} isAdmin={isAdmin} onWsMessage={onWsMessage} />
            </div>

            {/* Recording panel */}
            <RecordingPanel cam={cam} isAdmin={isAdmin} onRefresh={onRefresh} />
        </div>
    );
}

/**
 * RecordingPanel — collapsible per-camera recording control.
 *
 * - Toggle the recording plan (admin only) via setRecordingPlan.
 * - Lazy-load the most recent 20 recordings when first expanded.
 * - Play a recording by fetching it as a blob with the JWT
 *   Authorization header attached (the file endpoint is behind
 *   JWTAuth, and <video src> cannot set headers), then handing
 *   the blob URL to a <video controls> element. The blob URL is
 *   revoked on stop / switch / unmount to avoid leaks.
 * - Delete a recording (admin only) and refresh the list.
 */
function RecordingPanel({
    cam,
    isAdmin,
    onRefresh,
}: {
    cam: Camera;
    isAdmin: boolean;
    onRefresh: () => void | Promise<void>;
}) {
    const [open, setOpen] = useState(false);
    const [recordings, setRecordings] = useState<CameraRecording[]>([]);
    const [loading, setLoading] = useState(false);
    const [toggling, setToggling] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const [playingId, setPlayingId] = useState<number | null>(null);
    const [videoUrl, setVideoUrl] = useState<string | null>(null);
    const [fetchingPlay, setFetchingPlay] = useState<number | null>(null);

    // Track the current blob URL so we can revoke it before
    // replacing it or on unmount. Keeping it in a ref lets the
    // cleanup function read the latest value without re-running.
    const urlRef = useRef<string | null>(null);
    useEffect(() => {
        return () => {
            if (urlRef.current) {
                URL.revokeObjectURL(urlRef.current);
                urlRef.current = null;
            }
        };
    }, []);

    const recordingEnabled = cam.meta.recording?.enabled ?? false;

    const loadRecordings = useCallback(async () => {
        setLoading(true);
        setError(null);
        try {
            const list = await listRecordings(cam.id, 20);
            setRecordings(list);
        } catch (e) {
            setError(e instanceof Error ? e.message : String(e));
        } finally {
            setLoading(false);
        }
    }, [cam.id]);

    async function onToggleOpen() {
        const next = !open;
        setOpen(next);
        if (next && recordings.length === 0 && !loading) {
            await loadRecordings();
        }
    }

    async function onToggleRecording() {
        if (!isAdmin || toggling) return;
        setToggling(true);
        setError(null);
        try {
            await setRecordingPlan(cam.id, {
                enabled: !recordingEnabled,
                segment_seconds: 600,
                retention_days: 7,
            });
            await onRefresh();
        } catch (e) {
            setError(e instanceof Error ? e.message : String(e));
        } finally {
            setToggling(false);
        }
    }

    function revokeCurrentUrl() {
        if (urlRef.current) {
            URL.revokeObjectURL(urlRef.current);
            urlRef.current = null;
        }
        setVideoUrl(null);
    }

    async function onPlay(rec: CameraRecording) {
        if (fetchingPlay !== null) return;
        // Stop any current playback first.
        revokeCurrentUrl();
        setPlayingId(null);
        setFetchingPlay(rec.id);
        setError(null);
        try {
            const h = authHeaderFor();
            const resp = await fetch(
                `/api/v1/cameras/${cam.id}/recordings/${rec.id}/file`,
                { headers: h ? { [h.name]: h.value } : {} },
            );
            if (!resp.ok) {
                throw new Error(`HTTP ${resp.status}${resp.statusText ? ` ${resp.statusText}` : ""}`);
            }
            const blob = await resp.blob();
            const url = URL.createObjectURL(blob);
            urlRef.current = url;
            setVideoUrl(url);
            setPlayingId(rec.id);
        } catch (e) {
            setError(e instanceof Error ? e.message : String(e));
        } finally {
            setFetchingPlay(null);
        }
    }

    function onStopPlay() {
        revokeCurrentUrl();
        setPlayingId(null);
    }

    async function onDeleteRec(rec: CameraRecording) {
        if (!isAdmin) return;
        if (!confirm(`Delete recording ${rec.id}?`)) return;
        try {
            if (playingId === rec.id) onStopPlay();
            await deleteRecording(cam.id, rec.id);
            await loadRecordings();
        } catch (e) {
            setError(e instanceof Error ? e.message : String(e));
        }
    }

    return (
        <div className="border-t border-surface-border bg-surface-subtle/30">
            {/* Header row: always-visible recording toggle + collapse button.
                The recording switch lives here (not inside the collapsible
                body) so the operator can start/stop recording without
                expanding the panel. */}
            <div className="flex items-center justify-between gap-2 px-4 py-2">
                {/* Left: recording label + status */}
                <div className="flex min-w-0 items-center gap-2">
                    <button
                        type="button"
                        onClick={onToggleOpen}
                        className="flex items-center gap-1.5 text-xs font-medium text-fg-muted transition-colors hover:text-fg"
                        aria-expanded={open}
                        aria-label="Toggle recording list"
                    >
                        {open ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
                        录制
                    </button>
                    {recordingEnabled ? (
                        <Badge variant="success" className="text-[9px]">ON</Badge>
                    ) : (
                        <Badge variant="outline" className="text-[9px]">OFF</Badge>
                    )}
                    {!recordingEnabled && recordings.length > 0 && (
                        <Badge variant="outline" className="text-[9px]">{recordings.length}</Badge>
                    )}
                    <span className="truncate text-[10px] text-fg-subtle">
                        {recordingEnabled
                            ? `${cam.meta.recording?.segment_seconds ?? 600}s · ${cam.meta.recording?.retention_days ?? 7}d`
                            : ""}
                    </span>
                </div>
                {/* Right: recording toggle button (always visible) */}
                {isAdmin ? (
                    <Button
                        size="sm"
                        variant={recordingEnabled ? "danger" : "primary"}
                        onClick={onToggleRecording}
                        disabled={toggling}
                        className="h-7 px-2.5 text-[11px]"
                    >
                        {toggling && <Loader2 size={12} className="animate-spin" />}
                        {recordingEnabled ? "停止录制" : "启用录制"}
                    </Button>
                ) : null}
            </div>

            {open && (
                <div className="space-y-2 px-4 pb-3 pt-1">
                    {error && (
                        <div className="rounded-md border border-rose-500/40 bg-rose-500/10 px-2.5 py-1.5 text-[11px] text-rose-700 dark:text-rose-200">
                            {error}
                        </div>
                    )}

                    {/* Recording list */}
                    <div className="space-y-1">
                        <div className="flex items-center justify-between text-[10px] uppercase tracking-wider text-fg-subtle">
                            <span>最近录制</span>
                            <button
                                type="button"
                                onClick={loadRecordings}
                                disabled={loading}
                                className="inline-flex items-center gap-1 text-fg-muted transition-colors hover:text-fg disabled:opacity-50"
                            >
                                <RefreshCcw size={10} className={loading ? "animate-spin" : ""} />
                                刷新
                            </button>
                        </div>

                        {loading && recordings.length === 0 ? (
                            <div className="flex items-center justify-center py-4 text-[11px] text-fg-muted">
                                <Loader2 size={12} className="mr-1 animate-spin" />
                                加载中…
                            </div>
                        ) : recordings.length === 0 ? (
                            <div className="py-3 text-center text-[11px] text-fg-subtle">
                                暂无录制
                            </div>
                        ) : (
                            <ul className="max-h-56 space-y-1 overflow-y-auto pr-0.5">
                                {recordings.map((rec) => (
                                    <li
                                        key={rec.id}
                                        className="flex items-center gap-2 rounded-md border border-surface-border bg-surface-raised/60 px-2 py-1.5 text-[11px]"
                                    >
                                        <div className="min-w-0 flex-1">
                                            <div className="truncate font-medium text-fg">
                                                {new Date(rec.start_at).toLocaleString()}
                                            </div>
                                            <div className="truncate text-[10px] text-fg-muted">
                                                {formatDuration(rec.duration_seconds)} · {rec.size_human}
                                            </div>
                                        </div>
                                        {playingId === rec.id && videoUrl ? (
                                            <button
                                                type="button"
                                                onClick={onStopPlay}
                                                className="inline-flex h-6 items-center gap-1 rounded-md border border-surface-border bg-surface-subtle px-2 text-[10px] text-fg-muted transition-colors hover:text-fg"
                                                title="停止播放"
                                            >
                                                <Square size={10} />
                                                停止
                                            </button>
                                        ) : (
                                            <button
                                                type="button"
                                                onClick={() => onPlay(rec)}
                                                disabled={fetchingPlay !== null}
                                                className="inline-flex h-6 items-center gap-1 rounded-md bg-sky-500/15 px-2 text-[10px] text-sky-700 transition-colors hover:bg-sky-500/25 disabled:opacity-50 dark:text-sky-300"
                                                title="播放"
                                            >
                                                {fetchingPlay === rec.id ? (
                                                    <Loader2 size={10} className="animate-spin" />
                                                ) : (
                                                    <Play size={10} />
                                                )}
                                                播放
                                            </button>
                                        )}
                                        {isAdmin && (
                                            <button
                                                type="button"
                                                onClick={() => onDeleteRec(rec)}
                                                className="inline-flex h-6 items-center justify-center rounded-md p-1 text-fg-subtle transition-colors hover:bg-rose-500/10 hover:text-rose-600 dark:hover:text-rose-300"
                                                aria-label="Delete recording"
                                                title="删除录制"
                                            >
                                                <Trash2 size={11} />
                                            </button>
                                        )}
                                    </li>
                                ))}
                            </ul>
                        )}
                    </div>

                    {/* Inline video player */}
                    {videoUrl && playingId !== null && (
                        <div className="overflow-hidden rounded-md border border-surface-border bg-black">
                            <video
                                key={videoUrl}
                                src={videoUrl}
                                controls
                                autoPlay
                                className="aspect-video w-full bg-black"
                            />
                        </div>
                    )}
                </div>
            )}
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
