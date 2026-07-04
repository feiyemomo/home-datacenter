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
	Go2RTC    Go2RTCConfig    `mapstructure:"go2rtc"`
	Camera    CameraConfig    `mapstructure:"camera"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Port int `mapstructure:"port"`

	// AllowedOrigins restricts which hostnames may open a WebSocket
	// against /api/v1/ws (CSWSH protection). Empty = allow all
	// (local dev). In production list the dashboard hostname(s),
	// e.g. ["dashboard.feiyemomo.top"].
	AllowedOrigins []string `mapstructure:"allowed_origins"`
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

// Go2RTCConfig holds the go2rtc HTTP API endpoint. The camera module
// pushes RTSP sources to it and uses it as a WebRTC/HLS origin.
type Go2RTCConfig struct {
	// BaseURL is the in-network address of the go2rtc server, e.g.
	// "http://home-go2rtc:1984". Set GO2RTC_BASE_URL env var to
	// override without editing the YAML.
	BaseURL string `mapstructure:"base_url"`
}

// CameraConfig holds the camera platform settings.
type CameraConfig struct {
	// HealthIntervalSeconds is how often the HealthChecker dials
	// each camera's RTSP port. Default 15.
	HealthIntervalSeconds int `mapstructure:"health_interval_seconds"`

	// HealthTimeoutSeconds is the per-probe TCP-dial timeout.
	// Default 3.
	HealthTimeoutSeconds int `mapstructure:"health_timeout_seconds"`
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
	v.SetDefault("server.allowed_origins", []string{})
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

	// Phase 4 defaults — camera platformization
	v.SetDefault("go2rtc.base_url", "http://home-go2rtc:1984")
	v.SetDefault("camera.health_interval_seconds", 15)
	v.SetDefault("camera.health_timeout_seconds", 3)

	// Secret material may be supplied via env var instead of the YAML
	// file. This is the preferred path for production (Docker secret /
	// .env): the value never lands in the committed config file.
	// JWT_SECRET takes precedence over the file value.
	if envSecret := os.Getenv("JWT_SECRET"); envSecret != "" {
		v.Set("jwt.secret", envSecret)
	}
	if envURL := os.Getenv("GO2RTC_BASE_URL"); envURL != "" {
		v.Set("go2rtc.base_url", envURL)
	}

	v.SetConfigFile(path)

	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("read config %q: %w", path, err)
	}

	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return fmt.Errorf("parse config %q: %w", path, err)
	}

	// Refuse to boot with an insecure JWT secret. An empty or
	// placeholder secret lets anyone forge 365-day admin tokens, which
	// is the single highest-impact risk in the system.
	if err := validateJWTSecret(cfg.JWT.Secret); err != nil {
		return err
	}

	AppConfig = cfg
	return nil
}

// insecureSecrets is the set of placeholder values that must never be
// accepted as a real JWT signing key. They match the defaults baked into
// configs/config.yaml and internal/utils/jwt.go.
var insecureSecrets = map[string]struct{}{
	"":                                 {},
	"your-secret-key":                  {},
	"change-me":                        {},
	"PLEASE_CHANGE_TO_A_LONG_RANDOM_SECRET": {},
}

const minSecretLen = 32

// validateJWTSecret rejects empty / placeholder / too-short secrets.
// It does NOT log the value.
func validateJWTSecret(secret string) error {
	if _, bad := insecureSecrets[secret]; bad {
		return fmt.Errorf(
			"jwt.secret is not set (or is a placeholder). "+
				"Generate one with `openssl rand -hex 32`, "+
				"put it in configs/config.yaml (or config.local.yaml), "+
				"or set the JWT_SECRET env var.",
		)
	}
	if len(secret) < minSecretLen {
		return fmt.Errorf(
			"jwt.secret is too short (%d chars, need >= %d). "+
				"Use `openssl rand -hex 32` to generate a strong secret.",
			len(secret), minSecretLen,
		)
	}
	return nil
}
