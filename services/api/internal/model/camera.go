package model

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
)

// JSON is a generic JSON column stored as TEXT in SQLite (or jsonb in
// Postgres later). It is used for capability/credential/meta blobs
// that vary by device type.
type JSON map[string]any

func (j JSON) Value() (driver.Value, error) {
	if j == nil {
		return nil, nil
	}
	return json.Marshal(j)
}

func (j *JSON) Scan(src any) error {
	if src == nil {
		*j = nil
		return nil
	}
	var b []byte
	switch v := src.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return errors.New("model.JSON: unsupported scan type")
	}
	if len(b) == 0 {
		*j = nil
		return nil
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*j = m
	return nil
}

// Camera is a platformized device of type "camera". It deliberately
// stays in its own table (rather than overloading the existing
// devices table) because:
//
//  1. The legacy devices table models *auth* identities
//     (AccessKey/JWT/revocation), not *hardware*.
//  2. Cameras have richer fields (rtsp_url, onvif_port, capabilities)
//     and a 1:N relationship with future entities (recordings,
//     presets, motion clips).
//
// All Camera fields are surfaced as the unified device view via
// /api/v1/cameras and the EventBus "device.status" topic — a
// Dashboard/App consumes them through the same event channel as
// every other device type.
type Camera struct {
	ID         uint           `gorm:"primaryKey" json:"id"`
	Type       string         `gorm:"size:32;index" json:"type"` // fixed "camera"
	Name       string         `gorm:"size:64" json:"name"`
	Vendor     string         `gorm:"size:32" json:"vendor"`
	Host       string         `gorm:"size:128;index" json:"host"`
	ONVIFPort  int            `gorm:"default:80" json:"onvif_port"`
	RTSPPort   int            `gorm:"default:554" json:"rtsp_port"`
	ChannelID  int            `gorm:"default:1" json:"channel_id"`

	Status     string         `gorm:"size:16;index" json:"status"` // online/offline/unknown
	LastSeenAt *time.Time     `json:"last_seen_at,omitempty"`

	Capabilities JSON         `gorm:"type:text" json:"capabilities"`
	Credentials  JSON         `gorm:"type:text" json:"-"` // never serialize
	Meta         JSON         `gorm:"type:text" json:"meta"`

	StreamName string         `gorm:"size:64;uniqueIndex" json:"stream_name"` // cam_<id>

	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	DeletedAt  gorm.DeletedAt `gorm:"index" json:"-"`
}

func (Camera) TableName() string { return "cameras" }
