package ops

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

// ApproveProposalRequest represents approval payload options.
type ApproveProposalRequest struct {
	Action       string `json:"action"`        // "now" | "schedule"
	ScheduleTime string `json:"schedule_time"` // e.g. "03:00"
}

// ComputeApprovalDigest computes a deterministic SHA-256 digest over normalized canonical JSON of:
// target_component, execution_action, canonical sorted parameters, baseline_hash, and version.
func ComputeApprovalDigest(target string, action string, params interface{}, baseline string, version int) string {
	var paramMap map[string]interface{}
	switch v := params.(type) {
	case map[string]interface{}:
		paramMap = v
	case string:
		if strings.TrimSpace(v) != "" {
			_ = json.Unmarshal([]byte(v), &paramMap)
		}
	}
	return model.ComputeApprovalDigest(target, action, paramMap, baseline, version)
}

// ComputeProposalApprovalDigest extracts proposal attributes and generates the canonical approval digest.
func ComputeProposalApprovalDigest(p *model.OpsProposal) (string, error) {
	return model.ComputeProposalApprovalDigest(p)
}

// ListProposals lists pending system proposals.
//
//	Route: GET /api/v1/ops/proposals
func (h *OpsHandler) ListProposals(c *gin.Context) {
	var proposals []model.OpsProposal

	query := h.db.Model(&model.OpsProposal{})
	if hid := c.GetUint("household_id"); hid != 0 {
		query = query.Where("household_id = ?", hid)
	}
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}

	if err := query.Order("id DESC").Find(&proposals).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if proposals == nil {
		proposals = []model.OpsProposal{}
	}

	if c.Query("envelope") == "true" || c.GetHeader("X-Envelope") == "true" {
		utils.Success(c, proposals)
		return
	}

	c.JSON(http.StatusOK, proposals)
}

// CreateProposal creates a new pending proposal (used by ops daemon or administrators).
//
//	Route: POST /api/v1/ops/proposals
func (h *OpsHandler) CreateProposal(c *gin.Context) {
	// Guard against unauthorized proposal creation (admin or internal service daemon)
	if !c.GetBool("is_admin") && !c.GetBool("is_internal_service") {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "Permission denied: admin role or internal service required",
			"message": "权限不足，无法创建系统提案",
		})
		return
	}

	var req model.OpsProposal
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid proposal payload: " + err.Error()})
		return
	}

	if strings.TrimSpace(req.Title) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Title is required"})
		return
	}

	if hid := c.GetUint("household_id"); hid != 0 {
		req.HouseholdID = hid
	} else {
		req.HouseholdID = 1
	}

	if req.RiskLevel == "" {
		req.RiskLevel = model.RiskLevelLow
	}
	// Proposal status hardening: strictly enforce initial state as AWAITING_APPROVAL
	// and clear any forged approval/execution metadata from caller.
	req.Status = model.ProposalStatusAwaitingApproval
	req.ApprovedBy = ""
	req.ApprovedAt = utils.NullTime{}
	req.ExecutedAt = utils.NullTime{}
	req.ExecutionLog = ""
	req.ExecutionToken = ""
	if req.EstimatedSeconds == 0 {
		req.EstimatedSeconds = 30
	}
	if req.ProposalID == "" {
		req.ProposalID = fmt.Sprintf("prop-%d", time.Now().UnixNano()/1e6)
	}
	// Canonical component and BaselineHash Anti-Tampering (R4-17, R5)
	if req.TargetComponent != "" {
		canonicalTarget := model.CanonicalizeComponent(req.TargetComponent)
		if canonicalTarget == "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   ErrComponentNotWhitelisted.Error(),
				"message": "target component is unknown or not permitted",
			})
			return
		}
		computed := h.ComputeFingerprint(req.TargetComponent)
		if computed == "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   ErrEmptyBaseline.Error(),
				"message": "failed to compute valid baseline state fingerprint; proposal creation rejected",
			})
			return
		}
		// Authoritative system baseline: overwrite any unverified caller baseline
		req.BaselineHash = computed
	}
	if strings.TrimSpace(req.BaselineHash) == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   ErrEmptyBaseline.Error(),
			"message": "proposal requires a non-empty valid baseline hash",
		})
		return
	}
	req.Version = 1
	req.Revision = 1
	req.ApprovalDigest = ""
	req.CreatedAt = time.Now().UTC()

	// Validate action_spec structure strictly at creation time if provided
	if strings.TrimSpace(req.ActionSpec) != "" {
		exec := &DefaultOperationExecutor{}
		if _, err := exec.parseProposalSpec(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   err.Error(),
				"message": "invalid or mismatched action_spec payload",
			})
			return
		}
	}

	if err := h.db.Create(&req).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to persist proposal: " + err.Error()})
		return
	}

	// Publish proposal creation event to EventBus
	if h.bus != nil {
		payloadBytes, _ := json.Marshal(req)
		h.bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicSystemLog,
			Source:   eventbus.SourceSystem,
			Severity: eventbus.SeverityInfo,
			Payload:  payloadBytes,
		})
	}

	if c.Query("envelope") == "true" || c.GetHeader("X-Envelope") == "true" {
		utils.Success(c, req)
		return
	}

	c.JSON(http.StatusCreated, req)
}

// ApproveProposal approves a proposal for execution or low-peak scheduling.
//
//	Route: POST /api/v1/ops/proposals/:id/approve
func (h *OpsHandler) ApproveProposal(c *gin.Context) {
	// 1. Role permission gate
	if c.GetBool("is_internal_service") {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "Permission denied: internal daemon cannot approve proposals",
			"message": "内部服务探针无权执行提案审批，必须由管理员人工审批",
		})
		return
	}

	isAdmin := c.GetBool("is_admin")
	if !isAdmin {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "Permission denied: admin role required",
			"message": "管理员权限不足，无法执行提案审批",
		})
		return
	}

	idParam := strings.TrimSpace(c.Param("id"))
	if idParam == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Proposal ID is required"})
		return
	}

	var proposal model.OpsProposal
	if err := h.db.Where("id = ? OR proposal_id = ?", idParam, idParam).First(&proposal).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error":   "Proposal not found",
			"message": fmt.Sprintf("未找到提案: %s", idParam),
		})
		return
	}

	callerHouseholdID := c.GetUint("household_id")
	if callerHouseholdID != 0 && proposal.HouseholdID != 0 && proposal.HouseholdID != callerHouseholdID {
		c.JSON(http.StatusNotFound, gin.H{
			"error":   "Proposal not found",
			"message": fmt.Sprintf("未找到提案: %s", idParam),
		})
		return
	}

	// Only proposals in pending approval states can be approved
	if proposal.Status != model.ProposalStatusAwaitingApproval &&
		proposal.Status != model.ProposalStatusApprovalRequired &&
		proposal.Status != model.ProposalStatusPending {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("Invalid state transition from %s: must be AWAITING_APPROVAL/APPROVAL_REQUIRED first", proposal.Status),
		})
		return
	}

	// Anti-TOCTOU BaselineHash Verification (Fail-Closed: calculation failure or drift strictly blocks approval)
	if strings.TrimSpace(proposal.BaselineHash) == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   ErrEmptyBaseline.Error(),
			"message": "提案未包含有效基线指纹 (BaselineHash)，无法审批",
		})
		return
	}
	currentHash := h.ComputeFingerprint(proposal.TargetComponent)
	if currentHash == "" || currentHash != proposal.BaselineHash {
		c.JSON(http.StatusConflict, gin.H{
			"error":         ErrBaselineMismatch.Error(),
			"message":       "目标资源状态在提案创建后已发生漂移或指纹提取失败 (TOCTOU)，请重新生成或确认提案",
			"baseline_hash": proposal.BaselineHash,
			"current_hash":  currentHash,
		})
		return
	}

	var req ApproveProposalRequest
	_ = c.ShouldBindJSON(&req)

	// R4-12/13: Always derive operator identity from authenticated server context;
	// never trust any request-body approver field. Truncate to second granularity for deterministic SQLite roundtrip.
	operator := getOperatorName(c)
	now := time.Now().UTC().Truncate(time.Second)

	targetStatus := model.ProposalStatusApproved
	var schedPtr *string
	if strings.EqualFold(req.Action, "schedule") {
		schedTime := "03:00"
		if req.ScheduleTime != "" {
			schedTime = req.ScheduleTime
		}
		targetStatus = model.ProposalStatusScheduled
		schedPtr = &schedTime
		proposal.ScheduledAt = schedPtr
	}

	proposal.Status = targetStatus
	proposal.ApprovedBy = operator
	proposal.ApprovedAt = utils.NullTime{Time: now, Valid: true}

	if proposal.Version <= 0 {
		proposal.Version = 1
	}
	if proposal.Revision <= 0 {
		proposal.Revision = 1
	}

	digest, err := ComputeProposalApprovalDigest(&proposal)
	if err != nil || digest == "" {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("Failed to compute proposal approval digest: %v", err),
		})
		return
	}

	expectedStatuses := []string{
		model.ProposalStatusPending,
		model.ProposalStatusAwaitingApproval,
		model.ProposalStatusApprovalRequired,
	}
	reqVer := proposal.Version
	reqRev := proposal.Revision
	opID := GenerateOperationID("approve")

	updates := map[string]interface{}{
		"approved_by":     operator,
		"approved_at":     proposal.ApprovedAt,
		"approval_digest": digest,
	}
	if schedPtr != nil {
		updates["scheduled_at"] = *schedPtr
	}

	fsm := NewProposalFSMWithBus(h.db, h.bus)
	won, casErr := fsm.TransitionStrict(ProposalTransitionParams{
		ProposalID:       idParam,
		ExpectedStatuses: expectedStatuses,
		TargetStatus:     targetStatus,
		RequireVersion:   &reqVer,
		RequireRevision:  &reqRev,
		Updates:          updates,
		Audit: &ProposalTransitionAudit{
			OperationID: opID,
			EventType:   "ops.proposal.approved",
			Actor:       operator,
			Level:       model.LevelWarning,
			Message:     fmt.Sprintf("管理员 %s 审批通过系统提案 [%s] (%s): %s", operator, proposal.ProposalID, proposal.Title, req.Action),
			Metadata: map[string]interface{}{
				"title":         proposal.Title,
				"risk_level":    proposal.RiskLevel,
				"action":        req.Action,
				"rollback_plan": proposal.RollbackPlan,
				"version":       reqVer,
			},
		},
	})
	if casErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to approve proposal via CAS: " + casErr.Error()})
		return
	}
	if !won {
		// Reload authoritative DB state so caller sees why CAS lost (e.g. CANCELLED concurrently)
		var authProp model.OpsProposal
		_ = h.db.Where("id = ? OR proposal_id = ?", idParam, idParam).First(&authProp).Error
		c.JSON(http.StatusConflict, gin.H{
			"error":         ErrProposalCASConflict.Error(),
			"message":       "审批 CAS 冲突：提案状态已由其他请求并发变更，已中止审批且不会触发执行",
			"actual_status": authProp.Status,
			"proposal":      authProp,
		})
		return
	}

	// Reload authoritative persisted proposal after CAS win before dispatching execution
	var latest model.OpsProposal
	if err := h.db.Where("id = ? OR proposal_id = ?", idParam, idParam).First(&latest).Error; err == nil {
		proposal = latest
	} else {
		proposal.ApprovalDigest = digest
		proposal.Revision = reqRev + 1
	}

	// Dispatch execution through OperationExecutor for non-scheduled approvals
	if !strings.EqualFold(req.Action, "schedule") {
		exec := h.GetExecutor()
		if exec != nil {
			if c.Query("sync") == "true" {
				res, err := exec.ExecuteProposal(c.Request.Context(), &proposal)
				if err != nil {
					log.Printf("[ops] synchronous proposal %s execution failed: %v", proposal.ProposalID, err)
				} else if res != nil {
					log.Printf("[ops] synchronous proposal %s execution succeeded: %s", proposal.ProposalID, res.Output)
				}
			} else {
				go func(prop model.OpsProposal) {
					ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
					defer cancel()
					_, _ = exec.ExecuteProposal(ctx, &prop)
				}(proposal)
			}
		}
	}

	if c.Query("envelope") == "true" || c.GetHeader("X-Envelope") == "true" {
		utils.Success(c, proposal)
		return
	}

	c.JSON(http.StatusOK, proposal)
}

// ExecuteProposalEndpoint handles POST /proposals/:id/execute
// Allows authorized callers (admin or internal service) to trigger execution of an already
// APPROVED or SCHEDULED proposal. Strictly rejects execution of unapproved proposals.
func (h *OpsHandler) ExecuteProposalEndpoint(c *gin.Context) {
	id := c.Param("id")
	var proposal model.OpsProposal
	if err := h.db.Where("id = ? OR proposal_id = ?", id, id).First(&proposal).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Proposal not found"})
		return
	}

	// Require either admin or internal service
	isAdmin := c.GetBool("is_admin")
	isInternal := c.GetBool("is_internal_service")
	if !isAdmin && !isInternal {
		c.JSON(http.StatusForbidden, gin.H{"error": "Permission denied: admin or internal service required"})
		return
	}

	callerHouseholdID := c.GetUint("household_id")
	if !isInternal && callerHouseholdID != 0 && proposal.HouseholdID != 0 && proposal.HouseholdID != callerHouseholdID {
		c.JSON(http.StatusNotFound, gin.H{"error": "Proposal not found"})
		return
	}

	// 1. Unapproved proposal states strictly require approval first (HTTP 400 Bad Request)
	if proposal.Status == model.ProposalStatusAwaitingApproval || proposal.Status == model.ProposalStatusPending || proposal.Status == model.ProposalStatusApprovalRequired {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":  fmt.Sprintf("proposal %s requires approval before execution (status: %s)", proposal.ProposalID, proposal.Status),
			"status": proposal.Status,
		})
		return
	}

	// 2. Non-executable states (CANCELLED, EXECUTING, ROLLING_BACK, EXECUTED, SUCCESS, FAILED, ROLLED_BACK, etc.) return HTTP 409 Conflict
	if proposal.Status != model.ProposalStatusApproved && proposal.Status != model.ProposalStatusScheduled {
		c.JSON(http.StatusConflict, gin.H{
			"error":  fmt.Sprintf("proposal %s is not in an executable state (status: %s)", proposal.ProposalID, proposal.Status),
			"status": proposal.Status,
		})
		return
	}

	exec := h.GetExecutor()
	if exec == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Operation executor not configured"})
		return
	}

	res, err := exec.ExecuteProposal(c.Request.Context(), &proposal)
	if err != nil {
		if errors.Is(err, ErrProposalUnauthorized) || errors.Is(err, ErrScheduledWindowNotReached) || errors.Is(err, ErrEmptyBaseline) {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":  err.Error(),
				"status": proposal.Status,
			})
			return
		}
		if errors.Is(err, ErrProposalAlreadyRunning) || errors.Is(err, ErrProposalNotExecutable) ||
			errors.Is(err, ErrApprovalDigestMismatch) || errors.Is(err, ErrBaselineMismatch) {
			c.JSON(http.StatusConflict, gin.H{
				"error":  err.Error(),
				"status": proposal.Status,
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "result": res})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": proposal.Status,
		"result": res,
	})
}

// ExecuteProposal dispatches proposal execution to the OperationExecutor directly.
func (h *OpsHandler) ExecuteProposal(ctx context.Context, proposalID string) (*ExecutionResult, error) {
	exec := h.GetExecutor()
	if exec == nil {
		return nil, errors.New("operation executor not configured")
	}

	var proposal model.OpsProposal
	if err := h.db.Where("id = ? OR proposal_id = ?", proposalID, proposalID).First(&proposal).Error; err != nil {
		return nil, fmt.Errorf("proposal not found: %w", err)
	}

	return exec.ExecuteProposal(ctx, &proposal)
}

// CancelProposal handles POST /api/v1/ops/proposals/:id/cancel
// Rejects cancellation if proposal is currently EXECUTING or ROLLING_BACK (HTTP 409).
// Atomically transitions eligible proposals (PENDING, AWAITING_APPROVAL, APPROVED, SCHEDULED) to CANCELLED via CAS.
func (h *OpsHandler) CancelProposal(c *gin.Context) {
	idParam := strings.TrimSpace(c.Param("id"))
	if idParam == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Proposal ID is required"})
		return
	}

	operator := getOperatorName(c)

	var prop model.OpsProposal
	if err := h.db.Where("id = ? OR proposal_id = ?", idParam, idParam).First(&prop).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Proposal not found"})
		return
	}

	// Multi-tenant household anti-IDOR validation
	callerHouseholdID := c.GetUint("household_id")
	isInternal := c.GetBool("is_internal_service")
	if !isInternal && callerHouseholdID != 0 && prop.HouseholdID != 0 && prop.HouseholdID != callerHouseholdID {
		c.JSON(http.StatusNotFound, gin.H{"error": "Proposal not found"})
		return
	}

	// Active execution cannot be cancelled
	if prop.Status == model.ProposalStatusExecuting || prop.Status == model.ProposalStatusRollingBack {
		c.JSON(http.StatusConflict, gin.H{
			"error":  "Cannot cancel proposal: execution is actively in progress",
			"status": prop.Status,
		})
		return
	}

	// Allowed cancel statuses
	allowedCancelStatuses := []string{
		model.ProposalStatusPending,
		model.ProposalStatusAwaitingApproval,
		model.ProposalStatusApprovalRequired,
		model.ProposalStatusApproved,
		model.ProposalStatusScheduled,
	}

	reqVer := prop.Version
	if reqVer <= 0 {
		reqVer = 1
	}
	reqRev := prop.Revision
	if reqRev <= 0 {
		reqRev = 1
	}

	fsm := NewProposalFSMWithBus(h.db, h.bus)
	won, err := fsm.TransitionStrict(ProposalTransitionParams{
		ProposalID:       idParam,
		ExpectedStatuses: allowedCancelStatuses,
		TargetStatus:     model.ProposalStatusCancelled,
		RequireVersion:   &reqVer,
		RequireRevision:  &reqRev,
		Updates: map[string]interface{}{
			"execution_log": fmt.Sprintf("Cancelled by %s at %s", operator, time.Now().UTC().Format(time.RFC3339)),
		},
		Audit: &ProposalTransitionAudit{
			OperationID: GenerateOperationID("cancel"),
			EventType:   "ops.proposal.cancelled",
			Actor:       operator,
			Level:       model.LevelWarning,
			Message:     fmt.Sprintf("操作者 %s 取消系统提案 [%s]", operator, prop.ProposalID),
		},
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !won {
		var authProp model.OpsProposal
		_ = h.db.Where("id = ? OR proposal_id = ?", idParam, idParam).First(&authProp).Error
		c.JSON(http.StatusConflict, gin.H{
			"error":         "Cancel race lost: proposal state changed concurrently",
			"actual_status": authProp.Status,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Proposal cancelled successfully",
		"status":  model.ProposalStatusCancelled,
	})
}

// ReconcileProposalEndpoint handles POST /api/v1/ops/proposals/:id/reconcile (Admin only).
// Explicitly resolves EXECUTION_UNKNOWN or RECONCILIATION_REQUIRED proposals via ProposalFSM (R4-02).
func (h *OpsHandler) ReconcileProposalEndpoint(c *gin.Context) {
	if !c.GetBool("is_admin") || c.GetBool("is_internal_service") {
		c.JSON(http.StatusForbidden, gin.H{"error": "Permission denied: human admin role required for proposal reconciliation"})
		return
	}

	idParam := strings.TrimSpace(c.Param("id"))
	if idParam == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Proposal ID is required"})
		return
	}

	var req struct {
		ResolvedStatus string `json:"resolved_status" binding:"required"`
		Note           string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid reconciliation payload: " + err.Error()})
		return
	}

	operator := getOperatorName(c)
	fsm := NewProposalFSMWithBus(h.db, h.bus)
	updated, err := fsm.ReconcileProposal(idParam, req.ResolvedStatus, operator, req.Note)
	if err != nil {
		if errors.Is(err, ErrInvalidProposalTransition) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if errors.Is(err, ErrProposalCASConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":  "Proposal reconciled successfully",
		"proposal": updated,
	})
}

// recordProposalAudit logs approval action to EventBus and system_logs.
func (h *OpsHandler) recordProposalAudit(prop *model.OpsProposal, operator, action string) {
	actionDesc := "立即批准执行"
	if prop.Status == model.ProposalStatusScheduled {
		schedVal := "03:00"
		if prop.ScheduledAt != nil {
			schedVal = *prop.ScheduledAt
		}
		actionDesc = fmt.Sprintf("预约低峰期 %s 自动执行", schedVal)
	}

	logMsg := fmt.Sprintf("管理员 %s 审批通过系统提案 [%s] (%s): %s", operator, prop.ProposalID, prop.Title, actionDesc)

	payloadMap := map[string]any{
		"event":         "ops.proposal.approved",
		"proposal_id":   prop.ProposalID,
		"title":         prop.Title,
		"risk_level":    prop.RiskLevel,
		"status":        prop.Status,
		"operator":      operator,
		"action":        action,
		"rollback_plan": prop.RollbackPlan,
		"ts":            time.Now().Unix(),
	}
	payloadBytes, _ := json.Marshal(payloadMap)

	if h.bus != nil {
		h.bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicSystemLog,
			Source:   eventbus.SourceSystem,
			Severity: eventbus.SeverityWarn,
			Payload:  payloadBytes,
		})
	}

	if h.db != nil {
		entry := model.SystemLog{
			Ts:        time.Now().Unix(),
			EventType: "ops.proposal.approved",
			Level:     model.LevelWarning,
			Source:    eventbus.SourceSystem,
			Context:   prop.ProposalID,
			Message:   logMsg,
			Payload:   string(payloadBytes),
		}
		if err := h.db.Create(&entry).Error; err != nil {
			log.Printf("[ops] failed to persist proposal audit log: %v", err)
		}
	}
}

// ComputeFingerprint calculates a deterministic SHA-256 baseline state fingerprint
// for the given target component to protect against TOCTOU state drift.
func (h *OpsHandler) ComputeFingerprint(component string) string {
	clean := strings.TrimSpace(component)
	if clean == "" {
		return ""
	}

	canonical, err := ResolveComponent(clean)
	if err != nil {
		canonical = model.CanonicalizeComponent(clean)
		if canonical == "" {
			return ""
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var targetContainer *ContainerSummary

	// 1. Try querying via 1PanelClient if available
	if h.client != nil {
		if containers, err := h.client.ListContainers(ctx); err == nil {
			for i := range containers {
				cName := strings.TrimPrefix(containers[i].Name, "/")
				if strings.EqualFold(cName, canonical) {
					targetContainer = &containers[i]
					break
				}
			}
		}
	}

	// 2. Fallback to host Docker inspection via HostFallbackProvider
	if targetContainer == nil && h.fallback != nil {
		fallbackContainers := h.fallback.GetHostContainers(ctx)
		for i := range fallbackContainers {
			cName := strings.TrimPrefix(fallbackContainers[i].Name, "/")
			if strings.EqualFold(cName, canonical) {
				targetContainer = &fallbackContainers[i]
				break
			}
		}
	}

	hasher := sha256.New()
	if targetContainer != nil {
		raw := fmt.Sprintf("%s:%s:%s:%s", canonical, targetContainer.Image, targetContainer.State, targetContainer.CreatedAt)
		hasher.Write([]byte(raw))
	} else {
		// Deterministic baseline for offline, uncreated, or test components
		raw := fmt.Sprintf("%s:offline:none", canonical)
		hasher.Write([]byte(raw))
	}

	return fmt.Sprintf("sha256:%x", hasher.Sum(nil))
}

// InvalidateProposalApproval resets the approval status of a proposal when any bound field is modified.
// It sets Status to PENDING, clears ApprovalDigest and approval metadata, and increments Version.
func InvalidateProposalApproval(p *model.OpsProposal) {
	if p == nil {
		return
	}
	p.Status = model.ProposalStatusPending
	p.ApprovalDigest = ""
	p.ApprovedBy = ""
	p.ApprovedAt = utils.NullTime{}
	if p.Version <= 0 {
		p.Version = 1
	}
	p.Version++
}

// UpdateProposal updates mutable proposal fields using CAS and invalidates previous approval if any bound field changes.
func (h *OpsHandler) UpdateProposal(c *gin.Context) {
	if !c.GetBool("is_admin") && !c.GetBool("is_internal_service") {
		c.JSON(http.StatusForbidden, gin.H{"error": "Permission denied: admin role or internal service required"})
		return
	}

	id := strings.TrimSpace(c.Param("id"))
	var proposal model.OpsProposal
	if err := h.db.Where("id = ? OR proposal_id = ?", id, id).First(&proposal).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Proposal not found"})
		return
	}

	callerHouseholdID := c.GetUint("household_id")
	if !c.GetBool("is_internal_service") && callerHouseholdID != 0 && proposal.HouseholdID != 0 && proposal.HouseholdID != callerHouseholdID {
		c.JSON(http.StatusNotFound, gin.H{"error": "Proposal not found"})
		return
	}

	// Only editable prior to execution and prior to terminal states (R4-01, R4-02)
	if IsTerminalProposalStatus(proposal.Status) ||
		proposal.Status == model.ProposalStatusExecuting ||
		proposal.Status == model.ProposalStatusRollingBack ||
		proposal.Status == model.ProposalStatusExecutionUnknown ||
		proposal.Status == model.ProposalStatusReconciliationRequired {
		c.JSON(http.StatusConflict, gin.H{
			"error":  fmt.Sprintf("Cannot update proposal in immutable/active state %s", proposal.Status),
			"status": proposal.Status,
		})
		return
	}

	var updateReq struct {
		Title           *string `json:"title"`
		Description     *string `json:"description"`
		TargetComponent *string `json:"target_component"`
		ActionSpec      *string `json:"action_spec"`
		ExecutionPlan   *string `json:"execution_plan"`
		RollbackPlan    *string `json:"rollback_plan"`
		ExecutionPolicy *string `json:"execution_policy"`
		ScheduledAt     *string `json:"scheduled_at"`
		RiskLevel       *string `json:"risk_level"`
	}
	if err := c.ShouldBindJSON(&updateReq); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request payload: " + err.Error()})
		return
	}

	origStatus := proposal.Status
	origVersion := proposal.Version
	if origVersion <= 0 {
		origVersion = 1
	}
	origRev := proposal.Revision
	if origRev <= 0 {
		origRev = 1
	}

	updates := make(map[string]interface{})
	boundFieldChanged := false
	if updateReq.TargetComponent != nil && *updateReq.TargetComponent != proposal.TargetComponent {
		if model.CanonicalizeComponent(*updateReq.TargetComponent) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": ErrComponentNotWhitelisted.Error()})
			return
		}
		proposal.TargetComponent = *updateReq.TargetComponent
		fp := h.ComputeFingerprint(proposal.TargetComponent)
		if fp == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": ErrEmptyBaseline.Error()})
			return
		}
		proposal.BaselineHash = fp
		updates["target_component"] = proposal.TargetComponent
		updates["baseline_hash"] = proposal.BaselineHash
		boundFieldChanged = true
	}
	if updateReq.ActionSpec != nil && *updateReq.ActionSpec != proposal.ActionSpec {
		tmpProp := proposal
		tmpProp.ActionSpec = *updateReq.ActionSpec
		if exec, ok := h.GetExecutor().(*DefaultOperationExecutor); ok && exec != nil {
			if _, err := exec.parseProposalSpec(&tmpProp); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid action_spec: " + err.Error()})
				return
			}
		} else {
			defExec := &DefaultOperationExecutor{}
			if _, err := defExec.parseProposalSpec(&tmpProp); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid action_spec: " + err.Error()})
				return
			}
		}
		proposal.ActionSpec = *updateReq.ActionSpec
		updates["action_spec"] = proposal.ActionSpec
		boundFieldChanged = true
	}
	if updateReq.ExecutionPlan != nil && *updateReq.ExecutionPlan != proposal.ExecutionPlan {
		proposal.ExecutionPlan = *updateReq.ExecutionPlan
		updates["execution_plan"] = proposal.ExecutionPlan
		boundFieldChanged = true
	}
	if updateReq.RollbackPlan != nil && *updateReq.RollbackPlan != proposal.RollbackPlan {
		proposal.RollbackPlan = *updateReq.RollbackPlan
		updates["rollback_plan"] = proposal.RollbackPlan
		boundFieldChanged = true
	}
	if updateReq.ExecutionPolicy != nil && *updateReq.ExecutionPolicy != proposal.ExecutionPolicy {
		proposal.ExecutionPolicy = *updateReq.ExecutionPolicy
		updates["execution_policy"] = proposal.ExecutionPolicy
		boundFieldChanged = true
	}
	if updateReq.ScheduledAt != nil {
		proposal.ScheduledAt = updateReq.ScheduledAt
		updates["scheduled_at"] = *updateReq.ScheduledAt
		boundFieldChanged = true
	}
	if updateReq.Title != nil {
		proposal.Title = *updateReq.Title
		updates["title"] = proposal.Title
	}
	if updateReq.Description != nil {
		proposal.Description = *updateReq.Description
		updates["description"] = proposal.Description
	}
	if updateReq.RiskLevel != nil {
		proposal.RiskLevel = *updateReq.RiskLevel
		updates["risk_level"] = proposal.RiskLevel
	}

	if boundFieldChanged {
		InvalidateProposalApproval(&proposal)
		updates["status"] = proposal.Status
		updates["approval_digest"] = ""
		updates["approved_by"] = ""
		updates["approved_at"] = utils.NullTime{}
		updates["version"] = proposal.Version
	}

	if len(updates) == 0 {
		c.JSON(http.StatusOK, proposal)
		return
	}

	updates["updated_at"] = time.Now().UTC()
	updates["revision"] = gorm.Expr("COALESCE(revision, 1) + 1")

	res := h.db.Model(&model.OpsProposal{}).
		Where("id = ? AND status = ? AND version = ? AND COALESCE(revision, 1) = ?", proposal.ID, origStatus, origVersion, origRev).
		Updates(updates)
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update proposal: " + res.Error.Error()})
		return
	}
	if res.RowsAffected == 0 {
		GlobalSecurityMetrics.IncProposalCASConflict()
		c.JSON(http.StatusConflict, gin.H{"error": ErrProposalCASConflict.Error()})
		return
	}

	var refreshed model.OpsProposal
	if err := h.db.First(&refreshed, proposal.ID).Error; err == nil {
		proposal = refreshed
	}
	c.JSON(http.StatusOK, proposal)
}

