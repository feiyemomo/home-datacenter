package automation

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// setupTestDB opens an in-memory SQLite DB (single connection so the
// in-memory store is shared across queries) with the Rule table migrated.
func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&model.Rule{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// mockMQTT records Publish calls and signals via a channel so tests can
// wait deterministically on the fire() goroutine.
type mockMQTT struct {
	mu     sync.Mutex
	calls  []mqttCall
	signal chan struct{}
}

type mqttCall struct {
	topic   string
	payload string
	qos     byte
}

func (m *mockMQTT) Publish(topic, payload string, qos byte) error {
	m.mu.Lock()
	m.calls = append(m.calls, mqttCall{topic, payload, qos})
	m.mu.Unlock()
	if m.signal != nil {
		m.signal <- struct{}{}
	}
	return nil
}

func (m *mockMQTT) topics() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.calls))
	for _, c := range m.calls {
		out = append(out, c.topic)
	}
	return out
}

func TestThrottleAllows(t *testing.T) {
	// No throttle config → always allowed, no reason.
	rt := newRuleRuntime()
	allowed, reason := throttleAllows(rt, model.Throttle{}, eventbus.Event{})
	if !allowed || reason != "" {
		t.Errorf("no throttle: allowed=%v reason=%q, want true, \"\"", allowed, reason)
	}

	// Cooldown active: last fire was 10s ago, cooldown is 60s.
	rt2 := newRuleRuntime()
	rt2.lastFire = time.Now().Add(-10 * time.Second)
	allowed, _ = throttleAllows(rt2, model.Throttle{CooldownS: 60}, eventbus.Event{})
	if allowed {
		t.Error("cooldown should block fire within 60s window")
	}

	// Cooldown expired: last fire was 70s ago.
	rt3 := newRuleRuntime()
	rt3.lastFire = time.Now().Add(-70 * time.Second)
	allowed, _ = throttleAllows(rt3, model.Throttle{CooldownS: 60}, eventbus.Event{})
	if !allowed {
		t.Error("cooldown should allow fire after window elapsed")
	}

	// Rate limit: 3 fires in the window, limit is 3/min.
	rt4 := newRuleRuntime()
	for i := 0; i < 3; i++ {
		rt4.fireHistory = append(rt4.fireHistory, time.Now())
	}
	allowed, _ = throttleAllows(rt4, model.Throttle{RatePerMin: 3}, eventbus.Event{})
	if allowed {
		t.Error("rate limit should block when window is full")
	}

	// Rate limit: 2 fires in the window, limit is 3/min → allowed.
	rt5 := newRuleRuntime()
	for i := 0; i < 2; i++ {
		rt5.fireHistory = append(rt5.fireHistory, time.Now())
	}
	allowed, _ = throttleAllows(rt5, model.Throttle{RatePerMin: 3}, eventbus.Event{})
	if !allowed {
		t.Error("rate limit should allow when under limit")
	}

	// Dedup: identical event to last seen is blocked.
	rt6 := newRuleRuntime()
	ev := eventbus.Event{Topic: "device.status", Source: "mqtt", Payload: []byte(`{"status":"online"}`)}
	rt6.lastEventKey = dedupKey(ev)
	allowed, _ = throttleAllows(rt6, model.Throttle{Dedup: true}, ev)
	if allowed {
		t.Error("dedup should block identical event")
	}

	// Dedup: a different event is allowed.
	ev2 := eventbus.Event{Topic: "device.status", Source: "mqtt", Payload: []byte(`{"status":"offline"}`)}
	allowed, _ = throttleAllows(rt6, model.Throttle{Dedup: true}, ev2)
	if !allowed {
		t.Error("dedup should allow a different event")
	}
}

func TestDedupKey(t *testing.T) {
	// Empty payload → no identity.
	if k := dedupKey(eventbus.Event{}); k != "" {
		t.Errorf("dedupKey(empty) = %q, want \"\"", k)
	}

	e1 := eventbus.Event{Topic: "t", Source: "s", Payload: []byte(`{"x":1}`)}
	e2 := eventbus.Event{Topic: "t", Source: "s", Payload: []byte(`{"x":1}`)}
	e3 := eventbus.Event{Topic: "t", Source: "s", Payload: []byte(`{"x":2}`)}
	if dedupKey(e1) != dedupKey(e2) {
		t.Error("identical events should produce identical keys")
	}
	if dedupKey(e1) == dedupKey(e3) {
		t.Error("different payloads should produce different keys")
	}
}

func TestRecordFire(t *testing.T) {
	rt := newRuleRuntime()
	ev := eventbus.Event{Topic: "t", Source: "s", Payload: []byte(`{"x":1}`)}
	recordFire(rt, ev)
	if rt.fireCount != 1 {
		t.Errorf("fireCount = %d, want 1", rt.fireCount)
	}
	if rt.lastFire.IsZero() {
		t.Error("lastFire should be set")
	}
	if rt.lastEventKey != dedupKey(ev) {
		t.Error("lastEventKey should match the fired event")
	}
	if len(rt.fireHistory) != 1 {
		t.Errorf("fireHistory len = %d, want 1", len(rt.fireHistory))
	}
}

func TestReload_LoadsOnlyEnabledAndPrunes(t *testing.T) {
	db := setupTestDB(t)
	e := NewEngine(db, eventbus.New(), nil)

	a := model.Rule{Name: "a", Trigger: "device", Enabled: true}
	b := model.Rule{Name: "b", Trigger: "sensor", Enabled: true}
	if err := db.Create(&a).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.Create(&b).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := e.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(e.rules) != 2 {
		t.Fatalf("rules = %+v, want 2 enabled rules", e.rules)
	}

	// Disable b (Update sets the column explicitly, unlike Create which
	// strips zero-value fields behind a gorm default tag), then reload →
	// only a remains and b's runtime is pruned.
	if err := db.Model(&model.Rule{}).Where("id = ?", b.ID).Update("enabled", false).Error; err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := e.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(e.rules) != 1 || e.rules[0].Name != "a" {
		t.Errorf("rules = %+v, want only enabled rule a", e.rules)
	}
	if _, ok := e.runtime[b.ID]; ok {
		t.Error("disabled rule's runtime should be pruned")
	}

	// Disable a, then reload → no rules; runtime entries pruned.
	if err := db.Model(&model.Rule{}).Where("id = ?", a.ID).Update("enabled", false).Error; err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := e.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(e.rules) != 0 {
		t.Errorf("rules = %+v, want empty after disabling all", e.rules)
	}
	if len(e.runtime) != 0 {
		t.Errorf("runtime = %+v, want empty after prune", e.runtime)
	}
}

func TestReload_KeepsRuntimeForSurvivingRule(t *testing.T) {
	db := setupTestDB(t)
	e := NewEngine(db, eventbus.New(), nil)
	r := model.Rule{Name: "r", Trigger: "device", Enabled: true}
	if err := db.Create(&r).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := e.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	// Simulate an in-flight cooldown.
	e.runtime[r.ID].lastFire = time.Now()

	if err := e.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	rt, ok := e.runtime[r.ID]
	if !ok {
		t.Fatal("surviving rule runtime should be preserved across reload")
	}
	if rt.lastFire.IsZero() {
		t.Error("in-flight cooldown should survive reload")
	}
}

func TestPinCooldown(t *testing.T) {
	db := setupTestDB(t)
	e := NewEngine(db, eventbus.New(), nil)
	r := model.Rule{Name: "r", Trigger: "device", Enabled: true}
	if err := db.Create(&r).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := e.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if err := e.PinCooldown(r.ID, 10*time.Minute); err != nil {
		t.Fatalf("PinCooldown: %v", err)
	}
	rt := e.runtime[r.ID]
	if since := time.Since(rt.lastFire); since < 9*time.Minute {
		t.Errorf("PinCooldown lastFire pushed %v into past, want ~10m", since)
	}

	if err := e.PinCooldown(99999, time.Minute); err == nil {
		t.Error("PinCooldown on an unknown rule should error")
	}
}

func TestHandleEvent_MQTTActionAndCooldown(t *testing.T) {
	db := setupTestDB(t)
	bus := eventbus.New()
	mq := &mockMQTT{signal: make(chan struct{}, 16)}
	e := NewEngine(db, bus, mq)

	rule := model.Rule{
		Name:     "turn on fan",
		Trigger:  "device",
		Action:   model.Action{Type: "mqtt", Topic: "home-datacenter/devices/1/cmd", Payload: "on"},
		Throttle: model.Throttle{CooldownS: 60},
		Enabled:  true,
	}
	if err := db.Create(&rule).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := e.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	ev := eventbus.Event{Topic: "device.status", Source: "mqtt", Payload: []byte(`{"status":"online"}`)}
	e.handleEvent(ev)

	select {
	case <-mq.signal:
	case <-time.After(2 * time.Second):
		t.Fatal("expected an MQTT publish from the first event")
	}
	if got := e.metrics.Fires.Load(); got != 1 {
		t.Errorf("fires = %d, want 1", got)
	}
	if got := e.metrics.EventsSeen.Load(); got != 1 {
		t.Errorf("events_seen = %d, want 1", got)
	}
	if topics := mq.topics(); len(topics) != 1 || topics[0] != "home-datacenter/devices/1/cmd" {
		t.Errorf("published topics = %v, want exactly the rule topic", topics)
	}

	// Second identical event within the cooldown window → dropped.
	e.handleEvent(ev)
	select {
	case <-mq.signal:
		t.Fatal("cooldown should have blocked the second publish")
	case <-time.After(100 * time.Millisecond):
	}
	if got := e.metrics.Dropped.Load(); got != 1 {
		t.Errorf("dropped = %d, want 1", got)
	}
	if got := e.metrics.Fires.Load(); got != 1 {
		t.Errorf("fires = %d, want still 1 after cooldown drop", got)
	}
}

func TestHandleEvent_ConditionGatesAction(t *testing.T) {
	db := setupTestDB(t)
	mq := &mockMQTT{signal: make(chan struct{}, 16)}
	e := NewEngine(db, eventbus.New(), mq)

	rule := model.Rule{
		Name:    "alert on offline",
		Trigger: "device",
		Action:  model.Action{Type: "mqtt", Topic: "home-datacenter/alert", Payload: "1"},
		Condition: model.Condition{
			PayloadEQ: map[string]any{"status": "offline"},
		},
		Enabled: true,
	}
	if err := db.Create(&rule).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := e.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Non-matching payload → no action, no drop (condition fails pre-throttle).
	e.handleEvent(eventbus.Event{Topic: "device.status", Source: "mqtt", Payload: []byte(`{"status":"online"}`)})
	select {
	case <-mq.signal:
		t.Fatal("condition mismatch should not fire the action")
	case <-time.After(100 * time.Millisecond):
	}
	if got := e.metrics.Fires.Load(); got != 0 {
		t.Errorf("fires = %d, want 0", got)
	}
	if got := e.metrics.Dropped.Load(); got != 0 {
		t.Errorf("dropped = %d, want 0 (condition mismatch is not a throttle drop)", got)
	}

	// Matching payload → action fires.
	e.handleEvent(eventbus.Event{Topic: "device.status", Source: "mqtt", Payload: []byte(`{"status":"offline"}`)})
	select {
	case <-mq.signal:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the action to fire for a matching condition")
	}
	if got := e.metrics.Fires.Load(); got != 1 {
		t.Errorf("fires = %d, want 1", got)
	}
}

func TestActionMQTT(t *testing.T) {
	mq := &mockMQTT{}
	e := NewEngine(setupTestDB(t), eventbus.New(), mq)

	// Valid topic, default QoS 1.
	a := model.Action{Type: "mqtt", Topic: "home-datacenter/devices/1/cmd", Payload: "on"}
	if err := e.executeAction(a, eventbus.Event{}); err != nil {
		t.Fatalf("mqtt action: %v", err)
	}
	if len(mq.calls) != 1 || mq.calls[0].qos != 1 {
		t.Errorf("calls = %+v, want 1 call with qos 1", mq.calls)
	}

	// Explicit QoS.
	a2 := model.Action{Type: "mqtt", Topic: "home-datacenter/x", Payload: "p", QoS: 2}
	if err := e.executeAction(a2, eventbus.Event{}); err != nil {
		t.Fatalf("mqtt action: %v", err)
	}
	if mq.calls[1].qos != 2 {
		t.Errorf("qos = %d, want 2", mq.calls[1].qos)
	}

	// Disallowed topic ($SYS).
	a3 := model.Action{Type: "mqtt", Topic: "$SYS/broker/version"}
	if err := e.executeAction(a3, eventbus.Event{}); err == nil {
		t.Error("broker-control topic should be rejected")
	}

	// Missing topic.
	a4 := model.Action{Type: "mqtt"}
	if err := e.executeAction(a4, eventbus.Event{}); err == nil {
		t.Error("missing topic should be rejected")
	}

	// MQTT handler unavailable.
	e2 := NewEngine(setupTestDB(t), eventbus.New(), nil)
	if err := e2.executeAction(a, eventbus.Event{}); err == nil {
		t.Error("nil mqtt handler should error")
	}
}

func TestActionNotify(t *testing.T) {
	bus := eventbus.New()
	e := NewEngine(setupTestDB(t), bus, nil)

	got := make(chan eventbus.Event, 4)
	bus.Subscribe(eventbus.TopicUserNotification, func(ev eventbus.Event) { got <- ev })

	a := model.Action{Type: "notify", UserID: 5, Title: "Hello", Body: "World"}
	if err := e.executeAction(a, eventbus.Event{Topic: "device.status", Source: "mqtt"}); err != nil {
		t.Fatalf("notify action: %v", err)
	}
	select {
	case ev := <-got:
		var p struct {
			UserID uint   `json:"user_id"`
			Title  string `json:"title"`
			Body   string `json:"body"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if p.UserID != 5 || p.Title != "Hello" || p.Body != "World" {
			t.Errorf("notification = %+v, want user=5 title/body set", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected a user.notification event")
	}

	// Default title/body when omitted.
	a2 := model.Action{Type: "notify", UserID: 1}
	if err := e.executeAction(a2, eventbus.Event{Topic: "camera.motion", Source: "camera"}); err != nil {
		t.Fatalf("notify action: %v", err)
	}
	select {
	case ev := <-got:
		var p map[string]any
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if p["title"] != "Automation: camera.motion" {
			t.Errorf("default title = %v, want Automation: camera.motion", p["title"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected a second user.notification event")
	}
}

func TestExecuteAction_UnknownType(t *testing.T) {
	e := NewEngine(setupTestDB(t), eventbus.New(), nil)
	if err := e.executeAction(model.Action{Type: "time_machine"}, eventbus.Event{}); err == nil {
		t.Error("unknown action type should error")
	}
}

// roundTripFunc lets tests intercept HTTP without touching the network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func resp(status int, body string) *http.Response {
	// Status must mirror a real HTTP status line ("400 Bad Request") so
	// the engine's isRetryable() 4xx detection matches.
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestActionWebhook_SSRFGuard(t *testing.T) {
	e := NewEngine(setupTestDB(t), eventbus.New(), nil)
	cases := []string{
		"http://127.0.0.1/hook",        // loopback
		"http://10.0.0.5/hook",         // private
		"http://192.168.1.1/hook",      // private
		"http://169.254.169.254/latest", // link-local / cloud metadata
		"ftp://8.8.8.8/x",              // bad scheme
		"http:///nohost",               // missing host
		"",                             // missing url
	}
	for _, u := range cases {
		a := model.Action{Type: "webhook", URL: u}
		if err := e.executeAction(a, eventbus.Event{}); err == nil {
			t.Errorf("webhook %q should be rejected by validation/SSRF guard", u)
		}
	}
}

func TestActionWebhook_Success(t *testing.T) {
	e := NewEngine(setupTestDB(t), eventbus.New(), nil)
	var gotMethod, gotURL, gotBody string
	e.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotMethod = req.Method
		gotURL = req.URL.String()
		b, _ := io.ReadAll(req.Body)
		gotBody = string(b)
		return resp(200, "ok"), nil
	})}

	a := model.Action{Type: "webhook", URL: "http://8.8.8.8/hook", Method: "POST", Payload: `{"a":1}`}
	if err := e.executeAction(a, eventbus.Event{}); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if gotMethod != "POST" || gotURL != "http://8.8.8.8/hook" {
		t.Errorf("request method/url = %q %q", gotMethod, gotURL)
	}
	if gotBody != `{"a":1}` {
		t.Errorf("body = %q, want explicit payload", gotBody)
	}
}

func TestActionWebhook_RetryOn5xx(t *testing.T) {
	e := NewEngine(setupTestDB(t), eventbus.New(), nil)
	var calls int
	e.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return resp(500, "boom"), nil
	})}

	a := model.Action{Type: "webhook", URL: "http://8.8.8.8/hook", RetryMax: 1}
	if err := e.executeActionWithRetry(a, eventbus.Event{}); err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (initial + 1 retry)", calls)
	}
}

func TestActionWebhook_NoRetryOn4xx(t *testing.T) {
	e := NewEngine(setupTestDB(t), eventbus.New(), nil)
	var calls int
	e.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return resp(400, "bad"), nil
	})}

	a := model.Action{Type: "webhook", URL: "http://8.8.8.8/hook", RetryMax: 3}
	if err := e.executeActionWithRetry(a, eventbus.Event{}); err == nil {
		t.Fatal("expected error for 4xx")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (4xx is permanent, no retry)", calls)
	}
}