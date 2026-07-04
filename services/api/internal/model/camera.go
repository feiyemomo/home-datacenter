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

	// OwnerID is the user who registered the camera. Non-admin
	// List/Get calls are scoped to cameras whose OwnerID matches
	// the caller's user id; admin sees all.
	OwnerID uint `gorm:"index" json:"owner_id"`

	Capabilities JSON         `gorm:"type:text" json:"capabilities"`
	Credentials  JSON         `gorm:"type:text" json:"-"`     // never serialize
	Meta         JSON         `gorm:"type:text" json:"meta"`
	// OnvifProfileToken is the profile token we use for PTZ and
	// presets. Stored as a dedicated column (not inside Meta) so
	// the read path is a plain `string` — GORM's JSON scanning is
	// lossy across drivers and used to fail type-assertion in
	// `cam.Meta["onvif_profile"].(string)`, surfacing as a
	// spurious "missing onvif profile_token" error after register.
	OnvifProfileToken string     `gorm:"size:64" json:"onvif_profile"`
	// Presets maps a friendly name to an ONVIF preset token that the
	// user has pre-set up in the camera's own UI. The API never
	// *creates* presets (most firmware forbids it) — only stores
	// the alias and triggers GotoPreset on demand.
	Presets JSON             `gorm:"type:text" json:"presets"` // {"home":"Preset_1","away":"Preset_2"}

	StreamName string         `gorm:"size:64;uniqueIndex" json:"stream_name"` // cam_<id>

	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	DeletedAt  gorm.DeletedAt `gorm:"index" json:"-"`
}

func (Camera) TableName() string { return "cameras" }
