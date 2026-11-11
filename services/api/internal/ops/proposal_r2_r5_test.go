package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/middleware"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

// TestR2_ApprovalDigest_IncludesRiskLevelAndAllBoundFields verifies R2.1:
// Canonical ApprovalDigest includes TargetComponent, ExecutionAction, Parameters,
// BaselineHash, Version, RiskLevel, and RollbackPlan; tampering any of them in DB
// causes ExecuteProposal to reject with ErrApprovalDigestMismatch.
func TestR2_ApprovalDigest_IncludesRiskLevelAndAllBoundFields(t *testing.T) {
	model.SetApprovalHMACKeyringForTest("k1", map[string]string{
		"k1": "super-secret-hmac-key-material-r2-0123456789abcdef",
	})
	defer model.SetApprovalHMACKeyringForTest("", nil)

	db := setupR4DigestDB(t)
	var runnerCalled atomic.Int64
	executor := NewOperationExecutor(
		db, eventbus.New(), nil, nil,
		WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
			runnerCalled.Add(1)
			return "ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	makeProp := func(id string) model.OpsProposal {
		p := model.OpsProposal{
			ProposalID:      id,
			HouseholdID:     1,
			Title:           "R2 Digest Binding " + id,
			TargetComponent: "home-web",
			RiskLevel:       model.RiskLevelLow,
			ExecutionPlan:   "docker restart home-web",
			RollbackPlan:    "docker restart home-web --rollback",
			ActionSpec:      `{"action":"docker.restart","target":"home-web","params":{"grace_sec":5}}`,
			BaselineHash:    model.ComputeDefaultBaseline("home-web"),
			Status:          model.ProposalStatusApproved,
			ApprovedBy:      "admin-r2",
			ApprovedAt:      utils.NullTime{Time: time.Now().UTC().Truncate(time.Second), Valid: true},
			Version:         1,
			Revision:        1,
			CreatedAt:       time.Now().UTC(),
		}
		digest, err := model.ComputeProposalApprovalDigest(&p)
		if err != nil {
			t.Fatalf("ComputeProposalApprovalDigest failed: %v", err)
		}
		p.ApprovalDigest = digest
		if err := db.Create(&p).Error; err != nil {
			t.Fatalf("db.Create failed: %v", err)
		}
		return p
	}

	t.Run("TamperRiskLevelPostApprovalRejected", func(t *testing.T) {
		p := makeProp("r2-tamper-risk")
		if err := db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("risk_level", model.RiskLevelCritical).Error; err != nil {
			t.Fatalf("db tamper failed: %v", err)
		}
		_, err := executor.ExecuteProposal(context.Background(), &p)
		if !errors.Is(err, ErrApprovalDigestMismatch) {
			t.Fatalf("expected ErrApprovalDigestMismatch on risk_level tamper, got %v", err)
		}
	})

	t.Run("TamperRollbackPlanPostApprovalRejected", func(t *testing.T) {
		p := makeProp("r2-tamper-rb")
		if err := db.Model(&model.OpsProposal{}).Where("id = ?", p.ID).Update("rollback_plan", "noop").Error; err != nil {
			t.Fatalf("db tamper failed: %v", err)
		}
		_, err := executor.ExecuteProposal(context.Background(), &p)
		if !errors.Is(err, ErrApprovalDigestMismatch) {
			t.Fatalf("expected ErrApprovalDigestMismatch on rollback_plan tamper, got %v", err)
		}
	})

	if runnerCalled.Load() != 0 {
		t.Fatalf("expected 0 runner calls on tampered proposals, got %d", runnerCalled.Load())
	}
}

// TestR2_UpdateProposal_InvalidatesApprovalAndRequiresFreshApproval verifies R2.2 & R2.5:
// Modifying RiskLevel, RollbackPlan, ActionSpec, ExecutionPlan, ExecutionPolicy, TargetComponent,
// or ScheduledAt on an APPROVED/SCHEDULED proposal clears ApprovalDigest, resets Status to PENDING,
// increments Version, blocks execution until re-approved, and succeeds after fresh approval.
func TestR2_UpdateProposal_InvalidatesApprovalAndRequiresFreshApproval(t *testing.T) {
	model.SetApprovalHMACKeyringForTest("k1", map[string]string{
		"k1": "super-secret-hmac-key-material-r2-0123456789abcdef",
	})
	defer model.SetApprovalHMACKeyringForTest("", nil)

	db := setupR4DigestDB(t)
	bus := eventbus.New()
	var execCount atomic.Int64
	executor := NewOperationExecutor(
		db, bus, nil, nil,
		WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
			execCount.Add(1)
			return "ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	h := newInstantOfflineHandler(t, db, bus)
	h.SetExecutor(executor)
	r := makeAdminRouter(h)

	patchCases := []struct {
		name  string
		patch string
	}{
		{"RiskLevel", `{"risk_level":"CRITICAL"}`},
		{"RollbackPlan", `{"rollback_plan":"restore backup snapshot"}`},
		{"ActionSpec", `{"action_spec":"{\"action\":\"docker.restart\",\"target\":\"home-web\",\"params\":{\"grace_sec\":15}}"}`},
		{"ExecutionPlan", `{"execution_plan":"docker restart home-web --time=15"}`},
		{"ExecutionPolicy", `{"execution_policy":"strict"}`},
		{"TargetComponent", `{"target_component":"home-api","action_spec":"{\"action\":\"docker.restart\",\"target\":\"home-api\"}"}`},
		{"ScheduledAt", `{"scheduled_at":"04:00"}`},
	}

	for idx, tc := range patchCases {
		t.Run("InvalidateOn_"+tc.name, func(t *testing.T) {
			propID := fmt.Sprintf("r2-patch-%d-%s", idx, tc.name)
			p := model.OpsProposal{
				ProposalID:      propID,
				HouseholdID:     1,
				Title:           "Patch Invalidation " + tc.name,
				TargetComponent: "home-web",
				RiskLevel:       model.RiskLevelLow,
				ExecutionPlan:   "docker restart home-web",
				RollbackPlan:    "docker restart home-web",
				ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
				Status:          model.ProposalStatusApproved,
				Version:         1,
				Revision:        1,
				CreatedAt:       time.Now().UTC(),
			}
			seedTestSignedProposal(t, db, &p)
			origDigest := p.ApprovalDigest

			// 1. PATCH bound field
			wPatch := httptest.NewRecorder()
			reqPatch, _ := http.NewRequest(http.MethodPatch, "/api/v1/ops/proposals/"+propID, strings.NewReader(tc.patch))
			reqPatch.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(wPatch, reqPatch)
			if wPatch.Code != http.StatusOK {
				t.Fatalf("expected 200 OK on PATCH %s, got %d: %s", tc.name, wPatch.Code, wPatch.Body.String())
			}

			var updated model.OpsProposal
			if err := db.Where("proposal_id = ?", propID).First(&updated).Error; err != nil {
				t.Fatalf("reload failed: %v", err)
			}
			if updated.Status != model.ProposalStatusPending {
				t.Fatalf("expected status PENDING after modifying %s, got %s", tc.name, updated.Status)
			}
			if updated.ApprovalDigest != "" {
				t.Fatalf("expected ApprovalDigest cleared after modifying %s, got %q", tc.name, updated.ApprovalDigest)
			}
			if updated.ApprovedBy != "" || updated.ApprovedAt.Valid {
				t.Fatalf("expected ApprovedBy/ApprovedAt cleared after modifying %s", tc.name)
			}
			if updated.Version != 2 {
				t.Fatalf("expected Version incremented to 2 after modifying %s, got %d", tc.name, updated.Version)
			}

			// 2. Execution MUST be blocked until fresh approval
			wExecBefore := httptest.NewRecorder()
			reqExecBefore, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/"+propID+"/execute", nil)
			r.ServeHTTP(wExecBefore, reqExecBefore)
			if wExecBefore.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 rejecting unapproved modified proposal, got %d: %s", wExecBefore.Code, wExecBefore.Body.String())
			}

			// 3. Re-approve with fresh human approval -> new digest bound to Version 2
			wApp := httptest.NewRecorder()
			reqApp, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/"+propID+"/approve?sync=true", strings.NewReader(`{"action":"now"}`))
			reqApp.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(wApp, reqApp)
			if wApp.Code != http.StatusOK {
				t.Fatalf("expected 200 OK on fresh re-approval, got %d: %s", wApp.Code, wApp.Body.String())
			}

			var reapproved model.OpsProposal
			_ = db.Where("proposal_id = ?", propID).First(&reapproved).Error
			if reapproved.ApprovalDigest == "" || reapproved.ApprovalDigest == origDigest {
				t.Fatalf("expected new ApprovalDigest distinct from old digest after re-approval")
			}
			if reapproved.Status != model.ProposalStatusExecuted {
				t.Fatalf("expected EXECUTED after fresh sync re-approval, got %s", reapproved.Status)
			}
		})
	}

	t.Run("RejectInvalidRiskLevelAndCombinedActionSpecPolicyUpdate", func(t *testing.T) {
		propID := "r2-patch-validation"
		p := model.OpsProposal{
			ProposalID:      propID,
			HouseholdID:     1,
			Title:           "Validation Test",
			TargetComponent: "home-web",
			RiskLevel:       model.RiskLevelLow,
			ActionSpec:      `{"action":"docker.restart","target":"home-web","execution_policy":"strict"}`,
			ExecutionPolicy: "strict",
			Status:          model.ProposalStatusApproved,
			Version:         1,
			Revision:        1,
			CreatedAt:       time.Now().UTC(),
		}
		seedTestSignedProposal(t, db, &p)

		// Invalid risk_level rejected 400 without mutating DB
		wBad := httptest.NewRecorder()
		reqBad, _ := http.NewRequest(http.MethodPatch, "/api/v1/ops/proposals/"+propID, strings.NewReader(`{"risk_level":"SUPER_ADMIN"}`))
		reqBad.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(wBad, reqBad)
		if wBad.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid risk_level, got %d: %s", wBad.Code, wBad.Body.String())
		}

		// Simultaneous update of action_spec + execution_policy succeeds & invalidates approval
		wBoth := httptest.NewRecorder()
		reqBoth, _ := http.NewRequest(http.MethodPatch, "/api/v1/ops/proposals/"+propID, strings.NewReader(`{"action_spec":"{\"action\":\"docker.restart\",\"target\":\"home-web\",\"execution_policy\":\"guarded\"}","execution_policy":"guarded"}`))
		reqBoth.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(wBoth, reqBoth)
		if wBoth.Code != http.StatusOK {
			t.Fatalf("expected 200 OK updating action_spec + execution_policy together, got %d: %s", wBoth.Code, wBoth.Body.String())
		}

		var reloaded model.OpsProposal
		_ = db.Where("proposal_id = ?", propID).First(&reloaded).Error
		if reloaded.Status != model.ProposalStatusPending || reloaded.Version != 2 || reloaded.ExecutionPolicy != "guarded" {
			t.Fatalf("unexpected state after combined update: %+v", reloaded)
		}
	})
}

// TestR2_UpdateProposal_AuthPermissionsAndIDOR verifies R2.6:
// UpdateProposal route registration, admin permission checks, internal daemon rejection,
// multi-tenant household isolation, and cross-household IDOR protection.
func TestR2_UpdateProposal_AuthPermissionsAndIDOR(t *testing.T) {
	db := setupR4DigestDB(t)
	bus := eventbus.New()
	h := newInstantOfflineHandler(t, db, bus)

	opsToken := "test-internal-ops-token-with-over-32-chars-entropy-r2"
	t.Setenv("INTERNAL_OPS_TOKEN", opsToken)

	pHH1 := model.OpsProposal{
		ProposalID:      "r2-idor-hh1",
		HouseholdID:     1,
		Title:           "Household 1 Proposal",
		TargetComponent: "home-web",
		RiskLevel:       model.RiskLevelLow,
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
		Status:          model.ProposalStatusApproved,
		CreatedAt:       time.Now().UTC(),
	}
	seedTestSignedProposal(t, db, &pHH1)

	t.Run("UnauthenticatedPatchRejected401", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		h.RegisterProposalRoutes(r.Group("/api/v1/ops"))
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPatch, "/api/v1/ops/proposals/r2-idor-hh1", strings.NewReader(`{"risk_level":"HIGH"}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for unauthenticated PATCH, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("NonAdminUserPatchRejected403", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		rg := r.Group("/api/v1/ops")
		rg.Use(func(c *gin.Context) {
			c.Set("user_id", uint(99))
			c.Set("user_name", "member")
			c.Set("is_admin", false)
			c.Set("household_id", uint(1))
			c.Next()
		})
		h.RegisterProposalRoutes(rg)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPatch, "/api/v1/ops/proposals/r2-idor-hh1", strings.NewReader(`{"risk_level":"HIGH"}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for non-admin PATCH, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("InternalServicePatchRejected403", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		rg := r.Group("/api/v1/ops")
		rg.Use(middleware.OpsFlexibleAuth(nil, nil))
		h.RegisterProposalRoutes(rg)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPatch, "/api/v1/ops/proposals/r2-idor-hh1", strings.NewReader(`{"risk_level":"HIGH"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Internal-Secret", opsToken)
		req.RemoteAddr = "127.0.0.1:12345"
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for internal token PATCH, got %d: %s", w.Code, w.Body.String())
		}

		// Defense-in-depth direct handler check when is_internal_service=true
		wDirect := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(wDirect)
		c.Request, _ = http.NewRequest(http.MethodPatch, "/api/v1/ops/proposals/r2-idor-hh1", strings.NewReader(`{"risk_level":"HIGH"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = gin.Params{{Key: "id", Value: "r2-idor-hh1"}}
		c.Set("is_internal_service", true)
		c.Set("is_admin", false)
		h.UpdateProposal(c)
		if wDirect.Code != http.StatusForbidden {
			t.Fatalf("expected 403 from UpdateProposal defense-in-depth for internal service, got %d", wDirect.Code)
		}
	})

	t.Run("CrossHouseholdAdminPatchIDORRejected404", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		rg := r.Group("/api/v1/ops")
		rg.Use(func(c *gin.Context) {
			c.Set("user_id", uint(20))
			c.Set("user_name", "admin-hh2")
			c.Set("is_admin", true)
			c.Set("household_id", uint(2))
			c.Next()
		})
		h.RegisterProposalRoutes(rg)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPatch, "/api/v1/ops/proposals/r2-idor-hh1", strings.NewReader(`{"risk_level":"HIGH"}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found for cross-household PATCH IDOR, got %d: %s", w.Code, w.Body.String())
		}

		// Verify Household 1 proposal remained untouched
		var intact model.OpsProposal
		_ = db.Where("proposal_id = ?", "r2-idor-hh1").First(&intact).Error
		if intact.RiskLevel != model.RiskLevelLow || intact.Status != model.ProposalStatusApproved {
			t.Fatalf("HH1 proposal mutated by HH2 caller! %+v", intact)
		}
	})
}

// TestR5_PreApprovalBaselineAndPrefixedContainerResolution verifies R5 requirements on the proposal side:
// - Empty target_component with forged baseline_hash cannot bypass CreateProposal
// - ActionSpec with forged baseline_hash is rejected BEFORE approval (ApproveProposal)
// - ComputeFingerprint matches prefixed container names (frigate -> home-frigate, openresty -> 1Panel-openresty-WuDU)
// - Strict/production mode rejects offline placeholders, unknown state, missing container, and API timeout
func TestR5_PreApprovalBaselineAndPrefixedContainerResolution(t *testing.T) {
	db := setupR4DigestDB(t)
	bus := eventbus.New()

	t.Run("CreateProposal_EmptyTargetWithForgedBaselineRejected", func(t *testing.T) {
		h := newInstantOfflineHandler(t, db, bus)
		r := makeAdminRouter(h)
		payload := `{"title":"Forged Baseline","target_component":"","baseline_hash":"sha256:forged123456","execution_plan":"docker restart home-web"}`
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 rejecting empty target_component with forged baseline, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("ApproveProposal_RejectsActionSpecBaselineOverridePreApproval", func(t *testing.T) {
		h := newInstantOfflineHandler(t, db, bus)
		r := makeAdminRouter(h)

		p := model.OpsProposal{
			ProposalID:      "r5-preapprove-override",
			HouseholdID:     1,
			Title:           "Pre-Approval ActionSpec Baseline Override",
			TargetComponent: "home-web",
			RiskLevel:       model.RiskLevelLow,
			Status:          model.ProposalStatusAwaitingApproval,
			BaselineHash:    model.ComputeDefaultBaseline("home-web"),
			ActionSpec:      `{"action":"docker.restart","target":"home-web","baseline_hash":"sha256:forged_override_0000"}`,
			Version:         1,
			Revision:        1,
			CreatedAt:       time.Now().UTC(),
		}
		if err := db.Create(&p).Error; err != nil {
			t.Fatalf("seed failed: %v", err)
		}

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/r5-preapprove-override/approve", strings.NewReader(`{"action":"now"}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409 Conflict rejecting ActionSpec baseline override at approval time, got %d: %s", w.Code, w.Body.String())
		}

		var reloaded model.OpsProposal
		_ = db.Where("proposal_id = ?", "r5-preapprove-override").First(&reloaded).Error
		if reloaded.Status != model.ProposalStatusAwaitingApproval || reloaded.ApprovalDigest != "" {
			t.Fatalf("expected proposal to remain AWAITING_APPROVAL without digest, got %+v", reloaded)
		}
	})

	t.Run("PrefixedContainerLookupAndStrictModeFailClosed", func(t *testing.T) {
		var mu sync.Mutex
		mode := "prefixed"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			curMode := mode
			mu.Unlock()
			if strings.Contains(r.URL.Path, "/api/v2/containers/search") {
				if curMode == "timeout" {
					time.Sleep(2500 * time.Millisecond)
					return
				}
				var items []onePanelContainerItem
				switch curMode {
				case "prefixed":
					items = []onePanelContainerItem{
						{Name: "/home-frigate", Image: "ghcr.io/blakeblackshear/frigate:0.14.0", State: "running", Created: "2026-11-01T00:00:00Z"},
						{Name: "/1Panel-openresty-WuDU", Image: "1panel/openresty:1.21", State: "running", Created: "2026-11-02T00:00:00Z"},
					}
				case "unknown_state":
					items = []onePanelContainerItem{
						{Name: "/home-frigate", Image: "ghcr.io/blakeblackshear/frigate:0.14.0", State: "unknown", Created: "2026-11-01T00:00:00Z"},
					}
				case "missing":
					items = []onePanelContainerItem{}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"code":    200,
					"message": "OK",
					"data":    map[string]interface{}{"items": items, "total": len(items)},
				})
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		client := NewOnePanelClient(srv.URL, "btpanel", "u", "p")
		client.psession = "tok"
		client.lastLoginAt = time.Now()
		client.loginTTL = time.Hour

		h := &OpsHandler{client: client, fallback: NewHostFallbackProvider("", ""), db: db, bus: bus}

		// When 1Panel succeeds and omits the container in non-prod mode, do NOT fall back to static catalog
		mu.Lock()
		mode = "missing"
		mu.Unlock()
		if fp := h.ComputeFingerprint("frigate"); fp == model.ComputeDefaultBaseline("frigate") {
			t.Fatalf("expected ComputeFingerprint not to return static catalog baseline when 1Panel succeeded and omitted container, got %q", fp)
		}

		t.Setenv("OPS_STRICT_BASELINE", "1")
		mu.Lock()
		mode = "prefixed"
		mu.Unlock()
		fpFrigate := h.ComputeFingerprint("frigate")
		if fpFrigate == "" || IsOfflinePlaceholderBaseline("frigate", fpFrigate) {
			t.Fatalf("expected live fingerprint for prefixed home-frigate, got %q", fpFrigate)
		}
		fpOpenResty := h.ComputeFingerprint("openresty")
		if fpOpenResty == "" || IsOfflinePlaceholderBaseline("openresty", fpOpenResty) {
			t.Fatalf("expected live fingerprint for prefixed 1Panel-openresty-WuDU, got %q", fpOpenResty)
		}

		// Unknown container state -> empty (fail-closed)
		mu.Lock()
		mode = "unknown_state"
		mu.Unlock()
		if fp := h.ComputeFingerprint("frigate"); fp != "" {
			t.Fatalf("expected empty fingerprint for unknown container state, got %q", fp)
		}

		// Missing container in strict mode -> empty (no offline placeholder)
		mu.Lock()
		mode = "missing"
		mu.Unlock()
		if fp := h.ComputeFingerprint("frigate"); fp != "" {
			t.Fatalf("expected empty fingerprint for missing container in strict mode, got %q", fp)
		}

		// API timeout in strict mode -> empty
		mu.Lock()
		mode = "timeout"
		mu.Unlock()
		if fp := h.ComputeFingerprint("frigate"); fp != "" {
			t.Fatalf("expected empty fingerprint on 1Panel timeout in strict mode, got %q", fp)
		}

		// Placeholder hash cannot pass ApproveProposal in strict mode
		pPlaceholder := model.OpsProposal{
			ProposalID:      "r5-placeholder-reject",
			HouseholdID:     1,
			Title:           "Placeholder Baseline Reject",
			TargetComponent: "home-web",
			RiskLevel:       model.RiskLevelLow,
			Status:          model.ProposalStatusAwaitingApproval,
			BaselineHash:    model.ComputeDefaultBaseline("home-web"),
			ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
			Version:         1,
			Revision:        1,
			CreatedAt:       time.Now().UTC(),
		}
		_ = db.Create(&pPlaceholder).Error
		r := makeAdminRouter(h)
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/r5-placeholder-reject/approve", strings.NewReader(`{"action":"now"}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409 rejecting offline placeholder baseline in strict mode, got %d: %s", w.Code, w.Body.String())
		}
	})
}

// TestR2_PatchVsExecuteRace_CASMutualExclusion verifies concurrent PATCH /proposals/:id vs POST /execute.
func TestR2_PatchVsExecuteRace_CASMutualExclusion(t *testing.T) {
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

	h := newInstantOfflineHandler(t, db, bus)
	h.SetExecutor(executor)
	r := makeAdminRouter(h)

	for i := 0; i < 30; i++ {
		propID := fmt.Sprintf("r2-patch-exec-race-%d", i)
		p := model.OpsProposal{
			ProposalID:      propID,
			HouseholdID:     1,
			Title:           "Patch vs Execute Race",
			TargetComponent: "home-web",
			RiskLevel:       model.RiskLevelLow,
			Status:          model.ProposalStatusApproved,
			ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
			Version:         1,
			Revision:        1,
			CreatedAt:       time.Now().UTC(),
		}
		seedTestSignedProposal(t, db, &p)

		before := execCount.Load()
		barrier := make(chan struct{})
		var wg sync.WaitGroup
		var codePatch, codeExec int

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-barrier
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodPatch, "/api/v1/ops/proposals/"+propID, strings.NewReader(`{"risk_level":"CRITICAL"}`))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			codePatch = w.Code
		}()
		go func() {
			defer wg.Done()
			<-barrier
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/"+propID+"/execute", nil)
			r.ServeHTTP(w, req)
			codeExec = w.Code
		}()
		close(barrier)
		wg.Wait()

		if codePatch == http.StatusOK && codeExec == http.StatusOK {
			t.Fatalf("round %d CRITICAL: both PATCH and EXECUTE returned 200 OK", i)
		}
		delta := execCount.Load() - before
		var finalProp model.OpsProposal
		_ = db.Where("proposal_id = ?", propID).First(&finalProp).Error
		if codePatch == http.StatusOK {
			if delta != 0 || finalProp.Status != model.ProposalStatusPending {
				t.Fatalf("round %d PATCH won but delta=%d status=%s", i, delta, finalProp.Status)
			}
		} else if codeExec == http.StatusOK {
			if delta != 1 || finalProp.Status != model.ProposalStatusExecuted {
				t.Fatalf("round %d EXECUTE won but delta=%d status=%s", i, delta, finalProp.Status)
			}
		} else {
			t.Fatalf("round %d unexpected dual error: patch=%d exec=%d status=%s", i, codePatch, codeExec, finalProp.Status)
		}
	}
}

// TestR2_InMemoryVsDBDigestDisagreementRejected verifies R2.3 & R2.4:
// DB record is authoritative at execution time, and if the in-memory OpsProposal disagrees
// with the DB digest or bound fields, ExecuteProposal rejects execution with ErrApprovalDigestMismatch.
func TestR2_InMemoryVsDBDigestDisagreementRejected(t *testing.T) {
	model.SetApprovalHMACKeyringForTest("k1", map[string]string{
		"k1": "super-secret-hmac-key-material-r2-0123456789abcdef",
	})
	defer model.SetApprovalHMACKeyringForTest("", nil)

	db := setupR4DigestDB(t)
	var runnerCalled atomic.Int64
	executor := NewOperationExecutor(
		db, eventbus.New(), nil, nil,
		WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
			runnerCalled.Add(1)
			return "ok", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)

	p := model.OpsProposal{
		ProposalID:      "r2-mem-vs-db-001",
		HouseholdID:     1,
		Title:           "Memory vs DB Integrity",
		TargetComponent: "home-web",
		RiskLevel:       model.RiskLevelLow,
		ExecutionPlan:   "docker restart home-web",
		RollbackPlan:    "docker restart home-web",
		ExecutionPolicy: "strict",
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
		BaselineHash:    model.ComputeDefaultBaseline("home-web"),
		Status:          model.ProposalStatusApproved,
		ApprovedBy:      "admin",
		ApprovedAt:      utils.NullTime{Time: time.Now().UTC().Truncate(time.Second), Valid: true},
		Version:         1,
		Revision:        1,
		CreatedAt:       time.Now().UTC(),
	}
	digest, err := model.ComputeProposalApprovalDigest(&p)
	if err != nil {
		t.Fatalf("ComputeProposalApprovalDigest failed: %v", err)
	}
	p.ApprovalDigest = digest
	if err := db.Create(&p).Error; err != nil {
		t.Fatalf("db.Create failed: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(m *model.OpsProposal)
	}{
		{"DifferentDigest", func(m *model.OpsProposal) { m.ApprovalDigest = "hmac-sha256:k1:" + strings.Repeat("0", 64) }},
		{"DifferentRiskLevel", func(m *model.OpsProposal) { m.RiskLevel = model.RiskLevelCritical }},
		{"DifferentRollbackPlan", func(m *model.OpsProposal) { m.RollbackPlan = "tampered rollback" }},
		{"DifferentExecutionPlan", func(m *model.OpsProposal) { m.ExecutionPlan = "tampered exec" }},
		{"DifferentVersion", func(m *model.OpsProposal) { m.Version = 99 }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			memCopy := p
			tc.mutate(&memCopy)
			_, execErr := executor.ExecuteProposal(context.Background(), &memCopy)
			if !errors.Is(execErr, ErrApprovalDigestMismatch) {
				t.Fatalf("%s: expected ErrApprovalDigestMismatch, got %v", tc.name, execErr)
			}
		})
	}
	if runnerCalled.Load() != 0 {
		t.Fatalf("expected 0 runner calls on memory/DB mismatch, got %d", runnerCalled.Load())
	}
}

