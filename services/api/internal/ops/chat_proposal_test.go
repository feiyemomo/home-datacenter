package ops

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

func setupOpsHandlerForTest(t *testing.T) (*OpsHandler, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	_ = db.AutoMigrate(&model.OpsChatSession{}, &model.OpsChatMessage{}, &model.OpsProposal{})
	db.Create(&model.OpsProposal{
		ProposalID:       "prop-101",
		Title:            "【高危】Nginx 基础镜像安全升级至 v1.25.4-alpine",
		Description:      "修复 CVE-2024-24989 缓冲区溢出漏洞，更新 TLS 1.3 密码套件配置与 HTTP/2 流控。",
		TargetComponent:  "home-web (Nginx)",
		RiskLevel:        model.RiskLevelHigh,
		EstimatedSeconds: 45,
		DowntimeSeconds:  3,
		ExecutionPlan:    "docker tag home-web:prev home-web:latest && docker-compose restart home-web",
		RollbackPlan:     "docker tag home-web:prev home-web:latest && docker-compose restart home-web",
		ActionSpec:       `{"action":"docker.restart","target":"home-web","rollback_spec":{"action":"docker.restart","target":"home-web"}}`,
		BaselineHash:     model.ComputeDefaultBaseline("home-web"),
		Status:           model.ProposalStatusAwaitingApproval,
		CreatedAt:        time.Now().Add(-2 * time.Hour),
	})
	db.Create(&model.OpsProposal{
		ProposalID:       "prop-102",
		Title:            "【中风险】SQLite / PostgreSQL 慢查询复合索引重构",
		Description:      "为 security_events (timestamp, event_type) 建立索引",
		TargetComponent:  "home-api (Database)",
		RiskLevel:        model.RiskLevelMedium,
		EstimatedSeconds: 90,
		DowntimeSeconds:  0,
		ExecutionPlan:    "CREATE INDEX IF NOT EXISTS idx_security_events_time_type ON security_events(created_at, type);",
		RollbackPlan:     "DROP INDEX IF EXISTS idx_security_events_time_type;",
		ActionSpec:       `{"action":"db.create_index","target":"home-api","params":{"table_name":"security_events","index_name":"idx_security_events_time_type","columns":["created_at","type"]},"rollback_spec":{"action":"db.drop_index","target":"home-api","params":{"index_name":"idx_security_events_time_type"}}}`,
		BaselineHash:     model.ComputeDefaultBaseline("home-api"),
		Status:           model.ProposalStatusAwaitingApproval,
		CreatedAt:        time.Now().Add(-1 * time.Hour),
	})

	bus := eventbus.New()
	fallback := NewHostFallbackProvider("", "")
	client := NewOnePanelClient("", "", "", "")

	handler := NewOpsHandler(client, fallback, db, bus, nil)

	r := gin.New()
	rg := r.Group("/api/v1/ops")
	// simulate JWTAuth middleware injecting claims for tests
	rg.Use(func(c *gin.Context) {
		c.Set("user_id", uint(1))
		c.Set("user_name", "tester")
		c.Set("is_admin", c.GetHeader("X-Admin") == "true")
		c.Next()
	})
	handler.RegisterRoutes(rg)
	handler.RegisterProposalRoutes(rg)
	handler.RegisterChatRoutes(rg)

	return handler, r
}

func TestListAndCreateProposals(t *testing.T) {
	_, r := setupOpsHandlerForTest(t)

	// 1. Initial GET should seed and return default proposals
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/ops/proposals", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var proposals []model.OpsProposal
	if err := json.Unmarshal(w.Body.Bytes(), &proposals); err != nil {
		t.Fatalf("failed to parse proposals json: %v", err)
	}

	if len(proposals) < 2 {
		t.Fatalf("expected at least 2 seeded proposals, got %d", len(proposals))
	}

	// 2. POST create proposal
	newProp := model.OpsProposal{
		ProposalID:       "prop-test-999",
		Title:            "Test Docker Base Image Upgrade",
		Description:      "Upgrade base image in sandbox",
		TargetComponent:  "home-web",
		RiskLevel:        model.RiskLevelHigh,
		EstimatedSeconds: 60,
		DowntimeSeconds:  5,
		ExecutionPlan:    "docker pull home-web:latest",
		RollbackPlan:     "docker tag home-web:backup home-web:latest",
	}
	body, _ := json.Marshal(newProp)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/proposals", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin", "true")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", w.Code, w.Body.String())
	}
}

func TestApproveProposalPermissionsAndWorkflow(t *testing.T) {
	_, r := setupOpsHandlerForTest(t)

	// 1. Attempt approval without admin -> 403 Forbidden
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-101/approve", strings.NewReader(`{"action":"now"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for non-admin, got %d", w.Code)
	}

	// 2. Admin approves immediate execution
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-101/approve", strings.NewReader(`{"action":"now"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin", "true")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for admin approval, got %d: %s", w.Code, w.Body.String())
	}

	var approvedProp model.OpsProposal
	if err := json.Unmarshal(w.Body.Bytes(), &approvedProp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if approvedProp.Status != model.ProposalStatusApproved {
		t.Errorf("expected status APPROVED, got %s", approvedProp.Status)
	}
	if !approvedProp.ApprovedAt.Valid {
		t.Errorf("expected valid ApprovedAt timestamp")
	}

	// 3. Attempt approving again -> 400 Bad Request (invalid transition)
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-101/approve", strings.NewReader(`{"action":"now"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin", "true")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on duplicate approval, got %d", w.Code)
	}

	// 4. Admin schedules proposal for low-peak window
	w = httptest.NewRecorder()
	schedReq := `{"action":"schedule","schedule_time":"03:00"}`
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-102/approve", strings.NewReader(schedReq))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin", "true")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for scheduled approval, got %d: %s", w.Code, w.Body.String())
	}

	var schedProp model.OpsProposal
	if err := json.Unmarshal(w.Body.Bytes(), &schedProp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if schedProp.Status != model.ProposalStatusScheduled {
		t.Errorf("expected status SCHEDULED, got %s", schedProp.Status)
	}
	if schedProp.ScheduledAt == nil || *schedProp.ScheduledAt != "03:00" {
		t.Errorf("expected scheduled_at 03:00, got %v", schedProp.ScheduledAt)
	}
}

func TestChatSessionsAndMessages(t *testing.T) {
	_, r := setupOpsHandlerForTest(t)

	// 1. GET /api/v1/ops/chat/sessions
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/ops/chat/sessions", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var sessions []model.OpsChatSession
	if err := json.Unmarshal(w.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("failed to parse sessions: %v", err)
	}
	if len(sessions) == 0 {
		t.Fatalf("expected at least 1 session, got 0")
	}

	// 2. POST /api/v1/ops/chat/messages with SSH attack question
	chatBody := `{"session_id":"session-ops-001","content":"今天服务器遭遇了哪些SSH暴力破解攻击？"}`
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/chat/messages", strings.NewReader(chatBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var chatResp ChatMessageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &chatResp); err != nil {
		t.Fatalf("failed to decode chat response: %v", err)
	}

	if chatResp.Role != "assistant" {
		t.Errorf("expected role assistant, got %s", chatResp.Role)
	}
	if !strings.Contains(chatResp.Content, "SSH") {
		t.Errorf("expected SSH in reply, got %s", chatResp.Content)
	}
	if chatResp.Reply != chatResp.Content {
		t.Errorf("expected Reply to match Content")
	}

	// 3. POST /api/v1/ops/chat/messages with proposal question
	chatBody2 := `{"session_id":"session-ops-001","message":"当前有哪些待审批的高危升级提案？"}`
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/chat/messages", strings.NewReader(chatBody2))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var chatResp2 ChatMessageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &chatResp2); err != nil {
		t.Fatalf("failed to decode chat response: %v", err)
	}
	if !strings.Contains(chatResp2.Content, "提案") {
		t.Errorf("expected 提案 in reply, got %s", chatResp2.Content)
	}

	// 4. POST /api/v1/ops/chat/messages with CVE question
	chatBody3 := `{"session_id":"session-ops-001","content":"有收到什么cve漏洞报告吗？"}`
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/chat/messages", strings.NewReader(chatBody3))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var chatResp3 ChatMessageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &chatResp3); err != nil {
		t.Fatalf("failed to decode chat response: %v", err)
	}
	if !strings.Contains(chatResp3.Content, "CVE") && !strings.Contains(chatResp3.Content, "漏洞") {
		t.Errorf("expected CVE / 漏洞 in reply, got: %s", chatResp3.Content)
	}

	// 5. POST /api/v1/ops/chat/messages with container resource question
	chatBody4 := `{"session_id":"session-ops-001","content":"查询容器资源消耗排行"}`
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/chat/messages", strings.NewReader(chatBody4))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var chatResp4 ChatMessageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &chatResp4); err != nil {
		t.Fatalf("failed to decode chat response: %v", err)
	}
	if !strings.Contains(chatResp4.Content, "核心容器资源消耗") {
		t.Errorf("expected 核心容器资源消耗 in reply, got: %s", chatResp4.Content)
	}
}

func TestProposalRejectsNonAdminWithoutContextElevation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	_ = db.AutoMigrate(&model.OpsChatSession{}, &model.OpsChatMessage{}, &model.OpsProposal{})
	db.Create(&model.OpsProposal{
		ProposalID:       "prop-101",
		Title:            "【高危】Nginx 基础镜像安全升级至 v1.25.4-alpine",
		Description:      "修复 CVE-2024-24989 缓冲区溢出漏洞，更新 TLS 1.3 密码套件配置与 HTTP/2 流控。",
		TargetComponent:  "home-web (Nginx)",
		RiskLevel:        model.RiskLevelHigh,
		EstimatedSeconds: 45,
		DowntimeSeconds:  3,
		ExecutionPlan:    "docker tag home-web:prev home-web:latest && docker-compose restart home-web",
		RollbackPlan:     "docker tag home-web:prev home-web:latest && docker-compose restart home-web",
		ActionSpec:       `{"action":"docker.restart","target":"home-web","rollback_spec":{"action":"docker.restart","target":"home-web"}}`,
		BaselineHash:     model.ComputeDefaultBaseline("home-web"),
		Status:           model.ProposalStatusAwaitingApproval,
		CreatedAt:        time.Now().Add(-2 * time.Hour),
	})
	db.Create(&model.OpsProposal{
		ProposalID:       "prop-102",
		Title:            "【中风险】SQLite / PostgreSQL 慢查询复合索引重构",
		Description:      "为 security_events (timestamp, event_type) 建立索引",
		TargetComponent:  "home-api (Database)",
		RiskLevel:        model.RiskLevelMedium,
		EstimatedSeconds: 90,
		DowntimeSeconds:  0,
		ExecutionPlan:    "CREATE INDEX IF NOT EXISTS idx_security_events_time_type ON security_events(created_at, type);",
		RollbackPlan:     "DROP INDEX IF EXISTS idx_security_events_time_type;",
		ActionSpec:       `{"action":"db.create_index","target":"home-api","params":{"table_name":"security_events","index_name":"idx_security_events_time_type","columns":["created_at","type"]},"rollback_spec":{"action":"db.drop_index","target":"home-api","params":{"index_name":"idx_security_events_time_type"}}}`,
		BaselineHash:     model.ComputeDefaultBaseline("home-api"),
		Status:           model.ProposalStatusAwaitingApproval,
		CreatedAt:        time.Now().Add(-1 * time.Hour),
	})

	bus := eventbus.New()
	fallback := NewHostFallbackProvider("", "")
	client := NewOnePanelClient("", "", "", "")
	handler := NewOpsHandler(client, fallback, db, bus, nil)

	// Router WITHOUT any admin context elevation (pure production route behavior)
	r := gin.New()
	rg := r.Group("/api/v1/ops")
	handler.RegisterProposalRoutes(rg)

	// Seed proposals
	wSeed := httptest.NewRecorder()
	reqSeed, _ := http.NewRequest(http.MethodGet, "/api/v1/ops/proposals", nil)
	r.ServeHTTP(wSeed, reqSeed)

	// Attacker sends X-Admin: true header attempting to approve proposal
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-101/approve", strings.NewReader(`{"action":"now"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin", "true")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden && w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 403 Forbidden or 401 Unauthorized when X-Admin header is supplied without context elevation, got %d: %s", w.Code, w.Body.String())
	}
}

