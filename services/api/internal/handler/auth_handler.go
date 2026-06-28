package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/service"
	"home-datacenter-api/internal/utils"
)

type AuthHandler struct {
	authService *service.AuthService
}

func NewAuthHandler(
	authService *service.AuthService,
) *AuthHandler {
	return &AuthHandler{
		authService: authService,
	}
}

type BindRequest struct {
	UserID    uint   `json:"user_id" binding:"required"`
	AccessKey string `json:"access_key" binding:"required"`
}

// Bind exchanges (user_id, access_key) for a long-lived JWT.
//
//	Route: POST /api/v1/auth/bind
func (h *AuthHandler) Bind(c *gin.Context) {

	var req BindRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, err.Error())
		return
	}

	token, err := h.authService.Bind(req.UserID, req.AccessKey)
	if err != nil {
		utils.Fail(c, http.StatusUnauthorized, err.Error())
		return
	}

	utils.Success(c, gin.H{
		"token": token,
	})
}
