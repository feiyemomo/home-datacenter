// Package maintenance hosts the background "keep the box healthy on
// long-running deployments" loops (v1.8.27, v1.8.28):
//
//   - DiskMonitor: samples free space on the data filesystem and
//     writes a SystemLog alert when usage crosses warn/critical
//     thresholds, so a filling disk is noticed before Frigate
//     silently stops recording.
//   - SQLiteMaintenance: periodically checkpoints the WAL (folding
//     app.db-wal back into app.db and truncating it) and takes a
//     consistent daily backup via VACUUM INTO.
//   - RecordingMonitor: detects cameras that are expected to record
//     but whose newest on-disk segment is stale (v1.8.28).
//   - RecordingSizeMonitor: tracks the recordings tree's total size
//     and alerts on warn/crit thresholds (v1.8.28).
//   - SysResourceMonitor: samples host CPU + memory usage and alerts
//     on thresholds (v1.8.28).
//
// All loops are edge-triggered / self-healing: they log and continue
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
	// FrigateDBPath is Frigate's own SQLite database (frigate.db),
	// snapshotted daily into the same backup dir (v1.8.28). Empty
	// disables the extra backup.
	FrigateDBPath string

	// BackupStatePath is the host path of the off-NAS backup status
	// file (data/backup-state/last.json) mounted read-only into the
	// API container (v1.8.29). Empty disables the backup monitor.
	BackupStatePath string
	// BackupMonitorInterval is how often the backup status file is
	// polled. Default 5m.
	BackupMonitorInterval time.Duration
	// BackupStaleAfter is how old the last sync must be before the
	// backup is considered stalled. Default 2x the sync interval.
	BackupStaleAfter time.Duration
	// BackupWarnFiles / BackupCritFiles are bucket object-count
	// thresholds for the retention alert. 0 disables that level.
	BackupWarnFiles int64
	BackupCritFiles int64
	// BackupWarnBytes / BackupCritBytes are bucket total-size
	// thresholds for the retention alert. 0 disables that level.
	BackupWarnBytes int64
	BackupCritBytes int64

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

	// RecordingsRoot is Frigate's recording directory root (e.g.
	// /media/frigate/recordings). Empty disables the recording
	// health + size monitors.
	RecordingsRoot string
	// RecordingStaleAfter is how old the newest segment must be
	// before a camera is flagged as recording-stalled. Default 10m.
	RecordingStaleAfter time.Duration
	// RecordingCheckInterval is how often the recording health scan
	// runs. Default 5m.
	RecordingCheckInterval time.Duration
	// RecordingTargets returns the cameras expected to record
	// (slug + friendly name). Nil disables the health scan.
	RecordingTargets func() []RecordingTarget
	// RecordingSizeInterval is how often the recordings tree size is
	// walked. Default 1h.
	RecordingSizeInterval time.Duration
	// RecordingSizeWarnBytes / RecordingSizeCritBytes are size
	// thresholds for the recordings tree. 0 disables that level.
	RecordingSizeWarnBytes uint64
	RecordingSizeCritBytes uint64
	// RecordingQuotaBytes is the soft quota for the recordings tree
	// (v1.8.32). When the total size crosses it, OnQuotaExceeded fires
	// (e.g. shorten Frigate retention); when it drops back below,
	// OnQuotaRecovered restores normal retention. 0 disables the
	// automatic action.
	RecordingQuotaBytes uint64
	// OnQuotaExceeded / OnQuotaRecovered are invoked on quota edge
	// transitions. Nil callbacks are silently skipped (monitor still
	// runs; alerts unaffected). They return an error (v1.8.34): a
	// non-nil error means the action didn't take effect, so the monitor
	// keeps the previous state and retries on the next sample.
	OnQuotaExceeded  func() error
	OnQuotaRecovered func() error

	// SysResourceInterval is how often CPU + memory are sampled.
	// 0 disables the resource monitor.
	SysResourceInterval time.Duration
	// CPUWarnPct / CPUCritPct are CPU usage thresholds (0-100).
	CPUWarnPct uint64
	CPUCritPct uint64
	// MemWarnPct / MemCritPct are memory usage thresholds (0-100).
	MemWarnPct uint64
	MemCritPct uint64

	// ServiceProbes are the sibling services to probe for liveness
	// (v1.8.28). Empty disables the service monitor.
	ServiceProbes []ServiceProbe
	// ServiceInterval is how often the service probes run.
	ServiceInterval time.Duration
	// ServiceFails is how many consecutive failures flag a service
	// as down. Default 3.
	ServiceFails int
	// ServiceStartupDelay is how long to wait after process boot before
	// the FIRST service probe (v1.8.33). The api container's embedded
	// DNS (127.0.0.11) can briefly fail to resolve sibling hostnames
	// right after a (re)start, so an immediate probe would rack up
	// consecutive failures and fire a spurious "不可达" alert. Waiting
	// out the startup window before sampling eliminates the false
	// positive; the consecutive-failure threshold then only guards
	// against genuine outages.
	ServiceStartupDelay time.Duration
}

// StartAll launches every background maintenance loop in its own
// goroutine. It never returns; each loop logs and continues on error.
// Call once at process boot after the DB and EventBus are ready.
func StartAll(db *gorm.DB, bus *eventbus.Bus, cfg Config) {
	if cfg.DiskInterval > 0 && cfg.DiskPath != "" {
		go NewDiskMonitor(db, bus, cfg.DiskPath, cfg.DiskWarnPct, cfg.DiskCritPct, cfg.DiskInterval).Run()
	}
	if cfg.CheckpointInterval > 0 && cfg.DBPath != "" {
		sm := NewSQLiteMaintenance(db, cfg.DBPath, cfg.BackupDir, cfg.CheckpointInterval, cfg.BackupInterval, cfg.BackupKeep)
		if cfg.FrigateDBPath != "" {
			sm.SetFrigateDBPath(cfg.FrigateDBPath)
		}
		go sm.Run()
	}
	if cfg.BackupStatePath != "" && cfg.BackupMonitorInterval > 0 {
		go NewBackupMonitor(db, bus, cfg.BackupStatePath, cfg.BackupMonitorInterval, cfg.BackupStaleAfter, cfg.BackupWarnFiles, cfg.BackupCritFiles, cfg.BackupWarnBytes, cfg.BackupCritBytes).Run()
	}
	if cfg.RecordingsRoot != "" && cfg.RecordingCheckInterval > 0 && cfg.RecordingTargets != nil {
		go NewRecordingMonitor(db, bus, cfg.RecordingsRoot, cfg.RecordingStaleAfter, cfg.RecordingCheckInterval, cfg.RecordingTargets).Run()
	}
	if cfg.RecordingsRoot != "" && cfg.RecordingSizeInterval > 0 {
		go NewRecordingSizeMonitor(db, bus, cfg.RecordingsRoot, cfg.RecordingSizeInterval, cfg.RecordingSizeWarnBytes, cfg.RecordingSizeCritBytes, cfg.RecordingQuotaBytes, cfg.OnQuotaExceeded, cfg.OnQuotaRecovered).Run()
	}
	if cfg.SysResourceInterval > 0 {
		go NewSysResourceMonitor(db, bus, cfg.SysResourceInterval, cfg.CPUWarnPct, cfg.CPUCritPct, cfg.MemWarnPct, cfg.MemCritPct).Run()
	}
	if cfg.ServiceInterval > 0 && len(cfg.ServiceProbes) > 0 {
		go NewServiceMonitor(db, bus, cfg.ServiceProbes, cfg.ServiceInterval, cfg.ServiceFails, cfg.ServiceStartupDelay).Run()
	}
	log.Println("maintenance: background loops started")
}
