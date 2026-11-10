package model

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/utils"
)

// Proposal status constants.
const (
	ProposalStatusPending                = "PENDING"
	ProposalStatusAwaitingApproval       = "AWAITING_APPROVAL" // Canonical / backward compatible
	ProposalStatusApprovalRequired       = "APPROVAL_REQUIRED"
	ProposalStatusApproved               = "APPROVED"
	ProposalStatusScheduled              = "SCHEDULED"
	ProposalStatusExecuting              = "EXECUTING"
	ProposalStatusSuccess                = "SUCCESS"           // Canonical completed state
	ProposalStatusExecuted               = "EXECUTED"          // Backward compatible alias for SUCCESS
	ProposalStatusRejected               = "REJECTED"
	ProposalStatusCancelled              = "CANCELLED"
	ProposalStatusFailed                 = "FAILED"
	ProposalStatusRollingBack            = "ROLLING_BACK"
	ProposalStatusRolledBack             = "ROLLED_BACK"
	ProposalStatusFailedRolledback       = "FAILED_ROLLEDBACK" // Backward compatible alias for ROLLED_BACK
	ProposalStatusRollbackFailed         = "ROLLBACK_FAILED"
	ProposalStatusExecutionUnknown       = "EXECUTION_UNKNOWN"       // Network timeout / unknown response
	ProposalStatusReconciliationRequired = "RECONCILIATION_REQUIRED" // Process crash or ambiguous state
	ProposalStatusExpired                = "EXPIRED"                 // Scheduled window missed
)

// Risk level constants.
const (
	RiskLevelLow      = "LOW"
	RiskLevelMedium   = "MEDIUM"
	RiskLevelHigh     = "HIGH"
	RiskLevelCritical = "CRITICAL"
)

// OpsProposal represents a system improvement or maintenance proposal
// managed by the Ops Agent and pending administrator approval.
type OpsProposal struct {
	ID               uint           `gorm:"primaryKey" json:"id"`
	ProposalID       string         `gorm:"size:64;uniqueIndex" json:"proposal_id"`
	IdempotencyKey   string         `gorm:"size:128;index" json:"idempotency_key,omitempty"`
	HouseholdID      uint           `gorm:"index;default:1" json:"household_id"`
	Title            string         `gorm:"size:255;not null" json:"title"`
	Description      string         `gorm:"type:text" json:"description"`
	TargetComponent  string         `gorm:"size:128" json:"target_component"`
	RiskLevel        string         `gorm:"size:32;not null;default:'LOW'" json:"risk_level"`
	EstimatedSeconds int            `gorm:"default:30" json:"estimated_seconds"`
	DowntimeSeconds  int            `gorm:"default:0" json:"downtime_seconds"`
	ExecutionPlan    string         `gorm:"type:text;not null" json:"execution_plan"`
	RollbackPlan     string         `gorm:"type:text;not null" json:"rollback_plan"`
	Status           string         `gorm:"size:32;not null;index;default:'AWAITING_APPROVAL'" json:"status"`
	ScheduledAt      *string        `gorm:"size:64" json:"scheduled_at"`
	ApprovedAt       utils.NullTime `json:"approved_at"`
	ApprovedBy       string         `gorm:"size:64" json:"approved_by"`
	ExecutedAt       utils.NullTime `json:"executed_at"`
	ReconciledAt     utils.NullTime `json:"reconciled_at,omitempty"`
	ExecutionLog     string         `gorm:"type:text" json:"execution_log"`
	BaselineHash     string         `gorm:"size:128" json:"baseline_hash"`
	ActionSpec       string         `gorm:"type:text" json:"action_spec"`
	ApprovalDigest   string         `gorm:"column:approval_digest;type:varchar(128)" json:"approval_digest,omitempty"`
	Version          int            `gorm:"column:version;default:1" json:"version"`
	CreatedAt        time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
}

func (OpsProposal) TableName() string { return "pending_proposals" }

// CanonicalApprovalPayload is the normalized representation of proposal attributes
// cryptographically bound upon administrator approval.
type CanonicalApprovalPayload struct {
	TargetComponent string                 `json:"target_component"`
	ExecutionAction string                 `json:"execution_action"`
	Parameters      map[string]interface{} `json:"parameters"`
	BaselineHash    string                 `json:"baseline_hash"`
	Version         int                    `json:"version"`
}

// CanonicalizeComponent normalizes component names and aliases.
func CanonicalizeComponent(name string) string {
	clean := strings.TrimSpace(name)
	if idx := strings.Index(clean, "("); idx != -1 {
		prefix := strings.TrimSpace(clean[:idx])
		if prefix != "" {
			clean = prefix
		}
	}
	clean = strings.ToLower(clean)
	switch clean {
	case "home-frigate", "frigate", "nvr", "go2rtc":
		return "frigate"
	case "home-mosquitto", "mosquitto", "mqtt":
		return "mosquitto"
	case "home-api", "api", "backend", "go-api":
		return "home-api"
	case "home-web", "web", "frontend", "nginx":
		return "home-web"
	case "home-vision", "vision", "ai-vision":
		return "home-vision"
	case "openresty", "1panel-openresty", "nginx-proxy", "1panel-openresty-wudu":
		return "openresty"
	case "1pwaf", "waf", "openresty-waf":
		return "1pwaf"
	default:
		return clean
	}
}

// ComputeApprovalDigest computes a deterministic SHA-256 digest over normalized canonical JSON of:
// target_component, execution_action, canonical sorted parameters, baseline_hash, and version.
func ComputeApprovalDigest(target string, action string, params map[string]interface{}, baseline string, version int) string {
	if params == nil {
		params = make(map[string]interface{})
	}
	if version <= 0 {
		version = 1
	}
	canonicalTarget := CanonicalizeComponent(target)
	payload := CanonicalApprovalPayload{
		TargetComponent: canonicalTarget,
		ExecutionAction: strings.ToLower(strings.TrimSpace(action)),
		Parameters:      params,
		BaselineHash:    strings.TrimSpace(baseline),
		Version:         version,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum)
}

// ComputeProposalApprovalDigest extracts proposal attributes and generates the canonical approval digest.
func ComputeProposalApprovalDigest(p *OpsProposal) (string, error) {
	if p == nil {
		return "", errors.New("proposal is nil")
	}
	target := p.TargetComponent
	action := ""
	params := make(map[string]interface{})

	if strings.TrimSpace(p.ActionSpec) != "" {
		var spec struct {
			Action       string                 `json:"action"`
			Target       string                 `json:"target"`
			Params       map[string]interface{} `json:"params"`
			BaselineHash string                 `json:"baseline_hash"`
		}
		if err := json.Unmarshal([]byte(p.ActionSpec), &spec); err == nil {
			if spec.Action != "" {
				action = spec.Action
			}
			if target == "" && spec.Target != "" {
				target = spec.Target
			} else if target != "" && spec.Target != "" && CanonicalizeComponent(target) != CanonicalizeComponent(spec.Target) {
				return "", fmt.Errorf("target component mismatch: proposal target '%s' does not match action_spec target '%s'", target, spec.Target)
			}
			if spec.Params != nil {
				params = spec.Params
			}
		}
	}

	if action == "" {
		planLower := strings.ToLower(p.ExecutionPlan)
		switch {
		case strings.Contains(planLower, "restart"):
			action = "docker.restart"
		case strings.Contains(planLower, "update") || strings.Contains(planLower, "recreate") || strings.Contains(planLower, "pull"):
			action = "docker.recreate"
		case strings.Contains(planLower, "create index"):
			action = "db.create_index"
		case strings.Contains(planLower, "drop index"):
			action = "db.drop_index"
		case strings.Contains(planLower, "reload"):
			action = "waf.reload"
		case strings.Contains(planLower, "cache") || strings.Contains(planLower, "purge"):
			action = "cache.purge"
		default:
			action = strings.TrimSpace(p.ExecutionPlan)
		}
	}

	version := p.Version
	if version <= 0 {
		version = 1
	}

	digest := ComputeApprovalDigest(target, action, params, p.BaselineHash, version)
	if digest == "" {
		return "", errors.New("failed to compute approval digest")
	}
	return digest, nil
}

// ComputeDefaultBaseline computes fallback baseline matching HostFallbackProvider.
func ComputeDefaultBaseline(component string) string {
	canonical := CanonicalizeComponent(component)
	containerName := canonical
	switch canonical {
	case "frigate":
		containerName = "home-frigate"
	case "mosquitto":
		containerName = "home-mosquitto"
	}
	hasher := sha256.New()
	raw := fmt.Sprintf("%s:channels-tool/%s:latest:running:", canonical, containerName)
	hasher.Write([]byte(raw))
	return fmt.Sprintf("sha256:%x", hasher.Sum(nil))
}

// BeforeCreate initializes version, fallback baseline, and approval digest for directly seeded proposals.
func (p *OpsProposal) BeforeCreate(tx *gorm.DB) error {
	if p.Version <= 0 {
		p.Version = 1
	}
	if strings.TrimSpace(p.BaselineHash) == "" && strings.TrimSpace(p.TargetComponent) != "" {
		p.BaselineHash = ComputeDefaultBaseline(p.TargetComponent)
	}
	if (p.Status == ProposalStatusApproved || p.Status == ProposalStatusScheduled) && strings.TrimSpace(p.ApprovalDigest) == "" {
		if digest, err := ComputeProposalApprovalDigest(p); err == nil {
			p.ApprovalDigest = digest
		}
	}
	return nil
}


// MarshalJSON customizes JSON serialization so ID can map to proposal_id for frontend compatibility.
func (p OpsProposal) MarshalJSON() ([]byte, error) {
	idVal := any(p.ID)
	if p.ProposalID != "" {
		idVal = p.ProposalID
	} else if p.ID > 0 {
		idVal = fmt.Sprintf("prop-%d", p.ID)
	}

	type Alias OpsProposal
	return json.Marshal(&struct {
		ID any `json:"id"`
		Alias
	}{
		ID:    idVal,
		Alias: (Alias)(p),
	})
}

// UnmarshalJSON supports deserializing proposals whether ID is numeric or string formatted,
// and parses nullable timestamps for approved_at and executed_at.
func (p *OpsProposal) UnmarshalJSON(data []byte) error {
	type Alias OpsProposal
	aux := struct {
		RawID       any `json:"id"`
		ApprovedAt  any `json:"approved_at"`
		ExecutedAt  any `json:"executed_at"`
		*Alias
	}{
		Alias: (*Alias)(p),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	switch v := aux.RawID.(type) {
	case float64:
		p.ID = uint(v)
	case string:
		if p.ProposalID == "" {
			p.ProposalID = v
		}
		var num uint
		if _, err := fmt.Sscanf(v, "prop-%d", &num); err == nil {
			p.ID = num
		} else if _, err := fmt.Sscanf(v, "%d", &num); err == nil {
			p.ID = num
		}
	}

	if s, ok := aux.ApprovedAt.(string); ok && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			p.ApprovedAt = utils.NullTime{Time: t, Valid: true}
		} else if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
			p.ApprovedAt = utils.NullTime{Time: t, Valid: true}
		}
	}

	if s, ok := aux.ExecutedAt.(string); ok && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			p.ExecutedAt = utils.NullTime{Time: t, Valid: true}
		} else if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
			p.ExecutedAt = utils.NullTime{Time: t, Valid: true}
		}
	}

	return nil
}

const (
	ChatScopePersonal  = "personal"
	ChatScopeHousehold = "household"
)

// OpsChatSession models a multi-turn chat session with Ops Copilot.
type OpsChatSession struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	SessionID string    `gorm:"size:64;uniqueIndex" json:"session_id"`
	UserID    uint      `gorm:"index;not null;default:0" json:"user_id"`
	Scope     string    `gorm:"size:32;not null;default:'personal'" json:"scope"` // "personal" | "household"
	Title     string    `gorm:"size:255" json:"title"`
	CreatedAt time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (OpsChatSession) TableName() string { return "ops_chat_sessions" }

// OpsChatMessage models an individual message exchanged in an Ops Chat session.
type OpsChatMessage struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	MessageID string    `gorm:"size:64;index" json:"message_id"`
	SessionID string    `gorm:"size:64;index" json:"session_id"`
	UserID    uint      `gorm:"index;not null;default:0" json:"user_id"`
	Scope     string    `gorm:"size:32;not null;default:'personal'" json:"scope"` // "personal" | "household"
	Role      string    `gorm:"size:32;not null" json:"role"`                     // "user", "assistant", "system"
	Content   string    `gorm:"type:text;not null" json:"content"`
	Model     string    `gorm:"size:64" json:"model"`
	Timestamp int64     `json:"timestamp"`
	CreatedAt time.Time `gorm:"not null" json:"created_at"`
}

func (OpsChatMessage) TableName() string { return "ops_chat_messages" }

// OpsExecutionHistory models the historical processing record of autonomous agent operations,
// including component auto-updates, DB snapshot backups, health probe rollbacks, and vulnerability patching.
type OpsExecutionHistory struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	ExecutionType string    `gorm:"size:64;not null;index" json:"execution_type"` // "component_update", "vuln_patch", "auto_rollback", "proposal_exec"
	Component     string    `gorm:"size:128;not null" json:"component"`          // e.g. "home-api", "sqlite", "frigate"
	Action        string    `gorm:"size:255;not null" json:"action"`             // Human readable description
	OldVersion    string    `gorm:"size:64" json:"old_version"`
	NewVersion    string    `gorm:"size:64" json:"new_version"`
	Status        string    `gorm:"size:32;not null;index" json:"status"`         // "success", "rolled_back", "failed", "pending"
	SnapshotID    string    `gorm:"size:128" json:"snapshot_id"`                 // Backup snapshot file path or ID
	HealthOutput  string    `gorm:"type:text" json:"health_output"`
	Detail        string    `gorm:"type:text" json:"detail"`
	CreatedAt     time.Time `gorm:"not null;index" json:"created_at"`
}

func (OpsExecutionHistory) TableName() string { return "ops_execution_histories" }

// OpsMaintenanceLock models the persistent system-wide maintenance lock.
type OpsMaintenanceLock struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Active    bool      `gorm:"not null;default:false" json:"active"`
	LockedBy  string    `gorm:"size:64" json:"locked_by"`
	Reason    string    `gorm:"type:text" json:"reason"`
	LockedAt  time.Time `json:"locked_at"`
	ExpiresAt time.Time `gorm:"index" json:"expires_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (OpsMaintenanceLock) TableName() string { return "ops_maintenance_locks" }

// OpsCircuitBreakerState models the persistent container circuit breaker status and crash history.
type OpsCircuitBreakerState struct {
	ID              uint      `gorm:"primaryKey" json:"id"`
	Container       string    `gorm:"size:64;uniqueIndex;not null" json:"container"`
	State           string    `gorm:"size:32;not null;default:'CLOSED'" json:"state"` // CLOSED, OPEN, HALF_OPEN
	OpenedAt        time.Time `json:"opened_at"`
	CrashTimestamps string    `gorm:"type:text" json:"crash_timestamps"` // JSON array of RFC3339 timestamps
	UpdatedAt       time.Time `json:"updated_at"`
}

func (OpsCircuitBreakerState) TableName() string { return "ops_circuit_breaker_states" }

