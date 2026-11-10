package model

import "time"

type User struct {
    ID          uint      `gorm:"primaryKey" json:"id"`
    Name        string    `gorm:"unique;not null" json:"name"`
    IsAdmin     bool      `gorm:"default:false" json:"is_admin"`
    HouseholdID uint      `gorm:"index;default:1" json:"household_id"`

    CreatedAt   time.Time `json:"created_at"`
    UpdatedAt   time.Time `json:"updated_at"`
}