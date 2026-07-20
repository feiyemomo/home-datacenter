package camera

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

// Registry is the camera CRUD + Frigate/go2rtc sync boundary. It does NOT
// expose HTTP — the handler layer calls it.
//
// Frigate bundles go2rtc internally. We use two clients:
//   - Go2: talks to the bundled go2rtc API (stream add/remove, SDP
//     exchange, recorder). Same API as the old standalone container.
//   - Frigate: talks to the Frigate REST API (config save) so Frigate's
//     AI detection and recording pipelines know about each camera.
type Registry struct {
	DB        *gorm.DB
	Go2       *Go2RTCClient
	Frigate   *FrigateClient
	Box       *utils.SecretBox
	ONVIF     *ONVIFController
	WebRTCURL string // optional public base, e.g. https://cam.feiyemomo.top
}

func NewRegistry(db *gorm.DB, g *Go2RTCClient, fr *FrigateClient, box *utils.SecretBox, onvif *ONVIFController, webRTCURL string) *Registry {
	return &Registry{DB: db, Go2: g, Frigate: fr, Box: box, ONVIF: onvif, WebRTCURL: webRTCURL}
}

// RegisterInput is the wire format for POST /api/v1/cameras.
// The handler is responsible for binding it; the service is
// responsible for sanitising defaults and persisting it.
type RegisterInput struct {
	Name         string
	Vendor       string
	Host         string
	ONVIFPort    int
	RTSPPort     int
	ChannelID    int
	Username     string
	Password     string
	PTZ          bool
	Audio        bool
	Motion       bool
	ProfileToken string // optional; blank → controller picks the first profile
	// Transcode opts this camera into server-side H.264
	// transcoding. The registry translates the boolean to a
	// `#video=h264` URL fragment on the RTSP source; go2rtc
	// interprets that fragment as "use ffmpeg, output H.264".
	// Requires ffmpeg in the go2rtc image (always present in
	// this build). Per-camera so the operator can leave
	// H.264-friendly cameras untouched and only pay the
	// CPU/memory cost on HEVC cameras they want over WebRTC.
	Transcode bool
	// Codec overrides Transcode when non-empty. Values: "passthrough",
	// "h264", "h265". Empty inherits legacy Transcode behavior.
	Codec   string
	OwnerID uint // 0 = assign to caller from handler context
}

// Register inserts a Camera row, then asks go2rtc to start pulling
// the RTSP stream under the user-entered friendly name (e.g.
// "前门"). The go2rtc stream key matches the dashboard name 1:1, so
// `GET /api/streams` shows the operator's names directly and a
// 1:N rename of friendly names propagates naturally. The
// `stream_name` column carries a `UNIQUE` constraint — two
// cameras with the same name will fail to register the second
// one (the operator must rename one).
//
// If go2rtc is down we still keep the DB row (so the operator can
// see & retry) but bubble up the error so the handler can return
// 502.
//
// When ProfileToken is empty the registry transparently issues an
// ONVIF GetProfiles to discover the camera's first media profile,
// so the caller doesn't need to know ONVIF at all.
func (r *Registry) Register(ctx context.Context, in RegisterInput) (*model.Camera, error) {
	if in.RTSPPort == 0 {
		in.RTSPPort = 554
	}
	if in.ONVIFPort == 0 {
		in.ONVIFPort = 80
	}
	if in.ChannelID == 0 {
		in.ChannelID = 1
	}
	// Sanity: the friendly name is now the go2rtc stream key (Bug1
	// fix). Reject blank / whitespace-only names so we don't end up
	// with an empty go2rtc stream entry that's impossible to look up
	// from the dashboard.
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("name is required and must not be blank")
	}

	creds, err := r.boxCredentials(in.Username, in.Password)
	if err != nil {
		return nil, err
	}

	profile := in.ProfileToken
	if profile == "" && r.ONVIF != nil {
		if ps, perr := r.ONVIF.DiscoverProfiles(ctx, in.Host, in.ONVIFPort, in.Username, in.Password); perr == nil && len(ps) > 0 {
			profile = ps[0].Token
		}
	}

	cam := &model.Camera{
		Type:              "camera",
		Name:              name,
		Vendor:            in.Vendor,
		Host:              in.Host,
		ONVIFPort:          in.ONVIFPort,
		RTSPPort:          in.RTSPPort,
		ChannelID:         in.ChannelID,
		Status:            "unknown",
		OwnerID:           in.OwnerID,
		OnvifProfileToken: profile,
		Capabilities: model.JSON{
			"ptz":    in.PTZ,
			// v1.5.14: default audio=true so the mobile app hears
			// sound on live streams + recordings. The previous
			// default (false) made rtspURL emit #audio=0, which
			// silently stripped the audio track from go2rtc's
			// source — the user saw video but no sound. Audio
			// costs ~96 kbps/stream (AAC) and ~5% CPU per ffmpeg
			// transcode, an acceptable trade for working mobile
			// audio. Admins can still opt out per-camera via the
			// dashboard's audio switch (PUT /audio endpoint).
			"audio":  true,
			"motion": in.Motion,
		},
		Credentials: creds,
		Meta:        model.JSON{},
		Transcode:   in.Transcode,
		Codec:       in.Codec,
		// Bug1 fix: the go2rtc stream key is the friendly name,
		// not `cam_<id>`. Setting it BEFORE Create() lets the
		// UNIQUE constraint on stream_name reject duplicates at
		// the DB layer instead of crashing inside go2rtc.AddStream.
		StreamName: name,
	}

	if err := r.DB.Create(cam).Error; err != nil {
		return nil, err
	}

	rtspURL := r.rtspURL(cam, in.Username, in.Password)
	if err := r.Go2.AddStream(ctx, cam.StreamName, rtspURL); err != nil {
		// Roll back the DB row so the system doesn't claim a stream
		// that go2rtc doesn't have. Keep the original error.
		_ = r.DB.Delete(cam).Error
		return nil, fmt.Errorf("go2rtc add stream: %w", err)
	}

	// Preheat the go2rtc stream: force an RTSP source connection now
	// so the operator's first frame doesn't pay the 1-10s cold-start
	// latency. Best-effort and non-blocking — a failure here simply
	// means the first user request warms the source instead. We use
	// a detached context (no cancellation tied to this HTTP request)
	// so preheat continues after the handler returns.
	go func(streamName string) {
		preheatCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		r.Go2.Preheat(preheatCtx, streamName)
	}(cam.StreamName)

	// Push the full config to Frigate so its AI detection and
	// recording pipelines pick up the new camera. Best-effort:
	// if Frigate is down, the go2rtc stream is still live and
	// the operator can view video. The config will be pushed on
	// the next BootReplay.
	if r.Frigate != nil {
		if err := r.pushFrigateConfig(ctx); err != nil {
			log.Printf("camera: register: frigate config push (non-fatal): %v", err)
		}
	}

	if err := r.DB.Model(cam).Updates(map[string]any{
		"updated_at": time.Now(),
	}).Error; err != nil {
		return nil, err
	}
	return cam, nil
}

// Unregister removes the row and asks go2rtc to drop the stream.
// go2rtc errors are logged but not returned — the DB is the source
// of truth and we don't want a half-deleted camera. The Frigate config
// is also re-pushed so Frigate drops the camera from its detection
// pipeline.
func (r *Registry) Unregister(ctx context.Context, id uint) error {
	var cam model.Camera
	if err := r.DB.First(&cam, id).Error; err != nil {
		return err
	}
	if cam.StreamName != "" {
		_ = r.Go2.RemoveStream(ctx, cam.StreamName)
	}
	if err := r.DB.Delete(&cam).Error; err != nil {
		return err
	}
	if r.Frigate != nil {
		if err := r.pushFrigateConfig(ctx); err != nil {
			log.Printf("camera: unregister: frigate config push (non-fatal): %v", err)
		}
	}
	return nil
}

func (r *Registry) Get(id uint) (*model.Camera, error) {
	var c model.Camera
	if err := r.DB.First(&c, id).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

// LookupByFrigateSlug resolves a Frigate camera slug (e.g.
// "front_door") back to a home-api camera ID. It scans all cameras
// and computes each one's Frigate slug until it finds a match.
// Returns (0, false) if no camera matches.
func (r *Registry) LookupByFrigateSlug(slug string) (uint, bool) {
	var cams []model.Camera
	if err := r.DB.Find(&cams).Error; err != nil {
		return 0, false
	}
	for _, c := range cams {
		if r.FrigateSlug(&c) == slug {
			return c.ID, true
		}
	}
	return 0, false
}

// UpdateCodec changes the output codec for a camera and re-pushes
// the stream to go2rtc so the new codec takes effect immediately
// without requiring a container restart.
//
// Only "h264" is accepted. WebRTC's RTP codec registry mandates
// H.264 (plus VP8/VP9/AV1) but does NOT include H.265, so
// "passthrough" and "h265" always 502 on Chrome/Edge/Firefox WebRTC.
// Legacy cameras with codec=passthrough/h265 (set before this
// restriction) still work via effectiveCodec/rtspURL for backward
// compatibility, but cannot be (re)set to those values via this API.
// The dashboard dropdown only offers "H.264" and shows legacy values
// as a disabled "(legacy)" entry so the operator can migrate.
func (r *Registry) UpdateCodec(ctx context.Context, id uint, codec string) error {
	codec = strings.TrimSpace(codec)
	switch codec {
	case "h264":
	case "":
		codec = "h264"
	default:
		return fmt.Errorf("invalid codec %q (only \"h264\" is accepted — WebRTC does not support H.265)", codec)
	}
	var cam model.Camera
	if err := r.DB.First(&cam, id).Error; err != nil {
		return err
	}
	cam.Codec = codec
	cam.Transcode = codec != "passthrough"
	if err := r.DB.Model(&cam).Updates(map[string]any{
		"codec":       codec,
		"transcode":   cam.Transcode,
		"updated_at":  time.Now(),
	}).Error; err != nil {
		return err
	}
	// Re-push the go2rtc stream with the new URL so the codec
	// change is live immediately. go2rtc hot-reloads the stream
	// without interrupting other cameras.
	//
	// We do NOT call pushFrigateConfig here because Frigate's
	// recording pipeline uses the camera's NATIVE stream (plain
	// rtsp:// URL from frigateCameraPath), which does NOT change
	// when codec changes. The codec setting only affects go2rtc's
	// live transcode path. Avoiding pushFrigateConfig here prevents
	// an unnecessary Frigate restart (which would briefly interrupt
	// all streams and the recording pipeline).
	user, pass, err := r.DecryptCredentials(&cam)
	if err == nil {
		rtspURL := r.rtspURL(&cam, user, pass)
		_ = r.Go2.AddStream(ctx, cam.StreamName, rtspURL)
	}
	return nil
}

// UpdateAudio — PUT /api/v1/cameras/:id/audio
//
//	{ "enabled": true }
//
// Toggles the audio capability flag on a camera. When enabled, the
// next go2rtc stream push (performed inline here) rewrites the source
// URL to include `#audio=aac` so the camera's PCMA track is transcoded
// to AAC and exposed in the HLS/MP4 stream. ExoPlayer and modern
// browsers decode AAC natively; the original PCMA from Hikvision
// cameras is not browser-decodable.
//
// This endpoint does NOT touch Frigate's recording config — Frigate
// records the camera's native stream and is unaffected by the live
// audio toggle. Audio is only added to live HLS/MP4/WebRTC playback.
func (r *Registry) UpdateAudio(ctx context.Context, id uint, enabled bool) error {
	var cam model.Camera
	if err := r.DB.First(&cam, id).Error; err != nil {
		return err
	}
	if cam.Capabilities == nil {
		cam.Capabilities = model.JSON{}
	}
	cam.Capabilities["audio"] = enabled
	if err := r.DB.Model(&cam).Updates(map[string]any{
		"capabilities": cam.Capabilities,
		"updated_at":   time.Now(),
	}).Error; err != nil {
		return err
	}
	// Re-push the go2rtc stream so the audio change is live
	// immediately. The new rtspURL() picks up the new audio flag
	// and produces a URL with `#audio=aac` (or stripped if
	// disabled).
	user, pass, err := r.DecryptCredentials(&cam)
	if err == nil {
		rtspURL := r.rtspURL(&cam, user, pass)
		_ = r.Go2.AddStream(ctx, cam.StreamName, rtspURL)
	}
	return nil
}

func (r *Registry) List() []model.Camera {
	var cs []model.Camera
	r.DB.Find(&cs)
	return cs
}

// FindByFrigateCamera looks up a camera by its Frigate slug name.
// Frigate uses ASCII slugs (via slugifyName) while our StreamName
// keeps the original friendly name. This method iterates all cameras
// and matches the slugified name.
func (r *Registry) FindByFrigateCamera(frigateName string) (*model.Camera, error) {
	cams := r.List()
	for i := range cams {
		if slugifyName(cams[i].StreamName) == frigateName {
			return &cams[i], nil
		}
	}
	return nil, fmt.Errorf("camera with frigate name %q not found", frigateName)
}

// SetRecordingEnabled toggles Frigate's continuous recording for a
// single camera by re-pushing the full config with the target
// camera's Record.Enabled flipped. This is the backend behind the
// dashboard "启用录制/停止录制" button.
//
// The recording plan is also persisted in the camera's Meta.recording
// key so the dashboard can show the current state across refreshes.
//
// When enabling recording, the Frigate config push uses
// requires_restart=1 because Frigate only starts the recording
// ffmpeg pipeline during a restart — a hot config merge (requires_restart=0)
// returns 200 but never produces recordings. Disabling recording
// does not need a restart (the recorder just stops on the next cycle).
func (r *Registry) SetRecordingEnabled(ctx context.Context, camID uint, enabled bool, retentionDays int) error {
	var cam model.Camera
	if err := r.DB.First(&cam, camID).Error; err != nil {
		return err
	}
	if retentionDays <= 0 {
		retentionDays = 7
	}
	// Persist the plan on the camera's Meta so the dashboard can
	// show the current state.
	if cam.Meta == nil {
		cam.Meta = model.JSON{}
	}
	cam.Meta["recording"] = map[string]any{
		"enabled":         enabled,
		"retention_days":  retentionDays,
		"segment_seconds": 3600, // Frigate uses 1-hour segments
	}
	if err := r.DB.Model(&cam).Updates(map[string]any{
		"meta":       model.JSON(cam.Meta),
		"updated_at": time.Now(),
	}).Error; err != nil {
		return err
	}
	// Re-push the Frigate config. pushFrigateConfig reads from the
	// DB so it will pick up the updated Meta. But we need to
	// override the Record.Enabled for THIS camera specifically —
	// pushFrigateConfig enables recording for ALL cameras by
	// default. We push a custom config here that respects the
	// per-camera toggle.
	if r.Frigate != nil {
		if err := r.pushFrigateConfigWithRecording(ctx, &cam, enabled, retentionDays); err != nil {
			return fmt.Errorf("frigate config push: %w", err)
		}
	}
	return nil
}

// pushFrigateConfigWithRecording is like pushFrigateConfig but
// overrides the Record.Enabled for the specified camera. All other
// cameras keep their default (recording enabled). This lets the
// dashboard toggle recording per-camera.
//
// When enabling recording on the target camera, requires_restart=true
// is passed to PushConfig so Frigate restarts and spins up the
// recording ffmpeg pipeline. Disabling recording does not need a
// restart (the recorder stops on the next cycle).
func (r *Registry) pushFrigateConfigWithRecording(ctx context.Context, targetCam *model.Camera, enabled bool, retentionDays int) error {
	cams := r.List()
	frigateCams := make([]FrigateCameraConfig, 0, len(cams))
	go2rtcStreams := make(map[string]string)
	for _, c := range cams {
		if c.StreamName == "" {
			continue
		}
		u, p, err := r.DecryptCredentials(&c)
		if err != nil {
			log.Printf("camera: frigate config: cam %d: decrypt: %v", c.ID, err)
			continue
		}
		go2rtcURL := r.rtspURL(&c, u, p)
		frigatePath := r.frigateCameraPath(&c, u, p)
		slug := slugifyName(c.StreamName)

		// Default: recording enabled. Per-camera retention is set
		// globally via the `record` key in PushConfig.
		recEnabled := true
		if c.ID == targetCam.ID {
			recEnabled = enabled
		} else {
			// Respect other cameras' saved state.
			if raw, ok := c.Meta["recording"]; ok {
				if m, ok := raw.(map[string]any); ok {
					if v, ok := m["enabled"].(bool); ok {
						recEnabled = v
					}
				}
			}
		}

		frigateCams = append(frigateCams, FrigateCameraConfig{
			Name:    slug,
			Enabled: true,
			Ffmpeg: FrigateFfmpeg{
				Inputs: []FrigateInput{
					{
						Path:  frigatePath,
						Roles: []string{"detect", "record"},
					},
				},
			},
			Detect: FrigateDetect{Enabled: true, FPS: 2},
			Record: FrigateRecord{Enabled: recEnabled},
		})
		go2rtcStreams[c.StreamName] = go2rtcURL
	}
	// requires_restart=true when enabling recording so Frigate
	// starts the recording ffmpeg pipeline. Without a restart the
	// config push returns 200 but no recordings are produced.
	return r.Frigate.PushConfig(ctx, frigateCams, go2rtcStreams, enabled)
}

// FrigateSlug returns the ASCII slug Frigate uses for this camera.
func (r *Registry) FrigateSlug(cam *model.Camera) string {
	return slugifyName(cam.StreamName)
}

// RecordingSegmentsForMinute returns the on-disk paths of all 10-second
// recording segments that fall within the minute containing minuteStart.
//
// Frigate 0.17 stores recording segments as ~10s MP4 files at:
//
//	/media/frigate/recordings/YYYY-MM-DD/HH/<camera_slug>/MM.SS.mp4
//
// where the timestamp components are in UTC. Crucially, the SS (seconds)
// part is NOT aligned to 10-second boundaries — segments start whenever
// Frigate's recording pipeline started and continue every ~10s after
// that, so a minute can contain files like 13.08.mp4, 13.18.mp4,
// 13.28.mp4, 13.38.mp4, 13.48.mp4, 13.58.mp4 (offset by 8s from the
// minute edge). Constructing paths from minuteStart+offset(0,10,20,...)
// therefore misses every file. We list the directory instead and filter
// by the MM prefix.
//
// The API container bind-mounts ./data/frigate to /media/frigate
// (read-only) so these files are accessible for direct serving via
// http.ServeFile. Frigate 0.17 has NO REST endpoint for downloading
// individual recording segments — /api/<cam>/recording/<start>/index.mp4
// 404s. The only way to serve recordings is to read files from disk.
func (r *Registry) RecordingSegmentsForMinute(cam *model.Camera, minuteStart int64) ([]string, error) {
	slug := r.FrigateSlug(cam)
	// v1.5.15: use Asia/Shanghai timezone (matching the Frigate
	// container's TZ env) to compute the on-disk directory path.
	// Frigate stores recordings under
	// /media/frigate/recordings/YYYY-MM-DD/HH/<cam>/MM.SS.mp4 where
	// the timestamp components are in the container's LOCAL time
	// (Asia/Shanghai after v1.5.13's TZ fix). The previous code used
	// .UTC() which produced UTC date/hour components — fine when
	// Frigate ran with TZ=UTC (pre-v1.5.13), but now points to a
	// non-existent directory (8 hours behind the real one). The
	// user saw "time差8小时" because clicking a recording at 03:31
	// LOCAL made the backend look in the 19:00 UTC bucket from the
	// previous day, returning 404 or playing the wrong segment.
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.Local // fallback: container TZ should also be Asia/Shanghai
	}
	t := time.Unix(minuteStart, 0).In(loc)
	dir := fmt.Sprintf("/media/frigate/recordings/%s/%s/%s",
		t.Format("2006-01-02"), // YYYY-MM-DD
		t.Format("15"),         // HH
		slug,
	)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	mm := t.Format("04") // 2-digit minute, zero-padded
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Match "MM.SS.mp4" where MM equals the requested minute.
		// We prefix-match on "MM." to be tolerant of any SS value.
		if !strings.HasPrefix(name, mm+".") || !strings.HasSuffix(name, ".mp4") {
			continue
		}
		paths = append(paths, dir+"/"+name)
	}
	sort.Strings(paths)
	return paths, nil
}

// ListForOwner returns the cameras visible to a given user. Admins
// (isAdmin=true) see every row; non-admins only see cameras whose
// OwnerID matches their user id.
func (r *Registry) ListForOwner(userID uint, isAdmin bool) []model.Camera {
	var cs []model.Camera
	q := r.DB.Model(&model.Camera{})
	if !isAdmin {
		q = q.Where("owner_id = ?", userID)
	}
	q.Find(&cs)
	return cs
}

// CanRead reports whether a user is allowed to read the camera.
// Mirrors ListForOwner: admin always, non-admin only own.
func (r *Registry) CanRead(c *model.Camera, userID uint, isAdmin bool) bool {
	if isAdmin {
		return true
	}
	return c.OwnerID == userID
}

// SaveProfileToken persists a discovered ONVIF profile token so the
// next PTZ call doesn't need to re-run ONVIF discovery.
func (r *Registry) SaveProfileToken(id uint, token string) {
	r.DB.Model(&model.Camera{}).Where("id = ?", id).
		Update("onvif_profile_token", token)
}

// UpdateStatus is called by the HealthChecker after each probe.
// Keeping it in the Registry means the persistence path is the same
// whether the caller is the background loop or a manual webhook.
func (r *Registry) UpdateStatus(id uint, status string, seen *time.Time) {
	updates := map[string]any{"status": status, "updated_at": time.Now()}
	if seen != nil {
		updates["last_seen_at"] = seen
	}
	r.DB.Model(&model.Camera{}).Where("id = ?", id).Updates(updates)
}

// BootReplay re-registers every existing camera with the Frigate
// bundled go2rtc and pushes the full config to Frigate. Call this
// from main.go after the Frigate container has had a moment to come
// up. A failure on one camera must not stop the others.
//
// Robustness: the go2rtc API may not be ready the instant its
// container starts. We retry the whole replay pass with backoff so
// a slow-starting Frigate doesn't leave all cameras unregistered.
// Errors are logged, not swallowed silently.
//
// v1.5.14: BootReplay now also performs a one-time migration of
// Capabilities["audio"] from false → true for any camera that was
// registered before v1.5.14 (when audio defaulted to false). The
// migration is idempotent — it only updates cameras whose audio
// flag is currently false/missing. This brings existing cameras in
// line with the new v1.5.14 default (audio=true) so the mobile app
// can hear sound without requiring admins to manually toggle each
// camera's audio switch. The rtspURL function emits #audio=aac
// (transcode path) or #video=copy#audio=aac (passthrough) when
// audio is enabled, so go2rtc receives a source with audio.
func (r *Registry) BootReplay(ctx context.Context) error {
	cams := r.List()
	if len(cams) == 0 {
		return nil
	}

	// v1.5.14: migrate legacy cameras to audio=true. Best-effort:
	// if the DB update fails, we log and continue — the camera
	// still gets replayed with its current (false) audio flag, so
	// the user sees no audio until they manually toggle the audio
	// switch in the dashboard. Non-fatal.
	migrated := 0
	for i := range cams {
		c := &cams[i]
		if !cameraHasAudio(c) {
			if c.Capabilities == nil {
				c.Capabilities = model.JSON{}
			}
			c.Capabilities["audio"] = true
			if err := r.DB.Model(c).Update("capabilities", c.Capabilities).Error; err != nil {
				log.Printf("camera: boot replay: cam %d: failed to migrate audio=true: %v", c.ID, err)
				continue
			}
			migrated++
		}
	}
	if migrated > 0 {
		log.Printf("camera: boot replay: migrated %d camera(s) to audio=true (v1.5.14 default)", migrated)
	}

	const maxAttempts = 5
	baseDelay := 2 * time.Second

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Check go2rtc reachability first so we don't waste time
		// hammering AddStream on a dead endpoint.
		if !r.Go2.Alive(ctx) {
			log.Printf("camera: boot replay attempt %d/%d: go2rtc not reachable, waiting %s",
				attempt, maxAttempts, baseDelay)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(baseDelay):
			}
			baseDelay *= 2
			continue
		}

		var failed int
		for _, c := range cams {
			if c.StreamName == "" {
				continue
			}
			u, p, err := r.DecryptCredentials(&c)
			if err != nil {
				log.Printf("camera: boot replay: cam %d: decrypt credentials: %v", c.ID, err)
				failed++
				continue
			}
			rtspURL := r.rtspURL(&c, u, p)
			if err := r.Go2.AddStream(ctx, c.StreamName, rtspURL); err != nil {
				log.Printf("camera: boot replay: cam %d (%s): add stream: %v", c.ID, c.StreamName, err)
				failed++
				continue
			}
			log.Printf("camera: boot replay: cam %d (%s): stream added", c.ID, c.StreamName)
			// Preheat each stream so the first user request after a
			// container restart doesn't pay the cold-start cost. Best-effort,
			// non-blocking; a slow preheat must not hold up boot replay.
			go func(streamName string) {
				pCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				r.Go2.Preheat(pCtx, streamName)
			}(c.StreamName)
		}

		// Push the full config to Frigate so its AI detection and
		// recording pipelines pick up every camera. Best-effort:
		// if Frigate's REST API is down, the go2rtc streams are
		// still live and video works.
		if r.Frigate != nil {
			if err := r.pushFrigateConfig(ctx); err != nil {
				log.Printf("camera: boot replay: frigate config push (non-fatal): %v", err)
			}
		}

		if failed == 0 {
			log.Printf("camera: boot replay: %d camera(s) registered with go2rtc", len(cams))
			return nil
		}

		log.Printf("camera: boot replay attempt %d/%d: %d/%d failed, retrying in %s",
			attempt, maxAttempts, failed, len(cams), baseDelay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(baseDelay):
		}
		baseDelay *= 2
	}

	return fmt.Errorf("boot replay: go2rtc not ready after %d attempts", maxAttempts)
}

// pushFrigateConfig generates the full Frigate camera config from the
// DB and pushes it via the Frigate REST API. Called after each
// register/unregister and during BootReplay.
//
// The Frigate camera name uses a normalized ASCII slug (because
// Frigate's Pydantic model validates names against a strict regex),
// but the go2rtc stream key is the original friendly name from the
// dashboard. The two are linked by go2rtc.streams[name] — Frigate
// picks up the RTSP URL for each camera by looking up its slug in
// go2rtc.streams.
//
// IMPORTANT: Frigate's ffmpeg.inputs[].path is the path Frigate
// passes to its OWN ffmpeg child process for AI detection — it
// does NOT go through go2rtc. The `ffmpeg:` scheme prefix is
// go2rtc-specific; Frigate treats it as a literal filename and
// fails with "Protocol not found". We therefore send a plain
// rtsp:// URL (with the same `#video=h264#width=1280` ffmpeg
// directives) to Frigate's camera config, and the full
// `ffmpeg:rtsp://...` URL to go2rtc.streams for the streaming
// pipeline. The two URLs share the same credentials and transcode
// options but differ only in scheme.
func (r *Registry) pushFrigateConfig(ctx context.Context) error {
	cams := r.List()
	frigateCams := make([]FrigateCameraConfig, 0, len(cams))
	go2rtcStreams := make(map[string]string)
	for _, c := range cams {
		if c.StreamName == "" {
			continue
		}
		u, p, err := r.DecryptCredentials(&c)
		if err != nil {
			log.Printf("camera: frigate config: cam %d: decrypt: %v", c.ID, err)
			continue
		}
		go2rtcURL := r.rtspURL(&c, u, p)             // ffmpeg:rtsp://...
		frigatePath := r.frigateCameraPath(&c, u, p) // rtsp://...

		// Frigate's name validator: ^[a-zA-Z0-9_-]+$
		slug := slugifyName(c.StreamName)
		frigateCams = append(frigateCams, FrigateCameraConfig{
			Name:    slug,
			Enabled: true,
			Ffmpeg: FrigateFfmpeg{
				Inputs: []FrigateInput{
					{
						Path:  frigatePath,
						Roles: []string{"detect", "record"},
					},
				},
			},
			Detect: FrigateDetect{Enabled: true, FPS: 2},
			Record: FrigateRecord{Enabled: true},
		})
		// go2rtc stream key keeps the original friendly name so
		// the existing stream URLs (e.g. /api/stream.m3u8?src=前门)
		// continue to work.
		go2rtcStreams[c.StreamName] = go2rtcURL
	}
	// requires_restart=true is always passed. Frigate's ffmpeg pipeline
	// only picks up changes to detect.fps, record.enabled, or stream URLs
	// during a restart — a hot-merge (requires_restart=false) returns 200
	// but keeps the old pipeline running. This affects:
	//   - detect.fps changes (detector performance tuning)
	//   - record.enabled changes (recording on/off)
	//   - stream URL changes (camera credentials, codec)
	//
	// A restart causes a brief (~2s) interruption to all streams, which
	// is acceptable for the rare operations that call pushFrigateConfig
	// (boot replay, camera register/unregister). UpdateCodec does NOT
	// call this function — it only updates the go2rtc stream URL via
	// AddStream (hot-reload, no Frigate restart needed).
	return r.Frigate.PushConfig(ctx, frigateCams, go2rtcStreams, true)
}

// frigateCameraPath builds the URL Frigate's OWN ffmpeg child
// process (for AI detection) expects. Unlike go2rtc, Frigate does
// not honour the `ffmpeg:` scheme prefix — it passes the path
// directly to `ffmpeg -i <path>`, so the scheme must be one ffmpeg
// knows natively (`rtsp://` is fine, with the same `#video=h264`
// and `#width=...` directives that go2rtc understands).
//
// Transcode decision:
//   - transcode=true → ffmpeg H.264 720p pipeline
//     (`#video=h264#width=1280`). Universally compatible
//     with Chrome WebRTC, fits the 1Mbps Cloudflare Tunnel
//     link with room to spare (~250-400 kbps output).
//     Frigate's ffmpeg and the streaming layer use the
//     same directive syntax.
//   - transcode=false → raw RTSP, Frigate handles the
//     codec (HEVC, H.264, etc.) directly.
// frigateCameraPath returns the RTSP URL that Frigate's ffmpeg
// connects to. Frigate records the camera's NATIVE stream (it uses
// `-c:v copy` in its record preset) — the codec setting only
// affects go2rtc's live transcode path (see rtspURL). We therefore
// return a plain RTSP URL WITHOUT go2rtc directives like
// `#video=h264` or `#width=1280`: those are go2rtc-specific and
// Frigate's ffmpeg silently ignores them (treats `#...` as a URL
// fragment), so they were harmless but useless. Keeping the URL
// clean avoids confusion about which directives apply where.
func (r *Registry) frigateCameraPath(cam *model.Camera, user, pass string) string {
	return fmt.Sprintf("rtsp://%s:%s@%s:%d/Streaming/Channels/%d",
		user, pass, cam.Host, cam.RTSPPort, cam.ChannelID)
}

// slugifyName converts a human-friendly camera name (which may
// contain Chinese, spaces, or other non-ASCII characters) to an
// ASCII slug that passes Frigate's Pydantic name validator
// (^[a-zA-Z0-9_-]+$).
//
//	"前门"      → "front_door" (well-known map)
//	"Back Yard" → "back_yard"
//	"Camera-1"  → "camera-1" (already valid)
//	"摄像头 02" → "cam_02"
//
// The well-known Chinese map is intentionally small — operators
// can rename cameras in the dashboard to whatever they like; the
// slug only needs to be unique and ASCII-clean. If the result
// collides with an existing slug we append a numeric suffix.
func slugifyName(name string) string {
	// Well-known Chinese → English map. Operators can edit the
	// camera name in the dashboard if they want a different slug.
	cn := map[string]string{
		"前门": "front_door",
		"后门": "back_door",
		"客厅": "living_room",
		"卧室": "bedroom",
		"厨房": "kitchen",
		"院子": "yard",
		"车库": "garage",
	}
	if en, ok := cn[name]; ok {
		return en
	}
	// Generic: keep alnum + _ + -, replace everything else with _,
	// collapse runs of underscores, trim leading/trailing _.
	// v1.5.14: convert ASCII letters to lowercase so "Front Door"
	// and "front door" produce the same slug. The previous version
	// kept the original case, which caused Android's lowercased
	// client-side slugify to never match backend's mixed-case
	// camera_slug field — alert overlay stayed empty.
	var b strings.Builder
	prevUnderscore := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			prevUnderscore = false
		case r >= 'A' && r <= 'Z':
			// v1.5.14: lowercase ASCII letters.
			b.WriteRune(r + ('a' - 'A'))
			prevUnderscore = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			prevUnderscore = false
		case r == '_' || r == '-':
			b.WriteRune('_')
			prevUnderscore = false
		default:
			if !prevUnderscore && b.Len() > 0 {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		// v1.5.14: pure non-ASCII names (e.g. "前门摄像头", "室内监控")
		// used to fall back to the literal string "camera" — every
		// such camera collided on the same Frigate camera key,
		// causing pushFrigateConfig to silently overwrite earlier
		// cameras and LookupByFrigateSlug to return the wrong
		// camera ID. Use a stable hash of the original name so each
		// camera gets a unique, reproducible slug.
		h := sha256.Sum256([]byte(name))
		out = "cam_" + hex.EncodeToString(h[:4]) // 8 hex chars
	}
	return out
}

// --- credential helpers (also used by the ONVIF controller) ---

// rtspURL builds the canonical Hikvision-style URL:
//
//	rtsp://<user>:<pass>@<host>:<port>/Streaming/Channels/<channel>
//
// Most Dahua / Uniview / Ezviz devices accept the same shape; for
// vendors that diverge (Reolink, TP-Link) we add per-vendor paths
// later. For now this is the 90% case.
//
// IMPORTANT: we deliberately do NOT use net/url's UserPassword
// helper here. Go's url.UserPassword percent-encodes reserved chars
// in the userinfo (e.g. "@" → "%40"), which is standards-correct —
// but go2rtc's RTSP client does NOT URL-decode the password before
// sending it to the camera. So a password "Haikang@" becomes
// "Haikang%40" on the wire, and the camera rejects it with
// "wrong user/pass". Building the URL as a plain string with the
// raw password avoids this. The go2rtc URL parser splits at the
// last "@" before the host, so a password containing "@" (e.g.
// "Haikang@") produces "rtsp://admin:Haikang@@host..." which
// go2rtc parses correctly as user=admin, pass=Haikang@.
//
// Audio handling: the platform's HLS path defaults to dropping
// audio at the source. Camera audio codecs (G726 / PCMU /
// MPEG4-GENERIC) are not browser-decodable, and transcoding them
// would force us to keep a server-side transcoder installed in
// the go2rtc image (see deploy/go2rtc/Dockerfile). "#audio=0" is
// a go2rtc directive that just skips the audio track without
// requiring any transcoder. If the operator wants browser audio
// they can append "#audio=opus" later, but the default is silent.
//
// Video handling: by default the source's native video codec is
// passed through (HEVC stays HEVC, H.264 stays H.264). When
// `cam.Transcode` is true, the registry routes the source through
// go2rtc's `ffmpeg:` exec pipeline (`ffmpeg:rtsp://...#video=h264`),
// which spawns an ffmpeg process that transcodes the camera's
// native codec (typically H.265 on Hikvision) to H.264. This is
// the only escape for HEVC cameras on browsers whose WebRTC RTP
// codec registry does not include H.265 (Chrome / Edge / Android
// WebView — see docs/platformization.md for the matrix).
//
// The `ffmpeg:` scheme prefix is REQUIRED — the bare rtsp:// scheme
// has no transcode path, and a `#video=h264` fragment on a plain
// rtsp:// URL is silently ignored (the rtsp producer just connects
// to the camera and reports whatever codecs the camera advertises
// in its SDP). go2rtc's `ffmpeg:` scheme redirects through its
// internal parseArgs → exec: pipeline (see
// build-host/go2rtc/internal/ffmpeg/ffmpeg.go streams.RedirectFunc),
// so the URL must look like `ffmpeg:rtsp://...#video=h264` for
// transcoding to actually happen.
//
// rtspURL is the canonical RTSP source go2rtc pulls from.
//
// Audio policy: by default we strip audio (`#audio=0` on passthrough,
// no `audio=` directive on the ffmpeg path so go2rtc injects `-an`).
// The home dashboard never plays sound; the only consumer that
// benefits is the mobile app. When the camera has
// `Capabilities["audio"]==true` (set at registration time) we opt in
// to audio:
//   - Passthrough path: switch to `ffmpeg:rtsp://...#video=copy#audio=aac`
//     so the camera's native video codec is preserved and PCMA is
//     transcoded to AAC (universally browser- and Android-decodable).
//   - Transcode path: append `#audio=aac` to the existing
//     `ffmpeg:rtsp://...#video=h264...` URL — ffmpeg encodes audio
//     alongside the video transcode at ~96 kbps, a negligible cost
//     compared to the video bitrate.
//
// `cam.Transcode` opts the camera into ffmpeg-backed H.264
// transcoding, which is required for HEVC sources on browsers
// whose WebRTC RTP registry does not include H.265
// (Chrome / Edge / Android WebView — see
// docs/platformization.md for the matrix).
//
// IMPORTANT: go2rtc's RTSP scheme does NOT honour the
// `#video=h264` fragment on its own — that fragment is a
// directive for the `ffmpeg:` scheme handler (see
// build-host/go2rtc/internal/ffmpeg/ffmpeg.go
// streams.RedirectFunc + parseArgs). We therefore prefix the
// URL with `ffmpeg:` when transcode=true, which routes it
// through go2rtc's exec pipeline. The native H.264 path
// stays on the rtsp:// scheme with just `#audio=0`.
//
// Fragment form: `ffmpeg:rtsp://...#video=h264`
// — we deliberately do NOT add `#audio=...` to the ffmpeg
// URL when audio is disabled. go2rtc's parseArgs adds `-an`
// automatically when `query["audio"]` is empty, and any
// non-empty value (e.g. "0", "anull") is fed straight to
// ffmpeg as a raw codec arg, which produces a malformed
// command line. The Hikvision audio (PCMA) is not
// browser-decodable as raw PCMA, so when audio is enabled we
// transcode to AAC.
// effectiveCodec resolves the codec choice from the Codec field
// (source of truth when non-empty) or the legacy Transcode bool.
// Returns one of "passthrough", "h264", "h265".
func effectiveCodec(cam *model.Camera) string {
	if cam.Codec != "" {
		if cam.Codec == "passthrough" {
			return "passthrough"
		}
		return cam.Codec // "h264" or "h265"
	}
	// Legacy: Transcode bool
	if cam.Transcode {
		return "h264"
	}
	return "passthrough"
}

// cameraHasAudio reports whether the camera was registered with
// audio capability. The flag is stored as a generic JSON value in
// Capabilities, so we tolerate bool / numeric / string forms
// defensively (any non-empty truthy value counts).
func cameraHasAudio(cam *model.Camera) bool {
	if cam.Capabilities == nil {
		return false
	}
	v, ok := cam.Capabilities["audio"]
	if !ok || v == nil {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case int:
		return t != 0
	case string:
		return t != "" && t != "false" && t != "0"
	}
	return false
}

func (r *Registry) rtspURL(cam *model.Camera, user, pass string) string {
	raw := fmt.Sprintf("rtsp://%s:%s@%s:%d/Streaming/Channels/%d",
		user, pass, cam.Host, cam.RTSPPort, cam.ChannelID)
	codec := effectiveCodec(cam)
	audioOn := cameraHasAudio(cam)
	if codec == "passthrough" {
		if audioOn {
			// Audio requested but we're on the passthrough path —
			// the rtsp:// scheme cannot transcode PCMA to AAC, so
			// switch to ffmpeg with video=copy (passthrough video)
			// + audio=aac (transcode audio only). Adds ~5% CPU
			// for the AAC encoder but preserves the camera's
			// native video codec (no quality loss).
			return "ffmpeg:" + raw + "#video=copy#audio=aac"
		}
		// Native path: no ffmpeg, no transcode. Camera
		// delivers whatever codec it has (H.264 / H.265)
		// and we hope the consumer supports it. HLS
		// always works (hls.js transcodes on the fly
		// via Media Source Extensions for H.265 on
		// supporting browsers), WebRTC only works for
		// H.264 in Chrome. The `audio=0` directive
		// tells go2rtc to drop the camera's PCMA track
		// from the SDP — go2rtc exposes a PCMU/PCMA
		// audio track that the browser cannot decode.
		return raw + "#audio=0"
	}
	// Transcode path: route through go2rtc's ffmpeg
	// pipeline. `video=<codec>` selects a go2rtc ffmpeg
	// preset (h264: H.264 high@4.1 superfast/zerolatency;
	// h265: libx265). When audio is enabled we append
	// `#audio=aac` so ffmpeg encodes the camera's PCMA
	// audio to AAC alongside the video transcode. Without
	// the audio directive, parseArgs injects `-an` so
	// ffmpeg drops the camera's PCMA track entirely.
	//
	// `width=1280` downscales to 720p for h264 (bandwidth
	// optimization for Cloudflare Tunnel). h265 keeps
	// native resolution since it's used for high-quality
	// HLS recording, not live WebRTC.
	//
	// `hardware=vaapi` tells go2rtc to use Intel VAAPI for
	// decoding the camera's native H.265 stream on the GPU
	// instead of software decoding on CPU. go2rtc expands
	// this to `-hwaccel vaapi -hwaccel_output_format vaapi`
	// before the -i flag. Requires /dev/dri mounted in the
	// Frigate container (see compose.yaml). On hosts without
	// an Intel GPU, omit this directive to fall back to
	// software decode.
	audioFrag := ""
	if audioOn {
		audioFrag = "#audio=aac"
	}
	if codec == "h265" {
		return "ffmpeg:" + raw + "#video=h265#hardware=vaapi" + audioFrag
	}
	return "ffmpeg:" + raw + "#video=h264#width=1280#hardware=vaapi" + audioFrag
}

// boxCredentials encrypts the user/pass pair and packages them into
// a JSON blob the model stores as a single TEXT column.
func (r *Registry) boxCredentials(user, pass string) (model.JSON, error) {
	eu, err := r.Box.Encrypt(user)
	if err != nil {
		return nil, err
	}
	ep, err := r.Box.Encrypt(pass)
	if err != nil {
		return nil, err
	}
	return model.JSON{"onvif_user": eu, "onvif_pass": ep}, nil
}

// DecryptCredentials returns the plaintext user/pass for the
// supplied camera. The caller is expected to use them in-process
// and not log or persist them.
func (r *Registry) DecryptCredentials(c *model.Camera) (user, pass string, err error) {
	if c.Credentials == nil {
		return "", "", fmt.Errorf("camera %d: no credentials", c.ID)
	}
	eu, _ := c.Credentials["onvif_user"].(string)
	ep, _ := c.Credentials["onvif_pass"].(string)
	if user, err = r.Box.Decrypt(eu); err != nil {
		return "", "", err
	}
	if pass, err = r.Box.Decrypt(ep); err != nil {
		return "", "", err
	}
	return user, pass, nil
}

// StreamConfig is the small helper for the handler layer: it
// returns a JSON-safe struct describing the URLs the front-end
// should hit for live view.
type StreamConfig struct {
	StreamName string `json:"stream_name"`
	WebRTC     string `json:"webrtc_url"`
	HLS        string `json:"hls_url"`
}

func (r *Registry) StreamConfig(c *model.Camera) StreamConfig {
	// Bug2 fix: friendly names are usually non-ASCII ("前门",
	// "后院#1", etc.) and must be URL-escaped before being placed
	// in a query string. go2rtc's HTTP API percent-decodes the
	// `src` parameter, so a literal "前门" in the URL would be
	// interpreted as a path-mangled name on some proxies and
	// returned as 404 "stream not found". Always pre-escape here
	// — both the public-base branch and the in-network branch.
	enc := url.QueryEscape(c.StreamName)
	// If a public base is configured (tunnel / TURN), rewrite both
	// URLs to it. Otherwise return the in-network addresses.
	//
	// Both branches append `&mp4=` to the HLS URL: this is go2rtc's
	// switch to fragmented-MP4 (segment.m4s) instead of the default
	// MPEG-TS (segment.ts) container. hls.js's TS demuxer has weak
	// HEVC support and silently drops frames — the browser's MSE
	// receives data, the decoder produces nothing, `<video>` never
	// fires `playing`, and the front-end's stall watchdog eventually
	// reports "HLS stream stalled" with go2rtc falsely implicated.
	// fMP4 sidesteps the demuxer problem and is the recommended
	// container for HEVC over HLS. See go2rtc/internal/hls/hls.go:
	// `mp4.ParseQuery(r.URL.Query())` chooses between mp4.NewConsumer
	// and mpegts.NewConsumer based on the presence of `mp4` in the
	// query string. The `&mp4=` value matches the upstream "legacy"
	// media set (H.264+H.265 video, AAC audio).
	if r.WebRTCURL != "" {
		base := strings.TrimRight(r.WebRTCURL, "/")
		return StreamConfig{
			StreamName: c.StreamName,
			WebRTC:     base + "/api/webrtc?src=" + enc,
			HLS:        base + "/api/stream.m3u8?src=" + enc + "&mp4=",
		}
	}
	return StreamConfig{
		StreamName: c.StreamName,
		WebRTC:     r.Go2.WebRTCURL(c.StreamName),
		HLS:        r.Go2.HLSURL(c.StreamName),
	}
}

// itoa is a tiny convenience so callers don't need to import strconv
// just to format a probe target. Kept private; package users can keep
// using strconv.
func itoa(i int) string { return strconv.Itoa(i) }
