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
