package ops

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

func setupAdversarialOpsDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite in-memory db: %v", err)
	}
	_ = db.AutoMigrate(&model.SystemLog{})
	return db
}

// TestOnePanelAdversarial_502BadGateway verifies that when 1Panel daemon returns 502 Bad Gateway,
// OpsHandler smoothly falls back to HostFallbackProvider without returning error to client.
func TestOnePanelAdversarial_502BadGateway(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupAdversarialOpsDB(t)
	bus := eventbus.New()

	// Mock server that returns 502 Bad Gateway
	mock502Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("502 Bad Gateway"))
	}))
	defer mock502Server.Close()

	client := NewOnePanelClient(mock502Server.URL, "btpanel", "user", "pass")
	fallback := NewHostFallbackProvider(t.TempDir(), "")
	handler := NewOpsHandler(client, fallback, db, bus, nil)

	r := gin.New()
	handler.RegisterRoutes(r.Group("/api/v1/ops"))

	// 1. GET /api/v1/ops/nas/status
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ops/nas/status", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 via fallback on 502, got %d", w.Code)
	}

	var resp struct {
		Code int       `json:"code"`
		Data NASStatus `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}
	if resp.Data.Source != "host_fallback" {
		t.Errorf("expected source 'host_fallback', got '%s'", resp.Data.Source)
	}
	if resp.Data.Reachable {
		t.Errorf("expected reachable false, got true")
	}

	// 2. GET /api/v1/ops/nas/containers
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/ops/nas/containers", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 on containers query on 502, got %d", w2.Code)
	}
}

// TestOnePanelAdversarial_504GatewayTimeout verifies fallback on 504 Gateway Timeout.
func TestOnePanelAdversarial_504GatewayTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupAdversarialOpsDB(t)
	bus := eventbus.New()

	mock504Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
		w.Write([]byte("504 Gateway Timeout"))
	}))
	defer mock504Server.Close()

	client := NewOnePanelClient(mock504Server.URL, "btpanel", "user", "pass")
	fallback := NewHostFallbackProvider(t.TempDir(), "")
	handler := NewOpsHandler(client, fallback, db, bus, nil)

	r := gin.New()
	handler.RegisterRoutes(r.Group("/api/v1/ops"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/ops/nas/status", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 via fallback on 504, got %d", w.Code)
	}
}

// TestOnePanelAdversarial_DaemonCrashConnectionRefused verifies zero panic when daemon is completely down.
func TestOnePanelAdversarial_DaemonCrashConnectionRefused(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupAdversarialOpsDB(t)
	bus := eventbus.New()

	// Port 59123 is closed, simulating crashed 1Panel process
	client := NewOnePanelClient("http://127.0.0.1:59123/btpanel", "btpanel", "user", "pass")
	fallback := NewHostFallbackProvider(t.TempDir(), "")
	handler := NewOpsHandler(client, fallback, db, bus, nil)

	r := gin.New()
	handler.RegisterRoutes(r.Group("/api/v1/ops"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/ops/nas/status", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 via fallback when daemon crashed, got %d", w.Code)
	}

	reqSummary := httptest.NewRequest(http.MethodGet, "/api/v1/ops/nas/security-summary", nil)
	wSummary := httptest.NewRecorder()
	r.ServeHTTP(wSummary, reqSummary)

	if wSummary.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 on security-summary when daemon crashed, got %d", wSummary.Code)
	}
}

// TestOnePanelAdversarial_RestartPermissionAndWhitelistDefense verifies RBAC and whitelist guards.
func TestOnePanelAdversarial_RestartPermissionAndWhitelistDefense(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupAdversarialOpsDB(t)
	bus := eventbus.New()

	client := NewOnePanelClient("http://127.0.0.1:59123/btpanel", "btpanel", "user", "pass")
	fallback := NewHostFallbackProvider(t.TempDir(), "")
	handler := NewOpsHandler(client, fallback, db, bus, nil)

	// 1. Non-admin user attempt -> 403 Forbidden
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("is_admin", false)
		c.Next()
	})
	handler.RegisterRoutes(r.Group("/api/v1/ops"))

	reqNonAdmin := httptest.NewRequest(http.MethodPost, "/api/v1/ops/nas/containers/home-api/restart", nil)
	wNonAdmin := httptest.NewRecorder()
	r.ServeHTTP(wNonAdmin, reqNonAdmin)

	if wNonAdmin.Code != http.StatusForbidden {
		t.Fatalf("expected HTTP 403 for non-admin container restart, got %d", wNonAdmin.Code)
	}

	// 2. Admin user attempting to restart non-whitelisted dangerous container -> 400 Bad Request
	rAdmin := gin.New()
	rAdmin.Use(func(c *gin.Context) {
		c.Set("is_admin", true)
		c.Set("user_name", "admin")
		c.Next()
	})
	handler.RegisterRoutes(rAdmin.Group("/api/v1/ops"))

	reqDangerous := httptest.NewRequest(http.MethodPost, "/api/v1/ops/nas/containers/crypto-miner-container/restart", nil)
	wDangerous := httptest.NewRecorder()
	rAdmin.ServeHTTP(wDangerous, reqDangerous)

	if wDangerous.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for non-whitelisted container, got %d", wDangerous.Code)
	}
}
