package automation

import (
	"sync"
	"sync/atomic"
	"time"
)

// Metrics is a goroutine-safe, in-memory counter set the engine
// updates as it processes events. It is intentionally simple — no
// histograms, no labels, no Prometheus dependency. A home OS has a
// handful of rules at most; the operator reads this via the admin
// /api/v1/automation/metrics endpoint or via the automation.fired
// event stream.
//
// Concurrency
//
//   - EventsSeen / Fires / Errors / Dropped use atomic.Uint64 for
//     lock-free increments on the hot path.
//   - LastFire / TotalDurationMs / PerRule use a mutex; they are
//     updated on the fire() goroutine, which is one per rule per
//     event so contention is bounded by the rule count.
//
// Reset
//
//	Reset() is provided for /api/v1/automation/metrics?reset=1 (admin
//	only) and for tests. It zeros every counter and clears per-rule
//	stats.
type Metrics struct {
	EventsSeen      atomic.Uint64
	Fires           atomic.Uint64
	Errors          atomic.Uint64
	Dropped         atomic.Uint64 // dropped by Throttle
	TotalDurationNs atomic.Uint64
	MaxDurationNs   atomic.Uint64
	StartedAt       time.Time

	mu      sync.RWMutex
	perRule map[uint]*RuleMetrics
}

// RuleMetrics is the per-rule slice of Metrics.
type RuleMetrics struct {
	Fires    uint64
	Errors   uint64
	Dropped  uint64
	LastFire time.Time
	AvgMs    float64
	MaxMs    int64
}

// NewMetrics constructs a Metrics with a non-zero StartedAt.
func NewMetrics() *Metrics {
	return &Metrics{
		StartedAt: time.Now(),
		perRule:   make(map[uint]*RuleMetrics, 16),
	}
}

// IncEvent is called once per incoming EventBus event, before
// per-rule fan-out. Lock-free.
func (m *Metrics) IncEvent() {
	m.EventsSeen.Add(1)
}

// IncDropped is called once per rule that the throttle rejected.
func (m *Metrics) IncDropped() {
	m.Dropped.Add(1)
}

// RecordFire is called once per rule that actually fired (i.e. passed
// trigger + condition + throttle and ran the action). ok=false means
// the action returned an error.
//
// The duration is the total time the action took including any
// retries. We record both the global max and the per-rule max/avg
// so the operator can see "rule 3 has a flaky webhook" at a glance.
func (m *Metrics) RecordFire(ok bool, d time.Duration) {
	m.Fires.Add(1)
	ns := uint64(d.Nanoseconds())
	m.TotalDurationNs.Add(ns)
	// Atomic max via CAS loop. We only need an approximation, so
	// racing updates are acceptable — the recorded max is the
	// largest value the goroutine has ever observed.
	for {
		old := m.MaxDurationNs.Load()
		if ns <= old {
			break
		}
		if m.MaxDurationNs.CompareAndSwap(old, ns) {
			break
		}
	}
	if !ok {
		m.Errors.Add(1)
	}
}

// RecordRuleFire updates the per-rule slice. The handler layer
// exposes this through /api/v1/automation/rules/:id/metrics.
func (m *Metrics) RecordRuleFire(ruleID uint, ok bool, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rm, ok2 := m.perRule[ruleID]
	if !ok2 {
		rm = &RuleMetrics{}
		m.perRule[ruleID] = rm
	}
	rm.Fires++
	if !ok {
		rm.Errors++
	}
	rm.LastFire = time.Now()
	ms := d.Milliseconds()
	if ms > rm.MaxMs {
		rm.MaxMs = ms
	}
	// Running mean.
	rm.AvgMs = rm.AvgMs + (float64(ms)-rm.AvgMs)/float64(rm.Fires)
}

// RecordRuleDropped increments the per-rule dropped counter.
func (m *Metrics) RecordRuleDropped(ruleID uint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rm, ok := m.perRule[ruleID]
	if !ok {
		rm = &RuleMetrics{}
		m.perRule[ruleID] = rm
	}
	rm.Dropped++
}

// Snapshot returns a JSON-safe copy of the current counters and
// per-rule stats. Callers should treat the returned struct as
// read-only.
type Snapshot struct {
	EventsSeen    uint64                `json:"events_seen"`
	Fires         uint64                `json:"fires"`
	Errors        uint64                `json:"errors"`
	Dropped       uint64                `json:"dropped"`
	AvgDurationMs float64               `json:"avg_duration_ms"`
	MaxDurationMs int64                 `json:"max_duration_ms"`
	StartedAt     time.Time             `json:"started_at"`
	UptimeSeconds int64                 `json:"uptime_seconds"`
	PerRule       map[uint]*RuleMetrics `json:"per_rule"`
}

// Snapshot returns a point-in-time copy.
func (m *Metrics) Snapshot() Snapshot {
	fires := m.Fires.Load()
	totalNs := m.TotalDurationNs.Load()
	maxNs := m.MaxDurationNs.Load()
	var avgMs float64
	if fires > 0 {
		avgMs = float64(totalNs/fires) / 1e6
	}
	m.mu.RLock()
	per := make(map[uint]*RuleMetrics, len(m.perRule))
	for k, v := range m.perRule {
		// Copy so callers can mutate freely.
		cp := *v
		per[k] = &cp
	}
	m.mu.RUnlock()
	return Snapshot{
		EventsSeen:    m.EventsSeen.Load(),
		Fires:         fires,
		Errors:        m.Errors.Load(),
		Dropped:       m.Dropped.Load(),
		AvgDurationMs: avgMs,
		MaxDurationMs: int64(maxNs / 1e6),
		StartedAt:     m.StartedAt,
		UptimeSeconds: int64(time.Since(m.StartedAt).Seconds()),
		PerRule:       per,
	}
}

// Reset clears all counters. Intended for /api/v1/automation/metrics?reset=1
// and for tests. Cannot fail.
func (m *Metrics) Reset() {
	m.EventsSeen.Store(0)
	m.Fires.Store(0)
	m.Errors.Store(0)
	m.Dropped.Store(0)
	m.TotalDurationNs.Store(0)
	m.MaxDurationNs.Store(0)
	m.StartedAt = time.Now()
	m.mu.Lock()
	m.perRule = make(map[uint]*RuleMetrics, 16)
	m.mu.Unlock()
}
