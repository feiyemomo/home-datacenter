// Package device provides real-time device lifecycle management:
// online/offline tracking, heartbeat monitoring, and LastSeen updates.
//
// The Manager is the single source of truth for "is device X online
// right now". It is fed by MQTT connection events and periodic
// heartbeats; it persists LastSeen to the database and emits
// device.status events on the EventBus.
package device

import (
	"sync"
	"time"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/repository"
)

// heartbeatTimeout is how long without a heartbeat before a device
// is considered offline.
const heartbeatTimeout = 90 * time.Second

// offlineSweepInterval is how often the background sweeper checks
// for stale devices.
const offlineSweepInterval = 30 * time.Second

// deviceState holds the in-memory realtime state of one device.
type deviceState struct {
	Online     bool
	LastSeen   time.Time
	LastIP     string
}

// Manager tracks device online/offline status in memory and persists
// LastSeen to the database. It is concurrency-safe.
//
// Lifecycle:
//  1. MQTT broker reports a device connected  -> SetOnline
//  2. Device publishes periodic heartbeats    -> Heartbeat
//  3. MQTT broker reports device disconnected -> SetOffline
//  4. Background sweeper catches missed heartbeats -> auto-offline
type Manager struct {
	mu      sync.RWMutex
	devices map[uint]*deviceState
	bus     *eventbus.Bus
	repo    *repository.DeviceRepository
	stop    chan struct{}
}

// NewManager creates a Manager wired to the given EventBus and
// DeviceRepository. Call Start() to launch the background sweeper
// and Stop() to shut it down.
func NewManager(bus *eventbus.Bus, repo *repository.DeviceRepository) *Manager {
	return &Manager{
		devices: make(map[uint]*deviceState),
		bus:     bus,
		repo:    repo,
		stop:    make(chan struct{}),
	}
}

// Start launches the background goroutine that sweeps stale devices
// and marks them offline.
func (m *Manager) Start() {
	go m.sweepLoop()
}

// Stop terminates the background sweeper. It is idempotent.
func (m *Manager) Stop() {
	select {
	case <-m.stop:
		// already closed
	default:
		close(m.stop)
	}
}

// SetOnline marks a device as online and emits a status event.
// Also updates LastSeen in the database.
func (m *Manager) SetOnline(deviceID uint, ip string) {
	m.mu.Lock()
	st, ok := m.devices[deviceID]
	if !ok {
		st = &deviceState{}
		m.devices[deviceID] = st
	}
	st.Online = true
	st.LastSeen = time.Now()
	st.LastIP = ip
	m.mu.Unlock()

	// Persist LastSeen asynchronously so the hot path is not blocked.
	go m.repo.UpdateLastSeen(deviceID, ip)

	m.publishStatus(deviceID, "online")
}

// SetOffline marks a device as offline and emits a status event.
func (m *Manager) SetOffline(deviceID uint) {
	m.mu.Lock()
	if st, ok := m.devices[deviceID]; ok {
		st.Online = false
	}
	m.mu.Unlock()

	m.publishStatus(deviceID, "offline")
}

// Heartbeat refreshes a device's LastSeen timestamp. Called whenever
// a device publishes a heartbeat or any telemetry message.
func (m *Manager) Heartbeat(deviceID uint) {
	m.mu.Lock()
	st, ok := m.devices[deviceID]
	if !ok {
		st = &deviceState{}
		m.devices[deviceID] = st
	}
	wasOffline := !st.Online
	st.LastSeen = time.Now()
	st.Online = true
	m.mu.Unlock()

	// Persist to DB asynchronously.
	go m.repo.UpdateLastSeen(deviceID, st.LastIP)

	// If the device was offline, emit an "online" transition event.
	if wasOffline {
		m.publishStatus(deviceID, "online")
	}
}

// IsOnline reports whether a device is currently online.
func (m *Manager) IsOnline(deviceID uint) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.devices[deviceID]
	if !ok {
		return false
	}
	return st.Online
}

// GetOnlineDevices returns the IDs of all currently online devices.
func (m *Manager) GetOnlineDevices() []uint {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var ids []uint
	for id, st := range m.devices {
		if st.Online {
			ids = append(ids, id)
		}
	}
	return ids
}

// GetLastSeen returns the last-seen timestamp for a device and whether
// the device is known to the manager.
func (m *Manager) GetLastSeen(deviceID uint) (time.Time, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.devices[deviceID]
	if !ok {
		return time.Time{}, false
	}
	return st.LastSeen, true
}

// sweepLoop periodically scans for devices whose LastSeen exceeds
// heartbeatTimeout and marks them offline.
func (m *Manager) sweepLoop() {
	ticker := time.NewTicker(offlineSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.sweep()
		}
	}
}

// sweep is one pass of stale-device detection.
func (m *Manager) sweep() {
	now := time.Now()
	var toOffline []uint

	m.mu.RLock()
	for id, st := range m.devices {
		if st.Online && now.Sub(st.LastSeen) > heartbeatTimeout {
			toOffline = append(toOffline, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range toOffline {
		m.SetOffline(id)
	}
}

// publishStatus emits a device.status event on the EventBus.
func (m *Manager) publishStatus(deviceID uint, status string) {
	// Build payload manually to avoid json import cycle in events.go;
	// the payload struct in events.go is for reference/documentation.
	payload := []byte(`{"device_id":` + itoa(deviceID) +
		`,"status":"` + status +
		`","ts":` + itoa64(time.Now().Unix()) + `}`)

	m.bus.Publish(eventbus.Event{
		Topic:   eventbus.TopicDeviceStatus,
		Payload: payload,
		Source:  eventbus.SourceSystem,
	})
}

// itoa / itoa64 are tiny allocations-free integer-to-string helpers
// to keep this file dependency-light. In Go 1.21+ we could use
// strconv.AppendInt, but the manual version keeps imports minimal.
func itoa(n uint) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
