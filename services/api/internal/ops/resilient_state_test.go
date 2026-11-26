package ops

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestResilientState_ResourceMutexConcurrency(t *testing.T) {
	tempDir := t.TempDir()
	snapPath := filepath.Join(tempDir, "test_snapshot.json")
	logPath := filepath.Join(tempDir, "test_ops.log")

	mgr, err := NewResilientStateManager(ResilientStateConfig{
		SnapshotPath: snapPath,
		LogPath:      logPath,
	})
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	serviceA := "home-vision"
	serviceB := "frigate"

	// 1. Two concurrent locks on the same service must serialize
	var order int32
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		unlock, err := mgr.AcquireResource(serviceA)
		if err != nil {
			t.Errorf("acquire A error: %v", err)
			return
		}
		defer unlock()
		atomic.StoreInt32(&order, 1)
		time.Sleep(50 * time.Millisecond)
	}()

	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Millisecond)
		unlock, err := mgr.AcquireResource(serviceA)
		if err != nil {
			t.Errorf("acquire A second error: %v", err)
			return
		}
		defer unlock()
		if atomic.LoadInt32(&order) != 1 {
			t.Errorf("expected serialized execution, got order != 1")
		}
	}()

	// 2. Lock on serviceB should proceed without waiting for serviceA
	bUnlocked := make(chan struct{})
	go func() {
		unlockB, err := mgr.AcquireResource(serviceB)
		if err != nil {
			t.Errorf("acquire B error: %v", err)
			return
		}
		defer unlockB()
		close(bUnlocked)
	}()

	select {
	case <-bUnlocked:
		// Succeeded immediately without waiting for serviceA
	case <-time.After(30 * time.Millisecond):
		t.Errorf("resource B was blocked by resource A mutex")
	}

	wg.Wait()
}

func TestResilientState_RateLimitAndLatching(t *testing.T) {
	tempDir := t.TempDir()
	snapPath := filepath.Join(tempDir, "test_snapshot.json")
	logPath := filepath.Join(tempDir, "test_ops.log")

	mgr, err := NewResilientStateManager(ResilientStateConfig{
		SnapshotPath:         snapPath,
		LogPath:              logPath,
		WindowDuration:       100 * time.Millisecond,
		MaxAttemptsPerWindow: 3,
	})
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}

	svc := "home-vision"

	// 3 attempts succeed check
	for i := 1; i <= 3; i++ {
		can, err := mgr.CanExecute(svc)
		if !can || err != nil {
			t.Fatalf("attempt %d should be allowed, got can=%v, err=%v", i, can, err)
		}
		if err := mgr.RecordAttempt(svc, "docker.restart"); err != nil {
			t.Fatalf("record attempt %d: %v", i, err)
		}
		if err := mgr.RecordFailure(svc, "docker.restart", errors.New("simulated error")); err != nil {
			t.Fatalf("record failure %d: %v", i, err)
		}
	}

	// 4th attempt must be rejected because latched
	can, err := mgr.CanExecute(svc)
	if can || !errors.Is(err, ErrResourceLatched) {
		t.Fatalf("expected ErrResourceLatched, got can=%v, err=%v", can, err)
	}

	// Verify state is latched
	st, exists := mgr.GetState(svc)
	if !exists || !st.Latched {
		t.Fatalf("expected state to exist and be latched, got: %+v", st)
	}

	// Manual unlatch
	if err := mgr.Unlatch(svc, "admin"); err != nil {
		t.Fatalf("unlatch failed: %v", err)
	}

	// Now CanExecute should be true
	canAfter, errAfter := mgr.CanExecute(svc)
	if !canAfter || errAfter != nil {
		t.Fatalf("expected allowed after unlatch, got can=%v, err=%v", canAfter, errAfter)
	}
}

func TestResilientState_AutoDecayUnlatch(t *testing.T) {
	tempDir := t.TempDir()
	snapPath := filepath.Join(tempDir, "test_snapshot.json")
	logPath := filepath.Join(tempDir, "test_ops.log")

	mgr, err := NewResilientStateManager(ResilientStateConfig{
		SnapshotPath:               snapPath,
		LogPath:                    logPath,
		MaxAttemptsPerWindow:       2,
		ConsecutiveHealthyForDecay: 3, // quick decay for test
	})
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}

	svc := "frigate"

	// Trigger latch
	for i := 0; i < 2; i++ {
		_ = mgr.RecordAttempt(svc, "restart")
		_ = mgr.RecordFailure(svc, "restart", errors.New("fail"))
	}

	st, _ := mgr.GetState(svc)
	if !st.Latched {
		t.Fatalf("expected latched state")
	}

	// 2 healthy ticks: still latched
	mgr.TickHealth(svc, true)
	mgr.TickHealth(svc, true)
	st, _ = mgr.GetState(svc)
	if !st.Latched {
		t.Fatalf("should still be latched before decay threshold")
	}

	// 3rd healthy tick: triggers unlatch decay
	mgr.TickHealth(svc, true)
	st, _ = mgr.GetState(svc)
	if st.Latched {
		t.Fatalf("expected auto-unlatch after 3 healthy ticks")
	}
}

func TestResilientState_SnapshotPersistenceAndRestore(t *testing.T) {
	tempDir := t.TempDir()
	snapPath := filepath.Join(tempDir, "persist_snapshot.json")
	logPath := filepath.Join(tempDir, "persist_ops.log")

	mgr1, err := NewResilientStateManager(ResilientStateConfig{
		SnapshotPath: snapPath,
		LogPath:      logPath,
	})
	if err != nil {
		t.Fatalf("mgr1 create: %v", err)
	}

	_ = mgr1.RecordAttempt("mosquitto", "restart")
	_ = mgr1.RecordFailure("mosquitto", "restart", errors.New("err"))

	// Create mgr2 pointing to same snapshotPath, simulating process reboot
	mgr2, err := NewResilientStateManager(ResilientStateConfig{
		SnapshotPath: snapPath,
		LogPath:      logPath,
	})
	if err != nil {
		t.Fatalf("mgr2 create: %v", err)
	}

	st, exists := mgr2.GetState("mosquitto")
	if !exists {
		t.Fatalf("expected state restored in mgr2")
	}
	if st.AttemptCount != 1 {
		t.Fatalf("expected attempt count 1, got %d", st.AttemptCount)
	}
}

func TestResilientState_DecoupledAuditLogging(t *testing.T) {
	var buf bytes.Buffer
	mgr, err := NewResilientStateManager(ResilientStateConfig{
		LogWriterOverride: &buf,
	})
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}

	mgr.LogAudit("home-vision", "restart", "SUCCESS", "verified health")
	out := buf.String()

	if len(out) == 0 {
		t.Fatalf("expected audit log output, got empty")
	}
	if !bytes.Contains(buf.Bytes(), []byte("service=home-vision")) {
		t.Fatalf("expected service in log, got: %s", out)
	}
	if !bytes.Contains(buf.Bytes(), []byte("outcome=SUCCESS")) {
		t.Fatalf("expected outcome in log, got: %s", out)
	}
}

func TestResilientState_DailyDigest(t *testing.T) {
	tempDir := t.TempDir()
	stateMgr, err := NewResilientStateManager(ResilientStateConfig{
		SnapshotPath: tempDir + "/snap.json",
		LogPath:      tempDir + "/ops.log",
	})
	if err != nil {
		t.Fatalf("create state manager: %v", err)
	}

	// Record normal attempts
	_ = stateMgr.RecordAttempt("home-vision", "restart")
	_ = stateMgr.RecordSuccess("home-vision", "restart")

	// Trigger latch on another service
	_ = stateMgr.RecordAttempt("frigate", "restart")
	_ = stateMgr.RecordFailure("frigate", "restart", nil)
	_ = stateMgr.RecordAttempt("frigate", "restart")
	_ = stateMgr.RecordFailure("frigate", "restart", nil)
	_ = stateMgr.RecordAttempt("frigate", "restart")
	_ = stateMgr.RecordFailure("frigate", "restart", nil)

	digest := stateMgr.GetDailyDigest()
	if digest.TotalServices < 2 {
		t.Fatalf("expected at least 2 services, got %d", digest.TotalServices)
	}
	if digest.ActiveLatches != 1 {
		t.Fatalf("expected 1 active latch, got %d", digest.ActiveLatches)
	}
	if len(digest.LatchedServices) != 1 || digest.LatchedServices[0] != "frigate" {
		t.Fatalf("expected latched service frigate, got: %v", digest.LatchedServices)
	}
	if !bytes.Contains([]byte(digest.SummaryReport), []byte("告警")) {
		t.Fatalf("expected alarm in summary report, got: %s", digest.SummaryReport)
	}
}

func TestOpsHandler_GetDailyDigestEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)

	tempDir := t.TempDir()
	stateMgr, _ := NewResilientStateManager(ResilientStateConfig{
		SnapshotPath: tempDir + "/snap.json",
		LogPath:      tempDir + "/ops.log",
	})
	_ = stateMgr.RecordAttempt("home-vision", "restart")
	_ = stateMgr.RecordSuccess("home-vision", "restart")

	handler := NewOpsHandler(nil, nil, db, nil, nil)
	handler.SetResilientStateManager(stateMgr)

	r := gin.New()
	rg := r.Group("/api/v1/ops")
	handler.RegisterProposalRoutes(rg)

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/ops/digest", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var d DailyOpsDigest
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatalf("unmarshal digest: %v", err)
	}
	if d.TotalServices != 1 {
		t.Fatalf("expected 1 service, got %d", d.TotalServices)
	}
}
