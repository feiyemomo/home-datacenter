package camera

import (
	"context"
	"net"
	"strconv"
	"time"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// HealthChecker probes every registered camera on a fixed interval,
// updates its Status/LastSeenAt, and emits a canonical device.status
// event on the EventBus so the Dashboard/WS layer can react.
//
// We start with a cheap TCP-dial against the RTSP port. This catches
// "camera is powered off / unplugged / IP changed" within one tick
// (default 15s) without a single byte of video. A future iteration
// can layer an ONVIF GetSystemDateAndTime probe on top of this for
// "camera up but RTSP auth broken" detection.
type HealthChecker struct {
	Registry *Registry
	Bus      *eventbus.Bus
	Interval time.Duration
	Timeout  time.Duration
}

// Run blocks until ctx is cancelled. Pass the API's root context.
func (h *HealthChecker) Run(ctx context.Context) {
	if h.Interval == 0 {
		h.Interval = 15 * time.Second
	}
	if h.Timeout == 0 {
		h.Timeout = 3 * time.Second
	}
	t := time.NewTicker(h.Interval)
	defer t.Stop()

	// Run once immediately so the dashboard doesn't wait a full
	// interval for the first status update.
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

	if h.Bus != nil {
		h.Bus.Publish(eventbus.Event{
			Topic:  "device.status",
			Source: eventbus.SourceSystem,
			Payload: mustJSON(map[string]any{
				"device_id": c.ID,
				"type":      "camera",
				"status":    status,
				"ts":        now.Unix(),
			}),
		})
	}
}
