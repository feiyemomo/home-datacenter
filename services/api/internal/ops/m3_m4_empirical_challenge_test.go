package ops

import (
	"context"
	"encoding/json"
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

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// setupEmpiricalChallengeDB initializes an isolated in-memory SQLite database per test.
func setupEmpiricalChallengeDB(t *testing.T) (*gorm.DB, model.User, model.User) {
	dbName := fmt.Sprintf("file:chal_emp_%s_%d?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"), time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test sqlite db: %v", err)
	}
	err = db.AutoMigrate(
		&model.User{},
		&model.Device{},
		&model.OpsProposal{},
		&model.OpsExecutionHistory{},
		&model.SystemLog{},
	)
	if err != nil {
		t.Fatalf("failed to migrate db schema: %v", err)
	}

	adminUser := model.User{
		Name:    fmt.Sprintf("admin_%d", time.Now().UnixNano()),
		IsAdmin: true,
	}
	if err := db.Create(&adminUser).Error; err != nil {
		t.Fatalf("failed to create admin user: %v", err)
	}

	regularUser := model.User{
		Name:    fmt.Sprintf("reg_%d", time.Now().UnixNano()),
		IsAdmin: false,
	}
	if err := db.Create(&regularUser).Error; err != nil {
		t.Fatalf("failed to create regular user: %v", err)
	}

	return db, adminUser, regularUser
}

// mockChallengeExecutor records health check probes and execution dispatches.
type mockChallengeExecutor struct {
	mu               sync.Mutex
	healthCheckCalls []string
	healthCheckFunc  func(ctx context.Context, target string) error
	executeCalls     []OperationSpec
	executeFunc      func(ctx context.Context, spec OperationSpec) (*ExecutionResult, error)
	proposalCalls    []*model.OpsProposal
}

func (m *mockChallengeExecutor) Execute(ctx context.Context, spec OperationSpec) (*ExecutionResult, error) {
	m.mu.Lock()
	m.executeCalls = append(m.executeCalls, spec)
	m.mu.Unlock()
	if m.executeFunc != nil {
		return m.executeFunc(ctx, spec)
	}
	return &ExecutionResult{Success: true, Action: spec.Action, Target: spec.Target, Output: "mock executed"}, nil
}

func (m *mockChallengeExecutor) ExecuteRollback(ctx context.Context, spec OperationSpec) error {
	return nil
}

func (m *mockChallengeExecutor) ExecuteProposal(ctx context.Context, proposal *model.OpsProposal) (*ExecutionResult, error) {
	m.mu.Lock()
	m.proposalCalls = append(m.proposalCalls, proposal)
	m.mu.Unlock()
	proposal.Status = model.ProposalStatusExecuted
	proposal.ExecutionLog = "mock proposal executed"
	return &ExecutionResult{Success: true, Action: proposal.ActionSpec, Target: proposal.TargetComponent, Output: "mock executed"}, nil
}

func (m *mockChallengeExecutor) ValidateComponent(target string) (string, error) {
	return ResolveComponent(target)
}

func (m *mockChallengeExecutor) CheckHealth(ctx context.Context, target string) error {
	m.mu.Lock()
	m.healthCheckCalls = append(m.healthCheckCalls, target)
	m.mu.Unlock()
	if m.healthCheckFunc != nil {
		return m.healthCheckFunc(ctx, target)
	}
	return nil
}

// ----------------------------------------------------------------------------
// Challenge 1: ComputeFingerprint Determinism and Drift Detection
// ----------------------------------------------------------------------------
func TestChallengeEmpirical_ComputeFingerprint(t *testing.T) {
	db, _, _ := setupEmpiricalChallengeDB(t)
	bus := eventbus.New()

	t.Run("Offline and Uncreated Determinism Across Aliases", func(t *testing.T) {
		// Handler without 1Panel client or HostFallbackProvider: tests explicit non-prod offline placeholder
		handler := &OpsHandler{
			db:  db,
			bus: bus,
		}

		// 1. Deterministic across offline calls and strictly marked as offline placeholder
		hash1 := handler.ComputeFingerprint("home-web")
		hash2 := handler.ComputeFingerprint("home-web")
		expectedOffline := ComputeOfflinePlaceholderBaseline("home-web")
		if hash1 == "" || !IsOfflinePlaceholderBaseline("home-web", hash1) {
			t.Fatalf("expected non-empty offline placeholder, got: %s", hash1)
		}
		if IsRealBaselineHash("home-web", hash1) {
			t.Fatalf("offline placeholder must NOT satisfy IsRealBaselineHash: %s", hash1)
		}
		if hash1 != hash2 {
			t.Fatalf("expected deterministic hash across calls, got %s vs %s", hash1, hash2)
		}
		if hash1 != expectedOffline {
			t.Fatalf("expected %s, got %s", expectedOffline, hash1)
		}

		// 2. Canonical alias normalization
		aliases := []string{"home-web", "home-web (Nginx)", "web", "frontend", "nginx"}
		for _, alias := range aliases {
			h := handler.ComputeFingerprint(alias)
			if h != expectedOffline {
				t.Errorf("alias %s produced hash %s, expected %s", alias, h, expectedOffline)
			}
		}

		// 3. Different components produce distinct offline hashes
		hashAPI := handler.ComputeFingerprint("home-api")
		if hashAPI == hash1 {
			t.Fatalf("home-api and home-web produced collision: %s", hashAPI)
		}

		// 4. Empty and whitespace components return empty string
		if handler.ComputeFingerprint("") != "" {
			t.Errorf("expected empty string for empty component")
		}
		if handler.ComputeFingerprint("   ") != "" {
			t.Errorf("expected empty string for whitespace component")
		}
	})

	t.Run("Host Fallback Provider Mode Determinism", func(t *testing.T) {
		// Without live Docker socket or 1Panel, ExtractBaseline MUST NOT fabricate static catalog hashes;
		// in non-production it emits an explicit offline placeholder so real vs placeholder stays distinct.
		fallback := NewHostFallbackProvider("", "")
		handler := &OpsHandler{
			fallback: fallback,
			db:       db,
			bus:      bus,
		}

		hashFallback := handler.ComputeFingerprint("home-web")
		expectedOffline := ComputeOfflinePlaceholderBaseline("home-web")
		if hashFallback != expectedOffline {
			t.Fatalf("expected explicit offline placeholder %s, got: %s", expectedOffline, hashFallback)
		}
		if IsRealBaselineHash("home-web", hashFallback) {
			t.Fatalf("offline placeholder must not pass IsRealBaselineHash: %s", hashFallback)
		}

		// Unknown / non-whitelisted component MUST Fail-Closed to empty string (R4-17)
		hashUncreated := handler.ComputeFingerprint("uncreated-service")
		if hashUncreated != "" {
			t.Fatalf("expected empty Fail-Closed fingerprint for unknown component, got %s", hashUncreated)
		}
	})

	t.Run("Live 1Panel State and Drift Sensitivity", func(t *testing.T) {
		var mu sync.Mutex
		containerData := onePanelContainerItem{
			ContainerID: "cid-12345",
			Name:        "/home-web",
			State:       "running",
			Status:      "Up 3 hours",
			Image:       "home-web:v1.0.0",
			Created:     "2026-11-01T00:00:00Z",
		}
		returnEmptyList := false

		// Mock 1Panel server
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()

			if strings.Contains(r.URL.Path, "/api/v2/containers/search") {
				var items []onePanelContainerItem
				if !returnEmptyList {
					items = append(items, containerData)
				}
				resp := map[string]interface{}{
					"code":    200,
					"message": "OK",
					"data": map[string]interface{}{
						"items": items,
						"total": len(items),
					},
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(resp)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		mockClient := NewOnePanelClient(server.URL, "btpanel", "mock_user", "mock_pass")
		mockClient.psession = "mock_session_token"
		mockClient.lastLoginAt = time.Now()
		mockClient.loginTTL = 1 * time.Hour

		// Omit fallback provider so 1Panel response dictates container state directly
		handler := &OpsHandler{
			client: mockClient,
			db:     db,
			bus:    bus,
		}

		// 1. Initial Baseline Protocol v1 hash
		hashBase := handler.ComputeFingerprint("home-web")
		if !IsRealBaselineHashV1(hashBase, "home-web") {
			t.Fatalf("invalid initial baseline hash: %s", hashBase)
		}

		comp, compErr := ComputeContainerBaselineFromInspect("home-web", map[string]interface{}{
			"Id":      containerData.ContainerID,
			"Name":    containerData.Name,
			"Created": containerData.Created,
			"Config":  map[string]interface{}{"Image": containerData.Image},
			"State":   map[string]interface{}{"Status": containerData.State},
		})
		if compErr != nil {
			t.Fatalf("ComputeContainerBaselineFromInspect failed: %v", compErr)
		}
		if hashBase != comp.Hash {
			t.Fatalf("expected baseline hash %s, got %s", comp.Hash, hashBase)
		}

		// Re-verification of determinism
		hashBase2 := handler.ComputeFingerprint("home-web")
		if hashBase != hashBase2 {
			t.Fatalf("drift detected without state change: %s vs %s", hashBase, hashBase2)
		}

		// 2. Drift Image Tag
		mu.Lock()
		containerData.Image = "home-web:v1.1.0"
		mu.Unlock()

		hashDriftImage := handler.ComputeFingerprint("home-web")
		if hashDriftImage == hashBase {
			t.Fatalf("fingerprint FAILED to detect image tag drift! Both hashes: %s", hashBase)
		}

		// 3. Drift State (e.g. from running to exited)
		mu.Lock()
		containerData.Image = "home-web:v1.0.0" // restore image
		containerData.State = "exited"
		mu.Unlock()

		hashDriftState := handler.ComputeFingerprint("home-web")
		if hashDriftState == hashBase {
			t.Fatalf("fingerprint FAILED to detect container state drift! Both hashes: %s", hashBase)
		}

		// 4. Drift CreatedAt (container recreated)
		mu.Lock()
		containerData.State = "running" // restore state
		containerData.Created = "2026-11-08T12:00:00Z"
		mu.Unlock()

		hashDriftRecreated := handler.ComputeFingerprint("home-web")
		if hashDriftRecreated == hashBase {
			t.Fatalf("fingerprint FAILED to detect container recreation drift! Both hashes: %s", hashBase)
		}

		// 5. Container destroyed / omitted by live 1Panel -> MUST FAIL CLOSED (empty hash, no offline fallback)
		mu.Lock()
		returnEmptyList = true
		mu.Unlock()

		hashDriftMissing := handler.ComputeFingerprint("home-web")
		if hashDriftMissing == hashBase {
			t.Fatalf("fingerprint FAILED to detect container destruction! Both hashes: %s", hashBase)
		}
		if hashDriftMissing != "" {
			t.Fatalf("expected fail-closed empty hash when live 1Panel omits container, got: %s", hashDriftMissing)
		}
	})
}

// ----------------------------------------------------------------------------
// Challenge 2: Anti-TOCTOU Drift Detection (HTTP 409 Conflict vs 200 OK)
// ----------------------------------------------------------------------------
func TestChallengeEmpirical_AntiTOCTOU_DriftDetection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, adminUser, _ := setupEmpiricalChallengeDB(t)
	bus := eventbus.New()

	var mu sync.Mutex
	containerData := onePanelContainerItem{
		ContainerID: "cid-web-01",
		Name:        "/home-web",
		State:       "running",
		Image:       "home-web:v1.0.0",
		Created:     "2026-11-01T00:00:00Z",
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		resp := map[string]interface{}{
			"code":    200,
			"message": "OK",
			"data": map[string]interface{}{
				"items": []onePanelContainerItem{containerData},
				"total": 1,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	mockClient := NewOnePanelClient(server.URL, "btpanel", "mock_user", "mock_pass")
	mockClient.psession = "session_token"
	mockClient.lastLoginAt = time.Now()
	mockClient.loginTTL = 1 * time.Hour

	mockExec := &mockChallengeExecutor{}
	handler := &OpsHandler{
		client:   mockClient,
		db:       db,
		bus:      bus,
		executor: mockExec,
	}

	// Create Gin router with admin context
	r := gin.New()
	rg := r.Group("/api/v1/ops")
	rg.Use(func(c *gin.Context) {
		c.Set("user_id", adminUser.ID)
		c.Set("user_name", adminUser.Name)
		c.Set("is_admin", true)
		c.Next()
	})
	handler.RegisterProposalRoutes(rg)

	// Step 1: Compute initial baseline hash and create proposal
	initialHash := handler.ComputeFingerprint("home-web")
	prop := model.OpsProposal{
		ProposalID:       "prop-toctou-emp-001",
		Title:            "Restart home-web for Security Patch",
		TargetComponent:  "home-web",
		RiskLevel:        model.RiskLevelMedium,
		BaselineHash:     initialHash,
		Status:           model.ProposalStatusAwaitingApproval,
		ActionSpec:       `{"action":"docker.restart","target":"home-web"}`,
		EstimatedSeconds: 30,
		CreatedAt:        time.Now(),
	}
	if err := db.Create(&prop).Error; err != nil {
		t.Fatalf("failed to insert test proposal: %v", err)
	}

	// Step 2: Simulate state drift before administrator approval (TOCTOU)
	mu.Lock()
	containerData.Image = "home-web:v1.1.0-drifted"
	mu.Unlock()

	// Step 3: Attempt approval while drifted -> MUST return HTTP 409 Conflict
	w409 := httptest.NewRecorder()
	req409, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-toctou-emp-001/approve?sync=true", strings.NewReader(`{"action":"now"}`))
	req409.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w409, req409)

	if w409.Code != http.StatusConflict {
		t.Fatalf("expected HTTP 409 Conflict upon TOCTOU state drift, got: %d, body: %s", w409.Code, w409.Body.String())
	}

	var conflictResp map[string]interface{}
	if err := json.Unmarshal(w409.Body.Bytes(), &conflictResp); err != nil {
		t.Fatalf("failed to parse conflict response json: %v", err)
	}
	if !strings.Contains(fmt.Sprint(conflictResp["error"]), "baseline hash mismatch") {
		t.Errorf("expected error message to contain 'baseline hash mismatch', got: %v", conflictResp["error"])
	}
	if conflictResp["baseline_hash"] != initialHash {
		t.Errorf("expected baseline_hash %s, got: %v", initialHash, conflictResp["baseline_hash"])
	}

	// Verify executor was NEVER invoked
	if len(mockExec.proposalCalls) > 0 || len(mockExec.executeCalls) > 0 {
		t.Fatalf("executor MUST NOT be called when TOCTOU drift is detected")
	}

	// Verify proposal state in database remains strictly AWAITING_APPROVAL
	var dbPropAfterDrift model.OpsProposal
	if err := db.Where("proposal_id = ?", "prop-toctou-emp-001").First(&dbPropAfterDrift).Error; err != nil {
		t.Fatalf("failed to find proposal in db: %v", err)
	}
	if dbPropAfterDrift.Status != model.ProposalStatusAwaitingApproval {
		t.Fatalf("expected proposal status to remain %s, got: %s", model.ProposalStatusAwaitingApproval, dbPropAfterDrift.Status)
	}

	// Step 4: Re-align state to match baseline -> ApproveProposal MUST succeed with 200 OK
	mu.Lock()
	containerData.Image = "home-web:v1.0.0" // restore to baseline image
	mu.Unlock()

	w200 := httptest.NewRecorder()
	req200, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-toctou-emp-001/approve?sync=true", strings.NewReader(`{"action":"now"}`))
	req200.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w200, req200)

	if w200.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK when baseline hash matches, got: %d, body: %s", w200.Code, w200.Body.String())
	}

	// Verify executor WAS invoked
	if len(mockExec.proposalCalls) != 1 {
		t.Fatalf("expected exactly 1 proposal dispatch to executor, got: %d", len(mockExec.proposalCalls))
	}

	// Direct Executor Baseline Drift Check
	t.Run("Direct DefaultOperationExecutor Baseline Validation", func(t *testing.T) {
		currentFingerprint := "sha256:current_active_hash"
		exec := NewOperationExecutor(
			db, bus, nil, nil,
			WithBaselineExtractor(func(target string) string {
				return currentFingerprint
			}),
		)

		// Mismatch spec hash
		driftedSpec := OperationSpec{
			Action:       ActionDockerRestart,
			Target:       "home-web",
			BaselineHash: "sha256:old_stale_hash",
		}
		res, err := exec.Execute(context.Background(), driftedSpec)
		if !errors.Is(err, ErrBaselineMismatch) {
			t.Fatalf("expected ErrBaselineMismatch on executor.Execute, got: %v", err)
		}
		if res == nil || res.Success {
			t.Fatalf("expected unsuccessful ExecutionResult, got: %+v", res)
		}

		// Matching spec hash
		matchingSpec := OperationSpec{
			Action:       ActionDockerRestart,
			Target:       "home-web",
			BaselineHash: currentFingerprint,
		}
		execWithMockRunner := NewOperationExecutor(
			db, bus, nil, nil,
			WithBaselineExtractor(func(target string) string {
				return currentFingerprint
			}),
			WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
				return "restart succeeded", nil
			}),
			WithHealthChecker(func(ctx context.Context, target string) error {
				return nil
			}),
		)
		resMatch, errMatch := execWithMockRunner.Execute(context.Background(), matchingSpec)
		if errMatch != nil {
			t.Fatalf("expected success with matching baseline hash, got: %v", errMatch)
		}
		if resMatch == nil || !resMatch.Success {
			t.Fatalf("expected successful ExecutionResult, got: %+v", resMatch)
		}
	})
}

// ----------------------------------------------------------------------------
// Challenge 3: Proposal Status Forgery Attack
// ----------------------------------------------------------------------------
func TestChallengeEmpirical_ProposalStatusForgeryAttack(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, adminUser, _ := setupEmpiricalChallengeDB(t)
	bus := eventbus.New()

	// Fast mock client returning valid live containers in 0ms (live 1Panel returning empty items now fails closed per R5)
	fastServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		items := []onePanelContainerItem{
			{ContainerID: "cid-web", Name: "/home-web", State: "running", Status: "Up", Image: "home-web:v1.0.0", Created: "2026-11-01T00:00:00Z"},
			{ContainerID: "cid-api", Name: "/home-api", State: "running", Status: "Up", Image: "home-api:v1.0.0", Created: "2026-11-01T00:00:00Z"},
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code":    200,
			"message": "OK",
			"data": map[string]interface{}{
				"items": items,
				"total": len(items),
			},
		})
	}))
	defer fastServer.Close()

	fastClient := NewOnePanelClient(fastServer.URL, "btpanel", "mock_user", "mock_pass")
	fastClient.psession = "mock_token"
	fastClient.lastLoginAt = time.Now()
	fastClient.loginTTL = 1 * time.Hour

	handler := &OpsHandler{
		client: fastClient,
		db:     db,
		bus:    bus,
	}

	r := gin.New()
	rg := r.Group("/api/v1/ops")
	rg.Use(func(c *gin.Context) {
		c.Set("user_id", adminUser.ID)
		c.Set("user_name", adminUser.Name)
		c.Set("is_admin", true)
		c.Next()
	})
	handler.RegisterProposalRoutes(rg)

	t.Run("Forged APPROVED and approved_by payload rejection", func(t *testing.T) {
		forgedPayload := `{
			"title": "Privilege Escalation Injection",
			"target_component": "home-web",
			"risk_level": "CRITICAL",
			"status": "APPROVED",
			"approved_by": "hacker_overlord",
			"execution_log": "injected execution log",
			"action_spec": "{\"action\":\"docker.restart\",\"target\":\"home-web\"}"
		}`

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals", strings.NewReader(forgedPayload))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("expected HTTP 201 Created on proposal creation, got %d: %s", w.Code, w.Body.String())
		}

		// 1. Verify JSON response forces AWAITING_APPROVAL and zeroes forged fields
		var createdResp model.OpsProposal
		if err := json.Unmarshal(w.Body.Bytes(), &createdResp); err != nil {
			t.Fatalf("failed to decode created proposal json: %v", err)
		}

		if createdResp.Status != model.ProposalStatusAwaitingApproval {
			t.Fatalf("VULNERABILITY DETECTED! Status was NOT forced to AWAITING_APPROVAL, got: %s", createdResp.Status)
		}
		if createdResp.ApprovedBy != "" {
			t.Fatalf("VULNERABILITY DETECTED! approved_by was NOT cleared, got: %s", createdResp.ApprovedBy)
		}
		if createdResp.ExecutionLog != "" {
			t.Fatalf("VULNERABILITY DETECTED! execution_log was NOT cleared, got: %s", createdResp.ExecutionLog)
		}

		// 2. Verify Database record is strictly sanitized
		var dbRecord model.OpsProposal
		if err := db.Where("proposal_id = ?", createdResp.ProposalID).First(&dbRecord).Error; err != nil {
			t.Fatalf("failed to query created proposal from DB: %v", err)
		}
		if dbRecord.Status != model.ProposalStatusAwaitingApproval {
			t.Errorf("DB record status vulnerability: expected %s, got: %s", model.ProposalStatusAwaitingApproval, dbRecord.Status)
		}
		if dbRecord.ApprovedBy != "" {
			t.Errorf("DB record approved_by vulnerability: expected empty string, got: %s", dbRecord.ApprovedBy)
		}
		if dbRecord.ExecutionLog != "" {
			t.Errorf("DB record execution_log vulnerability: expected empty string, got: %s", dbRecord.ExecutionLog)
		}
		if dbRecord.ApprovedAt.Valid {
			t.Errorf("DB record approved_at should be invalid/null, got: %v", dbRecord.ApprovedAt)
		}
		if dbRecord.ExecutedAt.Valid {
			t.Errorf("DB record executed_at should be invalid/null, got: %v", dbRecord.ExecutedAt)
		}
	})

	t.Run("Multi-Status Forgery Attack Matrix", func(t *testing.T) {
		forgedStatuses := []string{
			"EXECUTED",
			"SCHEDULED",
			"FAILED_ROLLEDBACK",
			"RUNNING",
			"COMPLETED",
			"CANCELLED",
			"RANDOM_INVALID_STATE",
		}

		for _, statusToForge := range forgedStatuses {
			time.Sleep(2 * time.Millisecond) // Avoid sub-millisecond ProposalID collision
			payload := fmt.Sprintf(`{
				"title": "Matrix Forgery %s",
				"target_component": "home-api",
				"status": "%s",
				"approved_by": "attacker"
			}`, statusToForge, statusToForge)

			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals", strings.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)

			if w.Code != http.StatusCreated {
				t.Fatalf("expected 201 Created for status %s, got %d: %s", statusToForge, w.Code, w.Body.String())
			}

			var resp model.OpsProposal
			_ = json.Unmarshal(w.Body.Bytes(), &resp)

			if resp.Status != model.ProposalStatusAwaitingApproval {
				t.Fatalf("forgery attack succeeded for status %s! Got: %s", statusToForge, resp.Status)
			}
			if resp.ApprovedBy != "" {
				t.Fatalf("approved_by leak on status %s: got %s", statusToForge, resp.ApprovedBy)
			}
		}
	})

	t.Run("Re-Approval on Non-Awaiting Proposal Rejected with 400 Bad Request", func(t *testing.T) {
		// Insert an already EXECUTED proposal into DB
		executedProp := model.OpsProposal{
			ProposalID:       "prop-already-executed-001",
			Title:            "Already Executed Proposal",
			TargetComponent:  "home-web",
			Status:           model.ProposalStatusExecuted,
			ActionSpec:       `{"action":"docker.restart","target":"home-web"}`,
			EstimatedSeconds: 30,
			CreatedAt:        time.Now(),
		}
		if err := db.Create(&executedProp).Error; err != nil {
			t.Fatalf("failed to insert proposal: %v", err)
		}

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-already-executed-001/approve", strings.NewReader(`{"action":"now"}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected HTTP 400 Bad Request when approving non-Awaiting proposal, got %d: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "Invalid state transition from EXECUTED") {
			t.Errorf("expected state transition error message, got: %s", w.Body.String())
		}
	})

	t.Run("Sub-Millisecond ProposalID Collision Risk Under Rapid Ingestion", func(t *testing.T) {
		// When two proposals are submitted within the same millisecond without an explicit proposal_id,
		// fmt.Sprintf("prop-%d", time.Now().UnixNano()/1e6) generates identical IDs,
		// triggering UNIQUE constraint violation on pending_proposals.proposal_id.
		var collisionDetected bool
		for i := 0; i < 25; i++ {
			w1 := httptest.NewRecorder()
			req1, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals", strings.NewReader(`{"title":"Rapid 1","target_component":"home-api"}`))
			req1.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w1, req1)

			w2 := httptest.NewRecorder()
			req2, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals", strings.NewReader(`{"title":"Rapid 2","target_component":"home-api"}`))
			req2.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w2, req2)

			if w2.Code == http.StatusInternalServerError && strings.Contains(w2.Body.String(), "UNIQUE constraint failed") {
				collisionDetected = true
				break
			}
		}
		t.Logf("Empirical finding: Sub-millisecond ProposalID collision observed = %v (confirms UnixNano()/1e6 truncation vulnerability)", collisionDetected)
	})
}

// ----------------------------------------------------------------------------
// Challenge 4: TopologyEngine Live Health Probe Execution
// ----------------------------------------------------------------------------
func TestChallengeEmpirical_TopologyEngine_LiveHealthProbe(t *testing.T) {
	db, _, _ := setupEmpiricalChallengeDB(t)
	bus := eventbus.New()
	ctx := context.Background()

	t.Run("Live Health Checker Probes Upstream Dependencies Actively", func(t *testing.T) {
		mockExec := &mockChallengeExecutor{
			healthCheckFunc: func(ctx context.Context, target string) error {
				// All components healthy initially
				return nil
			},
		}

		fsm := NewContainerRecoveryFSM(db, bus, mockExec)

		// Trigger crash on Level 2 container (home-web)
		// Dependencies: Level 1 (home-api) -> Level 0 (mosquitto, frigate, app.db)
		crashEvent := ContainerEvent{
			ContainerName: "home-web",
			Action:        "die",
			ExitCode:      1,
		}

		decision, err := fsm.ProcessContainerEvent(ctx, crashEvent)
		if err != nil {
			t.Fatalf("expected healthy preflight check to pass, got: %v", err)
		}
		if decision.Action != ActionRestart {
			t.Fatalf("expected ActionRestart, got: %s", decision.Action)
		}

		// Verify executor.CheckHealth was actively called
		mockExec.mu.Lock()
		calls := append([]string{}, mockExec.healthCheckCalls...)
		mockExec.mu.Unlock()

		if len(calls) == 0 {
			t.Fatalf("executor.CheckHealth was NEVER called by TopologyEngine!")
		}

		// Verify direct and transitive dependencies were probed
		foundHomeAPI := false
		foundMosquitto := false
		foundFrigate := false

		for _, call := range calls {
			canonical := ResolveTopologyComponent(call)
			if canonical == "home-api" {
				foundHomeAPI = true
			}
			if canonical == "mosquitto" {
				foundMosquitto = true
			}
			if canonical == "frigate" {
				foundFrigate = true
			}
		}

		if !foundHomeAPI {
			t.Errorf("expected direct dependency 'home-api' to be probed, calls: %v", calls)
		}
		if !foundMosquitto {
			t.Errorf("expected transitive dependency 'mosquitto' to be probed, calls: %v", calls)
		}
		if !foundFrigate {
			t.Errorf("expected transitive dependency 'frigate' to be probed, calls: %v", calls)
		}
	})

	t.Run("Direct Dependency Failure Rejection", func(t *testing.T) {
		mockExec := &mockChallengeExecutor{
			healthCheckFunc: func(ctx context.Context, target string) error {
				canonical := ResolveTopologyComponent(target)
				if canonical == "home-api" {
					return errors.New("connection refused :8080")
				}
				return nil
			},
		}

		fsm := NewContainerRecoveryFSM(db, bus, mockExec)

		crashEvent := ContainerEvent{
			ContainerName: "home-web", // depends directly on home-api
			Action:        "die",
			ExitCode:      1,
		}

		decision, err := fsm.ProcessContainerEvent(ctx, crashEvent)
		if err == nil {
			t.Fatalf("expected error when direct dependency home-api is unhealthy, got nil")
		}
		if decision.Action != ActionBlocked {
			t.Fatalf("expected ActionBlocked for home-web, got: %s", decision.Action)
		}
		if !strings.Contains(err.Error(), "upstream dependency home-api is unhealthy") {
			t.Errorf("expected error message to mention 'home-api is unhealthy', got: %v", err)
		}
	})

	t.Run("Transitive Dependency Failure Rejection", func(t *testing.T) {
		mockExec := &mockChallengeExecutor{
			healthCheckFunc: func(ctx context.Context, target string) error {
				canonical := ResolveTopologyComponent(target)
				if canonical == "mosquitto" {
					return errors.New("mqtt broker down")
				}
				return nil // home-api and frigate report healthy
			},
		}

		fsm := NewContainerRecoveryFSM(db, bus, mockExec)

		crashEvent := ContainerEvent{
			ContainerName: "home-web", // depends on home-api which depends on mosquitto
			Action:        "die",
			ExitCode:      1,
		}

		decision, err := fsm.ProcessContainerEvent(ctx, crashEvent)
		if err == nil {
			t.Fatalf("expected error when transitive dependency mosquitto is unhealthy, got nil")
		}
		if decision.Action != ActionBlocked {
			t.Fatalf("expected ActionBlocked for home-web, got: %s", decision.Action)
		}
		if !strings.Contains(err.Error(), "upstream dependency mosquitto") {
			t.Errorf("expected error message to mention 'mosquitto', got: %v", err)
		}
	})

	t.Run("Database Ping Health Failure Rejection", func(t *testing.T) {
		// Close underlying sql.DB to force Ping failure
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatalf("failed to get sql.DB: %v", err)
		}
		_ = sqlDB.Close()

		mockExec := &mockChallengeExecutor{
			healthCheckFunc: func(ctx context.Context, target string) error {
				return nil
			},
		}

		fsm := NewContainerRecoveryFSM(db, bus, mockExec)

		crashEvent := ContainerEvent{
			ContainerName: "home-api", // depends on app.db
			Action:        "die",
			ExitCode:      1,
		}

		decision, err := fsm.ProcessContainerEvent(ctx, crashEvent)
		if err == nil {
			t.Fatalf("expected error when app.db ping fails, got nil")
		}
		if decision.Action != ActionBlocked {
			t.Fatalf("expected ActionBlocked for home-api when database ping fails, got: %s", decision.Action)
		}
		if !strings.Contains(err.Error(), "app.db is unhealthy") {
			t.Errorf("expected error message to mention 'app.db is unhealthy', got: %v", err)
		}
	})
}
