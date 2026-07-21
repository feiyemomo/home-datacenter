import { useCallback, useEffect, useRef, useState } from "react";
import { getIceConfig } from "@/api/camera";
import { authedFetch } from "@/api/client";
import type { IceConfig } from "@/types";

/**
 * waitForIceGathering — resolve once the browser has finished
 * gathering ICE candidates for `pc`, or `timeoutMs` elapses.
 *
 * The browser enumerates host candidates immediately, but STUN-
 * reflected and relay candidates require a round trip to the STUN
 * server. If we POST the SDP offer before that round trip completes,
 * the SDP lacks the late-arriving candidates and the offer only
 * contains the host candidates — which are useless when the host
 * is on a private subnet (e.g. Docker bridge 172.x.x.x).
 *
 * Most browsers finish gathering in 100-500ms on a healthy network;
 * the default 3000ms timeout is generous enough to cover slow STUN
 * servers but short enough to keep the fallback path responsive.
 */
function waitForIceGathering(
    pc: RTCPeerConnection,
    timeoutMs: number,
): Promise<void> {
    if (pc.iceGatheringState === "complete") {
        return Promise.resolve();
    }
    return new Promise((resolve) => {
        const onChange = () => {
            if (pc.iceGatheringState === "complete") {
                pc.removeEventListener("icegatheringstatechange", onChange);
                window.clearTimeout(timer);
                resolve();
            }
        };
        const timer = window.setTimeout(() => {
            pc.removeEventListener("icegatheringstatechange", onChange);
            resolve();
        }, timeoutMs);
        pc.addEventListener("icegatheringstatechange", onChange);
    });
}

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
    cameraId: number;
    streamName: string;
    webrtcUrl: string;
    /** When the WebRTC server is behind a different origin, override
     *  the SDP endpoint entirely. Default: relative path on the
     *  same origin as the dashboard. */
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
        let iceDisconnectTimer: number | null = null;
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
                pc.oniceconnectionstatechange = () => {
                    // We intentionally DO NOT treat iceConnectionState
                    // 'failed' as an immediate terminal error here. The
                    // browser briefly reports 'failed' as it cycles
                    // through candidate pairs (especially over ICE-TCP
                    // on slow links) before settling on 'connected' or
                    // 'completed'. Surfacing that transient as an
                    // error triggers LiveVideo's onFallback → HLS
                    // remount, which closes the peer connection just
                    // as it was about to stabilise.
                    //
                    // However, 'disconnected' must not be ignored
                    // indefinitely. The browser will only promote
                    // 'disconnected' to 'failed' after a 30s timeout,
                    // during which the user sees a frozen frame with
                    // no error. To keep the perceived latency low we
                    // start our own 8s timer on 'disconnected'; if
                    // ICE does not recover to 'connected'/'completed'
                    // in that window, we fall back to HLS.
                    if (cancelled) return;
                    const st = pc.iceConnectionState;
                    // eslint-disable-next-line no-console
                    console.debug(`[webrtc] iceConnectionState=${st}`);
                    if (st === "disconnected") {
                        if (iceDisconnectTimer) {
                            window.clearTimeout(iceDisconnectTimer);
                        }
                        iceDisconnectTimer = window.setTimeout(() => {
                            if (cancelled) return;
                            // Re-check state in case ICE recovered
                            // between the timer being armed and firing.
                            const cur = pc.iceConnectionState;
                            if (cur === "disconnected" || cur === "failed") {
                                // eslint-disable-next-line no-console
                                console.warn(
                                    `[webrtc] ICE ${cur} for >8s — falling back to HLS`,
                                );
                                setState("error");
                                setError(`ice ${cur} (8s timeout)`);
                            }
                        }, 8_000);
                    } else if (st === "connected" || st === "completed") {
                        if (iceDisconnectTimer) {
                            window.clearTimeout(iceDisconnectTimer);
                            iceDisconnectTimer = null;
                        }
                    }
                };
                pc.onconnectionstatechange = () => {
                    if (cancelled) return;
                    // connectionState is the most stable aggregate
                    // signal. 'failed' / 'closed' are terminal — at
                    // this point the peer connection is genuinely
                    // dead and the UI should fall back to HLS.
                    if (pc.connectionState === "failed" ||
                        pc.connectionState === "closed") {
                        setState("error");
                        setError(`connection ${pc.connectionState}`);
                    }
                };

                const offer = await pc.createOffer();
                await pc.setLocalDescription(offer);

                // Wait for ICE gathering to complete BEFORE POSTing the
                // SDP offer. If we send the offer while candidates are
                // still being gathered, the SDP we hand to go2rtc
                // contains only the first host candidate — and if that
                // one is wrong (e.g. Docker bridge IP 172.x.x.x that
                // the browser can't reach), the connection dies even
                // though a perfectly fine 127.0.0.1:8555 candidate
                // would have arrived 200ms later. iceGatheringState
                // transitions to 'complete' once the browser has
                // finished enumerating host + STUN-reflected
                // candidates, or our hard timeout fires.
                await waitForIceGathering(pc, 3000);
                if (cancelled) return;

                // Same-origin POST to home-api. The SDP body is read
                // exactly once in the Go handler and forwarded
                // exactly once via Go2RTCClient.ExchangeSDP, so
                // nginx's auth_request machinery is bypassed (and
                // the body-discard bug that used to hang the
                // request for 60s on proxy_send_timeout is
                // avoided).
                //
                // We deliberately do NOT prepend `ice.webrtc_base`
                // here. The old /go2rtc/ reverse-proxy path used
                // to be reached through that base, but the new
                // home-api proxy is a same-origin REST endpoint
                // that home-api's own JWT middleware already
                // protects — the public base isn't a hop on this
                // path. If we naively concatenated (e.g.
                // "/go2rtc" + "/api/v1/cameras/17/webrtc"), the
                // URL would hit nginx's /go2rtc/ location, get
                // proxied to go2rtc, which has no such endpoint
                // — go2rtc would hang waiting for ICE on a path
                // it never recognised, and the browser would see
                // a 60s 500. The fix is to always go through the
                // same-origin REST route.
                //
                // The browser still talks directly to go2rtc:8555
                // for the RTP media (via the ICE candidates in
                // the SDP answer), but the SDP path is just a
                // regular JWT-authenticated POST.
                const sdpUrl = opts.sdpUrlOverride
                    ?? `/api/v1/cameras/${opts.cameraId}/webrtc`;
                const resp = await authedFetch(sdpUrl, {
                    method: "POST",
                    headers: { "Content-Type": "application/sdp" },
                    body: offer.sdp ?? "",
                });
                if (!resp.ok) {
                    // The body of the error response is the most
                    // useful diagnostic. go2rtc returns plain text
                    // (not JSON) on its 5xx; nginx returns JSON via
                    // the @go2rtc_unauthorized handler on 401. We
                    // try to read a small slice of the body for
                    // both, falling back to the status line if the
                    // body is empty or unreadable.
                    let detail = "";
                    try {
                        detail = (await resp.text()).slice(0, 200);
                    } catch { /* */ }
                    throw new Error(
                        `SDP ${resp.status} ${resp.statusText}` +
                        (detail ? `: ${detail}` : ""),
                    );
                }
                const answer = await resp.text();
                if (cancelled) return;
                await pc.setRemoteDescription({
                    type: "answer",
                    sdp: answer,
                });

                // After setRemoteDescription, ICE connectivity checks
                // begin. We don't have an explicit 'connected' event
                // on RTCPeerConnection, but pc.oniceconnectionstatechange
                // will report 'connected' or 'completed' on success,
                // and 'failed' if all candidates are unreachable. The
                // parent's WebRTCVideo surface will see state "playing"
                // when ontrack fires, or "error" when oniceconnection
                // statechange reports 'failed' — both paths are
                // covered above.
            } catch (e) {
                if (cancelled) return;
                setState("error");
                setError(e instanceof Error ? e.message : String(e));
                teardown();
            }
        })();

        return () => {
            cancelled = true;
            if (iceDisconnectTimer) {
                window.clearTimeout(iceDisconnectTimer);
                iceDisconnectTimer = null;
            }
            teardown();
        };
    }, [opts.cameraId, opts.streamName, opts.webrtcUrl, opts.sdpUrlOverride, nonce, teardown]);

    const retry = useCallback(() => setNonce((n) => n + 1), []);
    const stop = useCallback(() => teardown(), [teardown]);

    // Video element error listener.
    //
    // The hook sets `state = "playing"` on `ontrack` (an RTP frame
    // is arriving), but the <video> element independently reports
    // codec decode failures via the `error` event. The most common
    // shape for an HEVC camera on a Chromium-derivative browser
    // (Chrome / Edge / WebView) is:
    //
    //   1. SDP exchange succeeds, ICE+DTLS connect, ontrack fires.
    //   2. RTP packets arrive carrying H.265 NAL units.
    //   3. Chromium's WebRTC codec registry does NOT include H.265
    //      (RFC 7798 is registered in the SDP but never wired to
    //      the system HEVC decoder on the WebRTC side — see
    //      docs/platformization.md for the per-browser matrix).
    //   4. The video element's MediaError reports
    //      MEDIA_ERR_SRC_NOT_SUPPORTED (code 4). `connectionState`
    //      stays "connected" because the network path is fine —
    //      it's the *decoder* that can't render the frames.
    //
    // Without this listener, the hook parks at "playing" forever
    // and the UI shows a black tile. The MediaError codes that
    // matter for the fallback decision are 3 (MEDIA_ERR_DECODE,
    // the stream itself is unsupported) and 4
    // (MEDIA_ERR_SRC_NOT_SUPPORTED, the browser can't play this
    // MIME/codec at all). Code 1 (aborted) and 2 (network) are
    // transient and not actionable here.
    useEffect(() => {
        const v = videoRef.current;
        if (!v) return;
        const onError = () => {
            const code = v.error?.code;
            // Only convert terminal codec / source failures into
            // the hook's "error" state. Codes 1/2 are transient
            // and we let the underlying recovery path (SDP retry,
            // ICE reconnect) handle them — flipping to HLS on a
            // momentary network blip would cause a needless
            // remount storm.
            if (code === 3 || code === 4) {
                const label = code === 4 ? "SRC_NOT_SUPPORTED" : "DECODE";
                setState("error");
                setError(`video element error: ${code} (${label})`);
                teardown();
            }
        };
        v.addEventListener("error", onError);
        return () => v.removeEventListener("error", onError);
    }, [teardown]);

    return { videoRef, state, error, retry, stop };
}
