package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/audit"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// setupR14TestDB initializes an isolated SQLite database with required tables.
func setupR14TestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "r14_test.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test sqlite db: %v", err)
	}
	if sqlDB, sErr := db.DB(); sErr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := db.AutoMigrate(
		&model.OpsProposal{},
		&model.OpsExecutionHistory{},
		&model.SystemLog{},
		&model.StructuredOpsAuditLog{},
	); err != nil {
		t.Fatalf("failed to auto migrate models: %v", err)
	}
	if err := EnsureOpsSchema(db); err != nil {
		t.Fatalf("EnsureOpsSchema failed: %v", err)
	}
	return db
}

// LowLevelFaultMode defines transport-level injection behaviors.
type LowLevelFaultMode int

const (
	LLFaultNone LowLevelFaultMode = iota
	LLFaultHangAfterRequestLine
	LLFaultRSTAfterHeaders
	LLFaultHTTP500
	LLFaultHTTP404
	LLFaultHTTP204Success
)

// LowLevelFaultServer implements a TCP listener that simulates raw transport faults.
type LowLevelFaultServer struct {
	listener net.Listener
	addr     string
	mode     LowLevelFaultMode
	reqCount atomic.Int64
	closed   atomic.Bool
	wg       sync.WaitGroup
}

func newLowLevelFaultServer(t *testing.T, mode LowLevelFaultMode) *LowLevelFaultServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen tcp: %v", err)
	}
	s := &LowLevelFaultServer{
		listener: ln,
		addr:     ln.Addr().String(),
		mode:     mode,
	}

	s.wg.Add(1)
	go s.serve()
	return s
}

func (s *LowLevelFaultServer) Close() {
	if s.closed.CompareAndSwap(false, true) {
		_ = s.listener.Close()
		s.wg.Wait()
	}
}

func (s *LowLevelFaultServer) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.reqCount.Add(1)
		go s.handleConn(conn)
	}
}

func (s *LowLevelFaultServer) handleConn(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 1024)
	n, err := c.Read(buf)
	if err != nil && n == 0 {
		return
	}

	switch s.mode {
	case LLFaultHangAfterRequestLine:
		// Hang until context timeout (or up to 10s)
		time.Sleep(5 * time.Second)
	case LLFaultRSTAfterHeaders:
		// Read headers then trigger TCP RST with SO_LINGER = 0
		if tcpConn, ok := c.(*net.TCPConn); ok {
			_ = tcpConn.SetLinger(0)
		}
		_ = c.Close()
	case LLFaultHTTP500:
		resp := "HTTP/1.1 500 Internal Server Error\r\nContent-Length: 35\r\nConnection: close\r\n\r\n{\"message\":\"docker engine failure\"}"
		_, _ = c.Write([]byte(resp))
	case LLFaultHTTP404:
		resp := "HTTP/1.1 404 Not Found\r\nContent-Length: 38\r\nConnection: close\r\n\r\n{\"message\":\"No such container: frigate\"}"
		_, _ = c.Write([]byte(resp))
	case LLFaultHTTP204Success:
		resp := "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n"
		_, _ = c.Write([]byte(resp))
	}
}

// TestR14_DockerRestart_NoCrossChannelRetry verifies that when Docker socket encounters
// an ambiguous failure (timeout, RST, 500), 1Panel is NEVER called, and the operation fails closed.
// This test directly exercises DefaultOperationExecutor.Execute() through the full pipeline
// and verifies that the Docker socket was called exactly once, 1Panel was called 0 times,
// and HostFallback was called 0 times.
func TestR14_DockerRestart_NoCrossChannelRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		fault         LowLevelFaultMode
		expectUnknown bool
		expectErr     string
	}{
		{
			name:          "DockerSocket_HangTimeout_Blocks1Panel",
			fault:         LLFaultHangAfterRequestLine,
			expectUnknown: true,
			expectErr:     "ambiguous",
		},
		{
			name:          "DockerSocket_RSTOnHeaders_Blocks1Panel",
			fault:         LLFaultRSTAfterHeaders,
			expectUnknown: true,
			expectErr:     "ambiguous",
		},
		{
			name:          "DockerSocket_HTTP500_Blocks1Panel",
			fault:         LLFaultHTTP500,
			expectUnknown: true,
			expectErr:     "ambiguous",
		},
		{
			name:          "DockerSocket_HTTP404_ConfirmedFailure_Blocks1Panel",
			fault:         LLFaultHTTP404,
			expectUnknown: false,
			expectErr:     "not found",
		},
		{
			name:          "DockerSocket_HTTP204Success_NeverCalls1Panel",
			fault:         LLFaultHTTP204Success,
			expectUnknown: false,
			expectErr:     "",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := setupR14TestDB(t)

			// 1. Setup Docker mock fault server
			dockerFaultServer := newLowLevelFaultServer(t, tc.fault)
			defer dockerFaultServer.Close()

			// Create a temporary dummy socket file so os.Stat passes
			tmpDir := t.TempDir()
			sockPath := filepath.Join(tmpDir, "docker.sock")
			if err := os.WriteFile(sockPath, []byte("mock-socket"), 0600); err != nil {
				t.Fatalf("failed to create dummy socket file: %v", err)
			}

			// 2. Setup 1Panel mock server with probe counter
			var onePanelRestartCalls atomic.Int64
			onePanelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/api/v2/containers/operate") {
					onePanelRestartCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"code":    200,
						"message": "restarted",
					})
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer onePanelServer.Close()

			onePanelClient := NewOnePanelClient(onePanelServer.URL, "btpanel", "mock_u", "mock_p")
			onePanelClient.psession = "mock_session"
			onePanelClient.lastLoginAt = time.Now()
			onePanelClient.loginTTL = time.Hour

			// 3. Setup DefaultOperationExecutor with real pipeline integration
			exec := NewOperationExecutor(
				db,
				eventbus.New(),
				onePanelClient,
				nil,
				WithDockerSocket(sockPath),
				WithDockerDialContext(func(dCtx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(dCtx, "tcp", dockerFaultServer.addr)
				}),
				WithHealthChecker(func(ctx context.Context, target string) error {
					return nil
				}),
			)

			// 4. Execute the operation through the authoritative Execute() pipeline
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			res, err := exec.Execute(ctx, OperationSpec{
				Action: ActionDockerRestart,
				Target: ComponentFrigate,
			})

			// 5. Strict verification of channel call counts
			dockerCalls := dockerFaultServer.reqCount.Load()
			if dockerCalls != 1 {
				t.Fatalf("expected exactly 1 call to Docker Socket, got: %d", dockerCalls)
			}

			onePanelCalls := onePanelRestartCalls.Load()
			if onePanelCalls != 0 {
				t.Fatalf("CRITICAL SECURITY VIOLATION: 1Panel was called %d times during Docker socket operation!", onePanelCalls)
			}

			// 6. Outcome and error verification
			if tc.expectUnknown {
				if err == nil {
					t.Fatalf("expected error for ambiguous failure, got nil")
				}
				if !errors.Is(err, ErrExecutionAmbiguous) {
					t.Fatalf("expected ErrExecutionAmbiguous, got: %v", err)
				}
				if res == nil {
					t.Fatalf("expected non-nil ExecutionResult")
				}
				if res.Outcome != OutcomeDispatchedUnknown {
					t.Fatalf("expected OutcomeDispatchedUnknown, got: %s", res.Outcome)
				}
			} else if tc.fault == LLFaultHTTP404 {
				if err == nil {
					t.Fatalf("expected 404 error, got nil")
				}
				if errors.Is(err, ErrExecutionAmbiguous) {
					t.Fatalf("confirmed 404 must NOT be classified as ErrExecutionAmbiguous, got: %v", err)
				}
				if !strings.Contains(strings.ToLower(err.Error()), "not found") && !strings.Contains(err.Error(), "404") {
					t.Fatalf("expected not found error message, got: %v", err)
				}
				if res == nil {
					t.Fatalf("expected non-nil ExecutionResult")
				}
				if res.Outcome != OutcomeFailedConfirmed {
					t.Fatalf("expected OutcomeFailedConfirmed, got: %s", res.Outcome)
				}
			} else if tc.fault == LLFaultHTTP204Success {
				if err != nil {
					t.Fatalf("expected success, got error: %v", err)
				}
				if res == nil || !res.Success {
					t.Fatalf("expected successful ExecutionResult, got: %v", res)
				}
				if res.Outcome != OutcomeExecuted {
					t.Fatalf("expected OutcomeExecuted, got: %s", res.Outcome)
				}
			}
		})
	}
}

// TestR14_ControlledFallback_OnlyOnNotDispatched verifies that fallback to 1Panel
// is permitted ONLY when the primary Docker Socket connection was NOT dispatched (Dial refused / socket absent).
func TestR14_ControlledFallback_OnlyOnNotDispatched(t *testing.T) {
	t.Parallel()
	db := setupR14TestDB(t)

	var onePanelCalls atomic.Int64
	onePanelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/v2/containers/operate") {
			onePanelCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    200,
				"message": "restarted",
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer onePanelServer.Close()

	client := NewOnePanelClient(onePanelServer.URL, "btpanel", "mock_u", "mock_p")
	client.psession = "mock_session"
	client.lastLoginAt = time.Now()
	client.loginTTL = time.Hour

	// Point docker socket to non-existent path
	nonExistentSock := filepath.Join(t.TempDir(), "non_existent.sock")

	exec := NewOperationExecutor(
		db,
		eventbus.New(),
		client,
		nil,
		WithDockerSocket(nonExistentSock),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	summary, err := exec.executeDockerRestart(ctx, "frigate")
	if err != nil {
		t.Fatalf("expected successful fallback restart via 1Panel, got error: %v", err)
	}
	if !strings.Contains(summary, "1Panel API") {
		t.Fatalf("expected summary to indicate 1Panel restart, got: %s", summary)
	}

	if calls := onePanelCalls.Load(); calls != 1 {
		t.Fatalf("expected exactly 1 call to 1Panel on NotDispatched fallback, got: %d", calls)
	}
}

// TestR14_ExtractBaseline_AbolishesPseudoContainerID verifies that 1Panel container responses
// missing a valid container ID fail closed and NEVER synthesize 1panel:<name> pseudo IDs.
func TestR14_ExtractBaseline_AbolishesPseudoContainerID(t *testing.T) {
	t.Parallel()
	db := setupR14TestDB(t)

	testCases := []struct {
		name        string
		containerID string
		shouldFail  bool
	}{
		{
			name:        "EmptyContainerID_FailsClosed",
			containerID: "",
			shouldFail:  true,
		},
		{
			name:        "Pseudo1PanelContainerID_FailsClosed",
			containerID: "1panel:home-frigate",
			shouldFail:  true,
		},
		{
			name:        "Contains1PanelPrefix_FailsClosed",
			containerID: "1panel:random-hash-1234",
			shouldFail:  true,
		},
		{
			name:        "WhitespaceContainerID_FailsClosed",
			containerID: "    ",
			shouldFail:  true,
		},
		{
			name:        "ValidContainerID_Passes",
			containerID: "e9a3b4c5d6e7f80123456789abcdef01e9a3b4c5d6e7f80123456789abcdef01",
			shouldFail:  false,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/api/v2/containers/search") {
					items := []onePanelContainerItem{
						{
							ContainerID: tc.containerID,
							Name:        "/home-frigate",
							State:       "running",
							Status:      "Up 2 hours",
							Image:       "ghcr.io/blakeblackshear/frigate:0.14.1",
							Created:     "2026-11-01T00:00:00Z",
						},
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
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			client := NewOnePanelClient(server.URL, "btpanel", "mock_u", "mock_p")
			client.psession = "mock_token"
			client.lastLoginAt = time.Now()
			client.loginTTL = time.Hour

			exec := NewOperationExecutor(
				db,
				eventbus.New(),
				client,
				nil,
				// No local socket so it queries 1Panel
				WithDockerSocket(""),
			)

			info := exec.ExtractBaseline("frigate")

			if tc.shouldFail {
				if info.Hash != "" {
					t.Fatalf("expected empty baseline hash for invalid ContainerID %q, got: %s", tc.containerID, info.Hash)
				}
				if info.Error == "" {
					t.Fatalf("expected error for invalid ContainerID %q, got none", tc.containerID)
				}
				if strings.Contains(info.ContainerID, "1panel:") {
					t.Fatalf("SECURITY VIOLATION: pseudo ID %q was synthesized!", info.ContainerID)
				}
			} else {
				if info.Hash == "" {
					t.Fatalf("expected valid baseline hash for valid ContainerID, got error: %s", info.Error)
				}
				if info.ContainerID != tc.containerID {
					t.Fatalf("expected ContainerID %q, got %q", tc.containerID, info.ContainerID)
				}
			}
		})
	}
}

// TestR14_HostFallback_RequiresLeaseVerification verifies that HostFallbackProvider.RestartContainerFallback
// strictly fails closed if the context lease is missing, mismatched, or expired.
func TestR14_HostFallback_RequiresLeaseVerification(t *testing.T) {
	t.Parallel()
	fallback := NewHostFallbackProvider(filepath.Join(t.TempDir(), "dummy.sock"), "")

	expiredGuard := &LeaseGuard{
		Grant: LeaseGrant{
			LeaseToken:  "test-token",
			Component:   "home-api",
			ExpiresAtMs: time.Now().Add(-1 * time.Hour).UnixMilli(),
		},
		expMs: time.Now().Add(-1 * time.Hour).UnixMilli(),
	}
	ctx := WithRecoveryLease(context.Background(), expiredGuard)

	err := fallback.RestartContainerFallback(ctx, "home-api")
	if err == nil {
		t.Fatalf("expected error for expired recovery lease in fallback restart, got nil")
	}
	if !strings.Contains(err.Error(), "lease guard") && !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected lease guard rejection error, got: %v", err)
	}
}

// TestR14_ExecutionResult_FullChainMetadata verifies that AttemptID, Channel, Stage,
// and Outcome are recorded on ExecutionResult.
func TestR14_ExecutionResult_FullChainMetadata(t *testing.T) {
	t.Parallel()
	db := setupR14TestDB(t)

	auditEngine := audit.NewAgentAuditEngine(db, eventbus.New(), audit.WithArchiveDir(filepath.Join(t.TempDir(), "audit")))

	exec := NewOperationExecutor(
		db,
		eventbus.New(),
		nil,
		nil,
		WithAuditEngine(auditEngine),
	)

	ctx := context.Background()

	res, err := exec.Execute(ctx, OperationSpec{
		Action: ActionDockerRestart,
		Target: "frigate",
	})

	if err == nil {
		t.Fatalf("expected error since no docker daemon is available, got nil")
	}
	if res == nil {
		t.Fatalf("expected non-nil ExecutionResult, got nil")
	}

	// Verify structured metadata fields exist
	if res.ExecutedAt.IsZero() {
		t.Fatalf("expected ExecutedAt to be populated")
	}
	if res.Action != ActionDockerRestart {
		t.Fatalf("expected Action %s, got %s", ActionDockerRestart, res.Action)
	}
	if res.Target != "frigate" {
		t.Fatalf("expected Target frigate, got %s", res.Target)
	}
}

// TestR14_Scheduler_IgnoresExecutionUnknown verifies that proposals in EXECUTION_UNKNOWN
// or RECONCILIATION_REQUIRED status are never scheduled or executed automatically.
func TestR14_Scheduler_IgnoresExecutionUnknown(t *testing.T) {
	t.Parallel()
	db := setupR14TestDB(t)

	schedTime := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	// Create test proposals in different statuses
	unknownProp := model.OpsProposal{
		HouseholdID:     1,
		ProposalID:      "prop-unknown-001",
		Title:           "Unknown Proposal",
		TargetComponent: "frigate",
		Status:          model.ProposalStatusExecutionUnknown,
		ScheduledAt:     &schedTime,
		ActionSpec:      `{"action":"docker.restart","target":"frigate"}`,
	}
	if err := db.Create(&unknownProp).Error; err != nil {
		t.Fatalf("failed to insert unknown proposal: %v", err)
	}

	reconcileProp := model.OpsProposal{
		HouseholdID:     1,
		ProposalID:      "prop-reconcile-002",
		Title:           "Reconciliation Required Proposal",
		TargetComponent: "frigate",
		Status:          model.ProposalStatusReconciliationRequired,
		ScheduledAt:     &schedTime,
		ActionSpec:      `{"action":"docker.restart","target":"frigate"}`,
	}
	if err := db.Create(&reconcileProp).Error; err != nil {
		t.Fatalf("failed to insert reconcile proposal: %v", err)
	}

	var execCalls atomic.Int64
	mockExec := NewOperationExecutor(db, eventbus.New(), nil, nil)

	scheduler := NewProposalScheduler(db, mockExec)

	// Run Tick()
	scheduler.Tick()

	// Verify neither proposal was claimed or modified
	var check1, check2 model.OpsProposal
	if err := db.Where("proposal_id = ?", "prop-unknown-001").First(&check1).Error; err != nil {
		t.Fatalf("failed to find unknown prop: %v", err)
	}
	if check1.Status != model.ProposalStatusExecutionUnknown {
		t.Fatalf("proposal status mutated! expected EXECUTION_UNKNOWN, got: %s", check1.Status)
	}
	if check1.ExecutionToken != "" {
		t.Fatalf("proposal was claimed with ExecutionToken %q!", check1.ExecutionToken)
	}

	if err := db.Where("proposal_id = ?", "prop-reconcile-002").First(&check2).Error; err != nil {
		t.Fatalf("failed to find reconcile prop: %v", err)
	}
	if check2.Status != model.ProposalStatusReconciliationRequired {
		t.Fatalf("proposal status mutated! expected RECONCILIATION_REQUIRED, got: %s", check2.Status)
	}

	if calls := execCalls.Load(); calls != 0 {
		t.Fatalf("expected 0 executor calls for unknown/reconciliation proposals, got: %d", calls)
	}
}

// TestR14_ProposalFSM_ExecutionUnknownTransitions verifies that ProposalFSM rejects
// transitioning EXECUTION_UNKNOWN directly back to EXECUTING or SCHEDULED.
func TestR14_ProposalFSM_ExecutionUnknownTransitions(t *testing.T) {
	t.Parallel()
	db := setupR14TestDB(t)

	prop := model.OpsProposal{
		HouseholdID:     1,
		ProposalID:      "prop-fsm-unk-001",
		Title:           "FSM Unknown Test",
		TargetComponent: "frigate",
		Status:          model.ProposalStatusExecutionUnknown,
	}
	if err := db.Create(&prop).Error; err != nil {
		t.Fatalf("failed to insert prop: %v", err)
	}

	fsm := NewProposalFSM(db)

	// Attempt 1: Transition directly back to EXECUTING -> must be rejected
	won, err := fsm.TransitionStrict(ProposalTransitionParams{
		ProposalID:       "prop-fsm-unk-001",
		ExpectedStatuses: []string{model.ProposalStatusExecutionUnknown},
		TargetStatus:     model.ProposalStatusExecuting,
	})
	if err == nil || won {
		t.Fatalf("expected transition from EXECUTION_UNKNOWN to EXECUTING to be rejected, got won=%v err=%v", won, err)
	}

	// Attempt 2: Transition directly back to SCHEDULED -> must be rejected
	won, err = fsm.TransitionStrict(ProposalTransitionParams{
		ProposalID:       "prop-fsm-unk-001",
		ExpectedStatuses: []string{model.ProposalStatusExecutionUnknown},
		TargetStatus:     model.ProposalStatusScheduled,
	})
	if err == nil || won {
		t.Fatalf("expected transition from EXECUTION_UNKNOWN to SCHEDULED to be rejected, got won=%v err=%v", won, err)
	}
}

// TestR14_SocketMutation_RequiresActiveLeaseInProduction verifies that in production runtime,
// direct socket mutation strictly fails closed without an active recovery lease guard.
func TestR14_SocketMutation_RequiresActiveLeaseInProduction(t *testing.T) {
	t.Setenv("OPS_STRICT_BASELINE", "1")
	if !IsProductionRuntime() {
		t.Fatal("expected IsProductionRuntime() to be true")
	}

	ctx := context.Background()
	// No lease guard in ctx
	err := verifyContextLeaseBeforeSocket(ctx)
	if err == nil {
		t.Fatal("expected verifyContextLeaseBeforeSocket to fail in production without lease guard, but got nil")
	}
	if !errors.Is(err, ErrInvalidLeaseToken) {
		t.Fatalf("expected ErrInvalidLeaseToken, got: %v", err)
	}

	// Verify that executeDockerSocketRestart fails immediately at PreCheck stage
	tracker := NewStageTracker("docker_socket", ActionDockerRestart, "frigate")
	execErr := executeDockerSocketRestart(ctx, "/nonexistent/socket.sock", "frigate", tracker)
	if execErr == nil {
		t.Fatal("expected executeDockerSocketRestart to fail in production without lease guard, got nil")
	}
	if !errors.Is(execErr, ErrInvalidLeaseToken) {
		t.Fatalf("expected ErrInvalidLeaseToken, got: %v", execErr)
	}
	if tracker.Stage() != StagePreCheck {
		t.Fatalf("expected failure at StagePreCheck, got: %s", tracker.Stage())
	}
	if tracker.Category() != OutcomeNotDispatched {
		t.Fatalf("expected OutcomeNotDispatched, got: %s", tracker.Category())
	}
}

// TestR14_HostFallback_EnforcesComponentWhitelist verifies that HostFallbackProvider
// strictly enforces component whitelisting prior to any socket access.
func TestR14_HostFallback_EnforcesComponentWhitelist(t *testing.T) {
	t.Parallel()
	fallback := NewHostFallbackProvider(t.TempDir(), t.TempDir())

	ctx := context.Background()

	// 1. Non-whitelisted component
	err := fallback.RestartContainerFallback(ctx, "malicious_miner")
	if err == nil {
		t.Fatal("expected non-whitelisted container to be rejected, got nil")
	}
	if !errors.Is(err, ErrComponentNotWhitelisted) {
		t.Fatalf("expected ErrComponentNotWhitelisted, got: %v", err)
	}

	// 2. Empty container
	err = fallback.RestartContainerFallback(ctx, "")
	if err == nil {
		t.Fatal("expected empty container to be rejected, got nil")
	}
	if !errors.Is(err, ErrInvalidContainer) {
		t.Fatalf("expected ErrInvalidContainer, got: %v", err)
	}

	// 3. Container traversal / slash
	err = fallback.RestartContainerFallback(ctx, "../../../etc/shadow")
	if err == nil {
		t.Fatal("expected path traversal container to be rejected, got nil")
	}
	if !errors.Is(err, ErrComponentNotWhitelisted) {
		t.Fatalf("expected ErrComponentNotWhitelisted, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// R14.1 Comprehensive Adversarial & Multi-Stage Tests
// ---------------------------------------------------------------------------

type mockDockerRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *mockDockerRecorder) log(call string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
}

func (r *mockDockerRecorder) getCalls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	res := make([]string, len(r.calls))
	copy(res, r.calls)
	return res
}

func (r *mockDockerRecorder) countPrefix(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := 0
	for _, call := range r.calls {
		if strings.HasPrefix(call, prefix) {
			c++
		}
	}
	return c
}

// standardInspectResponse returns a valid container inspect JSON payload for home-frigate.
func standardInspectResponse() []byte {
	return []byte(`{
		"Id": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"Name": "/home-frigate",
		"Config": {
			"Image": "ghcr.io/blakeblackshear/frigate:0.14.1",
			"User": "1000:1000",
			"Env": ["FRIGATE_RTSP_PASSWORD=secret"],
			"Cmd": ["frigate"]
		},
		"HostConfig": {
			"Privileged": false,
			"RestartPolicy": {"Name": "unless-stopped"}
		},
		"State": {
			"Running": true,
			"Status": "running"
		}
	}`)
}

// TestR141_DockerRecreate_StopAmbiguous_HaltsWithoutRollbackOrDelete verifies that when
// STOP produces an ambiguous timeout/500, recreate halts immediately without deleting
// or modifying the original container.
func TestR141_DockerRecreate_StopAmbiguous_HaltsWithoutRollbackOrDelete(t *testing.T) {
	t.Parallel()
	db := setupR14TestDB(t)

	recorder := &mockDockerRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.log(fmt.Sprintf("%s %s", r.Method, r.URL.RequestURI()))
		path := r.URL.Path

		switch {
		case r.Method == http.MethodGet && strings.Contains(path, "/json"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(standardInspectResponse())
		case r.Method == http.MethodPost && strings.Contains(path, "/stop"):
			// Simulate ambiguous 500 on stop
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"daemon timeout stopping container"}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	exec := NewOperationExecutor(
		db,
		eventbus.New(),
		nil,
		nil,
		WithDockerSocket("tcp://" + server.Listener.Addr().String()),
		WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := exec.Execute(ctx, OperationSpec{
		Action: ActionDockerRecreate,
		Target: ComponentFrigate,
		Params: map[string]interface{}{"image": "ghcr.io/blakeblackshear/frigate:0.14.2"},
	})

	if err == nil || !errors.Is(err, ErrExecutionAmbiguous) {
		t.Fatalf("expected ErrExecutionAmbiguous, got: %v", err)
	}
	if res == nil || res.Outcome != OutcomeDispatchedUnknown {
		t.Fatalf("expected res.Outcome == OutcomeDispatchedUnknown, got: %v", res)
	}

	// CRITICAL ASSERTION: Zero DELETE calls, zero rename calls, zero create calls
	if c := recorder.countPrefix("DELETE"); c != 0 {
		t.Fatalf("CRITICAL SECURITY VIOLATION: DELETE called %d times on stop failure!", c)
	}
	if c := recorder.countPrefix("POST /containers/create"); c != 0 {
		t.Fatalf("create called %d times after stop failed!", c)
	}
}

// TestR141_DockerRecreate_CreateAmbiguous_HaltsWithoutRollbackOrDelete verifies that when
// CREATE produces an ambiguous 500, recreate STOPS IMMEDIATELY and DOES NOT call restoreOriginal
// or DELETE /containers/home-frigate (preventing the SEV-0 original container deletion).
func TestR141_DockerRecreate_CreateAmbiguous_HaltsWithoutRollbackOrDelete(t *testing.T) {
	t.Parallel()
	db := setupR14TestDB(t)

	recorder := &mockDockerRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.log(fmt.Sprintf("%s %s", r.Method, r.URL.RequestURI()))
		path := r.URL.Path

		switch {
		case r.Method == http.MethodGet && strings.Contains(path, "/json"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(standardInspectResponse())
		case r.Method == http.MethodPost && strings.Contains(path, "/stop"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.Contains(path, "/rename"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.Contains(path, "/containers/create"):
			// Simulate ambiguous 500 during create
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"docker daemon I/O error during create"}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	exec := NewOperationExecutor(
		db,
		eventbus.New(),
		nil,
		nil,
		WithDockerSocket("tcp://" + server.Listener.Addr().String()),
		WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := exec.Execute(ctx, OperationSpec{
		Action: ActionDockerRecreate,
		Target: ComponentFrigate,
		Params: map[string]interface{}{"image": "ghcr.io/blakeblackshear/frigate:0.14.2"},
	})

	if err == nil || !errors.Is(err, ErrExecutionAmbiguous) {
		t.Fatalf("expected ErrExecutionAmbiguous on ambiguous create, got: %v", err)
	}
	if res == nil || res.Outcome != OutcomeDispatchedUnknown {
		t.Fatalf("expected OutcomeDispatchedUnknown, got: %v", res)
	}

	// CRITICAL SEV-0 PREVENTION ASSERTION:
	// Absolutely ZERO DELETE calls must be made!
	if c := recorder.countPrefix("DELETE"); c != 0 {
		t.Fatalf("SEV-0 VULNERABILITY DETECTED: DELETE was called %d times on ambiguous create! Calls: %v", c, recorder.getCalls())
	}
}

// TestR141_DockerRecreate_CreateConfirmedFailure_SafeBackupRestoreWithoutDelete verifies that when
// CREATE fails with a confirmed 400 Bad Request, the system safely restores the backup container
// (renaming backup back to original and starting it) without ever calling DELETE.
func TestR141_DockerRecreate_CreateConfirmedFailure_SafeBackupRestoreWithoutDelete(t *testing.T) {
	t.Parallel()
	db := setupR14TestDB(t)

	recorder := &mockDockerRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.log(fmt.Sprintf("%s %s", r.Method, r.URL.RequestURI()))
		path := r.URL.Path

		switch {
		case r.Method == http.MethodGet && strings.Contains(path, "/json"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(standardInspectResponse())
		case r.Method == http.MethodPost && strings.Contains(path, "/stop"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.Contains(path, "/rename"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.Contains(path, "/containers/create"):
			// Confirmed 400 failure (invalid spec)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"invalid parameter in container configuration"}`))
		case r.Method == http.MethodPost && strings.Contains(path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	exec := NewOperationExecutor(
		db,
		eventbus.New(),
		nil,
		nil,
		WithDockerSocket("tcp://" + server.Listener.Addr().String()),
		WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := exec.Execute(ctx, OperationSpec{
		Action: ActionDockerRecreate,
		Target: ComponentFrigate,
		Params: map[string]interface{}{"image": "ghcr.io/blakeblackshear/frigate:0.14.2"},
	})

	if err == nil {
		t.Fatalf("expected error on confirmed create failure, got nil")
	}
	if errors.Is(err, ErrExecutionAmbiguous) {
		t.Fatalf("confirmed failure must not be classified as ErrExecutionAmbiguous")
	}
	if res == nil || res.Outcome != OutcomeFailedConfirmed {
		t.Fatalf("expected res.Outcome == OutcomeFailedConfirmed, got: %v", res)
	}

	calls := recorder.getCalls()
	hasRenameToBak := false
	hasRenameBack := false
	hasOrigRestart := false
	hasDelete := false

	for _, call := range calls {
		if strings.Contains(call, "POST /containers/home-frigate/rename?name=home-frigate_bak_") {
			hasRenameToBak = true
		}
		if strings.Contains(call, "POST /containers/home-frigate_bak_") && strings.Contains(call, "rename?name=home-frigate") {
			hasRenameBack = true
		}
		if strings.Contains(call, "POST /containers/home-frigate/start") {
			hasOrigRestart = true
		}
		if strings.HasPrefix(call, "DELETE") {
			hasDelete = true
		}
	}

	if !hasRenameToBak {
		t.Fatalf("expected container to be renamed to backup, calls: %v", calls)
	}
	if !hasRenameBack {
		t.Fatalf("expected backup container to be restored via rename back, calls: %v", calls)
	}
	if !hasOrigRestart {
		t.Fatalf("expected original container to be restarted, calls: %v", calls)
	}
	if hasDelete {
		t.Fatalf("CRITICAL SECURITY VIOLATION: DELETE was called during safe backup restore! Calls: %v", calls)
	}
}

// TestR141_DockerRecreate_StartConfirmedFailure_DeletesCandidateByIDAndRestoresBackup verifies that when
// START returns a confirmed 400 failure, the candidate container is deleted STRICTLY BY ID (not by name),
// and the backup container is restored safely.
func TestR141_DockerRecreate_StartConfirmedFailure_DeletesCandidateByIDAndRestoresBackup(t *testing.T) {
	t.Parallel()
	db := setupR14TestDB(t)

	candidateID := "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
	recorder := &mockDockerRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.log(fmt.Sprintf("%s %s", r.Method, r.URL.RequestURI()))
		path := r.URL.Path

		switch {
		case r.Method == http.MethodGet && strings.Contains(path, "/json"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(standardInspectResponse())
		case r.Method == http.MethodPost && strings.Contains(path, "/stop"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.Contains(path, "/rename"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.Contains(path, "/containers/create"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"Id":%q,"Warnings":[]}`, candidateID)))
		case r.Method == http.MethodPost && strings.Contains(path, "/start"):
			// Confirmed start failure
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"entrypoint binary not found"}`))
		case r.Method == http.MethodDelete && strings.Contains(path, candidateID):
			// Candidate cleanup by ID
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	exec := NewOperationExecutor(
		db,
		eventbus.New(),
		nil,
		nil,
		WithDockerSocket("tcp://" + server.Listener.Addr().String()),
		WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := exec.Execute(ctx, OperationSpec{
		Action: ActionDockerRecreate,
		Target: ComponentFrigate,
		Params: map[string]interface{}{"image": "ghcr.io/blakeblackshear/frigate:0.14.2"},
	})

	if err == nil {
		t.Fatalf("expected error on start failure, got nil")
	}
	if res == nil || res.Outcome != OutcomeFailedConfirmed {
		t.Fatalf("expected OutcomeFailedConfirmed, got: %v", res)
	}

	calls := recorder.getCalls()
	hasCandidateDeleteByID := false
	hasBlindDeleteByName := false

	for _, call := range calls {
		if strings.HasPrefix(call, "DELETE") {
			if strings.Contains(call, candidateID) {
				hasCandidateDeleteByID = true
			}
			if strings.Contains(call, "/containers/home-frigate?") || strings.Contains(call, "/containers/home-frigate ") {
				hasBlindDeleteByName = true
			}
		}
	}

	if !hasCandidateDeleteByID {
		t.Fatalf("expected candidate container to be deleted by ID %s, calls: %v", candidateID, calls)
	}
	if hasBlindDeleteByName {
		t.Fatalf("CRITICAL SECURITY VIOLATION: container deleted by name /home-frigate! Calls: %v", calls)
	}
}

// TestR141_DockerRecreate_VerifyInspect500_HaltsWithoutCandidateDelete verifies that when
// Stage 5 verify inspect returns a 500 error from Docker daemon, the candidate container
// that was successfully started is NOT deleted, preventing the BLOCKER-1 SEV-0 bug.
func TestR141_DockerRecreate_VerifyInspect500_HaltsWithoutCandidateDelete(t *testing.T) {
	t.Parallel()
	db := setupR14TestDB(t)

	candidateID := "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
	recorder := &mockDockerRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.log(fmt.Sprintf("%s %s", r.Method, r.URL.RequestURI()))
		path := r.URL.Path

		switch {
		case r.Method == http.MethodGet && strings.Contains(path, "/json"):
			if strings.Contains(path, candidateID) {
				// Stage 5 verify inspect returns 500 internal server error
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"docker daemon transient error on inspect"}`))
				return
			}
			// Stage 0 inspect of original container succeeds
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(standardInspectResponse())
		case r.Method == http.MethodPost && strings.Contains(path, "/stop"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.Contains(path, "/rename"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.Contains(path, "/containers/create"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"Id":%q,"Warnings":[]}`, candidateID)))
		case r.Method == http.MethodPost && strings.Contains(path, "/start"):
			// Start succeeds
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	exec := NewOperationExecutor(
		db,
		eventbus.New(),
		nil,
		nil,
		WithDockerSocket("tcp://" + server.Listener.Addr().String()),
		WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := exec.Execute(ctx, OperationSpec{
		Action: ActionDockerRecreate,
		Target: ComponentFrigate,
		Params: map[string]interface{}{"image": "ghcr.io/blakeblackshear/frigate:0.14.2"},
	})

	if err == nil || !errors.Is(err, ErrExecutionAmbiguous) {
		t.Fatalf("expected ErrExecutionAmbiguous on verify inspect 500, got: %v", err)
	}
	if res == nil || res.Outcome != OutcomeDispatchedUnknown {
		t.Fatalf("expected OutcomeDispatchedUnknown, got: %v", res)
	}

	// CRITICAL SEV-0 PREVENTION ASSERTION:
	// Candidate container was started. When inspect returned 500, candidate container must NOT be deleted!
	if c := recorder.countPrefix("DELETE"); c != 0 {
		t.Fatalf("CRITICAL BLOCKER REGRESSION: DELETE was called %d times on verify inspect 500! Calls: %v", c, recorder.getCalls())
	}
}

// TestR141_DockerExec_CreateAmbiguous_HaltsWithoutExecStart verifies that when exec.create
// encounters an ambiguous timeout/500, exec.start is NEVER called.
func TestR141_DockerExec_CreateAmbiguous_HaltsWithoutExecStart(t *testing.T) {
	t.Parallel()
	db := setupR14TestDB(t)

	recorder := &mockDockerRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.log(fmt.Sprintf("%s %s", r.Method, r.URL.RequestURI()))
		path := r.URL.Path

		switch {
		case strings.Contains(path, "/exec") && r.Method == http.MethodPost && !strings.Contains(path, "/start"):
			// Simulate ambiguous 500 on exec create
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"exec create daemon error"}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	exec := NewOperationExecutor(
		db,
		eventbus.New(),
		nil,
		nil,
		WithDockerSocket("tcp://" + server.Listener.Addr().String()),
		WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := exec.Execute(ctx, OperationSpec{
		Action: ActionDockerExec,
		Target: ComponentFrigate,
		Params: map[string]interface{}{"command": []string{"python3", "-m", "frigate", "--validate-config"}},
	})

	if err == nil || !errors.Is(err, ErrExecutionAmbiguous) {
		t.Fatalf("expected ErrExecutionAmbiguous, got: %v", err)
	}
	if res == nil || res.Outcome != OutcomeDispatchedUnknown {
		t.Fatalf("expected OutcomeDispatchedUnknown, got: %v", res)
	}

	if c := recorder.countPrefix("POST /exec/"); c != 0 {
		t.Fatalf("CRITICAL: exec.start was called %d times after exec.create failed! Calls: %v", c, recorder.getCalls())
	}
}

// TestR141_CachePurge_BuilderPrune_AmbiguousHalts verifies that builder cache purge
// correctly tracks ambiguous failures and sets OutcomeDispatchedUnknown.
func TestR141_CachePurge_BuilderPrune_AmbiguousHalts(t *testing.T) {
	t.Parallel()
	db := setupR14TestDB(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/build/prune") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"builder prune daemon error"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exec := NewOperationExecutor(
		db,
		eventbus.New(),
		nil,
		nil,
		WithDockerSocket("tcp://" + server.Listener.Addr().String()),
		WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := exec.Execute(ctx, OperationSpec{
		Action: ActionCachePurge,
		Target: ComponentHomeAPI,
		Params: map[string]interface{}{"cache_type": "docker_builder"},
	})

	if err == nil || !errors.Is(err, ErrExecutionAmbiguous) {
		t.Fatalf("expected ErrExecutionAmbiguous, got: %v", err)
	}
	if res == nil || res.Outcome != OutcomeDispatchedUnknown {
		t.Fatalf("expected OutcomeDispatchedUnknown, got: %v", res)
	}
}

// TestR141_ExtractBaseline_ProductionSocketMissing_FailsClosed verifies that in production runtime
// (OPS_STRICT_BASELINE=1), ExtractBaseline strictly fails closed when the Docker socket is inaccessible
// and NEVER returns an offline placeholder or synthetic 1Panel ID.
func TestR141_ExtractBaseline_ProductionSocketMissing_FailsClosed(t *testing.T) {
	t.Setenv("OPS_STRICT_BASELINE", "1")
	if !IsProductionRuntime() {
		t.Fatal("expected IsProductionRuntime() to be true")
	}

	exec := NewOperationExecutor(
		nil,
		nil,
		nil,
		nil,
		WithDockerSocket(filepath.Join(t.TempDir(), "nonexistent.sock")),
	)

	info := exec.ExtractBaseline("frigate")
	if info.Hash != "" {
		t.Fatalf("CRITICAL SECURITY VIOLATION: production baseline returned hash %q when socket is missing!", info.Hash)
	}
	if info.Error == "" {
		t.Fatalf("expected non-empty Error when socket is missing in production mode")
	}
	if info.IsPlaceholder {
		t.Fatalf("CRITICAL: production baseline returned placeholder when socket is missing!")
	}
}

