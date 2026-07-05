// Package automation implements the rule engine that turns EventBus
// events into actions (notify / mqtt / webhook).
//
// Architecture (Phase 6 — Automation Runtime)
// ------------
//
//	EventBus (subscribe "*")
//	    │
//	    ▼
//	Engine.handleEvent                          ── per-rule fan-out
//	    │  ─ for each enabled Rule whose Trigger matches the event topic
//	    │  ─ Throttle:  cooldown / rate limit / dedup
//	    ▼
//	Condition.eval                              ── time / source /
//	    │                                         payload_eq / threshold /
//	    │                                         regex / Any(OR)
//	    ▼
//	Action.execute                              ── with timeout + retry
//	    │
//	    ▼
//	Metrics.RecordFire                          ── counters + last fire
//	    │
//	    ▼
//	publish automation.fired event              ── for UI / audit
//
// The engine keeps an in-memory copy of all enabled rules plus a
// per-rule runtime state (last fire, recent fires for rate limit,
// last seen event hash for dedup) so the hot path is allocation-
// free. Reload() refreshes the cache after a CRUD operation on
// /api/v1/automation/rules.
//
// Security
// --------
//
//   - MQTT actions are restricted to the "home-datacenter/" topic
//     namespace (same rule as /api/v1/mqtt/publish). A compromised
//     admin token cannot publish to $SYS or third-party plugin topics.
//   - Webhook actions block localhost, private, and link-local IP
//     ranges to mitigate SSRF. HTTPS is strongly recommended.
//   - All rule CRUD endpoints are admin-only (enforced in main.go).
//   - Rate limit / cooldown / dedup prevent event-flood storms from
//     triggering action storms (e.g. a noisy motion sensor firing
//     10 webhooks/s at a paid SaaS).
package automation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// MQTTPublisher is the subset of mqtt.Handler the engine needs.
// Implemented by *mqtt.Handler via Publish(topic, payload string, qos byte).
type MQTTPublisher interface {
	Publish(topic, payload string, qos byte) error
}

// defaultActionTimeoutMs is used when an action omits TimeoutMs.
const defaultActionTimeoutMs = 5000

// maxBackoff caps the exponential-backoff delay between retries.
const maxBackoff = 30 * time.Second

// Engine evaluates rules against every EventBus event.
//
// Goroutine-safety: Reload() and handleEvent() may run concurrently;
// the rules slice and per-rule runtime state are guarded by mu.
// handleEvent() takes a read snapshot.
type Engine struct {
	db   *gorm.DB
	bus  *eventbus.Bus
	mqtt MQTTPublisher // may be nil if MQTT is down
	http *http.Client

	mu      sync.RWMutex
	rules   []model.Rule
	runtime map[uint]*ruleRuntime // per-rule hot-path state
	unsub   func()                // EventBus unsubscribe handle

	// Global metrics counters.
	metrics *Metrics
}

// ruleRuntime is the per-rule hot-path state we keep in memory.
type ruleRuntime struct {
	lastFire     time.Time
	fireCount    uint64
	fireHistory  []time.Time // for rate_per_min sliding window
	lastEventKey string      // for dedup
}

func newRuleRuntime() *ruleRuntime {
	return &ruleRuntime{fireHistory: make([]time.Time, 0, 16)}
}

// NewEngine wires the engine to its dependencies. Call Start() to
// subscribe to the EventBus and load the initial rule set.
func NewEngine(db *gorm.DB, bus *eventbus.Bus, mqtt MQTTPublisher) *Engine {
	return &Engine{
		db:      db,
		bus:     bus,
		mqtt:    mqtt,
		http:    &http.Client{Timeout: 30 * time.Second},
		metrics: NewMetrics(),
	}
}

// Metrics returns the global metrics counter. Read-only; the
// handler layer exposes this via /api/v1/automation/metrics.
func (e *Engine) Metrics() *Metrics { return e.metrics }

// Start loads enabled rules from the DB and subscribes to "*" on the
// EventBus. Idempotent: calling it twice is safe.
func (e *Engine) Start() {
	if err := e.Reload(); err != nil {
		log.Printf("automation: initial reload: %v", err)
	}
	if e.unsub == nil {
		e.unsub = e.bus.Subscribe("*", e.handleEvent)
	}
}

// Stop unsubscribes from the EventBus. Idempotent.
func (e *Engine) Stop() {
	if e.unsub != nil {
		e.unsub()
		e.unsub = nil
	}
}

// Reload re-reads enabled rules from the DB. Call this after any CRUD
// operation on /api/v1/automation/rules.
func (e *Engine) Reload() error {
	var rules []model.Rule
	if err := e.db.Where("enabled = ?", true).Find(&rules).Error; err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules = rules
	// Prune runtime for rules that no longer exist; keep the
	// entries for surviving rules so a name edit doesn't reset
	// cooldowns mid-flight.
	keep := make(map[uint]struct{}, len(rules))
	for _, r := range rules {
		keep[r.ID] = struct{}{}
	}
	if e.runtime == nil {
		e.runtime = make(map[uint]*ruleRuntime, len(rules))
	}
	for id := range e.runtime {
		if _, ok := keep[id]; !ok {
			delete(e.runtime, id)
		}
	}
	for _, r := range rules {
		if _, ok := e.runtime[r.ID]; !ok {
			e.runtime[r.ID] = newRuleRuntime()
		}
	}
	log.Printf("automation: loaded %d enabled rule(s)", len(rules))
	return nil
}

// handleEvent is the EventBus callback. It runs in the publishing
// goroutine, so we keep work light: condition checks are O(1) per rule,
// and actions are dispatched to their own goroutines.
func (e *Engine) handleEvent(ev eventbus.Event) {
	e.metrics.IncEvent()

	// Snapshot rules under the read lock; release before running actions.
	e.mu.RLock()
	rules := make([]model.Rule, len(e.rules))
	copy(rules, e.rules)
	e.mu.RUnlock()

	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if !triggerMatches(r.Trigger, ev.Topic) {
			continue
		}
		if !conditionMatches(r.Condition, ev) {
			continue
		}
		// Throttle: cooldown / rate limit / dedup. Holds the
		// write lock briefly; we copy the rule so the action
		// goroutine doesn't need it.
		e.mu.Lock()
		rt, ok := e.runtime[r.ID]
		if !ok {
			rt = newRuleRuntime()
			e.runtime[r.ID] = rt
		}
		allowed, reason := throttleAllows(rt, r.Throttle, ev)
		if allowed {
			recordFire(rt, ev)
		}
		e.mu.Unlock()
		if !allowed {
			e.metrics.IncDropped()
			e.metrics.RecordRuleDropped(r.ID)
			log.Printf("automation: rule %d (%s) dropped by throttle: %s",
				r.ID, r.Name, reason)
			continue
		}
		// Fire in a goroutine so one slow webhook cannot stall
		// the engine or block the EventBus publisher.
		go e.fire(ev, r)
	}
}

// throttleAllows reports whether the rule may fire now, given its
// runtime state and configured Throttle. The reason string is for
// log lines (only used when allowed == false).
func throttleAllows(rt *ruleRuntime, t model.Throttle, ev eventbus.Event) (bool, string) {
	now := time.Now()

	// Cooldown: silent for cooldown_s seconds after the last fire.
	if t.CooldownS > 0 && !rt.lastFire.IsZero() {
		earliest := rt.lastFire.Add(time.Duration(t.CooldownS) * time.Second)
		if now.Before(earliest) {
			return false, fmt.Sprintf("cooldown until %s", earliest.Format(time.RFC3339))
		}
	}

	// Rate limit: sliding window of `rate_per_min` fires per 60s.
	if t.RatePerMin > 0 {
		cutoff := now.Add(-60 * time.Second)
		// Prune the window.
		i := 0
		for ; i < len(rt.fireHistory); i++ {
			if rt.fireHistory[i].After(cutoff) {
				break
			}
		}
		rt.fireHistory = rt.fireHistory[i:]
		if len(rt.fireHistory) >= t.RatePerMin {
			return false, fmt.Sprintf("rate limit %d/min exceeded", t.RatePerMin)
		}
	}

	// Dedup: collapse identical events.
	if t.Dedup {
		key := dedupKey(ev)
		if key != "" && key == rt.lastEventKey {
			return false, "dedup hit (identical event)"
		}
	}

	return true, ""
}

// dedupKey is a small fingerprint of an event for dedup matching.
// It hashes topic + source + payload. Returns "" for events with
// no useful identity (empty payload).
func dedupKey(ev eventbus.Event) string {
	if len(ev.Payload) == 0 {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(ev.Topic))
	h.Write([]byte{0})
	h.Write([]byte(ev.Source))
	h.Write([]byte{0})
	h.Write(ev.Payload)
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8]) // 16 hex chars; collision-resistant enough
}

// recordFire mutates the runtime state after a successful
// throttleAllows check. Must be called under e.mu.
func recordFire(rt *ruleRuntime, ev eventbus.Event) {
	now := time.Now()
	rt.lastFire = now
	rt.fireCount++
	rt.fireHistory = append(rt.fireHistory, now)
	if len(rt.fireHistory) > 256 {
		// Cap to keep memory bounded even if rate_per_min=0
		// and CooldownS=0 (i.e. fire is always allowed).
		rt.fireHistory = rt.fireHistory[len(rt.fireHistory)-128:]
	}
	if key := dedupKey(ev); key != "" {
		rt.lastEventKey = key
	}
}

// triggerMatches reports whether the rule's trigger pattern matches
// the event topic. Semantics are identical to EventBus.Subscribe:
//   - "*" or "" matches everything
//   - "device" matches "device.status", "device.telemetry", etc.
//   - "device.1" matches "device.1.x" (segment boundary)
//   - exact match is always allowed
func triggerMatches(trigger, topic string) bool {
	if trigger == "*" || trigger == "" {
		return true
	}
	if trigger == topic {
		return true
	}
	if len(topic) < len(trigger) {
		return false
	}
	if topic[:len(trigger)] != trigger {
		return false
	}
	// Segment boundary right after the trigger prefix.
	return topic[len(trigger)] == '.'
}

// conditionMatches evaluates the rule's Condition against the event.
// All specified fields are evaluated; the Any flag switches the
// combining operator from AND to OR.
func conditionMatches(c model.Condition, ev eventbus.Event) bool {
	payload, _ := decodePayload(ev.Payload)

	checks := []func() bool{}
	// Source.
	if c.Source != "" {
		s := c.Source
		checks = append(checks, func() bool { return ev.Source == s })
	}
	// Time window.
	if c.TimeGTE != "" || c.TimeLTE != "" {
		checks = append(checks, func() bool {
			now := ev.Timestamp
			if now.IsZero() {
				now = time.Now()
			}
			return timeInRange(now, c.TimeGTE, c.TimeLTE)
		})
	}
	// Payload equality filter.
	if len(c.PayloadEQ) > 0 {
		eq := c.PayloadEQ
		checks = append(checks, func() bool {
			for k, want := range eq {
				got, ok := payload[k]
				if !ok || !equalJSON(got, want) {
					return false
				}
			}
			return true
		})
	}
	// Numeric threshold.
	if len(c.Threshold) > 0 {
		th := c.Threshold
		checks = append(checks, func() bool {
			for k, want := range th {
				got, ok := payload[k]
				if !ok {
					return false
				}
				gotNum, ok := toFloat(got)
				if !ok {
					return false
				}
				if !compareNumeric(want.Op, gotNum, want.Val) {
					return false
				}
			}
			return true
		})
	}
	// Regex match (RE2).
	if len(c.Regex) > 0 {
		patterns := make(map[string]*regexp.Regexp, len(c.Regex))
		for k, pat := range c.Regex {
			re, err := regexp.Compile(pat)
			if err != nil {
				// Compile error at runtime → this check fails
				// for the event. We do NOT want to crash the
				// engine over a bad regex, so we log once via
				// a stub match.
				log.Printf("automation: regex %q: %v", pat, err)
				return false
			}
			patterns[k] = re
		}
		rx := patterns
		checks = append(checks, func() bool {
			for k, re := range rx {
				got, ok := payload[k]
				if !ok {
					return false
				}
				s, ok := got.(string)
				if !ok {
					// Non-string fields don't match.
					return false
				}
				if !re.MatchString(s) {
					return false
				}
			}
			return true
		})
	}

	if len(checks) == 0 {
		return true
	}
	if c.Any {
		// OR: at least one check must pass.
		for _, ok := range checks {
			if ok() {
				return true
			}
		}
		return false
	}
	// AND (default): every check must pass.
	for _, ok := range checks {
		if !ok() {
			return false
		}
	}
	return true
}

// decodePayload is a best-effort JSON decode. Returns an empty
// (but non-nil) map on failure so callers can safely use ok-checks.
func decodePayload(raw []byte) (map[string]any, bool) {
	if len(raw) == 0 {
		return map[string]any{}, false
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{}, false
	}
	return m, true
}

// toFloat coerces a JSON-decoded value to float64. JSON numbers
// decode as float64 already; this also handles the case where the
// field is encoded as a string ("42") for forward-compat with
// systems that send numeric values as strings.
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// compareNumeric applies Op. Unknown ops return false (i.e. the
// field is considered non-matching) so a typo in a rule doesn't
// silently match everything.
func compareNumeric(op string, got, want float64) bool {
	switch op {
	case ">":
		return got > want
	case ">=":
		return got >= want
	case "<":
		return got < want
	case "<=":
		return got <= want
	case "==", "=":
		return got == want
	case "!=", "<>":
		return got != want
	}
	return false
}

// timeInRange reports whether t falls inside [gte, lte] (inclusive),
// interpreted as 24h "HH:MM" bounds. If gte > lte the range wraps
// midnight (e.g. 22:00-06:00). Empty bounds are ignored.
func timeInRange(t time.Time, gte, lte string) bool {
	g, gteOK := parseHHMM(gte)
	l, lteOK := parseHHMM(lte)
	if !gteOK && !lteOK {
		return true
	}
	cur := t.Hour()*60 + t.Minute()
	switch {
	case gteOK && lteOK:
		if g <= l {
			return cur >= g && cur <= l
		}
		// Wrap midnight: g..23:59 OR 0..l.
		return cur >= g || cur <= l
	case gteOK:
		return cur >= g
	case lteOK:
		return cur <= l
	}
	return true
}

// parseHHMM parses "HH:MM" into minutes-of-day.
func parseHHMM(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, false
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, false
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// equalJSON compares two values after JSON normalisation. This makes
// 1 == 1.0 and "offline" == "offline" without float/reflect tag soup.
func equalJSON(a, b any) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

// fire executes the action (with retry), records the firing, and
// emits an automation.fired event for audit / UI.
func (e *Engine) fire(ev eventbus.Event, r model.Rule) {
	started := time.Now()
	err := e.executeActionWithRetry(r.Action, ev)
	dur := time.Since(started)
	e.metrics.RecordFire(err == nil, dur)
	e.metrics.RecordRuleFire(r.ID, err == nil, dur)

	now := time.Now()
	// Persist fire stats asynchronously so the action goroutine
	// is not blocked by DB I/O. On error we still bump fire_count
	// (the action was attempted) but also bump error_count via
	// metrics only — the DB column tracks attempts.
	updates := map[string]any{
		"fire_count":  gorm.Expr("fire_count + 1"),
		"last_fire_at": now,
	}
	if err != nil {
		log.Printf("automation: rule %d action %q failed: %v",
			r.ID, r.Action.Type, err)
	}
	go func(id uint, u map[string]any) {
		_ = e.db.Model(&model.Rule{}).Where("id = ?", id).Updates(u).Error
	}(r.ID, updates)

	// Emit audit event.
	e.bus.PublishAsync(eventbus.Event{
		Topic:    eventbus.TopicAutomationFired,
		Source:   eventbus.SourceAutomation,
		Severity: eventbus.SeverityInfo,
		Payload: mustJSON(map[string]any{
			"rule_id":    r.ID,
			"rule_name":  r.Name,
			"trigger":    r.Trigger,
			"action":     r.Action.Type,
			"event_id":   ev.ID,
			"event_type": ev.Topic,
			"event_src":  ev.Source,
			"ok":         err == nil,
			"err":        errString(err),
			"duration_ms": dur.Milliseconds(),
			"ts":         now.Unix(),
		}),
	})
}

// errString renders an error for audit JSON; nil → "".
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// PinCooldown sets a rule's last-fire timestamp to (now - cooldown),
// effectively silencing the rule for the requested duration. This is
// the admin escape hatch for a misbehaving rule (handy when a webhook
// target is down and the rule is firing every event). Returns an
// error if the rule is unknown to the engine.
func (e *Engine) PinCooldown(ruleID uint, cooldown time.Duration) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	rt, ok := e.runtime[ruleID]
	if !ok {
		return fmt.Errorf("rule %d not loaded in engine (does it exist and is it enabled?)", ruleID)
	}
	rt.lastFire = time.Now().Add(-cooldown)
	return nil
}

// executeActionWithRetry calls executeAction and retries on
// transient failures up to a.RetryMax times, with exponential
// backoff capped at maxBackoff. Non-retryable errors (validation,
// 4xx) fail fast.
//
// `notify` and `mqtt` actions are not retried even if RetryMax>0:
// the underlying call is best-effort and has its own internal
// retry/redelivery semantics (the MQTT broker for `mqtt`; the
// EventBus for `notify`).
func (e *Engine) executeActionWithRetry(a model.Action, ev eventbus.Event) error {
	if a.Type == "notify" || a.Type == "mqtt" {
		return e.executeAction(a, ev)
	}
	max := a.RetryMax
	if max < 0 {
		max = 0
	}
	delay := 500 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt <= max; attempt++ {
		err := e.executeAction(a, ev)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryable(err) {
			return err
		}
		if attempt == max {
			break
		}
		time.Sleep(delay)
		delay *= 2
		if delay > maxBackoff {
			delay = maxBackoff
		}
	}
	return lastErr
}

// isRetryable reports whether err is a transient failure (network
// error, 5xx). Validation errors and 4xx responses are permanent.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "returned 4") {
		// 4xx is permanent (bad request, unauthorized, etc).
		return false
	}
	// Everything else (network error, 5xx, timeout) is retryable.
	return true
}

// executeAction runs the configured action with the action's
// per-attempt timeout.
func (e *Engine) executeAction(a model.Action, ev eventbus.Event) error {
	timeoutMs := a.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = defaultActionTimeoutMs
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	return e.executeActionCtx(ctx, a, ev)
}

// executeActionCtx is the context-aware action dispatcher. The
// notify/mqtt actions ignore ctx (they're best-effort); webhook
// honors it.
func (e *Engine) executeActionCtx(ctx context.Context, a model.Action, ev eventbus.Event) error {
	switch a.Type {
	case "notify":
		return e.actionNotify(a, ev)
	case "mqtt":
		return e.actionMQTT(a)
	case "webhook":
		return e.actionWebhook(ctx, a, ev)
	default:
		return fmt.Errorf("unknown action type %q", a.Type)
	}
}

// actionNotify publishes a user.notification event on the EventBus.
// The WebSocket Hub already subscribes to that topic and routes it to
// the target user. If UserID is 0, the notification is broadcast to
// all admins (severity=warn so it surfaces in the audit trail too).
func (e *Engine) actionNotify(a model.Action, ev eventbus.Event) error {
	title := a.Title
	body := a.Body
	if title == "" {
		title = "Automation: " + ev.Topic
	}
	if body == "" {
		body = fmt.Sprintf("Rule fired on event %q from %q", ev.Topic, ev.Source)
	}
	e.bus.PublishAsync(eventbus.Event{
		Topic:    eventbus.TopicUserNotification,
		Source:   eventbus.SourceAutomation,
		Severity: eventbus.SeverityInfo,
		Payload: mustJSON(map[string]any{
			"user_id":    a.UserID,
			"title":      title,
			"body":       body,
			"event_type": ev.Topic,
			"ts":         time.Now().Unix(),
		}),
	})
	return nil
}

// actionMQTT publishes a raw MQTT message via the MQTT handler.
// The topic must be inside the "home-datacenter/" namespace — same
// rule as the /api/v1/mqtt/publish endpoint — to prevent a compromised
// admin token from writing to $SYS or third-party plugin topics.
func (e *Engine) actionMQTT(a model.Action) error {
	if e.mqtt == nil {
		return fmt.Errorf("mqtt not available")
	}
	if a.Topic == "" {
		return fmt.Errorf("mqtt action missing topic")
	}
	if !isAllowedMQTTTopic(a.Topic) {
		return fmt.Errorf("mqtt topic must be within home-datacenter/ namespace")
	}
	qos := a.QoS
	if qos == 0 {
		qos = 1
	}
	return e.mqtt.Publish(a.Topic, a.Payload, qos)
}

// actionWebhook fires an HTTP request to an external URL.
//
// SSRF mitigation: the resolved host must NOT be a private, loopback,
// or link-local address. This blocks attacks like
// http://169.254.169.254/latest/meta-data/ (cloud metadata) and
// http://localhost:8080/admin. HTTPS is strongly recommended but not
// enforced — the user may have a legitimate HTTP-only integration.
func (e *Engine) actionWebhook(ctx context.Context, a model.Action, ev eventbus.Event) error {
	if a.URL == "" {
		return fmt.Errorf("webhook action missing url")
	}
	u, err := url.Parse(a.URL)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("webhook url scheme must be http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("webhook url missing host")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("webhook url missing host")
	}
	if err := assertPublicHost(host); err != nil {
		return err
	}

	method := a.Method
	if method == "" {
		method = http.MethodPost
	}

	// Body: explicit payload if provided, otherwise the triggering
	// event's payload as-is (already JSON).
	bodyStr := a.Payload
	if bodyStr == "" {
		bodyStr = string(ev.Payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.URL, strings.NewReader(bodyStr))
	if err != nil {
		return err
	}
	if a.Payload != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range a.Headers {
		req.Header.Set(k, v)
	}

	resp, err := e.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// Avoid putting the response body in the error string
		// (could be megabytes of HTML).
		return fmt.Errorf("webhook returned %s", resp.Status)
	}
	return nil
}

// mqttNamespace is the root topic namespace the server manages.
const mqttNamespace = "home-datacenter/"

// isAllowedMQTTTopic reports whether a topic is inside the
// home-datacenter namespace and is not a broker control topic ($SYS).
func isAllowedMQTTTopic(topic string) bool {
	if topic == "" {
		return false
	}
	if strings.HasPrefix(topic, "$") {
		return false
	}
	return strings.HasPrefix(topic, mqttNamespace)
}

// assertPublicHost resolves host and returns an error if it points at
// a private, loopback, link-local, or unspecified address. This is the
// SSRF guard for webhook actions.
//
// We resolve once at fire time. A determined attacker with DNS
// rebinding could still race this check, but defence-in-depth + the
// admin-only rule surface makes the risk acceptable for a home OS.
func assertPublicHost(host string) error {
	// Allow a literal IP first.
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return fmt.Errorf("webhook host %s is private/loopback/link-local", host)
		}
		return nil
	}
	// Otherwise resolve A/AAAA and check every result.
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("webhook host %s resolve: %w", host, err)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return fmt.Errorf("webhook host %s resolves to private/loopback/link-local %s", host, ip)
		}
	}
	return nil
}

// isPublicIP reports whether ip is a publicly routable address.
// Returns false for loopback, private, link-local, and unspecified.
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return false
	}
	return true
}

// mustJSON is a tiny helper for building event payloads.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

