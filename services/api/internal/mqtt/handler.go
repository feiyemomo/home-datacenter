package mqtt

import (
	"encoding/json"
	"log"
	"regexp"
	"strings"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	"home-datacenter-api/internal/device"
	"home-datacenter-api/internal/eventbus"
)

// Handler dispatches incoming MQTT messages to the EventBus and
// DeviceManager. It is stateless beyond the references it holds.
type Handler struct {
	bus     *eventbus.Bus
	manager *device.Manager
	client  pahomqtt.Client // set by Client.Start() via OnConnect
}

// NewHandler creates a Handler wired to the given EventBus and
// device Manager.
func NewHandler(bus *eventbus.Bus, manager *device.Manager) *Handler {
	return &Handler{bus: bus, manager: manager}
}

// OnMessage is the paho.mqtt message callback. It inspects the topic
// and routes the payload to the appropriate downstream consumers.
func (h *Handler) OnMessage(client pahomqtt.Client, msg pahomqtt.Message) {
	topic := msg.Topic()
	payload := msg.Payload()

	log.Printf("mqtt: rx %s = %s", topic, string(payload))

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

	// Re-publish on the EventBus so WebSocket subscribers see the update.
	// Re-serialise as canonical JSON (the original may be loosely formatted)
	// so downstream consumers always get valid JSON.
	canonical, _ := json.Marshal(struct {
		DeviceID uint   `json:"device_id"`
		Status   string `json:"status"`
		TS       int64  `json:"ts"`
	}{deviceID, status, ts})

	h.bus.Publish(eventbus.Event{
		Topic:   eventbus.TopicDeviceStatus,
		Payload: canonical,
		Source:  eventbus.SourceMQTT,
	})
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
	}

	for _, s := range subs {
		if token := client.Subscribe(s.filter, s.qos, h.OnMessage); token.Wait() && token.Error() != nil {
			log.Printf("mqtt: subscribe %q failed: %v", s.filter, token.Error())
		} else {
			log.Printf("mqtt: subscribed %q (QoS %d)", s.filter, s.qos)
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
