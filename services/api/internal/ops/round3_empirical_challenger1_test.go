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
	"home-datacenter-api/internal/utils"
)

// setupChallenger1TestDB initializes an isolated in-memory SQLite database.
func setupChallenger1TestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbName := fmt.Sprintf("file:chal1_%s_%d?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"), time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test sqlite db: %v", err)
	}
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
		_, _ = sqlDB.Exec("PRAGMA busy_timeout = 5000;")
	}

	err = db.AutoMigrate(
		&model.OpsMaintenanceLock{},
		&model.OpsCircuitBreakerState{},
		&model.OpsProposal{},
		&model.OpsExecutionHistory{},
		&RecoveryLease{},
	)
	if err != nil {
		t.Fatalf("failed to automigrate test db: %v", err)
	}
	return db
}

// ============================================================================
// CHALLENGE 1: Probe Ticket Lifecycle Under Stress
// ============================================================================

// TestChallenger1_ProbeTicket_ExtremeConcurrency verifies that when 100 goroutines
// concurrently contest for recovery probe in HALF_OPEN, EXACTLY 1 ticket is granted,
// and 99 are rejected with ErrProbeInProgress.
func TestChallenger1_ProbeTicket_ExtremeConcurrency(t *testing.T) {
	db := setupChallenger1TestDB(t)
	cb := NewContainerCircuitBreaker()
	cb.SetDB(db)
	cb.cooldownDuration = 10 * time.Millisecond
	cb.probeTTL = 500 * time.Millisecond

	// Trip circuit breaker to OPEN
	for i := 0; i < 3; i++ {
		cb.RecordCrash("home-web")
	}
	if cb.GetState("home-web") != CircuitOpen {
		t.Fatalf("expected circuit OPEN, got: %s", cb.GetState("home-web"))
	}

	// Wait for cooldown to expire
	time.Sleep(20 * time.Millisecond)

	const workerCount = 100
	var (
		startBarrier sync.WaitGroup
		doneBarrier  sync.WaitGroup
		successCount atomic.Int32
		blockedCount atomic.Int32
		errCount     atomic.Int32
		issuedTicket *ProbeTicket
		ticketMutex  sync.Mutex
	)

	startBarrier.Add(1)
	for i := 0; i < workerCount; i++ {
		doneBarrier.Add(1)
		go func(id int) {
			defer doneBarrier.Done()
			startBarrier.Wait() // Synchronized thundering herd release

			allowed, ticket, err := cb.CanAttemptRecovery("home-web")
			if allowed && ticket != nil && err == nil {
				successCount.Add(1)
				ticketMutex.Lock()
				issuedTicket = ticket
				ticketMutex.Unlock()
			} else if !allowed && errors.Is(err, ErrProbeInProgress) {
				blockedCount.Add(1)
			} else {
				errCount.Add(1)
			}
		}(i)
	}

	startBarrier.Done()
	doneBarrier.Wait()

	if successCount.Load() != 1 {
		t.Fatalf("VIOLATION: Expected exactly 1 winner to receive probe ticket, got %d", successCount.Load())
	}
	if blockedCount.Load() != workerCount-1 {
		t.Fatalf("VIOLATION: Expected exactly %d workers blocked with ErrProbeInProgress, got %d (errs: %d)",
			workerCount-1, blockedCount.Load(), errCount.Load())
	}
	if issuedTicket == nil || issuedTicket.Token == "" {
		t.Fatalf("VIOLATION: Winner did not receive a valid non-empty probe ticket")
	}
	if cb.GetState("home-web") != CircuitHalfOpen {
		t.Fatalf("expected circuit state HALF_OPEN, got: %s", cb.GetState("home-web"))
	}
}

// TestChallenger1_ProbeTicket_StaleAndLateCallbacks verifies that invalid tokens,
// stale tokens, cross-component tokens, and late tokens after reset are rejected.
func TestChallenger1_ProbeTicket_StaleAndLateCallbacks(t *testing.T) {
	db := setupChallenger1TestDB(t)
	cb := NewContainerCircuitBreaker()
	cb.SetDB(db)
	cb.cooldownDuration = 10 * time.Millisecond
	cb.probeTTL = 200 * time.Millisecond

	// Trip home-web to OPEN
	for i := 0; i < 3; i++ {
		cb.RecordCrash("home-web")
	}
	time.Sleep(20 * time.Millisecond)

	// Acquire valid probe ticket for home-web
	allowed, ticket, err := cb.CanAttemptRecovery("home-web")
	if !allowed || ticket == nil || err != nil {
		t.Fatalf("failed to acquire probe ticket: %v", err)
	}

	// 1. Replay with empty token in HALF_OPEN -> ErrStaleProbeToken
	if err := cb.RecordRecoverySuccess("home-web", ""); !errors.Is(err, ErrStaleProbeToken) {
		t.Fatalf("expected ErrStaleProbeToken on empty token, got: %v", err)
	}

	// 2. Replay with forged/bogus token in HALF_OPEN -> ErrStaleProbeToken
	if err := cb.RecordRecoverySuccess("home-web", "probe-forged-random-token"); !errors.Is(err, ErrStaleProbeToken) {
		t.Fatalf("expected ErrStaleProbeToken on forged token, got: %v", err)
	}

	// 3. Cross-component replay: Trip frigate to HALF_OPEN, then try to use home-web's ticket on frigate
	for i := 0; i < 3; i++ {
		cb.RecordCrash("frigate")
	}
	time.Sleep(20 * time.Millisecond)
	allowedFrig, frigTicket, _ := cb.CanAttemptRecovery("frigate")
	if !allowedFrig || frigTicket == nil {
		t.Fatalf("failed to put frigate in HALF_OPEN")
	}
	// Try to submit home-web's ticket for frigate -> MUST FAIL with ErrStaleProbeToken
	if err := cb.RecordRecoverySuccess("frigate", ticket.Token); !errors.Is(err, ErrStaleProbeToken) {
		t.Fatalf("VIOLATION: Expected ErrStaleProbeToken when reusing home-web probe ticket on frigate, got: %v", err)
	}

	// 4. Token submitted to OPEN circuit -> ErrStaleProbeToken
	for i := 0; i < 3; i++ {
		cb.RecordCrash("home-api")
	}
	if cb.GetState("home-api") != CircuitOpen {
		t.Fatalf("expected home-api to be OPEN")
	}
	if err := cb.RecordRecoverySuccess("home-api", ticket.Token); !errors.Is(err, ErrStaleProbeToken) {
		t.Fatalf("VIOLATION: Expected ErrStaleProbeToken on token submitted to OPEN breaker, got: %v", err)
	}

	// 5. RecordRecoveryFailure with forged token -> ErrStaleProbeToken
	if err := cb.RecordRecoveryFailure("home-web", "fake-token", "failure"); !errors.Is(err, ErrStaleProbeToken) {
		t.Fatalf("expected ErrStaleProbeToken on failure with forged token, got: %v", err)
	}

	// 6. Valid token completes probe transition to CLOSED
	if err := cb.RecordRecoverySuccess("home-web", ticket.Token); err != nil {
		t.Fatalf("expected RecordRecoverySuccess to succeed with valid token, got: %v", err)
	}
	if cb.GetState("home-web") != CircuitClosed {
		t.Fatalf("expected state CLOSED after successful probe, got: %s", cb.GetState("home-web"))
	}

	// 7. Stale callback: Re-submitting the same token after circuit is re-tripped to OPEN -> MUST BE REJECTED
	for i := 0; i < 3; i++ {
		cb.RecordCrash("home-web")
	}
	if cb.GetState("home-web") != CircuitOpen {
		t.Fatalf("expected circuit OPEN, got: %s", cb.GetState("home-web"))
	}
	if err := cb.RecordRecoverySuccess("home-web", ticket.Token); !errors.Is(err, ErrStaleProbeToken) {
		t.Fatalf("expected ErrStaleProbeToken on old token submitted to OPEN breaker, got: %v", err)
	}
}

// TestChallenger1_ProbeTicket_Expiration_And_LateHolderRejection verifies that when
// probe TTL expires:
// 1. CanAttemptRecovery returns ErrProbeTimeout and trips back to OPEN.
// 2. The late holder calling RecordRecoverySuccess is rejected with ErrStaleProbeToken or ErrProbeExpired.
func TestChallenger1_ProbeTicket_Expiration_And_LateHolderRejection(t *testing.T) {
	db := setupChallenger1TestDB(t)
	cb := NewContainerCircuitBreaker()
	cb.SetDB(db)
	cb.cooldownDuration = 10 * time.Millisecond
	cb.probeTTL = 30 * time.Millisecond

	// Trip breaker
	for i := 0; i < 3; i++ {
		cb.RecordCrash("home-web")
	}
	time.Sleep(15 * time.Millisecond)

	// Acquire probe ticket
	allowed, ticket, err := cb.CanAttemptRecovery("home-web")
	if !allowed || ticket == nil || err != nil {
		t.Fatalf("failed to acquire probe ticket: %v", err)
	}

	// Wait for ticket lease to expire
	time.Sleep(50 * time.Millisecond)

	// CanAttemptRecovery should notice expired probe, clear it, and trip back to OPEN
	allowed2, _, err2 := cb.CanAttemptRecovery("home-web")
	if allowed2 || !errors.Is(err2, ErrProbeTimeout) {
		t.Fatalf("expected ErrProbeTimeout after lease expired, got allowed=%v, err=%v", allowed2, err2)
	}
	if cb.GetState("home-web") != CircuitOpen {
		t.Fatalf("expected circuit state reset to OPEN after probe timeout, got: %s", cb.GetState("home-web"))
	}

	// Late callback from the slow holder must be strictly rejected!
	lateErr := cb.RecordRecoverySuccess("home-web", ticket.Token)
	if lateErr == nil || (!errors.Is(lateErr, ErrStaleProbeToken) && !errors.Is(lateErr, ErrProbeExpired)) {
		t.Fatalf("expected ErrStaleProbeToken or ErrProbeExpired for late probe callback, got: %v", lateErr)
	}
}

// ============================================================================
// CHALLENGE 2: MaintenanceLock & CircuitBreaker Fail-Closed Persistence
// ============================================================================

// TestChallenger2_MaintenanceLock_Acquire_PersistenceFailure verifies that
// if SQLite write fails during Acquire, memory lock is rolled back,
// IsLocked() remains false, and error is returned.
func TestChallenger2_MaintenanceLock_Acquire_PersistenceFailure(t *testing.T) {
	db := setupChallenger1TestDB(t)
	lock := NewMaintenanceLock()
	lock.SetDB(db)

	// Inject a real SQLite trigger to fail all inserts into ops_maintenance_locks
	err := db.Exec(`
		CREATE TRIGGER fail_maintenance_acquire BEFORE INSERT ON ops_maintenance_locks
		BEGIN
			SELECT RAISE(FAIL, 'simulated disk write failure on lock acquire');
		END;
	`).Error
	if err != nil {
		t.Fatalf("failed to create sqlite trigger: %v", err)
	}

	acqErr := lock.Acquire("admin", "emergency maintenance", 30*time.Minute)
	if acqErr == nil {
		t.Fatalf("VIOLATION: Expected Acquire to fail when database write fails, got nil")
	}

	// CRITICAL INVARIANT: Memory state must NOT claim lock active if persistence failed!
	if lock.IsLocked() {
		t.Fatalf("VIOLATION: In-memory lock is active even though persistence failed! (Memory-DB mismatch)")
	}
}

// TestChallenger2_MaintenanceLock_Release_PersistenceFailure_RetainsMemoryLock verifies
// R6.6 & R11.1: When Release() fails to persist to SQLite, the in-memory protection
// MUST NOT be dropped! System MUST remain locked.
func TestChallenger2_MaintenanceLock_Release_PersistenceFailure_RetainsMemoryLock(t *testing.T) {
	db := setupChallenger1TestDB(t)
	lock := NewMaintenanceLock()
	lock.SetDB(db)

	// 1. Successfully acquire lock
	if err := lock.Acquire("admin", "planned work", 30*time.Minute); err != nil {
		t.Fatalf("failed to acquire lock: %v", err)
	}
	if !lock.IsLocked() {
		t.Fatalf("expected lock to be active")
	}

	// 2. Inject trigger to fail subsequent inserts (which Release uses to record lock release)
	err := db.Exec(`
		CREATE TRIGGER fail_maintenance_release BEFORE INSERT ON ops_maintenance_locks
		BEGIN
			SELECT RAISE(FAIL, 'simulated disk I/O lock error on release');
		END;
	`).Error
	if err != nil {
		t.Fatalf("failed to create release trigger: %v", err)
	}

	// 3. Attempt Release
	relErr := lock.Release("admin")
	if relErr == nil {
		t.Fatalf("VIOLATION: Expected Release to return error when DB write fails, got nil")
	}

	// 4. CRITICAL INVARIANT (R6.6, R11.1): In-memory lock MUST BE RETAINED!
	if !lock.IsLocked() {
		t.Fatalf("CRITICAL SECURITY VIOLATION: Memory lock was dropped despite DB release persistence failure!")
	}
}

// TestChallenger2_CircuitBreaker_PersistFailure_RollsBackMemoryState verifies that
// when CircuitBreaker transitions from OPEN to HALF_OPEN, if SQLite persistence fails,
// memory state rolls back to OPEN and active probe is dropped.
func TestChallenger2_CircuitBreaker_PersistFailure_RollsBackMemoryState(t *testing.T) {
	db := setupChallenger1TestDB(t)
	cb := NewContainerCircuitBreaker()
	cb.SetDB(db)
	cb.cooldownDuration = 10 * time.Millisecond

	// Trip to OPEN
	for i := 0; i < 3; i++ {
		cb.RecordCrash("home-web")
	}
	time.Sleep(20 * time.Millisecond)

	// Inject SQLite trigger to fail ops_circuit_breaker_states updates/inserts
	err := db.Exec(`
		CREATE TRIGGER fail_cb_update BEFORE UPDATE ON ops_circuit_breaker_states
		BEGIN
			SELECT RAISE(FAIL, 'simulated disk write failure on breaker update');
		END;
		CREATE TRIGGER fail_cb_insert BEFORE INSERT ON ops_circuit_breaker_states
		BEGIN
			SELECT RAISE(FAIL, 'simulated disk write failure on breaker insert');
		END;
	`).Error
	if err != nil {
		t.Fatalf("failed to create trigger: %v", err)
	}

	// Attempt recovery transition to HALF_OPEN
	allowed, ticket, err := cb.CanAttemptRecovery("home-web")
	if allowed || ticket != nil || err == nil {
		t.Fatalf("VIOLATION: Expected CanAttemptRecovery to fail when DB persistence fails, got allowed=%v, err=%v", allowed, err)
	}

	// CRITICAL INVARIANT: Memory state must remain OPEN and active probe must be cleared
	if cb.GetState("home-web") != CircuitOpen {
		t.Fatalf("VIOLATION: Circuit breaker memory state was not rolled back to OPEN on persist failure, got: %s", cb.GetState("home-web"))
	}
}

// TestChallenger2_DegradedMode_OnCorruptDatabase verifies that when LoadPersistedState
// encounters a database error, both MaintenanceLock and CircuitBreaker enter fail-closed degraded mode.
func TestChallenger2_DegradedMode_OnCorruptDatabase(t *testing.T) {
	db := setupChallenger1TestDB(t)

	// Drop tables to induce query failures during load
	if err := db.Migrator().DropTable(&model.OpsMaintenanceLock{}, &model.OpsCircuitBreakerState{}); err != nil {
		t.Fatalf("failed to drop tables: %v", err)
	}

	// Test MaintenanceLock fail-closed degraded mode
	lock := NewMaintenanceLock()
	lock.LoadPersistedState(db)

	if !lock.IsDegraded() {
		t.Fatalf("VIOLATION: MaintenanceLock did not enter degraded mode on database error")
	}
	if !lock.IsLocked() {
		t.Fatalf("VIOLATION: MaintenanceLock IsLocked() returned false while degraded! Must fail closed!")
	}
	if err := lock.Acquire("operator", "test", 10*time.Minute); !errors.Is(err, ErrFailClosedMode) {
		t.Fatalf("VIOLATION: MaintenanceLock Acquire did not return ErrFailClosedMode when degraded, got: %v", err)
	}

	// Test CircuitBreaker fail-closed degraded mode
	cb := NewContainerCircuitBreaker()
	cb.LoadPersistedState(db)

	allowed, ticket, err := cb.CanAttemptRecovery("home-web")
	if allowed || ticket != nil || !errors.Is(err, ErrFailClosedMode) {
		t.Fatalf("VIOLATION: CircuitBreaker CanAttemptRecovery did not fail closed on degraded mode, got allowed=%v, err=%v", allowed, err)
	}
}

// ============================================================================
// CHALLENGE 3: Dual-Path Lease Race Conditions (ops_recovery_leases)
// ============================================================================

// TestChallenger3_DualPathLease_MultiProcessContention verifies that when
// 100 concurrent workers with distinct process/worker IDs contest for
// the same component recovery lease, EXACTLY 1 winner acquires the lease,
// and 99 fail closed with ErrLeaseHeld.
func TestChallenger3_DualPathLease_MultiProcessContention(t *testing.T) {
	db := setupChallenger1TestDB(t)
	mgr := NewRecoveryLeaseManager(db)

	const workerCount = 100
	var (
		startBarrier sync.WaitGroup
		doneBarrier  sync.WaitGroup
		winnerCount  atomic.Int32
		blockedCount atomic.Int32
		errCount     atomic.Int32
		winningToken string
		tokenMutex   sync.Mutex
	)

	startBarrier.Add(1)
	for i := 0; i < workerCount; i++ {
		doneBarrier.Add(1)
		holder := fmt.Sprintf("worker-%03d", i)
		go func(workerID int, h string) {
			defer doneBarrier.Done()
			startBarrier.Wait()

			token, err := mgr.AcquireLease("frigate", h, 10*time.Second)
			if err == nil && token != "" {
				winnerCount.Add(1)
				tokenMutex.Lock()
				winningToken = token
				tokenMutex.Unlock()
			} else if errors.Is(err, ErrLeaseHeld) {
				blockedCount.Add(1)
			} else {
				errCount.Add(1)
			}
		}(i, holder)
	}

	startBarrier.Done()
	doneBarrier.Wait()

	if winnerCount.Load() != 1 {
		t.Fatalf("VIOLATION: Expected exactly 1 winner to acquire lease, got %d", winnerCount.Load())
	}
	if blockedCount.Load() != workerCount-1 {
		t.Fatalf("VIOLATION: Expected exactly %d workers blocked with ErrLeaseHeld, got %d (unexpected errs: %d)",
			workerCount-1, blockedCount.Load(), errCount.Load())
	}

	// Verify the database record matches the winner
	lease, err := mgr.GetLease("frigate")
	if err != nil || lease == nil {
		t.Fatalf("failed to retrieve active lease from DB: %v", err)
	}
	if lease.LeaseToken != winningToken {
		t.Fatalf("VIOLATION: Persisted lease token %s does not match winning token %s", lease.LeaseToken, winningToken)
	}
}

// TestChallenger3_DualPathLease_CrossPathMutualExclusion verifies that
// when Go Core holds a lease, Python Ops Agent is blocked with ErrLeaseHeld,
// and vice versa.
func TestChallenger3_DualPathLease_CrossPathMutualExclusion(t *testing.T) {
	db := setupChallenger1TestDB(t)
	mgr := NewRecoveryLeaseManager(db)

	// 1. Go Core acquires lease
	goToken, err := mgr.AcquireLease("frigate", "go-core", 10*time.Second)
	if err != nil || goToken == "" {
		t.Fatalf("failed to acquire lease for go-core: %v", err)
	}

	// 2. Python Ops Agent attempts to acquire for same component -> ErrLeaseHeld
	pyToken, pyErr := mgr.AcquireLease("frigate", "python-ops-agent", 10*time.Second)
	if pyToken != "" || !errors.Is(pyErr, ErrLeaseHeld) {
		t.Fatalf("VIOLATION: Python Ops Agent was able to acquire lease while Go Core held it! err=%v", pyErr)
	}

	// 3. Go Core releases lease
	if err := mgr.ReleaseLease("frigate", goToken); err != nil {
		t.Fatalf("failed to release lease: %v", err)
	}

	// 4. Now Python Ops Agent can acquire lease
	pyToken2, pyErr2 := mgr.AcquireLease("frigate", "python-ops-agent", 10*time.Second)
	if pyErr2 != nil || pyToken2 == "" {
		t.Fatalf("failed for python-ops-agent to acquire released lease: %v", pyErr2)
	}

	// 5. Go Core attempts to acquire -> ErrLeaseHeld
	goToken2, goErr2 := mgr.AcquireLease("frigate", "go-core", 10*time.Second)
	if goToken2 != "" || !errors.Is(goErr2, ErrLeaseHeld) {
		t.Fatalf("VIOLATION: Go Core was able to acquire lease while Python held it! err=%v", goErr2)
	}
}

// TestChallenger3_DualPathLease_CanonicalComponentAliasRace verifies that
// concurrent requests using different alias representations ("home-frigate",
// "frigate", "nvr", "/home-frigate") with distinct holders all resolve to
// the canonical ID and only 1 succeeds.
func TestChallenger3_DualPathLease_CanonicalComponentAliasRace(t *testing.T) {
	db := setupChallenger1TestDB(t)
	mgr := NewRecoveryLeaseManager(db)

	aliases := []string{
		"home-frigate",
		"frigate",
		"nvr",
		"/home-frigate",
		"home-frigate (NVR)",
	}

	var (
		startBarrier sync.WaitGroup
		doneBarrier  sync.WaitGroup
		winnerCount  atomic.Int32
		blockedCount atomic.Int32
	)

	startBarrier.Add(1)
	for i, alias := range aliases {
		doneBarrier.Add(1)
		holder := fmt.Sprintf("alias-worker-%d", i)
		go func(target string, h string) {
			defer doneBarrier.Done()
			startBarrier.Wait()

			_, err := mgr.AcquireLease(target, h, 10*time.Second)
			if err == nil {
				winnerCount.Add(1)
			} else if errors.Is(err, ErrLeaseHeld) {
				blockedCount.Add(1)
			}
		}(alias, holder)
	}

	startBarrier.Done()
	doneBarrier.Wait()

	if winnerCount.Load() != 1 {
		t.Fatalf("VIOLATION: Expected exactly 1 winner across canonical aliases, got %d", winnerCount.Load())
	}
	if blockedCount.Load() != int32(len(aliases)-1) {
		t.Fatalf("VIOLATION: Expected %d aliases blocked with ErrLeaseHeld, got %d", len(aliases)-1, blockedCount.Load())
	}
}

// TestChallenger3_DualPathLease_ExpiredLeaseTakeover verifies that an expired
// recovery lease is safely reclaimed under concurrent contention.
func TestChallenger3_DualPathLease_ExpiredLeaseTakeover(t *testing.T) {
	db := setupChallenger1TestDB(t)
	mgr := NewRecoveryLeaseManager(db)

	// Acquire short-lived lease (20ms TTL)
	token1, err := mgr.AcquireLease("mosquitto", "go-core", 20*time.Millisecond)
	if err != nil || token1 == "" {
		t.Fatalf("failed to acquire initial lease: %v", err)
	}

	// Wait for lease to expire
	time.Sleep(30 * time.Millisecond)

	// 50 concurrent workers contest for the expired lease
	const workerCount = 50
	var (
		startBarrier sync.WaitGroup
		doneBarrier  sync.WaitGroup
		winnerCount  atomic.Int32
		blockedCount atomic.Int32
	)

	startBarrier.Add(1)
	for i := 0; i < workerCount; i++ {
		doneBarrier.Add(1)
		go func(id int) {
			defer doneBarrier.Done()
			startBarrier.Wait()

			holder := fmt.Sprintf("worker-%d", id)
			_, err := mgr.AcquireLease("mosquitto", holder, 10*time.Second)
			if err == nil {
				winnerCount.Add(1)
			} else if errors.Is(err, ErrLeaseHeld) {
				blockedCount.Add(1)
			}
		}(i)
	}

	startBarrier.Done()
	doneBarrier.Wait()

	if winnerCount.Load() != 1 {
		t.Fatalf("VIOLATION: Expected exactly 1 worker to claim expired lease, got %d", winnerCount.Load())
	}
	if blockedCount.Load() != workerCount-1 {
		t.Fatalf("VIOLATION: Expected %d workers blocked on expired takeover, got %d", workerCount-1, blockedCount.Load())
	}
}

// TestChallenger3_DualPathLease_RogueTokenReleaseRejected verifies that
// attempting to release a lease with an incorrect or forged token is rejected.
func TestChallenger3_DualPathLease_RogueTokenReleaseRejected(t *testing.T) {
	db := setupChallenger1TestDB(t)
	mgr := NewRecoveryLeaseManager(db)

	token, err := mgr.AcquireLease("home-api", "go-core", 10*time.Second)
	if err != nil || token == "" {
		t.Fatalf("failed to acquire lease: %v", err)
	}

	// 1. Rogue release with forged token -> ErrInvalidLeaseToken
	if err := mgr.ReleaseLease("home-api", "lease-rogue-attacker-token"); !errors.Is(err, ErrInvalidLeaseToken) {
		t.Fatalf("expected ErrInvalidLeaseToken on rogue token release, got: %v", err)
	}

	// 2. Active lease must remain intact
	active, holder, err := mgr.IsLeaseActive("home-api")
	if err != nil || !active || holder != "go-core" {
		t.Fatalf("VIOLATION: Active lease was invalidated or stolen by rogue release attempt: active=%v, holder=%s", active, holder)
	}

	// 3. Proper release with authentic token succeeds
	if err := mgr.ReleaseLease("home-api", token); err != nil {
		t.Fatalf("failed to release lease with authentic token: %v", err)
	}
}

// ============================================================================
// CHALLENGE 4: Proposal Security & Tamper Detection
// ============================================================================

var challengerProposalCounter atomic.Int64

// setupChallengerProposal creates an approved proposal with authentic ApprovalDigest in DB.
func setupChallengerProposal(t *testing.T, db *gorm.DB) model.OpsProposal {
	t.Helper()
	prop := model.OpsProposal{
		ProposalID:      fmt.Sprintf("prop-chal-%d-%d", time.Now().UnixNano(), challengerProposalCounter.Add(1)),
		Title:           "Challenger Safe Proposal",
		TargetComponent: "home-web",
		ActionSpec:      `{"action":"docker_restart","target":"home-web","params":{"timeout":10}}`,
		ExecutionPlan:   "docker_restart",
		RiskLevel:       model.RiskLevelLow,
		Status:          model.ProposalStatusApproved,
		BaselineHash:    "f5a7e8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7",
		Version:         1,
		HouseholdID:     1,
		ApprovedBy:      "security_auditor",
		ApprovedAt:      utils.NullTime{Time: time.Now(), Valid: true},
	}

	digest, err := ComputeProposalApprovalDigest(&prop)
	if err != nil || digest == "" {
		t.Fatalf("failed to compute approval digest: %v", err)
	}
	prop.ApprovalDigest = digest

	if err := db.Create(&prop).Error; err != nil {
		t.Fatalf("failed to persist proposal: %v", err)
	}
	return prop
}

// TestChallenger4_ProposalSecurity_AllTamperDimensions verifies that modifying
// ANY of the 5 bound fields post-approval triggers ErrApprovalDigestMismatch:
// 1. TargetComponent
// 2. ExecutionPlan / Action in ActionSpec
// 3. Parameters in ActionSpec
// 4. BaselineHash
// 5. Version
func TestChallenger4_ProposalSecurity_AllTamperDimensions(t *testing.T) {
	db := setupChallenger1TestDB(t)

	executor := NewOperationExecutor(
		db,
		nil,
		nil,
		nil,
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			return "executed", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
		WithBaselineExtractor(func(target string) string {
			return "f5a7e8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7"
		}),
	)

	// Dimension 1: TargetComponent tampering post-approval
	t.Run("TargetComponent_Tampering", func(t *testing.T) {
		prop := setupChallengerProposal(t, db)
		// Attacker alters target component in DB from home-web to home-api
		db.Model(&model.OpsProposal{}).Where("id = ?", prop.ID).Update("target_component", "home-api")

		_, err := executor.ExecuteProposal(context.Background(), &prop)
		if !errors.Is(err, ErrApprovalDigestMismatch) {
			t.Fatalf("VIOLATION: Expected ErrApprovalDigestMismatch on TargetComponent tampering, got: %v", err)
		}
	})

	// Dimension 2: ExecutionAction tampering in ActionSpec post-approval
	t.Run("Action_Tampering", func(t *testing.T) {
		prop := setupChallengerProposal(t, db)
		// Attacker alters action inside ActionSpec from docker_restart to container_recreate
		tamperedSpec := `{"action":"container_recreate","target":"home-web","params":{"timeout":10}}`
		db.Model(&model.OpsProposal{}).Where("id = ?", prop.ID).Update("action_spec", tamperedSpec)

		_, err := executor.ExecuteProposal(context.Background(), &prop)
		if !errors.Is(err, ErrApprovalDigestMismatch) {
			t.Fatalf("VIOLATION: Expected ErrApprovalDigestMismatch on Action tampering, got: %v", err)
		}
	})

	// Dimension 3: Parameter tampering in ActionSpec post-approval
	t.Run("Parameters_Tampering", func(t *testing.T) {
		prop := setupChallengerProposal(t, db)
		// Attacker alters ActionSpec parameters (e.g. injects privileged flag and modified timeout)
		tamperedSpec := `{"action":"docker_restart","target":"home-web","params":{"privileged":true,"timeout":999}}`
		db.Model(&model.OpsProposal{}).Where("id = ?", prop.ID).Update("action_spec", tamperedSpec)

		_, err := executor.ExecuteProposal(context.Background(), &prop)
		if !errors.Is(err, ErrApprovalDigestMismatch) {
			t.Fatalf("VIOLATION: Expected ErrApprovalDigestMismatch on Parameter tampering, got: %v", err)
		}
	})

	// Dimension 4: BaselineHash tampering post-approval
	t.Run("BaselineHash_Tampering", func(t *testing.T) {
		prop := setupChallengerProposal(t, db)
		// Attacker changes baseline in DB
		db.Model(&model.OpsProposal{}).Where("id = ?", prop.ID).Update("baseline_hash", "0000000000000000000000000000000000000000000000000000000000000000")

		_, err := executor.ExecuteProposal(context.Background(), &prop)
		if !errors.Is(err, ErrApprovalDigestMismatch) {
			t.Fatalf("VIOLATION: Expected ErrApprovalDigestMismatch on BaselineHash tampering, got: %v", err)
		}
	})

	// Dimension 5: Version tampering post-approval
	t.Run("Version_Tampering", func(t *testing.T) {
		prop := setupChallengerProposal(t, db)
		// Attacker bumps or alters version without re-approval
		db.Model(&model.OpsProposal{}).Where("id = ?", prop.ID).Update("version", 2)

		_, err := executor.ExecuteProposal(context.Background(), &prop)
		if !errors.Is(err, ErrApprovalDigestMismatch) {
			t.Fatalf("VIOLATION: Expected ErrApprovalDigestMismatch on Version tampering, got: %v", err)
		}
	})

	// Dimension 6: Missing / Empty ApprovalDigest in DB
	t.Run("Missing_Digest", func(t *testing.T) {
		prop := setupChallengerProposal(t, db)
		db.Model(&model.OpsProposal{}).Where("id = ?", prop.ID).Update("approval_digest", "")

		_, err := executor.ExecuteProposal(context.Background(), &prop)
		if !errors.Is(err, ErrApprovalDigestMismatch) {
			t.Fatalf("VIOLATION: Expected ErrApprovalDigestMismatch when ApprovalDigest is cleared, got: %v", err)
		}
	})
}

// TestChallenger4_ProposalSecurity_ConcurrentExecutionReplay verifies that
// when 50 concurrent goroutines attempt to execute the exact same approved proposal,
// EXACTLY 1 execution proceeds, and 49 fail with ErrProposalAlreadyRunning or conflict.
func TestChallenger4_ProposalSecurity_ConcurrentExecutionReplay(t *testing.T) {
	db := setupChallenger1TestDB(t)

	var executionCount atomic.Int32
	executor := NewOperationExecutor(
		db,
		nil,
		nil,
		nil,
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			executionCount.Add(1)
			time.Sleep(20 * time.Millisecond) // Simulate execution work
			return "success", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
		WithBaselineExtractor(func(target string) string {
			return "f5a7e8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7"
		}),
	)

	prop := setupChallengerProposal(t, db)

	const concurrency = 50
	var (
		startBarrier sync.WaitGroup
		doneBarrier  sync.WaitGroup
		successCount atomic.Int32
		blockedCount atomic.Int32
		otherErrors  atomic.Int32
	)

	startBarrier.Add(1)
	for i := 0; i < concurrency; i++ {
		doneBarrier.Add(1)
		go func() {
			defer doneBarrier.Done()
			startBarrier.Wait()

			propCopy := prop // In-memory copy
			_, err := executor.ExecuteProposal(context.Background(), &propCopy)
			if err == nil {
				successCount.Add(1)
			} else if errors.Is(err, ErrProposalAlreadyRunning) || errors.Is(err, ErrProposalNotExecutable) {
				blockedCount.Add(1)
			} else {
				otherErrors.Add(1)
			}
		}()
	}

	startBarrier.Done()
	doneBarrier.Wait()

	if successCount.Load() != 1 {
		t.Fatalf("VIOLATION: Expected exactly 1 successful execution of proposal, got %d", successCount.Load())
	}
	if blockedCount.Load() != concurrency-1 {
		t.Fatalf("VIOLATION: Expected %d attempts blocked by CAS lock, got %d (other errs: %d)",
			concurrency-1, blockedCount.Load(), otherErrors.Load())
	}
	if executionCount.Load() != 1 {
		t.Fatalf("VIOLATION: Docker runner was invoked %d times instead of exactly 1!", executionCount.Load())
	}
}

// TestChallenger4_ProposalSecurity_AntiTOCTOU_PreExecStateDrift verifies that
// if the target component's state drifts between approval and execution,
// execution is fail-closed blocked with ErrBaselineMismatch.
func TestChallenger4_ProposalSecurity_AntiTOCTOU_PreExecStateDrift(t *testing.T) {
	db := setupChallenger1TestDB(t)

	var currentLiveFingerprint atomic.Value
	currentLiveFingerprint.Store("f5a7e8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7")

	executor := NewOperationExecutor(
		db,
		nil,
		nil,
		nil,
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			return "success", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
		WithBaselineExtractor(func(target string) string {
			return currentLiveFingerprint.Load().(string)
		}),
	)

	prop := setupChallengerProposal(t, db)

	// Simulate drift: container image or state changed before execution
	currentLiveFingerprint.Store("drifted_different_hash_9999999999999999999999999999999999999999")

	_, err := executor.ExecuteProposal(context.Background(), &prop)
	if !errors.Is(err, ErrBaselineMismatch) {
		t.Fatalf("VIOLATION: Expected ErrBaselineMismatch when live state drifted before execution, got: %v", err)
	}
}
