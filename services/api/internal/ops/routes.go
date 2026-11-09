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
	rg.GET("/proposals", h.ListProposals)
	rg.POST("/proposals", middleware.RequireAdmin(h.db), h.CreateProposal)
	rg.POST("/proposals/:id/approve", middleware.RequireAdmin(h.db), h.ApproveProposal)
	rg.POST("/proposals/:id/execute", middleware.RequireAdmin(h.db), h.ExecuteProposalEndpoint)

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

	// FSM telemetry and manual event testing
	rg.GET("/fsm/status", h.GetFSMStatus)
	rg.POST("/fsm/event", middleware.RequireAdmin(h.db), h.HandleContainerEvent)
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
	_ = fsm.GetMaintenanceLock().Release(operator)

	c.JSON(http.StatusOK, gin.H{
		"message": "已解除系统维护锁，主动自愈与自动化探针已恢复运行",
		"status":  fsm.GetMaintenanceLock().GetStatus(),
	})
}

// ============================================================================
// Circuit Breaker & FSM Handlers
// ============================================================================

// ResetContainerCircuit manually resets a container's circuit breaker to CLOSED.
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
	fsm.GetCircuitBreaker().Reset(name)

	c.JSON(http.StatusOK, gin.H{
		"message":        fmt.Sprintf("容器 %s 的熔断器状态已重置为 CLOSED，崩溃计数器已清零", name),
		"container":      name,
		"circuit_state":  CircuitClosed,
	})
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
	if !c.GetBool("is_admin") {
		c.JSON(http.StatusForbidden, gin.H{"error": "admin role required"})
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
	decision, err := fsm.ProcessContainerEvent(c.Request.Context(), event)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"decision": decision,
			"warning":  err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"decision": decision,
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
	SetGlobalRecoveryFSM(newFSM)
	return newFSM
}
