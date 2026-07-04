package model

import (
	"time"

	"gorm.io/gorm"
)

// Recording is one finished segment of a camera recording. A
// continuous recording session is broken into N-second chunks (see
// Camera.RecordingPlan.SegmentSeconds) so that:
//
//   - A corrupt segment doesn't take down the whole timeline.
//   - Housekeeping can delete whole files at a time.
//   - HTTP range requests stay reasonable.
//
// All times are stored in UTC. Size is in bytes; duration is the
// actual encoded length (may be < SegmentSeconds if the segment
// was the last one before a manual stop).
type Recording struct {
	ID             uint           `gorm:"primaryKey" json:"id"`
	CameraID       uint           `gorm:"index" json:"camera_id"`
	StreamName     string         `gorm:"size:64;index" json:"stream_name"`
	FilePath       string         `gorm:"size:512" json:"file_path"`
	StartAt        time.Time      `gorm:"index" json:"start_at"`
	EndAt          time.Time      `json:"end_at"`
	DurationSeconds int           `json:"duration_seconds"`
	SizeBytes      int64          `json:"size_bytes"`
	CreatedAt      time.Time      `json:"created_at"`
	DeletedAt      gorm.DeletedAt `gorm:"index" json:"-"`
}

func (Recording) TableName() string { return "recordings" }
