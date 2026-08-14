package maintenance

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// RecordingSizeMonitor tracks the total on-disk size of Frigate's
// recording tree and alerts when it crosses a configured threshold
// (v1.8.28).
//
// Why this exists: Frigate retains recordings by TIME (record.continuous
// .days / record.motion.days), not by size. On a small NAS disk a
// 1080p camera farm can still fill the disk within the retention window
// (7 days of continuous footage), and the only signal today is the
// generic disk monitor firing at 80%/90% of the WHOLE filesystem — by
// which point recordings may already be the dominant consumer. This
// monitor reports the recordings tree's own size so the operator can
// see, and be alerted about, recording growth specifically.
//
// Alerts are edge-triggered (normal → warn → crit → back to normal).
// The size walk is a full recursive stat of the recordings dir; run it
// on an interval (default 1h) to bound the cost.
//
// v1.8.32: a separate "quota" threshold triggers an automatic action
// (via OnQuotaExceeded / OnQuotaRecovered — e.g. shorten Frigate's
// retention) when the recordings tree crosses it. This is how the
// system keeps a small NAS disk from filling up: once recordings hit
// the quota, old footage is dropped (shorter retention) instead of the
// operator only learning about it from a disk-full alert afterwards.
type RecordingSizeMonitor struct {
	db        *gorm.DB
	bus       *eventbus.Bus
	root      string // recordings root, e.g. /media/frigate/recordings
	interval  time.Duration
	warnBytes uint64
	critBytes uint64

	// v1.8.32: quota + action callbacks. The callbacks return an error
	// so the monitor only commits the crossed state once the action
	// actually succeeds (v1.8.34). Otherwise a transient failure (e.g.
	// the first sample racing the Frigate restart that BootReplay just
	// triggered) would pin quotaActive=true, skip the action, and never
	// retry until the size drops back under quota.
	quotaBytes  uint64
	onExceed    func() error
	onRecover   func() error
	quotaActive bool

	lastLevel string
}

// NewRecordingSizeMonitor creates a monitor for the recordings tree.
func NewRecordingSizeMonitor(db *gorm.DB, bus *eventbus.Bus, root string, interval time.Duration, warnBytes, critBytes uint64, quotaBytes uint64, onExceed, onRecover func() error) *RecordingSizeMonitor {
	return &RecordingSizeMonitor{
		db:         db,
		bus:        bus,
		root:       root,
		interval:   interval,
		warnBytes:  warnBytes,
		critBytes:  critBytes,
		quotaBytes: quotaBytes,
		onExceed:   onExceed,
		onRecover:  onRecover,
	}
}

// Run samples once immediately, then on every tick forever.
func (m *RecordingSizeMonitor) Run() {
	m.sample()
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for range ticker.C {
		m.sample()
	}
}

// sample computes the total size of the recordings tree and emits an
// alert on a level transition.
func (m *RecordingSizeMonitor) sample() {
	size, fileCount, err := dirSize(m.root)
	if err != nil {
		// Root missing (fresh box, no recordings yet) is not an
		// error worth alerting on.
		if os.IsNotExist(err) {
			m.lastLevel = ""
			m.quotaActive = false
			return
		}
		log.Printf("maintenance: recordings size walk failed: %v", err)
		return
	}

	// v1.8.32: quota edge detection. When the recordings tree crosses
	// the quota, fire OnQuotaExceeded (typically shortens Frigate's
	// retention so old footage is deleted); when it drops back below,
	// fire OnQuotaRecovered (restores normal retention). Callbacks run
	// synchronously — the interval is long (1h) so a brief HTTP push
	// (bounded by the caller's context timeout) won't stall anything.
	// The callback closure is responsible for its own cancellation.
	if m.quotaBytes > 0 {
		over := size >= m.quotaBytes
		if over && !m.quotaActive {
			if m.onExceed != nil {
				log.Printf("maintenance: recordings size %s over quota %s — triggering retention reduction",
					humanBytes(size), humanBytes(m.quotaBytes))
				// v1.8.34: only commit the crossed state once the
				// action succeeds. A transient failure (first sample
				// racing the Frigate restart from BootReplay) keeps
				// quotaActive=false so the next sample retries instead
				// of silently skipping the retention reduction.
				if err := m.onExceed(); err != nil {
					log.Printf("maintenance: quota exceeded action failed, will retry next sample: %v", err)
				} else {
					m.quotaActive = true
				}
			} else {
				m.quotaActive = true
			}
		} else if !over && m.quotaActive {
			if m.onRecover != nil {
				log.Printf("maintenance: recordings size back under quota %s — restoring normal retention",
					humanBytes(m.quotaBytes))
				if err := m.onRecover(); err != nil {
					log.Printf("maintenance: quota recovered action failed, will retry next sample: %v", err)
				} else {
					m.quotaActive = false
				}
			} else {
				m.quotaActive = false
			}
		}
	}

	level := ""
	if m.critBytes > 0 && size >= m.critBytes {
		level = "crit"
	} else if m.warnBytes > 0 && size >= m.warnBytes {
		level = "warn"
	}
	if level == m.lastLevel {
		return
	}
	m.lastLevel = level

	if level == "" {
		log.Printf("maintenance: recordings size back to normal: %s", humanBytes(size))
		return
	}

	levelName := "warning"
	sev := model.LevelNormal
	busSev := eventbus.SeverityWarn
	if level == "crit" {
		levelName = "critical"
		sev = model.LevelCritical
		busSev = eventbus.SeverityCritical
	}
	msg := fmt.Sprintf("录像目录大小 %s（%s）", humanBytes(size), levelName)
	payload, _ := json.Marshal(map[string]any{
		"path":       m.root,
		"size_bytes": size,
		"files":      fileCount,
		"level":      level,
	})
	entry := &model.SystemLog{
		Ts:        time.Now().Unix(),
		EventType: "system.recordings_size",
		Level:     sev,
		Source:    "system",
		Message:   msg,
		Payload:   string(payload),
	}
	if err := m.db.Create(entry).Error; err != nil {
		log.Printf("maintenance: recordings size alert persist failed: %v", err)
		return
	}
	raw, _ := json.Marshal(entry)
	m.bus.Publish(eventbus.Event{
		Topic:    eventbus.TopicSystemLog,
		Source:   eventbus.SourceSystem,
		Severity: busSev,
		Payload:  raw,
	})
	log.Printf("maintenance: %s", msg)
}

// dirSize recursively sums the sizes of all regular files under root.
// It follows no symlinks (Frigate's tree is plain files) and skips
// unreadable entries instead of failing the whole walk.
func dirSize(root string) (uint64, int, error) {
	var total uint64
	var count int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// If the root itself is missing, surface that; otherwise
			// skip unreadable subdirs.
			if path == root {
				return err
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, iErr := d.Info()
		if iErr != nil {
			return nil
		}
		total += uint64(info.Size())
		count++
		return nil
	})
	return total, count, err
}

// humanBytes renders a byte count as a compact human string.
func humanBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
