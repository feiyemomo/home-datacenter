package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
)

// setupMiddlewareChallengeDB creates an in-memory SQLite database.
func setupMiddlewareChallengeDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open in-memory sqlite db: %v", err)
	}
	if err := db.AutoMigrate(&model.Household{}, &model.User{}, &model.Device{}); err != nil {
		t.Fatalf("failed to auto-migrate: %v", err)
	}
	return db
}

// ============================================================================
// Objective 6: Internal Token Anti-Escalation & Privilege Fencing
// ============================================================================
func TestEmpiricalChallenge6_InternalTokenAntiEscalation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupMiddlewareChallengeDB(t)

	validToken := "genuine-internal-ops-token-with-minimum-32-chars-entropy"
	t.Setenv("INTERNAL_OPS_TOKEN", validToken)

	// 1. Direct RequireAdmin Guard: Internal Service Token MUST be Rejected (HTTP 403)
	// Internal daemon possessing valid token MUST NOT be permitted to access admin-only endpoints.
	t.Run("Internal Token Calling RequireAdmin Returns 403 Forbidden", func(t *testing.T) {
		r := gin.New()
		r.POST("/api/v1/admin/action", OpsAgentAuth(), RequireAdmin(db), func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "escalated_should_never_happen"})
		})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/admin/action", nil)
		req.Header.Set("X-Internal-Secret", validToken)
		req.RemoteAddr = "127.0.0.1:12345"
		r.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("CRITICAL PRIVILEGE ESCALATION VULNERABILITY: expected 403 Forbidden for internal daemon calling RequireAdmin, got %d: %s", w.Code, w.Body.String())
		}
	})

	// 2. Header Tampering & Anti-Escalation: Attempting to Inject is_admin=true
	t.Run("Header Tampering Cannot Elevate is_admin to True", func(t *testing.T) {
		r := gin.New()
		var capturedIsAdmin any
		var capturedIsInternal any
		var capturedActorType any

		r.POST("/api/v1/ops/probe", OpsAgentAuth(), func(c *gin.Context) {
			capturedIsAdmin, _ = c.Get("is_admin")
			capturedIsInternal, _ = c.Get("is_internal_service")
			capturedActorType, _ = c.Get("actor_type")
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
		})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/probe", nil)
		req.Header.Set("X-Internal-Secret", validToken)
		req.Header.Set("X-Admin", "true")
		req.Header.Set("is_admin", "true")
		req.Header.Set("X-Role", "administrator")
		req.RemoteAddr = "127.0.0.1:12345"
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}
		if capturedIsAdmin != false {
			t.Fatalf("CRITICAL ESCALATION: is_admin must be strictly false, got %v", capturedIsAdmin)
		}
		if capturedIsInternal != true {
			t.Fatalf("expected is_internal_service to be true, got %v", capturedIsInternal)
		}
		if capturedActorType != "internal_service" {
			t.Fatalf("expected actor_type to be internal_service, got %v", capturedActorType)
		}
	})

	// 3. Unauthorized Scope Calling RequireAdminOrInternalScope Returns 403
	t.Run("Unauthorized Scope Rejected with 403", func(t *testing.T) {
		r := gin.New()
		r.POST("/api/v1/ops/restricted", OpsAgentAuth(), RequireAdminOrInternalScope("ops:unauthorized_scope"), func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "should_never_execute"})
		})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/restricted", nil)
		req.Header.Set("X-Internal-Secret", validToken)
		req.RemoteAddr = "127.0.0.1:12345"
		r.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for unauthorized scope, got %d: %s", w.Code, w.Body.String())
		}
	})

	// 4. Authorized Scope Calling RequireAdminOrInternalScope Succeeds
	t.Run("Authorized Scope Succeeds with 200", func(t *testing.T) {
		r := gin.New()
		r.POST("/api/v1/ops/fsm-event", OpsAgentAuth(), RequireAdminOrInternalScope(ScopeOpsFSM), func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "allowed"})
		})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/fsm-event", nil)
		req.Header.Set("X-Internal-Secret", validToken)
		req.RemoteAddr = "127.0.0.1:12345"
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for granted scope, got %d: %s", w.Code, w.Body.String())
		}
	})
}

// ============================================================================
// Objective 5: Tenant Context Resolution & Cross-Household Boundary
// ============================================================================
func TestEmpiricalChallenge5_TenantContextResolution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupMiddlewareChallengeDB(t)

	// Seed 2 distinct households
	h1 := model.Household{ID: 1, Name: "Household A"}
	h2 := model.Household{ID: 2, Name: "Household B"}
	db.Create(&h1)
	db.Create(&h2)

	// Seed user in Household A and user in Household B
	userA := model.User{ID: 101, Name: "alice", HouseholdID: 1, IsAdmin: true}
	userB := model.User{ID: 202, Name: "bob", HouseholdID: 2, IsAdmin: true}
	db.Create(&userA)
	db.Create(&userB)

	// 1. Authenticated User A -> Context resolves household_id = 1
	t.Run("User A resolves to Household 1", func(t *testing.T) {
		r := gin.New()
		var resolvedHID uint
		r.GET("/test-tenant", func(c *gin.Context) {
			c.Set("user_id", userA.ID)
			c.Next()
		}, TenantContext(db), func(c *gin.Context) {
			resolvedHID = GetHouseholdID(c)
			c.Status(http.StatusOK)
		})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/test-tenant", nil)
		r.ServeHTTP(w, req)

		if resolvedHID != 1 {
			t.Fatalf("expected household_id 1 for User A, got %d", resolvedHID)
		}
	})

	// 2. Authenticated User B -> Context resolves household_id = 2
	t.Run("User B resolves to Household 2", func(t *testing.T) {
		r := gin.New()
		var resolvedHID uint
		r.GET("/test-tenant", func(c *gin.Context) {
			c.Set("user_id", userB.ID)
			c.Next()
		}, TenantContext(db), func(c *gin.Context) {
			resolvedHID = GetHouseholdID(c)
			c.Status(http.StatusOK)
		})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/test-tenant", nil)
		r.ServeHTTP(w, req)

		if resolvedHID != 2 {
			t.Fatalf("expected household_id 2 for User B, got %d", resolvedHID)
		}
	})

	// 3. Internal service token -> Context pinned to household_id = 1
	t.Run("Internal daemon is pinned to Household 1", func(t *testing.T) {
		r := gin.New()
		var resolvedHID uint
		r.GET("/test-tenant", func(c *gin.Context) {
			c.Set("is_internal_service", true)
			c.Next()
		}, TenantContext(db), func(c *gin.Context) {
			resolvedHID = GetHouseholdID(c)
			c.Status(http.StatusOK)
		})

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/test-tenant", nil)
		r.ServeHTTP(w, req)

		if resolvedHID != 1 {
			t.Fatalf("expected household_id 1 for internal service, got %d", resolvedHID)
		}
	})
}
