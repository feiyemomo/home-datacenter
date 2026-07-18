package handler

import (
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
	"time"

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
	if _, _, ok := h.callerIsAdmin(c); !ok {
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
		utils.Fail(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.Reg.UpdateCodec(c.Request.Context(), uint(id), body.Codec); err != nil {
		utils.Fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	utils.Success(c, gin.H{"id": id, "codec": body.Codec})
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
		utils.Fail(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.Reg.SetRecordingEnabled(c.Request.Context(), uint(id), body.Enabled, body.RetentionDays); err != nil {
		utils.Fail(c, http.StatusInternalServerError, err.Error())
		return
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
// We pass after=now-7d, before=now (both required — Frigate returns
// an empty array when `after` is set but `before` is omitted).
func (h *CameraHandler) ListRecordings(c *gin.Context) {
	cam, ok := h.requireCanRead(c)
	if !ok {
		return
	}
	slug := h.Reg.FrigateSlug(cam)
	after := time.Now().AddDate(0, 0, -7).Unix()
	recs, err := h.Reg.Frigate.ListRecordings(c.Request.Context(), slug, after, 0)
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, err.Error())
		return
	}

	// Aggregate 10s segments into 60s buckets keyed by minute-start.
	type bucket struct {
		start    int64
		end      float64
		duration float64
		count    int
	}
	buckets := make(map[int64]*bucket)
	var order []int64
	for _, r := range recs {
		minuteStart := (r.StartTime / 60) * 60
		b, exists := buckets[minuteStart]
		if !exists {
			b = &bucket{start: minuteStart, end: r.EndTime, duration: r.Duration, count: 1}
			buckets[minuteStart] = b
			order = append(order, minuteStart)
		} else {
			if r.EndTime > b.end {
				b.end = r.EndTime
			}
			b.duration += r.Duration
			b.count++
		}
	}

	// Sort newest-first (descending by start time).
	sort.Slice(order, func(i, j int) bool { return order[i] > order[j] })

	views := make([]gin.H, 0, len(order))
	for _, ts := range order {
		b := buckets[ts]
		views = append(views, gin.H{
			"id":               b.start,
			"camera_id":        cam.ID,
			"start_at":         time.Unix(b.start, 0).UTC().Format(time.RFC3339),
			"end_at":           time.Unix(int64(b.end), 0).UTC().Format(time.RFC3339),
			"duration_seconds": int(b.duration),
			"segment_count":    b.count,
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
	if err != nil {
		utils.Fail(c, http.StatusNotFound, "no recording directory for this minute: "+err.Error())
		return
	}
	if len(paths) == 0 {
		utils.Fail(c, http.StatusNotFound, "no recording segments found in this minute")
		return
	}

	// Single segment: serve directly (fast path, no ffmpeg needed).
	if len(paths) == 1 {
		c.Header("Cache-Control", "no-store")
		http.ServeFile(c.Writer, c.Request, paths[0])
		return
	}

	// Multiple segments: concatenate with ffmpeg stream copy.
	tmpDir, err := os.MkdirTemp("", "rec_")
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, "create temp dir: "+err.Error())
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
		utils.Fail(c, http.StatusInternalServerError, "write concat list: "+err.Error())
		return
	}

	// Run ffmpeg: concat demuxer + stream copy + faststart (moov at
	// start for instant playback). Output to temp file, then serve.
	outPath := filepath.Join(tmpDir, "out.mp4")
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "concat", "-safe", "0",
		"-i", listPath,
		"-c", "copy",
		"-movflags", "faststart",
		outPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		log.Printf("PlayRecording: ffmpeg concat failed: %v: %s", err, string(output))
		utils.Fail(c, http.StatusInternalServerError, "ffmpeg concat failed")
		return
	}

	c.Header("Cache-Control", "no-store")
	http.ServeFile(c.Writer, c.Request, outPath)
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
		utils.Fail(c, http.StatusBadRequest, "read sdp body: "+err.Error())
		return
	}
	if len(body) == 0 {
		utils.Fail(c, http.StatusBadRequest, "empty SDP body")
		return
	}

	answer, err := h.Reg.Go2.ExchangeSDP(c.Request.Context(), cam.StreamName, body)
	if err != nil {
		// 502 because the upstream (go2rtc) is the failing party
		// — the request itself was well-formed. The go2rtc error
		// message is included so the front-end can render it.
		utils.Fail(c, http.StatusBadGateway, err.Error())
		return
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
		"codec":        cam.Codec,
		"created_at":   cam.CreatedAt,
		"updated_at":   cam.UpdatedAt,
	}
}
