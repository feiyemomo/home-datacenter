package model

import (
	"time"
)

// EventStatus represents the lifecycle state of a persisted event.
type EventStatus string

const (
	EventStatusCreated   EventStatus = "created"
	EventStatusProcessed EventStatus = "processed"
	EventStatusArchived  EventStatus = "archived"
)

// StoredEvent is the SQLite-backed persistent event record.
//
// Every event published to the EventBus is automatically persisted
// by an EventPersister that subscribes to "*". The in-memory bus is
// fire-and-forget; the DB table is the permanent audit trail.
//
// Payload is stored as opaque JSON text (SQLite TEXT column).
// Frontend consumers decode payload when displaying event details.
type StoredEvent struct {
	ID        uint        `gorm:"primaryKey"          json:"id"`
	Topic     string      `gorm:"not null;index"      json:"type"`
	Source    string      `gorm:"not null;index"      json:"source"`
	Severity  string      `gorm:"not null"            json:"severity"`
	Payload   string      `gorm:"not null"            json:"payload"`
	Status    EventStatus `gorm:"not null;default:created" json:"status"`
	Timestamp time.Time   `gorm:"not null;index"      json:"timestamp"`
	CreatedAt time.Time   `json:"created_at"`
}

// TableName overrides the default "stored_events" table name for clarity.
func (StoredEvent) TableName() string {
	return "events"
}
