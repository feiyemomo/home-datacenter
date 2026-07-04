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
		&model.Recording{},
	)
	if err != nil {
		log.Fatalf("failed to migrate database: %v", err)
	}

	// One-shot backfill: cameras that were registered before the
	// onvif_profile_token column was added had the value stored
	// inside cam.Meta. Move it into the dedicated column so the
	// "missing onvif profile_token" error stops surfacing on the
	// PTZ endpoint. We log and continue: a failed backfill is not
	// fatal — the user can re-register.
	if err := backfillOnvifProfile(db); err != nil {
		log.Printf("camera: onvif profile backfill: %v", err)
	}

	DB = db

	log.Println("sqlite initialized successfully")
}

// backfillOnvifProfile copies cam.Meta["onvif_profile"] into the
// dedicated cam.OnvifProfileToken column for any camera where the
// new column is empty. The Meta entry is left in place to avoid
// surprising the user — the next Register call will overwrite both.
func backfillOnvifProfile(db *gorm.DB) error {
	var cams []model.Camera
	if err := db.Find(&cams).Error; err != nil {
		return err
	}
	for i := range cams {
		if cams[i].OnvifProfileToken != "" {
			continue
		}
		raw, ok := cams[i].Meta["onvif_profile"]
		if !ok {
			continue
		}
		tok, _ := raw.(string)
		if tok == "" {
			continue
		}
		if err := db.Model(&model.Camera{}).
			Where("id = ?", cams[i].ID).
			Update("onvif_profile_token", tok).Error; err != nil {
			return err
		}
	}
	return nil
}
