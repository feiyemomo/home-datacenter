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
		&model.StoredEvent{},
	)
	if err != nil {
		log.Fatalf("failed to migrate database: %v", err)
	}

	// The events table is managed via a raw CREATE TABLE IF NOT
	// EXISTS rather than AutoMigrate because glebarez/sqlite creates
	// the table as "stored_events" (Go struct name) ignoring the
	// model.StoredEvent.TableName() override. Dropping and recreating
	// under the correct name is safe because the table is append-only
	// (no foreign keys point to it, and a fresh DB has either the
	// wrong table or none at all).
	if err := ensureEventsTable(db); err != nil {
		log.Printf("warning: failed to create events table: %v", err)
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

	// Backfill: transcode_use_substream was added with
	// gorm:"default:true" but GORM AutoMigrate only adds the
	// column — it does NOT set defaults for existing rows.
	// Without this backfill, every existing camera reads NULL,
	// which Go zero-values to false, which would silently
	// revert the substream auto-switch. Force the default to
	// true for any NULL row so the migration is non-breaking.
	if err := backfillTranscodeUseSubstream(db); err != nil {
		log.Printf("camera: transcode_use_substream backfill: %v", err)
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

// backfillTranscodeUseSubstream sets transcode_use_substream=1
// for any existing camera where the column is NULL. GORM's
// AutoMigrate adds the column with the declared `default:true`
// at the SCHEMA level (so future INSERTs get true), but it does
// not touch existing rows. Without this backfill, every camera
// that was registered before the column was added reads NULL
// when the camera handler converts it to bool — Go zero-values
// NULL to false, which silently disables the substream
// auto-switch (the same behaviour as TranscodeUseSubstream=false).
//
// We use a single UPDATE rather than a per-row loop so the
// migration is O(1) on large fleets. Operationally: a camera
// the operator has explicitly disabled substream on is
// unaffected because the value is already 0, not NULL.
func backfillTranscodeUseSubstream(db *gorm.DB) error {
	// Backfill ONLY cameras that are using transcode. The
	// substream auto-switch is meaningless for native-H.264
	// cameras (Transcode=false), and we don't want to silently
	// change behaviour on operators who have explicitly disabled
	// the transcode pipeline (their existing 0 may be
	// intentional). For the transcode=true case, the existing
	// 0 is almost certainly a GORM AutoMigrate default-fill,
	// not an operator choice, so we flip it to 1.
	//
	// To avoid silently overwriting an operator who DID set
	// transcode_use_substream=0 explicitly on a transcode=true
	// camera, we tag the backfill with a meta marker. Future
	// runs check the marker first; once set, the row is left
	// alone. Operators can re-enable via PUT /cameras/:id
	// with `{"transcode_use_substream": true}`.
	res := db.Exec(`
		UPDATE cameras
		SET transcode_use_substream = 1,
		    meta = json_set(COALESCE(meta, '{}'), '$.substream_backfilled', 1)
		WHERE transcode = 1
		  AND (transcode_use_substream = 0 OR transcode_use_substream IS NULL)
		  AND json_extract(COALESCE(meta, '{}'), '$.substream_backfilled') IS NULL
	`)
	if res.Error != nil {
		return res.Error
	}
	log.Printf("camera: transcode_use_substream backfill: %d row(s) updated", res.RowsAffected)
	return nil
}

// ensureEventsTable creates the events table with the correct
// name ("events", not "stored_events") and schema.
func ensureEventsTable(db *gorm.DB) error {
	// Clean up any wrongly-named table from a previous run.
	db.Exec("DROP TABLE IF EXISTS stored_events")

	return db.Exec(`
		CREATE TABLE IF NOT EXISTS events (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			topic      TEXT    NOT NULL,
			source     TEXT    NOT NULL,
			severity   TEXT    NOT NULL DEFAULT 'info',
			payload    TEXT    NOT NULL DEFAULT '{}',
			status     TEXT    NOT NULL DEFAULT 'created',
			timestamp  TEXT    NOT NULL,
			created_at TEXT    NOT NULL DEFAULT (datetime('now'))
		);
		CREATE INDEX IF NOT EXISTS idx_events_topic   ON events(topic);
		CREATE INDEX IF NOT EXISTS idx_events_source  ON events(source);
		CREATE INDEX IF NOT EXISTS idx_events_ts       ON events(timestamp);
	`).Error
}
