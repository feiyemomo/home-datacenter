package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/service"
	"home-datacenter-api/internal/utils"
)

type DeviceHandler struct {
	deviceService *service.DeviceService
	userService   *service.UserService
}

func NewDeviceHandler(
	deviceService *service.DeviceService,
	userService *service.UserService,
) *DeviceHandler {
	return &DeviceHandler{
		deviceService: deviceService,
		userService:   userService,
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
//	Admin    -> all devices
//	Non-admin -> only their own devices
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

	var devices []model.Device
	if user.IsAdmin {
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

	utils.Success(c, nil)
}

// Purge permanently deletes a device row from the database.
// Only works on already-revoked devices — call DELETE /device/:id
// first to revoke, then DELETE /device/:id/purge to remove.
//
// Non-admin -> may only purge their own devices.
//
// Route: DELETE /api/v1/device/:id/purge
func (h *DeviceHandler) Purge(c *gin.Context) {
	userID := c.GetUint("user_id")

	idStr := c.Param("id")
	idParsed, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid device id")
		return
	}
	deviceID := uint(idParsed)

	device, err := h.deviceService.GetDeviceByID(deviceID)
	if err != nil {
		utils.Fail(c, http.StatusNotFound, "device not found")
		return
	}

	user, err := h.userService.GetByID(userID)
	if err != nil {
		utils.Fail(c, http.StatusNotFound, "user not found")
		return
	}
	if !user.IsAdmin && device.UserID != userID {
		utils.Fail(c, http.StatusForbidden, "forbidden")
		return
	}

	if !device.RevokedAt.Valid {
		utils.Fail(c, http.StatusConflict, "device must be revoked before purging")
		return
	}

	if err := h.deviceService.HardDelete(deviceID); err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to delete device")
		return
	}

	utils.Success(c, nil)
}
