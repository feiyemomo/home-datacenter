package main

import (
	"fmt"
	"log"
	"os"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/database"
	"home-datacenter-api/internal/handler"
	"home-datacenter-api/internal/middleware"
	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/service"
	"home-datacenter-api/internal/utils"
)

func main() {

	// Release mode for production
	gin.SetMode(gin.ReleaseMode)

	// ---- Load configuration (Step16) ----
	// Default: configs/config.yaml (relative to CWD).
	// Override with APP_CONFIG env var (useful for Docker / tests).
	configPath := os.Getenv("APP_CONFIG")
	if configPath == "" {
		configPath = "configs/config.yaml"
	}

	if err := config.Load(configPath); err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	cfg := config.AppConfig

	// Apply JWT config to utils (kept as package vars to avoid
	// changing every GenerateToken/ParseToken call site).
	utils.JWTSecret = cfg.JWT.Secret
	utils.TokenExpireDays = cfg.JWT.ExpireDays

	// ---- Database ----
	database.InitDB(cfg.Database.Path)

	userRepo := repository.NewUserRepository(
		database.DB,
	)

	// Bootstrap admin user on first run
	bootstrapService := service.NewBootstrapService(
		userRepo,
	)

	if err := bootstrapService.InitAdmin(); err != nil {
		log.Fatalf(
			"failed to initialize admin: %v",
			err,
		)
	}

	log.Println("sqlite initialized successfully")
	log.Println("system bootstrap completed")

	deviceRepo := repository.NewDeviceRepository(
		database.DB,
	)

	authService := service.NewAuthService(
		userRepo,
		deviceRepo,
	)

	userService := service.NewUserService(
		userRepo,
	)

	authHandler := handler.NewAuthHandler(
		authService,
	)

	userHandler := handler.NewUserHandler(
		userService,
	)

	deviceService := service.NewDeviceService(
		deviceRepo,
	)

	deviceHandler := handler.NewDeviceHandler(
		deviceService,
		userService,
	)

	// ---- HTTP server ----
	r := gin.Default()

	// Cloudflare Tunnel: do not trust any proxy by default
	if err := r.SetTrustedProxies(nil); err != nil {
		log.Fatalf(
			"failed to set trusted proxies: %v",
			err,
		)
	}

	// =========================
	// Health Check
	// (intentionally NOT using the unified response envelope
	//  so Docker / Cloudflare probes keep working)
	// =========================
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status": "ok",
		})
	})

	api := r.Group("/api/v1")
	{
		auth := api.Group("/auth")
		{
			auth.POST("/bind", authHandler.Bind)
		}

		user := api.Group("/user")
		user.Use(
			middleware.JWTAuth(deviceRepo),
		)
		{
			user.GET("/me", userHandler.Me)
		}

		// Device management (Step14)
		//   GET    /api/v1/device/list  - list devices
		//   DELETE /api/v1/device/:id   - revoke a device
		device := api.Group("/device")
		device.Use(
			middleware.JWTAuth(deviceRepo),
		)
		{
			device.GET("/list", deviceHandler.List)
			device.DELETE("/:id", deviceHandler.Delete)
		}
	}

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	log.Printf("server started on %s", addr)

	if err := r.Run(addr); err != nil {
		log.Fatalf(
			"failed to start server: %v",
			err,
		)
	}
}
