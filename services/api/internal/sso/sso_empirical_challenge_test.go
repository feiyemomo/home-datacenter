package sso

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
)

// setupEmpiricalTestDB creates a designated in-memory SQLite database for challenge testing.
func setupEmpiricalTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbName := fmt.Sprintf("file:sso_empirical_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open in-memory sqlite db: %v", err)
	}

	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1) // Single-writer SQLite architecture prevents database locking
		_, _ = sqlDB.Exec("PRAGMA busy_timeout = 5000;")
	}

	if err := db.AutoMigrate(&model.SSOTicket{}); err != nil {
		t.Fatalf("failed to auto-migrate SSOTicket: %v", err)
	}
	return db
}

// ============================================================================
// Objective 1: SSO Ticket 256-bit Entropy & Storage Integrity
// ============================================================================
func TestEmpiricalChallenge1_SSOTicketEntropyAndStorage(t *testing.T) {
	db := setupEmpiricalTestDB(t)
	broker := NewBroker(db)
	ctx := context.Background()

	// 1. Issue a single ticket and inspect its entropy & representation
	ticket, expiresAt, err := broker.IssueTicket(ctx, 42, "superadmin", true, "127.0.0.1", "Empirical-Test-Agent/1.0")
	if err != nil {
		t.Fatalf("unexpected error issuing ticket: %v", err)
	}

	// Assert exactly 64 hex characters (32 bytes = 256 bits)
	if len(ticket) != 64 {
		t.Fatalf("CRITICAL ENTROPY VIOLATION: expected 64 hex characters (256 bits), got %d chars: %q", len(ticket), ticket)
	}

	rawBytes, err := hex.DecodeString(ticket)
	if err != nil {
		t.Fatalf("ticket is not valid hexadecimal string: %v", err)
	}
	if len(rawBytes) != 32 {
		t.Fatalf("expected exactly 32 bytes of raw entropy, got %d bytes", len(rawBytes))
	}

	// Verify expiration is default 30 seconds
	ttl := time.Until(expiresAt)
	if ttl <= 25*time.Second || ttl > 31*time.Second {
		t.Fatalf("expected ~30s ticket TTL, got %v", ttl)
	}

	// 2. Empirical Database Query: Assert Plaintext Ticket is NEVER Stored in SQLite
	var plainCount int64
	// Raw SQL search for the plaintext ticket in ticket_hash column
	if err := db.Raw("SELECT COUNT(*) FROM sso_tickets WHERE ticket_hash = ?", ticket).Scan(&plainCount).Error; err != nil {
		t.Fatalf("failed to query sso_tickets table: %v", err)
	}
	if plainCount != 0 {
		t.Fatalf("CRITICAL SECURITY VIOLATION: plaintext ticket found stored in ticket_hash column!")
	}

	// Raw SQL search across ALL columns in sso_tickets table
	rows, err := db.Raw("SELECT id, ticket_hash, user_id, username, client_ip, user_agent FROM sso_tickets").Rows()
	if err != nil {
		t.Fatalf("failed to query all sso_tickets columns: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id uint
		var hash, uname, ip, ua string
		var uid uint
		if err := rows.Scan(&id, &hash, &uid, &uname, &ip, &ua); err != nil {
			t.Fatalf("failed to scan row: %v", err)
		}
		if strings.Contains(hash, ticket) || strings.Contains(uname, ticket) || strings.Contains(ip, ticket) || strings.Contains(ua, ticket) {
			t.Fatalf("CRITICAL SECURITY LEAK: plaintext ticket leaked into database row %d", id)
		}
	}

	// 3. Verify SHA-256 Hash is strictly what is persisted
	expectedHash := hex.EncodeToString(func() []byte {
		h := sha256.Sum256([]byte(ticket))
		return h[:]
	}())

	var record model.SSOTicket
	if err := db.Where("ticket_hash = ?", expectedHash).First(&record).Error; err != nil {
		t.Fatalf("expected record with hash %s, got error: %v", expectedHash, err)
	}
	if record.Consumed {
		t.Fatalf("expected newly created ticket to have consumed = false")
	}
	if record.ConsumedAt.Valid {
		t.Fatalf("expected ConsumedAt.Valid = false for unconsumed ticket")
	}

	// 4. Verify Column Schema via PRAGMA table_info: no plaintext column exists
	type tableColumn struct {
		Cid       int
		Name      string
		Type      string
		Notnull   int
		DfltValue *string
		Pk        int
	}
	var cols []tableColumn
	if err := db.Raw("PRAGMA table_info(sso_tickets)").Scan(&cols).Error; err != nil {
		t.Fatalf("failed to read table_info: %v", err)
	}
	for _, col := range cols {
		lowerCol := strings.ToLower(col.Name)
		if lowerCol == "ticket" || lowerCol == "plaintext" || lowerCol == "raw_ticket" {
			t.Fatalf("UNAUTHORIZED COLUMN DETECTED: column %q must not exist in sso_tickets schema", col.Name)
		}
	}

	// 5. Entropy distribution & collision test: generate 100 tickets, verify all 100 are unique
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		tkt, _, err := broker.IssueTicket(ctx, uint(i+1), "admin", true, "127.0.0.1", "test")
		if err != nil {
			t.Fatalf("failed to issue ticket #%d: %v", i, err)
		}
		if len(tkt) != 64 {
			t.Fatalf("ticket #%d length %d != 64", i, len(tkt))
		}
		if seen[tkt] {
			t.Fatalf("CRITICAL ENTROPY COLLISION: duplicate ticket generated: %s", tkt)
		}
		seen[tkt] = true
	}
}

// ============================================================================
// Objective 2: Concurrent Replay Attack (50 Simultaneous Goroutines)
// ============================================================================
func TestEmpiricalChallenge2_ConcurrentReplayAttack_50Goroutines(t *testing.T) {
	db := setupEmpiricalTestDB(t)
	broker := NewBroker(db)
	ctx := context.Background()

	// Issue a single valid ticket
	ticket, _, err := broker.IssueTicket(ctx, 10, "admin_user", true, "192.168.1.50", "Race-Harness")
	if err != nil {
		t.Fatalf("failed to issue ticket: %v", err)
	}

	const concurrency = 50
	var (
		wg           sync.WaitGroup
		startBarrier = make(chan struct{}) // Synchronization barrier releasing all 50 goroutines at once
		successMu    sync.Mutex
		successCount int
		replayCount  int
		otherErrors  []error
	)

	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func(workerID int) {
			defer wg.Done()
			// Wait for the release barrier to trigger maximum race contention
			<-startBarrier

			_, consumeErr := broker.VerifyAndConsume(context.Background(), ticket, fmt.Sprintf("192.168.1.%d", workerID+1))

			successMu.Lock()
			defer successMu.Unlock()

			if consumeErr == nil {
				successCount++
			} else if errors.Is(consumeErr, ErrReplayAttackDetected) {
				replayCount++
			} else {
				otherErrors = append(otherErrors, fmt.Errorf("worker %d returned unexpected error: %w", workerID, consumeErr))
			}
		}(i)
	}

	// Unleash all 50 goroutines simultaneously
	close(startBarrier)
	wg.Wait()

	// Assertions: STRICTLY 1 success and 49 ErrReplayAttackDetected
	if len(otherErrors) > 0 {
		t.Fatalf("concurrency test encountered unexpected errors: %v", otherErrors)
	}
	if successCount != 1 {
		t.Fatalf("CRITICAL RACE CONDITION VULNERABILITY: expected strictly 1 successful consumption, got %d", successCount)
	}
	if replayCount != concurrency-1 {
		t.Fatalf("expected strictly %d ErrReplayAttackDetected, got %d", concurrency-1, replayCount)
	}

	// Subsequent 51st attempt MUST also fail with ErrReplayAttackDetected
	_, errPost := broker.VerifyAndConsume(ctx, ticket, "192.168.1.254")
	if !errors.Is(errPost, ErrReplayAttackDetected) {
		t.Fatalf("expected subsequent call to return ErrReplayAttackDetected, got: %v", errPost)
	}

	// Verify DB record status
	hashBytes := sha256.Sum256([]byte(ticket))
	expectedHash := hex.EncodeToString(hashBytes[:])
	var record model.SSOTicket
	if err := db.Where("ticket_hash = ?", expectedHash).First(&record).Error; err != nil {
		t.Fatalf("failed to query ticket record: %v", err)
	}
	if !record.Consumed {
		t.Fatalf("expected ticket consumed flag to be true in database")
	}
	if !record.ConsumedAt.Valid {
		t.Fatalf("expected ticket consumed_at to be valid in database")
	}
}

// ============================================================================
// Objective 3: Open Redirect & Origin Fuzzing
// ============================================================================
func TestEmpiricalChallenge3_OpenRedirectAndOriginFuzzing(t *testing.T) {
	// Adversarial targets must all be sanitized to /btpanel
	maliciousTargets := []string{
		"//evil.com",
		"//evil.com/path",
		"https://attacker.org",
		"http://attacker.org",
		"/\\attacker.com",
		"\\attacker.com",
		"\\\\attacker.com",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"///attacker.com",
		"////attacker.com",
		"http://evil.com/btpanel",
		"https://evil.com/btpanel",
		"ftp://evil.com/btpanel",
		"file:///etc/passwd",
		"vbscript:msgbox(1)",
		"\\/\\/attacker.com",
		"/btpanel?redirect=https://evil.com",
		"/btpanel/something://evil.com",
		" /btpanel",
		"http://127.0.0.1:10000",
		"https://dashboard.feiyemomo.top",
		"//feiyemomo.top",
		"//attacker.com/%2f..",
		"",
	}

	for _, target := range maliciousTargets {
		sanitized := ValidateRedirectTarget(target)
		// Must not preserve any protocol or attacker domain, must be sanitized to /btpanel
		if sanitized != "/btpanel" {
			t.Fatalf("CRITICAL OPEN REDIRECT SANITIZATION FAILED: payload %q sanitized to %q (expected /btpanel)", target, sanitized)
		}
	}

	// Legitimate targets must be preserved
	legitimateTargets := []string{
		"/btpanel",
		"/btpanel/",
		"/btpanel/#/",
		"/btpanel/#/home",
		"/btpanel/#/containers",
		"/btpanel/#/settings",
	}
	for _, target := range legitimateTargets {
		sanitized := ValidateRedirectTarget(target)
		if sanitized != target {
			t.Fatalf("legitimate target %q unexpectedly altered to %q", target, sanitized)
		}
	}

	// Origin Header Validation Fuzzing
	allowedOrigins := []string{"https://dashboard.feiyemomo.top", "http://127.0.0.1"}

	forbiddenOrigins := []string{
		"http://attacker.org",
		"https://attacker.org",
		"http://evil-feiyemomo.top",
		"https://dashboard.feiyemomo.top.attacker.org",
		"http://feiyemomo.top.evil.com",
		"null",
		"file://",
		"data:text/html",
	}

	for _, origin := range forbiddenOrigins {
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/sso/exchange", nil)
		req.Header.Set("Origin", origin)
		if ValidateOrigin(req, allowedOrigins) {
			t.Fatalf("CRITICAL CORS/ORIGIN VULNERABILITY: malicious origin %q was permitted!", origin)
		}
	}

	allowedTestOrigins := []string{
		"https://dashboard.feiyemomo.top",
		"http://127.0.0.1",
		"http://localhost:5173",
		"http://192.168.31.234",
		"http://10.0.0.1",
		"https://nas.feiyemomo.top",
	}
	for _, origin := range allowedTestOrigins {
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/sso/exchange", nil)
		req.Header.Set("Origin", origin)
		if !ValidateOrigin(req, allowedOrigins) {
			t.Fatalf("valid trusted origin %q was unexpectedly rejected", origin)
		}
	}
}

// ============================================================================
// Objective 4: Fail-Secure Secret Validation
// ============================================================================
func TestEmpiricalChallenge4_FailSecureSecretValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupEmpiricalTestDB(t)
	broker := NewBroker(db)
	verifier := NewInternalVerifierHandler(broker)

	r := gin.New()
	r.POST("/api/v1/internal/sso/verify", verifier.VerifyTicketInternal)

	// Issue a valid ticket to test with
	ticket, _, err := broker.IssueTicket(context.Background(), 1, "admin", true, "127.0.0.1", "test")
	if err != nil {
		t.Fatalf("failed to issue ticket: %v", err)
	}

	validBody := fmt.Sprintf(`{"ticket":"%s"}`, ticket)

	// 1. Scenario A: Secret is Unset / Empty -> Must return HTTP 503 (Fail-Secure)
	t.Run("Unset Secret Returns 503", func(t *testing.T) {
		t.Setenv("INTERNAL_SSO_SECRET", "")
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/internal/sso/verify", strings.NewReader(validBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Internal-SSO-Secret", "any-random-secret-provided-by-attacker")
		req.RemoteAddr = "127.0.0.1:12345"
		r.ServeHTTP(w, req)

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("CRITICAL FAIL-SECURE FAILURE: expected 503 Service Unavailable when secret is unset, got %d: %s", w.Code, w.Body.String())
		}
	})

	// 2. Scenario B: Secret is Too Short (<32 chars) -> Must return HTTP 503
	t.Run("Short Secret Returns 503", func(t *testing.T) {
		t.Setenv("INTERNAL_SSO_SECRET", "short-secret-only-24-bytes!") // 27 chars < 32
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/internal/sso/verify", strings.NewReader(validBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Internal-SSO-Secret", "short-secret-only-24-bytes!")
		req.RemoteAddr = "127.0.0.1:12345"
		r.ServeHTTP(w, req)

		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("CRITICAL FAIL-SECURE FAILURE: expected 503 Service Unavailable for short secret (<32 chars), got %d: %s", w.Code, w.Body.String())
		}
	})

	// 3. Scenario C: Secret Configured (>=32 chars), but Caller Passes Wrong Secret -> Must return HTTP 401
	t.Run("Incorrect Secret Returns 401", func(t *testing.T) {
		genuineSecret := "this-is-a-genuine-internal-secret-that-has-over-32-characters"
		t.Setenv("INTERNAL_SSO_SECRET", genuineSecret)

		// Subcase C.1: Mismatched secret
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/internal/sso/verify", strings.NewReader(validBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Internal-SSO-Secret", "attacker-guessed-wrong-secret-with-more-than-32-chars")
		req.RemoteAddr = "127.0.0.1:12345"
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("CRITICAL BYPASS VULNERABILITY: expected 401 Unauthorized for incorrect secret, got %d: %s", w.Code, w.Body.String())
		}

		// Subcase C.2: Missing header
		wMissing := httptest.NewRecorder()
		reqMissing, _ := http.NewRequest(http.MethodPost, "/api/v1/internal/sso/verify", strings.NewReader(validBody))
		reqMissing.Header.Set("Content-Type", "application/json")
		reqMissing.RemoteAddr = "127.0.0.1:12345"
		r.ServeHTTP(wMissing, reqMissing)

		if wMissing.Code != http.StatusUnauthorized {
			t.Fatalf("CRITICAL BYPASS VULNERABILITY: expected 401 Unauthorized for missing secret header, got %d: %s", wMissing.Code, wMissing.Body.String())
		}
	})

	// 4. Scenario D: Correct Secret, but Caller from Public IP -> Must return HTTP 403 Forbidden
	t.Run("Public IP Network Fencing Returns 403", func(t *testing.T) {
		genuineSecret := "this-is-a-genuine-internal-secret-that-has-over-32-characters"
		t.Setenv("INTERNAL_SSO_SECRET", genuineSecret)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/internal/sso/verify", strings.NewReader(validBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Internal-SSO-Secret", genuineSecret)
		req.RemoteAddr = "198.51.100.42:12345" // Public routable IP
		r.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("CRITICAL NETWORK FENCING VIOLATION: expected 403 Forbidden from public IP, got %d: %s", w.Code, w.Body.String())
		}
	})

	// 5. Scenario E: Genuine Secret + Loopback/Bridge IP -> Returns 200 OK and Valid
	t.Run("Genuine Secret and Internal Subnet Succeeds", func(t *testing.T) {
		genuineSecret := "this-is-a-genuine-internal-secret-that-has-over-32-characters"
		t.Setenv("INTERNAL_SSO_SECRET", genuineSecret)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/internal/sso/verify", strings.NewReader(validBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Internal-SSO-Secret", genuineSecret)
		req.RemoteAddr = "127.0.0.1:12345"
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for valid request, got %d: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"valid":true`) {
			t.Fatalf("expected valid:true in response, got %s", w.Body.String())
		}
	})
}
