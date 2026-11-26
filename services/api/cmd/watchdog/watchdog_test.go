package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"home-datacenter-api/internal/hostsec"
)

type mockWatchdogBridge struct {
	restartCount int32
}

func (m *mockWatchdogBridge) BridgeInfo() hostsec.HostBridgeInfo {
	return hostsec.HostBridgeInfo{Type: "mock", Authenticated: true}
}

func (m *mockWatchdogBridge) VerifyBinding(ctx context.Context, binding hostsec.HostExecutionBinding) error {
	return nil
}

func (m *mockWatchdogBridge) ExecuteTypedAction(ctx context.Context, req hostsec.HostActionRequest) (hostsec.HostActionResult, error) {
	if req.Action == hostsec.HostActionContainerRestart && req.ServiceName == "home-api" {
		atomic.AddInt32(&m.restartCount, 1)
		return hostsec.HostActionResult{Success: true, Stdout: "restarted home-api"}, nil
	}
	return hostsec.HostActionResult{Success: false}, nil
}

func TestWatchdog_HealthyProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := NewWatchdog(srv.URL, 50*time.Millisecond, 3, 10*time.Millisecond, 2, "mock", "key", []byte("key"))
	if !w.CheckOnce(context.Background()) {
		t.Fatalf("expected healthy check")
	}
	if w.consecutiveFails != 0 {
		t.Fatalf("expected 0 fails, got %d", w.consecutiveFails)
	}
}

func TestWatchdog_FailureThresholdAndRateLimit(t *testing.T) {
	// Failing health server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	bridge := &mockWatchdogBridge{}
	w := NewWatchdog(srv.URL, 10*time.Millisecond, 3, 10*time.Millisecond, 2, "mock", "key", []byte("key"))
	w.bridgeClient = bridge

	// 1st and 2nd checks fail without triggering restart
	w.CheckOnce(context.Background())
	w.CheckOnce(context.Background())
	if atomic.LoadInt32(&bridge.restartCount) != 0 {
		t.Fatalf("restart should not trigger before 3 failures")
	}

	// 3rd check reaches threshold and triggers restart #1
	w.CheckOnce(context.Background())
	if atomic.LoadInt32(&bridge.restartCount) != 1 {
		t.Fatalf("expected restart #1 triggered, got %d", atomic.LoadInt32(&bridge.restartCount))
	}

	// Next 3 checks trigger restart #2
	w.CheckOnce(context.Background())
	w.CheckOnce(context.Background())
	w.CheckOnce(context.Background())
	if atomic.LoadInt32(&bridge.restartCount) != 2 {
		t.Fatalf("expected restart #2 triggered, got %d", atomic.LoadInt32(&bridge.restartCount))
	}

	// Next 3 checks exceed maxPerHour (2) -> must REFUSE restart
	w.CheckOnce(context.Background())
	w.CheckOnce(context.Background())
	w.CheckOnce(context.Background())
	if atomic.LoadInt32(&bridge.restartCount) != 2 {
		t.Fatalf("hourly rate limit breached: expected restart count to stay 2, got %d", atomic.LoadInt32(&bridge.restartCount))
	}
}
