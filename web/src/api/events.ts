import client from "./client";
import type { StoredEvent } from "@/types";

/**
 * Query events with optional filters.
 * GET /api/v1/events
 */
export async function listEvents(params?: {
    page?: number;
    limit?: number;
    type?: string;
    source?: string;
    since?: string;   // RFC3339
    before?: string;  // RFC3339
}): Promise<{ items: StoredEvent[]; total: number }> {
    const q = new URLSearchParams();
    if (params?.page) q.set("page", String(params.page));
    if (params?.limit) q.set("limit", String(params.limit));
    if (params?.type) q.set("type", params.type);
    if (params?.source) q.set("source", params.source);
    if (params?.since) q.set("since", params.since);
    if (params?.before) q.set("before", params.before);
    const qs = q.toString();
    const res = await client.get(`/events${qs ? `?${qs}` : ""}`);
    return res.data;
}

/**
 * Get a single event by ID.
 * GET /api/v1/events/:id
 */
export async function getEvent(id: number): Promise<StoredEvent> {
    const res = await client.get(`/events/${id}`);
    return res.data;
}
