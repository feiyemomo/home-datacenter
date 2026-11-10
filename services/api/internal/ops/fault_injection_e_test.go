package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
)

// setupFITestDB creates an isolated in-memory SQLite database.
func setupFITestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbName := fmt.Sprintf("file:fault_injection_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test sqlite db: %v", err)
	}

	err = db.AutoMigrate(
		&model.OpsMaintenanceLock{},
		&model.OpsCircuitBreakerState{},
		&model.OpsProposal{},
		&model.OpsExecutionHistory{},
		&RecoveryLease{},
	)
	if err != nil {
		t.Fatalf("failed to automigrate models: %v", err)
	}
	return db
}

// ============================================================================
// Fault Injection 1 & 2: SQLite file missing / corrupt enters degraded mode
// ============================================================================
func TestFaultInjection_01_02_SQLiteCorruptOrMissing_DegradedMode(t *testing.T) {
	db := setupFITestDB(t)

	// Close database to simulate unreadable / corrupt / missing SQLite storage
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to get sqlDB: %v", err)
	}
	_ = sqlDB.Close()

	lock := NewMaintenanceLock()
	lock.LoadPersistedState(db)

	cb := NewContainerCircuitBreaker()
	cb.LoadPersistedState(db)

	// Fail-Closed: Lock must be active in degraded protection mode
	if !lock.IsLocked() {
		t.Fatalf("expected lock to be active in degraded mode on DB error")
	}
	if !lock.IsDegraded() {
		t.Fatalf("expected lock.IsDegraded() == true")
	}

	// Fail-Closed: Circuit breaker must reject recovery in degraded mode
	if !cb.IsDegraded() {
		t.Fatalf("expected cb.IsDegraded() == true")
	}
	canRecover, _, recErr := cb.CanAttemptRecovery("home-web")
	if canRecover || !errors.Is(recErr, ErrFailClosedMode) {
		t.Fatalf("expected ErrFailClosedMode from degraded breaker, got can=%v, err=%v", canRecover, recErr)
	}
}

// ============================================================================
// Fault Injection 4: SQLite insufficient write permissions (Acquire fails)
// ============================================================================
func TestFaultInjection_04_SQLiteInsufficientWritePermissions(t *testing.T) {
	db := setupFITestDB(t)
	lock := NewMaintenanceLock()
	lock.SetDB(db)

	// Inject SQL trigger to abort writes on ops_maintenance_locks
	err := db.Exec(`CREATE TRIGGER fail_lock_ins BEFORE INSERT ON ops_maintenance_locks 
		BEGIN SELECT RAISE(ABORT, 'sqlite readonly / permission denied'); END;`).Error
	if err != nil {
		t.Fatalf("failed to create fail trigger: %v", err)
	}

	// Acquire MUST fail with error
	err = lock.Acquire("admin", "emergency maintenance", 30*time.Minute)
	if err == nil {
		t.Fatalf("expected Acquire to fail when database write fails, got nil")
	}
	if !strings.Contains(err.Error(), "failed to persist maintenance lock acquisition") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// In-memory lock state MUST NOT be active (rolled back)
	if lock.IsLocked() {
		t.Fatalf("expected lock.IsLocked() == false after failed acquisition persistence")
	}
}

// ============================================================================
// Fault Injection 5: SQLite transaction write failure (rollback verified)
// ============================================================================
func TestFaultInjection_05_SQLiteTransactionWriteFailure_RollbackVerified(t *testing.T) {
	db := setupFITestDB(t)
	lock := NewMaintenanceLock()
	lock.SetDB(db)

	// Step 1: Acquire lock successfully
	err := lock.Acquire("admin", "security patching", 30*time.Minute)
	if err != nil {
		t.Fatalf("failed to acquire lock: %v", err)
	}
	if !lock.IsLocked() {
		t.Fatalf("expected lock to be active")
	}

	// Step 2: Inject transaction abort on release write
	err = db.Exec(`CREATE TRIGGER fail_lock_rel BEFORE INSERT ON ops_maintenance_locks 
		BEGIN SELECT RAISE(ABORT, 'transaction write error'); END;`).Error
	if err != nil {
		t.Fatalf("failed to create trigger: %v", err)
	}

	// Step 3: Attempt Release -> MUST fail and retain in-memory lock
	err = lock.Release("admin")
	if err == nil {
		t.Fatalf("expected Release to fail when persistence fails, got nil")
	}
	if !strings.Contains(err.Error(), "memory lock retained") {
		t.Fatalf("expected error mentioning memory lock retained, got: %v", err)
	}

	// Critical check: in-memory lock MUST remain locked (fail-closed!)
	if !lock.IsLocked() {
		t.Fatalf("VIOLATION: In-memory maintenance lock cleared despite transaction write failure!")
	}

	// Step 4: Circuit breaker transaction failure rollback
	cb := NewContainerCircuitBreaker()
	cb.SetDB(db)
	cb.SetCooldownDuration(10 * time.Millisecond)

	for i := 0; i < 3; i++ {
		cb.RecordCrash("home-api")
	}
	time.Sleep(20 * time.Millisecond)

	err = db.Exec(`CREATE TRIGGER fail_cb_upd BEFORE UPDATE ON ops_circuit_breaker_states 
		BEGIN SELECT RAISE(ABORT, 'transaction rollback on cb'); END;`).Error
	if err != nil {
		t.Fatalf("failed to create trigger: %v", err)
	}

	allowed, ticket, cbErr := cb.CanAttemptRecovery("home-api")
	if allowed || ticket != nil || cbErr == nil {
		t.Fatalf("expected CanAttemptRecovery to fail when DB update fails, got allowed=%v, err=%v", allowed, cbErr)
	}
	if state := cb.GetState("home-api"); state != CircuitOpen {
		t.Fatalf("expected breaker state rolled back to CircuitOpen, got: %s", state)
	}
	if _, exists := cb.GetActiveProbe("home-api"); exists {
		t.Fatalf("expected no active probe retained in memory after transaction failure")
	}
}

// ============================================================================
// Fault Injection 10: Docker container running but service unavailable
// ============================================================================
func TestFaultInjection_10_ContainerRunningServiceUnavailable(t *testing.T) {
	db := setupFITestDB(t)

	// Docker restart succeeds, but service health probe fails (connection refused)
	executor := NewOperationExecutor(
		db, nil, nil, nil,
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			return "container restarted successfully", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return errors.New("simulated TCP connection refused :8080")
		}),
	)

	fsm := NewContainerRecoveryFSM(db, nil, executor)
	ctx := context.Background()
	event := ContainerEvent{
		ContainerName: "home-api",
		Action:        "die",
		ExitCode:      1,
		Timestamp:     time.Now(),
	}

	res, err := fsm.ProcessAndRecover(ctx, event)
	if err == nil {
		t.Fatalf("expected error when service health probe fails, got nil")
	}
	if res.Status != StatusFailed {
		t.Fatalf("expected recovery status StatusFailed, got: %s", res.Status)
	}
	if res.HealthVerified {
		t.Fatalf("HealthVerified must be false when service probe fails")
	}
}

// ============================================================================
// Fault Injection 11: Go Core crashed / abnormal exit (orphan probe recovery)
// ============================================================================
func TestFaultInjection_11_GoCoreCrash_OrphanedProbeRecovery(t *testing.T) {
	db := setupFITestDB(t)

	// Seed database with orphan probe state left by crashed process
	pastOpened := time.Now().Add(-20 * time.Minute)
	orphanRow := model.OpsCircuitBreakerState{
		Container: "home-web",
		State:     string(CircuitHalfOpen),
		OpenedAt:  pastOpened,
		UpdatedAt: time.Now().Add(-10 * time.Minute),
	}
	if err := db.Create(&orphanRow).Error; err != nil {
		t.Fatalf("failed to insert orphan probe row: %v", err)
	}

	// Reboot Go Core: initialize fresh FSM
	fsm := NewContainerRecoveryFSM(db, nil, nil)
	cb := fsm.GetCircuitBreaker()

	// Orphaned HALF_OPEN probe MUST be recognized and reset to CircuitOpen
	if state := cb.GetState("home-web"); state != CircuitOpen {
		t.Fatalf("expected orphaned HALF_OPEN state to reset to CircuitOpen on restart, got: %s", state)
	}

	// No zombie probes in memory
	if _, exists := cb.GetActiveProbe("home-web"); exists {
		t.Fatalf("expected activeProbes to be empty of zombie probes")
	}

	// Clean recovery probe can now be granted
	canRecover, ticket, err := cb.CanAttemptRecovery("home-web")
	if !canRecover || ticket == nil || err != nil {
		t.Fatalf("expected fresh probe acquisition to succeed after restart, got can=%v, err=%v", canRecover, err)
	}
}

// ============================================================================
// Fault Injection 13: Go/Python simultaneous discovery race (ops_recovery_leases)
// ============================================================================
func TestFaultInjection_13_SimultaneousDiscoveryRace_MutualExclusion(t *testing.T) {
	db := setupFITestDB(t)
	mgr := NewRecoveryLeaseManager(db)

	goroutines := 20
	var winners int32
	var conflicts int32
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		h := fmt.Sprintf("race-candidate-%d", i)
		go func(holderName string) {
			defer wg.Done()
			tok, aErr := mgr.AcquireLease("frigate", holderName, 5*time.Second)
			if aErr == nil && tok != "" {
				atomic.AddInt32(&winners, 1)
			} else if errors.Is(aErr, ErrLeaseHeld) {
				atomic.AddInt32(&conflicts, 1)
			}
		}(h)
	}
	wg.Wait()

	if winners != 1 {
		t.Fatalf("expected exactly 1 winner in discovery race, got %d", winners)
	}
	if conflicts != int32(goroutines-1) {
		t.Fatalf("expected %d conflicts with ErrLeaseHeld, got %d", goroutines-1, conflicts)
	}
}

// ============================================================================
// Fault Injection 14: Recovery probe execution timeout (HALF_OPEN expires & resets)
// ============================================================================
func TestFaultInjection_14_ProbeExecutionTimeout_ResetsToOpen(t *testing.T) {
	db := setupFITestDB(t)
	fsm := NewContainerRecoveryFSM(db, nil, nil)
	cb := fsm.GetCircuitBreaker()
	cb.SetCooldownDuration(10 * time.Millisecond)
	cb.SetProbeTTL(50 * time.Millisecond)

	// Trip breaker and wait for cooldown
	for i := 0; i < 3; i++ {
		cb.RecordCrash("home-web")
	}
	time.Sleep(20 * time.Millisecond)

	// Acquire probe ticket
	allowed, ticket, err := cb.CanAttemptRecovery("home-web")
	if !allowed || ticket == nil || err != nil {
		t.Fatalf("failed to acquire initial probe: %v", err)
	}
	if cb.GetState("home-web") != CircuitHalfOpen {
		t.Fatalf("expected state HALF_OPEN")
	}

	// Stall past probe lease TTL (50ms)
	time.Sleep(70 * time.Millisecond)

	// Next CanAttemptRecovery call MUST detect timeout and reset to OPEN
	canRecover, _, timeoutErr := cb.CanAttemptRecovery("home-web")
	if canRecover || !errors.Is(timeoutErr, ErrProbeTimeout) {
		t.Fatalf("expected ErrProbeTimeout on expired probe, got can=%v, err=%v", canRecover, timeoutErr)
	}

	if state := cb.GetState("home-web"); state != CircuitOpen {
		t.Fatalf("expected state reset to OPEN after probe timeout, got: %s", state)
	}

	// Stale ticket late callback MUST be rejected
	lateErr := cb.RecordRecoverySuccess("home-web", ticket.Token)
	if lateErr == nil || !errors.Is(lateErr, ErrStaleProbeToken) {
		t.Fatalf("expected ErrStaleProbeToken on late callback, got: %v", lateErr)
	}
}

// ============================================================================
// Fault Injection 15: Proposal parameters modified post-approval (ErrApprovalDigestMismatch)
// ============================================================================
func TestFaultInjection_15_ProposalParametersModifiedPostApproval(t *testing.T) {
	db := setupFITestDB(t)

	var executedParams map[string]interface{}
	executor := NewOperationExecutor(
		db, nil, nil, nil,
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			executedParams = params
			return "ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	prop := model.OpsProposal{
		ProposalID:      "prop-fi-15",
		Title:           "Graceful Restart Proposal",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusApproved,
		BaselineHash:    model.ComputeDefaultBaseline("home-web"),
		ActionSpec:      `{"action":"docker.restart","target":"home-web","params":{"timeout":10,"force":false}}`,
		Version:         1,
		CreatedAt:       time.Now(),
	}

	// Compute genuine ApprovalDigest for original parameters
	digest, err := ComputeProposalApprovalDigest(&prop)
	if err != nil {
		t.Fatalf("failed to compute digest: %v", err)
	}
	prop.ApprovalDigest = digest
	db.Create(&prop)

	// MALICIOUS INJECTION: Modify parameters in SQLite post-approval
	db.Model(&model.OpsProposal{}).Where("id = ?", prop.ID).Update(
		"action_spec",
		`{"action":"docker.restart","target":"home-web","params":{"timeout":999,"force":true}}`,
	)

	// Execute proposal -> MUST be rejected with ErrApprovalDigestMismatch
	_, execErr := executor.ExecuteProposal(context.Background(), &prop)
	if execErr == nil {
		t.Fatalf("CRITICAL FLAW: execution succeeded after post-approval parameter tampering!")
	}
	if !errors.Is(execErr, ErrApprovalDigestMismatch) {
		t.Fatalf("expected ErrApprovalDigestMismatch, got: %v", execErr)
	}
	if executedParams != nil {
		t.Fatalf("security violation: runner was executed with tampered parameters: %v", executedParams)
	}
}
