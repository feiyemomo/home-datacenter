import { useCallback, useEffect, useRef, useState } from "react";
import { ChevronUp, ChevronDown, ChevronLeft, ChevronRight, ZoomIn, ZoomOut, Square, AlertTriangle, Loader2, RefreshCw, Power, Play, Trash2, RefreshCcw } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { useWebRTCStream } from "@/hooks/useWebRTCStream";
import { useHLSStream } from "@/hooks/useHLSStream";
import {
    ptzMove,
    gotoPreset,
    cameraFrameUrl,
    listRecordings,
    deleteRecording,
    setRecordingPlan,
} from "@/api/camera";
import { authHeaderFor } from "@/api/client";
import type { Camera, CameraEventMessage, CameraRecording, CameraStatusEvent, WsMessage } from "@/types";

interface LiveVideoProps {
    camera: Camera;
    isAdmin: boolean;
    /** Subscribe to a single topic and dispatch parsed events. */
    onWsMessage?: (handler: (m: WsMessage) => void) => () => void;
    /** Refresh camera data (used after recording plan changes). */
    onRefresh?: () => void | Promise<void>;
    /** When set, auto-switch to playback mode and play the matching recording. */
    targetTime?: number;
}

/**
 * Transport preference for the live stream.
 *
 *   auto    — try WebRTC first, fall back to HLS on failure
 *             (default; works on every browser + every codec)
 *   webrtc  — WebRTC only. If the SDP exchange fails (codec /
 *             ICE / network), surface the error and offer Retry.
 *             Useful when the operator wants to confirm a
 *             transcoding fix landed without HLS masking the
 *             regression.
 *   hls     — HLS only. Force the fragmented-MP4 path so the
 *             operator can compare against WebRTC during
 *             codec-bug triage.
 *
 * The preference is global (one toggle for all cameras) and
 * persisted in localStorage. Per-camera preference was
 * considered but adds UI surface for a feature that
 * almost-always wants the same value across cameras.
 */
export type TransportMode = "auto" | "webrtc" | "hls";

const TRANSPORT_KEY = "home.transport";

function readTransport(): TransportMode {
    if (typeof window === "undefined") return "auto";
    try {
        const v = window.localStorage.getItem(TRANSPORT_KEY);
        if (v === "auto" || v === "webrtc" || v === "hls") return v;
    } catch { /* private browsing etc. */ }
    return "auto";
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
 * LiveVideo — one camera with three modes:
 *
 *   preview  — static JPEG snapshot + Play button (default). Avoids
 *              spinning up a WebRTC/HLS connection for every camera
 *              on the page until the operator actually wants video.
 *   live     — WebRTC/HLS live stream with PTZ + transport picker.
 *   playback — recording list + inline `<video>` player.
 *
 * Streaming strategy (Phase 6)
 * ---------------------------
 *
 *   1. WebRTC (primary). Lowest latency. We always try this first.
 *   2. HLS (fallback). If WebRTC fails (SDP 5xx, ICE, codec, or any
 *      other error), we tear down the WebRTC peer connection and
 *      remount with hls.js bound to the HLS URL. HLS requires an
 *      HEVC-capable browser (see docs/platformization.md
 *      "Browser / Codec requirement"); if HLS also fails we surface
 *      the error and offer Retry.
 *
 * The two paths live in independent <WebRTCVideo> / <HLSVideo>
 * sub-components so React unmounts one when the other takes over —
 * a fresh hook tree is the only way to guarantee a clean state
 * transition between two independent playback engines (each owns
 * its own videoRef and pc/hls instance). Unmounting causes a
 * brief black flash, which is acceptable because fallback is a
 * recovery path, not a routine mode.
 *
 * The `transport` prop overrides this default policy: forcing
 * WebRTC or HLS suppresses the auto-fallback so the operator can
 * pin a single transport for debugging.
 *
 * Online status is updated either from the camera row's `status`
 * field (initial render) or from the WebSocket "device.<id>.status"
 * event the parent routes in via onWsMessage.
 *
 * Recording playback uses the same JWT-authenticated blob fetch
 * path that the previous standalone RecordingPanel used: fetch
 * the MP4 with the Authorization header attached (the file
 * endpoint is behind JWTAuth, and <video src> cannot set
 * headers), then hand the blob URL to a <video controls>
 * element. The blob URL is revoked on stop / switch / unmount.
 */
export function LiveVideo({ camera, isAdmin, onWsMessage, onRefresh, targetTime }: LiveVideoProps) {
    // Mode drives which surface is mounted. Preview is the
    // cheap default; live/playback mount their respective
    // engines. Switching back to preview unmounts both,
    // tearing down the WebRTC peer connection / HLS session
    // and revoking any in-flight recording blob URL.
    const [mode, setMode] = useState<"preview" | "live" | "playback">("preview");

    // Path drives which sub-component is mounted. We start on
    // WebRTC and flip to HLS on the primary's onError (in auto
    // mode only; explicit WebRTC/HLS selections are sticky).
    const [transport, setTransport] = useState<TransportMode>(readTransport);
    // Internal effective path. Differs from `transport` only
    // in "auto" mode where a WebRTC failure flipped us to HLS.
    const [path, setPath] = useState<"webrtc" | "hls">("webrtc");
    // Generation counter increments on every retry; changing it
    // forces a remount of the active sub-component.
    const [generation, setGeneration] = useState(0);

    function retry() {
        setGeneration((g) => g + 1);
    }

    // Reset to the requested transport on camera change. For
    // "auto" we start on WebRTC; for explicit selections we
    // honor the choice immediately.
    useEffect(() => {
        setPath(transport === "hls" ? "hls" : "webrtc");
        setGeneration(0);
    }, [camera.id, transport]);

    // Persist transport changes to localStorage. Survives reloads
    // and propagates to other tabs via the `storage` event (the
    // hook is re-read in those tabs the next time they remount).
    useEffect(() => {
        try {
            window.localStorage.setItem(TRANSPORT_KEY, transport);
        } catch { /* private browsing — accept loss of persistence */ }
    }, [transport]);

    const [status, setStatus] = useState<"online" | "offline" | "unknown">(camera.status);
    const [lastSeen, setLastSeen] = useState(camera.last_seen_at);
    const [busy, setBusy] = useState(false);
    const [ptzError, setPtzError] = useState<string | null>(null);
    const [eventToast, setEventToast] = useState<string | null>(null);

    // Subscribe to "device.<id>" over the existing WS layer.
    useEffect(() => {
        if (!onWsMessage) return;
        const off = onWsMessage((m) => {
            if (m.type !== "event") return;
            const p = m.payload as CameraStatusEvent | CameraEventMessage | null;
            if (!p) return;
            if (p.device_id !== camera.id) return;
            // status events
            if ("status" in p && (p as CameraStatusEvent).type === "camera") {
                const s = p as CameraStatusEvent;
                setStatus(s.status as "online" | "offline" | "unknown");
                setLastSeen(new Date(s.ts * 1000).toISOString());
                return;
            }
            // motion/AI events — show a brief toast in the corner
            if ("event" in p && (p as CameraEventMessage).type === "camera") {
                const ev = p as CameraEventMessage;
                setEventToast(`${ev.event} @ ${new Date(ev.ts * 1000).toLocaleTimeString()}`);
                const t = window.setTimeout(() => setEventToast(null), 4000);
                return () => window.clearTimeout(t);
            }
        });
        return off;
    }, [camera.id, onWsMessage]);

    async function sendPTZ(command: string) {
        if (!isAdmin) return;
        setBusy(true);
        setPtzError(null);
        try {
            await ptzMove(camera.id, command as never, 0.5);
        } catch (e) {
            setPtzError(e instanceof Error ? e.message : String(e));
        } finally {
            setBusy(false);
        }
    }

    async function sendPreset(alias: string) {
        if (!isAdmin) return;
        setBusy(true);
        setPtzError(null);
        try {
            await gotoPreset(camera.id, alias, 0.5);
        } catch (e) {
            setPtzError(e instanceof Error ? e.message : String(e));
        } finally {
            setBusy(false);
        }
    }

    // ----------------- Recording playback state -----------------
    // Ported from the previous standalone RecordingPanel. The
    // playback mode mounts this state; leaving playback mode
    // revokes any in-flight blob URL so we don't leak the MP4.
    const [recordings, setRecordings] = useState<CameraRecording[]>([]);
    const [loadingRecs, setLoadingRecs] = useState(false);
    const [toggling, setToggling] = useState(false);
    const [recError, setRecError] = useState<string | null>(null);
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

    const recordingEnabled = camera.meta.recording?.enabled ?? false;

    const loadRecordings = useCallback(async () => {
        setLoadingRecs(true);
        setRecError(null);
        try {
            const list = await listRecordings(camera.id, 20);
            setRecordings(list);
        } catch (e) {
            setRecError(e instanceof Error ? e.message : String(e));
        } finally {
            setLoadingRecs(false);
        }
    }, [camera.id]);

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
        setRecError(null);
        try {
            const h = authHeaderFor();
            const resp = await fetch(
                `/api/v1/cameras/${camera.id}/recordings/${rec.id}/file`,
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
            setRecError(e instanceof Error ? e.message : String(e));
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
            await deleteRecording(camera.id, rec.id);
            await loadRecordings();
        } catch (e) {
            setRecError(e instanceof Error ? e.message : String(e));
        }
    }

    async function onToggleRecording() {
        if (!isAdmin || toggling || !onRefresh) return;
        setToggling(true);
        setRecError(null);
        try {
            await setRecordingPlan(camera.id, {
                enabled: !recordingEnabled,
                segment_seconds: 600,
                retention_days: 7,
            });
            await onRefresh();
        } catch (e) {
            setRecError(e instanceof Error ? e.message : String(e));
        } finally {
            setToggling(false);
        }
    }

    // targetTime auto-playback — when the URL carries ?time=… the
    // parent routes it in here. We switch to playback mode, load
    // the recordings (if not already loaded), and play the one
    // whose start minute matches the requested minute. Logic
    // preserved verbatim from the previous RecordingPanel.
    useEffect(() => {
        if (targetTime === undefined || targetTime === 0) return;
        setMode("playback");
        if (recordings.length === 0 && !loadingRecs) {
            void loadRecordings();
        }
    }, [targetTime]); // eslint-disable-line react-hooks/exhaustive-deps

    useEffect(() => {
        if (targetTime === undefined || targetTime === 0) return;
        if (recordings.length === 0 || loadingRecs) return;

        const targetMinuteStart = Math.floor(targetTime / 60) * 60;
        const matchingRec = recordings.find((rec) => {
            const recStartTime = new Date(rec.start_at).getTime() / 1000;
            const recStartMinute = Math.floor(recStartTime / 60) * 60;
            return recStartMinute === targetMinuteStart;
        });

        if (matchingRec && playingId !== matchingRec.id && fetchingPlay === null) {
            void onPlay(matchingRec);
        }
    }, [recordings, targetTime, loadingRecs, playingId, fetchingPlay]); // eslint-disable-line react-hooks/exhaustive-deps

    // When entering playback mode, lazy-load the recording list.
    useEffect(() => {
        if (mode === "playback" && recordings.length === 0 && !loadingRecs) {
            void loadRecordings();
        }
    }, [mode]); // eslint-disable-line react-hooks/exhaustive-deps

    // Leaving playback mode revokes the blob URL so the MP4
    // doesn't keep a fetched-but-hidden reference. The next
    // entry into playback will re-fetch on demand.
    useEffect(() => {
        if (mode !== "playback" && videoUrl) {
            onStopPlay();
        }
    }, [mode]); // eslint-disable-line react-hooks/exhaustive-deps

    const statusColor =
        status === "online"
            ? "bg-emerald-500/20 text-emerald-300 ring-emerald-500/30"
            : status === "offline"
                ? "bg-rose-500/20 text-rose-300 ring-rose-500/30"
                : "bg-slate-500/20 text-slate-300 ring-slate-500/30";

    // Compute the "effective" transport for the badge label
    // and the fallback handler. In auto mode the effective
    // path is whatever `path` resolved to (WebRTC succeeded or
    // HLS took over); in explicit modes the effective path is
    // always the requested one.
    const effectivePath: "webrtc" | "hls" =
        transport === "hls"
            ? "hls"
            : transport === "webrtc"
                ? "webrtc"
                : path;

    // In explicit WebRTC mode we suppress the auto-fallback so
    // a failure surfaces as an error overlay the operator can
    // act on, instead of silently switching to HLS.
    const onWebRTCFallback = transport === "auto" ? () => setPath("hls") : undefined;

    return (
        <Card className="glass glass-glow glass-hover-lift rounded-2xl overflow-hidden">
            <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
                <CardTitle className="flex items-center gap-2 text-base font-semibold text-slate-100">
                    {camera.name}
                    {camera.transcode && (
                        <Badge
                            variant="info"
                            className="text-[9px]"
                            title="Server-side H.264 transcoding via ffmpeg (HEVC → H.264). Costs CPU on the home-api host."
                        >
                            x264
                        </Badge>
                    )}
                </CardTitle>
                <div className="flex items-center gap-2">
                    {/* Transport segmented control — live mode only.
                     * Compact three-button toggle, visually adjacent
                     * to the live path badge so the relationship
                     * (selection → resolved path) is obvious. */}
                    {mode === "live" && (
                        <div
                            className="inline-flex h-6 items-center glass-subtle rounded-lg p-0.5 text-[10px]"
                            role="radiogroup"
                            aria-label="Live stream transport"
                        >
                            {(["auto", "webrtc", "hls"] as TransportMode[]).map((t) => {
                                const active = transport === t;
                                return (
                                    <button
                                        key={t}
                                        role="radio"
                                        aria-checked={active}
                                        onClick={() => setTransport(t)}
                                        className={cn(
                                            "h-5 rounded px-1.5 font-medium uppercase tracking-wider transition-all",
                                            active
                                                ? "bg-white/15 text-sky-300 shadow-[0_1px_8px_rgba(56,189,248,0.25)]"
                                                : "text-slate-400 hover:text-slate-200 hover:bg-white/5",
                                        )}
                                        title={
                                            t === "auto"
                                                ? "Try WebRTC; fall back to HLS on failure"
                                                : t === "webrtc"
                                                    ? "Force WebRTC; show error on failure (no auto-fallback)"
                                                    : "Force HLS; never try WebRTC"
                                        }
                                    >
                                        {t}
                                    </button>
                                );
                            })}
                        </div>
                    )}
                    {mode === "live" && (
                        <Badge variant="outline" className="text-[10px]">
                            {effectivePath === "webrtc" ? "WebRTC" : "HLS"}
                        </Badge>
                    )}
                    {/* Mode tabs — only visible once we've left
                     * preview. Switching to live tears down the
                     * recording blob; switching to playback tears
                     * down the WebRTC/HLS pipeline. */}
                    {mode !== "preview" && (
                        <div
                            className="inline-flex h-6 items-center glass-subtle rounded-lg p-0.5 text-[10px]"
                            role="radiogroup"
                            aria-label="View mode"
                        >
                            <button
                                role="radio"
                                aria-checked={mode === "live"}
                                onClick={() => setMode("live")}
                                className={cn(
                                    "h-5 rounded px-1.5 font-medium uppercase tracking-wider transition-all",
                                    mode === "live"
                                        ? "bg-white/15 text-sky-300 shadow-[0_1px_8px_rgba(56,189,248,0.25)]"
                                        : "text-slate-400 hover:text-slate-200 hover:bg-white/5",
                                )}
                                title="Live stream"
                            >
                                Live
                            </button>
                            <button
                                role="radio"
                                aria-checked={mode === "playback"}
                                onClick={() => setMode("playback")}
                                className={cn(
                                    "h-5 rounded px-1.5 font-medium uppercase tracking-wider transition-all",
                                    mode === "playback"
                                        ? "bg-white/15 text-sky-300 shadow-[0_1px_8px_rgba(56,189,248,0.25)]"
                                        : "text-slate-400 hover:text-slate-200 hover:bg-white/5",
                                )}
                                title="Recording playback"
                            >
                                Playback
                            </button>
                        </div>
                    )}
                    {/* Stop button — return to preview mode. Tears
                     * down whichever engine is active. */}
                    {mode !== "preview" && (
                        <Button
                            size="sm"
                            variant="outline"
                            className="h-6 px-2 text-[10px] glass-subtle hover:bg-white/10"
                            onClick={() => setMode("preview")}
                            title="Return to preview"
                        >
                            <Square size={10} className="mr-1" />
                            Stop
                        </Button>
                    )}
                    {/* Recording toggle — admin only. Always
                     * visible so the operator can start/stop
                     * recording without entering playback mode. */}
                    {isAdmin && (
                        <Button
                            size="sm"
                            variant={recordingEnabled ? "danger" : "primary"}
                            onClick={onToggleRecording}
                            disabled={toggling || !onRefresh}
                            className="h-6 px-2 text-[10px]"
                            title={
                                recordingEnabled
                                    ? `Recording ON · ${camera.meta.recording?.segment_seconds ?? 600}s · ${camera.meta.recording?.retention_days ?? 7}d retention`
                                    : "Recording OFF"
                            }
                        >
                            {toggling && <Loader2 size={10} className="animate-spin" />}
                            <span
                                className={cn(
                                    "mr-1 inline-block h-1.5 w-1.5 rounded-full",
                                    recordingEnabled ? "bg-rose-200" : "bg-slate-200",
                                )}
                            />
                            Rec
                        </Button>
                    )}
                    <Badge variant="outline" className={cn("ring-1 ring-inset", statusColor)}>
                        <span className="mr-1 inline-block h-1.5 w-1.5 rounded-full bg-current" />
                        {status}
                    </Badge>
                    <Badge variant="outline" className="text-[10px]">
                        {camera.vendor || "onvif"}
                    </Badge>
                </div>
            </CardHeader>
            <CardContent className="space-y-3 p-0">
                <div className="relative aspect-video w-full bg-black">
                    {mode === "preview" && (
                        <PreviewFrame
                            cameraId={camera.id}
                            onPlay={() => setMode("live")}
                        />
                    )}
                    {mode === "live" && (
                        effectivePath === "webrtc" ? (
                            <WebRTCVideo
                                key={`webrtc-${generation}`}
                                cameraId={camera.id}
                                streamName={camera.stream.stream_name}
                                webrtcUrl={camera.stream.webrtc_url}
                                onFallback={onWebRTCFallback}
                            />
                        ) : (
                            <HLSVideo
                                key={`hls-${generation}`}
                                src={camera.stream.hls_url}
                                onRetry={retry}
                            />
                        )
                    )}
                    {mode === "playback" && (
                        videoUrl && playingId !== null ? (
                            <video
                                key={videoUrl}
                                src={videoUrl}
                                controls
                                autoPlay
                                className="h-full w-full object-contain"
                            />
                        ) : (
                            <div className="absolute inset-0 flex flex-col items-center justify-center text-slate-400">
                                {loadingRecs ? (
                                    <>
                                        <Loader2 className="mb-2 h-5 w-5 animate-spin" />
                                        <span className="text-xs">loading recordings…</span>
                                    </>
                                ) : recordings.length === 0 ? (
                                    <>
                                        <AlertTriangle className="mb-2 h-5 w-5 text-slate-500" />
                                        <span className="text-xs">no recordings</span>
                                    </>
                                ) : (
                                    <span className="text-xs">select a recording below</span>
                                )}
                            </div>
                        )
                    )}
                    {eventToast && (
                        <div className="absolute right-2 top-2 rounded-lg backdrop-blur-xl bg-black/50 px-3 py-1.5 text-xs font-semibold text-white shadow-lg border border-white/10">
                            {eventToast}
                        </div>
                    )}
                </div>

                {/* Recording list — playback mode only. Mirrors the
                 * previous RecordingPanel layout: each row shows
                 * start time, duration, size, play/stop + delete
                 * buttons. Inline video player is rendered in the
                 * aspect-video area above when a recording is
                 * playing. */}
                {mode === "playback" && (
                    <div className="space-y-2 px-4 pb-3 pt-1">
                        {recError && (
                            <div className="glass bg-[rgb(var(--accent-danger)/0.1)] text-[rgb(var(--accent-danger))] rounded-lg px-2.5 py-1.5 text-[11px]">
                                {recError}
                            </div>
                        )}

                        <div className="space-y-1">
                            <div className="flex items-center justify-between text-[10px] uppercase tracking-wider text-fg-subtle">
                                <span>最近录制</span>
                                <button
                                    type="button"
                                    onClick={loadRecordings}
                                    disabled={loadingRecs}
                                    className="inline-flex items-center gap-1 text-fg-muted transition-colors hover:text-fg disabled:opacity-50"
                                >
                                    <RefreshCcw size={10} className={loadingRecs ? "animate-spin" : ""} />
                                    刷新
                                </button>
                            </div>

                            {loadingRecs && recordings.length === 0 ? (
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
                                            className="flex items-center gap-2 glass-subtle rounded-lg px-2 py-1.5 text-[11px] hover:bg-[rgb(var(--bg-subtle)/0.3)] transition-colors"
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
                                                    className="inline-flex h-6 items-center gap-1 rounded-md glass-subtle px-2 text-[10px] text-fg-muted transition-colors hover:text-fg"
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
                                                    className="inline-flex h-6 items-center gap-1 rounded-md bg-[rgb(var(--accent-primary)/0.15)] px-2 text-[10px] text-[rgb(var(--accent-primary))] transition-colors hover:bg-[rgb(var(--accent-primary)/0.25)] disabled:opacity-50"
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
                                                    className="inline-flex h-6 items-center justify-center rounded-md p-1 text-fg-subtle transition-colors hover:bg-[rgb(var(--accent-danger)/0.1)] hover:text-[rgb(var(--accent-danger))]"
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
                    </div>
                )}

                {/* PTZ pad + presets — live mode only. Hidden in
                 * preview/playback because PTZ commands only make
                 * sense while the live stream is rendering. */}
                {mode === "live" && (
                    <div className="grid grid-cols-[auto_1fr] gap-3 p-4">
                        {/* PTZ pad */}
                        <div className="grid grid-cols-3 gap-1">
                            <span />
                            <Button
                                size="icon"
                                variant="outline"
                                disabled={!isAdmin || busy || !camera.capabilities.ptz}
                                onClick={() => sendPTZ("up")}
                                aria-label="PTZ up"
                                className="glass-subtle hover:bg-white/10"
                            >
                                <ChevronUp size={16} />
                            </Button>
                            <span />
                            <Button
                                size="icon"
                                variant="outline"
                                disabled={!isAdmin || busy || !camera.capabilities.ptz}
                                onClick={() => sendPTZ("left")}
                                aria-label="PTZ left"
                                className="glass-subtle hover:bg-white/10"
                            >
                                <ChevronLeft size={16} />
                            </Button>
                            <Button
                                size="icon"
                                variant="outline"
                                disabled={!isAdmin || busy}
                                onClick={() => sendPTZ("stop")}
                                aria-label="PTZ stop"
                                className="glass-subtle hover:bg-white/10"
                            >
                                <Square size={14} />
                            </Button>
                            <Button
                                size="icon"
                                variant="outline"
                                disabled={!isAdmin || busy || !camera.capabilities.ptz}
                                onClick={() => sendPTZ("right")}
                                aria-label="PTZ right"
                                className="glass-subtle hover:bg-white/10"
                            >
                                <ChevronRight size={16} />
                            </Button>
                            <span />
                            <Button
                                size="icon"
                                variant="outline"
                                disabled={!isAdmin || busy || !camera.capabilities.ptz}
                                onClick={() => sendPTZ("down")}
                                aria-label="PTZ down"
                                className="glass-subtle hover:bg-white/10"
                            >
                                <ChevronDown size={16} />
                            </Button>
                            <span />
                        </div>

                        {/* Zoom + presets + last seen */}
                        <div className="flex flex-col gap-2">
                            <div className="flex flex-wrap items-center gap-1">
                                <Button
                                    size="sm"
                                    variant="outline"
                                    disabled={!isAdmin || busy || !camera.capabilities.ptz}
                                    onClick={() => sendPTZ("zoom_in")}
                                    className="glass-subtle hover:bg-white/10"
                                >
                                    <ZoomIn size={14} className="mr-1" />
                                    Zoom+
                                </Button>
                                <Button
                                    size="sm"
                                    variant="outline"
                                    disabled={!isAdmin || busy || !camera.capabilities.ptz}
                                    onClick={() => sendPTZ("zoom_out")}
                                    className="glass-subtle hover:bg-white/10"
                                >
                                    <ZoomOut size={14} className="mr-1" />
                                    Zoom-
                                </Button>
                                {!isAdmin && (
                                    <Badge variant="outline" className="text-[10px]">
                                        <Power size={10} className="mr-1" />
                                        view-only
                                    </Badge>
                                )}
                            </div>
                            {Object.keys(camera.presets ?? {}).length > 0 && (
                                <div className="flex flex-wrap items-center gap-1">
                                    {Object.entries(camera.presets).map(([alias]) => (
                                        <Button
                                            key={alias}
                                            size="sm"
                                            variant="secondary"
                                            disabled={!isAdmin || busy}
                                            onClick={() => sendPreset(alias)}
                                            className="glass-subtle hover:bg-white/10"
                                        >
                                            {alias}
                                        </Button>
                                    ))}
                                </div>
                            )}
                            {ptzError && (
                                <p className="text-xs text-[rgb(var(--accent-danger))]">{ptzError}</p>
                            )}
                            {lastSeen && (
                                <p className="text-[10px] text-slate-500">
                                    last seen {new Date(lastSeen).toLocaleString()}
                                </p>
                            )}
                        </div>
                    </div>
                )}
            </CardContent>
        </Card>
    );
}

/**
 * PreviewFrame — default preview surface. Shows the static JPEG
 * snapshot from GET /cameras/:id/frame with a Play button overlay;
 * clicking Play hands control back to the parent, which flips to
 * live mode and mounts the WebRTC/HLS engine. On image error (404,
 * 401, codec issue, camera offline) we show a small placeholder
 * so the card isn't a black rectangle with no signal.
 */
function PreviewFrame({
    cameraId,
    onPlay,
}: {
    cameraId: number;
    onPlay: () => void;
}) {
    const [error, setError] = useState(false);
    return (
        <div className="absolute inset-0 flex items-center justify-center bg-black">
            {error ? (
                <div className="flex flex-col items-center text-slate-500">
                    <AlertTriangle className="mb-2 h-6 w-6 text-rose-400" />
                    <span className="text-xs">preview unavailable</span>
                </div>
            ) : (
                <>
                    <img
                        src={cameraFrameUrl(cameraId)}
                        alt="camera preview"
                        onError={() => setError(true)}
                        className="h-full w-full object-contain"
                    />
                    <button
                        type="button"
                        onClick={onPlay}
                        className="absolute inset-0 flex items-center justify-center bg-black/30 hover:bg-black/40 transition-colors group"
                        aria-label="Play live stream"
                        title="Play live stream"
                    >
                        <span className="flex h-14 w-14 items-center justify-center rounded-full bg-white/20 backdrop-blur-md ring-2 ring-white/40 group-hover:scale-110 transition-transform">
                            <Play className="ml-1 h-6 w-6 text-white" fill="currentColor" />
                        </span>
                    </button>
                </>
            )}
        </div>
    );
}

/**
 * WebRTCVideo — wraps useWebRTCStream in a sub-component so its
 * lifecycle is fully decoupled from HLSVideo's. When the parent
 * switches to HLS, React unmounts this component, the
 * RTCPeerConnection closes, and the next mount is a clean slate.
 */
function WebRTCVideo({
    cameraId,
    streamName,
    webrtcUrl,
    onFallback,
}: {
    cameraId: number;
    streamName: string;
    webrtcUrl: string;
    onFallback?: () => void;
}) {
    const { videoRef, state, error } = useWebRTCStream({
        cameraId,
        streamName,
        webrtcUrl,
    });
    useEffect(() => {
        if (state === "error") onFallback?.();
    }, [state, onFallback]);
    return (
        <VideoSurface videoRef={videoRef} state={state} error={error} label="WebRTC" />
    );
}

/**
 * HLSVideo — same pattern as WebRTCVideo, but for the hls.js
 * fallback. Retry is exposed because the parent owns the
 * generation counter (it decides when to remount).
 */
function HLSVideo({
    src,
    onRetry,
}: {
    src: string;
    onRetry: () => void;
}) {
    const { videoRef, state, error, retry } = useHLSStream({ src });
    return (
        <VideoSurface
            videoRef={videoRef}
            state={state === "idle" ? "loading" : state}
            error={error}
            label="HLS"
            onRetry={state === "error" ? () => { retry(); onRetry(); } : undefined}
        />
    );
}

/**
 * VideoSurface — the shared <video> + state overlay layout. Both
 * sub-components render this so the surrounding chrome is
 * identical regardless of which transport is active.
 */
function VideoSurface({
    videoRef,
    state,
    error,
    label: _label,
    onRetry,
}: {
    videoRef: React.RefObject<HTMLVideoElement>;
    state: "idle" | "loading" | "fetching-ice" | "connecting" | "playing" | "fallback" | "error";
    error: string | null;
    label: string;
    onRetry?: () => void;
}) {
    // Map underlying hook states to the three states VideoSurface
    // knows about (loading / playing / error). "idle",
    // "fetching-ice", "connecting" all mean "we're not playing
    // yet and haven't errored".
    const viewState: "loading" | "playing" | "error" =
        state === "playing" || state === "fallback"
            ? "playing"
            : state === "error"
                ? "error"
                : "loading";
    return (
        <>
            <video
                ref={videoRef}
                autoPlay
                playsInline
                muted
                controls
                className="h-full w-full object-contain"
            />
            {viewState === "loading" && (
                <Overlay>
                    <Loader2 className="mr-2 h-5 w-5 animate-spin" />
                    loading live stream
                </Overlay>
            )}
            {viewState === "error" && (
                <Overlay>
                    <AlertTriangle className="mb-2 h-6 w-6 text-rose-400" />
                    <p className="px-4 text-center text-sm text-slate-200">
                        {error ?? "playback failed"}
                    </p>
                    {onRetry && (
                        <Button
                            size="sm"
                            variant="outline"
                            className="mt-2 glass-subtle hover:bg-white/10"
                            onClick={onRetry}
                        >
                            <RefreshCw className="mr-1 h-3 w-3" />
                            Retry
                        </Button>
                    )}
                </Overlay>
            )}
        </>
    );
}

function Overlay({ children }: { children: React.ReactNode }) {
    return (
        <div className="absolute inset-0 flex flex-col items-center justify-center backdrop-blur-xl bg-black/50 text-slate-300">
            {children}
        </div>
    );
}

export default LiveVideo;
