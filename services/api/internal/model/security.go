package model

import (
	"time"

	"home-datacenter-api/internal/utils"
)

const (
	GuardModeDisarmed  = "disarmed"
	GuardModeArmedHome = "armed_home"
	GuardModeArmedAway = "armed_away"
)

// SecurityState records the global home security arming mode.
type SecurityState struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Mode      string    `gorm:"size:32;not null;default:'armed_away'" json:"mode"`
	UpdatedBy string    `gorm:"size:64" json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (SecurityState) TableName() string { return "security_state" }

// IPBan represents an actively or historically banned IP.
type IPBan struct {
	ID        uint           `gorm:"primaryKey" json:"id"`
	IP        string         `gorm:"size:64;uniqueIndex;not null" json:"ip"`
	Reason    string         `gorm:"size:255" json:"reason"`
	Source    string         `gorm:"size:32;not null;default:'auto'" json:"source"` // "auto" | "manual"
	BannedAt  time.Time      `gorm:"not null" json:"banned_at"`
	ExpiresAt time.Time      `gorm:"not null;index" json:"expires_at"`
	Permanent bool           `gorm:"not null;default:false" json:"permanent"`
	HitCount  int64          `gorm:"not null;default:0" json:"hit_count"`
	LastHitAt utils.NullTime `json:"last_hit_at"`
}

func (IPBan) TableName() string { return "ip_bans" }

// SecurityEvent records intrusion defense and administrative security actions.
type SecurityEvent struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	IP        string    `gorm:"size:64;index;not null" json:"ip"`
	Type      string    `gorm:"size:64;index;not null" json:"type"` // "auth_fail", "rate_limit", "scan", "manual_ban", "manual_unban", "gateway_warning", "test_alert", "config_change"
	Detail    string    `gorm:"type:text" json:"detail"`
	Action    string    `gorm:"size:255" json:"action"`
	CreatedAt time.Time `gorm:"index;not null" json:"created_at"`
}

func (SecurityEvent) TableName() string { return "security_events" }

// SecurityWhitelist persists dynamically managed IP or CIDR exemptions.
type SecurityWhitelist struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Entry     string    `gorm:"size:128;uniqueIndex;not null" json:"entry"` // IP (1.2.3.4) or CIDR (192.168.1.0/24)
	Note      string    `gorm:"size:255" json:"note"`
	CreatedAt time.Time `gorm:"not null" json:"created_at"`
}

func (SecurityWhitelist) TableName() string { return "security_whitelist" }

