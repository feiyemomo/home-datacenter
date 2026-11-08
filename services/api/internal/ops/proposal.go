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

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

// ApproveProposalRequest represents approval payload options.
type ApproveProposalRequest struct {
	Action       string `json:"action"`        // "now" | "schedule"
	ScheduleTime string `json:"schedule_time"` // e.g. "03:00"
}

// ListProposals lists pending system proposals.
//
//	Route: GET /api/v1/ops/proposals
func (h *OpsHandler) ListProposals(c *gin.Context) {
	var proposals []model.OpsProposal

	query := h.db.Model(&model.OpsProposal{})
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}

	if err := query.Order("id DESC").Find(&proposals).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Seed realistic initial proposals if table is empty
	if len(proposals) == 0 && c.Query("status") == "" {
		proposals = h.seedInitialProposals()
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
	// Guard against non-admin proposal creation (defense-in-depth)
	if !c.GetBool("is_admin") {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "Permission denied: admin role required",
			"message": "管理员权限不足，无法创建系统提案",
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
	if req.EstimatedSeconds == 0 {
		req.EstimatedSeconds = 30
	}
	if req.ProposalID == "" {
		req.ProposalID = fmt.Sprintf("prop-%d", time.Now().UnixNano()/1e6)
	}
	if req.BaselineHash == "" && req.TargetComponent != "" {
		req.BaselineHash = h.ComputeFingerprint(req.TargetComponent)
	}
	req.CreatedAt = time.Now()

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

	// Only proposals in pending approval states can be approved
	if proposal.Status != model.ProposalStatusAwaitingApproval &&
		proposal.Status != model.ProposalStatusApprovalRequired &&
		proposal.Status != model.ProposalStatusPending {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("Invalid state transition from %s: must be AWAITING_APPROVAL/APPROVAL_REQUIRED first", proposal.Status),
		})
		return
	}

	// Anti-TOCTOU BaselineHash Verification
	if proposal.BaselineHash != "" {
		currentHash := h.ComputeFingerprint(proposal.TargetComponent)
		if currentHash != "" && currentHash != proposal.BaselineHash {
			c.JSON(http.StatusConflict, gin.H{
				"error":         "baseline hash mismatch: target state has drifted",
				"message":       "目标资源状态在提案创建后已发生漂移 (TOCTOU)，请重新生成或确认提案",
				"baseline_hash": proposal.BaselineHash,
				"current_hash":  currentHash,
			})
			return
		}
	}

	var req ApproveProposalRequest
	_ = c.ShouldBindJSON(&req)

	operator := getOperatorName(c)
	now := time.Now()

	if strings.EqualFold(req.Action, "schedule") {
		schedTime := "03:00"
		if req.ScheduleTime != "" {
			schedTime = req.ScheduleTime
		}
		proposal.Status = model.ProposalStatusScheduled
		proposal.ScheduledAt = &schedTime
		proposal.ApprovedBy = operator
		proposal.ApprovedAt = utils.NullTime{Time: now, Valid: true}
	} else {
		proposal.Status = model.ProposalStatusApproved
		proposal.ApprovedBy = operator
		proposal.ApprovedAt = utils.NullTime{Time: now, Valid: true}
	}

	if err := h.db.Save(&proposal).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update proposal: " + err.Error()})
		return
	}

	// Trigger execution callback via EventBus & SystemLog
	h.recordProposalAudit(&proposal, operator, req.Action)

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

// seedInitialProposals provides realistic initial proposals matching datacenter baseline.
func (h *OpsHandler) seedInitialProposals() []model.OpsProposal {
	defaults := []model.OpsProposal{
		{
			ProposalID:       "prop-101",
			Title:            "【高危】Nginx 基础镜像安全升级至 v1.25.4-alpine",
			Description:      "修复 CVE-2024-24989 缓冲区溢出漏洞，更新 TLS 1.3 密码套件配置与 HTTP/2 流控。",
			TargetComponent:  "home-web (Nginx)",
			RiskLevel:        model.RiskLevelHigh,
			EstimatedSeconds: 45,
			DowntimeSeconds:  3,
			ExecutionPlan:    "docker tag home-web:prev home-web:latest && docker-compose restart home-web",
			RollbackPlan:     "docker tag home-web:prev home-web:latest && docker-compose restart home-web",
			ActionSpec:       `{"action":"docker.restart","target":"home-web","rollback_spec":{"action":"docker.restart","target":"home-web"}}`,
			Status:           model.ProposalStatusAwaitingApproval,
			CreatedAt:        time.Now().Add(-2 * time.Hour),
		},
		{
			ProposalID:       "prop-102",
			Title:            "【中风险】SQLite / PostgreSQL 慢查询复合索引重构",
			Description:      "为 security_events (timestamp, event_type) 与 recordings (camera_id, start_time) 建立联合覆盖索引，优化查询速度。",
			TargetComponent:  "home-api (Database)",
			RiskLevel:        model.RiskLevelMedium,
			EstimatedSeconds: 90,
			DowntimeSeconds:  0,
			ExecutionPlan:    "CREATE INDEX IF NOT EXISTS idx_security_events_time_type ON security_events(created_at, type);",
			RollbackPlan:     "DROP INDEX IF EXISTS idx_security_events_time_type;",
			ActionSpec:       `{"action":"db.create_index","target":"home-api","params":{"table_name":"security_events","index_name":"idx_security_events_time_type","columns":["created_at","type"]},"rollback_spec":{"action":"db.drop_index","target":"home-api","params":{"index_name":"idx_security_events_time_type"}}}`,
			Status:           model.ProposalStatusAwaitingApproval,
			CreatedAt:        time.Now().Add(-1 * time.Hour),
		},
	}

	for i := range defaults {
		_ = h.db.Create(&defaults[i]).Error
	}

	return defaults
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
		canonical = strings.ToLower(clean)
		if idx := strings.Index(canonical, "("); idx != -1 {
			canonical = strings.TrimSpace(canonical[:idx])
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

