/**
 * fmp4Mse — Minimal MediaSource play for the recording /stream endpoint.
 *
 * The backend emits a fragmented MP4 (H.264/AAC) and streams fragments as
 * the encoder produces them, so a cold HEVC minute delivers its first
 * frame in ~1-2s instead of waiting for the whole 60s transcode. MSE is
 * the only way to feed a growing byte stream into <video> reliably.
 *
 * This module hand-rolls the two parts MSE needs and keeps the bundle
 * dependency-free:
 *
 *   1. ISO-BMFF box walking to split the init segment (ftyp..moov) out of
 *      the live byte stream and to read the codec string from moov
 *      (avc1 from the avcC box, mp4a.40.2 assumed — we always encode AAC).
 *   2. An MP3-style append queue that feeds SourceBuffer sequentially.
 *
 * ftyp/moov are small and written first by ffmpeg's empty_moov, so the
 * whole init segment is available after a few KB. Everything after that is
 * moof/mdat fragments we forward in arrival order.
 */

function readUint32(ab: ArrayBufferLike, off: number): number {
    return new DataView(ab as ArrayBuffer).getUint32(off, false);
}

/** Resolve the byte offset where the top-level `moov` box ends (its end
 *  offset), or -1 if the buffer doesn't yet contain it. Handles 64-bit
 *  sizes but not size==0 (extend-to-end) — ffmpeg writes explicit sizes. */
function findMoovEnd(buf: Uint8Array): number {
    let pos = 0;
    while (pos + 8 <= buf.length) {
        const size32 = readUint32(buf.buffer, buf.byteOffset + pos);
        const type = String.fromCharCode(
            buf[pos + 4], buf[pos + 5], buf[pos + 6], buf[pos + 7],
        );
        let header = 8;
        let size = size32;
        if (size === 1) {
            header = 16;
            const dv = new DataView(buf.buffer as ArrayBuffer, buf.byteOffset + pos + 8, 8);
            size = Number(dv.getBigUint64(0, false));
        }
        if (type === "moov") return pos + header + (size - header);
        // size === 0 means "box extends to end of file"; we cannot safely
        // skip forward without knowing its bounds, so stop scanning. Without
        // this guard, pos never advances and the loop spins forever, freezing
        // the tab (this is exactly the "页面无响应" reported on recording
        // switches if a producer emits a size-0/legacy top-level box).
        if (size === 0) break;
        const next = pos + header + (size - header);
        if (next <= pos) break; // safety: no progress
        pos = next;
    }
    return -1;
}

/** First child box of the given type within [start,end). Returns its
 *  payload offset (start of the box header has been skipped) + size. */
function findBox(data: Uint8Array, start: number, end: number, type: string): number {
    let pos = start;
    while (pos + 8 <= end) {
        const size32 = readUint32(data.buffer, data.byteOffset + pos);
        const boxType = String.fromCharCode(
            data[pos + 4], data[pos + 5], data[pos + 6], data[pos + 7],
        );
        let header = 8;
        let size = size32;
        if (size === 1) {
            header = 16;
            const dv = new DataView(data.buffer as ArrayBuffer, data.byteOffset + pos + 8, 8);
            size = Number(dv.getBigUint64(0, false));
        }
        if (boxType === type) return pos + header;
        // size === 0 → extends to end; cannot advance safely, stop scanning.
        if (size === 0) break;
        const next = pos + size;
        if (next <= pos || next > end) break; // no progress / past the window
        pos = next;
    }
    return -1;
}

/** Read the avc1 codec string from the init segment's avcC box. Returns
 *  null if it can't be found/parsed. */
function parseVideoCodec(init: Uint8Array): string | null {
    const moov = findBox(init, 0, init.length, "moov");
    if (moov < 0) return null;
    // Walk each trak; pick the video track via its hdlr handler type.
    let pos = moov;
    let codec: string | null = null;
    while (true) {
        const trak = findBox(init, pos, init.length, "trak");
        if (trak < 0) break;
        pos = trak; // continue searching after this trak next iteration
        const mdia = findBox(init, trak, init.length, "mdia");
        if (mdia < 0) continue;
        const hdlr = findBox(init, mdia, init.length, "hdlr");
        if (hdlr < 0) continue;
        // hdlr payload: version/flags(4) + pre_defined(4) + handler_type(4).
        const handler = String.fromCharCode(
            init[hdlr + 8], init[hdlr + 9], init[hdlr + 10], init[hdlr + 11],
        );
        if (handler !== "vide") continue;
        const minf = findBox(init, mdia, init.length, "minf");
        if (minf < 0) continue;
        const stbl = findBox(init, minf, init.length, "stbl");
        if (stbl < 0) continue;
        const stsd = findBox(init, stbl, init.length, "stsd");
        if (stsd < 0) continue;
        // First sample entry inside stsd, then its avcC child.
        const avcC = findBox(init, stsd, init.length, "avcC");
        if (avcC < 0) continue;
        const profile = init[avcC + 8]; // payload: ver(1) prof(1) compat(1) lvl(1)
        const compat = init[avcC + 9];
        const level = init[avcC + 10];
        const hex = (n: number) => n.toString(16).padStart(2, "0");
        codec = `avc1.${hex(profile)}${hex(compat)}${hex(level)}`;
        break;
    }
    return codec;
}

function concat(a: Uint8Array, b: Uint8Array): Uint8Array {
    const out = new Uint8Array(a.length + b.length);
    out.set(a, 0);
    out.set(b, a.length);
    return out;
}

/** MediaSource capability gate (Safari 17+ requires WebKit prefixes). */
export function mseSupported(): boolean {
    return typeof MediaSource !== "undefined" || typeof (window as any).WebKitMediaSource !== "undefined";
}

export interface MseStreamHandle {
    /** Abort the fetch + revoke the object URL. Safe to call twice. */
    cleanup: () => void;
}

const FB_CODEC = "avc1.640028,mp4a.40.2";

/**
 * Upper bound on bytes in flight between the fetch reader and the append
 * pump. The /stream endpoint emits the whole 60s clip; without a cap we
 * read it at socket speed and shovel every byte into SourceBuffer
 * continuously, which balloons the JS/MSE heap and saturates the main
 * thread (the tab shows "页面无响应" while switching recordings). Keeping
 * this modest bounds memory and lets the decoder keep pace.
 */
const MAX_INFLIGHT_BYTES = 8 * 1024 * 1024;

/** Microtask/event-loop yield so heavy loop iterations don't block paint. */
function sleep(ms: number): Promise<void> {
    return new Promise((r) => setTimeout(r, ms));
}

/**
 * Stream a fragmented MP4 into `video` via MediaSource.
 *
 * Reads the init segment from the head of the stream, derives the codec
 * string from moov, then forwards the remaining fragments into a
 * SourceBuffer in order. The returned handle's cleanup() must be called
 * when the caller switches recordings or unmounts.
 */
export function startMseStream(
    url: string,
    video: HTMLVideoElement,
    onError: (message: string) => void,
): MseStreamHandle {
    // Referenced by cleanup() — declared here so the closure sees a stable
    // binding regardless of call order.
    const ac = new AbortController();
    let activeSb: SourceBuffer | null = null;

    let aborted = false;
    let revoke = () => {};

    const cleanup = () => {
        aborted = true;
        ac.abort();
        revoke();
        // Remove the source only — do NOT call video.load(). On a cold
        // switch load() re-runs the media load algorithm against an empty
        // src, firing a spurious error event that React's onError turns into
        // a "播放失败" banner, and it forces a full reset the next stream
        // must recover from. Removing the blob URL is enough to stop the
        // current playback; the next startMseStream assigns a fresh src.
        try { video.removeAttribute("src"); } catch { /* noop */ }
        activeSb = null;
    };

    (async () => {
        let resp: Response;
        try {
            resp = await fetch(url, { signal: ac.signal, credentials: "same-origin" });
        } catch (e) {
            if (!aborted) onError(mseMessage(e));
            return;
        }
        if (!resp.ok || !resp.body) {
            if (!aborted) onError(`录像流加载失败 (HTTP ${resp.status})`);
            return;
        }
        const reader = resp.body.getReader();
        const isWebKit = typeof MediaSource === "undefined";
        const MS = (isWebKit ? (window as any).WebKitMediaSource : MediaSource) as typeof MediaSource;

        // ---- 1. Gather the init segment (ftyp..moov) ----
        let buf: Uint8Array = new Uint8Array(0);
        let moovEnd = -1;
        while (moovEnd < 0 || buf.length < moovEnd) {
            const { done, value } = await reader.read();
            if (done) {
                if (!aborted && moovEnd < 0) onError("录像流不完整（缺少元数据）");
                return;
            }
            buf = concat(buf, value);
            if (moovEnd < 0) moovEnd = findMoovEnd(buf);
        }
        const init = buf.slice(0, moovEnd);
        let rest = buf.slice(moovEnd);

        const vc = parseVideoCodec(init);
        const codecs = vc ? `${vc},mp4a.40.2` : FB_CODEC;

        if (aborted) return;

        // ---- 2. Set up MediaSource + SourceBuffer ----
        const ms = new MS();
        const objUrl = URL.createObjectURL(ms as any);
        revoke = () => URL.revokeObjectURL(objUrl);
        if (aborted) {
            revoke();
            return;
        }
        video.src = objUrl;

        const opened = new Promise<void>((res, rej) => {
            const handler = () => { ms.removeEventListener("sourceopen", handler); res(); };
            const errHandler = () => { ms.removeEventListener("sourceerror", errHandler); rej(new Error("MediaSource open failed")); };
            ms.addEventListener("sourceopen", handler);
            ms.addEventListener("sourceerror", errHandler);
        });
        try {
            await opened;
        } catch (e) {
            if (!aborted) onError(mseMessage(e));
            revoke();
            return;
        }
        if (aborted) {
            revoke();
            return;
        }
        activeSb = ms.addSourceBuffer(`video/mp4; codecs="${codecs}"`);

        // ---- 3. Append init, then stream fragments ----
        if (!activeSb) {
            onError("无法创建解码缓冲区");
            return;
        }

        // Sequential append pump. Yields to the event loop after each
        // fragment so the browser can demux/decode/paint incrementally
        // instead of running appendBuffer back-to-back on the main thread.
        const queue: Uint8Array[] = [];
        let queuedBytes = 0;
        let flushing = false;
        let ended = false;
        const drain = async () => {
            if (flushing || !activeSb) return;
            flushing = true;
            try {
                while (queue.length && !aborted) {
                    const chunk = queue.shift()!;
                    queuedBytes -= chunk.byteLength;
                    await appendOnce(activeSb, chunk);
                    await sleep(0);
                }
            } finally {
                flushing = false;
            }
            if (ended && queue.length === 0 && activeSb && activeSb.updating === false) {
                try { (ms as any).endOfStream(); } catch { /* already ended */ }
            }
        };
        // The init segment (ftyp+moov) MUST be appended BEFORE any moof/mdat
        // fragment so the demuxer has the track + codec configuration. It was
        // parsed above for parseVideoCodec but never fed to the SourceBuffer —
        // without it the browser fails with CHUNK_DEMUXER_ERROR_APPEND_FAILED
        // (MEDIA_ERR_SRC_NOT_SUPPORTED) on the first fragment and the clip
        // plays black. This is the "同一天切换黑屏" root cause: direct-URL clips
        // (first of a day) never build a SourceBuffer, so they play; only the
        // same-day MSE clips (2nd+) hit this and only go black until the day
        // changes and remounts the panel via the direct path.
        if (init.length) { queue.push(init); queuedBytes += init.byteLength; }
        if (rest.length) { queue.push(rest); queuedBytes += rest.byteLength; rest = new Uint8Array(0); }
        void drain();

        let readErr: unknown = null;
        try {
            for (;;) {
                const { done, value } = await reader.read();
                if (done) break;
                queue.push(value);
                queuedBytes += value.byteLength;
                void drain();
                // Backpressure: pause the network read until the append pump
                // has digested most of what's already buffered. Without this
                // the whole clip is pulled at socket speed and appended
                // without pause (heap bloat + main-thread saturation → page
                // unresponsive when switching recordings).
                while (queuedBytes > MAX_INFLIGHT_BYTES && !aborted) {
                    await sleep(16);
                }
            }
        } catch (e) {
            readErr = e;
        }
        if (aborted) return;
        if (readErr) {
            onError(mseMessage(readErr));
            return;
        }
        // Drain the tail, then signal end-of-stream so playback reaches
        // the clip's true end instead of a mid-buffer stall.
        ended = true;
        while (queue.length && !aborted) await sleep(16);
        void drain();
    })().catch((e) => {
        if (!aborted) onError(mseMessage(e));
    });

    return { cleanup };
}

function appendOnce(sb: SourceBuffer, data: Uint8Array): Promise<void> {
    return new Promise((res) => {
        const onUpdate = () => { sb.removeEventListener("updateend", onUpdate); res(); };
        sb.addEventListener("updateend", onUpdate);
        try {
            // SourceBuffer.appendBuffer expects an ArrayBuffer-backed view;
            // fetch() buffers are array-buffer backed, so the cast is safe.
            sb.appendBuffer(data as unknown as ArrayBuffer);
        } catch (e) {
            sb.removeEventListener("updateend", onUpdate);
            res(); // tolerate append hiccups; dropping one chunk stalls but recovers
        }
    });
}

function mseMessage(e: unknown): string {
    if (e instanceof DOMException && e.name === "AbortError") return "已停止";
    if (e instanceof Error && e.message) return e.message;
    return "MSE 播放失败";
}