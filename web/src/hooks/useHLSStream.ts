import { useCallback, useEffect, useRef, useState } from "react";
import Hls, { type ErrorData } from "hls.js";

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
        // playlist 404s after `keepalive` of inactivity, so
        // hls.js never gets a new segment and `<video>` parks
        // itself in "waiting for data" forever.
        //
        // We size the watchdog to comfortably exceed the patched
        // go2rtc keepalive (30s, see deploy/go2rtc/Dockerfile) plus
        // one full segment download (~2-3s for a 1MB HEVC chunk
        // over a slow link) plus HLS startup latency (~2s for the
        // initial playlist + first segment round-trip). 45s is
        // generous enough that a healthy stream always wins, but
        // short enough that a user-facing error appears within a
        // reasonable wait if the session really is dead.
        const stallTimeoutMs = 45_000;
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

        // Native HLS — Safari (and iOS WebView). Hand it the URL
        // directly; no JS player required. The video element
        // listeners installed above (onPlaying / onError /
        // onWaiting) already cover the state transitions, so we
        // only kick off playback here.
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

        // MSE / hls.js — Chrome, Firefox, Edge.
        if (!Hls.isSupported()) {
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
            // Buffer settings. The go2rtc HLS server has a patched
            // consumer keepalive of 30s (see deploy/go2rtc/Dockerfile
            // — upstream hardcodes 5s, which is too short for HEVC
            // segments). With a 30s keepalive we can safely use
            // hls.js's default-ish buffer: ~12s in-flight, up to 30s
            // headroom. The old value of 6s was a workaround for the
            // 5s keepalive — it starved the decoder and made hls.js
            // park in "waiting" forever, which our stall watchdog
            // then misread as a dead session.
            maxBufferLength: 12,
            maxMaxBufferLength: 30,
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
