package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
)

func setupRollbackTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbName := fmt.Sprintf("file:rb_test_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test sqlite: %v", err)
	}
	err = db.AutoMigrate(
		&model.OpsProposal{},
		&model.OpsExecutionHistory{},
		&model.SystemLog{},
	)
	if err != nil {
		t.Fatalf("failed to auto migrate: %v", err)
	}
	return db
}

// 1. ContainerInspectSafetyConfig preserves non-root user and host configs
func TestContainerInspectSafetyConfig_Preservation(t *testing.T) {
	rawInspectJSON := `{
		"Id": "test-container-id-12345",
		"Name": "/home-web",
		"Config": {
			"Image": "nginx:alpine",
			"User": "1001:1001",
			"Env": ["ENV=prod", "PORT=8080"],
			"Cmd": ["nginx", "-g", "daemon off;"],
			"Entrypoint": ["/docker-entrypoint.sh"],
			"WorkingDir": "/app",
			"Labels": {"tier": "frontend"},
			"ExposedPorts": {"8080/tcp": {}},
			"StopSignal": "SIGTERM"
		},
		"HostConfig": {
			"Binds": ["/data/web:/app/data:ro"],
			"PortBindings": {
				"8080/tcp": [{"HostIp": "0.0.0.0", "HostPort": "8080"}]
			},
			"RestartPolicy": {"Name": "unless-stopped", "MaximumRetryCount": 0},
			"CapAdd": ["NET_BIND_SERVICE"],
			"CapDrop": ["ALL"],
			"SecurityOpt": ["no-new-privileges:true"],
			"NetworkMode": "custom-bridge"
		},
		"NetworkSettings": {
			"Networks": {
				"custom-bridge": {
					"NetworkID": "net-123",
					"IPAddress": "172.20.0.5"
				}
			}
		}
	}`

	var safetyConfig ContainerInspectSafetyConfig
	err := json.Unmarshal([]byte(rawInspectJSON), &safetyConfig)
	if err != nil {
		t.Fatalf("failed to unmarshal inspect config: %v", err)
	}

	// Verify non-root user is preserved
	if safetyConfig.Config.User != "1001:1001" {
		t.Errorf("expected User '1001:1001', got '%s'", safetyConfig.Config.User)
	}
	if safetyConfig.Config.Image != "nginx:alpine" {
		t.Errorf("expected Image 'nginx:alpine', got '%s'", safetyConfig.Config.Image)
	}
	if len(safetyConfig.Config.Env) != 2 || safetyConfig.Config.Env[0] != "ENV=prod" {
		t.Errorf("expected Env preserved, got: %v", safetyConfig.Config.Env)
	}
	if len(safetyConfig.HostConfig.Binds) != 1 || safetyConfig.HostConfig.Binds[0] != "/data/web:/app/data:ro" {
		t.Errorf("expected HostConfig Binds preserved, got: %v", safetyConfig.HostConfig.Binds)
	}
	if safetyConfig.HostConfig.RestartPolicy["Name"] != "unless-stopped" {
		t.Errorf("expected RestartPolicy 'unless-stopped', got: %v", safetyConfig.HostConfig.RestartPolicy["Name"])
	}
	if len(safetyConfig.NetworkSettings.Networks) != 1 {
		t.Errorf("expected Networks preserved")
	}
}

// 2. executeVerifiedRollback all 5 stages succeed
func TestExecuteVerifiedRollback_All5StagesSuccess(t *testing.T) {
	var stagesCalled []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/containers/home-web":
			stagesCalled = append(stagesCalled, "stage1_cleanup_candidate")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/containers/home-web_bak/rename":
			stagesCalled = append(stagesCalled, "stage2_rename_backup")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/containers/home-web/start":
			stagesCalled = append(stagesCalled, "stage3_start_original")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/containers/home-web/json":
			stagesCalled = append(stagesCalled, "stage4_inspect_running")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"State": map[string]interface{}{
					"Running": true,
					"Status":  "running",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	// Replace http.Client with one that talks to our test server
	testClient := server.Client()
	// Transport redirect
	testClient.Transport = &roundTripperRewrite{
		targetURL: server.URL,
		base:      http.DefaultTransport,
	}

	healthChecked := false
	healthChecker := func(ctx context.Context, target string) error {
		healthChecked = true
		return nil
	}

	err := executeVerifiedRollback(
		context.Background(),
		testClient,
		"home-web",
		"home-web_bak",
		healthChecker,
		"home-web",
		errors.New("simulated recreation failure"),
	)

	if err != nil {
		t.Fatalf("expected 5-stage rollback to succeed, got error: %v", err)
	}
	if !healthChecked {
		t.Fatalf("expected stage 5 health checker to be called")
	}
	if len(stagesCalled) != 4 {
		t.Fatalf("expected 4 HTTP stages called, got: %v", stagesCalled)
	}
}

// 3. executeVerifiedRollback stage failures
func TestExecuteVerifiedRollback_StageFailures(t *testing.T) {
	t.Run("RenameStageFails", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/containers/home-web_bak/rename" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("rename error"))
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client := server.Client()
		client.Transport = &roundTripperRewrite{targetURL: server.URL, base: http.DefaultTransport}

		err := executeVerifiedRollback(
			context.Background(),
			client,
			"home-web",
			"home-web_bak",
			nil,
			"home-web",
			errors.New("initial failure"),
		)
		if err == nil {
			t.Fatalf("expected error when rename fails, got nil")
		}
	})

	t.Run("InspectStageNotRunningFails", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/containers/home-web/json":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"State": map[string]interface{}{
						"Running": false,
						"Status":  "exited",
					},
				})
			default:
				w.WriteHeader(http.StatusOK)
			}
		}))
		defer server.Close()

		client := server.Client()
		client.Transport = &roundTripperRewrite{targetURL: server.URL, base: http.DefaultTransport}

		err := executeVerifiedRollback(
			context.Background(),
			client,
			"home-web",
			"home-web_bak",
			nil,
			"home-web",
			errors.New("initial failure"),
		)
		if err == nil {
			t.Fatalf("expected error when restored container is not running, got nil")
		}
	})

	t.Run("HealthProbeStageFails", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/containers/home-web/json":
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
		client.Transport = &roundTripperRewrite{targetURL: server.URL, base: http.DefaultTransport}

		healthChecker := func(ctx context.Context, target string) error {
			return errors.New("health check timeout / 503 service unavailable")
		}

		err := executeVerifiedRollback(
			context.Background(),
			client,
			"home-web",
			"home-web_bak",
			healthChecker,
			"home-web",
			errors.New("initial failure"),
		)
		if err == nil {
			t.Fatalf("expected error when health probe fails, got nil")
		}
	})
}

// 4. ReconcileStartupProposals
func TestReconcileStartupProposals_Comprehensive(t *testing.T) {
	db := setupRollbackTestDB(t)

	// Case A: Proposal in EXECUTING with passing health check -> EXECUTION_UNKNOWN
	propA := model.OpsProposal{
		ProposalID:      "reconcile-exec-healthy",
		TargetComponent: "home-api",
		Status:          model.ProposalStatusExecuting,
		CreatedAt:       time.Now(),
	}
	db.Create(&propA)

	// Case B: Proposal in EXECUTING with failing health check -> RECONCILIATION_REQUIRED
	propB := model.OpsProposal{
		ProposalID:      "reconcile-exec-unhealthy",
		TargetComponent: "home-broken",
		Status:          model.ProposalStatusExecuting,
		CreatedAt:       time.Now(),
	}
	db.Create(&propB)

	// Case C: Proposal in ROLLING_BACK -> RECONCILIATION_REQUIRED
	propC := model.OpsProposal{
		ProposalID:      "reconcile-rb",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusRollingBack,
		CreatedAt:       time.Now(),
	}
	db.Create(&propC)

	// Customize CheckHealth: "home-api" is healthy, "home-broken" is failing
	mockExecWithHealth := &mockReconcileExecutor{
		healthFn: func(ctx context.Context, target string) error {
			if target == "home-api" {
				return nil
			}
			return errors.New("connection refused")
		},
	}

	err := ReconcileStartupProposals(db, mockExecWithHealth)
	if err != nil {
		t.Fatalf("unexpected error during ReconcileStartupProposals: %v", err)
	}

	var resA, resB, resC model.OpsProposal
	db.Where("proposal_id = ?", propA.ProposalID).First(&resA)
	db.Where("proposal_id = ?", propB.ProposalID).First(&resB)
	db.Where("proposal_id = ?", propC.ProposalID).First(&resC)

	if resA.Status != model.ProposalStatusExecutionUnknown {
		t.Errorf("expected propA status %s, got %s", model.ProposalStatusExecutionUnknown, resA.Status)
	}
	if !resA.ReconciledAt.Valid {
		t.Errorf("expected propA reconciled_at to be valid")
	}

	if resB.Status != model.ProposalStatusReconciliationRequired {
		t.Errorf("expected propB status %s, got %s", model.ProposalStatusReconciliationRequired, resB.Status)
	}
	if !resB.ReconciledAt.Valid {
		t.Errorf("expected propB reconciled_at to be valid")
	}

	if resC.Status != model.ProposalStatusReconciliationRequired {
		t.Errorf("expected propC status %s, got %s", model.ProposalStatusReconciliationRequired, resC.Status)
	}
	if !resC.ReconciledAt.Valid {
		t.Errorf("expected propC reconciled_at to be valid")
	}
}

// 5. ExecuteProposal timeout transitions to EXECUTION_UNKNOWN
func TestExecuteProposal_TimeoutTransitionsToExecutionUnknown(t *testing.T) {
	db := setupRollbackTestDB(t)

	executor := NewOperationExecutor(
		db, nil, nil, nil,
		WithProbeTimeout(50*time.Millisecond),
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			return "", context.DeadlineExceeded
		}),
	)

	prop := model.OpsProposal{
		ProposalID:      "prop-timeout-001",
		Title:           "Timeout execution test",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusApproved,
		CreatedAt:       time.Now(),
	}
	db.Create(&prop)

	_, err := executor.ExecuteProposal(context.Background(), &prop)
	if err == nil {
		t.Fatalf("expected error from ExecuteProposal when runner times out")
	}

	var updated model.OpsProposal
	if err := db.Where("id = ?", prop.ID).First(&updated).Error; err != nil {
		t.Fatalf("failed to query proposal from DB: %v", err)
	}

	if updated.Status != model.ProposalStatusExecutionUnknown {
		t.Errorf("expected proposal status %s on timeout, got %s", model.ProposalStatusExecutionUnknown, updated.Status)
	}
	if !updated.ExecutedAt.Valid {
		t.Errorf("expected executed_at to be valid NullTime")
	}
}

type mockReconcileExecutor struct {
	healthFn func(ctx context.Context, target string) error
}

func (m *mockReconcileExecutor) Execute(ctx context.Context, spec OperationSpec) (*ExecutionResult, error) {
	return nil, nil
}
func (m *mockReconcileExecutor) ExecuteRollback(ctx context.Context, spec OperationSpec) error {
	return nil
}
func (m *mockReconcileExecutor) ExecuteProposal(ctx context.Context, proposal *model.OpsProposal) (*ExecutionResult, error) {
	return nil, nil
}
func (m *mockReconcileExecutor) ValidateComponent(target string) (string, error) {
	return target, nil
}
func (m *mockReconcileExecutor) CheckHealth(ctx context.Context, target string) error {
	if m.healthFn != nil {
		return m.healthFn(ctx, target)
	}
	return nil
}

type roundTripperRewrite struct {
	targetURL string
	base      http.RoundTripper
}

func (r *roundTripperRewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	// Rewrite host & scheme to targetURL
	parsedTarget, _ := http.NewRequest(req.Method, r.targetURL, nil)
	req.URL.Scheme = parsedTarget.URL.Scheme
	req.URL.Host = parsedTarget.URL.Host
	return r.base.RoundTrip(req)
}
