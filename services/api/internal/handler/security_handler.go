package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/security"
	"home-datacenter-api/internal/utils"
)

type SecurityHandler struct {
	guardMgr *security.GuardManager
}

func NewSecurityHandler(gm *security.GuardManager) *SecurityHandler {
	return &SecurityHandler{guardMgr: gm}
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
