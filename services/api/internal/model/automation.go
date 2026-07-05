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
//
// Rules are admin-managed via /api/v1/automation/rules.
type Rule struct {
	ID         uint           `gorm:"primaryKey" json:"id"`
	Name       string         `gorm:"size:128" json:"name"`
	Trigger    string         `gorm:"size:64;index" json:"trigger"`
	Condition  Condition      `gorm:"type:text" json:"condition"`
	Action     Action         `gorm:"type:text" json:"action"`
	Enabled    bool           `gorm:"default:true" json:"enabled"`
	FireCount  uint64         `json:"fire_count"`
	LastFireAt *time.Time     `json:"last_fire_at,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	DeletedAt  gorm.DeletedAt `gorm:"index" json:"-"`
}

func (Rule) TableName() string { return "automation_rules" }

// Condition is a declarative filter applied to an Event before the
// Action fires. All specified fields must match (logical AND).
//
//   time_gte / time_lte: 24h "HH:MM" bounds. If time_gte > time_lte
//     the range wraps midnight (e.g. gte=22:00, lte=06:00 means
//     22:00-23:59 OR 00:00-06:00). Empty fields are ignored.
//
//   payload_eq: each key is compared against the event payload's
//     top-level field of the same name. Values are compared after
//     JSON-normalising both sides, so 1 == 1.0 and "offline" ==
//     "offline". Missing keys do NOT match.
//
// Examples:
//
//	{"time_gte":"22:00","time_lte":"06:00"}      // night only
//	{"payload_eq":{"status":"offline"}}          // offline events
//	{"payload_eq":{"event":"motion"}}            // motion events
type Condition struct {
	TimeGTE   string         `json:"time_gte,omitempty"`
	TimeLTE   string         `json:"time_lte,omitempty"`
	PayloadEQ map[string]any `json:"payload_eq,omitempty"`
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
// All action types are fire-and-forget: failures are logged but do
// not block the engine.
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
