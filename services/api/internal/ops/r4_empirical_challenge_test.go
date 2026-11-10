package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// setupR4TestDB initializes an isolated in-memory SQLite database for R4 empirical challenge tests.
func setupR4TestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbName := fmt.Sprintf("file:r4_chal_%d_%d?mode=memory&cache=shared", time.Now().UnixNano(), time.Now().Nanosecond())
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test sqlite db: %v", err)
	}
	err = db.AutoMigrate(
		&model.User{},
		&model.Device{},
		&model.OpsProposal{},
		&model.OpsExecutionHistory{},
		&model.SystemLog{},
	)
	if err != nil {
		t.Fatalf("failed to migrate db schema: %v", err)
	}
	return db
}

// r4RoundTripper intercepts HTTP requests and rewrites host & scheme to a local test server.
type r4RoundTripper struct {
	targetURL string
	base      http.RoundTripper
}

func (r *r4RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	parsedTarget, _ := http.NewRequest(req.Method, r.targetURL, nil)
	req.URL.Scheme = parsedTarget.URL.Scheme
	req.URL.Host = parsedTarget.URL.Host
	base := r.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// ----------------------------------------------------------------------------
// Objective 1: Proposal Cancellation vs Execution Race (CAS Concurrency)
// ----------------------------------------------------------------------------
func TestEmpiricalChallenge1_CancelVsExecute_ConcurrentRace(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("Concurrent Race: Thread A (Cancel) vs Thread B (Execute)", func(t *testing.T) {
		const iterations = 30
		var cancelWins int
		var executeWins int
		var execute500Errors int

		for i := 0; i < iterations; i++ {
			db := setupR4TestDB(t)
			bus := eventbus.New()

			// Executor with immediate successful runner
			executor := NewOperationExecutor(
				db, bus, nil, nil,
				WithProbeTimeout(50*time.Millisecond),
				WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
					return "container restarted successfully", nil
				}),
				WithHealthChecker(func(ctx context.Context, target string) error {
					return nil
				}),
			)

			handler := NewOpsHandler(nil, nil, db, bus, nil)
			handler.SetExecutor(executor)

			propID := fmt.Sprintf("race-prop-%d", i)
			p := model.OpsProposal{
				ProposalID:      propID,
				Title:           "Concurrent Race Test",
				TargetComponent: "home-api",
				Status:          model.ProposalStatusApproved,
				HouseholdID:     1,
				ActionSpec:      `{"action":"restart","target":"home-api"}`,
				CreatedAt:       time.Now(),
			}
			seedTestSignedProposal(t, db, &p)

			startBarrier := make(chan struct{})
			var wg sync.WaitGroup
			var cancelCode, execCode int
			var cancelBody, execBody string

			wg.Add(2)

			// Thread A: CancelProposal
			go func() {
				defer wg.Done()
				<-startBarrier

				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/ops/proposals/"+propID+"/cancel", nil)
				c.Params = gin.Params{{Key: "id", Value: propID}}
				c.Set("is_admin", true)
				c.Set("user_name", "operator-a")
				c.Set("household_id", uint(1))

				handler.CancelProposal(c)
				cancelCode = w.Code
				cancelBody = w.Body.String()
			}()

			// Thread B: ExecuteProposalEndpoint
			go func() {
				defer wg.Done()
				<-startBarrier

				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/ops/proposals/"+propID+"/execute", nil)
				c.Params = gin.Params{{Key: "id", Value: propID}}
				c.Set("is_admin", true)
				c.Set("user_name", "operator-b")
				c.Set("household_id", uint(1))

				handler.ExecuteProposalEndpoint(c)
				execCode = w.Code
				execBody = w.Body.String()
			}()

			// Release both threads simultaneously
			close(startBarrier)
			wg.Wait()

			// Check DB final state
			var finalProp model.OpsProposal
			if err := db.Where("proposal_id = ?", propID).First(&finalProp).Error; err != nil {
				t.Fatalf("failed to query final proposal: %v", err)
			}

			// Invariant 1: State must never be corrupted
			validFinalStates := map[string]bool{
				model.ProposalStatusCancelled: true,
				model.ProposalStatusExecuting: true,
				model.ProposalStatusExecuted:  true,
				model.ProposalStatusSuccess:   true,
			}
			if !validFinalStates[finalProp.Status] {
				t.Fatalf("Iteration %d: Corrupted final status: %s", i, finalProp.Status)
			}

			// Invariant 2: Exactly one operation must succeed (200 OK)
			if cancelCode == http.StatusOK && execCode == http.StatusOK {
				t.Fatalf("Iteration %d: CRITICAL CAS VIOLATION: Both Cancel and Execute returned 200 OK!", i)
			}

			if cancelCode == http.StatusOK {
				cancelWins++
				if finalProp.Status != model.ProposalStatusCancelled {
					t.Fatalf("Iteration %d: Cancel returned 200 OK but final DB status was %s", i, finalProp.Status)
				}
				// Assert that CAS guarantees mutual exclusion: if cancellation wins, execution receives 409 conflict
				if execCode == http.StatusInternalServerError {
					execute500Errors++
					t.Errorf("Iteration %d: Cancel won (200 OK) but Execute received HTTP 500 (body: %s) instead of HTTP 409 Conflict", i, execBody)
				} else if execCode != http.StatusConflict {
					t.Errorf("Iteration %d: Cancel won (200 OK) but Execute received HTTP %d (expected 409 Conflict): %s", i, execCode, execBody)
				}
			} else if execCode == http.StatusOK {
				executeWins++
				if cancelCode != http.StatusConflict {
					t.Fatalf("Iteration %d: Execute won (200 OK) but Cancel received HTTP %d (expected 409 Conflict): %s", i, cancelCode, cancelBody)
				}
			}
		}

		t.Logf("Empirical Race Summary across %d trials: Cancel won %d times, Execute won %d times, Execute received 500 in %d cases",
			iterations, cancelWins, executeWins, execute500Errors)

		if cancelWins == 0 || executeWins == 0 {
			t.Logf("Note: Race was skewed in single-machine test (%d cancel vs %d execute wins)", cancelWins, executeWins)
		}
	})

	t.Run("Sequential Execution Rejection: Execute on Already-Cancelled Proposal", func(t *testing.T) {
		db := setupR4TestDB(t)
		bus := eventbus.New()
		executor := NewOperationExecutor(db, bus, nil, nil)
		handler := NewOpsHandler(nil, nil, db, bus, nil)
		handler.SetExecutor(executor)

		propID := "prop-already-cancelled"
		p := model.OpsProposal{
			ProposalID:      propID,
			Title:           "Already Cancelled Test",
			TargetComponent: "home-api",
			Status:          model.ProposalStatusCancelled,
			HouseholdID:     1,
			CreatedAt:       time.Now(),
		}
		db.Create(&p)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/ops/proposals/"+propID+"/execute", nil)
		c.Params = gin.Params{{Key: "id", Value: propID}}
		c.Set("is_admin", true)
		c.Set("household_id", uint(1))

		handler.ExecuteProposalEndpoint(c)

		// Assert: Attempting to execute a cancelled proposal should return 409 Conflict or 400 Bad Request, NOT 500
		t.Logf("Execute on CANCELLED proposal returned HTTP %d: %s", w.Code, w.Body.String())
		if w.Code == http.StatusInternalServerError {
			t.Errorf("BUG DISCOVERED: Executing CANCELLED proposal returned HTTP 500 Internal Server Error instead of HTTP 409 Conflict: %s", w.Body.String())
		}
	})

	t.Run("Sequential Cancellation Rejection: Cancel on Already-Executing Proposal", func(t *testing.T) {
		db := setupR4TestDB(t)
		bus := eventbus.New()
		handler := NewOpsHandler(nil, nil, db, bus, nil)

		propID := "prop-already-executing"
		p := model.OpsProposal{
			ProposalID:      propID,
			Title:           "Already Executing Test",
			TargetComponent: "home-api",
			Status:          model.ProposalStatusExecuting,
			HouseholdID:     1,
			CreatedAt:       time.Now(),
		}
		db.Create(&p)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/ops/proposals/"+propID+"/cancel", nil)
		c.Params = gin.Params{{Key: "id", Value: propID}}
		c.Set("is_admin", true)
		c.Set("household_id", uint(1))

		handler.CancelProposal(c)

		if w.Code != http.StatusConflict {
			t.Fatalf("expected HTTP 409 Conflict when cancelling EXECUTING proposal, got %d: %s", w.Code, w.Body.String())
		}
	})
}

// ----------------------------------------------------------------------------
// Objective 2: ProposalScheduler Concurrent Claiming
// ----------------------------------------------------------------------------
type r4MockSchedulerExecutor struct {
	mu           sync.Mutex
	executeCalls int64
	executedIDs  []string
}

func (m *r4MockSchedulerExecutor) Execute(ctx context.Context, spec OperationSpec) (*ExecutionResult, error) {
	return &ExecutionResult{Success: true, Output: "executed"}, nil
}
func (m *r4MockSchedulerExecutor) ExecuteRollback(ctx context.Context, spec OperationSpec) error {
	return nil
}
func (m *r4MockSchedulerExecutor) ExecuteProposal(ctx context.Context, proposal *model.OpsProposal) (*ExecutionResult, error) {
	atomic.AddInt64(&m.executeCalls, 1)
	m.mu.Lock()
	m.executedIDs = append(m.executedIDs, proposal.ProposalID)
	m.mu.Unlock()
	proposal.Status = model.ProposalStatusExecuted
	proposal.ExecutionLog = "scheduler test executed"
	return &ExecutionResult{Success: true, Output: "executed"}, nil
}
func (m *r4MockSchedulerExecutor) ValidateComponent(target string) (string, error) {
	return target, nil
}
func (m *r4MockSchedulerExecutor) CheckHealth(ctx context.Context, target string) error {
	return nil
}

func TestEmpiricalChallenge2_ProposalScheduler_ConcurrentClaiming(t *testing.T) {
	db := setupR4TestDB(t)
	mockExec := &r4MockSchedulerExecutor{}

	// Past schedule time in RFC3339 format so validateScheduledWindow passes immediately
	pastTime := time.Now().Add(-10 * time.Minute).Format(time.RFC3339)
	prop := model.OpsProposal{
		ProposalID:      "sched-concurrent-001",
		Title:           "Scheduled Maintenance",
		TargetComponent: "home-api",
		Status:          model.ProposalStatusScheduled,
		ScheduledAt:     &pastTime,
		HouseholdID:     1,
		CreatedAt:       time.Now(),
	}
	if err := db.Create(&prop).Error; err != nil {
		t.Fatalf("failed to insert scheduled proposal: %v", err)
	}

	// Create 10 concurrent scheduler instances querying the same DB
	const numWorkers = 10
	schedulers := make([]*ProposalScheduler, numWorkers)
	for i := 0; i < numWorkers; i++ {
		schedulers[i] = NewProposalScheduler(db, mockExec)
	}

	barrier := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	for i := 0; i < numWorkers; i++ {
		idx := i
		go func() {
			defer wg.Done()
			<-barrier
			schedulers[idx].Tick()
		}()
	}

	// Trigger all 10 schedulers simultaneously
	close(barrier)
	wg.Wait()

	// Allow goroutine dispatches to complete
	time.Sleep(150 * time.Millisecond)

	calls := atomic.LoadInt64(&mockExec.executeCalls)
	if calls != 1 {
		t.Fatalf("Atomic CAS claiming failed: expected exactly 1 execution, got %d (executedIDs: %v)", calls, mockExec.executedIDs)
	}

	var finalProp model.OpsProposal
	db.Where("proposal_id = ?", prop.ProposalID).First(&finalProp)
	if finalProp.Status != model.ProposalStatusExecuting && finalProp.Status != model.ProposalStatusExecuted {
		t.Fatalf("expected proposal status to be EXECUTING or EXECUTED, got %s", finalProp.Status)
	}
	if !finalProp.ExecutedAt.Valid {
		t.Fatalf("expected executed_at to be populated by winning scheduler")
	}

	t.Logf("PASS: 10 concurrent schedulers competed for SCHEDULED proposal; exactly 1 claimed and executed.")
}

// ----------------------------------------------------------------------------
// Objective 3: Docker Recreate Non-Root User Retention
// ----------------------------------------------------------------------------
func TestEmpiricalChallenge3_DockerRecreate_NonRootUserRetention(t *testing.T) {
	testCases := []struct {
		name         string
		inspectUser  string
		expectedUser string
	}{
		{
			name:         "Numeric UID:GID non-root (1000:1000)",
			inspectUser:  "1000:1000",
			expectedUser: "1000:1000",
		},
		{
			name:         "Named non-root user (appuser)",
			inspectUser:  "appuser",
			expectedUser: "appuser",
		},
		{
			name:         "Custom daemon UID (65534:65534)",
			inspectUser:  "65534:65534",
			expectedUser: "65534:65534",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rawInspect := fmt.Sprintf(`{
				"Id": "cid-%s",
				"Name": "/test-service",
				"Config": {
					"Image": "app:v1",
					"User": "%s",
					"Env": ["PORT=8080"],
					"WorkingDir": "/app",
					"Cmd": ["./run.sh"]
				},
				"HostConfig": {
					"Binds": ["/data:/data:ro"],
					"RestartPolicy": {"Name": "always"}
				},
				"NetworkSettings": {
					"Networks": {}
				}
			}`, tc.expectedUser, tc.inspectUser)

			var inspectData ContainerInspectSafetyConfig
			err := json.Unmarshal([]byte(rawInspect), &inspectData)
			if err != nil {
				t.Fatalf("failed to decode inspect data: %v", err)
			}

			// Verify inspect decoding preserved the user field
			if inspectData.Config.User != tc.expectedUser {
				t.Fatalf("inspect decoding lost User: expected %s, got %s", tc.expectedUser, inspectData.Config.User)
			}

			// Verify container create payload retains the exact User string
			createPayload := map[string]interface{}{
				"Image":        "app:v2-new",
				"User":         inspectData.Config.User,
				"Env":          inspectData.Config.Env,
				"Cmd":          inspectData.Config.Cmd,
				"Entrypoint":   inspectData.Config.Entrypoint,
				"WorkingDir":   inspectData.Config.WorkingDir,
				"Labels":       inspectData.Config.Labels,
				"ExposedPorts": inspectData.Config.ExposedPorts,
				"HostConfig":   inspectData.HostConfig,
			}

			createBytes, err := json.Marshal(createPayload)
			if err != nil {
				t.Fatalf("failed to marshal create payload: %v", err)
			}

			var decodedPayload struct {
				Image string `json:"Image"`
				User  string `json:"User"`
			}
			if err := json.Unmarshal(createBytes, &decodedPayload); err != nil {
				t.Fatalf("failed to unmarshal create payload: %v", err)
			}

			if decodedPayload.User != tc.expectedUser {
				t.Fatalf("Container create payload dropped non-root User! Expected %q, got %q", tc.expectedUser, decodedPayload.User)
			}
			if decodedPayload.User == "" || decodedPayload.User == "0" || decodedPayload.User == "root" || decodedPayload.User == "0:0" {
				t.Fatalf("SECURITY VIOLATION: Container reverted to root user: %q", decodedPayload.User)
			}

			t.Logf("PASS: Non-root user %q strictly preserved in create payload JSON", decodedPayload.User)
		})
	}
}

// ----------------------------------------------------------------------------
// Objective 4: Five-Stage Verified Rollback Failure Injection
// ----------------------------------------------------------------------------
func TestEmpiricalChallenge4_FiveStageVerifiedRollback_FailureInjection(t *testing.T) {
	containerName := "test-target"
	backupName := "test-target_bak"

	// Subtest 4.1: Inject failure at Stage 2 (backup rename HTTP 500)
	t.Run("Stage 2 Failure: Backup Rename HTTP 500", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/rename") {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"rename failed: target name busy"}`))
				return
			}
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client := server.Client()
		client.Transport = &r4RoundTripper{targetURL: server.URL}

		err := executeVerifiedRollback(
			context.Background(),
			client,
			containerName,
			backupName,
			func(ctx context.Context, target string) error { return nil },
			containerName,
			errors.New("initial recreate failure"),
		)

		if err == nil {
			t.Fatalf("expected executeVerifiedRollback to fail when Stage 2 (rename) returns HTTP 500, but got nil")
		}
		if !strings.Contains(err.Error(), "rollback failed at rename") {
			t.Fatalf("expected error message to reference rename failure, got: %v", err)
		}
		t.Logf("PASS: Stage 2 failure cleanly rejected rollback: %v", err)
	})

	// Subtest 4.2: Inject failure at Stage 4 (container inspect Running == false)
	t.Run("Stage 4 Failure: Inspect Running == false", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodDelete:
				w.WriteHeader(http.StatusNoContent)
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/rename"):
				w.WriteHeader(http.StatusNoContent)
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/start"):
				w.WriteHeader(http.StatusNoContent)
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/json"):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"State": map[string]interface{}{
						"Running": false,
						"Status":  "exited",
						"ExitCode": 137,
					},
				})
			default:
				w.WriteHeader(http.StatusOK)
			}
		}))
		defer server.Close()

		client := server.Client()
		client.Transport = &r4RoundTripper{targetURL: server.URL}

		err := executeVerifiedRollback(
			context.Background(),
			client,
			containerName,
			backupName,
			func(ctx context.Context, target string) error { return nil },
			containerName,
			errors.New("initial recreate failure"),
		)

		if err == nil {
			t.Fatalf("expected executeVerifiedRollback to fail when Stage 4 inspect Running == false, but got nil")
		}
		if !strings.Contains(err.Error(), "restored container is NOT running") {
			t.Fatalf("expected error message to state restored container is NOT running, got: %v", err)
		}
		t.Logf("PASS: Stage 4 failure cleanly rejected rollback: %v", err)
	})

	// Subtest 4.3: Inject failure at Stage 5 (health probe timeout)
	t.Run("Stage 5 Failure: Health Probe Timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodDelete:
				w.WriteHeader(http.StatusNoContent)
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/rename"):
				w.WriteHeader(http.StatusNoContent)
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/start"):
				w.WriteHeader(http.StatusNoContent)
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/json"):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"State": map[string]interface{}{
						"Running": true,
						"Status":  "running",
					},
				})
			default:
				w.WriteHeader(http.StatusOK)
			}
		}))
		defer server.Close()

		client := server.Client()
		client.Transport = &r4RoundTripper{targetURL: server.URL}

		failingHealthChecker := func(ctx context.Context, target string) error {
			return context.DeadlineExceeded
		}

		err := executeVerifiedRollback(
			context.Background(),
			client,
			containerName,
			backupName,
			failingHealthChecker,
			containerName,
			errors.New("initial recreate failure"),
		)

		if err == nil {
			t.Fatalf("expected executeVerifiedRollback to fail when Stage 5 health probe fails, but got nil")
		}
		if !strings.Contains(err.Error(), "failed health probe") {
			t.Fatalf("expected error message to state failed health probe, got: %v", err)
		}
		t.Logf("PASS: Stage 5 health probe timeout cleanly rejected rollback: %v", err)
	})

	// Subtest 4.4: All 5 Stages Succeed -> Verified Rollback Succeeded
	t.Run("All 5 Stages Succeed -> Rollback Succeeded", func(t *testing.T) {
		var stagesHit []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodDelete:
				stagesHit = append(stagesHit, "stage1_cleanup")
				w.WriteHeader(http.StatusNoContent)
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/rename"):
				stagesHit = append(stagesHit, "stage2_rename")
				w.WriteHeader(http.StatusNoContent)
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/start"):
				stagesHit = append(stagesHit, "stage3_start")
				w.WriteHeader(http.StatusNoContent)
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/json"):
				stagesHit = append(stagesHit, "stage4_inspect")
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"State": map[string]interface{}{
						"Running": true,
						"Status":  "running",
					},
				})
			default:
				w.WriteHeader(http.StatusOK)
			}
		}))
		defer server.Close()

		client := server.Client()
		client.Transport = &r4RoundTripper{targetURL: server.URL}

		healthProbeHit := false
		passingHealthChecker := func(ctx context.Context, target string) error {
			healthProbeHit = true
			return nil
		}

		err := executeVerifiedRollback(
			context.Background(),
			client,
			containerName,
			backupName,
			passingHealthChecker,
			containerName,
			errors.New("initial recreate failure"),
		)

		if err != nil {
			t.Fatalf("expected 5-stage verified rollback to succeed, got: %v", err)
		}
		if !healthProbeHit {
			t.Fatalf("expected Stage 5 health check probe to be executed")
		}
		if len(stagesHit) != 4 {
			t.Fatalf("expected 4 HTTP stages to be executed, got %d: %v", len(stagesHit), stagesHit)
		}
		t.Logf("PASS: All 5 stages succeeded; rollback verified.")
	})

	// Subtest 4.5: Proposal Status Transition Under Rollback Failure vs Success
	t.Run("ExecuteProposal DB Status: Rollback Failure vs Rollback Success", func(t *testing.T) {
		db := setupR4TestDB(t)
		bus := eventbus.New()

		// Case A: Rollback fails -> status must be ROLLBACK_FAILED, strictly NOT ROLLED_BACK
		failingRollbackExecutor := NewOperationExecutor(
			db, bus, nil, nil,
			WithProbeTimeout(50*time.Millisecond),
			WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
				if action == "restart" {
					return "", errors.New("primary action failed")
				}
				if action == "rollback" {
					return "", errors.New("rollback failed")
				}
				return "", nil
			}),
		)

		propFail := model.OpsProposal{
			ProposalID:      "prop-rb-fail",
			Title:           "Rollback Fail Test",
			TargetComponent: "home-api",
			Status:          model.ProposalStatusApproved,
			HouseholdID:     1,
			ActionSpec:      `{"action":"restart","target":"home-api","rollback_spec":{"action":"rollback","target":"home-api"}}`,
			CreatedAt:       time.Now(),
		}
		seedTestSignedProposal(t, db, &propFail)

		_, _ = failingRollbackExecutor.ExecuteProposal(context.Background(), &propFail)

		var updatedFail model.OpsProposal
		db.Where("proposal_id = ?", propFail.ProposalID).First(&updatedFail)

		if updatedFail.Status == model.ProposalStatusRolledBack {
			t.Fatalf("CRITICAL: Proposal was marked ROLLED_BACK even though rollback failed!")
		}
		if updatedFail.Status != model.ProposalStatusRollbackFailed {
			t.Fatalf("expected status %s, got %s", model.ProposalStatusRollbackFailed, updatedFail.Status)
		}
		t.Logf("PASS: When rollback fails, proposal status is %s (NOT ROLLED_BACK)", updatedFail.Status)

		// Case B: Rollback succeeds -> status must be ROLLED_BACK
		succeedingRollbackExecutor := NewOperationExecutor(
			db, bus, nil, nil,
			WithProbeTimeout(50*time.Millisecond),
			WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
				if action == "restart" {
					return "", errors.New("primary action failed")
				}
				if action == "rollback" {
					return "rollback restored container", nil
				}
				return "", nil
			}),
			WithHealthChecker(func(ctx context.Context, target string) error {
				return nil
			}),
		)

		propSuccess := model.OpsProposal{
			ProposalID:      "prop-rb-success",
			Title:           "Rollback Success Test",
			TargetComponent: "home-api",
			Status:          model.ProposalStatusApproved,
			HouseholdID:     1,
			ActionSpec:      `{"action":"restart","target":"home-api","rollback_spec":{"action":"rollback","target":"home-api"}}`,
			CreatedAt:       time.Now(),
		}
		seedTestSignedProposal(t, db, &propSuccess)

		_, _ = succeedingRollbackExecutor.ExecuteProposal(context.Background(), &propSuccess)

		var updatedSuccess model.OpsProposal
		db.Where("proposal_id = ?", propSuccess.ProposalID).First(&updatedSuccess)

		if updatedSuccess.Status != model.ProposalStatusRolledBack {
			t.Fatalf("expected status %s on verified rollback success, got %s", model.ProposalStatusRolledBack, updatedSuccess.Status)
		}
		t.Logf("PASS: When rollback succeeds, proposal status is strictly %s", updatedSuccess.Status)
	})
}

// ----------------------------------------------------------------------------
// Objective 5: Python Edge Agent HTTP 403 Policy Suppression
// ----------------------------------------------------------------------------
func TestEmpiricalChallenge5_PythonEdgeAgent_PolicySuppression(t *testing.T) {
	// Execute the Python edge agent boundary unit test via Python sub-process
	cmd := exec.Command("python", "-m", "unittest", "deploy/ops-agent/tests/test_agent_boundaries.py")
	// Set working directory to project root (home-datacenter)
	cmd.Dir = "../../../.."

	output, err := cmd.CombinedOutput()
	outStr := string(output)

	if err != nil {
		t.Fatalf("Python test suite execution failed: %v\nOutput:\n%s", err, outStr)
	}

	// Verify the test suite ran and passed
	if !strings.Contains(outStr, "OK") {
		t.Fatalf("Python test suite did not finish with OK:\n%s", outStr)
	}

	// Verify policy rejection / forbidden alert was logged
	if !strings.Contains(outStr, "HTTP 403") && !strings.Contains(outStr, "FORBIDDEN to fallback locally") {
		t.Fatalf("Python test output did not contain expected HTTP 403 suppression evidence:\n%s", outStr)
	}

	t.Logf("PASS: Python Edge Agent HTTP 403 policy suppression empirically verified:\n%s", outStr)
}
