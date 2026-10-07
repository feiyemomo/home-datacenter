package camera

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// lanIPDetector tracks the NAS's LAN IPv4 address by observing
// incoming HTTP requests. When a client connects from the LAN,
// the Host header contains the NAS's LAN IP (e.g. 192.168.31.235).
// This allows the system to auto-adapt when the NAS IP changes
// without requiring NAS_LAN_IP to be manually updated.
//
// Priority:
//  1. NAS_LAN_IP env var (explicit operator configuration)
//  2. Auto-detected from HTTP request Host headers
//
// When the auto-detected IP changes, the OnChange callback is
// invoked asynchronously so the caller can push updated WebRTC
// candidates to Frigate without blocking the request.
type lanIPDetector struct {
	mu       sync.RWMutex
	detected string
	seen     map[string]bool
	onChange func(newIP string)
}

// GlobalLanIP is the package-level instance, updated by the HTTP
// middleware in main.go on every request.
var GlobalLanIP = &lanIPDetector{}

// UpdateFromHost extracts a private IPv4 address from an HTTP Host
// header and stores it as the detected LAN IP. Non-private addresses
// (public IPs, loopback, link-local) are ignored. When the detected
// IP changes, OnChange is called asynchronously.
func (d *lanIPDetector) UpdateFromHost(host string) {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host // no port
	}
	ip := net.ParseIP(h)
	if ip == nil || ip.To4() == nil {
		return
	}
	if !ip.IsPrivate() && !cgnatNet.Contains(ip) {
		return // RFC 1918 private, or RFC 6598 100.64/10 (Tailscale / overlay / CGNAT)
	}
	d.mu.Lock()
	if d.detected == h {
		d.mu.Unlock()
		return
	}
	d.detected = h
	if d.seen == nil {
		d.seen = map[string]bool{}
	}
	if d.seen[h] {
		// Already advertised as a WebRTC candidate. Clients alternating
		// between the LAN address and the overlay address must not
		// trigger a candidates push (and go2rtc restart) on every switch.
		d.mu.Unlock()
		return
	}
	d.seen[h] = true
	cb := d.onChange
	d.mu.Unlock()
	log.Printf("frigate: LAN IP auto-detected from request Host: %s", h)
	if cb != nil {
		go cb(h) // async — don't block the HTTP request
	}
}

// Get returns the current LAN IP, preferring the NAS_LAN_IP env var
// over the auto-detected value.
func (d *lanIPDetector) Get() string {
	if env := os.Getenv("NAS_LAN_IP"); env != "" {
		return env
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.detected
}

// Seen returns every LAN/overlay IP the API has been reached on since
// startup (sorted). All of them are advertised as WebRTC host
// candidates so the media path works whichever address the client used.
func (d *lanIPDetector) Seen() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(d.seen))
	for ip := range d.seen {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

// cgnatNet is RFC 6598 shared address space (100.64.0.0/10), used by
// Tailscale / ZeroTier-style overlays and carrier-grade NAT.
var cgnatNet = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("100.64.0.0/10")
	return n
}()

// SetOnChange registers a callback invoked when the auto-detected
// LAN IP changes. Called asynchronously from UpdateFromHost.
func (d *lanIPDetector) SetOnChange(cb func(newIP string)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onChange = cb
}

// FrigateClient is the HTTP client for the Frigate NVR service.
//
// Frigate (https://frigate.video) is a full-featured NVR that bundles
// go2rtc internally. We use two APIs:
//
//  1. Frigate REST API (port 5000) — config set (PUT /api/config/set)
//     to push camera definitions so Frigate's AI detection, recording,
//     and snapshot features know about each camera.
//  2. Bundled go2rtc API (port 1984) — stream management (PUT/DELETE
//     /api/streams) and WebRTC SDP exchange (POST /api/webrtc). This
//     is the same go2rtc API the old standalone container exposed.
//
// Why both: the go2rtc API alone makes streams available for live
// viewing, but Frigate's detection/recording pipeline reads from its
// own config.yml — if a camera isn't in the config, Frigate won't
// run object detection or record clips for it. Pushing config via
// the Frigate API ensures both the streaming layer and the AI layer
// know about every camera.
//
// We use PUT /api/config/set (not POST /api/config/save):
//   - /config/set accepts JSON in the body wrapped in a {config_data: ...}
//     envelope, returns 200 on success, 422 on validation error.
//   - /config/save accepts raw YAML (text/plain), requires
//     ?save_option=restart, and forces a full Frigate restart. It's
//     designed for the Settings UI's raw YAML editor, not for our
//     incremental camera-list push.
type FrigateClient struct {
	// FrigateBase is the Frigate REST API endpoint, e.g.
	// "http://home-frigate:5000".
	FrigateBase string
	// Go2rtcBase is the bundled go2rtc API endpoint, e.g.
	// "http://home-frigate:1984". This is the same API the old
	// standalone go2rtc container exposed.
	Go2rtcBase string
	HC         *http.Client
	// v1.6.3: in-process TTL cache for ListMotionRanges. Keyed by
	// "<camera>:<after>:<before>". See cacheMotionRanges for the
	// eviction policy. RWMutex because reads (cache hits) far
	// outnumber writes (cache misses).
	motionCache   map[string]motionCacheEntry
	motionCacheMu sync.RWMutex
	// v1.8.32: global record retention (days) applied by PushConfig's
	// record block. Default 7; override via SetRetentionDays so the
	// recording-quota monitor can restore the normal window after a
	// reduction.
	//
	// v1.8.35: guarded by retentionMu because PushConfig (BootReplay /
	// camera add) and PushRecordRetention (quota monitor) can run
	// concurrently at api boot. Without the lock + shared value, the
	// startup full-config push (requires_restart=1) could overwrite the
	// quota monitor's reduction back to the normal window: the monitor
	// samples immediately on start, races BootReplay, and whichever
	// push lands last wins — so the quota reduction must be reflected
	// in the retention value PushConfig reads, regardless of order.
	retentionMu   sync.Mutex
	retentionDays int
}

// NewFrigateClient returns a client with a 30s timeout (config save
// can take 5-10s while Frigate reloads ffmpeg pipelines for the
// changed cameras; under load, slower).
func NewFrigateClient(frigateBase, go2rtcBase string) *FrigateClient {
	return &FrigateClient{
		FrigateBase:   frigateBase,
		Go2rtcBase:    go2rtcBase,
		HC:            &http.Client{Timeout: 30 * time.Second},
		retentionDays: 7,
	}
}

// SetRetentionDays overrides the normal record retention window (days)
// used by PushConfig. The recording-quota monitor restores this value
// when the recordings tree drops back under quota (v1.8.32). Values
// below 1 are clamped back to the 7-day default.
func (c *FrigateClient) SetRetentionDays(days int) {
	if days < 1 {
		days = 7
	}
	c.retentionMu.Lock()
	c.retentionDays = days
	c.retentionMu.Unlock()
}

// CurrentRetention returns the current global record retention window
// (days) that PushConfig will apply. Thread-safe (v1.8.35).
func (c *FrigateClient) CurrentRetention() int {
	c.retentionMu.Lock()
	defer c.retentionMu.Unlock()
	return c.retentionDays
}

// Alive reports whether the Frigate REST API is reachable. Used by
// BootReplay to decide whether to attempt config push or wait.
func (c *FrigateClient) Alive(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.FrigateBase+"/api/config", nil)
	if err != nil {
		return false
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 500
}

// LatestFrame fetches the real-time decoded frame directly from Frigate's RAM buffer.
// Returns within ~20-50ms (versus 4-5s for cold go2rtc ffmpeg keyframe capture).
func (c *FrigateClient) LatestFrame(ctx context.Context, cameraSlug string) (io.ReadCloser, string, error) {
	if c == nil || c.FrigateBase == "" {
		return nil, "", fmt.Errorf("frigate client not configured")
	}
	u := fmt.Sprintf("%s/api/%s/latest.jpg", c.FrigateBase, url.PathEscape(cameraSlug))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	hc := c.HC
	if hc == nil {
		hc = &http.Client{Timeout: 3 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, "", fmt.Errorf("frigate latest frame status %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/jpeg"
	}
	return resp.Body, ct, nil
}

// RecordOutputArgsFor returns the Frigate `ffmpeg.output_args.record`
// value for a camera.
//
// Frigate selects the recording ffmpeg arguments by preset NAME
// (frigate/ffmpeg_presets.py). `preset-record-generic` ends in `-an`, so
// it drops the audio track at the RECORDING stage without re-encoding,
// while `preset-record-generic-audio-aac` keeps audio and transcodes the
// camera's PCMA track to AAC.
//
// We pick the silent preset whenever audio pickup is disabled so new
// recording segments are genuinely audio-free: the per-camera
// StreamConfig callback that maps preset names to argument lists only
// re-runs on a config reload, and the API pushes the config with
// requires_restart=true + an explicit /api/restart, which respawns the
// recorder with the new arguments.
func RecordOutputArgsFor(audioOn bool) string {
	if audioOn {
		return "preset-record-generic-audio-aac"
	}
	return "preset-record-generic"
}

// FrigateCameraConfig is the per-camera section in Frigate's config.yml.
//
// Frigate's Pydantic model validates each camera name with a strict
// regex (typically `^[a-zA-Z0-9_-]+$`) and rejects any extra fields
// not declared on the model. We send ONLY the fields Frigate knows
// about and rely on a separate `go2rtc.streams` block in the same
// payload for the stream definition (Frigate accepts a name→url map
// there without per-name validation).
type FrigateCameraConfig struct {
	Name    string        `yaml:"name" json:"name"`
	Enabled bool          `yaml:"enabled" json:"enabled"`
	Ffmpeg  FrigateFfmpeg `yaml:"ffmpeg" json:"ffmpeg"`
	Detect  FrigateDetect `yaml:"detect" json:"detect"`
	Record  FrigateRecord `yaml:"record" json:"record"`
}

type FrigateFfmpeg struct {
	Inputs     []FrigateInput    `yaml:"inputs" json:"inputs"`
	OutputArgs map[string]string `yaml:"output_args,omitempty" json:"output_args,omitempty"`
}

type FrigateInput struct {
	Path  string   `yaml:"path" json:"path"`
	Roles []string `yaml:"roles" json:"roles"`
}

type FrigateDetect struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	FPS     int  `yaml:"fps" json:"fps"`
}

// FrigateRecord controls Frigate's NVR-style continuous recording.
// When Enabled=true, Frigate records the camera's video to its media
// directory (/media/frigate/recordings).
//
// NOTE: Frigate's per-camera record config only accepts `enabled`.
// Retention policy (retain_days, events, motion, etc.) is set at the
// GLOBAL level via the `record` key in config_data, not per-camera.
type FrigateRecord struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

// FrigateConfig is the full Frigate config.yml structure. Only the
// keys we manage are typed; everything else Frigate needs (mqtt,
// environment, detectors) is in the static config file and is
// preserved across config saves.
type FrigateConfig struct {
	MQTT      FrigateMQTTConfig             `yaml:"mqtt" json:"mqtt"`
	Cameras   map[string]FrigateCameraConfig `yaml:"cameras" json:"cameras"`
	Go2RTC    FrigateGo2RTCConfig           `yaml:"go2rtc" json:"go2rtc"`
	Extra     map[string]any                `yaml:"-,omitempty" json:"-,omitempty"`
}

type FrigateMQTTConfig struct {
	Host     string `yaml:"host" json:"host"`
	User     string `yaml:"user,omitempty" json:"user,omitempty"`
	Password string `yaml:"password,omitempty" json:"password,omitempty"`
}

type FrigateGo2RTCConfig struct {
	WebRTC WebRTCConfig `yaml:"webrtc" json:"webrtc"`
	HLS    HLSConfig    `yaml:"hls" json:"hls"`
}

type WebRTCConfig struct {
	Listen string `yaml:"listen" json:"listen"`
}

type HLSConfig struct {
	Segment int  `yaml:"segment" json:"segment"`
	Partial bool `yaml:"partial" json:"partial"`
	Window  int  `yaml:"window" json:"window"`
}

// PushConfig generates the camera-list portion of the Frigate config
// and pushes it via PUT /api/config/set. Frigate validates the change
// against its Pydantic schema and applies it.
//
// The body is JSON of the form:
//
//	{
//	  "requires_restart": 0|1,
//	  "update_topic": "config/cameras",
//	  "config_data": {
//	    "cameras": { "front_door": {...}, ... },
//	    "record": { "enabled": true, "motion": { "days": 7 } },
//	    "go2rtc": { "streams": { "前门": "rtsp://..." } }
//	  }
//	}
//
// We send ONLY the sections we manage (cameras + go2rtc.streams +
// global record retention). Sending the full config would re-send
// credentials that Frigate has already redacted (e.g. the
// mqtt.password shows up as REDACTED_CREDENTIAL_SENTINEL in
// /api/config) and Frigate's validator would reject the round-trip.
// By sending only our own sections, we let Frigate's deep-merge keep
// the other global settings (mqtt, detectors, environment, etc.)
// untouched.
//
// Note on camera naming: Frigate's Pydantic model validates each
// camera name against a strict regex (typically `^[a-zA-Z0-9_-]+$`)
// and rejects any extra fields. The dashboard's "friendly name"
// (e.g. "前门") is allowed in the go2rtc stream key but cannot be
// used as a Frigate camera name. Callers should pass a normalized
// ASCII slug (e.g. "front_door") as the camera name; the go2rtc
// stream is keyed by the original friendly name.
//
// requires_restart:
//   - false (0): camera add/remove changes are applied via ZMQ to
//     the running processes without a full Frigate restart.
//   - true (1): forces a full Frigate restart after applying the
//     config. REQUIRED when toggling record.enabled on a camera,
//     because Frigate only starts/stops the recording ffmpeg
//     pipeline during a restart — a hot config merge alone does
//     not spin up the recorder process. Without this, the config
//     push returns 200 but no recordings are ever produced.
func (c *FrigateClient) PushConfig(ctx context.Context, cameras []FrigateCameraConfig, go2rtcStreams map[string]any, requiresRestart bool) error {
	// Snapshot the current retention under the lock so the quota
	// monitor's concurrent reduction is respected (v1.8.35).
	c.retentionMu.Lock()
	retentionDays := c.retentionDays
	c.retentionMu.Unlock()

	partial := map[string]any{
		"cameras": camerasAsMap(cameras),
		// Global record config: enable 24/7 continuous recording
		// with 7-day retention. Per-camera record.enabled controls
		// which cameras actually record; this global block sets the
		// retention policy for all cameras that have recording enabled.
		//
		// IMPORTANT: Frigate 0.17 record schema:
		//   - record.enabled: master switch for 24/7 recording
		//   - record.continuous.days: retain ALL footage for N days
		//   - record.motion.days: retain motion segments for N days
		//   - record.alerts.retain.days/mode: keep alert segments
		//   - record.detections.retain.days/mode: keep detection segments
		// `record.retain` is NOT a valid key in Frigate 0.17 — the
		// Pydantic validator rejects it as extra_forbidden, causing 400.
		//
		// v1.8.32: the retention window is configurable (c.retentionDays,
		// default 7) instead of hardcoded, so the recording-quota monitor
		// can shorten it and restore it later.
		"record": map[string]any{
			"enabled": true,
			"continuous": map[string]any{
				"days": retentionDays,
			},
			"motion": map[string]any{
				"days": retentionDays,
			},
		},
		// Enable snapshots globally so Frigate captures a still JPEG
		// for each detection event. Without this, has_snapshot is
		// always false and the /api/events thumbnail field is empty,
		// leaving the dashboard's alert list without preview images.
		// Snapshots are stored in /media/frigate/clips and are also
		// served inline (base64) by GET /api/events?include_thumbnails=1.
		"snapshots": map[string]any{
			"enabled":   true,
			"clean_copy": true,
			"timestamp":  false,
			"bounding_box": true,
			"crop":      false,
			"quality":   95,
		},
	}
	if len(go2rtcStreams) > 0 {
		partial["go2rtc"] = map[string]any{
			"streams": go2rtcStreams,
			"webrtc": map[string]any{
				"listen": ":8555",
				// Same dynamic list as SetWebRTCCandidates. This push runs
				// with requires_restart, so a hardcoded list here used to
				// overwrite config.yml with stale addresses on every restart.
				"candidates": BuildWebRTCCandidates(WebRTCIPv6FromEnv()),
			},
			"hls": map[string]any{
				"segment": 1.5,
				"window":  8,
			},
		}
	}

	// Wrap the config in the {config_data: ...} envelope the
	// /api/config/set endpoint expects.
	restartVal := 0
	if requiresRestart {
		restartVal = 1
	}
	body, err := json.Marshal(map[string]any{
		"requires_restart": restartVal,
		"update_topic":     "config/cameras",
		"config_data":      partial,
	})
	if err != nil {
		return fmt.Errorf("marshal frigate config: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.FrigateBase+"/api/config/set", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("frigate config set: %s: %s", resp.Status, string(raw))
	}

	log.Printf("frigate: config pushed (%d cameras, requires_restart=%d)", len(cameras), restartVal)

	// v1.7.1: explicitly call /api/restart when requiresRestart is
	// true. The requires_restart field in the config/set body is a
	// hint that Frigate MAY honor, but in practice Frigate 0.17 only
	// hot-merges the config via ZMQ without spinning up the recording
	// ffmpeg pipeline for newly added cameras. A dedicated restart
	// call guarantees the camera processor and ffmpeg processes start
	// — without it, new cameras show up in the config but never
	// produce recordings (the exact symptom seen with camera "小路").
	// Best-effort: log failures but don't fail the PushConfig call,
	// since the config itself was already saved successfully.
	if requiresRestart {
		restartReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.FrigateBase+"/api/restart", nil)
		if err == nil {
			restartResp, err := c.HC.Do(restartReq)
			if err == nil {
				_ = restartResp.Body.Close()
				log.Printf("frigate: restart triggered (status %d)", restartResp.StatusCode)
			} else {
				log.Printf("frigate: restart call failed (non-fatal): %v", err)
			}
		}
	}
	return nil
}

// detectLANIP returns the host's LAN IPv4 address. It checks the
// NAS_LAN_IP env var first (explicit operator configuration), then
// falls back to the auto-detected value from HTTP request Host headers
// (see GlobalLanIP). Returns "" if neither source has a value.
func detectLANIP() string {
	return GlobalLanIP.Get()
}

// SetWebRTCCandidates pushes an updated go2rtc webrtc.candidates list
// to Frigate via PUT /api/config/set. Used by the PrefixWatcher when
// the ISP rotates the IPv6 prefix — the new outbound address becomes
// the new IPv6 host candidate, replacing the old (now unreachable) one.
//
// The push is a partial config update: only go2rtc.webrtc.candidates
// is sent, so Frigate's deep-merge preserves all other config (cameras,
// mqtt, detectors, etc.). Frigate applies the new candidates to the
// running go2rtc subsystem without a restart.
//
// The candidate list is built dynamically from:
//   - 127.0.0.1 (loopback, for same-host browser access)
//   - NAS_LAN_IP env var (host's physical LAN IPv4, e.g. 192.168.31.235)
//   - ipv6Addr parameter (host's public IPv6, if provided)
//
// If NAS_LAN_IP is unset, only loopback + IPv6 are advertised and a
// warning is logged — WebRTC will not work from LAN clients until the
// operator sets the env var. This is far better than the previous
// behavior which hardcoded a stale IP (192.168.1.3) that silently broke
// WebRTC after any NAS IP change.
//
func detectH3CTCPCandidate() string {
	keepaliveURL := os.Getenv("H3C_KEEPALIVE_URL")
	if keepaliveURL == "" {
		keepaliveURL = "http://home-h3c-keepalive:8087/status"
	}
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(keepaliveURL)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var st struct {
				Tunnels map[string]struct {
					ExternalAddr string `json:"externalAddr"`
					Status       string `json:"status"`
				} `json:"tunnels"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&st); err == nil {
				if t, ok := st.Tunnels["webrtc_test"]; ok && t.ExternalAddr != "" && t.Status == "ESTABLISHED" {
					return t.ExternalAddr
				}
			}
		}
	}
	return os.Getenv("WEBRTC_TCP_CANDIDATE")
}

// BuildWebRTCCandidates returns the go2rtc webrtc.candidates list. It is
// the single source of truth for both the full config push (PushConfig)
// and the partial candidates push. The full push used to hardcode
// 192.168.31.234 and a long-dead H3C port (154.8.195.220:32510). Because
// it runs with requires_restart (for example on every audio toggle),
// go2rtc came back up advertising stale addresses and WebRTC media never
// connected.
//
// Sources, de-duplicated in order:
//   - 127.0.0.1 (same-host browser)
//   - NAS_LAN_IP env / auto-detected LAN IP
//   - every private or 100.64/10 overlay IP the API was reached on
//   - WEBRTC_EXTRA_CANDIDATES env (comma separated; port defaults to 8555)
//   - public IPv6 (when not disabled)
//   - the H3C tunnel TCP candidate
//   - stun:8555
func BuildWebRTCCandidates(ipv6Addr string) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	withPort := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "" {
			return ""
		}
		if _, _, err := net.SplitHostPort(s); err == nil {
			return s
		}
		return net.JoinHostPort(strings.Trim(s, "[]"), "8555")
	}

	add("127.0.0.1:8555")
	if lanIP := detectLANIP(); lanIP != "" {
		add(withPort(lanIP))
	} else {
		log.Printf("frigate: webrtc candidates: NAS_LAN_IP not set and no LAN IP detected yet; WebRTC will not work from LAN clients")
	}
	for _, ip := range GlobalLanIP.Seen() {
		add(withPort(ip))
	}
	for _, extra := range strings.Split(os.Getenv("WEBRTC_EXTRA_CANDIDATES"), ",") {
		add(withPort(extra))
	}
	if ipv6Addr != "" {
		add(net.JoinHostPort(ipv6Addr, "8555"))
	}
	if tcp := detectH3CTCPCandidate(); tcp != "" {
		add(tcp)
	}
	add("stun:8555")
	return out
}

// WebRTCIPv6FromEnv returns NAS_IPV6_ADDRESS unless NAS_IPV6_DISABLED is set.
func WebRTCIPv6FromEnv() string {
	if d := os.Getenv("NAS_IPV6_DISABLED"); d == "true" || d == "1" || d == "yes" {
		return ""
	}
	return os.Getenv("NAS_IPV6_ADDRESS")
}

// Returns an error if the Frigate API call fails. The caller
// (PrefixWatcher, BootReplay) logs the error but doesn't block subsequent checks.
func (c *FrigateClient) SetWebRTCCandidates(ctx context.Context, ipv6Addr string) error {
	candidates := BuildWebRTCCandidates(ipv6Addr)

	partial := map[string]any{
		"go2rtc": map[string]any{
			"webrtc": map[string]any{
				"candidates": candidates,
			},
		},
	}

	// v1.8.32: wrap the partial in the same envelope PushRecordRetention
	// uses. Frigate's PUT /api/config/set rejects a bare config object
	// with 400 {"message":"No configuration data provided"} — it expects
	// the config under `config_data`. The old code sent `partial` raw,
	// so every candidate push (BootReplay AND the PrefixWatcher tick)
	// was silently failing with this 400, leaving go2rtc's webrtc
	// candidates stale forever. requires_restart=0 (no reboot needed) and
	// go2rtc.webrtc.candidates is a deep-merge update, so the other
	// config blocks are preserved.
	body, err := json.Marshal(map[string]any{
		"requires_restart": 0,
		"update_topic":     "config/webrtc-candidates",
		"config_data":      partial,
	})
	if err != nil {
		return fmt.Errorf("marshal candidates config: %w", err)
	}

	// Reuse the existing PUT /api/config/set endpoint.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.FrigateBase+"/api/config/set", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HC.Do(req)
	if err != nil {
		return fmt.Errorf("frigate config set: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
		return fmt.Errorf("frigate returned %d: %s", resp.StatusCode, string(respBody))
	}

	// Also push directly to go2rtc active primary config and trigger runtime reload
	if c.Go2rtcBase != "" {
		go func(cands []string) {
			go2Payload, err := json.Marshal(map[string]any{
				"webrtc": map[string]any{
					"candidates": cands,
				},
			})
			if err == nil {
				req, err := http.NewRequest(http.MethodPost, c.Go2rtcBase+"/api/config", bytes.NewReader(go2Payload))
				if err == nil {
					req.Header.Set("Content-Type", "application/json")
					if resp, err := c.HC.Do(req); err == nil {
						resp.Body.Close()
					}
				}
				if req, err := http.NewRequest(http.MethodPost, c.Go2rtcBase+"/api/restart", nil); err == nil {
					if resp, err := c.HC.Do(req); err == nil {
						resp.Body.Close()
					}
				}
			}
		}(candidates)
	}

	return nil
}

// PushRecordRetention updates Frigate's global record retention policy
// (record.continuous.days / record.motion.days) via PUT /api/config/set
// (v1.8.32). Used by the recording-quota monitor to shorten retention
// when the recordings tree exceeds its quota, and to restore the normal
// window once it drops back below.
//
// No requires_restart / dedicated restart is needed: Frigate's periodic
// cleanup job deletes footage beyond the new window on its next run, so
// the change takes effect without interrupting live streams. The push is
// a partial config update (only the record block), so Frigate's
// deep-merge preserves cameras, snapshots, go2rtc, etc.
func (c *FrigateClient) PushRecordRetention(ctx context.Context, days int) error {
	if days < 1 {
		days = 1
	}
	// v1.8.35: record the new window as the current retention so a
	// concurrent PushConfig (BootReplay / camera add) applies the same
	// value instead of overwriting it with the stale normal window. This
	// makes the quota reduction stick regardless of goroutine ordering.
	c.retentionMu.Lock()
	c.retentionDays = days
	c.retentionMu.Unlock()

	partial := map[string]any{
		"record": map[string]any{
			"enabled": true,
			"continuous": map[string]any{
				"days": days,
			},
			"motion": map[string]any{
				"days": days,
			},
		},
	}
	body, err := json.Marshal(map[string]any{
		"requires_restart": 0,
		"update_topic":     "config/record-retention",
		"config_data":      partial,
	})
	if err != nil {
		return fmt.Errorf("marshal record retention config: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.FrigateBase+"/api/config/set", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("frigate record retention set: %s: %s", resp.Status, string(raw))
	}

	log.Printf("frigate: record retention set to %d days", days)
	return nil
}

// fetchConfig retrieves the current Frigate config as a generic map.
// Currently unused — we send partial configs instead. Kept for
// future use (e.g. reading back the merged config to verify).
//
// Note: credentials are redacted in the response (mqtt.password,
// go2rtc stream URLs, etc. show as REDACTED_CREDENTIAL_SENTINEL),
// so this endpoint is NOT safe to use for round-tripping.
func (c *FrigateClient) fetchConfig(ctx context.Context) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.FrigateBase+"/api/config", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch config: %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// FrigateEvent is a simplified view of a Frigate detection event
// returned by GET /api/events. Only the fields we need for the
// dashboard's alert list are typed; the rest are ignored.
//
// NOTE: Frigate 0.17 moved `top_score` into a nested `data` object.
// The root-level `top_score` is now null for in-progress events and
// only populated when the event ends. The `data.top_score` field is
// always populated with the highest detection score seen so far.
// We read from `data` and fall back to the root field for older
// Frigate versions.
type FrigateEvent struct {
	ID          string  `json:"id"`
	Camera      string  `json:"camera"`
	Label       string  `json:"label"`
	TopScore    float64 `json:"top_score"`
	StartTime   float64 `json:"start_time"`
	EndTime     float64 `json:"end_time"`
	Zones       []string `json:"zones"`
	HasClip     bool    `json:"has_clip"`
	HasSnapshot bool    `json:"has_snapshot"`
	Thumbnail   string `json:"thumbnail,omitempty"`
	Data        FrigateEventData `json:"data,omitempty"`
}

// FrigateEventData holds the nested detection metadata that Frigate
// 0.17 puts under the `data` key of each event.
type FrigateEventData struct {
	TopScore float64 `json:"top_score"`
	Score    float64 `json:"score"`
}

// EffectiveTopScore returns the best-known detection confidence for
// the event, preferring the nested `data.top_score` (always populated
// in Frigate 0.17) and falling back to the root-level `top_score`
// (populated only after the event ends, or in older Frigate versions).
func (e *FrigateEvent) EffectiveTopScore() float64 {
	if e.Data.TopScore > 0 {
		return e.Data.TopScore
	}
	if e.Data.Score > 0 {
		return e.Data.Score
	}
	return e.TopScore
}

// EventFilter holds optional criteria for querying Frigate events.
type EventFilter struct {
	Cameras           string
	Labels            string
	Before            int64
	After             int64
	Limit             int
	IncludeThumbnails bool
}

// ListEventsFiltered queries Frigate for detection events matching filter criteria.
func (c *FrigateClient) ListEventsFiltered(ctx context.Context, f EventFilter) ([]FrigateEvent, error) {
	u := c.FrigateBase + "/api/events"
	params := url.Values{}
	if f.Limit > 0 {
		params.Set("limit", strconv.Itoa(f.Limit))
	}
	if f.IncludeThumbnails {
		params.Set("include_thumbnails", "1")
	} else {
		params.Set("include_thumbnails", "0")
	}
	if f.Cameras != "" {
		params.Set("cameras", f.Cameras)
	}
	if f.Labels != "" {
		params.Set("labels", f.Labels)
	}
	if f.Before > 0 {
		params.Set("before", strconv.FormatInt(f.Before, 10))
	}
	if f.After > 0 {
		params.Set("after", strconv.FormatInt(f.After, 10))
	}
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("frigate list events: %s: %s", resp.Status, string(raw))
	}
	var events []FrigateEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, fmt.Errorf("decode frigate events: %w", err)
	}
	return events, nil
}

// ListEvents queries Frigate for recent detection events.
// Frigate's GET /api/events returns events sorted newest-first.
// The limit parameter caps the number of results (0 = server default).
//
// When includeThumbnails is true, Frigate returns a small base64-encoded
// JPEG thumbnail for each event (typically 1-3KB). These are used by
// the dashboard's alert list for instant preview without a second
// round-trip per event.
func (c *FrigateClient) ListEvents(ctx context.Context, limit int, includeThumbnails bool) ([]FrigateEvent, error) {
	return c.ListEventsFiltered(ctx, EventFilter{
		Limit:             limit,
		IncludeThumbnails: includeThumbnails,
	})
}

// GetEvent retrieves metadata for a single detection event from Frigate
// via GET /api/events/<id>. Used to resolve the event's camera for
// authorization checks before proxying snapshots or thumbnails.
func (c *FrigateClient) GetEvent(ctx context.Context, eventID string) (*FrigateEvent, error) {
	u := fmt.Sprintf("%s/api/events/%s", c.FrigateBase, url.PathEscape(eventID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("frigate get event: %s: %s", resp.Status, string(raw))
	}
	var ev FrigateEvent
	if err := json.NewDecoder(resp.Body).Decode(&ev); err != nil {
		return nil, fmt.Errorf("decode frigate event: %w", err)
	}
	return &ev, nil
}


// EventSnapshot fetches the full-resolution snapshot JPEG for a given
// Frigate event ID. Frigate serves these at GET /api/events/<id>/snapshot.jpg.
// Defaults to full quality (100).
func (c *FrigateClient) EventSnapshot(ctx context.Context, eventID string) (io.ReadCloser, string, error) {
	return c.EventSnapshotWithQuality(ctx, eventID, 100)
}

// EventSnapshotWithQuality fetches the snapshot JPEG with an explicit quality parameter.
func (c *FrigateClient) EventSnapshotWithQuality(ctx context.Context, eventID string, quality int) (io.ReadCloser, string, error) {
	u := fmt.Sprintf("%s/api/events/%s/snapshot.jpg", c.FrigateBase, url.PathEscape(eventID))
	if quality > 0 {
		u += fmt.Sprintf("?quality=%d", quality)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return nil, "", fmt.Errorf("frigate snapshot: %s: %s", resp.Status, string(raw))
	}
	return resp.Body, resp.Header.Get("Content-Type"), nil
}

// EventThumbnail proxies Frigate's small JPEG thumbnail for an event.
// Frigate 0.17 no longer inlines base64 thumbnails in /api/events
// (the `thumbnail` field is null even with include_thumbnails=1), so
// the dashboard fetches each thumbnail via this endpoint instead.
// Thumbnails are ~6KB JPEGs suitable for list previews; the full
// snapshot is served separately via EventSnapshot.
func (c *FrigateClient) EventThumbnail(ctx context.Context, eventID string) (io.ReadCloser, string, error) {
	u := fmt.Sprintf("%s/api/events/%s/thumbnail.jpg", c.FrigateBase, url.PathEscape(eventID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return nil, "", fmt.Errorf("frigate thumbnail: %s: %s", resp.Status, string(raw))
	}
	return resp.Body, resp.Header.Get("Content-Type"), nil
}

// DeleteEvent deletes a single detection event from Frigate via
// DELETE /api/events/<id>. Used when a camera is unregistered so its
// detection history is removed along with the camera. Returns an
// error on non-2xx responses.
func (c *FrigateClient) DeleteEvent(ctx context.Context, eventID string) error {
	u := fmt.Sprintf("%s/api/events/%s", c.FrigateBase, url.PathEscape(eventID))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("frigate delete event: %s: %s", resp.Status, string(raw))
	}
	return nil
}

// camerasAsMap converts the typed camera config slice to the
// map[string]any shape Frigate's config save expects (cameras is a
// map keyed by camera name, not a list).
func camerasAsMap(cameras []FrigateCameraConfig) map[string]any {
	m := make(map[string]any, len(cameras))
	for _, c := range cameras {
		m[c.Name] = c
	}
	return m
}

// FrigateRecording is a single recording segment returned by
// GET /api/<camera>/recordings. Frigate stores recordings as
// ~10s MP4 files on disk; each entry in the API response covers
// one 10s segment.
//
// NOTE: Frigate's API returns end_time and duration as floats
// (e.g. 1784306209.996875), not ints. We use float64 to avoid
// JSON unmarshal errors. Callers that need int seconds should
// cast explicitly.
//
// v1.6.0: added Motion and Objects fields. Frigate's per-segment
// `motion` is the count of motion-active sub-segments within the
// 10s clip (0 = no motion, 1-10 = sub-segments with pixel-diff
// motion). `objects` is the count of sub-segments with tracked
// object detections (person, car, etc.). The dashboard uses
// motion > 0 to paint red marks on the day-playback SeekBar —
// without this field the overlay relied on /api/events (alerts)
// which only fires when AI detection finds a person/car, leaving
// the overlay empty even when there's clear motion activity.
type FrigateRecording struct {
	Camera    string  `json:"camera"`
	StartTime int64   `json:"start_time"`       // unix seconds
	EndTime   float64 `json:"end_time"`         // unix seconds (float)
	Duration  float64 `json:"duration"`         // seconds (float)
	HasClip   bool    `json:"has_clip"`
	HasSnap   bool    `json:"has_snapshot"`
	Motion    int     `json:"motion"`           // v1.6.0: motion-active sub-segments (0-10)
	Objects   int     `json:"objects"`          // v1.6.0: object-detection sub-segments (0-10)
}

// ListRecordings queries Frigate for recording segments of a camera.
// cameraName is the Frigate slug (ASCII, e.g. "front_door").
// after/before are unix seconds (0 = no bound). Returns segments
// newest-first.
func (c *FrigateClient) ListRecordings(ctx context.Context, cameraName string, after, before int64) ([]FrigateRecording, error) {
	u := fmt.Sprintf("%s/api/%s/recordings", c.FrigateBase, url.PathEscape(cameraName))
	params := url.Values{}
	if after > 0 {
		params.Set("after", strconv.FormatInt(after, 10))
		// Frigate's recordings API returns an EMPTY array when
		// `after` is set but `before` is omitted — it does NOT
		// default `before` to "now". We must always pair `after`
		// with an explicit `before` (defaulting to now) or the
		// response is silently empty.
		if before <= 0 {
			before = time.Now().Unix()
		}
	}
	if before > 0 {
		params.Set("before", strconv.FormatInt(before, 10))
	}
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("frigate list recordings: %s: %s", resp.Status, string(raw))
	}
	var recs []FrigateRecording
	if err := json.NewDecoder(resp.Body).Decode(&recs); err != nil {
		return nil, fmt.Errorf("decode recordings: %w", err)
	}
	return recs, nil
}

// ListMotionRanges returns time ranges (in unix seconds) where Frigate
// recorded motion activity for the given camera within [after, before).
//
// MotionRange is one contiguous motion-active time range with
// pre-aggregated metadata for the dashboard / mobile app.
//
// v1.6.3: replaced the v1.6.0 [][2]int64 return type with this
// struct so the mobile app can render a "motion chip" list without
// re-fetching each segment's score from Frigate. The user explicitly
// asked for "现场计算" to be avoided — all aggregation happens
// server-side here.
//
// Fields:
//   - StartUnix/EndUnix: contiguous range bounds (unix seconds)
//   - Duration: seconds = EndUnix - StartUnix (redundant but
//     convenient for clients that don't want to do math)
//   - MotionScore: sum of Frigate's per-segment motion counts.
//     Frigate's `motion` is the count of motion-active sub-segments
//     within a 10s clip (0-10). Summing across the merged range
//     gives a rough "how much motion" signal — useful for sorting
//     chips by intensity (high-score chips render larger / brighter).
//   - SegmentCount: how many 10s Frigate segments were merged into
//     this range. Lets the client show "N segments" without another
//     round-trip.
//   - PeakObjects: max `objects` across all merged segments. If
//     > 0, AI tracked something (person/car) — renders a different
//     chip color than pure motion.
type MotionRange struct {
	StartUnix    int64 `json:"start"`
	EndUnix      int64 `json:"end"`
	Duration     int64 `json:"duration"`
	MotionScore  int   `json:"motion_score"`
	SegmentCount int   `json:"segment_count"`
	PeakObjects  int   `json:"peak_objects"`
}

// motionCacheEntry stores a ListMotionRanges result with its fetch
// timestamp. Used by the in-process TTL cache to avoid re-querying
// Frigate when the mobile app re-opens the same day's playlist.
type motionCacheEntry struct {
	ranges    []MotionRange
	fetchedAt time.Time
}

// ListMotionRanges queries Frigate for recording segments with
// motion > 0 in the given [after, before) time window, then merges
// adjacent motion segments into contiguous ranges.
//
// Frigate's /api/<cam>/recordings endpoint has an internal cap of
// ~500 segments per request (verified in v1.5.20). For a 24h window
// that's 8640 segments, so we chunk the request into hourly calls
// (max 360 segments/hour, well under the cap). Total = 24 round-trips
// for a full day, which takes ~1-2s on a LAN Frigate.
//
// v1.6.3: returns []MotionRange (was [][2]int64 in v1.6.0-v1.6.2).
// Each entry is pre-aggregated with MotionScore/SegmentCount/
// PeakObjects so the mobile app can render motion chips without
// re-fetching. Merging threshold is 2s (was 0s in v1.6.1, 10s in
// v1.6.0): 0s produced ~750 chips for 24h which was too many to
// render readably; 10s produced ~77 fat bars which the user said
// was "标红太宽了"; 2s is the smallest gap that's perceptually a
// "different motion event" (anything <2s of stillness reads as
// the same ongoing action), and yields ~120-180 chips/24h which
// fits nicely in a horizontal chip scroller.
//
// v1.6.3: in-process TTL cache. The mobile app hits this endpoint
// every time the user opens the day playlist, and the Frigate query
// is the slowest part (1-2s). We cache the result for 60s per
// (camera, day) pair — short enough that the user sees fresh data
// after re-opening the dialog, long enough to absorb double-taps
// and tab switches. Cache is keyed by (cameraName, after, before)
// so different windows don't collide.
func (c *FrigateClient) ListMotionRanges(ctx context.Context, cameraName string, after, before int64) ([]MotionRange, error) {
	if after <= 0 || before <= 0 || before <= after {
		return nil, fmt.Errorf("invalid time range: after=%d before=%d", after, before)
	}

	// v1.6.3: cache lookup. Key includes camera + window so concurrent
	// days for the same camera don't collide. TTL is short (60s) to
	// keep cache fresh while absorbing repeat requests.
	cacheKey := fmt.Sprintf("%s:%d:%d", cameraName, after, before)
	c.motionCacheMu.RLock()
	if entry, ok := c.motionCache[cacheKey]; ok {
		if time.Since(entry.fetchedAt) < 60*time.Second {
			c.motionCacheMu.RUnlock()
			return entry.ranges, nil
		}
	}
	c.motionCacheMu.RUnlock()

	// Chunk into hourly windows. Each hour = at most 360 10s segments,
	// safely under Frigate's 500-segment cap.
	const chunkSeconds int64 = 3600
	var allSegments []FrigateRecording
	for start := after; start < before; start += chunkSeconds {
		end := start + chunkSeconds
		if end > before {
			end = before
		}
		recs, err := c.ListRecordings(ctx, cameraName, start, end)
		if err != nil {
			// Best-effort: a single chunk failure shouldn't abort
			// the whole query. Log and continue — the dashboard
			// will show partial motion data with a gap.
			log.Printf("frigate: ListMotionRanges chunk [%d,%d) failed: %v", start, end, err)
			continue
		}
		// v1.8.2 debug: log per-chunk segment count + sample motion
		// values so we can see exactly what Frigate returns.
		motionSegs := 0
		for _, r := range recs {
			if r.Motion > 0 {
				motionSegs++
			}
		}
		log.Printf("[motion-ranges] chunk [%d,%d) cam=%s segs=%d with_motion=%d", start, end, cameraName, len(recs), motionSegs)
		allSegments = append(allSegments, recs...)
	}

	if len(allSegments) == 0 {
		log.Printf("[motion-ranges] no segments from Frigate for cam=%s [%d,%d) — returning nil", cameraName, after, before)
		c.cacheMotionRanges(cacheKey, nil)
		return nil, nil
	}

	// Sort by start time (Frigate returns newest-first by default).
	sort.Slice(allSegments, func(i, j int) bool {
		return allSegments[i].StartTime < allSegments[j].StartTime
	})

	// v1.6.3: 2s gap threshold. See func doc for the rationale
	// (0s = too many chips, 10s = too few fat bars, 2s = human
	// perceptual "same action" boundary).
	// v1.6.4 rev6: tier-aware gap threshold. The user said "将同种
	// 颜色的chip段多合并一些吧（尤其是绿色）". Previously every
	// segment used the same 2s gap, which split low-motion segments
	// (motion=1-2, green tier) into many tiny chips that cluttered
	// the fisheye scroller. Now low-motion segments use a 60s gap
	// (still perceived as the same "quiet period"), while mid/high
	// motion keeps the strict 2s gap to preserve precise event
	// boundaries. Tiers map to the client's color buckets:
	//   - LOW (teal/green):   seg.Motion <= 2
	//   - MID (amber):        seg.Motion 3..5
	//   - HIGH (orange):      seg.Motion 6..8
	//   - ALERT (red):        seg.Objects > 0 (AI detected)
	// We compute a per-segment tier to decide the gap, but only
	// EXTEND a range if both the current range's "dominant tier"
	// and the incoming segment's tier are LOW — otherwise high
	// motion events stay precisely bounded.
	// v1.6.5 rev7: user said "chip还是很多" after rev6. Bumped LOW
	// gap from 60s → 180s (3 minutes — a single "quiet period" chip
	// can now span 3 minutes of near-zero activity) and added a MID
	// tier (15s gap) so consecutive amber segments also merge into
	// a single chip. HIGH/ALERT keep the strict 2s gap so fast-
	// moving events stay precisely bounded. Effective merge
	// decisions: curTier and segTier must match exactly (LOW+LOW
	// uses 180s, MID+MID uses 15s); cross-tier escalation always
	// uses the strict 2s gap to avoid blurring event boundaries.
	const (
		mergeGapLowSeconds     int64 = 180 // low-motion: 3min gap (was 60s)
		mergeGapMidSeconds     int64 = 15  // mid-motion: 15s gap (NEW)
		mergeGapDefaultSeconds int64 = 2   // high/alert: strict 2s gap
	)
	// tier returns 0=LOW, 1=MID, 2=HIGH/ALERT. Two segments with
	// the same tier can use that tier's gap; different tiers use
	// the strict 2s gap.
	tierOf := func(motion, objects int) int {
		if objects > 0 {
			return 2 // ALERT
		}
		if motion <= 2 {
			return 0 // LOW
		}
		if motion <= 5 {
			return 1 // MID
		}
		return 2 // HIGH
	}
	gapForTier := func(t int) int64 {
		switch t {
		case 0:
			return mergeGapLowSeconds
		case 1:
			return mergeGapMidSeconds
		default:
			return mergeGapDefaultSeconds
		}
	}
	var ranges []MotionRange
	var curStart, curEnd, curScore, curCount, curPeak int64
	curTier := 2 // dominant tier of the current range; 2 = HIGH/ALERT (strict)
	inRange := false
	for _, seg := range allSegments {
		if seg.Motion <= 0 {
			continue
		}
		segStart := seg.StartTime
		segEnd := int64(seg.EndTime)
		segTier := tierOf(int(seg.Motion), int(seg.Objects))
		if !inRange {
			curStart, curEnd = segStart, segEnd
			curScore = int64(seg.Motion)
			curCount = 1
			curPeak = int64(seg.Objects)
			curTier = segTier
			inRange = true
			continue
		}
		// Same tier: use that tier's gap (LOW 180s, MID 15s).
		// Cross-tier: use strict 2s gap to preserve boundaries.
		effectiveGap := mergeGapDefaultSeconds
		if curTier == segTier {
			effectiveGap = gapForTier(segTier)
		}
		if segStart-curEnd <= effectiveGap {
			// Extend the current range.
			curEnd = segEnd
			curScore += int64(seg.Motion)
			curCount++
			if int64(seg.Objects) > curPeak {
				curPeak = int64(seg.Objects)
			}
			// If the incoming segment escalates tier (e.g. range
			// was LOW but incoming is MID/HIGH/ALERT), the range is
			// no longer "dominated by low" — promote the dominant
			// tier to the higher value so subsequent merges use the
			// stricter gap. Demotion never happens (a HIGH range
			// stays HIGH even if a stray LOW seg follows).
			if segTier > curTier {
				curTier = segTier
			}
		} else {
			// Gap too large — flush the current range and start a new one.
			ranges = append(ranges, MotionRange{
				StartUnix:    curStart,
				EndUnix:      curEnd,
				Duration:     curEnd - curStart,
				MotionScore:  int(curScore),
				SegmentCount: int(curCount),
				PeakObjects:  int(curPeak),
			})
			curStart, curEnd = segStart, segEnd
			curScore = int64(seg.Motion)
			curCount = 1
			curPeak = int64(seg.Objects)
			curTier = segTier
		}
	}
	if inRange {
		ranges = append(ranges, MotionRange{
			StartUnix:    curStart,
			EndUnix:      curEnd,
			Duration:     curEnd - curStart,
			MotionScore:  int(curScore),
			SegmentCount: int(curCount),
			PeakObjects:  int(curPeak),
		})
	}

	c.cacheMotionRanges(cacheKey, ranges)
	return ranges, nil
}

// cacheMotionRanges stores a ListMotionRanges result under cacheKey.
// Caller already holds no lock — we acquire the write lock here.
func (c *FrigateClient) cacheMotionRanges(cacheKey string, ranges []MotionRange) {
	c.motionCacheMu.Lock()
	// Lazy-init the map so we don't allocate until first use.
	if c.motionCache == nil {
		c.motionCache = make(map[string]motionCacheEntry, 8)
	}
	c.motionCache[cacheKey] = motionCacheEntry{
		ranges:    ranges,
		fetchedAt: time.Now(),
	}
	// Opportunistic GC: if the cache has grown past 32 entries (e.g.
	// the user browsed many days), evict the oldest half. Prevents
	// unbounded growth from multi-day browsing sessions.
	if len(c.motionCache) > 32 {
		type kv struct {
			key string
			t   time.Time
		}
		entries := make([]kv, 0, len(c.motionCache))
		for k, v := range c.motionCache {
			entries = append(entries, kv{k, v.fetchedAt})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].t.Before(entries[j].t) })
		for i := 0; i < len(entries)-16; i++ {
			delete(c.motionCache, entries[i].key)
		}
	}
	c.motionCacheMu.Unlock()
}
