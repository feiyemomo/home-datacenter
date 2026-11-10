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
// "user_id" and "is_admin" into the gin context).
//
// If "is_admin" is already set in the context by JWTAuth, it uses it
// directly to save a redundant DB query. Otherwise, it queries the DB
// to check the latest admin status.
func RequireAdmin(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		if isInternal, exists := c.Get("is_internal_service"); exists && isInternal.(bool) {
			utils.Fail(c, http.StatusForbidden, "permission denied: internal daemon cannot approve proposals")
			c.Abort()
			return
		}

		if rawAdmin, exists := c.Get("is_admin"); exists {
			if isAdmin, ok := rawAdmin.(bool); ok {
				if !isAdmin {
					utils.Fail(c, http.StatusForbidden, "admin only")
					c.Abort()
					return
				}
				c.Next()
				return
			}
		}

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
			utils.FailWithCode(c, http.StatusUnauthorized, utils.ErrAuthUserNotFound, "user not found")
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
