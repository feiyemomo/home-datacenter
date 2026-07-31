// Package log turns EventBus events into human-readable SystemLog
// rows persisted to SQLite. A REST endpoint (internal/handler/
// system_log_handler.go) serves the backlog; the WS Hub subscribes
// to the "system.log" topic this package republishes on each
// successful write so connected dashboards get the new entry in
// real time.
package log

import (
	"encoding/json"
	"fmt"
	"log"

	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// Subscriber bridges EventBus events into the SystemLog table.
//
// On Start it subscribes to a fixed set of topics (device.status,
// camera.online / offline / status_changed, user.login / logout).
// Each event is decoded, turned into a Chinese human-readable
// message, persisted as a SystemLog row, and re-published on the
// "system.log" topic so the WS Hub can fan it out to dashboards.
type Subscriber struct {
	db  *gorm.DB
	bus *eventbus.Bus
}

// NewSubscriber wires a Subscriber to the given DB and Bus. The
// Subscriber must be started with Start() before it does anything.
func NewSubscriber(db *gorm.DB, bus *eventbus.Bus) *Subscriber {
	return &Subscriber{db: db, bus: bus}
}

// Start registers all the EventBus subscriptions. It is idempotent
// in the sense that calling it twice will subscribe twice — but the
// intended lifecycle is a single Start at process boot.
func (s *Subscriber) Start() {
	topics := []string{
		eventbus.TopicDeviceStatus,
		eventbus.TopicCameraOnline,
		eventbus.TopicCameraOffline,
		eventbus.TopicCameraStatusChanged,
		eventbus.TopicUserLogin,
		eventbus.TopicUserLogout,
	}
	for _, t := range topics {
		// Capture the topic in a local variable so the closure
		// sees the current value, not the last loop iteration's.
		topic := t
		s.bus.Subscribe(topic, func(e eventbus.Event) {
			s.handle(topic, e)
		})
	}
}

// handle is the per-topic dispatcher. It builds a SystemLog from
// the event payload, persists it, and re-publishes on system.log.
//
// Errors are logged but never panic: a failed DB write shouldn't
// take down the API, and the originating event has already been
// processed by its other subscribers (WS Hub, automation engine).
func (s *Subscriber) handle(topic string, e eventbus.Event) {
	entry := s.buildEntry(topic, e)
	if entry == nil {
		return
	}

	if err := s.db.Create(entry).Error; err != nil {
		log.Printf("systemlog: persist failed for topic=%s: %v", topic, err)
		return
	}

	// v1.6.37: prune per-level backlog so routine events don't
	// drown out urgent ones in SQLite. Critical events are kept
	// indefinitely (audit trail); normal/info are capped. See
	// pruneSystemLogs for the retention table.
	s.pruneSystemLogs()

	// Re-publish the freshly persisted row (now with its ID) on
	// the system.log topic so the WS Hub can broadcast it.
	payload, err := json.Marshal(entry)
	if err != nil {
		log.Printf("systemlog: marshal failed for topic=%s: %v", topic, err)
		return
	}
	s.bus.Publish(eventbus.Event{
		Topic:   eventbus.TopicSystemLog,
		Source:  eventbus.SourceSystem,
		Payload: payload,
	})
}

// pruneSystemLogs enforces per-level retention so routine events
// (user login, camera status_changed) don't grow system_logs
// unbounded and crowd out critical events in queries.
//
// Retention table (v1.6.37):
//   - critical: unlimited (audit trail — camera/device offline)
//   - normal:   keep newest 500 rows (user login/logout, online)
//   - info:     keep newest 200 rows (camera status_changed)
//
// Runs on every insert. Cheap because:
//   1. Count is on the indexed `level` column.
//   2. Delete only fires when count exceeds the cap (common case
//      is a no-op).
//   3. Uses a subquery to find the cutoff id, so it's a single
//      DELETE statement instead of a row-by-row loop.
//
// Empty/unknown level rows (pre-v1.6.36 backfilled to "normal")
// are treated as normal for pruning.
func (s *Subscriber) pruneSystemLogs() {
	caps := map[string]int64{
		model.LevelNormal: 500,
		model.LevelInfo:   200,
	}
	for level, keep := range caps {
		var count int64
		if err := s.db.Model(&model.SystemLog{}).
			Where("level = ?", level).
			Count(&count).Error; err != nil {
			log.Printf("systemlog: prune count failed level=%s: %v", level, err)
			continue
		}
		if count <= keep {
			continue
		}
		// Delete the oldest (count - keep) rows for this level.
		// "Oldest" = lowest (ts, id) — matches the ORDER BY used by
		// the REST endpoint's "newest first" listing, so the rows
		// dropped are exactly the ones no longer shown.
		excess := count - keep
		res := s.db.Where(
			"id IN (SELECT id FROM system_logs WHERE level = ? ORDER BY ts ASC, id ASC LIMIT ?)",
			level, excess,
		).Delete(&model.SystemLog{})
		if res.Error != nil {
			log.Printf("systemlog: prune delete failed level=%s: %v", level, res.Error)
			continue
		}
		log.Printf("systemlog: pruned %d row(s) level=%s (was %d, cap=%d)",
			res.RowsAffected, level, count, keep)
	}
}

// buildEntry maps an EventBus event to a SystemLog row, including
// the human-readable Chinese message. Returns nil if the event
// payload can't be decoded (the dispatcher will skip silently).
//
// v1.6.36: each branch now assigns a Level (critical / normal / info)
// so the dashboard can surface urgent events (camera offline) above
// routine ones (user login). See model.Level* constants.
func (s *Subscriber) buildEntry(topic string, e eventbus.Event) *model.SystemLog {
	var (
		message string
		ts      int64
		level   = model.LevelNormal // default for safety
	)

	switch topic {
	case eventbus.TopicDeviceStatus:
		var p eventbus.DeviceStatusPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil
		}
		ts = p.TS
		message = fmt.Sprintf("设备 #%d %s", p.DeviceID, translateStatus(p.Status))
		// Device offline is critical (lost connectivity); online is routine.
		if p.Status == "offline" {
			level = model.LevelCritical
		} else {
			level = model.LevelNormal
		}

	case eventbus.TopicCameraOnline,
		eventbus.TopicCameraOffline,
		eventbus.TopicCameraStatusChanged:

		var p eventbus.CameraStatusPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil
		}
		ts = p.TS
		// Camera events only carry camera_id + host. Look up the
		// friendly Name so the dashboard reads "摄像头 前门 上线"
		// instead of "摄像头 192.168.1.10 上线". Fall back to the
		// host (then ID) if the camera row is gone — a deleted
		// camera's offline event still has audit value.
		label := s.cameraLabel(p.CameraID, p.Host)
		message = fmt.Sprintf("摄像头 %s %s", label, translateStatus(p.Status))
		// Camera offline = critical (surveillance gap); online = normal;
		// status_changed (codec/quality) = info.
		switch topic {
		case eventbus.TopicCameraOffline:
			level = model.LevelCritical
		case eventbus.TopicCameraOnline:
			level = model.LevelNormal
		default: // TopicCameraStatusChanged
			level = model.LevelInfo
		}

	case eventbus.TopicUserLogin:
		var p eventbus.UserLoginPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil
		}
		ts = p.Ts
		// The auth handler doesn't include the username in the
		// payload (it has user_id only). Look it up so the log
		// reads "用户 admin 登录"; fall back to the ID.
		username := s.username(p.UserID)
		message = fmt.Sprintf("用户 %s 登录 (设备 %s)", username, deviceLabel(p.DeviceID, p.DeviceName))
		level = model.LevelNormal

	case eventbus.TopicUserLogout:
		var p eventbus.UserLogoutPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil
		}
		ts = p.Ts
		username := s.username(p.UserID)
		message = fmt.Sprintf("用户 %s 登出 (设备 %s)", username, deviceLabel(p.DeviceID, p.DeviceName))
		level = model.LevelNormal

	default:
		return nil
	}

	if ts == 0 {
		ts = e.Timestamp.Unix()
	}

	return &model.SystemLog{
		Ts:        ts,
		EventType: topic,
		Level:     level,
		Source:    e.Source,
		Message:   message,
		Payload:   string(e.Payload),
	}
}

// cameraLabel returns the camera's friendly Name, falling back to
// host then "#<id>" if the row is missing (e.g. a delete raced
// ahead of the offline event).
func (s *Subscriber) cameraLabel(id uint, host string) string {
	var cam model.Camera
	if err := s.db.Select("name").First(&cam, id).Error; err == nil && cam.Name != "" {
		return cam.Name
	}
	if host != "" {
		return host
	}
	return fmt.Sprintf("#%d", id)
}

// username returns the user's Name, falling back to "#<id>".
func (s *Subscriber) username(id uint) string {
	var u model.User
	if err := s.db.Select("name").First(&u, id).Error; err == nil && u.Name != "" {
		return u.Name
	}
	return fmt.Sprintf("#%d", id)
}

// deviceLabel prefers the device_name from the payload (no DB hit)
// and falls back to "#<id>".
func deviceLabel(id uint, name string) string {
	if name != "" {
		return name
	}
	return fmt.Sprintf("#%d", id)
}

// translateStatus maps the wire status string ("online"/"offline")
// to the Chinese verb used in the human-readable message. Unknown
// statuses are returned verbatim so new statuses surface in the UI
// rather than being silently dropped.
func translateStatus(status string) string {
	switch status {
	case "online":
		return "上线"
	case "offline":
		return "离线"
	default:
		return status
	}
}
