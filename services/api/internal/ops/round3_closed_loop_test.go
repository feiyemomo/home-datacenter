package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
)

// setupRound3TestDB initializes an isolated in-memory SQLite database with pure-Go driver.
func setupRound3TestDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:round3_%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test sqlite db: %v", err)
	}

	err = db.AutoMigrate(
		&model.OpsMaintenanceLock{},
		&model.OpsCircuitBreakerState{},
		&model.OpsProposal{},
		&model.OpsExecutionHistory{},
	)
	if err != nil {
		t.Fatalf("failed to automigrate: %v", err)
	}
	return db
}

// 1. P0: Closed-Loop Real Recovery Execution Test
func TestRound3_ClosedLoop_RecoveryExecution(t *testing.T) {
	db := setupRound3TestDB(t)

	var restartCalled atomic.Int32
	var healthCheckCalled atomic.Int32

	executor := NewOperationExecutor(
		db,
		nil,
		nil,
		nil,
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			if (action == ActionDockerRestart || action == "restart") && container == "home-web" {
				restartCalled.Add(1)
				return "container restarted successfully", nil
			}
			return "", fmt.Errorf("unexpected action: %s", action)
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			healthCheckCalled.Add(1)
			return nil
		}),
	)

	fsm := NewContainerRecoveryFSM(db, nil, executor)

	event := ContainerEvent{
		ContainerName: "home-web",
		Action:        "die",
		ExitCode:      1,
		Timestamp:     time.Now(),
	}

	res, err := fsm.ProcessAndRecover(context.Background(), event)
	if err != nil {
		t.Fatalf("ProcessAndRecover failed: %v", err)
	}

	if res.Status != StatusRecovered {
		t.Fatalf("expected StatusRecovered, got: %s", res.Status)
	}
	if !res.Executed {
		t.Fatalf("expected Executed == true")
	}
	if !res.HealthVerified {
		t.Fatalf("expected HealthVerified == true")
	}
	if restartCalled.Load() != 1 {
		t.Fatalf("expected Docker restart to be called exactly 1 time, got %d", restartCalled.Load())
	}
	if healthCheckCalled.Load() < 1 {
		t.Fatalf("expected health checker to be called at least 1 time, got %d", healthCheckCalled.Load())
	}

	// Verify circuit breaker was reset to CLOSED
	cbState := fsm.GetCircuitBreaker().GetState("home-web")
	if cbState != CircuitClosed {
		t.Fatalf("expected circuit breaker state CLOSED after successful recovery, got: %s", cbState)
	}
}

// 2. P0: Closed-Loop Recovery Fails When Health Probe Fails
func TestRound3_ClosedLoop_HealthProbeFailure(t *testing.T) {
	db := setupRound3TestDB(t)

	executor := NewOperationExecutor(
		db,
		nil,
		nil,
		nil,
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			return "container restarted", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return fmt.Errorf("simulated health probe connection refused")
		}),
	)

	fsm := NewContainerRecoveryFSM(db, nil, executor)

	event := ContainerEvent{
		ContainerName: "home-web",
		Action:        "die",
		ExitCode:      1,
		Timestamp:     time.Now(),
	}

	res, err := fsm.ProcessAndRecover(context.Background(), event)
	if err == nil {
		t.Fatalf("expected error when health check fails, got nil")
	}
	if res.Status != StatusFailed {
		t.Fatalf("expected StatusFailed when health probe fails, got: %s", res.Status)
	}
	if res.HealthVerified {
		t.Fatalf("expected HealthVerified == false when health probe fails")
	}
}

// 3. P1: Maintenance Lock Persistence and Blocked State
func TestRound3_MaintenanceLock_PersistenceAndBlock(t *testing.T) {
	db := setupRound3TestDB(t)

	fsm1 := NewContainerRecoveryFSM(db, nil, nil)
	err := fsm1.GetMaintenanceLock().Acquire("admin", "Emergency network upgrade", 30*time.Minute)
	if err != nil {
		t.Fatalf("failed to acquire maintenance lock: %v", err)
	}

	// Spin up fresh FSM instance with same DB to test persistence reload
	fsm2 := NewContainerRecoveryFSM(db, nil, nil)
	if !fsm2.GetMaintenanceLock().IsLocked() {
		t.Fatalf("expected maintenance lock to persist and be active in second FSM instance")
	}

	event := ContainerEvent{
		ContainerName: "home-web",
		Action:        "die",
		ExitCode:      1,
		Timestamp:     time.Now(),
	}

	res, err := fsm2.ProcessAndRecover(context.Background(), event)
	if err == nil {
		t.Fatalf("expected ErrSystemInMaintenance error, got nil")
	}
	if res.Status != StatusBlocked {
		t.Fatalf("expected StatusBlocked, got: %s", res.Status)
	}
	if res.Executed {
		t.Fatalf("expected Executed == false when locked")
	}

	// Release lock in fsm2 and verify persistence
	err = fsm2.GetMaintenanceLock().Release("admin")
	if err != nil {
		t.Fatalf("failed to release maintenance lock: %v", err)
	}

	fsm3 := NewContainerRecoveryFSM(db, nil, nil)
	if fsm3.GetMaintenanceLock().IsLocked() {
		t.Fatalf("expected maintenance lock to be released after persistence")
	}
}

// 4. P1: Circuit Breaker Persistence, Trip, and Half-Open Handling
func TestRound3_CircuitBreaker_PersistenceAndHalfOpen(t *testing.T) {
	db := setupRound3TestDB(t)

	fsm1 := NewContainerRecoveryFSM(db, nil, nil)
	cb1 := fsm1.GetCircuitBreaker()
	cb1.cooldownDuration = 600 * time.Millisecond

	// Record 3 crashes to trip circuit breaker
	cb1.RecordCrash("home-web")
	cb1.RecordCrash("home-web")
	state, tripped := cb1.RecordCrash("home-web")
	if !tripped || state != CircuitOpen {
		t.Fatalf("expected circuit breaker to trip to OPEN, got state: %s, tripped: %v", state, tripped)
	}

	// Spin up fresh FSM instance to test persistent state
	fsm2 := NewContainerRecoveryFSM(db, nil, nil)
	cb2 := fsm2.GetCircuitBreaker()
	cb2.cooldownDuration = 600 * time.Millisecond

	if cb2.GetState("home-web") != CircuitOpen {
		t.Fatalf("expected persisted circuit breaker state OPEN in fsm2, got: %s", cb2.GetState("home-web"))
	}

	// Before cooldown: CanAttemptRecovery must return false
	canRecover, _, err := cb2.CanAttemptRecovery("home-web")
	if canRecover || err == nil {
		t.Fatalf("expected recovery to be blocked while circuit OPEN, got canRecover=%v, err=%v", canRecover, err)
	}

	// Wait for cooldown to elapse
	time.Sleep(700 * time.Millisecond)

	// In HALF_OPEN: First probe allowed
	canRecover, ticket, err := cb2.CanAttemptRecovery("home-web")
	if !canRecover || err != nil || ticket == nil {
		t.Fatalf("expected first HALF_OPEN probe to be allowed with ticket, got canRecover=%v, err=%v", canRecover, err)
	}

	// Concurrency check: Second simultaneous probe must be BLOCKED (single in-flight probe token)
	canRecover2, _, err2 := cb2.CanAttemptRecovery("home-web")
	if canRecover2 || err2 == nil {
		t.Fatalf("expected second simultaneous probe in HALF_OPEN to be blocked, got canRecover=%v", canRecover2)
	}

	// Crash during HALF_OPEN probe -> must immediately trip back to OPEN
	newState, _ := cb2.RecordCrash("home-web")
	if newState != CircuitOpen {
		t.Fatalf("expected crash during HALF_OPEN to trip back to CircuitOpen, got: %s", newState)
	}
}

// 5. P1: Fail-Closed Anti-TOCTOU Baseline Fingerprint Verification
func TestRound3_BaselineHash_FailClosed(t *testing.T) {
	db := setupRound3TestDB(t)

	extractorReturnsEmpty := func(target string) string {
		return "" // Simulated fingerprint extraction failure
	}

	executor := NewOperationExecutor(
		db,
		nil,
		nil,
		nil,
		WithBaselineExtractor(extractorReturnsEmpty),
		WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
	)

	spec := OperationSpec{
		Action:       ActionDockerRestart,
		Target:       "home-web",
		BaselineHash: "sha256:valid_hash_value_123456",
	}

	res, err := executor.Execute(context.Background(), spec)
	if err == nil {
		t.Fatalf("expected error when baseline calculation fails, got nil")
	}
	if res.Success {
		t.Fatalf("expected execution failure when baseline calculation returns empty")
	}
}

// 6. P1: Proposal ActionSpec Target Tampering Rejection
func TestRound3_Proposal_ActionSpecTamperingRejected(t *testing.T) {
	db := setupRound3TestDB(t)
	executor := NewOperationExecutor(db, nil, nil, nil)

	// Tampered proposal: Title says home-web, Proposal Target is home-web,
	// but inner ActionSpec maliciously targets frigate!
	tamperedSpec := OperationSpec{
		Action: ActionDockerRestart,
		Target: "frigate", // Mismatched!
	}
	specBytes, _ := json.Marshal(tamperedSpec)

	proposal := &model.OpsProposal{
		ProposalID:      "prop-tamper-001",
		Title:           "Restart home-web",
		TargetComponent: "home-web",
		ActionSpec:      string(specBytes),
	}

	_, err := executor.parseProposalSpec(proposal)
	if err == nil {
		t.Fatalf("expected security violation error when ActionSpec target mismatches Proposal TargetComponent, got nil")
	}
	if !strings.Contains(err.Error(), "security violation") {
		t.Fatalf("expected security violation message, got: %v", err)
	}
}

// 7. P1: Docker Exec Strict Command Whitelist Verification
func TestRound3_DockerExec_CommandWhitelist(t *testing.T) {
	db := setupRound3TestDB(t)
	executor := NewOperationExecutor(
		db,
		nil,
		nil,
		nil,
		WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
	)

	ctx := context.Background()

	// Whitelisted command for openresty: ["nginx", "-t"]
	// Should pass whitelist validation (will fail on docker socket if not running, but NOT ErrActionNotSupported)
	_, err := executor.executeDockerExec(ctx, ComponentOpenResty, map[string]interface{}{
		"command": []string{"nginx", "-t"},
	})
	if err != nil && strings.Contains(err.Error(), "not in the approved whitelist") {
		t.Fatalf("expected ['nginx', '-t'] to pass whitelist check, got whitelist rejection: %v", err)
	}

	// Malicious / Unapproved command: ["rm", "-rf", "/"]
	_, err = executor.executeDockerExec(ctx, ComponentOpenResty, map[string]interface{}{
		"command": []string{"rm", "-rf", "/"},
	})
	if err == nil {
		t.Fatalf("expected unapproved command ['rm', '-rf', '/'] to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "not in the approved whitelist") {
		t.Fatalf("expected whitelist rejection error, got: %v", err)
	}
}

// 8. P0: Gin Route Integration - Structured Status Returned
func TestRound3_GinRoute_HandleContainerEvent_StructuredStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupRound3TestDB(t)

	executor := NewOperationExecutor(
		db,
		nil,
		nil,
		nil,
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			return "restarted ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	handler := NewOpsHandler(nil, nil, db, nil, nil)
	handler.SetExecutor(executor)
	r := gin.New()
	r.POST("/api/v1/ops/fsm/event", func(c *gin.Context) {
		// Mock internal service caller
		c.Set("is_admin", false)
		c.Set("is_internal_service", true)
		c.Next()
	}, handler.HandleContainerEvent)

	eventPayload := ContainerEvent{
		ContainerName: "home-web",
		Action:        "die",
		ExitCode:      1,
		Timestamp:     time.Now(),
	}
	body, _ := json.Marshal(eventPayload)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/ops/fsm/event", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got: %d, body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Status         string            `json:"status"`
		Executed       bool              `json:"executed"`
		HealthVerified bool              `json:"health_verified"`
		Decision       *RecoveryDecision `json:"decision"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Status != string(StatusRecovered) {
		t.Fatalf("expected status RECOVERED, got: %s", resp.Status)
	}
	if !resp.Executed {
		t.Fatalf("expected executed == true")
	}
	if !resp.HealthVerified {
		t.Fatalf("expected health_verified == true")
	}
}
