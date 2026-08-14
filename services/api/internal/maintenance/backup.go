package maintenance

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// BackupStatus is the shape of the state file written by the `backup`
// container (rclone) after every sync. It lives on the host at
// data/backup-state/last.json and is mounted read-only into the API
// container, so the monitor here is a passive reader — it never writes
// to the state the backup loop owns.
//
// The backup container writes this atomically (write temp + rename) so
// a partially-written file is never observed. All fields are numbers or
// strings; Ts is a Unix epoch in seconds.
type BackupStatus struct {
	// Ts is the wall-clock time of the last sync attempt (Unix sec).
	Ts int64 `json:"ts"`
	// OK is true when the last rclone sync succeeded.
	OK bool `json:"ok"`
	// Error is a short human-readable reason when OK is false.
	Error string `json:"error,omitempty"`
	// RemoteFiles is the number of objects found in the bucket after
	// the sync (rclone `size` line "Total objects").
	RemoteFiles int64 `json:"remote_files"`
	// RemoteBytes is the total bytes of all objects in the bucket
	// after the sync (rclone `size` line "Total size").
	RemoteBytes int64 `json:"remote_bytes"`
	// DurationSec is how long the last sync took.
	DurationSec float64 `json:"duration_sec,omitempty"`
}

// BackupMonitor watches the off-NAS backup's health by reading the
// state file written by the `backup` container (v1.8.29).
//
// Why this exists: the backup container is a separate rclone process
// that only talks to Bitiful (亿安云) — the API has no handle on whether
// it's succeeding. rclone sync failures today only land in the backup
// container's logs, invisible to the dashboard. This monitor reads the
// shared state file and raises two classes of alert:
//
//  1. Failure / staleness: if the last sync reported OK=false, or the
//     state file is older than StaleAfter (meaning the backup loop
//     stopped writing entirely — e.g. the container died or the
//     interval grew), a system.backup critical alert is raised.
//  2. Retention: if the bucket's object count or total bytes cross
//     warn/crit thresholds, a system.backup alert fires so the offsite
//     copy doesn't silently outgrow the free tier.
//
// Both are edge-triggered (only fire on transitions + self-heal when
// the condition clears), matching the other maintenance monitors.
type BackupMonitor struct {
	db         *gorm.DB
	bus        *eventbus.Bus
	statePath  string
	interval   time.Duration
	staleAfter time.Duration

	// capacity thresholds (0 = that threshold disabled)
	warnFiles int64
	critFiles int64
	warnBytes int64
	critBytes int64

	// lastFail / lastCapLevel track the last emitted alert state so we
	// only log on transitions.
	lastFail     bool
	lastCapLevel string // "", "warn", "crit"
	// sawHealthy records whether the last observed state was a healthy
	// (OK=true) sync, so a subsequent missing state file is treated as
	// a real failure rather than a fresh-install no-op.
	sawHealthy bool
}

// NewBackupMonitor creates a monitor for the off-NAS (Bitiful) backup.
// statePath is the host backup state file mounted read-only into the
// API container (e.g. /data/backup-state/last.json). If the file is
// absent the monitor is a quiet no-op until the backup container first
// writes it (no false "backup down" alert on a fresh install).
func NewBackupMonitor(db *gorm.DB, bus *eventbus.Bus, statePath string, interval, staleAfter time.Duration, warnFiles, critFiles, warnBytes, critBytes int64) *BackupMonitor {
	return &BackupMonitor{
		db:         db,
		bus:        bus,
		statePath:  statePath,
		interval:   interval,
		staleAfter: staleAfter,
		warnFiles:  warnFiles,
		critFiles:  critFiles,
		warnBytes:  warnBytes,
		critBytes:  critBytes,
	}
}

// Run polls the state file immediately, then on every tick forever.
func (m *BackupMonitor) Run() {
	m.check()
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for range ticker.C {
		m.check()
	}
}

// check reads the state file once and emits alerts on transitions.
func (m *BackupMonitor) check() {
	st, err := m.readState()
	if err != nil {
		// Missing / malformed state file is NOT itself a failure alert
		// (the backup container may legitimately not have run yet on a
		// fresh deploy). Log and return; if the backup loop dies later,
		// the staleness path below raises the alert once the file goes
		// older than StaleAfter.
		//
		// But if we previously saw a healthy state and now it's gone,
		// that IS interesting — surface it once.
		if m.lastFail == false && m.sawHealthy {
			log.Printf("maintenance: backup state file disappeared (last healthy sync lost)")
			// treat as a failure: the backup source of truth is gone
			m.lastFail = true
			m.emitAlert(model.LevelCritical, eventbus.SeverityCritical, "system.backup",
				"异地备份状态文件丢失，无法确认备份健康",
				[]byte(`{"ok":false,"error":"state file missing"}`))
		}
		log.Printf("maintenance: backup state read failed: %v", err)
		return
	}
	m.sawHealthy = false
	if st.OK {
		m.sawHealthy = true
	}

	// --- Failure / staleness ---
	now := time.Now().Unix()
	stale := st.Ts > 0 && now-st.Ts > int64(m.staleAfter.Seconds())
	fail := !st.OK || stale
	if fail != m.lastFail {
		m.lastFail = fail
		if fail {
			msg := "异地备份异常"
			if stale {
				msg = fmt.Sprintf("异地备份已停滞 %s 未同步", time.Duration(now-st.Ts)*time.Second)
			} else if st.Error != "" {
				msg = fmt.Sprintf("异地备份失败：%s", st.Error)
			}
			payload, _ := json.Marshal(map[string]any{
				"ok":           false,
				"ts":           st.Ts,
				"error":        st.Error,
				"stale_secs":   now - st.Ts,
				"remote_files": st.RemoteFiles,
				"remote_bytes": st.RemoteBytes,
			})
			m.emitAlert(model.LevelCritical, eventbus.SeverityCritical, "system.backup", msg, payload)
		} else {
			log.Printf("maintenance: backup back to normal (last sync ok)")
		}
	}

	// --- Retention / capacity ---
	capLevel := ""
	if m.critFiles > 0 && st.RemoteFiles >= m.critFiles {
		capLevel = "crit"
	} else if m.warnFiles > 0 && st.RemoteFiles >= m.warnFiles {
		capLevel = "warn"
	} else if m.critBytes > 0 && st.RemoteBytes >= m.critBytes {
		capLevel = "crit"
	} else if m.warnBytes > 0 && st.RemoteBytes >= m.warnBytes {
		capLevel = "warn"
	}
	if capLevel != m.lastCapLevel {
		m.lastCapLevel = capLevel
		if capLevel == "" {
			log.Printf("maintenance: backup retention back to normal")
			return
		}
		levelName := "warning"
		sev := model.LevelNormal
		busSev := eventbus.SeverityWarn
		if capLevel == "crit" {
			levelName = "critical"
			sev = model.LevelCritical
			busSev = eventbus.SeverityCritical
		}
		msg := fmt.Sprintf("异地备份容量告警（%s）：bucket 内 %d 个对象 / %s",
			levelName, st.RemoteFiles, humanBytes(uint64(st.RemoteBytes)))
		payload, _ := json.Marshal(map[string]any{
			"ok":           true,
			"ts":           st.Ts,
			"remote_files": st.RemoteFiles,
			"remote_bytes": st.RemoteBytes,
			"level":        capLevel,
		})
		m.emitAlert(sev, busSev, "system.backup", msg, payload)
	}
}

// emitAlert writes a SystemLog row and re-publishes it on system.log,
// identical to the other maintenance monitors.
func (m *BackupMonitor) emitAlert(sev, busSev, eventType, msg string, payload []byte) {
	entry := &model.SystemLog{
		Ts:        time.Now().Unix(),
		EventType: eventType,
		Level:     sev,
		Source:    "system",
		Message:   msg,
		Payload:   string(payload),
	}
	if err := m.db.Create(entry).Error; err != nil {
		log.Printf("maintenance: %s alert persist failed: %v", eventType, err)
		return
	}
	raw, _ := json.Marshal(entry)
	m.bus.Publish(eventbus.Event{
		Topic:    eventbus.TopicSystemLog,
		Source:   eventbus.SourceSystem,
		Severity: busSev,
		Payload:  raw,
	})
	log.Printf("maintenance: %s: %s", eventType, msg)
}

// readState parses the state file.
func (m *BackupMonitor) readState() (*BackupStatus, error) {
	data, err := os.ReadFile(m.statePath)
	if err != nil {
		return nil, err
	}
	var st BackupStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}