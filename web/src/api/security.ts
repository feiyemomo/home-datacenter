import client from "./client";
import type { SecurityGuardState, SecurityMode } from "@/types";

/**
 * Get current global home security arming mode.
 * GET /api/v1/security/guard
 */
export async function getSecurityGuard(): Promise<SecurityGuardState> {
    const { data } = await client.get<SecurityGuardState>("/security/guard");
    return data;
}

/**
 * Update global home security arming mode.
 * PUT /api/v1/security/guard
 */
export async function setSecurityGuard(mode: SecurityMode): Promise<SecurityGuardState> {
    const { data } = await client.put<SecurityGuardState>("/security/guard", { mode });
    return data;
}
