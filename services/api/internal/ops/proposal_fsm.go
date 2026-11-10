package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/audit"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/redact"
	"home-datacenter-api/internal/utils"
)

var (
	ErrInvalidProposalTransition = errors.New("invalid proposal state transition")
	ErrProposalCASConflict       = errors.New("proposal state changed concurrently (CAS conflict)")
)

// AllowedProposalTransitions defines the canonical, closed whitelist state transition matrix (R4-02).
// Terminal states (EXECUTED, SUCCESS, CANCELLED, REJECTED, EXPIRED, ROLLED_BACK, FAILED_ROLLEDBACK,
// ROLLBACK_FAILED, FAILED) cannot be overwritten back into active execution or approval states.
var AllowedProposalTransitions = map[string]map[string]bool{
	model.ProposalStatusPending: {
		model.ProposalStatusAwaitingApproval: true,
		model.ProposalStatusApprovalRequired: true,
		model.ProposalStatusApproved:         true,
		model.ProposalStatusScheduled:        true,
		model.ProposalStatusRejected:         true,
		model.ProposalStatusCancelled:        true,
		model.ProposalStatusExpired:          true,
	},
	model.ProposalStatusAwaitingApproval: {
		model.ProposalStatusPending:          true,
		model.ProposalStatusApprovalRequired: true,
		model.ProposalStatusApproved:         true,
		model.ProposalStatusScheduled:        true,
		model.ProposalStatusRejected:         true,
		model.ProposalStatusCancelled:        true,
		model.ProposalStatusExpired:          true,
	},
	model.ProposalStatusApprovalRequired: {
		model.ProposalStatusPending:          true,
		model.ProposalStatusAwaitingApproval: true,
		model.ProposalStatusApproved:         true,
		model.ProposalStatusScheduled:        true,
		model.ProposalStatusRejected:         true,
		model.ProposalStatusCancelled:        true,
		model.ProposalStatusExpired:          true,
	},
	model.ProposalStatusApproved: {
		model.ProposalStatusExecuting: true,
		model.ProposalStatusCancelled: true,
		model.ProposalStatusExpired:   true,
		model.ProposalStatusPending:   true, // InvalidateProposalApproval
	},
	model.ProposalStatusScheduled: {
		model.ProposalStatusExecuting: true,
		model.ProposalStatusCancelled: true,
		model.ProposalStatusExpired:   true,
		model.ProposalStatusPending:   true, // InvalidateProposalApproval
	},
	model.ProposalStatusExecuting: {
		model.ProposalStatusExecuted:               true,
		model.ProposalStatusSuccess:                true,
		model.ProposalStatusFailed:                 true,
		model.ProposalStatusRollingBack:            true,
		model.ProposalStatusExecutionUnknown:       true,
		model.ProposalStatusReconciliationRequired: true,
	},
	model.ProposalStatusRollingBack: {
		model.ProposalStatusRolledBack:             true,
		model.ProposalStatusFailedRolledback:       true,
		model.ProposalStatusRollbackFailed:         true,
		model.ProposalStatusReconciliationRequired: true,
	},
	// Ambiguous/unknown execution outcomes require explicit controlled reconciliation;
	// they must NEVER jump directly back to APPROVED/SCHEDULED/EXECUTING for blind retry.
	model.ProposalStatusExecutionUnknown: {
		model.ProposalStatusReconciliationRequired: true,
		model.ProposalStatusExecuted:               true,
		model.ProposalStatusSuccess:                true,
		model.ProposalStatusFailed:                 true,
		model.ProposalStatusCancelled:              true,
	},
	model.ProposalStatusReconciliationRequired: {
		model.ProposalStatusExecuted:   true,
		model.ProposalStatusSuccess:    true,
		model.ProposalStatusFailed:     true,
		model.ProposalStatusRolledBack: true,
		model.ProposalStatusCancelled:  true,
	},
	// Terminal states: strictly empty (immutable)
	model.ProposalStatusExecuted:        {},
	model.ProposalStatusSuccess:         {},
	model.ProposalStatusFailed:          {},
	model.ProposalStatusCancelled:       {},
	model.ProposalStatusRejected:        {},
	model.ProposalStatusExpired:         {},
	model.ProposalStatusRolledBack:      {},
	model.ProposalStatusFailedRolledback: {},
	model.ProposalStatusRollbackFailed:  {},
}

// IsProposalTransitionAllowed returns true if from -> to is permitted by the unified matrix.
func IsProposalTransitionAllowed(fromStatus, toStatus string) bool {
	from := strings.ToUpper(strings.TrimSpace(fromStatus))
	to := strings.ToUpper(strings.TrimSpace(toStatus))
	allowed, ok := AllowedProposalTransitions[from]
	if !ok {
		return false
	}
	return allowed[to]
}

// IsTerminalProposalStatus returns true for terminal proposal states that cannot resume execution.
func IsTerminalProposalStatus(status string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case model.ProposalStatusExecuted,
		model.ProposalStatusSuccess,
		model.ProposalStatusFailed,
		model.ProposalStatusCancelled,
		model.ProposalStatusRejected,
		model.ProposalStatusExpired,
		model.ProposalStatusRolledBack,
		model.ProposalStatusFailedRolledback,
		model.ProposalStatusRollbackFailed:
		return true
	default:
		return false
	}
}

// ProposalTransitionAudit describes an immutable audit entry written atomically on state transition.
type ProposalTransitionAudit struct {
	Context     context.Context
	OperationID string
	EventType   string
	Actor       string
	Level       string
	Message     string
	Metadata    map[string]interface{}
}

// ProposalTransitionParams specifies strict CAS preconditions for ProposalFSM.TransitionStrict.
type ProposalTransitionParams struct {
	ProposalID             string
	ExpectedStatuses       []string
	TargetStatus           string
	RequireVersion         *int
	RequireRevision        *int
	RequireExecutionToken  string
	Updates                map[string]interface{}
	Audit                  *ProposalTransitionAudit
}

// ProposalFSM manages atomic Compare-And-Swap state transitions for OpsProposal.
type ProposalFSM struct {
	db          *gorm.DB
	bus         *eventbus.Bus
	auditEngine *audit.AgentAuditEngine
}

// NewProposalFSM initializes a ProposalFSM.
func NewProposalFSM(db *gorm.DB) *ProposalFSM {
	return &ProposalFSM{db: db}
}

// NewProposalFSMWithBus initializes a ProposalFSM with EventBus audit publishing.
func NewProposalFSMWithBus(db *gorm.DB, bus *eventbus.Bus) *ProposalFSM {
	return &ProposalFSM{db: db, bus: bus}
}

// SetAuditEngine configures the unified AgentAuditEngine for FSM state transition recording.
func (fsm *ProposalFSM) SetAuditEngine(engine *audit.AgentAuditEngine) {
	if fsm != nil {
		fsm.auditEngine = engine
	}
}

func (fsm *ProposalFSM) resolvedAuditEngine() *audit.AgentAuditEngine {
	if fsm == nil || fsm.db == nil {
		return nil
	}
	if fsm.auditEngine != nil {
		return fsm.auditEngine
	}
	fsm.auditEngine = audit.NewAgentAuditEngine(fsm.db, fsm.bus, audit.WithFailureCallback(GlobalSecurityMetrics.IncAuditFailed))
	return fsm.auditEngine
}

// TransitionCAS provides backward-compatible 4-arg CAS filtering expectedStatuses against AllowedProposalTransitions.
func (fsm *ProposalFSM) TransitionCAS(
	proposalID string,
	expectedStatuses []string,
	targetStatus string,
	updates map[string]interface{},
) (bool, error) {
	validExpected := make([]string, 0, len(expectedStatuses))
	for _, st := range expectedStatuses {
		if IsProposalTransitionAllowed(st, targetStatus) {
			validExpected = append(validExpected, st)
		}
	}
	if len(validExpected) == 0 {
		GlobalSecurityMetrics.IncProposalCASConflict()
		return false, nil
	}
	won, err := fsm.TransitionStrict(ProposalTransitionParams{
		ProposalID:       proposalID,
		ExpectedStatuses: validExpected,
		TargetStatus:     targetStatus,
		Updates:          updates,
	})
	if errors.Is(err, ErrInvalidProposalTransition) {
		return false, nil
	}
	return won, err
}

// TransitionStrict validates the state transition matrix, enforces atomic CAS preconditions
// (ID/ProposalID, Status IN (...), optional Version, Revision, ExecutionToken), increments Revision,
// and optionally writes the transition audit record inside the same database transaction (R4-01 ~ R4-03).
func (fsm *ProposalFSM) TransitionStrict(p ProposalTransitionParams) (bool, error) {
	targetStatus := strings.ToUpper(strings.TrimSpace(p.TargetStatus))
	if targetStatus == "" || len(p.ExpectedStatuses) == 0 {
		return false, ErrInvalidProposalTransition
	}

	validExpected := make([]string, 0, len(p.ExpectedStatuses))
	for _, st := range p.ExpectedStatuses {
		upper := strings.ToUpper(strings.TrimSpace(st))
		if IsProposalTransitionAllowed(upper, targetStatus) {
			validExpected = append(validExpected, upper)
		}
	}
	if len(validExpected) == 0 {
		GlobalSecurityMetrics.IncProposalCASConflict()
		return false, fmt.Errorf("%w: cannot transition from %v to %s", ErrInvalidProposalTransition, p.ExpectedStatuses, targetStatus)
	}

	if fsm == nil || fsm.db == nil {
		return true, nil
	}

	now := time.Now().UTC()
	updates := make(map[string]interface{}, len(p.Updates)+4)
	for k, v := range p.Updates {
		updates[k] = v
	}
	updates["status"] = targetStatus
	updates["updated_at"] = now
	updates["revision"] = gorm.Expr("COALESCE(revision, 1) + 1")

	var won bool
	txErr := fsm.db.Transaction(func(tx *gorm.DB) error {
		q := tx.Model(&model.OpsProposal{}).
			Where("(id = ? OR proposal_id = ?) AND status IN (?)", p.ProposalID, p.ProposalID, validExpected)

		if p.RequireVersion != nil {
			q = q.Where("version = ?", *p.RequireVersion)
		}
		if p.RequireRevision != nil {
			q = q.Where("COALESCE(revision, 1) = ?", *p.RequireRevision)
		}
		if p.RequireExecutionToken != "" {
			q = q.Where("execution_token = ?", p.RequireExecutionToken)
		}

		res := q.Updates(updates)
		if res.Error != nil {
			GlobalSecurityMetrics.IncDatabaseError()
			return res.Error
		}
		if res.RowsAffected == 0 {
			won = false
			return nil
		}
		won = true

		if p.Audit != nil {
			hasLogTbl, tblErr := tableExists(tx, "system_logs")
			if tblErr != nil {
				return tblErr
			}
			if hasLogTbl {
				metaMap := make(map[string]interface{}, len(p.Audit.Metadata)+4)
				for k, v := range p.Audit.Metadata {
					metaMap[k] = redact.Value(v)
				}
				metaMap["proposal_id"] = p.ProposalID
				metaMap["to_status"] = targetStatus
				metaMap["actor"] = redact.SensitiveString(p.Audit.Actor)
				metaMap["operation_id"] = p.Audit.OperationID
				metaBytes, _ := json.Marshal(metaMap)

				lvl := p.Audit.Level
				if lvl == "" {
					lvl = model.LevelInfo
				}
				evType := p.Audit.EventType
				if evType == "" {
					evType = "ops.proposal.transition"
				}
				entry := model.SystemLog{
					HouseholdID: 1,
					Ts:          now.Unix(),
					EventType:   evType,
					Level:       lvl,
					Source:      eventbus.SourceSystem,
					Context:     p.ProposalID,
					Message:     redact.SensitiveString(p.Audit.Message),
					Payload:     redact.SensitiveString(string(metaBytes)),
				}
				if err := tx.Create(&entry).Error; err != nil {
					GlobalSecurityMetrics.IncAuditFailed()
					return err
				}
			} else {
				log.Printf("[ops-audit] system_logs table absent during transition audit for %s -> %s", p.ProposalID, targetStatus)
			}
		}
		return nil
	})
	if txErr != nil {
		return false, txErr
	}

	if !won {
		GlobalSecurityMetrics.IncProposalCASConflict()
	} else {
		if ae := fsm.resolvedAuditEngine(); ae != nil {
			var propRow model.OpsProposal
			_ = fsm.db.Where("id = ? OR proposal_id = ?", p.ProposalID, p.ProposalID).First(&propRow).Error
			hhID := propRow.HouseholdID
			if hhID == 0 {
				hhID = 1
			}
			actor := "proposal-fsm"
			opID := ""
			var msg string
			var meta map[string]interface{}
			var evCtx context.Context
			if p.Audit != nil {
				evCtx = p.Audit.Context
				if strings.TrimSpace(p.Audit.Actor) != "" {
					actor = p.Audit.Actor
				}
				opID = p.Audit.OperationID
				msg = p.Audit.Message
				meta = p.Audit.Metadata
			}
			if evCtx == nil {
				evCtx = context.Background()
			}
			canonEventType, runFinalStatus, dec := mapProposalStatusToAudit(targetStatus)
			_, _ = ae.EmitEvent(evCtx, audit.EmitEventInput{
				HouseholdID:    hhID,
				TriggerSource:  "proposal_fsm",
				ActorType:      "user",
				ActorID:        actor,
				EventType:      canonEventType,
				Action:         "proposal." + strings.ToLower(targetStatus),
				Target:         propRow.TargetComponent,
				Component:      propRow.TargetComponent,
				Decision:       dec,
				RiskLevel:      model.AuditRiskHigh,
				ProposalID:     defaultPropStr(propRow.ProposalID, p.ProposalID),
				ApprovalID:     propRow.ApprovalDigest,
				OperationID:    opID,
				AfterState:     map[string]interface{}{"status": targetStatus, "revision": propRow.Revision, "message": msg},
				Parameters:     meta,
				Result:         targetStatus,
				FinalRunStatus: runFinalStatus,
				MirrorLegacy:   IsTerminalProposalStatus(targetStatus) || targetStatus == model.ProposalStatusApproved || targetStatus == model.ProposalStatusScheduled,
				LegacyApprover: propRow.ApprovedBy,
			})
		}
		if fsm.bus != nil && p.Audit != nil {
			payloadBytes, _ := json.Marshal(map[string]any{
				"event":        p.Audit.EventType,
				"proposal_id":  p.ProposalID,
				"to_status":    targetStatus,
				"actor":        redact.SensitiveString(p.Audit.Actor),
				"operation_id": p.Audit.OperationID,
				"ts":           now.Unix(),
			})
			sev := eventbus.SeverityInfo
			if p.Audit.Level == model.LevelWarning {
				sev = eventbus.SeverityWarn
			} else if p.Audit.Level == model.LevelCritical {
				sev = eventbus.SeverityCritical
			}
			fsm.bus.Publish(eventbus.Event{
				Topic:    eventbus.TopicSystemLog,
				Source:   eventbus.SourceSystem,
				Severity: sev,
				Payload:  payloadBytes,
			})
		}
	}
	return won, nil
}

func mapProposalStatusToAudit(status string) (eventType, runFinalStatus, decision string) {
	switch status {
	case model.ProposalStatusApproved, model.ProposalStatusScheduled:
		return model.AuditEventApprovalGranted, "", model.AuditDecisionAllow
	case model.ProposalStatusRejected:
		return model.AuditEventApprovalRejected, model.AuditRunStatusFailed, model.AuditDecisionDeny
	case model.ProposalStatusExpired:
		return model.AuditEventApprovalExpired, model.AuditRunStatusFailed, model.AuditDecisionDeny
	case model.ProposalStatusCancelled:
		return model.AuditEventExecutionCancelled, model.AuditRunStatusCancelled, model.AuditDecisionDeny
	case model.ProposalStatusExecuting:
		return model.AuditEventExecutionLockAcquired, "", model.AuditDecisionAllow
	case model.ProposalStatusExecuted, model.ProposalStatusSuccess:
		return model.AuditEventExecutionCompleted, model.AuditRunStatusCompleted, model.AuditDecisionAllow
	case model.ProposalStatusRollingBack:
		return model.AuditEventRollbackStarted, "", model.AuditDecisionAllow
	case model.ProposalStatusRolledBack, model.ProposalStatusFailedRolledback:
		return model.AuditEventRollbackCompleted, model.AuditRunStatusRolledBack, model.AuditDecisionAllow
	case model.ProposalStatusExecutionUnknown, model.ProposalStatusReconciliationRequired:
		return model.AuditEventExecutionUnknown, model.AuditRunStatusExecutionUnknown, model.AuditDecisionAllow
	default:
		return model.AuditEventExecutionFailed, model.AuditRunStatusFailed, model.AuditDecisionAllow
	}
}

func defaultPropStr(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

// ReconcileProposal transitions an EXECUTION_UNKNOWN or RECONCILIATION_REQUIRED proposal
// to a verified outcome state under explicit human/operator reconciliation (R4-02).
func (fsm *ProposalFSM) ReconcileProposal(proposalID string, resolvedStatus string, actor string, note string) (*model.OpsProposal, error) {
	resolvedStatus = strings.ToUpper(strings.TrimSpace(resolvedStatus))
	allowedSource := []string{
		model.ProposalStatusExecutionUnknown,
		model.ProposalStatusReconciliationRequired,
	}
	if !IsProposalTransitionAllowed(model.ProposalStatusExecutionUnknown, resolvedStatus) &&
		!IsProposalTransitionAllowed(model.ProposalStatusReconciliationRequired, resolvedStatus) {
		return nil, fmt.Errorf("%w: target status %s is not a valid reconciliation target", ErrInvalidProposalTransition, resolvedStatus)
	}

	now := time.Now().UTC()
	updates := map[string]interface{}{
		"reconciled_at": utils.NullTime{Time: now, Valid: true},
		"execution_log": fmt.Sprintf("Reconciled to %s by %s at %s: %s", resolvedStatus, actor, now.Format(time.RFC3339), note),
	}
	won, err := fsm.TransitionStrict(ProposalTransitionParams{
		ProposalID:       proposalID,
		ExpectedStatuses: allowedSource,
		TargetStatus:     resolvedStatus,
		Updates:          updates,
		Audit: &ProposalTransitionAudit{
			OperationID: GenerateOperationID("reconcile"),
			EventType:   "ops.proposal.reconciled",
			Actor:       actor,
			Level:       model.LevelWarning,
			Message:     fmt.Sprintf("管理员 %s 核对并将不确定提案 [%s] 人工收敛为 %s: %s", actor, proposalID, resolvedStatus, note),
			Metadata:    map[string]interface{}{"note": note},
		},
	})
	if err != nil {
		return nil, err
	}
	if !won {
		return nil, ErrProposalCASConflict
	}

	var latest model.OpsProposal
	if err := fsm.db.Where("id = ? OR proposal_id = ?", proposalID, proposalID).First(&latest).Error; err != nil {
		return nil, err
	}
	return &latest, nil
}
