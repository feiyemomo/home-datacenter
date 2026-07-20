package handler

import (
	"errors"
	"net/http"
	"strconv"

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

// userWithCount is the union of model.User + an optional device_count
// used by List. We avoid embedding model.User in the handler DTO so
// the JSON shape is owned by this file (not by GORM's model tag
// convention).
type userWithCount struct {
	ID          uint
	Name        string
	IsAdmin     bool
	CreatedAt   string
	UpdatedAt   string
	DeviceCount int64
}

func toUserWithCount(s service.UserSummary) userWithCount {
	return userWithCount{
		ID:          s.User.ID,
		Name:        s.User.Name,
		IsAdmin:     s.User.IsAdmin,
		CreatedAt:   s.User.CreatedAt.Format("2006-01-02 15:04:05"),
		UpdatedAt:   s.User.UpdatedAt.Format("2006-01-02 15:04:05"),
		DeviceCount: s.DeviceCount,
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

// List returns every user along with each user's device_count.
// Admin-only (route-level RequireAdmin guard).
//
//	Route: GET /api/v1/user
func (h *UserHandler) List(c *gin.Context) {
	rows, err := h.userService.ListWithDeviceCount()
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to list users")
		return
	}
	resps := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		uc := toUserWithCount(r)
		resps = append(resps, gin.H{
			"id":           uc.ID,
			"name":         uc.Name,
			"is_admin":     uc.IsAdmin,
			"created_at":   uc.CreatedAt,
			"updated_at":   uc.UpdatedAt,
			"device_count": uc.DeviceCount,
		})
	}
	utils.Success(c, gin.H{"users": resps})
}

// createUserRequest is the JSON body for POST /api/v1/user.
//
// initial_device_name is optional. When provided, a first auth
// device is created alongside the user and the plaintext
// AccessKey is returned in the response so the admin can hand
// it to the new user immediately.
type createUserRequest struct {
	Name              string `json:"name"`
	IsAdmin           bool   `json:"is_admin"`
	InitialDeviceName string `json:"initial_device_name"`
}

// Create inserts a new user. Admin-only.
//
//	Route: POST /api/v1/user
//	Status: 200 + user payload on success
//	Status: 400 on invalid name
//	Status: 409 on duplicate name
//
// When initial_device_name is provided in the request body, the
// response includes a `device` object and an `access_key` field
// with the plaintext AccessKey. The AccessKey is only available
// at creation time — only its SHA256 hash is stored in the DB.
func (h *UserHandler) Create(c *gin.Context) {
	var req createUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	result, err := h.userService.Create(req.Name, req.IsAdmin, req.InitialDeviceName)
	if err != nil {
		writeUserServiceError(c, err)
		return
	}
	u := result.User
	resp := gin.H{
		"id":         u.ID,
		"name":       u.Name,
		"is_admin":   u.IsAdmin,
		"created_at": u.CreatedAt.Format("2006-01-02 15:04:05"),
		"updated_at": u.UpdatedAt.Format("2006-01-02 15:04:05"),
	}
	if result.Device != nil {
		resp["device"] = gin.H{
			"id":          result.Device.ID,
			"device_name": result.Device.DeviceName,
		}
		resp["access_key"] = result.AccessKey
	}
	utils.Success(c, resp)
}

// getUser fetches one user. Admin-only.
//
//	Route: GET /api/v1/user/:id
func (h *UserHandler) Get(c *gin.Context) {
	id, ok := parseUserID(c)
	if !ok {
		return
	}
	u, err := h.userService.GetByID(id)
	if err != nil {
		if errors.Is(err, service.ErrUserNotFound) {
			utils.Fail(c, http.StatusNotFound, "user not found")
			return
		}
		utils.Fail(c, http.StatusInternalServerError, "failed to fetch user")
		return
	}
	utils.Success(c, gin.H{
		"id":         u.ID,
		"name":       u.Name,
		"is_admin":   u.IsAdmin,
		"created_at": u.CreatedAt.Format("2006-01-02 15:04:05"),
		"updated_at": u.UpdatedAt.Format("2006-01-02 15:04:05"),
	})
}

// updateUserRequest is the JSON body for PUT /api/v1/user/:id.
// Both fields are optional — a missing key means "leave unchanged".
// A pointer-typed struct field makes the "absent vs. zero" check
// trivial: `nil` means "don't touch", `&""` means "set to empty"
// (which the service will reject with ErrInvalidName).
type updateUserRequest struct {
	Name    *string `json:"name,omitempty"`
	IsAdmin *bool   `json:"is_admin,omitempty"`
}

// Update performs a partial update. Admin-only.
//
//	Route: PUT /api/v1/user/:id
//	Status: 200 + user payload on success
//	Status: 400 on invalid name / self-demote
//	Status: 404 on unknown user
//	Status: 409 on duplicate name
//	Status: 500 on internal error
func (h *UserHandler) Update(c *gin.Context) {
	id, ok := parseUserID(c)
	if !ok {
		return
	}
	callerID := c.GetUint("user_id")

	var req updateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == nil && req.IsAdmin == nil {
		utils.Fail(c, http.StatusBadRequest, "no fields to update")
		return
	}
	u, err := h.userService.Update(id, callerID, req.Name, req.IsAdmin)
	if err != nil {
		writeUserServiceError(c, err)
		return
	}
	utils.Success(c, gin.H{
		"id":         u.ID,
		"name":       u.Name,
		"is_admin":   u.IsAdmin,
		"created_at": u.CreatedAt.Format("2006-01-02 15:04:05"),
		"updated_at": u.UpdatedAt.Format("2006-01-02 15:04:05"),
	})
}

// Delete removes a user and cascades to their devices. Admin-only.
//
//	Route: DELETE /api/v1/user/:id
//	Status: 200 + {deleted_devices: N} on success
//	Status: 400 on self-delete or last-admin guard
//	Status: 404 on unknown user
//	Status: 500 on internal error (DB failure)
func (h *UserHandler) Delete(c *gin.Context) {
	id, ok := parseUserID(c)
	if !ok {
		return
	}
	callerID := c.GetUint("user_id")
	deletedDevices, err := h.userService.Delete(id, callerID)
	if err != nil {
		writeUserServiceError(c, err)
		return
	}
	utils.Success(c, gin.H{
		"deleted_devices": deletedDevices,
	})
}

// parseUserID extracts :id from the path and translates parse
// failures into 400. Returns (id, true) on success; on failure
// the 400 has already been written to the response.
func parseUserID(c *gin.Context) (uint, bool) {
	idStr := c.Param("id")
	idParsed, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid user id")
		return 0, false
	}
	return uint(idParsed), true
}

// writeUserServiceError centralises the service-error → HTTP
// status mapping so every handler that calls into UserService
// returns the same code for the same condition.
//
//	400 — invalid name, self-delete, self-demote, last-admin
//	404 — user not found
//	409 — name taken
//	500 — internal GORM errors
//
// Last-admin surfaces as 400 with the message "cannot remove/
// demote the last admin" — it's a state-guard, not an internal
// error, so the client can retry after promoting another user.
func writeUserServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidName),
		errors.Is(err, service.ErrSelfDelete),
		errors.Is(err, service.ErrSelfDemote):
		utils.Fail(c, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrUserNotFound):
		utils.Fail(c, http.StatusNotFound, "user not found")
	case errors.Is(err, service.ErrNameTaken):
		utils.Fail(c, http.StatusConflict, "name already in use")
	case errors.Is(err, service.ErrLastAdmin):
		utils.Fail(c, http.StatusBadRequest, err.Error())
	default:
		utils.Fail(c, http.StatusInternalServerError, "operation failed")
	}
}
