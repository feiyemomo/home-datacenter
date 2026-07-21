import client from "./client";
import type { Camera, CameraRecording, IceConfig, PTZCommand, PresetEntry } from "@/types";

/**
 * Camera platform API surface.
 *
 * All routes live under /api/v1/cameras. Mutation routes
 * (POST/DELETE/PUT) require admin.
 */

export interface ListCamerasResponse {
    items: Camera[];
}

export async function listCameras(): Promise<Camera[]> {
    const { data } = await client.get<Camera[]>("/cameras");
    return data ?? [];
}

export async function getCamera(id: number): Promise<Camera> {
    const { data } = await client.get<Camera>(`/cameras/${id}`);
    return data;
}

export interface RegisterCameraPayload {
    name: string;
    vendor?: string;
    host: string;
    onvif_port?: number;
    rtsp_port?: number;
    channel_id?: number;
    username?: string;
    password: string;
    ptz?: boolean;
    audio?: boolean;
    motion?: boolean;
    profile_token?: string;
    /** Opt into ffmpeg-based H.264 transcoding (HEVC → H.264). */
    transcode?: boolean;
    /** Frigate camera name override (defaults to slugified name). */
    frigate_camera?: string;
}

export async function registerCamera(req: RegisterCameraPayload): Promise<Camera> {
    const { data } = await client.post<Camera>("/cameras", req);
    return data;
}

export async function deleteCamera(id: number): Promise<void> {
    await client.delete(`/cameras/${id}`);
}

/**
 * Update the output codec for a camera. Admin-only.
 *
 * Only "h264" is accepted — WebRTC's RTP codec registry does not
 * include H.265, so passthrough/h265 always 502 on Chrome/Edge/Firefox.
 * The backend rejects other values with 400.
 */
export async function updateCodec(
    id: number,
    codec: "h264",
): Promise<void> {
    await client.put(`/cameras/${id}/codec`, { codec });
}

export async function ptzMove(
    id: number,
    command: PTZCommand,
    speed = 0.5,
    profile_token?: string,
): Promise<void> {
    await client.post(`/cameras/${id}/ptz`, {
        command,
        speed,
        profile_token,
    });
}

export async function listPresets(id: number): Promise<PresetEntry[]> {
    const { data } = await client.get<PresetEntry[]>(
        `/cameras/${id}/presets/discover`,
    );
    return data ?? [];
}

export async function setPreset(
    id: number,
    alias: string,
    token: string,
): Promise<void> {
    await client.put(`/cameras/${id}/presets/${encodeURIComponent(alias)}`, {
        token,
    });
}

export async function deletePreset(id: number, alias: string): Promise<void> {
    await client.delete(
        `/cameras/${id}/presets/${encodeURIComponent(alias)}`,
    );
}

export async function gotoPreset(
    id: number,
    alias: string,
    speed = 0.5,
): Promise<void> {
    await client.post(`/cameras/${id}/preset/${encodeURIComponent(alias)}`, {
        speed,
    });
}

export async function setRecordingPlan(
    id: number,
    plan: {
        enabled: boolean;
        segment_seconds?: number;
        retention_days?: number;
        output_dir?: string;
        cron?: string;
    },
): Promise<void> {
    await client.put(`/cameras/${id}/recording`, plan);
}

export async function listRecordings(
    id: number,
    limit = 50,
): Promise<CameraRecording[]> {
    const { data } = await client.get<CameraRecording[]>(
        `/cameras/${id}/recordings?limit=${limit}`,
    );
    return data ?? [];
}

export async function deleteRecording(
    id: number,
    recId: number,
): Promise<void> {
    await client.delete(`/cameras/${id}/recordings/${recId}`);
}

/**
 * A motion-active time range for a camera. Returned by
 * GET /api/v1/cameras/:id/motion-ranges?after=&before=.
 *
 * The backend pre-aggregates Frigate's per-segment motion data
 * into 2s-gap-merged ranges with motion_score (intensity),
 * segment_count, and peak_objects (AI detection max objects).
 *
 *   start/end     — unix seconds (absolute)
 *   duration      — seconds
 *   motion_score  — sum of motion pixels across merged segments
 *   segment_count — number of 10s Frigate segments merged
 *   peak_objects  — max AI-detected object count in any segment
 *
 * `peak_objects > 0` is the signal the dashboard uses to color a
 * chip red (AI-detected motion) vs amber (motion-only).
 */
export interface MotionRange {
    start: number;
    end: number;
    duration: number;
    motion_score: number;
    segment_count: number;
    peak_objects: number;
}

export interface MotionRangesResponse {
    ranges: MotionRange[];
    total: number;
}

/**
 * Fetch motion-active time ranges within [after, before) for a camera.
 *
 * Used by the recording-playback SeekBar overlay (red marks at
 * positions where motion happened) and the fisheye chip scroller
 * (each chip = one motion range).
 *
 * The backend has a 60s TTL cache per <camera>:<after>:<before>,
 * so repeat opens of the same day are instant.
 */
export async function getMotionRanges(
    id: number,
    after: number,
    before: number,
): Promise<MotionRangesResponse> {
    // v1.8.2: override the global 15s axios timeout. The backend
    // chunks a 24h window into 24 hourly Frigate API requests (each
    // 1-2s), so the total can take 24-48s. The default 15s timeout
    // was silently aborting the request, causing the frontend to
    // cache null and the UI to show "0 events".
    const { data } = await client.get<MotionRangesResponse>(
        `/cameras/${id}/motion-ranges?after=${after}&before=${before}`,
        { timeout: 90000 },
    );
    return data ?? { ranges: [], total: 0 };
}

/**
 * Build the URL for a 60-second recording clip (concatenated from
 * Frigate's 10s segments server-side via ffmpeg stream copy).
 *
 * `recId` is the minute-start unix timestamp (the `id` field of a
 * CameraRecording returned by listRecordings). The endpoint streams
 * a single MP4 with Content-Length + Range support.
 *
 * The JWT cookie is sent automatically for same-origin <video> tags,
 * but for fetch()-based blob loading (used when we need byte ranges
 * or want to use MSE), attach the Authorization header manually.
 */
export function recordingFileUrl(cameraId: number, recId: number): string {
    return `/api/v1/cameras/${cameraId}/recordings/${recId}/file`;
}

export async function getIceConfig(): Promise<IceConfig> {
    const { data } = await client.get<IceConfig>("/cameras/ice");
    return data;
}

/**
 * A Frigate detection alert from GET /api/v1/cameras/alerts.
 */
export interface CameraAlert {
    id: string;
    camera_slug: string;
    camera_id?: number;
    camera_name?: string;
    label: string;
    confidence: number;
    start_time: number;
    end_time: number;
    zones?: string[];
    has_clip: boolean;
    has_snapshot: boolean;
    /** Base64-encoded small JPEG thumbnail (may be empty). */
    thumbnail?: string;
}

export interface ListAlertsResponse {
    alerts: CameraAlert[];
    total: number;
}

export async function listAlerts(limit = 20): Promise<ListAlertsResponse> {
    const { data } = await client.get<ListAlertsResponse>(
        `/cameras/alerts?limit=${limit}`,
    );
    return data ?? { alerts: [], total: 0 };
}

/**
 * Build the URL for a full-resolution snapshot of a detection event.
 * Use as the `src` of an <img> tag. The JWT cookie is sent
 * automatically because this is a same-origin GET.
 */
export function alertSnapshotUrl(eventId: string): string {
	return `/api/v1/cameras/alerts/${encodeURIComponent(eventId)}/snapshot`;
}

/**
 * Build the URL for a small JPEG thumbnail of a detection event.
 * Frigate 0.17 no longer inlines base64 thumbnails in the events
 * list, so each thumbnail is fetched on demand via this URL.
 * Use as the `src` of an <img> tag in alert lists.
 */
export function alertThumbnailUrl(eventId: string): string {
	return `/api/v1/cameras/alerts/${encodeURIComponent(eventId)}/thumbnail`;
}

/**
 * Build the URL for a single JPEG frame from the camera's live
 * stream. Used by the camera card to show a static preview before
 * the operator clicks Play — avoids spinning up a WebRTC/HLS
 * connection for every camera on the page.
 *
 * The JWT cookie is sent automatically because this is a same-
 * origin GET. Add a cache-busting query param (e.g. `?t=Date.now()`)
 * to force a fresh frame on reload.
 */
export function cameraFrameUrl(cameraId: number): string {
	return `/api/v1/cameras/${cameraId}/frame`;
}
