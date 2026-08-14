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

// SysResourceMonitor samples the host's CPU and memory usage and
// writes a SystemLog alert when either crosses warn/critical
// thresholds (v1.8.28).
//
// Why this exists: the disk monitor covers a full disk, but a
// long-running Frigate (OpenVINO detection + ffmpeg transcode) can
// also degrade through CPU saturation or memory pressure without any
// active signal. Sampling /proc/stat + /proc/meminfo and alerting on
// thresholds surfaces the condition in the dashboard before the box
// becomes unresponsive.
//
// Reading strategy: inside the Docker container /proc is the NAMESPACED
// view of the host kernel (the API container shares the host kernel),
// so /proc/stat (CPU agg) and /proc/meminfo are host-wide. This is
// exactly what we want — a monitor for the whole NAS box, not just the
// small API process. Platform-specific readers live in
// (sysres_linux.go / sysres_other.go); non-Linux builds are a no-op.
//
// Alerts are edge-triggered per resource (absent → warn → crit → back
// to normal), so a saturated box doesn't spam the log every tick.
type SysResourceMonitor struct {
	db         *gorm.DB
	bus        *eventbus.Bus
	interval   time.Duration
	cpuWarnPct uint64
	cpuCritPct uint64
	memWarnPct uint64
	memCritPct uint64

	// lastCPULevel / lastMemLevel track the last emitted state
	// ("", "warn", "crit") so we only log on transitions.
	lastCPULevel string
	lastMemLevel string
	// lastCPUTimes holds the previous /proc/stat aggregate for the
	// delta computation.
	lastCPUTotal   uint64
	lastCPUIdle    uint64
	hasLastCPUData bool
}

// NewSysResourceMonitor creates a monitor for CPU + memory usage.
func NewSysResourceMonitor(db *gorm.DB, bus *eventbus.Bus, interval time.Duration, cpuWarn, cpuCrit, memWarn, memCrit uint64) *SysResourceMonitor {
	return &SysResourceMonitor{
		db:         db,
		bus:        bus,
		interval:   interval,
		cpuWarnPct: cpuWarn,
		cpuCritPct: cpuCrit,
		memWarnPct: memWarn,
		memCritPct: memCrit,
	}
}

// Run samples immediately, then on every tick forever.
func (m *SysResourceMonitor) Run() {
	m.sample()
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for range ticker.C {
		m.sample()
	}
}

// sample reads CPU + memory and emits alerts on transitions.
func (m *SysResourceMonitor) sample() {
	// --- Memory ---
	total, available, memErr := memUsage()
	if memErr == nil && total > 0 {
		used := total - available
		pct := used * 100 / total
		m.emitMem(float64(pct), total, available)
	} else if memErr != nil {
		log.Printf("maintenance: memory read failed: %v", memErr)
	}

	// --- CPU ---
	total, idle, cpuErr := cpuUsage()
	if cpuErr != nil {
		log.Printf("maintenance: cpu read failed: %v", cpuErr)
		return
	}
	if m.hasLastCPUData {
		dTotal := total - m.lastCPUTotal
		dIdle := idle - m.lastCPUIdle
		if dTotal > 0 {
			pct := float64(dTotal-dIdle) * 100 / float64(dTotal)
			m.emitCPU(pct)
		}
	}
	m.lastCPUTotal = total
	m.lastCPUIdle = idle
	m.hasLastCPUData = true
}

// emitMem handles the memory alert edge-trigger.
func (m *SysResourceMonitor) emitMem(pct float64, total, available uint64) {
	level := ""
	if pct >= float64(m.memCritPct) {
		level = "crit"
	} else if pct >= float64(m.memWarnPct) {
		level = "warn"
	}
	if level == m.lastMemLevel {
		return
	}
	m.lastMemLevel = level
	if level == "" {
		log.Printf("maintenance: memory usage back to normal: %.1f%% used", pct)
		return
	}
	levelName := "warning"
	sev := model.LevelWarning
	busSev := eventbus.SeverityWarn
	if level == "crit" {
		levelName = "critical"
		sev = model.LevelCritical
		busSev = eventbus.SeverityCritical
	}
	msg := fmt.Sprintf("内存使用率 %.1f%%（%s）", pct, levelName)
	payload, _ := json.Marshal(map[string]any{
		"resource": "memory",
		"used_pct": pct,
		"total":    total,
		"avail":    available,
		"level":    level,
	})
	m.emit(sev, busSev, "system.memory", msg, payload)
}

// emitCPU handles the CPU alert edge-trigger.
func (m *SysResourceMonitor) emitCPU(pct float64) {
	level := ""
	if pct >= float64(m.cpuCritPct) {
		level = "crit"
	} else if pct >= float64(m.cpuWarnPct) {
		level = "warn"
	}
	if level == m.lastCPULevel {
		return
	}
	m.lastCPULevel = level
	if level == "" {
		log.Printf("maintenance: cpu usage back to normal: %.1f%% used", pct)
		return
	}
	levelName := "warning"
	sev := model.LevelWarning
	busSev := eventbus.SeverityWarn
	if level == "crit" {
		levelName = "critical"
		sev = model.LevelCritical
		busSev = eventbus.SeverityCritical
	}
	msg := fmt.Sprintf("CPU 使用率 %.1f%%（%s）", pct, levelName)
	payload, _ := json.Marshal(map[string]any{
		"resource": "cpu",
		"used_pct": pct,
		"level":    level,
	})
	m.emit(sev, busSev, "system.cpu", msg, payload)
}

// emit writes a SystemLog row and re-publishes it on system.log.
func (m *SysResourceMonitor) emit(sev, busSev, eventType, msg string, payload []byte) {
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
