import { useEffect, useRef, useState } from "react";
import { ChevronUp, ChevronDown, ChevronLeft, ChevronRight, ZoomIn, ZoomOut, Square, AlertTriangle, Loader2, RefreshCw, Power, Play, MoreVertical } from "lucide-react";
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
    setRecordingPlan,
} from "@/api/camera";
import { RecordingTimeline } from "@/components/RecordingTimeline";
import type { Camera, CameraEventMessage, CameraStatusEvent, WsMessage } from "@/types";

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

/**
 * LiveVideo — one camera with three modes:
 *
 *   preview  — static JPEG snapshot + Play button (default). Avoids
 *              spinning up a WebRTC/HLS connection for every camera
 *              on the page until the operator actually wants video.
 *   live     — WebRTC/HLS live stream with PTZ + transport picker.
 *   playback — 24h recording timeline; the <video> is rendered into
 *              this card's main video area via React Portal, sharing
 *              the same physical surface as the live stream.
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
 * Header layout (v1.8.0):
 *   The header used to cram 7+ controls (transport segmented control,
 *   transport badge, mode tabs, Stop, Rec, status, vendor) into one
 *   row, which overflowed on narrow viewports. The restructure moves
 *   infrequently-used controls (transport selector, recording toggle)
 *   into a kebab (⋮) dropdown, keeping the visible header to:
 *     [title + x264] [status badge] [mode tabs] [stop] [⋮]
 *
 * Player merge (v1.8.0):
 *   Recording playback previously rendered its own <video> surface
 *   in a separate aspect-video container below the main video area,
 *   leaving the main area showing a "切换至下方时间轴" placeholder.
 *   RecordingTimeline now portals its <video> + custom controls
 *   into this card's main video area, so live and playback share
 *   the same physical surface.
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

    // Kebab menu (⋮) open state for the header overflow menu.
    const [menuOpen, setMenuOpen] = useState(false);
    const menuRef = useRef<HTMLDivElement>(null);

    // Click-outside handler for the kebab menu.
    useEffect(() => {
        if (!menuOpen) return;
        function onDocClick(e: MouseEvent) {
            if (menuRef.current && !menuRef.current.contains(e.target as Node)) {
                setMenuOpen(false);
            }
        }
        document.addEventListener("mousedown", onDocClick);
        return () => document.removeEventListener("mousedown", onDocClick);
    }, [menuOpen]);

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

    // Video area element — RecordingTimeline portals its <video>
    // here when in playback mode, so live and playback share the
    // same physical surface. Using a state-backed ref so the
    // RecordingTimeline render is triggered once the element mounts.
    const [videoAreaEl, setVideoAreaEl] = useState<HTMLDivElement | null>(null);

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
    // Recording playback is now fully owned by the RecordingTimeline
    // component (day picker, 24h seekbar, event ribbon, motion
    // overlay, custom controls). LiveVideo only tracks the recording
    // toggle (admin) and whether we're in playback mode.
    const [toggling, setToggling] = useState(false);

    const recordingEnabled = camera.meta.recording?.enabled ?? false;

    async function onToggleRecording() {
        if (!isAdmin || toggling || !onRefresh) return;
        setToggling(true);
        try {
            await setRecordingPlan(camera.id, {
                enabled: !recordingEnabled,
                segment_seconds: 600,
                retention_days: 7,
            });
            await onRefresh();
        } finally {
            setToggling(false);
        }
    }

    // targetTime auto-playback — when the URL carries ?time=… the
    // parent routes it in here. We switch to playback mode; the
    // RecordingTimeline component handles the actual seek.
    useEffect(() => {
        if (targetTime === undefined || targetTime === 0) return;
        setMode("playback");
    }, [targetTime]); // eslint-disable-line react-hooks/exhaustive-deps

    const statusColor =
        status === "online"
            ? "bg-[rgb(var(--accent-success)/0.2)] text-[rgb(var(--accent-success))] ring-[rgb(var(--accent-success)/0.3)]"
            : status === "offline"
                ? "bg-[rgb(var(--accent-danger)/0.2)] text-[rgb(var(--accent-danger))] ring-[rgb(var(--accent-danger)/0.3)]"
                : "bg-[rgb(var(--fg-subtle)/0.2)] text-fg-muted ring-[rgb(var(--border)/0.3)]";

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
        <Card className="glass glass-glow glass-hover-lift rounded-2xl bg-[rgb(var(--glass-bg)/0.92)]">
            <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
                <CardTitle className="flex items-center gap-2 text-base font-semibold text-fg">
                    <span
                        className={cn(
                            "inline-block h-1.5 w-1.5 rounded-full",
                            status === "online"
                                ? "bg-[rgb(var(--accent-success))]"
                                : status === "offline"
                                    ? "bg-[rgb(var(--accent-danger))]"
                                    : "bg-[rgb(var(--fg-subtle))]",
                        )}
                        title={`状态：${status}`}
                    />
                    {camera.name}
                    {camera.transcode && (
                        <Badge
                            variant="info"
                            className="text-[9px]"
                            title="服务端 ffmpeg 转码（HEVC → H.264），会占用 home-api 主机 CPU"
                        >
                            x264
                        </Badge>
                    )}
                </CardTitle>
                <div className="flex items-center gap-1.5">
                    {/* Status badge — always visible, compact. */}
                    <Badge variant="outline" className={cn("ring-1 ring-inset text-[10px]", statusColor)}>
                        {status === "online" ? "在线" : status === "offline" ? "离线" : "未知"}
                    </Badge>
                    {/* Mode tabs — only visible once we've left
                     * preview. Switching to live tears down the
                     * recording blob; switching to playback tears
                     * down the WebRTC/HLS pipeline. */}
                    {mode !== "preview" && (
                        <div
                            className="inline-flex h-7 items-center glass-subtle rounded-lg p-0.5 text-[11px]"
                            role="radiogroup"
                            aria-label="查看模式"
                        >
                            <button
                                role="radio"
                                aria-checked={mode === "live"}
                                onClick={() => setMode("live")}
                                className={cn(
                                    "inline-flex h-6 items-center justify-center rounded px-2 font-medium transition-all",
                                    mode === "live"
                                        ? "bg-[rgb(var(--accent-info)/0.25)] text-[rgb(var(--accent-info))] shadow-[0_1px_8px_rgb(var(--accent-info)/0.25)]"
                                        : "text-fg-muted hover:text-fg hover:bg-[rgb(var(--bg-subtle)/0.5)]",
                                )}
                                title="直播"
                            >
                                直播
                            </button>
                            <button
                                role="radio"
                                aria-checked={mode === "playback"}
                                onClick={() => setMode("playback")}
                                className={cn(
                                    "inline-flex h-6 items-center justify-center rounded px-2 font-medium transition-all",
                                    mode === "playback"
                                        ? "bg-[rgb(var(--accent-info)/0.25)] text-[rgb(var(--accent-info))] shadow-[0_1px_8px_rgb(var(--accent-info)/0.25)]"
                                        : "text-fg-muted hover:text-fg hover:bg-[rgb(var(--bg-subtle)/0.5)]",
                                )}
                                title="录像回放"
                            >
                                回放
                            </button>
                        </div>
                    )}
                    {/* Stop button — return to preview mode. Tears
                     * down whichever engine is active. */}
                    {mode !== "preview" && (
                        <Button
                            size="sm"
                            variant="outline"
                            className="h-7 px-2.5 text-[11px] hover:bg-[rgb(var(--bg-subtle)/0.6)]"
                            onClick={() => setMode("preview")}
                            title="返回预览"
                        >
                            <Square size={11} className="mr-1" />
                            停止
                        </Button>
                    )}
                    {/* Kebab menu (⋮) — overflow controls.
                     * Holds the transport selector (live mode only)
                     * and the recording toggle (admin only).
                     * Visible at all times so admin operators can
                     * toggle recording without entering playback. */}
                    <div className="relative" ref={menuRef}>
                        <Button
                            size="icon"
                            variant="ghost"
                            className="h-8 w-8 text-fg-muted hover:text-fg hover:bg-[rgb(var(--bg-subtle)/0.5)]"
                            onClick={() => setMenuOpen((o) => !o)}
                            title="更多操作"
                            aria-label="更多操作"
                            aria-expanded={menuOpen}
                        >
                            <MoreVertical size={18} className="text-fg" />
                        </Button>
                        {menuOpen && (
                            <div className="absolute right-0 top-full mt-1 z-50 min-w-[240px] rounded-xl glass-strong p-3 shadow-xl ring-1 ring-[rgb(var(--border)/0.4)] space-y-2.5">
                                {/* Vendor + last seen info */}
                                <div className="space-y-1 text-xs text-fg-muted">
                                    <div>厂商：<span className="text-fg font-medium">{camera.vendor || "onvif"}</span></div>
                                    {lastSeen && (
                                        <div>最后在线：<span className="text-fg font-medium">{new Date(lastSeen).toLocaleString()}</span></div>
                                    )}
                                </div>
                                {/* Transport selector — live mode only */}
                                {mode === "live" && (
                                    <div className="space-y-1.5 pt-2 border-t border-[rgb(var(--border)/0.2)]">
                                        <div className="text-xs font-medium text-fg">传输方式</div>
                                        <div
                                            className="inline-flex h-8 w-full items-center glass-subtle rounded-lg p-0.5 text-xs"
                                            role="radiogroup"
                                            aria-label="直播传输方式"
                                        >
                                            {(["auto", "webrtc", "hls"] as TransportMode[]).map((t) => {
                                                const active = transport === t;
                                                const label = t === "auto" ? "自动" : t === "webrtc" ? "WebRTC" : "HLS";
                                                return (
                                                    <button
                                                        key={t}
                                                        role="radio"
                                                        aria-checked={active}
                                                        onClick={() => setTransport(t)}
                                                        className={cn(
                                                            "inline-flex h-7 flex-1 items-center justify-center rounded px-2 font-medium transition-all",
                                                            active
                                                                ? "bg-[rgb(var(--accent-info)/0.25)] text-[rgb(var(--accent-info))]"
                                                                : "text-fg-muted hover:text-fg hover:bg-[rgb(var(--bg-subtle)/0.5)]",
                                                        )}
                                                        title={
                                                            t === "auto"
                                                                ? "优先 WebRTC，失败自动回退到 HLS"
                                                                : t === "webrtc"
                                                                    ? "强制 WebRTC，失败显示错误（不自动回退）"
                                                                    : "强制 HLS，不尝试 WebRTC"
                                                        }
                                                    >
                                                        {label}
                                                    </button>
                                                );
                                            })}
                                        </div>
                                        <div className="text-[11px] text-fg-muted">
                                            当前：<span className="text-fg font-medium">{effectivePath === "webrtc" ? "WebRTC" : "HLS"}</span>
                                        </div>
                                    </div>
                                )}
                                {/* Recording toggle — admin only */}
                                {isAdmin && (
                                    <div className="space-y-1.5 pt-2 border-t border-[rgb(var(--border)/0.2)]">
                                        <div className="flex items-center justify-between">
                                            <div className="text-xs font-medium text-fg">录像计划</div>
                                            <span
                                                className={cn(
                                                    "inline-flex items-center gap-1 text-xs font-medium",
                                                    recordingEnabled
                                                        ? "text-[rgb(var(--accent-danger))]"
                                                        : "text-fg-muted",
                                                )}
                                            >
                                                <span
                                                    className={cn(
                                                        "inline-block h-2 w-2 rounded-full",
                                                        recordingEnabled
                                                            ? "bg-[rgb(var(--accent-danger))] animate-pulse"
                                                            : "bg-[rgb(var(--fg-subtle))]",
                                                    )}
                                                />
                                                {recordingEnabled ? "开启" : "关闭"}
                                            </span>
                                        </div>
                                        <Button
                                            size="sm"
                                            variant={recordingEnabled ? "danger" : "primary"}
                                            onClick={onToggleRecording}
                                            disabled={toggling || !onRefresh}
                                            className="w-full h-8 text-xs"
                                            title={
                                                recordingEnabled
                                                    ? `录像中 · ${camera.meta.recording?.segment_seconds ?? 600}秒/段 · 保留 ${camera.meta.recording?.retention_days ?? 7} 天`
                                                    : "录像未开启"
                                            }
                                        >
                                            {toggling && <Loader2 size={12} className="animate-spin mr-1" />}
                                            {recordingEnabled ? "停止录像" : "开始录像"}
                                        </Button>
                                    </div>
                                )}
                            </div>
                        )}
                    </div>
                </div>
            </CardHeader>
            <CardContent className="space-y-3 p-0">
                <div ref={setVideoAreaEl} className="relative aspect-video w-full bg-black overflow-hidden">
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
                    {/* Playback mode: RecordingTimeline portals its
                     * <video> + custom controls into this same video
                     * area (see videoAreaEl above). No placeholder
                     * needed — RecordingTimeline handles the empty
                     * state ("点击下方时间轴开始播放") inside the
                     * portaled surface. */}
                    {eventToast && (
                        <div className="absolute right-2 top-2 rounded-lg backdrop-blur-xl bg-black/50 px-3 py-1.5 text-xs font-semibold text-white shadow-lg border border-white/10 pointer-events-none">
                            {eventToast}
                        </div>
                    )}
                </div>

                {/* RecordingTimeline — playback mode only. Renders
                 * its <video> surface into the main video area above
                 * (via portal) and its day picker + 24h seekbar +
                 * event ribbon below. The targetTime prop is
                 * forwarded so alert clicks (?time=…&mode=recording)
                 * auto-seek. */}
                {mode === "playback" && videoAreaEl && (
                    <RecordingTimeline
                        cameraId={camera.id}
                        targetTime={targetTime}
                        videoPortalTarget={videoAreaEl}
                    />
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
                                aria-label="上转"
                                className="hover:bg-[rgb(var(--bg-subtle)/0.6)]"
                            >
                                <ChevronUp size={16} />
                            </Button>
                            <span />
                            <Button
                                size="icon"
                                variant="outline"
                                disabled={!isAdmin || busy || !camera.capabilities.ptz}
                                onClick={() => sendPTZ("left")}
                                aria-label="左转"
                                className="hover:bg-[rgb(var(--bg-subtle)/0.6)]"
                            >
                                <ChevronLeft size={16} />
                            </Button>
                            <Button
                                size="icon"
                                variant="outline"
                                disabled={!isAdmin || busy}
                                onClick={() => sendPTZ("stop")}
                                aria-label="停止转动"
                                className="hover:bg-[rgb(var(--bg-subtle)/0.6)]"
                            >
                                <Square size={14} />
                            </Button>
                            <Button
                                size="icon"
                                variant="outline"
                                disabled={!isAdmin || busy || !camera.capabilities.ptz}
                                onClick={() => sendPTZ("right")}
                                aria-label="右转"
                                className="hover:bg-[rgb(var(--bg-subtle)/0.6)]"
                            >
                                <ChevronRight size={16} />
                            </Button>
                            <span />
                            <Button
                                size="icon"
                                variant="outline"
                                disabled={!isAdmin || busy || !camera.capabilities.ptz}
                                onClick={() => sendPTZ("down")}
                                aria-label="下转"
                                className="hover:bg-[rgb(var(--bg-subtle)/0.6)]"
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
                                    className="hover:bg-[rgb(var(--bg-subtle)/0.6)]"
                                >
                                    <ZoomIn size={14} className="mr-1" />
                                    拉近
                                </Button>
                                <Button
                                    size="sm"
                                    variant="outline"
                                    disabled={!isAdmin || busy || !camera.capabilities.ptz}
                                    onClick={() => sendPTZ("zoom_out")}
                                    className="hover:bg-[rgb(var(--bg-subtle)/0.6)]"
                                >
                                    <ZoomOut size={14} className="mr-1" />
                                    拉远
                                </Button>
                                {!isAdmin && (
                                    <Badge variant="outline" className="text-[10px]">
                                        <Power size={10} className="mr-1" />
                                        仅观看
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
                                <p className="text-[10px] text-fg-subtle">
                                    最后在线 {new Date(lastSeen).toLocaleString()}
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
                <div className="flex flex-col items-center text-fg-muted">
                    <AlertTriangle className="mb-2 h-6 w-6 text-[rgb(var(--accent-danger))]" />
                    <span className="text-xs">预览不可用</span>
                </div>
            ) : (
                <>
                    <img
                        src={cameraFrameUrl(cameraId)}
                        alt="摄像头预览"
                        onError={() => setError(true)}
                        className="h-full w-full object-contain"
                    />
                    <button
                        type="button"
                        onClick={onPlay}
                        className="absolute inset-0 flex items-center justify-center bg-black/30 hover:bg-black/40 transition-colors group"
                        aria-label="开始直播"
                        title="开始直播"
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
                    正在加载直播
                </Overlay>
            )}
            {viewState === "error" && (
                <Overlay>
                    <AlertTriangle className="mb-2 h-6 w-6 text-[rgb(var(--accent-danger))]" />
                    <p className="px-4 text-center text-sm text-fg">
                        {error ?? "播放失败"}
                    </p>
                    {onRetry && (
                        <Button
                            size="sm"
                            variant="outline"
                            className="mt-2 hover:bg-[rgb(var(--bg-subtle)/0.6)]"
                            onClick={onRetry}
                        >
                            <RefreshCw className="mr-1 h-3 w-3" />
                            重试
                        </Button>
                    )}
                </Overlay>
            )}
        </>
    );
}

function Overlay({ children }: { children: React.ReactNode }) {
    return (
        <div className="absolute inset-0 flex flex-col items-center justify-center backdrop-blur-xl bg-black/60 text-fg">
            {children}
        </div>
    );
}

export default LiveVideo;
