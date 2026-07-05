import { useEffect, useState } from "react";
import { ChevronUp, ChevronDown, ChevronLeft, ChevronRight, ZoomIn, ZoomOut, Square, AlertTriangle, Loader2, RefreshCw, Power } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { useWebRTCStream } from "@/hooks/useWebRTCStream";
import { useHLSStream } from "@/hooks/useHLSStream";
import { ptzMove, gotoPreset } from "@/api/camera";
import type { Camera, CameraEventMessage, CameraStatusEvent, WsMessage } from "@/types";

interface LiveVideoProps {
    camera: Camera;
    isAdmin: boolean;
    /** Subscribe to a single topic and dispatch parsed events. */
    onWsMessage?: (handler: (m: WsMessage) => void) => () => void;
}

/**
 * LiveVideo — one camera: video pane + PTZ pad + preset bar.
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
 * Online status is updated either from the camera row's `status`
 * field (initial render) or from the WebSocket "device.<id>.status"
 * event the parent routes in via onWsMessage.
 *
 * Recording playback (NOT in this component) uses useHLSStream
 * directly with the recording's per-segment m3u8 URL.
 */
export function LiveVideo({ camera, isAdmin, onWsMessage }: LiveVideoProps) {
    // Path drives which sub-component is mounted. We start on
    // WebRTC and flip to HLS on the primary's onError.
    const [path, setPath] = useState<"webrtc" | "hls">("webrtc");
    // Generation counter increments on every retry; changing it
    // forces a remount of the active sub-component.
    const [generation, setGeneration] = useState(0);

    function retry() {
        setGeneration((g) => g + 1);
    }

    // Reset to WebRTC on camera change (e.g. operator switched
    // tiles). The next render of LiveVideo remounts both hooks.
    useEffect(() => {
        setPath("webrtc");
        setGeneration(0);
    }, [camera.id]);

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

    const statusColor =
        status === "online"
            ? "bg-emerald-500/20 text-emerald-300 ring-emerald-500/30"
            : status === "offline"
                ? "bg-rose-500/20 text-rose-300 ring-rose-500/30"
                : "bg-slate-500/20 text-slate-300 ring-slate-500/30";

    return (
        <Card className="overflow-hidden">
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
                    <Badge variant="outline" className="text-[10px]">
                        {path === "webrtc" ? "WebRTC" : "HLS"}
                    </Badge>
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
                    {path === "webrtc" ? (
                        <WebRTCVideo
                            key={`webrtc-${generation}`}
                            streamName={camera.stream.stream_name}
                            webrtcUrl={camera.stream.webrtc_url}
                            onFallback={() => setPath("hls")}
                        />
                    ) : (
                        <HLSVideo
                            key={`hls-${generation}`}
                            src={camera.stream.hls_url}
                            onRetry={retry}
                        />
                    )}
                    {eventToast && (
                        <div className="absolute right-2 top-2 rounded-md bg-rose-500/80 px-2 py-1 text-xs font-semibold text-white shadow-lg">
                            {eventToast}
                        </div>
                    )}
                </div>

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
                        >
                            <ChevronLeft size={16} />
                        </Button>
                        <Button
                            size="icon"
                            variant="outline"
                            disabled={!isAdmin || busy}
                            onClick={() => sendPTZ("stop")}
                            aria-label="PTZ stop"
                        >
                            <Square size={14} />
                        </Button>
                        <Button
                            size="icon"
                            variant="outline"
                            disabled={!isAdmin || busy || !camera.capabilities.ptz}
                            onClick={() => sendPTZ("right")}
                            aria-label="PTZ right"
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
                            >
                                <ZoomIn size={14} className="mr-1" />
                                Zoom+
                            </Button>
                            <Button
                                size="sm"
                                variant="outline"
                                disabled={!isAdmin || busy || !camera.capabilities.ptz}
                                onClick={() => sendPTZ("zoom_out")}
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
                                    >
                                        {alias}
                                    </Button>
                                ))}
                            </div>
                        )}
                        {ptzError && (
                            <p className="text-xs text-rose-300">{ptzError}</p>
                        )}
                        {lastSeen && (
                            <p className="text-[10px] text-slate-500">
                                last seen {new Date(lastSeen).toLocaleString()}
                            </p>
                        )}
                    </div>
                </div>
            </CardContent>
        </Card>
    );
}

/**
 * WebRTCVideo — wraps useWebRTCStream in a sub-component so its
 * lifecycle is fully decoupled from HLSVideo's. When the parent
 * switches to HLS, React unmounts this component, the
 * RTCPeerConnection closes, and the next mount is a clean slate.
 */
function WebRTCVideo({
    streamName,
    webrtcUrl,
    onFallback,
}: {
    streamName: string;
    webrtcUrl: string;
    onFallback: () => void;
}) {
    const { videoRef, state, error } = useWebRTCStream({
        streamName,
        webrtcUrl,
    });
    useEffect(() => {
        if (state === "error") onFallback();
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
                            className="mt-2"
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
        <div className="absolute inset-0 flex flex-col items-center justify-center bg-black/60 text-slate-300">
            {children}
        </div>
    );
}

export default LiveVideo;
