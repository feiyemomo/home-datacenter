package model

import (
	"time"
)

// CameraAlert models a persisted detection alert across all AI sources
// (on-board camera ISAPI AI or Frigate NVR).
type CameraAlert struct {
	ID          string    `gorm:"primaryKey;size:64" json:"id"`
	CameraID    uint      `gorm:"index;not null" json:"camera_id"`
	CameraSlug  string    `gorm:"size:64;index" json:"camera_slug"`
	CameraName  string    `gorm:"size:64" json:"camera_name"`
	Label       string    `gorm:"size:32;index" json:"label"`
	Confidence  float64   `json:"confidence"`
	StartTime   float64   `gorm:"index" json:"start_time"`
	EndTime     float64   `json:"end_time"`
	HasClip     bool      `json:"has_clip"`
	HasSnapshot bool      `json:"has_snapshot"`
	SnapshotPath string   `gorm:"size:255" json:"snapshot_path,omitempty"`
	Thumbnail   string    `gorm:"type:text" json:"thumbnail,omitempty"`
	Source      string    `gorm:"size:32;default:'camera'" json:"source"` // "camera" | "frigate"
	IsIntrusion bool      `json:"is_intrusion"`
	CreatedAt   time.Time `gorm:"index" json:"created_at"`
}

func (CameraAlert) TableName() string { return "camera_alerts" }
