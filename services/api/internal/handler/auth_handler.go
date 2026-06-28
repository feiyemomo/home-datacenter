package handler

import (
    "net/http"

    "github.com/gin-gonic/gin"

    "home-datacenter-api/internal/service"
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

func (h *AuthHandler) Bind(c *gin.Context) {

    var req BindRequest

    if err := c.ShouldBindJSON(&req); err != nil {

    	c.JSON(http.StatusBadRequest, gin.H{
    		"message": err.Error(),
    	})

    	return
	}

    token, err := h.authService.Bind(
        req.UserID,
        req.AccessKey,
    )

    if err != nil {
        c.JSON(http.StatusUnauthorized, gin.H{
            "message": err.Error(),
        })
        return
    }

    c.JSON(http.StatusOK, gin.H{
        "token": token,
    })
}