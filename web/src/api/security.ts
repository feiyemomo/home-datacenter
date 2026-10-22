import client from "./client";
import type {
    DefenseOverview,
    BanEntry,
    ManualBanRequest,
    SecurityGuardState,
    SecurityMode,
    SecurityEvent,
    SecurityWhitelistItem,
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
 * Update an existing IP ban.
 * PUT /api/v1/security/ban/:ip
 */
export async function updateBan(
    ip: string,
    duration_minutes: number,
    permanent: boolean,
): Promise<{ ip: string; status: string }> {
    const { data } = await client.put<{ ip: string; status: string }>(
        `/security/ban/${encodeURIComponent(ip)}`,
        { duration_minutes, permanent },
    );
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

/**
 * List recent security events timeline.
 * GET /api/v1/security/events
 */
export async function getSecurityEvents(limit: number = 50): Promise<SecurityEvent[]> {
    const { data } = await client.get<SecurityEvent[]>(`/security/events?limit=${limit}`);
    return data;
}

/**
 * Get dynamic security whitelist.
 * GET /api/v1/security/whitelist
 */
export async function getSecurityWhitelist(): Promise<SecurityWhitelistItem[]> {
    const { data } = await client.get<SecurityWhitelistItem[]>("/security/whitelist");
    return data;
}

/**
 * Add an IP or CIDR to the dynamic security whitelist.
 * POST /api/v1/security/whitelist
 */
export async function addSecurityWhitelist(
    entry: string,
    note: string = "",
): Promise<{ entry: string; status: string }> {
    const { data } = await client.post<{ entry: string; status: string }>("/security/whitelist", { entry, note });
    return data;
}

/**
 * Remove an IP or CIDR from the dynamic security whitelist.
 * DELETE /api/v1/security/whitelist/:entry
 */
export async function deleteSecurityWhitelist(entry: string): Promise<{ entry: string; status: string }> {
    const { data } = await client.delete<{ entry: string; status: string }>(
        `/security/whitelist/${encodeURIComponent(entry)}`,
    );
    return data;
}

/**
 * Update dynamic defense policy configuration.
 * PUT /api/v1/security/config
 */
export async function updateSecurityConfig(cfg: {
    max_auth_failures: number;
    failure_window_mins: number;
    ban_duration_mins: number;
}): Promise<{ status: string }> {
    const { data } = await client.put<{ status: string }>("/security/config", cfg);
    return data;
}

