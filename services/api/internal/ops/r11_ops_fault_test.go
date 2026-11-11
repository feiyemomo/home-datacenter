package ops

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/audit"
	"home-datacenter-api/internal/hostsec"
	"home-datacenter-api/internal/model"
)

func resetTestRecoveryFSM(t *testing.T) {
	t.Helper()
	prev := GetGlobalRecoveryFSM()
	SetGlobalRecoveryFSM(nil)
	t.Cleanup(func() { SetGlobalRecoveryFSM(prev) })
}

// TestR11_FI_16_DBCorruptionDuringUpdateFailsClosed verifies that dropping or corrupting
// required audit tables prior to mutation causes ExecuteProposal to fail closed before
// invoking dockerRunner.
func TestR11_FI_16_DBCorruptionDuringUpdateFailsClosed(t *testing.T) {
	resetTestRecoveryFSM(t)
	db := newPhase2TestDB(t)
	ae, err := audit.NewStrictAgentAuditEngine(db, nil, audit.WithArchiveDir(t.TempDir()))
	if err != nil {
		t.Fatalf("NewStrictAgentAuditEngine: %v", err)
	}
	defer ae.Close()

	v1Hash := "v1:" + strings.Repeat("1", 64)
	var dockerCalls atomic.Int32
	exec := NewOperationExecutor(
		db, nil, nil, nil,
		WithBaselineExtractor(func(target string) string { return v1Hash }),
		WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
			dockerCalls.Add(1)
			return "unexpected", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
	)
	exec.SetAuditEngine(ae)

	prop := &model.OpsProposal{
		ProposalID:      "prop-r11-fi-16",
		Title:           "Restart home-web under DB fault",
		TargetComponent: "home-web",
		ExecutionPlan:   "docker restart home-web",
		RollbackPlan:    "none",
		Status:          model.ProposalStatusApproved,
		BaselineHash:    v1Hash,
		RiskLevel:       model.RiskLevelHigh,
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
	}
	seedTestSignedProposal(t, db, prop)

	// Simulate SQLite corruption of audit event store mid-operation before mutation starts.
	if err := db.Exec("DROP TABLE agent_audit_events").Error; err != nil {
		t.Fatalf("drop agent_audit_events: %v", err)
	}

	if _, err := exec.ExecuteProposal(context.Background(), prop); err == nil {
		t.Fatalf("expected ExecuteProposal to fail closed when audit DB table is corrupted/missing")
	}
	if dockerCalls.Load() != 0 {
		t.Fatalf("expected zero dockerRunner executions after DB corruption, got %d", dockerCalls.Load())
	}
}

// TestR11_FI_17_PostApprovalSpecAndBaselineTampering verifies that tampering with any
// digest-covered proposal field after approval fails closed with ErrApprovalDigestMismatch
// and zero side effects.
func TestR11_FI_17_PostApprovalSpecAndBaselineTampering(t *testing.T) {
	resetTestRecoveryFSM(t)
	db := newPhase2TestDB(t)
	v1Orig := "v1:" + strings.Repeat("2", 64)
	v1Evil := "v1:" + strings.Repeat("e", 64)

	var dockerCalls atomic.Int32
	exec := NewOperationExecutor(
		db, nil, nil, nil,
		WithBaselineExtractor(func(target string) string { return v1Orig }),
		WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
			dockerCalls.Add(1)
			return "tampered-run", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
	)

	tamperCases := []struct {
		name   string
		mutate func(p *model.OpsProposal)
	}{
		{
			name: "action_spec_tampered",
			mutate: func(p *model.OpsProposal) {
				_ = db.Model(&model.OpsProposal{}).Where("proposal_id = ?", p.ProposalID).
					Update("action_spec", `{"action":"docker.restart","target":"home-api"}`).Error
			},
		},
		{
			name: "baseline_hash_tampered",
			mutate: func(p *model.OpsProposal) {
				_ = db.Model(&model.OpsProposal{}).Where("proposal_id = ?", p.ProposalID).
					Update("baseline_hash", v1Evil).Error
			},
		},
		{
			name: "target_component_tampered",
			mutate: func(p *model.OpsProposal) {
				_ = db.Model(&model.OpsProposal{}).Where("proposal_id = ?", p.ProposalID).
					Update("target_component", "home-api").Error
			},
		},
		{
			name: "risk_level_downgraded",
			mutate: func(p *model.OpsProposal) {
				_ = db.Model(&model.OpsProposal{}).Where("proposal_id = ?", p.ProposalID).
					Update("risk_level", model.RiskLevelLow).Error
			},
		},
	}

	for _, tc := range tamperCases {
		t.Run(tc.name, func(t *testing.T) {
			prop := &model.OpsProposal{
				ProposalID:      "prop-r11-fi-17-" + tc.name,
				Title:           "Signed proposal " + tc.name,
				TargetComponent: "home-web",
				ExecutionPlan:   "docker restart home-web",
				RollbackPlan:    "none",
				Status:          model.ProposalStatusApproved,
				BaselineHash:    v1Orig,
				RiskLevel:       model.RiskLevelCritical,
				ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
			}
			seedTestSignedProposal(t, db, prop)
			tc.mutate(prop)

			if _, err := exec.ExecuteProposal(context.Background(), prop); !errors.Is(err, ErrApprovalDigestMismatch) {
				t.Fatalf("tamper case %s: expected ErrApprovalDigestMismatch, got %v", tc.name, err)
			}
		})
	}
	if dockerCalls.Load() != 0 {
		t.Fatalf("expected zero dockerRunner executions across tampered proposals, got %d", dockerCalls.Load())
	}
}

// TestR11_FI_18_StrictRuntimeProtocolAndAuditConfigBoundaries verifies strict baseline
// enforcement in production mode (`v1:` vs `host-v1:`) and fail-closed strict handler construction
// on missing remote / invalid audit configuration.
func TestR11_FI_18_StrictRuntimeProtocolAndAuditConfigBoundaries(t *testing.T) {
	resetTestRecoveryFSM(t)
	t.Setenv("OPS_STRICT_BASELINE", "1")

	legacyHash := "sha256:" + strings.Repeat("a", 64)
	containerV1 := "v1:" + strings.Repeat("b", 64)
	hostV1 := "host-v1:" + strings.Repeat("c", 64)

	if IsRealBaselineHash(legacyHash, "home-web") {
		t.Fatalf("production mode must reject legacy sha256 baseline for containers")
	}
	if IsRealBaselineHash(containerV1, ComponentHostOS) {
		t.Fatalf("host-os must reject container v1: baseline and require host-v1:")
	}
	if IsRealBaselineHash(hostV1, "home-web") {
		t.Fatalf("container target must reject host-v1: baseline")
	}
	if !IsRealBaselineHash(containerV1, "home-web") || !IsRealBaselineHash(hostV1, ComponentHostOS) {
		t.Fatalf("expected matching v1 and host-v1 baselines to validate")
	}

	db := newPhase2TestDB(t)
	t.Setenv("OPS_AUDIT_REMOTE_TYPE", "http-append")
	t.Setenv("OPS_AUDIT_REMOTE_URL", "")
	if _, err := NewStrictOpsHandler(nil, nil, db, nil, nil, audit.WithArchiveDir(t.TempDir())); err == nil {
		t.Fatalf("expected NewStrictOpsHandler to fail closed when http-append remote URL is empty")
	}

	t.Setenv("OPS_AUDIT_REMOTE_TYPE", "")
	t.Setenv("OPS_AUDIT_STRICT_REMOTE", "1")
	if _, err := NewStrictOpsHandler(nil, nil, db, nil, nil, audit.WithArchiveDir(t.TempDir())); !errors.Is(err, audit.ErrAuditRemoteRequired) {
		t.Fatalf("expected ErrAuditRemoteRequired when strict remote set without store, got %v", err)
	}
}

// TestR11_FI_19_ConcurrentApprovalCancelAndSingleExecutionCAS stress-tests concurrent
// ApproveProposal vs CancelProposal CAS transitions and concurrent ExecuteProposal leasing.
func TestR11_FI_19_ConcurrentApprovalCancelAndSingleExecutionCAS(t *testing.T) {
	resetTestRecoveryFSM(t)
	gin.SetMode(gin.TestMode)

	db := newPhase2TestDB(t)
	if sqlDB, err := db.DB(); err == nil && sqlDB != nil {
		sqlDB.SetMaxOpenConns(1)
	}

	v1Hash := "v1:" + strings.Repeat("4", 64)
	pendingProp := &model.OpsProposal{
		ProposalID:      "prop-r11-fi-19-cas",
		Title:           "Concurrent approve vs cancel",
		TargetComponent: "home-web",
		ExecutionPlan:   "docker restart home-web",
		RollbackPlan:    "none",
		Status:          model.ProposalStatusPending,
		BaselineHash:    v1Hash,
		RiskLevel:       model.RiskLevelHigh,
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
	}
	seedTestSignedProposal(t, db, pendingProp)

	var runCount atomic.Int32
	exec := NewOperationExecutor(
		db, nil, nil, nil,
		WithBaselineExtractor(func(target string) string { return v1Hash }),
		WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
			runCount.Add(1)
			time.Sleep(30 * time.Millisecond)
			return "ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
	)
	h := NewOpsHandler(nil, nil, db, nil, nil)
	h.SetExecutor(exec)

	router := gin.New()
	router.POST("/api/v1/ops/proposals/:id/approve", func(c *gin.Context) {
		c.Set("is_admin", true)
		c.Set("username", "admin")
		c.Set("household_id", uint(1))
		h.ApproveProposal(c)
	})
	router.POST("/api/v1/ops/proposals/:id/cancel", func(c *gin.Context) {
		c.Set("is_admin", true)
		c.Set("username", "admin")
		c.Set("household_id", uint(1))
		h.CancelProposal(c)
	})

	const racers = 8
	var wg sync.WaitGroup
	var winCount atomic.Int32
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			path := "/api/v1/ops/proposals/prop-r11-fi-19-cas/approve"
			if idx%2 == 1 {
				path = "/api/v1/ops/proposals/prop-r11-fi-19-cas/cancel"
			}
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code == http.StatusOK {
				winCount.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if winCount.Load() != 1 {
		t.Fatalf("expected exactly 1 atomic CAS winner between concurrent approve/cancel, got %d", winCount.Load())
	}
	var afterCAS model.OpsProposal
	if err := db.Where("proposal_id = ?", "prop-r11-fi-19-cas").First(&afterCAS).Error; err != nil {
		t.Fatal(err)
	}
	if afterCAS.Status != model.ProposalStatusApproved && afterCAS.Status != model.ProposalStatusCancelled {
		t.Fatalf("unexpected post-CAS status %q", afterCAS.Status)
	}

	// Part B: Concurrent ExecuteProposal calls on one APPROVED proposal must execute at most once.
	execProp := &model.OpsProposal{
		ProposalID:      "prop-r11-fi-19-exec",
		Title:           "Concurrent execution race",
		TargetComponent: "home-web",
		ExecutionPlan:   "docker restart home-web",
		RollbackPlan:    "none",
		Status:          model.ProposalStatusApproved,
		BaselineHash:    v1Hash,
		RiskLevel:       model.RiskLevelHigh,
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
	}
	seedTestSignedProposal(t, db, execProp)

	const execWorkers = 6
	var execWG sync.WaitGroup
	var execSuccess atomic.Int32
	var execRejected atomic.Int32
	for i := 0; i < execWorkers; i++ {
		execWG.Add(1)
		go func() {
			defer execWG.Done()
			propCopy := *execProp
			_, err := exec.ExecuteProposal(context.Background(), &propCopy)
			if err == nil {
				execSuccess.Add(1)
			} else if errors.Is(err, ErrProposalAlreadyRunning) || errors.Is(err, ErrProposalNotExecutable) {
				execRejected.Add(1)
			} else {
				t.Errorf("unexpected non-CAS execution error: %v", err)
			}
		}()
	}
	execWG.Wait()

	if execSuccess.Load() != 1 || execRejected.Load() != execWorkers-1 || runCount.Load() != 1 {
		t.Fatalf("expected 1 single winner and %d CAS rejections (docker runs=1), got success=%d rejected=%d runs=%d",
			execWorkers-1, execSuccess.Load(), execRejected.Load(), runCount.Load())
	}
}

// TestR11_FI_20_MissingBridgeAndDegradedCircuitFailClosed verifies fail-closed gating
// across degraded storage circuit state and missing host execution bridge on /host/update.
func TestR11_FI_20_MissingBridgeAndDegradedCircuitFailClosed(t *testing.T) {
	resetTestRecoveryFSM(t)
	gin.SetMode(gin.TestMode)

	db := newPhase2TestDB(t)
	debRoot := writeOpsHostFixture(t, false)
	hue := hostsec.NewHostUpdateExecutor(debRoot, t.TempDir(), nil)
	hue.Detector.InDocker = true
	hostV1, _, bhErr := hue.ComputeBaselineHash(context.Background(), []string{"openssh-server"})
	if bhErr != nil {
		t.Fatalf("ComputeBaselineHash: %v", bhErr)
	}

	v1Hash := "v1:" + strings.Repeat("5", 64)
	var dockerCalls atomic.Int32
	exec := NewOperationExecutor(
		db, nil, nil, nil,
		WithBaselineExtractor(func(target string) string {
			if target == ComponentHostOS {
				return hostV1
			}
			return v1Hash
		}),
		WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
			dockerCalls.Add(1)
			return "should-not-run", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
	)

	// 1. Degraded circuit breaker state blocks operations fail-closed.
	fsm := NewContainerRecoveryFSM(db, nil, nil)
	fsm.GetCircuitBreaker().SetDegraded(true)
	exec.SetRecoveryFSM(fsm)
	degradedProp := &model.OpsProposal{
		ProposalID:      "prop-r11-fi-20-degraded",
		Title:           "Blocked by degraded circuit breaker",
		TargetComponent: "home-web",
		ExecutionPlan:   "docker restart home-web",
		RollbackPlan:    "none",
		Status:          model.ProposalStatusApproved,
		BaselineHash:    v1Hash,
		RiskLevel:       model.RiskLevelHigh,
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
	}
	seedTestSignedProposal(t, db, degradedProp)
	if _, err := exec.ExecuteProposal(context.Background(), degradedProp); !errors.Is(err, ErrFailClosedMode) && !errors.Is(err, ErrCircuitBreakerOpen) {
		t.Fatalf("expected ErrFailClosedMode or ErrCircuitBreakerOpen when circuit breaker is degraded, got %v", err)
	}
	if dockerCalls.Load() != 0 {
		t.Fatalf("expected zero dockerRunner executions while circuit breaker is degraded, got %d", dockerCalls.Load())
	}

	// 2. HTTP POST /api/v1/ops/host/update fails closed with 409 when host executor is unbridged across container boundary.
	exec.SetRecoveryFSM(nil)
	h := NewOpsHandler(nil, nil, db, nil, nil)
	h.SetExecutor(exec)
	h.SetHostUpdateExecutor(hue)

	hostProp := &model.OpsProposal{
		ProposalID:      "prop-r11-fi-20-host",
		Title:           "Host update without bridge",
		TargetComponent: ComponentHostOS,
		ExecutionPlan:   "apt-get upgrade openssh-server",
		RollbackPlan:    "restore sshd",
		Status:          model.ProposalStatusApproved,
		BaselineHash:    hostV1,
		RiskLevel:       model.RiskLevelCritical,
		ActionSpec:      `{"action":"host.pkg_upgrade","target":"host-os","params":{"packages":["openssh-server"]}}`,
	}
	seedTestSignedProposal(t, db, hostProp)

	router := gin.New()
	router.POST("/api/v1/ops/host/update", func(c *gin.Context) {
		c.Set("is_admin", true)
		c.Set("username", "admin")
		c.Set("household_id", uint(1))
		h.ExecuteHostUpdate(c)
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ops/host/update", bytes.NewBufferString(`{"proposal_id":"prop-r11-fi-20-host"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "container namespace boundary") {
		t.Fatalf("expected 409 Conflict container namespace boundary error on unbridged /host/update, got %d: %s", rec.Code, rec.Body.String())
	}
}
