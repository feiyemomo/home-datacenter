package sso

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/ops"
)

// setupTestDB creates an in-memory SQLite database and migrates SSOTicket model.
func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open in-memory sqlite db: %v", err)
	}
	if err := db.AutoMigrate(&model.SSOTicket{}); err != nil {
		t.Fatalf("failed to auto-migrate SSOTicket: %v", err)
	}
	return db
}

// mockSessionProvider implements OnePanelSessionProvider for testing.
type mockSessionProvider struct {
	pSession   string
	pCSRFToken string
	err        error
}

func (m *mockSessionProvider) GetSession(ctx context.Context) (*ops.SessionCredentials, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &ops.SessionCredentials{
		BaseURL:    "http://172.18.0.1:10000",
		Entrance:   "btpanel",
		EntryURL:   "http://172.18.0.1:10000/btpanel",
		PSession:   m.pSession,
		PCSRFToken: m.pCSRFToken,
		ExpiresAt:  time.Now().Add(30 * time.Minute),
	}, nil
}

func TestSSOBroker_256BitEntropyAndHashStorage(t *testing.T) {
	db := setupTestDB(t)
	broker := NewBroker(db)

	ctx := context.Background()
	ticket, expiresAt, err := broker.IssueTicket(ctx, 1, "admin", true, "127.0.0.1", "Go-Test-Agent")
	if err != nil {
		t.Fatalf("expected successful ticket issuance, got: %v", err)
	}

	// 1. Check 256 bits (32 bytes) hex string length
	if len(ticket) != 64 {
		t.Fatalf("expected 64 hex chars (256 bits), got %d chars: %s", len(ticket), ticket)
	}

	// 2. Verify expiresAt is ~30 seconds in the future
	if time.Until(expiresAt) <= 25*time.Second || time.Until(expiresAt) > 31*time.Second {
		t.Fatalf("expected ~30s expiration TTL, got: %v", time.Until(expiresAt))
	}

	// 3. Plaintext ticket must NEVER be found in DB
	var plainCheck model.SSOTicket
	if err := db.Where("ticket_hash = ?", ticket).First(&plainCheck).Error; err == nil {
		t.Fatalf("CRITICAL SECURITY VIOLATION: Plaintext ticket found in database!")
	}

	// 4. SHA-256 hash must exist and match
	hashBytes := sha256.Sum256([]byte(ticket))
	expectedHash := hex.EncodeToString(hashBytes[:])

	var record model.SSOTicket
	if err := db.Where("ticket_hash = ?", expectedHash).First(&record).Error; err != nil {
		t.Fatalf("expected ticket record with hash %s, got error: %v", expectedHash, err)
	}

	if record.Consumed {
		t.Fatalf("expected unconsumed ticket, got consumed = true")
	}
	if record.ConsumedAt.Valid {
		t.Fatalf("expected ConsumedAt.Valid = false, got true")
	}
	if record.UserID != 1 || record.Username != "admin" {
		t.Fatalf("expected UserID=1 Username=admin, got UserID=%d Username=%s", record.UserID, record.Username)
	}
}

func TestSSOBroker_AdminOnlyRestriction(t *testing.T) {
	db := setupTestDB(t)
	broker := NewBroker(db)

	ctx := context.Background()

	// Non-admin user must be rejected
	ticket, _, err := broker.IssueTicket(ctx, 2, "normal_user", false, "127.0.0.1", "Go-Test-Agent")
	if err != ErrUnauthorizedAdminOnly {
		t.Fatalf("expected ErrUnauthorizedAdminOnly for non-admin, got: %v (ticket: %s)", err, ticket)
	}

	// Admin user must succeed
	ticket, _, err = broker.IssueTicket(ctx, 1, "admin_user", true, "127.0.0.1", "Go-Test-Agent")
	if err != nil {
		t.Fatalf("expected success for admin user, got: %v", err)
	}
	if len(ticket) != 64 {
		t.Fatalf("expected 64 char ticket, got: %s", ticket)
	}
}

func TestSSOBroker_TicketExpiration(t *testing.T) {
	db := setupTestDB(t)
	broker := NewBroker(db)
	broker.SetTicketTTL(20 * time.Millisecond)

	ctx := context.Background()
	ticket, _, err := broker.IssueTicket(ctx, 1, "admin", true, "127.0.0.1", "Go-Test-Agent")
	if err != nil {
		t.Fatalf("failed to issue ticket: %v", err)
	}

	// Wait for expiration
	time.Sleep(50 * time.Millisecond)

	_, err = broker.VerifyAndConsume(ctx, ticket, "127.0.0.1")
	if err != ErrTicketInvalidOrExpired {
		t.Fatalf("expected ErrTicketInvalidOrExpired for expired ticket, got: %v", err)
	}
}

func TestSSOBroker_AtomicCAS_ReplayAttack(t *testing.T) {
	db := setupTestDB(t)
	broker := NewBroker(db)

	ctx := context.Background()
	ticket, _, err := broker.IssueTicket(ctx, 1, "admin", true, "127.0.0.1", "Go-Test-Agent")
	if err != nil {
		t.Fatalf("failed to issue ticket: %v", err)
	}

	// Concurrent consumption with 15 goroutines
	concurrency := 15
	var successCount int32
	var replayCount int32
	var wg sync.WaitGroup

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, consumeErr := broker.VerifyAndConsume(ctx, ticket, "127.0.0.1")
			if consumeErr == nil {
				atomic.AddInt32(&successCount, 1)
			} else if consumeErr == ErrReplayAttackDetected || consumeErr == ErrTicketInvalidOrExpired {
				atomic.AddInt32(&replayCount, 1)
			}
		}()
	}

	wg.Wait()

	if successCount != 1 {
		t.Fatalf("CAS Atomic Failure! Expected exactly 1 successful consumption, got %d", successCount)
	}
	if replayCount != int32(concurrency-1) {
		t.Fatalf("Expected %d replay/invalid rejections, got %d", concurrency-1, replayCount)
	}

	// Subsequent consumption must return ErrReplayAttackDetected
	_, err = broker.VerifyAndConsume(ctx, ticket, "127.0.0.1")
	if err != ErrReplayAttackDetected {
		t.Fatalf("expected ErrReplayAttackDetected on subsequent attempt, got: %v", err)
	}
}

func TestSecurity_ValidateRedirectTarget(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", "/btpanel"},
		{"/btpanel", "/btpanel"},
		{"/btpanel/", "/btpanel/"},
		{"/btpanel/#/", "/btpanel/#/"},
		{"/btpanel/dashboard", "/btpanel/dashboard"},
		{"/btpanel/#/hosts", "/btpanel/#/hosts"},
		{"//evil.com", "/btpanel"},
		{"https://evil.com", "/btpanel"},
		{"http://attacker.com/btpanel", "/btpanel"},
		{"/\\evil.com", "/btpanel"},
		{"\\evil.com", "/btpanel"},
		{"javascript:alert(1)", "/btpanel"},
		{"/unauthorized/path", "/btpanel"},
		{"/btpanel?target=http://evil.com", "/btpanel"},
	}

	for _, tc := range tests {
		actual := ValidateRedirectTarget(tc.input)
		if actual != tc.expected {
			t.Errorf("ValidateRedirectTarget(%q) = %q; expected %q", tc.input, actual, tc.expected)
		}
	}
}

func TestSecurity_ValidateOrigin(t *testing.T) {
	allowed := []string{"https://dashboard.feiyemomo.top"}

	tests := []struct {
		origin   string
		referer  string
		expected bool
	}{
		{"https://dashboard.feiyemomo.top", "", true},
		{"http://localhost:5173", "", true},
		{"http://127.0.0.1:8080", "", true},
		{"http://192.168.31.234:8080", "", true},
		{"http://172.18.0.5:10000", "", true},
		{"https://app.feiyemomo.top", "", true},
		{"https://malicious.com", "", false},
		{"http://evil.feiyemomo.top.evil.com", "", false},
		{"", "https://dashboard.feiyemomo.top/dashboard", true},
		{"", "https://evil-phishing.site/attack", false},
		{"", "", true}, // Empty origin/referer for form post
	}

	for _, tc := range tests {
		req, _ := http.NewRequest("POST", "/api/v1/sso/exchange", nil)
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		if tc.referer != "" {
			req.Header.Set("Referer", tc.referer)
		}

		res := ValidateOrigin(req, allowed)
		if res != tc.expected {
			t.Errorf("ValidateOrigin(origin=%q, referer=%q) = %v; expected %v", tc.origin, tc.referer, res, tc.expected)
		}
	}
}

func TestSSOAdapter_ExchangeTicket_SuccessAndCookies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	broker := NewBroker(db)

	mockClient := &mockSessionProvider{
		pSession:   "session_token_xyz_789",
		pCSRFToken: "csrf_token_abc_123",
	}

	adapter := NewAdapterHandler(broker, mockClient, false)

	r := gin.New()
	r.POST("/api/v1/sso/exchange", adapter.ExchangeTicket)

	// 1. Issue a valid ticket
	ticket, _, err := broker.IssueTicket(context.Background(), 10, "admin_user", true, "127.0.0.1", "Test-Browser")
	if err != nil {
		t.Fatalf("failed to issue ticket: %v", err)
	}

	// 2. Perform Form POST
	formData := url.Values{}
	formData.Set("ticket", ticket)
	formData.Set("redirect_url", "/btpanel/#/home")

	req, _ := http.NewRequest("POST", "/api/v1/sso/exchange", strings.NewReader(formData.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://dashboard.feiyemomo.top")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// 3. Assert HTTP 302 Found
	if w.Code != http.StatusFound {
		t.Fatalf("expected HTTP 302, got %d: %s", w.Code, w.Body.String())
	}

	// 4. Assert Redirect Location
	location := w.Header().Get("Location")
	if location != "/btpanel/#/home" {
		t.Fatalf("expected redirect to /btpanel/#/home, got %s", location)
	}

	// 5. Assert Set-Cookie attributes
	cookies := w.Result().Cookies()
	var psessionCookie, pcsrfCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "psession" {
			psessionCookie = c
		} else if c.Name == "pcsrftoken" {
			pcsrfCookie = c
		}
	}

	if psessionCookie == nil {
		t.Fatalf("expected psession cookie in response")
	}
	if psessionCookie.Value != "session_token_xyz_789" {
		t.Fatalf("expected psession value session_token_xyz_789, got %s", psessionCookie.Value)
	}
	if !psessionCookie.HttpOnly {
		t.Fatalf("expected psession HttpOnly = true for XSS protection")
	}
	if psessionCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("expected psession SameSite = Lax, got %v", psessionCookie.SameSite)
	}
	if psessionCookie.Path != "/" {
		t.Fatalf("expected psession Path = /, got %s", psessionCookie.Path)
	}

	if pcsrfCookie == nil {
		t.Fatalf("expected pcsrftoken cookie in response")
	}
	if pcsrfCookie.Value != "csrf_token_abc_123" {
		t.Fatalf("expected pcsrftoken value csrf_token_abc_123, got %s", pcsrfCookie.Value)
	}
	if pcsrfCookie.HttpOnly {
		t.Fatalf("expected pcsrftoken HttpOnly = false so 1Panel frontend can read X-CSRF-Token")
	}

	// 6. Test Replay on exchange endpoint
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/api/v1/sso/exchange", strings.NewReader(formData.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("Origin", "https://dashboard.feiyemomo.top")
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401 on replay exchange, got %d", w2.Code)
	}
}

func TestSSOAdapter_ExchangeTicket_OpenRedirectSanitization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	broker := NewBroker(db)

	mockClient := &mockSessionProvider{
		pSession:   "sess_1",
		pCSRFToken: "csrf_1",
	}

	adapter := NewAdapterHandler(broker, mockClient, false)

	r := gin.New()
	r.POST("/api/v1/sso/exchange", adapter.ExchangeTicket)

	ticket, _, _ := broker.IssueTicket(context.Background(), 1, "admin", true, "127.0.0.1", "Test")

	formData := url.Values{}
	formData.Set("ticket", ticket)
	formData.Set("redirect_url", "https://attacker.com/evil")

	req, _ := http.NewRequest("POST", "/api/v1/sso/exchange", strings.NewReader(formData.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected HTTP 302, got %d", w.Code)
	}
	// Must be sanitized to default /btpanel
	if w.Header().Get("Location") != "/btpanel" {
		t.Fatalf("Open Redirect vulnerability! Expected /btpanel, got: %s", w.Header().Get("Location"))
	}
}

func TestInternalVerifier_FailSecure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	broker := NewBroker(db)

	verifier := NewInternalVerifierHandler(broker)
	r := gin.New()
	r.POST("/api/v1/internal/sso/verify", verifier.VerifyTicketInternal)

	// Case 1: INTERNAL_SSO_SECRET unset
	os.Unsetenv("INTERNAL_SSO_SECRET")

	body, _ := json.Marshal(map[string]string{"ticket": "dummy_ticket"})
	req, _ := http.NewRequest("POST", "/api/v1/internal/sso/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-SSO-Secret", "test")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected HTTP 503 when secret is unset, got %d", w.Code)
	}

	// Case 2: INTERNAL_SSO_SECRET < 32 chars (insufficient entropy)
	os.Setenv("INTERNAL_SSO_SECRET", "short_weak_secret_123")
	defer os.Unsetenv("INTERNAL_SSO_SECRET")

	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/api/v1/internal/sso/verify", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Internal-SSO-Secret", "short_weak_secret_123")
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected HTTP 503 for weak secret (< 32 chars), got %d", w2.Code)
	}
}

func TestInternalVerifier_SecretAndIPFencing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	broker := NewBroker(db)

	verifier := NewInternalVerifierHandler(broker)
	r := gin.New()
	r.POST("/api/v1/internal/sso/verify", verifier.VerifyTicketInternal)

	validSecret := "strong_cryptographic_secret_must_be_over_32_bytes_long"
	os.Setenv("INTERNAL_SSO_SECRET", validSecret)
	defer os.Unsetenv("INTERNAL_SSO_SECRET")

	ticket, _, _ := broker.IssueTicket(context.Background(), 5, "root_admin", true, "127.0.0.1", "Test")

	// Case 1: Unauthorized secret
	body, _ := json.Marshal(map[string]string{"ticket": ticket})
	req1, _ := http.NewRequest("POST", "/api/v1/internal/sso/verify", bytes.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Internal-SSO-Secret", "wrong_secret")
	req1.RemoteAddr = "127.0.0.1:12345"

	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401 for wrong secret, got %d", w1.Code)
	}

	// Case 2: External IP restriction
	req2, _ := http.NewRequest("POST", "/api/v1/internal/sso/verify", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Internal-SSO-Secret", validSecret)
	req2.RemoteAddr = "8.8.8.8:12345"

	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("expected HTTP 403 for external IP, got %d", w2.Code)
	}

	// Case 3: Valid internal request (Docker bridge or loopback)
	req3, _ := http.NewRequest("POST", "/api/v1/internal/sso/verify", bytes.NewReader(body))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("X-Internal-SSO-Secret", validSecret)
	req3.RemoteAddr = "172.18.0.10:45678"

	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 for valid internal call, got %d: %s", w3.Code, w3.Body.String())
	}

	var res map[string]any
	if err := json.Unmarshal(w3.Body.Bytes(), &res); err != nil {
		t.Fatalf("failed to decode json response: %v", err)
	}
	if res["valid"] != true || res["username"] != "root_admin" || res["user_id"] != float64(5) {
		t.Fatalf("unexpected verification payload: %+v", res)
	}

	// Case 4: Replay on internal endpoint
	w4 := httptest.NewRecorder()
	req4, _ := http.NewRequest("POST", "/api/v1/internal/sso/verify", bytes.NewReader(body))
	req4.Header.Set("Content-Type", "application/json")
	req4.Header.Set("X-Internal-SSO-Secret", validSecret)
	req4.RemoteAddr = "172.18.0.10:45678"
	r.ServeHTTP(w4, req4)

	var res4 map[string]any
	json.Unmarshal(w4.Body.Bytes(), &res4)
	if res4["valid"] == true {
		t.Fatalf("expected valid: false on replayed ticket verification")
	}
}
