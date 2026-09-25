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
	TopicCameraOnline       = "camera.online"
	TopicCameraOffline      = "camera.offline"
	TopicCameraRTSPLost     = "camera.rtsp_lost"
	TopicCameraStatusChanged = "camera.status_changed"
	TopicCameraMotion       = "camera.motion"
	TopicCameraPersonRecognized = "camera.person_recognized"
	TopicCameraFallDetected     = "camera.fall_detected"

	// --- System events ---
	TopicSystemAlert      = "system.alert"
	TopicUserNotification = "user.notification"
	TopicSystemBroadcast  = "system.broadcast"
	// TopicSystemLog is published by the log subscriber after a
	// SystemLog row is persisted. The WS Hub subscribes to it so
	// connected dashboards see new log entries in real time.
	TopicSystemLog = "system.log"

	// --- Security events (v1.11.0) ---
	TopicSecurityGuardMode = "security.guard_mode"

	// --- User auth events ---
	// Emitted by the auth handler on /auth/bind success and by the
	// device handler on device revoke. Persisted by the log
	// subscriber as a human-readable audit entry.
	TopicUserLogin  = "user.login"
	TopicUserLogout = "user.logout"

	// --- User management events (v1.8.20) ---
	// Emitted by the user handler on Create/Update/Delete. Persisted
	// by the log subscriber so admin actions are auditable.
	TopicUserCreate = "user.create"
	TopicUserUpdate = "user.update"
	TopicUserDelete = "user.delete"

	// --- Camera management events (v1.8.20) ---
	// Emitted by the camera handler on Delete. Persisted by the log
	// subscriber so camera removals are auditable.
	TopicCameraDelete = "camera.delete"

	// --- Camera management events (v1.8.22) ---
	// Emitted by the camera handler on Create / Update (codec, audio,
	// recording plan). Persisted by the log subscriber so admin
	// actions on cameras are auditable end-to-end.
	TopicCameraCreate = "camera.create"
	TopicCameraUpdate = "camera.update"

	// --- Automation rule management events (v1.8.22) ---
	// Emitted by the automation handler on Create / Update / Delete.
	// Persisted by the log subscriber so rule changes are auditable.
	TopicAutomationCreate = "automation.create"
	TopicAutomationUpdate = "automation.update"
	TopicAutomationDelete = "automation.delete"

	// --- Device management events (v1.8.22) ---
	// Emitted by the device handler on hard-delete / token-rotate.
	// Persisted by the log subscriber so admin actions on devices
	// are auditable.
	TopicDeviceHardDelete  = "device.hard_delete"
	TopicDeviceTokenRotate = "device.token_rotate"

	// --- Vision AI person profile events ---
	TopicVisionPersonRegister = "vision.person_register"
	TopicVisionPersonDelete   = "vision.person_delete"

	// --- Automation events (Phase 5) ---
	TopicAutomationFired = "automation.fired"
)

// Source identifiers — recorded on every Event for debugging.
const (
	SourceMQTT      = "mqtt"
	SourceWS        = "ws"
	SourceSystem    = "system"
	SourceCamera    = "camera"
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

// UserLoginPayload is the JSON shape for TopicUserLogin events,
// emitted by /auth/bind on successful credential exchange.
type UserLoginPayload struct {
	UserID     uint   `json:"user_id"`
	DeviceID   uint   `json:"device_id"`
	DeviceName string `json:"device_name"`
	Ts         int64  `json:"ts"`
}

// UserLogoutPayload is the JSON shape for TopicUserLogout events,
// emitted when an admin revokes a device (the closest equivalent
// to an explicit logout in this codebase).
type UserLogoutPayload struct {
	UserID     uint   `json:"user_id"`
	DeviceID   uint   `json:"device_id"`
	DeviceName string `json:"device_name"`
	Ts         int64  `json:"ts"`
}

// UserManagePayload is the JSON shape for TopicUserCreate / Update /
// Delete events (v1.8.20). Emitted by the user handler so admin
// actions are auditable in the system log.
type UserManagePayload struct {
	AdminID    uint   `json:"admin_id"`          // the user performing the action
	AdminName  string `json:"admin_name"`        // friendly name of the admin
	TargetID   uint   `json:"target_id"`         // the user being created/updated/deleted
	TargetName string `json:"target_name"`       // friendly name of the target
	Action     string `json:"action"`            // "create" | "update" | "delete"
	IsAdmin    bool   `json:"is_admin"`          // target's admin flag (for update)
	Detail     string `json:"detail,omitempty"` // specific settings modified
	Ts         int64  `json:"ts"`
}

// CameraDeletePayload is the JSON shape for TopicCameraDelete events
// (v1.8.20). Emitted by the camera handler when a camera is removed.
type CameraDeletePayload struct {
	AdminID    uint   `json:"admin_id"`
	AdminName  string `json:"admin_name"`
	CameraID   uint   `json:"camera_id"`
	CameraName string `json:"camera_name"`
	Ts         int64  `json:"ts"`
}

// CameraManagePayload is the JSON shape for TopicCameraCreate /
// TopicCameraUpdate events (v1.8.22). Emitted by the camera handler
// when a camera is registered or its codec / audio / recording plan
// is changed, so admin actions are auditable in the system log.
type CameraManagePayload struct {
	AdminID    uint   `json:"admin_id"`
	CameraID   uint   `json:"camera_id"`
	CameraName string `json:"camera_name"`
	Action     string `json:"action"` // "create" | "update"
	Detail     string `json:"detail"` // update scope: "编码" | "音频" | "录制计划" | etc.
	Ts         int64  `json:"ts"`
}

// AutomationManagePayload is the JSON shape for TopicAutomationCreate /
// Update / Delete events (v1.8.22). Emitted by the automation handler
// so rule lifecycle changes are auditable in the system log.
type AutomationManagePayload struct {
	AdminID  uint   `json:"admin_id"`
	RuleID   uint   `json:"rule_id"`
	RuleName string `json:"rule_name"`
	Action   string `json:"action"`           // "create" | "update" | "delete"
	Detail   string `json:"detail,omitempty"` // specific settings or action
	Ts       int64  `json:"ts"`
}

// VisionPersonManagePayload is the JSON shape for TopicVisionPersonRegister /
// TopicVisionPersonDelete events. Emitted by the vision handler when family
// face profiles are enrolled or deleted.
type VisionPersonManagePayload struct {
	AdminID   uint   `json:"admin_id"`
	AdminName string `json:"admin_name,omitempty"`
	Name      string `json:"name"`
	Action    string `json:"action"` // "register" | "delete"
	Ts        int64  `json:"ts"`
}

// DeviceManagePayload is the JSON shape for TopicDeviceHardDelete /
// TopicDeviceTokenRotate events (v1.8.22). Emitted by the device
// handler so admin actions on devices are auditable in the system log.
type DeviceManagePayload struct {
	AdminID    uint   `json:"admin_id"`
	DeviceID   uint   `json:"device_id"`
	DeviceName string `json:"device_name"`
	Action     string `json:"action"` // "hard_delete" | "token_rotate"
	Ts         int64  `json:"ts"`
}
