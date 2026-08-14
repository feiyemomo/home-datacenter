package ws

import (
	"encoding/json"
	"testing"

	"home-datacenter-api/internal/eventbus"
)

// newTestClient builds a Client with a buffered send channel but no real
// WebSocket connection. The hub's fan-out methods only marshal into the
// send channel (they never touch conn), so this is sufficient to assert
// routing decisions deterministically.
func newTestClient(userID uint, isAdmin bool, subs ...string) *Client {
	c := &Client{
		userID:        userID,
		isAdmin:       isAdmin,
		subscriptions: make(map[string]struct{}),
		send:          make(chan []byte, 64),
	}
	for _, s := range subs {
		c.subscriptions[s] = struct{}{}
	}
	return c
}

// drainOne reports whether the client has a pending outbound message.
func drainOne(t *testing.T, c *Client) bool {
	t.Helper()
	select {
	case <-c.send:
		return true
	default:
		return false
	}
}

func mustRaw(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestRegisterUnregister(t *testing.T) {
	h := NewHub(eventbus.New())
	c1 := newTestClient(1, false)
	c2 := newTestClient(2, false)

	id1 := h.Register(c1)
	id2 := h.Register(c2)
	if id1 != 1 || id2 != 2 {
		t.Errorf("ids = %d,%d, want increasing from 1", id1, id2)
	}
	if c1.id != id1 || c2.id != id2 {
		t.Error("client id should be assigned by Register")
	}
	if got := h.OnlineClientCount(); got != 2 {
		t.Errorf("online = %d, want 2", got)
	}

	h.Unregister(id1)
	if got := h.OnlineClientCount(); got != 1 {
		t.Errorf("online after unregister = %d, want 1", got)
	}
}

func TestHubCloseEmpty(t *testing.T) {
	h := NewHub(eventbus.New())
	h.Close()
	h.Close() // idempotent, no clients to disconnect
}

func TestHubBroadcast(t *testing.T) {
	h := NewHub(eventbus.New())
	c1 := newTestClient(1, false)
	c2 := newTestClient(2, false)
	c3 := newTestClient(3, true)
	h.Register(c1)
	h.Register(c2)
	h.Register(c3)

	msg := Message{Type: MsgEvent, Topic: "system.broadcast", Payload: json.RawMessage(`{"m":1}`)}
	h.Broadcast(msg)

	for i, c := range []*Client{c1, c2, c3} {
		if !drainOne(t, c) {
			t.Errorf("client %d did not receive broadcast", i)
		}
	}
}

func TestHubSendToUser(t *testing.T) {
	h := NewHub(eventbus.New())
	c1 := newTestClient(10, false)
	c2 := newTestClient(10, false)
	c3 := newTestClient(20, false)
	h.Register(c1)
	h.Register(c2)
	h.Register(c3)

	h.SendToUser(10, Message{Type: MsgEvent, Topic: "user.notification"})
	if !drainOne(t, c1) {
		t.Error("user 10 client 1 should receive")
	}
	if !drainOne(t, c2) {
		t.Error("user 10 client 2 should receive")
	}
	if drainOne(t, c3) {
		t.Error("user 20 should not receive a user-10 notification")
	}
}

func TestHubSendToAdmins(t *testing.T) {
	h := NewHub(eventbus.New())
	admin1 := newTestClient(1, true)
	admin2 := newTestClient(2, true)
	user := newTestClient(3, false)
	h.Register(admin1)
	h.Register(admin2)
	h.Register(user)

	h.SendToAdmins(Message{Type: MsgEvent, Topic: "admin.only"})
	if !drainOne(t, admin1) || !drainOne(t, admin2) {
		t.Error("admins should receive admin messages")
	}
	if drainOne(t, user) {
		t.Error("non-admin should not receive admin messages")
	}
}

func TestHubRouteDeviceEvent(t *testing.T) {
	h := NewHub(eventbus.New())
	admin := newTestClient(1, true)
	sub := newTestClient(2, false, "device.1")
	other := newTestClient(3, false, "camera")
	h.Register(admin)
	h.Register(sub)
	h.Register(other)

	msg := Message{Type: MsgEvent, Topic: "device.1.status"}
	h.routeDeviceEvent(eventbus.Event{Topic: "device.1.status"}, msg)

	if !drainOne(t, admin) {
		t.Error("admin should receive every device event")
	}
	if !drainOne(t, sub) {
		t.Error("subscribed client should receive the device event")
	}
	if drainOne(t, other) {
		t.Error("client subscribed to camera should not receive device.1")
	}
}

func TestClientMatchesSubscription(t *testing.T) {
	admin := newTestClient(1, true)
	if !admin.matchesSubscription("anything.at.all") {
		t.Error("admin should match every topic")
	}

	c := newTestClient(2, false, "device.1")
	cases := map[string]bool{
		"device.1.status": true,
		"device.1":        true,
		"camera.online":   false,
	}
	for topic, want := range cases {
		if got := c.matchesSubscription(topic); got != want {
			t.Errorf("matchesSubscription(%q) = %v, want %v", topic, got, want)
		}
	}
}

func TestHubOnEvent(t *testing.T) {
	bus := eventbus.New()
	h := NewHub(bus)
	user1 := newTestClient(10, false)
	user2 := newTestClient(20, false)
	admin := newTestClient(1, true)
	h.Register(user1)
	h.Register(user2)
	h.Register(admin)

	// Targeted user notification → only the target user.
	notif := eventbus.Event{
		Topic:   eventbus.TopicUserNotification,
		Source:  eventbus.SourceAutomation,
		Payload: mustRaw(t, eventbus.UserNotificationPayload{UserID: 10, Title: "t"}),
	}
	h.onEvent(notif)
	if !drainOne(t, user1) {
		t.Error("targeted user should receive their notification")
	}
	if drainOne(t, user2) {
		t.Error("non-target user should not receive the notification")
	}
	if drainOne(t, admin) {
		t.Error("admin should not receive a targeted notification unless they are the target")
	}

	// System broadcast → every client.
	bc := eventbus.Event{Topic: eventbus.TopicSystemBroadcast, Payload: []byte(`{"m":"hi"}`)}
	h.onEvent(bc)
	if !drainOne(t, user1) || !drainOne(t, user2) || !drainOne(t, admin) {
		t.Error("system.broadcast should reach every client")
	}

	// Device event → admin + matching subscriber only.
	dev := eventbus.Event{Topic: "device.1.status", Source: "mqtt", Payload: []byte(`{"status":"online"}`)}
	h.onEvent(dev)
	if !drainOne(t, admin) {
		t.Error("admin should receive device events")
	}
	if drainOne(t, user1) {
		t.Error("unsubscribed user should not receive the device event")
	}
}

func TestHubOnEvent_MalformedNotificationFallsBackToBroadcast(t *testing.T) {
	h := NewHub(eventbus.New())
	user := newTestClient(10, false)
	h.Register(user)

	// user.notification with a non-JSON payload → parse error → broadcast fallback.
	ev := eventbus.Event{Topic: eventbus.TopicUserNotification, Payload: []byte(`not-json`)}
	h.onEvent(ev)
	if !drainOne(t, user) {
		t.Error("malformed notification should fall through to broadcast")
	}
}