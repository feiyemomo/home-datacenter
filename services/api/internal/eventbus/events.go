package eventbus

import "time"

// Canonical event topics used across MQTT <-> EventBus <-> WebSocket
// <-> Automation Engine.
//
// Naming convention: "<domain>.<subtype>"
// Subscribers can use prefix matching (e.g. "device" catches all
// device.* events) or "*" to receive everything.

const (
	// --- Device events ---
	TopicDeviceStatus    = "device.status"
	TopicDeviceTelemetry = "device.telemetry"
	TopicDeviceCommand   = "device.command"
	TopicDeviceEvent     = "device.event"

	// --- Camera events (Phase 5) ---
	// Emitted by the camera HealthChecker on status transitions.
	TopicCameraOnline        = "camera.online"
	TopicCameraOffline       = "camera.offline"
	TopicCameraRTSPLost      = "camera.rtsp_lost"
	TopicCameraStatusChanged = "camera.status_changed"
	TopicCameraMotion        = "camera.motion"

	// --- System events ---
	TopicSystemAlert      = "system.alert"
	TopicUserNotification = "user.notification"
	TopicSystemBroadcast  = "system.broadcast"

	// --- Server lifecycle events (Phase 1) ---
	// Emitted once at boot and at graceful shutdown so clients can
	// react to the server coming up or going down.
	TopicServerOnline  = "server.online"
	TopicServerOffline = "server.offline"

	// --- Disk events (Phase 1) ---
	// Emitted when disk space falls below a threshold. Subscribers
	// (e.g. Automation Engine, future App push) can react before
	// writes fail.
	TopicDiskWarning = "disk.warning"

	// --- Automation events (Phase 5) ---
	TopicAutomationFired = "automation.fired"
)

// Source identifiers — recorded on every Event for debugging.
const (
	SourceMQTT       = "mqtt"
	SourceWS         = "ws"
	SourceSystem     = "system"
	SourceCamera     = "camera"
	SourceAutomation = "automation"
)

// Severity levels for events.
const (
	SeverityInfo     = "info"
	SeverityWarn     = "warn"
	SeverityError    = "error"
	SeverityCritical = "critical"
)

// Event is the unit of communication on the bus.
//
//   - ID:        auto-incremented unique identifier
//   - Topic:     logical channel name (e.g. "device.status")
//   - Source:    origin of the event ("mqtt" | "ws" | "system" | "camera" | "automation")
//   - Severity:  "info" | "warn" | "error" | "critical"
//   - Payload:   opaque JSON bytes; subscribers decide how to decode
//   - Timestamp: when the event was created (auto-filled by Publish)
type Event struct {
	ID        uint64    `json:"id"`
	Topic     string    `json:"type"`
	Source    string    `json:"source"`
	Severity  string    `json:"severity"`
	Payload   []byte    `json:"payload"`
	Timestamp time.Time `json:"timestamp"`
}

// DeviceStatusPayload is the JSON shape for TopicDeviceStatus events.
type DeviceStatusPayload struct {
	DeviceID uint   `json:"device_id"`
	Status   string `json:"status"`
	TS       int64  `json:"ts"`
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

// CameraStatusPayload is the JSON shape for camera.online/offline events.
type CameraStatusPayload struct {
	CameraID uint   `json:"camera_id"`
	Status   string `json:"status"`
	Host     string `json:"host"`
	TS       int64  `json:"ts"`
}

// ServerOnlinePayload is published once at boot on server.online.
type ServerOnlinePayload struct {
	ServerID     string   `json:"server_id"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
	TS           int64    `json:"ts"`
}

// ServerOfflinePayload is published at graceful shutdown on
// server.offline. Uptime is the total seconds the server ran.
type ServerOfflinePayload struct {
	ServerID string `json:"server_id"`
	Uptime   int64  `json:"uptime_seconds"`
	TS       int64  `json:"ts"`
}

// DiskWarningPayload is published on disk.warning when free space
// drops below the threshold. Path is the mount point or directory
// being monitored.
type DiskWarningPayload struct {
	Path         string `json:"path"`
	FreeBytes    int64  `json:"free_bytes"`
	TotalBytes   int64  `json:"total_bytes"`
	ThresholdPct int    `json:"threshold_pct"`
	TS           int64  `json:"ts"`
}
