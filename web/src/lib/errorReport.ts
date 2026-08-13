import client from "@/api/client";

/**
 * Client-side error reporting (v1.8.25).
 *
 * Fire-and-forget sender for browser errors: uncaught JS exceptions,
 * unhandled promise rejections, and failed media playback. Each report
 * is POSTed to /api/v1/system/client-errors and persisted as a
 * SystemLog "client.error" row, so the operator can see client-side
 * failures in the dashboard's log pane instead of guessing remotely.
 *
 * The sender is deliberately defensive:
 *   - local rate limit (at most 1 report per 2s per tab) so a playback
 *     retry loop or a noisy app can't spam the server;
 *   - swallows all errors — reporting must never break the app.
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

let lastSend = 0;

/** Minimum gap between two reports from the same tab. */
const MIN_INTERVAL_MS = 2000;

export function reportClientError(report: ClientErrorReport): void {
    const now = Date.now();
    if (now - lastSend < MIN_INTERVAL_MS) return;
    lastSend = now;

    const body = {
        message: report.message.slice(0, 500),
        stack: report.stack?.slice(0, 2000),
        url: report.url ?? window.location.href,
        level: report.level ?? "normal",
        context: report.context?.slice(0, 64),
    };

    // Fire-and-forget. The axios interceptor attaches the JWT; if the
    // token is missing/expired the POST fails silently — acceptable,
    // since a 401 report is itself not actionable.
    client.post("/system/client-errors", body).catch(() => {
        /* ignore — reporting must not break the app */
    });
}