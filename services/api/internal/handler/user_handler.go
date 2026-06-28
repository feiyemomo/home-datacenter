package handler

import (
    "net/http"

    "github.com/gin-gonic/gin"

    "home-datacenter-api/internal/service"
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

func (h *UserHandler) Me(c *gin.Context) {

    userID := c.GetUint("user_id")

    user, err := h.userService.GetByID(userID)
    if err != nil {
        c.JSON(http.StatusNotFound, gin.H{
            "message": "user not found",
        })
        return
    }

    c.JSON(http.StatusOK, gin.H{
        "id":       user.ID,
        "name":     user.Name,
        "is_admin": user.IsAdmin,
    })
}