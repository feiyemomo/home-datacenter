import client from "./client";
import type {
    PublishMqttRequest,
    PublishMqttResponse,
    SystemLogListResponse,
    SystemStatus,
    User,
} from "@/types";

/**
 * Real-time system metrics for the dashboard.
 *
 * GET /api/v1/system/status
 */
export async function getSystemStatus(): Promise<SystemStatus> {
    const { data } = await client.get<SystemStatus>("/system/status");
    return data as SystemStatus;
}

/**
 * Current user identity.
 *
 * GET /api/v1/user/me
 */
export async function getCurrentUser(): Promise<User> {
    const { data } = await client.get<User>("/user/me");
    return data as User;
}

/**
 * Publish a message to an MQTT topic. Admin only.
 *
 * POST /api/v1/mqtt/publish
 */
export async function publishMqtt(
    req: PublishMqttRequest,
): Promise<PublishMqttResponse> {
    const { data } = await client.post<PublishMqttResponse>("/mqtt/publish", req);
    return data as PublishMqttResponse;
}

/**
 * List system log entries, newest first.
 *
 * GET /api/v1/system/logs
 */
export async function listSystemLogs(
    limit = 50,
    offset = 0,
): Promise<SystemLogListResponse> {
    const { data } = await client.get<SystemLogListResponse>("/system/logs", {
        params: { limit, offset },
    });
    return data as SystemLogListResponse;
}

export interface CleanCacheResponse {
    reclaimed_bytes: number;
    deleted_files: number;
}

/**
 * Purge recordings transcode cache. Admin only.
 *
 * POST /api/v1/system/clean-cache
 */
export async function cleanTranscodeCache(): Promise<CleanCacheResponse> {
    const { data } = await client.post<CleanCacheResponse>("/system/clean-cache");
    return data;
}

export interface StorageConfig {
    quota_gb: number;
    retention_days: number;
    reduced_retention_days: number;
    archive_schedule_hour: number;
    archive_min_age_days: number;
    archive_retention_days: number;
    last_sync_timestamp: number;
    last_sync_ok: boolean;
    last_sync_error: string;
}

export async function getStorageConfig(): Promise<StorageConfig> {
    const { data } = await client.get<StorageConfig>("/system/storage/config");
    return data;
}

export async function updateStorageConfig(cfg: Partial<StorageConfig>): Promise<StorageConfig> {
    const { data } = await client.put<StorageConfig>("/system/storage/config", cfg);
    return data;
}

export async function triggerArchiveSync(): Promise<{ message: string; ts: number }> {
    const { data } = await client.post<{ message: string; ts: number }>("/system/storage/sync-archive");
    return data;
}
