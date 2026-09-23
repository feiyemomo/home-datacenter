package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/automation"
	"home-datacenter-api/internal/camera"
	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/database"
	"home-datacenter-api/internal/device"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/handler"
	logpkg "home-datacenter-api/internal/log"
	"home-datacenter-api/internal/maintenance"
	"home-datacenter-api/internal/middleware"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/mqtt"
	"home-datacenter-api/internal/network"
	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/security"
	"home-datacenter-api/internal/service"
	"home-datacenter-api/internal/utils"
	"home-datacenter-api/internal/vision"
	"home-datacenter-api/internal/ws"
)

func main() {

	// Release mode for production
	gin.SetMode(gin.ReleaseMode)

	// ---- Load configuration (Step16) ----
	// Pass empty string to enable auto-detection:
	//   1. APP_CONFIG env var (if set)
	//   2. configs/config.local.yaml (local dev override)
	//   3. configs/config.yaml (Docker / default)
	configPath := os.Getenv("APP_CONFIG")

	if err := config.Load(configPath); err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	cfg := config.AppConfig

	// Apply JWT config to utils
	utils.JWTSecret = cfg.JWT.Secret
	utils.TokenExpireDays = cfg.JWT.ExpireDays

	// ---- Database ----
	database.InitDB(cfg.Database.Path)

	userRepo := repository.NewUserRepository(database.DB)

	// Bootstrap admin user on first run
	bootstrapService := service.NewBootstrapService(userRepo)
	if err := bootstrapService.InitAdmin(); err != nil {
		log.Fatalf("failed to initialize admin: %v", err)
	}

	log.Println("sqlite initialized successfully")
	log.Println("system bootstrap completed")

	deviceRepo := repository.NewDeviceRepository(database.DB)

	// ---- Phase 3: Real-time communication ----

	// EventBus is the central pub/sub bridge between MQTT and WebSocket.
	bus := eventbus.New()

	// DeviceManager tracks online/offline state in memory and
	// persists LastSeen to the database.
	deviceMgr := device.NewManager(bus, deviceRepo)
	deviceMgr.Start()
	defer deviceMgr.Stop()

	// ---- Phase 4: Camera platformization (early init) ----
	//
	// The camera Registry must be created before the MQTT handler
	// so it can serve as a slug lookup for Frigate event translation.
	// SecretBox derives its AES-256-GCM key from the same JWT secret
	// we already trust (one root secret to rotate, not two). The
	// go2rtc client talks HTTP to Frigate's bundled go2rtc on port
	// 1984; the Frigate client talks to Frigate's REST API on port
	// 5000 for config push (AI detection / recording pipeline).
	box, err := utils.NewSecretBox(cfg.JWT.Secret)
	if err != nil {
		log.Fatalf("camera: secret box init: %v", err)
	}
	go2 := camera.NewGo2RTCClient(cfg.Go2RTC.BaseURL)
	frigate := camera.NewFrigateClient(cfg.Frigate.BaseURL, cfg.Go2RTC.BaseURL)

	// v1.8.32: honour the configured normal retention window so the
	// full config push no longer hardcodes 7 days. The recording-quota
	// monitor later restores this value after a quota-driven reduction.
	frigate.SetRetentionDays(cfg.Maintenance.RecordingRetentionDays)

	// PrefixWatcher probes the outbound IPv6 address every 5 minutes and
	// auto-detects ISP DHCPv6-PD prefix rotations. On rotation it publishes
	// an event and pushes updated go2rtc webrtc.candidates to Frigate so
	// IPv6 direct-mode WebRTC keeps working without manual intervention.
	prefixWatcher := network.NewPrefixWatcher(bus, frigate)
	prefixWatcher.Start()
	defer prefixWatcher.Stop()

	// v1.8.24: auto-detect NAS LAN IP from incoming HTTP requests.
	// When a client connects from the LAN, the Host header contains
	// the NAS's LAN IP. This callback fires on the first detection
	// (or when the IP changes) and pushes updated WebRTC candidates
	// to Frigate — no manual NAS_LAN_IP configuration needed.
	camera.GlobalLanIP.SetOnChange(func(newIP string) {
		ipv6Addr := os.Getenv("NAS_IPV6_ADDRESS")
		if d := os.Getenv("NAS_IPV6_DISABLED"); d == "true" || d == "1" || d == "yes" {
			ipv6Addr = ""
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := frigate.SetWebRTCCandidates(ctx, ipv6Addr); err != nil {
			log.Printf("main: LAN IP changed to %s but WebRTC candidates update failed: %v", newIP, err)
		} else {
			log.Printf("main: LAN IP changed to %s, WebRTC candidates updated", newIP)
		}
	})

	camONVIF := camera.NewONVIFController()
	camReg := camera.NewRegistry(database.DB, go2, frigate, box, camONVIF, cfg.Camera.WebRTCPublicBase)

	// MQTT client connects to Mosquitto and routes messages to
	// the EventBus via the Handler. The camera Registry is passed
	// as the slug lookup so Frigate events (which use ASCII slugs
	// like "front_door") can be mapped back to camera IDs.
	mqttHandler := mqtt.NewHandler(bus, deviceMgr, camReg)

	var visionClient *vision.Client
	if cfg.Vision.Enabled && cfg.Vision.BaseURL != "" {
		visionClient = vision.NewClient(cfg.Vision.BaseURL)
		mqttHandler.SetVision(visionClient, frigate)
		log.Printf("vision: AI client initialized targeting %s", cfg.Vision.BaseURL)
	}

	mqttClient := mqtt.NewClient(mqtt.Config{
		Broker:   cfg.MQTT.Broker,
		ClientID: cfg.MQTT.ClientID,
		Username: cfg.MQTT.Username,
		Password: cfg.MQTT.Password,
		QoS:      cfg.MQTT.QoS,
	}, mqttHandler)

	if err := mqttClient.Start(); err != nil {
		log.Printf("WARNING: mqtt connect failed: %v (real-time features disabled)", err)
		// Non-fatal: the app can still serve REST APIs without MQTT.
	} else {
		log.Printf("mqtt connected to %s", cfg.MQTT.Broker)
	}
	defer mqttClient.Stop()

	// WebSocket Hub subscribes to the EventBus and pushes events to
	// connected app clients.
	hub := ws.NewHub(bus)
	defer hub.Close()

	// SystemLog subscriber: persists a human-readable audit entry
	// for every device / camera / user event and re-publishes on
	// the "system.log" topic so the WS Hub fans it out to
	// dashboards. Started before the auth handler is wired so
	// the very first /auth/bind produces a log row.
	logSub := logpkg.NewSubscriber(database.DB, bus)
	logSub.Start()

	// ---- Services & Handlers ----
	authService := service.NewAuthService(userRepo, deviceRepo)
	userService := service.NewUserService(userRepo, deviceRepo)
	deviceService := service.NewDeviceService(deviceRepo)

	authHandler := handler.NewAuthHandler(authService, bus)
	userHandler := handler.NewUserHandler(userService, deviceService, deviceMgr, deviceRepo, bus)
	deviceHandler := handler.NewDeviceHandler(deviceService, userService, bus)

	// WebSocket handler. If server.allowed_origins is configured, use
	// the origin-allowlisting constructor to block cross-site WebSocket
	// hijacking (CSWSH) at the app layer; otherwise fall back to the
	// permissive constructor for local dev.
	var wsHandler *handler.WebSocketHandler
	if len(cfg.Server.AllowedOrigins) > 0 {
		wsHandler = handler.NewWebSocketHandlerWithOrigins(
			hub, deviceRepo, deviceMgr, userService,
			cfg.Server.AllowedOrigins,
		)
	} else {
		wsHandler = handler.NewWebSocketHandler(
			hub, deviceRepo, deviceMgr, userService,
		)
	}
	guardMgr := security.NewGuardManager(database.DB, bus)
	mqttHandler.SetGuardProvider(guardMgr)
	securityHandler := handler.NewSecurityHandler(guardMgr)

	systemHandler := handler.NewSystemHandler(mqttClient, hub, deviceMgr)
	systemHandler.ConfigureMetrics(cfg.Maintenance.DiskPath, cfg.Camera.RecordingDir, cfg.Maintenance.RecordingQuotaBytes, bus)
	systemLogHandler := handler.NewSystemLogHandler(database.DB)
	clientErrorHandler := handler.NewClientErrorHandler(database.DB)

	// ---- Phase 4: Camera platformization (continued) ----
	//
	// camReg was already created above (before MQTT init) so it can
	// serve as the slug lookup. The remaining camera setup continues here.
	camRecorder := &camera.Recorder{
		DB:        database.DB,
		Go2:       go2,
		OutputDir: cfg.Camera.RecordingDir,
	}
	camHandler := handler.NewCameraHandler(camReg, camONVIF, camRecorder, cfg.Camera.WebRTCPublicBase, cfg.Camera.ICEServers, userService, bus)

	// v1.8.26: start the transcode-cache cleaner. Keeps the
	// .transcode-cache directory bounded (default: keep 7 days, sweep
	// every 6 hours) so replayed camera-minutes don't accumulate
	// forever. Root is the recordings dir; the cache lives under
	// <RecordingDir>/.transcode-cache.
	camHandler.StartCacheCleaner(filepath.Join(cfg.Camera.RecordingDir, ".transcode-cache"), 7*24*time.Hour, 6*time.Hour)

	// v1.8.27: background maintenance loops — SQLite WAL checkpoint +
	// daily backup, and disk-space monitoring with SystemLog alerts.
	// These keep the box healthy on long-running deployments: the WAL
	// file stays bounded, the DB is backed up daily, and a filling
	// disk surfaces in the dashboard before Frigate silently stops
	// recording. See internal/maintenance for details.
	//
	// v1.8.28: added a recording-health monitor (detects cameras that
	// are expected to record but whose newest on-disk segment is
	// stale), a recordings-tree size monitor, a host CPU/memory
	// monitor, and daily backup of Frigate's own frigate.db.
	//
	// RecordingTargets resolves "which cameras should be recording"
	// from the registry each tick. A camera is expected to record when
	// BOTH:
	//   1. it is ONLINE (Frigate sets camera Enabled = Status !=
	//      "offline", and disables the capture/record pipeline for
	//      offline cameras — so an offline camera legitimately writes
	//      nothing), AND
	//   2. recording has not been explicitly disabled on the dashboard
	//      (Meta.recording.enabled == false).
	// With both conditions true, a stale segment means "online but
	// silently stopped recording" — the exact failure this monitor
	// exists to catch. Offline cameras are left to the existing
	// camera.offline health alert instead.
	recordingTargets := func() []maintenance.RecordingTarget {
		var out []maintenance.RecordingTarget
		for _, cam := range camReg.List() {
			if cam.StreamName == "" {
				continue
			}
			// Offline cameras are disabled in Frigate (no recording
			// pipeline) — skip; the camera.offline alert covers it.
			if cam.Status == "offline" {
				continue
			}
			// Explicit "recording off" (dashboard toggle) → skip.
			if rec, ok := cam.Meta["recording"].(map[string]any); ok {
				if en, ok := rec["enabled"].(bool); ok && !en {
					continue
				}
			}
			out = append(out, maintenance.RecordingTarget{
				Slug: camReg.FrigateSlugUnique(&cam),
				Name: cam.Name,
			})
		}
		return out
	}

	maintenance.StartAll(database.DB, bus, maintenance.Config{
		DBPath:             cfg.Maintenance.SQLiteDBPath,
		BackupDir:          cfg.Maintenance.BackupDir,
		BackupKeep:         cfg.Maintenance.BackupKeep,
		CheckpointInterval: time.Duration(cfg.Maintenance.CheckpointIntervalMinutes) * time.Minute,
		BackupInterval:     time.Duration(cfg.Maintenance.BackupIntervalHours) * time.Hour,
		FrigateDBPath:      cfg.Maintenance.FrigateDBPath,
		// v1.8.29: monitor the off-NAS (Bitiful) backup by reading the
		// state file written by the backup container. `sync` mirrors
		// the local dir, so the bucket shouldn't grow — but a failed or
		// stalled sync must surface in the dashboard.
		BackupStatePath:        cfg.Maintenance.BackupStatePath,
		BackupMonitorInterval:  time.Duration(cfg.Maintenance.BackupMonitorIntervalMinutes) * time.Minute,
		BackupStaleAfter:       time.Duration(cfg.Maintenance.BackupStaleAfterMinutes) * time.Minute,
		BackupWarnFiles:        cfg.Maintenance.BackupWarnFiles,
		BackupCritFiles:        cfg.Maintenance.BackupCritFiles,
		BackupWarnBytes:        cfg.Maintenance.BackupWarnBytes,
		BackupCritBytes:        cfg.Maintenance.BackupCritBytes,
		DiskPath:               cfg.Maintenance.DiskPath,
		DiskWarnPct:        cfg.Maintenance.DiskWarnPct,
		DiskCritPct:        cfg.Maintenance.DiskCritPct,
		DiskInterval:       time.Duration(cfg.Maintenance.DiskIntervalMinutes) * time.Minute,

		RecordingsRoot:         cfg.Maintenance.RecordingsRoot,
		RecordingStaleAfter:    time.Duration(cfg.Maintenance.RecordingStaleAfterMinutes) * time.Minute,
		RecordingCheckInterval: time.Duration(cfg.Maintenance.RecordingCheckIntervalMinutes) * time.Minute,
		RecordingTargets:       recordingTargets,
		RecordingSizeInterval:  time.Duration(cfg.Maintenance.RecordingSizeIntervalHours) * time.Hour,
		RecordingSizeWarnBytes: cfg.Maintenance.RecordingSizeWarnBytes,
		RecordingSizeCritBytes: cfg.Maintenance.RecordingSizeCritBytes,
		// v1.8.32: recording quota → auto-shorten Frigate retention.
		// When the recordings tree exceeds the quota, push a shorter
		// retain window to Frigate so old footage is dropped on the
		// next cleanup cycle; when it drops back below, restore the
		// normal window. Both pushes are best-effort (no restart), so
		// live streams stay up. The quota monitor runs on the same
		// interval as the size walk (default 1h).
		RecordingQuotaBytes: cfg.Maintenance.RecordingQuotaBytes,
		OnQuotaExceeded: func() error {
			// v1.8.34: retry to ride out the transient Frigate restart
			// that BootReplay triggers at api boot. The first size sample
			// can fire while Frigate is still reloading config, and
			// /api/config/set then returns 400 "Error parsing config".
			// Retry over a ~3min window; Frigate's restart completes in
			// ~2min, so the action usually lands on the 2nd or 3rd try.
			return pushRetentionWithRetry(cfg.Maintenance.RecordingReducedRetentionDays, frigate)
		},
		OnQuotaRecovered: func() error {
			return pushRetentionWithRetry(cfg.Maintenance.RecordingRetentionDays, frigate)
		},
		SysResourceInterval:    time.Duration(cfg.Maintenance.SysResourceIntervalMinutes) * time.Minute,
		CPUWarnPct:             cfg.Maintenance.CPUWarnPct,
		CPUCritPct:             cfg.Maintenance.CPUCritPct,
		MemWarnPct:             cfg.Maintenance.MemWarnPct,
		MemCritPct:             cfg.Maintenance.MemCritPct,
		// v1.8.28: probe the sibling services the dashboard depends
		// on. The api container is on the home-net bridge, so it
		// reaches them by container name (no host port needed).
		// NOTE (v1.8.33): use the container names (home-web /
		// home-mosquitto), NOT the compose service names (web /
		// mosquitto). compose.yaml sets `container_name`, so Docker
		// registers the container name as the DNS alias and the bare
		// service name is NOT resolvable — probing `web` / `mosquitto`
		// always failed with "bad address" and fired false "不可达"
		// alerts for every service since v1.8.28.
		ServiceProbes: []maintenance.ServiceProbe{
			{Name: "Web 前端", URL: "http://home-web/"},
			{Name: "Mosquitto", NetworkAddr: "home-mosquitto:1883"},
		},
		ServiceInterval: 30 * time.Second,
		ServiceFails:    3,
		// v1.8.33: wait 60s before the first probe so a container
		// restart's transient DNS/network race can't trip the
		// consecutive-failure threshold and fire a spurious alert.
		ServiceStartupDelay: 60 * time.Second,
	})

	// Purge any soft-deleted camera rows left over from older
	// deployments where Unregister performed a soft delete. Those
	// rows still occupy the stream_name UNIQUE index and would
	// block re-registration of the same friendly name (409 Conflict).
	// Must run BEFORE BootReplay/pushFrigateConfig so the Frigate
	// config push sees a clean DB.
	if err := camReg.CleanupSoftDeleted(); err != nil {
		log.Printf("camera: cleanup soft-deleted: %v (non-fatal)", err)
	}

	// Replay every persisted camera to go2rtc so a container restart
	// doesn't drop the streams. Best-effort: log and continue.
	if err := camReg.BootReplay(context.Background()); err != nil {
		log.Printf("camera: boot replay: %v", err)
	}

	// Background loops: health probes.
	// The go2rtc recorder loop (camRecorder.Run) is disabled because
	// go2rtc does not expose a /api/recorder endpoint (404). Recording
	// is now handled by Frigate's own record pipeline, controlled via
	// the dashboard's "启用录制" button which calls
	// Registry.SetRecordingEnabled → pushFrigateConfig.
	camHealth := &camera.HealthChecker{
		Registry: camReg,
		Bus:      bus,
		Interval: time.Duration(cfg.Camera.HealthIntervalSeconds) * time.Second,
		Timeout:  time.Duration(cfg.Camera.HealthTimeoutSeconds) * time.Second,
	}
	camRecorder.HC = camHealth
	go camHealth.Run(context.Background())
	_ = camRecorder // kept for SetPlan/listRecordings API surface

	// ---- Phase 5: Automation Engine ----
	//
	// The engine subscribes to "*" on the EventBus and evaluates every
	// enabled Rule against each event. Actions are fire-and-forget
	// (notify / mqtt / webhook). The mqtt handler is the publish
	// interface for "mqtt" actions; nil-safe if MQTT is down.
	automationEngine := automation.NewEngine(database.DB, bus, mqttHandler)
	automationEngine.Start()
	defer automationEngine.Stop()
	automationHandler := automation.NewHandler(database.DB, automationEngine, bus)

	// ---- HTTP server ----
	// v1.8.41: gin.New() + explicit middleware instead of gin.Default()
	// so we control recovery. The custom Recovery middleware captures
	// panics, writes a critical "server.panic" SystemLog row and
	// broadcasts it live, and returns a unified {code:500,data:null}
	// envelope instead of gin's empty body.
	r := gin.New()
	r.Use(gin.Logger())
	r.Use(middleware.Recovery(func(c *gin.Context, panicVal any, stack []byte) {
		entry := &model.SystemLog{
			Ts:        time.Now().Unix(),
			EventType: "server.panic",
			Level:     model.LevelCritical,
			Source:    "server",
			Message:   fmt.Sprintf("服务器内部错误: %v", panicVal),
			Payload:   string(stack),
		}
		if err := database.DB.Create(entry).Error; err != nil {
			log.Printf("recovery: persist panic log failed: %v", err)
		}
		if pj, err := json.Marshal(entry); err == nil {
			bus.Publish(eventbus.Event{
				Topic:   eventbus.TopicSystemLog,
				Source:  eventbus.SourceSystem,
				Payload: pj,
			})
		}
	}))

	// Trust Docker bridge / LAN proxy ranges so c.ClientIP() resolves
	// the real client IP from X-Forwarded-For for rate limiting.
	if err := r.SetTrustedProxies([]string{
		"172.16.0.0/12",  // Docker bridge
		"192.168.0.0/16", // LAN
		"10.0.0.0/8",     // LAN
		"127.0.0.0/8",    // localhost (nginx same container)
	}); err != nil {
		log.Fatalf("failed to set trusted proxies: %v", err)
	}

	// Global request body size limit (1MB). Defends against DoS via
	// oversized payloads that would otherwise be buffered fully before
	// any handler runs. Installed after gin.Default()'s Logger/Recovery
	// middleware so that rejected requests are still logged, and before
	// any route group so it covers every endpoint. Routes that
	// legitimately need larger bodies (e.g. file uploads) can wrap
	// c.Request.Body with a larger http.MaxBytesReader in their own
	// handler. No such routes exist today.
	r.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20) // 1MB limit
		c.Next()
	})

	// v1.8.24: auto-detect NAS LAN IP from the Host header of every
	// incoming request. When a LAN client hits http://192.168.31.235:8088/,
	// the Host header is "192.168.31.235:8088" — we extract the IP and
	// feed it to GlobalLanIP, which triggers WebRTC candidates update on
	// first detection or IP change. Zero overhead on the hot path: the
	// detector short-circuits when the IP hasn't changed.
	r.Use(func(c *gin.Context) {
		camera.GlobalLanIP.UpdateFromHost(c.Request.Host)
		c.Next()
	})

	// v1.8.41: persist every >=500 response as a "server.error" SystemLog
	// row so backend failures surface in the dashboard log pane (not just
	// container stderr). See internal/middleware/errorlog.go.
	r.Use(middleware.ErrorLogMiddleware(database.DB, bus))

	// Health check (kept simple for Docker / Cloudflare probes)
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	// v1.8.41: readiness probe — distinguishes "process alive" (/health)
	// from "ready to serve". Only the DB ping gates readiness; MQTT is
	// reported but optional (the app intentionally runs without it).
	r.GET("/health/ready", func(c *gin.Context) {
		ready := true
		checks := gin.H{}
		if sqlDB, err := database.DB.DB(); err != nil {
			ready = false
			checks["db"] = "error"
		} else if err := sqlDB.Ping(); err != nil {
			ready = false
			checks["db"] = "down"
		} else {
			checks["db"] = "ok"
		}
		if mqttClient.IsConnected() {
			checks["mqtt"] = "ok"
		} else {
			checks["mqtt"] = "disconnected"
		}
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gin.H{"status": "ready", "checks": checks})
	})

	// ---- Phase 10: Network capability detection ----
	//
	// The network service runs IPv6/STUN/NAT detection in the
	// background and caches results for the configured TTL. The P2P
	// peer registry is an in-memory signaling store for UDP hole
	// punching — the mobile app registers its STUN-discovered public
	// endpoint, then looks up the server's endpoint to start punching.
	var stunServers []network.STUNServer
	for _, s := range cfg.Network.STUNServers {
		stunServers = append(stunServers, network.STUNServer{Host: s.Host, Port: s.Port})
	}
	netService := network.NewService(stunServers,
		time.Duration(cfg.Network.CheckIntervalSeconds)*time.Second)
	netService.StartBackground(context.Background())
	peerRegistry := network.NewPeerRegistry()
	netHandler := handler.NewNetworkHandler(netService, peerRegistry, prefixWatcher)

	api := r.Group("/api/v1")
	{
		// /auth/bind is gated by an IP-based rate limiter to
		// slow down online brute-force attacks against the
		// AccessKey. The 256-bit keyspace makes offline attacks
		// infeasible, but a determined attacker can still grind
		// the live endpoint — 5 attempts, then 1 per 10s. The
		// limiter is in-process and best-effort; see
		// internal/middleware/ratelimit.go for the storage /
		// eviction semantics. The same generic "invalid
		// credentials" error is returned whether the limiter or
		// the auth check rejected the request, so a probing
		// attacker cannot distinguish throttling from failure.
		bindLimiter := middleware.NewIPLimiter(
			cfg.Auth.RateLimit.RPS,
			cfg.Auth.RateLimit.Burst,
		)
		defer bindLimiter.Stop()
		bindLimit := gin.HandlerFunc(func(c *gin.Context) { c.Next() })
		if cfg.Auth.RateLimit.Enabled != nil && *cfg.Auth.RateLimit.Enabled {
			bindLimit = middleware.RateLimitByIP(bindLimiter)
		}

		auth := api.Group("/auth")
		{
			auth.POST("/bind", bindLimit, authHandler.Bind)
			// GET /auth/verify does NOT go through JWTAuth middleware
			// — it IS the JWT validator. It exists to back nginx's
			// auth_request on /go2rtc/, gating the previously
			// unauthenticated go2rtc API + media path with a JWT
			// (see web/nginx.conf for the auth_request directive).
			// No auth is required to *call* /auth/verify — you just
			// need a valid bearer token in the Authorization header.
			auth.GET("/verify", authHandler.Verify)
			// POST /auth/logout clears the home_token cookie server-side.
			// The cookie is HttpOnly so JS cannot delete it — the
			// frontend must call this endpoint to expire it properly.
			auth.POST("/logout", authHandler.Logout)
			// POST /auth/refresh re-issues a fresh JWT for an authenticated client with updated iat
			auth.POST("/refresh", middleware.JWTAuth(deviceRepo), authHandler.Refresh)
		}

		user := api.Group("/user")
		user.Use(middleware.JWTAuth(deviceRepo))
		{
			// /me is available to any authenticated user.
			user.GET("/me", userHandler.Me)
			// /user (list/create) and /user/:id (get/update/delete)
			// are admin-only. Mounted under a sub-group that
			// stacks the RequireAdmin guard on top of the JWT
			// guard installed above.
			adminUser := user.Group("")
			adminUser.Use(middleware.RequireAdmin(database.DB))
			{
				adminUser.GET("", userHandler.List)
				adminUser.POST("", userHandler.Create)
				adminUser.GET(":id", userHandler.Get)
				adminUser.PUT(":id", userHandler.Update)
				adminUser.DELETE(":id", userHandler.Delete)
			}
		}

		device := api.Group("/device")
		device.Use(middleware.JWTAuth(deviceRepo))
		{
			device.GET("/list", deviceHandler.List)
			device.POST("", deviceHandler.Create)
			device.DELETE("/:id", deviceHandler.Delete)
			// Hard delete: permanently removes the row. Only callable on
			// already-revoked devices (see HardDelete handler). Kept on a
			// distinct path so the original DELETE /:id (revoke) stays
			// unchanged.
			device.DELETE("/:id/hard", deviceHandler.HardDelete)
			// v1.8.15: admin-only token rotation. Increments the device's
			// token_version, invalidating all existing JWT tokens. The
			// client detects "token version mismatch" on the next request
			// and re-binds silently with its access_key.
			device.POST("/:id/rotate-token", middleware.RequireAdmin(database.DB), deviceHandler.RotateToken)
		}

		system := api.Group("/system")
		system.Use(middleware.JWTAuth(deviceRepo))
		{
			system.GET("/status", systemHandler.Status)
			// Persisted audit log: device / camera / user events
			// turned into human-readable rows by the log
			// subscriber. Newest first; supports limit/offset/
			// event_type filters (see SystemLogHandler.List).
			system.GET("/logs", systemLogHandler.List)
			// v1.8.25: client-side error ingest. The web frontend
			// fire-and-forgets uncaught JS errors / failed media
			// playback here; the handler persists them as
			// "client.error" SystemLog rows so they surface in the
			// log pane (see ClientErrorHandler).
			system.POST("/client-errors", clientErrorHandler.Report)
		}
		// Admin-only system routes. DELETE /logs/:id requires admin
		// so a non-admin authenticated user can read audit logs but
		// cannot purge them.
		systemAdmin := api.Group("/system")
		systemAdmin.Use(middleware.JWTAuth(deviceRepo), middleware.RequireAdmin(database.DB))
		{
			// v1.8.14: delete a single log entry after manual
			// verification. Used by the "核查并删除" workflow
			// where the user reviews a critical offline log and
			// removes it once the issue is resolved.
			systemAdmin.DELETE("/logs/:id", systemLogHandler.Delete)
			// v1.8.21: verify (downgrade) a critical log to normal
			// level. The log stays in the audit trail but is
			// removed from the "pending" section.
			systemAdmin.PATCH("/logs/:id", systemLogHandler.Verify)
			// v1.11.0: clean-cache purges /data/recordings/.transcode-cache.
			systemAdmin.POST("/clean-cache", systemHandler.CleanCache)
		}

		// v1.11.0: Security Guard arm/disarm modes.
		securityGroup := api.Group("/security")
		securityGroup.Use(middleware.JWTAuth(deviceRepo))
		{
			securityGroup.GET("/guard", securityHandler.GetGuard)
			securityGroup.PUT("/guard", securityHandler.SetGuard)
		}

		// v1.6.11: in-app self-update endpoints. JWT-protected so
		// anonymous clients cannot enumerate or download APKs.
		// GET  /release/latest     → metadata (version, size, url)
		// GET  /release/latest/apk → stream the APK file
		//
		// The handler scans config.AppConfig.Releases.Dir for files
		// matching "app-debug-vX.Y.Z.apk" and returns the highest
		// version. Publishing a new release is just scp'ing a new
		// APK into the directory — no DB row, no restart.
		releaseHandler := handler.NewReleaseHandler(config.AppConfig.Releases.Dir, database.DB)
		releaseGroup := api.Group("/release")
		releaseGroup.Use(middleware.JWTAuth(deviceRepo))
		{
			releaseGroup.GET("/latest", releaseHandler.Latest)
			releaseGroup.GET("/latest/apk", releaseHandler.Download)
		}

		// v1.8.27: keep only the newest 5 APK releases on disk. Old
		// versions accumulated ~5GB while the in-app updater only ever
		// needs the latest. Runs once at startup (so an in-place
		// upgrade immediately reclaims space) then daily.
		go func() {
			releaseHandler.CleanupOldReleases(5)
			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for range ticker.C {
				releaseHandler.CleanupOldReleases(5)
			}
		}()

		// Weather proxy: serves cached wttr.in JSON so the Android
		// app can show current weather on the dashboard. JWT-protected
		// (auth users only — no anonymous weather queries).
		weatherHandler := handler.NewWeatherHandler()
		api.GET("/weather", middleware.JWTAuth(deviceRepo), weatherHandler.Weather)

		mqttGroup := api.Group("/mqtt")
		mqttGroup.Use(middleware.JWTAuth(deviceRepo), middleware.RequireAdmin(database.DB))
		{
			mqttGroup.POST("/publish", systemHandler.Publish)
		}

		// Phase 4: camera platformization endpoints
		camGroup := api.Group("/cameras")
		camGroup.Use(middleware.JWTAuth(deviceRepo))
		{
			// Read endpoints are available to any authenticated user.
			camGroup.GET("", camHandler.List)
			camGroup.GET("ice", camHandler.ICE)
			camGroup.GET("alerts", camHandler.ListAlerts)
			camGroup.GET("alerts/:id/snapshot", camHandler.AlertSnapshot)
			camGroup.GET("alerts/:id/thumbnail", camHandler.AlertThumbnail)
			camGroup.GET(":id", camHandler.Get)
			camGroup.GET(":id/frame", camHandler.Frame)
			// Streaming fMP4 — used by Android's ExoPlayer ProgressiveMediaSource
			// to skip HLS playlist round-trips and cut first-frame latency from
			// 5-10s to ~1-2s on cold streams.
			camGroup.GET(":id/stream.mp4", camHandler.StreamMP4)
			camGroup.GET(":id/presets/discover", camHandler.ListPresets)
			camGroup.GET(":id/recordings", camHandler.ListRecordings)
			camGroup.GET(":id/recordings/:recId/file", camHandler.PlayRecording)
			// v1.8.48: fMP4 streaming variant for the web MSE player. On a
			// cache hit it serves the cached fragmented MP4; on a miss it
			// transcodes on the fly (HEVC→H.264) and streams segments as
			// the encoder emits them, so the browser's first frame arrives
			// in ~1-2s instead of after the whole 60s encodes.
			camGroup.GET(":id/recordings/:recId/stream", camHandler.PlayRecordingStream)
			// v1.6.0: motion ranges for day-playback SeekBar overlay.
			// Replaces alerts-as-overlay-source — motion fires on any
			// pixel-diff activity, alerts only fire on AI detection.
			camGroup.GET(":id/motion-ranges", camHandler.MotionRanges)
			// WebRTC SDP exchange. Lives in the cameras group so it
			// shares the JWT middleware (any authenticated user with
			// read access to the camera can call it). The SDP body
			// is read once in camHandler.WebRTC and forwarded once
			// to go2rtc — going through home-api avoids the
			// nginx auth_request + body-discard interaction that
			// used to make /go2rtc/api/webrtc hang for 60s.
			camGroup.POST(":id/webrtc", camHandler.WebRTC)
			// Preheat: triggers go2rtc to connect to the RTSP source
			// before the first real video request, so the first
			// WebRTC/HLS/MP4 request doesn't pay the 1-10s cold-start.
			// Best-effort, non-blocking — available to any authenticated
			// user (like WebRTC) since it's a read-ish "prepare to view"
			// operation, not a config mutation.
			camGroup.POST(":id/preheat", camHandler.Preheat)
			// PTZ control (v1.7.1). Lives in camGroup (not adminCam)
			// so non-admin users with shared read access can also
			// control PTZ. The handler enforces visibility via
			// requireCanRead — same as WebRTC/preheat.
			camGroup.POST(":id/ptz", camHandler.PTZ)
			// Camera sharing (v1.7.0). Lives in camGroup (not
			// adminCam) because non-admin owners can share their
			// own cameras. ShareCamera/UnshareCamera enforce
			// owner-or-admin inside the handler via
			// requireCanManageShares; ListShares only requires
			// read access (so a shared viewer can see who else
			// has access).
			camGroup.POST(":id/shares", camHandler.ShareCamera)
			camGroup.DELETE(":id/shares/:user_id", camHandler.UnshareCamera)
			camGroup.GET(":id/shares", camHandler.ListShares)
			// Mutating endpoints are admin-only.
			adminCam := camGroup.Group("")
			adminCam.Use(middleware.RequireAdmin(database.DB))
			{
				adminCam.POST("", camHandler.Register)
				adminCam.DELETE(":id", camHandler.Delete)
				adminCam.PUT(":id/presets/:alias", camHandler.SetPreset)
				adminCam.DELETE(":id/presets/:alias", camHandler.DeletePreset)
				adminCam.POST(":id/preset/:alias", camHandler.GotoPreset)
				adminCam.PUT(":id/recording", camHandler.SetRecordingPlan)
				adminCam.PUT(":id/codec", camHandler.UpdateCodec)
				adminCam.PUT(":id/audio", camHandler.UpdateAudio)
				adminCam.DELETE(":id/recordings/:recId", camHandler.DeleteRecording)
			}
		}

		// Phase 3: WebSocket endpoint
		// Auth is handled inside the handler (query param or header).
		api.GET("/ws", wsHandler.Handle)

		// Phase 5: Automation Engine endpoints (admin-only).
		// Rules are CRUD-managed here; the engine itself runs in the
		// background and reacts to EventBus events.
		automationGroup := api.Group("/automation")
		automationGroup.Use(middleware.JWTAuth(deviceRepo), middleware.RequireAdmin(database.DB))
		{
			automationGroup.GET("/rules", automationHandler.List)
			automationGroup.POST("/rules", automationHandler.Create)
			automationGroup.GET("/rules/:id", automationHandler.Get)
			automationGroup.PUT("/rules/:id", automationHandler.Update)
			automationGroup.DELETE("/rules/:id", automationHandler.Delete)
			automationGroup.POST("/rules/:id/test", automationHandler.Test)
			// Phase 6: runtime introspection. Global metrics show
			// total event throughput + drop/error rates; per-rule
			// metrics are the operator's "is this rule healthy?"
			// pane. Cooldown is the admin escape hatch for
			// silencing a misbehaving rule without deleting it.
			automationGroup.GET("/metrics", automationHandler.Metrics)
			automationGroup.GET("/rules/:id/metrics", automationHandler.RuleMetrics)
			automationGroup.POST("/rules/:id/cooldown", automationHandler.Cooldown)
		}

		// Phase 10: Network capability detection + P2P signaling.
		// Status is available to any authenticated user (the mobile
		// app needs it to decide the connection strategy). P2P peer
		// registration is also per-user. The peer list is admin-only.
		netGroup := api.Group("/network")
		netGroup.Use(middleware.JWTAuth(deviceRepo))
		{
			netGroup.GET("/status", netHandler.Status)
			// P2P signaling endpoints.
			netGroup.POST("/p2p/register", netHandler.RegisterP2P)
			netGroup.DELETE("/p2p/register", netHandler.UnregisterP2P)
			netGroup.GET("/p2p/server-endpoint", netHandler.LookupServer)
			netGroup.GET("/p2p/peers/:id", netHandler.LookupPeer)
			// Admin-only: list all registered peers.
			adminNet := netGroup.Group("")
			adminNet.Use(middleware.RequireAdmin(database.DB))
			{
				adminNet.GET("/p2p/peers", netHandler.ListPeers)
			}
		}

		// Vision AI service (face recognition & pose/fall estimation)
		visionGroup := api.Group("/vision")
		visionGroup.Use(middleware.JWTAuth(deviceRepo))
		{
			visionHandler := vision.NewHandler(visionClient)
			visionHandler.RegisterRoutes(visionGroup)
		}
	}

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	log.Printf("server started on %s", addr)

	s := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		// v1.8.26: WriteTimeout restored to 15s. v1.8.25 had raised it
		// to 120s globally so PlayRecording's software transcode (~40s)
		// wouldn't trip the deadline before writing any bytes. But that
		// widened the slow-write window for EVERY route (a slow client
		// could hold a connection open for 2 minutes). Go's WriteTimeout
		// actually starts counting at request read (verified), so we only
		// need to extend the deadline for the one long-running route:
		// PlayRecording calls http.NewResponseController(c.Writer).
		// SetWriteDeadline at the top of the handler to get its own
		// 120s budget, while every other route stays bounded at 15s.
		// Slow-loris is still bounded by ReadTimeout / ReadHeaderTimeout,
		// and IdleTimeout reaps idle keep-alive connections.
		WriteTimeout:   15 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1MB
	}

	// v1.8.41: graceful shutdown. On SIGTERM/SIGINT we stop accepting new
	// connections, drain in-flight requests (up to 10s), stop the audit-log
	// subscriber and close the event bus so no new events fan out during
	// teardown. The per-component Stop/Close methods (deviceMgr, prefixWatcher,
	// mqttClient, hub, automationEngine, bindLimiter) then run via the deferred
	// stack when main returns, and the process exits cleanly — instead of Docker
	// SIGKILL tearing the box down mid-write.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Println("main: received shutdown signal, draining in-flight requests...")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.Shutdown(shutCtx); err != nil {
			log.Printf("main: shutdown drain error: %v", err)
		}
		logSub.Stop()
		bus.Close()
		log.Println("main: shutdown complete")
	}()

	if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("failed to start server: %v", err)
	}
}

// pushRetentionWithRetry pushes a Frigate record-retention update with
// retries (v1.8.34). The quota monitor can fire its first action while
// the Frigate container is still reloading config after the api boot
// restart (BootReplay pushes requires_restart=1), and /api/config/set
// then returns a transient 400 "Error parsing config". Retrying over a
// ~3min window rides out the restart; Frigate's own cleanup job applies
// the new window on its next run. Returns the last error if it never
// succeeds, so the quota monitor leaves the state un-derived and retries
// on a later sample.
func pushRetentionWithRetry(days int, frigate *camera.FrigateClient) error {
	// v1.8.41: unified Retry util — exponential backoff (5s→40s, jittered)
	// replaces the old fixed 30s interval. Retry on any error; the quota
	// monitor re-fires on the next sample if we never succeed.
	return utils.Retry(6, 5*time.Second, 40*time.Second, nil, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := frigate.PushRecordRetention(ctx, days)
		if err != nil {
			log.Printf("maintenance: frigate retention push (%d days) failed: %v", days, err)
		}
		return err
	})
}
