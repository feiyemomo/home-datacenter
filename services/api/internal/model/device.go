package model

import (
	"time"

	"home-datacenter-api/internal/utils"
)

type Device struct {
	ID uint `gorm:"primaryKey"`

	UserID uint `gorm:"index"`

	DeviceName string `gorm:"not null"`

	AccessKeyHash string `gorm:"not null"`

	// LastLoginAt and RevokedAt use utils.NullTime instead of *time.Time
	// because glebarez/sqlite (modernc.org/sqlite, pure-Go) returns TEXT
	// datetime columns as strings; *time.Time cannot scan those, causing:
	//   Scan error: revoked_at string -> *time.Time
	LastLoginAt utils.NullTime

	RevokedAt utils.NullTime

	// LastSeenAt is updated by the device Manager on every heartbeat /
	// telemetry message, giving a near-real-time "last active" stamp.
	LastSeenAt utils.NullTime

	LastIP string

	// TokenVersion is incremented by the admin API to invalidate
	// all existing JWT tokens for this device. When a client
	// presents a JWT whose token_version < DB value, the middleware
	// rejects it with "token version mismatch", forcing the client
	// to re-bind with its access_key to get a fresh token.
	// Defaults to 1 for newly created devices.
	TokenVersion int `gorm:"default:1"`

	CreatedAt time.Time
	UpdatedAt time.Time
}
