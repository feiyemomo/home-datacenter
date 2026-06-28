package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/utils"
)

// JWTAuth validates the Authorization: Bearer <jwt> header,
// ensures the device still exists and is not revoked, and
// stores user_id / device_id in the gin context.
//
// Revocation is enforced per-request: as soon as an admin calls
// DeviceRepository.Revoke(), subsequent requests with that
// device's JWT are rejected immediately.
func JWTAuth(
	deviceRepo *repository.DeviceRepository,
) gin.HandlerFunc {

	return func(c *gin.Context) {

		// 1. Read Authorization header
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{
				"message": "missing authorization header",
			})
			c.Abort()
			return
		}

		// 2. Expect "Bearer <token>"
		if !strings.HasPrefix(authHeader, "Bearer ") {
			c.JSON(http.StatusUnauthorized, gin.H{
				"message": "invalid authorization format",
			})
			c.Abort()
			return
		}

		tokenString := strings.TrimPrefix(authHeader, "Bearer ")

		// 3. Parse and verify the JWT
		claims, err := utils.ParseToken(tokenString)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{
				"message": "invalid token",
			})
			c.Abort()
			return
		}

		// 4. Load the device. A revoked device row is kept on purpose
		// so the revocation check below can reject it; only a truly
		// missing row means "device not found".
		device, err := deviceRepo.GetByID(claims.DeviceID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				c.JSON(http.StatusUnauthorized, gin.H{
					"message": "device not found",
				})
			} else {
				// Real DB / scan error — surface a generic 500-ish
				// auth failure rather than misleading "not found".
				c.JSON(http.StatusUnauthorized, gin.H{
					"message": "device lookup failed",
				})
			}
			c.Abort()
			return
		}

		// 5. Enforce revocation. NullTime.Valid is true when
		// revoked_at is not NULL, i.e. the device has been revoked.
		if device.RevokedAt.Valid {
			c.JSON(http.StatusUnauthorized, gin.H{
				"message": "device revoked",
			})
			c.Abort()
			return
		}

		// 6. Expose identity to downstream handlers
		c.Set("user_id", claims.UserID)
		c.Set("device_id", claims.DeviceID)

		c.Next()
	}
}
