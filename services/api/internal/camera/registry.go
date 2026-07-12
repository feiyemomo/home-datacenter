package camera

import (
	"context"
	"fmt"
	"log"
	"net/url"
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
	// FrigateCamera overrides the auto-derived slug from StreamName.
	// Leave empty to use the default (e.g. "前门" → "front_door").
	FrigateCamera string
	OwnerID       uint // 0 = assign to caller from handler context
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
		ONVIFPort:         in.ONVIFPort,
		RTSPPort:          in.RTSPPort,
		ChannelID:         in.ChannelID,
		Status:            "unknown",
		OwnerID:           in.OwnerID,
		OnvifProfileToken: profile,
		Capabilities: model.JSON{
			"ptz":    in.PTZ,
			"audio":  in.Audio,
			"motion": in.Motion,
		},
		Credentials: creds,
		Meta:        model.JSON{},
		Transcode:   in.Transcode,
		// Bug1 fix: the go2rtc stream key is the friendly name,
		// not `cam_<id>`. Setting it BEFORE Create() lets the
		// UNIQUE constraint on stream_name reject duplicates at
		// the DB layer instead of crashing inside go2rtc.AddStream.
		StreamName: name,
		// Frigate camera name: explicit override wins, else slug.
		FrigateCamera: orDefault(in.FrigateCamera, slugifyName(name)),
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

// FindByFrigateCamera looks up a camera by its Frigate camera name
// (the identifier used in Frigate MQTT events like after.camera).
// Returns nil if no camera maps to the given Frigate name.
func (r *Registry) FindByFrigateCamera(frigateName string) (*model.Camera, error) {
	var c model.Camera
	if err := r.DB.Where("frigate_camera = ?", frigateName).First(&c).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *Registry) List() []model.Camera {
	var cs []model.Camera
	r.DB.Find(&cs)
	return cs
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
func (r *Registry) BootReplay(ctx context.Context) error {
	cams := r.List()
	if len(cams) == 0 {
		return nil
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
		// Prefer the explicit frigate_camera mapping; fall back to
		// the auto-derived slug from StreamName.
		slug := c.FrigateCamera
		if slug == "" {
			slug = slugifyName(c.StreamName)
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
			Detect: FrigateDetect{Enabled: true},
			Record: FrigateRecord{Enabled: false},
		})
		// go2rtc stream key keeps the original friendly name so
		// the existing stream URLs (e.g. /api/stream.m3u8?src=前门)
		// continue to work.
		go2rtcStreams[c.StreamName] = go2rtcURL
	}
	return r.Frigate.PushConfig(ctx, frigateCams, go2rtcStreams)
}

// frigateCameraPath is the URL form Frigate's OWN ffmpeg child
// process (for AI detection) expects. Unlike go2rtc, Frigate does
// not honour the `ffmpeg:` scheme prefix — it passes the path
// directly to `ffmpeg -i <path>`, so the scheme must be one ffmpeg
// knows natively (`rtsp://` is fine, with the same `#video=h264`
// and `#width=...` directives that go2rtc understands).
//
// Channel selection: when `transcode=true` AND
// `transcode_use_substream=true` (the default), we use the
// substream channel (ChannelID + 100, Hikvision convention:
// 101 → 201). The substream is typically 720x576 / 1 Mbps —
// designed for low-bandwidth remote viewing, and a perfect match
// for Cloudflare Tunnel links where the main 1080p HEVC stream
// produces 1+ MB HLS segments that hit the 5s go2rtc keepalive.
//
// IMPORTANT codec caveat: many newer Hikvision / Dahua
// cameras deliver the substream as H.265/HEVC too, which
// Chrome does NOT support for WebRTC (SDP 502 "codecs not
// matched: video:H265" — only Safari and Firefox nightly
// have HEVC WebRTC). If your substream is H.265 you must
// EITHER change the camera's substream video encoding to
// H.264 in its web admin UI, OR set
// `transcode_use_substream: false` in the camera config
// (which sources the main 101 stream and re-encodes it
// via the ffmpeg `#video=h264#width=1280` pipeline).
//
// (Previous `subStreamChannel` helper removed: the substream
// auto-switching heuristic was unreliable because newer
// Hikvision / Dahua cameras deliver the substream as H.265
// too, which Chrome cannot decode for WebRTC. We now source
// the configured ChannelID as-is and let the transcode
// pipeline re-encode to H.264 720p — universally compatible
// and the only stable path on slow Cloudflare Tunnel links.)

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
func (r *Registry) frigateCameraPath(cam *model.Camera, user, pass string) string {
	raw := fmt.Sprintf("rtsp://%s:%s@%s:%d/Streaming/Channels/%d",
		user, pass, cam.Host, cam.RTSPPort, cam.ChannelID)
	if !cam.Transcode {
		// Native path (operator-disabled transcode on a
		// codec-compatible camera). Skip audio. Frigate's
		// ffmpeg reads H.264 / H.265 directly.
		return raw + "#audio=0"
	}
	// Frigate ffmpeg reads the same `#video=h264` and
	// `#width=1280` query directives (Frigate reuses go2rtc's
	// ffmpeg arg parser). Transcoded streams hit Frigate's
	// detection pipeline as H.264 720p instead of raw HEVC 1080p,
	// which is the same path the streaming layer takes.
	return raw + "#video=h264#width=1280"
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
	var b strings.Builder
	prevUnderscore := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
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
		out = "camera"
	}
	return out
}

// orDefault returns a if non-empty, else b.
func orDefault(a, b string) string {
	if a != "" {
		return a
	}
	return b
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
// We always strip audio at the source (`#audio=0`) because the
// home dashboard doesn't play sound and the only consumer that
// would benefit is the mobile app — the savings in bandwidth and
// decode cost are not worth the extra wiring.
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
// URL. go2rtc's parseArgs adds `-an` automatically when
// `query["audio"]` is empty, and any non-empty value (e.g.
// "0", "anull") is fed straight to ffmpeg as a raw codec
// arg, which produces a malformed command line. The Hikvision
// audio (PCMA) is not browser-decodable anyway, so dropping
// it is the right default.
func (r *Registry) rtspURL(cam *model.Camera, user, pass string) string {
	raw := fmt.Sprintf("rtsp://%s:%s@%s:%d/Streaming/Channels/%d",
		user, pass, cam.Host, cam.RTSPPort, cam.ChannelID)
	if !cam.Transcode {
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
	// pipeline. `video=h264` is a hard-coded ffmpeg preset
	// in go2rtc (see defaults["h264"] in ffmpeg.go: H.264
	// high@4.1, superfast/zerolatency, yuv420p). Omitting
	// `audio=` causes parseArgs to inject `-an` so ffmpeg
	// drops the camera's PCMA track entirely.
	//
	// `width=1280` downscales the source to 1280px wide
	// (preserving aspect ratio). The HEVC front-door camera
	// is 1920x1080 at ~8 Mbps after transcode, which on a
	// 1 Mbps Cloudflare Tunnel link produces ~1 MB per
	// 1-second HLS segment and reliably hits the upstream
	// 5s go2rtc HLS keepalive. Downscaling to 720p (1280px
	// wide) cuts that to ~250-400 kbps, fitting 4-5
	// segments inside the 5s window. The aspect ratio
	// produces ~720p height.
	return "ffmpeg:" + raw + "#video=h264#width=1280"
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
