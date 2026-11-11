package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

func setupR1R3R5TestDB(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "r1_r3_r5_core.db")
	dsn := dbPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open sqlite db: %v", err)
	}
	if err := EnsureOpsSchema(db); err != nil {
		t.Fatalf("EnsureOpsSchema failed: %v", err)
	}
	_ = db.AutoMigrate(&model.SystemLog{})
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(8)
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	return db, dbPath
}

// ============================================================================
// R1: Lease Same-Holder Contention, Unified Lifecycle, Timeout & Multi-Instance
// ============================================================================

func TestR1_LeaseSameHolderInvalidStateAndUnifiedLifecycle(t *testing.T) {
	db, _ := setupR1R3R5TestDB(t)
	mgr := NewRecoveryLeaseManager(db)

	t.Run("SameHolderCannotOverwriteActiveLeaseAndOldTokenCannotReleaseNewLease", func(t *testing.T) {
		g1, err := mgr.AcquireLeaseGrant("home-web", "go-core-same-holder", 80*time.Millisecond)
		if err != nil {
			t.Fatalf("initial AcquireLeaseGrant failed: %v", err)
		}

		// Same holder, different execution instance -> MUST reject with ErrLeaseHeld
		if _, err := mgr.AcquireLeaseGrant("home-web", "go-core-same-holder", 5*time.Second); !errors.Is(err, ErrLeaseHeld) {
			t.Fatalf("expected ErrLeaseHeld for same-holder overwrite attempt, got: %v", err)
		}

		// Wait for lease 1 to expire (simulated crash of executor 1)
		time.Sleep(120 * time.Millisecond)

		// Executor 2 safely takes over expired lease with monotonic fencing generation
		g2, err := mgr.AcquireLeaseGrant("home-web", "go-core-same-holder", 5*time.Second)
		if err != nil {
			t.Fatalf("expected safe takeover after expiry, got: %v", err)
		}
		if g2.LeaseToken == g1.LeaseToken {
			t.Fatalf("expected fresh cryptographic token per execution")
		}
		if g2.FencingGeneration <= g1.FencingGeneration {
			t.Fatalf("expected monotonic fencing generation (%d > %d)", g2.FencingGeneration, g1.FencingGeneration)
		}

		// Late release/renew with old token g1 MUST fail and NOT disturb g2
		if err := mgr.ReleaseLease("home-web", g1.LeaseToken); !errors.Is(err, ErrInvalidLeaseToken) {
			t.Fatalf("expected ErrInvalidLeaseToken on stale release, got: %v", err)
		}
		if _, err := mgr.RenewLease("home-web", g1.LeaseToken, 5*time.Second); !errors.Is(err, ErrInvalidLeaseToken) {
			t.Fatalf("expected ErrInvalidLeaseToken on stale renew, got: %v", err)
		}
		if err := mgr.VerifyLeaseFencing("home-web", g2.LeaseToken, g2.FencingGeneration, 0); err != nil {
			t.Fatalf("expected new lease g2 to remain intact, got: %v", err)
		}
		if err := mgr.ReleaseLease("home-web", g2.LeaseToken); err != nil {
			t.Fatalf("failed to release g2: %v", err)
		}
	})

	t.Run("CorruptLeaseStateFailsClosedAndDBErrorNotMaskedAsLeaseHeld", func(t *testing.T) {
		if err := db.Exec(
			`INSERT INTO ops_recovery_leases (component, holder, lease_token, fencing_generation, acquired_at_ms, expires_at_ms, state)
			 VALUES ('frigate', 'rogue', 'tok-bogus', 1, 1000, 9999999999999, 'CORRUPT_STATE')`,
		).Error; err != nil {
			t.Fatalf("seed corrupt lease failed: %v", err)
		}
		if _, err := mgr.AcquireLeaseGrant("frigate", "go-core", 5*time.Second); !errors.Is(err, ErrRecordInconsistent) {
			t.Fatalf("expected ErrRecordInconsistent on corrupt lease state, got: %v", err)
		}
		_ = db.Exec("DELETE FROM ops_recovery_leases WHERE component = 'frigate'").Error

		// Read-only DB error on INSERT must NOT masquerade as ErrLeaseHeld
		if err := db.Exec("PRAGMA query_only = ON").Error; err != nil {
			t.Fatalf("query_only failed: %v", err)
		}
		_, roErr := mgr.AcquireLeaseGrant("frigate", "go-core", 5*time.Second)
		_ = db.Exec("PRAGMA query_only = OFF").Error
		if roErr == nil || errors.Is(roErr, ErrLeaseHeld) {
			t.Fatalf("expected real storage error on read-only DB INSERT, got: %v", roErr)
		}
	})

	t.Run("UnifiedFSMAndExecutorLeaseCoversExecutionHealthAndRollback", func(t *testing.T) {
		var observedExecToken string
		var observedHealthToken string

		exec := NewOperationExecutor(
			db, nil, nil, nil,
			WithProbeTimeout(200*time.Millisecond),
			WithProbeInterval(10*time.Millisecond),
			WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
				g := RecoveryLeaseFromContext(ctx)
				if g == nil {
					return "", errors.New("missing LeaseGuard in dockerRunner ctx")
				}
				row, err := mgr.GetLease("home-web")
				if err != nil || row == nil {
					return "", fmt.Errorf("expected active lease in DB during docker dispatch, got row=%v err=%v", row, err)
				}
				if row.LeaseToken != g.Grant.LeaseToken {
					return "", fmt.Errorf("token overwritten! ctx=%s db=%s", g.Grant.LeaseToken, row.LeaseToken)
				}
				observedExecToken = g.Grant.LeaseToken
				return "restarted", nil
			}),
			WithHealthChecker(func(ctx context.Context, target string) error {
				if ResolveTopologyComponent(target) == "home-web" {
					row, err := mgr.GetLease("home-web")
					if err != nil || row == nil {
						return fmt.Errorf("expected lease still held during health verification, got row=%v err=%v", row, err)
					}
					observedHealthToken = row.LeaseToken
				}
				return nil
			}),
		)

		fsm := NewContainerRecoveryFSM(db, nil, exec)
		res, err := fsm.ProcessAndRecover(context.Background(), ContainerEvent{
			ContainerName: "home-web",
			Action:        "die",
			ExitCode:      1,
			Timestamp:     time.Now().UTC(),
		})
		if err != nil || res == nil || res.Status != StatusRecovered {
			t.Fatalf("expected StatusRecovered, got res=%+v err=%v", res, err)
		}
		if observedExecToken == "" || observedExecToken != observedHealthToken {
			t.Fatalf("expected unified single lease token across exec & health check, got exec=%q health=%q", observedExecToken, observedHealthToken)
		}
		active, _, _ := mgr.IsLeaseActive("home-web")
		if active {
			t.Fatalf("expected lease to be released after recovery & RecordRecoverySuccess completed")
		}
	})

	t.Run("LeaseHeldDuringRollbackAndReleasedOnTimeout", func(t *testing.T) {
		var observedRollbackToken string
		exec := NewOperationExecutor(
			db, nil, nil, nil,
			WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
				g := RecoveryLeaseFromContext(ctx)
				if g == nil {
					return "", errors.New("missing LeaseGuard")
				}
				if action == ActionDockerRecreate {
					return "", errors.New("definite container recreate failure")
				}
				// Rollback path
				observedRollbackToken = g.Grant.LeaseToken
				row, err := mgr.GetLease("home-api")
				if err != nil || row == nil || row.LeaseToken != g.Grant.LeaseToken {
					return "", errors.New("lease not held during rollback")
				}
				return "rolled back ok", nil
			}),
			WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
		)

		spec := OperationSpec{
			Action: ActionDockerRecreate,
			Target: "home-api",
			RollbackSpec: &OperationSpec{
				Action: ActionDockerRestart,
				Target: "home-api",
			},
		}
		res, err := exec.Execute(context.Background(), spec)
		if err == nil || res == nil || !res.RolledBack {
			t.Fatalf("expected failure with RolledBack=true, got res=%+v err=%v", res, err)
		}
		if observedRollbackToken == "" {
			t.Fatalf("expected LeaseGuard token to cover rollback execution")
		}
		if active, _, _ := mgr.IsLeaseActive("home-api"); active {
			t.Fatalf("expected lease released after rollback")
		}

		// Execution timeout releases lease cleanly
		execTimeout := NewOperationExecutor(
			db, nil, nil, nil,
			WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			}),
			WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
		)
		tCtx, tCancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer tCancel()
		_, tErr := execTimeout.Execute(tCtx, OperationSpec{Action: ActionDockerRestart, Target: "home-api"})
		if !errors.Is(tErr, ErrExecutionAmbiguous) {
			t.Fatalf("expected ErrExecutionAmbiguous on timeout, got: %v", tErr)
		}
		if active, _, _ := mgr.IsLeaseActive("home-api"); active {
			t.Fatalf("expected lease released after execution timeout")
		}
	})
}

func TestR1_TwoGoCoreInstancesMutualExclusion(t *testing.T) {
	db, dbPath := setupR1R3R5TestDB(t)
	db2, err := gorm.Open(sqlite.Open(dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open second db handle failed: %v", err)
	}
	if sqlDB2, err := db2.DB(); err == nil {
		t.Cleanup(func() { _ = sqlDB2.Close() })
	}

	var inFlight atomic.Int32
	var maxConcurrent atomic.Int32
	var totalExecs atomic.Int32

	makeFSM := func(d *gorm.DB) *ContainerRecoveryFSM {
		exec := NewOperationExecutor(
			d, nil, nil, nil,
			WithProbeTimeout(100*time.Millisecond),
			WithProbeInterval(10*time.Millisecond),
			WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
				cur := inFlight.Add(1)
				defer inFlight.Add(-1)
				for {
					prev := maxConcurrent.Load()
					if cur <= prev || maxConcurrent.CompareAndSwap(prev, cur) {
						break
					}
				}
				totalExecs.Add(1)
				time.Sleep(150 * time.Millisecond)
				return "ok", nil
			}),
			WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
		)
		return NewContainerRecoveryFSM(d, nil, exec)
	}

	fsm1 := makeFSM(db)
	fsm2 := makeFSM(db2)

	barrier := make(chan struct{})
	var wg sync.WaitGroup
	var recoveredCount, blockedCount atomic.Int32

	for _, f := range []*ContainerRecoveryFSM{fsm1, fsm2} {
		wg.Add(1)
		go func(instance *ContainerRecoveryFSM) {
			defer wg.Done()
			<-barrier
			res, _ := instance.ProcessAndRecover(context.Background(), ContainerEvent{
				ContainerName: "home-vision",
				Action:        "die",
				ExitCode:      1,
				Timestamp:     time.Now().UTC(),
			})
			if res != nil && res.Status == StatusRecovered {
				recoveredCount.Add(1)
			} else if res != nil && (res.Status == StatusBlocked || res.Status == StatusFailed) {
				blockedCount.Add(1)
			}
		}(f)
	}
	close(barrier)
	wg.Wait()

	if maxConcurrent.Load() > 1 {
		t.Fatalf("MUTUAL EXCLUSION VIOLATION: 2 Go Core instances executed concurrently (%d)", maxConcurrent.Load())
	}
	if recoveredCount.Load() != 1 || blockedCount.Load() != 1 || totalExecs.Load() != 1 {
		t.Fatalf("expected exactly 1 recovered and 1 blocked across 2 Go Core instances; got recovered=%d blocked=%d execs=%d",
			recoveredCount.Load(), blockedCount.Load(), totalExecs.Load())
	}
}

// ============================================================================
// R3: Circuit Breaker Split-Brain, Persistence Fault Matrix & Restart Recovery
// ============================================================================

func TestR3_CircuitBreakerFaultMatrixAndSplitBrain(t *testing.T) {
	t.Run("RecordRecoverySuccessPersistenceFailureBlocksFullSuccess", func(t *testing.T) {
		db, _ := setupR1R3R5TestDB(t)
		exec := NewOperationExecutor(
			db, nil, nil, nil,
			WithProbeTimeout(100*time.Millisecond),
			WithProbeInterval(10*time.Millisecond),
			WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
				return "restarted", nil
			}),
			WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
		)
		fsm := NewContainerRecoveryFSM(db, eventbus.New(), exec)
		cb := fsm.GetCircuitBreaker()
		cb.SetCooldownDuration(10 * time.Millisecond)

		for i := 0; i < 3; i++ {
			cb.RecordCrash("home-web")
		}
		time.Sleep(25 * time.Millisecond)

		// Fail UPDATE right before HALF_OPEN->CLOSED commit (allow OPEN->HALF_OPEN first by hooking after CanAttemptRecovery)
		can, ticket, err := cb.CanAttemptRecovery("home-web")
		if !can || ticket == nil || err != nil {
			t.Fatalf("expected HALF_OPEN ticket, got can=%v err=%v", can, err)
		}

		if err := db.Exec(`CREATE TRIGGER fail_cb_upd_success BEFORE UPDATE ON ops_circuit_breaker_states
			WHEN NEW.state = 'CLOSED'
			BEGIN SELECT RAISE(ABORT, 'simulated tx commit fail on CLOSED'); END;`).Error; err != nil {
			t.Fatalf("create trigger failed: %v", err)
		}

		recErr := cb.RecordRecoverySuccess("home-web", ticket.Token)
		if !errors.Is(recErr, ErrFailClosedMode) {
			t.Fatalf("expected ErrFailClosedMode when RecordRecoverySuccess fails persistence, got: %v", recErr)
		}
		if !cb.IsDegraded() {
			t.Fatalf("expected circuit breaker to enter degraded mode on RecordRecoverySuccess persistence failure")
		}
		if cb.GetState("home-web") != CircuitOpen {
			t.Fatalf("expected state rolled back to CircuitOpen, got %s", cb.GetState("home-web"))
		}
	})

	t.Run("FSMProcessAndRecoverFailsClosedIfRecordRecoverySuccessFails", func(t *testing.T) {
		db, _ := setupR1R3R5TestDB(t)
		exec := NewOperationExecutor(
			db, nil, nil, nil,
			WithProbeTimeout(100*time.Millisecond),
			WithProbeInterval(10*time.Millisecond),
			WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
				return "restarted", nil
			}),
			WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
		)
		fsm := NewContainerRecoveryFSM(db, eventbus.New(), exec)
		cb := fsm.GetCircuitBreaker()
		cb.SetCooldownDuration(10 * time.Millisecond)

		for i := 0; i < 3; i++ {
			cb.RecordCrash("home-web")
		}
		time.Sleep(25 * time.Millisecond)

		// Allow OPEN->HALF_OPEN update, abort ONLY the HALF_OPEN->CLOSED update!
		if err := db.Exec(`CREATE TRIGGER fail_cb_close_only BEFORE UPDATE ON ops_circuit_breaker_states
			WHEN NEW.state = 'CLOSED'
			BEGIN SELECT RAISE(ABORT, 'simulated commit failure on recovery close'); END;`).Error; err != nil {
			t.Fatalf("create trigger failed: %v", err)
		}

		res, err := fsm.ProcessAndRecover(context.Background(), ContainerEvent{
			ContainerName: "home-web",
			Action:        "die",
			ExitCode:      1,
			Timestamp:     time.Now().UTC(),
		})
		if !errors.Is(err, ErrFailClosedMode) {
			t.Fatalf("expected ErrFailClosedMode from ProcessAndRecover when CB commit fails, got err=%v", err)
		}
		if res == nil || res.Status != StatusFailed {
			t.Fatalf("expected StatusFailed (NOT StatusRecovered) when CB persistence failed, got: %+v", res)
		}
		if !cb.IsDegraded() {
			t.Fatalf("expected breaker degraded after persistence failure")
		}
	})

	t.Run("MemoryClosedVsDBOpenSplitBrainPrevented", func(t *testing.T) {
		db, _ := setupR1R3R5TestDB(t)

		// Instance A trips home-api to OPEN in SQLite
		cbA := NewContainerCircuitBreaker()
		cbA.SetDB(db)
		for i := 0; i < 3; i++ {
			cbA.RecordCrash("home-api")
		}
		if cbA.GetState("home-api") != CircuitOpen {
			t.Fatalf("expected cbA OPEN")
		}

		// Instance B has memory=CLOSED (SetDB without LoadPersistedState)
		cbB := NewContainerCircuitBreaker()
		cbB.SetDB(db)

		// 1. GetState MUST see DB=OPEN, not memory=CLOSED
		if st := cbB.GetState("home-api"); st != CircuitOpen {
			t.Fatalf("SPLIT-BRAIN: memory=CLOSED instance reported %s instead of CircuitOpen!", st)
		}
		// 2. CanAttemptRecovery MUST block with ErrCircuitBreakerOpen
		cbB2 := NewContainerCircuitBreaker()
		cbB2.SetDB(db)
		if ok, _, err := cbB2.CanAttemptRecovery("home-api"); ok || !errors.Is(err, ErrCircuitBreakerOpen) {
			t.Fatalf("SPLIT-BRAIN: expected ErrCircuitBreakerOpen, got ok=%v err=%v", ok, err)
		}
		// 3. RecordRecoverySuccess MUST NOT return nil on DB=OPEN
		cbB3 := NewContainerCircuitBreaker()
		cbB3.SetDB(db)
		if err := cbB3.RecordRecoverySuccess("home-api"); !errors.Is(err, ErrStaleProbeToken) {
			t.Fatalf("SPLIT-BRAIN: expected ErrStaleProbeToken on DB=OPEN, got: %v", err)
		}
		// 4. RecordCrash MUST NOT overwrite DB=OPEN back to CLOSED
		cbB4 := NewContainerCircuitBreaker()
		cbB4.SetDB(db)
		if st, _ := cbB4.RecordCrash("home-api"); st != CircuitOpen {
			t.Fatalf("SPLIT-BRAIN: RecordCrash returned %s instead of CircuitOpen", st)
		}
	})

	t.Run("HalfOpenPersistsResetToOpenAcrossProcessRestartsAndRecovers", func(t *testing.T) {
		db, _ := setupR1R3R5TestDB(t)
		cb1 := NewContainerCircuitBreaker()
		cb1.SetDB(db)
		cb1.SetCooldownDuration(20 * time.Millisecond)

		for i := 0; i < 3; i++ {
			cb1.RecordCrash("home-web")
		}
		time.Sleep(35 * time.Millisecond)

		ok, ticket, err := cb1.CanAttemptRecovery("home-web")
		if !ok || ticket == nil || err != nil {
			t.Fatalf("expected HALF_OPEN ticket, got ok=%v err=%v", ok, err)
		}

		// Simulate crash and process restart: new FSM + CircuitBreaker loading from DB
		fsm2 := NewContainerRecoveryFSM(db, nil, nil)
		cb2 := fsm2.GetCircuitBreaker()
		cb2.SetCooldownDuration(20 * time.Millisecond)

		// Both memory AND SQLite row MUST now be OPEN with active_probe_token cleared
		if cb2.GetState("home-web") != CircuitOpen {
			t.Fatalf("expected CircuitOpen in memory after restart, got %s", cb2.GetState("home-web"))
		}
		var dbRow model.OpsCircuitBreakerState
		if err := db.Where("container = ?", "home-web").First(&dbRow).Error; err != nil {
			t.Fatalf("query dbRow failed: %v", err)
		}
		if dbRow.State != string(CircuitOpen) || dbRow.ActiveProbeToken != "" {
			t.Fatalf("expected DB row reset to OPEN with cleared active_probe_token, got state=%s tok=%q", dbRow.State, dbRow.ActiveProbeToken)
		}

		// After cooldown, cb2 can acquire a fresh probe and close cleanly
		time.Sleep(35 * time.Millisecond)
		ok2, ticket2, err2 := cb2.CanAttemptRecovery("home-web")
		if !ok2 || ticket2 == nil || err2 != nil {
			t.Fatalf("expected fresh probe after restart cooldown, got ok=%v err=%v", ok2, err2)
		}
		if err := cb2.RecordRecoverySuccess("home-web", ticket2.Token); err != nil {
			t.Fatalf("RecordRecoverySuccess failed: %v", err)
		}
		if cb2.GetState("home-web") != CircuitClosed {
			t.Fatalf("expected CircuitClosed after successful restarted probe, got %s", cb2.GetState("home-web"))
		}
	})

	t.Run("SQLiteInsertFailEntersDegradedMode", func(t *testing.T) {
		db, _ := setupR1R3R5TestDB(t)
		if err := db.Exec(`CREATE TRIGGER fail_cb_ins BEFORE INSERT ON ops_circuit_breaker_states
			BEGIN SELECT RAISE(ABORT, 'injected insert failure'); END;`).Error; err != nil {
			t.Fatalf("trigger failed: %v", err)
		}
		cb := NewContainerCircuitBreaker()
		cb.SetDB(db)
		state, _ := cb.RecordCrash("home-web")
		if state != CircuitOpen || !cb.IsDegraded() {
			t.Fatalf("expected CircuitOpen + degraded on INSERT fail, got state=%s degraded=%v", state, cb.IsDegraded())
		}
	})
}

// ============================================================================
// R5: BaselineHash Validation, Real vs Placeholder & Strict Production Mode
// ============================================================================

func TestR5_BaselineHashStrictValidationAndPlaceholderDistinction(t *testing.T) {
	t.Run("UnknownComponentRejectedByExtractorAndExecutor", func(t *testing.T) {
		db, _ := setupR1R3R5TestDB(t)
		exec := NewOperationExecutor(db, nil, nil, nil)

		info := exec.ExtractBaseline("non-existent-rogue-container")
		if info.Hash != "" {
			t.Fatalf("expected empty BaselineHash for unknown component, got %q", info.Hash)
		}

		_, err := exec.Execute(context.Background(), OperationSpec{
			Action: ActionDockerRestart,
			Target: "non-existent-rogue-container",
		})
		if !errors.Is(err, ErrComponentNotWhitelisted) {
			t.Fatalf("expected ErrComponentNotWhitelisted, got: %v", err)
		}
	})

	t.Run("DistinguishRealFingerprintFromOfflinePlaceholderAndBlockInProduction", func(t *testing.T) {
		db, _ := setupR1R3R5TestDB(t)

		var mu sync.Mutex
		mode := "live"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			curMode := mode
			mu.Unlock()

			if curMode == "timeout" {
				time.Sleep(2500 * time.Millisecond)
				return
			}
			var items []onePanelContainerItem
			switch curMode {
			case "live":
				items = []onePanelContainerItem{
					{
						ContainerID: "cid-frigate-real",
						Name:        "/home-frigate",
						State:       "running",
						Status:      "Up 1 hour",
						Image:       "ghcr.io/blakeblackshear/frigate:0.14.1",
						Created:     "2026-11-10T01:00:00Z",
					},
				}
			case "unknown_state":
				items = []onePanelContainerItem{
					{
						ContainerID: "cid-frigate-unk",
						Name:        "/home-frigate",
						State:       "unknown",
						Image:       "ghcr.io/blakeblackshear/frigate:0.14.1",
						Created:     "2026-11-10T01:00:00Z",
					},
				}
			case "missing":
				items = []onePanelContainerItem{}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    200,
				"message": "OK",
				"data": map[string]interface{}{
					"items": items,
					"total": len(items),
				},
			})
		}))
		defer srv.Close()

		client := NewOnePanelClient(srv.URL, "btpanel", "u", "p")
		client.psession = "tok"
		client.lastLoginAt = time.Now()
		client.loginTTL = time.Hour

		// 1. Offline executor (no client, non-prod) produces identifiable placeholder
		offlineExec := NewOperationExecutor(db, nil, nil, nil)
		offlineInfo := offlineExec.ExtractBaseline("frigate")
		if offlineInfo.Hash == "" || !offlineInfo.IsPlaceholder || !IsOfflinePlaceholderBaseline("frigate", offlineInfo.Hash) {
			t.Fatalf("expected identifiable offline placeholder, got %+v", offlineInfo)
		}

		// 2. Live 1Panel executor resolves prefixed container "home-frigate" as real non-placeholder
		liveExec := NewOperationExecutor(
			db, nil, client, nil,
			WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
				return "ok", nil
			}),
			WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
		)
		liveInfo := liveExec.ExtractBaseline("frigate")
		if liveInfo.Hash == "" || liveInfo.IsPlaceholder || IsOfflinePlaceholderBaseline("frigate", liveInfo.Hash) || liveInfo.Source != "1panel" {
			t.Fatalf("expected real 1panel fingerprint for home-frigate, got %+v", liveInfo)
		}

		// 3. Unknown state MUST return empty hash
		mu.Lock()
		mode = "unknown_state"
		mu.Unlock()
		if unk := liveExec.ExtractBaseline("frigate"); unk.Hash != "" {
			t.Fatalf("expected empty hash for unknown container state, got %+v", unk)
		}

		// 4. Non-existent container MUST return empty hash
		mu.Lock()
		mode = "missing"
		mu.Unlock()
		if miss := liveExec.ExtractBaseline("frigate"); miss.Hash != "" {
			t.Fatalf("expected empty hash for missing container, got %+v", miss)
		}

		// 5. Production mode strictly rejects placeholder baselines at execution time
		t.Setenv("OPS_STRICT_BASELINE", "1")
		if prodOff := offlineExec.ExtractBaseline("frigate"); prodOff.Hash != "" {
			t.Fatalf("expected empty hash in production when container runtime unavailable, got %+v", prodOff)
		}
		_, prodErr := offlineExec.Execute(context.Background(), OperationSpec{
			Action:       ActionDockerRestart,
			Target:       "frigate",
			BaselineHash: offlineInfo.Hash,
		})
		if !errors.Is(prodErr, ErrBaselineMismatch) {
			t.Fatalf("expected ErrBaselineMismatch rejecting placeholder baseline in production, got: %v", prodErr)
		}

		// 6. Docker/1Panel API timeout MUST return empty hash (no placeholder fallback)
		mu.Lock()
		mode = "timeout"
		mu.Unlock()
		if to := liveExec.ExtractBaseline("frigate"); to.Hash != "" {
			t.Fatalf("expected empty hash on API timeout, got %+v", to)
		}
	})

	t.Run("ActionSpecCannotOverrideAuthoritativeProposalBaseline", func(t *testing.T) {
		db, _ := setupR1R3R5TestDB(t)
		exec := NewOperationExecutor(db, nil, nil, nil)

		// Empty proposal baseline + forged ActionSpec baseline -> rejected
		_, err := exec.parseProposalSpec(&model.OpsProposal{
			ProposalID:      "p-no-base",
			TargetComponent: "home-web",
			BaselineHash:    "",
			ActionSpec:      `{"action":"docker.restart","target":"home-web","baseline_hash":"sha256:forged"}`,
		})
		if !errors.Is(err, ErrBaselineMismatch) {
			t.Fatalf("expected ErrBaselineMismatch when ActionSpec sets baseline on empty proposal baseline, got: %v", err)
		}

		// Mismatched ActionSpec baseline -> rejected
		_, err2 := exec.parseProposalSpec(&model.OpsProposal{
			ProposalID:      "p-mismatch-base",
			TargetComponent: "home-web",
			BaselineHash:    "sha256:authoritative",
			ActionSpec:      `{"action":"docker.restart","target":"home-web","baseline_hash":"sha256:forged"}`,
		})
		if !errors.Is(err2, ErrBaselineMismatch) {
			t.Fatalf("expected ErrBaselineMismatch on ActionSpec override, got: %v", err2)
		}
	})

	t.Run("ProductionRejectsOfflinePlaceholderInExecuteProposal", func(t *testing.T) {
		db, _ := setupR1R3R5TestDB(t)
		t.Setenv("OPS_STRICT_BASELINE", "1")

		exec := NewOperationExecutor(
			db, nil, nil, nil,
			WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
				t.Fatalf("dockerRunner must NOT run with offline placeholder baseline in production")
				return "", nil
			}),
			WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
		)

		prop := model.OpsProposal{
			ProposalID:      "prop-prod-placeholder",
			Title:           "Prod Placeholder Rejection",
			TargetComponent: "home-web",
			RiskLevel:       model.RiskLevelLow,
			Status:          model.ProposalStatusApproved,
			BaselineHash:    model.ComputeDefaultBaseline("home-web"),
			ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
			Version:         1,
			Revision:        1,
			CreatedAt:       time.Now().UTC(),
		}
		seedTestSignedProposal(t, db, &prop)

		_, err := exec.ExecuteProposal(context.Background(), &prop)
		if !errors.Is(err, ErrBaselineMismatch) {
			t.Fatalf("expected ErrBaselineMismatch when ExecuteProposal runs with placeholder in production, got: %v", err)
		}
	})
}

func TestR2_ExecuteProposalRejectsMemoryVsDBMismatchOnAllFields(t *testing.T) {
	db, _ := setupR1R3R5TestDB(t)
	exec := NewOperationExecutor(
		db, nil, nil, nil,
		WithBaselineExtractor(func(target string) string { return "sha256:live-fp-1" }),
		WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
			return "ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
	)

	makeProp := func(id string) model.OpsProposal {
		p := model.OpsProposal{
			ProposalID:      id,
			Title:           "Memory Integrity",
			TargetComponent: "home-web",
			RiskLevel:       model.RiskLevelLow,
			ExecutionPlan:   "docker restart home-web",
			RollbackPlan:    "docker logs home-web",
			ExecutionPolicy: "standard",
			Status:          model.ProposalStatusApproved,
			BaselineHash:    "sha256:live-fp-1",
			ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
			Version:         1,
			Revision:        1,
			CreatedAt:       time.Now().UTC(),
		}
		seedTestSignedProposal(t, db, &p)
		return p
	}

	cases := []struct {
		name   string
		mutate func(p *model.OpsProposal)
	}{
		{"RiskLevel", func(p *model.OpsProposal) { p.RiskLevel = model.RiskLevelCritical }},
		{"RollbackPlan", func(p *model.OpsProposal) { p.RollbackPlan = "rm -rf /" }},
		{"ExecutionPlan", func(p *model.OpsProposal) { p.ExecutionPlan = "tampered plan" }},
		{"ExecutionPolicy", func(p *model.OpsProposal) { p.ExecutionPolicy = "bypass" }},
		{"BaselineHash", func(p *model.OpsProposal) { p.BaselineHash = "sha256:tampered" }},
		{"Version", func(p *model.OpsProposal) { p.Version = 99 }},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := makeProp(fmt.Sprintf("prop-mem-chk-%d", i))
			tc.mutate(&p)
			_, err := exec.ExecuteProposal(context.Background(), &p)
			if !errors.Is(err, ErrApprovalDigestMismatch) {
				t.Fatalf("expected ErrApprovalDigestMismatch when in-memory %s differs from DB, got: %v", tc.name, err)
			}
		})
	}
}

// Verify test also works when Real Docker socket is simulated via Unix socket (Linux/Windows if supported)
func TestR5_ExtractBaseline_RealDockerSocketErrorsAndMissingContainer(t *testing.T) {
	db, _ := setupR1R3R5TestDB(t)
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "missing_or_fake.sock")

	// Create a regular non-socket file so os.Stat succeeds but dialing unix socket fails (simulates unreachable/broken Docker socket)
	if err := os.WriteFile(sockPath, []byte("not a socket"), 0600); err != nil {
		t.Fatalf("write fake socket file failed: %v", err)
	}

	exec := NewOperationExecutor(db, nil, nil, nil, WithDockerSocket(sockPath))
	info := exec.ExtractBaseline("home-web")
	if info.Hash != "" {
		t.Fatalf("expected empty BaselineHash when configured Docker socket fails, got: %+v", info)
	}
}

func TestR5_RealDockerSocket_Integration(t *testing.T) {
	sockPath := os.Getenv("DOCKER_SOCKET_PATH")
	if sockPath == "" {
		sockPath = "/var/run/docker.sock"
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Skipf("real Docker Unix socket not available at %s: %v", sockPath, err)
		return
	}

	db, _ := setupR1R3R5TestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fb := &HostFallbackProvider{dockerSocket: sockPath}
	containers, err := fb.queryDockerSocket(ctx)
	if err != nil {
		t.Fatalf("real Docker socket query failed: %v", err)
	}
	t.Logf("real Docker Engine returned %d containers", len(containers))

	// With real Docker socket present, missing whitelisted container must fail closed (no offline placeholder)
	exec := NewOperationExecutor(db, nil, nil, fb, WithDockerSocket(sockPath))
	t.Setenv("OPS_STRICT_BASELINE", "1")
	info := exec.ExtractBaseline("home-web")
	foundLive := false
	for _, c := range containers {
		if c.Name == "home-web" {
			foundLive = true
			break
		}
	}
	if !foundLive && info.Hash != "" {
		t.Fatalf("expected empty hash for absent home-web on real Docker socket, got %+v", info)
	}
	if foundLive && (info.Hash == "" || info.IsPlaceholder || info.Source != "docker_socket") {
		t.Fatalf("expected real docker_socket fingerprint for live home-web, got %+v", info)
	}
}
