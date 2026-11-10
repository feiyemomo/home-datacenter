package ops

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/middleware"
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

	if err := fsm.GetMaintenanceLock().Acquire(operator, req.Reason, ttl); err != nil {
		c.JSON(http.StatusConflict, gin.H{
			"error":   err.Error(),
			"message": "系统排他维护锁已被占用，无法重复加锁",
		})
		return
	}

	status := fsm.GetMaintenanceLock().GetStatus()
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
	if err := fsm.GetMaintenanceLock().Release(operator); err != nil {
		c.JSON(http.StatusConflict, gin.H{
			"error":   err.Error(),
			"message": "解除系统维护锁失败：存储持久化或降级锁尚未解除",
			"status":  fsm.GetMaintenanceLock().GetStatus(),
		})
		return
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
	fsm.GetCircuitBreaker().Reset(canonical)
	if fsm.GetCircuitBreaker().IsDegraded() {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":         ErrFailClosedMode.Error(),
			"circuit_state": CircuitOpen,
		})
		return
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
	if err := fsm.ReconcileDegradedState(operator); err != nil {
		c.JSON(http.StatusConflict, gin.H{
			"error":   err.Error(),
			"message": "存储与安全状态对账失败，系统维持 Fail-Closed 冻结保护",
		})
		return
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
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"decision":        result.Decision,
			"status":          result.Status,
			"executed":        result.Executed,
			"health_verified": result.HealthVerified,
			"output":          result.Output,
			"error":           result.Error,
			"warning":         err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"decision":        result.Decision,
		"status":          result.Status,
		"executed":        result.Executed,
		"health_verified": result.HealthVerified,
		"output":          result.Output,
		"error":           result.Error,
	})
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
