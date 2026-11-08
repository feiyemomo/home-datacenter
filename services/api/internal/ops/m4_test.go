package ops

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
)

func setupM4TestRouter(t *testing.T) (*OpsHandler, *gin.Engine, *gorm.DB) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite memory db: %v", err)
	}

	if err := db.AutoMigrate(
		&model.OpsChatSession{},
		&model.OpsChatMessage{},
		&model.OpsProposal{},
		&model.OpsExecutionHistory{},
	); err != nil {
		t.Fatalf("auto-migration failed: %v", err)
	}

	handler := NewOpsHandler(nil, nil, db, nil, nil)

	r := gin.New()
	rg := r.Group("/api/v1/ops")
	rg.Use(func(c *gin.Context) {
		// Mock auth context from headers
		if u := c.GetHeader("X-User-ID"); u != "" {
			var uid uint
			if _, err := json.Number(u).Int64(); err == nil {
				var n uint
				for _, ch := range u {
					n = n*10 + uint(ch-'0')
				}
				uid = n
			}
			c.Set("user_id", uid)
		} else {
			c.Set("user_id", uint(1))
		}

		if c.GetHeader("X-Admin") == "true" {
			c.Set("is_admin", true)
		} else {
			c.Set("is_admin", false)
		}
		c.Next()
	})

	handler.RegisterChatRoutes(rg)
	handler.RegisterProposalRoutes(rg)

	return handler, r, db
}

func TestOpsChat_SessionIsolation_AntiIDOR(t *testing.T) {
	_, r, db := setupM4TestRouter(t)

	// Seed private session for user 10
	s1 := model.OpsChatSession{
		SessionID: "session-user10-private",
		UserID:    10,
		Scope:     model.ChatScopePersonal,
		Title:     "User 10 Private Ops",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	// Seed household session for user 10
	s2 := model.OpsChatSession{
		SessionID: "session-shared-household",
		UserID:    10,
		Scope:     model.ChatScopeHousehold,
		Title:     "Household Shared Ops",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	db.Create(&s1)
	db.Create(&s2)

	// 1. User 20 lists sessions: should NOT see User 10's private session, but SHOULD see household session
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/ops/chat/sessions", nil)
	req.Header.Set("X-User-ID", "20")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}
	var sessions []model.OpsChatSession
	if err := json.Unmarshal(w.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("failed to decode sessions: %v", err)
	}

	for _, s := range sessions {
		if s.SessionID == "session-user10-private" {
			t.Fatalf("Anti-IDOR violation: User 20 saw User 10's private session!")
		}
	}

	foundHousehold := false
	for _, s := range sessions {
		if s.SessionID == "session-shared-household" {
			foundHousehold = true
			break
		}
	}
	if !foundHousehold {
		t.Fatalf("User 20 should see household shared session")
	}

	// 2. User 20 attempts to send a message to User 10's private session -> 403 Forbidden
	payload, _ := json.Marshal(PostChatMessageRequest{
		SessionID: "session-user10-private",
		Content:   "Hello private session",
	})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/chat/messages", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "20")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for cross-user private session access, got %d: %s", w.Code, w.Body.String())
	}

	// 3. Admin attempts to send message to User 10's private session -> 200 OK
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/chat/messages", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "20")
	req.Header.Set("X-Admin", "true")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for admin override, got %d: %s", w.Code, w.Body.String())
	}

	// 4. User 20 sends message to household session -> 200 OK
	hPayload, _ := json.Marshal(PostChatMessageRequest{
		SessionID: "session-shared-household",
		Content:   "Household message from User 20",
	})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/chat/messages", bytes.NewReader(hPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "20")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for household session access, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOpsChat_DeFaking_Telemetry(t *testing.T) {
	handler, _, _ := setupM4TestRouter(t)

	// 1. SSH query with nil DefenseManager
	replySSH, _ := handler.domainReasoningFallback("帮我分析一下最近的SSH攻击情况")
	if bytes.Contains([]byte(replySSH), []byte("142")) || bytes.Contains([]byte(replySSH), []byte("194.26.29.112")) {
		t.Fatalf("De-faking violation: SSH response contains fake hardcoded numbers/IPs: %s", replySSH)
	}
	if !bytes.Contains([]byte(replySSH), []byte("未配置/状态未知")) {
		t.Fatalf("De-faking violation: unconfigured DefenseManager must report 未配置/状态未知, got: %s", replySSH)
	}

	// 2. S3 Backup query with unconfigured S3
	replyS3, _ := handler.domainReasoningFallback("检查一下异地S3备份健康度")
	if bytes.Contains([]byte(replyS3), []byte("102.4")) || bytes.Contains([]byte(replyS3), []byte("今日 04:00")) {
		t.Fatalf("De-faking violation: S3 response contains fake 102.4GB or 04:00: %s", replyS3)
	}
	if !bytes.Contains([]byte(replyS3), []byte("状态未知/未配置")) {
		t.Fatalf("De-faking violation: unconfigured S3 must report 状态未知/未配置, got: %s", replyS3)
	}

	// 3. Vision AI query
	replyVision, _ := handler.domainReasoningFallback("诊断下视觉AI vision 重启原因")
	if bytes.Contains([]byte(replyVision), []byte("凌晨 03:15")) || bytes.Contains([]byte(replyVision), []byte("3 级动态 CPU 门控硬件保护熔断")) {
		t.Fatalf("De-faking violation: Vision response contains fake 03:15 breaker trip: %s", replyVision)
	}

	// 3b. Vision AI query with nil fallback
	handlerNoFallback := NewOpsHandler(nil, nil, nil, nil, nil)
	handlerNoFallback.fallback = nil
	replyVisionNil, _ := handlerNoFallback.domainReasoningFallback("诊断下视觉AI vision 重启原因")
	if !bytes.Contains([]byte(replyVisionNil), []byte("状态未知")) {
		t.Fatalf("De-faking violation: missing vision container must report 状态未知, got: %s", replyVisionNil)
	}

	// 4. Default query
	replyDefault, _ := handler.domainReasoningFallback("系统整体状态如何")
	if bytes.Contains([]byte(replyDefault), []byte("7 个微服务网格正常连通")) {
		t.Fatalf("De-faking violation: default response contains fake 7-mesh claim: %s", replyDefault)
	}
}

func TestVulnScanner_DynamicParseAndCVSS31(t *testing.T) {
	// 1. Dynamic go.mod parsing
	goModSample := `
module home-datacenter-api

go 1.26

require (
	github.com/gin-gonic/gin v1.12.0
	github.com/glebarez/sqlite v1.11.0
	gorm.io/gorm v1.31.2
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
)
`
	targets, err := ParseGoMod(goModSample)
	if err != nil {
		t.Fatalf("ParseGoMod failed: %v", err)
	}
	if len(targets) != 4 {
		t.Fatalf("expected 4 targets, got %d", len(targets))
	}
	if targets[0].Package != "github.com/gin-gonic/gin" || targets[0].Version != "v1.12.0" {
		t.Fatalf("unexpected target 0: %+v", targets[0])
	}

	// 2. Dynamic requirements.txt parsing
	reqTxtSample := `
# OpenCV headless for AI vision
opencv-python-headless>=4.8.0
requests==2.28.0
cryptography~=41.0.0
numpy
`
	pyTargets, err := ParseRequirementsTxt(reqTxtSample)
	if err != nil {
		t.Fatalf("ParseRequirementsTxt failed: %v", err)
	}
	if len(pyTargets) != 4 {
		t.Fatalf("expected 4 python targets, got %d", len(pyTargets))
	}
	if pyTargets[0].Package != "opencv-python-headless" || pyTargets[0].Version != "4.8.0" {
		t.Fatalf("unexpected pyTarget 0: %+v", pyTargets[0])
	}
	if pyTargets[1].Package != "requests" || pyTargets[1].Version != "2.28.0" {
		t.Fatalf("unexpected pyTarget 1: %+v", pyTargets[1])
	}

	// 3. FIRST CVSS 3.1 Equations
	score, risk, err := ParseCVSS31Vector("CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H")
	if err != nil {
		t.Fatalf("ParseCVSS31Vector failed: %v", err)
	}
	if score != 9.8 || risk != "CRITICAL" {
		t.Fatalf("expected 9.8 CRITICAL, got %.1f %s", score, risk)
	}

	// Changed Scope vector
	scoreScope, riskScope, err := ParseCVSS31Vector("CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:C/C:H/I:H/A:H")
	if err != nil {
		t.Fatalf("ParseCVSS31Vector scope changed failed: %v", err)
	}
	if scoreScope < 9.0 || riskScope != "CRITICAL" {
		t.Fatalf("expected CRITICAL for scope changed critical vuln, got %.1f %s", scoreScope, riskScope)
	}
}

func TestSafetyFSM_SnapshotPathTraversalAndMagicHeader(t *testing.T) {
	// 1. CleanSnapshotComponentName sanitization
	sanitized, err := CleanSnapshotComponentName("../../../../etc/passwd")
	if err != nil {
		t.Fatalf("CleanSnapshotComponentName failed: %v", err)
	}
	if sanitized != "passwd" {
		t.Fatalf("expected passwd, got %s", sanitized)
	}

	// Special characters sanitization
	sanitizedQuote, err := CleanSnapshotComponentName("comp'inject;rm -rf")
	if err != nil {
		t.Fatalf("CleanSnapshotComponentName failed on quote: %v", err)
	}
	if sanitizedQuote != "comp_inject_rm_-rf" {
		t.Fatalf("expected sanitized name, got %s", sanitizedQuote)
	}

	// 2. ValidateSQLiteSnapshot: invalid file
	tempDir := t.TempDir()
	dummyFile := filepath.Join(tempDir, "dummy.db")
	_ = os.WriteFile(dummyFile, []byte("NOT_A_SQLITE_HEADER"), 0644)
	if err := ValidateSQLiteSnapshot(dummyFile); err == nil {
		t.Fatalf("expected ValidateSQLiteSnapshot to fail on dummy file")
	}

	// 3. ValidateSQLiteSnapshot: valid SQLite database
	validDBFile := filepath.Join(tempDir, "valid.db")
	db, err := gorm.Open(sqlite.Open(validDBFile), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to create valid sqlite db: %v", err)
	}
	_ = db.AutoMigrate(&model.OpsProposal{})
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}

	if err := ValidateSQLiteSnapshot(validDBFile); err != nil {
		t.Fatalf("ValidateSQLiteSnapshot failed on valid db: %v", err)
	}

	// 4. RestorePhysicalSnapshot
	fsm := NewSafetyFSM(db, nil, nil, tempDir, "http://127.0.0.1:9999")
	restoredTarget := filepath.Join(tempDir, "restored.db")
	if err := fsm.RestorePhysicalSnapshot(validDBFile, restoredTarget); err != nil {
		t.Fatalf("RestorePhysicalSnapshot failed: %v", err)
	}
	if err := ValidateSQLiteSnapshot(restoredTarget); err != nil {
		t.Fatalf("Restored file failed SQLite validation: %v", err)
	}
}
