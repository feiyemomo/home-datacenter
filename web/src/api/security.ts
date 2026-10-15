import client from "./client";
import type {
    DefenseOverview,
    BanEntry,
    ManualBanRequest,
    SecurityGuardState,
    SecurityMode,
} from "@/types";

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

/**
 * Get intrusion defense overview, active bans, and alert status.
 * GET /api/v1/security/overview
 */
export async function getDefenseOverview(): Promise<DefenseOverview> {
    const { data } = await client.get<DefenseOverview>("/security/overview");
    return data;
}

/**
 * List actively banned IPs.
 * GET /api/v1/security/banned
 */
export async function getBannedIPs(): Promise<BanEntry[]> {
    const { data } = await client.get<BanEntry[]>("/security/banned");
    return data;
}

/**
 * Manually ban an IP.
 * POST /api/v1/security/ban
 */
export async function manualBan(req: ManualBanRequest): Promise<{ ip: string; status: string }> {
    const { data } = await client.post<{ ip: string; status: string }>("/security/ban", req);
    return data;
}

/**
 * Manually unban an IP.
 * DELETE /api/v1/security/ban/:ip
 */
export async function manualUnban(ip: string): Promise<{ ip: string; status: string }> {
    const { data } = await client.delete<{ ip: string; status: string }>(`/security/ban/${encodeURIComponent(ip)}`);
    return data;
}

/**
 * Trigger a test security alert email via QQ SMTP.
 * POST /api/v1/security/test-alert
 */
export async function testAlertEmail(): Promise<{ status: string; recipient: string }> {
    const { data } = await client.post<{ status: string; recipient: string }>("/security/test-alert");
    return data;
}
