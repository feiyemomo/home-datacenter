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

// Registry is the camera CRUD + go2rtc sync boundary. It does NOT
// expose HTTP — the handler layer calls it.
type Registry struct {
	DB        *gorm.DB
	Go2       *Go2RTCClient
	Box       *utils.SecretBox
	ONVIF     *ONVIFController
	WebRTCURL string // optional public base, e.g. https://cam.feiyemomo.top
}

func NewRegistry(db *gorm.DB, g *Go2RTCClient, box *utils.SecretBox, onvif *ONVIFController, webRTCURL string) *Registry {
	return &Registry{DB: db, Go2: g, Box: box, ONVIF: onvif, WebRTCURL: webRTCURL}
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
	OwnerID   uint // 0 = assign to caller from handler context
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

	if err := r.DB.Model(cam).Updates(map[string]any{
		"updated_at": time.Now(),
	}).Error; err != nil {
		return nil, err
	}
	return cam, nil
}

// Unregister removes the row and asks go2rtc to drop the stream.
// go2rtc errors are logged but not returned — the DB is the source
// of truth and we don't want a half-deleted camera.
func (r *Registry) Unregister(ctx context.Context, id uint) error {
	var cam model.Camera
	if err := r.DB.First(&cam, id).Error; err != nil {
		return err
	}
	if cam.StreamName != "" {
		_ = r.Go2.RemoveStream(ctx, cam.StreamName)
	}
	return r.DB.Delete(&cam).Error
}

func (r *Registry) Get(id uint) (*model.Camera, error) {
	var c model.Camera
	if err := r.DB.First(&c, id).Error; err != nil {
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

// BootReplay re-registers every existing camera with go2rtc. Call
// this from main.go after the go2rtc container has had a moment to
// come up. A failure on one camera must not stop the others.
//
// Robustness: go2rtc's HTTP API may not be ready the instant its
// container starts (Docker Compose depends_on without a health
// condition only waits for the container to start, not for the
// service inside). We retry the whole replay pass with backoff so
// a slow-starting go2rtc doesn't leave all cameras unregistered —
// which was the root cause of the "SDP 500" / "cam_X not in go2rtc"
// issue. Errors are logged, not swallowed silently.
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
		// Native H.264 (or HEVC-on-HEVC-browser) path. The
		// rtsp scheme honours `#audio=0` to skip audio at
		// the source (Hikvision's PCMA/PCMU would otherwise
		// show up in the SDP but not be negotiated).
		return raw + "#audio=0"
	}
	// Transcode path: route through go2rtc's ffmpeg
	// pipeline. `video=h264` is a hard-coded ffmpeg preset
	// in go2rtc (see defaults["h264"] in ffmpeg.go: H.264
	// high@4.1, superfast/zerolatency, yuv420p). Omitting
	// `audio=` causes parseArgs to inject `-an` so ffmpeg
	// drops the camera's PCMA track entirely.
	return "ffmpeg:" + raw + "#video=h264"
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
