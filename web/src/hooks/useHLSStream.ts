import { useCallback, useEffect, useRef, useState } from "react";
import Hls, { type ErrorData } from "hls.js";
import { authHeaderFor } from "@/api/client";

/**
 * useHLSStream — single-camera HLS viewer.
 *
 * HLS is the simpler path vs WebRTC: no SDP exchange, no ICE
 * negotiation, just a regular HTTP pull of an m3u8 playlist. The
 * cost is ~1-2s of extra latency (HLS segments are usually 2-4s
 * long), which is fine for a home dashboard that just shows "is
 * the camera alive?".
 *
 * The camera here streams H.265 over RTSP. go2rtc's HLS endpoint
 * (GET /api/stream.m3u8?src=<name>) defaults to passthrough, so
 * the resulting m3u8 advertises `CODECS="hvc1..."` (HEVC). Whether
 * the browser can actually decode that is a property of the
 * underlying MSE/decoder support — Chrome on Windows 11+ /
 * macOS Apple Silicon has HEVC hardware decode, Firefox does not.
 * We always attach the source, and surface the error so the UI
 * can render a "your browser can't decode H.265" hint.
 *
 * Usage:
 *   const { videoRef, state, retry } = useHLSStream({ src: "/go2rtc/api/stream.m3u8?src=cam_1" });
 *   <video ref={videoRef} autoPlay playsInline muted controls />
 */
export type HLSState =
    | "idle"
    | "loading"
    | "playing"
    | "error";

export interface UseHLSStreamOptions {
    /** Fully-resolved m3u8 URL (relative to dashboard origin is fine). */
    src: string;
}

export interface UseHLSStreamResult {
    videoRef: React.RefObject<HTMLVideoElement>;
    state: HLSState;
    error: string | null;
    retry: () => void;
    stop: () => void;
}

export function useHLSStream(
    opts: UseHLSStreamOptions,
): UseHLSStreamResult {
    const videoRef = useRef<HTMLVideoElement>(null);
    const hlsRef = useRef<Hls | null>(null);
    const [state, setState] = useState<HLSState>("idle");
    const [error, setError] = useState<string | null>(null);
    const [nonce, setNonce] = useState(0);

    const teardown = useCallback(() => {
        const hls = hlsRef.current;
        if (hls) {
            try { hls.destroy(); } catch { /* */ }
            hlsRef.current = null;
        }
        const v = videoRef.current;
        if (v) {
            try {
                v.pause();
                v.removeAttribute("src");
                v.load();
            } catch { /* */ }
        }
    }, []);

    useEffect(() => {
        // Null/undefined src means the parent has suspended us
        // (e.g. waiting on a primary playback path). Skip every
        // side effect; the hook stays in "idle" until the parent
        // hands us a real URL.
        if (!opts.src) {
            setState("idle");
            setError(null);
            return;
        }
        let cancelled = false;
        setError(null);
        setState("loading");

        const v = videoRef.current;
        if (!v) {
            setState("error");
            setError("video element not mounted");
            return;
        }

        // The video element's native events are the ground truth
        // for "is this thing actually playing?". `v.play()`
        // resolves once playback starts, but it can also stay
        // pending (e.g. the first frame is decoded but vsync has
        // not fired yet) or — in our HEVC passthrough case — it
        // can resolve immediately while the decoder chokes on the
        // first segment and we never see any visual progress. We
        // need *both* signals: a `playing` event is the
        // authoritative "frames are on screen"; an `error` event
        // or a stalled-MSE timeout is the authoritative "this
        // can't play".
        const onPlaying = () => {
            if (cancelled) return;
            setState("playing");
            setError(null);
        };
        const onError = () => {
            if (cancelled) return;
            // If hls.js already set a more specific error (e.g.
            // manifestIncompatibleCodecsError), keep it.
            setState("error");
            setError((prev) => prev ?? `video element error: ${v.error?.code ?? "unknown"}`);
        };
        const onWaiting = () => {
            if (cancelled) return;
            // The browser is waiting for more media data. If the
            // HLS session has been reaped (404s on the media
            // playlist), no new data will ever come and we'll sit
            // here forever. Mark "loading" so the UI can show a
            // spinner; the stall watchdog below will turn it into
            // an error if nothing arrives.
            setState("loading");
        };
        v.addEventListener("playing", onPlaying);
        v.addEventListener("error", onError);
        v.addEventListener("waiting", onWaiting);

        // Stall watchdog: if neither `playing` nor an error fires
        // within stallTimeoutMs, treat the stream as broken. The
        // most common cause is the go2rtc HLS consumer being
        // reaped — the segments download fine but the media-
        // playlist 404s after the keepalive of inactivity, so
        // hls.js never gets a new segment and `<video>` parks
        // itself in "waiting for data" forever.
        //
        // We size the watchdog to comfortably exceed the go2rtc
        // keepalive plus a couple of segment downloads. Frigate's
        // bundled go2rtc uses the upstream 5s keepalive which
        // cannot be patched; instead we drive segment size down
        // (hls.segment=1) so each .m4s fits inside the 5s window
        // on Tunnel links, and we let the watchdog absorb the
        // hls.js frag-load retry stack.
        //
        // Stall watchdog: if the HLS stream doesn't reach "playing"
        // state within 15s of starting, declare it dead. The previous
        // 60s timeout was far too long — on LAN the first segment
        // loads in <2s, and even on a slow Cloudflare Tunnel link a
        // 1.8MB HEVC .m4s at 2.5 Mbps arrives in ~10s. 15s covers
        // the slow-link cold start without leaving the user staring
        // at a frozen frame for a full minute. If hls.js emits a
        // fatal error before the timer fires, the error handler
        // below will surface it immediately.
        const stallTimeoutMs = 15_000;
        const stallTimer = window.setTimeout(() => {
            if (cancelled) return;
            setState((cur) => {
                if (cur === "playing") return cur;
                setError("HLS stream stalled: no new segments arrived in time (check that the go2rtc HLS session is alive)");
                return "error";
            });
        }, stallTimeoutMs);

        // Probe HEVC decode support *before* handing off to hls.js.
        // go2rtc's HLS passthrough ships the camera's native HEVC
        // (see `CODECS="hvc1..."` in the master playlist) — we have
        // no transcoder in the pipeline (deploy/go2rtc/Dockerfile
        // deliberately excludes ffmpeg). If the browser's MSE can't
        // actually decode the stream, hls.js will happily pull
        // every segment, feed them to MSE, and the decoder will
        // silently produce black frames. The `<video>` element
        // never fires `playing`, the stall watchdog eventually
        // trips, and the user sees a misleading "HLS stream
        // stalled" message that points at go2rtc when the actual
        // problem is the browser.
        //
        // `canPlayType` returns "" when the browser has no decoder
        // for the requested codec. Chrome on Windows additionally
        // requires the paid "HEVC Video Extensions" plugin from the
        // Microsoft Store; Firefox and Linux Chrome have no HEVC
        // support at all. Safari on Apple Silicon decodes HEVC in
        // hardware. Probe with a realistic codec string — we use
        // the profile that Hikvision ships (`hvc1.1.6.L153.B0`).
        //
        // hls.js doesn't actually use the `<video>` element's
        // decoder for its own MSE pipeline — it pushes bytes to
        // `MediaSource` and relies on the browser's MSE decoder.
        // `video.canPlayType` and `MediaSource.isTypeSupported`
        // can disagree on Chrome-on-Windows without the HEVC
        // extension installed: the former returns "" (no element
        // decoder), but the latter can claim "maybe" and hls.js
        // will then send bytes that decode to nothing. Probe BOTH
        // and require both to be non-empty before we attempt
        // playback; if either says no, surface a clear error.
        const hevcProbe = 'video/mp4; codecs="hvc1.1.6.L153.B0"';
        const canPlayHEVC = v.canPlayType(hevcProbe) !== "";
        const mseSupportsHEVC = typeof MediaSource !== "undefined"
            && typeof MediaSource.isTypeSupported === "function"
            && MediaSource.isTypeSupported(hevcProbe);
        if (!canPlayHEVC || !mseSupportsHEVC) {
            window.clearTimeout(stallTimer);
            v.removeEventListener("playing", onPlaying);
            v.removeEventListener("error", onError);
            v.removeEventListener("waiting", onWaiting);
            setState("error");
            setError(
                `Browser cannot decode H.265/HEVC over MSE (canPlayType=${canPlayHEVC ? "yes" : "no"}, MSE=${mseSupportsHEVC ? "yes" : "no"}). ` +
                "The camera streams HEVC and this platform does not include a server-side transcoder. " +
                "Options: (1) install 'HEVC Video Extensions' from the Microsoft Store (Chrome on Windows), " +
                "(2) switch to a browser with built-in HEVC support (Safari on Apple Silicon), or " +
                "(3) swap the camera for an H.264 model.",
            );
            return;
        }

        // Native HLS — legacy Safari / iOS WebView without MSE.
        // The modern Safari 11+, Edge, Chrome, Firefox all support
        // MSE and hls.js works on every one of them. We only hit
        // this branch on the rare WebKit build that lacks MSE
        // entirely, in which case the browser's own HLS pipeline
        // is the only path available. The video element listeners
        // installed above (onPlaying / onError / onWaiting) cover
        // the state transitions, so we just kick off playback.
        //
        // **Order matters**: this branch is now strictly a
        // FALLBACK. The hls.js path below is tried first so that
        // browsers which support MSE go through hls.js (where the
        // Authorization header can be attached via xhrSetup).
        // Previously this was the *first* check, and Edge on
        // Windows 11+ — which ships a built-in HLS player on top
        // of Media Foundation and answers
        // `canPlayType("application/vnd.apple.mpegurl")` with
        // "probably" — took the URL via `video.src = url`, the
        // hls.js xhrSetup was never invoked, no Authorization
        // header was attached, nginx's auth_request rejected the
        // m3u8 request with 401, and the HLS pipeline stalled
        // with an empty 401 body. hls.js works on every modern
        // browser that supports MSE (Chrome, Edge, Firefox,
        // Safari 11+), so preferring it everywhere is safe.
        if (!Hls.isSupported()) {
            if (v.canPlayType("application/vnd.apple.mpegurl")) {
                v.src = opts.src;
                v.play().catch(() => { /* handled by onError / stall watchdog */ });
                return () => {
                    cancelled = true;
                    window.clearTimeout(stallTimer);
                    v.removeEventListener("playing", onPlaying);
                    v.removeEventListener("error", onError);
                    v.removeEventListener("waiting", onWaiting);
                    teardown();
                };
            }

            // No HLS path at all — neither hls.js nor native.
            window.clearTimeout(stallTimer);
            v.removeEventListener("playing", onPlaying);
            v.removeEventListener("error", onError);
            v.removeEventListener("waiting", onWaiting);
            setState("error");
            setError("HLS not supported in this browser");
            return;
        }

        const hls = new Hls({
            // The stream is HEVC. hls.js will refuse to play it on a
            // browser without HEVC codec support and surface a
            // manifestIncompatibleCodecsError. We let that bubble up
            // to the UI so the user can read a clear error message.
            enableWorker: true,
            lowLatencyMode: false,
            // Cameras are stable bandwidth; no aggressive ABR.
            capLevelToPlayerSize: true,
            // Buffer: 12s in-flight, 30s headroom. The go2rtc
            // HLS server uses an upstream consumer keepalive of 5s
            // (Frigate's bundled go2rtc — we can't patch it like we
            // did for the standalone container). On slow Cloudflare
            // Tunnel links (~2.5 Mbps) a 1.8 MB HEVC segment can
            // take 9.97s to download, which is well past 5s. We
            // compensate with a long maxBufferLength so the player
            // can re-buffer after a stall without reloading the
            // master playlist, plus aggressive frag-load retries.
            maxBufferLength: 12,
            maxMaxBufferLength: 60,
            // Frag-level loading: a slow segment is the dominant
            // failure mode on Tunnel links. hls.js defaults to 1
            // retry with a 4s base backoff — we bump to 6 retries
            // with a 2s base so a single 9.97s segment doesn't
            // fail the whole pipeline. Max wait becomes ~64s.
            fragLoadingMaxRetry: 6,
            fragLoadingMaxRetryTimeout: 32000,
            manifestLoadingMaxRetry: 6,
            manifestLoadingMaxRetryTimeout: 16000,
            // Each segment/playlist request must complete in 20s
            // (was 60s default — a stalled playlist poll was
            // keeping the connection alive long past the go2rtc
            // session timeout, causing the next poll to 404).
            fragLoadingTimeOut: 20000,
            manifestLoadingTimeOut: 20000,
            // Attach the dashboard's JWT to every XHR hls.js makes
            // (master playlist, media playlist, every segment, every
            // key). nginx's `/go2rtc/` location is gated by
            // `auth_request /api/v1/auth/verify`, so without the
            // header the master-playlist request returns 401 and
            // hls.js sees an empty body — looks like a "stalled"
            // stream in the UI. `xhrSetup` runs once per XHR
            // instance, so the header is added on playlist polls,
            // segment downloads, and the eventual key fetch.
            //
            // **Order matters.** hls.js calls xhrSetup BEFORE
            // open() in openAndSendXhr — and XMLHttpRequest
            // spec mandates setRequestHeader() to be called AFTER
            // open() (in OPENED state), otherwise the call throws
            // InvalidStateError on spec-compliant browsers
            // (Chromium-family honours this strictly). If we
            // call setRequestHeader first, the header is silently
            // dropped, the auth_request sub-call has no
            // Authorization, nginx returns 401, and the HLS
            // pipeline appears to "stall" with an empty body.
            //
            // We use xhrSetup (per-stream instance) rather than
            // Hls.DefaultConfig.xhrSetup or overriding the loader
            // globally: the global config is shared across all
            // consumers on the page, which would let a stale JWT
            // (e.g. after the user re-binds) keep being attached
            // by detached players. The per-instance closure calls
            // authHeaderFor() lazily, so a token refreshed mid-
            // playback picks up correctly.
            //
            // After our open() call, the hls.js openAndSendXhr
            // path's `e.readyState||e.open(...)` check sees
            // readyState === 1 (OPENED) and skips its own
            // open(), so we don't trigger an
            // "open() already called" exception.
            xhrSetup: (xhr, url) => {
                xhr.open("GET", url, true);
                const h = authHeaderFor();
                if (h) xhr.setRequestHeader(h.name, h.value);
            },
        });
        hlsRef.current = hls;

        hls.on(Hls.Events.MANIFEST_PARSED, () => {
            if (cancelled) return;
            // play() returns a Promise; we don't rely on it alone
            // for the state transition because (a) the browser can
            // resolve it before frames are actually visible, and
            // (b) some browsers hold it pending while the MSE
            // source is still buffering. The video element's
            // `playing` event is the authoritative signal.
            v.play().catch(() => { /* handled by onError / stall watchdog */ });
        });

        hls.on(Hls.Events.ERROR, (_evt, data: ErrorData) => {
            if (cancelled) return;
            // hls.js retries network errors internally; we only
            // surface fatal errors. manifestIncompatibleCodecsError
            // is fatal in our setup because the source is HEVC and
            // the browser cannot decode it. We do NOT ship a server-
            // side transcoder (see deploy/go2rtc/Dockerfile), so
            // the only escape is an HEVC-capable browser or an
            // H.264 camera.
            if (data.fatal) {
                let msg = `${data.type}: ${data.details}`;
                if (data.details === "manifestIncompatibleCodecsError") {
                    msg = "Browser cannot decode H.265/HEVC; use a browser with HEVC support or swap the camera for an H.264 model";
                }
                setState("error");
                setError(msg);
                hls.destroy();
                hlsRef.current = null;
            }
        });

        hls.loadSource(opts.src);
        hls.attachMedia(v);

        return () => {
            cancelled = true;
            window.clearTimeout(stallTimer);
            v.removeEventListener("playing", onPlaying);
            v.removeEventListener("error", onError);
            v.removeEventListener("waiting", onWaiting);
            teardown();
        };
    }, [opts.src, nonce, teardown]);

    const retry = useCallback(() => setNonce((n) => n + 1), []);
    const stop = useCallback(() => teardown(), [teardown]);

    return { videoRef, state, error, retry, stop };
}
