package model

import (
	"encoding/json"
	"fmt"
	"time"

	"home-datacenter-api/internal/utils"
)

// Proposal status constants.
const (
	ProposalStatusAwaitingApproval = "AWAITING_APPROVAL"
	ProposalStatusApproved         = "APPROVED"
	ProposalStatusScheduled        = "SCHEDULED"
	ProposalStatusExecuting        = "EXECUTING"
	ProposalStatusExecuted         = "EXECUTED"
	ProposalStatusRejected         = "REJECTED"
	ProposalStatusFailed           = "FAILED"
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
	ExecutionLog     string         `gorm:"type:text" json:"execution_log"`
	CreatedAt        time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
}

func (OpsProposal) TableName() string { return "pending_proposals" }

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

// OpsChatSession models a multi-turn chat session with Ops Copilot.
type OpsChatSession struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	SessionID string    `gorm:"size:64;uniqueIndex" json:"session_id"`
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
	Role      string    `gorm:"size:32;not null" json:"role"` // "user", "assistant", "system"
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
