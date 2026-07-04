// Package config loads application configuration from a YAML file
// using viper. The loaded config is exposed via the package-level
// AppConfig variable.
//
//	Usage:
//	  if err := config.Load("configs/config.yaml"); err != nil {
//	      log.Fatal(err)
//	  }
//	  port := config.AppConfig.Server.Port
package config

import (
	"fmt"
	"os"

	"github.com/spf13/viper"
)

// Config is the root configuration object.
type Config struct {
	Server    ServerConfig    `mapstructure:"server"`
	Database  DatabaseConfig  `mapstructure:"database"`
	JWT       JWTConfig       `mapstructure:"jwt"`
	MQTT      MQTTConfig      `mapstructure:"mqtt"`
	WebSocket WebSocketConfig `mapstructure:"websocket"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Port int `mapstructure:"port"`
}

// DatabaseConfig holds database connection settings.
type DatabaseConfig struct {
	Path string `mapstructure:"path"`
}

// JWTConfig holds JWT signing parameters.
type JWTConfig struct {
	Secret     string `mapstructure:"secret"`
	ExpireDays int    `mapstructure:"expire_days"`
}

// MQTTConfig holds MQTT broker connection settings (Phase 3).
type MQTTConfig struct {
	Broker   string `mapstructure:"broker"`    // e.g. "tcp://mosquitto:1883"
	ClientID string `mapstructure:"client_id"` // e.g. "home-datacenter"
	Username string `mapstructure:"username"`  // empty = anonymous
	Password string `mapstructure:"password"`  // empty = anonymous
	QoS      byte   `mapstructure:"qos"`       // default 1
}

// WebSocketConfig holds WebSocket server settings (Phase 3).
type WebSocketConfig struct {
	// Path is the HTTP path for the WebSocket upgrade endpoint.
	Path string `mapstructure:"path"` // default "/api/v1/ws"

	// HeartbeatSeconds is the interval (in seconds) between server
	// ping frames sent to clients.
	HeartbeatSeconds int `mapstructure:"heartbeat_seconds"` // default 30
}

// AppConfig is the globally accessible configuration instance,
// populated by Load(). It is safe to read after Load returns nil.
var AppConfig *Config

// Load reads the YAML config file at path, applies defaults for
// any missing fields, and populates AppConfig.
//
// Resolution order:
//  1. If path is non-empty, use it (typically set via APP_CONFIG env var).
//  2. Else if configs/config.local.yaml exists, use it (local dev override).
//  3. Else fall back to configs/config.yaml (Docker / default).
func Load(path string) error {
	if path == "" {
		// Auto-detect: prefer local override for dev, fall back to default.
		if _, err := os.Stat("configs/config.local.yaml"); err == nil {
			path = "configs/config.local.yaml"
		} else {
			path = "configs/config.yaml"
		}
	}

	v := viper.New()

	// Defaults — keep the app runnable even if a field is omitted.
	v.SetDefault("server.port", 8080)
	v.SetDefault("database.path", "/data/sqlite/app.db")
	v.SetDefault("jwt.secret", "")
	v.SetDefault("jwt.expire_days", 365)

	// Phase 3 defaults — MQTT disabled by default (empty broker).
	v.SetDefault("mqtt.broker", "tcp://mosquitto:1883")
	v.SetDefault("mqtt.client_id", "home-datacenter")
	v.SetDefault("mqtt.username", "")
	v.SetDefault("mqtt.password", "")
	v.SetDefault("mqtt.qos", 1)

	v.SetDefault("websocket.path", "/api/v1/ws")
	v.SetDefault("websocket.heartbeat_seconds", 30)

	v.SetConfigFile(path)

	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("read config %q: %w", path, err)
	}

	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return fmt.Errorf("parse config %q: %w", path, err)
	}

	AppConfig = cfg
	return nil
}
