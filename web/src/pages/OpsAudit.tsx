import { useCallback, useEffect, useId, useRef, useState } from "react";
import {
    AlertTriangle,
    CheckCircle2,
    ChevronDown,
    ChevronRight,
    Cloud,
    Download,
    KeyRound,
    RefreshCw,
    Search,
    ShieldAlert,
    ShieldCheck,
    X,
} from "lucide-react";
import {
    createAuditCheckpoint,
    exportAuditFile,
    getAuditKeyStatus,
    getAuditRemoteStatus,
    getAuditRunDetail,
    listAuditRuns,
    syncAuditRemote,
    verifyAuditChain,
    type AgentAuditRun,
    type AuditKeyStatus,
    type AuditRemoteStatusResponse,
    type AuditRunDetail,
    type AuditVerifyResult,
} from "@/api/opsAudit";
import { useAuth } from "@/hooks/useAuth";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

function statusTone(status: string): string {
    switch (status?.toUpperCase()) {
        case "COMPLETED":
        case "SUCCESS":
        case "RECOVERED":
            return "border-emerald-500/40 bg-emerald-500/15 text-emerald-400";
        case "RUNNING":
            return "border-sky-500/40 bg-sky-500/15 text-sky-400";
        case "ROLLED_BACK":
            return "border-amber-500/40 bg-amber-500/15 text-amber-400";
        case "EXECUTION_UNKNOWN":
        case "AUDIT_COMMIT_FAILED":
            return "border-orange-500/50 bg-orange-500/20 text-orange-300";
        default:
            return "border-rose-500/40 bg-rose-500/15 text-rose-400";
    }
}

function integrityBadgeInfo(res: AuditVerifyResult | null) {
    if (!res) {
        return {
            label: "尚未校验",
            className: "border-fg-muted/30 bg-fg-muted/10 text-fg-muted",
            icon: <ShieldAlert size={14} />,
        };
    }
    if (res.valid && res.integrity_status === "VERIFIED_AGAINST_REMOTE") {
        return {
            label: "远程副本与全链已验签",
            className: "border-emerald-500/40 bg-emerald-500/15 text-emerald-400",
            icon: <ShieldCheck size={14} />,
        };
    }
    if (res.valid && res.integrity_status === "VERIFIED_AGAINST_CHECKPOINT") {
        return {
            label: "检查点与全链已验签",
            className: "border-emerald-500/40 bg-emerald-500/15 text-emerald-400",
            icon: <ShieldCheck size={14} />,
        };
    }
    if (res.valid) {
        return {
            label: "本地哈希链完整",
            className: "border-sky-500/40 bg-sky-500/15 text-sky-400",
            icon: <CheckCircle2 size={14} />,
        };
    }
    if (res.integrity_status === "UNVERIFIABLE") {
        return {
            label: "无可验记录",
            className: "border-amber-500/40 bg-amber-500/15 text-amber-400",
            icon: <AlertTriangle size={14} />,
        };
    }
    return {
        label: `完整性异常 (${res.integrity_status})`,
        className: "border-rose-500/50 bg-rose-500/20 text-rose-400",
        icon: <ShieldAlert size={14} />,
    };
}

function remoteStatusTone(status?: string): string {
    switch (status) {
        case "IN_SYNC":
            return "border-emerald-500/40 bg-emerald-500/15 text-emerald-400";
        case "PENDING_REMOTE_SYNC":
            return "border-sky-500/40 bg-sky-500/15 text-sky-400";
        case "DISABLED":
            return "border-amber-500/40 bg-amber-500/15 text-amber-300";
        case "REMOTE_UNREACHABLE":
            return "border-orange-500/50 bg-orange-500/20 text-orange-300";
        default:
            return "border-rose-500/50 bg-rose-500/20 text-rose-400";
    }
}

function formatTs(iso?: string | null, ms?: number): string {
    if (iso) {
        const d = new Date(iso);
        if (!Number.isNaN(d.getTime())) {
            return d.toLocaleString("zh-CN", { hour12: false });
        }
    }
    if (ms && ms > 0) {
        return new Date(ms).toLocaleString("zh-CN", { hour12: false });
    }
    return "-";
}

export default function OpsAudit() {
    const { isAdmin } = useAuth();
    const tableHeadingId = useId();
    const dialogTitleId = useId();
    const dialogRef = useRef<HTMLDialogElement | null>(null);

    const [runs, setRuns] = useState<AgentAuditRun[]>([]);
    const [total, setTotal] = useState(0);
    const [page, setPage] = useState(1);
    const pageSize = 20;
    const [loading, setLoading] = useState(false);
    const [error, setError] = useState<string | null>(null);

    const [search, setSearch] = useState("");
    const [triggerSource, setTriggerSource] = useState("");
    const [statusFilter, setStatusFilter] = useState("");
    const [riskFilter, setRiskFilter] = useState("");
    const [operationIdFilter, setOperationIdFilter] = useState("");
    const [proposalIdFilter, setProposalIdFilter] = useState("");
    const [actorIdFilter, setActorIdFilter] = useState("");
    const [offlineOnly, setOfflineOnly] = useState(false);

    const [verifyResult, setVerifyResult] = useState<AuditVerifyResult | null>(null);
    const [verifying, setVerifying] = useState(false);
    const [keyStatus, setKeyStatus] = useState<AuditKeyStatus | null>(null);
    const [remoteStatus, setRemoteStatus] = useState<AuditRemoteStatusResponse | null>(null);
    const [syncingRemote, setSyncingRemote] = useState(false);
    const [creatingCheckpoint, setCreatingCheckpoint] = useState(false);
    const [exporting, setExporting] = useState(false);

    const [detail, setDetail] = useState<AuditRunDetail | null>(null);
    const [detailLoading, setDetailLoading] = useState(false);
    const [expandedEventSeq, setExpandedEventSeq] = useState<number | null>(null);

    const loadRuns = useCallback(async () => {
        if (!isAdmin) return;
        setLoading(true);
        setError(null);
        try {
            const res = await listAuditRuns({
                page,
                page_size: pageSize,
                q: search.trim() || undefined,
                trigger_source: triggerSource || undefined,
                status: statusFilter || undefined,
                risk_level: riskFilter || undefined,
                operation_id: operationIdFilter.trim() || undefined,
                proposal_id: proposalIdFilter.trim() || undefined,
                actor_id: actorIdFilter.trim() || undefined,
                is_offline: offlineOnly ? true : undefined,
            });
            setRuns(res.items || []);
            setTotal(res.total || 0);
        } catch (err: any) {
            setError(err?.message || "加载 Agent 审计历史失败");
        } finally {
            setLoading(false);
        }
    }, [
        isAdmin,
        page,
        search,
        triggerSource,
        statusFilter,
        riskFilter,
        operationIdFilter,
        proposalIdFilter,
        actorIdFilter,
        offlineOnly,
    ]);

    const loadSecurityPosture = useCallback(async () => {
        if (!isAdmin) return;
        try {
            const [ks, rs] = await Promise.all([getAuditKeyStatus(), getAuditRemoteStatus("core")]);
            setKeyStatus(ks);
            setRemoteStatus(rs);
        } catch {
            // Best-effort posture panel load; verification endpoint still reports core errors.
        }
    }, [isAdmin]);

    const runVerification = useCallback(async () => {
        if (!isAdmin) return;
        setVerifying(true);
        try {
            const res = await verifyAuditChain(true, true, true);
            setVerifyResult(res);
            await loadSecurityPosture();
        } catch (err: any) {
            setVerifyResult({
                valid: false,
                integrity_status: "UNVERIFIABLE",
                stream_id: "core",
                checked_events: 0,
                checked_checkpoints: 0,
                head_seq: 0,
                tail_seq: 0,
                triggers_intact: false,
                failure_reason: err?.message || "校验请求失败",
                limitation_note: "",
                verified_at: new Date().toISOString(),
            });
        } finally {
            setVerifying(false);
        }
    }, [isAdmin, loadSecurityPosture]);

    useEffect(() => {
        loadRuns();
    }, [loadRuns]);

    useEffect(() => {
        runVerification();
    }, [runVerification]);

    const handleRemoteSync = async () => {
        setSyncingRemote(true);
        setError(null);
        try {
            const rs = await syncAuditRemote("core");
            setRemoteStatus(rs);
            await runVerification();
        } catch (err: any) {
            setError(err?.message || "远程副本同步失败");
        } finally {
            setSyncingRemote(false);
        }
    };

    const handleCreateCheckpoint = async () => {
        setCreatingCheckpoint(true);
        setError(null);
        try {
            await createAuditCheckpoint("core");
            await runVerification();
        } catch (err: any) {
            setError(err?.message || "创建审计检查点失败");
        } finally {
            setCreatingCheckpoint(false);
        }
    };

    const openRunDetail = async (runId: string) => {
        setDetail(null);
        setExpandedEventSeq(null);
        setDetailLoading(true);
        if (dialogRef.current && !dialogRef.current.open) {
            dialogRef.current.showModal();
        }
        try {
            const d = await getAuditRunDetail(runId);
            setDetail(d);
            if (d.events?.length) {
                setExpandedEventSeq(d.events[0].seq);
            }
        } catch (err: any) {
            setError(err?.message || "加载运行时间线失败");
            dialogRef.current?.close();
        } finally {
            setDetailLoading(false);
        }
    };

    const closeDetailModal = () => {
        dialogRef.current?.close();
    };

    const handleExport = async (fmt: "json" | "csv") => {
        setExporting(true);
        try {
            await exportAuditFile(fmt, {
                trigger_source: triggerSource || undefined,
                proposal_id: proposalIdFilter.trim() || undefined,
                operation_id: operationIdFilter.trim() || undefined,
                actor_id: actorIdFilter.trim() || undefined,
                risk_level: riskFilter || undefined,
                is_offline: offlineOnly ? true : undefined,
            });
        } catch (err: any) {
            setError(err?.message || `导出 ${fmt.toUpperCase()} 失败`);
        } finally {
            setExporting(false);
        }
    };

    if (!isAdmin) {
        return (
            <div className="space-y-4">
                <h2 className="text-lg font-semibold text-fg">Agent 操作审计追溯</h2>
                <p className="text-xs text-fg-muted">需要管理员权限访问全链路不可变审计账本。</p>
            </div>
        );
    }

    const totalPages = Math.max(1, Math.ceil(total / pageSize));
    const badge = integrityBadgeInfo(verifyResult);

    return (
        <div className="space-y-6">
            {/* Header & Integrity Status Banner */}
            <div className="flex flex-col gap-4 border-b border-[rgb(var(--border)/0.3)] pb-4 md:flex-row md:items-center md:justify-between">
                <div>
                    <h2 className="text-lg font-semibold tracking-tight text-fg">
                        自主协作 Agent 全链路审计追溯
                    </h2>
                    <p className="mt-0.5 text-xs text-fg-muted">
                        覆盖事件接收、策略门控、审批签名、租约 fencing、物理变更与回滚验真的防篡改执行账本。
                    </p>
                </div>
                <div className="flex flex-wrap items-center gap-2">
                    <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        onClick={runVerification}
                        disabled={verifying}
                        className="gap-1.5 text-xs"
                    >
                        <RefreshCw size={13} className={verifying ? "animate-spin" : ""} />
                        立即验签
                    </Button>
                    <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        onClick={handleCreateCheckpoint}
                        disabled={creatingCheckpoint}
                        className="gap-1.5 text-xs"
                    >
                        <ShieldCheck size={13} />
                        {creatingCheckpoint ? "生成检查点…" : "创建检查点"}
                    </Button>
                    <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        onClick={handleRemoteSync}
                        disabled={syncingRemote || !remoteStatus?.diff?.configured}
                        className="gap-1.5 text-xs"
                    >
                        <Cloud size={13} className={syncingRemote ? "animate-pulse" : ""} />
                        同步远程副本
                    </Button>
                    <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        onClick={() => handleExport("json")}
                        disabled={exporting}
                        className="gap-1.5 text-xs"
                    >
                        <Download size={13} />
                        导出 JSON
                    </Button>
                    <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        onClick={() => handleExport("csv")}
                        disabled={exporting}
                        className="gap-1.5 text-xs"
                    >
                        <Download size={13} />
                        导出 CSV
                    </Button>
                </div>
            </div>

            {/* Integrity Verification, Remote Replica & Key Security Card */}
            <Card>
                <CardContent className="space-y-3 pt-4">
                    <div className="flex flex-col gap-3 md:flex-row md:items-center md:justify-between">
                        <div className="space-y-1">
                            <div className="flex flex-wrap items-center gap-2">
                                <span
                                    className={`inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs font-medium ${badge.className}`}
                                >
                                    {badge.icon}
                                    {badge.label}
                                </span>
                                {remoteStatus?.diff && (
                                    <span
                                        className={`inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[11px] font-medium ${remoteStatusTone(
                                            remoteStatus.diff.status,
                                        )}`}
                                    >
                                        <Cloud size={11} />
                                        远程副本: {remoteStatus.diff.status}
                                        {remoteStatus.diff.configured
                                            ? ` (${remoteStatus.diff.store_type})`
                                            : ""}
                                    </span>
                                )}
                                {verifyResult && (
                                    <span className="text-xs text-fg-muted">
                                        已校验 {verifyResult.checked_events} 条事件 ·{" "}
                                        {verifyResult.checked_checkpoints} 个检查点 · 触发器{" "}
                                        {verifyResult.triggers_intact ? "完好" : "缺失"} · Local Tail #
                                        {verifyResult.tail_seq}
                                        {remoteStatus?.diff?.configured
                                            ? ` · Remote Tail #${remoteStatus.diff.remote_tail_seq}`
                                            : ""}
                                    </span>
                                )}
                            </div>
                            {verifyResult?.failure_reason && (
                                <p className="text-xs text-rose-400">
                                    诊断详情：{verifyResult.failure_reason}
                                    {verifyResult.broken_at_seq
                                        ? ` (断链位置 Seq #${verifyResult.broken_at_seq})`
                                        : ""}
                                </p>
                            )}
                            {remoteStatus?.diff?.discrepancies &&
                                remoteStatus.diff.discrepancies.length > 0 && (
                                    <div className="rounded-lg border border-rose-500/40 bg-rose-500/10 px-3 py-2 text-xs text-rose-300">
                                        <div className="font-medium">远程差异告警：</div>
                                        <ul className="mt-1 list-disc pl-4 text-[11px]">
                                            {remoteStatus.diff.discrepancies.map((item, idx) => (
                                                <li key={idx}>{item}</li>
                                            ))}
                                        </ul>
                                    </div>
                                )}
                        </div>
                        {verifyResult?.last_checkpoint_id && (
                            <div className="shrink-0 text-right text-[11px] font-mono text-fg-muted">
                                <div>Anchor: {verifyResult.last_checkpoint_id}</div>
                                <div>Seq #{verifyResult.last_checkpoint_seq}</div>
                                {verifyResult.external_sig_verified && (
                                    <div className="text-emerald-400">Ed25519 外部验签通过</div>
                                )}
                            </div>
                        )}
                    </div>

                    {/* Key Lifecycle & fnOS Root-Attacker Boundary */}
                    <div className="flex flex-col gap-2 border-t border-[rgb(var(--border)/0.25)] pt-2.5 text-[11px] text-fg-muted md:flex-row md:items-center md:justify-between">
                        <div className="flex flex-wrap items-center gap-2">
                            <span className="inline-flex items-center gap-1 font-medium text-fg">
                                <KeyRound size={12} />
                                签名密钥生命周期:
                            </span>
                            <span className="font-mono">
                                Active KeyID: {keyStatus?.active_key_id || "-"}
                            </span>
                            {keyStatus?.configured_key_ids &&
                                keyStatus.configured_key_ids.length > 0 && (
                                    <span className="font-mono text-fg-subtle">
                                        (Keyring: {keyStatus.configured_key_ids.join(", ")})
                                    </span>
                                )}
                            {keyStatus?.external_sig_alg && (
                                <span className="font-mono text-emerald-400">
                                    非对称锚点: {keyStatus.external_sig_alg} ({keyStatus.external_key_id})
                                </span>
                            )}
                            {keyStatus && (
                                <span>轮换次数: {keyStatus.rotation_count}</span>
                            )}
                            {remoteStatus?.diff?.pending_events ? (
                                <span className="text-amber-300">
                                    待同步事件: {remoteStatus.diff.pending_events} 条
                                </span>
                            ) : null}
                        </div>
                    </div>
                    {(verifyResult?.security_boundary || keyStatus?.security_boundary_note) && (
                        <p className="text-[11px] text-fg-subtle">
                            {verifyResult?.security_boundary || keyStatus?.security_boundary_note}
                        </p>
                    )}
                </CardContent>
            </Card>

            {/* Filters Bar */}
            <Card>
                <CardContent className="grid grid-cols-1 gap-3 pt-4 sm:grid-cols-2 lg:grid-cols-6">
                    <div className="relative lg:col-span-2">
                        <Search
                            size={14}
                            className="pointer-events-none absolute left-2.5 top-2.5 text-fg-muted"
                        />
                        <input
                            type="search"
                            value={search}
                            onChange={(e) => {
                                setPage(1);
                                setSearch(e.target.value);
                            }}
                            placeholder="搜索 run_id / operation_id / 提案号 / 目标…"
                            className="w-full rounded-lg border border-[rgb(var(--border)/0.4)] bg-[rgb(var(--bg-elevated)/0.6)] py-1.5 pl-8 pr-3 text-xs text-fg focus:outline-none focus:ring-1 focus:ring-[rgb(var(--accent-warm))]"
                        />
                    </div>
                    <select
                        value={triggerSource}
                        onChange={(e) => {
                            setPage(1);
                            setTriggerSource(e.target.value);
                        }}
                        aria-label="触发来源筛选"
                        className="rounded-lg border border-[rgb(var(--border)/0.4)] bg-[rgb(var(--bg-elevated)/0.6)] px-2.5 py-1.5 text-xs text-fg"
                    >
                        <option value="">全部触发源</option>
                        <option value="api_manual">人工控制台 (api_manual)</option>
                        <option value="proposal_exec">提案执行 (proposal_exec)</option>
                        <option value="docker_fsm">容器自愈 FSM (docker_fsm)</option>
                        <option value="python_offline">离线应急代理 (python_offline)</option>
                        <option value="safety_fsm">安全更新 FSM (safety_fsm)</option>
                        <option value="waf_collector">WAF 防火墙 (waf_collector)</option>
                        <option value="security_defense">入侵防御 (security_defense)</option>
                        <option value="automation">联动规则 (automation)</option>
                        <option value="vuln_scanner">漏洞扫描 (vuln_scanner)</option>
                        <option value="audit_keyring">密钥轮换 (audit_keyring)</option>
                        <option value="maintenance">系统维护 (maintenance)</option>
                    </select>
                    <select
                        value={statusFilter}
                        onChange={(e) => {
                            setPage(1);
                            setStatusFilter(e.target.value);
                        }}
                        aria-label="状态筛选"
                        className="rounded-lg border border-[rgb(var(--border)/0.4)] bg-[rgb(var(--bg-elevated)/0.6)] px-2.5 py-1.5 text-xs text-fg"
                    >
                        <option value="">全部状态</option>
                        <option value="COMPLETED">已完成 (COMPLETED)</option>
                        <option value="FAILED">失败 (FAILED)</option>
                        <option value="ROLLED_BACK">已回滚 (ROLLED_BACK)</option>
                        <option value="BLOCKED">已阻断 (BLOCKED)</option>
                        <option value="EXECUTION_UNKNOWN">结果未知 (EXECUTION_UNKNOWN)</option>
                        <option value="AUDIT_COMMIT_FAILED">审计提交失败 (AUDIT_COMMIT_FAILED)</option>
                    </select>
                    <select
                        value={riskFilter}
                        onChange={(e) => {
                            setPage(1);
                            setRiskFilter(e.target.value);
                        }}
                        aria-label="风险等级筛选"
                        className="rounded-lg border border-[rgb(var(--border)/0.4)] bg-[rgb(var(--bg-elevated)/0.6)] px-2.5 py-1.5 text-xs text-fg"
                    >
                        <option value="">全部风险</option>
                        <option value="CRITICAL">CRITICAL</option>
                        <option value="HIGH">HIGH</option>
                        <option value="MEDIUM">MEDIUM</option>
                        <option value="LOW">LOW</option>
                    </select>
                    <label className="flex cursor-pointer items-center gap-2 rounded-lg border border-[rgb(var(--border)/0.4)] px-3 py-1.5 text-xs text-fg">
                        <input
                            type="checkbox"
                            checked={offlineOnly}
                            onChange={(e) => {
                                setPage(1);
                                setOfflineOnly(e.target.checked);
                            }}
                        />
                        仅离线应急恢复
                    </label>
                    <input
                        type="search"
                        value={operationIdFilter}
                        onChange={(e) => {
                            setPage(1);
                            setOperationIdFilter(e.target.value);
                        }}
                        aria-label="按 operation_id 筛选"
                        placeholder="按 operation_id 精确筛选…"
                        className="rounded-lg border border-[rgb(var(--border)/0.4)] bg-[rgb(var(--bg-elevated)/0.6)] px-2.5 py-1.5 font-mono text-xs text-fg focus:outline-none focus:ring-1 focus:ring-[rgb(var(--accent-warm))] lg:col-span-2"
                    />
                    <input
                        type="search"
                        value={proposalIdFilter}
                        onChange={(e) => {
                            setPage(1);
                            setProposalIdFilter(e.target.value);
                        }}
                        aria-label="按 proposal_id 筛选"
                        placeholder="按 proposal_id 筛选…"
                        className="rounded-lg border border-[rgb(var(--border)/0.4)] bg-[rgb(var(--bg-elevated)/0.6)] px-2.5 py-1.5 font-mono text-xs text-fg focus:outline-none focus:ring-1 focus:ring-[rgb(var(--accent-warm))] lg:col-span-2"
                    />
                    <input
                        type="search"
                        value={actorIdFilter}
                        onChange={(e) => {
                            setPage(1);
                            setActorIdFilter(e.target.value);
                        }}
                        aria-label="按 actor_id 筛选"
                        placeholder="按 actor_id 筛选…"
                        className="rounded-lg border border-[rgb(var(--border)/0.4)] bg-[rgb(var(--bg-elevated)/0.6)] px-2.5 py-1.5 font-mono text-xs text-fg focus:outline-none focus:ring-1 focus:ring-[rgb(var(--accent-warm))] lg:col-span-2"
                    />
                </CardContent>
            </Card>

            {error && (
                <div className="rounded-xl border border-rose-500/40 bg-rose-500/10 px-4 py-2.5 text-xs text-rose-400">
                    {error}
                </div>
            )}

            {/* Operation History Table */}
            <Card>
                <CardHeader className="flex-row items-center justify-between pb-2">
                    <CardTitle id={tableHeadingId} className="text-xs tracking-wider text-fg-muted">
                        Agent 执行任务账本 ({total} 条)
                    </CardTitle>
                    <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        onClick={loadRuns}
                        disabled={loading}
                        className="h-7 gap-1 text-xs"
                    >
                        <RefreshCw size={12} className={loading ? "animate-spin" : ""} />
                        刷新
                    </Button>
                </CardHeader>
                <CardContent>
                    <div
                        tabIndex={0}
                        role="region"
                        aria-labelledby={tableHeadingId}
                        className="max-h-[560px] overflow-auto rounded-lg border border-[rgb(var(--border)/0.25)]"
                    >
                        <table className="w-full border-collapse text-left text-xs">
                            <thead className="sticky top-0 z-10 bg-[rgb(var(--bg-elevated)/0.95)] backdrop-blur">
                                <tr className="border-b border-[rgb(var(--border)/0.3)] text-[11px] text-fg-muted">
                                    <th scope="col" className="px-3 py-2.5 font-medium">
                                        任务 / 时间
                                    </th>
                                    <th scope="col" className="px-3 py-2.5 font-medium">
                                        触发源与执行者
                                    </th>
                                    <th scope="col" className="px-3 py-2.5 font-medium">
                                        动作与目标组件
                                    </th>
                                    <th scope="col" className="px-3 py-2.5 font-medium">
                                        风险 / 状态
                                    </th>
                                    <th scope="col" className="px-3 py-2.5 font-medium">
                                        步骤 / 耗时
                                    </th>
                                    <th scope="col" className="px-3 py-2.5 text-right font-medium">
                                        详情
                                    </th>
                                </tr>
                            </thead>
                            <tbody className="divide-y divide-[rgb(var(--border)/0.2)]">
                                {runs.length === 0 ? (
                                    <tr>
                                        <td
                                            colSpan={6}
                                            className="px-4 py-8 text-center text-xs text-fg-subtle"
                                        >
                                            {loading ? "加载中…" : "暂无匹配的 Agent 操作审计记录"}
                                        </td>
                                    </tr>
                                ) : (
                                    runs.map((run) => (
                                        <tr
                                            key={run.run_id}
                                            className="transition-colors hover:bg-white/[0.04]"
                                        >
                                            <th scope="row" className="px-3 py-2.5 font-normal">
                                                <div className="font-mono text-[11px] font-medium text-fg">
                                                    {run.run_id}
                                                </div>
                                                <div className="text-[11px] text-fg-muted">
                                                    {formatTs(run.started_at, run.started_at_ms)}
                                                </div>
                                            </th>
                                            <td className="px-3 py-2.5">
                                                <div className="flex flex-wrap items-center gap-1">
                                                    <Badge variant="outline" className="text-[10px]">
                                                        {run.trigger_source}
                                                    </Badge>
                                                    {run.is_offline && (
                                                        <Badge className="border-amber-500/40 bg-amber-500/15 text-[10px] text-amber-300">
                                                            离线应急
                                                        </Badge>
                                                    )}
                                                </div>
                                                <div className="mt-0.5 text-[11px] text-fg-muted">
                                                    {run.actor_type}: {run.actor_id}
                                                </div>
                                            </td>
                                            <td className="px-3 py-2.5">
                                                <div className="font-medium text-fg">{run.action}</div>
                                                <div className="text-[11px] text-fg-muted">
                                                    {run.component || run.target || "-"}
                                                    {run.proposal_id ? ` · ${run.proposal_id}` : ""}
                                                    {run.operation_id ? ` · op:${run.operation_id}` : ""}
                                                </div>
                                            </td>
                                            <td className="px-3 py-2.5">
                                                <div className="flex flex-wrap items-center gap-1">
                                                    <Badge variant="outline" className="text-[10px]">
                                                        {run.risk_level || "MEDIUM"}
                                                    </Badge>
                                                    <span
                                                        className={`inline-flex rounded border px-1.5 py-0.5 text-[10px] font-medium ${statusTone(
                                                            run.status,
                                                        )}`}
                                                    >
                                                        {run.status}
                                                    </span>
                                                </div>
                                            </td>
                                            <td className="px-3 py-2.5 font-mono text-[11px] text-fg-muted">
                                                <div>{run.event_count} 步</div>
                                                <div>{run.duration_ms} ms</div>
                                            </td>
                                            <td className="px-3 py-2.5 text-right">
                                                <Button
                                                    type="button"
                                                    variant="ghost"
                                                    size="sm"
                                                    onClick={() => openRunDetail(run.run_id)}
                                                    className="h-7 text-xs"
                                                >
                                                    查看时间线
                                                </Button>
                                            </td>
                                        </tr>
                                    ))
                                )}
                            </tbody>
                        </table>
                    </div>

                    {totalPages > 1 && (
                        <div className="mt-4 flex items-center justify-between text-xs text-fg-muted">
                            <span>
                                第 {page} / {totalPages} 页
                            </span>
                            <div className="flex gap-2">
                                <Button
                                    type="button"
                                    variant="outline"
                                    size="sm"
                                    disabled={page <= 1}
                                    onClick={() => setPage((p) => Math.max(1, p - 1))}
                                >
                                    上一页
                                </Button>
                                <Button
                                    type="button"
                                    variant="outline"
                                    size="sm"
                                    disabled={page >= totalPages}
                                    onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
                                >
                                    下一页
                                </Button>
                            </div>
                        </div>
                    )}
                </CardContent>
            </Card>

            {/* Native Top-Layer Modal Dialog for Run Timeline */}
            <dialog
                ref={dialogRef}
                aria-labelledby={dialogTitleId}
                onClick={(e) => {
                    const dlg = dialogRef.current;
                    if (!dlg) return;
                    const rect = dlg.getBoundingClientRect();
                    const outside =
                        e.clientX < rect.left ||
                        e.clientX > rect.right ||
                        e.clientY < rect.top ||
                        e.clientY > rect.bottom;
                    if (outside) dlg.close();
                }}
                style={{ overscrollBehavior: "contain" }}
                className="m-auto w-[min(92vw,880px)] max-h-[85vh] overflow-auto rounded-2xl border border-[rgb(var(--border)/0.45)] bg-[rgb(var(--bg-surface))] p-0 text-fg shadow-2xl backdrop:bg-black/60 backdrop:backdrop-blur-sm"
            >
                <div className="sticky top-0 z-10 flex items-center justify-between border-b border-[rgb(var(--border)/0.3)] bg-[rgb(var(--bg-surface)/0.95)] px-5 py-3.5 backdrop-blur">
                    <div>
                        <h3 id={dialogTitleId} className="text-sm font-semibold text-fg">
                            执行生命周期时间线
                        </h3>
                        {detail && (
                            <p className="font-mono text-[11px] text-fg-muted">
                                {detail.run.run_id} · Correlation: {detail.run.correlation_id}
                            </p>
                        )}
                    </div>
                    <button
                        type="button"
                        onClick={closeDetailModal}
                        aria-label="关闭时间线弹窗"
                        className="rounded-lg p-1.5 text-fg-muted transition-colors hover:bg-white/10 hover:text-fg"
                    >
                        <X size={16} />
                    </button>
                </div>

                <div className="space-y-4 p-5">
                    {detailLoading && (
                        <div className="flex items-center justify-center py-8 text-xs text-fg-muted">
                            <RefreshCw size={14} className="mr-2 animate-spin" />
                            正在校验哈希链并加载时间线…
                        </div>
                    )}

                    {detail && (
                        <>
                            <div className="flex flex-wrap items-center gap-2 rounded-xl border border-[rgb(var(--border)/0.3)] bg-white/[0.02] p-3 text-xs">
                                <span
                                    className={`inline-flex rounded border px-2 py-0.5 text-[11px] font-medium ${statusTone(
                                        detail.run.status,
                                    )}`}
                                >
                                    {detail.run.status}
                                </span>
                                <span className="text-fg-muted">
                                    完整性校验：{detail.integrity.integrity_status}{" "}
                                    {detail.integrity.valid ? "✅" : "⚠️"}
                                </span>
                                <span className="text-fg-muted">
                                    执行人：{detail.run.actor_type} ({detail.run.actor_id})
                                </span>
                                {detail.run.proposal_id && (
                                    <span className="font-mono text-[11px] text-fg-muted">
                                        提案：{detail.run.proposal_id}
                                    </span>
                                )}
                                {detail.run.operation_id && (
                                    <span className="font-mono text-[11px] text-fg-muted">
                                        Operation：{detail.run.operation_id}
                                    </span>
                                )}
                            </div>

                            <ol className="space-y-2.5">
                                {detail.events.map((ev) => {
                                    const expanded = expandedEventSeq === ev.seq;
                                    return (
                                        <li
                                            key={ev.audit_id}
                                            className="rounded-xl border border-[rgb(var(--border)/0.3)] bg-white/[0.02] p-3 text-xs"
                                        >
                                            <button
                                                type="button"
                                                onClick={() =>
                                                    setExpandedEventSeq(expanded ? null : ev.seq)
                                                }
                                                className="flex w-full items-center justify-between text-left"
                                            >
                                                <div className="flex flex-wrap items-center gap-2">
                                                    {expanded ? (
                                                        <ChevronDown size={14} />
                                                    ) : (
                                                        <ChevronRight size={14} />
                                                    )}
                                                    <span className="font-mono text-[11px] text-fg-muted">
                                                        #{ev.seq}
                                                    </span>
                                                    <span className="font-semibold text-fg">
                                                        {ev.event_type}
                                                    </span>
                                                    <Badge variant="outline" className="text-[10px]">
                                                        {ev.action}
                                                    </Badge>
                                                    {ev.decision && (
                                                        <Badge variant="outline" className="text-[10px]">
                                                            {ev.decision}
                                                        </Badge>
                                                    )}
                                                    {ev.result && (
                                                        <span className="text-[11px] text-fg-muted">
                                                            → {ev.result}
                                                        </span>
                                                    )}
                                                </div>
                                                <span className="font-mono text-[11px] text-fg-subtle">
                                                    {formatTs(ev.created_at, ev.created_at_ms)}
                                                </span>
                                            </button>

                                            {expanded && (
                                                <div className="mt-3 space-y-2 border-t border-[rgb(var(--border)/0.2)] pt-2.5 font-mono text-[11px] text-fg-muted">
                                                    <div>
                                                        AuditID: {ev.audit_id} | FencingGen:{" "}
                                                        {ev.fencing_generation} | Duration:{" "}
                                                        {ev.duration_ms}ms
                                                        {ev.key_id ? ` | KeyID: ${ev.key_id}` : ""}
                                                        {ev.operation_id ? ` | OpID: ${ev.operation_id}` : ""}
                                                    </div>
                                                    {ev.error_code && (
                                                        <div className="text-rose-400">
                                                            Error: {ev.error_code}
                                                        </div>
                                                    )}
                                                    {ev.parameters_redacted && (
                                                        <div>
                                                            <div className="text-fg-subtle">
                                                                脱敏参数 (Digest:{" "}
                                                                {ev.parameters_digest?.slice(0, 16)}…):
                                                            </div>
                                                            <pre className="mt-1 overflow-x-auto rounded bg-black/25 p-2 text-[10px] text-fg">
                                                                {ev.parameters_redacted}
                                                            </pre>
                                                        </div>
                                                    )}
                                                    {(ev.before_state || ev.after_state) && (
                                                        <div className="grid grid-cols-1 gap-2 md:grid-cols-2">
                                                            {ev.before_state && (
                                                                <div>
                                                                    <div className="text-fg-subtle">
                                                                        变更前状态:
                                                                    </div>
                                                                    <pre className="mt-1 overflow-x-auto rounded bg-black/25 p-2 text-[10px]">
                                                                        {ev.before_state}
                                                                    </pre>
                                                                </div>
                                                            )}
                                                            {ev.after_state && (
                                                                <div>
                                                                    <div className="text-fg-subtle">
                                                                        变更后状态:
                                                                    </div>
                                                                    <pre className="mt-1 overflow-x-auto rounded bg-black/25 p-2 text-[10px]">
                                                                        {ev.after_state}
                                                                    </pre>
                                                                </div>
                                                            )}
                                                        </div>
                                                    )}
                                                    {ev.evidence && (
                                                        <div>
                                                            <div className="text-fg-subtle">证据 / 输出:</div>
                                                            <pre className="mt-1 overflow-x-auto rounded bg-black/25 p-2 text-[10px]">
                                                                {ev.evidence}
                                                            </pre>
                                                        </div>
                                                    )}
                                                    <div className="text-[10px] text-fg-subtle">
                                                        PrevHash: {ev.previous_hash.slice(0, 16)}… →
                                                        EntryHash: {ev.entry_hash.slice(0, 16)}… |{" "}
                                                        {ev.hmac_signature.slice(0, 26)}…
                                                    </div>
                                                </div>
                                            )}
                                        </li>
                                    );
                                })}
                            </ol>
                        </>
                    )}
                </div>
            </dialog>
        </div>
    );
}
