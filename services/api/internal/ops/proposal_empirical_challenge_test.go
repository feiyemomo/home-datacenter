package ops

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/middleware"
	"home-datacenter-api/internal/model"
)

// setupOpsEmpiricalTestDB sets up an in-memory test database for ops challenge testing.
func setupOpsEmpiricalTestDB(t *testing.T) (*gorm.DB, model.Household, model.Household, model.User, model.User) {
	t.Helper()
	dbName := fmt.Sprintf("file:ops_empirical_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open in-memory sqlite db: %v", err)
	}

	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
		_, _ = sqlDB.Exec("PRAGMA busy_timeout = 5000;")
	}

	if err := db.AutoMigrate(
		&model.Household{},
		&model.User{},
		&model.Device{},
		&model.OpsProposal{},
		&model.SystemLog{},
	); err != nil {
		t.Fatalf("failed to auto-migrate: %v", err)
	}

	hA := model.Household{ID: 1, Name: "Household A"}
	hB := model.Household{ID: 2, Name: "Household B"}
	db.Create(&hA)
	db.Create(&hB)

	adminA := model.User{ID: 10, Name: "admin_A", HouseholdID: 1, IsAdmin: true}
	adminB := model.User{ID: 20, Name: "admin_B", HouseholdID: 2, IsAdmin: true}
	db.Create(&adminA)
	db.Create(&adminB)

	return db, hA, hB, adminA, adminB
}

// ============================================================================
// Objective 5: Anti-IDOR Cross-Household Proposal Boundary
// ============================================================================
func TestEmpiricalChallenge5_ProposalCrossHouseholdIDOR(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, _, _, adminA, adminB := setupOpsEmpiricalTestDB(t)
	bus := eventbus.New()
	handler := NewOpsHandler(nil, nil, db, bus, nil)

	// Seed proposal for Household A
	propA := model.OpsProposal{
		ProposalID:      "prop-hA-001",
		Title:           "Proposal of Household A",
		HouseholdID:     adminA.HouseholdID,
		TargetComponent: "home-web",
		RiskLevel:       model.RiskLevelLow,
		Status:          model.ProposalStatusAwaitingApproval,
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
		CreatedAt:       time.Now(),
	}
	db.Create(&propA)

	// Seed proposal for Household B
	propB := model.OpsProposal{
		ProposalID:      "prop-hB-002",
		Title:           "Proposal of Household B",
		HouseholdID:     adminB.HouseholdID,
		TargetComponent: "home-api",
		RiskLevel:       model.RiskLevelLow,
		Status:          model.ProposalStatusAwaitingApproval,
		ActionSpec:      `{"action":"docker.restart","target":"home-api"}`,
		CreatedAt:       time.Now(),
	}
	db.Create(&propB)

	// Router setup for caller in Household B
	rB := gin.New()
	rgB := rB.Group("/api/v1/ops")
	rgB.Use(func(c *gin.Context) {
		c.Set("user_id", adminB.ID)
		c.Set("user_name", adminB.Name)
		c.Set("is_admin", adminB.IsAdmin)
		c.Set("household_id", adminB.HouseholdID) // Household 2
		c.Next()
	})
	handler.RegisterProposalRoutes(rgB)

	// 1. ListProposals: Admin B should ONLY see proposals belonging to Household B
	t.Run("ListProposals Filters Out Other Household Proposals", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/ops/proposals", nil)
		rB.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}
		var list []model.OpsProposal
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatalf("failed to parse list response: %v", err)
		}
		for _, p := range list {
			if p.HouseholdID != adminB.HouseholdID {
				t.Fatalf("CRITICAL IDOR LEAK: proposal from Household %d leaked to Household %d", p.HouseholdID, adminB.HouseholdID)
			}
		}
		if len(list) != 1 || list[0].ProposalID != "prop-hB-002" {
			t.Fatalf("expected exactly 1 proposal belonging to Household B, got %d", len(list))
		}
	})

	// 2. ApproveProposal IDOR: Admin B attempts to approve Proposal of Household A -> Must return 404 Not Found
	t.Run("ApproveProposal Rejects Cross-Household Access with 404", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-hA-001/approve", strings.NewReader(`{"action":"now"}`))
		req.Header.Set("Content-Type", "application/json")
		rB.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("CRITICAL IDOR VULNERABILITY: expected 404 Not Found for cross-household proposal approve, got %d: %s", w.Code, w.Body.String())
		}
	})

	// 3. ExecuteProposalEndpoint IDOR: Admin B attempts to execute Proposal of Household A -> Must return 404 Not Found
	t.Run("ExecuteProposal Rejects Cross-Household Access with 404", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-hA-001/execute", nil)
		rB.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("CRITICAL IDOR VULNERABILITY: expected 404 Not Found for cross-household proposal execute, got %d: %s", w.Code, w.Body.String())
		}
	})
}

// ============================================================================
// Objective 6: Internal Token Anti-Escalation on Proposal Approval & Admin Routes
// ============================================================================
func TestEmpiricalChallenge6_InternalTokenCannotApproveProposalOrAdminRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, _, _, adminA, _ := setupOpsEmpiricalTestDB(t)
	bus := eventbus.New()
	handler := NewOpsHandler(nil, nil, db, bus, nil)

	opsToken := "test-internal-ops-token-with-over-32-chars-entropy-random"
	t.Setenv("INTERNAL_OPS_TOKEN", opsToken)

	// Seed proposal
	prop := model.OpsProposal{
		ProposalID:      "prop-internal-guard-001",
		Title:           "Proposal for Internal Token Guard Test",
		HouseholdID:     adminA.HouseholdID,
		TargetComponent: "home-web",
		RiskLevel:       model.RiskLevelLow,
		Status:          model.ProposalStatusAwaitingApproval,
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
		CreatedAt:       time.Now(),
	}
	db.Create(&prop)

	// Setup router with OpsFlexibleAuth
	r := gin.New()
	rg := r.Group("/api/v1/ops")
	rg.Use(middleware.OpsFlexibleAuth(nil, nil))
	handler.RegisterProposalRoutes(rg)

	// 1. Internal token attempting POST /api/v1/ops/proposals/:id/approve -> MUST receive HTTP 403 Forbidden
	t.Run("Internal Token Calling Proposal Approval Returns 403 Forbidden", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-internal-guard-001/approve", strings.NewReader(`{"action":"now"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Internal-Secret", opsToken)
		req.RemoteAddr = "127.0.0.1:12345"
		r.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("CRITICAL PRIVILEGE ESCALATION: internal daemon was not forbidden from approving proposals! Got %d: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "cannot approve proposals") && !strings.Contains(w.Body.String(), "admin only") {
			t.Fatalf("expected admin-only / daemon rejection message, got: %s", w.Body.String())
		}
	})

	// 2. Internal token attempting POST /api/v1/ops/maintenance/lock -> MUST receive HTTP 403 Forbidden
	t.Run("Internal Token Calling Maintenance Lock Returns 403 Forbidden", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/maintenance/lock", strings.NewReader(`{"reason":"daemon trying to lock"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Internal-Secret", opsToken)
		req.RemoteAddr = "127.0.0.1:12345"
		r.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("CRITICAL ESCALATION: internal daemon acquired maintenance lock! Got %d: %s", w.Code, w.Body.String())
		}
	})

	// 3. Internal token attempting POST /api/v1/ops/containers/:name/reset-circuit -> MUST receive HTTP 403 Forbidden
	t.Run("Internal Token Calling Reset Circuit Returns 403 Forbidden", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/containers/home-api/reset-circuit", nil)
		req.Header.Set("X-Internal-Secret", opsToken)
		req.RemoteAddr = "127.0.0.1:12345"
		r.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("CRITICAL ESCALATION: internal daemon reset circuit! Got %d: %s", w.Code, w.Body.String())
		}
	})

	// 4. In contrast, genuine admin user calling proposal approve -> succeeds (200 OK)
	t.Run("Genuine Admin User Can Approve Proposal", func(t *testing.T) {
		rAdmin := gin.New()
		rgAdmin := rAdmin.Group("/api/v1/ops")
		rgAdmin.Use(func(c *gin.Context) {
			c.Set("user_id", adminA.ID)
			c.Set("user_name", adminA.Name)
			c.Set("is_admin", true)
			c.Set("household_id", adminA.HouseholdID)
			c.Next()
		})
		handler.RegisterProposalRoutes(rgAdmin)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-internal-guard-001/approve", strings.NewReader(`{"action":"schedule","schedule_time":"03:00"}`))
		req.Header.Set("Content-Type", "application/json")
		rAdmin.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for genuine admin approval, got %d: %s", w.Code, w.Body.String())
		}
	})
}
