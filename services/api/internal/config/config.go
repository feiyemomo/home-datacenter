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
	"path/filepath"

	"github.com/spf13/viper"
)

// Config is the root configuration object.
type Config struct {
	Server      ServerConfig      `mapstructure:"server"`
	Database    DatabaseConfig    `mapstructure:"database"`
	JWT         JWTConfig         `mapstructure:"jwt"`
	Auth        AuthConfig        `mapstructure:"auth"`
	MQTT        MQTTConfig        `mapstructure:"mqtt"`
	WebSocket   WebSocketConfig   `mapstructure:"websocket"`
	Go2RTC      Go2RTCConfig      `mapstructure:"go2rtc"`
	Frigate     FrigateConfig     `mapstructure:"frigate"`
	Camera      CameraConfig      `mapstructure:"camera"`
	Network     NetworkConfig     `mapstructure:"network"`
	Releases    ReleasesConfig    `mapstructure:"releases"`
	Maintenance MaintenanceConfig `mapstructure:"maintenance"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Port int `mapstructure:"port"`

	// AllowedOrigins restricts which hostnames may open a WebSocket
	// against /api/v1/ws (CSWSH protection). Empty = allow all
	// (local dev). In production list the dashboard hostname(s),
	// e.g. ["dashboard.feiyemomo.top"].
	AllowedOrigins []string `mapstructure:"allowed_origins"`

	// SecureCookie toggles the Secure attribute on the home_token
	// auth cookie set by /auth/bind. Default false because the LAN
	// dashboard is served over plain HTTP (Secure cookies would be
	// silently dropped by the browser, breaking /frigate/ and
	// /go2rtc/ auth_request navigations). Set to true when the
	// dashboard is reachable only over HTTPS (e.g. production
	// Cloudflare Tunnel deployment) so the cookie is never sent
	// over an unencrypted hop.
	SecureCookie bool `mapstructure:"secure_cookie"`
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

// AuthConfig holds authentication-related tunables.
type AuthConfig struct {
	// RateLimit throttles /auth/bind per source IP. The 256-bit
	// AccessKey makes offline brute-force infeasible, but a
	// determined attacker can still mount an online attack against
	// /auth/bind. The default rps=0.1 / burst=5 (i.e. 5 quick
	// attempts, then 1 per 10s) keeps a single client honest
	// without inconveniencing a user who mistypes their key.
	RateLimit RateLimitConfig `mapstructure:"rate_limit"`
}

// RateLimitConfig holds the IP token-bucket parameters.
type RateLimitConfig struct {
	// RPS is the steady-state refill rate in events per second.
	// 0.1 = 1 attempt per 10 seconds.
	RPS float64 `mapstructure:"rps"`
	// Burst is the initial bucket size — the number of attempts
	// allowed back-to-back before throttling kicks in.
	Burst int `mapstructure:"burst"`
	// Enabled toggles the limiter. Set to false to disable
	// (e.g. when running automated integration tests that need
	// thousands of binds per second).
	Enabled *bool `mapstructure:"enabled"`
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

// Go2RTCConfig holds the go2rtc HTTP API endpoint. In the Frigate
// era this points to Frigate's bundled go2rtc on port 1984. The
// camera module pushes RTSP sources to it and uses it as a
// WebRTC/HLS origin.
type Go2RTCConfig struct {
	// BaseURL is the in-network address of the go2rtc server, e.g.
	// "http://home-frigate:1984". Set GO2RTC_BASE_URL env var to
	// override without editing the YAML.
	BaseURL string `mapstructure:"base_url"`
}

// FrigateConfig holds the Frigate REST API endpoint (port 5000).
// home-api pushes the full camera config to Frigate so its AI
// detection, recording, and snapshot features know about each camera.
type FrigateConfig struct {
	// BaseURL is the in-network address of the Frigate REST API,
	// e.g. "http://home-frigate:5000". Set FRIGATE_BASE_URL env
	// var to override without editing the YAML.
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

	// RecordingDir is the on-disk root for the recorder. Must be a
	// path that BOTH the API and go2rtc containers can reach, i.e.
	// a shared volume mounted at the same location in both.
	// Default: /data/recordings.
	RecordingDir string `mapstructure:"recording_dir"`

	// WebRTCPublicBase is the absolute URL prefix the front-end
	// should hit for WebRTC SDP exchange. Three values make sense:
	//
	//   ""             → "lan"      — the Go API rewrites the URL to
	//                                home-go2rtc:1984 inside the LAN.
	//   "<public URL>" → "tunnel"   — the go2rtc ingress hostname
	//                                (e.g. https://cam.feiyemomo.top)
	//                                is used as-is. SDP works; UDP
	//                                media does not.
	//   "<turn URL>"   → "turn"     — same as tunnel + a TURN server
	//                                injected into the ICE config.
	WebRTCPublicBase string `mapstructure:"webrtc_public_base"`

	// ICEServers is the JSON-encoded list of STUN/TURN servers
	// returned by GET /api/v1/cameras/ice. Format follows the
	// browser RTCIceServer shape: [{"urls":"stun:..."},
	// {"urls":["turn:...","turns:..."],"username":"...","credential":"..."}]
	ICEServers string `mapstructure:"ice_servers"`
}

// NetworkConfig holds the network capability detection settings
// (Phase 10: Network Layer).
type NetworkConfig struct {
	// STUNServers is the list of public STUN servers used for NAT
	// detection and P2P signaling. If empty, a built-in default list
	// (Google, Cloudflare, MiWiFi) is used.
	STUNServers []STUNServerConfig `mapstructure:"stun_servers"`

	// CheckIntervalSeconds is how often the background detection loop
	// re-runs all checks. Default 60. The cache TTL matches this
	// interval, so the API always serves fresh-enough data without
	// blocking on STUN round-trips.
	CheckIntervalSeconds int `mapstructure:"check_interval_seconds"`
}

// STUNServerConfig is a single STUN server entry in the config file.
type STUNServerConfig struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
}

// ReleasesConfig holds the directory where Android APK release
// files are stored. The release handler scans this directory for
// files matching "app-debug-vX.Y.Z.apk" and serves the highest
// version. Default: /data/releases (mapped to ./data/releases on
// the host in compose.yaml).
type ReleasesConfig struct {
	Dir string `mapstructure:"dir"`
}

// MaintenanceConfig holds the background maintenance loop settings
// (v1.8.27): SQLite WAL checkpoint / daily backup and disk-space
// monitoring. Every field has a sane default applied in Load(); the
// operator only needs to override what their deployment differs on.
type MaintenanceConfig struct {
	// SQLiteDBPath is the SQLite database file path. Used by the
	// maintenance loop for VACUUM/backup bookkeeping. Empty falls
	// back to database.path.
	SQLiteDBPath string `mapstructure:"sqlite_db_path"`
	// BackupDir is where daily SQLite snapshots are written.
	// Default: <db dir>/backups (e.g. /data/sqlite/backups).
	BackupDir string `mapstructure:"backup_dir"`
	// BackupKeep is how many daily backups to retain. Default 7.
	BackupKeep int `mapstructure:"backup_keep"`
	// CheckpointIntervalMinutes is how often the WAL is checkpointed
	// and truncated. Default 360 (every 6h).
	CheckpointIntervalMinutes int `mapstructure:"checkpoint_interval_minutes"`
	// BackupIntervalHours is how often a full DB backup is taken.
	// Default 24 (daily). 0 disables backups.
	BackupIntervalHours int `mapstructure:"backup_interval_hours"`
	// DiskPath is the directory whose filesystem is monitored for
	// free space. Default: the database directory's filesystem.
	DiskPath string `mapstructure:"disk_path"`
	// DiskWarnPct / DiskCritPct are the usage thresholds (0-100).
	// Default 80 / 90.
	DiskWarnPct uint64 `mapstructure:"disk_warn_pct"`
	DiskCritPct uint64 `mapstructure:"disk_crit_pct"`
	// DiskIntervalMinutes is how often the disk monitor samples.
	// Default 10.
	DiskIntervalMinutes int `mapstructure:"disk_interval_minutes"`

	// RecordingsRoot is Frigate's recording directory root (e.g.
	// /media/frigate/recordings). Empty disables the recording
	// health + size monitors.
	RecordingsRoot string `mapstructure:"recordings_root"`
	// RecordingStaleAfterMinutes is how old the newest recording
	// segment must be before a camera is flagged as recording-stalled.
	// Default 10.
	RecordingStaleAfterMinutes int `mapstructure:"recording_stale_after_minutes"`
	// RecordingCheckIntervalMinutes is how often the recording health
	// scan runs. Default 5.
	RecordingCheckIntervalMinutes int `mapstructure:"recording_check_interval_minutes"`
	// RecordingSizeIntervalHours is how often the recordings tree size
	// is walked. Default 1. 0 disables.
	RecordingSizeIntervalHours int `mapstructure:"recording_size_interval_hours"`
	// RecordingSizeWarnBytes / RecordingSizeCritBytes are size
	// thresholds for the recordings tree. 0 disables that level.
	RecordingSizeWarnBytes uint64 `mapstructure:"recording_size_warn_bytes"`
	RecordingSizeCritBytes uint64 `mapstructure:"recording_size_crit_bytes"`
	// RecordingQuotaBytes is the soft quota for the recordings tree
	// (v1.8.32). When the total size crosses it, the quota monitor
	// invokes OnQuotaExceeded (e.g. to shorten Frigate retention so
	// old footage is deleted on the next cleanup cycle); when it drops
	// back below, OnQuotaRecovered restores normal retention. 0 disables
	// the automatic action (the tree is still monitored/alerts only).
	RecordingQuotaBytes uint64 `mapstructure:"recording_quota_bytes"`
	// RecordingRetentionDays is the normal Frigate record retention
	// (record.continuous.days / record.motion.days) applied by the full
	// config push. Default 7.
	RecordingRetentionDays int `mapstructure:"recording_retention_days"`
	// RecordingReducedRetentionDays is the retention (days) applied while
	// the recordings tree is over quota. Must be < RecordingRetentionDays.
	// Default 3.
	RecordingReducedRetentionDays int `mapstructure:"recording_reduced_retention_days"`

	// FrigateDBPath is Frigate's own SQLite database (frigate.db),
	// backed up daily alongside app.db. Empty disables.
	FrigateDBPath string `mapstructure:"frigate_db_path"`

	// BackupStatePath is the host path of the off-NAS backup status
	// file (data/backup-state/last.json) mounted read-only into the
	// API container (v1.8.29). Empty disables the backup monitor.
	BackupStatePath string `mapstructure:"backup_state_path"`
	// BackupMonitorIntervalMinutes is how often the backup status file
	// is polled. Default 5.
	BackupMonitorIntervalMinutes int `mapstructure:"backup_monitor_interval_minutes"`
	// BackupStaleAfterMinutes is how old the last sync must be before
	// the backup is considered stalled. Default 2x sync interval.
	BackupStaleAfterMinutes int `mapstructure:"backup_stale_after_minutes"`
	// BackupWarnFiles / BackupCritFiles are bucket object-count
	// thresholds for the retention alert. 0 disables that level.
	BackupWarnFiles int64 `mapstructure:"backup_warn_files"`
	BackupCritFiles int64 `mapstructure:"backup_crit_files"`
	// BackupWarnBytes / BackupCritBytes are bucket total-size
	// thresholds for the retention alert. 0 disables that level.
	BackupWarnBytes int64 `mapstructure:"backup_warn_bytes"`
	BackupCritBytes int64 `mapstructure:"backup_crit_bytes"`

	// SysResourceIntervalMinutes is how often CPU + memory are
	// sampled. Default 5. 0 disables the resource monitor.
	SysResourceIntervalMinutes int `mapstructure:"sys_resource_interval_minutes"`
	// CPUWarnPct / CPUCritPct are CPU usage thresholds (0-100).
	// Default 80 / 95.
	CPUWarnPct uint64 `mapstructure:"cpu_warn_pct"`
	CPUCritPct uint64 `mapstructure:"cpu_crit_pct"`
	// MemWarnPct / MemCritPct are memory usage thresholds (0-100).
	// Default 80 / 90.
	MemWarnPct uint64 `mapstructure:"mem_warn_pct"`
	MemCritPct uint64 `mapstructure:"mem_crit_pct"`
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
	// SecureCookie defaults to false: the LAN dashboard is served
	// over plain HTTP, where a Secure cookie would be dropped by
	// the browser. HTTPS deployments opt in via config.
	v.SetDefault("server.secure_cookie", false)
	v.SetDefault("database.path", "/data/sqlite/app.db")
	v.SetDefault("jwt.secret", "")
	v.SetDefault("jwt.expire_days", 365)

	// Auth rate-limit defaults: 5 burst, 1 attempt per 10s.
	// These are the in-process IPLimiter parameters. See
	// internal/middleware/ratelimit.go for the implementation.
	v.SetDefault("auth.rate_limit.rps", 0.1)
	v.SetDefault("auth.rate_limit.burst", 5)
	v.SetDefault("auth.rate_limit.enabled", true)

	// Phase 3 defaults — MQTT disabled by default (empty broker).
	v.SetDefault("mqtt.broker", "tcp://mosquitto:1883")
	v.SetDefault("mqtt.client_id", "home-datacenter")
	v.SetDefault("mqtt.username", "")
	v.SetDefault("mqtt.password", "")
	v.SetDefault("mqtt.qos", 1)

	v.SetDefault("websocket.path", "/api/v1/ws")
	v.SetDefault("websocket.heartbeat_seconds", 30)

	// Phase 4 defaults — camera platformization
	v.SetDefault("go2rtc.base_url", "http://home-frigate:1984")
	v.SetDefault("frigate.base_url", "http://home-frigate:5000")
	v.SetDefault("camera.health_interval_seconds", 15)
	v.SetDefault("camera.health_timeout_seconds", 3)
	v.SetDefault("camera.recording_dir", "/data/recordings")
	v.SetDefault("camera.webrtc_public_base", "")
	v.SetDefault("camera.ice_servers", "")

	// Phase 10 defaults — network capability detection
	v.SetDefault("network.check_interval_seconds", 60)

	// v1.6.11: APK releases directory. The host bind-mount in
	// compose.yaml maps ./data/releases → /data/releases inside
	// the api container. The release handler scans this directory
	// for files named "app-debug-vX.Y.Z.apk" and serves the
	// highest version to the Android in-app updater.
	v.SetDefault("releases.dir", "/data/releases")

	// v1.8.27: background maintenance loops. Defaults keep the box
	// healthy with zero config: checkpoint the WAL every 6h, back up
	// the DB daily (keep 7), alert on disk at 80%/90%, sample every
	// 10m. The disk path and sqlite path fall back to the database
	// directory below (after the config file is read).
	v.SetDefault("maintenance.sqlite_db_path", "")
	v.SetDefault("maintenance.backup_dir", "")
	v.SetDefault("maintenance.backup_keep", 7)
	v.SetDefault("maintenance.checkpoint_interval_minutes", 360)
	v.SetDefault("maintenance.backup_interval_hours", 24)
	v.SetDefault("maintenance.disk_path", "")
	v.SetDefault("maintenance.disk_warn_pct", 80)
	v.SetDefault("maintenance.disk_crit_pct", 90)
	v.SetDefault("maintenance.disk_interval_minutes", 10)

	// v1.8.28: recording + resource monitors. RecordingsRoot is
	// left empty by default (disabled); main.go wires it to the
	// configured Camera.RecordingDir when the container path is
	// present. The other fields get sane defaults so an operator
	// only needs to set recordings_root / frigate_db_path.
	v.SetDefault("maintenance.recordings_root", "")
	v.SetDefault("maintenance.recording_stale_after_minutes", 10)
	v.SetDefault("maintenance.recording_check_interval_minutes", 5)
	v.SetDefault("maintenance.recording_size_interval_hours", 1)
	v.SetDefault("maintenance.recording_size_warn_bytes", 0)
	v.SetDefault("maintenance.recording_size_crit_bytes", 0)
	v.SetDefault("maintenance.recording_quota_bytes", 0)
	v.SetDefault("maintenance.recording_retention_days", 7)
	v.SetDefault("maintenance.recording_reduced_retention_days", 3)
	v.SetDefault("maintenance.frigate_db_path", "")
	v.SetDefault("maintenance.sys_resource_interval_minutes", 5)
	v.SetDefault("maintenance.cpu_warn_pct", 80)
	v.SetDefault("maintenance.cpu_crit_pct", 95)
	v.SetDefault("maintenance.mem_warn_pct", 80)
	v.SetDefault("maintenance.mem_crit_pct", 90)

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
	if envURL := os.Getenv("FRIGATE_BASE_URL"); envURL != "" {
		v.Set("frigate.base_url", envURL)
	}
	// MQTT credentials are injected via env vars in production
	// (see compose.yaml -> MQTT_USERNAME / MQTT_PASSWORD). Without
	// this, the API would connect anonymously and be rejected by
	// `allow_anonymous false` in mosquitto.conf (CONNACK 0x05).
	if envUser := os.Getenv("MQTT_USERNAME"); envUser != "" {
		v.Set("mqtt.username", envUser)
	}
	if envPass := os.Getenv("MQTT_PASSWORD"); envPass != "" {
		v.Set("mqtt.password", envPass)
	}

	v.SetConfigFile(path)

	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("read config %q: %w", path, err)
	}

	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return fmt.Errorf("parse config %q: %w", path, err)
	}

	// v1.8.27: resolve maintenance path fallbacks. Empty values fall
	// back to the database directory so the loops work with zero
	// config. The disk monitor targets the filesystem that holds the
	// database (which is also where recordings/cache live on the
	// shared /data volume).
	dbDir := filepath.Dir(cfg.Database.Path)
	if cfg.Maintenance.SQLiteDBPath == "" {
		cfg.Maintenance.SQLiteDBPath = cfg.Database.Path
	}
	if cfg.Maintenance.BackupDir == "" {
		cfg.Maintenance.BackupDir = filepath.Join(dbDir, "backups")
	}
	if cfg.Maintenance.DiskPath == "" {
		cfg.Maintenance.DiskPath = dbDir
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
	"":                                      {},
	"your-secret-key":                       {},
	"change-me":                             {},
	"PLEASE_CHANGE_TO_A_LONG_RANDOM_SECRET": {},
}

const minSecretLen = 32

// validateJWTSecret rejects empty / placeholder / too-short secrets.
// It does NOT log the value.
func validateJWTSecret(secret string) error {
	if _, bad := insecureSecrets[secret]; bad {
		return fmt.Errorf(
			"jwt.secret is not set (or is a placeholder). " +
				"Generate one with `openssl rand -hex 32`, " +
				"put it in configs/config.yaml (or config.local.yaml), " +
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
