package model

// SystemLog level constants. v1.6.36 introduced the Level field so
// the dashboard can surface critical events (camera offline) above
// routine ones (user login). The three levels map 1:1 to UI badges:
//
//   - critical: red badge — camera offline, device offline
//   - warning:  amber badge — recordings quota / disk / backup thresholds
//               crossed (v1.8.36). Sits above routine "normal" entries so
//               the operator can spot a filling disk or shrunken retention.
//   - normal:   blue badge — user login/logout, device/camera online
//   - info:     grey badge — camera status_changed (codec/quality)
//
// Empty string (from rows written before v1.6.36) is treated as
// "normal" by the UI for backward compatibility.
const (
	LevelCritical = "critical"
	LevelWarning  = "warning"
	LevelNormal   = "normal"
	LevelInfo     = "info"
)

// SystemLog is a human-readable audit log entry persisted to SQLite.
// It is fed by the EventBus subscriber (internal/log/subscriber.go)
// which turns device / camera / user events into a uniform "what
// happened" stream for the dashboard's system log pane.
type SystemLog struct {
	ID uint `gorm:"primaryKey" json:"id"`

	// Ts is the Unix timestamp the log entry refers to. Indexed so
	// the REST list endpoint can ORDER BY ts DESC without a sort.
	Ts int64 `gorm:"index" json:"ts"`

	// EventType is the originating EventBus topic
	// (e.g. "device.status", "user.login", "camera.online").
	EventType string `gorm:"index" json:"event_type"`

	// Level is the severity bucket (v1.6.36). See the Level*
	// constants above. Indexed so the REST endpoint can filter by
	// level without a full scan.
	Level string `gorm:"index" json:"level"`

	// Source is the EventBus source identifier of the originating
	// event ("mqtt" | "ws" | "system" | "camera" | "automation").
	Source string `json:"source"`

	// Message is a human-readable summary in Chinese, e.g.
	// "设备 #3 上线" / "用户 admin 登录" / "摄像头 前门 上线".
	Message string `json:"message"`

	// Payload is the raw JSON payload of the originating EventBus
	// event, stored as TEXT so callers can replay the original
	// fields (device_id, status, host, ...) without a second query.
	Payload string `gorm:"type:text" json:"payload"`
}

// TableName overrides GORM's default pluralized "system_logs" ->
// "system_logs" (kept explicit so future renames are searchable).
func (SystemLog) TableName() string { return "system_logs" }
