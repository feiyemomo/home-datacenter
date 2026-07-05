// Package automation implements the rule engine that turns EventBus
// events into actions (notify / mqtt / webhook).
//
// Architecture
// ------------
//
//	EventBus (subscribe "*")
//	    │
//	    ▼
//	Engine.handleEvent
//	    │  ─ for each enabled Rule whose Trigger matches the event topic
//	    ▼
//	Condition matches?
//	    │  ─ time window + payload_eq filter
//	    ▼
//	executeAction (notify | mqtt | webhook)
//	    │
//	    ▼
//	publish automation.fired event (for UI / audit)
//
// The engine keeps an in-memory copy of all enabled rules so the hot
// path is allocation-free; Reload() refreshes the cache after a CRUD
// operation on /api/v1/automation/rules.
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
package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
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

// Engine evaluates rules against every EventBus event.
//
// Goroutine-safety: Reload() and handleEvent() may run concurrently;
// the rules slice is guarded by mu. handleEvent() takes a read snapshot.
type Engine struct {
	db   *gorm.DB
	bus  *eventbus.Bus
	mqtt MQTTPublisher // may be nil if MQTT is down
	http *http.Client

	mu    sync.RWMutex
	rules []model.Rule // in-memory cache of enabled rules
	unsub func()        // EventBus unsubscribe handle

	// actionTimeout caps how long a single webhook action may take.
	// Default 5s; overridable for tests.
	actionTimeout time.Duration
}

// NewEngine wires the engine to its dependencies. Call Start() to
// subscribe to the EventBus and load the initial rule set.
func NewEngine(db *gorm.DB, bus *eventbus.Bus, mqtt MQTTPublisher) *Engine {
	return &Engine{
		db:            db,
		bus:           bus,
		mqtt:          mqtt,
		http:          &http.Client{Timeout: 5 * time.Second},
		actionTimeout: 5 * time.Second,
	}
}

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
	e.rules = rules
	e.mu.Unlock()
	log.Printf("automation: loaded %d enabled rule(s)", len(rules))
	return nil
}

// handleEvent is the EventBus callback. It runs in the publishing
// goroutine, so we keep work light: condition checks are O(1) per rule,
// and actions are dispatched to their own goroutines.
func (e *Engine) handleEvent(ev eventbus.Event) {
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
		// Fire in a goroutine so one slow webhook cannot stall the
		// engine or block the EventBus publisher.
		go e.fire(ev, r)
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
// All specified fields must match (AND).
func conditionMatches(c model.Condition, ev eventbus.Event) bool {
	// Time window.
	if c.TimeGTE != "" || c.TimeLTE != "" {
		now := ev.Timestamp
		if now.IsZero() {
			now = time.Now()
		}
		if !timeInRange(now, c.TimeGTE, c.TimeLTE) {
			return false
		}
	}
	// Payload equality filter.
	if len(c.PayloadEQ) > 0 {
		var payload map[string]any
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			return false // malformed JSON never matches
		}
		for k, want := range c.PayloadEQ {
			got, ok := payload[k]
			if !ok || !equalJSON(got, want) {
				return false
			}
		}
	}
	return true
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

// fire executes the action, records the firing, and emits an
// automation.fired event for audit / UI.
func (e *Engine) fire(ev eventbus.Event, r model.Rule) {
	if err := e.executeAction(r.Action, ev); err != nil {
		log.Printf("automation: rule %d action %q failed: %v",
			r.ID, r.Action.Type, err)
	}

	now := time.Now()
	// Persist fire stats asynchronously so the action goroutine is
	// not blocked by DB I/O.
	go func(id uint) {
		_ = e.db.Model(&model.Rule{}).Where("id = ?", id).
			Updates(map[string]any{
				"fire_count":  gorm.Expr("fire_count + 1"),
				"last_fire_at": now,
			}).Error
	}(r.ID)

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
			"ts":         now.Unix(),
		}),
	})
}

// executeAction runs the configured action.
func (e *Engine) executeAction(a model.Action, ev eventbus.Event) error {
	switch a.Type {
	case "notify":
		return e.actionNotify(a, ev)
	case "mqtt":
		return e.actionMQTT(a)
	case "webhook":
		return e.actionWebhook(a, ev)
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
func (e *Engine) actionWebhook(a model.Action, ev eventbus.Event) error {
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
	// Resolve the host and reject private / loopback / link-local IPs.
	// We split host:port here because url.Host may include a port.
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

	ctx, cancel := context.WithTimeout(context.Background(), e.actionTimeout)
	defer cancel()

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
	// Default content type for POST/PUT.
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
