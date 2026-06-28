// Package ws implements the WebSocket layer for real-time push to apps.
//
// Architecture: Hub pattern
//
//	Client (per WebSocket connection)
//	   ↕
//	Hub (registry of all clients, routes messages)
//	   ↕
//	EventBus (subscribes to device.* events, broadcasts to clients)
//
// Auth model (hybrid):
//   - Initial HTTP upgrade is authenticated with the existing 365-day
//     JWT (same Authorization: Bearer header used by REST).
//   - After upgrade, the connection is kept alive by a ping/pong
//     heartbeat (no per-message token revalidation).
//   - The JWT's (user_id, device_id) claims identify the connection
//     and gate which topics it can subscribe to.
package ws

import (
	"encoding/json"
	"time"
)

// MessageType enumerates the WebSocket application-level message types.
// All messages are JSON with a "type" field.
const (
	// MsgHeartbeat — client → server: keep-alive ping.
	// server → client: heartbeat ack.
	MsgHeartbeat = "heartbeat"

	// MsgSubscribe — client → server: subscribe to a topic prefix.
	// Payload: {"topic":"device.1"}
	MsgSubscribe = "subscribe"

	// MsgUnsubscribe — client → server: stop receiving a topic prefix.
	MsgUnsubscribe = "unsubscribe"

	// MsgEvent — server → client: an event from the EventBus.
	// Payload: {"topic":"device.telemetry","data":{...},"source":"mqtt"}
	MsgEvent = "event"

	// MsgBroadcast — server → client: a system broadcast.
	MsgBroadcast = "broadcast"

	// MsgError — server → client: error description.
	MsgError = "error"

	// MsgOnlineList — server → client: list of currently online devices.
	MsgOnlineList = "online_list"
)

// Message is the canonical WebSocket message envelope.
//
// Wire format:
//
//	{
//	  "type": "heartbeat",
//	  "topic": "",
//	  "payload": {},
//	  "ts": 1234567890
//	}
//
// For server→client events, `topic` is the EventBus topic and
// `payload` is the raw event payload (often nested JSON).
type Message struct {
	Type    string          `json:"type"`
	Topic   string          `json:"topic,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	TS      int64           `json:"ts"`
}

// NewMessage is a convenience constructor.
func NewMessage(msgType string, topic string, payload interface{}) (Message, error) {
	m := Message{
		Type: msgType,
		Topic: topic,
		TS:   time.Now().Unix(),
	}
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return Message{}, err
		}
		m.Payload = data
	}
	return m, nil
}

// heartbeatInterval is how often the server sends pings to the client.
const heartbeatInterval = 30 * time.Second

// writeWait is the deadline for writing a message to the peer.
const writeWait = 10 * time.Second

// pongWait is the deadline for receiving a pong from the peer.
const pongWait = 60 * time.Second

// pingPeriod is slightly less than pongWait to avoid races.
const pingPeriod = (pongWait * 9) / 10
