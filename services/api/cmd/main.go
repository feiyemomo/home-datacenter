package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/camera"
	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/database"
	"home-datacenter-api/internal/device"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/handler"
	"home-datacenter-api/internal/middleware"
	"home-datacenter-api/internal/mqtt"
	"home-datacenter-api/internal/repository"
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
	userService := service.NewUserService(userRepo)
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
	// go2rtc client talks HTTP to home-go2rtc:1984 on the internal
	// home-net network.
	box, err := utils.NewSecretBox(cfg.JWT.Secret)
	if err != nil {
		log.Fatalf("camera: secret box init: %v", err)
	}
	go2 := camera.NewGo2RTCClient(cfg.Go2RTC.BaseURL)
	camReg := camera.NewRegistry(database.DB, go2, box)
	camONVIF := camera.NewONVIFController()
	camHandler := handler.NewCameraHandler(camReg, camONVIF)

	// Replay every persisted camera to go2rtc so a container restart
	// doesn't drop the streams. Best-effort: log and continue.
	if err := camReg.BootReplay(context.Background()); err != nil {
		log.Printf("camera: boot replay: %v", err)
	}

	// Background health probe loop.
	camHealth := &camera.HealthChecker{
		Registry: camReg,
		Bus:      bus,
		Interval: time.Duration(cfg.Camera.HealthIntervalSeconds) * time.Second,
		Timeout:  time.Duration(cfg.Camera.HealthTimeoutSeconds) * time.Second,
	}
	go camHealth.Run(context.Background())

	// ---- HTTP server ----
	r := gin.Default()

	if err := r.SetTrustedProxies(nil); err != nil {
		log.Fatalf("failed to set trusted proxies: %v", err)
	}

	// Health check (kept simple for Docker / Cloudflare probes)
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	api := r.Group("/api/v1")
	{
		auth := api.Group("/auth")
		{
			auth.POST("/bind", authHandler.Bind)
		}

		user := api.Group("/user")
		user.Use(middleware.JWTAuth(deviceRepo))
		{
			user.GET("/me", userHandler.Me)
		}

		device := api.Group("/device")
		device.Use(middleware.JWTAuth(deviceRepo))
		{
			device.GET("/list", deviceHandler.List)
			device.DELETE("/:id", deviceHandler.Delete)
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
			camGroup.GET(":id", camHandler.Get)
			// Mutating endpoints are admin-only.
			adminCam := camGroup.Group("")
			adminCam.Use(middleware.RequireAdmin(database.DB))
			{
				adminCam.POST("", camHandler.Register)
				adminCam.DELETE(":id", camHandler.Delete)
				adminCam.POST(":id/ptz", camHandler.PTZ)
			}
		}

		// Phase 3: WebSocket endpoint
		// Auth is handled inside the handler (query param or header).
		api.GET("/ws", wsHandler.Handle)
	}

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	log.Printf("server started on %s", addr)

	if err := r.Run(addr); err != nil {
		log.Fatalf("failed to start server: %v", err)
	}
}
