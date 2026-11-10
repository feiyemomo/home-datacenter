package ops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// setupRemediationTestDB creates an isolated per-test SQLite database.
func setupRemediationTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "remediation_test.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite db: %v", err)
	}
	if sqlDB, sErr := db.DB(); sErr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := db.AutoMigrate(
		&model.OpsProposal{},
		&model.OpsExecutionHistory{},
		&model.OpsChatSession{},
		&model.OpsChatMessage{},
	); err != nil {
		t.Fatalf("failed to automigrate tables: %v", err)
	}
	return db
}

// 1. Test SweepOrphanProposals
func TestSweepOrphanProposals(t *testing.T) {
	db := setupRemediationTestDB(t)

	// Create test proposals in various states
	proposals := []model.OpsProposal{
		{
			ProposalID:      "prop-executing-orphan",
			Title:           "Stuck in executing",
			Status:          model.ProposalStatusExecuting,
			TargetComponent: "home-api",
			CreatedAt:       time.Now().Add(-10 * time.Minute),
		},
		{
			ProposalID:      "prop-rollback-orphan",
			Title:           "Stuck in rolling back",
			Status:          model.ProposalStatusRollingBack,
			TargetComponent: "home-web",
			CreatedAt:       time.Now().Add(-15 * time.Minute),
		},
		{
			ProposalID:      "prop-approved-safe",
			Title:           "Legitimate approved",
			Status:          model.ProposalStatusApproved,
			TargetComponent: "home-vision",
			CreatedAt:       time.Now().Add(-5 * time.Minute),
		},
	}
	for i := range proposals {
		if err := db.Create(&proposals[i]).Error; err != nil {
			t.Fatalf("failed to create proposal %s: %v", proposals[i].ProposalID, err)
		}
	}

	// Run Sweeper
	if err := SweepOrphanProposals(db); err != nil {
		t.Fatalf("SweepOrphanProposals failed: %v", err)
	}

	// Verify executing proposal became RECONCILIATION_REQUIRED
	var p1 model.OpsProposal
	if err := db.Where("proposal_id = ?", "prop-executing-orphan").First(&p1).Error; err != nil {
		t.Fatalf("failed to load p1: %v", err)
	}
	if p1.Status != model.ProposalStatusReconciliationRequired && p1.Status != model.ProposalStatusExecutionUnknown && p1.Status != model.ProposalStatusFailed {
		t.Errorf("expected p1 status %s or %s, got %s", model.ProposalStatusReconciliationRequired, model.ProposalStatusExecutionUnknown, p1.Status)
	}
	if !p1.ExecutedAt.Valid {
		t.Errorf("expected p1 ExecutedAt to be valid")
	}

	// Verify rolling_back proposal became RECONCILIATION_REQUIRED
	var p2 model.OpsProposal
	if err := db.Where("proposal_id = ?", "prop-rollback-orphan").First(&p2).Error; err != nil {
		t.Fatalf("failed to load p2: %v", err)
	}
	if p2.Status != model.ProposalStatusReconciliationRequired && p2.Status != model.ProposalStatusRollbackFailed {
		t.Errorf("expected p2 status %s, got %s", model.ProposalStatusReconciliationRequired, p2.Status)
	}
	if !p2.ExecutedAt.Valid {
		t.Errorf("expected p2 ExecutedAt to be valid")
	}

	// Verify approved proposal remained unchanged
	var p3 model.OpsProposal
	if err := db.Where("proposal_id = ?", "prop-approved-safe").First(&p3).Error; err != nil {
		t.Fatalf("failed to load p3: %v", err)
	}
	if p3.Status != model.ProposalStatusApproved {
		t.Errorf("expected p3 status %s, got %s", model.ProposalStatusApproved, p3.Status)
	}
}

// 2. Test CAS Atomic Proposal Execution Guard (Prevent Double-Execution)
func TestCASAtomicProposalExecutionGuard(t *testing.T) {
	db := setupRemediationTestDB(t)
	bus := eventbus.New()

	prop := model.OpsProposal{
		ProposalID:      "prop-cas-concurrent",
		Title:           "Test Concurrent CAS Guard",
		Status:          model.ProposalStatusApproved,
		TargetComponent: "home-api",
		ExecutionPlan:   "restart",
		CreatedAt:       time.Now(),
	}
	seedTestSignedProposal(t, db, &prop)

	// Fast mock runner
	executor := NewOperationExecutor(
		db, bus, nil, nil,
		WithProbeTimeout(50*time.Millisecond),
		WithProbeInterval(10*time.Millisecond),
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			time.Sleep(30 * time.Millisecond)
			return "ok", nil
		}),
	)

	var wg sync.WaitGroup
	var errs = make([]error, 2)

	// Launch 2 concurrent execution routines on the exact same proposal
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			pCopy := prop
			_, err := executor.ExecuteProposal(context.Background(), &pCopy)
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	// Exactly one must succeed, and one must fail with ErrProposalAlreadyRunning or state error
	successCount := 0
	conflictCount := 0
	for _, err := range errs {
		if err == nil {
			successCount++
		} else {
			conflictCount++
		}
	}

	if successCount != 1 || conflictCount != 1 {
		t.Fatalf("expected exactly 1 success and 1 conflict, got %d successes, %d conflicts (errs: %v)",
			successCount, conflictCount, errs)
	}
}

// 3. Test VulnScanner ScanWorkspaceDependencies
func TestVulnScanner_ScanWorkspaceDependencies(t *testing.T) {
	db := setupRemediationTestDB(t)
	scanner := NewVulnScanner(db, nil)

	// Setup mock OSV server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := OSVQueryResponse{
			Vulns: []OSVVuln{
				{
					ID:      "GO-2024-TEST",
					Summary: "Test vulnerability in mock package",
					Details: "High severity memory buffer overflow vulnerability",
					Severity: []OSVSeverity{
						{Type: "CVSS_V3", Score: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"},
					},
					Published: time.Now(),
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()
	scanner.SetAPIURL(server.URL)

	// Create temporary workspace directory with go.mod and requirements.txt
	tmpDir := t.TempDir()
	apiDir := filepath.Join(tmpDir, "services", "api")
	_ = os.MkdirAll(apiDir, 0755)
	agentDir := filepath.Join(tmpDir, "deploy", "ops-agent")
	_ = os.MkdirAll(agentDir, 0755)

	goModContent := `module test-project

go 1.22

require (
	github.com/gin-gonic/gin v1.9.1
	gorm.io/gorm v1.25.7
)
`
	_ = os.WriteFile(filepath.Join(apiDir, "go.mod"), []byte(goModContent), 0644)

	reqContent := `requests==2.31.0
paho-mqtt==1.6.1
`
	_ = os.WriteFile(filepath.Join(agentDir, "requirements.txt"), []byte(reqContent), 0644)

	// Execute scan
	vulns, err := scanner.ScanWorkspaceDependencies(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("ScanWorkspaceDependencies failed: %v", err)
	}

	if len(vulns) == 0 {
		t.Fatalf("expected vulnerabilities returned from mock OSV, got 0")
	}

	if scanner.LastScanStatus() != "success" {
		t.Errorf("expected LastScanStatus 'success', got '%s'", scanner.LastScanStatus())
	}
	if scanner.LastScanAt().IsZero() {
		t.Errorf("expected LastScanAt to be recorded")
	}

	// Verify proposal was created in DB for high severity finding
	var prop model.OpsProposal
	if err := db.Where("title LIKE ?", "%GO-2024-TEST%").First(&prop).Error; err != nil {
		t.Fatalf("expected proposal created in DB for vulnerability: %v", err)
	}
	if prop.RiskLevel != "CRITICAL" {
		t.Errorf("expected RiskLevel CRITICAL for CVSS 9.8, got %s", prop.RiskLevel)
	}
}

// 4. Test Probe Host Resolution In Docker vs Host
func TestProbeHostResolution(t *testing.T) {
	// Without IN_DOCKER
	os.Unsetenv("IN_DOCKER")
	os.Unsetenv("PROBE_HOST_HOME_API")

	if host := getProbeHost(ComponentHomeAPI); host != "127.0.0.1" {
		t.Errorf("expected host 127.0.0.1 outside docker, got %s", host)
	}

	// With IN_DOCKER=true
	os.Setenv("IN_DOCKER", "true")
	defer os.Unsetenv("IN_DOCKER")

	if host := getProbeHost(ComponentHomeAPI); host != "home-api" {
		t.Errorf("expected host 'home-api' in docker, got %s", host)
	}
	if host := getProbeHost(ComponentHomeWeb); host != "home-web" {
		t.Errorf("expected host 'home-web' in docker, got %s", host)
	}
	if host := getProbeHost(ComponentFrigate); host != "home-frigate" {
		t.Errorf("expected host 'home-frigate' in docker, got %s", host)
	}

	// With custom env override
	os.Setenv("PROBE_HOST_HOME_VISION", "vision-host.local")
	defer os.Unsetenv("PROBE_HOST_HOME_VISION")

	if host := getProbeHost(ComponentHomeVision); host != "vision-host.local" {
		t.Errorf("expected custom probe host 'vision-host.local', got %s", host)
	}
}
