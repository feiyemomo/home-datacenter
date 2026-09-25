package mqtt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	"home-datacenter-api/internal/camera"
	"home-datacenter-api/internal/device"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/maintenance"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/vision"
)

// minAlertConfidence is the minimum detection confidence (0–1)
// required for a camera motion event to be forwarded to the EventBus.
// Detections below this threshold are logged but not published,
// preventing low-score false alerts from reaching the app and
// automation engine.
// v1.6.39: introduced at 0.80.
// v1.7.2: lowered to 0.78 per operator tuning. Also tightened the
// guard so that confidence==0 (Frigate "new" events where neither
// top_score nor score has been populated yet) is rejected rather
// than silently passed through — previously the `confidence > 0`
// pre-condition let zero-confidence events bypass the filter.
const minAlertConfidence = 0.78

// visionTask encapsulates a pending image analysis request.
type visionTask struct {
	eventID   string
	cameraID  uint
	slug      string
	guardMode string
}

// Handler dispatches incoming MQTT messages to the EventBus and
// DeviceManager. It is stateless beyond the references it holds.
//
// In addition to the home-datacenter/ namespace topics, the handler
// also subscribes to frigate/events to receive object detection
// alerts from the Frigate NVR and re-publish them on the EventBus
// as camera.motion events.
type Handler struct {
	bus           *eventbus.Bus
	manager       *device.Manager
	client        pahomqtt.Client // set by Client.Start() via OnConnect
	slugLookup    SlugLookup
	guardProvider GuardModeProvider
	visionClient  *vision.Client
	frigateClient *camera.FrigateClient
	visionQueue   chan visionTask
	visionOnce    sync.Once
}

// GuardModeProvider returns the active security arming mode.
type GuardModeProvider interface {
	GetMode() string
}

// SlugLookup resolves a Frigate camera slug to a camera ID.
// Returns (0, false) if no matching camera is found.
type SlugLookup interface {
	LookupByFrigateSlug(slug string) (uint, bool)
	AllFrigateSlugs() []string
}

// NewHandler creates a Handler wired to the given EventBus,
// device Manager, and optional slug lookup.
func NewHandler(bus *eventbus.Bus, manager *device.Manager, slugLookup SlugLookup) *Handler {
	if slugLookup == nil {
		slugLookup = &noopSlugLookup{}
	}
	h := &Handler{bus: bus, manager: manager, slugLookup: slugLookup}
	if bus != nil {
		bus.Subscribe(eventbus.TopicSecurityGuardMode, func(e eventbus.Event) {
			var p struct {
				Mode string `json:"mode"`
			}
			if err := json.Unmarshal(e.Payload, &p); err == nil && p.Mode != "" {
				h.SyncFrigateDetection(p.Mode)
			}
		})
	}
	return h
}

// SetGuardProvider attaches a GuardModeProvider to the handler.
func (h *Handler) SetGuardProvider(gp GuardModeProvider) {
	h.guardProvider = gp
}

// SetVision attaches vision AI client and Frigate client for intelligent frame analysis.
func (h *Handler) SetVision(vc *vision.Client, fc *camera.FrigateClient) {
	h.visionClient = vc
	h.frigateClient = fc
	if vc != nil && fc != nil {
		h.visionOnce.Do(func() {
			h.visionQueue = make(chan visionTask, 2)
			go h.visionWorker()
		})
	}
}

// noopSlugLookup is a fallback that never resolves.
type noopSlugLookup struct{}

func (n *noopSlugLookup) LookupByFrigateSlug(slug string) (uint, bool) { return 0, false }
func (n *noopSlugLookup) AllFrigateSlugs() []string                    { return nil }

// OnMessage is the paho.mqtt message callback. It inspects the topic
// and routes the payload to the appropriate downstream consumers.
func (h *Handler) OnMessage(client pahomqtt.Client, msg pahomqtt.Message) {
	topic := msg.Topic()
	payload := msg.Payload()

	log.Printf("mqtt: rx %s = %s", topic, string(payload))

	// Handle Frigate events on the frigate/events topic.
	if topic == "frigate/events" {
		h.handleFrigateEvent(payload)
		return
	}

	parsed, ok := ParseTopic(topic)
	if !ok {
		log.Printf("mqtt: unparseable topic %q", topic)
		return
	}

	switch parsed.Domain {
	case "devices":
		h.handleDeviceMessage(parsed, payload)
	case "cameras":
		h.handleCameraMessage(parsed, payload)
	case "system":
		// System topics from devices are uncommon; pass through.
		h.bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicSystemBroadcast,
			Payload: payload,
			Source:  eventbus.SourceMQTT,
		})
	}
}

// handleCameraMessage processes messages under "cameras/{id}/*".
// Today we only care about `event` (motion/AI). Anything else is
// logged and dropped — cameras don't have a "status" topic of their
// own; the platform TCP-probes them and publishes device.status.
func (h *Handler) handleCameraMessage(pt ParsedTopic, payload []byte) {
	switch pt.Subtype {
	case "event":
		h.handleCameraEvent(pt.ID, payload)
	default:
		log.Printf("mqtt: unknown camera subtype %q for camera %d", pt.Subtype, pt.ID)
	}
}

// handleCameraEvent ingests a motion/AI event and re-publishes it on
// the EventBus so the App / WebSocket layer can react. We
// canonicalise the JSON to ensure subscribers can always json.Decode.
func (h *Handler) handleCameraEvent(cameraID uint, payload []byte) {
	var ev struct {
		Event      string  `json:"event"`
		Confidence float64 `json:"confidence,omitempty"`
		TS         int64   `json:"ts"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		log.Printf("mqtt: invalid camera event payload from %d: %q", cameraID, payload)
		return
	}
	// v1.6.39: apply the confidence threshold. v1.7.2: reject
	// confidence==0 too (previously passed through due to the
	// `> 0` guard). A zero-confidence event carries no real
	// detection signal and should not wake the app.
	if ev.Confidence < minAlertConfidence {
		log.Printf("mqtt: camera event below confidence threshold: camera=%d confidence=%.2f",
			cameraID, ev.Confidence)
		return
	}

	if ev.TS == 0 {
		ev.TS = time.Now().Unix()
	}
	canonical, _ := json.Marshal(struct {
		DeviceID   uint    `json:"device_id"`
		Type       string  `json:"type"`
		Event      string  `json:"event"`
		Confidence float64 `json:"confidence,omitempty"`
		TS         int64   `json:"ts"`
	}{cameraID, "camera", ev.Event, ev.Confidence, ev.TS})
	h.bus.Publish(eventbus.Event{
		Topic:   eventbus.TopicDeviceEvent,
		Payload: canonical,
		Source:  eventbus.SourceMQTT,
	})
}

// handleFrigateEvent processes a message from the frigate/events MQTT
// topic. Frigate publishes these when its AI detector finds a tracked
// object (person, car, dog, etc.) in a camera's video feed.
//
// Payload format (simplified):
//
//	{
//	  "type": "new" | "update" | "end",
//	  "before": { "camera": "front_door", "label": "person", ... },
//	  "after":  { "camera": "front_door", "label": "person",
//	              "current_zones": ["driveway"], "top_score": 0.96, ... }
//	}
//
// We only react to "new" events (first detection) to avoid flooding
// the EventBus with updates. The event is translated into a
// camera.motion EventBus event with the Frigate camera slug mapped
// back to a home-api camera ID via the slugLookup interface.
func (h *Handler) handleFrigateEvent(payload []byte) {
	var frigEv struct {
		Type   string `json:"type"`
		Before struct {
			Camera string  `json:"camera"`
			Label  string  `json:"label"`
			Score  float64 `json:"score"`
		} `json:"before"`
		After struct {
			ID            string   `json:"id"`
			Camera        string   `json:"camera"`
			Label         string   `json:"label"`
			TopScore      float64  `json:"top_score"`
			Score         float64  `json:"score"`
			CurrentZones  []string `json:"current_zones"`
			EnteredZones  []string `json:"entered_zones"`
			FalsePositive bool     `json:"false_positive"`
			Stationary    bool     `json:"stationary"`
			StartTime     float64  `json:"start_time"`
			EndTime       *float64 `json:"end_time"`
			HasSnapshot   bool     `json:"has_snapshot"`
			HasClip       bool     `json:"has_clip"`
		} `json:"after"`
	}
	if err := json.Unmarshal(payload, &frigEv); err != nil {
		log.Printf("mqtt: invalid frigate event payload: %q", payload)
		return
	}

	// Only react to "new" events (initial detection) to avoid
	// flooding. "update" events fire on every zone change or
	// snapshot improvement; "end" fires when the object leaves.
	if frigEv.Type != "new" {
		return
	}

	slug := frigEv.After.Camera
	cameraID, ok := h.slugLookup.LookupByFrigateSlug(slug)
	if !ok {
		log.Printf("mqtt: frigate event for unknown camera slug %q", slug)
		return
	}

	// Skip false positives — Frigate sends these but they are not
	// real detections.
	if frigEv.After.FalsePositive {
		return
	}

	confidence := frigEv.After.TopScore
	if confidence == 0 {
		confidence = frigEv.After.Score
	}

	// v1.6.39: only forward detections above the confidence
	// threshold to avoid flooding the app and automation engine
	// with low-score false alerts (shadows, insects, motion blur).
	// v1.7.2: also reject confidence==0 (Frigate "new" events
	// where top_score/score are not yet populated). Previously
	// the `> 0` guard let these through, producing spurious
	// low-confidence pushes — the exact "73% alerts still push"
	// symptom.
	if confidence < minAlertConfidence {
		log.Printf("mqtt: frigate detection below confidence threshold: camera=%s label=%s confidence=%.2f",
			slug, frigEv.After.Label, confidence)
		return
	}

	ts := int64(frigEv.After.StartTime)
	if ts == 0 {
		ts = time.Now().Unix()
	}

	guardMode := model.GuardModeArmedAway
	if h.guardProvider != nil {
		guardMode = h.guardProvider.GetMode()
	}
	switch guardMode {
	case "away", model.GuardModeArmedAway:
		guardMode = model.GuardModeArmedAway
	case "home", "stay", model.GuardModeArmedHome:
		guardMode = model.GuardModeArmedHome
	case "disarm", "off", model.GuardModeDisarmed:
		guardMode = model.GuardModeDisarmed
	}

	muted := (guardMode == model.GuardModeDisarmed)

	canonical, _ := json.Marshal(struct {
		EventID     string   `json:"event_id"`
		CameraID    uint     `json:"camera_id"`
		Type        string   `json:"type"`
		Label       string   `json:"label"`
		Confidence  float64  `json:"confidence"`
		Zones       []string `json:"zones,omitempty"`
		HasSnapshot bool     `json:"has_snapshot"`
		HasClip     bool     `json:"has_clip"`
		Muted       bool     `json:"muted"`
		TS          int64    `json:"ts"`
	}{
		EventID:     frigEv.After.ID,
		CameraID:    cameraID,
		Type:        "detection",
		Label:       frigEv.After.Label,
		Confidence:  confidence,
		Zones:       frigEv.After.CurrentZones,
		HasSnapshot: frigEv.After.HasSnapshot,
		HasClip:     frigEv.After.HasClip,
		Muted:       muted,
		TS:          ts,
	})

	h.bus.Publish(eventbus.Event{
		Topic:    eventbus.TopicCameraMotion,
		Source:   eventbus.SourceMQTT,
		Severity: eventbus.SeverityInfo,
		Payload:  canonical,
	})

	log.Printf("mqtt: frigate detection: camera=%s id=%d label=%s confidence=%.2f guard=%s muted=%v",
		slug, cameraID, frigEv.After.Label, confidence, guardMode, muted)

	// Trigger asynchronous Vision AI pipeline:
	// - 撤防免打扰 (disarmed): 完全跳过 AI 视觉检测，保护 CPU 并不产生告警
	// - 离家布防 (armed_away): 仅开启人物识别（任何人员均为入侵）
	// - 在家守护 (armed_home): 开启人物识别 + 人脸识别 + 姿态跌倒检测
	if !muted && frigEv.After.Label == "person" && frigEv.After.ID != "" && h.visionQueue != nil {
		select {
		case h.visionQueue <- visionTask{
			eventID:   frigEv.After.ID,
			cameraID:  cameraID,
			slug:      slug,
			guardMode: guardMode,
		}:
		default:
			log.Printf("vision: queue full, skipping frame analysis for event %s to protect CPU", frigEv.After.ID)
		}
	}
}

// visionWorker runs a single-threaded background worker for vision analysis
// ensuring bounded CPU utilization on Intel Celeron J4125.
func (h *Handler) visionWorker() {
	log.Printf("vision: background analysis worker started (bounded concurrency 1)")
	for task := range h.visionQueue {
		h.processVisionTask(task)
	}
}

// processVisionTask executes the multi-tier vision analysis with dynamic CPU gating.
func (h *Handler) processVisionTask(task visionTask) {
	ts := time.Now().Unix()

	// 离家布防模式 (armed_away):
	// 任何人在屋内均为异常闯入！无需耗费算力跑人脸对比，直接触发入侵人员告警
	if task.guardMode == model.GuardModeArmedAway {
		payload, _ := json.Marshal(map[string]any{
			"event_id":    task.eventID,
			"camera_id":   task.cameraID,
			"camera_slug": task.slug,
			"persons":     []string{"离家布防异常入侵人员"},
			"ts":          ts,
		})
		h.bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicCameraPersonRecognized,
			Source:   eventbus.SourceSystem,
			Severity: eventbus.SeverityCritical,
			Payload:  payload,
		})
		log.Printf("vision: AWAY MODE ALERT: person detected on camera %s (event %s)!", task.slug, task.eventID)
		return
	}

	// 在家守护模式 (armed_home):
	// Pre-flight CPU usage load gating to protect Celeron J4125
	cpu := maintenance.SampleCPU()
	if cpu >= 80.0 {
		log.Printf("vision: host CPU is high (%.1f%% >= 80%%), skipping vision analysis for event %s (circuit break)", cpu, task.eventID)
		return
	}

	detectFace := true
	detectPose := true
	if cpu >= 60.0 {
		detectPose = false // Degraded mode: run face recognition only, skip heavy 17-point pose estimation
		log.Printf("vision: host CPU is moderate (%.1f%% >= 60%%), degrading to face recognition only (pose skipped)", cpu)
	}

	// Give Frigate a short moment (150ms) to ensure the snapshot JPEG is fully written to disk
	time.Sleep(150 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	rc, _, err := h.frigateClient.EventSnapshot(ctx, task.eventID)
	if err != nil {
		cancel()
		log.Printf("vision: failed to fetch snapshot for event %s: %v", task.eventID, err)
		return
	}
	imgBytes, err := io.ReadAll(rc)
	rc.Close()
	cancel()
	if err != nil {
		log.Printf("vision: failed to read snapshot bytes for event %s: %v", task.eventID, err)
		return
	}

	analyzeCtx, analyzeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer analyzeCancel()

	imgB64 := base64.StdEncoding.EncodeToString(imgBytes)
	req := vision.AnalyzeRequest{
		ImageBase64: imgB64,
		DetectFace:  detectFace,
		DetectPose:  detectPose,
	}
	res, err := h.visionClient.Analyze(analyzeCtx, req)
	if err != nil {
		log.Printf("vision: analyze error for event %s: %v", task.eventID, err)
		return
	}

	ts = time.Now().Unix()

	// Emit person recognized event if any registered face matched
	if len(res.Data.MatchedPersons) > 0 {
		payload, _ := json.Marshal(map[string]any{
			"event_id":    task.eventID,
			"camera_id":   task.cameraID,
			"camera_slug": task.slug,
			"persons":     res.Data.MatchedPersons,
			"faces":       res.Data.Faces,
			"ts":          ts,
		})
		h.bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicCameraPersonRecognized,
			Source:   eventbus.SourceSystem,
			Severity: eventbus.SeverityInfo,
			Payload:  payload,
		})
		log.Printf("vision: person recognized on camera %s: %v", task.slug, res.Data.MatchedPersons)
	}

	// Emit fall detected event if fall is detected
	if res.Data.HasFall {
		payload, _ := json.Marshal(map[string]any{
			"event_id":    task.eventID,
			"camera_id":   task.cameraID,
			"camera_slug": task.slug,
			"poses":       res.Data.Poses,
			"ts":          ts,
		})
		h.bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicCameraFallDetected,
			Source:   eventbus.SourceSystem,
			Severity: eventbus.SeverityCritical,
			Payload:  payload,
		})
		log.Printf("vision: WARNING: fall detected on camera %s (event %s)!", task.slug, task.eventID)
	}
}

// handleDeviceMessage processes messages under "devices/{id}/*".
func (h *Handler) handleDeviceMessage(pt ParsedTopic, payload []byte) {
	switch pt.Subtype {
	case "status":
		h.handleStatus(pt.ID, payload)
	case "telemetry":
		h.handleTelemetry(pt.ID, payload)
	case "events":
		h.handleEvents(pt.ID, payload)
	default:
		log.Printf("mqtt: unknown device subtype %q for device %d", pt.Subtype, pt.ID)
	}
}

// handleStatus processes a device status message. Expected payload:
//
//	{"status":"online|offline|heartbeat","ts":1234567890}
//
// Real-world devices and simulators sometimes publish unquoted keys
// (e.g. {status:online,ts:1234567890}), which Go's strict encoding/json
// rejects. To be tolerant, we first try a strict decode; if that fails
// we look for the literal `status:<value>` token by hand. Anything
// truly malformed is still rejected — we just want a wider net for
// half-correct JSON.
func (h *Handler) handleStatus(deviceID uint, payload []byte) {
	status, ts, ok := parseStatusPayload(payload)
	if !ok {
		log.Printf("mqtt: invalid status payload from device %d: %q", deviceID, payload)
		return
	}

	switch status {
	case "online":
		h.manager.SetOnline(deviceID, "")
	case "offline":
		h.manager.SetOffline(deviceID)
	case "heartbeat":
		h.manager.Heartbeat(deviceID)
	default:
		log.Printf("mqtt: unknown status %q from device %d", status, deviceID)
		return
	}

	// v1.6.36: do NOT re-publish device.status here. The Manager's
	// SetOnline / SetOffline / Heartbeat methods already publish
	// device.status on the EventBus (with the correct SourceSystem
	// and a wasOffline / wasOnline transition guard). Re-publishing
	// here caused two bugs:
	//   1. Every MQTT heartbeat (status="heartbeat") wrote a
	//      spurious "设备 #N heartbeat" SystemLog row — the
	//      Subscriber's translateStatus() didn't recognise
	//      "heartbeat" and surfaced the raw string.
	//   2. Every real online/offline transition was published
	//      twice (once by the Manager with SourceSystem, once
	//      here with SourceMQTT), doubling every SystemLog entry.
	// The ts value is still consumed by parseStatusPayload above
	// and forwarded into the Manager via the heartbeat/online
	// path (LastSeen), so dropping the re-publish loses nothing.
	_ = ts
}

// parseStatusPayload extracts (status, ts) from a status message.
// Returns ok=false if neither strict nor lenient parsing can recover
// a status string.
func parseStatusPayload(payload []byte) (string, int64, bool) {
	// 1. Strict path: well-formed JSON.
	var s struct {
		Status string `json:"status"`
		TS     int64  `json:"ts"`
	}
	if err := json.Unmarshal(payload, &s); err == nil && s.Status != "" {
		return s.Status, s.TS, true
	}

	// 2. Lenient path: tolerate unquoted keys. Strip everything that
	// is not a JSON-meaningful character and re-quote keys.
	fixed := lenientJSON(payload)
	if fixed != nil {
		if err := json.Unmarshal(fixed, &s); err == nil && s.Status != "" {
			return s.Status, s.TS, true
		}
	}

	// 3. Last-ditch: regex out the status value, ignore everything else.
	// Matches status followed by optional ws and a value that's either
	// "..." (quoted) or a bare identifier.
	re := regexp.MustCompile(`(?i)\bstatus\b\s*[:=]\s*"?([A-Za-z_]+)"?`)
	m := re.FindSubmatch(payload)
	if len(m) >= 2 {
		return string(m[1]), 0, true
	}
	return "", 0, false
}

// lenientJSON converts an unquoted-key JSON object into one whose keys
// are quoted. It is intentionally narrow: it only handles the
// {key:value,key:value} shape that hand-built / naive publishers emit.
//
// Example input:  {status:online,ts:1234567890}
// Example output: {"status":"online","ts":1234567890}
//
// Returns nil if the input is not a recognisable object or if it is
// already well-formed (caller should retry strict path).
func lenientJSON(in []byte) []byte {
	s := strings.TrimSpace(string(in))
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return nil
	}
	inner := s[1 : len(s)-1]

	// Split on top-level commas (we don't need to handle nested arrays
	// or objects — status payloads are flat).
	depth := 0
	var parts []string
	start := 0
	inStr := false
	esc := false
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if esc {
			esc = false
			continue
		}
		if c == '\\' && inStr {
			esc = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		switch c {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, inner[start:i])
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return nil
	}
	parts = append(parts, inner[start:])

	// If every key is already quoted, the input was well-formed; the
	// caller's strict path will have handled it. Bail so we don't
	// re-emit a possibly-broken rewrite.
	alreadyStrict := true
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		colon := strings.IndexAny(p, ":")
		if colon < 0 {
			return nil
		}
		key := strings.TrimSpace(p[:colon])
		if !(len(key) >= 2 && key[0] == '"' && key[len(key)-1] == '"') {
			alreadyStrict = false
			break
		}
	}
	if alreadyStrict {
		return nil
	}

	var b strings.Builder
	b.WriteByte('{')
	first := true
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		colon := strings.IndexAny(p, ":")
		if colon < 0 {
			return nil
		}
		key := strings.TrimSpace(p[:colon])
		val := strings.TrimSpace(p[colon+1:])

		// Re-quote the key.
		key = strings.Trim(key, `"`)
		key = `"` + key + `"`

		// Re-quote the value if it isn't a JSON literal.
		if !isJSONLiteral(val) {
			val = strings.Trim(val, `"`)
			val = `"` + val + `"`
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(key)
		b.WriteByte(':')
		b.WriteString(val)
	}
	b.WriteByte('}')
	return []byte(b.String())
}

// isJSONLiteral reports whether a JSON value is a literal (number, bool,
// null) and therefore does not need quoting.
func isJSONLiteral(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return true
	}
	if v == "null" || v == "true" || v == "false" {
		return true
	}
	// Number: optional sign, digits, optional fraction/exponent.
	for i, r := range v {
		if i == 0 && (r == '-' || r == '+') {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '.' || r == 'e' || r == 'E' || r == '+' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// handleTelemetry processes a device telemetry message. Payload is
// opaque (device-defined JSON); the server just forwards it.
func (h *Handler) handleTelemetry(deviceID uint, payload []byte) {
	// A telemetry message also counts as a heartbeat.
	h.manager.Heartbeat(deviceID)

	h.bus.Publish(eventbus.Event{
		Topic:   eventbus.TopicDeviceTelemetry,
		Payload: payload,
		Source:  eventbus.SourceMQTT,
	})
}

// handleEvents processes a device events message. Forwarded as-is.
func (h *Handler) handleEvents(deviceID uint, payload []byte) {
	h.manager.Heartbeat(deviceID)

	h.bus.Publish(eventbus.Event{
		Topic:   eventbus.TopicDeviceCommand,
		Payload: payload,
		Source:  eventbus.SourceMQTT,
	})
}

// OnConnect is called by paho when the client (re)connects to the
// broker. It re-subscribes to all server-side topics.
func (h *Handler) OnConnect(client pahomqtt.Client) {
	log.Println("mqtt: connected to broker, subscribing...")

	subs := []struct {
		filter string
		qos    byte
	}{
		{SubscribeDeviceStatus(), 1},
		{SubscribeDeviceTelemetry(), 1},
		{SubscribeDeviceEvents(), 1},
		{SubscribeCameraEvent(), 1},
		// Frigate NVR publishes object detection events on
		// frigate/events. Subscribe here so the handler can
		// translate them into EventBus camera.motion events.
		{"frigate/events", 1},
	}

	for _, s := range subs {
		if token := client.Subscribe(s.filter, s.qos, h.OnMessage); token.Wait() && token.Error() != nil {
			log.Printf("mqtt: subscribe %q failed: %v", s.filter, token.Error())
		} else {
			log.Printf("mqtt: subscribed %q (QoS %d)", s.filter, s.qos)
		}
	}

	if h.guardProvider != nil {
		h.SyncFrigateDetection(h.guardProvider.GetMode())
	}
}

// SyncFrigateDetection synchronizes Frigate's object detection toggle
// with the system security mode. Disarmed shuts down detection to save
// 100% CPU and eliminate nuisance events. Armed modes turn it ON.
func (h *Handler) SyncFrigateDetection(guardMode string) {
	if h.slugLookup == nil {
		return
	}
	payload := "ON"
	switch guardMode {
	case "disarm", "off", model.GuardModeDisarmed:
		payload = "OFF"
	}
	slugs := h.slugLookup.AllFrigateSlugs()
	for _, slug := range slugs {
		topic := fmt.Sprintf("frigate/%s/detect/set", slug)
		if err := h.Publish(topic, payload, 1); err != nil {
			log.Printf("mqtt: failed to set frigate detect for %s: %v", slug, err)
		} else {
			log.Printf("mqtt: set frigate detect for %s -> %s (guardMode=%s)", slug, payload, guardMode)
		}
	}
}

// OnDisconnect is called by paho when the client loses the broker
// connection. Paho auto-reconnects; we just log.
func (h *Handler) OnDisconnect(client pahomqtt.Client, err error) {
	log.Printf("mqtt: disconnected from broker: %v", err)
}

// PublishDeviceCommand sends a command to a specific device via MQTT.
// Used by the WebSocket layer (via a service) to control devices.
func (h *Handler) PublishDeviceCommand(deviceID uint, command string, params interface{}) error {
	payload := struct {
		Command string      `json:"command"`
		Params  interface{} `json:"params,omitempty"`
		TS      int64       `json:"ts"`
	}{
		Command: command,
		Params:  params,
		TS:      time.Now().Unix(),
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	return h.publish(DeviceCommand(deviceID), data, 1)
}

// PublishUserNotification pushes a notification to a user's apps.
func (h *Handler) PublishUserNotification(userID uint, title, body string) error {
	payload := struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		TS    int64  `json:"ts"`
	}{
		Title: title,
		Body:  body,
		TS:    time.Now().Unix(),
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	return h.publish(UserNotifications(userID), data, 1)
}

// PublishBroadcast sends a system-wide broadcast.
func (h *Handler) PublishBroadcast(message string) error {
	payload := struct {
		Message string `json:"message"`
		TS      int64  `json:"ts"`
	}{
		Message: message,
		TS:      time.Now().Unix(),
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	return h.publish(SystemBroadcast(), data, 1)
}

// Publish sends a raw message to the given topic. Used by the web
// dashboard's MQTT debug page.
func (h *Handler) Publish(topic string, payload string, qos byte) error {
	return h.publish(topic, []byte(payload), qos)
}

// publish is the low-level publish helper. It uses the client stored
// on the handler (set by Client.Connect).
func (h *Handler) publish(topic string, payload []byte, qos byte) error {
	if h.client == nil {
		return ErrNotConnected
	}
	token := h.client.Publish(topic, qos, false, payload)
	token.Wait()
	return token.Error()
}
