package network

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"

	"home-datacenter-api/internal/camera"
	"home-datacenter-api/internal/eventbus"
)

// PrefixWatcher periodically probes the outbound IPv6 address and
// detects ISP prefix rotations (DHCPv6-PD). When a rotation is
// detected, it:
//  1. Updates the in-memory cached outbound address.
//  2. Publishes a "network.ipv6.prefix_rotated" event to the EventBus.
//  3. Attempts to push the new webrtc.candidates to go2rtc via the
//     Frigate config API (best-effort; failure is logged but doesn't
//     block subsequent checks).
//
// The watcher is the long-term fix for the v1.6.x "IPv6 direct latency
// jumps to ~1000ms" issue: instead of requiring manual config updates
// when the ISP rotates the IPv6 prefix, the watcher auto-detects the
// rotation and updates both the API endpoint (which the Android client
// polls) and the go2rtc candidates (which WebRTC uses).
type PrefixWatcher struct {
	mu             sync.RWMutex
	lastOutbound   string
	lastChecked    time.Time
	bus            *eventbus.Bus
	frigate        *camera.FrigateClient
	configuredAddr string // from NAS_IPV6_ADDRESS env var (may be empty)
	stopCh         chan struct{}
	interval       time.Duration // default 5 minutes
}

// NewPrefixWatcher creates a watcher that probes every 5 minutes.
// The configuredAddr is read from NAS_IPV6_ADDRESS at construction
// time so repeated probes don't re-parse the env var.
func NewPrefixWatcher(bus *eventbus.Bus, frigate *camera.FrigateClient) *PrefixWatcher {
	return &PrefixWatcher{
		bus:            bus,
		frigate:        frigate,
		configuredAddr: os.Getenv("NAS_IPV6_ADDRESS"),
		interval:       5 * time.Minute,
		stopCh:         make(chan struct{}),
	}
}

// Start launches the background goroutine. Non-blocking.
func (w *PrefixWatcher) Start() {
	go w.loop()
}

// Stop signals the background goroutine to exit. Blocks until done.
func (w *PrefixWatcher) Stop() {
	close(w.stopCh)
}

// Status returns the current cached outbound IPv6 status. Thread-safe.
// If the watcher hasn't run yet, triggers an immediate check.
func (w *PrefixWatcher) Status() OutboundIPv6Status {
	w.mu.RLock()
	if !w.lastChecked.IsZero() {
		s := OutboundIPv6Status{
			OutboundAddress:   w.lastOutbound,
			ConfiguredAddress: w.configuredAddr,
			PrefixRotated:     !IPv6PrefixMatches(w.lastOutbound, w.configuredAddr),
			LastChecked:       w.lastChecked,
		}
		w.mu.RUnlock()
		return s
	}
	w.mu.RUnlock()
	// First call — do a synchronous check.
	w.checkOnce()
	w.mu.RLock()
	defer w.mu.RUnlock()
	return OutboundIPv6Status{
		OutboundAddress:   w.lastOutbound,
		ConfiguredAddress: w.configuredAddr,
		PrefixRotated:     !IPv6PrefixMatches(w.lastOutbound, w.configuredAddr),
		LastChecked:       w.lastChecked,
	}
}

// Refresh forces an immediate probe and returns the fresh status.
func (w *PrefixWatcher) Refresh() OutboundIPv6Status {
	w.checkOnce()
	return w.Status()
}

func (w *PrefixWatcher) loop() {
	// Run an immediate check at startup so the endpoint has data right away.
	w.checkOnce()
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.checkOnce()
		case <-w.stopCh:
			return
		}
	}
}

func (w *PrefixWatcher) checkOnce() {
	outbound := OutboundIPv6Address()
	w.mu.Lock()
	prev := w.lastOutbound
	w.lastOutbound = outbound
	w.lastChecked = time.Now()
	rotated := prev != "" && outbound != "" && !IPv6PrefixMatches(prev, outbound)
	w.mu.Unlock()

	if rotated {
		log.Printf("PrefixWatcher: IPv6 prefix rotation detected: %s → %s", prev, outbound)
		// Publish event
		if w.bus != nil {
			payload, _ := json.Marshal(map[string]string{
				"previous_address": prev,
				"new_address":      outbound,
			})
			w.bus.Publish(eventbus.Event{
				Topic:    "network.ipv6.prefix_rotated",
				Source:   eventbus.SourceSystem,
				Severity: eventbus.SeverityWarn,
				Payload:  payload,
			})
		}
		// Best-effort: push new webrtc.candidates to Frigate
		if w.frigate != nil {
			if err := w.frigate.SetWebRTCCandidates(context.Background(), outbound); err != nil {
				log.Printf("PrefixWatcher: failed to push new WebRTC candidates: %v", err)
			}
		}
	} else if outbound == "" {
		log.Printf("PrefixWatcher: outbound IPv6 probe returned empty (configured=%s)", w.configuredAddr)
	}
}
