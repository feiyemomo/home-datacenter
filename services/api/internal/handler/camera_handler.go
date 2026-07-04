package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/camera"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

// CameraHandler exposes platformized camera endpoints.
//
// Routes (all under JWT auth, admin-only mutations):
//
//	POST   /api/v1/cameras       Register a new camera (admin)
//	GET    /api/v1/cameras       List cameras
//	GET    /api/v1/cameras/:id   Fetch a single camera
//	DELETE /api/v1/cameras/:id   Unregister (admin)
//	POST   /api/v1/cameras/:id/ptz  PTZ control (admin)
//
// Credentials are never returned in responses — they are encrypted
// at rest via utils.SecretBox.
type CameraHandler struct {
	Reg   *camera.Registry
	ONVIF *camera.ONVIFController
}

func NewCameraHandler(reg *camera.Registry, onvif *camera.ONVIFController) *CameraHandler {
	return &CameraHandler{Reg: reg, ONVIF: onvif}
}

// registerReq is the wire format for POST /api/v1/cameras.
//
//	{
//	  "name": "前门",
//	  "vendor": "hikvision",
//	  "host": "192.168.31.100",
//	  "onvif_port": 80,
//	  "rtsp_port": 554,
//	  "channel_id": 101,
//	  "username": "admin",
//	  "password": "...",
//	  "ptz": true,
//	  "audio": true,
//	  "motion": true,
//	  "profile_token": ""        // optional; auto-discovered if blank
//	}
type registerReq struct {
	Name         string `json:"name" binding:"required"`
	Vendor       string `json:"vendor"`
	Host         string `json:"host" binding:"required"`
	ONVIFPort    int    `json:"onvif_port"`
	RTSPPort     int    `json:"rtsp_port"`
	ChannelID    int    `json:"channel_id"`
	Username     string `json:"username"`
	Password     string `json:"password" binding:"required"`
	PTZ          bool   `json:"ptz"`
	Audio        bool   `json:"audio"`
	Motion       bool   `json:"motion"`
	ProfileToken string `json:"profile_token"`
}

// Register — POST /api/v1/cameras
func (h *CameraHandler) Register(c *gin.Context) {
	var req registerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, err.Error())
		return
	}
	cam, err := h.Reg.Register(c.Request.Context(), camera.RegisterInput{
		Name:         req.Name,
		Vendor:       req.Vendor,
		Host:         req.Host,
		ONVIFPort:    req.ONVIFPort,
		RTSPPort:     req.RTSPPort,
		ChannelID:    req.ChannelID,
		Username:     req.Username,
		Password:     req.Password,
		PTZ:          req.PTZ,
		Audio:        req.Audio,
		Motion:       req.Motion,
		ProfileToken: req.ProfileToken,
	})
	if err != nil {
		// 502: go2rtc didn't accept the stream
		utils.Fail(c, http.StatusBadGateway, err.Error())
		return
	}
	utils.Success(c, cameraView(cam, h.Reg.StreamConfig(cam)))
}

// List — GET /api/v1/cameras
func (h *CameraHandler) List(c *gin.Context) {
	cams := h.Reg.List()
	views := make([]gin.H, 0, len(cams))
	for i := range cams {
		views = append(views, cameraView(&cams[i], h.Reg.StreamConfig(&cams[i])))
	}
	utils.Success(c, views)
}

// Get — GET /api/v1/cameras/:id
func (h *CameraHandler) Get(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	cam, err := h.Reg.Get(uint(id))
	if err != nil {
		utils.Fail(c, http.StatusNotFound, "camera not found")
		return
	}
	utils.Success(c, cameraView(cam, h.Reg.StreamConfig(cam)))
}

// Delete — DELETE /api/v1/cameras/:id
func (h *CameraHandler) Delete(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.Reg.Unregister(c.Request.Context(), uint(id)); err != nil {
		utils.Fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	h.ONVIF.Forget(uint(id))
	utils.Success(c, gin.H{"id": id})
}

// ptzReq is the wire format for POST /api/v1/cameras/:id/ptz.
//
//	{ "command": "left", "speed": 0.5, "profile_token": "" }
//
// `speed` is 0..1; the controller clamps to that range.
// `profile_token` defaults to cam.Meta["onvif_profile"] if empty.
type ptzReq struct {
	Command      string  `json:"command" binding:"required"`
	Speed        float64 `json:"speed"`
	ProfileToken string  `json:"profile_token"`
}

// PTZ — POST /api/v1/cameras/:id/ptz
func (h *CameraHandler) PTZ(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	var req ptzReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, err.Error())
		return
	}
	cam, err := h.Reg.Get(uint(id))
	if err != nil {
		utils.Fail(c, http.StatusNotFound, "camera not found")
		return
	}
	if cam.Credentials == nil {
		utils.Fail(c, http.StatusFailedDependency, "no credentials on file")
		return
	}

	user, pass, err := h.Reg.DecryptCredentials(cam)
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, "credentials decrypt failed")
		return
	}

	profile := req.ProfileToken
	if profile == "" {
		if v, ok := cam.Meta["onvif_profile"].(string); ok {
			profile = v
		}
	}
	if profile == "" {
		utils.Fail(c, http.StatusBadRequest,
			"missing onvif profile_token (re-register with profile_token)")
		return
	}

	speed := req.Speed
	if speed == 0 {
		speed = 0.5
	}

	if err := h.ONVIF.ContinuousMove(
		c.Request.Context(),
		cam.ID, cam.Host, cam.ONVIFPort,
		user, pass, profile,
		camera.PTZCommand(req.Command), speed,
	); err != nil {
		utils.Fail(c, http.StatusBadGateway, err.Error())
		return
	}
	utils.Success(c, gin.H{
		"id":      cam.ID,
		"command": req.Command,
		"speed":   speed,
	})
}

// cameraView is the public projection of a Camera record: it drops
// the encrypted Credentials blob and embeds the live stream URLs.
func cameraView(cam *model.Camera, stream camera.StreamConfig) gin.H {
	return gin.H{
		"id":           cam.ID,
		"type":         cam.Type,
		"name":         cam.Name,
		"vendor":       cam.Vendor,
		"host":         cam.Host,
		"onvif_port":   cam.ONVIFPort,
		"rtsp_port":    cam.RTSPPort,
		"channel_id":   cam.ChannelID,
		"status":       cam.Status,
		"last_seen_at": cam.LastSeenAt,
		"capabilities": cam.Capabilities,
		"meta":         cam.Meta,
		"stream":       stream,
		"created_at":   cam.CreatedAt,
		"updated_at":   cam.UpdatedAt,
	}
}
