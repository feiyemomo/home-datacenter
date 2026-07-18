package camera

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

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
}

// NewFrigateClient returns a client with a 30s timeout (config save
// can take 5-10s while Frigate reloads ffmpeg pipelines for the
// changed cameras; under load, slower).
func NewFrigateClient(frigateBase, go2rtcBase string) *FrigateClient {
	return &FrigateClient{
		FrigateBase: frigateBase,
		Go2rtcBase:  go2rtcBase,
		HC:          &http.Client{Timeout: 30 * time.Second},
	}
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
	Inputs []FrigateInput `yaml:"inputs" json:"inputs"`
}

type FrigateInput struct {
	Path  string   `yaml:"path" json:"path"`
	Roles []string `yaml:"roles" json:"roles"`
}

type FrigateDetect struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
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
func (c *FrigateClient) PushConfig(ctx context.Context, cameras []FrigateCameraConfig, go2rtcStreams map[string]string, requiresRestart bool) error {
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
		"record": map[string]any{
			"enabled": true,
			"continuous": map[string]any{
				"days": 7,
			},
			"motion": map[string]any{
				"days": 7,
			},
		},
	}
	if len(go2rtcStreams) > 0 {
		partial["go2rtc"] = map[string]any{
			"streams": go2rtcStreams,
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
// hour-bounded segments; each entry covers one hour (or part
// thereof) of continuous video.
//
// NOTE: Frigate's API returns end_time and duration as floats
// (e.g. 1784306209.996875), not ints. We use float64 to avoid
// JSON unmarshal errors. Callers that need int seconds should
// cast explicitly.
type FrigateRecording struct {
	Camera    string  `json:"camera"`
	StartTime int64   `json:"start_time"`       // unix seconds
	EndTime   float64 `json:"end_time"`         // unix seconds (float)
	Duration  float64 `json:"duration"`         // seconds (float)
	HasClip   bool    `json:"has_clip"`
	HasSnap   bool    `json:"has_snapshot"`
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
