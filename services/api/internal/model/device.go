package model

import "time"

type Device struct {
    ID uint `gorm:"primaryKey"`

    UserID uint `gorm:"index"`

    DeviceName string `gorm:"not null"`

    AccessKeyHash string `gorm:"not null"`

    LastLoginAt *time.Time

    RevokedAt *time.Time

    LastIP string

    CreatedAt time.Time
    UpdatedAt time.Time
}