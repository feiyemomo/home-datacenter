package eventbus

import (
	"sync"
	"sync/atomic"
	"time"
)

// Bus is a thread-safe in-memory pub/sub bus.
//
// Subscriptions match by topic prefix, allowing wildcards naturally:
// subscribing to "device.1" receives "device.1.telemetry",
// "device.1.status", etc. Subscribing to "*" receives all events.
//
// Publish is non-blocking: each callback runs in its own goroutine
// to prevent one slow consumer from blocking others (fan-out).
type Bus struct {
	mu     sync.RWMutex
	nextID uint64 // atomic counter for Event.ID
	subs   map[string][]handler
	closed bool
}

// handler wraps a subscriber callback together with an identifier so
// it can be unsubscribed later.
type handler struct {
	id       uint64
	callback func(Event)
}

// New creates a ready-to-use Bus.
func New() *Bus {
	return &Bus{
		subs: make(map[string][]handler),
	}
}

// Subscribe registers a callback for any event whose topic starts with
// prefix. Returns an unsubscribe function.
//
// Special prefix "*" matches all events (wildcard).
// Empty prefix "" also matches all events (catch-all).
//
// Example:
//
//	unsub := bus.Subscribe("device.1", func(e Event) { ... })
//	defer unsub()
func (b *Bus) Subscribe(prefix string, cb func(Event)) func() {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := atomic.AddUint64(&b.nextID, 1)
	h := handler{id: id, callback: cb}
	b.subs[prefix] = append(b.subs[prefix], h)
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
//
// Auto-fills ID, Timestamp, and Severity if not set by the caller.
func (b *Bus) Publish(e Event) {
	// Auto-fill metadata.
	if e.ID == 0 {
		e.ID = atomic.AddUint64(&b.nextID, 1)
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	if e.Severity == "" {
		e.Severity = SeverityInfo
	}

	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return
	}

	// Collect matching handlers. We match by prefix: a subscription to
	// "device.1" should receive "device.1.telemetry".
	// "*" and "" are catch-all subscribers.
	var matches []handler
	for prefix, handlers := range b.subs {
		if prefix == "*" || prefix == "" || hasPrefix(e.Topic, prefix) {
			matches = append(matches, handlers...)
		}
	}
	b.mu.RUnlock()

	for _, h := range matches {
		h.callback(e) // runs in the caller's goroutine by default
	}
}

// PublishAsync is like Publish but each callback runs in its own
// goroutine. Use this for fire-and-forget scenarios where the caller
// must not be blocked by slow subscribers.
func (b *Bus) PublishAsync(e Event) {
	// Auto-fill metadata.
	if e.ID == 0 {
		e.ID = atomic.AddUint64(&b.nextID, 1)
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	if e.Severity == "" {
		e.Severity = SeverityInfo
	}

	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return
	}

	var matches []handler
	for prefix, handlers := range b.subs {
		if prefix == "*" || prefix == "" || hasPrefix(e.Topic, prefix) {
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
	b.closed = true
}
