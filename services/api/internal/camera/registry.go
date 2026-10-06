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
	// StopTimeout is the go2rtc producer idle timeout in seconds.
	// When no consumer is watching, go2rtc keeps the RTSP source
	// connection alive for this many seconds before tearing it down.
	// Default 30. Increase for environments with flaky RTSP where
	// frequent reconnections cause HLS stalls. v1.9.x bumped to 120 so
	// the splash-screen preheat keeps producers warm long enough for a
	// normal login → live-view navigation to hit a hot source without
	// paying the 1-2s cold-start.
	StopTimeout int
	Cloud       *CloudArchiveClient
}

func NewRegistry(db *gorm.DB, g *Go2RTCClient, fr *FrigateClient, box *utils.SecretBox, onvif *ONVIFController, webRTCURL string) *Registry {
	return &Registry{
		DB:          db,
		Go2:         g,
		Frigate:     fr,
		Box:         box,
		ONVIF:       onvif,
		WebRTCURL:   webRTCURL,
		StopTimeout: 600,
		Cloud:       NewCloudArchiveClient(),
	}
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

	// Auto-codec detection (v1.9.x): when the operator did not explicitly
	// choose a codec or transcode, probe the camera's native video codec.
	// An HEVC/H.265 camera would otherwise stream H.265 passthrough
	// (`#video=copy`), which breaks go2rtc's frame-grab (ffmpeg exit 183 →
	// HTTP 500) and WebRTC live view (Chrome has no H.265 codec). Routing
	// it to the h264 transcode pipeline transparently makes it usable.
	// H.264 cameras keep the cheap passthrough path. Best-effort: if the
	// probe fails (camera offline, ffprobe missing) we keep the requested
	// default rather than fail the registration.
	if in.Codec == "" && !in.Transcode {
		raw := fmt.Sprintf("rtsp://%s:%s@%s:%d/Streaming/Channels/%d",
			in.Username, in.Password, in.Host, in.RTSPPort, in.ChannelID)
		probed := probeVideoCodec(ctx, raw)
		if codec, trans := codecFromProbe(probed); trans {
			in.Codec, in.Transcode = codec, true
			log.Printf("camera: %q: auto-transcode to h264 (native codec %q)", name, probed)
		} else if probed != "" {
			log.Printf("camera: %q: native codec %q, keeping passthrough", name, probed)
		} else {
			log.Printf("camera: %q: codec probe failed, keeping passthrough", name)
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
	// Native-HEVC camera gets a second zero-transcode passthrough live
	// stream (<name>_hevc) so HEVC-capable browsers can skip the J4125
	// transcode. Best-effort — a failure falls back to H.264.
	r.add1080pStream(ctx, cam, in.Username, in.Password)
	r.addHEVCStream(ctx, cam, in.Username, in.Password)

	// Preheat the go2rtc stream: force an RTSP source connection now
	// so the operator's first frame doesn't pay the 1-10s cold-start
	// latency. Synchronous as of v1.7.3 — registering returns only
	// after the stream is warmed, so a freshly registered camera is
	// immediately watchable (previously the fire-and-forget goroutine
	// raced the first view request and caused cold-start playback
	// failures). Preheat failures are logged inside Preheat and are
	// not surfaced here: a failed warm-up just means the first user
	// request warms the source instead.
	preheatCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r.Go2.Preheat(preheatCtx, cam.StreamName)
	r.preheatHEVCStream(cam)

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

	// If profile_token discovery failed during registration (ONVIF
	// service temporarily unavailable, camera still booting, network
	// settling after IP change), start a background retry loop. We
	// retry every 30s for up to 10 minutes, persisting the token on
	// first success. Without this, cameras registered during a
	// transient ONVIF failure would permanently lack a profile_token,
	// breaking PTZ and stream management until manually re-registered.
	if profile == "" && r.ONVIF != nil {
		go r.retryProfileDiscovery(cam.ID, in.Host, in.ONVIFPort, in.Username, in.Password)
	}

	if err := r.DB.Model(cam).Updates(map[string]any{
		"updated_at": time.Now(),
	}).Error; err != nil {
		return nil, err
	}
	return cam, nil
}

// retryProfileDiscovery attempts to discover the ONVIF profile_token
// for a camera that was registered without one. It retries every 30s
// for up to 10 minutes (20 attempts). On first success it persists
// the token to the database and logs the recovery. This is a
// fire-and-forget goroutine started by Register when the initial
// DiscoverProfiles call fails.
func (r *Registry) retryProfileDiscovery(camID uint, host string, onvifPort int, user, pass string) {
	const maxAttempts = 20
	const interval = 30 * time.Second

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		time.Sleep(interval)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ps, err := r.ONVIF.DiscoverProfiles(ctx, host, onvifPort, user, pass)
		cancel()

		if err == nil && len(ps) > 0 {
			if err := r.DB.Model(&model.Camera{}).Where("id = ?", camID).
				Update("onvif_profile_token", ps[0].Token).Error; err != nil {
				log.Printf("camera: profile retry cam %d: discovered token but failed to persist: %v", camID, err)
				return
			}
			log.Printf("camera: profile retry cam %d: discovered profile_token=%s on attempt %d/%d",
				camID, ps[0].Token, attempt, maxAttempts)
			return
		}

		log.Printf("camera: profile retry cam %d: attempt %d/%d failed: %v",
			camID, attempt, maxAttempts, err)
	}
	log.Printf("camera: profile retry cam %d: exhausted %d attempts, giving up", camID, maxAttempts)
}

// Unregister removes the row and asks go2rtc to drop the stream.
// go2rtc errors are logged but not returned — the DB is the source
// of truth and we don't want a half-deleted camera. The Frigate config
// is also re-pushed so Frigate drops the camera from its detection
// pipeline.
//
// Physical delete (Unscoped().Delete) is used instead of GORM's default
// soft delete. The cameras table has a UNIQUE index on stream_name, and
// a soft-deleted row still occupies that index — re-registering a camera
// with the same friendly name (e.g. re-adding "前门" after deleting it)
// would hit the UNIQUE constraint and surface to the operator as a
// 409 Conflict. Hard-deleting the row frees the stream_name slot so
// the same name can be reused immediately. The go2rtc RemoveStream call
// and the Frigate pushFrigateConfig re-push below are unaffected: they
// key off the in-memory cam.StreamName / DB List() (which already
// filters out soft-deleted rows), not off the DB row's existence.
func (r *Registry) Unregister(ctx context.Context, id uint) error {
	var cam model.Camera
	if err := r.DB.First(&cam, id).Error; err != nil {
		return err
	}
	// Compute the unique Frigate slug BEFORE the hard delete below:
	// FrigateSlugUnique resolves against the current camera set, and
	// once the row is gone the fallback would return the base slug
	// (wrong for cameras whose slug carried a _N suffix).
	slug := r.FrigateSlugUnique(&cam)
	if cam.StreamName != "" {
		_ = r.Go2.RemoveStream(ctx, cam.StreamName)
		_ = r.Go2.RemoveStream(ctx, cam.StreamName+"_1080p")
		// Drop the native-HEVC passthrough companion stream too
		// (best-effort; a 404 when it never existed is fine).
		_ = r.Go2.RemoveStream(ctx, hevcStreamName(cam.StreamName))
	}
	if err := r.DB.Unscoped().Delete(&cam).Error; err != nil {
		return err
	}
	// Best-effort cleanup of all camera-associated data. Each step
	// logs on failure but never blocks the delete flow.
	r.cleanupCameraShares(id)
	r.cleanupFrigateRecordings(&cam, slug)
	r.cleanupFrigateEvents(ctx, &cam, slug)
	if r.Frigate != nil {
		if err := r.pushFrigateConfig(ctx); err != nil {
			log.Printf("camera: unregister: frigate config push (non-fatal): %v", err)
		}
	}
	return nil
}

// cleanupCameraShares removes every camera_shares row referencing the
// camera. Best-effort: failures are logged, never returned.
func (r *Registry) cleanupCameraShares(cameraID uint) {
	if err := r.DB.Where("camera_id = ?", cameraID).Delete(&model.CameraShare{}).Error; err != nil {
		log.Printf("camera: unregister: cam %d: delete shares: %v", cameraID, err)
	}
}

// cleanupFrigateRecordings removes the camera's on-disk recording
// directory at /media/frigate/recordings/YYYY-MM-DD/HH/<slug> for
// every date/hour combination present. Best-effort: missing
// directories are not errors, failures are logged.
func (r *Registry) cleanupFrigateRecordings(cam *model.Camera, slug string) {
	if slug == "" {
		return
	}
	root := "/media/frigate/recordings"
	dateEntries, err := os.ReadDir(root)
	if err != nil {
		log.Printf("camera: unregister: cam %d: read recordings root: %v", cam.ID, err)
		return
	}
	for _, dateEntry := range dateEntries {
		if !dateEntry.IsDir() {
			continue
		}
		hourEntries, err := os.ReadDir(root + "/" + dateEntry.Name())
		if err != nil {
			continue
		}
		for _, hourEntry := range hourEntries {
			if !hourEntry.IsDir() {
				continue
			}
			slugDir := fmt.Sprintf("%s/%s/%s/%s", root, dateEntry.Name(), hourEntry.Name(), slug)
			if err := os.RemoveAll(slugDir); err != nil {
				log.Printf("camera: unregister: cam %d: remove %s: %v", cam.ID, slugDir, err)
			}
		}
	}
}

// cleanupFrigateEvents deletes the camera's detection events from
// Frigate via the REST API. Best-effort: failures are logged, never
// returned. Events are listed in pages of 100 (newest-first) and
// deleted as they are found; the loop stops once Frigate returns
// fewer than 100 events or a full page contained nothing to delete.
func (r *Registry) cleanupFrigateEvents(ctx context.Context, cam *model.Camera, slug string) {
	if r.Frigate == nil || slug == "" {
		return
	}
	for {
		events, err := r.Frigate.ListEvents(ctx, 100, false)
		if err != nil {
			log.Printf("camera: unregister: cam %d: list frigate events: %v", cam.ID, err)
			return
		}
		deleted := 0
		for _, ev := range events {
			if ev.Camera != slug {
				continue
			}
			if err := r.Frigate.DeleteEvent(ctx, ev.ID); err != nil {
				log.Printf("camera: unregister: cam %d: delete frigate event %s: %v", cam.ID, ev.ID, err)
				continue
			}
			deleted++
		}
		if len(events) < 100 || deleted == 0 {
			return
		}
	}
}

// CleanupSoftDeleted purges any soft-deleted camera rows left over
// from earlier deployments where Unregister performed a soft delete.
// Those rows still occupy the stream_name UNIQUE index, blocking
// re-registration of the same friendly name with a 409 Conflict.
// Call this once during startup, before BootReplay/pushFrigateConfig,
// so the Frigate config push sees a clean DB and stream_name reuse
// works immediately. Safe to call when there is nothing to clean —
// it is a no-op that returns nil with RowsAffected=0.
func (r *Registry) CleanupSoftDeleted() error {
	res := r.DB.Unscoped().Where("deleted_at IS NOT NULL").Delete(&model.Camera{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected > 0 {
		log.Printf("camera: cleanup: purged %d soft-deleted camera row(s)", res.RowsAffected)
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

// PreheatStream triggers a best-effort go2rtc preheat for the given
// camera: forces go2rtc to connect to the RTSP source (and start any
// transcoder) before the first real client request arrives, so the
// first WebRTC SDP or HLS request doesn't pay the 1-10s cold-start
// latency.
//
// Non-blocking — the preheat runs in a detached goroutine (with a
// 15s timeout context, matching Register/BootReplay) so the caller
// returns immediately. The goroutine's errors are logged but not
// propagated: preheat is an optimization, so a failure just means
// the first user request warms the source instead.
//
// A lookup failure (camera not found / no stream name) is returned
// as an error so the handler can decide what to do — but the Preheat
// handler ignores it (best-effort, 200 either way).
func (r *Registry) PreheatStream(cameraID uint) error {
	cam, err := r.Get(cameraID)
	if err != nil {
		return err
	}
	if cam.StreamName == "" {
		return fmt.Errorf("camera %d has no stream name", cameraID)
	}
	streamName := cam.StreamName
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		r.Go2.Preheat(ctx, streamName)
	}()
	return nil
}

// LookupByFrigateSlug resolves a Frigate camera slug (e.g.
// "front_door") back to a home-api camera ID. It computes each
// camera's unique Frigate slug (same algorithm as pushFrigateConfig)
// until it finds a match. Returns (0, false) if no camera matches.
func (r *Registry) LookupByFrigateSlug(slug string) (uint, bool) {
	for id, s := range r.computeUniqueSlugs() {
		if s == slug {
			return id, true
		}
	}
	return 0, false
}

// LookupCameraName resolves a camera ID to its display name.
func (r *Registry) LookupCameraName(id uint) string {
	if id == 0 {
		return ""
	}
	var cam model.Camera
	if err := r.DB.Select("name").First(&cam, id).Error; err == nil {
		return cam.Name
	}
	return ""
}

// LookupFrigateSlugByCameraID resolves a home-api camera ID to its unique
// Frigate slug for query filtering.
func (r *Registry) LookupFrigateSlugByCameraID(id uint) (string, bool) {
	slug, ok := r.computeUniqueSlugs()[id]
	return slug, ok
}

// AllFrigateSlugs returns all unique Frigate slugs currently mapped.
func (r *Registry) AllFrigateSlugs() []string {
	slugsMap := r.computeUniqueSlugs()
	out := make([]string, 0, len(slugsMap))
	for _, s := range slugsMap {
		out = append(out, s)
	}
	return out
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
		r.add1080pStream(ctx, &cam, user, pass)
		r.addHEVCStream(ctx, &cam, user, pass)
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
		if r.Go2 != nil {
			_ = r.Go2.RemoveStream(ctx, cam.StreamName)
			_ = r.Go2.RemoveStream(ctx, cam.StreamName+"_1080p")
			_ = r.Go2.RemoveStream(ctx, hevcStreamName(cam.StreamName))
			rtspURL := r.rtspURL(&cam, user, pass)
			if cameraHasTwoWayAudio(&cam) {
				_ = r.Go2.AddStreamSources(ctx, cam.StreamName, []string{
					rtspURL,
					r.rtspBackchannelURL(&cam, user, pass),
				})
			} else {
				_ = r.Go2.AddStream(ctx, cam.StreamName, rtspURL)
			}
			r.add1080pStream(ctx, &cam, user, pass)
			r.addHEVCStream(ctx, &cam, user, pass)
		}
		go SetCameraMicEnabled(ctx, cam.Host, cam.ONVIFPort, user, pass, enabled)
	}
	if r.Frigate != nil {
		if err := r.pushFrigateConfig(ctx); err != nil {
			log.Printf("camera: update audio: push frigate config (non-fatal): %v", err)
		}
	}
	return nil
}

// CameraDetectFPS returns the configured AI detection sampling rate in fps (1-10, default 2).
func CameraDetectFPS(c *model.Camera) int {
	if c.Meta != nil {
		if raw, ok := c.Meta["detect_fps"]; ok {
			switch v := raw.(type) {
			case float64:
				if v >= 1 && v <= 10 {
					return int(v)
				}
			case int:
				if v >= 1 && v <= 10 {
					return v
				}
			}
		}
	}
	return 2
}

// UpdateDetectFPS updates the camera's AI detection sample rate in Frigate (1-10 fps).
func (r *Registry) UpdateDetectFPS(ctx context.Context, id uint, fps int) (*model.Camera, error) {
	if fps < 1 || fps > 10 {
		return nil, fmt.Errorf("detect fps must be between 1 and 10")
	}
	var cam model.Camera
	if err := r.DB.First(&cam, id).Error; err != nil {
		return nil, err
	}
	if cam.Meta == nil {
		cam.Meta = model.JSON{}
	}
	cam.Meta["detect_fps"] = fps
	if err := r.DB.Model(&cam).Updates(map[string]any{
		"meta":       cam.Meta,
		"updated_at": time.Now(),
	}).Error; err != nil {
		return nil, err
	}

	// Push config to Frigate with restart to apply new pipeline fps
	if err := r.pushFrigateConfig(ctx); err != nil {
		log.Printf("camera: update detect fps to %d for cam %d push config error: %v", fps, id, err)
	}

	return &cam, nil
}

func (r *Registry) List() []model.Camera {
	var cs []model.Camera
	r.DB.Find(&cs)
	return cs
}

// FindByFrigateCamera looks up a camera by its Frigate slug name.
// Frigate uses ASCII slugs (via slugifyName + uniqueSlug) while our
// StreamName keeps the original friendly name. This method iterates
// all cameras and matches the unique slugified name.
func (r *Registry) FindByFrigateCamera(frigateName string) (*model.Camera, error) {
	cams := r.List()
	slugs := r.computeUniqueSlugs()
	for i := range cams {
		if slugs[cams[i].ID] == frigateName {
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
	slugs := r.computeUniqueSlugs()
	frigateCams := make([]FrigateCameraConfig, 0, len(cams))
	go2rtcStreams := make(map[string]any)
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
		slug := slugs[c.ID]

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

		detectEnabled := c.Status != "offline"
		var sec model.SecurityState
		if r.DB != nil && r.DB.First(&sec).Error == nil && sec.Mode == model.GuardModeDisarmed {
			detectEnabled = false
		}
		recordOutputArgs := "preset-record-generic-audio-aac"
		if !cameraHasAudio(&c) {
			recordOutputArgs = "preset-record-generic"
		}
		frigateCams = append(frigateCams, FrigateCameraConfig{
			Name:    slug,
			Enabled: c.Status != "offline",
			Ffmpeg: FrigateFfmpeg{
				Inputs: r.frigateInputs(&c, u, p),
				OutputArgs: map[string]string{
					"record": recordOutputArgs,
				},
			},
			Detect: FrigateDetect{Enabled: detectEnabled, FPS: CameraDetectFPS(&c)},
			Record: FrigateRecord{Enabled: recEnabled && c.Status != "offline"},
		})
		if cameraHasTwoWayAudio(&c) {
			go2rtcStreams[c.StreamName] = []string{
				go2rtcURL,
				r.rtspBackchannelURL(&c, u, p),
			}
			go BoostSpeakerVolume(ctx, c.Host, c.ONVIFPort, u, p)
		} else {
			go2rtcStreams[c.StreamName] = go2rtcURL
		}
		if cameraIsNativeHEVC(&c) {
			go2rtcStreams[hevcStreamName(c.StreamName)] = r.hevcPassthroughURL(&c, u, p)
		}
	}
	// requires_restart=true when enabling recording so Frigate
	// starts the recording ffmpeg pipeline. Without a restart the
	// config push returns 200 but no recordings are produced.
	return r.Frigate.PushConfig(ctx, frigateCams, go2rtcStreams, enabled)
}

// FrigateSlug returns the ASCII slug Frigate uses for this camera.
// Kept for backward compatibility; it delegates to FrigateSlugUnique
// so recording path lookups and config pushes always agree on the
// same (unique) slug.
func (r *Registry) FrigateSlug(cam *model.Camera) string {
	return r.FrigateSlugUnique(cam)
}

// FrigateSlugUnique returns the unique Frigate slug for this camera,
// computed against the current set of cameras in the DB with the same
// algorithm pushFrigateConfig uses (slugifyName + uniqueSlug). Falls
// back to the plain slugifyName result when the camera is not in the
// DB (e.g. after its row was deleted).
func (r *Registry) FrigateSlugUnique(cam *model.Camera) string {
	if cam == nil {
		return ""
	}
	if slug, ok := r.computeUniqueSlugs()[cam.ID]; ok {
		return slug
	}
	return slugifyName(cam.StreamName)
}

// computeUniqueSlugs computes the unique Frigate slug for every
// camera currently in the DB, using the same algorithm as
// pushFrigateConfig: iterate cameras in List() order and assign each
// one slugifyName(StreamName) uniquified against the slugs already
// taken. Returns a map of camera ID → unique slug.
func (r *Registry) computeUniqueSlugs() map[uint]string {
	cams := r.List()
	taken := make(map[string]bool, len(cams))
	slugs := make(map[uint]string, len(cams))
	for i := range cams {
		c := &cams[i]
		if c.StreamName == "" {
			continue
		}
		slug := uniqueSlug(slugifyName(c.StreamName), taken)
		taken[slug] = true
		slugs[c.ID] = slug
	}
	return slugs
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
	slug := r.FrigateSlugUnique(cam)
	// Frigate's recording directory layout is
	// /media/frigate/recordings/YYYY-MM-DD/HH/<cam>/MM.SS.mp4 where
	// the timestamp components are in UTC — verified by inspecting
	// the Birth time of /recordings/2026-07-20/04/ which was created
	// at 2026-07-20 12:00 LOCAL CST (= 04:00 UTC). So even though the
	// Frigate container runs with TZ=Asia/Shanghai, it names recording
	// directories with UTC date/hour components. The previous v1.5.15
	// change to use .In(Asia/Shanghai) was wrong — it shifted the
	// path lookup +8h, pointing at non-existent directories and
	// causing every PlayRecording request to 404.
	t := time.Unix(minuteStart, 0).UTC()
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

// RecordingMinute is a single aggregated minute of recordings on disk.
// The ID is the unix-second timestamp of the minute's start (floor to 60s),
// which the front-end uses to build play URLs (/recordings/<id>/file).
type RecordingMinute struct {
	StartUnix    int64  `json:"start_unix"`
	EndUnix      int64  `json:"end_unix"`
	SegmentCount int    `json:"segment_count"`
	Storage      string `json:"storage"` // "local" or "cloud"
}

// ListRecordingMinutesFromDisk walks Frigate's on-disk recording
// directory and aggregates 10s MP4 segment files into 60s buckets
// keyed by minute-start (unix seconds, floor to 60).
//
// Why this exists: Frigate 0.17's /api/<cam>/recordings endpoint has
// an internal limit of ~500 segments (verified by probing with
// after=1h, 6h, 24h, 7d — all return exactly 503 segments, ~83
// minutes). For a home-surveillance app that needs to show 7 days
// of history, this is unusable — the user only sees the last ~80
// minutes of recordings. Disk has the full retention (verified:
// 18233 mp4 files across 3 days when record.continuous.days=7).
//
// Layout walked:
//
//	/media/frigate/recordings/YYYY-MM-DD/HH/<slug>/MM.SS.mp4
//
// YYYY-MM-DD and HH are UTC (verified — see RecordingSegmentsForMinute
// comment). MM.SS is the minute and second within that UTC hour. We
// parse each filename's MM.SS back to a unix-second timestamp by
// combining it with the YYYY-MM-DD/HH directory components, then floor
// to 60s for the bucket key.
//
// `afterUnix` and `beforeUnix` are unix-second bounds (inclusive).
// Pass 0 to skip the bound. The walk still honors disk retention —
// Frigate's cleanup process deletes files older than
// record.continuous.days, so we don't need to re-filter by age here.
//
// Returns buckets sorted newest-first (descending start_unix),
// matching the order the Frigate API returned, so the handler can
// drop-in replace the API call.
func (r *Registry) ListRecordingMinutesFromDisk(cam *model.Camera, afterUnix, beforeUnix int64) ([]RecordingMinute, error) {
	slug := r.FrigateSlugUnique(cam)
	root := "/media/frigate/recordings"

	// Bucket aggregation: map minute-start-unix -> aggregate.
	type bucket struct {
		startUnix int64
		endUnix   int64
		count     int
	}
	buckets := make(map[int64]*bucket)

	// Layout: YYYY-MM-DD/HH/<slug>/MM.SS.mp4
	// Walk the top-level date dirs. We could glob directly but
	// os.ReadDir is cheap and lets us skip non-existent slugs
	// without touching every file.
	dateEntries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read recordings root: %w", err)
	}
	for _, dateEntry := range dateEntries {
		if !dateEntry.IsDir() {
			continue
		}
		dateStr := dateEntry.Name() // "2026-07-20"
		// Quick filter: skip dates entirely outside [after, before]
		// when both bounds are set. Parse as UTC midnight.
		dateStart, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			continue // not a date dir (e.g. "lost+found")
		}
		dateStartUnix := dateStart.Unix()
		dateEndUnix := dateStartUnix + 24*3600
		if afterUnix > 0 && dateEndUnix < afterUnix {
			continue
		}
		if beforeUnix > 0 && dateStartUnix > beforeUnix {
			continue
		}

		hourEntries, err := os.ReadDir(root + "/" + dateStr)
		if err != nil {
			continue
		}
		for _, hourEntry := range hourEntries {
			if !hourEntry.IsDir() {
				continue
			}
			hourStr := hourEntry.Name() // "09"
			hour, err := strconv.Atoi(hourStr)
			if err != nil || hour < 0 || hour > 23 {
				continue
			}
			// Compute the unix-second start of this UTC hour.
			hourStart := time.Date(dateStart.Year(), dateStart.Month(),
				dateStart.Day(), hour, 0, 0, 0, time.UTC).Unix()
			if afterUnix > 0 && hourStart+3600 < afterUnix {
				continue
			}
			if beforeUnix > 0 && hourStart > beforeUnix {
				continue
			}

			slugDir := fmt.Sprintf("%s/%s/%s/%s", root, dateStr, hourStr, slug)
			segEntries, err := os.ReadDir(slugDir)
			if err != nil {
				continue // slug subdir missing for this hour
			}
			for _, segEntry := range segEntries {
				if segEntry.IsDir() {
					continue
				}
				name := segEntry.Name() // "13.38.mp4"
				if !strings.HasSuffix(name, ".mp4") {
					continue
				}
				// Parse "MM.SS" — split off ".mp4" then split on ".".
				stem := strings.TrimSuffix(name, ".mp4")
				parts := strings.SplitN(stem, ".", 2)
				if len(parts) != 2 {
					continue
				}
				min, err1 := strconv.Atoi(parts[0])
				sec, err2 := strconv.Atoi(parts[1])
				if err1 != nil || err2 != nil || min < 0 || min > 59 || sec < 0 || sec > 59 {
					continue
				}
				segStartUnix := hourStart + int64(min)*60 + int64(sec)
				if afterUnix > 0 && segStartUnix < afterUnix {
					continue
				}
				if beforeUnix > 0 && segStartUnix > beforeUnix {
					continue
				}
				// Floor to 60s for the bucket key.
				minuteStart := (segStartUnix / 60) * 60
				b, ok := buckets[minuteStart]
				if !ok {
					b = &bucket{startUnix: minuteStart, endUnix: segStartUnix + 10, count: 1}
					buckets[minuteStart] = b
				} else {
					b.count++
					end := segStartUnix + 10
					if end > b.endUnix {
						b.endUnix = end
					}
				}
			}
		}
	}

	// Sort newest-first (descending start_unix).
	out := make([]RecordingMinute, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, RecordingMinute{
			StartUnix:    b.startUnix,
			EndUnix:      b.endUnix,
			SegmentCount: b.count,
			Storage:      "local",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartUnix > out[j].StartUnix })
	return out, nil
}

// ListRecordingMinutesFromCloud queries Quark Cloud Drive via Alist WebDAV.
func (r *Registry) ListRecordingMinutesFromCloud(ctx context.Context, cam *model.Camera, afterUnix, beforeUnix int64, localBuckets []RecordingMinute) ([]RecordingMinute, error) {
	if r.Cloud == nil {
		return nil, nil
	}
	slug := r.FrigateSlugUnique(cam)
	return r.Cloud.ListRecordingMinutesFromCloud(ctx, slug, afterUnix, beforeUnix, localBuckets)
}

// RecordingSegmentsFromCloud downloads segments for a minute from Quark Cloud Drive via Alist WebDAV.
func (r *Registry) RecordingSegmentsFromCloud(ctx context.Context, cam *model.Camera, minuteStart int64) ([]string, error) {
	if r.Cloud == nil {
		return nil, os.ErrNotExist
	}
	slug := r.FrigateSlugUnique(cam)
	return r.Cloud.FetchMinuteSegments(ctx, slug, minuteStart)
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
// Mirrors ListForOwner: admin always, non-admin only own — plus,
// since v1.7.0, any user explicitly granted access via a
// CameraShare row (see ShareCamera). The signature stays bool so
// the dozens of existing call sites don't have to grow an error
// return; a DB error from IsSharedWith is treated as "no access"
// (fail-closed) and logged via the registry's silent path.
func (r *Registry) CanRead(c *model.Camera, userID uint, isAdmin bool) bool {
	if isAdmin {
		return true
	}
	if c.OwnerID == userID {
		return true
	}
	// Non-owner, non-admin: only if an explicit CameraShare row
	// exists. Fail-closed on DB error — a transient SQLite
	// busy_timeout must not widen visibility.
	ok, err := r.IsSharedWith(c.ID, userID)
	if err != nil {
		return false
	}
	return ok
}

// CanPTZ reports whether a user is allowed to control PTZ on the camera.
// Admins and owners always can; shared users can only if CanPTZ is true on their CameraShare row.
func (r *Registry) CanPTZ(c *model.Camera, userID uint, isAdmin bool) bool {
	if isAdmin {
		return true
	}
	if c.OwnerID == userID {
		return true
	}
	var share model.CameraShare
	if err := r.DB.Where("camera_id = ? AND user_id = ?", c.ID, userID).First(&share).Error; err != nil {
		return false
	}
	return share.CanPTZ
}

// ShareCamera grants userID read access to cameraID with optional PTZ control.
// If the share already exists, it updates can_ptz.
func (r *Registry) ShareCamera(cameraID, userID uint, canPTZ bool) error {
	var share model.CameraShare
	err := r.DB.Where("camera_id = ? AND user_id = ?", cameraID, userID).First(&share).Error
	if err == nil {
		return r.DB.Model(&share).Update("can_ptz", canPTZ).Error
	}
	share = model.CameraShare{CameraID: cameraID, UserID: userID, CanPTZ: canPTZ}
	if err := r.DB.Create(&share).Error; err != nil {
		return fmt.Errorf("camera: share %d->%d: %w", cameraID, userID, err)
	}
	return nil
}

// UnshareCamera revokes userID's read access to cameraID. Missing
// rows are not an error — DELETE on a non-existent share is a
// no-op, which lets the dashboard's "remove viewer" button be
// idempotent across retries and stale UI state.
func (r *Registry) UnshareCamera(cameraID, userID uint) error {
	if err := r.DB.Where("camera_id = ? AND user_id = ?", cameraID, userID).
		Delete(&model.CameraShare{}).Error; err != nil {
		return fmt.Errorf("camera: unshare %d->%d: %w", cameraID, userID, err)
	}
	return nil
}

// ListShares returns every CameraShare row for the given camera,
// ordered by CreatedAt ascending so the dashboard's viewer list
// is stable across refreshes (oldest grant at the top).
func (r *Registry) ListShares(cameraID uint) ([]model.CameraShare, error) {
	var shares []model.CameraShare
	if err := r.DB.Where("camera_id = ?", cameraID).
		Order("created_at ASC").Find(&shares).Error; err != nil {
		return nil, fmt.Errorf("camera: list shares %d: %w", cameraID, err)
	}
	return shares, nil
}

// IsSharedWith reports whether userID has been granted read access
// to cameraID via an explicit CameraShare row. Used by CanRead to
// widen visibility beyond admin/owner without changing its bool
// signature. Returns (false, err) on DB error so the caller can
// fail-closed.
func (r *Registry) IsSharedWith(cameraID, userID uint) (bool, error) {
	var count int64
	if err := r.DB.Model(&model.CameraShare{}).
		Where("camera_id = ? AND user_id = ?", cameraID, userID).
		Count(&count).Error; err != nil {
		return false, fmt.Errorf("camera: is shared with %d->%d: %w", cameraID, userID, err)
	}
	return count > 0, nil
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
			r.add1080pStream(ctx, &c, u, p)
			// Native-HEVC camera also re-registers its passthrough
			// companion stream (<name>_hevc). Best-effort — a failure
			// just means the front-end uses the transcoded H.264 path.
			if cameraIsNativeHEVC(&c) {
				if err := r.Go2.AddStream(ctx, hevcStreamName(c.StreamName), r.hevcPassthroughURL(&c, u, p)); err != nil {
					log.Printf("camera: boot replay: cam %d (%s): add HEVC stream (non-fatal): %v", c.ID, c.StreamName, err)
				}
			}
			// Preheat each stream so the first user request after a
			// container restart doesn't pay the cold-start cost. Best-effort,
			// non-blocking; a slow preheat must not hold up boot replay.
			go func(streamName string) {
				pCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				r.Go2.Preheat(pCtx, streamName)
			}(c.StreamName)
			r.preheatHEVCStream(&c)
		}

		// Push WebRTC candidates FIRST, before the full config push.
		// SetWebRTCCandidates is a deep-merge partial update that
		// persists go2rtc.webrtc.candidates to Frigate's config.yml;
		// the full config push that follows triggers a restart
		// (requires_restart=true), and that restart loads the
		// candidates straight from config.yml. So the candidates are
		// in place BEFORE the reboot — there is no race window at all.
		//
		// v1.8.32 (restart-aware fix): the old code pushed candidates
		// after the config push, racing the very restart it triggered —
		// a single-shot SetWebRTCCandidates then hit "connection
		// refused" and left go2rtc's candidates stale until the next
		// PrefixWatcher tick (up to 5 min). Ordering the push before
		// the restart eliminates the race entirely; the wait+retry in
		// pushWebRTCCandidatesWithRetry only has to absorb the boot-up
		// window (Frigate may not be alive yet when go2rtc already is).
		if r.Frigate != nil {
			if err := r.pushWebRTCCandidatesWithRetry(ctx); err != nil {
				log.Printf("camera: boot replay: webrtc candidates push (non-fatal): %v", err)
			}
		}

		// Push the full config to Frigate so its AI detection and
		// recording pipelines pick up every camera. Its restart
		// now picks up the just-written candidates from config.yml.
		// Best-effort: if Frigate's REST API is down, retry in the
		// background so cameras are pushed as soon as Frigate is ready.
		if r.Frigate != nil {
			if err := r.pushFrigateConfig(ctx); err != nil {
				log.Printf("camera: boot replay: frigate config push (non-fatal): %v", err)
				go r.retryPushFrigateConfigInBackground(12, 10*time.Second)
			}
		}

		if failed == 0 {
			log.Printf("camera: boot replay: %d camera(s) registered with go2rtc", len(cams))
			go r.preheatAllCamerasInBackground(cams)
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

func (r *Registry) preheatAllCamerasInBackground(cams []model.Camera) {
	// Wait 2s for go2rtc and frigate to settle
	time.Sleep(2 * time.Second)
	log.Printf("camera: preheating %d camera streams for zero-delay WebRTC...", len(cams))
	for _, cam := range cams {
		go func(id uint, name string) {
			if err := r.PreheatStream(id); err == nil {
				log.Printf("camera: preheated stream for %s (id=%d)", name, id)
			}
		}(cam.ID, cam.Name)
	}
}

// pushWebRTCCandidatesWithRetry pushes the current WebRTC candidates
// to Frigate, waiting for Frigate's REST API to be reachable first
// (v1.8.32).
//
// Ordering: BootReplay calls this BEFORE pushFrigateConfig. Because
// SetWebRTCCandidates is a deep-merge partial update that persists
// go2rtc.webrtc.candidates to config.yml, the candidates survive the
// restart that pushFrigateConfig triggers (requires_restart=true) —
// the new Frigate process loads them from config.yml. Pushing first
// removes the race entirely: the old code pushed after the config
// push, so a single-shot SetWebRTCCandidates raced the reboot and
// failed with "connection refused", leaving go2rtc's candidates stale
// until the next PrefixWatcher tick (up to 5 min).
//
// The wait here only has to absorb the boot-up window: at boot Frigate
// (REST 5000) may not be alive yet when go2rtc (1984) already is. The
// candidates carry the NAS_LAN_IP / IPv6 address, so WebRTC stays
// routable after the NAS IP changes. NAS_IPV6_DISABLED is honoured so
// the harmless extra IPv6 candidate isn't advertised on non-broadband
// installs.
func (r *Registry) pushWebRTCCandidatesWithRetry(ctx context.Context) error {
	ipv6Addr := os.Getenv("NAS_IPV6_ADDRESS")
	if d := os.Getenv("NAS_IPV6_DISABLED"); d == "true" || d == "1" || d == "yes" {
		ipv6Addr = ""
	}

	// Wait (bounded) for Frigate's REST API to be reachable again after
	// the restart triggered by pushFrigateConfig. go2rtc (1984) comes up
	// faster than the Frigate REST front (5000), so this is the exact
	// window the old single-shot push could fail in.
	const maxWaitTries = 10
	for i := 0; i < maxWaitTries; i++ {
		if r.Frigate.Alive(ctx) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}

	// Retry the push to absorb a transient connection refused.
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := r.Frigate.SetWebRTCCandidates(ctx, ipv6Addr); err == nil {
			log.Printf("camera: webrtc candidates pushed after boot replay")
			return nil
		} else {
			lastErr = err
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	return lastErr
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
	slugs := r.computeUniqueSlugs()
	frigateCams := make([]FrigateCameraConfig, 0, len(cams))
	go2rtcStreams := make(map[string]any)
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

		// Frigate's name validator: ^[a-zA-Z0-9_-]+$
		// The slug is uniquified against the other cameras so two
		// cameras whose names slugify to the same base (e.g. "前门"
		// and "Front Door") get distinct Frigate camera names.
		slug := slugs[c.ID]
		// Disable offline cameras in Frigate to prevent endless ffmpeg
		// reconnect attempts and "video stream offline" errors. When the
		// camera comes back online, the next config push re-enables it.
		camEnabled := c.Status != "offline"
		detectEnabled := camEnabled
		var sec model.SecurityState
		if r.DB != nil && r.DB.First(&sec).Error == nil && sec.Mode == model.GuardModeDisarmed {
			detectEnabled = false
		}
		recordOutputArgs := "preset-record-generic-audio-aac"
		if !cameraHasAudio(&c) {
			recordOutputArgs = "preset-record-generic"
		}
		frigateCams = append(frigateCams, FrigateCameraConfig{
			Name:    slug,
			Enabled: camEnabled,
			Ffmpeg: FrigateFfmpeg{
				Inputs: r.frigateInputs(&c, u, p),
				OutputArgs: map[string]string{
					"record": recordOutputArgs,
				},
			},
			Detect: FrigateDetect{Enabled: detectEnabled, FPS: CameraDetectFPS(&c)},
			Record: FrigateRecord{Enabled: camEnabled},
		})
		// go2rtc stream key keeps the original friendly name so
		// the existing stream URLs (e.g. /api/stream.m3u8?src=前门)
		// continue to work.
		if cameraHasTwoWayAudio(&c) {
			go2rtcStreams[c.StreamName] = []string{
				go2rtcURL,
				r.rtspBackchannelURL(&c, u, p),
			}
			go BoostSpeakerVolume(ctx, c.Host, c.ONVIFPort, u, p)
		} else {
			go2rtcStreams[c.StreamName] = go2rtcURL
		}
		stream1080pURL := r.rtsp1080pURL(&c, u, p)
		if cameraHasTwoWayAudio(&c) {
			go2rtcStreams[c.StreamName+"_1080p"] = []string{
				stream1080pURL,
				r.rtspBackchannelURL(&c, u, p),
			}
		} else {
			go2rtcStreams[c.StreamName+"_1080p"] = stream1080pURL
		}
		if cameraIsNativeHEVC(&c) {
			go2rtcStreams[hevcStreamName(c.StreamName)] = r.hevcPassthroughURL(&c, u, p)
		}
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
	//
	// v1.8.24: retry up to 3 times with 2s/4s backoff. Frigate may
	// return 500 during startup (nginx not yet ready) or under
	// transient load. The retry is safe because a failed PushConfig
	// does not trigger a Frigate restart — only a successful one does.
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		err := r.Frigate.PushConfig(ctx, frigateCams, go2rtcStreams, true)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt < 3 {
			log.Printf("camera: frigate config push attempt %d/3 failed: %v", attempt, err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(1<<(attempt-1)) * 2 * time.Second):
			}
		}
	}
	return lastErr
}

// retryPushFrigateConfigInBackground retries pushFrigateConfig periodically in the background
// until Frigate's REST API is ready (covering cold-start delays or transient Frigate restarts).
func (r *Registry) retryPushFrigateConfigInBackground(maxAttempts int, interval time.Duration) {
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		time.Sleep(interval)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := r.pushFrigateConfig(ctx)
		cancel()
		if err == nil {
			log.Printf("camera: background frigate config push succeeded on attempt %d", attempt)
			return
		}
		log.Printf("camera: background frigate config push attempt %d/%d failed: %v", attempt, maxAttempts, err)
	}
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

// frigateSubstreamPath returns the RTSP URL for the camera's lower-resolution
// substream (e.g. 640x360), optimal for AI object detection without saturating
// CPU/GPU resources with 2.5K/4K decoding.
func (r *Registry) frigateSubstreamPath(cam *model.Camera, user, pass string) string {
	subChannel := cam.ChannelID
	if subChannel%10 == 1 {
		subChannel++
	} else if subChannel == 1 {
		subChannel = 102
	} else {
		return ""
	}
	return fmt.Sprintf("rtsp://%s:%s@%s:%d/Streaming/Channels/%d",
		user, pass, cam.Host, cam.RTSPPort, subChannel)
}

// frigateInputs builds the dual-stream input list for Frigate.
// High-resolution main stream is assigned role "record" (lossless copy),
// while lower-resolution substream is assigned role "detect" (low-overhead AI decode).
func (r *Registry) frigateInputs(cam *model.Camera, user, pass string) []FrigateInput {
	mainPath := r.frigateCameraPath(cam, user, pass)
	subPath := r.frigateSubstreamPath(cam, user, pass)
	if subPath != "" && subPath != mainPath {
		return []FrigateInput{
			{
				Path:  mainPath,
				Roles: []string{"record"},
			},
			{
				Path:  subPath,
				Roles: []string{"detect"},
			},
		}
	}
	return []FrigateInput{
		{
			Path:  mainPath,
			Roles: []string{"detect", "record"},
		},
	}
}

// uniqueSlug returns base if it is not yet taken, otherwise the first
// of base_2, base_3, ... that is not taken. Used on top of slugifyName
// to guarantee every camera pushed to Frigate gets a globally unique
// slug even when two friendly names slugify to the same base.
func uniqueSlug(base string, taken map[string]bool) string {
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s_%d", base, i)
		if !taken[candidate] {
			return candidate
		}
	}
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
		"小路": "xiao_lu",
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
// sending it to the camera. So a password "pass@word" becomes
// "pass%40word" on the wire, and the camera rejects it with
// "wrong user/pass". Building the URL as a plain string with the
// raw password avoids this. The go2rtc URL parser splits at the
// last "@" before the host, so a password containing "@" (e.g.
// "pass@word") produces "rtsp://admin:pass@word@host..." which
// go2rtc parses correctly as user=admin, pass=pass@word.
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

// hevcStreamName is the go2rtc stream key of a camera's second,
// native-HEVC passthrough live stream. The primary stream (keyed by
// the friendly name) transcodes to H.264 for universal WebRTC/HLS
// compatibility; this companion stream serves the camera's native
// HEVC with zero transcoding so HEVC-capable browsers can watch the
// high-quality source without tying up the J4125 ffmpeg pipeline.
const hevcStreamSuffix = "_hevc"

func hevcStreamName(name string) string { return name + hevcStreamSuffix }

// cameraIsNativeHEVC reports whether the camera's native video source
// is HEVC/H.265 — i.e. `effectiveCodec` equals "h264", which is only
// the case when we transcode the camera's HEVC source to H.264.
// Native-H.264 cameras keep `passthrough` and are NOT given a HEVC
// companion stream (their H.264 native stream already plays on every
// browser; a "HEVC" stream would just duplicate identical H.264).
func cameraIsNativeHEVC(cam *model.Camera) bool {
	return effectiveCodec(cam) == "h264"
}

// hevcPassthroughURL builds the go2rtc source for a camera's native
// HEVC companion stream. It forces passthrough regardless of the
// camera's configured servicing codec: the video track is handed
// through untouched (native HEVC via `rtsp://` scheme, or
// `#video=copy` when audio must be transcoded to AAC), so the fanout
// rtsp1080pURL builds the 1080p H.264 stream URL for WebRTC / high-res live view.
// It keeps video encoded as H.264 (width=1920) so Android and desktop browsers
// can decode via WebRTC without crashing on unsupported HEVC RTP streams.
func (r *Registry) rtsp1080pURL(cam *model.Camera, user, pass string) string {
	raw := fmt.Sprintf("rtsp://%s:%s@%s:%d/Streaming/Channels/%d",
		user, pass, cam.Host, cam.RTSPPort, cam.ChannelID)
	stopFrag := "#stop=0"
	if r.StopTimeout > 0 {
		stopFrag = "#stop=" + strconv.Itoa(r.StopTimeout)
	}
	audioOn := cameraHasAudio(cam)
	audioFrag := ""
	if audioOn {
		audioFrag = "#audio=opus#audio=aac#async=3000"
	}
	codec := effectiveCodec(cam)
	// If the camera is on passthrough (native H.264), pass video through untouched with #video=copy.
	// This uses ZERO GPU/CPU transcoding, avoids J4125 bottleneck, and provides buttery-smooth 1080p stream!
	if codec == "passthrough" {
		if audioOn {
			return "ffmpeg:" + raw + "#video=copy#audio=opus#audio=aac#async=3000" + stopFrag
		}
		return raw + "#audio=0" + stopFrag
	}
	// For cameras requiring H.264 transcoding (native HEVC sources):
	// v1.13.30: async=3000 absorbs network jitter to eliminate stuttering, stop=0 avoids cold-start delay
	return "ffmpeg:" + raw + "#video=h264#width=1920#hardware=vaapi#async=3000" + audioFrag + stopFrag
}

// add1080pStream registers the camera's 1080p H.264 stream (<name>_1080p).
func (r *Registry) add1080pStream(ctx context.Context, cam *model.Camera, user, pass string) {
	if r.Go2 == nil {
		return
	}
	streamName := cam.StreamName + "_1080p"
	streamURL := r.rtsp1080pURL(cam, user, pass)
	var err error
	if cameraHasTwoWayAudio(cam) {
		err = r.Go2.AddStreamSources(ctx, streamName, []string{
			streamURL,
			r.rtspBackchannelURL(cam, user, pass),
		})
	} else {
		err = r.Go2.AddStream(ctx, streamName, streamURL)
	}
	if err != nil {
		log.Printf("camera: add 1080p stream %q (non-fatal): %v", streamName, err)
	}
}

// hevcPassthroughURL builds the go2rtc source for a camera's native
// HEVC companion stream. It forces passthrough regardless of the
// camera's configured servicing codec.
func (r *Registry) hevcPassthroughURL(cam *model.Camera, user, pass string) string {
	raw := fmt.Sprintf("rtsp://%s:%s@%s:%d/Streaming/Channels/%d",
		user, pass, cam.Host, cam.RTSPPort, cam.ChannelID)
	stopFrag := "#stop=30"
	if r.StopTimeout > 0 {
		stopFrag = "#stop=" + strconv.Itoa(r.StopTimeout)
	}
	if cameraHasAudio(cam) {
		return "ffmpeg:" + raw + "#video=copy#audio=opus#audio=aac" + stopFrag
	}
	return raw + "#audio=0" + stopFrag
}

// addHEVCStream best-effort registers a camera's native-HEVC
// passthrough companion stream (<name>_hevc). No-op for cameras that
// are not native HEVC. Explicitly non-fatal: a missing passthrough
// stream just means the front-end falls back to the transcoded H.264
// path, so a transient go2rtc error must never fail registration or
// boot replay.
func (r *Registry) addHEVCStream(ctx context.Context, cam *model.Camera, user, pass string) {
	if r.Go2 == nil || !cameraIsNativeHEVC(cam) {
		return
	}
	if err := r.Go2.AddStream(ctx, hevcStreamName(cam.StreamName), r.hevcPassthroughURL(cam, user, pass)); err != nil {
		log.Printf("camera: add HEVC passthrough stream %q (non-fatal): %v", hevcStreamName(cam.StreamName), err)
	}
}

// preheatHEVCStream warms a camera's native-HEVC passthrough stream so
// the first HLS request on it doesn't pay the RTSP cold-start. Fire
// into a goroutine that owns its own timeout; best-effort.
func (r *Registry) preheatHEVCStream(cam *model.Camera) {
	if r.Go2 == nil || !cameraIsNativeHEVC(cam) {
		return
	}
	go func(streamName string) {
		pCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		r.Go2.Preheat(pCtx, streamName)
	}(hevcStreamName(cam.StreamName))
}

// CameraHasAudio reports whether the camera was registered with
// audio capability (microphone pickup enabled). The flag is stored as
// a generic JSON value in Capabilities, so we tolerate bool / numeric / string forms
// defensively (any non-empty truthy value counts).
func CameraHasAudio(cam *model.Camera) bool {
	if cam == nil || cam.Capabilities == nil {
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

func cameraHasAudio(cam *model.Camera) bool {
	return CameraHasAudio(cam)
}

// cameraHasTwoWayAudio reports whether the camera was registered with
// two-way audio / talkback capability.
func cameraHasTwoWayAudio(cam *model.Camera) bool {
	if cam.Capabilities == nil {
		return false
	}
	for _, key := range []string{"two_way_audio", "talkback", "audio_back"} {
		if v, ok := cam.Capabilities[key]; ok && v != nil {
			switch t := v.(type) {
			case bool:
				if t {
					return true
				}
			case float64:
				if t != 0 {
					return true
				}
			case int:
				if t != 0 {
					return true
				}
			case string:
				if t != "" && t != "false" && t != "0" {
					return true
				}
			}
		}
	}
	return false
}

func (r *Registry) rtspBackchannelURL(cam *model.Camera, user, pass string) string {
	vendor := strings.ToLower(cam.Vendor)
	if vendor == "" || strings.Contains(vendor, "hik") || strings.Contains(vendor, "haikang") {
		port := cam.ONVIFPort
		if port <= 0 {
			port = 80
		}
		return fmt.Sprintf("isapi://%s:%s@%s:%d/", user, pass, cam.Host, port)
	}
	return fmt.Sprintf("rtsp://%s:%s@%s:%d/Streaming/Channels/%d#backchannel=1",
		user, pass, cam.Host, cam.RTSPPort, cam.ChannelID)
}

func (r *Registry) rtspURL(cam *model.Camera, user, pass string) string {
	channel := cam.ChannelID
	if cam.TranscodeUseSubstream {
		if channel%10 == 1 {
			channel++
		} else if channel == 1 {
			channel = 102
		}
	}
	raw := fmt.Sprintf("rtsp://%s:%s@%s:%d/Streaming/Channels/%d",
		user, pass, cam.Host, cam.RTSPPort, channel)
	codec := effectiveCodec(cam)
	audioOn := cameraHasAudio(cam)
	// v1.13.19: Keep RTSP ffmpeg upstream permanently active (#stop=0)
	// so reconnection never suffers a 2-4s cold-start handshake delay.
	// Increased async buffer from 1000ms to 3000ms to absorb public network jitter
	// and prevent video frame drops that freeze the MediaCodec decoder.
	stopFrag := "#stop=0"
	if r.StopTimeout > 0 {
		stopFrag = "#stop=" + strconv.Itoa(r.StopTimeout)
	}
	if codec == "passthrough" || cam.TranscodeUseSubstream {
		if audioOn {
			return "ffmpeg:" + raw + "#video=copy#audio=opus#audio=aac#async=3000" + stopFrag
		}
		return raw + "#audio=0" + stopFrag
	}
	audioFrag := ""
	if audioOn {
		audioFrag = "#audio=opus#audio=aac#async=3000"
	}
	if codec == "h265" {
		return "ffmpeg:" + raw + "#video=h265#hardware=vaapi" + audioFrag + stopFrag
	}
	return "ffmpeg:" + raw + "#video=h264#width=1280#hardware=vaapi" + audioFrag + stopFrag
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
	// HLSHEVC is the HLS URL of the camera's native-HEVC passthrough
	// companion stream (<name>_hevc). Empty for non-HEVC cameras. When
	// present, a browser that can decode HEVC can watch this zero-
	// transcode HLS instead of the transcoded H.264 stream above.
	HLSHEVC string `json:"hls_hevc_url"`
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
	var hevcHLS string
	if cameraIsNativeHEVC(c) {
		hevcHLS = "/api/stream.m3u8?src=" + url.QueryEscape(hevcStreamName(c.StreamName)) + "&mp4="
	}
	if r.WebRTCURL != "" {
		base := strings.TrimRight(r.WebRTCURL, "/")
		return StreamConfig{
			StreamName: c.StreamName,
			WebRTC:     base + "/api/webrtc?src=" + enc,
			HLS:        base + "/api/stream.m3u8?src=" + enc + "&mp4=",
			HLSHEVC:    pickHEVCHLS(base, hevcHLS),
		}
	}
	return StreamConfig{
		StreamName: c.StreamName,
		WebRTC:     r.Go2.WebRTCURL(c.StreamName),
		HLS:        r.Go2.HLSURL(c.StreamName),
		HLSHEVC:    pickHEVCHLS(r.Go2.Base, hevcHLS),
	}
}

// pickHEVCHLS returns base+hevcPath when hevcPath is non-empty (a
// native-HEVC camera), else "". Centralizes the "empty means no HEVC
// companion stream" contract so the public-base and in-network
// branches stay identical.
func pickHEVCHLS(base, hevcPath string) string {
	if hevcPath == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + hevcPath
}

// itoa is a tiny convenience so callers don't need to import strconv
// just to format a probe target. Kept private; package users can keep
// using strconv.
func itoa(i int) string { return strconv.Itoa(i) }
