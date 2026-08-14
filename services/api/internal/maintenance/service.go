package maintenance

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// ServiceProbe is one dependency the API probes for liveness. The
// probe runs from inside the api container on the home-net bridge, so
// it reaches sibling services by name (no host port needed).
type ServiceProbe struct {
	Name string // friendly name for the alert, e.g. "Web 前端"
	// URL for HTTP(S) probes (e.g. "http://web/"). NetworkAddr is
	// used for TCP probes. Exactly one should be set.
	URL         string
	NetworkAddr string // "host:port" for TCP probes
}

// ServiceMonitor probes sibling services and writes a SystemLog alert
// when one becomes unreachable (v1.8.28).
//
// Why this exists: the compose healthchecks make a dead container
// "unhealthy" in `docker ps`, but nothing surfaces that to the
// dashboard. This monitor actively probes the services the API depends
// on (web front-end, mosquitto broker) and writes a critical
// event_type=system.service row + live system.log broadcast when a
// dependency fails, so an operator opening the dashboard sees the
// outage instead of discovering it passively.
//
// Alerts are edge-triggered per service (down → up), with a small
// consecutive-failure threshold so one transient TCP/HTTP blip doesn't
// fire a false alarm.
type ServiceMonitor struct {
	db       *gorm.DB
	bus      *eventbus.Bus
	probes   []ServiceProbe
	interval time.Duration
	// consecutiveFailures tracks how many checks in a row each
	// service has failed (keyed by name). A service is only flagged
	// down after failing `threshold` consecutive probes.
	consecutiveFailures map[string]int
	threshold           int
	// down tracks services currently in the down state so alerts are
	// edge-triggered.
	down map[string]bool
}

// NewServiceMonitor creates a monitor that probes the given services.
func NewServiceMonitor(db *gorm.DB, bus *eventbus.Bus, probes []ServiceProbe, interval time.Duration, threshold int) *ServiceMonitor {
	if threshold <= 0 {
		threshold = 3
	}
	return &ServiceMonitor{
		db:                  db,
		bus:                 bus,
		probes:              probes,
		interval:            interval,
		consecutiveFailures: make(map[string]int),
		threshold:           threshold,
		down:                make(map[string]bool),
	}
}

// Run probes once immediately, then on every tick forever.
func (m *ServiceMonitor) Run() {
	m.sample()
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for range ticker.C {
		m.sample()
	}
}

// sample probes every configured service and emits alerts on state
// transitions.
func (m *ServiceMonitor) sample() {
	for _, p := range m.probes {
		ok := m.probe(p)
		if ok {
			m.consecutiveFailures[p.Name] = 0
			m.recover(p)
		} else {
			m.consecutiveFailures[p.Name]++
			if m.consecutiveFailures[p.Name] >= m.threshold {
				m.fail(p)
			}
		}
	}
}

// probe performs the actual reachability check for a service.
func (m *ServiceMonitor) probe(p ServiceProbe) bool {
	if p.URL != "" {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(p.URL)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		// Any HTTP response (even 4xx/5xx) means the server is up;
		// only a transport error means it's down.
		return true
	}
	if p.NetworkAddr != "" {
		conn, err := net.DialTimeout("tcp", p.NetworkAddr, 3*time.Second)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}
	return true
}

// fail emits a down alert on the first consecutive-failure crossing.
func (m *ServiceMonitor) fail(p ServiceProbe) {
	if m.down[p.Name] {
		return // already down
	}
	m.down[p.Name] = true
	m.emit(p, true)
}

// recover clears a down alert when the service comes back.
func (m *ServiceMonitor) recover(p ServiceProbe) {
	if !m.down[p.Name] {
		return
	}
	m.down[p.Name] = false
	m.emit(p, false)
}

// emit writes a SystemLog row (and live broadcast) for a state change.
func (m *ServiceMonitor) emit(p ServiceProbe, isDown bool) {
	var msg string
	sev := model.LevelNormal
	busSev := eventbus.SeverityInfo
	if isDown {
		msg = fmt.Sprintf("服务 %s 不可达", p.Name)
		sev = model.LevelCritical
		busSev = eventbus.SeverityCritical
	} else {
		msg = fmt.Sprintf("服务 %s 已恢复", p.Name)
	}
	payload, _ := json.Marshal(map[string]any{
		"service": p.Name,
		"url":     p.URL,
		"addr":    p.NetworkAddr,
		"down":    isDown,
		"ts":      time.Now().Unix(),
	})
	entry := &model.SystemLog{
		Ts:        time.Now().Unix(),
		EventType: "system.service",
		Level:     sev,
		Source:    "system",
		Message:   msg,
		Payload:   string(payload),
	}
	if err := m.db.Create(entry).Error; err != nil {
		log.Printf("maintenance: service alert persist failed: %v", err)
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
