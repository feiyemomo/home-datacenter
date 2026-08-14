package maintenance

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// SQLiteMaintenance keeps the SQLite database healthy on long-running
// deployments. Two jobs run on independent timers:
//
//  1. WAL checkpoint (TRUNCATE) — the app opens SQLite in WAL mode
//     (see database.InitDB). Writes land in app.db-wal and are only
//     folded back into app.db when the WAL reaches its auto-checkpoint
//     size (default 1000 pages). On a busy home-api the -wal file can
//     grow to tens of MB while app.db stays small. A periodic
//     PRAGMA wal_checkpoint(TRUNCATE) folds the WAL into the main DB
//     and truncates the file back to ~0, reclaiming disk and keeping
//     crash-recovery fast.
//
//  2. Daily backup — VACUUM INTO produces a consistent, compacted
//     snapshot of the live DB even while it is being written (WAL
//     mode). Backups are written to BackupDir with a timestamped name
//     and pruned to the newest BackupKeep.
type SQLiteMaintenance struct {
	db                 *gorm.DB
	dbPath             string
	backupDir          string
	checkpointInterval time.Duration
	backupInterval     time.Duration
	backupKeep         int
}

// NewSQLiteMaintenance creates a maintenance loop bound to the given
// DB. dbPath is the SQLite file path (for backup bookkeeping),
// backupDir is where snapshots go.
func NewSQLiteMaintenance(db *gorm.DB, dbPath, backupDir string, checkpointInterval, backupInterval time.Duration, backupKeep int) *SQLiteMaintenance {
	return &SQLiteMaintenance{
		db:                 db,
		dbPath:             dbPath,
		backupDir:          backupDir,
		checkpointInterval: checkpointInterval,
		backupInterval:     backupInterval,
		backupKeep:         backupKeep,
	}
}

// Run executes the maintenance loop forever. It checkpoints once at
// startup (so an in-place upgrade immediately folds any accumulated
// WAL), then runs both jobs on their own timers. A nil backup channel
// (backupInterval == 0) simply blocks forever in select, so only
// checkpointing runs.
func (m *SQLiteMaintenance) Run() {
	m.checkpoint()
	ckpt := time.NewTicker(m.checkpointInterval)
	defer ckpt.Stop()

	var backup <-chan time.Time
	if m.backupInterval > 0 {
		t := time.NewTicker(m.backupInterval)
		defer t.Stop()
		backup = t.C
	}

	for {
		select {
		case <-ckpt.C:
			m.checkpoint()
		case <-backup:
			m.backup()
		}
	}
}

// checkpoint folds the WAL into the main DB and truncates the -wal
// file back to ~0, reclaiming disk space.
func (m *SQLiteMaintenance) checkpoint() {
	if err := m.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error; err != nil {
		log.Printf("maintenance: sqlite wal checkpoint failed: %v", err)
		return
	}
	log.Println("maintenance: sqlite WAL checkpointed (TRUNCATE)")
}

// backup writes a consistent snapshot via VACUUM INTO and prunes old
// backups. VACUUM INTO is safe to run against a live WAL-mode DB: it
// produces a compacted, self-contained copy without blocking writers.
func (m *SQLiteMaintenance) backup() {
	if m.backupDir == "" {
		return
	}
	if err := os.MkdirAll(m.backupDir, 0o755); err != nil {
		log.Printf("maintenance: backup dir create failed: %v", err)
		return
	}
	name := "app-" + time.Now().Format("20060102-150405") + ".db"
	dst := filepath.Join(m.backupDir, name)
	// VACUUM INTO requires the destination not to exist.
	_ = os.Remove(dst)
	// The path is internal (not user input); single-quote it into the
	// SQL literal so spaces/special chars are safe.
	quoted := "'" + strings.ReplaceAll(dst, "'", "''") + "'"
	if err := m.db.Exec("VACUUM INTO " + quoted).Error; err != nil {
		log.Printf("maintenance: sqlite backup failed: %v", err)
		return
	}
	log.Printf("maintenance: sqlite backup written to %s", dst)
	m.pruneBackups()
}

// pruneBackups keeps only the newest backupKeep backups. Timestamped
// names sort chronologically, so the lexicographically-first entries
// are the oldest.
func (m *SQLiteMaintenance) pruneBackups() {
	if m.backupKeep <= 0 {
		return
	}
	entries, err := os.ReadDir(m.backupDir)
	if err != nil {
		return
	}
	var backups []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "app-") && strings.HasSuffix(e.Name(), ".db") {
			backups = append(backups, e.Name())
		}
	}
	sort.Strings(backups)
	for len(backups) > m.backupKeep {
		old := filepath.Join(m.backupDir, backups[0])
		if err := os.Remove(old); err == nil {
			log.Printf("maintenance: pruned old sqlite backup %s", old)
		}
		backups = backups[1:]
	}
}
