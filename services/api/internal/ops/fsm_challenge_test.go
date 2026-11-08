package ops

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
)

// TestChallenge_5ClassFSMClassification empirically stress-tests the 5 distinct
// event classes to verify that MANUAL_STOP, UPDATE, and EXIT_ZERO never trigger auto-restarts,
// while OOM and CRASH correctly request restarts.
func TestChallenge_5ClassFSMClassification(t *testing.T) {
	fsm := NewContainerRecoveryFSM(nil, nil, nil)
	ctx := context.Background()

	testCases := []struct {
		name           string
		event          ContainerEvent
		expectedClass  ContainerEventClass
		expectedAction RecoveryDecisionAction
		expectRestart  bool
	}{
		// Class 1: MANUAL_STOP
		{
			name: "Manual stop via IsManual flag",
			event: ContainerEvent{
				ContainerName: "home-web",
				Action:        "die",
				ExitCode:      137,
				IsManual:      true,
			},
			expectedClass:  EventManualStop,
			expectedAction: ActionIgnore,
			expectRestart:  false,
		},
		{
			name: "Manual stop via stop action string",
			event: ContainerEvent{
				ContainerName: "home-web",
				Action:        "stop",
				ExitCode:      0,
			},
			expectedClass:  EventManualStop,
			expectedAction: ActionIgnore,
			expectRestart:  false,
		},
		{
			name: "Case-insensitive STOP action",
			event: ContainerEvent{
				ContainerName: "home-web",
				Action:        "STOP",
				ExitCode:      143,
			},
			expectedClass:  EventManualStop,
			expectedAction: ActionIgnore,
			expectRestart:  false,
		},

		// Class 2: UPDATE
		{
			name: "Container image update via IsUpdate flag",
			event: ContainerEvent{
				ContainerName: "home-api",
				Action:        "die",
				ExitCode:      0,
				IsUpdate:      true,
			},
			expectedClass:  EventUpdate,
			expectedAction: ActionIgnore,
			expectRestart:  false,
		},
		{
			name: "Container update via update action string",
			event: ContainerEvent{
				ContainerName: "home-api",
				Action:        "update",
				ExitCode:      1,
			},
			expectedClass:  EventUpdate,
			expectedAction: ActionIgnore,
			expectRestart:  false,
		},

		// Class 3: EXIT_ZERO
		{
			name: "Clean task termination with exit code 0",
			event: ContainerEvent{
				ContainerName: "home-vision",
				Action:        "die",
				ExitCode:      0,
				OOMKilled:     false,
			},
			expectedClass:  EventExitZero,
			expectedAction: ActionIgnore,
			expectRestart:  false,
		},
		{
			name: "Clean exit with empty action and code 0",
			event: ContainerEvent{
				ContainerName: "home-vision",
				Action:        "",
				ExitCode:      0,
			},
			expectedClass:  EventExitZero,
			expectedAction: ActionIgnore,
			expectRestart:  false,
		},

		// Class 4: OOM
		{
			name: "OOM explicitly flagged with OOMKilled",
			event: ContainerEvent{
				ContainerName: "home-api",
				Action:        "die",
				ExitCode:      137,
				OOMKilled:     true,
			},
			expectedClass:  EventOOM,
			expectedAction: ActionRestart,
			expectRestart:  true,
		},
		{
			name: "OOM via action oom",
			event: ContainerEvent{
				ContainerName: "home-api",
				Action:        "oom",
				ExitCode:      137,
			},
			expectedClass:  EventOOM,
			expectedAction: ActionRestart,
			expectRestart:  true,
		},
		{
			name: "OOM via exit code 137 and error attributes",
			event: ContainerEvent{
				ContainerName: "home-api",
				Action:        "die",
				ExitCode:      137,
				Attributes:    map[string]string{"error": "fatal: container killed by oom killer"},
			},
			expectedClass:  EventOOM,
			expectedAction: ActionRestart,
			expectRestart:  true,
		},

		// Class 5: CRASH
		{
			name: "Unexpected crash with exit code 1",
			event: ContainerEvent{
				ContainerName: "home-web",
				Action:        "die",
				ExitCode:      1,
			},
			expectedClass:  EventCrash,
			expectedAction: ActionRestart,
			expectRestart:  true,
		},
		{
			name: "Abnormal kill with exit code 143",
			event: ContainerEvent{
				ContainerName: "home-web",
				Action:        "kill",
				ExitCode:      143,
			},
			expectedClass:  EventCrash,
			expectedAction: ActionRestart,
			expectRestart:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Test direct classification function
			class := ClassifyContainerEvent(tc.event, false)
			if class != tc.expectedClass {
				t.Fatalf("ClassifyContainerEvent mismatch: got %s, want %s", class, tc.expectedClass)
			}

			// Test full FSM pipeline decision
			fsm.breaker.Reset(tc.event.ContainerName)
			decision, err := fsm.ProcessContainerEvent(ctx, tc.event)
			if err != nil && tc.expectRestart {
				t.Fatalf("unexpected error processing event: %v", err)
			}

			if decision.Action != tc.expectedAction {
				t.Fatalf("FSM decision action mismatch: got %s, want %s (reason: %s)",
					decision.Action, tc.expectedAction, decision.Reason)
			}

			if tc.expectRestart && decision.Action != ActionRestart {
				t.Fatalf("expected autonomous restart request, got %s", decision.Action)
			}
			if !tc.expectRestart && decision.Action == ActionRestart {
				t.Fatalf("prohibited auto-restart triggered for suppressed event class %s", class)
			}
		})
	}
}

// TestChallenge_CircuitBreaker_EdgeCases verifies:
// 1. 2 crashes in 10 minutes -> no trip (state CLOSED, auto-restart allowed)
// 2. 3 crashes in 10 minutes -> trip breaker (state OPEN, Tier 2 proposal, block auto-restarts)
// 3. Timestamps older than 10 minutes roll off properly and do not count toward trip threshold.
func TestChallenge_CircuitBreaker_EdgeCases(t *testing.T) {
	fsm := NewContainerRecoveryFSM(nil, nil, nil)
	ctx := context.Background()
	container := "home-web"

	tripCount := 0
	fsm.breaker.SetOnTrip(func(c string, count int) {
		tripCount++
	})

	crashEvent := ContainerEvent{
		ContainerName: container,
		Action:        "die",
		ExitCode:      1,
	}

	// 1. First Crash -> Should NOT trip (crashes: 1)
	d1, err1 := fsm.ProcessContainerEvent(ctx, crashEvent)
	if err1 != nil {
		t.Fatalf("crash 1 unexpected error: %v", err1)
	}
	if d1.Action != ActionRestart {
		t.Fatalf("crash 1 expected ActionRestart, got %s", d1.Action)
	}
	if fsm.breaker.GetState(container) != CircuitClosed {
		t.Fatalf("expected breaker state CLOSED after 1 crash, got %s", fsm.breaker.GetState(container))
	}

	// 2. Second Crash -> Should NOT trip (crashes: 2)
	d2, err2 := fsm.ProcessContainerEvent(ctx, crashEvent)
	if err2 != nil {
		t.Fatalf("crash 2 unexpected error: %v", err2)
	}
	if d2.Action != ActionRestart {
		t.Fatalf("crash 2 expected ActionRestart, got %s", d2.Action)
	}
	if fsm.breaker.GetState(container) != CircuitClosed {
		t.Fatalf("expected breaker state CLOSED after 2 crashes, got %s", fsm.breaker.GetState(container))
	}
	if tripCount != 0 {
		t.Fatalf("expected 0 trips after 2 crashes, got %d", tripCount)
	}

	// 3. Third Crash -> Must TRIP breaker (crashes: 3 in window)
	d3, err3 := fsm.ProcessContainerEvent(ctx, crashEvent)
	if !errors.Is(err3, ErrCircuitBreakerOpen) {
		t.Fatalf("crash 3 expected ErrCircuitBreakerOpen, got: %v", err3)
	}
	if d3.Action != ActionTripped {
		t.Fatalf("crash 3 expected ActionTripped, got %s", d3.Action)
	}
	if fsm.breaker.GetState(container) != CircuitOpen {
		t.Fatalf("expected breaker state OPEN after 3 crashes, got %s", fsm.breaker.GetState(container))
	}

	// Give async trip callback goroutine a moment to increment counter
	time.Sleep(50 * time.Millisecond)
	if tripCount != 1 {
		t.Fatalf("expected exactly 1 trip invocation, got %d", tripCount)
	}

	// 4. Fourth Crash -> Must be BLOCKED (circuit is already OPEN)
	d4, err4 := fsm.ProcessContainerEvent(ctx, crashEvent)
	if !errors.Is(err4, ErrCircuitBreakerOpen) {
		t.Fatalf("crash 4 expected ErrCircuitBreakerOpen, got: %v", err4)
	}
	if d4.Action != ActionBlocked {
		t.Fatalf("crash 4 expected ActionBlocked, got %s", d4.Action)
	}

	// 5. Test Timestamp Sliding Window Rolloff
	fsm.breaker.Reset(container)
	now := time.Now()
	// Manually inject 2 old crashes from 12 minutes ago (outside 10m window)
	canonical := ResolveTopologyComponent(container)
	fsm.breaker.mu.Lock()
	fsm.breaker.crashTimestamps[canonical] = []time.Time{
		now.Add(-15 * time.Minute),
		now.Add(-12 * time.Minute),
	}
	fsm.breaker.mu.Unlock()

	// Verify GetCrashCount prunes old timestamps
	if count := fsm.breaker.GetCrashCount(container); count != 0 {
		t.Fatalf("expected 0 crashes within 10m window for expired timestamps, got %d", count)
	}

	// Record a new crash now -> should only have 1 active crash in window, NO TRIP
	state, tripped := fsm.breaker.RecordCrash(container)
	if tripped {
		t.Fatalf("breaker tripped unexpectedly on expired timestamps")
	}
	if state != CircuitClosed {
		t.Fatalf("expected state CLOSED after rolloff, got %s", state)
	}
	if count := fsm.breaker.GetCrashCount(container); count != 1 {
		t.Fatalf("expected crash count 1 after 1 fresh crash, got %d", count)
	}
}

// TestChallenge_CircuitBreaker_ConcurrentStress simulates 50 concurrent goroutines
// recording crashes and reading breaker state to detect race conditions under -race.
func TestChallenge_CircuitBreaker_ConcurrentStress(t *testing.T) {
	cb := NewContainerCircuitBreaker()
	container := "home-web"

	const goroutines = 50
	const iterations = 20

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Writers: concurrently recording crashes
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				cb.RecordCrash(container)
			}
		}()
	}

	// Readers: concurrently querying state and crash counts
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_ = cb.GetState(container)
				_ = cb.GetCrashCount(container)
				_, _ = cb.CanAttemptRecovery(container)
			}
		}()
	}

	wg.Wait()

	// Verify terminal state is consistent and not corrupted
	state := cb.GetState(container)
	if state != CircuitOpen {
		t.Fatalf("expected circuit OPEN after 1000 concurrent crashes, got %s", state)
	}
}

// TestChallenge_DependencyTopology_LevelEnforcement verifies:
// 1. Level 1 container (home-api) restart is rejected when Level 0 dependency (mosquitto) is unhealthy.
// 2. Level 2 container (home-web) restart is rejected when Level 1 or Level 0 dependency is unhealthy.
// 3. Independent / Level 0 containers restart without dependency constraints.
func TestChallenge_DependencyTopology_LevelEnforcement(t *testing.T) {
	fsm := NewContainerRecoveryFSM(nil, nil, nil)
	ctx := context.Background()

	// Mock component health map
	healthMap := map[string]bool{
		"mosquitto": true,
		"frigate":   true,
		"app.db":    true,
		"home-api":  true,
		"home-web":  true,
	}

	var mu sync.Mutex
	fsm.topology.SetHealthChecker(func(ctx context.Context, comp string) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		canonical := ResolveTopologyComponent(comp)
		h, ok := healthMap[canonical]
		if !ok {
			return false, fmt.Errorf("unknown component: %s", comp)
		}
		return h, nil
	})

	crashEventAPI := ContainerEvent{
		ContainerName: "home-api", // Level 1
		Action:        "die",
		ExitCode:      1,
	}

	// 1. All dependencies healthy -> home-api should be allowed to restart
	d1, err1 := fsm.ProcessContainerEvent(ctx, crashEventAPI)
	if err1 != nil {
		t.Fatalf("expected healthy pre-flight restart, got error: %v", err1)
	}
	if d1.Action != ActionRestart {
		t.Fatalf("expected ActionRestart for home-api, got: %s", d1.Action)
	}

	// 2. Set Level 0 dependency 'mosquitto' unhealthy -> home-api restart MUST be rejected
	fsm.breaker.Reset("home-api")
	mu.Lock()
	healthMap["mosquitto"] = false
	mu.Unlock()

	d2, err2 := fsm.ProcessContainerEvent(ctx, crashEventAPI)
	if err2 == nil {
		t.Fatalf("expected error when Level 0 dependency is unhealthy, got nil")
	}
	if d2.Action != ActionBlocked {
		t.Fatalf("expected ActionBlocked for home-api when mosquitto is down, got: %s", d2.Action)
	}

	// 3. Test Level 2 container (home-web): direct dep home-api is healthy, but transitive dep mosquitto is unhealthy
	mu.Lock()
	healthMap["home-api"] = true
	healthMap["mosquitto"] = false
	mu.Unlock()

	crashEventWeb := ContainerEvent{
		ContainerName: "home-web", // Level 2
		Action:        "die",
		ExitCode:      1,
	}
	fsm.breaker.Reset("home-web")
	d3, err3 := fsm.ProcessContainerEvent(ctx, crashEventWeb)
	if err3 == nil {
		t.Fatalf("expected error when transitive dependency mosquitto is unhealthy, got nil")
	}
	if d3.Action != ActionBlocked {
		t.Fatalf("expected ActionBlocked for home-web, got: %s", d3.Action)
	}

	// 4. Test Level 0 container restart (mosquitto itself): has no dependencies, should not be blocked by topology
	crashEventMosquitto := ContainerEvent{
		ContainerName: "mosquitto", // Level 0
		Action:        "die",
		ExitCode:      1,
	}
	fsm.breaker.Reset("mosquitto")
	d4, err4 := fsm.ProcessContainerEvent(ctx, crashEventMosquitto)
	if err4 != nil {
		t.Fatalf("expected Level 0 restart to succeed without upstream dependencies, got: %v", err4)
	}
	if d4.Action != ActionRestart {
		t.Fatalf("expected ActionRestart for mosquitto, got: %s", d4.Action)
	}
}

// TestChallenge_DependencyTopology_CyclicAndUnknownDependencies tests resilience against
// cyclic graph loops and unknown dependency targets.
func TestChallenge_DependencyTopology_CyclicAndUnknownDependencies(t *testing.T) {
	topo := NewTopologyEngine()
	ctx := context.Background()

	// Inject cyclic dependency graph: cycle-a -> cycle-b -> cycle-a
	topo.mu.Lock()
	topo.graph["cycle-a"] = []string{"cycle-b"}
	topo.graph["cycle-b"] = []string{"cycle-a"}
	topo.mu.Unlock()

	topo.SetHealthChecker(func(ctx context.Context, comp string) (bool, error) {
		return true, nil
	})

	// Must complete cleanly without stack overflow, infinite recursion, or panic
	done := make(chan error, 1)
	go func() {
		err := topo.PreflightDependencyCheck(ctx, "cycle-a")
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected cyclic dependency check to resolve without error when healthy, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("preflight check deadlocked or looped infinitely on cyclic graph!")
	}

	// Test unknown container target -> should return nil without panic
	errUnknown := topo.PreflightDependencyCheck(ctx, "nonexistent-container-xyz")
	if errUnknown != nil {
		t.Fatalf("expected unknown target to pass gracefully, got error: %v", errUnknown)
	}
}

// TestChallenge_MaintenanceLock_BlocksAllRemediations empirically stress-tests the
// system-wide maintenance lock to verify all autonomous actions are halted while held.
func TestChallenge_MaintenanceLock_BlocksAllRemediations(t *testing.T) {
	fsm := NewContainerRecoveryFSM(nil, nil, nil)
	ctx := context.Background()
	lock := fsm.GetMaintenanceLock()

	// 1. Acquire Maintenance Lock
	err := lock.Acquire("operator-alice", "Emergency disk upgrade", 10*time.Minute)
	if err != nil {
		t.Fatalf("failed to acquire maintenance lock: %v", err)
	}

	if !lock.IsLocked() {
		t.Fatalf("expected lock.IsLocked() == true after acquisition")
	}

	// Verify conflicting acquisition by another operator is rejected
	errConflict := lock.Acquire("operator-bob", "Conflicting operation", 10*time.Minute)
	if !errors.Is(errConflict, ErrMaintenanceLockActive) {
		t.Fatalf("expected ErrMaintenanceLockActive on duplicate acquisition, got: %v", errConflict)
	}

	// 2. Container crashes while lock is held -> Must be BLOCKED immediately
	crashEvent := ContainerEvent{
		ContainerName: "home-api",
		Action:        "die",
		ExitCode:      1,
	}

	decision, errDecision := fsm.ProcessContainerEvent(ctx, crashEvent)
	if !errors.Is(errDecision, ErrSystemInMaintenance) {
		t.Fatalf("expected ErrSystemInMaintenance while locked, got: %v", errDecision)
	}
	if decision.Action != ActionBlocked {
		t.Fatalf("expected ActionBlocked during maintenance mode, got: %s", decision.Action)
	}

	// 3. Release Lock -> Proactive recovery should resume
	errRelease := lock.Release("operator-alice")
	if errRelease != nil {
		t.Fatalf("failed to release maintenance lock: %v", errRelease)
	}
	if lock.IsLocked() {
		t.Fatalf("expected lock.IsLocked() == false after release")
	}

	decisionAfterRelease, errAfterRelease := fsm.ProcessContainerEvent(ctx, crashEvent)
	if errAfterRelease != nil {
		t.Fatalf("expected recovery after release, got error: %v", errAfterRelease)
	}
	if decisionAfterRelease.Action != ActionRestart {
		t.Fatalf("expected ActionRestart after maintenance lock release, got: %s", decisionAfterRelease.Action)
	}

	// 4. Test Lock Expiry: Lock acquired with expired TTL (negative duration)
	errExpired := lock.Acquire("operator-alice", "Quick probe", -1*time.Second)
	if errExpired != nil {
		t.Fatalf("failed to acquire probe lock: %v", errExpired)
	}
	// Expire immediately
	lock.mu.Lock()
	lock.ExpiresAt = time.Now().Add(-1 * time.Second)
	lock.mu.Unlock()

	if lock.IsLocked() {
		t.Fatalf("expected expired lock to report IsLocked() == false")
	}
}

func setupChallengeTestDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite test DB: %v", err)
	}
	if err := db.AutoMigrate(&model.OpsProposal{}); err != nil {
		t.Fatalf("failed to automigrate ops_proposals: %v", err)
	}
	return db
}

// TestChallenge_TripProposal_Creation verifies that when circuit trips,
// a Tier 2 OpsProposal is generated in the database.
func TestChallenge_TripProposal_Creation(t *testing.T) {
	db := setupChallengeTestDB(t)
	if db == nil {
		t.Skip("skipping proposal DB verification: nil db")
	}

	fsm := NewContainerRecoveryFSM(db, nil, nil)
	ctx := context.Background()
	container := "home-api"

	crashEvent := ContainerEvent{
		ContainerName: container,
		Action:        "die",
		ExitCode:      1,
	}

	// Trip circuit with 3 crashes
	for i := 0; i < 3; i++ {
		_, _ = fsm.ProcessContainerEvent(ctx, crashEvent)
	}

	// Wait for async proposal creation
	time.Sleep(100 * time.Millisecond)

	var prop model.OpsProposal
	errQuery := db.Where("target_component = ? AND status = ?", container, model.ProposalStatusAwaitingApproval).First(&prop).Error
	if errQuery != nil {
		t.Fatalf("expected Tier 2 OpsProposal created on breaker trip, got query err: %v", errQuery)
	}

	if prop.RiskLevel != model.RiskLevelHigh {
		t.Fatalf("expected RiskLevelHigh, got: %s", prop.RiskLevel)
	}
	if prop.TargetComponent != container {
		t.Fatalf("expected target component %s, got: %s", container, prop.TargetComponent)
	}
}
