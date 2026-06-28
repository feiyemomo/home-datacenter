// Package eventbus provides an in-memory publish/subscribe event bus
// that decouples MQTT message handling from WebSocket distribution.
//
// The bus is the single bridge between the MQTT side (devices pushing
// telemetry/status) and the WebSocket side (apps consuming live data).
// Neither side knows about the other — they only share event types.
package eventbus

import (
	"sync"
)

// Event is the unit of communication on the bus.
//
//   - Topic:   logical channel name (e.g. "device.1.telemetry")
//   - Payload: opaque bytes; subscribers decide how to decode
//   - Source:  "mqtt" | "ws" | "system" for debugging/routing
type Event struct {
	Topic   string
	Payload []byte
	Source  string
}

// handler wraps a subscriber callback together with an identifier so
// it can be unsubscribed later.
type handler struct {
	id       uint64
	callback func(Event)
}

// Bus is a thread-safe in-memory pub/sub bus.
//
// Subscriptions match by topic prefix, allowing wildcards naturally:
// subscribing to "device.1" receives "device.1.telemetry",
// "device.1.status", etc.
type Bus struct {
	mu        sync.RWMutex
	nextID    uint64
	subs      map[string][]handler
	closeChan chan struct{}
	closed    bool
}

// New creates a ready-to-use Bus.
func New() *Bus {
	return &Bus{
		subs:      make(map[string][]handler),
		closeChan: make(chan struct{}),
	}
}

// Subscribe registers a callback for any event whose topic starts with
// prefix. Returns an unsubscribe function.
//
// Example:
//
//	unsub := bus.Subscribe("device.1", func(e Event) { ... })
//	defer unsub()
func (b *Bus) Subscribe(prefix string, cb func(Event)) func() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.nextID++
	h := handler{id: b.nextID, callback: cb}
	b.subs[prefix] = append(b.subs[prefix], h)

	id := h.id
	p := prefix
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		subs := b.subs[p]
		for i, s := range subs {
			if s.id == id {
				b.subs[p] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
	}
}

// Publish broadcasts an event to all subscribers whose prefix matches
// the event topic. Non-blocking per subscriber: each callback runs in
// its own goroutine to prevent one slow consumer from blocking others.
func (b *Bus) Publish(e Event) {
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return
	}

	// Collect matching handlers. We match by prefix: a subscription to
	// "device.1" should receive "device.1.telemetry".
	// Also match the empty-prefix subscriber (catch-all).
	var matches []handler
	for prefix, handlers := range b.subs {
		if prefix == "" || hasPrefix(e.Topic, prefix) {
			matches = append(matches, handlers...)
		}
	}
	b.mu.RUnlock()

	for _, h := range matches {
		go h.callback(e)
	}
}

// hasPrefix reports whether s starts with prefix, treating the prefix
// boundary as a dot-separated segment match. "device.1" matches
// "device.1.x" but NOT "device.10.x".
func hasPrefix(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	if s[:len(prefix)] != prefix {
		return false
	}
	// Exact match is fine.
	if len(s) == len(prefix) {
		return true
	}
	// Otherwise require a segment boundary right after the prefix.
	return s[len(prefix)] == '.'
}

// Close shuts down the bus. Subsequent Publish calls are no-ops.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.closed = true
		close(b.closeChan)
	}
}
