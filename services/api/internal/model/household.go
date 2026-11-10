package model

import "time"

// Household represents a multi-tenant family boundary for resources,
// devices, cameras, proposals, and audit events.
type Household struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Name      string    `gorm:"size:128;not null" json:"name"`
	OwnerID   uint      `gorm:"index;not null" json:"owner_id"`
	CreatedAt time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (Household) TableName() string { return "households" }
