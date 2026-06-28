package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/service"
	"home-datacenter-api/internal/utils"
)

type UserHandler struct {
	userService *service.UserService
}

func NewUserHandler(
	userService *service.UserService,
) *UserHandler {
	return &UserHandler{
		userService: userService,
	}
}

// Me returns the identity of the current (JWT-authenticated) user.
//
//	Route: GET /api/v1/user/me
func (h *UserHandler) Me(c *gin.Context) {

	userID := c.GetUint("user_id")

	user, err := h.userService.GetByID(userID)
	if err != nil {
		utils.Fail(c, http.StatusNotFound, "user not found")
		return
	}

	utils.Success(c, gin.H{
		"id":       user.ID,
		"name":     user.Name,
		"is_admin": user.IsAdmin,
	})
}
