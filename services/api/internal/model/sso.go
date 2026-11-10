package model

import (
	"time"

	"home-datacenter-api/internal/utils"
)

// SSOTicket represents a cryptographically secure, single-use ticket for 1Panel SSO.
type SSOTicket struct {
	ID         uint           `gorm:"primaryKey" json:"id"`
	TicketHash string         `gorm:"size:64;uniqueIndex;not null" json:"ticket_hash"` // SHA-256 hex string
	UserID     uint           `gorm:"index;not null" json:"user_id"`
	Username   string         `gorm:"size:64;not null" json:"username"`
	ExpiresAt  time.Time      `gorm:"index;not null" json:"expires_at"`
	Consumed   bool           `gorm:"default:false;index;not null" json:"consumed"`
	ConsumedAt utils.NullTime `json:"consumed_at"`
	ClientIP   string         `gorm:"size:64" json:"client_ip"`
	UserAgent  string         `gorm:"size:255" json:"user_agent"`
	CreatedAt  time.Time      `gorm:"not null" json:"created_at"`
}

func (SSOTicket) TableName() string {
	return "sso_tickets"
}
