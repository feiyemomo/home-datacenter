import { useEffect, useRef, useState } from "react";
import { ChevronUp, ChevronDown, ChevronLeft, ChevronRight, ZoomIn, ZoomOut, Square, AlertTriangle, Loader2, RefreshCw, Power } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { useWebRTCStream } from "@/hooks/useWebRTCStream";
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
 * Online status is updated either from the camera row's `status`
 * field (initial render) or from the WebSocket
 * "device.<id>.status" event the parent routes in via onWsMessage.
 */
export function LiveVideo({ camera, isAdmin, onWsMessage }: LiveVideoProps) {
    const videoRef = useRef<HTMLVideoElement>(null);
    const { state, error, retry } = useWebRTCStream({
        streamName: camera.stream.stream_name,
        webrtcUrl: camera.stream.webrtc_url,
    });

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
                <CardTitle className="text-base font-semibold text-slate-100">
                    {camera.name}
                </CardTitle>
                <div className="flex items-center gap-2">
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
                    <video
                        ref={videoRef}
                        autoPlay
                        playsInline
                        muted
                        controls={false}
                        className="h-full w-full object-contain"
                    />
                    {eventToast && (
                        <div className="absolute right-2 top-2 rounded-md bg-rose-500/80 px-2 py-1 text-xs font-semibold text-white shadow-lg">
                            {eventToast}
                        </div>
                    )}
                    {state === "fetching-ice" && (
                        <Overlay>
                            <Loader2 className="mr-2 h-5 w-5 animate-spin" />
                            fetching ICE config
                        </Overlay>
                    )}
                    {state === "connecting" && (
                        <Overlay>
                            <Loader2 className="mr-2 h-5 w-5 animate-spin" />
                            negotiating WebRTC
                        </Overlay>
                    )}
                    {state === "error" && (
                        <Overlay>
                            <AlertTriangle className="mb-2 h-6 w-6 text-rose-400" />
                            <p className="text-sm text-slate-200">
                                {error ?? "playback failed"}
                            </p>
                            <Button
                                size="sm"
                                variant="outline"
                                className="mt-2"
                                onClick={retry}
                            >
                                <RefreshCw className="mr-1 h-3 w-3" />
                                Retry
                            </Button>
                        </Overlay>
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

function Overlay({ children }: { children: React.ReactNode }) {
    return (
        <div className="absolute inset-0 flex flex-col items-center justify-center bg-black/60 text-slate-300">
            {children}
        </div>
    );
}

export default LiveVideo;
