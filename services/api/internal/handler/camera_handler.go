package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/camera"
	"home-datacenter-api/internal/eventbus"
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
	bus        *eventbus.Bus
	// frameCache holds the most recent JPEG frame per stream name
	// for up to 2 seconds. The dashboard's camera card polls
	// /frame on every page mount and sometimes rapid-refreshes;
	// serving a cached frame cuts go2rtc round-trips (and the
	// 1-2s cold-stream cost) dramatically for burst traffic while
	// still being "fresh enough" for a live preview. Keyed by
	// stream name only — quality/width variations share the same
	// slot, which is intentional: the dashboard always uses the
	// same defaults, and a stale-but-correct-dimension frame is
	// preferable to multiplying upstream calls.
	frameCache sync.Map

	// transcodeSem serializes recording transcodes. The J4125's iGPU
	// thrashes under concurrent VAAPI load: 5 simultaneous 60s
	// transcodes each balloon from ~10s to ~65s (measured on the NAS),
	// and the app/browser fires several recording requests at once
	// when opening a timeline. A single slot keeps every transcode at
	// full speed. Already-cached minutes serve instantly without
	// touching the semaphore, so re-plays never block the queue.
	transcodeSem chan struct{}
}

// frameCacheEntry is the value type stored in CameraHandler.frameCache.
type frameCacheEntry struct {
	data        []byte
	contentType string
	ts          time.Time
}

// UserResolver is the subset of the user service CameraHandler
// needs to enforce per-user visibility. A concrete *service.UserService
// satisfies it.
type UserResolver interface {
	GetIsAdmin(userID uint) (bool, error)
}

func NewCameraHandler(reg *camera.Registry, onvif *camera.ONVIFController, rec *camera.Recorder, publicBase, rawIce string, userSvc UserResolver, bus *eventbus.Bus) *CameraHandler {
	return &CameraHandler{Reg: reg, ONVIF: onvif, Rec: rec, PublicBase: publicBase, RawIce: rawIce, UserSvc: userSvc, bus: bus, transcodeSem: make(chan struct{}, 1)}
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
	// Codec overrides Transcode. Only "h264" is accepted via the
	// dashboard; "passthrough"/"h265" are legacy values that may
	// exist in the DB (set before the WebRTC-only-H.264 restriction)
	// but cannot be set via UpdateCodec. Empty string inherits
	// legacy Transcode behavior.
	Codec string `json:"codec"`
}

// Register — POST /api/v1/cameras
func (h *CameraHandler) Register(c *gin.Context) {
	var req registerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Printf("[handler] register invalid request body: %v", err)
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
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
		Codec:        req.Codec,
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
		log.Printf("[handler] failed to register camera: %v", err)
		utils.Fail(c, http.StatusBadGateway, "failed to register camera")
		return
	}
	// v1.8.22: audit-trail event for camera registration.
	if h.bus != nil {
		payload, _ := json.Marshal(eventbus.CameraManagePayload{
			AdminID:    c.GetUint("user_id"),
			CameraID:   cam.ID,
			CameraName: cam.Name,
			Action:     "create",
			Ts:         time.Now().Unix(),
		})
		h.bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicCameraCreate,
			Payload: payload,
			Source:  eventbus.SourceSystem,
		})
	}
	utils.Success(c, cameraView(cam, h.Reg.StreamConfig(cam), true, true))
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
		log.Printf("[handler] set preset invalid request body: %v", err)
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	cam, err := h.Reg.SetPreset(uint(id), alias, req.Token)
	if err != nil {
		log.Printf("[handler] failed to set preset: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "failed to set preset")
		return
	}
	if h.bus != nil {
		payload, _ := json.Marshal(eventbus.CameraManagePayload{
			AdminID:    c.GetUint("user_id"),
			CameraID:   uint(id),
			CameraName: cam.Name,
			Action:     "update",
			Detail:     fmt.Sprintf("保存预置位【%s】", alias),
			Ts:         time.Now().Unix(),
		})
		h.bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicCameraUpdate,
			Payload: payload,
			Source:  eventbus.SourceSystem,
		})
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
		log.Printf("[handler] failed to delete preset: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "failed to delete preset")
		return
	}
	if h.bus != nil {
		payload, _ := json.Marshal(eventbus.CameraManagePayload{
			AdminID:    c.GetUint("user_id"),
			CameraID:   uint(id),
			CameraName: cam.Name,
			Action:     "update",
			Detail:     fmt.Sprintf("删除预置位【%s】", c.Param("alias")),
			Ts:         time.Now().Unix(),
		})
		h.bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicCameraUpdate,
			Payload: payload,
			Source:  eventbus.SourceSystem,
		})
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
		log.Printf("[handler] presets not available for camera %d: %v", id, err)
		utils.Success(c, []camera.Preset{})
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
		log.Printf("[handler] goto preset invalid request body: %v", err)
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.Reg.GotoPreset(c.Request.Context(), uint(id), alias, req.Speed); err != nil {
		log.Printf("[handler] failed to goto preset: %v", err)
		utils.Fail(c, http.StatusBadGateway, "failed to goto preset")
		return
	}
	utils.Success(c, gin.H{"id": id, "alias": alias, "speed": req.Speed})
}

// UpdateCodec — PUT /api/v1/cameras/:id/codec
//
//	{ "codec": "h264" }
//
// Changes the output video codec for a camera and re-pushes the
// go2rtc stream so the change is live immediately. Only "h264" is
// accepted — WebRTC's RTP codec registry does not include H.265,
// so passthrough/h265 always 502 on Chrome/Edge/Firefox WebRTC.
// Legacy cameras with codec=passthrough/h265 (set before this
// restriction) still work for backward compatibility but cannot be
// (re)set to those values via this API.
func (h *CameraHandler) UpdateCodec(c *gin.Context) {
	if _, isAdmin, ok := h.callerIsAdmin(c); !ok || !isAdmin {
		utils.Fail(c, http.StatusForbidden, "admin only")
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	var body struct {
		Codec string `json:"codec"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		log.Printf("[handler] update codec invalid request body: %v", err)
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.Reg.UpdateCodec(c.Request.Context(), uint(id), body.Codec); err != nil {
		log.Printf("[handler] failed to update codec: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "failed to update codec")
		return
	}
	// v1.8.22: audit-trail event for codec change.
	if h.bus != nil {
		cameraName := ""
		if cam, gerr := h.Reg.Get(uint(id)); gerr == nil {
			cameraName = cam.Name
		}
		codecName := body.Codec
		if codecName == "hevc" || codecName == "h265" {
			codecName = "H.265 (HEVC)"
		} else if codecName == "h264" {
			codecName = "H.264"
		}
		payload, _ := json.Marshal(eventbus.CameraManagePayload{
			AdminID:    c.GetUint("user_id"),
			CameraID:   uint(id),
			CameraName: cameraName,
			Action:     "update",
			Detail:     fmt.Sprintf("编码格式更改为【%s】", codecName),
			Ts:         time.Now().Unix(),
		})
		h.bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicCameraUpdate,
			Payload: payload,
			Source:  eventbus.SourceSystem,
		})
	}
	utils.Success(c, gin.H{"id": id, "codec": body.Codec})
}

// UpdateAudio — PUT /api/v1/cameras/:id/audio
//
//	{ "enabled": true }
//
// Toggles live audio for a camera. When enabled, the camera's PCMA
// audio track is transcoded to AAC and exposed in the HLS/MP4 stream
// so ExoPlayer and modern browsers can decode it. Disabling strips
// the audio track at the source (saves bandwidth). Re-pushes the
// go2rtc stream so the change is live immediately.
func (h *CameraHandler) UpdateAudio(c *gin.Context) {
	cam, ok := h.requireCanRead(c)
	if !ok {
		return
	}
	id := int(cam.ID)
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		log.Printf("[handler] update audio invalid request body: %v", err)
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.Reg.UpdateAudio(c.Request.Context(), uint(id), body.Enabled); err != nil {
		log.Printf("[handler] failed to update audio: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "failed to update audio")
		return
	}
	// v1.8.22: audit-trail event for audio toggle.
	if h.bus != nil {
		cameraName := ""
		if cam, gerr := h.Reg.Get(uint(id)); gerr == nil {
			cameraName = cam.Name
		}
		audioState := "开启"
		if !body.Enabled {
			audioState = "关闭"
		}
		payload, _ := json.Marshal(eventbus.CameraManagePayload{
			AdminID:    c.GetUint("user_id"),
			CameraID:   uint(id),
			CameraName: cameraName,
			Action:     "update",
			Detail:     fmt.Sprintf("音频输入设置为【%s】", audioState),
			Ts:         time.Now().Unix(),
		})
		h.bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicCameraUpdate,
			Payload: payload,
			Source:  eventbus.SourceSystem,
		})
	}
	utils.Success(c, gin.H{"id": id, "audio": body.Enabled})
}

// UpdateDetectFPS — PUT /api/v1/cameras/:id/detect-fps
//
//	{ "fps": 5 }
func (h *CameraHandler) UpdateDetectFPS(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	if _, isAdmin, ok := h.callerIsAdmin(c); !ok || !isAdmin {
		utils.Fail(c, http.StatusForbidden, "admin only")
		return
	}

	var body struct {
		FPS int `json:"fps" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		utils.Fail(c, http.StatusBadRequest, "fps is required and must be an integer (1-10)")
		return
	}
	if body.FPS < 1 || body.FPS > 10 {
		utils.Fail(c, http.StatusBadRequest, "fps must be between 1 and 10")
		return
	}

	updatedCam, err := h.Reg.UpdateDetectFPS(c.Request.Context(), uint(id), body.FPS)
	if err != nil {
		log.Printf("[handler] failed to update detect fps: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "failed to update detect fps")
		return
	}

	if h.bus != nil {
		payload, _ := json.Marshal(eventbus.CameraManagePayload{
			AdminID:    c.GetUint("user_id"),
			CameraID:   uint(id),
			CameraName: updatedCam.Name,
			Action:     "update_detect_fps",
			Detail:     fmt.Sprintf("更新抽样检测频率为 %d fps", body.FPS),
			Ts:         time.Now().Unix(),
		})
		h.bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicCameraUpdate,
			Payload: payload,
			Source:  eventbus.SourceSystem,
		})
	}

	utils.Success(c, gin.H{
		"id":         updatedCam.ID,
		"detect_fps": body.FPS,
		"meta":       updatedCam.Meta,
	})
}

// SetRecordingPlan — PUT /api/v1/cameras/:id/recording
//
//	{ "enabled": true, "retention_days": 7 }
//
// Toggles Frigate's continuous recording for this camera. Unlike the
// old go2rtc-based recorder (which used /api/recorder — an endpoint
// go2rtc does not actually expose), this delegates to Frigate's own
// record pipeline via the config push API.
func (h *CameraHandler) SetRecordingPlan(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	var body struct {
		Enabled        bool `json:"enabled"`
		SegmentSeconds int  `json:"segment_seconds"` // ignored — Frigate uses 1h segments
		RetentionDays  int  `json:"retention_days"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		log.Printf("[handler] set recording plan invalid request body: %v", err)
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.Reg.SetRecordingEnabled(c.Request.Context(), uint(id), body.Enabled, body.RetentionDays); err != nil {
		log.Printf("[handler] failed to set recording plan: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "failed to set recording plan")
		return
	}
	// v1.8.22: audit-trail event for recording plan change.
	if h.bus != nil {
		cameraName := ""
		if cam, gerr := h.Reg.Get(uint(id)); gerr == nil {
			cameraName = cam.Name
		}
		detail := "录制计划更改为：关闭全天录像"
		if body.Enabled {
			detail = fmt.Sprintf("录制计划更改为：开启全天录像（保留 %d 天）", body.RetentionDays)
		}
		payload, _ := json.Marshal(eventbus.CameraManagePayload{
			AdminID:    c.GetUint("user_id"),
			CameraID:   uint(id),
			CameraName: cameraName,
			Action:     "update",
			Detail:     detail,
			Ts:         time.Now().Unix(),
		})
		h.bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicCameraUpdate,
			Payload: payload,
			Source:  eventbus.SourceSystem,
		})
	}
	utils.Success(c, gin.H{
		"id": id,
		"plan": gin.H{
			"enabled":         body.Enabled,
			"segment_seconds": 3600,
			"retention_days":  body.RetentionDays,
		},
	})
}

// ListRecordings — GET /api/v1/cameras/:id/recordings
//
// Aggregates Frigate's 10-second recording segments into 60-second
// buckets for display. Frigate 0.17 stores all recordings as ~10s
// MP4 files on disk and the /api/<cam>/recordings endpoint returns
// them individually — that's ~360 entries per hour, which is too
// granular for the dashboard. We group segments by minute (floor to
// 60s) and return one entry per minute, with the minute-start
// timestamp as the "id" so the front-end can build a play URL.
//
// v1.5.20: switched from Frigate API to direct disk scan. Frigate
// 0.17's /api/<cam>/recordings endpoint has an internal cap of ~500
// segments regardless of the `after`/`before` window (verified by
// probing with after=1h, 6h, 24h, 7d — all return exactly 503
// segments, ~83 minutes). For a 7-day retention that's unusable —
// the user only sees the last ~80 minutes. Disk has the full
// retention (18233 mp4 files across 3 days when
// record.continuous.days=7), so we walk the disk directly via
// Registry.ListRecordingMinutesFromDisk.
//
// `after` defaults to now-7d (matches Frigate's record.continuous.days
// retention config); `before` defaults to now.
func (h *CameraHandler) ListRecordings(c *gin.Context) {
	cam, ok := h.requireCanRead(c)
	if !ok {
		return
	}
	// v1.8.47: honor optional before/after (unix seconds) so the client
	// can fetch recordings older than the default 7-day window (e.g. a
	// camera's history before it went offline). Defaults match Frigate's
	// record.continuous.days=7 retention: now-7d .. now.
	after := time.Now().AddDate(0, 0, -7).Unix()
	before := time.Now().Unix()
	if s := strings.TrimSpace(c.Query("after")); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			after = v
		}
	}
	if s := strings.TrimSpace(c.Query("before")); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			before = v
		}
	}
	if before < after {
		before, after = after, before
	}
	buckets, err := h.Reg.ListRecordingMinutesFromDisk(cam, after, before)
	if err != nil {
		log.Printf("[handler] failed to list recordings from disk: %v", err)
	}

	// v1.8.35: Cloud cascading — merge archived minutes from Quark Cloud Drive via Alist WebDAV
	if cloudBuckets, cloudErr := h.Reg.ListRecordingMinutesFromCloud(c.Request.Context(), cam, after, before, buckets); cloudErr == nil && len(cloudBuckets) > 0 {
		buckets = append(buckets, cloudBuckets...)
		sort.Slice(buckets, func(i, j int) bool { return buckets[i].StartUnix > buckets[j].StartUnix })
	}

	// buckets is already sorted newest-first.
	views := make([]gin.H, 0, len(buckets))
	for _, b := range buckets {
		storageType := b.Storage
		if storageType == "" {
			storageType = "local"
		}
		views = append(views, gin.H{
			"id":               b.StartUnix,
			"camera_id":        cam.ID,
			"start_at":         time.Unix(b.StartUnix, 0).UTC().Format(time.RFC3339),
			"end_at":           time.Unix(b.EndUnix, 0).UTC().Format(time.RFC3339),
			"duration_seconds": b.SegmentCount * 10,
			"segment_count":    b.SegmentCount,
			"storage":          storageType, // "local" (热) or "cloud" (冷)
			"size_bytes":       0,
			"size_human":       "--",
			"file_path":        "",
		})
	}
	utils.Success(c, views)
}

// DeleteRecording — DELETE /api/v1/cameras/:id/recordings/:recId
//
// Frigate doesn't expose a per-segment delete API via REST (deletion
// is handled by retention policy). We return 405 to signal the
// front-end that this operation is not supported.
func (h *CameraHandler) DeleteRecording(c *gin.Context) {
	utils.Fail(c, http.StatusMethodNotAllowed, "Frigate manages recording deletion via retention policy; per-segment delete is not supported")
}

// ListAlerts — GET /api/v1/cameras/alerts
//
// Returns recent Frigate detection events (newest first). Each event
// includes the Frigate camera slug, detected label, confidence score,
// timestamp, zone information, and a base64 thumbnail (small JPEG)
// for instant preview in the dashboard. The slug is mapped to the
// home-api camera ID so the frontend can cross-reference.
//
// Query params:
//
//	limit — max results (default 20, max 100)
//	camera_id — filter by home-api camera ID
//	camera — filter by Frigate camera slug
//	label — filter by detection label (person, car, etc.)
//	before — unix timestamp upper bound
//	after — unix timestamp lower bound
func (h *CameraHandler) ListAlerts(c *gin.Context) {
	uid, isAdmin, ok := h.callerIsAdmin(c)
	if !ok {
		utils.Fail(c, http.StatusUnauthorized, "unauthenticated")
		return
	}

	limit := 20
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 {
		limit = v
		if limit > 100 {
			limit = 100
		}
	}

	filter := camera.EventFilter{
		Limit:             limit,
		IncludeThumbnails: true,
		Labels:            strings.TrimSpace(c.Query("label")),
	}

	if beforeStr := c.Query("before"); beforeStr != "" {
		if b, err := strconv.ParseInt(beforeStr, 10, 64); err == nil {
			filter.Before = b
		}
	}
	if afterStr := c.Query("after"); afterStr != "" {
		if a, err := strconv.ParseInt(afterStr, 10, 64); err == nil {
			filter.After = a
		}
	}

	var allowedSlugs map[string]bool
	if !isAdmin {
		allowedSlugs = make(map[string]bool)
		for _, cam := range h.Reg.List() {
			if h.Reg.CanRead(&cam, uid, isAdmin) {
				if s, found := h.Reg.LookupFrigateSlugByCameraID(cam.ID); found {
					allowedSlugs[s] = true
				}
			}
		}
		if len(allowedSlugs) == 0 {
			utils.Success(c, gin.H{"alerts": []any{}, "total": 0})
			return
		}
	}

	if camIDStr := c.Query("camera_id"); camIDStr != "" {
		if cid, err := strconv.ParseUint(camIDStr, 10, 64); err == nil && cid > 0 {
			cam, err := h.Reg.Get(uint(cid))
			if err != nil {
				utils.Fail(c, http.StatusNotFound, "camera not found")
				return
			}
			if !isAdmin && !h.Reg.CanRead(cam, uid, isAdmin) {
				utils.Fail(c, http.StatusForbidden, "not your camera")
				return
			}
			if slug, ok := h.Reg.LookupFrigateSlugByCameraID(uint(cid)); ok {
				filter.Cameras = slug
			}
		}
	} else if slug := strings.TrimSpace(c.Query("camera")); slug != "" {
		camID, found := h.Reg.LookupByFrigateSlug(slug)
		if !found {
			utils.Fail(c, http.StatusNotFound, "camera not found")
			return
		}
		cam, err := h.Reg.Get(camID)
		if err != nil || (!isAdmin && !h.Reg.CanRead(cam, uid, isAdmin)) {
			utils.Fail(c, http.StatusForbidden, "not your camera")
			return
		}
		filter.Cameras = slug
	} else if !isAdmin {
		var slugs []string
		for s := range allowedSlugs {
			slugs = append(slugs, s)
		}
		filter.Cameras = strings.Join(slugs, ",")
	}

	// Include thumbnails so the dashboard can render preview images
	// without a second round-trip per event.
	if h.Reg == nil || h.Reg.Frigate == nil {
		utils.Success(c, gin.H{"alerts": []any{}, "total": 0})
		return
	}
	events, err := h.Reg.Frigate.ListEventsFiltered(c.Request.Context(), filter)
	if err != nil {
		log.Printf("[handler] frigate events unavailable: %v", err)
		utils.Success(c, gin.H{"alerts": []any{}, "total": 0})
		return
	}

	// Map Frigate camera slugs to home-api camera IDs.
	type alertEntry struct {
		ID          string   `json:"id"`
		CameraSlug  string   `json:"camera_slug"`
		CameraID    uint     `json:"camera_id,omitempty"`
		CameraName  string   `json:"camera_name,omitempty"`
		Label       string   `json:"label"`
		Confidence  float64  `json:"confidence"`
		StartTime   float64  `json:"start_time"`
		EndTime     float64  `json:"end_time"`
		Zones       []string `json:"zones,omitempty"`
		HasClip     bool     `json:"has_clip"`
		HasSnapshot bool     `json:"has_snapshot"`
		Thumbnail   string   `json:"thumbnail,omitempty"`
	}

	alerts := make([]alertEntry, 0, len(events))
	for _, ev := range events {
		if !isAdmin && !allowedSlugs[ev.Camera] {
			continue
		}
		entry := alertEntry{
			ID:          ev.ID,
			CameraSlug:  ev.Camera,
			Label:       ev.Label,
			Confidence:  ev.EffectiveTopScore(),
			StartTime:   ev.StartTime,
			EndTime:     ev.EndTime,
			Zones:       ev.Zones,
			HasClip:     ev.HasClip,
			HasSnapshot: ev.HasSnapshot,
			Thumbnail:   ev.Thumbnail,
		}
		// Resolve slug to camera ID.
		if camID, ok := h.Reg.LookupByFrigateSlug(ev.Camera); ok {
			entry.CameraID = camID
			if cam, err := h.Reg.Get(camID); err == nil {
				entry.CameraName = cam.StreamName
			}
		}
		alerts = append(alerts, entry)
	}

	utils.Success(c, gin.H{"alerts": alerts, "total": len(alerts)})
}

// MotionRanges — GET /api/v1/cameras/:id/motion-ranges?after=UNIX&before=UNIX
//
// Returns time ranges (unix seconds) where Frigate recorded motion
// activity for the given camera within [after, before). Used by the
// Android app's day-playback SeekBar to paint red marks at positions
// where motion happened.
//
// v1.6.0: replaces the previous approach of using /api/events
// (alerts) for the overlay. Alerts only fire when Frigate's AI
// detector finds a tracked object (person/car), which leaves the
// overlay empty when the camera catches motion that doesn't meet
// the AI threshold. Motion data comes from Frigate's per-segment
// `motion` field (pixel-diff pre-filter) and is more reliable for
// the user's "show me when something happened" mental model.
//
// Query params:
//
//	after  — unix seconds, range start (required)
//	before — unix seconds, range end (required)
//
// Response:
//
//	{
//	  "ranges": [[startUnix, endUnix], ...],
//	  "total": N
//	}
func (h *CameraHandler) MotionRanges(c *gin.Context) {
	cam, ok := h.requireCanRead(c)
	if !ok {
		return
	}
	after, err := strconv.ParseInt(c.Query("after"), 10, 64)
	if err != nil || after <= 0 {
		utils.Fail(c, http.StatusBadRequest, "missing or invalid 'after' param (unix seconds)")
		return
	}
	before, err := strconv.ParseInt(c.Query("before"), 10, 64)
	if err != nil || before <= 0 {
		utils.Fail(c, http.StatusBadRequest, "missing or invalid 'before' param (unix seconds)")
		return
	}
	if before <= after {
		utils.Fail(c, http.StatusBadRequest, "'before' must be greater than 'after'")
		return
	}

	if h.Reg.Frigate == nil {
		utils.Success(c, gin.H{"ranges": []any{}, "total": 0})
		return
	}

	slug := h.Reg.FrigateSlug(cam)
	ranges, err := h.Reg.Frigate.ListMotionRanges(c.Request.Context(), slug, after, before)
	if err != nil {
		log.Printf("[handler] failed to fetch motion ranges: %v", err)
		utils.Fail(c, http.StatusBadGateway, "failed to fetch motion ranges")
		return
	}
	if ranges == nil {
		// v1.6.3: ranges is now []MotionRange, not [][2]int64.
		// Keep the empty-array serialization so clients don't have
		// to handle null separately.
		ranges = []camera.MotionRange{}
	}
	utils.Success(c, gin.H{"ranges": ranges, "total": len(ranges)})
}

// canReadEvent verifies that the caller has permission to view the Frigate
// detection event identified by eventID. Admins are permitted unconditionally;
// non-admins are checked against the camera owning the event.
func (h *CameraHandler) canReadEvent(c *gin.Context, eventID string) bool {
	uid, isAdmin, ok := h.callerIsAdmin(c)
	if !ok {
		utils.Fail(c, http.StatusUnauthorized, "unauthenticated")
		return false
	}
	if isAdmin {
		return true
	}
	if h.Reg == nil || h.Reg.Frigate == nil {
		utils.Fail(c, http.StatusBadGateway, "frigate not available")
		return false
	}
	ev, err := h.Reg.Frigate.GetEvent(c.Request.Context(), eventID)
	if err != nil {
		log.Printf("[handler] failed to get event for auth check: %v", err)
		utils.Fail(c, http.StatusNotFound, "event not found")
		return false
	}
	camID, found := h.Reg.LookupByFrigateSlug(ev.Camera)
	if !found {
		utils.Fail(c, http.StatusForbidden, "camera access forbidden")
		return false
	}
	cam, err := h.Reg.Get(camID)
	if err != nil || !h.Reg.CanRead(cam, uid, isAdmin) {
		utils.Fail(c, http.StatusForbidden, "not your camera")
		return false
	}
	return true
}

// AlertSnapshot — GET /api/v1/cameras/alerts/:id/snapshot
//
// Proxies the full-resolution snapshot JPEG for a Frigate detection
// event. Frigate serves these at /api/events/<id>/snapshot.jpg but
// requires nginx auth; home-api proxies the request so the dashboard
// can load snapshots via an <img> tag without exposing Frigate
// credentials or CORS issues.
//
// The response is cached for 1 hour (snapshots are immutable — once
// an event ends its snapshot never changes).
func (h *CameraHandler) AlertSnapshot(c *gin.Context) {
	eventID := c.Param("id")
	if eventID == "" {
		utils.Fail(c, http.StatusBadRequest, "missing event id")
		return
	}
	if !h.canReadEvent(c, eventID) {
		return
	}

	quality, _ := strconv.Atoi(c.DefaultQuery("quality", "100"))
	if quality <= 0 || quality > 100 {
		quality = 100
	}

	body, contentType, err := h.Reg.Frigate.EventSnapshotWithQuality(c.Request.Context(), eventID, quality)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			utils.Fail(c, http.StatusNotFound, "snapshot not found")
			return
		}
		log.Printf("[handler] failed to fetch snapshot: %v", err)
		utils.Fail(c, http.StatusBadGateway, "failed to fetch snapshot")
		return
	}
	defer body.Close()

	// Snapshots are immutable — cache aggressively.
	c.Header("Cache-Control", "public, max-age=3600")
	if contentType == "" {
		contentType = "image/jpeg"
	}
	c.DataFromReader(http.StatusOK, -1, contentType, body, nil)
}

// AlertThumbnail — GET /api/v1/cameras/alerts/:id/thumbnail
//
// Proxies the small JPEG thumbnail for a Frigate detection event.
// Frigate 0.17 no longer inlines base64 thumbnails in /api/events
// (the `thumbnail` field is null), so the dashboard fetches each
// thumbnail via this endpoint. Thumbnails are ~6KB JPEGs suitable
// for list previews; the full snapshot is served via AlertSnapshot.
//
// The response is cached for 1 hour (thumbnails are immutable).
func (h *CameraHandler) AlertThumbnail(c *gin.Context) {
	eventID := c.Param("id")
	if eventID == "" {
		utils.Fail(c, http.StatusBadRequest, "missing event id")
		return
	}
	if !h.canReadEvent(c, eventID) {
		return
	}

	body, contentType, err := h.Reg.Frigate.EventThumbnail(c.Request.Context(), eventID)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			utils.Fail(c, http.StatusNotFound, "thumbnail not found")
			return
		}
		log.Printf("[handler] failed to fetch thumbnail: %v", err)
		utils.Fail(c, http.StatusBadGateway, "failed to fetch thumbnail")
		return
	}
	defer body.Close()

	c.Header("Cache-Control", "public, max-age=3600")
	if contentType == "" {
		contentType = "image/jpeg"
	}
	c.DataFromReader(http.StatusOK, -1, contentType, body, nil)
}

// Frame — GET /api/v1/cameras/:id/frame
//
// Proxies a single JPEG frame from the go2rtc stream. Used by the
// dashboard's camera card to show a static preview before the
// operator clicks Play — avoids spinning up a WebRTC/HLS
// connection for every camera on the page. The response is a
// fresh keyframe from the live RTSP source, so it always
// reflects the camera's current view.
//
// Query parameters:
//
//	quality (1-100, default 30)  — JPEG encoder quality forwarded to go2rtc
//	width   (80-1920, default 640) — optional resize width forwarded to go2rtc
//
// Out-of-range or unparseable values silently fall back to the
// defaults so a malformed client URL never 502s the dashboard.
//
// A 10-second in-memory cache (sync.Map, keyed by stream name)
// absorbs burst traffic — e.g. the dashboard opening multiple
// camera cards at once, or the operator's browser firing
// prefetch requests. The cache is intentionally short so the
// preview still tracks live motion. The X-Frame-Cache response
// header exposes HIT/MISS for debugging.
func (h *CameraHandler) Frame(c *gin.Context) {
	cam, ok := h.requireCanRead(c)
	if !ok {
		return
	}
	if cam.StreamName == "" {
		utils.Fail(c, http.StatusNotFound, "camera stream not configured")
		return
	}

	// Parse quality (1-100, default 30). Fall back to the default
	// on any parse or range error so a bad client value can never
	// 502 the upstream.
	quality, err := strconv.Atoi(c.DefaultQuery("quality", "30"))
	if err != nil || quality < 1 || quality > 100 {
		quality = 30
	}
	// Parse width (80-1920, default 640). Same fallback policy.
	width, err := strconv.Atoi(c.DefaultQuery("width", "640"))
	if err != nil || width < 80 || width > 1920 {
		width = 640
	}

	// Frames are fresh snapshots — discourage browser/proxy
	// caching so the operator always sees the latest view on
	// reload. The 10s server-side cache below is the only caching
	// layer applied.
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate")

	// Cache lookup. A hit within TTL is returned verbatim
	// without contacting go2rtc, which collapses cold-stream cost
	// during burst traffic. Multi-camera split-screen passes nocache=1
	// to fetch fresh live frames.
	noCache := c.Query("nocache") == "1" || c.Query("live") == "1"
	if !noCache {
		if v, ok := h.frameCache.Load(cam.StreamName); ok {
			if entry, ok := v.(*frameCacheEntry); ok && time.Since(entry.ts) < 2*time.Second {
				contentType := entry.contentType
				if contentType == "" {
					contentType = "image/jpeg"
				}
				c.Header("X-Frame-Cache", "HIT")
				c.Data(http.StatusOK, contentType, entry.data)
				return
			}
		}
	}

	var body io.ReadCloser
	var contentType string
	var fetchErr error

	// Fast path: Frigate maintains continuously decoded frames in memory (~20-50ms response)
	if h.Reg != nil && h.Reg.Frigate != nil {
		slug := h.Reg.FrigateSlugUnique(cam)
		if slug != "" {
			body, contentType, fetchErr = h.Reg.Frigate.LatestFrame(c.Request.Context(), slug)
			if fetchErr != nil {
				body = nil
			}
		}
	}

	// Fallback path: go2rtc frame capture if Frigate is not configured or failed
	if body == nil {
		body, contentType, fetchErr = h.Reg.Go2.Frame(c.Request.Context(), cam.StreamName, quality, width)
		if fetchErr != nil {
			// v1.7.3: retry once after 3s to cover ffmpeg cold start / restart
			log.Printf("frame: first attempt failed for %s, retrying in 3s: %v", cam.StreamName, fetchErr)
			select {
			case <-time.After(3 * time.Second):
			case <-c.Request.Context().Done():
				log.Printf("[handler] failed to fetch frame: %v", fetchErr)
				utils.Fail(c, http.StatusBadGateway, "failed to fetch frame")
				return
			}
			body, contentType, fetchErr = h.Reg.Go2.Frame(c.Request.Context(), cam.StreamName, quality, width)
			if fetchErr != nil {
				log.Printf("[handler] failed to fetch frame: %v", fetchErr)
				utils.Fail(c, http.StatusBadGateway, "failed to fetch frame")
				return
			}
			log.Printf("frame: retry succeeded for %s", cam.StreamName)
		}
	}
	// 10MB cap: a go2rtc frame is a single JPEG/MJPEG snapshot,
	// typically well under 1MB. Without a bound, a malicious or
	// buggy upstream streaming an unbounded body could exhaust API
	// memory (OOM). LimitReader prevents that while staying far
	// above any legitimate frame size.
	data, err := io.ReadAll(io.LimitReader(body, 10*1024*1024))
	body.Close()
	if err != nil {
		log.Printf("[handler] failed to read frame: %v", err)
		utils.Fail(c, http.StatusBadGateway, "failed to read frame")
		return
	}

	h.frameCache.Store(cam.StreamName, &frameCacheEntry{
		data:        data,
		contentType: contentType,
		ts:          time.Now(),
	})

	if contentType == "" {
		contentType = "image/jpeg"
	}
	c.Header("X-Frame-Cache", "MISS")
	c.Data(http.StatusOK, contentType, data)
}

// StreamMP4 — GET /api/v1/cameras/:id/stream.mp4
//
// Proxies a streaming fragmented-MP4 feed from go2rtc to the
// Android client. ExoPlayer's ProgressiveMediaSource consumes this
// directly (no HLS playlist round-trips), reducing first-frame
// latency from 5-10s on a cold stream to ~1-2s.
//
// The response is an infinite, length-delimited fMP4 stream — we
// pass the body through verbatim and let the client disconnect
// when playback stops. We deliberately do NOT set Content-Length
// (unknown) or Cache-Control (live stream) headers.
func (h *CameraHandler) StreamMP4(c *gin.Context) {
	cam, ok := h.requireCanRead(c)
	if !ok {
		return
	}
	if cam.StreamName == "" {
		utils.Fail(c, http.StatusNotFound, "camera stream not configured")
		return
	}

	targetStream := cam.StreamName
	if c.Query("quality") == "1080p" {
		targetStream = targetStream + "_1080p"
	}

	// Use the request's context so the upstream connection is
	// cancelled when the client disconnects (ExoPlayer stops, the
	// user navigates away, etc.) — without this go2rtc would keep
	// the RTSP source connection alive indefinitely.
	body, contentType, err := h.Reg.Go2.StreamMP4(c.Request.Context(), targetStream)
	if err != nil {
		log.Printf("[handler] failed to open stream for %s: %v", targetStream, err)
		utils.Fail(c, http.StatusBadGateway, "failed to open stream")
		return
	}
	defer body.Close()

	// Disable buffering — ExoPlayer needs bytes as they arrive.
	c.Writer.Flush()

	if contentType == "" {
		contentType = "video/mp4"
	}
	// -1 tells gin DataFromReader to chunked transfer (no
	// Content-Length header). This is what we want for a stream
	// of unknown length.
	c.DataFromReader(http.StatusOK, -1, contentType, body, nil)
}

// PlayRecording — GET /api/v1/cameras/:id/recordings/:recId/file
//
// Serves a 60-second recording clip. The :recId is the minute-start
// timestamp (from ListRecordings). Frigate stores recordings as 10s
// MP4 files on disk; this handler finds all 10s segments within the
// requested minute and concatenates them into a single 60s MP4 using
// ffmpeg stream copy (no re-encoding — sub-second for ~24MB).
//
// Segment file names are NOT aligned to 10-second boundaries — Frigate
// starts the first segment whenever the recording pipeline spins up,
// so a minute might contain 13.08.mp4, 13.18.mp4, 13.28.mp4, etc. We
// therefore list the on-disk directory and filter by the MM prefix
// rather than constructing paths from minuteStart+offset.
//
// If only one segment exists in the minute (e.g., camera was briefly
// offline), it is served directly without ffmpeg. http.ServeFile
// provides Content-Type (video/mp4), Content-Length, Range support
// (for <video> seeking), and ETag/Last-Modified automatically.
func (h *CameraHandler) PlayRecording(c *gin.Context) {
	// v1.8.26: this route's ffmpeg transcode can take longer than the
	// server's 15s WriteTimeout, so extend the write deadline for THIS
	// request only (NewResponseController unwraps gin's ResponseWriter
	// via its Unwrap() method). All other routes keep the 15s cap.
	if rc := http.NewResponseController(c.Writer); rc != nil {
		if err := rc.SetWriteDeadline(time.Now().Add(120 * time.Second)); err != nil {
			log.Printf("[handler] PlayRecording: set write deadline: %v", err)
		}
	}

	cam, ok := h.requireCanRead(c)
	if !ok {
		return
	}
	minuteStart, err := strconv.ParseInt(c.Param("recId"), 10, 64)
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid recId (expected unix timestamp)")
		return
	}

	// List all 10s segment files that fall within this minute.
	// Frigate's segment SS values are not aligned to 00/10/20/30/40/50,
	// so we list the directory and match by MM prefix instead of
	// constructing paths from minuteStart + offset.
	paths, err := h.Reg.RecordingSegmentsForMinute(cam, minuteStart)
	if err != nil || len(paths) == 0 {
		// Fallback to Quark Cloud Archive via Alist WebDAV
		paths, err = h.Reg.RecordingSegmentsFromCloud(c.Request.Context(), cam, minuteStart)
	}
	if err != nil || len(paths) == 0 {
		log.Printf("[handler] no recording segments found for minute %d: %v", minuteStart, err)
		utils.Fail(c, http.StatusNotFound, "no recording segments found in this minute (neither local nor cloud)")
		return
	}

	// v1.8.25: transcode to H.264 regardless of segment count. Frigate
	// records the camera's native stream (HEVC/H.265 on Hikvision) with
	// -c:v copy, and a raw stream-copy serve does NOT decode in Chrome
	// (no HEVC support in the <video> element). Transcoding here is what
	// makes web monitoring playback work on every browser. The single-
	// segment fast path is removed because it too served raw HEVC.
	//
	// v1.8.25: result is cached on disk so re-plays of the same minute
	// are instant (see transcodeRecording).
	h.transcodeRecording(c, cam, minuteStart, paths)
}

// PlayRecordingStream — GET /api/v1/cameras/:id/recordings/:recId/stream
//
// fMP4 streaming variant of PlayRecording for the web MediaSource player.
// Unlike /file (which encodes the whole 60s to a faststart MP4, copies it
// to the cache volume, THEN sends it — first frame waits for the full
// transcode, typically ~5s cold on a HEVC source), this endpoint streams
// fragmented MP4 segments to the client as the encoder emits them, so the
// browser's first frame arrives in ~1-2s.
//
// Cache: reuse .stream-cache/<camID>/<minuteStart>.fmp4 when present
// (serve directly, Range works). On a miss we transcode on the fly and
// TEE stdout to both the response and a temp file; for a closed minute we
// promote the temp to the cache so the next replay is instant. The init
// segment (ftyp+moov) is written at the very start (empty_moov), which is
// exactly what the MSE client needs to set up its SourceBuffer.
func (h *CameraHandler) PlayRecordingStream(c *gin.Context) {
	if rc := http.NewResponseController(c.Writer); rc != nil {
		if err := rc.SetWriteDeadline(time.Now().Add(120 * time.Second)); err != nil {
			log.Printf("[handler] PlayRecordingStream: set write deadline: %v", err)
		}
	}
	cam, ok := h.requireCanRead(c)
	if !ok {
		return
	}
	minuteStart, err := strconv.ParseInt(c.Param("recId"), 10, 64)
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid recId (expected unix timestamp)")
		return
	}
	paths, err := h.Reg.RecordingSegmentsForMinute(cam, minuteStart)
	if err != nil || len(paths) == 0 {
		// Fallback to Quark Cloud Archive via Alist WebDAV
		paths, err = h.Reg.RecordingSegmentsFromCloud(c.Request.Context(), cam, minuteStart)
	}
	if err != nil || len(paths) == 0 {
		utils.Fail(c, http.StatusNotFound, "no recording segments found in this minute (neither local nor cloud)")
		return
	}
	quality := strings.ToLower(strings.TrimSpace(c.Query("quality")))
	if quality != "1080p" {
		quality = "720p"
	}
	h.streamRecording(c, cam, minuteStart, paths, quality)
}

func (h *CameraHandler) streamRecording(c *gin.Context, cam *model.Camera, minuteStart int64, paths []string, quality string) {
	cacheDir := fmt.Sprintf("/data/recordings/.stream-cache/%d", cam.ID)
	cacheSuffix := ""
	if quality == "1080p" {
		cacheSuffix = "_1080p"
	}
	cacheFile := filepath.Join(cacheDir, fmt.Sprintf("%d%s.fmp4", minuteStart, cacheSuffix))
	// Cache hit: serve the fragmented MP4 directly (http.ServeFile gives
	// Content-Length + Range, which the MSE client tolerates fine).
	if fi, err := os.Stat(cacheFile); err == nil && fi.Size() > 0 {
		c.Header("Cache-Control", "no-store")
		http.ServeFile(c.Writer, c.Request, cacheFile)
		return
	}

	// Build the ffmpeg concat list for the 60s minute.
	tmpDir, err := os.MkdirTemp("", "stream_")
	if err != nil {
		log.Printf("[handler] create temp dir: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "create temp dir")
		return
	}
	defer os.RemoveAll(tmpDir)
	var listBuilder strings.Builder
	for _, p := range paths {
		escaped := strings.ReplaceAll(p, "'", "'\\''")
		listBuilder.WriteString(fmt.Sprintf("file '%s'\n", escaped))
	}
	listPath := filepath.Join(tmpDir, "list.txt")
	if err := os.WriteFile(listPath, []byte(listBuilder.String()), 0o644); err != nil {
		utils.Fail(c, http.StatusInternalServerError, "write concat list")
		return
	}

	// One transcode slot (see transcodeSem notes in transcodeRecording).
	h.transcodeSem <- struct{}{}
	releaseSem := func() {
		select {
		case <-h.transcodeSem:
		default:
		}
	}
	defer releaseSem()

	cmd := buildFMP4Cmd(listPath, vaapiAvailable(), quality)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, "pipe ffmpeg")
		return
	}
	cmd.Stderr = nil // discard progress; errors surface via exit status

	// Tee encoder output to the temp file for cache promotion.
	tmpOut := filepath.Join(tmpDir, fmt.Sprintf("%d.fmp4", minuteStart))
	outF, err := os.Create(tmpOut)
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, "create output")
		return
	}
	defer outF.Close()

	if err := cmd.Start(); err != nil {
		log.Printf("[handler] start ffmpeg stream: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "start ffmpeg")
		return
	}

	// Commit the response with no Content-Length → chunked transfer.
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "video/mp4")
	c.Status(http.StatusOK)

	flusher, _ := c.Writer.(http.Flusher)
	ctxDone := c.Request.Context().Done()
	buf := make([]byte, 32*1024)
	writeErr := error(nil)
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			if _, werr := c.Writer.Write(buf[:n]); werr != nil {
				writeErr = werr
				break
			}
			if _, werr := outF.Write(buf[:n]); werr != nil {
				writeErr = werr
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			writeErr = rerr
			break
		}
		if ctxDone != nil {
			select {
			case <-ctxDone:
				writeErr = fmt.Errorf("client disconnected")
			default:
			}
			if writeErr != nil {
				break
			}
		}
	}

	// If the client left (or the write failed), stop the encoder so we
	// don't hold the transcode slot for a partial 60s we'll never send.
	if writeErr != nil {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		log.Printf("[handler] streamRecording aborted: %v", writeErr)
		return
	}
	if err := cmd.Wait(); err != nil {
		log.Printf("[handler] streamRecording ffmpeg: %v", err)
		return
	}

	// Cache only closed minutes (see transcodeRecording). tmpOut is
	// complete here (client consumed the whole stream), so promotion is
	// safe and makes the next replay instant.
	if time.Now().Unix()-minuteStart > 90 {
		if err := os.MkdirAll(cacheDir, 0o755); err == nil {
			if cerr := copyToCache(tmpOut, cacheFile); cerr != nil {
				log.Printf("[handler] streamRecording cache promote failed: %v", cerr)
			}
		}
	}
}

// buildFMP4Cmd returns the ffmpeg command that concatenates the segments
// in listPath and emits a fragmented MP4 (H.264/AAC) to stdout, so the
// handler can stream segments as they're encoded. empty_moov puts the
// init segment (ftyp+moov) at the very front; frag_keyframe emits one
// fragment per keyframe so the client's MediaSource gets contiguous
// playable chunks immediately. hw uses the VAAPI hardware pipeline.
func buildFMP4Cmd(listPath string, hw bool, quality string) *exec.Cmd {
	w, h := "1280", "720"
	if quality == "1080p" {
		w, h = "1920", "1080"
	}
	if hw {
		vf := fmt.Sprintf("scale_vaapi=w=min(%s\\,iw):h=min(%s\\,ih):force_original_aspect_ratio=decrease", w, h)
		return exec.Command("ffmpeg", "-y",
			"-vaapi_device", "/dev/dri/renderD128",
			"-hwaccel", "vaapi",
			"-hwaccel_output_format", "vaapi",
			"-f", "concat", "-safe", "0",
			"-i", listPath,
			"-vf", vf,
			"-c:v", "h264_vaapi", "-qp", "24",
			"-c:a", "aac", "-b:a", "96k",
			"-f", "mp4",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"pipe:1")
	}
	vf := fmt.Sprintf("scale=min(%s\\,iw):min(%s\\,ih):force_original_aspect_ratio=decrease", w, h)
	return exec.Command("ffmpeg", "-y",
		"-f", "concat", "-safe", "0",
		"-i", listPath,
		"-vf", vf,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
		"-c:a", "aac", "-b:a", "96k",
		"-f", "mp4",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"pipe:1")
}

// transcodeRecording concatenates the given recording segments into a
// single H.264/AAC MP4 and serves it, transcoding on the fly so ANY
// browser/player can decode it.
//
// Why transcode: Frigate's record input is the camera's plain RTSP
// (`-c:v copy`, see deploy/frigate/config.yml), so on Hikvision the
// stored segments are HEVC/H.265 + PCMA audio. Chromium lacks HEVC
// decoding in <video>, Firefox only with proprietary plugins — a raw
// stream-copy serve fails to play (black screen / onError). We force
// libx264 (High profile) + AAC. libx264 is built into Alpine's ffmpeg
// package, so no extra image dependency.
//
// v1.8.25 disk cache: software transcoding a 60s segment on the NAS
// (Celeron J4125, no VAAPI in the API container) takes ~40s of CPU.
// Without a cache every request — including the operator re-viewing
// the same minute — pays that full cost before the first frame loads,
// which makes monitoring playback feel broken. Past-minute segments
// are immutable (Frigate only appends to the running minute), so we
// transcode once and reuse the result for all later requests.
//
// v1.8.26: the API container now passes /dev/dri/renderD128 through
// (compose.yaml devices + group_add render) and installs
// intel-media-driver/libva (Dockerfile), so ffmpeg uses h264_vaapi —
// a 60s clip drops from ~40s of CPU to ~3s on the iGPU. If the
// hardware path fails for any reason, buildTranscodeCmd falls back to
// the software libx264 pipeline so playback never breaks.
//
// Cache location: /data/recordings/.transcode-cache/<camID>/<minuteStart>.mp4
// /data/recordings is a writable bind mount (see compose.yaml). Keying by
// minuteStart bounds the cache to one small MP4 per viewed camera-minute
// and makes invalidation trivial (delete the file to re-transcode).
//
// -fflags +genpts -avoid_negative_ts make_zero regenerate PTS/DTS so
// the concatenated 10s segments have continuous timestamps (no glitch
// at each boundary). -movflags faststart puts moov at the front for
// instant playback.
func (h *CameraHandler) transcodeRecording(c *gin.Context, cam *model.Camera, minuteStart int64, paths []string) {
	cacheDir := fmt.Sprintf("/data/recordings/.transcode-cache/%d", cam.ID)
	codec := strings.ToLower(strings.TrimSpace(c.Query("codec")))
	isCopyMode := codec == "copy"
	audioOn := camera.CameraHasAudio(cam)
	cacheFileName := fmt.Sprintf("%d.mp4", minuteStart)
	if isCopyMode {
		cacheFileName = fmt.Sprintf("%d_copy.mp4", minuteStart)
	}
	if !audioOn {
		if isCopyMode {
			cacheFileName = fmt.Sprintf("%d_copy_noaudio.mp4", minuteStart)
		} else {
			cacheFileName = fmt.Sprintf("%d_noaudio.mp4", minuteStart)
		}
	}
	cacheFile := filepath.Join(cacheDir, cacheFileName)

	// Serve from cache if present and non-empty. A past minute's segments
	// never change, so a cached transcode is correct indefinitely.
	serve := func(path string) {
		c.Header("Cache-Control", "no-store")
		if c.Query("download") == "1" {
			c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"camera_%d_%d.mp4\"", cam.ID, minuteStart))
		}
		http.ServeFile(c.Writer, c.Request, path)
	}
	if fi, err := os.Stat(cacheFile); err == nil && fi.Size() > 0 {
		serve(cacheFile)
		return
	}

	tmpDir, err := os.MkdirTemp("", "rec_")
	if err != nil {
		log.Printf("[handler] create temp dir: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "create temp dir")
		return
	}
	defer os.RemoveAll(tmpDir)

	// Build ffmpeg concat list: file 'path1'\nfile 'path2'\n...
	var listBuilder strings.Builder
	for _, p := range paths {
		// Escape single quotes for ffmpeg's concat demuxer.
		escaped := strings.ReplaceAll(p, "'", "'\\''")
		listBuilder.WriteString(fmt.Sprintf("file '%s'\n", escaped))
	}
	listPath := filepath.Join(tmpDir, "list.txt")
	if err := os.WriteFile(listPath, []byte(listBuilder.String()), 0o644); err != nil {
		log.Printf("[handler] write concat list: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "write concat list")
		return
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		log.Printf("[handler] mkdir cache dir: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "create cache dir")
		return
	}

	tmpOut := filepath.Join(tmpDir, cacheFileName)

	h.transcodeSem <- struct{}{}
	releaseSem := func() {
		select {
		case <-h.transcodeSem:
		default:
		}
	}
	defer releaseSem()

	var cmd *exec.Cmd
	if isCopyMode {
		cmd = buildCopyConcatCmd(listPath, tmpOut, audioOn)
	} else {
		cmd = buildTranscodeCmd(listPath, tmpOut, vaapiAvailable(), audioOn)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		log.Printf("PlayRecording: ffmpeg transcode failed: %v: %s", err, string(output))
		if isCopyMode {
			// Fallback to full transcode if stream-copy concat failed
			log.Printf("PlayRecording: copy mode concat failed, falling back to VAAPI transcode")
			cmd = buildTranscodeCmd(listPath, tmpOut, vaapiAvailable(), audioOn)
			if output, err := cmd.CombinedOutput(); err != nil {
				log.Printf("PlayRecording: fallback transcode failed: %v: %s", err, string(output))
				utils.Fail(c, http.StatusInternalServerError, "ffmpeg transcode failed")
				return
			}
		} else if vaapiAvailable() {
			log.Printf("PlayRecording: VAAPI failed, retrying with software libx264")
			cmd = buildTranscodeCmd(listPath, tmpOut, false, audioOn)
			if output, err := cmd.CombinedOutput(); err != nil {
				log.Printf("PlayRecording: ffmpeg software transcode failed: %v: %s", err, string(output))
				utils.Fail(c, http.StatusInternalServerError, "ffmpeg transcode failed")
				return
			}
		} else {
			utils.Fail(c, http.StatusInternalServerError, "ffmpeg transcode failed")
			return
		}
	}

	// Promote to the cache only for "closed" minutes. The minute containing
	// minuteStart is still being written while it is the current minute
	// (Frigate appends segments until the minute rolls over); caching a
	// partial clip would serve a truncated/replayed-tail video forever.
	// Past minutes are immutable, so promote the finished transcode
	// into the cache atomically (copyToCache writes a temp sibling on
	// the cache volume then renames) — a reader never sees a half-
	// written file.
	finalPath := tmpOut
	if time.Now().Unix()-minuteStart > 90 {
		if err := copyToCache(tmpOut, cacheFile); err == nil {
			finalPath = cacheFile
		} else {
			log.Printf("PlayRecording: cache promote failed (serving temp): %v", err)
		}
	}

	// Release the transcode slot before streaming the body so a queued
	// request for the next minute can start transcoding while this one
	// is being served/downloaded.
	releaseSem()

	serve(finalPath)
}

// copyToCache copies a finished transcode into the cache volume
// atomically: write to a temp sibling on the same filesystem, then
// rename (a same-fs rename is atomic, so readers never see a half-
// written file). We copy rather than rename the transcode result
// because the temp now lives on the container's overlay FS (/tmp)
// while the cache volume is a btrfs bind mount — a cross-filesystem
// rename would fail with EXDEV. Copying a fully-written local file is
// cheap and safe.
func copyToCache(src, dst string) error {
	stage, err := os.CreateTemp(filepath.Dir(dst), ".stage-*")
	if err != nil {
		return err
	}
	sname := stage.Name()
	defer os.Remove(sname) // no-op after a successful rename
	in, err := os.Open(src)
	if err != nil {
		stage.Close()
		return err
	}
	if _, err := io.Copy(stage, in); err != nil {
		in.Close()
		stage.Close()
		return err
	}
	if err := in.Close(); err != nil {
		stage.Close()
		return err
	}
	if err := stage.Close(); err != nil {
		return err
	}
	return os.Rename(sname, dst)
}

// vaapiAvailable reports whether the Intel iGPU render node is
// reachable from this container. When true, PlayRecording uses
// h264_vaapi (hardware) instead of software libx264. The device is
// passed in via compose.yaml (devices + group_add render); the check
// is defensive so a missing device silently falls back to software.
func vaapiAvailable() bool {
	fi, err := os.Stat("/dev/dri/renderD128")
	return err == nil && fi.Mode()&os.ModeDevice != 0
}

// buildCopyConcatCmd concatenates segments without re-encoding (-c copy).
// Extremely fast (~0.15s) and preserves native resolution/HEVC bitrate with 0 CPU load.
func buildCopyConcatCmd(listPath, outPath string, audioOn bool) *exec.Cmd {
	args := []string{"-y", "-f", "concat", "-safe", "0", "-i", listPath}
	if audioOn {
		args = append(args, "-c", "copy")
	} else {
		args = append(args, "-c:v", "copy", "-an")
	}
	args = append(args, "-movflags", "faststart", outPath)
	return exec.Command("ffmpeg", args...)
}

// buildTranscodeCmd returns the ffmpeg command that concatenates the
// segments listed in listPath into outPath as H.264/AAC MP4. When hw
// is true it uses the VAAPI hardware pipeline (h264_vaapi); otherwise
// the software libx264 pipeline. Both produce browser-compatible
// output; hardware is ~10x faster on the J4125 iGPU.
func buildTranscodeCmd(listPath, outPath string, hw bool, audioOn bool) *exec.Cmd {
	audioArgs := []string{"-c:a", "aac", "-b:a", "96k"}
	if !audioOn {
		audioArgs = []string{"-an"}
	}
	// v1.8.46: cap the encode at 1280x720 (downscale only, aspect kept).
	// Recordings are monitored in a small player where 1440p is wasted;
	// halving pixels cuts both decode and encode work, trimming a tmp
	// (HEVC 2560x1440) minute from ~10s to ~7s on the J4125. The min()
	// expression never upscales, so 720p-or-smaller sources pass through
	// unchanged. force_original_aspect_ratio=decrease keeps the frame
	// letterboxed to the correct shape for non-16:9 sensors.
	if hw {
		// -hwaccel vaapi -hwaccel_output_format vaapi decodes each
		// concat segment on the iGPU and hands VAAPI surfaces straight
		// to h264_vaapi, avoiding a GPU→RAM→GPU round trip. -qp 24 is
		// the VAAPI equivalent of a CRF around 23-24.
		args := []string{
			"-y",
			"-vaapi_device", "/dev/dri/renderD128",
			"-hwaccel", "vaapi",
			"-hwaccel_output_format", "vaapi",
			"-f", "concat", "-safe", "0",
			"-i", listPath,
			"-vf", "scale_vaapi=w=min(1280\\,iw):h=min(720\\,ih):force_original_aspect_ratio=decrease",
			"-c:v", "h264_vaapi", "-qp", "24",
		}
		args = append(args, audioArgs...)
		args = append(args, "-fflags", "+genpts", "-avoid_negative_ts", "make_zero", "-movflags", "faststart", outPath)
		return exec.Command("ffmpeg", args...)
	}
	args := []string{
		"-y",
		"-f", "concat", "-safe", "0",
		"-i", listPath,
		"-vf", "scale=min(1280\\,iw):min(720\\,ih):force_original_aspect_ratio=decrease",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
	}
	args = append(args, audioArgs...)
	args = append(args, "-fflags", "+genpts", "-avoid_negative_ts", "make_zero", "-movflags", "faststart", outPath)
	return exec.Command("ffmpeg", args...)
}

// StartCacheCleaner launches a background goroutine that periodically
// deletes transcoded cache files older than maxAge. The cache grows
// one small MP4 per viewed camera-minute; without cleanup it would
// accumulate forever on long-running deployments. Default: keep 7
// days, sweep every 6 hours. A sweep also runs once at startup so an
// in-place upgrade cleans stale files immediately.
func (h *CameraHandler) StartCacheCleaner(root string, maxAge, interval time.Duration) {
	go func() {
		cleanTranscodeCache(root, maxAge)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			cleanTranscodeCache(root, maxAge)
		}
	}()
}

// cleanTranscodeCache removes cache files under root whose mtime is
// older than maxAge. It walks the <camID>/<minuteStart>.mp4 layout and
// deletes stale files, then prunes empty camera directories. Errors
// are logged and skipped — a cleanup failure must never break playback.
func cleanTranscodeCache(root string, maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	removed, freed := 0, int64(0)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if info.IsDir() {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			if os.Remove(path) == nil {
				removed++
				freed += info.Size()
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("[handler] transcode cache sweep error: %v", err)
	}
	if removed > 0 {
		log.Printf("[handler] transcode cache sweep: removed %d files, freed %s", removed, humanSize(freed))
	}
}

// PretranscodeMinute transcodes the given minuteStart into the cache volume
// in the background if it is not already cached.
func (h *CameraHandler) PretranscodeMinute(cam *model.Camera, minuteStart int64) error {
	cacheDir := fmt.Sprintf("/data/recordings/.transcode-cache/%d", cam.ID)
	audioOn := camera.CameraHasAudio(cam)
	cacheFileName := fmt.Sprintf("%d.mp4", minuteStart)
	if !audioOn {
		cacheFileName = fmt.Sprintf("%d_noaudio.mp4", minuteStart)
	}
	cacheFile := filepath.Join(cacheDir, cacheFileName)

	// Serve/skip from cache if present and non-empty.
	if fi, err := os.Stat(cacheFile); err == nil && fi.Size() > 0 {
		return nil
	}

	paths, err := h.Reg.RecordingSegmentsForMinute(cam, minuteStart)
	if err != nil || len(paths) == 0 {
		// Fallback to Quark Cloud Archive via Alist WebDAV
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		paths, err = h.Reg.RecordingSegmentsFromCloud(ctx, cam, minuteStart)
		cancel()
	}
	if err != nil || len(paths) == 0 {
		return nil // No recordings for this minute yet
	}

	tmpDir, err := os.MkdirTemp("", "pretrans_")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	var listBuilder strings.Builder
	for _, p := range paths {
		escaped := strings.ReplaceAll(p, "'", "'\\''")
		listBuilder.WriteString(fmt.Sprintf("file '%s'\n", escaped))
	}
	listPath := filepath.Join(tmpDir, "list.txt")
	if err := os.WriteFile(listPath, []byte(listBuilder.String()), 0o644); err != nil {
		return fmt.Errorf("write concat list: %w", err)
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	tmpOut := filepath.Join(tmpDir, cacheFileName)

	// Serialize transcode through the global transcode slot
	h.transcodeSem <- struct{}{}
	defer func() {
		select {
		case <-h.transcodeSem:
		default:
		}
	}()

	cmd := buildTranscodeCmd(listPath, tmpOut, vaapiAvailable(), audioOn)
	if output, err := cmd.CombinedOutput(); err != nil {
		if vaapiAvailable() {
			cmd = buildTranscodeCmd(listPath, tmpOut, false, audioOn)
			if out2, err2 := cmd.CombinedOutput(); err2 != nil {
				return fmt.Errorf("software transcode failed: %v (%s)", err2, string(out2))
			}
		} else {
			return fmt.Errorf("transcode failed: %v (%s)", err, string(output))
		}
	}

	// Promote to cache
	if err := copyToCache(tmpOut, cacheFile); err != nil {
		return fmt.Errorf("promote to cache: %w", err)
	}
	log.Printf("[camera_handler] Pre-transcoded and cached minute %d for camera %d (%s)", minuteStart, cam.ID, cam.Name)
	return nil
}

// QueueAlertPretranscode enqueues a background job to pre-transcode the minute(s)
// corresponding to an alert. If the minute is currently ongoing, it waits until
// the minute rolls over so full 10s recording segments are finalized.
func (h *CameraHandler) QueueAlertPretranscode(camID uint, startTime, endTime int64) {
	go func() {
		cam, err := h.Reg.Get(camID)
		if err != nil {
			return
		}
		startMin := (startTime / 60) * 60
		endMin := startMin
		if endTime > 0 {
			endMin = (endTime / 60) * 60
		}
		// Also include the minute before if the alert occurred in the first 15s of the minute
		var targetMinutes []int64
		if startTime-startMin < 15 {
			targetMinutes = append(targetMinutes, startMin-60)
		}
		for m := startMin; m <= endMin; m += 60 {
			targetMinutes = append(targetMinutes, m)
		}

		for _, m := range targetMinutes {
			now := time.Now().Unix()
			targetTime := m + 65 // wait until 5s after the minute closes
			if targetTime > now {
				waitSec := targetTime - now
				if waitSec > 180 {
					waitSec = 65
				}
				time.Sleep(time.Duration(waitSec) * time.Second)
			}
			if err := h.PretranscodeMinute(cam, m); err != nil {
				log.Printf("[camera_handler] pretranscode error for camera %d, minute %d: %v", camID, m, err)
			}
		}
	}()
}

// StartAlertPretranscoder warms up recent alerts from Frigate (past 24 hours)
// so that clicking alert clips in the app or dashboard is instant from the start.
func (h *CameraHandler) StartAlertPretranscoder() {
	go func() {
		// Wait for Frigate to stabilize after startup
		time.Sleep(15 * time.Second)
		for retries := 0; retries < 6; retries++ {
			events, err := h.Reg.Frigate.ListEventsFiltered(context.Background(), camera.EventFilter{
				Limit: 50,
				After: time.Now().Add(-24 * time.Hour).Unix(),
			})
			if err != nil {
				log.Printf("[camera_handler] StartAlertPretranscoder: retry %d/6 listing events: %v", retries+1, err)
				time.Sleep(10 * time.Second)
				continue
			}
			log.Printf("[camera_handler] StartAlertPretranscoder: warming up %d recent alerts...", len(events))
			for _, ev := range events {
				camID, ok := h.Reg.LookupByFrigateSlug(ev.Camera)
				if !ok {
					continue
				}
				cam, err := h.Reg.Get(camID)
				if err != nil {
					continue
				}
				startMin := (int64(ev.StartTime) / 60) * 60
				_ = h.PretranscodeMinute(cam, startMin)
				time.Sleep(200 * time.Millisecond) // gentle pacing
			}
			log.Printf("[camera_handler] StartAlertPretranscoder: warm up completed")
			break
		}
	}()
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

	// Derive a stable ETag from the config payload so clients can
	// short-circuit with a 304 when nothing changed. SHA256 over the
	// canonical JSON serialization; 16 hex chars (8 bytes) is far
	// more collision resistance than needed for a handful of ICE
	// configs. The ETag is wrapped in quotes per RFC 7232.
	payload, err := json.Marshal(cfg)
	if err != nil {
		log.Printf("[handler] marshal ice config: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "marshal ice config")
		return
	}
	sum := sha256.Sum256(payload)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`

	c.Header("Cache-Control", "private, max-age=300")
	c.Header("ETag", etag)

	// If-None-Match: return 304 with no body when the client's cached
	// representation is still current. The ICE config changes rarely
	// (only when the operator reconfigures STUN/TURN or the public
	// base URL), so most repeat requests from the dashboard collapse
	// into a cheap 304.
	if inm := c.GetHeader("If-None-Match"); inm == etag {
		c.Status(http.StatusNotModified)
		return
	}

	utils.Success(c, cfg)
}

// WebRTC — POST /api/v1/cameras/:id/webrtc
//
// Body: the browser's SDP offer (`Content-Type: application/sdp`).
// Response: the SDP answer verbatim (`Content-Type: application/sdp`).
//
// This endpoint exists because proxying the SDP POST through nginx +
// auth_request hits a hard nginx quirk: the auth_request sub-call
// reads (and discards) the request body while preparing the
// sub-request, and the original proxy_pass upstream is left
// without bytes to send to go2rtc. The upstream connection then
// hangs on proxy_send_timeout (60s) and the browser sees a 504/500.
//
// Going front-end → home-api → go2rtc instead means the SDP body
// is read exactly once (in this handler) and forwarded exactly
// once (via Go2RTCClient.ExchangeSDP). Auth is the existing
// camGroup JWT middleware; no new auth surface. The WebRTC media
// path (RTP / RTCP over UDP 8555) is unchanged — that's a
// direct browser-to-go2rtc link that doesn't touch nginx.
//
// Errors:
//   - 400 if the SDP body is empty or unreadable
//   - 404 if the camera doesn't exist (handled by requireCanRead)
//   - 403 if the caller doesn't own the camera (handled by requireCanRead)
//   - 502 if go2rtc is down or returns 5xx (the answer body is
//     included in the error message so the front-end can surface it)
func (h *CameraHandler) WebRTC(c *gin.Context) {
	cam, ok := h.requireCanRead(c)
	if !ok {
		return
	}

	// Reject obviously bad bodies. SDP offers from a healthy browser
	// are 1–4 KiB; 64 KiB is a generous cap that still leaves room
	// for a future trickle-ICE variant while not letting a
	// misbehaving client push us into reading a multi-GB body into
	// memory.
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<16))
	if err != nil {
		log.Printf("[handler] read sdp body: %v", err)
		utils.Fail(c, http.StatusBadRequest, "read sdp body")
		return
	}
	if len(body) == 0 {
		utils.Fail(c, http.StatusBadRequest, "empty SDP body")
		return
	}

	streamName := cam.StreamName
	quality := strings.ToLower(strings.TrimSpace(c.Query("quality")))
	if quality == "1080p" {
		streamName = cam.StreamName + "_1080p"
	}

	answer, err := h.Reg.Go2.ExchangeSDP(c.Request.Context(), streamName, body)
	if err != nil && streamName != cam.StreamName {
		// Fallback to standard 720p stream if 1080p stream fails
		log.Printf("webrtc: %s failed (%v), falling back to standard stream %s", streamName, err, cam.StreamName)
		streamName = cam.StreamName
		answer, err = h.Reg.Go2.ExchangeSDP(c.Request.Context(), streamName, body)
	}
	if err != nil {
		for attempt := 1; attempt <= 2; attempt++ {
			backoff := time.Duration(attempt*250) * time.Millisecond
			log.Printf("webrtc: SDP exchange attempt %d failed for %s (%v), retrying in %v", attempt, streamName, err, backoff)
			select {
			case <-time.After(backoff):
			case <-c.Request.Context().Done():
				utils.Fail(c, http.StatusBadGateway, "client canceled SDP exchange")
				return
			}
			answer, err = h.Reg.Go2.ExchangeSDP(c.Request.Context(), streamName, body)
			if err == nil {
				log.Printf("webrtc: retry %d succeeded for %s", attempt, streamName)
				break
			}
		}
		if err != nil {
			log.Printf("[handler] failed to exchange SDP for %s: %v", streamName, err)
			utils.Fail(c, http.StatusBadGateway, "failed to exchange SDP: "+err.Error())
			return
		}
	}

	// go2rtc returns the SDP answer as the response body. We
	// mirror that contract so the browser can do
	// `await resp.text()` and feed it straight into
	// pc.setRemoteDescription({type:"answer", sdp}). Use the
	// successful SDP-200 status code (utils.Success wraps in our
	// standard {code,message,data} envelope, which is NOT what
	// the browser expects for the SDP answer), so write the raw
	// body instead.
	c.Header("Content-Type", "application/sdp")
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusOK)
	_, _ = c.Writer.Write(answer)
}

// Preheat — POST /api/v1/cameras/:id/preheat
//
// Triggers go2rtc to connect to the RTSP source proactively so the
// first real video request (WebRTC SDP or HLS) doesn't pay the
// 1-10s cold-start. Best-effort, non-blocking — returns 200
// immediately.
//
// If the camera doesn't exist or preheat fails, the response is
// still 200 (the client shouldn't fail just because preheat failed
// — the next real request will warm the source anyway). The actual
// preheat runs in a detached goroutine inside PreheatStream, so the
// handler returns as soon as the camera lookup completes.
func (h *CameraHandler) Preheat(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	// Best-effort: ignore the error. A missing camera or a down
	// go2rtc is not a client-visible failure — preheat is purely an
	// optimization.
	_ = h.Reg.PreheatStream(uint(id))
	utils.Success(c, gin.H{"status": "preheating"})
}

// List — GET /api/v1/cameras
func (h *CameraHandler) List(c *gin.Context) {
	uid, isAdmin, ok := h.callerIsAdmin(c)
	cams := h.Reg.List()
	views := make([]gin.H, 0, len(cams))
	for i := range cams {
		if !isAdmin && !h.Reg.CanRead(&cams[i], uid, isAdmin) {
			continue
		}
		isPrivileged := ok && (isAdmin || cams[i].OwnerID == uid)
		canPTZ := ok && h.Reg.CanPTZ(&cams[i], uid, isAdmin)
		views = append(views, cameraView(&cams[i], h.Reg.StreamConfig(&cams[i]), isPrivileged, canPTZ))
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
	uid, isAdmin, ok := h.callerIsAdmin(c)
	if !isAdmin && !h.Reg.CanRead(cam, uid, isAdmin) {
		utils.Fail(c, http.StatusForbidden, "camera access forbidden")
		return
	}
	isPrivileged := ok && (isAdmin || cam.OwnerID == uid)
	canPTZ := ok && h.Reg.CanPTZ(cam, uid, isAdmin)
	utils.Success(c, cameraView(cam, h.Reg.StreamConfig(cam), isPrivileged, canPTZ))
}

// Delete — DELETE /api/v1/cameras/:id
func (h *CameraHandler) Delete(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	// v1.8.20: snapshot the camera name + caller identity BEFORE
	// Unregister so the audit-log event carries a friendly label
	// (the row is gone after Unregister).
	adminID, _, _ := h.callerIsAdmin(c)
	cameraName := ""
	if cam, gerr := h.Reg.Get(uint(id)); gerr == nil {
		cameraName = cam.Name
	}
	if err := h.Reg.Unregister(c.Request.Context(), uint(id)); err != nil {
		log.Printf("[handler] failed to unregister camera: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "failed to unregister camera")
		return
	}
	h.ONVIF.Forget(uint(id))

	// v1.8.20: audit-trail event for camera deletion.
	if h.bus != nil {
		payload, _ := json.Marshal(eventbus.CameraDeletePayload{
			AdminID:    adminID,
			CameraID:   uint(id),
			CameraName: cameraName,
			Ts:         time.Now().Unix(),
		})
		h.bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicCameraDelete,
			Payload: payload,
			Source:  eventbus.SourceSystem,
		})
	}

	utils.Success(c, gin.H{"id": id})
}

// requireCanManageShares loads the camera at :id and rejects the
// request unless the caller is an admin or the camera's owner. This
// is STRICTER than requireCanRead: a user who only has access via a
// CameraShare row can READ the camera (stream, recordings, frames)
// but cannot grant access to others or revoke existing shares.
// Sharing is an owner/admin privilege — letting a shared viewer
// re-share would be a privilege escalation.
//
// Returns the loaded *model.Camera on success so handlers don't
// re-fetch.
func (h *CameraHandler) requireCanManageShares(c *gin.Context) (*model.Camera, bool) {
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
	if !isAdmin && cam.OwnerID != uid {
		utils.Fail(c, http.StatusForbidden, "only the camera owner or an admin can manage shares")
		return nil, false
	}
	return cam, true
}

// shareReq is the wire format for POST /api/v1/cameras/:id/shares.
//
//	{ "user_id": 42 }
type shareReq struct {
	UserID uint `json:"user_id" binding:"required"`
	CanPTZ bool `json:"can_ptz"`
}

// ShareCamera — POST /api/v1/cameras/:id/shares
//
// Grants the given user read access to the camera (with optional PTZ permission). Idempotent —
// re-sharing with the same user updates permissions (200, not 409). Only
// the camera owner or an admin may call this (enforced by
// requireCanManageShares).
func (h *CameraHandler) ShareCamera(c *gin.Context) {
	cam, ok := h.requireCanManageShares(c)
	if !ok {
		return
	}
	var req shareReq
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Printf("[handler] share camera invalid request body: %v", err)
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.UserID == 0 {
		utils.Fail(c, http.StatusBadRequest, "user_id required")
		return
	}
	// Prevent self-sharing: the owner already has access by
	// virtue of ownership, and an admin already has access by
	// virtue of being admin. Inserting a redundant row would
	// just clutter ListShares and confuse the dashboard's
	// viewer list.
	if req.UserID == cam.OwnerID {
		utils.Fail(c, http.StatusBadRequest, "cannot share with the camera owner (already has access)")
		return
	}
	if err := h.Reg.ShareCamera(cam.ID, req.UserID, req.CanPTZ); err != nil {
		log.Printf("[handler] failed to share camera: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "failed to share camera")
		return
	}
	if h.bus != nil {
		payload, _ := json.Marshal(eventbus.CameraManagePayload{
			AdminID:    c.GetUint("user_id"),
			CameraID:   cam.ID,
			CameraName: cam.Name,
			Action:     "update",
			Detail:     fmt.Sprintf("将摄像头共享给用户 #%d", req.UserID),
			Ts:         time.Now().Unix(),
		})
		h.bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicCameraUpdate,
			Payload: payload,
			Source:  eventbus.SourceSystem,
		})
	}
	utils.Success(c, gin.H{"camera_id": cam.ID, "user_id": req.UserID, "can_ptz": req.CanPTZ})
}

// UnshareCamera — DELETE /api/v1/cameras/:id/shares/:user_id
//
// Revokes the given user's read access to the camera. Idempotent —
// unsharing a user who was never shared is a no-op (200, not 404).
// Only the camera owner or an admin may call this.
func (h *CameraHandler) UnshareCamera(c *gin.Context) {
	cam, ok := h.requireCanManageShares(c)
	if !ok {
		return
	}
	uid, err := strconv.Atoi(c.Param("user_id"))
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid user_id")
		return
	}
	if err := h.Reg.UnshareCamera(cam.ID, uint(uid)); err != nil {
		log.Printf("[handler] failed to unshare camera: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "failed to unshare camera")
		return
	}
	if h.bus != nil {
		payload, _ := json.Marshal(eventbus.CameraManagePayload{
			AdminID:    c.GetUint("user_id"),
			CameraID:   cam.ID,
			CameraName: cam.Name,
			Action:     "update",
			Detail:     fmt.Sprintf("取消用户 #%d 的摄像头共享权限", uid),
			Ts:         time.Now().Unix(),
		})
		h.bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicCameraUpdate,
			Payload: payload,
			Source:  eventbus.SourceSystem,
		})
	}
	utils.Success(c, gin.H{"camera_id": cam.ID, "user_id": uid})
}

// ListShares — GET /api/v1/cameras/:id/shares
//
// Returns the list of users the camera has been shared with. The
// caller must be able to read the camera (requireCanRead) — a
// shared viewer can see who else has access, but only the owner
// or an admin can mutate the list.
func (h *CameraHandler) ListShares(c *gin.Context) {
	cam, ok := h.requireCanRead(c)
	if !ok {
		return
	}
	shares, err := h.Reg.ListShares(cam.ID)
	if err != nil {
		log.Printf("[handler] failed to list shares: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "failed to list shares")
		return
	}
	utils.Success(c, shares)
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
	cam, ok := h.requireCanRead(c)
	if !ok {
		return
	}
	uid, isAdmin, _ := h.callerIsAdmin(c)
	if !h.Reg.CanPTZ(cam, uid, isAdmin) {
		utils.Fail(c, http.StatusForbidden, "ptz permission denied")
		return
	}
	var req ptzReq
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Printf("[handler] ptz invalid request body: %v", err)
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
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
		speed = 0.25
	}

	if err := h.ONVIF.ContinuousMove(
		c.Request.Context(),
		cam.ID, cam.Host, cam.ONVIFPort,
		user, pass, profile,
		camera.PTZCommand(req.Command), speed,
	); err != nil {
		log.Printf("[handler] failed to execute PTZ command: %v", err)
		utils.Fail(c, http.StatusBadGateway, "failed to execute PTZ command")
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
// When isPrivileged is false (non-admin shared viewer), host and
// port fields are masked to prevent internal network disclosure.
func cameraView(cam *model.Camera, stream camera.StreamConfig, isPrivileged bool, canPTZ bool) gin.H {
	host := cam.Host
	onvifPort := cam.ONVIFPort
	rtspPort := cam.RTSPPort
	if !isPrivileged {
		host = "***"
		onvifPort = 0
		rtspPort = 0
	}
	return gin.H{
		"id":           cam.ID,
		"type":         cam.Type,
		"name":         cam.Name,
		"vendor":       cam.Vendor,
		"host":         host,
		"onvif_port":   onvifPort,
		"rtsp_port":    rtspPort,
		"channel_id":   cam.ChannelID,
		"status":       cam.Status,
		"last_seen_at": cam.LastSeenAt,
		"capabilities": cam.Capabilities,
		"can_ptz":      canPTZ,
		"meta":         cam.Meta,
		"detect_fps":   camera.CameraDetectFPS(cam),
		"stream":       stream,
		"transcode":    cam.Transcode,
		"codec":        cam.Codec,
		"created_at":   cam.CreatedAt,
		"updated_at":   cam.UpdatedAt,
	}
}
