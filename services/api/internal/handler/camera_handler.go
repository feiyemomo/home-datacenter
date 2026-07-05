package handler

import (
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

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
	Reg        *camera.Registry
	ONVIF      *camera.ONVIFController
	Rec        *camera.Recorder
	PublicBase string // mirrors camera.webrtc_public_base (LAN if blank)
	RawIce     string // JSON string from camera.ice_servers
	UserSvc    UserResolver
}

// UserResolver is the subset of the user service CameraHandler
// needs to enforce per-user visibility. A concrete *service.UserService
// satisfies it.
type UserResolver interface {
	GetIsAdmin(userID uint) (bool, error)
}

func NewCameraHandler(reg *camera.Registry, onvif *camera.ONVIFController, rec *camera.Recorder, publicBase, rawIce string, userSvc UserResolver) *CameraHandler {
	return &CameraHandler{Reg: reg, ONVIF: onvif, Rec: rec, PublicBase: publicBase, RawIce: rawIce, UserSvc: userSvc}
}

// callerIsAdmin returns (userID, isAdmin, ok) for the current request.
// ok is false if the user is not in the gin context (route misconfig)
// or the lookup failed.
func (h *CameraHandler) callerIsAdmin(c *gin.Context) (uint, bool, bool) {
	raw, exists := c.Get("user_id")
	if !exists {
		return 0, false, false
	}
	uid, ok := raw.(uint)
	if !ok {
		return 0, false, false
	}
	if h.UserSvc == nil {
		return uid, true, true // dev / test
	}
	isAdmin, err := h.UserSvc.GetIsAdmin(uid)
	if err != nil {
		return uid, false, true
	}
	return uid, isAdmin, true
}

// requireCanRead loads the camera at :id and rejects the request
// when the caller is not allowed to see it. The success path
// returns the loaded *model.Camera so handlers don't have to
// re-fetch.
func (h *CameraHandler) requireCanRead(c *gin.Context) (*model.Camera, bool) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return nil, false
	}
	cam, err := h.Reg.Get(uint(id))
	if err != nil {
		utils.Fail(c, http.StatusNotFound, "camera not found")
		return nil, false
	}
	uid, isAdmin, ok := h.callerIsAdmin(c)
	if !ok {
		utils.Fail(c, http.StatusUnauthorized, "unauthenticated")
		return nil, false
	}
	if !h.Reg.CanRead(cam, uid, isAdmin) {
		utils.Fail(c, http.StatusForbidden, "not your camera")
		return nil, false
	}
	return cam, true
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
	// Transcode opts the camera into ffmpeg-based H.264
	// transcoding (see model.Camera.Transcode comment). Default
	// false to match the platform's "HEVC passthrough works on
	// HEVC-capable browsers, transcode is opt-in" contract.
	Transcode bool `json:"transcode"`
}

// Register — POST /api/v1/cameras
func (h *CameraHandler) Register(c *gin.Context) {
	var req registerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, err.Error())
		return
	}
	uid, _, ok := h.callerIsAdmin(c)
	if !ok {
		utils.Fail(c, http.StatusUnauthorized, "unauthenticated")
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
		Transcode:    req.Transcode,
		OwnerID:      uid,
	})
	if err != nil {
		// Distinguish the two failure modes the registry can
		// surface so the front-end can render a useful message.
		//
		//   - UNIQUE constraint on cameras.stream_name → 409
		//     (the operator tried to register two cameras with
		//     the same friendly name; the DB rejected the
		//     second one before we even talked to go2rtc).
		//   - Everything else → 502 (go2rtc rejected or was
		//     unreachable; the underlying problem is the
		//     go2rtc side, not the request).
		msg := err.Error()
		if strings.Contains(msg, "UNIQUE constraint failed") {
			utils.Fail(c, http.StatusConflict, "a camera with this name already exists (the dashboard name is the go2rtc stream key); pick a unique name")
			return
		}
		utils.Fail(c, http.StatusBadGateway, msg)
		return
	}
	utils.Success(c, cameraView(cam, h.Reg.StreamConfig(cam)))
}

// SetPreset — PUT /api/v1/cameras/:id/presets/:alias
//
//	{ "token": "Preset_1" }
type presetSetReq struct {
	Token string `json:"token" binding:"required"`
}

func (h *CameraHandler) SetPreset(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	alias := c.Param("alias")
	if alias == "" {
		utils.Fail(c, http.StatusBadRequest, "alias required")
		return
	}
	var req presetSetReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, err.Error())
		return
	}
	cam, err := h.Reg.SetPreset(uint(id), alias, req.Token)
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	utils.Success(c, gin.H{"id": id, "alias": alias, "token": req.Token, "presets": cam.Presets})
}

// DeletePreset — DELETE /api/v1/cameras/:id/presets/:alias
func (h *CameraHandler) DeletePreset(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	cam, err := h.Reg.DeletePreset(uint(id), c.Param("alias"))
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	utils.Success(c, gin.H{"id": id, "presets": cam.Presets})
}

// ListPresets — GET /api/v1/cameras/:id/presets/discover
// Returns the canonical ONVIF preset list (no aliases).
func (h *CameraHandler) ListPresets(c *gin.Context) {
	if _, ok := h.requireCanRead(c); !ok {
		return
	}
	id, _ := strconv.Atoi(c.Param("id"))
	ps, err := h.Reg.ListPresets(c.Request.Context(), uint(id))
	if err != nil {
		utils.Fail(c, http.StatusBadGateway, err.Error())
		return
	}
	utils.Success(c, ps)
}

// GotoPreset — POST /api/v1/cameras/:id/preset/:alias
//
//	{ "speed": 0.5 }
type gotoPresetReq struct {
	Speed float64 `json:"speed"`
}

func (h *CameraHandler) GotoPreset(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	alias := c.Param("alias")
	var req gotoPresetReq
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		utils.Fail(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.Reg.GotoPreset(c.Request.Context(), uint(id), alias, req.Speed); err != nil {
		utils.Fail(c, http.StatusBadGateway, err.Error())
		return
	}
	utils.Success(c, gin.H{"id": id, "alias": alias, "speed": req.Speed})
}

// SetRecordingPlan — PUT /api/v1/cameras/:id/recording
//
//	{ "enabled": true, "segment_seconds": 600, "retention_days": 7,
//	  "output_dir": "/data/recordings", "cron": "" }
func (h *CameraHandler) SetRecordingPlan(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	var plan camera.RecordingPlan
	if err := c.ShouldBindJSON(&plan); err != nil {
		utils.Fail(c, http.StatusBadRequest, err.Error())
		return
	}
	if plan.SegmentSeconds == 0 {
		plan.SegmentSeconds = 600
	}
	if err := h.Rec.SetPlan(uint(id), plan); err != nil {
		utils.Fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	utils.Success(c, gin.H{"id": id, "plan": plan})
}

// ListRecordings — GET /api/v1/cameras/:id/recordings?limit=100
func (h *CameraHandler) ListRecordings(c *gin.Context) {
	if _, ok := h.requireCanRead(c); !ok {
		return
	}
	id, _ := strconv.Atoi(c.Param("id"))
	limit, _ := strconv.Atoi(c.Query("limit"))
	recs, err := h.Rec.ListRecordings(uint(id), limit)
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	views := make([]gin.H, 0, len(recs))
	for _, r := range recs {
		views = append(views, gin.H{
			"id":               r.ID,
			"camera_id":        r.CameraID,
			"start_at":         r.StartAt,
			"end_at":           r.EndAt,
			"duration_seconds": r.DurationSeconds,
			"size_bytes":       r.SizeBytes,
			"size_human":       humanSize(r.SizeBytes),
			"file_path":        r.FilePath,
		})
	}
	utils.Success(c, views)
}

// DeleteRecording — DELETE /api/v1/cameras/:id/recordings/:recId
func (h *CameraHandler) DeleteRecording(c *gin.Context) {
	recID, err := strconv.Atoi(c.Param("recId"))
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid recId")
		return
	}
	if err := h.Rec.DeleteRecording(uint(recID)); err != nil {
		utils.Fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	utils.Success(c, gin.H{"id": recID})
}

// PlayRecording — GET /api/v1/cameras/:id/recordings/:recId/file
//
// Streams the underlying mp4 to the client with HTTP Range support
// (so the browser <video> element can seek). The file path is
// resolved through Recorder.RecordingFilePath which enforces that
// the recording belongs to the camera in the URL — this prevents
// any /:recId/../ trickery from reading arbitrary files on disk.
func (h *CameraHandler) PlayRecording(c *gin.Context) {
	if _, ok := h.requireCanRead(c); !ok {
		return
	}
	camID, _ := strconv.Atoi(c.Param("id"))
	recID, err := strconv.Atoi(c.Param("recId"))
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid recId")
		return
	}
	path, rec, err := h.Rec.RecordingFilePath(uint(camID), uint(recID))
	if err != nil {
		utils.Fail(c, http.StatusNotFound, "recording not found")
		return
	}
	clean := filepath.Clean(path)
	if strings.Contains(clean, "..") {
		utils.Fail(c, http.StatusBadRequest, "invalid path")
		return
	}
	_ = rec
	c.Header("Content-Disposition", "inline")
	http.ServeFile(c.Writer, c.Request, clean)
}

// humanSize is exposed at handler scope (mirrors camera.humanSize).
func humanSize(n int64) string {
	const k = 1024
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	f := float64(n)
	for f >= k && i < len(units)-1 {
		f /= k
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// ICE — GET /api/v1/cameras/ice
//
// Returns the global ICE config (STUN/TURN) and the WebRTC base URL
// the front-end should use. The base URL is set by the server-side
// config:
//
//   - camera.webrtc_public_base="" → LAN: "http://home-go2rtc:1984"
//   - camera.webrtc_public_base="https://cam.feiyemomo.top" → tunnel
//   - (TURN is just a STUN/TURN entry in camera.ice_servers)
//
// This is mounted on the cameras group but not on /:id so it never
// collides with the numeric id route. Auth required (any user) so
// non-admin apps can still pick up the ICE config.
func (h *CameraHandler) ICE(c *gin.Context) {
	lanBase := h.Reg.Go2.Base
	cfg := camera.BuildIceConfig(h.RawIce, h.PublicBase, lanBase)
	utils.Success(c, cfg)
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
		profile = cam.OnvifProfileToken
	}
	// Auto-discover the ONVIF media profile if neither the request
	// nor the DB row has one. This self-heals cameras that were
	// registered while ONVIF was briefly unreachable.
	if profile == "" && h.ONVIF != nil {
		if ps, perr := h.ONVIF.DiscoverProfiles(
			c.Request.Context(), cam.Host, cam.ONVIFPort, user, pass,
		); perr == nil && len(ps) > 0 {
			profile = ps[0].Token
			// Persist for next time so we skip the discovery round-trip.
			h.Reg.SaveProfileToken(cam.ID, profile)
		} else if perr != nil {
			log.Printf("ptz: onvif discover %s:%d: %v", cam.Host, cam.ONVIFPort, perr)
		}
	}
	if profile == "" {
		utils.Fail(c, http.StatusBadRequest,
			"missing onvif profile_token (re-register with profile_token, or set it in /api/v1/cameras/:id)")
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
		"transcode":    cam.Transcode,
		"created_at":   cam.CreatedAt,
		"updated_at":   cam.UpdatedAt,
	}
}
