/**
 * cn: tiny className combiner.
 * Filters out falsy values and joins the rest with a space.
 * Deliberately dependency-free (no clsx/tailwind-merge).
 */
export function cn(...classes: (string | false | null | undefined)[]): string {
    return classes.filter(Boolean).join(" ");
}

/**
 * Decode a JWT payload without verifying the signature.
 * Returns null if the token is malformed.
 *
 * The backend signs with HS256 and the claims we care about
 * (user_id, device_id, exp, iat) live in the payload segment.
 */
export function decodeJwtPayload<T = Record<string, unknown>>(
    token: string,
): T | null {
    try {
        const parts = token.split(".");
        if (parts.length !== 3) return null;
        // base64url -> base64
        const base64 = parts[1].replace(/-/g, "+").replace(/_/g, "/");
        // Pad to length multiple of 4
        const padded = base64.padEnd(
            base64.length + ((4 - (base64.length % 4)) % 4),
            "=",
        );
        const json = decodeURIComponent(
            atob(padded)
                .split("")
                .map((c) => "%" + ("00" + c.charCodeAt(0).toString(16)).slice(-2))
                .join(""),
        );
        return JSON.parse(json) as T;
    } catch {
        return null;
    }
}

/**
 * Format an uptime (seconds) as "Xd Yh Zm".
 * Days/hours/minutes are dropped when zero on the leading side,
 * but minutes are always shown.
 */
export function formatUptime(totalSeconds: number): string {
    const secs = Math.max(0, Math.floor(totalSeconds));
    const d = Math.floor(secs / 86400);
    const h = Math.floor((secs % 86400) / 3600);
    const m = Math.floor((secs % 3600) / 60);
    const parts: string[] = [];
    if (d > 0) parts.push(`${d}d`);
    if (h > 0 || d > 0) parts.push(`${h}h`);
    parts.push(`${m}m`);
    return parts.join(" ");
}

/**
 * Convert a backend datetime value to a display string.
 *
 * The backend NullTime.MarshalJSON renders either `null` or an
 * RFC3339 string. We also defensively accept the raw {Time, Valid}
 * struct shape in case the wire format changes.
 *
 * Accepts: string | { Time: string; Valid: boolean } | null | undefined
 */
export function formatDateTime(
    value: unknown,
    fallback = "—",
): string {
    if (value == null) return fallback;

    let raw: string | undefined;
    if (typeof value === "string") {
        raw = value;
    } else if (
        typeof value === "object" &&
        value !== null &&
        "Valid" in value &&
        "Time" in value
    ) {
        const v = value as { Time: string; Valid: boolean };
        if (!v.Valid) return fallback;
        raw = v.Time;
    }

    if (!raw) return fallback;

    // The backend emits two formats:
    //   - RFC3339 ("2026-06-28T12:00:00Z") from NullTime.MarshalJSON
    //   - "2006-01-02 15:04:05" from the device handler's .Format(...)
    const normalized = raw.includes("T") ? raw : raw.replace(" ", "T");
    const dt = new Date(normalized);
    if (Number.isNaN(dt.getTime())) return raw;
    return dt.toLocaleString(undefined, {
        year: "numeric",
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
        second: "2-digit",
    });
}

/**
 * Human-readable countdown to a future epoch (ms).
 * Returns "expired" if the timestamp is in the past.
 */
export function formatCountdown(targetEpochMs: number): string {
    const diff = targetEpochMs - Date.now();
    if (diff <= 0) return "expired";
    const secs = Math.floor(diff / 1000);
    const d = Math.floor(secs / 86400);
    const h = Math.floor((secs % 86400) / 3600);
    const m = Math.floor((secs % 3600) / 60);
    const s = secs % 60;
    const parts: string[] = [];
    if (d > 0) parts.push(`${d}d`);
    if (h > 0 || d > 0) parts.push(`${h}h`);
    if (m > 0 || h > 0 || d > 0) parts.push(`${m}m`);
    parts.push(`${s}s`);
    return parts.join(" ");
}
