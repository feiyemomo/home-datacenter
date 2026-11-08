package ops

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"home-datacenter-api/internal/model"
)



// TestAdversarial_M4_SessionIsolation_AntiIDOR empirically tests horizontal privilege boundaries.
func TestAdversarial_M4_SessionIsolation_AntiIDOR(t *testing.T) {
	_, r, db := setupM4TestRouter(t)

	// Seed User 100 private session
	user100Private := model.OpsChatSession{
		SessionID: "session-u100-secret",
		UserID:    100,
		Scope:     model.ChatScopePersonal,
		Title:     "Top Secret Ops",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	// Seed User 100 household session
	user100Household := model.OpsChatSession{
		SessionID: "session-u100-family",
		UserID:    100,
		Scope:     model.ChatScopeHousehold,
		Title:     "Family Living Room Devices",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	db.Create(&user100Private)
	db.Create(&user100Household)

	// --- Attack 1: User 200 attempts to list sessions -> must NOT see user100Private ---
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/ops/chat/sessions", nil)
	req.Header.Set("X-User-ID", "200")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var listed []model.OpsChatSession
	_ = json.Unmarshal(w.Body.Bytes(), &listed)
	for _, s := range listed {
		if s.SessionID == user100Private.SessionID {
			t.Fatalf("IDOR EXPLOIT: User 200 was able to discover User 100 private session %s", s.SessionID)
		}
	}

	// --- Attack 2: User 200 attempts to read or post messages to User 100 private session ---
	postPayload, _ := json.Marshal(PostChatMessageRequest{
		SessionID: user100Private.SessionID,
		Content:   "Inject malicious prompt into victim session",
	})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/chat/messages", bytes.NewReader(postPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "200")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("IDOR EXPLOIT: User 200 posted into User 100 private session without 403 Forbidden! Got %d: %s", w.Code, w.Body.String())
	}

	// --- Attack 3: User 200 attempts to convert victim private session to household scope via request body ---
	tamperPayload, _ := json.Marshal(PostChatMessageRequest{
		SessionID: user100Private.SessionID,
		Content:   "Try to tamper session scope",
		Scope:     model.ChatScopeHousehold,
	})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/chat/messages", bytes.NewReader(tamperPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "200")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("Scope tampering exploit: User 200 bypassed check by passing scope=household! Got %d", w.Code)
	}

	// Verify session scope in DB was NOT altered
	var refreshedSession model.OpsChatSession
	db.Where("session_id = ?", user100Private.SessionID).First(&refreshedSession)
	if refreshedSession.Scope != model.ChatScopePersonal {
		t.Fatalf("Integrity violation: session scope was mutated from personal to %s", refreshedSession.Scope)
	}

	// --- Attack 4: Unauthenticated or UserID=0 attempt ---
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/chat/messages", bytes.NewReader(postPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "0")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("Expected 403 for UserID=0 against User 100 private session, got %d", w.Code)
	}

	// --- Legitimate Case 1: Household session accessible by User 200 ---
	housePayload, _ := json.Marshal(PostChatMessageRequest{
		SessionID: user100Household.SessionID,
		Content:   "Hello family session from User 200",
	})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/chat/messages", bytes.NewReader(housePayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "200")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Household session must be accessible to household members! Got %d: %s", w.Code, w.Body.String())
	}

	// --- Legitimate Case 2: Admin override on private session ---
	adminPayload, _ := json.Marshal(PostChatMessageRequest{
		SessionID: user100Private.SessionID,
		Content:   "Admin security inspection message",
	})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/chat/messages", bytes.NewReader(adminPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "999")
	req.Header.Set("X-Admin", "true")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("Admin must be able to inspect sessions! Got %d: %s", w.Code, w.Body.String())
	}
}

// TestAdversarial_M4_TelemetryDeFaking_ThoroughStress verifies complete absence of fake telemetry.
func TestAdversarial_M4_TelemetryDeFaking_ThoroughStress(t *testing.T) {
	handler, _, _ := setupM4TestRouter(t)

	// Purge LLM client so domainReasoningFallback is strictly executed
	setOpsLLMClient(nil)

	// List of forbidden fabricated strings that must NEVER appear as factual metrics
	forbiddenFabrications := []string{
		"142",
		"194.26.29.112",
		"102.4",
		"3 级动态 CPU 门控硬件保护熔断",
		"凌晨 03:15",
		"7 个微服务网格正常连通",
	}

	adversarialQueries := []struct {
		name             string
		query            string
		expectedContains string
	}{
		{
			name:             "SSH attack query without DefenseManager",
			query:            "分析下最近24小时内黑客对SSH密码爆破攻击的142次拦截详情",
			expectedContains: "未配置/状态未知",
		},
		{
			name:             "S3 backup query without configured S3",
			query:            "检查下S3备份是不是有102.4GB容量，耗时是多少",
			expectedContains: "状态未知/未配置",
		},
		{
			name:             "Vision AI crash diagnosis (live container)",
			query:            "视觉AI vision 是不是在凌晨 03:15 触发熔断重启了？",
			expectedContains: "Vision AI 智能视觉分析诊断",
		},
		{
			name:             "Default general architecture status",
			query:            "系统整体服务网格状态如何？",
			expectedContains: "微服务网格: 共检测到",
		},
	}

	for _, tc := range adversarialQueries {
		t.Run(tc.name, func(t *testing.T) {
			reply, _ := handler.domainReasoningFallback(tc.query)

			// Assert forbidden fabrications do NOT appear in the response
			for _, forbidden := range forbiddenFabrications {
				// Special case: if query itself had the phrase, ensure response does not assert it as factual state
				if strings.Contains(reply, forbidden) {
					t.Fatalf("De-Faking Failure: response for query %q contained fabricated string %q!\nFull Reply: %s",
						tc.query, forbidden, reply)
				}
			}

			// Assert required status indicator
			if !strings.Contains(reply, tc.expectedContains) {
				t.Fatalf("Expected explicit status %q in response, but got:\n%s", tc.expectedContains, reply)
			}
		})
	}

	// Additional test: Vision AI query when container is completely absent (fallback == nil)
	t.Run("Vision AI when fallback is nil", func(t *testing.T) {
		hNil := NewOpsHandler(nil, nil, nil, nil, nil)
		hNil.fallback = nil
		reply, _ := hNil.domainReasoningFallback("视觉AI vision 是不是在凌晨 03:15 触发熔断重启了？")
		if !strings.Contains(reply, "状态未知/未检测到运行实例") {
			t.Fatalf("Expected '状态未知/未检测到运行实例' when vision container is missing, got:\n%s", reply)
		}
		for _, forbidden := range forbiddenFabrications {
			if strings.Contains(reply, forbidden) {
				t.Fatalf("Response contained forbidden fabrication %q: %s", forbidden, reply)
			}
		}
	})
}

// TestAdversarial_M4_CVSS31_OfficialFIRSTEquations verifies CVSS 3.1 calculation across test vectors.
func TestAdversarial_M4_CVSS31_OfficialFIRSTEquations(t *testing.T) {
	cases := []struct {
		name         string
		vector       string
		expectedScore float64
		expectedRisk string
		expectErr    bool
	}{
		{
			name:         "Critical network unauthenticated (9.8 CRITICAL)",
			vector:       "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
			expectedScore: 9.8,
			expectedRisk: "CRITICAL",
		},
		{
			name:         "Log4Shell / Zerologon Scope Changed (10.0 CRITICAL)",
			vector:       "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:C/C:H/I:H/A:H",
			expectedScore: 10.0,
			expectedRisk: "CRITICAL",
		},
		{
			name:         "High network authenticated PR:L (8.8 HIGH)",
			vector:       "CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:H/A:H",
			expectedScore: 8.8,
			expectedRisk: "HIGH",
		},
		{
			name:         "Scope changed network authenticated PR:L (9.9 CRITICAL)",
			vector:       "CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:C/C:H/I:H/A:H",
			expectedScore: 9.9,
			expectedRisk: "CRITICAL",
		},
		{
			name:         "High network authenticated admin PR:H (7.2 HIGH)",
			vector:       "CVSS:3.1/AV:N/AC:L/PR:H/UI:N/S:U/C:H/I:H/A:H",
			expectedScore: 7.2,
			expectedRisk: "HIGH",
		},
		{
			name:         "Scope changed network authenticated admin PR:H (9.1 CRITICAL)",
			vector:       "CVSS:3.1/AV:N/AC:L/PR:H/UI:N/S:C/C:H/I:H/A:H",
			expectedScore: 9.1,
			expectedRisk: "CRITICAL",
		},
		{
			name:         "Zero Impact (0.0 LOW)",
			vector:       "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:N",
			expectedScore: 0.0,
			expectedRisk: "LOW",
		},
		{
			name:         "Invalid CVSS version prefix (reject)",
			vector:       "CVSS:2.0/AV:N/AC:L/Au:N/C:P/I:P/A:P",
			expectErr:    true,
		},
		{
			name:         "Malformed random string (reject)",
			vector:       "random_text_not_cvss",
			expectErr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			score, risk, err := ParseCVSS31Vector(tc.vector)
			if tc.expectErr {
				if err == nil {
					t.Fatalf("expected vector %s to fail validation, but got score %.1f", tc.vector, score)
				}
				return
			}

			if err != nil {
				t.Fatalf("ParseCVSS31Vector failed unexpectedly on %s: %v", tc.vector, err)
			}

			if score != tc.expectedScore {
				t.Fatalf("Vector %s score mismatch: expected %.1f, got %.1f", tc.vector, tc.expectedScore, score)
			}
			if risk != tc.expectedRisk {
				t.Fatalf("Vector %s risk mismatch: expected %s, got %s", tc.vector, tc.expectedRisk, risk)
			}
		})
	}
}

// TestAdversarial_M4_SnapshotSecurityStress tests path traversal and SQLite magic byte enforcement.
func TestAdversarial_M4_SnapshotSecurityStress(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Path traversal attacks on CleanSnapshotComponentName
	traversalPayloads := []struct {
		input       string
		expectClean string
		expectErr   bool
	}{
		{"../../../../etc/shadow", "shadow", false},
		{"..\\..\\..\\Windows\\System32\\SAM", "SAM", false},
		{"/vol1/docker/home-datacenter/app.db", "app_db", false},
		{"comp; rm -rf /", "comp__rm_-rf_", false},
		{"comp' OR '1'='1", "comp__OR__1___1", false},
		{"valid-name_01", "valid-name_01", false},
		{"", "", true},
		{".", "", true},
		{"/", "", true},
		{"\\", "", true},
		{strings.Repeat("a", 100), strings.Repeat("a", 32), false}, // Truncated to 32
	}

	for _, tc := range traversalPayloads {
		res, err := CleanSnapshotComponentName(tc.input)
		if tc.expectErr {
			if err == nil {
				t.Fatalf("Expected CleanSnapshotComponentName(%q) to fail, got %q", tc.input, res)
			}
		} else {
			if err != nil {
				t.Fatalf("CleanSnapshotComponentName(%q) returned unexpected error: %v", tc.input, err)
			}
			if res != tc.expectClean {
				t.Fatalf("CleanSnapshotComponentName(%q): expected %q, got %q", tc.input, tc.expectClean, res)
			}
			if len(res) > 32 {
				t.Fatalf("CleanSnapshotComponentName output exceeds 32 chars: %d", len(res))
			}
			if strings.Contains(res, "/") || strings.Contains(res, "\\") || strings.Contains(res, "..") {
				t.Fatalf("CleanSnapshotComponentName returned path separators: %q", res)
			}
		}
	}

	// 2. Magic byte validation
	// 2a. 0-byte file
	emptyFile := filepath.Join(tempDir, "empty.db")
	_ = os.WriteFile(emptyFile, []byte{}, 0644)
	if err := ValidateSQLiteSnapshot(emptyFile); err == nil {
		t.Fatalf("ValidateSQLiteSnapshot should fail on empty file")
	}

	// 2b. 15-byte file (too small)
	shortFile := filepath.Join(tempDir, "short.db")
	_ = os.WriteFile(shortFile, []byte("SQLite format 3"), 0644) // 15 bytes
	if err := ValidateSQLiteSnapshot(shortFile); err == nil {
		t.Fatalf("ValidateSQLiteSnapshot should fail on 15-byte file")
	}

	// 2c. 16-byte corrupted / wrong magic header
	wrongMagic := filepath.Join(tempDir, "wrong_magic.db")
	_ = os.WriteFile(wrongMagic, []byte("NOT_A_SQLITE_HDR"), 0644)
	if err := ValidateSQLiteSnapshot(wrongMagic); err == nil {
		t.Fatalf("ValidateSQLiteSnapshot should fail on incorrect magic bytes")
	}

	// 2d. Exact 16-byte valid magic header
	exactMagic := filepath.Join(tempDir, "exact_magic.db")
	_ = os.WriteFile(exactMagic, []byte("SQLite format 3\x00"), 0644)
	if err := ValidateSQLiteSnapshot(exactMagic); err != nil {
		t.Fatalf("ValidateSQLiteSnapshot should accept exact 16-byte SQLite header: %v", err)
	}

	// 2e. Non-existent file
	if err := ValidateSQLiteSnapshot(filepath.Join(tempDir, "does_not_exist.db")); err == nil {
		t.Fatalf("ValidateSQLiteSnapshot should fail on non-existent file")
	}

	// 2f. Directory path
	if err := ValidateSQLiteSnapshot(tempDir); err == nil {
		t.Fatalf("ValidateSQLiteSnapshot should fail on directory")
	}

	// 3. RestorePhysicalSnapshot security gate: must abort when source is invalid
	fsm := NewSafetyFSM(nil, nil, nil, tempDir, "http://127.0.0.1:8080/health")
	targetDB := filepath.Join(tempDir, "active_app.db")
	_ = os.WriteFile(targetDB, []byte("original_active_content"), 0644)

	// Try restoring corrupt snapshot
	err := fsm.RestorePhysicalSnapshot(wrongMagic, targetDB)
	if err == nil {
		t.Fatalf("RestorePhysicalSnapshot must reject corrupted snapshot")
	}
	// Verify targetDB was NOT overwritten
	content, _ := os.ReadFile(targetDB)
	if string(content) != "original_active_content" {
		t.Fatalf("Target DB was corrupted or overwritten by failed restore!")
	}
}
