package middleware

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/utils"
)

// Standard Least-Privilege Scopes for Ops Agent Daemons
const (
	ScopeOpsFSM             = "ops:fsm"              // Report container crash and lifecycle events
	ScopeOpsProposalRead    = "ops:proposal:read"    // Query system proposals and status
	ScopeOpsProposalCreate  = "ops:proposal:create"  // Submit automated diagnostic proposals for human review
	ScopeOpsProposalExecute = "ops:proposal:execute" // Execute already-approved proposals (low-peak / manual trigger)
	ScopeOpsAuditSync       = "ops:audit:sync"       // Synchronize buffered offline recovery audit logs
	ScopeOpsTelemetry       = "ops:telemetry"        // Report telemetry metrics and health status
)

// OpsAgentAuth returns a Gin middleware specifically authenticating internal Ops Agent daemons.
// It enforces:
// 1. Independent INTERNAL_OPS_TOKEN (strictly forbids falling back to JWT_SECRET).
// 2. Minimum 32-character secret length (Fail-Closed).
// 3. Network boundary: source IP must be private/loopback.
// 4. Strict privilege isolation: NEVER sets is_admin = true (strictly false).
// 5. Scope-based authorization verification.
func OpsAgentAuth(requiredScopes ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		internalHeader := c.GetHeader("X-Internal-Secret")
		if internalHeader == "" {
			if auth := c.GetHeader("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				internalHeader = strings.TrimPrefix(auth, "Bearer ")
			}
		}

		if internalHeader == "" {
			utils.FailWithCode(c, http.StatusUnauthorized, utils.ErrAuthMissing, "missing internal service token")
			c.Abort()
			return
		}

		internalSecret := os.Getenv("INTERNAL_OPS_TOKEN")
		// Fail-Secure: INTERNAL_OPS_TOKEN must be explicitly set and >= 32 characters
		if internalSecret == "" || len(internalSecret) < 32 {
			utils.FailWithCode(c, http.StatusUnauthorized, utils.ErrAuthTokenInvalid, "INTERNAL_OPS_TOKEN not properly configured on server")
			c.Abort()
			return
		}

		if subtle.ConstantTimeCompare([]byte(internalHeader), []byte(internalSecret)) != 1 {
			utils.FailWithCode(c, http.StatusUnauthorized, utils.ErrAuthTokenInvalid, "invalid internal service token")
			c.Abort()
			return
		}

		// Network boundary check: must originate from loopback or internal private bridge
		clientIP := c.ClientIP()
		if !isPrivateOrLoopbackIP(clientIP) {
			utils.Fail(c, http.StatusForbidden, "internal service requests from public IPs are forbidden")
			c.Abort()
			return
		}

		// Granted scopes for home-ops-agent daemon
		grantedScopes := map[string]bool{
			ScopeOpsFSM:             true,
			ScopeOpsProposalRead:    true,
			ScopeOpsProposalCreate:  true,
			ScopeOpsProposalExecute: true,
			ScopeOpsAuditSync:       true,
			ScopeOpsTelemetry:       true,
		}

		// Verify required scopes
		for _, reqScope := range requiredScopes {
			if !grantedScopes[reqScope] {
				utils.Fail(c, http.StatusForbidden, "insufficient scope for internal service")
				c.Abort()
				return
			}
		}

		// Inject explicit internal service identity (NO ADMIN ESCALATION)
		c.Set("user_id", uint(0))
		c.Set("user_name", "ops-agent")
		c.Set("actor_type", "internal_service")
		c.Set("is_admin", false) // STRICTLY FALSE: never escalate internal token to admin!
		c.Set("is_internal_service", true)
		c.Set("household_id", uint(1)) // Internal daemon is pinned to primary household
		c.Set("scopes", grantedScopes)

		c.Next()
	}
}

// RequireAdminOrInternalScope allows access if the caller is an Admin User OR
// an internal service holding the specific required scope.
func RequireAdminOrInternalScope(requiredScope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Check if unauthenticated (neither user_id nor is_internal_service in context)
		_, hasUser := c.Get("user_id")
		isInternal, hasInternal := c.Get("is_internal_service")
		if !hasUser && (!hasInternal || !isInternal.(bool)) {
			utils.Fail(c, http.StatusUnauthorized, "unauthenticated")
			c.Abort()
			return
		}

		// 1. Check if authenticated as admin user
		if isAdmin, exists := c.Get("is_admin"); exists && isAdmin.(bool) {
			c.Next()
			return
		}

		// 2. Check if internal service with required scope
		if hasInternal && isInternal.(bool) {
			if scopesRaw, ok := c.Get("scopes"); ok {
				if scopesMap, ok := scopesRaw.(map[string]bool); ok && scopesMap[requiredScope] {
					c.Next()
					return
				}
			}
		}

		utils.Fail(c, http.StatusForbidden, "permission denied: admin role or internal scope required")
		c.Abort()
	}
}

// RequireAuthOrInternalScope allows access if the caller is an Authenticated User (admin or member)
// OR an internal service holding the specific required scope.
func RequireAuthOrInternalScope(requiredScope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. Authenticated user (admin or standard member)
		if uid, exists := c.Get("user_id"); exists {
			if id, ok := uid.(uint); ok && id > 0 {
				c.Next()
				return
			}
		}

		// 2. Internal service with required scope
		if isInternal, exists := c.Get("is_internal_service"); exists && isInternal.(bool) {
			if scopesRaw, ok := c.Get("scopes"); ok {
				if scopesMap, ok := scopesRaw.(map[string]bool); ok && scopesMap[requiredScope] {
					c.Next()
					return
				}
			}
			utils.Fail(c, http.StatusForbidden, "permission denied: internal scope required")
			c.Abort()
			return
		}

		utils.Fail(c, http.StatusUnauthorized, "unauthenticated")
		c.Abort()
	}
}

// OpsFlexibleAuth provides unified gateway authentication for Ops router group.
// Requests with X-Internal-Secret are authenticated via OpsAgentAuth;
// Standard browser and API requests are authenticated via JWTAuth.
func OpsFlexibleAuth(deviceRepo *repository.DeviceRepository, userRepo *repository.UserRepository) gin.HandlerFunc {
	jwtAuth := JWTAuth(deviceRepo, userRepo)
	opsAuth := OpsAgentAuth()

	return func(c *gin.Context) {
		if c.GetHeader("X-Internal-Secret") != "" {
			opsAuth(c)
			return
		}
		jwtAuth(c)
	}
}
