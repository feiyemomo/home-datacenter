package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

func setupR4CBDB(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "r4_cb_fault.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite db: %v", err)
	}
	if err := EnsureOpsSchema(db); err != nil {
		t.Fatalf("EnsureOpsSchema failed: %v", err)
	}
	_ = db.AutoMigrate(&model.SystemLog{})
	if sqlDB, sErr := db.DB(); sErr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	return db, dbPath
}

// TestR4_21_CircuitBreakerFaultMatrix exercises all 10 Fail-Closed & concurrency scenarios (R4-09, R4-10, R4-21).
func TestR4_21_CircuitBreakerFaultMatrix(t *testing.T) {
	t.Run("01_DatabaseReadOnly_TripsToDegradedFailClosed", func(t *testing.T) {
		db, _ := setupR4CBDB(t)
		fsm := NewContainerRecoveryFSM(db, eventbus.New(), nil)

		if err := db.Exec("PRAGMA query_only = ON").Error; err != nil {
			t.Fatalf("failed to enable query_only: %v", err)
		}
		defer func() { _ = db.Exec("PRAGMA query_only = OFF").Error }()

		_, _ = fsm.GetCircuitBreaker().RecordCrash("home-api")
		if !fsm.GetCircuitBreaker().IsDegraded() {
			t.Fatalf("expected circuit breaker to enter Fail-Closed degraded mode on read-only DB write failure")
		}
		can, _, canErr := fsm.GetCircuitBreaker().CanAttemptRecovery("home-api")
		if can || !errors.Is(canErr, ErrFailClosedMode) {
			t.Fatalf("expected subsequent recovery attempts blocked with ErrFailClosedMode, got can=%v err=%v", can, canErr)
		}
	})

	t.Run("02_TriggerPersistenceAbort_MaintenanceLock", func(t *testing.T) {
		db, _ := setupR4CBDB(t)
		lock := NewMaintenanceLock()
		lock.SetDB(db)

		// 1. Acquire on healthy DB then drop table before Release -> Release MUST fail closed and enter degraded
		if err := lock.Acquire("admin", "upgrade", 5*time.Minute); err != nil {
			t.Fatalf("initial Acquire failed: %v", err)
		}
		if err := db.Exec("DROP TABLE ops_maintenance_locks").Error; err != nil {
			t.Fatalf("drop failed: %v", err)
		}
		if relErr := lock.Release("admin"); relErr == nil {
			t.Fatalf("expected Release failure when table dropped")
		}
		if !lock.IsDegraded() || !lock.IsLocked() {
			t.Fatalf("expected failed Release to retain protection (IsDegraded=true and IsLocked=true)")
		}

		// 2. Acquire into broken table from clean state MUST return error and roll back memory cleanly
		lock2 := NewMaintenanceLock()
		lock2.SetDB(db)
		if acqErr := lock2.Acquire("admin", "upgrade", 5*time.Minute); acqErr == nil {
			t.Fatalf("expected Acquire error when persistence fails")
		}
		if lock2.IsLocked() {
			t.Fatalf("expected failed Acquire not to claim lock active in memory")
		}
	})

	t.Run("03_CorruptSQLiteFile_FailsClosed", func(t *testing.T) {
		corruptPath := filepath.Join(t.TempDir(), "corrupt.db")
		if err := os.WriteFile(corruptPath, []byte("NOT A VALID SQLITE DATABASE HEADER!!!"), 0600); err != nil {
			t.Fatalf("write failed: %v", err)
		}
		db, err := gorm.Open(sqlite.Open(corruptPath), &gorm.Config{})
		if err == nil {
			cb := NewContainerCircuitBreaker()
			cb.SetDB(db)
			cb.LoadPersistedState(db)
			if !cb.IsDegraded() {
				t.Fatalf("expected corrupt DB LoadPersistedState to mark degraded=true")
			}
			if sqlDB, _ := db.DB(); sqlDB != nil {
				_ = sqlDB.Close()
			}
		}
	})

	t.Run("04_DroppedTable_ReconcileStaysDegraded", func(t *testing.T) {
		db, _ := setupR4CBDB(t)
		fsm := NewContainerRecoveryFSM(db, eventbus.New(), nil)

		// First record a crash cleanly while healthy so state has entry
		_, _ = fsm.GetCircuitBreaker().RecordCrash("home-web")

		if err := db.Exec("DROP TABLE ops_circuit_breaker_states").Error; err != nil {
			t.Fatalf("drop failed: %v", err)
		}
		_, _ = fsm.GetCircuitBreaker().RecordCrash("home-web")
		if !fsm.GetCircuitBreaker().IsDegraded() {
			t.Fatalf("expected IsDegraded=true after dropped table persistence failure")
		}

		// Reset / ResetAll MUST fail closed while degraded (degraded flag remains, CanAttemptRecovery blocked)
		fsm.GetCircuitBreaker().Reset("home-web")
		fsm.GetCircuitBreaker().ResetAll()
		if !fsm.GetCircuitBreaker().IsDegraded() {
			t.Fatalf("expected Reset/ResetAll not to clear degraded protection")
		}
		if can, _, err := fsm.GetCircuitBreaker().CanAttemptRecovery("home-web"); can || !errors.Is(err, ErrFailClosedMode) {
			t.Fatalf("expected ErrFailClosedMode after Reset attempt while degraded, got can=%v err=%v", can, err)
		}

		// ReconcileDegradedState MUST fail closed because bootstrapped DB lost critical table (R4-18)
		if recErr := fsm.ReconcileDegradedState("ops-admin"); recErr == nil || !fsm.GetCircuitBreaker().IsDegraded() {
			t.Fatalf("expected ReconcileDegradedState to stay degraded when table missing, got %v", recErr)
		}
	})

	t.Run("05_CorruptCrashTimestampsJSON_FailsClosed", func(t *testing.T) {
		db, _ := setupR4CBDB(t)
		err := db.Exec(
			"INSERT INTO ops_circuit_breaker_states (container, state, crash_timestamps) VALUES ('home-api', 'CLOSED', '{broken-json')",
		).Error
		if err != nil {
			t.Fatalf("insert failed: %v", err)
		}
		cb := NewContainerCircuitBreaker()
		cb.SetDB(db)
		cb.LoadPersistedState(db)
		if !cb.IsDegraded() {
			t.Fatalf("expected LoadPersistedState to fail closed (IsDegraded=true) on corrupt crash_timestamps JSON")
		}
	})

	t.Run("06_UnknownCircuitStateEnum_FailsClosed", func(t *testing.T) {
		db, _ := setupR4CBDB(t)
		err := db.Exec(
			"INSERT INTO ops_circuit_breaker_states (container, state, crash_timestamps) VALUES ('home-web', 'ALIEN_STATE', '[]')",
		).Error
		if err != nil {
			t.Fatalf("insert failed: %v", err)
		}
		cb := NewContainerCircuitBreaker()
		cb.SetDB(db)
		cb.LoadPersistedState(db)
		if !cb.IsDegraded() {
			t.Fatalf("expected LoadPersistedState to reject unknown state string and enter degraded mode")
		}
	})

	t.Run("07_ExpiredHalfOpenProbeTicket_ReclaimableWithoutDeadlock", func(t *testing.T) {
		db, _ := setupR4CBDB(t)
		cb := NewContainerCircuitBreaker()
		cb.SetDB(db)
		cb.SetCooldownDuration(60 * time.Millisecond)
		cb.SetProbeTTL(200 * time.Millisecond)

		for i := 0; i < 3; i++ {
			_, tripped := cb.RecordCrash("home-api")
			if i == 2 && !tripped {
				t.Fatalf("expected trip on crash #3")
			}
		}

		// Wait cooldown -> acquire first HALF_OPEN probe ticket
		time.Sleep(75 * time.Millisecond)
		ok1, ticket1, err1 := cb.CanAttemptRecovery("home-api")
		if !ok1 || err1 != nil || ticket1 == nil || ticket1.Token == "" {
			t.Fatalf("expected first probe ticket, got ok=%v ticket=%v err=%v", ok1, ticket1, err1)
		}

		// Concurrent second caller immediately blocked with ErrProbeInProgress
		ok2, _, err2 := cb.CanAttemptRecovery("home-api")
		if ok2 || !errors.Is(err2, ErrProbeInProgress) {
			t.Fatalf("expected ErrProbeInProgress for concurrent caller, got ok=%v err=%v", ok2, err2)
		}

		// Wait probeTTL so ticket1 expires -> stale callback MUST fail and reopen with cooldown
		time.Sleep(250 * time.Millisecond)
		if staleErr := cb.RecordRecoverySuccess("home-api", ticket1.Token); !errors.Is(staleErr, ErrProbeExpired) && !errors.Is(staleErr, ErrStaleProbeToken) {
			t.Fatalf("expected expired probe callback rejected, got %v", staleErr)
		}

		// During cooldown, new probe is still gated by ErrCircuitBreakerOpen; once cooldown passes, new probe slot opens without deadlock
		time.Sleep(75 * time.Millisecond)
		ok3, ticket3, err3 := cb.CanAttemptRecovery("home-api")
		if !ok3 || err3 != nil || ticket3 == nil || ticket3.Token == "" || ticket3.Token == ticket1.Token {
			t.Fatalf("expected new probe slot after expiry without deadlock, got ok=%v ticket=%v err=%v", ok3, ticket3, err3)
		}
		if recErr := cb.RecordRecoverySuccess("home-api", ticket3.Token); recErr != nil {
			t.Fatalf("expected fresh probe success to close circuit, got %v", recErr)
		}
		if cb.GetState("home-api") != CircuitClosed {
			t.Fatalf("expected CircuitClosed after fresh probe success, got %s", cb.GetState("home-api"))
		}
	})

	t.Run("08_LateStaleProbeCallback_Rejected", func(t *testing.T) {
		db, _ := setupR4CBDB(t)
		cb := NewContainerCircuitBreaker()
		cb.SetDB(db)
		cb.SetCooldownDuration(15 * time.Millisecond)
		cb.SetProbeTTL(200 * time.Millisecond)

		for i := 0; i < 3; i++ {
			_, _ = cb.RecordCrash("frigate")
		}
		time.Sleep(25 * time.Millisecond)
		ok, oldTicket, _ := cb.CanAttemptRecovery("frigate")
		if !ok || oldTicket == nil {
			t.Fatalf("expected half-open ticket")
		}
		// Fail the probe -> transitions back to OPEN and clears active ticket
		if err := cb.RecordRecoveryFailure("frigate", oldTicket.Token, "simulated health fail"); err != nil {
			t.Fatalf("RecordRecoveryFailure failed: %v", err)
		}
		// Late stale success callback arrives with oldTicket.Token -> MUST fail
		if err := cb.RecordRecoverySuccess("frigate", oldTicket.Token); !errors.Is(err, ErrStaleProbeToken) && !errors.Is(err, ErrCircuitBreakerOpen) {
			t.Fatalf("expected stale/open error for late callback after probe failure, got %v", err)
		}
		if cb.GetState("frigate") != CircuitOpen {
			t.Fatalf("expected circuit to remain OPEN after rejected late callback, got %s", cb.GetState("frigate"))
		}
	})

	t.Run("09_50GoroutineConcurrentEventStorm", func(t *testing.T) {
		db, _ := setupR4CBDB(t)
		fsm := NewContainerRecoveryFSM(db, eventbus.New(), nil)
		var wg sync.WaitGroup
		var restarted, blocked atomic.Int64

		mockExec := &stubStormExecutor{}
		fsm.SetExecutor(mockExec)

		barrier := make(chan struct{})
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-barrier
				res, _ := fsm.ProcessAndRecover(context.Background(), ContainerEvent{
					ContainerName: "home-api",
					Action:        "die",
					ExitCode:      1,
					Timestamp:     time.Now().UTC(),
				})
				if res != nil && res.Status == StatusRecovered {
					restarted.Add(1)
				} else {
					blocked.Add(1)
				}
			}()
		}
		close(barrier)
		wg.Wait()

		// Threshold is 3 crashes in 10m -> at most 2 restarts can succeed before tripping OPEN
		if restarted.Load() > 2 {
			t.Fatalf("CRITICAL: 50-goroutine storm exceeded breaker threshold: restarted=%d", restarted.Load())
		}
		if fsm.GetCircuitBreaker().GetState("home-api") != CircuitOpen {
			t.Fatalf("expected circuit breaker OPEN after 50-goroutine crash storm, got %s", fsm.GetCircuitBreaker().GetState("home-api"))
		}
		_ = blocked.Load()
	})

	t.Run("10_RestartAndReconcileDegradedState_RestoresOpenLock", func(t *testing.T) {
		db, _ := setupR4CBDB(t)
		fsm1 := NewContainerRecoveryFSM(db, nil, nil)

		if err := fsm1.GetMaintenanceLock().Acquire("ops-admin", "disk maintenance", 10*time.Minute); err != nil {
			t.Fatalf("lock acquire failed: %v", err)
		}
		for i := 0; i < 3; i++ {
			_, _ = fsm1.GetCircuitBreaker().RecordCrash("home-web")
		}

		// Simulate process restart with new FSM loading from the same persistent DB
		fsm2 := NewContainerRecoveryFSM(db, nil, nil)
		if !fsm2.GetMaintenanceLock().IsLocked() {
			t.Fatalf("expected lock to persist across restart")
		}
		if fsm2.GetCircuitBreaker().GetState("home-web") != CircuitOpen {
			t.Fatalf("expected OPEN circuit state to persist across restart, got %s", fsm2.GetCircuitBreaker().GetState("home-web"))
		}

		// Mark degraded then call ReconcileDegradedState against healthy storage
		fsm2.GetCircuitBreaker().SetDegraded(true)

		if err := fsm2.ReconcileDegradedState("ops-admin"); err != nil {
			t.Fatalf("ReconcileDegradedState failed on healthy storage: %v", err)
		}
		if fsm2.GetCircuitBreaker().IsDegraded() {
			t.Fatalf("expected ReconcileDegradedState to clear degraded flag on healthy storage")
		}
		// While degraded is cleared, persistent OPEN state & MaintenanceLock MUST remain intact!
		if fsm2.GetCircuitBreaker().GetState("home-web") != CircuitOpen {
			t.Fatalf("expected circuit state to remain OPEN after reconcile, got %s", fsm2.GetCircuitBreaker().GetState("home-web"))
		}
		if !fsm2.GetMaintenanceLock().IsLocked() {
			t.Fatalf("expected maintenance lock to remain LOCKED after reconcile")
		}
	})
}

type stubStormExecutor struct {
	count atomic.Int64
}

func (s *stubStormExecutor) Execute(ctx context.Context, spec OperationSpec) (*ExecutionResult, error) {
	s.count.Add(1)
	return &ExecutionResult{Success: true, Action: spec.Action, Target: spec.Target, ExecutedAt: time.Now()}, nil
}

func (s *stubStormExecutor) ExecuteRollback(ctx context.Context, rollbackSpec OperationSpec) error {
	return nil
}

func (s *stubStormExecutor) ExecuteProposal(ctx context.Context, proposal *model.OpsProposal) (*ExecutionResult, error) {
	return nil, nil
}

func (s *stubStormExecutor) CheckHealth(ctx context.Context, target string) error {
	return nil
}

func (s *stubStormExecutor) ReconcileProposal(ctx context.Context, proposal *model.OpsProposal, probeErr error) error {
	return nil
}

func (s *stubStormExecutor) ValidateComponent(component string) (string, error) {
	return ResolveComponent(component)
}
