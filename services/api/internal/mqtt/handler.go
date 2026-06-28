package mqtt

import (
	"encoding/json"
	"log"
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

	parsed, ok := ParseTopic(topic)
	if !ok {
		log.Printf("mqtt: unparseable topic %q", topic)
		return
	}

	switch parsed.Domain {
	case "devices":
		h.handleDeviceMessage(parsed, payload)
	case "system":
		// System topics from devices are uncommon; pass through.
		h.bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicSystemBroadcast,
			Payload: payload,
			Source:  eventbus.SourceMQTT,
		})
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
//   {"status":"online|offline|heartbeat","ts":1234567890}
func (h *Handler) handleStatus(deviceID uint, payload []byte) {
	var s struct {
		Status string `json:"status"`
		TS     int64  `json:"ts"`
	}
	if err := json.Unmarshal(payload, &s); err != nil {
		log.Printf("mqtt: invalid status payload from device %d: %v", deviceID, err)
		return
	}

	switch s.Status {
	case "online":
		h.manager.SetOnline(deviceID, "")
	case "offline":
		h.manager.SetOffline(deviceID)
	case "heartbeat":
		h.manager.Heartbeat(deviceID)
	default:
		log.Printf("mqtt: unknown status %q from device %d", s.Status, deviceID)
		return
	}

	// Re-publish on the EventBus so WebSocket subscribers see the update.
	h.bus.Publish(eventbus.Event{
		Topic:   eventbus.TopicDeviceStatus,
		Payload: payload,
		Source:  eventbus.SourceMQTT,
	})
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
