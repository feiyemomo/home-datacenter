package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/device"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/service"
	"home-datacenter-api/internal/utils"
)

type UserHandler struct {
	userService   *service.UserService
	deviceService *service.DeviceService
	deviceMgr     *device.Manager
	deviceRepo    *repository.DeviceRepository
	bus           *eventbus.Bus
}

func NewUserHandler(
	userService *service.UserService,
	deviceService *service.DeviceService,
	deviceMgr *device.Manager,
	deviceRepo *repository.DeviceRepository,
	bus *eventbus.Bus,
) *UserHandler {
	return &UserHandler{
		userService:   userService,
		deviceService: deviceService,
		deviceMgr:     deviceMgr,
		deviceRepo:    deviceRepo,
		bus:           bus,
	}
}

// publishUserManageEvent emits a user.create / user.update /
// user.delete event so the log subscriber can persist a
// human-readable audit entry. TargetName is snapshotted from the
// model row so a delete event still carries the friendly name
// after the row is gone. action must be "create" | "update" | "delete".
func (h *UserHandler) publishUserManageEvent(adminID, targetID uint, targetName, action string, isAdmin bool) {
	if h.bus == nil {
		return
	}
	var topic string
	switch action {
	case "create":
		topic = eventbus.TopicUserCreate
	case "update":
		topic = eventbus.TopicUserUpdate
	case "delete":
		topic = eventbus.TopicUserDelete
	default:
		return
	}
	payload, _ := json.Marshal(eventbus.UserManagePayload{
		AdminID:    adminID,
		TargetID:   targetID,
		TargetName: targetName,
		Action:     action,
		IsAdmin:    isAdmin,
		Ts:         time.Now().Unix(),
	})
	h.bus.Publish(eventbus.Event{
		Topic:   topic,
		Payload: payload,
		Source:  eventbus.SourceSystem,
	})
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

// List returns every user along with each user's device_count and online status.
// Admin-only (route-level RequireAdmin guard).
//
//	Route: GET /api/v1/user
func (h *UserHandler) List(c *gin.Context) {
	rows, err := h.userService.ListWithDeviceCount()
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to list users")
		return
	}

	// Build a set of online device IDs for O(1) lookup.
	onlineIDs := make(map[uint]bool)
	for _, id := range h.deviceMgr.GetOnlineDevices() {
		onlineIDs[id] = true
	}

	resps := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		uc := toUserWithCount(r)

		// Check if the user has any online device.
		online := false
		devices, err := h.deviceRepo.GetByUserID(uc.ID)
		if err == nil {
			for _, d := range devices {
				if onlineIDs[d.ID] {
					online = true
					break
				}
			}
		}

		resps = append(resps, gin.H{
			"id":           uc.ID,
			"name":         uc.Name,
			"is_admin":     uc.IsAdmin,
			"created_at":   uc.CreatedAt,
			"updated_at":   uc.UpdatedAt,
			"device_count": uc.DeviceCount,
			"online":       online,
		})
	}
	utils.Success(c, gin.H{"users": resps})
}

// createUserRequest is the JSON body for POST /api/v1/user.
type createUserRequest struct {
	Name    string `json:"name"`
	IsAdmin bool   `json:"is_admin"`
}

// Create inserts a new user. Admin-only.
//
//	Route: POST /api/v1/user
//	Status: 200 + user payload on success
//	Status: 400 on invalid name
//	Status: 409 on duplicate name
func (h *UserHandler) Create(c *gin.Context) {
	var req createUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	result, err := h.userService.Create(req.Name, req.IsAdmin)
	if err != nil {
		writeUserServiceError(c, err)
		return
	}
	u := result.User

	// v1.8.20: audit-trail event for user creation.
	h.publishUserManageEvent(c.GetUint("user_id"), u.ID, u.Name, "create", u.IsAdmin)

	// Create a default device for the new user so they can
	// immediately bind with the returned access_key.
	deviceName := fmt.Sprintf("%s-device", u.Name)
	device, accessKey, err := h.deviceService.CreateDevice(u.ID, deviceName)
	if err != nil {
		// Device creation failed but user was created — log and
		// still return the user without access_key.
		utils.Success(c, gin.H{
			"id":         u.ID,
			"name":       u.Name,
			"is_admin":   u.IsAdmin,
			"created_at": u.CreatedAt.Format("2006-01-02 15:04:05"),
			"updated_at": u.UpdatedAt.Format("2006-01-02 15:04:05"),
		})
		return
	}

	utils.Success(c, gin.H{
		"id":         u.ID,
		"name":       u.Name,
		"is_admin":   u.IsAdmin,
		"created_at": u.CreatedAt.Format("2006-01-02 15:04:05"),
		"updated_at": u.UpdatedAt.Format("2006-01-02 15:04:05"),
		"access_key": accessKey,
		"device": gin.H{
			"id":          device.ID,
			"device_name": device.DeviceName,
		},
	})
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
	// v1.8.20: audit-trail event for user update.
	h.publishUserManageEvent(callerID, u.ID, u.Name, "update", u.IsAdmin)
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
	// v1.8.20: snapshot the target user's name BEFORE the row is
	// deleted so the audit-log event carries a friendly label
	// instead of just "#<id>".
	targetName := ""
	targetIsAdmin := false
	if u, err := h.userService.GetByID(id); err == nil {
		targetName = u.Name
		targetIsAdmin = u.IsAdmin
	}
	deletedDevices, err := h.userService.Delete(id, callerID)
	if err != nil {
		writeUserServiceError(c, err)
		return
	}
	// v1.8.20: audit-trail event for user deletion.
	h.publishUserManageEvent(callerID, id, targetName, "delete", targetIsAdmin)
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
