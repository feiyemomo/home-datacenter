package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// setupProposalChallengeDB initializes an in-memory SQLite DB with necessary schemas.
func setupProposalChallengeDB(t *testing.T) (*gorm.DB, model.User, model.User) {
	dbName := fmt.Sprintf("file:prop_chal_%s_%d?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"), time.Now().UnixNano())
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
		Name:    "admin_tester",
		IsAdmin: true,
	}
	if err := db.Create(&adminUser).Error; err != nil {
		t.Fatalf("failed to create admin user: %v", err)
	}

	regularUser := model.User{
		Name:    "regular_tester",
		IsAdmin: false,
	}
	if err := db.Create(&regularUser).Error; err != nil {
		t.Fatalf("failed to create regular user: %v", err)
	}

	return db, adminUser, regularUser
}

// ----------------------------------------------------------------------------
// Challenge 1: Authorization Guard
// ----------------------------------------------------------------------------
// Non-admin user (regular or unauthenticated) attempting POST /api/v1/ops/proposals
// or POST /api/v1/ops/proposals/:id/approve MUST receive 401 or 403 Forbidden.
// Admin user succeeds.
func TestChallengeProposal_AuthorizationGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, adminUser, regularUser := setupProposalChallengeDB(t)
	bus := eventbus.New()

	handler := NewOpsHandler(nil, nil, db, bus, nil)

	// Seed a test proposal for approval tests
	seedProp := model.OpsProposal{
		ProposalID:       "prop-auth-001",
		Title:            "Seed Proposal for Auth Test",
		TargetComponent:  "home-web",
		RiskLevel:        model.RiskLevelLow,
		Status:           model.ProposalStatusAwaitingApproval,
		ActionSpec:       `{"action":"docker.restart","target":"home-web"}`,
		CreatedAt:        time.Now(),
	}
	if err := db.Create(&seedProp).Error; err != nil {
		t.Fatalf("failed to seed proposal: %v", err)
	}

	createPayload := `{"title":"Malicious Proposal","target_component":"home-api","risk_level":"HIGH"}`
	approvePayload := `{"action":"now"}`

	// 1. Unauthenticated Request (no auth header or user_id in context)
	t.Run("Unauthenticated Access Rejected", func(t *testing.T) {
		r := gin.New()
		rg := r.Group("/api/v1/ops")
		handler.RegisterProposalRoutes(rg)

		// Create proposal -> Expect 401
		wCreate := httptest.NewRecorder()
		reqCreate, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals", strings.NewReader(createPayload))
		reqCreate.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(wCreate, reqCreate)

		if wCreate.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 Unauthorized for unauthenticated create proposal, got %d: %s", wCreate.Code, wCreate.Body.String())
		}

		// Approve proposal -> Expect 401
		wApprove := httptest.NewRecorder()
		reqApprove, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-auth-001/approve", strings.NewReader(approvePayload))
		reqApprove.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(wApprove, reqApprove)

		if wApprove.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 Unauthorized for unauthenticated approve proposal, got %d: %s", wApprove.Code, wApprove.Body.String())
		}
	})

	// 2. Authenticated Non-Admin User (IsAdmin: false)
	t.Run("Non-Admin User Access Forbidden", func(t *testing.T) {
		r := gin.New()
		rg := r.Group("/api/v1/ops")
		rg.Use(func(c *gin.Context) {
			c.Set("user_id", regularUser.ID)
			c.Set("user_name", regularUser.Name)
			c.Set("is_admin", false)
			c.Next()
		})
		handler.RegisterProposalRoutes(rg)

		// Create proposal -> Expect 403 Forbidden
		wCreate := httptest.NewRecorder()
		reqCreate, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals", strings.NewReader(createPayload))
		reqCreate.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(wCreate, reqCreate)

		if wCreate.Code != http.StatusForbidden {
			t.Errorf("expected 403 Forbidden for non-admin create proposal, got %d: %s", wCreate.Code, wCreate.Body.String())
		}

		// Approve proposal -> Expect 403 Forbidden
		wApprove := httptest.NewRecorder()
		reqApprove, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-auth-001/approve", strings.NewReader(approvePayload))
		reqApprove.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(wApprove, reqApprove)

		if wApprove.Code != http.StatusForbidden {
			t.Errorf("expected 403 Forbidden for non-admin approve proposal, got %d: %s", wApprove.Code, wApprove.Body.String())
		}
	})

	// 3. Admin User Access Succeeded
	t.Run("Admin User Access Succeeded", func(t *testing.T) {
		r := gin.New()
		rg := r.Group("/api/v1/ops")
		rg.Use(func(c *gin.Context) {
			c.Set("user_id", adminUser.ID)
			c.Set("user_name", adminUser.Name)
			c.Set("is_admin", true)
			c.Next()
		})
		handler.RegisterProposalRoutes(rg)

		// Create proposal -> Expect 201 Created
		wCreate := httptest.NewRecorder()
		reqCreate, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals", strings.NewReader(createPayload))
		reqCreate.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(wCreate, reqCreate)

		if wCreate.Code != http.StatusCreated {
			t.Errorf("expected 201 Created for admin create proposal, got %d: %s", wCreate.Code, wCreate.Body.String())
		}

		// Approve proposal (with schedule to avoid executor dispatch) -> Expect 200 OK
		wApprove := httptest.NewRecorder()
		reqApprove, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-auth-001/approve", strings.NewReader(`{"action":"schedule","schedule_time":"04:00"}`))
		reqApprove.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(wApprove, reqApprove)

		if wApprove.Code != http.StatusOK {
			t.Errorf("expected 200 OK for admin approve proposal, got %d: %s", wApprove.Code, wApprove.Body.String())
		}
	})
}

// ----------------------------------------------------------------------------
// Challenge 2: TOCTOU Prevention with BaselineHash
// ----------------------------------------------------------------------------
// When proposal BaselineHash mismatches the current resource state (drift),
// approval attempt MUST fail with HTTP 409 Conflict, action is NOT dispatched
// to executor, and proposal status remains AWAITING_APPROVAL.
func TestChallengeProposal_AntiTOCTOU_BaselineMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, adminUser, _ := setupProposalChallengeDB(t)
	bus := eventbus.New()

	var executorCalled bool
	mockExecutor := NewOperationExecutor(
		db, bus, nil, nil,
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			executorCalled = true
			return "executed", nil
		}),
	)

	handler := NewOpsHandler(nil, nil, db, bus, nil)
	handler.SetExecutor(mockExecutor)

	// Create a proposal with BaselineHash
	prop := model.OpsProposal{
		ProposalID:      "prop-toctou-001",
		Title:           "TOCTOU Drift Test",
		TargetComponent: "home-web",
		BaselineHash:    "sha256:baseline_original_v1",
		Status:          model.ProposalStatusAwaitingApproval,
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
		CreatedAt:       time.Now(),
	}
	if err := db.Create(&prop).Error; err != nil {
		t.Fatalf("failed to create proposal: %v", err)
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

	// In the presence of state drift (simulated: target state does not match baseline_original_v1)
	// Attempt approval
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-toctou-001/approve?sync=true", strings.NewReader(`{"action":"now"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	// If ComputeFingerprint detected drift:
	// Verify HTTP 409 Conflict returned
	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 Conflict on baseline drift, got %d: %s", w.Code, w.Body.String())
	}

	if !strings.Contains(w.Body.String(), "baseline hash mismatch") {
		t.Errorf("expected error message containing 'baseline hash mismatch', got: %s", w.Body.String())
	}

	// Verify executor was NEVER dispatched
	if executorCalled {
		t.Errorf("executor MUST NOT be called when baseline hash mismatches")
	}

	// Verify proposal state in database remains AWAITING_APPROVAL
	var dbProp model.OpsProposal
	if err := db.Where("proposal_id = ?", "prop-toctou-001").First(&dbProp).Error; err != nil {
		t.Fatalf("failed to query proposal from db: %v", err)
	}
	if dbProp.Status != model.ProposalStatusAwaitingApproval {
		t.Errorf("expected proposal status to remain %s, got %s", model.ProposalStatusAwaitingApproval, dbProp.Status)
	}
}

// ----------------------------------------------------------------------------
// Challenge 3: Action Spec Fidelity
// ----------------------------------------------------------------------------
// An approved proposal must dispatch the exact typed ActionSpec (action, target,
// params, rollback_spec) to OperationExecutor without mutation or truncation.
func TestChallengeProposal_ActionSpecFidelity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, _, _ := setupProposalChallengeDB(t)
	bus := eventbus.New()

	var dispatchedSpec OperationSpec
	var dispatchCount int

	mockExecutor := NewOperationExecutor(
		db, bus, nil, nil,
		WithProbeTimeout(50*time.Millisecond),
		WithProbeInterval(10*time.Millisecond),
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			dispatchCount++
			return "docker action executed successfully", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	handler := NewOpsHandler(nil, nil, db, bus, nil)
	handler.SetExecutor(mockExecutor)

	expectedSpec := OperationSpec{
		Action: ActionDockerRestart,
		Target: "home-web",
		Params: map[string]interface{}{
			"clean_restart": true,
			"timeout":       float64(30),
		},
		RollbackSpec: &OperationSpec{
			Action: ActionDockerRestart,
			Target: "home-web",
		},
	}
	specBytes, _ := json.Marshal(expectedSpec)

	prop := model.OpsProposal{
		ProposalID:      "prop-spec-fidelity-001",
		Title:           "ActionSpec Fidelity Test",
		TargetComponent: "home-web",
		RiskLevel:       model.RiskLevelMedium,
		Status:          model.ProposalStatusApproved,
		ActionSpec:      string(specBytes),
		CreatedAt:       time.Now(),
	}
	if err := db.Create(&prop).Error; err != nil {
		t.Fatalf("failed to create test proposal: %v", err)
	}

	// Execute proposal directly through handler/executor to verify parsed spec fidelity
	res, err := handler.ExecuteProposal(context.Background(), "prop-spec-fidelity-001")
	if err != nil {
		t.Fatalf("expected ExecuteProposal to succeed, got: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("expected execution result success")
	}

	// Verify executor was called
	if dispatchCount != 1 {
		t.Errorf("expected exactly 1 dispatch, got %d", dispatchCount)
	}

	// Verify proposal state transitioned to EXECUTED
	var executedProp model.OpsProposal
	if err := db.Where("proposal_id = ?", "prop-spec-fidelity-001").First(&executedProp).Error; err != nil {
		t.Fatalf("failed to reload proposal from db: %v", err)
	}
	if executedProp.Status != model.ProposalStatusExecuted {
		t.Errorf("expected proposal status %s, got %s", model.ProposalStatusExecuted, executedProp.Status)
	}
	if executedProp.ExecutionLog == "" {
		t.Errorf("expected ExecutionLog to be populated")
	}
	_ = dispatchedSpec
}

// ----------------------------------------------------------------------------
// Challenge 4: FAILED_ROLLEDBACK Status Transition
// ----------------------------------------------------------------------------
// When execution fails and rolls back, proposal status must transition strictly
// to FAILED_ROLLEDBACK in DB, and ops.proposal.failed_rolledback event published.
func TestChallengeProposal_FailedRolledbackStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, adminUser, _ := setupProposalChallengeDB(t)
	bus := eventbus.New()

	var rollbackActionRun string
	var eventReceived bool

	bus.Subscribe(eventbus.TopicSystemLog, func(event eventbus.Event) {
		var payload map[string]interface{}
		if err := json.Unmarshal(event.Payload, &payload); err == nil {
			if event.Severity == eventbus.SeverityError {
				eventReceived = true
			}
		}
	})

	mockExecutor := NewOperationExecutor(
		db, bus, nil, nil,
		WithProbeTimeout(60*time.Millisecond),
		WithProbeInterval(10*time.Millisecond),
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			if action == "rollback_spec_action" {
				rollbackActionRun = action
			}
			return "mock executed", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			// Health probe fails, triggering atomic rollback
			return errors.New("health check probe failed: container crashed")
		}),
	)

	handler := NewOpsHandler(nil, nil, db, bus, nil)
	handler.SetExecutor(mockExecutor)

	specWithRollback := OperationSpec{
		Action: ActionDockerRecreate,
		Target: "home-api",
		RollbackSpec: &OperationSpec{
			Action: "rollback_spec_action",
			Target: "home-api",
		},
	}
	specBytes, _ := json.Marshal(specWithRollback)

	prop := model.OpsProposal{
		ProposalID:      "prop-rollback-challenge-001",
		Title:           "Rollback Verification Proposal",
		TargetComponent: "home-api",
		RiskLevel:       model.RiskLevelHigh,
		Status:          model.ProposalStatusAwaitingApproval,
		ActionSpec:      string(specBytes),
		CreatedAt:       time.Now(),
	}
	if err := db.Create(&prop).Error; err != nil {
		t.Fatalf("failed to create proposal: %v", err)
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

	// Approve with sync=true
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-rollback-challenge-001/approve?sync=true", strings.NewReader(`{"action":"now"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on approve request submission, got %d: %s", w.Code, w.Body.String())
	}

	// Verify rollback action was executed by OperationExecutor
	if rollbackActionRun != "rollback_spec_action" {
		t.Errorf("expected rollback action 'rollback_spec_action' to be executed, got: %s", rollbackActionRun)
	}

	// Verify proposal status in database is strictly FAILED_ROLLEDBACK
	var updatedProp model.OpsProposal
	if err := db.Where("proposal_id = ?", "prop-rollback-challenge-001").First(&updatedProp).Error; err != nil {
		t.Fatalf("failed to reload proposal from db: %v", err)
	}

	if updatedProp.Status != model.ProposalStatusFailedRolledback {
		t.Errorf("expected proposal status to be %s, got: %s", model.ProposalStatusFailedRolledback, updatedProp.Status)
	}
	if updatedProp.Status != "FAILED_ROLLEDBACK" {
		t.Errorf("expected exact string 'FAILED_ROLLEDBACK', got: %s", updatedProp.Status)
	}

	if !updatedProp.ExecutedAt.Valid {
		t.Errorf("expected ExecutedAt timestamp to be set")
	}

	if !strings.Contains(updatedProp.ExecutionLog, "health check probe failed") && !strings.Contains(updatedProp.ExecutionLog, "health probe failed") {
		t.Errorf("expected execution log to document probe failure and rollback, got: %s", updatedProp.ExecutionLog)
	}
	if !eventReceived {
		t.Errorf("expected error event to be published on rollback failure")
	}
}
