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
	"strings"
	"sync"

	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// Subscriber bridges EventBus events into the SystemLog table.
//
// On Start it subscribes to a fixed set of topics: camera
// online/offline/status_changed, user login/logout, and user/camera
// management events. Each event is decoded, turned into a Chinese
// human-readable message, persisted as a SystemLog row, and
// re-published on the "system.log" topic so the WS Hub can fan it
// out to dashboards.
type Subscriber struct {
	db  *gorm.DB
	bus *eventbus.Bus

	mu     sync.Mutex
	unsubs []func()
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
		// v1.6.41: TopicDeviceStatus removed — it duplicated the
		// camera online/offline logs ("设备 #N 上线" alongside
		// "摄像头 X 上线"). Camera-specific topics below carry the
		// friendly camera name and are sufficient for auditing.
		eventbus.TopicCameraOnline,
		eventbus.TopicCameraOffline,
		eventbus.TopicCameraStatusChanged,
		// v1.8.20: user login/logout re-added — users requested
		// these events be visible in the audit log again.
		eventbus.TopicUserLogin,
		eventbus.TopicUserLogout,
		// v1.8.20: user/camera management events for audit trail.
		eventbus.TopicUserCreate,
		eventbus.TopicUserUpdate,
		eventbus.TopicUserDelete,
		eventbus.TopicCameraDelete,
		// v1.8.22: camera create/update + automation lifecycle +
		// device hard-delete/token-rotate + motion for audit trail.
		eventbus.TopicCameraCreate,
		eventbus.TopicCameraUpdate,
		eventbus.TopicCameraMotion,
		eventbus.TopicCameraPersonRecognized,
		eventbus.TopicCameraFallDetected,
		eventbus.TopicAutomationFired,
		eventbus.TopicAutomationCreate,
		eventbus.TopicAutomationUpdate,
		eventbus.TopicAutomationDelete,
		eventbus.TopicDeviceHardDelete,
		eventbus.TopicDeviceTokenRotate,
	}
	for _, t := range topics {
		// Capture the topic in a local variable so the closure
		// sees the current value, not the last loop iteration's.
		topic := t
		unsub := s.bus.Subscribe(topic, func(e eventbus.Event) {
			s.handle(topic, e)
		})
		s.mu.Lock()
		s.unsubs = append(s.unsubs, unsub)
		s.mu.Unlock()
	}
}

// Stop unsubscribes every topic this Subscriber registered. Safe to
// call once during graceful shutdown; idempotent.
func (s *Subscriber) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, unsub := range s.unsubs {
		if unsub != nil {
			unsub()
			s.unsubs[i] = nil
		}
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
// (device online, camera status_changed) don't grow system_logs
// unbounded and crowd out critical events in queries.
//
// Retention table (v1.6.37):
//   - critical: unlimited (audit trail — camera/device offline)
//   - normal:   keep newest 500 rows (device online)
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
		model.LevelNormal:  500,
		model.LevelInfo:    200,
		model.LevelWarning: 200,
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
// routine ones. See model.Level* constants.
//
// v1.8.20: user login/logout and user/camera management events
// re-added/added for audit trail.
func (s *Subscriber) buildEntry(topic string, e eventbus.Event) *model.SystemLog {
	var (
		message string
		ts      int64
		level   = model.LevelNormal // default for safety
	)

	switch topic {
	// v1.6.41: TopicDeviceStatus case removed — no longer subscribed.
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
		userLabel := s.userLabel(p.UserID)
		deviceLabel := p.DeviceName
		if deviceLabel == "" {
			deviceLabel = fmt.Sprintf("#%d", p.DeviceID)
		}
		message = fmt.Sprintf("用户 %s 登录（设备 %s）", userLabel, deviceLabel)
		level = model.LevelNormal

	case eventbus.TopicUserLogout:
		var p eventbus.UserLogoutPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil
		}
		ts = p.Ts
		userLabel := s.userLabel(p.UserID)
		deviceLabel := p.DeviceName
		if deviceLabel == "" {
			deviceLabel = fmt.Sprintf("#%d", p.DeviceID)
		}
		message = fmt.Sprintf("用户 %s 登出（设备 %s 已撤销）", userLabel, deviceLabel)
		level = model.LevelNormal

	case eventbus.TopicUserCreate,
		eventbus.TopicUserUpdate,
		eventbus.TopicUserDelete:
		var p eventbus.UserManagePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil
		}
		ts = p.Ts
		// Admin name: query from DB (admin row still exists).
		adminLabel := s.userLabel(p.AdminID)
		// Target name: prefer payload (snapshotted before delete),
		// fallback to DB query (for create/update the row exists).
		targetLabel := p.TargetName
		if targetLabel == "" {
			targetLabel = s.userLabel(p.TargetID)
		}
		actionVerb := map[string]string{
			"create": "创建用户",
			"update": "更新用户",
			"delete": "删除用户",
		}[p.Action]
		if actionVerb == "" {
			actionVerb = "管理用户"
		}
		message = fmt.Sprintf("管理员 %s %s %s", adminLabel, actionVerb, targetLabel)
		level = model.LevelNormal

	case eventbus.TopicCameraDelete:
		var p eventbus.CameraDeletePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil
		}
		ts = p.Ts
		// Admin name: prefer payload (when the handler snapshotted
		// it), fall back to a DB lookup, then "#<id>".
		adminLabel := p.AdminName
		if adminLabel == "" {
			adminLabel = s.userLabel(p.AdminID)
		}
		cameraLabel := p.CameraName
		if cameraLabel == "" {
			cameraLabel = fmt.Sprintf("#%d", p.CameraID)
		}
		message = fmt.Sprintf("管理员 %s 删除摄像头 %s", adminLabel, cameraLabel)
		level = model.LevelNormal

	case eventbus.TopicCameraCreate:
		var p eventbus.CameraManagePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil
		}
		ts = p.Ts
		adminLabel := s.userLabel(p.AdminID)
		message = fmt.Sprintf("管理员 %s 注册摄像头 %s", adminLabel, p.CameraName)
		level = model.LevelNormal

	case eventbus.TopicCameraUpdate:
		var p eventbus.CameraManagePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil
		}
		ts = p.Ts
		adminLabel := s.userLabel(p.AdminID)
		detail := p.Detail
		if detail == "" {
			detail = "配置"
		}
		message = fmt.Sprintf("管理员 %s 更新摄像头 %s 的 %s", adminLabel, p.CameraName, detail)
		level = model.LevelNormal

	case eventbus.TopicAutomationFired:
		// automation.fired payload is a map[string]any, not a struct.
		var p map[string]any
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil
		}
		if v, ok := p["ts"].(float64); ok {
			ts = int64(v)
		}
		ruleName, _ := p["rule_name"].(string)
		action, _ := p["action"].(string)
		ok, _ := p["ok"].(bool)
		status := "成功"
		if !ok {
			status = "失败"
		}
		message = fmt.Sprintf("自动化规则 %s 触发，执行 %s 动作（%s）", ruleName, action, status)
		level = model.LevelInfo

	case eventbus.TopicAutomationCreate:
		var p eventbus.AutomationManagePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil
		}
		ts = p.Ts
		adminLabel := s.userLabel(p.AdminID)
		message = fmt.Sprintf("管理员 %s 创建自动化规则 %s", adminLabel, p.RuleName)
		level = model.LevelNormal

	case eventbus.TopicAutomationUpdate:
		var p eventbus.AutomationManagePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil
		}
		ts = p.Ts
		adminLabel := s.userLabel(p.AdminID)
		message = fmt.Sprintf("管理员 %s 更新自动化规则 %s", adminLabel, p.RuleName)
		level = model.LevelNormal

	case eventbus.TopicAutomationDelete:
		var p eventbus.AutomationManagePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil
		}
		ts = p.Ts
		adminLabel := s.userLabel(p.AdminID)
		message = fmt.Sprintf("管理员 %s 删除自动化规则 %s", adminLabel, p.RuleName)
		level = model.LevelNormal

	case eventbus.TopicDeviceHardDelete:
		var p eventbus.DeviceManagePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil
		}
		ts = p.Ts
		adminLabel := s.userLabel(p.AdminID)
		deviceLabel := p.DeviceName
		if deviceLabel == "" {
			deviceLabel = fmt.Sprintf("#%d", p.DeviceID)
		}
		message = fmt.Sprintf("管理员 %s 永久删除设备 %s", adminLabel, deviceLabel)
		level = model.LevelNormal

	case eventbus.TopicDeviceTokenRotate:
		var p eventbus.DeviceManagePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil
		}
		ts = p.Ts
		adminLabel := s.userLabel(p.AdminID)
		deviceLabel := p.DeviceName
		if deviceLabel == "" {
			deviceLabel = fmt.Sprintf("#%d", p.DeviceID)
		}
		message = fmt.Sprintf("管理员 %s 轮换设备 %s 的访问令牌", adminLabel, deviceLabel)
		level = model.LevelNormal

	case eventbus.TopicCameraMotion:
		// camera.motion payload (see mqtt/handler.go handleFrigateEvent):
		// {event_id, camera_id, type, label, confidence, zones,
		//  has_snapshot, has_clip, ts}. No camera_name field — look
		// up via cameraLabel (DB), then fall back to "#<id>".
		var p map[string]any
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil
		}
		if v, ok := p["ts"].(float64); ok {
			ts = int64(v)
		}
		var cameraName string
		if id, ok := p["camera_id"].(float64); ok {
			cameraName = s.cameraLabel(uint(id), "")
		}
		if cameraName == "" {
			if name, ok := p["camera_name"].(string); ok {
				cameraName = name
			}
		}
		if cameraName == "" {
			if id, ok := p["camera_id"].(float64); ok {
				cameraName = fmt.Sprintf("#%d", int64(id))
			}
		}
		message = fmt.Sprintf("摄像头 %s 检测到运动", cameraName)
		level = model.LevelInfo

	case eventbus.TopicCameraPersonRecognized:
		var p struct {
			CameraID uint     `json:"camera_id"`
			Persons  []string `json:"persons"`
			TS       int64    `json:"ts"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil
		}
		if p.TS > 0 {
			ts = p.TS
		}
		cameraName := s.cameraLabel(p.CameraID, "")
		if cameraName == "" {
			cameraName = fmt.Sprintf("#%d", p.CameraID)
		}
		message = fmt.Sprintf("摄像头 %s 识别到人物: %s", cameraName, strings.Join(p.Persons, ", "))
		level = model.LevelInfo

	case eventbus.TopicCameraFallDetected:
		var p struct {
			CameraID uint  `json:"camera_id"`
			TS       int64 `json:"ts"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil
		}
		if p.TS > 0 {
			ts = p.TS
		}
		cameraName := s.cameraLabel(p.CameraID, "")
		if cameraName == "" {
			cameraName = fmt.Sprintf("#%d", p.CameraID)
		}
		message = fmt.Sprintf("摄像头 %s 警报: 监测到疑似跌倒事件！", cameraName)
		level = model.LevelCritical

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

// userLabel returns the user's friendly Name, falling back to
// "#<id>" if the row is missing (e.g. deleted user).
func (s *Subscriber) userLabel(id uint) string {
	var user model.User
	if err := s.db.Select("name").First(&user, id).Error; err == nil && user.Name != "" {
		return user.Name
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
