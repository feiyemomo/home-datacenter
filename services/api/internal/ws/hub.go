package ws

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"home-datacenter-api/internal/eventbus"
)

// Hub maintains the set of active WebSocket clients and routes
// events from the EventBus to the appropriate clients.
//
// A Hub is created once at startup and shared across all WebSocket
// connections. It subscribes to the EventBus on creation and
// unsubscribes on Close().
type Hub struct {
	mu      sync.RWMutex
	clients map[uint64]*Client // keyed by internal client ID
	nextID  uint64

	bus    *eventbus.Bus
	unsub  func() // EventBus unsubscribe handle
	closed atomic.Bool
}

// NewHub creates a Hub and subscribes it to all device.* and
// system.* events on the EventBus.
func NewHub(bus *eventbus.Bus) *Hub {
	h := &Hub{
		clients: make(map[uint64]*Client),
		bus:     bus,
	}

	// Subscribe to all relevant event topics. Prefix matching means
	// "device" catches device.status, device.telemetry, device.command;
	// "camera" catches camera.online, camera.offline, camera.motion, etc.
	topics := []string{
		"device",
		"camera",
		eventbus.TopicUserNotification,
		eventbus.TopicSystemBroadcast,
		eventbus.TopicAutomationFired,
	}
	for _, t := range topics {
		bus.Subscribe(t, h.onEvent)
	}

	return h
}

// Close shuts down the hub and disconnects all clients.
func (h *Hub) Close() {
	if h.closed.Swap(true) {
		return
	}
	if h.unsub != nil {
		h.unsub()
	}
	h.mu.Lock()
	for _, c := range h.clients {
		c.close()
	}
	h.clients = nil
	h.mu.Unlock()
}

// Register adds a client to the hub and returns its assigned ID.
func (h *Hub) Register(c *Client) uint64 {
	id := atomic.AddUint64(&h.nextID, 1)
	c.id = id

	h.mu.Lock()
	h.clients[id] = c
	h.mu.Unlock()

	return id
}

// Unregister removes a client from the hub.
func (h *Hub) Unregister(id uint64) {
	h.mu.Lock()
	delete(h.clients, id)
	h.mu.Unlock()
}

// Broadcast sends a message to every connected client.
func (h *Hub) Broadcast(msg Message) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.clients {
		c.sendMsg(msg)
	}
}

// SendToUser sends a message to every client authenticated as the
// given user. Used for targeted notifications.
func (h *Hub) SendToUser(userID uint, msg Message) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.clients {
		if c.userID == userID {
			c.sendMsg(msg)
		}
	}
}

// SendToAdmins sends a message to every admin client.
func (h *Hub) SendToAdmins(msg Message) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.clients {
		if c.isAdmin {
			c.sendMsg(msg)
		}
	}
}

// onEvent is the EventBus callback. It inspects the event topic and
// routes the message to the right clients.
func (h *Hub) onEvent(e eventbus.Event) {
	// Use the payload as raw JSON if valid; otherwise wrap as a string
	// so invalid JSON from malformed MQTT messages doesn't break delivery.
	var payload json.RawMessage = e.Payload
	if !json.Valid(e.Payload) {
		wrapped, _ := json.Marshal(string(e.Payload))
		payload = wrapped
	}

	msg := Message{
		Type:    MsgEvent,
		Topic:   e.Topic,
		Payload: payload,
		TS:      time.Now().Unix(),
	}

	// Prepend the event source for client-side debugging.
	// (payload is already raw JSON; we just wrap it.)

	switch e.Topic {
	case eventbus.TopicUserNotification:
		// Targeted: parse user_id from payload and send to that user.
		var p eventbus.UserNotificationPayload
		if err := json.Unmarshal(e.Payload, &p); err == nil {
			h.SendToUser(p.UserID, msg)
			return
		}
		// fallthrough to broadcast on parse error
	case eventbus.TopicSystemBroadcast:
		h.Broadcast(msg)
		return
	}

	// Device events: send to admins and to the device's owner.
	// We need the device_id from the payload to find the owner, but
	// since we don't have a user↔device map here, we broadcast to
	// admins and also push to all clients subscribed to the topic.
	h.routeDeviceEvent(e, msg)
}

// routeDeviceEvent sends a device event to admins and to any client
// whose subscription prefix matches the event topic.
func (h *Hub) routeDeviceEvent(e eventbus.Event, msg Message) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, c := range h.clients {
		// Admins always receive all device events.
		if c.isAdmin {
			c.sendMsg(msg)
			continue
		}
		// Non-admins receive only events whose topic matches one of
		// their subscriptions.
		if c.matchesSubscription(e.Topic) {
			c.sendMsg(msg)
		}
	}
}

// OnlineDeviceCount returns the number of clients currently connected.
// (Not the number of online devices — that's the device.Manager's job.)
func (h *Hub) OnlineClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// PushOnlineList sends the current list of online device IDs to a
// specific client. The list is provided by the caller (from
// device.Manager).
func (h *Hub) PushOnlineList(client *Client, deviceIDs []uint) {
	msg, err := NewMessage(MsgOnlineList, "", map[string]interface{}{
		"device_ids": deviceIDs,
		"count":      len(deviceIDs),
	})
	if err != nil {
		return
	}
	client.sendMsg(msg)
}

// conn is a minimal interface around *websocket.Conn to allow
// easier testing. In production the concrete type is used.
type conn interface {
	WriteJSON(v interface{}) error
	ReadMessage() (int, []byte, error)
	WriteMessage(messageType int, data []byte) error
	WriteControl(messageType int, data []byte, deadline time.Time) error
	SetReadDeadline(t time.Time) error
	SetPongHandler(h func(string) error)
	Close() error
}

var _ conn = (*websocket.Conn)(nil)
