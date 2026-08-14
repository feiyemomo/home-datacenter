// Package maintenance hosts the background "keep the box healthy on
// long-running deployments" loops (v1.8.27):
//
//   - DiskMonitor: samples free space on the data filesystem and
//     writes a SystemLog alert when usage crosses warn/critical
//     thresholds, so a filling disk is noticed before Frigate
//     silently stops recording.
//   - SQLiteMaintenance: periodically checkpoints the WAL (folding
//     app.db-wal back into app.db and truncating it) and takes a
//     consistent daily backup via VACUUM INTO.
//
// Both loops are edge-triggered / self-healing: they log and continue
// on error, never panic, and are safe to run for months unattended.
package maintenance

import (
	"log"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
)

// Config bundles the tunables for all background maintenance loops.
// Zero values disable the corresponding loop.
type Config struct {
	// DBPath is the SQLite database file path (used for backup
	// bookkeeping). Empty disables the SQLite loop.
	DBPath string
	// BackupDir is where daily SQLite snapshots are written.
	BackupDir string
	// BackupKeep is how many daily backups to retain. Default 7.
	BackupKeep int
	// CheckpointInterval is how often the WAL is checkpointed and
	// truncated. Default 6h.
	CheckpointInterval time.Duration
	// BackupInterval is how often a full DB backup is taken.
	// Default 24h. Zero disables backups (checkpoint still runs).
	BackupInterval time.Duration

	// DiskPath is the directory whose filesystem is monitored for
	// free space. Empty disables the disk monitor.
	DiskPath string
	// DiskWarnPct / DiskCritPct are the usage thresholds (0-100).
	// Default 80 / 90.
	DiskWarnPct uint64
	DiskCritPct uint64
	// DiskInterval is how often the disk monitor samples usage.
	// Default 10m.
	DiskInterval time.Duration
}

// StartAll launches every background maintenance loop in its own
// goroutine. It never returns; each loop logs and continues on error.
// Call once at process boot after the DB and EventBus are ready.
func StartAll(db *gorm.DB, bus *eventbus.Bus, cfg Config) {
	if cfg.DiskInterval > 0 && cfg.DiskPath != "" {
		go NewDiskMonitor(db, bus, cfg.DiskPath, cfg.DiskWarnPct, cfg.DiskCritPct, cfg.DiskInterval).Run()
	}
	if cfg.CheckpointInterval > 0 && cfg.DBPath != "" {
		go NewSQLiteMaintenance(db, cfg.DBPath, cfg.BackupDir, cfg.CheckpointInterval, cfg.BackupInterval, cfg.BackupKeep).Run()
	}
	log.Println("maintenance: background loops started")
}
