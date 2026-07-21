package ws

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Client wraps a single WebSocket connection with its identity,
// subscriptions, and a buffered send channel.
//
// Lifecycle:
//  1. HTTP handler upgrades the connection and creates a Client with
//     the JWT-derived (user_id, device_id, is_admin).
//  2. Hub.Register adds it to the hub.
//  3. readPump and writePump goroutines run until the connection closes.
//  4. Hub.Unregister + Client.close() clean up.
type Client struct {
	hub      *Hub
	conn     *websocket.Conn
	id       uint64
	userID   uint
	deviceID uint
	isAdmin  bool

	// subscriptions is the set of topic prefixes this client wants to
	// receive events for. Admins implicitly receive everything.
	mu            sync.RWMutex
	subscriptions map[string]struct{}

	send chan []byte

	closeOnce sync.Once
	closed    bool

	// v1.6.16: optional callbacks invoked by the client lifecycle.
	// Used by ws_handler to wire WebSocket connect/disconnect into
	// the device.Manager so that Android app clients (which connect
	// via WS, not MQTT) are counted as "online devices" on the
	// dashboard. Both may be nil.
	onHeartbeat  func(deviceID uint)
	onDisconnect func(deviceID uint)
}

// NewClient creates a Client for the given WebSocket connection.
// The caller is responsible for registering it with the hub.
func NewClient(hub *Hub, conn *websocket.Conn, userID, deviceID uint, isAdmin bool) *Client {
	return &Client{
		hub:           hub,
		conn:          conn,
		userID:        userID,
		deviceID:      deviceID,
		isAdmin:       isAdmin,
		subscriptions: make(map[string]struct{}),
		send:          make(chan []byte, 64), // buffered to absorb bursts
	}
}

// SetLifecycleCallbacks wires optional heartbeat/disconnect callbacks.
// v1.6.16: used by ws_handler to bridge WS client lifecycle into
// device.Manager so Android app clients (HTTP+WS only, no MQTT) are
// counted as online devices. Both callbacks receive the client's
// deviceID. Either may be nil.
func (c *Client) SetLifecycleCallbacks(onHeartbeat, onDisconnect func(uint)) {
	c.onHeartbeat = onHeartbeat
	c.onDisconnect = onDisconnect
}

// ReadPump pumps messages from the WebSocket connection to the hub.
// It runs in its own goroutine and exits when the connection closes.
//
// The client may send:
//   - {"type":"heartbeat"}                        → server replies with ack
//   - {"type":"subscribe","topic":"device.1"}     → add subscription
//   - {"type":"unsubscribe","topic":"device.1"}   → remove subscription
func (c *Client) ReadPump() {
	defer func() {
		c.hub.Unregister(c.id)
		c.close()
	}()

	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("ws: read error (user=%d device=%d): %v",
					c.userID, c.deviceID, err)
			}
			return
		}

		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			c.sendError("invalid json")
			continue
		}

		c.handleClientMessage(msg)
	}
}

// handleClientMessage dispatches an inbound client message.
func (c *Client) handleClientMessage(msg Message) {
	switch msg.Type {
	case MsgHeartbeat:
		// Ack the heartbeat.
		ack, _ := NewMessage(MsgHeartbeat, "", map[string]string{
			"status": "ok",
		})
		c.sendMsg(ack)
		// v1.6.16: forward to device.Manager so Android app clients
		// (which send WS heartbeats, not MQTT) stay marked online.
		if c.onHeartbeat != nil {
			c.onHeartbeat(c.deviceID)
		}

	case MsgSubscribe:
		if msg.Topic == "" {
			c.sendError("missing topic")
			return
		}
		c.addSubscription(msg.Topic)

	case MsgUnsubscribe:
		c.removeSubscription(msg.Topic)

	default:
		c.sendError("unknown message type: " + msg.Type)
	}
}

// addSubscription adds a topic prefix to this client's subscription set.
func (c *Client) addSubscription(prefix string) {
	c.mu.Lock()
	c.subscriptions[prefix] = struct{}{}
	c.mu.Unlock()
}

// removeSubscription removes a topic prefix.
func (c *Client) removeSubscription(prefix string) {
	c.mu.Lock()
	delete(c.subscriptions, prefix)
	c.mu.Unlock()
}

// matchesSubscription reports whether any of the client's subscriptions
// is a prefix of the given topic. Admins always match.
func (c *Client) matchesSubscription(topic string) bool {
	if c.isAdmin {
		return true
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for prefix := range c.subscriptions {
		if strings.HasPrefix(topic, prefix) {
			return true
		}
	}
	return false
}

// WritePump pumps messages from the send channel to the WebSocket
// connection. It also runs a periodic ping to keep the connection
// alive and detect dead peers.
func (c *Client) WritePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.close()
	}()

	for {
		select {
		case data, ok := <-c.send:
			if !ok {
				// Channel closed: write a close frame and exit.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// sendMsg queues a message for delivery to this client. Non-blocking: if
// the buffer is full, the message is dropped (and the client is likely
// to be disconnected soon by the ping timeout).
func (c *Client) sendMsg(msg Message) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	select {
	case c.send <- data:
	default:
		// Buffer full — drop. The client will recover via reconnect.
		log.Printf("ws: send buffer full, dropping message (user=%d)", c.userID)
	}
}

// sendError sends an error message to the client.
func (c *Client) sendError(message string) {
	msg, _ := NewMessage(MsgError, "", map[string]string{
		"error": message,
	})
	c.sendMsg(msg)
}

// close terminates the connection. Idempotent.
func (c *Client) close() {
	c.closeOnce.Do(func() {
		c.closed = true
		close(c.send)
		c.conn.Close()
		// v1.6.16: notify device.Manager that this WS client
		// disconnected, so the device's online state can be
		// reconciled (the manager's sweep loop will finalize
		// the offline transition after heartbeatTimeout).
		if c.onDisconnect != nil {
			c.onDisconnect(c.deviceID)
		}
	})
}
