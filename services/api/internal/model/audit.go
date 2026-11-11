package model

import (
	"time"

	"home-datacenter-api/internal/utils"
)

// AuditGenesisHash is the fixed 64-zero anchor for stream sequence 1.
const AuditGenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// AuditPrimaryStreamID identifies the authoritative Go Core append-only hash chain.
const AuditPrimaryStreamID = "core"

// Canonical 24-stage Agent lifecycle event types (shared 1:1 with Python Ops Agent).
const (
	AuditEventReceived             = "EVENT_RECEIVED"
	AuditEventContextCollected     = "CONTEXT_COLLECTED"
	AuditEventAnalysisCompleted    = "ANALYSIS_COMPLETED"
	AuditEventRiskAssessed         = "RISK_ASSESSED"
	AuditEventProposalCreated      = "PROPOSAL_CREATED"
	AuditEventPolicyEvaluated      = "POLICY_EVALUATED"
	AuditEventApprovalRequested    = "APPROVAL_REQUESTED"
	AuditEventApprovalGranted      = "APPROVAL_GRANTED"
	AuditEventApprovalRejected     = "APPROVAL_REJECTED"
	AuditEventApprovalExpired      = "APPROVAL_EXPIRED"
	AuditEventExecutionLockAcquired = "EXECUTION_LOCK_ACQUIRED"
	AuditEventRecoveryLeaseAcquired = "RECOVERY_LEASE_ACQUIRED"
	AuditEventExecutionStarted     = "EXECUTION_STARTED"
	AuditEventExecutionStepCompleted = "EXECUTION_STEP_COMPLETED"
	AuditEventExecutionFailed      = "EXECUTION_FAILED"
	AuditEventHealthCheckStarted   = "HEALTH_CHECK_STARTED"
	AuditEventHealthCheckCompleted = "HEALTH_CHECK_COMPLETED"
	AuditEventRollbackStarted      = "ROLLBACK_STARTED"
	AuditEventRollbackCompleted    = "ROLLBACK_COMPLETED"
	AuditEventRecoveryVerified     = "RECOVERY_VERIFIED"
	AuditEventExecutionCompleted   = "EXECUTION_COMPLETED"
	AuditEventExecutionUnknown     = "EXECUTION_UNKNOWN"
	AuditEventAuditCommitFailed    = "AUDIT_COMMIT_FAILED"
	AuditEventExecutionCancelled   = "EXECUTION_CANCELLED"
	AuditEventAuditKeyRotated      = "AUDIT_KEY_ROTATED"
)

// AuditDecision values returned by policy/authorization checks.
const (
	AuditDecisionAllow           = "ALLOW"
	AuditDecisionDeny            = "DENY"
	AuditDecisionRequireApproval = "REQUIRE_APPROVAL"
)

// AuditRiskLevel values.
const (
	AuditRiskLow      = "LOW"
	AuditRiskMedium   = "MEDIUM"
	AuditRiskHigh     = "HIGH"
	AuditRiskCritical = "CRITICAL"
)

// AuditRunStatus terminal/active lifecycle states.
const (
	AuditRunStatusRunning           = "RUNNING"
	AuditRunStatusCompleted         = "COMPLETED"
	AuditRunStatusFailed            = "FAILED"
	AuditRunStatusRolledBack        = "ROLLED_BACK"
	AuditRunStatusCancelled         = "CANCELLED"
	AuditRunStatusBlocked           = "BLOCKED"
	AuditRunStatusExecutionUnknown  = "EXECUTION_UNKNOWN"
	AuditRunStatusAuditCommitFailed = "AUDIT_COMMIT_FAILED"
	AuditRunStatusPendingSync       = "PENDING_SYNC"
)

// StructuredOpsAuditLog represents the backward-compatible audit trail entity
// synced from offline ops events and mirrored on terminal agent operations.
type StructuredOpsAuditLog struct {
	ID              uint      `gorm:"primaryKey" json:"id"`
	EventUUID       string    `gorm:"size:64;uniqueIndex;not null" json:"event_uuid"`
	HouseholdID     uint      `gorm:"index;not null;default:1" json:"household_id"`
	RequestID       string    `gorm:"size:64;index" json:"request_id"`
	ActorType       string    `gorm:"size:32;not null" json:"actor_type"`
	ActorID         uint      `json:"actor_id"`
	ActorName       string    `gorm:"size:64;not null" json:"actor_name"`
	ClientIP        string    `gorm:"size:64" json:"client_ip"`
	TargetType      string    `gorm:"size:32;not null" json:"target_type"`
	TargetID        string    `gorm:"size:128;not null" json:"target_id"`
	Action          string    `gorm:"size:64;not null;index" json:"action"`
	ProposalID      string    `gorm:"size:64;index" json:"proposal_id,omitempty"`
	Approver        string    `gorm:"size:64" json:"approver,omitempty"`
	ExecutionResult string    `gorm:"type:text" json:"execution_result"`
	RollbackResult  string    `gorm:"type:text" json:"rollback_result,omitempty"`
	IsOfflineAction bool      `gorm:"default:false;index" json:"is_offline_action"`
	CreatedAt       time.Time `gorm:"not null;index" json:"created_at"`
}

func (StructuredOpsAuditLog) TableName() string { return "structured_ops_audit_logs" }

// AgentAuditRun tracks the end-to-end execution lifecycle of an agent task or mutating operation.
type AgentAuditRun struct {
	ID              uint           `gorm:"primaryKey" json:"id"`
	RunID           string         `gorm:"size:64;uniqueIndex;not null" json:"run_id"`
	CorrelationID   string         `gorm:"size:64;index;not null" json:"correlation_id"`
	HouseholdID     uint           `gorm:"index;not null;default:1" json:"household_id"`
	TriggerSource   string         `gorm:"size:48;not null;index" json:"trigger_source"`
	ActorType       string         `gorm:"size:32;not null" json:"actor_type"`
	ActorID         string         `gorm:"size:64;not null" json:"actor_id"`
	Action          string         `gorm:"size:64;not null;index" json:"action"`
	Target          string         `gorm:"size:128;index" json:"target"`
	Component       string         `gorm:"size:64;index" json:"component"`
	Decision        string         `gorm:"size:32;index" json:"decision"`
	RiskLevel       string         `gorm:"size:16;index" json:"risk_level"`
	Status          string         `gorm:"size:32;not null;index" json:"status"`
	ProposalID      string         `gorm:"size:64;index" json:"proposal_id,omitempty"`
	ApprovalID      string         `gorm:"size:64;index" json:"approval_id,omitempty"`
	OperationID     string         `gorm:"size:64;index" json:"operation_id,omitempty"`
	Summary         string         `gorm:"type:text" json:"summary,omitempty"`
	ErrorCode       string         `gorm:"size:64;index" json:"error_code,omitempty"`
	IsOffline       bool           `gorm:"not null;default:false;index" json:"is_offline"`
	Synced          bool           `gorm:"not null;default:true;index" json:"synced"`
	EventCount      int            `gorm:"not null;default:0" json:"event_count"`
	FirstSeq        uint64         `gorm:"not null;default:0" json:"first_seq"`
	LastSeq         uint64         `gorm:"not null;default:0" json:"last_seq"`
	LastAuditID     string         `gorm:"size:64" json:"last_audit_id,omitempty"`
	LastEntryHash   string         `gorm:"size:64" json:"last_entry_hash,omitempty"`
	IntegrityStatus string         `gorm:"size:32;not null;default:'CHAIN_INTACT_LOCAL_ONLY'" json:"integrity_status"`
	StartedAt       time.Time      `gorm:"not null;index" json:"started_at"`
	StartedAtMs     int64          `gorm:"not null;index" json:"started_at_ms"`
	EndedAt         utils.NullTime `gorm:"type:datetime" json:"ended_at,omitempty"`
	EndedAtMs       int64          `gorm:"not null;default:0" json:"ended_at_ms"`
	DurationMs      int64          `gorm:"not null;default:0" json:"duration_ms"`
}

func (AgentAuditRun) TableName() string { return "agent_audit_runs" }

// AgentAuditEvent is an append-only, hash-chained and HMAC-authenticated audit record.
type AgentAuditEvent struct {
	ID                 uint      `gorm:"primaryKey" json:"id"`
	StreamID           string    `gorm:"size:64;not null;default:'core';uniqueIndex:idx_audit_stream_seq" json:"stream_id"`
	Seq                uint64    `gorm:"not null;uniqueIndex:idx_audit_stream_seq" json:"seq"`
	AuditID            string    `gorm:"size:64;uniqueIndex;not null" json:"audit_id"`
	RunID              string    `gorm:"size:64;index:idx_audit_events_run_seq;not null" json:"run_id"`
	ParentEventID      string    `gorm:"size:64;index" json:"parent_event_id,omitempty"`
	CorrelationID      string    `gorm:"size:64;index;not null" json:"correlation_id"`
	HouseholdID        uint      `gorm:"index:idx_audit_events_hh_time;not null;default:1" json:"household_id"`
	TriggerSource      string    `gorm:"size:48;not null;index" json:"trigger_source"`
	ActorType          string    `gorm:"size:32;not null" json:"actor_type"`
	ActorID            string    `gorm:"size:64;not null" json:"actor_id"`
	EventType          string    `gorm:"size:48;not null;index" json:"event_type"`
	Action             string    `gorm:"size:64;not null;index" json:"action"`
	Target             string    `gorm:"size:128;index" json:"target"`
	Component          string    `gorm:"size:64;index" json:"component"`
	Decision           string    `gorm:"size:32;index" json:"decision"`
	RiskLevel          string    `gorm:"size:16;index" json:"risk_level"`
	PolicyVersion      string    `gorm:"size:32" json:"policy_version"`
	ProposalID         string    `gorm:"size:64;index" json:"proposal_id,omitempty"`
	ApprovalID         string    `gorm:"size:64;index" json:"approval_id,omitempty"`
	OperationID        string    `gorm:"size:64;index" json:"operation_id,omitempty"`
	FencingGeneration  int64     `gorm:"not null;default:0" json:"fencing_generation"`
	BeforeState        string    `gorm:"type:text" json:"before_state,omitempty"`
	AfterState         string    `gorm:"type:text" json:"after_state,omitempty"`
	ParametersDigest   string    `gorm:"size:64" json:"parameters_digest,omitempty"`
	ParametersRedacted string    `gorm:"type:text" json:"parameters_redacted,omitempty"`
	Result             string    `gorm:"size:32;index" json:"result,omitempty"`
	ErrorCode          string    `gorm:"size:64;index" json:"error_code,omitempty"`
	DurationMs         int64     `gorm:"not null;default:0" json:"duration_ms"`
	Evidence           string    `gorm:"type:text" json:"evidence,omitempty"`
	CreatedAt          time.Time `gorm:"not null;index:idx_audit_events_hh_time" json:"created_at"`
	CreatedAtMs        int64     `gorm:"not null;index" json:"created_at_ms"`
	IsOffline          bool      `gorm:"not null;default:false;index" json:"is_offline"`
	Synced             bool      `gorm:"not null;default:true;index" json:"synced"`
	OriginStreamID     string    `gorm:"size:64;index" json:"origin_stream_id,omitempty"`
	OriginSeq          uint64    `gorm:"not null;default:0" json:"origin_seq,omitempty"`
	OriginEntryHash    string    `gorm:"size:64" json:"origin_entry_hash,omitempty"`
	KeyID              string    `gorm:"size:32;index" json:"key_id,omitempty"`
	PreviousHash       string    `gorm:"size:64;not null" json:"previous_hash"`
	EntryHash          string    `gorm:"size:64;uniqueIndex;not null" json:"entry_hash"`
	HMACSignature      string    `gorm:"size:160;not null" json:"hmac_signature"`
}

func (AgentAuditEvent) TableName() string { return "agent_audit_events" }

// AgentAuditCheckpoint anchors contiguous segments of the audit hash chain.
type AgentAuditCheckpoint struct {
	ID              uint      `gorm:"primaryKey" json:"id"`
	CheckpointID    string    `gorm:"size:64;uniqueIndex;not null" json:"checkpoint_id"`
	StreamID        string    `gorm:"size:64;not null;default:'core';index" json:"stream_id"`
	FromSeq         uint64    `gorm:"not null" json:"from_seq"`
	ToSeq           uint64    `gorm:"not null;index" json:"to_seq"`
	EventCount      int64     `gorm:"not null" json:"event_count"`
	HeadEntryHash   string    `gorm:"size:64;not null" json:"head_entry_hash"`
	TailEntryHash   string    `gorm:"size:64;not null" json:"tail_entry_hash"`
	ChainDigest     string    `gorm:"size:64;not null" json:"chain_digest"`
	PrevChainDigest string    `gorm:"size:64;not null" json:"prev_chain_digest"`
	KeyID           string    `gorm:"size:32;not null" json:"key_id"`
	HMACSignature   string    `gorm:"size:160;not null" json:"hmac_signature"`
	ExternalSig     string    `gorm:"type:text" json:"external_sig,omitempty"`
	ExternalSigAlg  string    `gorm:"size:32" json:"external_sig_alg,omitempty"`
	ExternalKeyID   string    `gorm:"size:64" json:"external_key_id,omitempty"`
	ArchiveFile     string    `gorm:"size:255" json:"archive_file,omitempty"`
	CreatedAt       time.Time `gorm:"not null;index" json:"created_at"`
	CreatedAtMs     int64     `gorm:"not null" json:"created_at_ms"`
}

func (AgentAuditCheckpoint) TableName() string { return "agent_audit_checkpoints" }

// AgentAuditRemoteSyncState tracks mutable replication progress and divergence status against
// an external append-only/WORM remote audit replica without altering append-only audit tables.
type AgentAuditRemoteSyncState struct {
	StreamID                string `gorm:"primaryKey;size:64" json:"stream_id"`
	RemoteStoreType         string `gorm:"size:32;not null;default:'none'" json:"remote_store_type"`
	RemoteEndpoint          string `gorm:"size:255" json:"remote_endpoint,omitempty"`
	LastSyncedSeq           uint64 `gorm:"not null;default:0" json:"last_synced_seq"`
	LastSyncedEntryHash     string `gorm:"size:64" json:"last_synced_entry_hash,omitempty"`
	LastSyncedCheckpointID  string `gorm:"size:64" json:"last_synced_checkpoint_id,omitempty"`
	LastSyncedCheckpointSeq uint64 `gorm:"not null;default:0" json:"last_synced_checkpoint_seq"`
	LastSyncAtMs            int64  `gorm:"not null;default:0" json:"last_sync_at_ms"`
	LastError               string `gorm:"type:text" json:"last_error,omitempty"`
	ConsecutiveFailures     int    `gorm:"not null;default:0" json:"consecutive_failures"`
	SyncStatus              string `gorm:"size:32;not null;default:'DISABLED'" json:"sync_status"`
	UpdatedAtMs             int64  `gorm:"not null;default:0" json:"updated_at_ms"`
}

func (AgentAuditRemoteSyncState) TableName() string { return "agent_audit_remote_sync_states" }
