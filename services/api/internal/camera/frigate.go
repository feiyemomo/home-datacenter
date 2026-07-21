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
	"sort"
	"strconv"
	"sync"
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
	// v1.6.3: in-process TTL cache for ListMotionRanges. Keyed by
	// "<camera>:<after>:<before>". See cacheMotionRanges for the
	// eviction policy. RWMutex because reads (cache hits) far
	// outnumber writes (cache misses).
	motionCache   map[string]motionCacheEntry
	motionCacheMu sync.RWMutex
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
			"quality":   70,
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

// ListEvents queries Frigate for recent detection events.
// Frigate's GET /api/events returns events sorted newest-first.
// The limit parameter caps the number of results (0 = server default).
//
// When includeThumbnails is true, Frigate returns a small base64-encoded
// JPEG thumbnail for each event (typically 1-3KB). These are used by
// the dashboard's alert list for instant preview without a second
// round-trip per event.
func (c *FrigateClient) ListEvents(ctx context.Context, limit int, includeThumbnails bool) ([]FrigateEvent, error) {
	u := c.FrigateBase + "/api/events"
	params := url.Values{}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	if includeThumbnails {
		params.Set("include_thumbnails", "1")
	} else {
		params.Set("include_thumbnails", "0")
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

// EventSnapshot fetches the full-resolution snapshot JPEG for a given
// Frigate event ID. Frigate serves these at GET /api/events/<id>/snapshot.jpg.
// The returned contentType is image/jpeg (or whatever Frigate returns).
// Caller is responsible for closing the returned ReadCloser.
func (c *FrigateClient) EventSnapshot(ctx context.Context, eventID string) (io.ReadCloser, string, error) {
	u := fmt.Sprintf("%s/api/events/%s/snapshot.jpg", c.FrigateBase, url.PathEscape(eventID))
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
