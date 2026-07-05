package model

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Rule is a persisted automation rule. The Automation Engine loads all
// enabled rules into memory and evaluates each one against every Event
// that flows through the EventBus.
//
//   - Trigger:   event topic or prefix (e.g. "camera.motion", "device",
//                 or "*" for all events). Prefix matching is identical
//                 to EventBus.Subscribe.
//   - Condition: optional filter on event time / payload. Stored as
//                 JSON TEXT. See Condition below.
//   - Action:    what to do when trigger + condition match. Stored as
//                 JSON TEXT. See Action below.
//   - Throttle:  optional rate limit / cooldown / dedup on how often
//                 the rule may fire. See Throttle below.
//   - Enabled:   soft disable; re-enable to resume firing without
//                 re-creating the rule.
//
// Rules are admin-managed via /api/v1/automation/rules.
type Rule struct {
	ID         uint           `gorm:"primaryKey" json:"id"`
	Name       string         `gorm:"size:128" json:"name"`
	Trigger    string         `gorm:"size:64;index" json:"trigger"`
	Condition  Condition      `gorm:"type:text" json:"condition"`
	Action     Action         `gorm:"type:text" json:"action"`
	Throttle   Throttle       `gorm:"type:text" json:"throttle"`
	Enabled    bool           `gorm:"default:true" json:"enabled"`
	FireCount  uint64         `json:"fire_count"`
	LastFireAt *time.Time     `json:"last_fire_at,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	DeletedAt  gorm.DeletedAt `gorm:"index" json:"-"`
}

func (Rule) TableName() string { return "automation_rules" }

// Throttle caps how often a rule may fire, to prevent event floods
// from triggering action storms (e.g. a motion event arriving at
// 10 Hz from a noisy camera would otherwise fire 10 webhooks/s).
//
//   cooldown_s: minimum seconds between consecutive fires. After
//     firing, the rule is silent for this many seconds. 0 means
//     "no cooldown" (default).
//
//   rate_per_min: maximum fires per rolling 60s window. 0 means
//     "no rate limit" (default). Implemented as a token bucket
//     refilled lazily from the last fire time.
//
//   dedup: if true, identical events (same Topic + Source +
//     Payload) collapse to a single fire. Useful for
//     status-changed cascades where a camera going offline
//     emits multiple co-temporal events.
type Throttle struct {
	CooldownS  int  `json:"cooldown_s,omitempty"`
	RatePerMin int  `json:"rate_per_min,omitempty"`
	Dedup      bool `json:"dedup,omitempty"`
}

// Condition is a declarative filter applied to an Event before the
// Action fires. By default all specified fields must match (logical
// AND); set `Any: true` to switch to OR semantics.
//
// Time
//
//   time_gte / time_lte: 24h "HH:MM" bounds. If time_gte > time_lte
//     the range wraps midnight (e.g. gte=22:00, lte=06:00 means
//     22:00-23:59 OR 00:00-06:00). Empty fields are ignored.
//
// Payload
//
//   payload_eq: each key is compared against the event payload's
//     top-level field of the same name. Values are compared after
//     JSON-normalising both sides, so 1 == 1.0 and "offline" ==
//     "offline". Missing keys do NOT match.
//
//   threshold: each key is a numeric field in the payload; the
//     value is compared against Val using Op. Op is one of
//     ">", ">=", "<", "<=", "==", "!=". The payload field must
//     parse as a number; otherwise the field is considered
//     non-matching. Useful for motion confidence
//     (`{"confidence": {">": 0.8}}`), sensor thresholds, etc.
//
//   regex: each key is a payload field and each value is a RE2
//     regular expression that the field's string representation
//     must match in full. RE2 (not PCRE) — no backrefs. We
//     deliberately avoid Lua/CEL/expr-lang to keep the engine
//     dependency-free and auditable.
//
// Source
//
//   source: exact match on the Event.Source field
//     ("mqtt" | "ws" | "system" | "camera" | "automation" |
//      "test"). Useful for routing rules to a specific producer
//     without having to wire new topics.
//
// Examples:
//
//	{"time_gte":"22:00","time_lte":"06:00"}                   // night only
//	{"payload_eq":{"status":"offline"}}                       // offline events
//	{"payload_eq":{"event":"motion"}}                         // motion events
//	{"threshold":{"confidence":{">":0.8}}}                    // high-confidence motion
//	{"regex":{"device_id":"^cam_[0-9]+$"}}                    // camera-* sources
//	{"source":"camera","payload_eq":{"event":"motion"}, "any":true}  // camera.motion OR ws.motion
type Condition struct {
	TimeGTE   string             `json:"time_gte,omitempty"`
	TimeLTE   string             `json:"time_lte,omitempty"`
	PayloadEQ map[string]any     `json:"payload_eq,omitempty"`
	Source    string             `json:"source,omitempty"`
	Threshold map[string]NumberOp `json:"threshold,omitempty"`
	Regex     map[string]string  `json:"regex,omitempty"`
	Any       bool               `json:"any,omitempty"`
}

// NumberOp is a numeric comparison used by Condition.Threshold.
// Op is one of: ">", ">=", "<", "<=", "==", "!=".
type NumberOp struct {
	Op  string  `json:"op"`
	Val float64 `json:"val"`
}

// Action describes what the engine does when a rule fires.
//
// Supported types:
//
//	notify  → publish a user.notification event on the EventBus;
//	          UserID + Title + Body are required.
//	mqtt    → publish a raw MQTT message; Topic + Payload required.
//	          QoS defaults to 1.
//	webhook → HTTP POST/GET to an external URL; URL required.
//	          Method defaults to POST. Body is sent as-is.
//
// All action types are fire-and-forget by default: failures are
// logged but do not block the engine. Tune `TimeoutMs` to cap
// how long a single execution may take (default 5000ms), and
// set `RetryMax` to enable exponential-backoff retries on
// transient failures (network errors, 5xx responses). Retries
// use 500ms × 2^n delay, capped at 30s, and respect the rule's
// overall cooldown window.
type Action struct {
	Type    string            `json:"type"`
	UserID  uint              `json:"user_id,omitempty"`
	Title   string            `json:"title,omitempty"`
	Body    string            `json:"body,omitempty"`
	Topic   string            `json:"topic,omitempty"`
	Payload string            `json:"payload,omitempty"`
	QoS     byte              `json:"qos,omitempty"`
	URL     string            `json:"url,omitempty"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Per-action timeout in milliseconds. Default 5000. Caps the
	// single execution time; retries have their own per-attempt
	// budget. 0 means "use default".
	TimeoutMs int `json:"timeout_ms,omitempty"`
	// Number of retries on transient failure. Default 0
	// (no retry). Each retry waits 500ms × 2^n (n=0,1,2...) up
	// to a 30s cap. A 4xx response is treated as a permanent
	// failure (no retry); a 5xx or network error is retried.
	RetryMax int `json:"retry_max,omitempty"`
}

// Value / Scan let Condition / Action be stored as JSON TEXT in SQLite.
// We implement them on typed structs (not map[string]any) so the engine
// can read fields without type-asserting at runtime.

func (c Condition) Value() (driver.Value, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("condition: %w", err)
	}
	return string(b), nil
}

func (c *Condition) Scan(src any) error {
	if src == nil {
		*c = Condition{}
		return nil
	}
	var b []byte
	switch v := src.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return errors.New("Condition: unsupported scan type")
	}
	if len(b) == 0 {
		*c = Condition{}
		return nil
	}
	if err := json.Unmarshal(b, c); err != nil {
		return fmt.Errorf("condition: %w", err)
	}
	return nil
}

func (a Action) Value() (driver.Value, error) {
	b, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("action: %w", err)
	}
	return string(b), nil
}

func (a *Action) Scan(src any) error {
	if src == nil {
		*a = Action{}
		return nil
	}
	var b []byte
	switch v := src.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return errors.New("Action: unsupported scan type")
	}
	if len(b) == 0 {
		*a = Action{}
		return nil
	}
	if err := json.Unmarshal(b, a); err != nil {
		return fmt.Errorf("action: %w", err)
	}
	return nil
}

func (t Throttle) Value() (driver.Value, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("throttle: %w", err)
	}
	return string(b), nil
}

func (t *Throttle) Scan(src any) error {
	if src == nil {
		*t = Throttle{}
		return nil
	}
	var b []byte
	switch v := src.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return errors.New("Throttle: unsupported scan type")
	}
	if len(b) == 0 {
		*t = Throttle{}
		return nil
	}
	if err := json.Unmarshal(b, t); err != nil {
		return fmt.Errorf("throttle: %w", err)
	}
	return nil
}
