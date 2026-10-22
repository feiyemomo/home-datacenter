package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/security"
	"home-datacenter-api/internal/utils"
)

type SecurityHandler struct {
	guardMgr   *security.GuardManager
	defenseMgr *security.DefenseManager
	alertMgr   *security.AlertManager
}

func NewSecurityHandler(gm *security.GuardManager, dm *security.DefenseManager, am *security.AlertManager) *SecurityHandler {
	return &SecurityHandler{
		guardMgr:   gm,
		defenseMgr: dm,
		alertMgr:   am,
	}
}

// GetGuard returns the current security guard state.
//
//	Route: GET /api/v1/security/guard
func (h *SecurityHandler) GetGuard(c *gin.Context) {
	state := h.guardMgr.GetState()
	utils.Success(c, gin.H{
		"mode":       state.Mode,
		"updated_by": state.UpdatedBy,
		"updated_at": state.UpdatedAt.Unix(),
	})
}

type setGuardRequest struct {
	Mode string `json:"mode" binding:"required"`
}

// SetGuard updates the global arming mode.
//
//	Route: PUT /api/v1/security/guard
func (h *SecurityHandler) SetGuard(c *gin.Context) {
	var req setGuardRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}

	userName := c.GetString("user_name")
	if userName == "" {
		userName = "user"
	}

	state, err := h.guardMgr.SetMode(req.Mode, userName)
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, err.Error())
		return
	}

	utils.Success(c, gin.H{
		"mode":       state.Mode,
		"updated_by": state.UpdatedBy,
		"updated_at": state.UpdatedAt.Unix(),
	})
}

// GetOverview returns real-time defense posture, active bans, and SMTP status.
//
//	Route: GET /api/v1/security/overview
func (h *SecurityHandler) GetOverview(c *gin.Context) {
	if h.defenseMgr == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "defense manager not configured")
		return
	}
	utils.Success(c, h.defenseMgr.GetOverview())
}

// ListBanned returns all actively banned IPs.
//
//	Route: GET /api/v1/security/banned
func (h *SecurityHandler) ListBanned(c *gin.Context) {
	if h.defenseMgr == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "defense manager not configured")
		return
	}
	utils.Success(c, h.defenseMgr.ListBanned())
}

type manualBanRequest struct {
	IP              string `json:"ip" binding:"required"`
	DurationMinutes int    `json:"duration_minutes"` // 0 = permanent
	Reason          string `json:"reason"`
}

// ManualBan manually adds an IP to the active ban list with self-protection.
//
//	Route: POST /api/v1/security/ban
func (h *SecurityHandler) ManualBan(c *gin.Context) {
	var req manualBanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}

	if h.defenseMgr == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "defense manager not configured")
		return
	}

	operatorIP := c.GetString("real_client_ip")
	if operatorIP == "" {
		operatorIP = c.ClientIP()
	}
	operatorUser := c.GetString("user_name")
	if operatorUser == "" {
		operatorUser = "admin"
	}

	duration := time.Duration(req.DurationMinutes) * time.Minute
	reason := req.Reason
	if reason == "" {
		reason = "管理员手动拦截"
	}

	if err := h.defenseMgr.ManualBan(operatorIP, operatorUser, req.IP, duration, reason); err != nil {
		if errors.Is(err, security.ErrLoopbackIP) || errors.Is(err, security.ErrWhitelistedIP) ||
			errors.Is(err, security.ErrSelfIP) || errors.Is(err, security.ErrInvalidIP) {
			utils.Fail(c, http.StatusConflict, err.Error())
			return
		}
		utils.Fail(c, http.StatusBadRequest, err.Error())
		return
	}

	utils.Success(c, gin.H{"ip": req.IP, "status": "banned"})
}

// ManualUnban removes an IP from the ban list.
//
//	Route: DELETE /api/v1/security/ban/:ip
func (h *SecurityHandler) ManualUnban(c *gin.Context) {
	ip := c.Param("ip")
	if ip == "" {
		utils.Fail(c, http.StatusBadRequest, "ip is required")
		return
	}

	if h.defenseMgr == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "defense manager not configured")
		return
	}

	operatorUser := c.GetString("user_name")
	if operatorUser == "" {
		operatorUser = "admin"
	}

	if err := h.defenseMgr.ManualUnban(operatorUser, ip); err != nil {
		utils.Fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	utils.Success(c, gin.H{"ip": ip, "status": "unbanned"})
}

type updateBanRequest struct {
	DurationMinutes int  `json:"duration_minutes"`
	Permanent       bool `json:"permanent"`
}

// UpdateBan adjusts duration or permanent flag of an existing ban.
//
//	Route: PUT /api/v1/security/ban/:ip
func (h *SecurityHandler) UpdateBan(c *gin.Context) {
	ip := c.Param("ip")
	if ip == "" {
		utils.Fail(c, http.StatusBadRequest, "ip is required")
		return
	}

	var req updateBanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}

	if h.defenseMgr == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "defense manager not configured")
		return
	}

	duration := time.Duration(req.DurationMinutes) * time.Minute
	if err := h.defenseMgr.UpdateBan(ip, duration, req.Permanent); err != nil {
		utils.Fail(c, http.StatusBadRequest, err.Error())
		return
	}
	utils.Success(c, gin.H{"ip": ip, "status": "updated"})
}

// ListEvents queries the security events timeline.
//
//	Route: GET /api/v1/security/events
func (h *SecurityHandler) ListEvents(c *gin.Context) {
	if h.defenseMgr == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "defense manager not configured")
		return
	}
	limit := 50
	if l := c.Query("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 {
			limit = v
		}
	}
	events, err := h.defenseMgr.ListEvents(limit)
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	utils.Success(c, events)
}

// GetWhitelist returns all dynamic whitelist records.
//
//	Route: GET /api/v1/security/whitelist
func (h *SecurityHandler) GetWhitelist(c *gin.Context) {
	if h.defenseMgr == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "defense manager not configured")
		return
	}
	list, err := h.defenseMgr.ListWhitelist()
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	utils.Success(c, list)
}

type addWhitelistRequest struct {
	Entry string `json:"entry" binding:"required"`
	Note  string `json:"note"`
}

// AddWhitelist registers a dynamic whitelist exemption.
//
//	Route: POST /api/v1/security/whitelist
func (h *SecurityHandler) AddWhitelist(c *gin.Context) {
	if h.defenseMgr == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "defense manager not configured")
		return
	}
	var req addWhitelistRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.defenseMgr.AddWhitelist(req.Entry, req.Note); err != nil {
		utils.Fail(c, http.StatusBadRequest, err.Error())
		return
	}
	utils.Success(c, gin.H{"entry": req.Entry, "status": "whitelisted"})
}

// DeleteWhitelist removes a dynamic whitelist exemption.
//
//	Route: DELETE /api/v1/security/whitelist/:entry
func (h *SecurityHandler) DeleteWhitelist(c *gin.Context) {
	if h.defenseMgr == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "defense manager not configured")
		return
	}
	entry := c.Param("entry")
	if entry == "" {
		utils.Fail(c, http.StatusBadRequest, "entry is required")
		return
	}
	if err := h.defenseMgr.RemoveWhitelist(entry); err != nil {
		utils.Fail(c, http.StatusBadRequest, err.Error())
		return
	}
	utils.Success(c, gin.H{"entry": entry, "status": "removed"})
}

type updateDefenseConfigRequest struct {
	MaxAuthFailures   int `json:"max_auth_failures"`
	FailureWindowMins int `json:"failure_window_mins"`
	BanDurationMins   int `json:"ban_duration_mins"`
}

// UpdateConfig updates runtime thresholds dynamically.
//
//	Route: PUT /api/v1/security/config
func (h *SecurityHandler) UpdateConfig(c *gin.Context) {
	if h.defenseMgr == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "defense manager not configured")
		return
	}
	var req updateDefenseConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.defenseMgr.UpdateConfig(req.MaxAuthFailures, req.FailureWindowMins, req.BanDurationMins); err != nil {
		utils.Fail(c, http.StatusBadRequest, err.Error())
		return
	}
	utils.Success(c, gin.H{"status": "updated"})
}

// TestAlertEmail dispatches a test alert via QQ Mail SMTP to confirm delivery.
//
//	Route: POST /api/v1/security/test-alert
func (h *SecurityHandler) TestAlertEmail(c *gin.Context) {
	if h.alertMgr == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "alert manager not configured")
		return
	}

	clientIP := c.GetString("real_client_ip")
	if clientIP == "" {
		clientIP = c.ClientIP()
	}

	err := h.alertMgr.SendSecurityEmail(
		"测试安全警报邮件连通性",
		clientIP,
		"系统管理员连通性测试",
		fmt.Sprintf("这是由管理员从控制台触发的 SMTP 连通性测试邮件。当前触发 IP: %s，系统安全响应中心运行正常。", clientIP),
		"测试验证通过（无阻断动作）",
	)

	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, fmt.Sprintf("failed to trigger test email: %v", err))
		return
	}

	cfg := h.alertMgr.GetConfig()
	utils.Success(c, gin.H{
		"message":   "测试警报邮件已加入发送队列",
		"recipient": cfg.Recipient,
		"smtp_host": cfg.Host,
		"enabled":   cfg.Enabled,
	})
}
