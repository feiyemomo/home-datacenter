package middleware

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
)

// TenantContext resolves the active household context for the request.
// 1. Internal service requests are pinned to household_id = 1.
// 2. Authenticated user requests resolve household_id from the database User model.
// 3. Fallbacks safely to household_id = 1 if user record is not yet bound to a household.
func TenantContext(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		if isInternal, exists := c.Get("is_internal_service"); exists && isInternal == true {
			c.Set("household_id", uint(1))
			c.Next()
			return
		}

		uid := c.GetUint("user_id")
		if uid == 0 {
			c.Next()
			return
		}

		var user model.User
		if err := db.Select("id, is_admin, household_id").First(&user, uid).Error; err == nil {
			if user.HouseholdID != 0 {
				c.Set("household_id", user.HouseholdID)
			} else {
				c.Set("household_id", uint(1))
			}
		} else {
			c.Set("household_id", uint(1))
		}

		c.Next()
	}
}

// GetHouseholdID retrieves the household ID from gin context, defaulting to 1.
func GetHouseholdID(c *gin.Context) uint {
	if hid := c.GetUint("household_id"); hid != 0 {
		return hid
	}
	return 1
}
