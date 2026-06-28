package main

import (
    "log"

    "github.com/gin-gonic/gin"

    "home-datacenter-api/internal/handler"
    "home-datacenter-api/internal/database"
    "home-datacenter-api/internal/repository"
    "home-datacenter-api/internal/service"
    "home-datacenter-api/internal/middleware"
)

func main() {

    // 生产模式
    gin.SetMode(gin.ReleaseMode)

    // 初始化数据库
    database.InitDB("/data/sqlite/app.db")

    // 初始化 Repository
    userRepo := repository.NewUserRepository(
        database.DB,
    )

    // 初始化系统管理员
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


    // 创建 Gin 实例
    r := gin.Default()

    // Cloudflare Tunnel 场景
    if err := r.SetTrustedProxies(nil); err != nil {
        log.Fatalf(
            "failed to set trusted proxies: %v",
            err,
        )
    }

    // =========================
    // Health Check
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
    }

    log.Println("server started on :8080")

    // 启动服务
    if err := r.Run(":8080"); err != nil {
        log.Fatalf(
            "failed to start server: %v",
            err,
        )
    }
}