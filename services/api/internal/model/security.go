package model

import "time"

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
