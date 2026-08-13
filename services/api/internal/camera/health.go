package camera

import (
	"context"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// HealthChecker probes every registered camera on a fixed interval,
// updates its Status/LastSeenAt, and emits events on the EventBus:
//
//   - camera.online  (on offline→online transition)
//   - camera.offline (on online→offline transition)
//   - device.status  (always, for backward compatibility)
//
// We start with a cheap TCP-dial against the RTSP port. This catches
// "camera is powered off / unplugged / IP changed" within one tick
// (default 15s) without a single byte of video.
type HealthChecker struct {
	Registry *Registry
	Bus      *eventbus.Bus
	Interval time.Duration
	Timeout  time.Duration
	// FailThreshold is the number of consecutive failed probes
	// before a camera is marked offline. Default 3. This prevents
	// transient network blips (WiFi interference, AC controller
	// hiccups, ARP cache misses) from causing false offline→online
	// flapping that triggers unnecessary Frigate config pushes.
	FailThreshold int

	mu         sync.RWMutex
	prevStatus map[uint]string // camera ID -> last known status
	failCount  map[uint]int    // camera ID -> consecutive failure count
}

// Run blocks until ctx is cancelled. Pass the API's root context.
func (h *HealthChecker) Run(ctx context.Context) {
	if h.Interval == 0 {
		h.Interval = 15 * time.Second
	}
	if h.Timeout == 0 {
		h.Timeout = 3 * time.Second
	}
	if h.FailThreshold == 0 {
		h.FailThreshold = 3
	}
	if h.prevStatus == nil {
		h.prevStatus = make(map[uint]string)
	}
	if h.failCount == nil {
		h.failCount = make(map[uint]int)
	}
	t := time.NewTicker(h.Interval)
	defer t.Stop()

	h.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.tick(ctx)
		}
	}
}

func (h *HealthChecker) tick(ctx context.Context) {
	for _, cam := range h.Registry.List() {
		go h.probe(ctx, cam)
	}
}

func (h *HealthChecker) probe(ctx context.Context, c model.Camera) {
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.RTSPPort))
	d := net.Dialer{Timeout: h.Timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	status := "online"
	now := time.Now()
	if err != nil {
		conn = nil
		// Debounce: require FailThreshold consecutive failures
		// before marking offline. This prevents transient network
		// blips (WiFi interference, AC controller hiccups, ARP
		// cache misses) from causing false offline→online flapping
		// that triggers unnecessary Frigate config pushes.
		fails := h.incFailCount(c.ID)
		if fails < h.FailThreshold {
			return // skip this tick, camera still considered online
		}
		status = "offline"
	} else {
		_ = conn.Close()
		h.resetFailCount(c.ID)
	}
	h.Registry.UpdateStatus(c.ID, status, &now)

	// Check for status transition.
	prev := h.getPrevStatus(c.ID)
	transitioned := prev != "" && prev != status
	h.setPrevStatus(c.ID, status)

	if h.Bus != nil {
		ts := now.Unix()

		// v1.6.39: only emit device.status on state transitions.
		// Previously this fired every 15s tick unconditionally,
		// flooding system_logs with repetitive "设备 #N 上线/离线"
		// entries. The WS Hub and device manager already receive
		// status updates via the camera-specific topics below.
		if transitioned || prev == "" {
			h.Bus.Publish(eventbus.Event{
				Topic:    eventbus.TopicDeviceStatus,
				Source:   eventbus.SourceCamera,
				Severity: eventbus.SeverityInfo,
				Payload: mustJSON(map[string]any{
					"device_id": c.ID,
					"type":      "camera",
					"status":    status,
					"ts":        ts,
				}),
			})
		}

		// Emit camera-specific events on transitions.
		if transitioned {
			topic := eventbus.TopicCameraOnline
			severity := eventbus.SeverityInfo
			if status == "offline" {
				topic = eventbus.TopicCameraOffline
				severity = eventbus.SeverityWarn
			}
			h.Bus.Publish(eventbus.Event{
				Topic:    topic,
				Source:   eventbus.SourceCamera,
				Severity: severity,
				Payload: mustJSON(map[string]any{
					"camera_id": c.ID,
					"status":    status,
					"host":      c.Host,
					"ts":        ts,
				}),
			})

			// v1.8.22: also publish a generic status_changed event
			// so the audit log captures every state transition at
			// info level (online/offline above carry normal/critical
			// levels). Mirrors the CameraStatusPayload shape the
			// subscriber decodes for camera.* topics.
			h.Bus.Publish(eventbus.Event{
				Topic:    eventbus.TopicCameraStatusChanged,
				Source:   eventbus.SourceCamera,
				Severity: eventbus.SeverityInfo,
				Payload: mustJSON(eventbus.CameraStatusPayload{
					CameraID: c.ID,
					Status:   status,
					Host:     c.Host,
					TS:       ts,
				}),
			})

			// Re-push Frigate config on online<->offline transitions
			// so that offline cameras are disabled (Enabled: false) in
			// Frigate, stopping endless ffmpeg reconnect attempts, and
			// recovered cameras are re-enabled. Runs asynchronously to
			// avoid blocking the health-check loop.
			if h.Registry.Frigate != nil {
				go func() {
					pushCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					if err := h.Registry.pushFrigateConfig(pushCtx); err != nil {
						log.Printf("health: frigate config push on %s transition: %v", status, err)
					}
				}()
			}
		}
	}
}

func (h *HealthChecker) getPrevStatus(id uint) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.prevStatus[id]
}

func (h *HealthChecker) setPrevStatus(id uint, status string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prevStatus[id] = status
}

func (h *HealthChecker) incFailCount(id uint) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failCount[id]++
	return h.failCount[id]
}

func (h *HealthChecker) resetFailCount(id uint) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.failCount, id)
}
