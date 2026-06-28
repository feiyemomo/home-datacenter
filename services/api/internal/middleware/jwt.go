package middleware

import (
    "net/http"
    "strings"

    "github.com/gin-gonic/gin"

    "home-datacenter-api/internal/repository"
    "home-datacenter-api/internal/utils"
)

func JWTAuth(
    deviceRepo *repository.DeviceRepository,
) gin.HandlerFunc {

    return func(c *gin.Context) {

        authHeader := c.GetHeader("Authorization")

        if authHeader == "" {
            c.JSON(http.StatusUnauthorized, gin.H{
                "message": "missing authorization header",
            })
            c.Abort()
            return
        }

        if !strings.HasPrefix(authHeader, "Bearer ") {
            c.JSON(http.StatusUnauthorized, gin.H{
                "message": "invalid authorization format",
            })
            c.Abort()
            return
        }

        tokenString := strings.TrimPrefix(
            authHeader,
            "Bearer ",
        )

        claims, err := utils.ParseToken(tokenString)
        if err != nil {
            c.JSON(http.StatusUnauthorized, gin.H{
                "message": "invalid token",
            })
            c.Abort()
            return
        }

        // 检查设备是否存在
        device, err := deviceRepo.GetByID(
            claims.DeviceID,
        )

        if err != nil {
            c.JSON(http.StatusUnauthorized, gin.H{
                "message": "device not found",
            })
            c.Abort()
            return
        }

        // 检查设备是否已吊销
        if device.RevokedAt != nil {
            c.JSON(http.StatusUnauthorized, gin.H{
                "message": "device revoked",
            })
            c.Abort()
            return
        }

        // 写入上下文
        c.Set("user_id", claims.UserID)
        c.Set("device_id", claims.DeviceID)

        c.Next()
    }
}