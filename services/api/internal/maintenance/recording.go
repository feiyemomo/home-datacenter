package maintenance

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// RecordingTarget is one camera that is expected to be writing
// recordings. The caller (main.go) resolves the Frigate slug + friendly
// name from the camera registry and hands them in via a callback, so
// this package stays decoupled from the camera package.
type RecordingTarget struct {
	Slug string // unique Frigate slug, e.g. "front_door"
	Name string // friendly name for the alert message
}

// RecordingMonitor detects cameras that are supposed to be recording
// but have silently stopped (v1.8.28).
//
// Why this exists: the existing camera health check is only an RTSP TCP
// probe. It reports "online" when the RTSP port accepts a connection,
// but says nothing about whether Frigate's recording pipeline is
// actually writing files. A failed FFmpeg decode, a detached stream, or
// a broken record pipeline can leave the camera "online" while recordings
// silently stop — and the user only discovers it when they try to replay
// a missing minute days later.
//
// This monitor walks Frigate's on-disk recording layout
//
//	/media/frigate/recordings/YYYY-MM-DD/HH/<slug>/MM.SS.mp4
//
// for every expected camera and finds the newest 10s segment's mtime.
// If it is older than `staleAfter` (default 10 min), the camera is
// reported as recording-stalled: a critical SystemLog row
// (event_type=system.recording) is written and a live system.log event
// broadcast so the dashboard flags it immediately.
//
// Alerts are edge-triggered per camera (a "stalled" state and a
// "recovered" state), so a camera that stays broken doesn't spam the
// log every tick.
type RecordingMonitor struct {
	db         *gorm.DB
	bus        *eventbus.Bus
	root       string // recordings root, e.g. /media/frigate/recordings
	staleAfter time.Duration
	interval   time.Duration
	// targets refreshes the expected-to-record camera set each tick
	// (cheap: it's a DB List + slug computation). A nil or empty list
	// means "nothing to check".
	targets func() []RecordingTarget

	// stalled tracks cameras currently in the stalled state (slug →
	// true) so we only emit on transitions.
	stalled map[string]bool
}

// NewRecordingMonitor creates a monitor that periodically scans the
// recordings tree for stale cameras.
func NewRecordingMonitor(db *gorm.DB, bus *eventbus.Bus, root string, staleAfter, interval time.Duration, targets func() []RecordingTarget) *RecordingMonitor {
	return &RecordingMonitor{
		db:         db,
		bus:        bus,
		root:       root,
		staleAfter: staleAfter,
		interval:   interval,
		targets:    targets,
		stalled:    make(map[string]bool),
	}
}

// Run scans once immediately, then every interval forever.
func (m *RecordingMonitor) Run() {
	m.sample()
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for range ticker.C {
		m.sample()
	}
}

// sample checks every expected camera and emits alerts on state
// transitions. Errors are logged and swallowed — a transient scan
// failure must never take down the loop.
func (m *RecordingMonitor) sample() {
	if m.targets == nil {
		return
	}
	targets := m.targets()
	if len(targets) == 0 {
		// No cameras to check; clear any stale stalled state so a
		// camera removed from the set doesn't linger.
		m.stalled = make(map[string]bool)
		return
	}

	now := time.Now()
	for _, t := range targets {
		latest, ok := m.latestSegment(t.Slug)
		if !ok {
			// No segments at all — treat as stalled (camera is
			// expected to record but has recorded nothing).
			m.transition(t, now, true, "no recording segments found")
			continue
		}
		age := now.Sub(latest)
		if age > m.staleAfter {
			m.transition(t, now, true, fmt.Sprintf("last segment %d min ago", int(age.Minutes())))
		} else {
			m.transition(t, now, false, "")
		}
	}
}

// transition emits (or clears) the stalled alert for a camera only on
// a state change.
func (m *RecordingMonitor) transition(t RecordingTarget, now time.Time, stalled bool, reason string) {
	if m.stalled[t.Slug] == stalled {
		return // no change
	}
	m.stalled[t.Slug] = stalled

	if !stalled {
		log.Printf("maintenance: recording recovered for %s (%s)", t.Name, t.Slug)
		return
	}

	msg := fmt.Sprintf("摄像头 %s 录像疑似中断（%s）", t.Name, reason)
	payload, _ := json.Marshal(map[string]any{
		"camera": t.Name,
		"slug":   t.Slug,
		"reason": reason,
		"ts":     now.Unix(),
	})
	entry := &model.SystemLog{
		Ts:        now.Unix(),
		EventType: "system.recording",
		Level:     model.LevelCritical,
		Source:    "system",
		Message:   msg,
		Payload:   string(payload),
	}
	if err := m.db.Create(entry).Error; err != nil {
		log.Printf("maintenance: recording alert persist failed: %v", err)
		return
	}
	raw, _ := json.Marshal(entry)
	m.bus.Publish(eventbus.Event{
		Topic:    eventbus.TopicSystemLog,
		Source:   eventbus.SourceSystem,
		Severity: eventbus.SeverityCritical,
		Payload:  raw,
	})
	log.Printf("maintenance: recording alert: %s", msg)
}

// latestSegment returns the mtime of the newest .mp4 segment under the
// camera's slug directory, and whether any was found.
//
// Recording layout is /recordings/YYYY-MM-DD/HH/<slug>/MM.SS.mp4 with
// UTC date/hour components (see Registry.RecordingSegmentsForMinute).
// To keep the scan cheap we only descend into the most recent 2 day
// directories (today + yesterday in UTC) — anything older than that is
// by definition stale for a 10-minute window, so a camera that last
// wrote 2 days ago returns "stale" without a full-tree walk.
func (m *RecordingMonitor) latestSegment(slug string) (time.Time, bool) {
	latest := time.Time{}
	found := false

	days := m.recentDays(2)
	for _, day := range days {
		dayDir := filepath.Join(m.root, day)
		hours, err := os.ReadDir(dayDir)
		if err != nil {
			continue // no such day dir
		}
		for _, h := range hours {
			if !h.IsDir() {
				continue
			}
			slugDir := filepath.Join(dayDir, h.Name(), slug)
			entries, err := os.ReadDir(slugDir)
			if err != nil {
				continue
			}
			var names []string
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				nm := e.Name()
				if len(nm) < 5 || nm[len(nm)-4:] != ".mp4" {
					continue
				}
				names = append(names, filepath.Join(slugDir, nm))
			}
			if len(names) == 0 {
				continue
			}
			// Sort by name; the timestamped MM.SS.mp4 names sort
			// chronologically within a day/hour, and different
			// hour dirs are walked in ascending order. The newest
			// across both days is the max mtime anyway.
			sort.Strings(names)
			for _, p := range names {
				fi, err := os.Stat(p)
				if err != nil {
					continue
				}
				if fi.ModTime().After(latest) {
					latest = fi.ModTime()
					found = true
				}
			}
		}
	}
	return latest, found
}

// recentDays returns the last n day-directory names in UTC
// (YYYY-MM-DD), oldest first, so the scan naturally hits today then
// yesterday.
func (m *RecordingMonitor) recentDays(n int) []string {
	days := make([]string, 0, n)
	now := time.Now().UTC()
	for i := n - 1; i >= 0; i-- {
		days = append(days, now.AddDate(0, 0, -i).Format("2006-01-02"))
	}
	return days
}
