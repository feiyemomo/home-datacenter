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

/** Update the output codec for a camera. Admin-only. */
export async function updateCodec(
    id: number,
    codec: "passthrough" | "h264" | "h265",
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

export async function getIceConfig(): Promise<IceConfig> {
    const { data } = await client.get<IceConfig>("/cameras/ice");
    return data;
}
