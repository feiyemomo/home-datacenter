package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/automation"
	"home-datacenter-api/internal/camera"
	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/database"
	"home-datacenter-api/internal/device"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/handler"
	"home-datacenter-api/internal/middleware"
	"home-datacenter-api/internal/mqtt"
	"home-datacenter-api/internal/network"
	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/server"
	"home-datacenter-api/internal/service"
	"home-datacenter-api/internal/utils"
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

	// ---- Phase 1: Server Identity ----
	//
	// Each home server has a stable, unique identity generated on
	// first boot and persisted across restarts: a UUIDv4 server_id,
	// an Ed25519 keypair (public key advertised, private key never
	// leaves the process), the build version, and a capabilities
	// list. Clients use GET /api/v1/server/info to discover the
	// server before authenticating.
	sqlDB, err := database.DB.DB()
	if err != nil {
		log.Fatalf("failed to get sql.DB: %v", err)
	}
	if err := server.MigrateSchema(sqlDB); err != nil {
		log.Fatalf("failed to migrate server_identity: %v", err)
	}
	identity, err := server.LoadOrCreateIdentity(sqlDB, "", server.Version)
	if err != nil {
		log.Fatalf("failed to initialize server identity: %v", err)
	}
	log.Printf("server identity: id=%s name=%q version=%s", identity.ServerID, identity.Name, identity.Version)

	deviceRepo := repository.NewDeviceRepository(database.DB)

	// ---- Phase 3: Real-time communication ----

	// EventBus is the central pub/sub bridge between MQTT and WebSocket.
	bus := eventbus.New()

	// DeviceManager tracks online/offline state in memory and
	// persists LastSeen to the database.
	deviceMgr := device.NewManager(bus, deviceRepo)
	deviceMgr.Start()
	defer deviceMgr.Stop()

	// MQTT client connects to Mosquitto and routes messages to
	// the EventBus via the Handler.
	mqttHandler := mqtt.NewHandler(bus, deviceMgr)
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

	// ---- Services & Handlers ----
	authService := service.NewAuthService(userRepo, deviceRepo)
	userService := service.NewUserService(userRepo, deviceRepo)
	deviceService := service.NewDeviceService(deviceRepo)

	authHandler := handler.NewAuthHandler(authService)
	userHandler := handler.NewUserHandler(userService)
	deviceHandler := handler.NewDeviceHandler(deviceService, userService)

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
	systemHandler := handler.NewSystemHandler(mqttClient, hub, deviceMgr)

	// ---- Phase 4: Camera platformization ----
	//
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
	camONVIF := camera.NewONVIFController()
	camReg := camera.NewRegistry(database.DB, go2, frigate, box, camONVIF, cfg.Camera.WebRTCPublicBase)
	camRecorder := &camera.Recorder{
		DB:        database.DB,
		Go2:       go2,
		OutputDir: cfg.Camera.RecordingDir,
	}
	camHandler := handler.NewCameraHandler(camReg, camONVIF, camRecorder, cfg.Camera.WebRTCPublicBase, cfg.Camera.ICEServers, userService)

	// Replay every persisted camera to go2rtc so a container restart
	// doesn't drop the streams. Best-effort: log and continue.
	if err := camReg.BootReplay(context.Background()); err != nil {
		log.Printf("camera: boot replay: %v", err)
	}

	// Background loops: health probes + recording roll.
	camHealth := &camera.HealthChecker{
		Registry: camReg,
		Bus:      bus,
		Interval: time.Duration(cfg.Camera.HealthIntervalSeconds) * time.Second,
		Timeout:  time.Duration(cfg.Camera.HealthTimeoutSeconds) * time.Second,
	}
	camRecorder.HC = camHealth
	go camHealth.Run(context.Background())
	go camRecorder.Run(context.Background())

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
	r := gin.Default()

	if err := r.SetTrustedProxies(nil); err != nil {
		log.Fatalf("failed to set trusted proxies: %v", err)
	}

	// Health check (kept simple for Docker / Cloudflare probes)
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
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

	// P2P hole puncher — binds a persistent UDP socket, discovers the
	// server's public endpoint via STUN, and listens for hole-punching
	// packets from peers. Disabled when p2p_port = 0 (dev default).
	var holePuncher *network.HolePuncher
	if cfg.Network.P2PPort > 0 {
		holePuncher = network.NewHolePuncher(cfg.Network.P2PPort, stunServers)
		if err := holePuncher.Listen(); err != nil {
			log.Printf("warning: holepunch listen failed: %v — P2P disabled", err)
			holePuncher = nil
		}
	}

	netService := network.NewService(stunServers,
		time.Duration(cfg.Network.CheckIntervalSeconds)*time.Second,
		cfg.Network.DirectPort, cfg.Network.PublicIPv6, holePuncher)
	netService.StartBackground(context.Background())
	peerRegistry := network.NewPeerRegistry()
	netHandler := handler.NewNetworkHandler(netService, peerRegistry, holePuncher)

	// ---- Phase 1: Server capabilities advertisement ----
	//
	// Compute the capability list from the actually-wired subsystems
	// so a dev build without P2P (p2p_port=0) correctly omits "p2p"
	// from GET /api/v1/server/info. The list is captured once at boot;
	// a subsystem going down later (e.g. MQTT drops) does NOT remove
	// its capability — the flag means "the server supports this
	// feature", not "the feature is healthy right now". Live health
	// is surfaced via /system/status and the EventBus.
	status := netService.Status()
	serverCaps := server.AllCapabilities(
		status.IPv6.Reachable,     // ipv6
		holePuncher != nil,        // p2p
		true,                      // relay (Cloudflare Tunnel always configured)
		camReg != nil,             // camera
		cfg.Frigate.BaseURL != "", // frigate
		mqttClient.IsConnected(),  // mqtt
		true,                      // ws (always available)
	)
	serverHandler := handler.NewServerHandler(identity, serverCaps)

	// ---- Phase 2: Connection Manager ----
	//
	// Wraps the network detection service, hole puncher, and peer
	// registry behind a single Connect() API. Clients call
	// POST /api/v1/network/connect to discover the best transport
	// for their peer, and the ConnectionManager picks IPv6 Direct,
	// P2P UDP, or Relay (Cloudflare Tunnel) automatically.
	connMgr := network.NewConnectionManager(netService, holePuncher, peerRegistry, cfg.Server.RelayURL)
	netHandler.SetConnectionManager(connMgr)

	api := r.Group("/api/v1")
	{
		// Phase 1: Server identity & capability advertisement.
		// No auth — clients need to discover the server before they
		// can authenticate. The public_key is not a secret; the
		// private_key never leaves the process.
		api.GET("/server/info", serverHandler.Info)

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
			device.DELETE("/:id", deviceHandler.Delete)
			device.DELETE("/:id/purge", deviceHandler.Purge)
		}

		system := api.Group("/system")
		system.Use(middleware.JWTAuth(deviceRepo))
		{
			system.GET("/status", systemHandler.Status)
		}

		mqttGroup := api.Group("/mqtt")
		mqttGroup.Use(middleware.JWTAuth(deviceRepo))
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
			camGroup.GET(":id", camHandler.Get)
			camGroup.GET(":id/presets/discover", camHandler.ListPresets)
			camGroup.GET(":id/recordings", camHandler.ListRecordings)
			camGroup.GET(":id/recordings/:recId/file", camHandler.PlayRecording)
			// WebRTC SDP exchange. Lives in the cameras group so it
			// shares the JWT middleware (any authenticated user with
			// read access to the camera can call it). The SDP body
			// is read once in camHandler.WebRTC and forwarded once
			// to go2rtc — going through home-api avoids the
			// nginx auth_request + body-discard interaction that
			// used to make /go2rtc/api/webrtc hang for 60s.
			camGroup.POST(":id/webrtc", camHandler.WebRTC)
			// Mutating endpoints are admin-only.
			adminCam := camGroup.Group("")
			adminCam.Use(middleware.RequireAdmin(database.DB))
			{
				adminCam.POST("", camHandler.Register)
				adminCam.DELETE(":id", camHandler.Delete)
				adminCam.POST(":id/ptz", camHandler.PTZ)
				adminCam.PUT(":id/presets/:alias", camHandler.SetPreset)
				adminCam.DELETE(":id/presets/:alias", camHandler.DeletePreset)
				adminCam.POST(":id/preset/:alias", camHandler.GotoPreset)
				adminCam.PUT(":id/recording", camHandler.SetRecordingPlan)
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
		// Probe endpoint — no auth required (clients use it to test
		// IPv6 direct connectivity without a JWT).
		api.GET("/network/probe", netHandler.Probe)

		netGroup := api.Group("/network")
		netGroup.Use(middleware.JWTAuth(deviceRepo))
		{
			netGroup.GET("/status", netHandler.Status)
			netGroup.POST("/connect", netHandler.Connect)
			// P2P signaling endpoints.
			netGroup.POST("/p2p/register", netHandler.RegisterP2P)
			netGroup.DELETE("/p2p/register", netHandler.UnregisterP2P)
			netGroup.GET("/p2p/server-endpoint", netHandler.LookupServer)
			netGroup.GET("/p2p/peers/:id", netHandler.LookupPeer)
			// Admin-only: list all registered peers + P2P sessions.
			adminNet := netGroup.Group("")
			adminNet.Use(middleware.RequireAdmin(database.DB))
			{
				adminNet.GET("/p2p/peers", netHandler.ListPeers)
				adminNet.GET("/p2p/sessions", netHandler.ListSessions)
			}
		}
	}

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	log.Printf("server started on %s", addr)

	// ---- Phase 1: Publish server.online event ----
	//
	// Notify all WebSocket clients (and future consumers) that the
	// server is fully booted. The event includes the server identity
	// so clients can verify they're connected to the right server.
	// We also record the boot time for the eventual server.offline
	// uptime calculation.
	bootTime := time.Now()
	capsStr := make([]string, len(serverCaps))
	for i, c := range serverCaps {
		capsStr[i] = string(c)
	}
	bus.Publish(eventbus.Event{
		Topic:    eventbus.TopicServerOnline,
		Source:   eventbus.SourceSystem,
		Severity: eventbus.SeverityInfo,
		Payload: mustJSON(map[string]any{
			"server_id":    identity.ServerID,
			"name":         identity.Name,
			"version":      identity.Version,
			"capabilities": capsStr,
			"ts":           bootTime.Unix(),
		}),
	})

	// Publish server.offline at graceful shutdown.
	defer func() {
		bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicServerOffline,
			Source:   eventbus.SourceSystem,
			Severity: eventbus.SeverityWarn,
			Payload: mustJSON(map[string]any{
				"server_id":      identity.ServerID,
				"uptime_seconds": int64(time.Since(bootTime).Seconds()),
				"ts":             time.Now().Unix(),
			}),
		})
	}()

	// ---- Phase 1: Disk space monitor ----
	//
	// Periodically checks the recording directory free space and
	// publishes disk.warning events when it drops below 10%. Runs
	// every 5 minutes in the background.
	go func() {
		runDiskCheck(bus, cfg.Camera.RecordingDir)
	}()

	if err := r.Run(addr); err != nil {
		log.Fatalf("failed to start server: %v", err)
	}
}

// mustJSON marshals v to []byte, panicking on error. Only used for
// event payloads at startup/shutdown where a marshal error indicates
// a programming bug (non-JSON-serializable type).
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("event payload not JSON-serializable: %v", err))
	}
	return b
}

// runDiskCheck periodically checks free disk space on the given path
// and publishes disk.warning events when free space drops below the
// threshold (10%).
func runDiskCheck(bus *eventbus.Bus, path string) {
	if path == "" {
		path = "/data/recordings"
	}
	const (
		checkInterval = 5 * time.Minute
		thresholdPct  = 10 // publish disk.warning when free < 10%
	)

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	// Run one check immediately on startup.
	doDiskCheck(bus, path, thresholdPct)

	for range ticker.C {
		doDiskCheck(bus, path, thresholdPct)
	}
}

// doDiskCheck checks the filesystem at path and publishes a
// disk.warning event if free space is below thresholdPct.
func doDiskCheck(bus *eventbus.Bus, path string, thresholdPct int) {
	total, free, err := getDiskUsage(path)
	if err != nil {
		log.Printf("disk check: %v", err)
		return
	}
	if total == 0 {
		return
	}
	pctFree := int(float64(free) / float64(total) * 100)
	if pctFree < thresholdPct {
		bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicDiskWarning,
			Source:   eventbus.SourceSystem,
			Severity: eventbus.SeverityWarn,
			Payload: mustJSON(map[string]any{
				"path":          path,
				"free_bytes":    free,
				"total_bytes":   total,
				"threshold_pct": thresholdPct,
			}),
		})
		log.Printf("disk warning: %s %d%% free (%d/%d bytes)", path, pctFree, free, total)
	}
}
