import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import {
    Play,
    Pause,
    SkipBack,
    SkipForward,
    Loader2,
    RefreshCw,
    AlertTriangle,
    Gauge,
} from "lucide-react";
import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import {
    listRecordings,
    getMotionRanges,
    recordingFileUrl,
    type MotionRange,
} from "@/api/camera";
import { authHeaderFor } from "@/api/client";
import type { CameraRecording } from "@/types";

/**
 * RecordingTimeline — recording browser with 24-hour seekbar,
 * prominent event ribbon, and custom controls (double-tap ±10s,
 * long-press 5x, playback speed slider).
 *
 * The <video> element is rendered via React Portal into a target
 * element provided by the parent (LiveVideo's main video area) so
 * live and playback share the same video surface — there is no
 * separate "playback player" anymore.
 *
 * Mirrors the Android app's DayPlaybackFragment:
 *   - Day picker chips (Today / Yesterday / … / Day-6)
 *   - Event ribbon: red bars = personnel/AI, amber bars = motion-only
 *   - 24h timeline with recording buckets + motion overlay
 *   - Click anywhere on the timeline → play the matching 60s bucket
 *
 * The fisheye chip scroller that existed in v1.7.0 has been removed
 * per design feedback: the event ribbon above the seekbar is a more
 * compact, glanceable visualization of the same data.
 */

const DAY_COUNT = 7; // matches Frigate's record.continuous.days retention

/** Format a Date as YYYY-MM-DD (local time). */
function dayKey(d: Date): string {
    const y = d.getFullYear();
    const m = String(d.getMonth() + 1).padStart(2, "0");
    const day = String(d.getDate()).padStart(2, "0");
    return `${y}-${m}-${day}`;
}

/** Format a Date as a short label (今天 / 昨天 / 周X / MM-DD). */
function dayLabel(d: Date, today: Date): string {
    const diffDays = Math.round(
        (new Date(today.getFullYear(), today.getMonth(), today.getDate()).getTime() -
            new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()) /
            86_400_000,
    );
    if (diffDays === 0) return "今天";
    if (diffDays === 1) return "昨天";
    if (diffDays === 2) return "前天";
    // Day-of-week for older days.
    const weekDays = ["周日", "周一", "周二", "周三", "周四", "周五", "周六"];
    if (diffDays < 7) return weekDays[d.getDay()];
    return `${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}

/** Start-of-day unix seconds (local TZ). */
function dayStartUnix(d: Date): number {
    return Math.floor(new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime() / 1000);
}

/** Format a unix seconds timestamp as HH:MM:SS (local TZ). */
function formatHMS(unix: number): string {
    const d = new Date(unix * 1000);
    const pad = (n: number) => String(n).padStart(2, "0");
    return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

/** Format a duration (seconds) as H:MM:SS or M:SS. */
function formatDuration(sec: number): string {
    if (!Number.isFinite(sec) || sec < 0) return "--";
    const s = Math.floor(sec % 60);
    const m = Math.floor(sec / 60) % 60;
    const h = Math.floor(sec / 3600);
    const pad = (n: number) => n.toString().padStart(2, "0");
    return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`;
}

export interface RecordingTimelineProps {
    cameraId: number;
    /** Unix seconds. When set, auto-selects the matching day + seeks. */
    targetTime?: number;
    /** DOM element to render the video surface into (via portal).
     *  When provided, the <video> + custom controls overlay are
     *  portaled into this element, merging the playback surface
     *  with the parent's video area. */
    videoPortalTarget?: HTMLElement | null;
}

/**
 * The 24-hour timeline is divided into 60s buckets. Each bucket is
 * either "has recording" (Frigate wrote a segment in that minute)
 * or "no recording" (gap). We render the buckets as a horizontal bar
 * where recorded minutes are highlighted and gaps are dimmed.
 *
 * Above the seekbar we render a dedicated event ribbon: each
 * MotionRange paints a tall colored bar (red for AI/personnel,
 * amber for motion-only) so events are glanceable at a distance.
 */
export function RecordingTimeline({ cameraId, targetTime, videoPortalTarget }: RecordingTimelineProps) {
    const today = useMemo(() => new Date(), []);
    const days = useMemo(() => {
        const arr: Date[] = [];
        for (let i = 0; i < DAY_COUNT; i++) {
            const d = new Date(today);
            d.setDate(d.getDate() - i);
            arr.push(d);
        }
        return arr;
    }, [today]);

    // Resolve initial day from targetTime if provided.
    const initialDay = useMemo(() => {
        if (targetTime && targetTime > 0) {
            const d = new Date(targetTime * 1000);
            // Only pick from our 7-day window.
            const earliestStart = dayStartUnix(days[days.length - 1]);
            if (targetTime >= earliestStart) return d;
        }
        return days[0];
    }, [targetTime, days]);

    const [selectedDay, setSelectedDay] = useState<Date>(initialDay);
    const [allRecordings, setAllRecordings] = useState<CameraRecording[]>([]);
    const [loadingRecs, setLoadingRecs] = useState(false);
    const [recError, setRecError] = useState<string | null>(null);

    // Motion-range cache: dayKey → ranges. Loaded on demand when a
    // day is selected. The backend has a 60s TTL cache per
    // <camera>:<after>:<before>, so repeat opens are instant.
    const [motionCache, setMotionCache] = useState<Record<string, MotionRange[]>>({});
    const [loadingMotion, setLoadingMotion] = useState(false);

    // Active playback: the recording being played + the <video> state.
    const [activeRec, setActiveRec] = useState<CameraRecording | null>(null);
    const [videoUrl, setVideoUrl] = useState<string | null>(null);
    const [fetchingId, setFetchingId] = useState<number | null>(null);
    const [playError, setPlayError] = useState<string | null>(null);
    const videoRef = useRef<HTMLVideoElement>(null);
    const urlRef = useRef<string | null>(null);

    // Player state.
    const [isPlaying, setIsPlaying] = useState(false);
    const [currentTime, setCurrentTime] = useState(0);
    const [duration, setDuration] = useState(0);
    const [playbackRate, setPlaybackRate] = useState(1);
    const [showSpeed, setShowSpeed] = useState(false);

    // Gestures: double-tap ±10s, long-press 5x.
    const lastTapRef = useRef<{ time: number; x: number; side: "left" | "right" } | null>(null);
    const longPressTimerRef = useRef<number | null>(null);
    const [longPressActive, setLongPressActive] = useState(false);
    const savedRateRef = useRef<number>(1);

    // ----- Load recordings (7-day window, once per camera) -----
    const loadRecordings = useCallback(async () => {
        setLoadingRecs(true);
        setRecError(null);
        try {
            // The backend already returns 7 days of recordings.
            const list = await listRecordings(cameraId, 10000);
            setAllRecordings(list);
        } catch (e) {
            setRecError(e instanceof Error ? e.message : String(e));
        } finally {
            setLoadingRecs(false);
        }
    }, [cameraId]);

    useEffect(() => {
        void loadRecordings();
    }, [loadRecordings]);

    // Cleanup blob URL on unmount.
    useEffect(() => {
        return () => {
            if (urlRef.current) {
                URL.revokeObjectURL(urlRef.current);
                urlRef.current = null;
            }
        };
    }, []);

    // ----- Filter recordings by selected day -----
    const dayStart = dayStartUnix(selectedDay);
    const dayEnd = dayStart + 86_400; // 24h

    const dayRecordings = useMemo(() => {
        return allRecordings.filter((r) => {
            const start = Math.floor(new Date(r.start_at).getTime() / 1000);
            return start >= dayStart && start < dayEnd;
        });
    }, [allRecordings, dayStart, dayEnd]);

    // Build a Set of minute-bucket start timestamps for quick lookup.
    const recordingMinuteSet = useMemo(() => {
        const s = new Set<number>();
        for (const r of dayRecordings) {
            s.add(r.id); // r.id is already the minute-start unix
        }
        return s;
    }, [dayRecordings]);

    // ----- Load motion ranges for the selected day -----
    useEffect(() => {
        const key = dayKey(selectedDay);
        if (motionCache[key]) return; // cached
        if (loadingMotion) return;

        let cancelled = false;
        setLoadingMotion(true);
        (async () => {
            try {
                const res = await getMotionRanges(cameraId, dayStart, dayEnd);
                if (cancelled) return;
                setMotionCache((prev) => ({ ...prev, [key]: res.ranges ?? [] }));
            } catch {
                // Non-fatal: motion overlay just won't render.
                if (!cancelled) {
                    setMotionCache((prev) => ({ ...prev, [key]: [] }));
                }
            } finally {
                if (!cancelled) setLoadingMotion(false);
            }
        })();
        return () => { cancelled = true; };
    }, [selectedDay, dayStart, dayEnd, cameraId, motionCache, loadingMotion]);

    const dayMotion = motionCache[dayKey(selectedDay)] ?? [];

    // ----- targetTime handling: pick the right day + auto-play -----
    useEffect(() => {
        if (!targetTime || targetTime <= 0) return;
        const d = new Date(targetTime * 1000);
        setSelectedDay(d);
    }, [targetTime]);

    useEffect(() => {
        if (!targetTime || targetTime <= 0) return;
        if (loadingRecs || dayRecordings.length === 0) return;
        if (activeRec) return; // already playing

        const targetMinute = Math.floor(targetTime / 60) * 60;
        const match = dayRecordings.find((r) => r.id === targetMinute) ??
            dayRecordings.find((r) => Math.abs(r.id - targetMinute) < 600);
        if (match) {
            void playRecording(match, targetTime - match.id);
        }
    }, [targetTime, loadingRecs, dayRecordings, activeRec]); // eslint-disable-line react-hooks/exhaustive-deps

    // ----- Play a recording (fetch MP4 with JWT, build blob URL) -----
    async function playRecording(rec: CameraRecording, seekOffset = 0) {
        // Stop any current playback.
        if (urlRef.current) {
            URL.revokeObjectURL(urlRef.current);
            urlRef.current = null;
        }
        setVideoUrl(null);
        setActiveRec(null);
        setPlayError(null);
        setFetchingId(rec.id);

        try {
            const h = authHeaderFor();
            // Try the blob path first (full file). For 60s MP4s this
            // is ~10-30MB which is fast on LAN; on Tunnel it can take
            // a few seconds but Range support makes streaming better.
            const resp = await fetch(recordingFileUrl(cameraId, rec.id), {
                headers: h ? { [h.name]: h.value } : {},
            });
            if (!resp.ok) {
                throw new Error(`HTTP ${resp.status}${resp.statusText ? ` ${resp.statusText}` : ""}`);
            }
            const blob = await resp.blob();
            const url = URL.createObjectURL(blob);
            urlRef.current = url;
            setVideoUrl(url);
            setActiveRec(rec);
            // Seek to the requested offset once metadata loads.
            if (seekOffset > 0) {
                const v = videoRef.current;
                if (v) {
                    const onMeta = () => {
                        v.currentTime = seekOffset;
                        v.removeEventListener("loadedmetadata", onMeta);
                    };
                    v.addEventListener("loadedmetadata", onMeta);
                }
            }
        } catch (e) {
            setPlayError(e instanceof Error ? e.message : String(e));
        } finally {
            setFetchingId(null);
        }
    }

    function stopPlayback() {
        if (urlRef.current) {
            URL.revokeObjectURL(urlRef.current);
            urlRef.current = null;
        }
        setVideoUrl(null);
        setActiveRec(null);
        setIsPlaying(false);
        setCurrentTime(0);
        setDuration(0);
    }

    // ----- <video> event wiring -----
    useEffect(() => {
        const v = videoRef.current;
        if (!v) return;
        const onPlay = () => setIsPlaying(true);
        const onPause = () => setIsPlaying(false);
        const onTime = () => setCurrentTime(v.currentTime);
        const onDur = () => setDuration(v.duration || 0);
        const onEnd = () => {
            // Auto-advance to the next recording bucket.
            if (!activeRec) return;
            const next = dayRecordings.find((r) => r.id > activeRec.id);
            if (next) void playRecording(next);
        };
        v.addEventListener("play", onPlay);
        v.addEventListener("pause", onPause);
        v.addEventListener("timeupdate", onTime);
        v.addEventListener("durationchange", onDur);
        v.addEventListener("ended", onEnd);
        return () => {
            v.removeEventListener("play", onPlay);
            v.removeEventListener("pause", onPause);
            v.removeEventListener("timeupdate", onTime);
            v.removeEventListener("durationchange", onDur);
            v.removeEventListener("ended", onEnd);
        };
    }, [videoUrl, activeRec, dayRecordings]);

    // Apply playback rate changes to the video element.
    useEffect(() => {
        if (videoRef.current) videoRef.current.playbackRate = playbackRate;
    }, [playbackRate, videoUrl]);

    // ----- Seekbar click → play the matching 60s bucket -----
    const seekBarRef = useRef<HTMLDivElement>(null);
    function seekToHour(hourFraction: number) {
        const targetUnix = dayStart + Math.floor(hourFraction * 86_400);
        const targetMinute = Math.floor(targetUnix / 60) * 60;
        const match = dayRecordings.find((r) => r.id === targetMinute) ??
            dayRecordings.find((r) => Math.abs(r.id - targetMinute) < 600);
        if (match) {
            void playRecording(match, targetUnix - match.id);
        }
    }

    function onSeekBarClick(e: React.MouseEvent<HTMLDivElement>) {
        const rect = seekBarRef.current?.getBoundingClientRect();
        if (!rect) return;
        const x = e.clientX - rect.left;
        const fraction = x / rect.width;
        seekToHour(fraction);
    }

    // ----- Within-recording seek (drag inside the playing bucket) -----
    function onWithinRecSeek(e: React.MouseEvent<HTMLDivElement>) {
        const v = videoRef.current;
        if (!v || !duration) return;
        const rect = e.currentTarget.getBoundingClientRect();
        const x = e.clientX - rect.left;
        const fraction = Math.max(0, Math.min(1, x / rect.width));
        v.currentTime = fraction * duration;
    }

    // ----- Gestures: double-tap ±10s, long-press 5x -----
    function onVideoClick(e: React.MouseEvent<HTMLVideoElement>) {
        const v = videoRef.current;
        if (!v) return;
        const rect = v.getBoundingClientRect();
        const x = e.clientX - rect.left;
        const side: "left" | "right" = x < rect.width / 2 ? "left" : "right";
        const now = Date.now();
        const last = lastTapRef.current;
        if (last && now - last.time < 300 && last.side === side) {
            // Double-tap: ±10s
            const delta = side === "left" ? -10 : 10;
            v.currentTime = Math.max(0, Math.min((duration || 0), v.currentTime + delta));
            lastTapRef.current = null;
        } else {
            lastTapRef.current = { time: now, x, side };
            // Single-tap → toggle play/pause after a short delay
            // (so we don't toggle on the first tap of a double-tap).
            window.setTimeout(() => {
                const l = lastTapRef.current;
                if (l && l.time === now) {
                    if (v.paused) v.play().catch(() => undefined);
                    else v.pause();
                    lastTapRef.current = null;
                }
            }, 280);
        }
    }

    function onVideoMouseDown(_e: React.MouseEvent<HTMLVideoElement>) {
        if (longPressTimerRef.current) window.clearTimeout(longPressTimerRef.current);
        longPressTimerRef.current = window.setTimeout(() => {
            // Long-press → 5x speed while held.
            const v = videoRef.current;
            if (!v) return;
            savedRateRef.current = playbackRate;
            setLongPressActive(true);
            v.playbackRate = 5;
        }, 500);
    }

    function onVideoMouseUp() {
        if (longPressTimerRef.current) {
            window.clearTimeout(longPressTimerRef.current);
            longPressTimerRef.current = null;
        }
        if (longPressActive) {
            const v = videoRef.current;
            if (v) v.playbackRate = playbackRate;
            setLongPressActive(false);
        }
    }

    useEffect(() => {
        return () => {
            if (longPressTimerRef.current) window.clearTimeout(longPressTimerRef.current);
        };
    }, []);

    // ----- Render helpers -----
    // 24-hour timeline: render 1440 minute-buckets as a horizontal bar.
    // Each bucket is 60s/86400s = 0.0694% of the bar width.
    const minuteBuckets = useMemo(() => {
        const buckets: { startUnix: number; hasRec: boolean; motion?: MotionRange[] }[] = [];
        for (let m = 0; m < 1440; m++) {
            const startUnix = dayStart + m * 60;
            const hasRec = recordingMinuteSet.has(startUnix);
            const motion = dayMotion.filter(
                (r) => r.start >= startUnix && r.start < startUnix + 60,
            );
            buckets.push({ startUnix, hasRec, motion: motion.length > 0 ? motion : undefined });
        }
        return buckets;
    }, [dayStart, recordingMinuteSet, dayMotion]);

    // Active recording position on the 24h timeline (for the playhead).
    const playheadFraction = activeRec
        ? (activeRec.id - dayStart + currentTime) / 86_400
        : null;

    // Event counts for the ribbon header.
    const aiEventCount = dayMotion.filter((r) => r.peak_objects > 0).length;
    const motionEventCount = dayMotion.length - aiEventCount;

    // ----- Video surface (portaled into LiveVideo's main video area) -----
    // Rendered via createPortal so live and playback share the same
    // physical video area. The parent provides the target element.
    const videoSurface = (
        <>
            {videoUrl && activeRec ? (
                <>
                    <video
                        ref={videoRef}
                        src={videoUrl}
                        autoPlay
                        playsInline
                        className="h-full w-full object-contain cursor-pointer"
                        onClick={onVideoClick}
                        onMouseDown={onVideoMouseDown}
                        onMouseUp={onVideoMouseUp}
                        onMouseLeave={onVideoMouseUp}
                    />
                    {/* Long-press 5x indicator */}
                    {longPressActive && (
                        <div className="absolute right-2 top-2 rounded-md bg-black/60 px-1.5 py-0.5 text-[10px] font-semibold text-amber-300 backdrop-blur-md">
                            5x ▶▶
                        </div>
                    )}
                    {/* Bottom gradient + custom controls */}
                    <div className="absolute inset-x-0 bottom-0 bg-gradient-to-t from-black/80 to-transparent p-2 pt-6">
                        {/* Within-recording seekbar */}
                        <div
                            className="group relative h-1.5 cursor-pointer rounded-full bg-white/20"
                            onClick={onWithinRecSeek}
                        >
                            <div
                                className="absolute inset-y-0 left-0 rounded-full bg-[rgb(var(--accent-primary))]"
                                style={{ width: `${duration ? (currentTime / duration) * 100 : 0}%` }}
                            />
                            <div
                                className="absolute top-1/2 h-3 w-3 -translate-y-1/2 rounded-full bg-white shadow-md opacity-0 group-hover:opacity-100 transition-opacity"
                                style={{ left: `${duration ? (currentTime / duration) * 100 : 0}%`, transform: "translate(-50%, -50%)" }}
                            />
                        </div>
                        {/* Buttons + time + speed */}
                        <div className="mt-1 flex items-center gap-2 text-[10px] text-white">
                            <button
                                type="button"
                                onClick={() => {
                                    const v = videoRef.current;
                                    if (!v) return;
                                    if (v.paused) v.play().catch(() => undefined);
                                    else v.pause();
                                }}
                                className="flex h-6 w-6 items-center justify-center rounded-md bg-white/10 hover:bg-white/20 transition-colors"
                                aria-label={isPlaying ? "暂停" : "播放"}
                            >
                                {isPlaying ? <Pause size={11} /> : <Play size={11} />}
                            </button>
                            <button
                                type="button"
                                onClick={() => {
                                    const v = videoRef.current;
                                    if (!v) return;
                                    v.currentTime = Math.max(0, v.currentTime - 10);
                                }}
                                className="flex h-6 w-6 items-center justify-center rounded-md bg-white/10 hover:bg-white/20 transition-colors"
                                aria-label="后退 10 秒"
                                title="后退 10 秒（双击左侧亦可）"
                            >
                                <SkipBack size={11} />
                            </button>
                            <button
                                type="button"
                                onClick={() => {
                                    const v = videoRef.current;
                                    if (!v) return;
                                    v.currentTime = Math.min(duration, v.currentTime + 10);
                                }}
                                className="flex h-6 w-6 items-center justify-center rounded-md bg-white/10 hover:bg-white/20 transition-colors"
                                aria-label="前进 10 秒"
                                title="前进 10 秒（双击右侧亦可）"
                            >
                                <SkipForward size={11} />
                            </button>
                            <span className="font-mono tabular-nums">
                                {formatDuration(currentTime)} / {formatDuration(duration)}
                            </span>
                            <span className="ml-1 truncate text-white/60">
                                {formatHMS(activeRec.id)} · {Math.floor(activeRec.duration_seconds)}s
                            </span>
                            <div className="ml-auto relative">
                                <button
                                    type="button"
                                    onClick={() => setShowSpeed((v) => !v)}
                                    className={cn(
                                        "inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 transition-colors",
                                        showSpeed || playbackRate !== 1
                                            ? "bg-[rgb(var(--accent-primary)/0.4)] text-white"
                                            : "bg-white/10 hover:bg-white/20 text-white/80",
                                    )}
                                    title="播放速度"
                                >
                                    <Gauge size={10} />
                                    {playbackRate}x
                                </button>
                                {showSpeed && (
                                    <div className="absolute bottom-full right-0 mb-1 rounded-lg glass-strong p-1 shadow-lg z-10">
                                        <div className="flex flex-col gap-0.5">
                                            {[0.5, 1, 1.5, 2, 3, 5].map((r) => (
                                                <button
                                                    key={r}
                                                    type="button"
                                                    onClick={() => {
                                                        setPlaybackRate(r);
                                                        setShowSpeed(false);
                                                    }}
                                                    className={cn(
                                                        "rounded px-2 py-0.5 text-left text-[10px] transition-colors",
                                                        playbackRate === r
                                                            ? "bg-[rgb(var(--accent-primary)/0.3)] text-white"
                                                            : "text-fg-muted hover:bg-[rgb(var(--bg-subtle)/0.5)] hover:text-fg",
                                                    )}
                                                >
                                                    {r}x
                                                </button>
                                            ))}
                                        </div>
                                    </div>
                                )}
                            </div>
                        </div>
                    </div>
                </>
            ) : fetchingId !== null ? (
                <div className="absolute inset-0 flex flex-col items-center justify-center text-slate-300">
                    <Loader2 className="mb-2 h-6 w-6 animate-spin" />
                    <span className="text-xs">加载录像中…</span>
                </div>
            ) : dayRecordings.length === 0 ? (
                <div className="absolute inset-0 flex flex-col items-center justify-center text-slate-400">
                    <AlertTriangle className="mb-2 h-6 w-6 text-slate-500" />
                    <span className="text-xs">{loadingRecs ? "加载中…" : "当日无录像"}</span>
                </div>
            ) : (
                <div className="absolute inset-0 flex flex-col items-center justify-center text-slate-400">
                    <Play className="mb-2 h-6 w-6 opacity-50" />
                    <span className="text-xs">点击下方时间轴开始播放</span>
                </div>
            )}
        </>
    );

    // ----- Render -----
    return (
        <>
            {/* Video surface — portaled into LiveVideo's main video area.
             * When no target is provided (standalone usage), we skip
             * rendering the video surface entirely. */}
            {videoPortalTarget
                ? createPortal(videoSurface, videoPortalTarget)
                : null}

            <div className="space-y-2 px-4 pb-3 pt-1">
                {/* Errors */}
                {recError && (
                    <div className="glass bg-[rgb(var(--accent-danger)/0.1)] text-[rgb(var(--accent-danger))] rounded-lg px-2.5 py-1.5 text-[11px]">
                        录像加载失败：{recError}
                    </div>
                )}
                {playError && (
                    <div className="glass bg-[rgb(var(--accent-danger)/0.1)] text-[rgb(var(--accent-danger))] rounded-lg px-2.5 py-1.5 text-[11px]">
                        播放失败：{playError}
                    </div>
                )}

                {/* Day picker */}
                <div className="flex items-center gap-1.5 overflow-x-auto pb-1">
                    <button
                        type="button"
                        onClick={loadRecordings}
                        disabled={loadingRecs}
                        className="shrink-0 inline-flex items-center gap-1 rounded-lg glass-subtle px-2 py-1 text-[10px] text-fg-muted hover:text-fg transition-colors disabled:opacity-50"
                        title="刷新录像列表"
                    >
                        <RefreshCw size={10} className={loadingRecs ? "animate-spin" : ""} />
                        刷新
                    </button>
                    {days.map((d) => {
                        const active = dayKey(d) === dayKey(selectedDay);
                        return (
                            <button
                                key={dayKey(d)}
                                type="button"
                                onClick={() => { stopPlayback(); setSelectedDay(d); }}
                                className={cn(
                                    "shrink-0 rounded-lg px-2.5 py-1 text-[10px] font-medium transition-all",
                                    active
                                        ? "bg-[rgb(var(--accent-primary)/0.2)] text-[rgb(var(--accent-primary))] ring-1 ring-inset ring-[rgb(var(--accent-primary)/0.4)]"
                                        : "glass-subtle text-fg-muted hover:text-fg hover:bg-[rgb(var(--bg-subtle)/0.5)]",
                                )}
                            >
                                {dayLabel(d, today)}
                            </button>
                        );
                    })}
                </div>

                {/* 24-hour timeline SeekBar with event ribbon + motion overlay */}
                <div className="space-y-1">
                    <div className="flex items-center justify-between text-[10px] uppercase tracking-wider text-fg-subtle">
                        <span>24 小时时间轴</span>
                        <span className="text-fg-muted">
                            {dayRecordings.length} 分钟录像 · {dayMotion.length} 段活动
                            {loadingMotion && <Loader2 size={10} className="ml-1 inline animate-spin" />}
                        </span>
                    </div>

                    {/* Event ribbon — prominent markers for motion/AI events.
                     * Each MotionRange becomes a tall colored bar so events
                     * are glanceable at a distance. Red = personnel/AI,
                     * amber = motion-only (scene change). */}
                    {dayMotion.length > 0 && (
                        <div className="relative h-3.5 w-full overflow-hidden rounded-md glass-subtle">
                            {dayMotion.map((r, i) => {
                                const startFrac = (r.start - dayStart) / 86_400;
                                const endFrac = Math.min(1, (r.start + r.duration - dayStart) / 86_400);
                                const hasAI = r.peak_objects > 0;
                                return (
                                    <div
                                        key={`${r.start}-${i}`}
                                        className={cn(
                                            "absolute inset-y-0.5 rounded-sm transition-all",
                                            hasAI
                                                ? "bg-[rgb(var(--accent-danger))]"
                                                : "bg-[rgb(var(--accent-warm))]",
                                        )}
                                        style={{
                                            left: `${startFrac * 100}%`,
                                            width: `${Math.max(0.8, (endFrac - startFrac) * 100)}%`,
                                        }}
                                        title={`${formatHMS(r.start)} · ${formatDuration(r.duration)} · ${hasAI ? "人员活动" : "画面变动"} · score ${r.motion_score} · ${r.segment_count} segments${hasAI ? ` · ${r.peak_objects} objects` : ""}`}
                                    />
                                );
                            })}
                        </div>
                    )}

                    {/* SeekBar */}
                    <div
                        ref={seekBarRef}
                        onClick={onSeekBarClick}
                        className="relative h-8 w-full cursor-pointer overflow-hidden rounded-md glass-subtle"
                        title="点击播放对应时间点的录像"
                    >
                        {/* Hour grid lines */}
                        <div className="absolute inset-0 flex">
                            {Array.from({ length: 24 }).map((_, h) => (
                                <div
                                    key={h}
                                    className="flex-1 border-r border-[rgb(var(--border)/0.2)]"
                                />
                            ))}
                        </div>
                        {/* Recording buckets (highlighted) */}
                        <div className="absolute inset-0 flex">
                            {minuteBuckets.map((b) => (
                                <div
                                    key={b.startUnix}
                                    className="flex-1 relative"
                                    style={{ minWidth: "0.5px" }}
                                >
                                    {b.hasRec && (
                                        <div className="absolute inset-0 bg-[rgb(var(--accent-primary)/0.35)]" />
                                    )}
                                    {b.motion && b.motion.length > 0 && (
                                        <div
                                            className={cn(
                                                "absolute inset-0",
                                                b.motion.some((r) => r.peak_objects > 0)
                                                    ? "bg-[rgb(var(--accent-danger)/0.6)]"
                                                    : "bg-[rgb(var(--accent-warm)/0.55)]",
                                            )}
                                        />
                                    )}
                                </div>
                            ))}
                        </div>
                        {/* Playhead */}
                        {playheadFraction !== null && playheadFraction >= 0 && playheadFraction <= 1 && (
                            <div
                                className="absolute inset-y-0 w-0.5 bg-white shadow-md"
                                style={{ left: `${playheadFraction * 100}%` }}
                            />
                        )}
                        {/* Hour labels */}
                        <div className="pointer-events-none absolute inset-x-0 bottom-0 flex justify-between px-1 text-[8px] text-fg-subtle">
                            <span>00</span>
                            <span>06</span>
                            <span>12</span>
                            <span>18</span>
                            <span>24</span>
                        </div>
                    </div>

                    {/* Legend */}
                    <div className="flex items-center gap-3 text-[9px] text-fg-subtle">
                        <span className="inline-flex items-center gap-1">
                            <span className="inline-block h-2 w-2 rounded-sm bg-[rgb(var(--accent-danger))]" />
                            人员活动
                            {aiEventCount > 0 && <span className="text-fg-muted">({aiEventCount})</span>}
                        </span>
                        <span className="inline-flex items-center gap-1">
                            <span className="inline-block h-2 w-2 rounded-sm bg-[rgb(var(--accent-warm))]" />
                            画面变动
                            {motionEventCount > 0 && <span className="text-fg-muted">({motionEventCount})</span>}
                        </span>
                        <span className="inline-flex items-center gap-1">
                            <span className="inline-block h-2 w-2 rounded-sm bg-[rgb(var(--accent-primary)/0.35)]" />
                            录像段
                        </span>
                    </div>
                </div>

                {/* Hint footer */}
                <div className="flex flex-wrap items-center gap-2 text-[9px] text-fg-subtle">
                    <Badge variant="outline" className="text-[9px]">双击</Badge>
                    <span>左右两侧 ±10s</span>
                    <Badge variant="outline" className="text-[9px]">长按</Badge>
                    <span>5x 倍速</span>
                    <Badge variant="outline" className="text-[9px]">点击时间轴</Badge>
                    <span>跳转到对应时间</span>
                </div>
            </div>
        </>
    );
}

export default RecordingTimeline;
