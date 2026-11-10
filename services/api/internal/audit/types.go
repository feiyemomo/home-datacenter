package audit

import (
	"context"
	"errors"
	"time"

	"home-datacenter-api/internal/model"
)

const (
	// PolicyVersion identifies the active AgentAuditEngine protocol contract version.
	PolicyVersion = "v5.0"

	// MaxEvidenceBytes caps serialized evidence/state blobs to prevent SQLite/memory bloat.
	MaxEvidenceBytes = 16 * 1024

	// DefaultCheckpointInterval defines how many events trigger an automatic checkpoint anchor.
	DefaultCheckpointInterval = 100

	// BoundedEmergencyWindow limits emergency degraded restarts when the primary audit DB is unavailable.
	BoundedEmergencyWindow = 10 * time.Minute
)

// AuditFailurePolicy determines how side-effect execution reacts when audit persistence fails.
type AuditFailurePolicy string

const (
	// PolicyFailClosed aborts the operation before side effects if pre-execution audit fails.
	PolicyFailClosed AuditFailurePolicy = "FAIL_CLOSED"
	// PolicyBoundedEmergency allows bounded container emergency restart via signed fallback JSONL.
	PolicyBoundedEmergency AuditFailurePolicy = "BOUNDED_EMERGENCY_DEGRADED"
	// PolicyBestEffortTelemetry emits warning metrics without halting low-risk read-only diagnostics.
	PolicyBestEffortTelemetry AuditFailurePolicy = "BEST_EFFORT"
)

// Integrity / verification status outcomes reported honestly without false positives.
const (
	IntegrityVerifiedAgainstCheckpoint = "VERIFIED_AGAINST_CHECKPOINT"
	IntegrityChainIntactLocalOnly      = "CHAIN_INTACT_LOCAL_ONLY"
	IntegrityPendingCloudSync          = "PENDING_CLOUD_SYNC"
	IntegrityTruncatedTail             = "TRUNCATED_TAIL"
	IntegrityHashMismatch              = "HASH_MISMATCH"
	IntegrityHMACInvalid               = "HMAC_INVALID"
	IntegritySeqGap                    = "SEQ_GAP"
	IntegrityTriggerMissing            = "TRIGGER_MISSING"
	IntegrityUnverifiable              = "UNVERIFIABLE"
)

var (
	// ErrAuditCommitFailed indicates persistent audit commit failure blocking further mutation.
	ErrAuditCommitFailed = errors.New("AUDIT_COMMIT_FAILED: failed to commit mandatory audit record")
	// ErrAuditKeyMissing indicates external OPS_AUDIT_HMAC_KEY is not configured.
	ErrAuditKeyMissing = errors.New("external OPS_AUDIT_HMAC_KEY is not configured")
	// ErrAuditKeyWeak indicates the configured HMAC key does not satisfy minimum entropy length.
	ErrAuditKeyWeak = errors.New("OPS_AUDIT_HMAC_KEY is weak or shorter than 32 bytes")
	// ErrAuditDegradedQuotaExceeded indicates bounded emergency fallback quota has been exhausted.
	ErrAuditDegradedQuotaExceeded = errors.New("AUDIT_DEGRADED_QUOTA_EXCEEDED: bounded emergency restart limit reached")
	// ErrAuditRunNotFound indicates the requested audit run does not exist or belongs to another household.
	ErrAuditRunNotFound = errors.New("audit run not found")
)

type ctxKey string

const (
	ctxKeySession ctxKey = "agent_audit_session"
)

// AuditSession carries correlation and parent event lineage through context.Context.
type AuditSession struct {
	RunID         string
	CorrelationID string
	ParentEventID string
	HouseholdID   uint
	TriggerSource string
	ActorType     string
	ActorID       string
}

// ContextWithSession binds an AuditSession to ctx.
func ContextWithSession(ctx context.Context, s AuditSession) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, ctxKeySession, s)
}

// SessionFromContext extracts the active AuditSession if present.
func SessionFromContext(ctx context.Context) (AuditSession, bool) {
	if ctx == nil {
		return AuditSession{}, false
	}
	s, ok := ctx.Value(ctxKeySession).(AuditSession)
	return s, ok && s.RunID != ""
}

// StartRunInput initializes a unified AgentAuditRun.
type StartRunInput struct {
	RunID         string
	CorrelationID string
	HouseholdID   uint
	TriggerSource string
	ActorType     string
	ActorID       string
	Action        string
	Target        string
	Component     string
	Decision      string
	RiskLevel     string
	ProposalID    string
	ApprovalID    string
	OperationID   string
	Summary       string
	IsOffline     bool
	Synced        *bool
	InitialEvent  *EmitEventInput
}

// EmitEventInput describes a single lifecycle event emitted to AgentAuditEngine.
type EmitEventInput struct {
	AuditID           string
	RunID             string
	ParentEventID     string
	CorrelationID     string
	HouseholdID       uint
	TriggerSource     string
	ActorType         string
	ActorID           string
	EventType         string
	Action            string
	Target            string
	Component         string
	Decision          string
	RiskLevel         string
	PolicyVersion     string
	ProposalID        string
	ApprovalID        string
	OperationID       string
	FencingGeneration int64
	BeforeState       interface{}
	AfterState        interface{}
	Parameters        interface{}
	Result            string
	ErrorCode         string
	DurationMs        int64
	Evidence          interface{}
	CreatedAt         *time.Time
	IsOffline         bool
	Synced            *bool
	FailurePolicy     AuditFailurePolicy
	FinalRunStatus    string
	RunSummary        string
	MirrorLegacy      bool
	LegacyTargetType  string
	LegacyApprover    string
	LegacyClientIP    string
}

// OfflineSyncEvent represents an offline audit entry submitted by the edge Python daemon.
type OfflineSyncEvent struct {
	EventUUID         string `json:"event_uuid"`
	AuditID           string `json:"audit_id"`
	RunID             string `json:"run_id"`
	ParentEventID     string `json:"parent_event_id"`
	CorrelationID     string `json:"correlation_id"`
	OperationID       string `json:"operation_id"`
	TriggerSource     string `json:"trigger_source"`
	ActorType         string `json:"actor_type"`
	ActorID           string `json:"actor_id"`
	EventType         string `json:"event_type"`
	ContainerName     string `json:"container_name"`
	Component         string `json:"component"`
	Target            string `json:"target"`
	Action            string `json:"action"`
	Decision          string `json:"decision"`
	RiskLevel         string `json:"risk_level"`
	Holder            string `json:"holder"`
	FencingGeneration int64  `json:"fencing_generation"`
	ExitCode          int    `json:"exit_code"`
	Success           int    `json:"success"`
	AuditStatus       string `json:"audit_status"`
	BeforeState       string `json:"before_state"`
	AfterState        string `json:"after_state"`
	ParametersDigest  string `json:"parameters_digest"`
	ParametersRedacted string `json:"parameters_redacted"`
	Evidence          string `json:"evidence"`
	ErrorMessage      string `json:"error_message"`
	ExecutedAt        string `json:"executed_at"`
	ExecutedAtMs      int64  `json:"executed_at_ms"`
	OfflineSeq        uint64 `json:"offline_seq"`
	LocalPrevHash     string `json:"local_prev_hash"`
	EntryHash         string `json:"entry_hash"`
	HMACSignature     string `json:"hmac_signature"`
}

// OfflineSyncRejection captures per-record validation failures without masking acceptance of valid siblings.
type OfflineSyncRejection struct {
	EventUUID string `json:"event_uuid"`
	Reason    string `json:"reason"`
}

// OfflineSyncResult reports explicit server-accepted UUIDs for idempotent Python acking.
type OfflineSyncResult struct {
	AcceptedUUIDs []string               `json:"accepted_uuids"`
	Rejected      []OfflineSyncRejection `json:"rejected,omitempty"`
	SyncedCount   int                    `json:"synced_count"`
	AnchorSeq     uint64                 `json:"anchor_seq,omitempty"`
}

// RunQuery filters paginated audit runs.
type RunQuery struct {
	HouseholdID   uint
	TriggerSource string
	Component     string
	Action        string
	Status        string
	RiskLevel     string
	ActorID       string
	IsOffline     *bool
	FromMs        int64
	ToMs          int64
	Search        string
	Page          int
	PageSize      int
}

// EventQuery filters paginated/exportable audit events.
type EventQuery struct {
	HouseholdID   uint
	RunID         string
	EventType     string
	TriggerSource string
	Component     string
	Action        string
	Target        string
	CorrelationID string
	ProposalID    string
	OperationID   string
	Result        string
	FromMs        int64
	ToMs          int64
	Page          int
	PageSize      int
}

// VerifyRequest configures chain/checkpoint/archive integrity validation.
type VerifyRequest struct {
	StreamID          string `json:"stream_id,omitempty"`
	FromSeq           uint64 `json:"from_seq,omitempty"`
	ToSeq             uint64 `json:"to_seq,omitempty"`
	VerifyCheckpoints bool   `json:"verify_checkpoints"`
	VerifyTriggers    bool   `json:"verify_triggers"`
	VerifyArchive     bool   `json:"verify_archive"`
}

// VerifyResult reports structured tamper-evidence diagnostics.
type VerifyResult struct {
	Valid              bool     `json:"valid"`
	IntegrityStatus    string   `json:"integrity_status"`
	StreamID           string   `json:"stream_id"`
	CheckedEvents      int64    `json:"checked_events"`
	CheckedCheckpoints int64    `json:"checked_checkpoints"`
	HeadSeq            uint64   `json:"head_seq"`
	TailSeq            uint64   `json:"tail_seq"`
	TailEntryHash      string   `json:"tail_entry_hash,omitempty"`
	LastCheckpointID   string   `json:"last_checkpoint_id,omitempty"`
	LastCheckpointSeq  uint64   `json:"last_checkpoint_seq,omitempty"`
	TriggersIntact     bool     `json:"triggers_intact"`
	BrokenAtSeq        uint64   `json:"broken_at_seq,omitempty"`
	FailureReason      string   `json:"failure_reason,omitempty"`
	KeyIDsUsed         []string `json:"key_ids_used,omitempty"`
	LimitationNote     string   `json:"limitation_note"`
	VerifiedAt         string   `json:"verified_at"`
}

// RunDetail bundles the run header, ordered event timeline, and chain verification result.
type RunDetail struct {
	Run          model.AgentAuditRun     `json:"run"`
	Events       []model.AgentAuditEvent `json:"events"`
	Integrity    VerifyResult            `json:"integrity"`
}
