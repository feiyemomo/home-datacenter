// Package camera — Frigate event translation layer.
//
// The FrigateEventTranslator subscribes to "frigate.event" on the
// EventBus (raw payload from MQTT), looks up the home-datacenter
// camera by frigate_camera name, and re-publishes a platform-native
// "camera.object.detected" event that the rest of the system can
// consume without knowing anything about Frigate internals.
package camera

import (
	"encoding/json"
	"log"
	"time"

	"home-datacenter-api/internal/eventbus"
)

// frigateRawEvent is the JSON shape Frigate publishes to
// frigate/events for new/update detection events.
type frigateRawEvent struct {
	Type  string           `json:"type"`  // "new" | "update" | "end"
	After frigateEventData `json:"after"` // detection payload
}

type frigateEventData struct {
	ID           string   `json:"id"`
	Camera       string   `json:"camera"`     // Frigate camera name (e.g. "front_door")
	FrameTime    float64  `json:"frame_time"` // Unix timestamp
	Label        string   `json:"label"`      // "person", "car", "dog", ...
	TopScore     float64  `json:"top_score"`  // 0.0–1.0 confidence
	HasClip      bool     `json:"has_clip"`
	HasSnapshot  bool     `json:"has_snapshot"`
	CurrentZones []string `json:"current_zones"`
	Stationary   bool     `json:"stationary"`
}

// frigateTranslatedEvent is the camera.object.detected payload that
// the rest of the platform sees. It augments the Frigate data with
// home-datacenter camera metadata (name, id).
type frigateTranslatedEvent struct {
	CameraID      int     `json:"camera_id"`
	CameraName    string  `json:"camera_name"`
	FrigateCamera string  `json:"frigate_camera"`
	Object        string  `json:"object"`
	Confidence    float64 `json:"confidence"`
	// FrigateEventID is the raw Frigate event ID, used to construct
	// snapshot/video URLs on the frontend.
	FrigateEventID string   `json:"frigate_event_id"`
	HasClip        bool     `json:"has_clip"`
	HasSnapshot    bool     `json:"has_snapshot"`
	Zones          []string `json:"zones,omitempty"`
	Stationary     bool     `json:"stationary"`
}

// FrigateEventTranslator converts Frigate MQTT events into
// platform-native camera events. It is wired in main.go.
type FrigateEventTranslator struct {
	bus      *eventbus.Bus
	registry *Registry
}

// NewFrigateEventTranslator creates a translator. Call Start() to
// begin listening for frigate.event on the EventBus.
func NewFrigateEventTranslator(bus *eventbus.Bus, registry *Registry) *FrigateEventTranslator {
	return &FrigateEventTranslator{bus: bus, registry: registry}
}

// Start subscribes to "frigate.event" on the EventBus and begins
// translating raw Frigate payloads into camera.object.detected events.
func (t *FrigateEventTranslator) Start() {
	t.bus.Subscribe("frigate.event", func(ev eventbus.Event) {
		t.translate(ev)
	})
}

// translate parses a raw Frigate event, looks up the camera, and
// re-publishes as camera.object.detected.
func (t *FrigateEventTranslator) translate(ev eventbus.Event) {
	var raw frigateRawEvent
	if err := json.Unmarshal(ev.Payload, &raw); err != nil {
		log.Printf("frigate_event: parse failed: %v", err)
		return
	}

	// Skip stationary objects in the persistence layer (they
	// generate an "update" for every frame). The real-time
	// WebSocket path still receives them via frigate.event.
	if raw.Type == "update" && raw.After.Stationary {
		return
	}

	// Look up the home-datacenter camera by Frigate name.
	frigateName := raw.After.Camera
	if frigateName == "" {
		return
	}
	cam, err := t.registry.FindByFrigateCamera(frigateName)
	if err != nil {
		log.Printf("frigate_event: camera %q not found: %v", frigateName, err)
		return
	}

	translated := frigateTranslatedEvent{
		CameraID:       int(cam.ID),
		CameraName:     cam.Name,
		FrigateCamera:  frigateName,
		Object:         raw.After.Label,
		Confidence:     raw.After.TopScore,
		FrigateEventID: raw.After.ID,
		HasClip:        raw.After.HasClip,
		HasSnapshot:    raw.After.HasSnapshot,
		Zones:          raw.After.CurrentZones,
		Stationary:     raw.After.Stationary,
	}

	payload, _ := json.Marshal(translated)

	// Publish the platform-native event.
	// The EventPersister persists this (it subscribes to *).
	// The WebSocket hub forwards it to connected clients.
	t.bus.Publish(eventbus.Event{
		ID:        ev.ID,
		Topic:     "camera.object.detected",
		Source:    "camera",
		Severity:  "info",
		Timestamp: time.Unix(int64(raw.After.FrameTime), 0),
		Payload:   payload,
	})

	log.Printf("frigate_event: %s → camera.object.detected (cam=%s, obj=%s, conf=%.0f%%)",
		frigateName, cam.Name, raw.After.Label, raw.After.TopScore*100)
}
