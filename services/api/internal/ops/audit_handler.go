package ops

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/audit"
	"home-datacenter-api/internal/middleware"
	"home-datacenter-api/internal/utils"
)

// RegisterAuditRoutes mounts tamper-evident audit query, timeline, verification, and export endpoints.
// All endpoints strictly enforce Admin privileges and caller household_id isolation.
func (h *OpsHandler) RegisterAuditRoutes(rg *gin.RouterGroup) {
	ag := rg.Group("/audit")
	{
		ag.GET("/runs", middleware.RequireAdmin(h.db), h.ListAuditRuns)
		ag.GET("/runs/:run_id", middleware.RequireAdmin(h.db), h.GetAuditRunDetail)
		ag.GET("/events", middleware.RequireAdmin(h.db), h.ListAuditEvents)
		ag.POST("/verify", middleware.RequireAdmin(h.db), h.VerifyAuditChain)
		ag.GET("/export", middleware.RequireAdmin(h.db), h.ExportAuditEvents)
	}
}

func resolveAdminAuditHousehold(c *gin.Context) (uint, bool) {
	if c.GetBool("is_internal_service") || !c.GetBool("is_admin") {
		return 0, false
	}
	hhID := c.GetUint("household_id")
	if hhID == 0 {
		// Authenticated admin without multi-tenant claim binds strictly to primary household (never 0 wildcard)
		hhID = 1
	}
	return hhID, true
}

// ListAuditRuns returns paginated AgentAuditRun records for the caller's household.
//
//	Route: GET /api/v1/ops/audit/runs
func (h *OpsHandler) ListAuditRuns(c *gin.Context) {
	hhID, ok := resolveAdminAuditHousehold(c)
	if !ok {
		utils.Fail(c, http.StatusForbidden, "admin role required for audit history")
		return
	}
	ae := h.getAuditEngine()
	if ae == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "audit engine unavailable")
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	fromMs, _ := strconv.ParseInt(c.Query("from_ms"), 10, 64)
	toMs, _ := strconv.ParseInt(c.Query("to_ms"), 10, 64)

	var isOfflinePtr *bool
	if v := strings.TrimSpace(c.Query("is_offline")); v != "" {
		b := strings.EqualFold(v, "true") || v == "1"
		isOfflinePtr = &b
	}

	runs, total, err := ae.QueryRuns(audit.RunQuery{
		HouseholdID:   hhID,
		TriggerSource: c.Query("trigger_source"),
		Component:     c.Query("component"),
		Action:        c.Query("action"),
		Status:        c.Query("status"),
		RiskLevel:     c.Query("risk_level"),
		ActorID:       c.Query("actor_id"),
		IsOffline:     isOfflinePtr,
		FromMs:        fromMs,
		ToMs:          toMs,
		Search:        c.Query("q"),
		Page:          page,
		PageSize:      pageSize,
	})
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, err.Error())
		return
	}

	utils.Success(c, gin.H{
		"items":     runs,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

// GetAuditRunDetail returns the run header, ordered 24-stage event timeline, and integrity verification result.
//
//	Route: GET /api/v1/ops/audit/runs/:run_id
func (h *OpsHandler) GetAuditRunDetail(c *gin.Context) {
	hhID, ok := resolveAdminAuditHousehold(c)
	if !ok {
		utils.Fail(c, http.StatusForbidden, "admin role required for audit timeline")
		return
	}
	ae := h.getAuditEngine()
	if ae == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "audit engine unavailable")
		return
	}

	runID := strings.TrimSpace(c.Param("run_id"))
	detail, err := ae.GetRunDetail(runID, hhID)
	if err != nil {
		if errors.Is(err, audit.ErrAuditRunNotFound) {
			utils.Fail(c, http.StatusNotFound, "audit run not found")
			return
		}
		utils.Fail(c, http.StatusInternalServerError, err.Error())
		return
	}

	utils.Success(c, detail)
}

// ListAuditEvents searches household-scoped AgentAuditEvent records across runs.
//
//	Route: GET /api/v1/ops/audit/events
func (h *OpsHandler) ListAuditEvents(c *gin.Context) {
	hhID, ok := resolveAdminAuditHousehold(c)
	if !ok {
		utils.Fail(c, http.StatusForbidden, "admin role required for audit events")
		return
	}
	ae := h.getAuditEngine()
	if ae == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "audit engine unavailable")
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	fromMs, _ := strconv.ParseInt(c.Query("from_ms"), 10, 64)
	toMs, _ := strconv.ParseInt(c.Query("to_ms"), 10, 64)

	events, total, err := ae.QueryEvents(audit.EventQuery{
		HouseholdID:   hhID,
		RunID:         c.Query("run_id"),
		EventType:     c.Query("event_type"),
		TriggerSource: c.Query("trigger_source"),
		Component:     c.Query("component"),
		Action:        c.Query("action"),
		Target:        c.Query("target"),
		CorrelationID: c.Query("correlation_id"),
		ProposalID:    c.Query("proposal_id"),
		OperationID:   c.Query("operation_id"),
		Result:        c.Query("result"),
		FromMs:        fromMs,
		ToMs:          toMs,
		Page:          page,
		PageSize:      pageSize,
	}, 100)
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, err.Error())
		return
	}

	utils.Success(c, gin.H{
		"items":     events,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

// VerifyAuditChain verifies the hash chain, append-only SQLite triggers, external HMAC signatures, and checkpoints.
//
//	Route: POST /api/v1/ops/audit/verify
func (h *OpsHandler) VerifyAuditChain(c *gin.Context) {
	if _, ok := resolveAdminAuditHousehold(c); !ok {
		utils.Fail(c, http.StatusForbidden, "admin role required for audit chain verification")
		return
	}
	ae := h.getAuditEngine()
	if ae == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "audit engine unavailable")
		return
	}

	var req audit.VerifyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		req = audit.VerifyRequest{VerifyCheckpoints: true, VerifyTriggers: true}
	} else if !req.VerifyCheckpoints && !req.VerifyTriggers && req.FromSeq == 0 && req.ToSeq == 0 {
		req.VerifyCheckpoints = true
		req.VerifyTriggers = true
	}

	res := ae.VerifyChain(req)
	utils.Success(c, res)
}

// ExportAuditEvents exports household-scoped redacted audit events in JSON or formula-safe CSV format.
//
//	Route: GET /api/v1/ops/audit/export?format=json|csv
func (h *OpsHandler) ExportAuditEvents(c *gin.Context) {
	hhID, ok := resolveAdminAuditHousehold(c)
	if !ok {
		utils.Fail(c, http.StatusForbidden, "admin role required for audit export")
		return
	}
	ae := h.getAuditEngine()
	if ae == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "audit engine unavailable")
		return
	}

	format := strings.ToLower(strings.TrimSpace(c.DefaultQuery("format", "json")))
	if format != "json" && format != "csv" {
		utils.Fail(c, http.StatusBadRequest, "unsupported export format: must be json or csv")
		return
	}
	fromMs, _ := strconv.ParseInt(c.Query("from_ms"), 10, 64)
	toMs, _ := strconv.ParseInt(c.Query("to_ms"), 10, 64)

	raw, contentType, digest, err := ae.ExportEvents(audit.EventQuery{
		HouseholdID:   hhID,
		RunID:         c.Query("run_id"),
		EventType:     c.Query("event_type"),
		TriggerSource: c.Query("trigger_source"),
		Component:     c.Query("component"),
		Action:        c.Query("action"),
		Target:        c.Query("target"),
		CorrelationID: c.Query("correlation_id"),
		ProposalID:    c.Query("proposal_id"),
		OperationID:   c.Query("operation_id"),
		Result:        c.Query("result"),
		FromMs:        fromMs,
		ToMs:          toMs,
	}, format)
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, err.Error())
		return
	}

	filename := fmt.Sprintf("home-datacenter-audit-hh%d.%s", hhID, format)
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	c.Header("X-Audit-Export-SHA256", digest)
	c.Data(http.StatusOK, contentType, raw)
}
