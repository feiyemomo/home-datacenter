package ops

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func setupR4RaceDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "r4_race.db")
	db, err := gorm.Open(sqlite.Open(dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite db: %v", err)
	}
	if err := EnsureOpsSchema(db); err != nil {
		t.Fatalf("EnsureOpsSchema failed: %v", err)
	}
	_ = db.AutoMigrate(&model.SystemLog{})
	if sqlDB, sErr := db.DB(); sErr == nil {
		sqlDB.SetMaxOpenConns(8)
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	return db
}

func newInstantOfflineHandler(t *testing.T, db *gorm.DB, bus *eventbus.Bus) *OpsHandler {
	t.Helper()
	// Point client to instant local server returning 503 so ComputeFingerprint falls back in <1ms
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	client := NewOnePanelClient(srv.URL, "offline", "", "")
	h := NewOpsHandler(client, NewHostFallbackProvider("", ""), db, bus, nil)
	return h
}

func makeAdminRouter(h *OpsHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	rg := r.Group("/api/v1/ops")
	rg.Use(func(c *gin.Context) {
		c.Set("user_id", uint(1))
		c.Set("user_name", "admin-race")
		c.Set("is_admin", true)
		c.Set("household_id", uint(1))
		c.Next()
	})
	h.RegisterProposalRoutes(rg)
	return r
}

// TestR4_19_ProposalApproveVsCancel_100Rounds exercises 100 concurrent Approve vs Cancel races (R4-01).
func TestR4_19_ProposalApproveVsCancel_100Rounds(t *testing.T) {
	db := setupR4RaceDB(t)
	bus := eventbus.New()

	var execCount atomic.Int64
	executor := NewOperationExecutor(
		db, bus, nil, nil,
		WithProbeTimeout(40*time.Millisecond),
		WithProbeInterval(5*time.Millisecond),
		WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
			execCount.Add(1)
			return "ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	handler := newInstantOfflineHandler(t, db, bus)
	handler.SetExecutor(executor)
	r := makeAdminRouter(handler)

	const rounds = 100
	var approveWins, cancelWins int

	for i := 0; i < rounds; i++ {
		propID := fmt.Sprintf("r4-app-can-%d", i)
		p := model.OpsProposal{
			ProposalID:      propID,
			Title:           "Approve vs Cancel Race",
			TargetComponent: "home-web",
			RiskLevel:       model.RiskLevelHigh,
			Status:          model.ProposalStatusAwaitingApproval,
			ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
			BaselineHash:    model.ComputeDefaultBaseline("home-web"),
			Version:         1,
			Revision:        1,
			CreatedAt:       time.Now().UTC(),
		}
		if err := db.Create(&p).Error; err != nil {
			t.Fatalf("round %d seed failed: %v", i, err)
		}

		beforeCount := execCount.Load()
		barrier := make(chan struct{})
		var wg sync.WaitGroup
		var codeApp, codeCan int

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-barrier
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/"+propID+"/approve?sync=true", strings.NewReader(`{"action":"now"}`))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			codeApp = w.Code
		}()
		go func() {
			defer wg.Done()
			<-barrier
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/"+propID+"/cancel", nil)
			r.ServeHTTP(w, req)
			codeCan = w.Code
		}()

		close(barrier)
		wg.Wait()

		if codeApp == http.StatusOK && codeCan == http.StatusOK {
			t.Fatalf("round %d CRITICAL VIOLATION: both Approve and Cancel returned 200 OK", i)
		}
		var finalProp model.OpsProposal
		if err := db.Where("proposal_id = ?", propID).First(&finalProp).Error; err != nil {
			t.Fatalf("round %d reload failed: %v", i, err)
		}
		deltaExec := execCount.Load() - beforeCount

		if codeApp == http.StatusOK {
			approveWins++
			if codeCan != http.StatusConflict && codeCan != http.StatusBadRequest {
				t.Fatalf("round %d approve won, cancel expected 409/400 got %d", i, codeCan)
			}
			if finalProp.Status != model.ProposalStatusExecuted {
				t.Fatalf("round %d approve won cleanly, expected EXECUTED, got %s", i, finalProp.Status)
			}
			if deltaExec != 1 {
				t.Fatalf("round %d approve won, expected 1 execution, got %d", i, deltaExec)
			}
		} else if codeCan == http.StatusOK {
			cancelWins++
			if codeApp != http.StatusConflict && codeApp != http.StatusBadRequest {
				t.Fatalf("round %d cancel won, approve expected 409/400 got %d", i, codeApp)
			}
			if finalProp.Status != model.ProposalStatusCancelled {
				t.Fatalf("round %d cancel won, expected CANCELLED, got %s", i, finalProp.Status)
			}
			if deltaExec != 0 {
				t.Fatalf("round %d CRITICAL: cancel won but executor ran %d times", i, deltaExec)
			}
		} else {
			t.Fatalf("round %d unexpected dual failure: approve=%d cancel=%d status=%s", i, codeApp, codeCan, finalProp.Status)
		}
	}

	t.Logf("R4-19 ApproveVsCancel 100 rounds passed: approveWins=%d cancelWins=%d", approveWins, cancelWins)
}

// TestR4_19_ProposalApproveVsApprove_100Rounds verifies double-approval CAS mutual exclusion.
func TestR4_19_ProposalApproveVsApprove_100Rounds(t *testing.T) {
	db := setupR4RaceDB(t)
	bus := eventbus.New()
	var execCount atomic.Int64

	executor := NewOperationExecutor(
		db, bus, nil, nil,
		WithProbeTimeout(40*time.Millisecond),
		WithProbeInterval(5*time.Millisecond),
		WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
			execCount.Add(1)
			return "ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	handler := newInstantOfflineHandler(t, db, bus)
	handler.SetExecutor(executor)
	r := makeAdminRouter(handler)

	for i := 0; i < 100; i++ {
		propID := fmt.Sprintf("r4-app-app-%d", i)
		p := model.OpsProposal{
			ProposalID:      propID,
			Title:           "Approve vs Approve Race",
			TargetComponent: "home-api",
			RiskLevel:       model.RiskLevelHigh,
			Status:          model.ProposalStatusAwaitingApproval,
			ActionSpec:      `{"action":"docker.restart","target":"home-api"}`,
			BaselineHash:    model.ComputeDefaultBaseline("home-api"),
			Version:         1,
			Revision:        1,
			CreatedAt:       time.Now().UTC(),
		}
		if err := db.Create(&p).Error; err != nil {
			t.Fatalf("round %d seed failed: %v", i, err)
		}

		beforeCount := execCount.Load()
		barrier := make(chan struct{})
		var wg sync.WaitGroup
		codes := make([]int, 2)

		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-barrier
				w := httptest.NewRecorder()
				req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/"+propID+"/approve?sync=true", strings.NewReader(`{"action":"now"}`))
				req.Header.Set("Content-Type", "application/json")
				r.ServeHTTP(w, req)
				codes[idx] = w.Code
			}(g)
		}
		close(barrier)
		wg.Wait()

		ok200 := 0
		for _, c := range codes {
			if c == http.StatusOK {
				ok200++
			}
		}
		if ok200 != 1 {
			t.Fatalf("round %d expected strictly 1 approval winner, got codes=%v", i, codes)
		}
		if execCount.Load()-beforeCount != 1 {
			t.Fatalf("round %d expected strictly 1 execution dispatch, got %d", i, execCount.Load()-beforeCount)
		}
	}
}

// TestR4_19_ProposalExecuteVsExecute_100Rounds exercises 100 concurrent Execute races.
func TestR4_19_ProposalExecuteVsExecute_100Rounds(t *testing.T) {
	db := setupR4RaceDB(t)
	bus := eventbus.New()
	var execCount atomic.Int64

	executor := NewOperationExecutor(
		db, bus, nil, nil,
		WithProbeTimeout(40*time.Millisecond),
		WithProbeInterval(5*time.Millisecond),
		WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
			execCount.Add(1)
			return "ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	for i := 0; i < 100; i++ {
		propID := fmt.Sprintf("r4-exe-exe-%d", i)
		p := model.OpsProposal{
			ProposalID:      propID,
			Title:           "Execute vs Execute Race",
			TargetComponent: "home-web",
			RiskLevel:       model.RiskLevelHigh,
			Status:          model.ProposalStatusApproved,
			ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
			CreatedAt:       time.Now().UTC(),
		}
		seedTestSignedProposal(t, db, &p)

		before := execCount.Load()
		barrier := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)

		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				copyP := p
				<-barrier
				_, err := executor.ExecuteProposal(context.Background(), &copyP)
				errs[idx] = err
			}(g)
		}
		close(barrier)
		wg.Wait()

		wins := 0
		for _, e := range errs {
			if e == nil {
				wins++
			}
		}
		if wins != 1 {
			t.Fatalf("round %d expected strictly 1 ExecuteProposal winner, got wins=%d errs=%v", i, wins, errs)
		}
		if execCount.Load()-before != 1 {
			t.Fatalf("round %d expected 1 docker invocation, got %d", i, execCount.Load()-before)
		}
	}
}

// TestR4_19_ProposalScheduleVsCancel_100Rounds exercises ProposalScheduler.Tick vs CancelProposal CAS mutual exclusion.
func TestR4_19_ProposalScheduleVsCancel_100Rounds(t *testing.T) {
	db := setupR4RaceDB(t)
	bus := eventbus.New()
	var execCount atomic.Int64

	executor := NewOperationExecutor(
		db, bus, nil, nil,
		WithProbeTimeout(40*time.Millisecond),
		WithProbeInterval(5*time.Millisecond),
		WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
			execCount.Add(1)
			return "ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	handler := newInstantOfflineHandler(t, db, bus)
	handler.SetExecutor(executor)
	r := makeAdminRouter(handler)
	sched := NewProposalScheduler(db, executor)

	pastSched := time.Now().Add(-2 * time.Minute).Format(time.RFC3339)
	for i := 0; i < 100; i++ {
		propID := fmt.Sprintf("r4-sched-can-%d", i)
		p := model.OpsProposal{
			ProposalID:      propID,
			Title:           "Schedule vs Cancel Race",
			TargetComponent: "home-vision",
			RiskLevel:       model.RiskLevelMedium,
			Status:          model.ProposalStatusScheduled,
			ScheduledAt:     &pastSched,
			ActionSpec:      `{"action":"docker.restart","target":"home-vision"}`,
			CreatedAt:       time.Now().UTC(),
		}
		seedTestSignedProposal(t, db, &p)

		before := execCount.Load()
		barrier := make(chan struct{})
		var wg sync.WaitGroup
		var cancelCode int

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-barrier
			sched.Tick()
		}()
		go func() {
			defer wg.Done()
			<-barrier
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/"+propID+"/cancel", nil)
			r.ServeHTTP(w, req)
			cancelCode = w.Code
		}()
		close(barrier)
		wg.Wait()

		var finalProp model.OpsProposal
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if err := db.Where("proposal_id = ?", propID).First(&finalProp).Error; err != nil {
				t.Fatalf("round %d query failed: %v", i, err)
			}
			if finalProp.Status != model.ProposalStatusExecuting {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		delta := execCount.Load() - before
		if cancelCode == http.StatusOK {
			if finalProp.Status != model.ProposalStatusCancelled || delta != 0 {
				t.Fatalf("round %d cancel won but status=%s deltaExec=%d", i, finalProp.Status, delta)
			}
		} else {
			if finalProp.Status != model.ProposalStatusExecuted || delta != 1 {
				t.Fatalf("round %d scheduler won expected EXECUTED+1, got status=%s deltaExec=%d", i, finalProp.Status, delta)
			}
		}
	}
}

// TestR4_19_CrashDuringTransitionStartupReconcile verifies crash recovery reconciliation on startup.
func TestR4_19_CrashDuringTransitionStartupReconcile(t *testing.T) {
	db := setupR4RaceDB(t)
	executor := NewOperationExecutor(db, eventbus.New(), nil, nil,
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	pExec := model.OpsProposal{
		ProposalID:      "r4-crash-exec",
		Title:           "Crashed While Executing",
		TargetComponent: "home-api",
		Status:          model.ProposalStatusExecuting,
		ExecutionToken:  "crashed-token-1",
		ActionSpec:      `{"action":"docker.restart","target":"home-api"}`,
		BaselineHash:    model.ComputeDefaultBaseline("home-api"),
		CreatedAt:       time.Now().UTC().Add(-5 * time.Minute),
	}
	if err := db.Create(&pExec).Error; err != nil {
		t.Fatalf("seed crashed executing failed: %v", err)
	}
	pRB := model.OpsProposal{
		ProposalID:      "r4-crash-rb",
		Title:           "Crashed While Rolling Back",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusRollingBack,
		ExecutionToken:  "crashed-token-2",
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
		BaselineHash:    model.ComputeDefaultBaseline("home-web"),
		CreatedAt:       time.Now().UTC().Add(-5 * time.Minute),
	}
	if err := db.Create(&pRB).Error; err != nil {
		t.Fatalf("seed crashed rolling back failed: %v", err)
	}

	if err := ReconcileStartupProposals(db, executor); err != nil {
		t.Fatalf("ReconcileStartupProposals failed: %v", err)
	}

	var updatedExec, updatedRB model.OpsProposal
	if err := db.Where("proposal_id = ?", "r4-crash-exec").First(&updatedExec).Error; err != nil {
		t.Fatalf("reload pExec failed: %v", err)
	}
	if updatedExec.Status != model.ProposalStatusExecutionUnknown {
		t.Fatalf("expected EXECUTION_UNKNOWN for crashed EXECUTING with healthy probe, got %s", updatedExec.Status)
	}
	if err := db.Where("proposal_id = ?", "r4-crash-rb").First(&updatedRB).Error; err != nil {
		t.Fatalf("reload pRB failed: %v", err)
	}
	if updatedRB.Status != model.ProposalStatusReconciliationRequired {
		t.Fatalf("expected RECONCILIATION_REQUIRED for crashed ROLLING_BACK, got %s", updatedRB.Status)
	}
}

// TestR4_19_SchedulerPreClaimAbortAndPatchInvalidation verifies Blocker 2 fix (pre-claimed scheduler
// proposals transition cleanly to FAILED on tamper/precheck failure instead of stranding in EXECUTING)
// and verifies PATCH /proposals/:id invalidates prior approval via CAS.
func TestR4_19_SchedulerPreClaimAbortAndPatchInvalidation(t *testing.T) {
	db := setupR4RaceDB(t)
	bus := eventbus.New()
	executor := NewOperationExecutor(db, bus, nil, nil,
		WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
			return "ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	// 1. Tamper a SCHEDULED proposal's baseline_hash after signing -> Scheduler claims -> ExecuteProposal aborts to FAILED
	past := time.Now().Add(-1 * time.Minute).Format(time.RFC3339)
	p := model.OpsProposal{
		ProposalID:      "r4-sched-abort-1",
		Title:           "Tampered Scheduled Proposal",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusScheduled,
		ScheduledAt:     &past,
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
		CreatedAt:       time.Now().UTC(),
	}
	seedTestSignedProposal(t, db, &p)
	db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("execution_plan", "tampered-after-sign")

	sched := NewProposalScheduler(db, executor)
	sched.Tick()

	var after model.OpsProposal
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_ = db.Where("id = ?", p.ID).First(&after).Error
		if after.Status != model.ProposalStatusExecuting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after.Status != model.ProposalStatusFailed {
		t.Fatalf("expected scheduler pre-claimed tampered proposal to transition to FAILED (not stranded in EXECUTING), got %s", after.Status)
	}
	if after.ExecutionToken != "" {
		t.Fatalf("expected aborted proposal execution_token cleared, got %s", after.ExecutionToken)
	}

	// 2. PATCH /proposals/:id modifying rollback_plan on an APPROVED proposal must invalidate approval & bump version
	pApp := model.OpsProposal{
		ProposalID:      "r4-patch-inval-1",
		Title:           "Approved Patch Test",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusApproved,
		RollbackPlan:    "docker restart home-web",
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
		CreatedAt:       time.Now().UTC(),
	}
	seedTestSignedProposal(t, db, &pApp)

	h := newInstantOfflineHandler(t, db, bus)
	h.SetExecutor(executor)
	r := makeAdminRouter(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPatch, "/api/v1/ops/proposals/r4-patch-inval-1", strings.NewReader(`{"rollback_plan":"modified rollback"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected PATCH 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var patched model.OpsProposal
	_ = db.Where("proposal_id = ?", "r4-patch-inval-1").First(&patched).Error
	if patched.Status != model.ProposalStatusPending || patched.ApprovalDigest != "" || patched.Version <= 1 {
		t.Fatalf("expected PATCH bound field edit to invalidate approval -> PENDING/empty digest/version>1, got status=%s digest=%q version=%d", patched.Status, patched.ApprovalDigest, patched.Version)
	}
}
