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
// Token resolution order (mirrors auth_handler.go's Verify):
//  1. Authorization: Bearer <jwt> (the SPA's axios interceptors add
//     this on /api/ calls).
//  2. Cookie: home_token=<jwt> (the browser auto-sends this on
//     same-origin requests like <img src="/api/v1/cameras/.../thumbnail">.
//     Without this fallback, image tags and other non-JS requests
//     cannot authenticate, so thumbnails/snapshots/frame previews
//     would all 401).
//
// Revocation is enforced per-request: as soon as an admin calls
// DeviceRepository.Revoke(), subsequent requests with that
// device's JWT are rejected immediately.
//
// Security note: the cookie is SameSite=Lax (set by /auth/bind),
// so cross-site XHR/fetch is blocked by the browser — only
// top-level navigations and same-origin sub-resource requests
// (images, etc.) carry it. GET requests that go through this
// middleware are side-effect-free (thumbnail/snapshot/frame
// proxies), so the CSRF risk is minimal.
func JWTAuth(
	deviceRepo *repository.DeviceRepository,
) gin.HandlerFunc {

	return func(c *gin.Context) {

		// 1. Read Authorization header (primary path)
		tokenString := ""
		if authHeader := c.GetHeader("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
			tokenString = strings.TrimPrefix(authHeader, "Bearer ")
		}
		// 2. Fall back to the home_token cookie (same-origin
		//    sub-resource requests like <img> tags).
		if tokenString == "" {
			if cookie, err := c.Cookie("home_token"); err == nil && cookie != "" {
				tokenString = cookie
			}
		}
		if tokenString == "" {
			utils.Fail(c, http.StatusUnauthorized, "missing authorization header")
			c.Abort()
			return
		}

		// 3. Parse and verify the JWT
		claims, err := utils.ParseToken(tokenString)
		if err != nil {
			utils.Fail(c, http.StatusUnauthorized, "invalid token")
			c.Abort()
			return
		}

		// 4. Load the device. A revoked device row is kept on purpose
		// so the revocation check below can reject it; only a truly
		// missing row means "device not found".
		device, err := deviceRepo.GetByID(claims.DeviceID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				utils.Fail(c, http.StatusUnauthorized, "device not found")
			} else {
				// Real DB / scan error — surface a generic 500-ish
				// auth failure rather than misleading "not found".
				utils.Fail(c, http.StatusUnauthorized, "device lookup failed")
			}
			c.Abort()
			return
		}

		// 5. Enforce revocation. NullTime.Valid is true when
		// revoked_at is not NULL, i.e. the device has been revoked.
		if device.RevokedAt.Valid {
			utils.Fail(c, http.StatusUnauthorized, "device revoked")
			c.Abort()
			return
		}

		// 6. Expose identity to downstream handlers
		c.Set("user_id", claims.UserID)
		c.Set("device_id", claims.DeviceID)

		c.Next()
	}
}
