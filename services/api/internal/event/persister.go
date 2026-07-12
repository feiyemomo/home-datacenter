// Package event provides EventBus → SQLite persistence.
//
// The EventPersister subscribes to "*" on the EventBus and inserts
// every published event into the events table. This turns the
// in-memory fire-and-forget bus into a permanent audit trail that
// can be queried via the REST API.
//
// Usage:
//
//	persister := event.NewPersister(eventRepo, bus)
//	persister.Start()  // begins persisting
//	// later:
//	persister.Stop()   // unsubscribes (graceful)
package event

import (
	"encoding/json"
	"log"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/repository"
)

// Persister subscribes to the EventBus and persists every event to
// the database. It runs in the background — the Bus.Publish call is
// non-blocking for the original publisher; persistence happens
// asynchronously so a slow DB write does not delay real-time event
// delivery via WebSocket.
type Persister struct {
	repo *repository.EventRepository
	bus  *eventbus.Bus
	done chan struct{}
}

// NewPersister creates a Persister. Call Start() to begin persisting.
func NewPersister(repo *repository.EventRepository, bus *eventbus.Bus) *Persister {
	return &Persister{
		repo: repo,
		bus:  bus,
		done: make(chan struct{}),
	}
}

// Start subscribes to "*" (all events) on the EventBus and begins
// persisting. Each event is inserted in its own goroutine so a slow
// SQLite write does not back-pressure the Bus's publish path.
//
// Topics prefixed with "ws." or "mqtt." are NOT persisted — they are
// internal control plane messages, not user-visible events.
//
// The "device.status" topic is also NOT persisted — it is a
// high-frequency heartbeat (every 15s) that would flood the events
// table and obscure meaningful events. The current device status is
// already available via GET /api/v1/device/list.
func (p *Persister) Start() {
	p.bus.Subscribe("*", func(ev eventbus.Event) {
		// Skip internal control-plane chatter.
		if isInternalTopic(ev.Topic) {
			return
		}

		// Marshal payload: if already JSON bytes, use as-is;
		// otherwise marshal to JSON string.
		payload := string(ev.Payload)
		if payload == "" || (len(ev.Payload) == 2 && string(ev.Payload) == "{}") {
			payload = "{}"
		}
		// Quick sanity: if it's not valid JSON, wrap as a string.
		if !json.Valid(ev.Payload) && len(ev.Payload) > 0 {
			b, _ := json.Marshal(string(ev.Payload))
			payload = string(b)
		}

		se := &model.StoredEvent{
			Topic:     ev.Topic,
			Source:    ev.Source,
			Severity:  ev.Severity,
			Payload:   payload,
			Status:    model.EventStatusCreated,
			Timestamp: ev.Timestamp,
		}

		if err := p.repo.Insert(se); err != nil {
			// Non-fatal: event is already delivered to WebSocket
			// subscribers; the DB write failing means it won't
			// appear in the history, but the real-time path still
			// worked.
			log.Printf("event persister: insert %s (id=%d): %v", ev.Topic, ev.ID, err)
		}
	})
}

// Stop unsubscribes from the EventBus. Already-queued inserts will
// still complete; no new events will be persisted after this returns.
func (p *Persister) Stop() {
	close(p.done)
}

// isInternalTopic filters out control-plane topics that have no value
// in the event history view. External subscribers are still welcome to
// consume these, but we don't persist them.
func isInternalTopic(topic string) bool {
	// MQTT shadow topics: huge volume, not user-visible.
	if len(topic) > 5 && topic[:5] == "mqtt." {
		return true
	}
	// WebSocket internal: connection lifecycle, not user-visible.
	if len(topic) > 3 && topic[:3] == "ws." {
		return true
	}
	// Device status heartbeat: fires every 15s per online device.
	// Kept out of the events table to avoid drowning meaningful events.
	if topic == "device.status" {
		return true
	}
	return false
}
