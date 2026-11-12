package ops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/audit"
	"home-datacenter-api/internal/hostsec"
	"home-datacenter-api/internal/middleware"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/redact"
	"home-datacenter-api/internal/utils"
)

// RegisterProposalRoutes registers pending proposals management endpoints.
// All mutating proposal endpoints (creation and approval) are strictly guarded
// with middleware.RequireAdmin and defense-in-depth context checks.
func (h *OpsHandler) RegisterProposalRoutes(rg *gin.RouterGroup) {
	rg.GET("/proposals", middleware.RequireAuthOrInternalScope(middleware.ScopeOpsProposalRead), h.ListProposals)
	rg.POST("/proposals", middleware.RequireAdminOrInternalScope(middleware.ScopeOpsProposalCreate), h.CreateProposal)
	rg.PATCH("/proposals/:id", middleware.RequireAdmin(h.db), h.UpdateProposal)
	rg.POST("/proposals/:id/approve", middleware.RequireAdmin(h.db), h.ApproveProposal)
	rg.POST("/proposals/:id/reject", middleware.RequireAdmin(h.db), h.RejectProposal)
	rg.POST("/proposals/:id/execute", middleware.RequireAdminOrInternalScope(middleware.ScopeOpsProposalExecute), h.ExecuteProposalEndpoint)
	rg.POST("/proposals/:id/cancel", middleware.RequireAdminOrInternalScope(middleware.ScopeOpsProposalExecute), h.CancelProposal)
	rg.POST("/proposals/:id/reconcile", middleware.RequireAdmin(h.db), h.ReconcileProposalEndpoint)

	// Automatically mount FSM & Maintenance Lock routes alongside proposal endpoints
	h.RegisterFSMRoutes(rg)
}

// RegisterFSMRoutes registers maintenance lock, circuit breaker, and FSM management endpoints.
func (h *OpsHandler) RegisterFSMRoutes(rg *gin.RouterGroup) {
	// Maintenance Lock endpoints
	maint := rg.Group("/maintenance")
	{
		maint.GET("/lock", h.GetMaintenanceStatus)
		maint.POST("/lock", middleware.RequireAdmin(h.db), h.AcquireMaintenanceLock)
		maint.POST("/unlock", middleware.RequireAdmin(h.db), h.ReleaseMaintenanceLock)
	}

	// Container Circuit Breaker manual reset
	rg.POST("/containers/:name/reset-circuit", middleware.RequireAdmin(h.db), h.ResetContainerCircuit)

	// FSM telemetry, reconciliation, security metrics, and manual event testing
	rg.GET("/fsm/status", h.GetFSMStatus)
	rg.POST("/fsm/event", middleware.RequireAdminOrInternalScope(middleware.ScopeOpsFSM), h.HandleContainerEvent)
	rg.POST("/fsm/reconcile", middleware.RequireAdmin(h.db), h.ReconcileFSMStorage)
	rg.GET("/security-metrics", middleware.RequireAdminOrInternalScope(middleware.ScopeOpsTelemetry), h.GetSecurityMetrics)
	rg.POST("/audit/offline-sync", middleware.OpsAgentAuth(middleware.ScopeOpsAuditSync), h.SyncOfflineAuditLogs)

	// Unified tamper-evident AgentAuditEngine query/verify/export routes
	h.RegisterAuditRoutes(rg)

	// Host OS security telemetry, CVE correlation, R9.3 monitor, and guarded updates (R9)
	h.RegisterHostRoutes(rg)
}

// RegisterHostRoutes registers Admin-only host OS security inspection, CVE correlation,
// intrusion monitor, and guarded package update endpoints (R9.1-R9.5).
func (h *OpsHandler) RegisterHostRoutes(rg *gin.RouterGroup) {
	hg := rg.Group("/host")
	{
		hg.GET("/assets", middleware.RequireAdmin(h.db), h.GetHostAssets)
		hg.GET("/vulns", middleware.RequireAdmin(h.db), h.GetHostVulnerabilities)
		hg.POST("/vulns/scan", middleware.RequireAdmin(h.db), h.ScanHostVulnerabilities)
		hg.GET("/monitor", middleware.RequireAdmin(h.db), h.GetHostMonitorFindings)
		hg.POST("/monitor/scan", middleware.RequireAdmin(h.db), h.RunHostMonitorScan)
		hg.POST("/update", middleware.RequireAdmin(h.db), h.ExecuteHostUpdate)
	}
}

// ============================================================================
// Maintenance Lock Handlers
// ============================================================================

// AcquireMaintenanceLockRequest models the payload for locking the system.
type AcquireMaintenanceLockRequest struct {
	Reason     string `json:"reason"`
	TTLMinutes int    `json:"ttl_minutes"` // Default: 60 minutes
}

// GetMaintenanceStatus queries the current state of the System Maintenance Lock.
//
//	Route: GET /api/v1/ops/maintenance/lock
func (h *OpsHandler) GetMaintenanceStatus(c *gin.Context) {
	fsm := h.GetRecoveryFSM()
	status := fsm.GetMaintenanceLock().GetStatus()

	if c.Query("envelope") == "true" || c.GetHeader("X-Envelope") == "true" {
		utils.Success(c, status)
		return
	}
	c.JSON(http.StatusOK, status)
}

// AcquireMaintenanceLock engages the exclusive System Maintenance Lock.
// Proactive agent recoveries and automated restarts will be suspended while active.
//
//	Route: POST /api/v1/ops/maintenance/lock
func (h *OpsHandler) AcquireMaintenanceLock(c *gin.Context) {
	if !c.GetBool("is_admin") {
		c.JSON(http.StatusForbidden, gin.H{"error": "admin role required"})
		return
	}

	var req AcquireMaintenanceLockRequest
	_ = c.ShouldBindJSON(&req)

	if strings.TrimSpace(req.Reason) == "" {
		req.Reason = "Manual administrative maintenance"
	}

	ttl := 60 * time.Minute
	if req.TTLMinutes > 0 {
		ttl = time.Duration(req.TTLMinutes) * time.Minute
	}

	operator := getOperatorName(c)
	fsm := h.GetRecoveryFSM()
	ae := h.getAuditEngine()
	runCtx := c.Request.Context()
	opID := GenerateOperationID("maint_lock")
	if ae != nil {
		_, rCtx, sErr := ae.StartRun(runCtx, audit.StartRunInput{
			HouseholdID:   1,
			TriggerSource: "api_manual",
			ActorType:     "user",
			ActorID:       operator,
			OperationID:   opID,
			Action:        "maintenance.lock.acquire",
			Target:        "system",
			Component:     "maintenance-lock",
			Decision:      model.AuditDecisionAllow,
			RiskLevel:     model.AuditRiskHigh,
			Summary:       fmt.Sprintf("Acquire maintenance lock by %s", operator),
		})
		if sErr != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "audit persistence failed, maintenance lock blocked", "details": sErr.Error()})
			return
		}
		runCtx = rCtx
		if _, preErr := ae.RecordPreMutation(runCtx, audit.EmitEventInput{
			EventType:     model.AuditEventExecutionStarted,
			Action:        "maintenance.lock.acquire",
			Target:        "system",
			Component:     "maintenance-lock",
			OperationID:   opID,
			Decision:      model.AuditDecisionAllow,
			RiskLevel:     model.AuditRiskHigh,
			Parameters:    req,
			Result:        "STARTED",
			FailurePolicy: audit.PolicyFailClosed,
		}); preErr != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "pre-execution audit failed, maintenance lock blocked", "details": preErr.Error()})
			return
		}
	}

	if err := fsm.GetMaintenanceLock().Acquire(operator, req.Reason, ttl); err != nil {
		if ae != nil {
			_, _ = ae.EmitEvent(runCtx, audit.EmitEventInput{
				TriggerSource: "api_manual", ActorType: "user", ActorID: operator,
				EventType: model.AuditEventExecutionFailed, Action: "maintenance.lock.acquire",
				Target: "system", Component: "maintenance-lock", OperationID: opID, Decision: model.AuditDecisionDeny,
				RiskLevel: model.AuditRiskHigh, Result: "CONFLICT", ErrorCode: err.Error(),
				Parameters: req, FinalRunStatus: model.AuditRunStatusFailed,
			})
		}
		c.JSON(http.StatusConflict, gin.H{
			"error":   err.Error(),
			"message": "系统排他维护锁已被占用，无法重复加锁",
		})
		return
	}

	status := fsm.GetMaintenanceLock().GetStatus()
	if ae != nil {
		if _, postErr := ae.RecordPostMutation(runCtx, audit.EmitEventInput{
			TriggerSource: "api_manual", ActorType: "user", ActorID: operator,
			EventType: model.AuditEventExecutionCompleted, Action: "maintenance.lock.acquire",
			Target: "system", Component: "maintenance-lock", OperationID: opID, Decision: model.AuditDecisionAllow,
			RiskLevel: model.AuditRiskHigh, Parameters: req, AfterState: status,
			Result: "LOCKED", FinalRunStatus: model.AuditRunStatusCompleted, MirrorLegacy: true,
			FailurePolicy: audit.PolicyFailClosed,
		}, nil); postErr != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			GlobalSecurityMetrics.IncExecutionUnknown()
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":   ErrExecutionAmbiguous.Error(),
				"message": "维护锁已获取但事后审计写入失败，执行结果进入不确定状态",
				"details": postErr.Error(),
				"status":  status,
			})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"message": "已成功启动系统维护模式，主动自愈与自动重启已挂起",
		"status":  status,
	})
}

// ReleaseMaintenanceLock releases the System Maintenance Lock, restoring automated recovery.
//
//	Route: POST /api/v1/ops/maintenance/unlock
func (h *OpsHandler) ReleaseMaintenanceLock(c *gin.Context) {
	if !c.GetBool("is_admin") {
		c.JSON(http.StatusForbidden, gin.H{"error": "admin role required"})
		return
	}

	operator := getOperatorName(c)
	fsm := h.GetRecoveryFSM()
	ae := h.getAuditEngine()
	runCtx := c.Request.Context()
	opID := GenerateOperationID("maint_unlock")
	if ae != nil {
		_, rCtx, sErr := ae.StartRun(runCtx, audit.StartRunInput{
			HouseholdID:   1,
			TriggerSource: "api_manual",
			ActorType:     "user",
			ActorID:       operator,
			OperationID:   opID,
			Action:        "maintenance.lock.release",
			Target:        "system",
			Component:     "maintenance-lock",
			Decision:      model.AuditDecisionAllow,
			RiskLevel:     model.AuditRiskHigh,
			Summary:       fmt.Sprintf("Release maintenance lock by %s", operator),
		})
		if sErr != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "audit persistence failed, maintenance unlock blocked", "details": sErr.Error()})
			return
		}
		runCtx = rCtx
		if _, preErr := ae.RecordPreMutation(runCtx, audit.EmitEventInput{
			EventType:     model.AuditEventExecutionStarted,
			Action:        "maintenance.lock.release",
			Target:        "system",
			Component:     "maintenance-lock",
			OperationID:   opID,
			Decision:      model.AuditDecisionAllow,
			RiskLevel:     model.AuditRiskHigh,
			Result:        "STARTED",
			FailurePolicy: audit.PolicyFailClosed,
		}); preErr != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "pre-execution audit failed, maintenance unlock blocked", "details": preErr.Error()})
			return
		}
	}

	if err := fsm.GetMaintenanceLock().Release(operator); err != nil {
		if ae != nil {
			_, _ = ae.EmitEvent(runCtx, audit.EmitEventInput{
				TriggerSource: "api_manual", ActorType: "user", ActorID: operator,
				EventType: model.AuditEventExecutionFailed, Action: "maintenance.lock.release",
				Target: "system", Component: "maintenance-lock", OperationID: opID, Decision: model.AuditDecisionDeny,
				RiskLevel: model.AuditRiskHigh, Result: "CONFLICT", ErrorCode: err.Error(),
				FinalRunStatus: model.AuditRunStatusFailed,
			})
		}
		c.JSON(http.StatusConflict, gin.H{
			"error":   err.Error(),
			"message": "解除系统维护锁失败：存储持久化或降级锁尚未解除",
			"status":  fsm.GetMaintenanceLock().GetStatus(),
		})
		return
	}

	if ae != nil {
		if _, postErr := ae.RecordPostMutation(runCtx, audit.EmitEventInput{
			TriggerSource: "api_manual", ActorType: "user", ActorID: operator,
			EventType: model.AuditEventExecutionCompleted, Action: "maintenance.lock.release",
			Target: "system", Component: "maintenance-lock", OperationID: opID, Decision: model.AuditDecisionAllow,
			RiskLevel: model.AuditRiskHigh, AfterState: fsm.GetMaintenanceLock().GetStatus(),
			Result: "UNLOCKED", FinalRunStatus: model.AuditRunStatusCompleted, MirrorLegacy: true,
			FailurePolicy: audit.PolicyFailClosed,
		}, nil); postErr != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			GlobalSecurityMetrics.IncExecutionUnknown()
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":   ErrExecutionAmbiguous.Error(),
				"message": "维护锁已解除但事后审计写入失败，执行结果进入不确定状态",
				"details": postErr.Error(),
				"status":  fsm.GetMaintenanceLock().GetStatus(),
			})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "已解除系统维护锁，主动自愈与自动化探针已恢复运行",
		"status":  fsm.GetMaintenanceLock().GetStatus(),
	})
}

// ============================================================================
// Circuit Breaker & FSM Handlers
// ============================================================================

// ResetContainerCircuit manually resets a container's circuit breaker to CLOSED.
// Refuses to clear degraded fail-closed state; storage must be reconciled via /fsm/reconcile first.
//
//	Route: POST /api/v1/ops/containers/:name/reset-circuit
func (h *OpsHandler) ResetContainerCircuit(c *gin.Context) {
	if !c.GetBool("is_admin") {
		c.JSON(http.StatusForbidden, gin.H{"error": "admin role required"})
		return
	}

	name := strings.TrimSpace(c.Param("name"))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Container name is required"})
		return
	}

	fsm := h.GetRecoveryFSM()
	if fsm.GetCircuitBreaker().IsDegraded() {
		c.JSON(http.StatusConflict, gin.H{
			"error":   ErrFailClosedMode.Error(),
			"message": "熔断器处于存储降级冻结状态，必须先执行存储对账 /fsm/reconcile",
		})
		return
	}
	canonical := ResolveTopologyComponent(name)
	if canonical == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": ErrComponentNotWhitelisted.Error()})
		return
	}
	operator := getOperatorName(c)
	ae := h.getAuditEngine()
	runCtx := c.Request.Context()
	opID := GenerateOperationID("circuit_reset")
	if ae != nil {
		_, rCtx, sErr := ae.StartRun(runCtx, audit.StartRunInput{
			HouseholdID:   1,
			TriggerSource: "api_manual",
			ActorType:     "user",
			ActorID:       operator,
			OperationID:   opID,
			Action:        "circuit_breaker.reset",
			Target:        canonical,
			Component:     canonical,
			Decision:      model.AuditDecisionAllow,
			RiskLevel:     model.AuditRiskHigh,
			Summary:       fmt.Sprintf("Reset circuit breaker for %s by %s", canonical, operator),
		})
		if sErr != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "audit persistence failed, circuit reset blocked", "details": sErr.Error()})
			return
		}
		runCtx = rCtx
		if _, preErr := ae.RecordPreMutation(runCtx, audit.EmitEventInput{
			EventType:     model.AuditEventExecutionStarted,
			Action:        "circuit_breaker.reset",
			Target:        canonical,
			Component:     canonical,
			OperationID:   opID,
			Decision:      model.AuditDecisionAllow,
			RiskLevel:     model.AuditRiskHigh,
			Result:        "STARTED",
			FailurePolicy: audit.PolicyFailClosed,
		}); preErr != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "pre-execution audit failed, circuit reset blocked", "details": preErr.Error()})
			return
		}
	}

	fsm.GetCircuitBreaker().Reset(canonical)
	if fsm.GetCircuitBreaker().IsDegraded() {
		if ae != nil {
			_, _ = ae.EmitEvent(runCtx, audit.EmitEventInput{
				TriggerSource: "api_manual", ActorType: "user", ActorID: operator,
				EventType: model.AuditEventExecutionFailed, Action: "circuit_breaker.reset",
				Target: canonical, Component: canonical, OperationID: opID, Decision: model.AuditDecisionDeny,
				RiskLevel: model.AuditRiskHigh, Result: "DEGRADED", ErrorCode: ErrFailClosedMode.Error(),
				FinalRunStatus: model.AuditRunStatusFailed,
			})
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":         ErrFailClosedMode.Error(),
			"circuit_state": CircuitOpen,
		})
		return
	}

	if ae != nil {
		if _, postErr := ae.RecordPostMutation(runCtx, audit.EmitEventInput{
			TriggerSource: "api_manual", ActorType: "user", ActorID: operator,
			EventType: model.AuditEventExecutionCompleted, Action: "circuit_breaker.reset",
			Target: canonical, Component: canonical, OperationID: opID, Decision: model.AuditDecisionAllow,
			RiskLevel: model.AuditRiskHigh, AfterState: map[string]interface{}{"circuit_state": CircuitClosed},
			Result: "RESET", FinalRunStatus: model.AuditRunStatusCompleted, MirrorLegacy: true,
			FailurePolicy: audit.PolicyFailClosed,
		}, nil); postErr != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			GlobalSecurityMetrics.IncExecutionUnknown()
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":         ErrExecutionAmbiguous.Error(),
				"details":       postErr.Error(),
				"container":     canonical,
				"circuit_state": CircuitClosed,
			})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message":       fmt.Sprintf("容器 %s 的熔断器状态已重置为 CLOSED，崩溃计数器已清零", canonical),
		"container":     canonical,
		"circuit_state": CircuitClosed,
	})
}

// ReconcileFSMStorage reconciles SQLite security state and clears Degraded freeze only after verification.
//
//	Route: POST /api/v1/ops/fsm/reconcile
func (h *OpsHandler) ReconcileFSMStorage(c *gin.Context) {
	if !c.GetBool("is_admin") {
		c.JSON(http.StatusForbidden, gin.H{"error": "admin role required"})
		return
	}
	operator := getOperatorName(c)
	fsm := h.GetRecoveryFSM()
	ae := h.getAuditEngine()
	runCtx := c.Request.Context()
	opID := GenerateOperationID("fsm_reconcile")
	if ae != nil {
		_, rCtx, sErr := ae.StartRun(runCtx, audit.StartRunInput{
			HouseholdID:   1,
			TriggerSource: "api_manual",
			ActorType:     "user",
			ActorID:       operator,
			OperationID:   opID,
			Action:        "fsm.reconcile",
			Target:        "app.db",
			Component:     "sqlite-ops",
			Decision:      model.AuditDecisionAllow,
			RiskLevel:     model.AuditRiskCritical,
			Summary:       fmt.Sprintf("FSM storage reconciliation by %s", operator),
		})
		if sErr != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "audit persistence failed, reconciliation blocked", "details": sErr.Error()})
			return
		}
		runCtx = rCtx
		if _, preErr := ae.RecordPreMutation(runCtx, audit.EmitEventInput{
			EventType:     model.AuditEventExecutionStarted,
			Action:        "fsm.reconcile",
			Target:        "app.db",
			Component:     "sqlite-ops",
			OperationID:   opID,
			Decision:      model.AuditDecisionAllow,
			RiskLevel:     model.AuditRiskCritical,
			Result:        "STARTED",
			FailurePolicy: audit.PolicyFailClosed,
		}); preErr != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "pre-execution audit failed, reconciliation blocked", "details": preErr.Error()})
			return
		}
	}

	if err := fsm.ReconcileDegradedState(operator); err != nil {
		if ae != nil {
			_, _ = ae.EmitEvent(runCtx, audit.EmitEventInput{
				TriggerSource: "api_manual", ActorType: "user", ActorID: operator,
				EventType: model.AuditEventExecutionFailed, Action: "fsm.reconcile",
				Target: "app.db", Component: "sqlite-ops", OperationID: opID, Decision: model.AuditDecisionDeny,
				RiskLevel: model.AuditRiskCritical, Result: "FAILED", ErrorCode: err.Error(),
				FinalRunStatus: model.AuditRunStatusFailed,
			})
		}
		c.JSON(http.StatusConflict, gin.H{
			"error":   err.Error(),
			"message": "存储与安全状态对账失败，系统维持 Fail-Closed 冻结保护",
		})
		return
	}
	if ae != nil {
		if _, postErr := ae.RecordPostMutation(runCtx, audit.EmitEventInput{
			TriggerSource: "api_manual", ActorType: "user", ActorID: operator,
			EventType: model.AuditEventRecoveryVerified, Action: "fsm.reconcile",
			Target: "app.db", Component: "sqlite-ops", OperationID: opID, Decision: model.AuditDecisionAllow,
			RiskLevel: model.AuditRiskCritical, Result: "RECONCILED",
			FinalRunStatus: model.AuditRunStatusCompleted, MirrorLegacy: true,
			FailurePolicy: audit.PolicyFailClosed,
		}, nil); postErr != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			GlobalSecurityMetrics.IncExecutionUnknown()
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":                    ErrExecutionAmbiguous.Error(),
				"details":                  postErr.Error(),
				"maintenance_lock_degraded": fsm.GetMaintenanceLock().IsDegraded(),
				"circuit_breaker_degraded":  fsm.GetCircuitBreaker().IsDegraded(),
			})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"message":                  "存储完整性对账通过，Degraded 冻结已安全解除",
		"maintenance_lock_degraded": fsm.GetMaintenanceLock().IsDegraded(),
		"circuit_breaker_degraded":  fsm.GetCircuitBreaker().IsDegraded(),
	})
}

// GetSecurityMetrics exposes Round 4 closed-loop security counters (R4-25).
//
//	Route: GET /api/v1/ops/security-metrics
func (h *OpsHandler) GetSecurityMetrics(c *gin.Context) {
	c.JSON(http.StatusOK, GlobalSecurityMetrics.Snapshot())
}

// GetFSMStatus returns the status of the maintenance lock, dependency topology, and circuit breakers.
//
//	Route: GET /api/v1/ops/fsm/status
func (h *OpsHandler) GetFSMStatus(c *gin.Context) {
	fsm := h.GetRecoveryFSM()
	lockStatus := fsm.GetMaintenanceLock().GetStatus()

	coreContainers := []string{"home-api", "home-web", "home-vision", "frigate", "mosquitto"}
	breakers := make(map[string]gin.H)
	for _, cc := range coreContainers {
		breakers[cc] = gin.H{
			"state":         fsm.GetCircuitBreaker().GetState(cc),
			"crashes_in_10m": fsm.GetCircuitBreaker().GetCrashCount(cc),
			"level":         fsm.GetTopologyEngine().GetLevel(cc),
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"maintenance_lock": lockStatus,
		"circuit_breakers": breakers,
		"topology": gin.H{
			"level_0": []string{"mosquitto", "frigate", "app.db"},
			"level_1": []string{"home-api"},
			"level_2": []string{"home-web", "home-vision", "home-cloudflared", "home-h3c-keepalive", "home-ops-agent"},
		},
	})
}

// HandleContainerEvent processes a container event through the 5-class FSM.
//
//	Route: POST /api/v1/ops/fsm/event
func (h *OpsHandler) HandleContainerEvent(c *gin.Context) {
	isAdmin := c.GetBool("is_admin")
	isInternal := c.GetBool("is_internal_service")
	if !isAdmin && !isInternal {
		c.JSON(http.StatusForbidden, gin.H{"error": "admin role or internal scope required"})
		return
	}

	var event ContainerEvent
	if err := c.ShouldBindJSON(&event); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid container event payload: " + err.Error()})
		return
	}

	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	fsm := h.GetRecoveryFSM()
	if exec := h.GetExecutor(); exec != nil {
		fsm.mu.RLock()
		hasExec := fsm.executor != nil
		fsm.mu.RUnlock()
		if !hasExec {
			fsm.SetExecutor(exec)
		}
	}

	result, err := fsm.ProcessAndRecover(c.Request.Context(), event)
	respBody := gin.H{
		"decision":        result.Decision,
		"status":          result.Status,
		"executed":        result.Executed,
		"health_verified": result.HealthVerified,
		"output":          result.Output,
		"error":           result.Error,
	}
	if result != nil && result.Decision != nil && result.Decision.BaselineHash != "" {
		respBody["baseline_hash"] = result.Decision.BaselineHash
		respBody["baseline_protocol_version"] = result.Decision.BaselineProtocolVersion
	}
	if err != nil {
		respBody["warning"] = err.Error()
		c.JSON(http.StatusOK, respBody)
		return
	}

	c.JSON(http.StatusOK, respBody)
}

// GetRecoveryFSM retrieves the ContainerRecoveryFSM instance bound to OpsHandler.
func (h *OpsHandler) GetRecoveryFSM() *ContainerRecoveryFSM {
	fsm := GetGlobalRecoveryFSM()
	if fsm != nil {
		return fsm
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// Double-check after acquiring lock
	if fsm = GetGlobalRecoveryFSM(); fsm != nil {
		return fsm
	}

	// Lazy initialization with handler resources
	newFSM := NewContainerRecoveryFSM(h.db, h.bus, h.executor)
	if defExec, ok := h.executor.(*DefaultOperationExecutor); ok && defExec != nil {
		defExec.SetRecoveryFSM(newFSM)
	}
	SetGlobalRecoveryFSM(newFSM)
	return newFSM
}

// ============================================================================
// Host Security & Guarded Update Handlers (R9.1 - R9.5)
// ============================================================================

// GetHostAssets collects read-only host OS asset posture (R9.1, safe on Debian & fnOS).
//
//	Route: GET /api/v1/ops/host/assets
func (h *OpsHandler) GetHostAssets(c *gin.Context) {
	hhID, ok := resolveAdminAuditHousehold(c)
	if !ok {
		utils.Fail(c, http.StatusForbidden, "admin role required for host security inspection")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	prov := h.GetHostSecurityProvider()
	snap, err := prov.CollectAssets(ctx)
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, redact.SensitiveString(err.Error()))
		return
	}
	utils.Success(c, gin.H{
		"household_id":  hhID,
		"baseline_hash": h.ComputeFingerprint(ComponentHostOS),
		"snapshot":      snap,
	})
}

// GetHostVulnerabilities returns the latest R9.2 host CVE correlation report (or runs a fresh scan if uncached/refresh=true).
//
//	Route: GET /api/v1/ops/host/vulns
func (h *OpsHandler) GetHostVulnerabilities(c *gin.Context) {
	hhID, ok := resolveAdminAuditHousehold(c)
	if !ok {
		utils.Fail(c, http.StatusForbidden, "admin role required for host vulnerability inspection")
		return
	}
	scanner := h.GetVulnScanner()
	if scanner == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "vulnerability scanner not configured")
		return
	}

	report := scanner.LastHostReport()
	if report == nil || c.Query("refresh") == "true" {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
		defer cancel()
		var err error
		report, err = scanner.ScanHostAssets(ctx)
		if err != nil {
			utils.Fail(c, http.StatusBadGateway, redact.SensitiveString(err.Error()))
			return
		}
	}

	utils.Success(c, gin.H{
		"household_id": hhID,
		"report":       report,
	})
}

// ScanHostVulnerabilities triggers an on-demand R9.2 host asset CVE correlation scan.
//
//	Route: POST /api/v1/ops/host/vulns/scan
func (h *OpsHandler) ScanHostVulnerabilities(c *gin.Context) {
	hhID, ok := resolveAdminAuditHousehold(c)
	if !ok {
		utils.Fail(c, http.StatusForbidden, "admin role required for host vulnerability scan")
		return
	}
	scanner := h.GetVulnScanner()
	if scanner == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "vulnerability scanner not configured")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()

	var (
		report *hostsec.CorrelationReport
		err    error
	)
	if c.Query("include_deps") == "true" {
		report, _, err = scanner.ScanHostAndDependencies(ctx, ".")
	} else {
		report, err = scanner.ScanHostAssets(ctx)
	}
	if err != nil {
		utils.Fail(c, http.StatusBadGateway, redact.SensitiveString(err.Error()))
		return
	}

	utils.Success(c, gin.H{
		"household_id": hhID,
		"report":       report,
	})
}

// GetHostMonitorFindings returns the latest R9.3 host security drift/anomaly findings (running an initial sweep if needed).
//
//	Route: GET /api/v1/ops/host/monitor
func (h *OpsHandler) GetHostMonitorFindings(c *gin.Context) {
	hhID, ok := resolveAdminAuditHousehold(c)
	if !ok {
		utils.Fail(c, http.StatusForbidden, "admin role required for host monitor findings")
		return
	}
	mon := h.GetHostSecurityMonitor()
	findings, scannedAt := mon.LastFindings()
	if scannedAt.IsZero() || c.Query("live") == "true" {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
		defer cancel()
		var err error
		findings, err = mon.ScanOnce(ctx)
		if err != nil {
			utils.Fail(c, http.StatusInternalServerError, redact.SensitiveString(err.Error()))
			return
		}
		_, scannedAt = mon.LastFindings()
	}

	utils.Success(c, gin.H{
		"household_id": hhID,
		"scanned_at":   scannedAt,
		"count":        len(findings),
		"findings":     findings,
	})
}

// RunHostMonitorScan triggers an immediate R9.3 sweep across all 10 host threat surfaces.
//
//	Route: POST /api/v1/ops/host/monitor/scan
func (h *OpsHandler) RunHostMonitorScan(c *gin.Context) {
	hhID, ok := resolveAdminAuditHousehold(c)
	if !ok {
		utils.Fail(c, http.StatusForbidden, "admin role required for host monitor scan")
		return
	}
	mon := h.GetHostSecurityMonitor()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	findings, err := mon.ScanOnce(ctx)
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, redact.SensitiveString(err.Error()))
		return
	}
	_, scannedAt := mon.LastFindings()

	utils.Success(c, gin.H{
		"household_id": hhID,
		"scanned_at":   scannedAt,
		"count":        len(findings),
		"findings":     findings,
	})
}

// HostUpdateEndpointRequest triggers guarded execution of an approved host-os proposal.
type HostUpdateEndpointRequest struct {
	ProposalID string `json:"proposal_id" binding:"required"`
}

// ExecuteHostUpdate executes an approved host OS package upgrade proposal through
// DefaultOperationExecutor -> HostUpdateExecutor (fail-closed on fnOS, unapproved/tampered
// proposals, baseline drift, insecure APT sources, and non-whitelisted packages).
//
//	Route: POST /api/v1/ops/host/update
func (h *OpsHandler) ExecuteHostUpdate(c *gin.Context) {
	hhID, ok := resolveAdminAuditHousehold(c)
	if !ok {
		utils.Fail(c, http.StatusForbidden, "admin role required for host package updates")
		return
	}
	if h.db == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "ops database unavailable")
		return
	}
	exec := h.GetExecutor()
	if exec == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "operation executor unavailable")
		return
	}

	var req HostUpdateEndpointRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "proposal_id is required: "+err.Error())
		return
	}

	var proposal model.OpsProposal
	if err := h.db.Where("id = ? OR proposal_id = ?", strings.TrimSpace(req.ProposalID), strings.TrimSpace(req.ProposalID)).First(&proposal).Error; err != nil {
		utils.Fail(c, http.StatusNotFound, "proposal not found")
		return
	}
	if !sameHouseholdAccess(hhID, proposal.HouseholdID) {
		utils.Fail(c, http.StatusNotFound, "proposal not found")
		return
	}
	if model.CanonicalizeComponent(proposal.TargetComponent) != ComponentHostOS {
		utils.Fail(c, http.StatusBadRequest, fmt.Sprintf("proposal %s target_component %q is not %s", proposal.ProposalID, proposal.TargetComponent, ComponentHostOS))
		return
	}

	res, err := exec.ExecuteProposal(c.Request.Context(), &proposal)
	if err != nil {
		statusCode := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrProposalUnauthorized),
			errors.Is(err, hostsec.ErrApprovalRequired),
			errors.Is(err, hostsec.ErrExecutionGrantRequired),
			errors.Is(err, hostsec.ErrExecutionGrantInvalid),
			errors.Is(err, hostsec.ErrExecutionGrantExpired),
			errors.Is(err, hostsec.ErrExecutionGrantRevoked),
			errors.Is(err, hostsec.ErrExecutionGrantConsumed),
			errors.Is(err, hostsec.ErrExecutionGrantMismatch),
			errors.Is(err, hostsec.ErrExecutionGrantKeyConflict),
			errors.Is(err, ErrApprovalDigestMismatch):
			statusCode = http.StatusForbidden
		case errors.Is(err, ErrInvalidSpec),
			errors.Is(err, ErrActionNotSupported),
			errors.Is(err, hostsec.ErrPackageNotWhitelisted),
			errors.Is(err, hostsec.ErrInvalidPackageName),
			errors.Is(err, hostsec.ErrInvalidPackageVersion):
			statusCode = http.StatusBadRequest
		case errors.Is(err, hostsec.ErrFnOSUpdateUnsupported),
			errors.Is(err, hostsec.ErrUnsupportedHostOS),
			errors.Is(err, hostsec.ErrContainerHostExecutionBoundary),
			errors.Is(err, hostsec.ErrBridgeBindingInvalid),
			errors.Is(err, ErrBaselineMismatch),
			errors.Is(err, hostsec.ErrBaselineDrift),
			errors.Is(err, ErrProposalAlreadyRunning),
			errors.Is(err, ErrProposalNotExecutable),
			errors.Is(err, ErrExecutionAmbiguous),
			errors.Is(err, ErrSystemInMaintenance),
			errors.Is(err, ErrCircuitBreakerOpen),
			errors.Is(err, ErrFailClosedMode):
			statusCode = http.StatusConflict
		}
		utils.Fail(c, statusCode, redact.SensitiveString(err.Error()))
		return
	}

	utils.Success(c, gin.H{
		"household_id": hhID,
		"proposal_id":  proposal.ProposalID,
		"result":       res,
	})
}
