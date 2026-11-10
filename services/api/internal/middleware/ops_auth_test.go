package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/repository"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func setupOpsAuthTestDB(t *testing.T) (*gorm.DB, *repository.DeviceRepository, *repository.UserRepository) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Device{}); err != nil {
		t.Fatalf("failed to auto migrate: %v", err)
	}
	return db, repository.NewDeviceRepository(db), repository.NewUserRepository(db)
}

func TestOpsAgentAuth_ValidTokenAndLoopbackIP(t *testing.T) {
	validToken := "valid-secret-token-must-be-at-least-32-chars-long"
	t.Setenv("INTERNAL_OPS_TOKEN", validToken)

	r := gin.New()
	var capturedIsAdmin any
	var capturedIsInternal any
	r.GET("/internal-test", OpsAgentAuth(ScopeOpsFSM), func(c *gin.Context) {
		capturedIsAdmin, _ = c.Get("is_admin")
		capturedIsInternal, _ = c.Get("is_internal_service")
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/internal-test", nil)
	req.Header.Set("X-Internal-Secret", validToken)
	req.RemoteAddr = "127.0.0.1:12345"
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}
	if capturedIsAdmin != false {
		t.Fatalf("CRITICAL SECURITY VIOLATION: is_admin must be strictly false, got %v", capturedIsAdmin)
	}
	if capturedIsInternal != true {
		t.Fatalf("expected is_internal_service to be true, got %v", capturedIsInternal)
	}
}

func TestOpsAgentAuth_PrivateNetworkIPAllowed(t *testing.T) {
	validToken := "valid-secret-token-must-be-at-least-32-chars-long"
	t.Setenv("INTERNAL_OPS_TOKEN", validToken)

	r := gin.New()
	r.GET("/internal-test", OpsAgentAuth(ScopeOpsTelemetry), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	for _, ip := range []string{"10.0.1.5:4321", "172.18.0.2:8080", "192.168.31.50:5555"} {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/internal-test", nil)
		req.Header.Set("X-Internal-Secret", validToken)
		req.RemoteAddr = ip
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200 for private IP %s, got %d: %s", ip, w.Code, w.Body.String())
		}
	}
}

func TestOpsAgentAuth_PublicIPForbidden(t *testing.T) {
	validToken := "valid-secret-token-must-be-at-least-32-chars-long"
	t.Setenv("INTERNAL_OPS_TOKEN", validToken)

	r := gin.New()
	r.GET("/internal-test", OpsAgentAuth(ScopeOpsFSM), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/internal-test", nil)
	req.Header.Set("X-Internal-Secret", validToken)
	req.RemoteAddr = "8.8.8.8:12345"
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for public IP, got %d", w.Code)
	}
}

func TestOpsAgentAuth_WeakOrMissingTokenFailSecure(t *testing.T) {
	t.Run("Missing Token Config Fails Closed", func(t *testing.T) {
		t.Setenv("INTERNAL_OPS_TOKEN", "")
		r := gin.New()
		r.GET("/test", OpsAgentAuth(), func(c *gin.Context) { c.Status(200) })

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Internal-Secret", "some-token")
		req.RemoteAddr = "127.0.0.1:1234"
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 when INTERNAL_OPS_TOKEN is empty, got %d", w.Code)
		}
	})

	t.Run("Short Token Config (< 32 chars) Fails Closed", func(t *testing.T) {
		t.Setenv("INTERNAL_OPS_TOKEN", "short-secret-only-19")
		r := gin.New()
		r.GET("/test", OpsAgentAuth(), func(c *gin.Context) { c.Status(200) })

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Internal-Secret", "short-secret-only-19")
		req.RemoteAddr = "127.0.0.1:1234"
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 when INTERNAL_OPS_TOKEN is < 32 chars, got %d", w.Code)
		}
	})

	t.Run("Mismatched Token Rejected", func(t *testing.T) {
		t.Setenv("INTERNAL_OPS_TOKEN", "super-secret-token-at-least-32-characters-long")
		r := gin.New()
		r.GET("/test", OpsAgentAuth(), func(c *gin.Context) { c.Status(200) })

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Internal-Secret", "wrong-secret-token-at-least-32-characters-long")
		req.RemoteAddr = "127.0.0.1:1234"
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for mismatched token, got %d", w.Code)
		}
	})
}

func TestOpsAgentAuth_AntiEscalationGuards(t *testing.T) {
	validToken := "valid-secret-token-must-be-at-least-32-chars-long"
	t.Setenv("INTERNAL_OPS_TOKEN", validToken)
	db, _, _ := setupOpsAuthTestDB(t)

	r := gin.New()
	// Endpoint protected by RequireAdmin (must reject internal service without admin user)
	r.POST("/admin-only", OpsAgentAuth(), RequireAdmin(db), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "admin_granted"})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/admin-only", nil)
	req.Header.Set("X-Internal-Secret", validToken)
	req.RemoteAddr = "127.0.0.1:1234"
	r.ServeHTTP(w, req)

	// Since OpsAgentAuth explicitly sets is_admin = false, RequireAdmin must return 403 Forbidden
	if w.Code != http.StatusForbidden {
		t.Fatalf("CRITICAL SECURITY VIOLATION: internal token must NOT escalate to admin; expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestJWTAuth_PurgedXInternalSecretBackdoor(t *testing.T) {
	validToken := "valid-secret-token-must-be-at-least-32-chars-long"
	t.Setenv("INTERNAL_OPS_TOKEN", validToken)
	_, devRepo, userRepo := setupOpsAuthTestDB(t)

	r := gin.New()
	r.Use(JWTAuth(devRepo, userRepo))
	r.GET("/protected", func(c *gin.Context) {
		isAdmin := c.GetBool("is_admin")
		c.JSON(http.StatusOK, gin.H{"is_admin": isAdmin})
	})

	// Send request with X-Internal-Secret to JWTAuth (which should NOT accept it anymore)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("X-Internal-Secret", validToken)
	req.RemoteAddr = "127.0.0.1:1234"
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("CRITICAL: JWTAuth must no longer accept X-Internal-Secret backdoor; expected 401 Unauthorized, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequireAdminOrInternalScope(t *testing.T) {
	validToken := "valid-secret-token-must-be-at-least-32-chars-long"
	t.Setenv("INTERNAL_OPS_TOKEN", validToken)

	r := gin.New()
	r.GET("/scope-test", OpsAgentAuth(), RequireAdminOrInternalScope(ScopeOpsAuditSync), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// 1. Internal service with granted scope succeeds
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/scope-test", nil)
	req.Header.Set("X-Internal-Secret", validToken)
	req.RemoteAddr = "127.0.0.1:1234"
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid scope, got %d: %s", w.Code, w.Body.String())
	}

	// 2. Internal service missing required scope fails 403
	r2 := gin.New()
	r2.GET("/scope-test-unauthorized", OpsAgentAuth(), RequireAdminOrInternalScope("ops:forbidden:scope"), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest(http.MethodGet, "/scope-test-unauthorized", nil)
	req2.Header.Set("X-Internal-Secret", validToken)
	req2.RemoteAddr = "127.0.0.1:1234"
	r2.ServeHTTP(w2, req2)

	if w2.Code != http.StatusForbidden {
		t.Fatalf("expected 403 with missing scope, got %d: %s", w2.Code, w2.Body.String())
	}
}
