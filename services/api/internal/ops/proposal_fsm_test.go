package ops

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

func setupProposalFSMTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbName := fmt.Sprintf("file:prop_fsm_test_%d?mode=memory&cache=shared", time.Now().UnixNano())
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

// TestProposalFSM_TransitionCAS verifies atomic CAS state transitions.
func TestProposalFSM_TransitionCAS(t *testing.T) {
	db := setupProposalFSMTestDB(t)
	fsm := NewProposalFSM(db)

	prop := model.OpsProposal{
		ProposalID:      "prop-fsm-001",
		Title:           "CAS Transition Test",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusAwaitingApproval,
		CreatedAt:       time.Now(),
	}
	if err := db.Create(&prop).Error; err != nil {
		t.Fatalf("failed to create proposal: %v", err)
	}

	// 1. Valid transition: AWAITING_APPROVAL -> APPROVED
	won, err := fsm.TransitionCAS("prop-fsm-001", []string{model.ProposalStatusAwaitingApproval}, model.ProposalStatusApproved, nil)
	if err != nil {
		t.Fatalf("unexpected error on valid CAS transition: %v", err)
	}
	if !won {
		t.Fatalf("expected CAS transition to succeed, got won=false")
	}

	var updated model.OpsProposal
	db.Where("proposal_id = ?", "prop-fsm-001").First(&updated)
	if updated.Status != model.ProposalStatusApproved {
		t.Fatalf("expected status %s, got %s", model.ProposalStatusApproved, updated.Status)
	}

	// 2. Invalid transition: Attempting CAS from AWAITING_APPROVAL when status is APPROVED
	won, err = fsm.TransitionCAS("prop-fsm-001", []string{model.ProposalStatusAwaitingApproval}, model.ProposalStatusExecuting, nil)
	if err != nil {
		t.Fatalf("unexpected error on invalid CAS: %v", err)
	}
	if won {
		t.Fatalf("expected CAS transition to fail due to status mismatch, but won=true")
	}

	// 3. Transition to CANCELLED from APPROVED
	won, err = fsm.TransitionCAS("prop-fsm-001", []string{model.ProposalStatusApproved}, model.ProposalStatusCancelled, map[string]interface{}{
		"execution_log": "cancelled by test",
	})
	if err != nil || !won {
		t.Fatalf("expected CAS to CANCELLED to succeed, got won=%v, err=%v", won, err)
	}

	db.Where("proposal_id = ?", "prop-fsm-001").First(&updated)
	if updated.Status != model.ProposalStatusCancelled {
		t.Fatalf("expected status %s, got %s", model.ProposalStatusCancelled, updated.Status)
	}
	if updated.ExecutionLog != "cancelled by test" {
		t.Fatalf("expected execution log updated, got %s", updated.ExecutionLog)
	}
}

// TestCancelProposal_Endpoint verifies POST /api/v1/ops/proposals/:id/cancel
func TestCancelProposal_Endpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupProposalFSMTestDB(t)
	bus := eventbus.New()
	handler := NewOpsHandler(nil, nil, db, bus, nil)

	r := gin.New()
	rg := r.Group("/api/v1/ops")
	handler.RegisterProposalRoutes(rg)

	// Helper to create proposal
	createProp := func(id, status string, householdID uint) {
		p := model.OpsProposal{
			ProposalID:      id,
			Title:           "Proposal " + id,
			TargetComponent: "home-api",
			Status:          status,
			HouseholdID:     householdID,
			CreatedAt:       time.Now(),
		}
		db.Create(&p)
	}

	// Case 1: Cancel from AWAITING_APPROVAL -> HTTP 200
	createProp("prop-cancel-1", model.ProposalStatusAwaitingApproval, 1)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-cancel-1/cancel", nil)
	w := httptest.NewRecorder()
	// Set mock auth context
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	c.Params = gin.Params{{Key: "id", Value: "prop-cancel-1"}}
	c.Set("is_admin", true)
	c.Set("user_name", "admin-tester")
	c.Set("household_id", uint(1))
	handler.CancelProposal(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 for cancel, got %d: %s", w.Code, w.Body.String())
	}
	var p1 model.OpsProposal
	db.Where("proposal_id = ?", "prop-cancel-1").First(&p1)
	if p1.Status != model.ProposalStatusCancelled {
		t.Fatalf("expected status CANCELLED, got %s", p1.Status)
	}

	// Case 2: Cancel while EXECUTING -> HTTP 409 Conflict
	createProp("prop-cancel-executing", model.ProposalStatusExecuting, 1)
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-cancel-executing/cancel", nil)
	c.Params = gin.Params{{Key: "id", Value: "prop-cancel-executing"}}
	c.Set("is_admin", true)
	c.Set("household_id", uint(1))
	handler.CancelProposal(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected HTTP 409 when cancelling EXECUTING proposal, got %d: %s", w.Code, w.Body.String())
	}

	// Case 3: Cancel while ROLLING_BACK -> HTTP 409 Conflict
	createProp("prop-cancel-rolling-back", model.ProposalStatusRollingBack, 1)
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-cancel-rolling-back/cancel", nil)
	c.Params = gin.Params{{Key: "id", Value: "prop-cancel-rolling-back"}}
	c.Set("is_admin", true)
	c.Set("household_id", uint(1))
	handler.CancelProposal(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected HTTP 409 when cancelling ROLLING_BACK proposal, got %d: %s", w.Code, w.Body.String())
	}

	// Case 4: Anti-IDOR check -> foreign household proposal returns 404
	createProp("prop-cancel-foreign", model.ProposalStatusAwaitingApproval, 2)
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-cancel-foreign/cancel", nil)
	c.Params = gin.Params{{Key: "id", Value: "prop-cancel-foreign"}}
	c.Set("is_admin", true)
	c.Set("household_id", uint(1)) // User belongs to household 1, proposal is household 2
	c.Set("is_internal_service", false)
	handler.CancelProposal(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected HTTP 404 for cross-household proposal cancel, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCancelProposal_ConcurrentRaceCondition tests concurrent Execute vs Cancel race.
// Exactly one must win the CAS transition, and the other must be rejected.
func TestCancelProposal_ConcurrentRaceCondition(t *testing.T) {
	db := setupProposalFSMTestDB(t)
	fsm := NewProposalFSM(db)

	prop := model.OpsProposal{
		ProposalID:      "prop-race-001",
		Title:           "Race Test",
		TargetComponent: "home-api",
		Status:          model.ProposalStatusApproved,
		CreatedAt:       time.Now(),
	}
	db.Create(&prop)

	var wg sync.WaitGroup
	var cancelWon, executeWon bool
	var mu sync.Mutex

	wg.Add(2)
	// Goroutine 1: Execute CAS claim (APPROVED -> EXECUTING)
	go func() {
		defer wg.Done()
		won, err := fsm.TransitionCAS("prop-race-001", []string{model.ProposalStatusApproved, model.ProposalStatusScheduled}, model.ProposalStatusExecuting, nil)
		if err == nil && won {
			mu.Lock()
			executeWon = true
			mu.Unlock()
		}
	}()

	// Goroutine 2: Cancel CAS claim (APPROVED -> CANCELLED)
	go func() {
		defer wg.Done()
		won, err := fsm.TransitionCAS("prop-race-001", []string{model.ProposalStatusApproved, model.ProposalStatusScheduled, model.ProposalStatusAwaitingApproval}, model.ProposalStatusCancelled, nil)
		if err == nil && won {
			mu.Lock()
			cancelWon = true
			mu.Unlock()
		}
	}()

	wg.Wait()

	// Exactly one CAS must win
	if cancelWon && executeWon {
		t.Fatalf("CAS race invariant violated: both cancel and execute won!")
	}
	if !cancelWon && !executeWon {
		t.Fatalf("CAS race invariant violated: neither cancel nor execute won!")
	}

	var finalProp model.OpsProposal
	db.Where("proposal_id = ?", "prop-race-001").First(&finalProp)
	if cancelWon && finalProp.Status != model.ProposalStatusCancelled {
		t.Fatalf("expected status CANCELLED, got %s", finalProp.Status)
	}
	if executeWon && finalProp.Status != model.ProposalStatusExecuting {
		t.Fatalf("expected status EXECUTING, got %s", finalProp.Status)
	}
}

func TestArchiveProposal_Endpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupProposalFSMTestDB(t)
	bus := eventbus.New()
	handler := NewOpsHandler(nil, nil, db, bus, nil)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", uint(1))
		c.Set("user_name", "admin")
		c.Set("is_admin", true)
		c.Set("household_id", uint(1))
		c.Next()
	})
	handler.RegisterProposalRoutes(r.Group("/api/v1/ops"))

	// Seed proposals
	activeProp := model.OpsProposal{
		ProposalID:   "prop-active-001",
		Status:       model.ProposalStatusAwaitingApproval,
		HouseholdID:  1,
		Title:        "Active Proposal",
		Version:      1,
		Revision:     1,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	executedProp := model.OpsProposal{
		ProposalID:   "prop-exec-001",
		Status:       model.ProposalStatusExecuted,
		HouseholdID:  1,
		Title:        "Executed Proposal",
		Version:      1,
		Revision:     1,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	db.Create(&activeProp)
	db.Create(&executedProp)

	// Test 1: Archiving active proposal must fail (HTTP 409 Conflict from CAS guard)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-active-001/archive", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict && w.Code != http.StatusBadRequest {
		t.Fatalf("expected 409 Conflict / 400 Bad Request for active proposal archive, got %d: %s", w.Code, w.Body.String())
	}

	// Test 2: Archiving executed proposal must succeed (HTTP 200)
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-exec-001/archive", nil)
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for executed proposal archive, got %d: %s", w2.Code, w2.Body.String())
	}

	var checked model.OpsProposal
	if err := db.Where("proposal_id = ?", "prop-exec-001").First(&checked).Error; err != nil {
		t.Fatalf("failed to query archived proposal: %v", err)
	}
	if checked.Status != model.ProposalStatusArchived {
		t.Fatalf("expected status ARCHIVED, got %s", checked.Status)
	}

	// Test 3: ListProposals excludes ARCHIVED by default
	w3 := httptest.NewRecorder()
	req3, _ := http.NewRequest(http.MethodGet, "/api/v1/ops/proposals", nil)
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for list, got %d: %s", w3.Code, w3.Body.String())
	}
	if strings.Contains(w3.Body.String(), "prop-exec-001") {
		t.Fatalf("expected archived proposal to be excluded by default, but it was present: %s", w3.Body.String())
	}

	// Test 4: ListProposals includes ARCHIVED when include_archived=true
	w4 := httptest.NewRecorder()
	req4, _ := http.NewRequest(http.MethodGet, "/api/v1/ops/proposals?include_archived=true", nil)
	r.ServeHTTP(w4, req4)
	if w4.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for list with include_archived, got %d: %s", w4.Code, w4.Body.String())
	}
	if !strings.Contains(w4.Body.String(), "prop-exec-001") {
		t.Fatalf("expected archived proposal to be included when include_archived=true: %s", w4.Body.String())
	}
}

func TestBatchArchiveProposals_Endpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupProposalFSMTestDB(t)
	bus := eventbus.New()
	handler := NewOpsHandler(nil, nil, db, bus, nil)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", uint(1))
		c.Set("user_name", "admin")
		c.Set("is_admin", true)
		c.Set("household_id", uint(1))
		c.Next()
	})
	handler.RegisterProposalRoutes(r.Group("/api/v1/ops"))

	// Seed 3 terminal proposals and 1 active proposal
	db.Create(&model.OpsProposal{ProposalID: "batch-1", Status: model.ProposalStatusExecuted, HouseholdID: 1, Title: "P1", Version: 1, Revision: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	db.Create(&model.OpsProposal{ProposalID: "batch-2", Status: model.ProposalStatusRejected, HouseholdID: 1, Title: "P2", Version: 1, Revision: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	db.Create(&model.OpsProposal{ProposalID: "batch-3", Status: model.ProposalStatusCancelled, HouseholdID: 1, Title: "P3", Version: 1, Revision: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	db.Create(&model.OpsProposal{ProposalID: "batch-active", Status: model.ProposalStatusAwaitingApproval, HouseholdID: 1, Title: "PA", Version: 1, Revision: 1, CreatedAt: time.Now(), UpdatedAt: time.Now()})

	// Call batch archive with empty body (all eligible terminal proposals)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/archive", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from batch archive, got %d: %s", w.Code, w.Body.String())
	}

	var count int64
	db.Model(&model.OpsProposal{}).Where("status = ?", model.ProposalStatusArchived).Count(&count)
	if count != 3 {
		t.Fatalf("expected exactly 3 proposals archived, got %d", count)
	}

	var activeProp model.OpsProposal
	db.Where("proposal_id = ?", "batch-active").First(&activeProp)
	if activeProp.Status != model.ProposalStatusAwaitingApproval {
		t.Fatalf("expected batch-active to remain AWAITING_APPROVAL, got %s", activeProp.Status)
	}
}
