import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
    Activity,
    Bell,
    Camera as CameraIcon,
    Server,
    Filter,
    X,
    ChevronDown,
} from "lucide-react";
import { listEvents } from "@/api/events";
import { useWebSocket } from "@/hooks/useWebSocket";
import { cn, formatDateTime } from "@/lib/utils";
import { ApiError } from "@/api/client";
import type { StoredEvent } from "@/types";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";

// --- Event type color/icon mapping ---
const SOURCE_ICONS: Record<string, typeof CameraIcon> = {
    camera: CameraIcon,
    system: Server,
    automation: Activity,
};

const SEVERITY_COLORS: Record<string, string> = {
    info: "text-sky-600 dark:text-sky-400 bg-sky-500/10 border-sky-500/20",
    warn: "text-amber-600 dark:text-amber-400 bg-amber-500/10 border-amber-500/20",
    error: "text-rose-600 dark:text-rose-400 bg-rose-500/10 border-rose-500/20",
    critical: "text-red-600 dark:text-red-400 bg-red-500/10 border-red-500/20",
};

// Human-readable event labels
function eventLabel(ev: StoredEvent): string {
    const t = ev.type;

    // Camera events
    if (t === "camera.online") {
        const name = (ev.payload as Record<string, unknown>)?.camera_name ?? `#${(ev.payload as Record<string, unknown>)?.camera_id}`;
        return `Camera ${name} came online`;
    }
    if (t === "camera.offline") {
        const name = (ev.payload as Record<string, unknown>)?.camera_name ?? `#${(ev.payload as Record<string, unknown>)?.camera_id}`;
        return `Camera ${name} went offline`;
    }
    if (t === "camera.object.detected") {
        const obj = (ev.payload as Record<string, unknown>)?.object ?? "motion";
        const conf = (ev.payload as Record<string, unknown>)?.confidence as number;
        const name = (ev.payload as Record<string, unknown>)?.camera_name ?? "";
        let label = `Camera ${name} detected ${obj}`;
        if (conf != null) label += ` (${Math.round(conf * 100)}%)`;
        return label;
    }
    if (t === "camera.motion") {
        const obj = (ev.payload as Record<string, unknown>)?.object ?? "motion";
        const conf = (ev.payload as Record<string, unknown>)?.confidence;
        const name = (ev.payload as Record<string, unknown>)?.camera_name ?? `#${(ev.payload as Record<string, unknown>)?.camera_id}`;
        let label = `Camera ${name} detected ${obj}`;
        if (conf != null) label += ` (${Math.round(Number(conf) * 100)}%)`;
        return label;
    }
    if (t === "camera.status_changed") {
        const name = (ev.payload as Record<string, unknown>)?.camera_name ?? `#${(ev.payload as Record<string, unknown>)?.camera_id}`;
        return `Camera ${name} status changed`;
    }
    if (t === "camera.rtsp_lost") {
        const name = (ev.payload as Record<string, unknown>)?.camera_name ?? `#${(ev.payload as Record<string, unknown>)?.camera_id}`;
        return `Camera ${name} RTSP stream lost`;
    }

    // Server events
    if (t === "server.online") return "Server started";
    if (t === "server.offline") {
        const uptime = (ev.payload as Record<string, unknown>)?.uptime_seconds;
        return `Server stopped (uptime: ${uptime}s)`;
    }
    if (t === "server.heartbeat") return "Server heartbeat";

    // System events
    if (t === "system.alert") {
        const title = (ev.payload as Record<string, unknown>)?.title ?? "alert";
        return `System alert: ${title}`;
    }
    if (t === "disk.warning") {
        const path = (ev.payload as Record<string, unknown>)?.path ?? "/data";
        return `Disk warning: ${path}`;
    }

    // Device events
    if (t === "device.status") {
        const status = (ev.payload as Record<string, unknown>)?.status;
        return `Device #${(ev.payload as Record<string, unknown>)?.device_id} ${status}`;
    }

    // Automation
    if (t === "automation.fired") {
        const rule = (ev.payload as Record<string, unknown>)?.rule_name ?? (ev.payload as Record<string, unknown>)?.rule_id;
        return `Automation rule "${rule}" fired`;
    }

    // Fallback
    return t.replace(/\./g, " ");
}

// Group events by calendar date for the timeline
interface DateGroup {
    label: string;
    events: StoredEvent[];
}

// Build a Frigate snapshot URL from an event payload.
// The frigate_event_id is the raw Frigate event ID (e.g. "123456.abcdef").
// Frigate serves snapshots at /api/events/{event_id}/snapshot.jpg.
function snapshotUrl(ev: StoredEvent): string | null {
    const fid = (ev.payload as Record<string, unknown>)?.frigate_event_id as string | undefined;
    if (!fid) return null;
    return `/frigate/api/events/${encodeURIComponent(fid)}/snapshot.jpg`;
}

function groupByDate(events: StoredEvent[]): DateGroup[] {
    const today = new Date();
    today.setHours(0, 0, 0, 0);
    const yesterday = new Date(today);
    yesterday.setDate(yesterday.getDate() - 1);

    const groups = new Map<string, StoredEvent[]>();
    for (const ev of events) {
        const d = new Date(ev.timestamp);
        d.setHours(0, 0, 0, 0);
        const key = d.toISOString().slice(0, 10);
        if (!groups.has(key)) groups.set(key, []);
        groups.get(key)!.push(ev);
    }

    const result: DateGroup[] = [];
    for (const [key, evts] of groups) {
        const d = new Date(key);
        let label: string;
        if (d.getTime() === today.getTime()) {
            label = "Today";
        } else if (d.getTime() === yesterday.getTime()) {
            label = "Yesterday";
        } else {
            label = d.toLocaleDateString("en-US", { month: "short", day: "numeric", year: "numeric" });
        }
        result.push({ label, events: evts });
    }
    return result;
}

const PAGE_SIZE = 30;

export default function EventsPage() {
    const [events, setEvents] = useState<StoredEvent[]>([]);
    const [total, setTotal] = useState(0);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState<string | null>(null);

    // Filters
    const [filterType, setFilterType] = useState("");
    const [filterSource, setFilterSource] = useState("");
    const [filterTime, setFilterTime] = useState<"all" | "today" | "7d">("all");
    const [showFilters, setShowFilters] = useState(false);

    // Detail panel
    const [selected, setSelected] = useState<StoredEvent | null>(null);

    // Real-time
    const { lastMessage, subscribe, isConnected } = useWebSocket();
    const [newCount, setNewCount] = useState(0);

    // Loading more
    const loadingMore = useRef(false);

    // Refresh
    const fetchEvents = useCallback(async (append = false) => {
        setError(null);
        if (!append) setLoading(true);
        try {
            const params: Record<string, string | number> = {
                limit: PAGE_SIZE,
                page: 1,
            };
            if (filterType) params.type = filterType;
            if (filterSource) params.source = filterSource;
            if (filterTime === "today") {
                const d = new Date();
                d.setHours(0, 0, 0, 0);
                params.since = d.toISOString();
            } else if (filterTime === "7d") {
                const d = new Date();
                d.setDate(d.getDate() - 7);
                params.since = d.toISOString();
            }

            const res = await listEvents(params);
            setEvents(res.items);
            setTotal(res.total);
            setNewCount(0);
        } catch (err) {
            setError(
                err instanceof ApiError
                    ? err.message
                    : err instanceof Error
                        ? err.message
                        : "failed to load events",
            );
        } finally {
            setLoading(false);
            loadingMore.current = false;
        }
    }, [filterType, filterSource, filterTime]);

    useEffect(() => {
        fetchEvents();
    }, [fetchEvents]);

    // Subscribe to real-time events via WebSocket
    useEffect(() => {
        subscribe("*");
    }, [subscribe]);

    // Handle incoming WebSocket events
    useEffect(() => {
        if (!lastMessage) return;
        if (lastMessage.type !== "event") return;
        const topic = lastMessage.topic ?? "";

        // Convert WsMessage payload to StoredEvent shape for the UI.
        // The WsMessage wraps EventBus event payloads; the actual
        // data is inside lastMessage.payload (which varies per topic).
        const raw = (lastMessage.payload ?? {}) as Record<string, unknown>;
        const newEv: StoredEvent = {
            id: (raw.id as number) ?? Date.now(),
            type: topic,
            source: (raw.source as string) ?? "unknown",
            severity: (raw.severity as StoredEvent["severity"]) ?? "info",
            payload: (raw.data as Record<string, unknown>) ?? raw,
            status: "created",
            timestamp: (raw.timestamp as string) ?? new Date().toISOString(),
        };

        // Respect active filters
        if (filterType && newEv.type !== filterType) return;
        if (filterSource && newEv.source !== filterSource) return;

        setEvents((prev) => [newEv, ...prev]);
        setTotal((t) => t + 1);
        setNewCount((c) => c + 1);
    }, [lastMessage, filterType, filterSource]);

    // Clear new count when user scrolls to top or clicks
    function clearNew() {
        setNewCount(0);
    }

    const grouped = useMemo(() => groupByDate(events), [events]);

    // Available types/sources from current data
    const availableTypes = useMemo(() => {
        const s = new Set<string>();
        events.forEach((e) => s.add(e.type));
        return Array.from(s).sort();
    }, [events]);
    const availableSources = useMemo(() => {
        const s = new Set<string>();
        events.forEach((e) => s.add(e.source));
        return Array.from(s).sort();
    }, [events]);

    return (
        <div className="space-y-6">
            {/* Header */}
            <div className="flex flex-wrap items-center justify-between gap-3">
                <div>
                    <h2 className="text-lg font-semibold text-fg">Event Center</h2>
                    <p className="text-xs text-fg-muted">
                        {total} event{total === 1 ? "" : "s"} recorded
                        {isConnected ? " — live" : ""}
                    </p>
                </div>
                <div className="flex items-center gap-2">
                    {newCount > 0 && (
                        <Badge variant="info" className="animate-pulse cursor-pointer" onClick={clearNew}>
                            +{newCount} new
                        </Badge>
                    )}
                    <Button
                        variant="outline"
                        size="sm"
                        onClick={() => setShowFilters(!showFilters)}
                    >
                        <Filter size={14} /> Filters
                        {(filterType || filterSource || filterTime !== "all") && (
                            <Badge variant="info" className="ml-1 h-4 w-4 p-0 text-[10px] leading-4">
                                !
                            </Badge>
                        )}
                    </Button>
                </div>
            </div>

            {error && (
                <div className="rounded-md border border-rose-500/40 bg-rose-500/10 px-4 py-3 text-sm text-rose-600 dark:text-rose-300">
                    {error}
                </div>
            )}

            {/* Filters */}
            {showFilters && (
                <Card>
                    <CardContent className="p-4">
                        <div className="flex flex-wrap items-end gap-4">
                            <label className="flex flex-col gap-1">
                                <span className="text-[11px] uppercase tracking-wider text-fg-muted">Type</span>
                                <select
                                    value={filterType}
                                    onChange={(e) => setFilterType(e.target.value)}
                                    className="rounded-md border border-surface-border bg-surface-subtle px-3 py-1.5 text-sm text-fg"
                                >
                                    <option value="">All types</option>
                                    {availableTypes.map((t) => (
                                        <option key={t} value={t}>{t}</option>
                                    ))}
                                </select>
                            </label>
                            <label className="flex flex-col gap-1">
                                <span className="text-[11px] uppercase tracking-wider text-fg-muted">Source</span>
                                <select
                                    value={filterSource}
                                    onChange={(e) => setFilterSource(e.target.value)}
                                    className="rounded-md border border-surface-border bg-surface-subtle px-3 py-1.5 text-sm text-fg"
                                >
                                    <option value="">All sources</option>
                                    {availableSources.map((s) => (
                                        <option key={s} value={s}>{s}</option>
                                    ))}
                                </select>
                            </label>
                            <label className="flex flex-col gap-1">
                                <span className="text-[11px] uppercase tracking-wider text-fg-muted">Time</span>
                                <select
                                    value={filterTime}
                                    onChange={(e) => setFilterTime(e.target.value as "all" | "today" | "7d")}
                                    className="rounded-md border border-surface-border bg-surface-subtle px-3 py-1.5 text-sm text-fg"
                                >
                                    <option value="all">All time</option>
                                    <option value="today">Today</option>
                                    <option value="7d">Last 7 days</option>
                                </select>
                            </label>
                            {(filterType || filterSource || filterTime !== "all") && (
                                <Button
                                    variant="ghost"
                                    size="sm"
                                    onClick={() => { setFilterType(""); setFilterSource(""); setFilterTime("all"); }}
                                >
                                    <X size={14} /> Clear
                                </Button>
                            )}
                        </div>
                    </CardContent>
                </Card>
            )}

            {/* Main content: timeline + detail panel */}
            <div className="flex gap-6">
                {/* Timeline */}
                <div className={cn("flex-1 space-y-6", selected && "max-w-2xl")}>
                    {loading && events.length === 0 && (
                        <p className="py-10 text-center text-sm text-fg-muted">Loading events...</p>
                    )}
                    {!loading && events.length === 0 && (
                        <div className="flex flex-col items-center justify-center py-16 text-fg-muted">
                            <Bell size={40} className="mb-3 opacity-30" />
                            <p className="text-sm">No events yet</p>
                            <p className="text-xs opacity-70">Events will appear here as they occur</p>
                        </div>
                    )}

                    {grouped.map((group) => (
                        <div key={group.label}>
                            <h3 className="mb-3 text-xs font-semibold uppercase tracking-wider text-fg-muted">
                                {group.label}
                            </h3>
                            <div className="space-y-0.5">
                                {group.events.map((ev) => {
                                    const Icon = SOURCE_ICONS[ev.source] ?? Bell;
                                    const sevColor = SEVERITY_COLORS[ev.severity] ?? SEVERITY_COLORS.info;

                                    return (
                                        <div
                                            key={ev.id}
                                            onClick={() => setSelected(ev)}
                                            className={cn(
                                                "group flex cursor-pointer items-start gap-3 rounded-lg px-3 py-2 transition-colors hover:bg-surface-subtle",
                                                selected?.id === ev.id && "bg-surface-subtle ring-1 ring-inset ring-sky-500/20",
                                            )}
                                        >
                                            {/* Icon */}
                                            <div className={cn(
                                                "mt-0.5 flex h-7 w-7 shrink-0 items-center justify-center rounded-md border",
                                                sevColor,
                                            )}>
                                                <Icon size={14} />
                                            </div>

                                            {/* Content */}
                                            <div className="min-w-0 flex-1">
                                                <p className="text-sm text-fg">{eventLabel(ev)}</p>
                                                <div className="mt-0.5 flex items-center gap-2">
                                                    <span className="text-[11px] text-fg-muted">
                                                        {formatDateTime(ev.timestamp)}
                                                    </span>
                                                    <span className="text-[11px] text-fg-muted/60">·</span>
                                                    <span className="text-[11px] text-fg-muted/60 uppercase">
                                                        {ev.source}
                                                    </span>
                                                </div>
                                            </div>

                                            {/* Snapshot thumbnail for Frigate detection events */}
                                            {snapshotUrl(ev) && (
                                                <img
                                                    src={snapshotUrl(ev)!}
                                                    alt=""
                                                    className="h-8 w-12 shrink-0 rounded border border-surface-border object-cover opacity-0 group-hover:opacity-100 transition-opacity"
                                                    loading="lazy"
                                                />
                                            )}

                                            {/* Severity badge */}
                                            <Badge
                                                variant="outline"
                                                className={cn("text-[10px] uppercase", sevColor)}
                                            >
                                                {ev.severity}
                                            </Badge>

                                            <ChevronDown size={14} className="mt-0.5 text-fg-muted/40 transition-transform group-hover:text-fg-muted" />
                                        </div>
                                    );
                                })}
                            </div>
                        </div>
                    ))}

                    {events.length > 0 && events.length < total && (
                        <div className="py-4 text-center">
                            <Button
                                variant="outline"
                                size="sm"
                                disabled={loadingMore.current}
                                onClick={() => fetchEvents(true)}
                            >
                                Load more ({total - events.length} remaining)
                            </Button>
                        </div>
                    )}
                </div>

                {/* Detail panel */}
                {selected && (
                    <div className="w-80 shrink-0">
                        <Card className="sticky top-4">
                            <CardHeader className="p-4">
                                <div className="flex items-center justify-between">
                                    <CardTitle className="text-sm">Event Detail</CardTitle>
                                    <Button variant="ghost" size="icon" className="h-6 w-6" onClick={() => setSelected(null)}>
                                        <X size={14} />
                                    </Button>
                                </div>
                            </CardHeader>
                            <CardContent className="space-y-3 p-4 pt-0">
                                <div>
                                    <span className="text-[10px] uppercase text-fg-muted">Type</span>
                                    <p className="text-sm font-medium text-fg">{selected.type}</p>
                                </div>
                                <div>
                                    <span className="text-[10px] uppercase text-fg-muted">Source</span>
                                    <p className="text-sm text-fg">{selected.source}</p>
                                </div>
                                <div>
                                    <span className="text-[10px] uppercase text-fg-muted">Time</span>
                                    <p className="text-sm text-fg">{formatDateTime(selected.timestamp)}</p>
                                </div>
                                <div>
                                    <span className="text-[10px] uppercase text-fg-muted">Severity</span>
                                    <Badge variant="outline" className={cn("mt-0.5 text-[10px] uppercase", SEVERITY_COLORS[selected.severity])}>
                                        {selected.severity}
                                    </Badge>
                                </div>
                                {snapshotUrl(selected) && (
                                    <div>
                                        <span className="text-[10px] uppercase text-fg-muted">Snapshot</span>
                                        <img
                                            src={snapshotUrl(selected)!}
                                            alt="Event snapshot"
                                            className="mt-1 w-full rounded-md border border-surface-border"
                                            loading="lazy"
                                        />
                                    </div>
                                )}
                                <div>
                                    <span className="text-[10px] uppercase text-fg-muted">Status</span>
                                    <p className="text-sm text-fg capitalize">{selected.status}</p>
                                </div>
                                <div>
                                    <span className="text-[10px] uppercase text-fg-muted">Details</span>
                                    <pre className="mt-1 max-h-48 overflow-auto rounded-md bg-surface-subtle p-2 text-[11px] text-fg-muted">
                                        {JSON.stringify(selected.payload, null, 2)}
                                    </pre>
                                </div>
                            </CardContent>
                        </Card>
                    </div>
                )}
            </div>
        </div>
    );
}
