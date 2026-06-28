package model

import "time"

type User struct {
    ID        uint      `gorm:"primaryKey"`
    Name      string    `gorm:"unique;not null"`
    IsAdmin   bool      `gorm:"default:false"`

    CreatedAt time.Time
    UpdatedAt time.Time
}