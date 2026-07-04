package database

import (
    "log"

    "github.com/glebarez/sqlite"
    "gorm.io/gorm"

    "home-datacenter-api/internal/model"
)

var DB *gorm.DB

func InitDB(dbPath string) {
    db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
    if err != nil {
        log.Fatalf("failed to connect sqlite: %v", err)
    }

    err = db.AutoMigrate(
        &model.User{},
        &model.Device{},
        &model.Camera{},
    )
    if err != nil {
        log.Fatalf("failed to migrate database: %v", err)
    }

    DB = db

    log.Println("sqlite initialized successfully")
}