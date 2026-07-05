import { useCallback, useEffect, useRef, useState } from "react";
import { getIceConfig } from "@/api/camera";
import type { IceConfig } from "@/types";

/**
 * useWebRTCStream — single-camera WebRTC viewer.
 *
 * Usage:
 *   const { videoRef, state, retry } = useWebRTCStream({
 *     streamName: "cam_1",
 *     webrtcUrl:  "http://home-go2rtc:1984/api/webrtc?src=cam_1",
 *   });
 *   <video ref={videoRef} autoPlay playsInline muted />
 *
 * Lifecycle:
 *   1. On mount, fetch /api/v1/cameras/ice (cached per page) to
 *      learn the STUN/TURN list and the public WebRTC base URL.
 *   2. Open an RTCPeerConnection, add a video transceiver, send an
 *      SDP offer to `webrtcUrl` (or to `<ice.webrtc_base>/api/webrtc?src=<name>`
 *      if a base override is configured).
 *   3. The remote track is wired to the supplied <video> ref.
 *   4. On any failure, surface the error in `state` so the UI can
 *      render a retry button. `retry()` re-runs the handshake.
 *
 * The hook tears the connection down on unmount. It also tears it
 * down when `streamName` changes (parent component is responsible
 * for remounting the <video> in that case if it cares about that).
 */
export type WebRTCState =
    | "idle"
    | "fetching-ice"
    | "connecting"
    | "playing"
    | "error";

export interface UseWebRTCStreamOptions {
    streamName: string;
    webrtcUrl: string;
    /** When the WebRTC server is behind a different origin, override
     *  the SDP endpoint entirely. Default: `webrtcUrl`. */
    sdpUrlOverride?: string;
    /** Disable the auto-reconnect on connection-state changes. */
    autoReconnect?: boolean;
}

export interface UseWebRTCStreamResult {
    videoRef: React.RefObject<HTMLVideoElement>;
    state: WebRTCState;
    error: string | null;
    retry: () => void;
    stop: () => void;
}

export function useWebRTCStream(
    opts: UseWebRTCStreamOptions,
): UseWebRTCStreamResult {
    const videoRef = useRef<HTMLVideoElement>(null);
    const pcRef = useRef<RTCPeerConnection | null>(null);
    const [state, setState] = useState<WebRTCState>("idle");
    const [error, setError] = useState<string | null>(null);
    const [nonce, setNonce] = useState(0);

    // Tear down an in-flight peer connection (idempotent).
    const teardown = useCallback(() => {
        const pc = pcRef.current;
        if (pc) {
            try {
                pc.getSenders().forEach((s) => {
                    try { s.track?.stop(); } catch { /* */ }
                });
                pc.close();
            } catch { /* */ }
            pcRef.current = null;
        }
        if (videoRef.current) {
            try { videoRef.current.srcObject = null; } catch { /* */ }
        }
    }, []);

    useEffect(() => {
        let cancelled = false;
        setError(null);

        (async () => {
            try {
                setState("fetching-ice");
                let ice: IceConfig | null = null;
                try {
                    ice = await getIceConfig();
                } catch {
                    // ICE config is optional — proceed with browser defaults.
                }
                if (cancelled) return;

                setState("connecting");
                const iceServers = (ice?.ice_servers ?? []) as RTCIceServer[];
                const pc = new RTCPeerConnection({ iceServers });
                pcRef.current = pc;

                // Video only — camera audio codecs (G726/PCMU/MPEG4-
                // GENERIC) are not browser-decodable via WebRTC. The
                // API also appends #audio=0 to the go2rtc source URL
                // so go2rtc won't even try to negotiate audio.
                pc.addTransceiver("video", { direction: "recvonly" });

                pc.ontrack = (ev) => {
                    if (videoRef.current && ev.streams[0]) {
                        videoRef.current.srcObject = ev.streams[0];
                        setState("playing");
                    }
                };
                pc.onconnectionstatechange = () => {
                    if (cancelled) return;
                    if (pc.connectionState === "failed" ||
                        pc.connectionState === "closed") {
                        setState("error");
                        setError(`connection ${pc.connectionState}`);
                    }
                };

                const offer = await pc.createOffer();
                await pc.setLocalDescription(offer);

                // Prefer the browser-accessible base from ICE config.
                // The per-camera webrtcUrl may contain an internal Docker
                // hostname (e.g. http://home-go2rtc:1984) that the browser
                // cannot resolve, causing "Failed to fetch".
                const sdpUrl = opts.sdpUrlOverride
                    ?? (ice?.webrtc_base
                        ? `${ice.webrtc_base}/api/webrtc?src=${encodeURIComponent(opts.streamName)}`
                        : opts.webrtcUrl);
                const resp = await fetch(sdpUrl, {
                    method: "POST",
                    headers: { "Content-Type": "application/sdp" },
                    body: offer.sdp ?? "",
                });
                if (!resp.ok) {
                    throw new Error(`SDP ${resp.status}`);
                }
                const answer = await resp.text();
                if (cancelled) return;
                await pc.setRemoteDescription({
                    type: "answer",
                    sdp: answer,
                });
            } catch (e) {
                if (cancelled) return;
                setState("error");
                setError(e instanceof Error ? e.message : String(e));
                teardown();
            }
        })();

        return () => {
            cancelled = true;
            teardown();
        };
    }, [opts.streamName, opts.webrtcUrl, opts.sdpUrlOverride, nonce, teardown]);

    const retry = useCallback(() => setNonce((n) => n + 1), []);
    const stop = useCallback(() => teardown(), [teardown]);

    return { videoRef, state, error, retry, stop };
}
