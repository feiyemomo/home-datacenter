package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

// RequireAdmin returns a Gin middleware that rejects requests from
// non-admin users. It must be installed AFTER JWTAuth (which sets
// "user_id" into the gin context).
//
// We look up the user on every call rather than encoding
// is_admin in the JWT so an admin can be demoted and the change
// takes effect on the next request — matching the same
// "immediate revocation" posture as devices.
func RequireAdmin(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := c.Get("user_id")
		if !ok {
			utils.Fail(c, http.StatusUnauthorized, "unauthenticated")
			c.Abort()
			return
		}
		uid, ok := raw.(uint)
		if !ok {
			utils.Fail(c, http.StatusUnauthorized, "invalid auth context")
			c.Abort()
			return
		}
		var u model.User
		if err := db.First(&u, uid).Error; err != nil {
			utils.Fail(c, http.StatusUnauthorized, "user not found")
			c.Abort()
			return
		}
		if !u.IsAdmin {
			utils.Fail(c, http.StatusForbidden, "admin only")
			c.Abort()
			return
		}
		c.Next()
	}
}
