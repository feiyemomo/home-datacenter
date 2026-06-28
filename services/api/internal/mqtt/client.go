package mqtt

import (
	"errors"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
)

// ErrNotConnected is returned when a publish is attempted before the
// MQTT client is connected.
var ErrNotConnected = errors.New("mqtt: client not connected")

// Config holds MQTT broker connection settings.
type Config struct {
	Broker   string // e.g. "tcp://mosquitto:1883"
	ClientID string // e.g. "home-datacenter"
	Username string // optional; empty = anonymous
	Password string // optional
	QoS      byte   // default QoS for subscriptions
}

// Client wraps a paho.mqtt client with auto-reconnect and
// auto-resubscribe. Reconnection is handled by paho itself; this
// wrapper configures it and exposes a clean Start/Stop lifecycle.
type Client struct {
	config  Config
	paho    pahomqtt.Client
	handler *Handler
}

// NewClient creates a Client. The handler receives all messages and
// connection lifecycle callbacks.
func NewClient(cfg Config, handler *Handler) *Client {
	c := &Client{
		config:  cfg,
		handler: handler,
	}

	opts := pahomqtt.NewClientOptions()
	opts.AddBroker(cfg.Broker)
	opts.SetClientID(cfg.ClientID)
	opts.SetAutoReconnect(true)
	opts.SetMaxReconnectInterval(30 * time.Second)
	opts.SetCleanSession(false)

	if cfg.Username != "" {
		opts.SetUsername(cfg.Username)
		opts.SetPassword(cfg.Password)
	}

	// Wire lifecycle callbacks. The handler holds the paho client
	// reference so it can publish; we set that in Start().
	opts.OnConnect = func(cl pahomqtt.Client) {
		handler.client = cl
		handler.OnConnect(cl)
	}
	opts.OnConnectionLost = handler.OnDisconnect

	c.paho = pahomqtt.NewClient(opts)
	return c
}

// Start connects to the broker. It retries the initial connection
// up to maxRetries times with the given interval. Once the initial
// connection succeeds, paho's auto-reconnect handles subsequent drops.
func (c *Client) Start() error {
	const maxRetries = 5
	const retryInterval = 3 * time.Second

	var lastErr error
	for i := 0; i < maxRetries; i++ {
		token := c.paho.Connect()
		token.Wait()
		err := token.Error()
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(retryInterval)
	}
	return lastErr
}

// Stop disconnects cleanly.
func (c *Client) Stop() {
	if c.paho != nil && c.paho.IsConnected() {
		c.paho.Disconnect(500) // 500ms grace period
	}
}

// IsConnected reports whether the underlying paho client is currently
// connected to the broker.
func (c *Client) IsConnected() bool {
	return c.paho != nil && c.paho.IsConnected()
}

// Handler returns the message handler (used by callers to publish).
func (c *Client) Handler() *Handler {
	return c.handler
}
