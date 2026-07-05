package camera

import (
	"context"
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

	mu         sync.RWMutex
	prevStatus map[uint]string // camera ID -> last known status
}

// Run blocks until ctx is cancelled. Pass the API's root context.
func (h *HealthChecker) Run(ctx context.Context) {
	if h.Interval == 0 {
		h.Interval = 15 * time.Second
	}
	if h.Timeout == 0 {
		h.Timeout = 3 * time.Second
	}
	if h.prevStatus == nil {
		h.prevStatus = make(map[uint]string)
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
		status = "offline"
	} else {
		_ = conn.Close()
	}
	h.Registry.UpdateStatus(c.ID, status, &now)

	// Check for status transition.
	prev := h.getPrevStatus(c.ID)
	transitioned := prev != "" && prev != status
	h.setPrevStatus(c.ID, status)

	if h.Bus != nil {
		ts := now.Unix()

		// Always emit device.status for backward compatibility.
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
