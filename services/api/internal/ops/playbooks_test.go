package ops

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"home-datacenter-api/internal/hostsec"
)

type mockBridgeClient struct {
	actions []hostsec.HostActionRequest
	retOut  string
	retErr  error
	mu      sync.Mutex
}

func (m *mockBridgeClient) BridgeInfo() hostsec.HostBridgeInfo {
	return hostsec.HostBridgeInfo{Type: "mock", Authenticated: true}
}

func (m *mockBridgeClient) VerifyBinding(ctx context.Context, binding hostsec.HostExecutionBinding) error {
	return nil
}

func (m *mockBridgeClient) ExecuteTypedAction(ctx context.Context, req hostsec.HostActionRequest) (hostsec.HostActionResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.actions = append(m.actions, req)
	return hostsec.HostActionResult{Success: m.retErr == nil, Stdout: m.retOut}, m.retErr
}

func TestPlaybookEngine_RestartServiceWithHealthVerification(t *testing.T) {
	tempDir := t.TempDir()
	stateMgr, err := NewResilientStateManager(ResilientStateConfig{
		SnapshotPath: tempDir + "/snap.json",
		LogPath:      tempDir + "/ops.log",
	})
	if err != nil {
		t.Fatalf("create state manager: %v", err)
	}

	// Fake healthy HTTP server
	healthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer healthSrv.Close()

	bridge := &mockBridgeClient{retOut: "container restarted"}
	engine, err := NewPlaybookEngine(PlaybookEngineConfig{
		BridgeClient: bridge,
		StateManager: stateMgr,
		HealthCheckURLs: map[string]string{
			"home-vision": healthSrv.URL,
		},
	})
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	out, err := engine.RestartService(context.Background(), "home-vision", 10)
	if err != nil {
		t.Fatalf("restart service failed: %v", err)
	}
	if out != "container restarted" {
		t.Fatalf("unexpected out: %s", out)
	}

	st, exists := stateMgr.GetState("home-vision")
	if !exists || st.AttemptCount != 1 || st.ConsecutiveHealthyTicks != 1 {
		t.Fatalf("unexpected state: %+v", st)
	}
}

func TestPlaybookEngine_PanicContainment(t *testing.T) {
	tempDir := t.TempDir()
	stateMgr, err := NewResilientStateManager(ResilientStateConfig{
		SnapshotPath: tempDir + "/snap.json",
		LogPath:      tempDir + "/ops.log",
	})
	if err != nil {
		t.Fatalf("create state manager: %v", err)
	}

	engine, err := NewPlaybookEngine(PlaybookEngineConfig{
		StateManager: stateMgr,
	})
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	// Intentionally trigger a panic inside ExecuteSafe
	_, execErr := engine.ExecuteSafe(context.Background(), "home-vision", "panic-test", func(ctx context.Context) (string, error) {
		panic("nil pointer dereference simulation")
	})

	if execErr == nil || !errors.Is(execErr, ErrExecutionPanic) {
		t.Fatalf("expected ErrExecutionPanic, got: %v", execErr)
	}

	// Verify that state manager recorded failure and did not crash
	st, exists := stateMgr.GetState("home-vision")
	if !exists || st.AttemptCount != 1 {
		t.Fatalf("state manager should have recorded attempt: %+v", st)
	}
}

func TestPlaybookEngine_BulkheadWorkerLimit(t *testing.T) {
	tempDir := t.TempDir()
	stateMgr, err := NewResilientStateManager(ResilientStateConfig{
		SnapshotPath: tempDir + "/snap.json",
		LogPath:      tempDir + "/ops.log",
	})
	if err != nil {
		t.Fatalf("create state manager: %v", err)
	}

	engine, err := NewPlaybookEngine(PlaybookEngineConfig{
		StateManager:     stateMgr,
		MaxConcurrentOps: 1, // Only 1 worker permitted
	})
	if err != nil {
		t.Fatalf("create engine: %v", err)
	}

	start := make(chan struct{})
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)

	// Long running task occupying the 1 slot
	go func() {
		defer wg.Done()
		_, _ = engine.ExecuteSafe(context.Background(), "service-a", "long-task", func(ctx context.Context) (string, error) {
			close(start)
			<-done
			return "ok", nil
		})
	}()

	<-start

	// Second concurrent task should be rejected immediately by bulkhead
	_, err2 := engine.ExecuteSafe(context.Background(), "service-b", "quick-task", func(ctx context.Context) (string, error) {
		return "ok", nil
	})

	close(done)
	wg.Wait()

	if !errors.Is(err2, ErrBulkheadCapacityExceeded) {
		t.Fatalf("expected ErrBulkheadCapacityExceeded, got: %v", err2)
	}
}

func TestPlaybookEngine_PruneOrphansFilterValidation(t *testing.T) {
	tempDir := t.TempDir()
	stateMgr, _ := NewResilientStateManager(ResilientStateConfig{
		SnapshotPath: tempDir + "/snap.json",
		LogPath:      tempDir + "/ops.log",
	})
	bridge := &mockBridgeClient{retOut: "deleted"}
	engine, _ := NewPlaybookEngine(PlaybookEngineConfig{
		BridgeClient: bridge,
		StateManager: stateMgr,
	})

	// 1. Rejected prefix
	_, errBad := engine.PruneOrphans(context.Background(), "production-container")
	if errBad == nil || !strings.Contains(errBad.Error(), "rejected") {
		t.Fatalf("expected rejection for non-tmp filter, got: %v", errBad)
	}

	// 2. Permitted prefix
	out, errGood := engine.PruneOrphans(context.Background(), "hdc-tmp-container-99")
	if errGood != nil {
		t.Fatalf("expected success for hdc-tmp-*, got: %v", errGood)
	}
	if out != "deleted" {
		t.Fatalf("expected deleted, got: %s", out)
	}
}
