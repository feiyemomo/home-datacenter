import client from "./client";
import type { Device, DeviceListResponse } from "@/types";

/**
 * List devices visible to the current user.
 * Admin -> all devices; non-admin -> only their own.
 *
 * GET /api/v1/device/list
 */
export async function listDevices(): Promise<Device[]> {
    const { data } = await client.get<DeviceListResponse>("/device/list");
    return data?.devices ?? [];
}

/**
 * Revoke a device (soft delete). Idempotent.
 *
 * DELETE /api/v1/device/:id
 */
export async function revokeDevice(deviceId: number): Promise<void> {
    await client.delete(`/device/${deviceId}`);
}

/**
 * Permanently delete a device row (hard delete).
 * Only works on already-revoked devices.
 *
 * DELETE /api/v1/device/:id/purge
 */
export async function deleteDevice(deviceId: number): Promise<void> {
    await client.delete(`/device/${deviceId}/purge`);
}
