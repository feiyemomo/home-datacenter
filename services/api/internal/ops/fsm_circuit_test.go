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

// setupCircuitTestDB creates an isolated in-memory SQLite database for circuit tests.
func setupCircuitTestDB(t *testing.T) *gorm.DB {
	dbName := fmt.Sprintf("file:fsm_circuit_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
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
		t.Fatalf("failed to automigrate models: %v", err)
	}
	return db
}

// 1. OPEN -> Cooldown -> HALF_OPEN -> ProcessAndRecover Success -> CLOSED full lifecycle test (R2, R10)
func TestRound3_CircuitBreaker_FullRecoveryCycle_ProcessAndRecover(t *testing.T) {
	db := setupCircuitTestDB(t)

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
	cb := fsm.GetCircuitBreaker()
	cb.SetCooldownDuration(60 * time.Millisecond)
	cb.SetProbeTTL(500 * time.Millisecond)

	ctx := context.Background()
	event := ContainerEvent{
		ContainerName: "home-web",
		Action:        "die",
		ExitCode:      1,
		Timestamp:     time.Now(),
	}

	// Step 1: Trigger 3 crashes to trip the breaker to OPEN
	for i := 0; i < 3; i++ {
		res, _ := fsm.ProcessAndRecover(ctx, event)
		if i < 2 {
			if res.Status != StatusRecovered {
				t.Fatalf("crash #%d: expected StatusRecovered, got: %s", i+1, res.Status)
			}
		} else {
			if res.Status != StatusBlocked {
				t.Fatalf("crash #3: expected StatusBlocked (tripped to OPEN), got: %s", res.Status)
			}
		}
	}

	// Verify breaker is OPEN
	if state := cb.GetState("home-web"); state != CircuitOpen {
		t.Fatalf("expected breaker state OPEN, got: %s", state)
	}

	// Step 2: Immediate recovery attempt during cooldown MUST be rejected
	canRecover, ticket, err := cb.CanAttemptRecovery("home-web")
	if canRecover || ticket != nil || !errors.Is(err, ErrCircuitBreakerOpen) {
		t.Fatalf("expected recovery to be blocked during cooldown, got can=%v, ticket=%v, err=%v", canRecover, ticket, err)
	}

	// Step 3: Wait for quiet cooldown period to elapse
	time.Sleep(80 * time.Millisecond)

	// Step 4: Process incoming crash event via ProcessAndRecover
	// Decoupling check: The crash event MUST NOT be counted as a probe failure!
	// It should grant a HALF_OPEN probe ticket and execute physical recovery!
	res, err := fsm.ProcessAndRecover(ctx, event)
	if err != nil {
		t.Fatalf("expected ProcessAndRecover to succeed in HALF_OPEN, got err: %v", err)
	}

	if res.Status != StatusRecovered {
		t.Fatalf("expected StatusRecovered, got: %s (error: %s)", res.Status, res.Error)
	}
	if !res.Executed {
		t.Fatalf("expected Executed == true")
	}
	if !res.HealthVerified {
		t.Fatalf("expected HealthVerified == true")
	}
	if res.Decision.ProbeToken == "" {
		t.Fatalf("expected non-empty ProbeToken in recovery decision")
	}

	// Step 5: Verify terminal state transitioned to CLOSED
	if finalState := cb.GetState("home-web"); finalState != CircuitClosed {
		t.Fatalf("expected breaker state CLOSED after successful probe, got: %s", finalState)
	}

	// Step 6: Verify DB persistence reflects CLOSED
	var dbState model.OpsCircuitBreakerState
	if err := db.Where("container = ?", "home-web").First(&dbState).Error; err != nil {
		t.Fatalf("failed to query persisted circuit breaker state: %v", err)
	}
	if dbState.State != string(CircuitClosed) {
		t.Fatalf("expected persisted state CLOSED in DB, got: %s", dbState.State)
	}
}

// 2. ProcessAndRecover failure -> RecordRecoveryFailure -> immediately trips back to OPEN (R2.4, R10.4)
func TestRound3_CircuitBreaker_HalfOpen_Failure_TripsBackToOpen(t *testing.T) {
	db := setupCircuitTestDB(t)

	executor := NewOperationExecutor(
		db,
		nil,
		nil,
		nil,
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			return "container restarted", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			if ResolveTopologyComponent(target) == "home-web" {
				return errors.New("simulated health probe connection refused :80")
			}
			return nil
		}),
	)

	fsm := NewContainerRecoveryFSM(db, nil, executor)
	cb := fsm.GetCircuitBreaker()
	cb.SetCooldownDuration(50 * time.Millisecond)

	ctx := context.Background()
	event := ContainerEvent{
		ContainerName: "home-web",
		Action:        "die",
		ExitCode:      1,
		Timestamp:     time.Now(),
	}

	// Trip breaker to OPEN
	for i := 0; i < 3; i++ {
		cb.RecordCrash("home-web")
	}
	if cb.GetState("home-web") != CircuitOpen {
		t.Fatalf("expected breaker to be OPEN")
	}

	// Wait for cooldown
	time.Sleep(60 * time.Millisecond)

	// Attempt recovery in HALF_OPEN where health probe fails
	res, err := fsm.ProcessAndRecover(ctx, event)
	if err == nil {
		t.Fatalf("expected recovery to return error due to failed health probe")
	}
	if res.Status != StatusFailed {
		t.Fatalf("expected StatusFailed, got: %s", res.Status)
	}

	// Breaker MUST immediately trip back to CircuitOpen! (R2.4)
	if state := cb.GetState("home-web"); state != CircuitOpen {
		t.Fatalf("expected breaker to trip back to CircuitOpen on probe failure, got: %s", state)
	}

	// Active probe MUST be cleared (no probe leak!) (R10.4)
	if probe, exists := cb.GetActiveProbe("home-web"); exists || probe != nil {
		t.Fatalf("expected active probe to be cleared after failure, got: %v", probe)
	}

	// Verify DB state is OPEN
	var dbState model.OpsCircuitBreakerState
	if err := db.Where("container = ?", "home-web").First(&dbState).Error; err != nil {
		t.Fatalf("failed to query DB: %v", err)
	}
	if dbState.State != string(CircuitOpen) {
		t.Fatalf("expected persisted state OPEN, got: %s", dbState.State)
	}
}

// 3. Probe concurrency: concurrent events during HALF_OPEN only grant 1 probe (R10.2)
func TestRound3_CircuitBreaker_ProbeConcurrency_SingleTicket(t *testing.T) {
	db := setupCircuitTestDB(t)

	fsm := NewContainerRecoveryFSM(db, nil, nil)
	cb := fsm.GetCircuitBreaker()
	cb.SetCooldownDuration(10 * time.Millisecond)

	// Trip breaker to OPEN
	for i := 0; i < 3; i++ {
		cb.RecordCrash("home-web")
	}
	time.Sleep(20 * time.Millisecond) // Cooldown elapsed

	goroutines := 30
	var successCount atomic.Int32
	var blockedCount atomic.Int32
	var ticketTokens sync.Map

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			allowed, ticket, err := cb.CanAttemptRecovery("home-web")
			if allowed && ticket != nil {
				successCount.Add(1)
				ticketTokens.Store(ticket.Token, true)
			} else if !allowed && errors.Is(err, ErrProbeInProgress) {
				blockedCount.Add(1)
			}
		}()
	}

	wg.Wait()

	if successCount.Load() != 1 {
		t.Fatalf("expected strictly 1 goroutine to acquire probe ticket, got: %d", successCount.Load())
	}
	if blockedCount.Load() != int32(goroutines-1) {
		t.Fatalf("expected %d goroutines to be blocked with ErrProbeInProgress, got: %d", goroutines-1, blockedCount.Load())
	}

	// Confirm exactly 1 active probe ticket exists
	activeProbe, exists := cb.GetActiveProbe("home-web")
	if !exists || activeProbe == nil {
		t.Fatalf("expected active probe ticket to exist")
	}
}

// 4. Probe timeout: probe expires after TTL; state resets to OPEN (R10.4)
func TestRound3_CircuitBreaker_ProbeTimeout_ResetsToOpen(t *testing.T) {
	db := setupCircuitTestDB(t)

	fsm := NewContainerRecoveryFSM(db, nil, nil)
	cb := fsm.GetCircuitBreaker()
	cb.SetCooldownDuration(10 * time.Millisecond)
	cb.SetProbeTTL(50 * time.Millisecond) // Short probe lease

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

	// Wait for probe lease to expire
	time.Sleep(70 * time.Millisecond)

	// Next CanAttemptRecovery call MUST detect timeout and reset to OPEN (R10.4)
	canRecover, _, timeoutErr := cb.CanAttemptRecovery("home-web")
	if canRecover || !errors.Is(timeoutErr, ErrProbeTimeout) {
		t.Fatalf("expected ErrProbeTimeout on expired probe, got can=%v, err=%v", canRecover, timeoutErr)
	}

	if state := cb.GetState("home-web"); state != CircuitOpen {
		t.Fatalf("expected state reset to OPEN after probe timeout, got: %s", state)
	}

	// Probe ticket MUST be cleared
	if _, exists := cb.GetActiveProbe("home-web"); exists {
		t.Fatalf("expected expired probe to be cleared from activeProbes")
	}
}

// 5. Late callback protection: stale token rejected, does not alter circuit state (R10.3, R10.5)
func TestRound3_CircuitBreaker_StaleProbe_LateCallback_Rejected(t *testing.T) {
	db := setupCircuitTestDB(t)

	fsm := NewContainerRecoveryFSM(db, nil, nil)
	cb := fsm.GetCircuitBreaker()
	cb.SetCooldownDuration(10 * time.Millisecond)
	cb.SetProbeTTL(50 * time.Millisecond)

	// Trip breaker and acquire probe ticket T1
	for i := 0; i < 3; i++ {
		cb.RecordCrash("home-web")
	}
	time.Sleep(20 * time.Millisecond)

	allowed, ticket1, _ := cb.CanAttemptRecovery("home-web")
	if !allowed || ticket1 == nil {
		t.Fatalf("failed to acquire ticket 1")
	}

	// Let ticket 1 expire and trigger reset to OPEN
	time.Sleep(70 * time.Millisecond)
	_, _, _ = cb.CanAttemptRecovery("home-web") // Detect timeout, resets to OPEN

	if cb.GetState("home-web") != CircuitOpen {
		t.Fatalf("expected state OPEN")
	}

	// Late callback from ticket 1 arrives attempting to record success
	err := cb.RecordRecoverySuccess("home-web", ticket1.Token)
	if err == nil || !errors.Is(err, ErrStaleProbeToken) {
		t.Fatalf("expected ErrStaleProbeToken on late callback in OPEN state, got: %v", err)
	}

	// State MUST NOT be altered to CLOSED! (R10.5)
	if state := cb.GetState("home-web"); state != CircuitOpen {
		t.Fatalf("expected circuit to REMAIN OPEN after late callback rejection, got: %s", state)
	}

	// Wait for cooldown again and acquire ticket 2
	time.Sleep(20 * time.Millisecond)
	allowed2, ticket2, _ := cb.CanAttemptRecovery("home-web")
	if !allowed2 || ticket2 == nil {
		t.Fatalf("failed to acquire ticket 2")
	}

	// Attempt to report success using stale ticket 1 token while ticket 2 is active
	errStale := cb.RecordRecoverySuccess("home-web", ticket1.Token)
	if errStale == nil || !errors.Is(errStale, ErrStaleProbeToken) {
		t.Fatalf("expected ErrStaleProbeToken for mismatched token, got: %v", errStale)
	}

	// Attempt to report failure using stale ticket 1 token
	errFailStale := cb.RecordRecoveryFailure("home-web", ticket1.Token, "late fail")
	if errFailStale == nil || !errors.Is(errFailStale, ErrStaleProbeToken) {
		t.Fatalf("expected ErrStaleProbeToken for mismatched token in RecordRecoveryFailure, got: %v", errFailStale)
	}

	// Active probe ticket 2 MUST NOT be disturbed
	active, exists := cb.GetActiveProbe("home-web")
	if !exists || active.Token != ticket2.Token {
		t.Fatalf("expected active probe to remain ticket 2")
	}
}

// 6. Process restart state recovery: loading saved state from DB (R10.6, R11.4)
func TestRound3_CircuitBreaker_ProcessRestart_OrphanedProbeRecovery(t *testing.T) {
	db := setupCircuitTestDB(t)

	// Simulate a previous process that died while in HALF_OPEN with an orphaned probe
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

	// Spin up fresh FSM instance loading this DB
	fsm := NewContainerRecoveryFSM(db, nil, nil)
	cb := fsm.GetCircuitBreaker()

	// Orphaned probe MUST be recognized on restart and reset to CircuitOpen (R10.6)
	if state := cb.GetState("home-web"); state != CircuitOpen {
		t.Fatalf("expected orphaned HALF_OPEN state to reset to CircuitOpen on restart, got: %s", state)
	}

	// No zombie probes in activeProbes map
	if _, exists := cb.GetActiveProbe("home-web"); exists {
		t.Fatalf("expected activeProbes to be empty of zombie probes")
	}

	// Since openedAt was 20m ago (>= 15m default cooldown), CanAttemptRecovery can immediately grant a clean probe
	canRecover, ticket, err := cb.CanAttemptRecovery("home-web")
	if !canRecover || ticket == nil || err != nil {
		t.Fatalf("expected fresh probe acquisition to succeed after restart, got can=%v, err=%v", canRecover, err)
	}
}

// 7. MaintenanceLock Acquire SQLite write failure rolls back memory state (R6.2, R6.6)
func TestRound3_MaintenanceLock_Acquire_PersistenceFailure_FailsClosed(t *testing.T) {
	db := setupCircuitTestDB(t)
	lock := NewMaintenanceLock()
	lock.SetDB(db)

	// Inject SQL trigger to abort writes on ops_maintenance_locks
	err := db.Exec(`CREATE TRIGGER fail_lock_insert BEFORE INSERT ON ops_maintenance_locks 
		BEGIN SELECT RAISE(ABORT, 'simulated disk write failure'); END;`).Error
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

	// In-memory lock state MUST NOT be active (rolled back) (R6.6)
	if lock.IsLocked() {
		t.Fatalf("expected lock.IsLocked() == false after failed acquisition persistence")
	}
}

// 8. MaintenanceLock Release DB write failure retains in-memory lock (R6.6, R11.1)
func TestRound3_MaintenanceLock_Release_PersistenceFailure_RetainsMemoryLock(t *testing.T) {
	db := setupCircuitTestDB(t)
	lock := NewMaintenanceLock()
	lock.SetDB(db)

	// Acquire lock successfully
	err := lock.Acquire("admin", "planned upgrade", 30*time.Minute)
	if err != nil {
		t.Fatalf("failed to acquire lock: %v", err)
	}
	if !lock.IsLocked() {
		t.Fatalf("expected lock to be active")
	}

	// Inject write failure on ops_maintenance_locks
	err = db.Exec(`CREATE TRIGGER fail_lock_release BEFORE INSERT ON ops_maintenance_locks 
		BEGIN SELECT RAISE(ABORT, 'simulated disk I/O lock error'); END;`).Error
	if err != nil {
		t.Fatalf("failed to create trigger: %v", err)
	}

	// Attempt Release
	err = lock.Release("admin")
	if err == nil {
		t.Fatalf("expected Release to fail when persistence fails, got nil")
	}
	if !strings.Contains(err.Error(), "memory lock retained") {
		t.Fatalf("expected error mentioning memory lock retained, got: %v", err)
	}

	// CRITICAL ASSERTION: Memory lock MUST RETAIN ACTIVE PROTECTION! (R6.6, R11.1)
	if !lock.IsLocked() {
		t.Fatalf("VIOLATION: Maintenance lock memory protection was cleared despite DB write failure!")
	}
}

// 9. CircuitBreaker persist failure returns error and enforces fail-closed (R6.3, R11.1)
func TestRound3_CircuitBreaker_PersistFailure_FailClosed(t *testing.T) {
	db := setupCircuitTestDB(t)
	cb := NewContainerCircuitBreaker()
	cb.SetDB(db)
	cb.SetCooldownDuration(10 * time.Millisecond)

	// Trip breaker
	for i := 0; i < 3; i++ {
		cb.RecordCrash("home-web")
	}
	time.Sleep(20 * time.Millisecond)

	// Inject failure on ops_circuit_breaker_states
	err := db.Exec(`CREATE TRIGGER fail_cb_update BEFORE UPDATE ON ops_circuit_breaker_states 
		BEGIN SELECT RAISE(ABORT, 'simulated disk read-only failure'); END;`).Error
	if err != nil {
		t.Fatalf("failed to create trigger: %v", err)
	}

	// Attempt transition to HALF_OPEN
	allowed, ticket, cbErr := cb.CanAttemptRecovery("home-web")
	if allowed || ticket != nil || cbErr == nil {
		t.Fatalf("expected CanAttemptRecovery to fail when DB update fails, got allowed=%v, err=%v", allowed, cbErr)
	}

	// Must rollback to CircuitOpen and not hold active probe
	if state := cb.GetState("home-web"); state != CircuitOpen {
		t.Fatalf("expected state rolled back to CircuitOpen, got: %s", state)
	}
	if _, exists := cb.GetActiveProbe("home-web"); exists {
		t.Fatalf("expected no active probe held in memory after persist failure")
	}
}

// 10. Database load failure enters fail-closed degraded mode (R6.5, R11.4)
func TestRound3_DBLoad_Corrupt_EntersProtectedMode(t *testing.T) {
	db := setupCircuitTestDB(t)

	// Close database to simulate unreadable / corrupt SQLite storage
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

// 11. Proposal ID generation with UnixNano prevents collision on rapid trips (R2.4)
func TestRound3_CircuitTripProposal_NanoTimestamp_NoCollision(t *testing.T) {
	db := setupCircuitTestDB(t)
	fsm := NewContainerRecoveryFSM(db, nil, nil)

	count := 20
	var wg sync.WaitGroup
	wg.Add(count)

	validContainers := []string{"home-web", "home-api", "frigate", "mosquitto", "home-vision"}
	for i := 0; i < count; i++ {
		go func(idx int) {
			defer wg.Done()
			fsm.createCircuitTripProposal(validContainers[idx%len(validContainers)], 3)
		}(i)
	}

	wg.Wait()

	var proposals []model.OpsProposal
	if err := db.Find(&proposals).Error; err != nil {
		t.Fatalf("failed to query proposals: %v", err)
	}

	if len(proposals) != count {
		t.Fatalf("expected %d proposals created without collision, got: %d", count, len(proposals))
	}

	idMap := make(map[string]bool)
	for _, p := range proposals {
		if idMap[p.ProposalID] {
			t.Fatalf("duplicate proposal ID detected: %s", p.ProposalID)
		}
		idMap[p.ProposalID] = true
	}
}
