package ops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// TestRound3_P2_CanonicalComponentIDs tests canonical ID normalization across Go and Python (R4).
func TestRound3_P2_CanonicalComponentIDs(t *testing.T) {
	testCases := []struct {
		input    string
		expected string
	}{
		// Frigate
		{"home-frigate", "frigate"},
		{"frigate", "frigate"},
		{"nvr", "frigate"},
		{"go2rtc", "frigate"},
		{"/home-frigate", "frigate"},
		{"home-frigate (NVR)", "frigate"},

		// Mosquitto
		{"home-mosquitto", "mosquitto"},
		{"mosquitto", "mosquitto"},
		{"mqtt", "mosquitto"},
		{"/home-mosquitto", "mosquitto"},
		{"home-mosquitto (MQTT)", "mosquitto"},

		// Home API
		{"home-api", "home-api"},
		{"api", "home-api"},
		{"backend", "home-api"},
		{"go-api", "home-api"},
		{"/home-api", "home-api"},
		{"home-api (Go API)", "home-api"},

		// Home Web
		{"home-web", "home-web"},
		{"web", "home-web"},
		{"frontend", "home-web"},
		{"nginx", "home-web"},
		{"/home-web", "home-web"},
		{"home-web (Nginx)", "home-web"},

		// Home Vision
		{"home-vision", "home-vision"},
		{"vision", "home-vision"},
		{"ai-vision", "home-vision"},
		{"/home-vision", "home-vision"},

		// OpenResty
		{"1Panel-openresty-WuDU", "openresty"},
		{"1panel-openresty-wudu", "openresty"},
		{"1panel-openresty", "openresty"},
		{"openresty", "openresty"},
		{"nginx-proxy", "openresty"},
		{"1pwaf", "openresty"},
		{"waf", "openresty"},
		{"openresty-waf", "openresty"},

		// Database & Others
		{"app.db", "app.db"},
		{"database", "app.db"},
		{"sqlite", "app.db"},
		{"db", "app.db"},
		{"home-cloudflared", "home-cloudflared"},
		{"cloudflared", "home-cloudflared"},
		{"home-h3c-keepalive", "home-h3c-keepalive"},
		{"home-ops-agent", "home-ops-agent"},

		// Custom unmapped must fail-closed to "" (R4-17)
		{"custom-service", ""},
	}

	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			actual := ResolveTopologyComponent(tc.input)
			if actual != tc.expected {
				t.Errorf("ResolveTopologyComponent(%q) = %q; want %q", tc.input, actual, tc.expected)
			}
		})
	}
}

// TestRound3_P2_DualPathLeaseManager tests persistent recovery lease acquisition and mutual exclusion (R13).
func TestRound3_P2_DualPathLeaseManager(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "lease_test.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	// 1. Nil DB error check
	var nilMgr *RecoveryLeaseManager
	if _, err := nilMgr.AcquireLease("frigate", "go-core", time.Second); !errors.Is(err, ErrLeaseDBRequired) {
		t.Errorf("expected ErrLeaseDBRequired, got %v", err)
	}

	mgr := NewRecoveryLeaseManager(db)

	// 2. Basic Acquire and Release
	token1, err := mgr.AcquireLease("home-frigate", "go-core", 2*time.Second)
	if err != nil {
		t.Fatalf("failed to acquire lease: %v", err)
	}
	if token1 == "" {
		t.Fatal("expected non-empty token")
	}

	active, holder, err := mgr.IsLeaseActive("frigate")
	if err != nil || !active || holder != "go-core" {
		t.Fatalf("expected lease active for go-core, got active=%v holder=%s err=%v", active, holder, err)
	}

	// 3. Mutual Exclusion: Second holder (python-offline) cannot acquire active lease
	_, err = mgr.AcquireLease("frigate", "python-offline", 2*time.Second)
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("expected ErrLeaseHeld for concurrent holder, got %v", err)
	}

	// 4. Same-holder AcquireLease re-entry is strictly forbidden (R4-04); must use RenewLease
	_, err = mgr.AcquireLease("frigate", "go-core", 2*time.Second)
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("expected ErrLeaseHeld for same-holder re-entry via AcquireLease, got: %v", err)
	}
	if _, err := mgr.RenewLease("frigate", token1, 2*time.Second); err != nil {
		t.Fatalf("expected RenewLease with valid token to succeed, got: %v", err)
	}
	token2 := token1

	// 5. Release lease with invalid token fails
	if err := mgr.ReleaseLease("frigate", "invalid-token"); !errors.Is(err, ErrInvalidLeaseToken) {
		t.Fatalf("expected ErrInvalidLeaseToken on bad token release, got %v", err)
	}

	// 6. Release lease with valid token succeeds
	if err := mgr.ReleaseLease("frigate", token2); err != nil {
		t.Fatalf("expected clean release, got: %v", err)
	}
	active, _, _ = mgr.IsLeaseActive("frigate")
	if active {
		t.Fatal("expected lease to be inactive after release")
	}

	// 7. Lease Expiration: After TTL, another holder can acquire
	shortToken, err := mgr.AcquireLease("mosquitto", "go-core", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("failed to acquire short lease: %v", err)
	}
	_ = shortToken
	time.Sleep(80 * time.Millisecond) // Wait for lease to expire

	pyToken, err := mgr.AcquireLease("mosquitto", "python-offline", 1*time.Second)
	if err != nil {
		t.Fatalf("expected python-offline to acquire expired lease, got %v", err)
	}
	if pyToken == "" {
		t.Fatal("expected valid token after expiration acquisition")
	}
	_ = mgr.ReleaseLease("mosquitto", pyToken)

	// 8. High-Concurrency Race: 20 goroutines competing for the same lease
	var winners int32
	var conflicts int32
	var wg sync.WaitGroup
	goroutines := 20
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		h := fmt.Sprintf("worker-%d", i)
		go func(holderName string) {
			defer wg.Done()
			tok, aErr := mgr.AcquireLease("home-web", holderName, 5*time.Second)
			if aErr == nil && tok != "" {
				atomic.AddInt32(&winners, 1)
			} else if errors.Is(aErr, ErrLeaseHeld) {
				atomic.AddInt32(&conflicts, 1)
			}
		}(h)
	}
	wg.Wait()

	if winners != 1 {
		t.Fatalf("expected exactly 1 winner in lease race, got %d (conflicts=%d)", winners, conflicts)
	}
	if conflicts != int32(goroutines-1) {
		t.Fatalf("expected %d conflicts, got %d", goroutines-1, conflicts)
	}
}

// TestRound3_P2_TrustedProxies_SpoofingDefense tests perimeter IP spoofing defense (R7.1).
func TestRound3_P2_TrustedProxies_SpoofingDefense(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Controlled trusted proxies: localhost and specific compose reverse proxy gateway (NOT broad 172.16.0.0/12)
	controlledProxies := []string{
		"127.0.0.1/32",
		"::1/128",
		"172.18.0.0/16",
	}

	r := gin.New()
	if err := r.SetTrustedProxies(controlledProxies); err != nil {
		t.Fatalf("failed to set trusted proxies: %v", err)
	}

	r.GET("/test-ip", func(c *gin.Context) {
		c.String(http.StatusOK, c.ClientIP())
	})

	// Scenario 1: Untrusted client from LAN attempts to spoof X-Forwarded-For: 127.0.0.1
	req1 := httptest.NewRequest(http.MethodGet, "/test-ip", nil)
	req1.RemoteAddr = "192.168.1.100:4567"
	req1.Header.Set("X-Forwarded-For", "127.0.0.1")
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)

	if w1.Body.String() != "192.168.1.100" {
		t.Errorf("spoofed X-Forwarded-For accepted from untrusted LAN peer! got %s, want 192.168.1.100", w1.Body.String())
	}

	// Scenario 2: Unprivileged container on 172.16.5.10 (trusted under OLD 172.16.0.0/12, now UNTRUSTED) attempts to spoof
	req2 := httptest.NewRequest(http.MethodGet, "/test-ip", nil)
	req2.RemoteAddr = "172.16.5.10:4567"
	req2.Header.Set("X-Forwarded-For", "127.0.0.1")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Body.String() != "172.16.5.10" {
		t.Errorf("spoofed X-Forwarded-For accepted from 172.16.5.10! got %s, want 172.16.5.10", w2.Body.String())
	}

	// Scenario 3: Legitimate in-container localhost proxy passes genuine client IP
	req3 := httptest.NewRequest(http.MethodGet, "/test-ip", nil)
	req3.RemoteAddr = "127.0.0.1:4567"
	req3.Header.Set("X-Forwarded-For", "203.0.113.195")
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)

	if w3.Body.String() != "203.0.113.195" {
		t.Errorf("trusted proxy X-Forwarded-For not respected! got %s, want 203.0.113.195", w3.Body.String())
	}

	// Scenario 4: Legitimate docker reverse proxy on 172.18.0.2 passes client IP
	req4 := httptest.NewRequest(http.MethodGet, "/test-ip", nil)
	req4.RemoteAddr = "172.18.0.2:4567"
	req4.Header.Set("X-Forwarded-For", "198.51.100.50")
	w4 := httptest.NewRecorder()
	r.ServeHTTP(w4, req4)

	if w4.Body.String() != "198.51.100.50" {
		t.Errorf("trusted compose gateway X-Forwarded-For not respected! got %s, want 198.51.100.50", w4.Body.String())
	}

	// Scenario 5: Verify environment variable override
	_ = os.Setenv("TRUSTED_PROXIES", "10.0.0.1, 10.0.0.2")
	defer os.Unsetenv("TRUSTED_PROXIES")
	envProxies := []string{"10.0.0.1", "10.0.0.2"}
	rEnv := gin.New()
	if err := rEnv.SetTrustedProxies(envProxies); err != nil {
		t.Fatalf("failed to set env trusted proxies: %v", err)
	}
}

// TestRound3_P2_TopologyEngine_CoreDependencyFailClosed tests Fail-Closed semantics on core dependencies (R7.2).
func TestRound3_P2_TopologyEngine_CoreDependencyFailClosed(t *testing.T) {
	ctx := context.Background()

	// 1. Unconfigured HealthChecker MUST FAIL CLOSED for core dependencies
	t.Run("Unconfigured HealthChecker Fails Closed", func(t *testing.T) {
		topo := NewTopologyEngine()
		// NewTopologyEngine has healthChecker == nil by default (optimistic stub removed)
		err := topo.PreflightDependencyCheck(ctx, "home-api")
		if err == nil {
			t.Fatal("expected error when healthChecker is unconfigured, got nil (FAIL-OPEN violation!)")
		}
		if !errors.Is(err, ErrHealthCheckerNotConfigured) {
			t.Errorf("expected ErrHealthCheckerNotConfigured, got %v", err)
		}
	})

	// 2. Core dependency check failure strictly blocks recovery
	t.Run("Core Dependency Failure Blocks Recovery", func(t *testing.T) {
		topo := NewTopologyEngine()
		topo.SetHealthChecker(func(ctx context.Context, comp string) (bool, error) {
			canonical := ResolveTopologyComponent(comp)
			if canonical == "frigate" {
				return false, errors.New("frigate container dead")
			}
			return true, nil
		})

		err := topo.PreflightDependencyCheck(ctx, "home-api")
		if err == nil {
			t.Fatal("expected error when core dependency frigate is unhealthy, got nil")
		}
		if !strings.Contains(err.Error(), "upstream dependency frigate is unhealthy") {
			t.Errorf("expected upstream dependency frigate is unhealthy, got: %v", err)
		}
	})

	// 3. Core dependency timeout strictly blocks recovery
	t.Run("Core Dependency Timeout Blocks Recovery", func(t *testing.T) {
		topo := NewTopologyEngine()
		topo.SetHealthChecker(func(ctx context.Context, comp string) (bool, error) {
			canonical := ResolveTopologyComponent(comp)
			if canonical == "mosquitto" {
				return false, context.DeadlineExceeded
			}
			return true, nil
		})

		err := topo.PreflightDependencyCheck(ctx, "home-api")
		if err == nil {
			t.Fatal("expected error when core dependency mosquitto times out, got nil")
		}
		if !errors.Is(err, ErrDependencyCheckTimeout) {
			t.Errorf("expected ErrDependencyCheckTimeout, got: %v", err)
		}
	})

	// 4. Distinguish Core vs Optional dependencies
	t.Run("Optional Dependency Failure Does Not Block Non-Dependent Recovery", func(t *testing.T) {
		topo := NewTopologyEngine()
		topo.SetHealthChecker(func(ctx context.Context, comp string) (bool, error) {
			canonical := ResolveTopologyComponent(comp)
			if canonical == "home-vision" {
				return false, errors.New("vision AI service offline")
			}
			return true, nil
		})

		// home-api does NOT depend on home-vision, only level 0 core
		err := topo.PreflightDependencyCheck(ctx, "home-api")
		if err != nil {
			t.Fatalf("home-api recovery blocked by unrelated optional service failure: %v", err)
		}

		if IsCoreDependency("home-vision") {
			t.Error("home-vision should be classified as optional peripheral, not core")
		}
		if !IsCoreDependency("frigate") || !IsCoreDependency("mosquitto") || !IsCoreDependency("app.db") || !IsCoreDependency("home-api") {
			t.Error("frigate, mosquitto, app.db, and home-api MUST all be classified as core dependencies")
		}
	})
}
