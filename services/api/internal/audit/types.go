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
	IntegrityVerifiedAgainstRemote     = "VERIFIED_AGAINST_REMOTE"
	IntegrityVerifiedAgainstCheckpoint = "VERIFIED_AGAINST_CHECKPOINT"
	IntegrityChainIntactLocalOnly      = "CHAIN_INTACT_LOCAL_ONLY"
	IntegrityPendingCloudSync          = "PENDING_CLOUD_SYNC"
	IntegrityTruncatedTail             = "TRUNCATED_TAIL"
	IntegrityDBRollbackDetected        = "DB_ROLLBACK_DETECTED"
	IntegrityChainDiverged             = "CHAIN_DIVERGED"
	IntegrityCheckpointMismatch        = "CHECKPOINT_MISMATCH"
	IntegrityHashMismatch              = "HASH_MISMATCH"
	IntegrityHMACInvalid               = "HMAC_INVALID"
	IntegritySeqGap                    = "SEQ_GAP"
	IntegrityChainFork                 = "CHAIN_FORK"
	IntegrityTriggerMissing            = "TRIGGER_MISSING"
	IntegrityUnverifiable              = "UNVERIFIABLE"
)

// Remote replica sync & divergence statuses.
const (
	RemoteStatusLocalOnly          = "LOCAL_ONLY"
	RemoteStatusDisabled           = RemoteStatusLocalOnly
	RemoteStatusInSync             = "IN_SYNC"
	RemoteStatusPendingSync        = "PENDING_REMOTE_SYNC"
	RemoteStatusUnreachable        = "REMOTE_UNREACHABLE"
	RemoteStatusTailTruncated      = "TAIL_TRUNCATED"
	RemoteStatusDBRollback         = "DB_ROLLBACK_DETECTED"
	RemoteStatusChainDiverged      = "CHAIN_DIVERGED"
	RemoteStatusCheckpointMismatch = "CHECKPOINT_MISMATCH"
)

// Explicit 4-axis audit trust boundary constants.
const (
	LocalChainBoundaryIntact       = "INTACT_LOCAL_SQLITE"
	LocalChainBoundaryBroken       = "BROKEN_LOCAL_CHAIN"
	LocalChainBoundaryUnverifiable = "UNVERIFIABLE"

	WORMGuaranteeNoneLocalOnly      = "NONE_LOCAL_ONLY"
	WORMGuaranteeHostPathAppendOnly = "HOST_PATH_APPEND_ONLY"
	WORMGuaranteeRemoteAppendProxy  = "REMOTE_APPEND_PROXY"

	KeySepHMACCoResidentEnvOnly      = "UNCONFIGURED_HMAC_ONLY"
	KeySepEd25519PrivCoResident      = "PRIV_KEY_CO_RESIDENT"
	KeySepEd25519VerifyOnlySeparated = "VERIFY_ONLY_PUB_SEPARATED"

	HMACKeyScopeCoResident = "CO_RESIDENT_API_ENV"
)

// AuditTrustBoundaries clarifies exact cryptographic and storage boundaries without false trust claims.
type AuditTrustBoundaries struct {
	LocalOnly             bool   `json:"local_only"`
	LocalChain            string `json:"local_chain"`
	RemoteStore           string `json:"remote_store"`
	RemoteReachable       bool   `json:"remote_reachable"`
	WORMGuarantee         string `json:"worm_guarantee"`
	PrivateKeySeparation  string `json:"private_key_separation"`
	HMACKeyScope          string `json:"hmac_key_scope"`
	SigningPrivKeyPresent bool   `json:"signing_priv_key_present"`
	Summary               string `json:"summary"`
}

var (
	// ErrAuditCommitFailed indicates persistent audit commit failure blocking further mutation.
	ErrAuditCommitFailed = errors.New("AUDIT_COMMIT_FAILED: failed to commit mandatory audit record")
	// ErrAuditConfigInvalid indicates invalid audit remote store, signer, or period configuration.
	ErrAuditConfigInvalid = errors.New("AUDIT_CONFIG_INVALID: invalid audit configuration")
	// ErrAuditRemoteRequired indicates strict remote audit mode cannot proceed without an available remote store.
	ErrAuditRemoteRequired = errors.New("AUDIT_REMOTE_REQUIRED: strict remote audit store is unavailable or unconfigured")
	// ErrAuditSyncFailed indicates remote audit replication or checkpoint submission failed.
	ErrAuditSyncFailed = errors.New("AUDIT_SYNC_FAILED: remote audit replication failed")
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
	ProposalID    string
	ApprovalID    string
	OperationID   string
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
	ProposalID        string
	ApprovalID        string
	OperationID       string
	FencingGeneration int64
	Summary           string
	IsOffline         bool
	Synced            *bool
	InitialEvent      *EmitEventInput
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
	ApprovalID         string
	OperationID        string
	FencingGeneration  int64
	DecisionReason     string
	BaselineBeforeHash string
	BaselineAfterHash  string
	BeforeState        interface{}
	AfterState         interface{}
	Parameters         interface{}
	RollbackResult     interface{}
	Result             string
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
	EventUUID          string `json:"event_uuid"`
	AuditID            string `json:"audit_id"`
	RunID              string `json:"run_id"`
	ParentEventID      string `json:"parent_event_id"`
	CorrelationID      string `json:"correlation_id"`
	HouseholdID        uint   `json:"household_id"`
	OperationID        string `json:"operation_id"`
	ProposalID         string `json:"proposal_id"`
	ApprovalID         string `json:"approval_id"`
	PolicyVersion      string `json:"policy_version"`
	TriggerSource      string `json:"trigger_source"`
	ActorType          string `json:"actor_type"`
	ActorID            string `json:"actor_id"`
	EventType          string `json:"event_type"`
	ContainerName      string `json:"container_name"`
	Component          string `json:"component"`
	Target             string `json:"target"`
	Action             string `json:"action"`
	Decision           string `json:"decision"`
	RiskLevel          string `json:"risk_level"`
	Holder             string `json:"holder"`
	FencingGeneration  int64  `json:"fencing_generation"`
	ExitCode           int    `json:"exit_code"`
	Success            int    `json:"success"`
	AuditStatus        string `json:"audit_status"`
	BeforeState        string `json:"before_state"`
	AfterState         string `json:"after_state"`
	ParametersDigest   string `json:"parameters_digest"`
	ParametersRedacted string `json:"parameters_redacted"`
	Evidence           string `json:"evidence"`
	ErrorMessage       string `json:"error_message"`
	DurationMs         int64  `json:"duration_ms"`
	ExecutedAt         string `json:"executed_at"`
	ExecutedAtMs       int64  `json:"executed_at_ms"`
	OriginStreamID     string `json:"origin_stream_id"`
	OfflineSeq         uint64 `json:"offline_seq"`
	LocalPrevHash      string `json:"local_prev_hash"`
	EntryHash          string `json:"entry_hash"`
	KeyID              string `json:"key_id"`
	HMACSignature      string `json:"hmac_signature"`
}

// OfflineSyncRejection captures per-record validation failures without masking acceptance of valid siblings.
type OfflineSyncRejection struct {
	EventUUID string `json:"event_uuid"`
	Reason    string `json:"reason"`
	ErrorCode string `json:"error_code,omitempty"`
}

// OfflineSyncResult reports explicit server-accepted UUIDs for idempotent Python acking.
type OfflineSyncResult struct {
	AcceptedUUIDs   []string               `json:"accepted_uuids"`
	Rejected        []OfflineSyncRejection `json:"rejected,omitempty"`
	SyncedCount     int                    `json:"synced_count"`
	AnchorSeq       uint64                 `json:"anchor_seq,omitempty"`
	ChainStatus     string                 `json:"chain_status,omitempty"`
	IntegrityAlerts []string               `json:"integrity_alerts,omitempty"`
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
	ProposalID    string
	ApprovalID    string
	OperationID   string
	CorrelationID string
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
	ApprovalID    string
	OperationID   string
	ActorID       string
	RiskLevel     string
	IsOffline     *bool
	Result        string
	FromMs        int64
	ToMs          int64
	SortAsc       bool
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
	VerifyRemote      *bool  `json:"verify_remote,omitempty"`
}

// VerifyResult reports structured tamper-evidence diagnostics.
type VerifyResult struct {
	Valid                 bool     `json:"valid"`
	IntegrityStatus       string   `json:"integrity_status"`
	StreamID              string   `json:"stream_id"`
	CheckedEvents         int64    `json:"checked_events"`
	CheckedCheckpoints    int64    `json:"checked_checkpoints"`
	HeadSeq               uint64   `json:"head_seq"`
	TailSeq               uint64   `json:"tail_seq"`
	TailEntryHash         string   `json:"tail_entry_hash,omitempty"`
	LastCheckpointID      string   `json:"last_checkpoint_id,omitempty"`
	LastCheckpointSeq     uint64   `json:"last_checkpoint_seq,omitempty"`
	TriggersIntact        bool     `json:"triggers_intact"`
	BrokenAtSeq           uint64   `json:"broken_at_seq,omitempty"`
	FailureReason         string   `json:"failure_reason,omitempty"`
	KeyIDsUsed            []string             `json:"key_ids_used,omitempty"`
	LocalOnly             bool                 `json:"local_only"`
	RemoteStoreConfigured bool                 `json:"remote_store_configured"`
	RemoteStoreType       string               `json:"remote_store_type,omitempty"`
	RemoteSyncStatus      string               `json:"remote_sync_status,omitempty"`
	RemoteTailSeq         uint64               `json:"remote_tail_seq,omitempty"`
	RemoteTailHash        string               `json:"remote_tail_hash,omitempty"`
	ExternalSigVerified   bool                 `json:"external_sig_verified"`
	TrustBoundaries       AuditTrustBoundaries `json:"trust_boundaries"`
	SecurityBoundary      string               `json:"security_boundary"`
	LimitationNote        string               `json:"limitation_note"`
	VerifiedAt            string               `json:"verified_at"`
}

// RemoteTailState summarizes the highest contiguous state recorded on the external remote store.
type RemoteTailState struct {
	StreamID          string `json:"stream_id"`
	TailSeq           uint64 `json:"tail_seq"`
	TailEntryHash     string `json:"tail_entry_hash,omitempty"`
	EventCount        int64  `json:"event_count"`
	LastCheckpointID  string `json:"last_checkpoint_id,omitempty"`
	LastCheckpointSeq uint64 `json:"last_checkpoint_seq,omitempty"`
	LastChainDigest   string `json:"last_chain_digest,omitempty"`
	UpdatedAt         string `json:"updated_at,omitempty"`
}

// RemoteDiffReport compares local SQLite audit ledger state against an external append-only/WORM store.
type RemoteDiffReport struct {
	LocalOnly           bool                 `json:"local_only"`
	Configured          bool                 `json:"configured"`
	StoreType           string               `json:"store_type"`
	Endpoint            string               `json:"endpoint,omitempty"`
	Reachable           bool                 `json:"reachable"`
	Status              string               `json:"status"`
	LocalTailSeq        uint64               `json:"local_tail_seq"`
	LocalTailHash       string               `json:"local_tail_hash,omitempty"`
	LocalCheckpointSeq  uint64               `json:"local_checkpoint_seq"`
	RemoteTailSeq       uint64               `json:"remote_tail_seq"`
	RemoteTailHash      string               `json:"remote_tail_hash,omitempty"`
	RemoteCheckpointID  string               `json:"remote_checkpoint_id,omitempty"`
	RemoteCheckpointSeq uint64               `json:"remote_checkpoint_seq"`
	RemoteChainDigest   string               `json:"remote_chain_digest,omitempty"`
	PendingEvents       uint64               `json:"pending_events"`
	ExternalSigVerified bool                 `json:"external_sig_verified"`
	TrustBoundaries     AuditTrustBoundaries `json:"trust_boundaries"`
	Discrepancies       []string             `json:"discrepancies,omitempty"`
	Error               string               `json:"error,omitempty"`
	CheckedAt           string               `json:"checked_at"`
}

// AuditKeyStatus exposes non-secret key lifecycle, rotation history, and fnOS root-attacker boundary info.
type AuditKeyStatus struct {
	ActiveKeyID           string               `json:"active_key_id"`
	ConfiguredKeyIDs      []string             `json:"configured_key_ids"`
	HistoricalKeyIDs      []string             `json:"historical_key_ids"`
	ExternalSigAlg        string               `json:"external_sig_alg,omitempty"`
	ExternalKeyID         string               `json:"external_key_id,omitempty"`
	ExternalPublicKey     string               `json:"external_public_key,omitempty"`
	LastRotationAt        string               `json:"last_rotation_at,omitempty"`
	RotationCount         int64                `json:"rotation_count"`
	LocalOnly             bool                 `json:"local_only"`
	RemoteStoreConfigured bool                 `json:"remote_store_configured"`
	RemoteStoreType       string               `json:"remote_store_type,omitempty"`
	TrustBoundaries       AuditTrustBoundaries `json:"trust_boundaries"`
	SecurityBoundaryNote  string               `json:"security_boundary_note"`
}

// RunDetail bundles the run header, ordered event timeline, and chain verification result.
type RunDetail struct {
	Run          model.AgentAuditRun     `json:"run"`
	Events       []model.AgentAuditEvent `json:"events"`
	Integrity    VerifyResult            `json:"integrity"`
}
