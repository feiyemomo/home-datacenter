package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/service"
	"home-datacenter-api/internal/utils"
)

type DeviceHandler struct {
	deviceService *service.DeviceService
	userService   *service.UserService
	bus           *eventbus.Bus
}

func NewDeviceHandler(
	deviceService *service.DeviceService,
	userService *service.UserService,
	bus *eventbus.Bus,
) *DeviceHandler {
	return &DeviceHandler{
		deviceService: deviceService,
		userService:   userService,
		bus:           bus,
	}
}

// deviceResponse is the JSON shape returned by the device endpoints.
// AccessKeyHash is intentionally excluded so the hash never leaves
// the server over the API.
type deviceResponse struct {
	ID          uint           `json:"id"`
	UserID      uint           `json:"user_id"`
	DeviceName  string         `json:"device_name"`
	LastLoginAt utils.NullTime `json:"last_login_at"`
	RevokedAt   utils.NullTime `json:"revoked_at"`
	LastIP      string         `json:"last_ip"`
	CreatedAt   string         `json:"created_at"`
	UpdatedAt   string         `json:"updated_at"`
}

func toDeviceResponse(d model.Device) deviceResponse {
	return deviceResponse{
		ID:          d.ID,
		UserID:      d.UserID,
		DeviceName:  d.DeviceName,
		LastLoginAt: d.LastLoginAt,
		RevokedAt:   d.RevokedAt,
		LastIP:      d.LastIP,
		CreatedAt:   d.CreatedAt.Format("2006-01-02 15:04:05"),
		UpdatedAt:   d.UpdatedAt.Format("2006-01-02 15:04:05"),
	}
}

// List returns devices visible to the current user.
//
// The optional `scope` query param selects the visibility range:
//
//	scope=mine (default) -> the caller's own devices
//	scope=all            -> all devices (admin only)
//
// Non-admin callers always receive their own devices, even when
// they ask for scope=all — the request silently falls back to
// "mine" rather than 403 so the endpoint never leaks the existence
// of other users' devices. Admins preserve the historical
// behaviour (all devices) when scope=all.
//
// Revoked devices are included so admins can audit them.
//
//	Route: GET /api/v1/device/list
func (h *DeviceHandler) List(c *gin.Context) {
	userID := c.GetUint("user_id")

	user, err := h.userService.GetByID(userID)
	if err != nil {
		utils.Fail(c, http.StatusNotFound, "user not found")
		return
	}

	// scope=all + admin -> every device; everything else (mine, or a
	// non-admin asking for all) -> the caller's own devices only.
	scope := c.DefaultQuery("scope", "mine")
	var devices []model.Device
	if scope == "all" && user.IsAdmin {
		devices, err = h.deviceService.ListDevices()
	} else {
		devices, err = h.deviceService.ListDevicesByUser(userID)
	}
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to list devices")
		return
	}

	result := make([]deviceResponse, 0, len(devices))
	for _, d := range devices {
		result = append(result, toDeviceResponse(d))
	}

	utils.Success(c, gin.H{
		"devices": result,
	})
}

// createDeviceRequest is the JSON body for POST /api/v1/device.
type createDeviceRequest struct {
	DeviceName string `json:"device_name"`
}

// Create handles POST /api/v1/device.
//
// Creates a new device for the authenticated user and returns the
// plaintext access_key exactly once — only the SHA256 hash is
// persisted, so the caller must store the key immediately because
// it can never be recovered from the database.
//
// Request body: {"device_name": "我的手机"}
// Response:     {"code":0, "data": {"device": {...}, "access_key": "..."}}
//
//	Route: POST /api/v1/device
//	Status: 200 + device/access_key on success
//	Status: 400 on invalid body or name length
//	Status: 500 on internal error
func (h *DeviceHandler) Create(c *gin.Context) {
	userID := c.GetUint("user_id")

	var req createDeviceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate length 1-64 runes (after trim). The service re-checks,
	// but doing it here gives a clean 400 without crossing layers.
	if n := utf8.RuneCountInString(strings.TrimSpace(req.DeviceName)); n < 1 || n > 64 {
		utils.Fail(c, http.StatusBadRequest, "device_name must be 1-64 chars")
		return
	}

	device, accessKey, err := h.deviceService.CreateDevice(userID, req.DeviceName)
	if err != nil {
		if errors.Is(err, service.ErrInvalidDeviceName) {
			utils.Fail(c, http.StatusBadRequest, "device_name must be 1-64 chars")
			return
		}
		utils.Fail(c, http.StatusInternalServerError, "failed to create device")
		return
	}

	utils.Success(c, gin.H{
		"device":     toDeviceResponse(*device),
		"access_key": accessKey,
	})
}

// Delete revokes a device (soft delete).
//
// The device row is kept for audit; revoked_at is set so the JWT
// middleware immediately rejects tokens issued for that device.
//
//	Admin    -> may revoke any device
//	Non-admin -> may only revoke their own devices
//
// Route: DELETE /api/v1/device/:id
// Idempotent: revoking an already-revoked device still returns success.
func (h *DeviceHandler) Delete(c *gin.Context) {
	userID := c.GetUint("user_id")

	idStr := c.Param("id")
	idParsed, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid device id")
		return
	}
	deviceID := uint(idParsed)

	// Load the device first so we can check ownership.
	device, err := h.deviceService.GetDeviceByID(deviceID)
	if err != nil {
		utils.Fail(c, http.StatusNotFound, "device not found")
		return
	}

	// Ownership / admin check.
	user, err := h.userService.GetByID(userID)
	if err != nil {
		utils.Fail(c, http.StatusNotFound, "user not found")
		return
	}
	if !user.IsAdmin && device.UserID != userID {
		utils.Fail(c, http.StatusForbidden, "forbidden")
		return
	}

	// Idempotent: revoking an already-revoked device is a no-op.
	if device.RevokedAt.Valid {
		utils.Success(c, nil)
		return
	}

	if err := h.deviceService.RevokeDevice(deviceID); err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to revoke device")
		return
	}

	// Publish user.logout so the log subscriber can persist a
	// human-readable audit entry ("设备 #N 登出") and the WS Hub
	// can push it to connected dashboards. We snapshot the device
	// fields BEFORE the revoke so DeviceName / UserID are the
	// pre-revoke values, not zero values after a row update.
	if h.bus != nil {
		payload, _ := json.Marshal(eventbus.UserLogoutPayload{
			UserID:     device.UserID,
			DeviceID:   device.ID,
			DeviceName: device.DeviceName,
			Ts:         time.Now().Unix(),
		})
		h.bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicUserLogout,
			Payload: payload,
			Source:  eventbus.SourceSystem,
		})
	}

	utils.Success(c, nil)
}
