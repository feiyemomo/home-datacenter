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

// setupProposalSecurityDB initializes an isolated in-memory SQLite database for security tests.
func setupProposalSecurityDB(t *testing.T) (*gorm.DB, model.User) {
	t.Helper()
	dbName := fmt.Sprintf("file:prop_sec_%s_%d?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"), time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test sqlite db: %v", err)
	}
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
		_, _ = sqlDB.Exec("PRAGMA busy_timeout = 5000;")
	}

	err = db.AutoMigrate(
		&model.User{},
		&model.Household{},
		&model.OpsProposal{},
		&model.OpsExecutionHistory{},
		&model.SystemLog{},
	)
	if err != nil {
		t.Fatalf("failed to migrate db schema: %v", err)
	}

	adminUser := model.User{
		ID:          1,
		Name:        "security_admin",
		IsAdmin:     true,
		HouseholdID: 1,
	}
	if err := db.Create(&adminUser).Error; err != nil {
		t.Fatalf("failed to create admin user: %v", err)
	}

	return db, adminUser
}

// setupProposalSecurityRouter configures a Gin engine with admin session context.
func setupProposalSecurityRouter(handler *OpsHandler, admin model.User) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	rg := r.Group("/api/v1/ops")
	rg.Use(func(c *gin.Context) {
		c.Set("user_id", admin.ID)
		c.Set("user_name", admin.Name)
		c.Set("is_admin", admin.IsAdmin)
		c.Set("household_id", admin.HouseholdID)
		c.Next()
	})
	handler.RegisterProposalRoutes(rg)
	return r
}

// ----------------------------------------------------------------------------
// 1. Empty baseline proposal creation rejected (ErrEmptyBaseline)
// ----------------------------------------------------------------------------
func TestProposalSecurity_EmptyBaselineCreationRejected(t *testing.T) {
	db, admin := setupProposalSecurityDB(t)
	bus := eventbus.New()
	handler := NewOpsHandler(nil, nil, db, bus, nil)
	r := setupProposalSecurityRouter(handler, admin)

	// Case 1A: Proposal creation payload missing target_component and baseline_hash
	payloadNoTarget := `{"title":"Empty Target Proposal","risk_level":"LOW"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals", strings.NewReader(payloadNoTarget))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 Bad Request for proposal with empty baseline, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), ErrEmptyBaseline.Error()) && !strings.Contains(w.Body.String(), "baseline hash") {
		t.Errorf("expected error message to reference ErrEmptyBaseline, got: %s", w.Body.String())
	}

	// Verify no proposal persisted in DB
	var count int64
	db.Model(&model.OpsProposal{}).Count(&count)
	if count != 0 {
		t.Errorf("expected 0 proposals created in DB, got: %d", count)
	}
}

// ----------------------------------------------------------------------------
// 2. Empty baseline approval rejected (ErrEmptyBaseline)
// ----------------------------------------------------------------------------
func TestProposalSecurity_EmptyBaselineApprovalRejected(t *testing.T) {
	db, admin := setupProposalSecurityDB(t)
	bus := eventbus.New()
	handler := NewOpsHandler(nil, nil, db, bus, nil)
	r := setupProposalSecurityRouter(handler, admin)

	// Seed proposal with explicitly empty BaselineHash in database
	prop := model.OpsProposal{
		ProposalID:      "prop-empty-base-001",
		Title:           "Proposal without baseline",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusAwaitingApproval,
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
		CreatedAt:       time.Now(),
	}
	db.Create(&prop)
	// Explicitly clear baseline_hash in DB to simulate legacy/corrupted record
	db.Model(&model.OpsProposal{}).Where("id = ?", prop.ID).Update("baseline_hash", "")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-empty-base-001/approve", strings.NewReader(`{"action":"now"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 Bad Request for approving proposal with empty baseline, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), ErrEmptyBaseline.Error()) && !strings.Contains(w.Body.String(), "BaselineHash") {
		t.Errorf("expected error to mention empty baseline, got: %s", w.Body.String())
	}

	// Verify status remains AWAITING_APPROVAL in DB
	var reloaded model.OpsProposal
	db.First(&reloaded, prop.ID)
	if reloaded.Status != model.ProposalStatusAwaitingApproval {
		t.Errorf("expected status to remain AWAITING_APPROVAL, got: %s", reloaded.Status)
	}
}

// ----------------------------------------------------------------------------
// 3. ActionSpec BaselineHash override rejected (R5.3, R5.4)
// ----------------------------------------------------------------------------
func TestProposalSecurity_ActionSpecBaselineOverrideRejected(t *testing.T) {
	db, _ := setupProposalSecurityDB(t)
	bus := eventbus.New()

	approvedBaseline := "sha256:approved_baseline_1111111111111111111111111111111111111111111111111111111111111111"
	forgedBaseline := "sha256:forged_override_2222222222222222222222222222222222222222222222222222222222222222"

	actionRun := false
	executor := NewOperationExecutor(
		db, bus, nil, nil,
		WithBaselineExtractor(func(target string) string {
			return approvedBaseline
		}),
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			actionRun = true
			return "restarted", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	// ActionSpec attempts to supply its own baseline hash differing from proposal.BaselineHash
	specPayload := fmt.Sprintf(`{"action":"docker.restart","target":"home-web","baseline_hash":"%s"}`, forgedBaseline)
	prop := model.OpsProposal{
		ProposalID:      "prop-override-001",
		Title:           "Baseline Override Attempt",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusApproved,
		BaselineHash:    approvedBaseline,
		ActionSpec:      specPayload,
		CreatedAt:       time.Now(),
	}
	// Compute genuine ApprovalDigest for this proposal
	digest, err := ComputeProposalApprovalDigest(&prop)
	if err != nil {
		t.Fatalf("failed to compute digest: %v", err)
	}
	prop.ApprovalDigest = digest
	db.Create(&prop)

	_, execErr := executor.ExecuteProposal(context.Background(), &prop)
	if execErr == nil {
		t.Fatalf("expected ErrBaselineMismatch when ActionSpec attempts to override approved baseline, got nil")
	}
	if !errors.Is(execErr, ErrBaselineMismatch) {
		t.Fatalf("expected error wrapping ErrBaselineMismatch, got: %v", execErr)
	}
	if actionRun {
		t.Fatalf("security violation: docker runner executed despite baseline mismatch!")
	}
}

// ----------------------------------------------------------------------------
// 4. Pre-execution baseline mismatch (state changed after approval) rejected
// ----------------------------------------------------------------------------
func TestProposalSecurity_PreExecutionBaselineMismatch_StateChangedAfterApproval(t *testing.T) {
	db, _ := setupProposalSecurityDB(t)
	bus := eventbus.New()

	approvedBaseline := "sha256:original_state_1111111111111111111111111111111111111111111111111111111111111111"
	driftedState := "sha256:drifted_post_approval_333333333333333333333333333333333333333333333333"

	// Live state drifts between approval and execution!
	currentState := driftedState
	actionExecuted := false

	executor := NewOperationExecutor(
		db, bus, nil, nil,
		WithBaselineExtractor(func(target string) string {
			return currentState
		}),
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			actionExecuted = true
			return "restarted", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	prop := model.OpsProposal{
		ProposalID:      "prop-toctou-exec-001",
		Title:           "Pre-execution TOCTOU Test",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusApproved,
		BaselineHash:    approvedBaseline,
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
		CreatedAt:       time.Now(),
	}
	digest, err := ComputeProposalApprovalDigest(&prop)
	if err != nil {
		t.Fatalf("failed to compute digest: %v", err)
	}
	prop.ApprovalDigest = digest
	db.Create(&prop)

	_, execErr := executor.ExecuteProposal(context.Background(), &prop)
	if execErr == nil {
		t.Fatalf("expected ErrBaselineMismatch on pre-execution drift, got nil")
	}
	if !errors.Is(execErr, ErrBaselineMismatch) {
		t.Fatalf("expected ErrBaselineMismatch, got: %v", execErr)
	}
	if actionExecuted {
		t.Fatalf("security violation: action executed on drifted container!")
	}

	// Verify proposal state in database was NOT moved to EXECUTED
	var reloaded model.OpsProposal
	db.First(&reloaded, prop.ID)
	if reloaded.Status == model.ProposalStatusExecuted {
		t.Errorf("proposal in DB should NOT be marked EXECUTED on baseline mismatch")
	}
}

// ----------------------------------------------------------------------------
// 5. ApprovalDigest generated upon approval and saved to database (R9)
// ----------------------------------------------------------------------------
func TestProposalSecurity_ApprovalDigestGeneratedUponApproval(t *testing.T) {
	db, admin := setupProposalSecurityDB(t)
	bus := eventbus.New()
	handler := NewOpsHandler(nil, nil, db, bus, nil)
	r := setupProposalSecurityRouter(handler, admin)

	// Step 1: Create proposal
	createPayload := `{"title":"Web Service Maintenance","target_component":"home-web","risk_level":"LOW","action_spec":"{\"action\":\"docker.restart\",\"target\":\"home-web\",\"params\":{\"graceful\":true}}"}`
	wCreate := httptest.NewRecorder()
	reqCreate, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals", strings.NewReader(createPayload))
	reqCreate.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wCreate, reqCreate)

	if wCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created on proposal creation, got %d: %s", wCreate.Code, wCreate.Body.String())
	}

	var createdProp model.OpsProposal
	if err := json.Unmarshal(wCreate.Body.Bytes(), &createdProp); err != nil {
		t.Fatalf("failed to decode create response: %v", err)
	}
	if createdProp.ApprovalDigest != "" {
		t.Errorf("new proposal must NOT have ApprovalDigest before approval, got: %s", createdProp.ApprovalDigest)
	}

	// Step 2: Approve proposal
	wApprove := httptest.NewRecorder()
	reqApprove, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/ops/proposals/%s/approve", createdProp.ProposalID), strings.NewReader(`{"action":"now"}`))
	reqApprove.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wApprove, reqApprove)

	if wApprove.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on approve, got %d: %s", wApprove.Code, wApprove.Body.String())
	}

	// Step 3: Verify ApprovalDigest in SQLite
	var dbProp model.OpsProposal
	if err := db.Where("proposal_id = ?", createdProp.ProposalID).First(&dbProp).Error; err != nil {
		t.Fatalf("failed to find proposal in db: %v", err)
	}

	if dbProp.ApprovalDigest == "" {
		t.Fatalf("expected ApprovalDigest to be saved in DB upon approval, got empty string")
	}
	if !strings.HasPrefix(dbProp.ApprovalDigest, "hmac-sha256:") {
		t.Errorf("expected ApprovalDigest to have 'hmac-sha256:' prefix, got: %s", dbProp.ApprovalDigest)
	}
	if len(dbProp.ApprovalDigest) < 78 {
		t.Errorf("expected ApprovalDigest HMAC length >= 78 (hmac-sha256:<kid>:<64 hex>), got: %d (%s)", len(dbProp.ApprovalDigest), dbProp.ApprovalDigest)
	}
	if dbProp.Version != 1 {
		t.Errorf("expected proposal version 1, got: %d", dbProp.Version)
	}

	// Verify cryptographic match
	expectedDigest, err := ComputeProposalApprovalDigest(&dbProp)
	if err != nil {
		t.Fatalf("failed to re-compute digest: %v", err)
	}
	if dbProp.ApprovalDigest != expectedDigest {
		t.Errorf("approval digest mismatch: saved %s != recomputed %s", dbProp.ApprovalDigest, expectedDigest)
	}
}

// ----------------------------------------------------------------------------
// 6. Target component tampering post-approval -> execution rejected (R9)
// ----------------------------------------------------------------------------
func TestProposalSecurity_TargetComponentTamperingPostApproval(t *testing.T) {
	db, _ := setupProposalSecurityDB(t)
	bus := eventbus.New()

	executedTarget := ""
	executor := NewOperationExecutor(
		db, bus, nil, nil,
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			executedTarget = container
			return "ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	prop := model.OpsProposal{
		ProposalID:      "prop-tamper-target-001",
		Title:           "Web Service Restart",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusApproved,
		BaselineHash:    model.ComputeDefaultBaseline("home-web"),
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
		Version:         1,
		CreatedAt:       time.Now(),
	}
	digest, _ := ComputeProposalApprovalDigest(&prop)
	prop.ApprovalDigest = digest
	db.Create(&prop)

	// Malicious tampering: SQLite row modified post-approval to target 'home-api'
	db.Model(&model.OpsProposal{}).Where("id = ?", prop.ID).Update("target_component", "home-api")

	_, execErr := executor.ExecuteProposal(context.Background(), &prop)
	if execErr == nil {
		t.Fatalf("CRITICAL SECURITY HOLE: execution succeeded after post-approval target component tampering!")
	}
	if !errors.Is(execErr, ErrApprovalDigestMismatch) {
		t.Fatalf("expected ErrApprovalDigestMismatch on target tampering, got: %v", execErr)
	}
	if executedTarget != "" {
		t.Fatalf("security violation: runner executed on tampered target %s!", executedTarget)
	}
}

// ----------------------------------------------------------------------------
// 7. Action tampering post-approval -> execution rejected (R9)
// ----------------------------------------------------------------------------
func TestProposalSecurity_ActionTamperingPostApproval(t *testing.T) {
	db, _ := setupProposalSecurityDB(t)
	bus := eventbus.New()

	executedAction := ""
	executor := NewOperationExecutor(
		db, bus, nil, nil,
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			executedAction = action
			return "ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	prop := model.OpsProposal{
		ProposalID:      "prop-tamper-action-001",
		Title:           "Benign Restart",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusApproved,
		BaselineHash:    model.ComputeDefaultBaseline("home-web"),
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
		Version:         1,
		CreatedAt:       time.Now(),
	}
	digest, _ := ComputeProposalApprovalDigest(&prop)
	prop.ApprovalDigest = digest
	db.Create(&prop)

	// Malicious tampering: SQLite action modified post-approval to 'docker.recreate'
	db.Model(&model.OpsProposal{}).Where("id = ?", prop.ID).Update("action_spec", `{"action":"docker.recreate","target":"home-web"}`)

	_, execErr := executor.ExecuteProposal(context.Background(), &prop)
	if execErr == nil {
		t.Fatalf("CRITICAL SECURITY HOLE: execution succeeded after post-approval action tampering!")
	}
	if !errors.Is(execErr, ErrApprovalDigestMismatch) {
		t.Fatalf("expected ErrApprovalDigestMismatch on action tampering, got: %v", execErr)
	}
	if executedAction != "" {
		t.Fatalf("security violation: runner executed tampered action %s!", executedAction)
	}
}

// ----------------------------------------------------------------------------
// 8. Parameter tampering post-approval -> execution rejected (R9)
// ----------------------------------------------------------------------------
func TestProposalSecurity_ParameterTamperingPostApproval(t *testing.T) {
	db, _ := setupProposalSecurityDB(t)
	bus := eventbus.New()

	var executedParams map[string]interface{}
	executor := NewOperationExecutor(
		db, bus, nil, nil,
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			executedParams = params
			return "ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	prop := model.OpsProposal{
		ProposalID:      "prop-tamper-params-001",
		Title:           "Graceful Restart",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusApproved,
		BaselineHash:    model.ComputeDefaultBaseline("home-web"),
		ActionSpec:      `{"action":"docker.restart","target":"home-web","params":{"timeout":10,"force":false}}`,
		Version:         1,
		CreatedAt:       time.Now(),
	}
	digest, _ := ComputeProposalApprovalDigest(&prop)
	prop.ApprovalDigest = digest
	db.Create(&prop)

	// Malicious tampering: parameters tampered post-approval in SQLite to force=true, timeout=999
	db.Model(&model.OpsProposal{}).Where("id = ?", prop.ID).Update("action_spec", `{"action":"docker.restart","target":"home-web","params":{"timeout":999,"force":true}}`)

	_, execErr := executor.ExecuteProposal(context.Background(), &prop)
	if execErr == nil {
		t.Fatalf("CRITICAL SECURITY HOLE: execution succeeded after post-approval parameter tampering!")
	}
	if !errors.Is(execErr, ErrApprovalDigestMismatch) {
		t.Fatalf("expected ErrApprovalDigestMismatch on parameter tampering, got: %v", execErr)
	}
	if executedParams != nil {
		t.Fatalf("security violation: runner executed with tampered parameters: %v", executedParams)
	}
}

// ----------------------------------------------------------------------------
// 9. Baseline tampering post-approval -> execution rejected (R9)
// ----------------------------------------------------------------------------
func TestProposalSecurity_BaselineTamperingPostApproval(t *testing.T) {
	db, _ := setupProposalSecurityDB(t)
	bus := eventbus.New()

	executor := NewOperationExecutor(
		db, bus, nil, nil,
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			return "ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	prop := model.OpsProposal{
		ProposalID:      "prop-tamper-base-001",
		Title:           "Baseline Tamper Test",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusApproved,
		BaselineHash:    model.ComputeDefaultBaseline("home-web"),
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
		Version:         1,
		CreatedAt:       time.Now(),
	}
	digest, _ := ComputeProposalApprovalDigest(&prop)
	prop.ApprovalDigest = digest
	db.Create(&prop)

	// Malicious tampering: SQLite baseline_hash altered post-approval
	db.Model(&model.OpsProposal{}).Where("id = ?", prop.ID).Update("baseline_hash", "sha256:tampered_baseline_9999999999999999999999999999999999999999999999999999999999999999")

	_, execErr := executor.ExecuteProposal(context.Background(), &prop)
	if execErr == nil {
		t.Fatalf("CRITICAL SECURITY HOLE: execution succeeded after post-approval baseline hash tampering!")
	}
	if !errors.Is(execErr, ErrApprovalDigestMismatch) {
		t.Fatalf("expected ErrApprovalDigestMismatch on baseline tampering, got: %v", execErr)
	}
}

// ----------------------------------------------------------------------------
// 10. Proposal version increment on edit invalidates prior approval (R9.4)
// ----------------------------------------------------------------------------
func TestProposalSecurity_VersionIncrementOnEditInvalidatesPriorApproval(t *testing.T) {
	db, _ := setupProposalSecurityDB(t)
	bus := eventbus.New()

	executor := NewOperationExecutor(
		db, bus, nil, nil,
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			return "ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	prop := model.OpsProposal{
		ProposalID:      "prop-version-inval-001",
		Title:           "Initial Approved Proposal",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusApproved,
		BaselineHash:    model.ComputeDefaultBaseline("home-web"),
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
		Version:         1,
		CreatedAt:       time.Now(),
	}
	digest, _ := ComputeProposalApprovalDigest(&prop)
	prop.ApprovalDigest = digest
	db.Create(&prop)

	// Proposal is edited / modified -> InvalidateProposalApproval is invoked
	InvalidateProposalApproval(&prop)

	if prop.Status != model.ProposalStatusPending {
		t.Errorf("expected status to be reset to PENDING, got: %s", prop.Status)
	}
	if prop.ApprovalDigest != "" {
		t.Errorf("expected ApprovalDigest to be cleared, got: %s", prop.ApprovalDigest)
	}
	if prop.Version != 2 {
		t.Errorf("expected Version incremented to 2, got: %d", prop.Version)
	}

	db.Save(&prop)

	// Attempting execution must be strictly rejected
	_, execErr := executor.ExecuteProposal(context.Background(), &prop)
	if execErr == nil {
		t.Fatalf("expected execution rejection on invalidated proposal, got nil")
	}
	if !errors.Is(execErr, ErrProposalUnauthorized) && !errors.Is(execErr, ErrApprovalDigestMismatch) {
		t.Errorf("expected ErrProposalUnauthorized or ErrApprovalDigestMismatch, got: %v", execErr)
	}
}

// ----------------------------------------------------------------------------
// 11. Full happy path: create -> approve with digest -> execute cleanly
// ----------------------------------------------------------------------------
func TestProposalSecurity_FullHappyPath(t *testing.T) {
	db, admin := setupProposalSecurityDB(t)
	bus := eventbus.New()

	executed := false
	executor := NewOperationExecutor(
		db, bus, nil, nil,
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			executed = true
			return "container restarted successfully", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	handler := NewOpsHandler(nil, nil, db, bus, nil)
	handler.SetExecutor(executor)
	r := setupProposalSecurityRouter(handler, admin)

	// 1. Create proposal
	createBody := `{"title":"Happy Path Restart","target_component":"home-web","risk_level":"LOW","action_spec":"{\"action\":\"docker.restart\",\"target\":\"home-web\"}"}`
	wCreate := httptest.NewRecorder()
	reqCreate, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals", strings.NewReader(createBody))
	reqCreate.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wCreate, reqCreate)

	if wCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", wCreate.Code, wCreate.Body.String())
	}

	var prop model.OpsProposal
	json.Unmarshal(wCreate.Body.Bytes(), &prop)
	if prop.BaselineHash == "" {
		t.Fatalf("expected non-empty BaselineHash on created proposal")
	}

	// 2. Approve proposal with scheduled execution (anti-immediate race, testing execute endpoint)
	wApprove := httptest.NewRecorder()
	reqApprove, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/ops/proposals/%s/approve", prop.ProposalID), strings.NewReader(`{"action":"schedule","schedule_time":"2020-01-01T00:00:00Z"}`))
	reqApprove.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wApprove, reqApprove)

	if wApprove.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on approve, got %d: %s", wApprove.Code, wApprove.Body.String())
	}

	var approvedProp model.OpsProposal
	json.Unmarshal(wApprove.Body.Bytes(), &approvedProp)
	if approvedProp.ApprovalDigest == "" {
		t.Fatalf("expected ApprovalDigest to be generated upon approval")
	}
	if approvedProp.Status != model.ProposalStatusScheduled {
		t.Fatalf("expected status %s, got %s", model.ProposalStatusScheduled, approvedProp.Status)
	}

	// 3. Execute proposal
	wExec := httptest.NewRecorder()
	reqExec, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/ops/proposals/%s/execute", prop.ProposalID), nil)
	r.ServeHTTP(wExec, reqExec)

	if wExec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on execute, got %d: %s", wExec.Code, wExec.Body.String())
	}

	if !executed {
		t.Fatalf("expected runner to be executed on happy path!")
	}

	// Verify proposal status in database is EXECUTED
	var finalProp model.OpsProposal
	db.Where("proposal_id = ?", prop.ProposalID).First(&finalProp)
	if finalProp.Status != model.ProposalStatusExecuted {
		t.Errorf("expected final status %s, got: %s", model.ProposalStatusExecuted, finalProp.Status)
	}
	if !finalProp.ExecutedAt.Valid {
		t.Errorf("expected valid ExecutedAt timestamp")
	}

	// 4. Test synchronous approval execution (?sync=true)
	executedSync := false
	executorSync := NewOperationExecutor(
		db, bus, nil, nil,
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			executedSync = true
			return "sync restart ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)
	handler.SetExecutor(executorSync)

	wCreate2 := httptest.NewRecorder()
	reqCreate2, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals", strings.NewReader(`{"title":"Sync Path","target_component":"home-api","risk_level":"LOW","action_spec":"{\"action\":\"docker.restart\",\"target\":\"home-api\"}"}`))
	reqCreate2.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wCreate2, reqCreate2)
	if wCreate2.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created for sync proposal, got %d: %s", wCreate2.Code, wCreate2.Body.String())
	}
	var prop2 model.OpsProposal
	json.Unmarshal(wCreate2.Body.Bytes(), &prop2)

	wApproveSync := httptest.NewRecorder()
	reqApproveSync, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/ops/proposals/%s/approve?sync=true", prop2.ProposalID), strings.NewReader(`{"action":"now"}`))
	reqApproveSync.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wApproveSync, reqApproveSync)

	if wApproveSync.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on sync approve, got %d: %s", wApproveSync.Code, wApproveSync.Body.String())
	}
	if !executedSync {
		t.Fatalf("expected synchronous execution to run during ?sync=true approval")
	}
}

// ----------------------------------------------------------------------------
// 12. Deterministic canonical serialization and digest generation (R9.2)
// ----------------------------------------------------------------------------
func TestProposalSecurity_DeterministicCanonicalDigest(t *testing.T) {
	// Equivalent parameters with different map insertion order and JSON whitespace
	paramsA := map[string]interface{}{
		"timeout": 30,
		"force":   false,
		"mode":    "graceful",
	}
	paramsB := map[string]interface{}{
		"mode":    "graceful",
		"force":   false,
		"timeout": 30,
	}

	digestA := ComputeApprovalDigest("home-web", "docker.restart", paramsA, "sha256:base123", 1)
	digestB := ComputeApprovalDigest("home-web", "docker.restart", paramsB, "sha256:base123", 1)

	if digestA == "" || digestB == "" {
		t.Fatalf("expected non-empty digests")
	}
	if digestA != digestB {
		t.Fatalf("canonical digest must be deterministic regardless of parameter order! %s != %s", digestA, digestB)
	}

	// Mutating any single component MUST change the digest:
	// Target mutation
	digestTargetChanged := ComputeApprovalDigest("home-api", "docker.restart", paramsA, "sha256:base123", 1)
	if digestTargetChanged == digestA {
		t.Errorf("digest must change when target changes")
	}

	// Action mutation
	digestActionChanged := ComputeApprovalDigest("home-web", "docker.recreate", paramsA, "sha256:base123", 1)
	if digestActionChanged == digestA {
		t.Errorf("digest must change when action changes")
	}

	// Params mutation
	paramsMutated := map[string]interface{}{
		"timeout": 31,
		"force":   false,
		"mode":    "graceful",
	}
	digestParamsChanged := ComputeApprovalDigest("home-web", "docker.restart", paramsMutated, "sha256:base123", 1)
	if digestParamsChanged == digestA {
		t.Errorf("digest must change when parameters change")
	}

	// Baseline mutation
	digestBaseChanged := ComputeApprovalDigest("home-web", "docker.restart", paramsA, "sha256:base456", 1)
	if digestBaseChanged == digestA {
		t.Errorf("digest must change when baseline changes")
	}

	// Version mutation
	digestVersionChanged := ComputeApprovalDigest("home-web", "docker.restart", paramsA, "sha256:base123", 2)
	if digestVersionChanged == digestA {
		t.Errorf("digest must change when version changes")
	}
}

// ----------------------------------------------------------------------------
// 13. Criteria E.15 Fault Injection: TestProposal_PostApprovalTampering_Rejected
// ----------------------------------------------------------------------------
func TestProposal_PostApprovalTampering_Rejected(t *testing.T) {
	db, admin := setupProposalSecurityDB(t)
	bus := eventbus.New()

	runnerCalled := false
	executor := NewOperationExecutor(
		db, bus, nil, nil,
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			runnerCalled = true
			return "ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	handler := NewOpsHandler(nil, nil, db, bus, nil)
	handler.SetExecutor(executor)
	r := setupProposalSecurityRouter(handler, admin)

	// Step 1: Create and approve a legitimate proposal
	prop := model.OpsProposal{
		ProposalID:      "fault-inj-15-prop",
		Title:           "Legitimate Admin Approved Restart",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusApproved,
		BaselineHash:    model.ComputeDefaultBaseline("home-web"),
		ActionSpec:      `{"action":"docker.restart","target":"home-web","params":{"timeout":10}}`,
		Version:         1,
		CreatedAt:       time.Now(),
	}
	digest, err := ComputeProposalApprovalDigest(&prop)
	if err != nil {
		t.Fatalf("failed to compute digest: %v", err)
	}
	prop.ApprovalDigest = digest
	if err := db.Create(&prop).Error; err != nil {
		t.Fatalf("failed to create proposal: %v", err)
	}

	// Step 2: Fault Injection 15: An unauthorized process modifies parameters in SQLite
	// Attack simulation: change target component and execution payload to malicious action
	maliciousPayload := `{"action":"docker.exec","target":"home-web","params":{"cmd":"rm -rf /data"}}`
	if err := db.Model(&model.OpsProposal{}).Where("proposal_id = ?", prop.ProposalID).Update("action_spec", maliciousPayload).Error; err != nil {
		t.Fatalf("failed to inject fault in db: %v", err)
	}

	// Step 3: Trigger execution via POST /api/v1/ops/proposals/:id/execute
	wExec := httptest.NewRecorder()
	reqExec, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/ops/proposals/%s/execute", prop.ProposalID), nil)
	r.ServeHTTP(wExec, reqExec)

	// Must fail-closed with HTTP 409 Conflict
	if wExec.Code != http.StatusConflict {
		t.Fatalf("FAULT INJECTION 15 FAILED TO FAIL-CLOSED: expected HTTP 409 Conflict, got %d: %s", wExec.Code, wExec.Body.String())
	}

	if runnerCalled {
		t.Fatalf("CRITICAL SECURITY FAILURE: Docker runner executed the tampered action payload!")
	}

	// Verify direct executor call also returns ErrApprovalDigestMismatch
	var tamperedDB model.OpsProposal
	db.Where("proposal_id = ?", prop.ProposalID).First(&tamperedDB)
	_, directErr := executor.ExecuteProposal(context.Background(), &tamperedDB)
	if !errors.Is(directErr, ErrApprovalDigestMismatch) {
		t.Fatalf("expected direct ExecuteProposal to fail with ErrApprovalDigestMismatch, got: %v", directErr)
	}
}
