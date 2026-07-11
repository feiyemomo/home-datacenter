package database

import (
	"log"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
)

var DB *gorm.DB

func InitDB(dbPath string) {
	// The bare `glebarez/sqlite` driver defaults to a 60s busy
	// timeout (the upstream _modernc.org/sqlite default) and a
	// `journal=DELETE` mode. On a busy home-api where the
	// automation engine, camera health checker, and EventBus-
	// bridged MQTT handler all touch the same DB, a single long
	// write (e.g. an automation `webhook` action retry, or a
	// camera health tick that updates `last_seen`) can hold the
	// write lock for milliseconds — but a concurrent /api/v1/auth/verify
	// (called by nginx `auth_request` for every /go2rtc/ SDP
	// POST) will then stall for the full 60s on its primary-key
	// lookup, cascade into nginx's default 60s
	// `proxy_read_timeout`, and surface as a WebRTC SDP 500.
	//
	// We override:
	//   * busy_timeout=1000ms: read requests fail fast (and
	//     can be retried by nginx) instead of waiting the full
	//     SQLite default of 60s.
	//   * journal_mode=WAL: concurrent readers no longer block
	//     on a writer, and vice versa, so a slow automation
	//     webhook can no longer freeze the auth path.
	dsn := dbPath + "?_pragma=busy_timeout(1000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("failed to connect sqlite: %v", err)
	}

	err = db.AutoMigrate(
		&model.User{},
		&model.Device{},
		&model.Camera{},
		&model.Recording{},
		&model.Rule{},
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
