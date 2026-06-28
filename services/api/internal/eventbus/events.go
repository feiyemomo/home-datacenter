package eventbus

// Canonical event topics used across MQTT <-> EventBus <-> WebSocket.
//
// Naming convention: "<domain>.<id>.<subtype>"
// Subscribers can use prefix matching (e.g. "device.1" catches all
// events for device 1).

const (
	// TopicDeviceStatus — device online/offline/heartbeat.
	// Payload: JSON {"device_id":1,"status":"online","ts":...}
	TopicDeviceStatus = "device.status"

	// TopicDeviceTelemetry — sensor / state data from a device.
	// Payload: opaque JSON, device-defined.
	TopicDeviceTelemetry = "device.telemetry"

	// TopicDeviceCommand — command issued to a device (from app or system).
	// Payload: JSON {"device_id":1,"command":"...","params":{...}}
	TopicDeviceCommand = "device.command"

	// TopicUserNotification — push notification to a user's app(s).
	// Payload: JSON {"user_id":1,"title":"...","body":"..."}
	TopicUserNotification = "user.notification"

	// TopicSystemBroadcast — system-wide announcement.
	TopicSystemBroadcast = "system.broadcast"
)

// Source identifiers — recorded on every Event for debugging.
const (
	SourceMQTT   = "mqtt"
	SourceWS     = "ws"
	SourceSystem = "system"
)

// DeviceStatusPayload is the JSON shape for TopicDeviceStatus events.
type DeviceStatusPayload struct {
	DeviceID uint   `json:"device_id"`
	Status   string `json:"status"` // "online" | "offline" | "heartbeat"
	TS       int64  `json:"ts"`     // unix seconds
}

// DeviceCommandPayload is the JSON shape for TopicDeviceCommand events.
type DeviceCommandPayload struct {
	DeviceID uint        `json:"device_id"`
	Command  string      `json:"command"`
	Params   interface{} `json:"params,omitempty"`
}

// UserNotificationPayload is the JSON shape for TopicUserNotification.
type UserNotificationPayload struct {
	UserID uint   `json:"user_id"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}
