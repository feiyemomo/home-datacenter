package maintenance

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// DiskMonitor samples free space on the data filesystem and writes a
// SystemLog alert when usage crosses the warn/critical thresholds.
//
// Alerts are edge-triggered: a level is only logged when the usage
// TRANSITIONS into that level (or back to normal), so a disk hovering
// at 90% doesn't spam the log every tick. The operator sees one
// "磁盘使用率 90%（critical）" row when it crosses, and one "已恢复"
// row when it drops back below warn.
//
// Why this exists: Frigate's recording pipeline silently stops when
// the disk fills — there is no built-in alert. A full disk also
// breaks SQLite writes and the transcode cache. This monitor surfaces
// the condition in the dashboard's system log (and, via the EventBus,
// to connected WebSocket clients) before it becomes an outage.
//
// The actual statfs syscall is platform-specific: the production
// deployment is a Linux Docker container (disk_linux.go); non-Linux
// builds (local Windows dev) get a stub that logs and skips
// (disk_other.go).
type DiskMonitor struct {
	db       *gorm.DB
	bus      *eventbus.Bus
	path     string
	warnPct  uint64
	critPct  uint64
	interval time.Duration

	// lastLevel tracks the last emitted state ("", "warn", "crit")
	// so we only log on transitions.
	lastLevel string
}

// NewDiskMonitor creates a monitor for the filesystem containing path.
func NewDiskMonitor(db *gorm.DB, bus *eventbus.Bus, path string, warnPct, critPct uint64, interval time.Duration) *DiskMonitor {
	return &DiskMonitor{db: db, bus: bus, path: path, warnPct: warnPct, critPct: critPct, interval: interval}
}

// Run samples disk usage immediately, then on every tick forever.
func (m *DiskMonitor) Run() {
	m.sample()
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for range ticker.C {
		m.sample()
	}
}

// sample reads the filesystem stats and emits an alert on a level
// transition. Errors are logged and swallowed — a transient statfs
// failure must not take down the loop.
func (m *DiskMonitor) sample() {
	total, avail, err := diskUsage(m.path)
	if err != nil {
		log.Printf("maintenance: disk statfs failed for %s: %v", m.path, err)
		return
	}
	used := total - avail
	pct := uint64(0)
	if total > 0 {
		pct = used * 100 / total
	}

	level := ""
	if pct >= m.critPct {
		level = "crit"
	} else if pct >= m.warnPct {
		level = "warn"
	}
	if level == m.lastLevel {
		return
	}
	m.lastLevel = level

	if level == "" {
		log.Printf("maintenance: disk usage back to normal on %s: %d%% used", m.path, pct)
		return
	}

	// Build a human-readable SystemLog row + push it to the bus so
	// connected dashboards see it live.
	levelName := "warning"
	sev := model.LevelNormal
	busSev := eventbus.SeverityWarn
	if level == "crit" {
		levelName = "critical"
		sev = model.LevelCritical
		busSev = eventbus.SeverityCritical
	}
	msg := fmt.Sprintf("磁盘使用率 %d%%（%s）", pct, levelName)
	payload, _ := json.Marshal(map[string]any{
		"path":     m.path,
		"used_pct": pct,
		"total":    total,
		"avail":    avail,
		"level":    level,
	})
	entry := &model.SystemLog{
		Ts:        time.Now().Unix(),
		EventType: "system.disk",
		Level:     sev,
		Source:    "system",
		Message:   msg,
		Payload:   string(payload),
	}
	if err := m.db.Create(entry).Error; err != nil {
		log.Printf("maintenance: disk alert persist failed: %v", err)
		return
	}
	// Re-publish on system.log so the WS Hub broadcasts it live.
	raw, _ := json.Marshal(entry)
	m.bus.Publish(eventbus.Event{
		Topic:    eventbus.TopicSystemLog,
		Source:   eventbus.SourceSystem,
		Severity: busSev,
		Payload:  raw,
	})
	log.Printf("maintenance: disk alert: %s", msg)
}
