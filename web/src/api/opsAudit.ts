import client from "./client";

export interface AgentAuditRun {
    id: number;
    run_id: string;
    correlation_id: string;
    household_id: number;
    trigger_source: string;
    actor_type: string;
    actor_id: string;
    action: string;
    target: string;
    component: string;
    decision: string;
    risk_level: string;
    status: string;
    proposal_id?: string;
    approval_id?: string;
    operation_id?: string;
    summary?: string;
    error_code?: string;
    is_offline: boolean;
    synced: boolean;
    event_count: number;
    first_seq: number;
    last_seq: number;
    last_audit_id?: string;
    last_entry_hash?: string;
    integrity_status: string;
    started_at: string;
    started_at_ms: number;
    ended_at?: string | null;
    ended_at_ms: number;
    duration_ms: number;
}

export interface AgentAuditEvent {
    id: number;
    stream_id: string;
    seq: number;
    audit_id: string;
    run_id: string;
    parent_event_id?: string;
    correlation_id: string;
    household_id: number;
    trigger_source: string;
    actor_type: string;
    actor_id: string;
    event_type: string;
    action: string;
    target: string;
    component: string;
    decision: string;
    risk_level: string;
    policy_version: string;
    proposal_id?: string;
    approval_id?: string;
    operation_id?: string;
    fencing_generation: number;
    before_state?: string;
    after_state?: string;
    parameters_digest?: string;
    parameters_redacted?: string;
    result?: string;
    error_code?: string;
    duration_ms: number;
    evidence?: string;
    created_at: string;
    created_at_ms: number;
    is_offline: boolean;
    synced: boolean;
    origin_stream_id?: string;
    origin_seq?: number;
    origin_entry_hash?: string;
    previous_hash: string;
    entry_hash: string;
    hmac_signature: string;
}

export interface AuditVerifyResult {
    valid: boolean;
    integrity_status: string;
    stream_id: string;
    checked_events: number;
    checked_checkpoints: number;
    head_seq: number;
    tail_seq: number;
    tail_entry_hash?: string;
    last_checkpoint_id?: string;
    last_checkpoint_seq?: number;
    triggers_intact: boolean;
    broken_at_seq?: number;
    failure_reason?: string;
    key_ids_used?: string[];
    limitation_note: string;
    verified_at: string;
}

export interface AuditRunDetail {
    run: AgentAuditRun;
    events: AgentAuditEvent[];
    integrity: AuditVerifyResult;
}

export interface ListRunsParams {
    page?: number;
    page_size?: number;
    trigger_source?: string;
    component?: string;
    status?: string;
    risk_level?: string;
    is_offline?: boolean;
    q?: string;
}

export interface ListRunsResponse {
    items: AgentAuditRun[];
    total: number;
    page: number;
    page_size: number;
}

export async function listAuditRuns(params: ListRunsParams = {}): Promise<ListRunsResponse> {
    const query: Record<string, string | number | boolean> = {};
    if (params.page) query.page = params.page;
    if (params.page_size) query.page_size = params.page_size;
    if (params.trigger_source) query.trigger_source = params.trigger_source;
    if (params.component) query.component = params.component;
    if (params.status) query.status = params.status;
    if (params.risk_level) query.risk_level = params.risk_level;
    if (params.is_offline !== undefined) query.is_offline = params.is_offline;
    if (params.q) query.q = params.q;

    const { data } = await client.get<ListRunsResponse>("/ops/audit/runs", { params: query });
    return data;
}

export async function getAuditRunDetail(runId: string): Promise<AuditRunDetail> {
    const { data } = await client.get<AuditRunDetail>(`/ops/audit/runs/${encodeURIComponent(runId)}`);
    return data;
}

export async function verifyAuditChain(verifyTriggers = true, verifyCheckpoints = true): Promise<AuditVerifyResult> {
    const { data } = await client.post<AuditVerifyResult>("/ops/audit/verify", {
        verify_triggers: verifyTriggers,
        verify_checkpoints: verifyCheckpoints,
    });
    return data;
}

export async function exportAuditFile(format: "json" | "csv", filters: { run_id?: string; component?: string; trigger_source?: string } = {}): Promise<void> {
    try {
        const res = await client.get<Blob>("/ops/audit/export", {
            params: { format, ...filters },
            responseType: "blob",
        });
        const blob = res.data instanceof Blob ? res.data : new Blob([JSON.stringify(res.data, null, 2)]);
        const url = URL.createObjectURL(blob);
        const a = document.createElement("a");
        a.href = url;
        a.download = `home-datacenter-audit.${format}`;
        document.body.appendChild(a);
        a.click();
        document.body.removeChild(a);
        URL.revokeObjectURL(url);
    } catch (err: unknown) {
        const maybeResp = (err as { response?: { data?: unknown } })?.response;
        if (maybeResp?.data instanceof Blob) {
            const text = await maybeResp.data.text();
            let parsedMsg = "";
            try {
                const parsed = JSON.parse(text) as { message?: string; error?: string };
                parsedMsg = parsed.message || parsed.error || "";
            } catch {
                parsedMsg = "";
            }
            throw new Error(parsedMsg || text || "Export failed");
        }
        throw err;
    }
}
