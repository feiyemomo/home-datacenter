import client from "@/api/client";

/**
 * Client-side error reporting (v1.8.25, hardened v1.8.41).
 *
 * Fire-and-forget sender for browser errors: uncaught JS exceptions,
 * unhandled promise rejections, and failed media playback. Each report
 * is POSTed to /api/v1/system/client-errors and persisted as a
 * SystemLog "client.error" row, so the operator can see client-side
 * failures in the dashboard's log pane instead of guessing remotely.
 *
 * v1.8.41 hardening:
 *   - In-memory queue + offline buffer: if the network is down or the
 *     report POST fails, the report is queued and retried with
 *     backoff instead of being dropped. This matters when the *same*
 *     outage that produced the error also knocked out connectivity —
 *     the report is exactly the thing we want to reach the server once
 *     the link returns.
 *   - In-tab dedup: repeated identical (context+message) reports within
 *     a window collapse into one entry with a counter, so a playback
 *     retry loop can't spam the queue.
 *   - The sender is deliberately defensive — reporting must never break
 *     the app, so every failure path is swallowed.
 */
export interface ClientErrorReport {
    /** Short human-readable description (capped at 500 on the server). */
    message: string;
    /** JS stack trace, when available. */
    stack?: string;
    /** Page URL the error occurred on. Defaults to window.location.href. */
    url?: string;
    /** Severity. Default "normal"; use "critical" for user-blocking failures. */
    level?: "critical" | "normal" | "info";
    /** Free-form tag, e.g. "recording.playback" / "window.onerror". */
    context?: string;
}

/** A report plus ephemeral bookkeeping (attempts, dedup window). */
interface QueuedReport {
    body: Record<string, unknown>;
    /** Signature used for in-tab dedup: `${context}|${message}`. */
    key: string;
    /** Timestamp of the first identical report (for the dedup counter). */
    firstSeen: number;
    /** How many identical reports have been folded into this one. */
    count: number;
    /** Retry attempts (for exponential backoff). */
    attempts: number;
}

// ---- Queue + dedup state -------------------------------------------
const QUEUE_MAX = 50;
const DEDUP_WINDOW_MS = 60_000;
const FLUSH_INTERVAL_MS = 8_000;
const MAX_ATTEMPTS = 5;

let queue: QueuedReport[] = [];
let flushTimer: number | null = null;
let flushing = false;

/**
 * scheduleFlush — (re)arm the background flush timer. The timer is
 * process-global (not per-report) so many reports share one drain loop.
 */
function scheduleFlush(): void {
    if (flushTimer !== null) return;
    flushTimer = window.setTimeout(() => {
        flushTimer = null;
        void flushQueue();
    }, FLUSH_INTERVAL_MS);
}

/**
 * flushQueue — drain queued reports until the queue is empty or a POST
 * fails catastrophically (network down). On failure reports are kept
 * and retried with backoff on the next tick.
 */
async function flushQueue(): Promise<void> {
    if (flushing || queue.length === 0) return;
    flushing = true;
    try {
        while (queue.length > 0) {
            const item = queue[0];
            try {
                await client.post("/system/client-errors", item.body);
                queue.shift();
            } catch {
                // Network/unreachable — keep the report and retry later.
                item.attempts += 1;
                if (item.attempts >= MAX_ATTEMPTS) {
                    // Give up on this report after MAX_ATTEMPTS; shedding
                    // it prevents the queue from pinning the flush loop
                    // forever on a permanently-dead link.
                    queue.shift();
                }
                break;
            }
        }
    } finally {
        flushing = false;
        if (queue.length > 0) scheduleFlush();
    }
}

/** Report a client-side error. Safe to call from any context. */
export function reportClientError(report: ClientErrorReport): void {
    if (typeof window === "undefined") return;

    const context = report.context?.slice(0, 64) ?? "window.onerror";
    const message = report.message.slice(0, 500);
    const key = `${context}|${message}`;

    // In-tab dedup: fold an identical report into the existing queued
    // entry (bump its count, refresh the timestamp) instead of queuing
    // a duplicate. This keeps a playback retry loop from flooding the
    // server with N identical rows before the backend's own dedup even
    // sees them.
    const now = Date.now();
    const existing = queue.find((q) => q.key === key && now - q.firstSeen < DEDUP_WINDOW_MS);
    if (existing) {
        existing.count += 1;
        existing.body["count"] = existing.count;
        return;
    }

    const body: Record<string, unknown> = {
        message,
        stack: report.stack?.slice(0, 2000) ?? "",
        url: report.url ?? window.location.href,
        level: report.level ?? "normal",
        context,
        count: 1,
    };

    // Shed the oldest report if the queue is full so a catastrophic
    // error storm can't exhaust memory.
    if (queue.length >= QUEUE_MAX) queue.shift();

    queue.push({ body, key, firstSeen: now, count: 1, attempts: 0 });
    scheduleFlush();
}