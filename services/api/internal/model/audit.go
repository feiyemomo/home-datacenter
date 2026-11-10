package model

import "time"

// StructuredOpsAuditLog represents the comprehensive audit trail entity.
// It records complete execution context (actor, household, target, action, proposal_id,
// approver, results, request_id, is_offline_action).
type StructuredOpsAuditLog struct {
	ID              uint      `gorm:"primaryKey" json:"id"`
	EventUUID       string    `gorm:"size:64;uniqueIndex;not null" json:"event_uuid"` // Anti-replay UUID
	HouseholdID     uint      `gorm:"index;not null;default:1" json:"household_id"`
	RequestID       string    `gorm:"size:64;index" json:"request_id"`                // Correlation tracking ID
	ActorType       string    `gorm:"size:32;not null" json:"actor_type"`             // "user", "internal_service", "offline_agent"
	ActorID         uint      `json:"actor_id"`
	ActorName       string    `gorm:"size:64;not null" json:"actor_name"`
	ClientIP        string    `gorm:"size:64" json:"client_ip"`
	TargetType      string    `gorm:"size:32;not null" json:"target_type"`            // "container", "proposal", "network"
	TargetID        string    `gorm:"size:128;not null" json:"target_id"`
	Action          string    `gorm:"size:64;not null;index" json:"action"`           // "container.emergency_restart", "proposal.execute"
	ProposalID      string    `gorm:"size:64;index" json:"proposal_id,omitempty"`
	Approver        string    `gorm:"size:64" json:"approver,omitempty"`
	ExecutionResult string    `gorm:"type:text" json:"execution_result"`              // JSON serialized execution outcome
	RollbackResult  string    `gorm:"type:text" json:"rollback_result,omitempty"`     // JSON serialized rollback outcome
	IsOfflineAction bool      `gorm:"default:false;index" json:"is_offline_action"`
	CreatedAt       time.Time `gorm:"not null;index" json:"created_at"`
}

func (StructuredOpsAuditLog) TableName() string { return "structured_ops_audit_logs" }
