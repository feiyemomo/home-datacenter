package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/device"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/mqtt"
	"home-datacenter-api/internal/ops"
	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/ws"
)

func setupTestSystemHandler(t *testing.T, onePanelURL string) (*SystemHandler, *ops.HostFallbackProvider, *gorm.DB) {
	gin.SetMode(gin.TestMode)

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test sqlite: %v", err)
	}
	_ = db.AutoMigrate(&model.Device{}, &model.User{})

	bus := eventbus.New()
	deviceRepo := repository.NewDeviceRepository(db)
	deviceMgr := device.NewManager(bus, deviceRepo)

	mqttHandler := mqtt.NewHandler(bus, deviceMgr, nil)
	mqttClient := mqtt.NewClient(mqtt.Config{Broker: "tcp://127.0.0.1:1883"}, mqttHandler)

	hub := ws.NewHub(bus)
	t.Cleanup(func() {
		hub.Close()
	})

	handler := NewSystemHandler(mqttClient, hub, deviceMgr)

	tmpDir := t.TempDir()
	fallbackProvider := ops.NewHostFallbackProvider(
		filepath.Join(tmpDir, "disk"),
		filepath.Join(tmpDir, "backup-state"),
	)

	var onePanelClient *ops.OnePanelClient
	if onePanelURL != "" {
		onePanelClient = ops.NewOnePanelClient(onePanelURL, "btpanel", "admin", "password")
	}

	handler.SetOpsTelemetry(onePanelClient, fallbackProvider)
	handler.ConfigureMetrics(filepath.Join(tmpDir, "disk"), filepath.Join(tmpDir, "rec"), 400*1024*1024*1024, bus)

	return handler, fallbackProvider, db
}

// -------------------------------------------------------------
// Challenge 3: SystemHandler 1Panel Adversarial Fallback Tests
// -------------------------------------------------------------

// Test 3.1: 1Panel returns 502 Bad Gateway
func TestSystemHandler_Challenge3_502BadGatewayFallback(t *testing.T) {
	// 1Panel mock server returns 502 Bad Gateway
	onePanelSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html><body>502 Bad Gateway from Nginx reverse proxy</body></html>`))
	}))
	defer onePanelSrv.Close()

	sysHandler, _, _ := setupTestSystemHandler(t, onePanelSrv.URL)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
	c.Request = req

	// Execute Status call
	sysHandler.Status(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 despite 1Panel 502, got %d. Body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Code int `json:"code"`
		Data struct {
			Metrics struct {
				CPU struct {
					Percent float64 `json:"percent"`
					Cores   int     `json:"cores"`
				} `json:"cpu"`
				Memory struct {
					TotalBytes  uint64  `json:"total_bytes"`
					UsedPercent float64 `json:"used_percent"`
				} `json:"memory"`
			} `json:"metrics"`
		} `json:"data"`
	}

	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response JSON: %v", err)
	}

	if resp.Code != 0 {
		t.Fatalf("expected code 0, got %d", resp.Code)
	}

	// Verify that fallback CPU cores were populated (fallback CPU cores >= 1)
	if resp.Data.Metrics.CPU.Cores <= 0 {
		t.Errorf("expected fallback CPU cores > 0, got %d", resp.Data.Metrics.CPU.Cores)
	}
	t.Logf("502 Fallback Successful: CPU Cores=%d, MemoryTotal=%d",
		resp.Data.Metrics.CPU.Cores, resp.Data.Metrics.Memory.TotalBytes)
}

// Test 3.2: 1Panel returns 504 Gateway Timeout
func TestSystemHandler_Challenge3_504GatewayTimeoutFallback(t *testing.T) {
	onePanelSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = w.Write([]byte(`504 Gateway Time-out`))
	}))
	defer onePanelSrv.Close()

	sysHandler, _, _ := setupTestSystemHandler(t, onePanelSrv.URL)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
	c.Request = req

	sysHandler.Status(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 despite 1Panel 504, got %d", w.Code)
	}
	t.Logf("504 Fallback Passed with status 200")
}

// Test 3.3: 1Panel daemon crashed (Connection Refused on closed port)
func TestSystemHandler_Challenge3_ConnectionRefusedFallback(t *testing.T) {
	// Point to an unused local port
	sysHandler, _, _ := setupTestSystemHandler(t, "http://127.0.0.1:59199")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
	c.Request = req

	sysHandler.Status(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 when 1Panel daemon is dead, got %d", w.Code)
	}
	t.Logf("Dead daemon connection-refused fallback Passed with status 200")
}

// Test 3.4: 1Panel returns corrupted or truncated JSON
func TestSystemHandler_Challenge3_IncompleteJSONFallback(t *testing.T) {
	corruptedBodies := []string{
		`{"code": 200, "message": "success", "data": {`, // Truncated
		`{"code": 200, "data": null}`,                   // Null data
		`{"code": 500, "message": "panic"}`,             // Error code
		`not json at all`,                               // Garbage text
	}

	for i, badJSON := range corruptedBodies {
		t.Run(badJSON, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(badJSON))
			}))
			defer srv.Close()

			sysHandler, _, _ := setupTestSystemHandler(t, srv.URL)

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
			c.Request = req

			// Must not panic, must return 200 OK
			sysHandler.Status(c)

			if w.Code != http.StatusOK {
				t.Fatalf("case %d: expected HTTP 200, got %d", i, w.Code)
			}
		})
	}
}

// Test 3.5: Concurrent access to Status during 1Panel failure (Race safety)
func TestSystemHandler_Challenge3_ConcurrentAccessDuringFailure(t *testing.T) {
	// 1Panel is dead
	sysHandler, _, _ := setupTestSystemHandler(t, "http://127.0.0.1:59188")

	const callers = 20
	var wg sync.WaitGroup
	wg.Add(callers)

	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			req, _ := http.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
			c.Request = req

			sysHandler.Status(c)

			if w.Code != http.StatusOK {
				t.Errorf("concurrent call failed: code %d", w.Code)
			}
		}()
	}

	wg.Wait()
	t.Logf("All %d concurrent status queries during 1Panel failure succeeded without race/panic", callers)
}
