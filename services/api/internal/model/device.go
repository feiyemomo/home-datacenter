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

	LastIP string

	CreatedAt time.Time
	UpdatedAt time.Time
}
