package ops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/security"
)

// setupAdversarialDB creates an isolated SQLite database in a temp file or in-memory
func setupAdversarialDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open in-memory sqlite database: %v", err)
	}

	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil && sqlDB != nil {
			_ = sqlDB.Close()
		}
	})

	if err := db.AutoMigrate(
		&model.OpsExecutionHistory{},
		&model.OpsProposal{},
		&model.SecurityAlertFilter{},
		&model.SecurityRecipient{},
	); err != nil {
		t.Fatalf("failed to auto-migrate: %v", err)
	}

	return db
}

// -------------------------------------------------------------
// Challenge 1: SafetyFSM Empirical Tests
// -------------------------------------------------------------

// Test 1.1: Unusual paths in VACUUM INTO
func TestSafetyFSM_Challenge1_UnusualBackupPaths(t *testing.T) {
	db := setupAdversarialDB(t)

	// Mock health probe returning 200 OK
	probeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer probeSrv.Close()

	alertMgr := security.NewAlertManager(config.AlertConfig{Enabled: false}, db)

	// Subcase A: Path containing spaces
	t.Run("PathWithSpaces", func(t *testing.T) {
		baseDir := t.TempDir()
		backupDir := filepath.Join(baseDir, "dir with spaces and # symbols")
		if err := os.MkdirAll(backupDir, 0755); err != nil {
			t.Fatalf("failed to create dir with spaces: %v", err)
		}

		fsm := NewSafetyFSM(db, nil, alertMgr, backupDir, probeSrv.URL)
		err := fsm.ExecuteSafeUpdate(context.Background(), "comp-space", "v1", "v2", nil, nil)
		if err != nil {
			t.Fatalf("expected update to succeed with path with spaces: %v", err)
		}

		entries, err := os.ReadDir(backupDir)
		if err != nil || len(entries) == 0 {
			t.Fatalf("expected snapshot created in dir with spaces, found %d entries: %v", len(entries), err)
		}
	})

	// Subcase B: Path or component name containing single quote (')
	t.Run("SingleQuotesInComponentOrPath", func(t *testing.T) {
		baseDir := t.TempDir()
		backupDir := filepath.Join(baseDir, "dir_quote")
		_ = os.MkdirAll(backupDir, 0755)

		fsm := NewSafetyFSM(db, nil, alertMgr, backupDir, probeSrv.URL)
		// Component name has a single quote: e.g. "comp'inject"
		// This tests whether SQL escaping handles single quote without syntax crash
		err := fsm.ExecuteSafeUpdate(context.Background(), "comp'inject", "v1", "v2", nil, nil)
		if err != nil {
			t.Fatalf("expected update not to crash even if component has single quote: %v", err)
		}

		var history model.OpsExecutionHistory
		if err := db.Where("component = ?", "comp'inject").First(&history).Error; err != nil {
			t.Fatalf("history should record update even if VACUUM INTO fell back: %v", err)
		}
		t.Logf("SnapshotID recorded for single-quote component: %s", history.SnapshotID)
	})

	// Subcase C: Non-existent or unwritable backup path
	t.Run("UnwritableBackupPathFallback", func(t *testing.T) {
		invalidBackupDir := filepath.Join(t.TempDir(), "nonexistent_sub", "invalid_dir")
		// Do not mkdir, or point to an impossible path
		fsm := NewSafetyFSM(db, nil, alertMgr, invalidBackupDir, probeSrv.URL)
		err := fsm.ExecuteSafeUpdate(context.Background(), "comp-unwritable", "v1", "v2", nil, nil)
		if err != nil {
			t.Fatalf("unexpected failure: %v", err)
		}

		var history model.OpsExecutionHistory
		if err := db.Where("component = ?", "comp-unwritable").First(&history).Error; err != nil {
			t.Fatalf("history record not found: %v", err)
		}
		if history.SnapshotID != "in-memory-checkpoint" {
			t.Logf("SnapshotID fallback value: %s", history.SnapshotID)
		}
	})

	// Subcase D: Genuinely unwritable snapshot path causes fast-fail
	t.Run("SnapshotFailureFastFail", func(t *testing.T) {
		tempDir := t.TempDir()
		blockedFile := filepath.Join(tempDir, "blocked_file")
		_ = os.WriteFile(blockedFile, []byte("data"), 0644)
		// Pointing to a file as directory makes MkdirAll/VACUUM INTO fail
		impossibleDir := filepath.Join(blockedFile, "sub_dir")

		fsm := NewSafetyFSM(db, nil, alertMgr, impossibleDir, probeSrv.URL)
		applied := false
		err := fsm.ExecuteSafeUpdate(context.Background(), "comp-fail-snapshot", "v1", "v2",
			func() error {
				applied = true
				return nil
			},
			nil,
		)
		if err == nil {
			t.Fatalf("expected update to fail fast when snapshot creation fails")
		}
		if applied {
			t.Fatalf("applyFn must not be executed when snapshot fails")
		}

		var history model.OpsExecutionHistory
		if err := db.Where("component = ? AND status = ?", "comp-fail-snapshot", "failed").First(&history).Error; err != nil {
			t.Fatalf("expected failed history record for snapshot failure: %v", err)
		}
	})
}

// Test 1.2: Timeout handling and Context Cancellation
// Challenge: Does SafetyFSM stop immediately and trigger rollback when ctx is canceled,
// or does it hang for the full 10-second probing loop?
func TestSafetyFSM_Challenge1_ContextCancellationResponsiveness(t *testing.T) {
	db := setupAdversarialDB(t)
	backupDir := t.TempDir()

	// Slow / hung probe that never returns 200 OK (returns 503)
	probeCalls := 0
	probeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCalls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer probeSrv.Close()

	fsm := NewSafetyFSM(db, nil, nil, backupDir, probeSrv.URL)

	applied := false
	rolledBack := false

	// Pass a context with a short timeout of 500ms
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	startTime := time.Now()
	err := fsm.ExecuteSafeUpdate(ctx, "timeout-comp", "v1", "v2",
		func() error {
			applied = true
			return nil
		},
		func() error {
			rolledBack = true
			return nil
		},
	)
	elapsed := time.Since(startTime)

	if err == nil {
		t.Fatalf("expected error due to canceled probe, got nil")
	}
	if !applied {
		t.Fatalf("expected applyFn to have run")
	}
	if !rolledBack {
		t.Fatalf("expected rollbackFn to have run")
	}

	t.Logf("Context timeout was 500ms. Elapsed execution time: %v. Total probe calls: %d", elapsed, probeCalls)

	// Context cancellation must abort the probe loop promptly (well under 3s budget for a 500ms ctx)
	if elapsed >= 3*time.Second {
		t.Fatalf("SafetyFSM health probe loop ignored context cancellation: took %v (budget was 500ms, expected < 3s)", elapsed)
	}
}

// Test 1.3: Rollback behavior when rollbackFn itself fails
func TestSafetyFSM_Challenge1_RollbackFailureHandling(t *testing.T) {
	db := setupAdversarialDB(t)
	backupDir := t.TempDir()

	// Probe always fails
	probeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer probeSrv.Close()

	fsm := NewSafetyFSM(db, nil, nil, backupDir, probeSrv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rollbackErr := errors.New("docker daemon IO error during rollback")
	err := fsm.ExecuteSafeUpdate(ctx, "failing-rb-comp", "v1", "v2",
		func() error { return nil },
		func() error { return rollbackErr },
	)

	if err == nil {
		t.Fatalf("expected error from failed probe, got nil")
	}
	if !errors.Is(err, rollbackErr) {
		t.Fatalf("expected returned error to wrap rollbackErr (%v), got: %v", rollbackErr, err)
	}

	// Verify history record captured rollback failure detail
	var history model.OpsExecutionHistory
	if err := db.Where("component = ? AND status = ?", "failing-rb-comp", "rolled_back").First(&history).Error; err != nil {
		t.Fatalf("history record not found: %v", err)
	}

	t.Logf("History Detail captured: %s", history.Detail)
	if history.Detail == "" {
		t.Errorf("history detail should capture rollback result")
	}
}

// Test 1.4: Concurrency and Race Safety
func TestSafetyFSM_Challenge1_ConcurrencyAndBusyGuard(t *testing.T) {
	db := setupAdversarialDB(t)
	backupDir := t.TempDir()

	// Probe blocks for 500ms
	probeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer probeSrv.Close()

	fsm := NewSafetyFSM(db, nil, nil, backupDir, probeSrv.URL)

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	successCount := 0
	busyCount := 0
	var mu sync.Mutex

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			err := fsm.ExecuteSafeUpdate(ctx, fmt.Sprintf("concurrent-comp-%d", id), "v1", "v2",
				func() error {
					time.Sleep(100 * time.Millisecond)
					return nil
				},
				nil,
			)

			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successCount++
			} else {
				busyCount++
			}
		}(i)
	}

	wg.Wait()

	t.Logf("Concurrency results: %d succeeded, %d rejected due to busy guard", successCount, busyCount)
	if successCount == 0 {
		t.Fatalf("expected at least one update to succeed")
	}
	if busyCount == 0 {
		t.Fatalf("expected concurrent callers to be rejected by busy guard")
	}
}

// -------------------------------------------------------------
// Challenge 2: VulnScanner Empirical Tests
// -------------------------------------------------------------

// Test 2.1: Malformed JSON from Google OSV
func TestVulnScanner_Challenge2_MalformedJSONHandling(t *testing.T) {
	malformedBodies := []string{
		`{not-json-at-all}`,
		`{"vulns": [`,
		`{"vulns": "this should be an array"}`,
		``,
		`<html><body>502 Bad Gateway</body></html>`,
		`{"vulns": [{"id": 12345}]}`, // id should be string
	}

	for i, badBody := range malformedBodies {
		t.Run(fmt.Sprintf("Malformed_%d", i), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(badBody))
			}))
			defer server.Close()

			scanner := NewVulnScanner(nil, nil)
			scanner.SetAPIURL(server.URL)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			vulns, err := scanner.ScanPackage(ctx, "Go", "github.com/gin-gonic/gin", "v1.9.1")
			if err == nil && len(vulns) > 0 {
				t.Fatalf("expected error or empty vulns on malformed JSON %q, got vulns: %+v", badBody, vulns)
			}
			t.Logf("Handled malformed body gracefully with error: %v", err)
		})
	}
}

// Test 2.2: Empty Vulnerabilities from OSV
func TestVulnScanner_Challenge2_EmptyVulnerabilities(t *testing.T) {
	emptyBodies := []string{
		`{"vulns": []}`,
		`{}`,
		`{"vulns": null}`,
	}

	for i, emptyBody := range emptyBodies {
		t.Run(fmt.Sprintf("Empty_%d", i), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(emptyBody))
			}))
			defer server.Close()

			scanner := NewVulnScanner(nil, nil)
			scanner.SetAPIURL(server.URL)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			vulns, err := scanner.ScanPackage(ctx, "Go", "github.com/gin-gonic/gin", "v1.9.1")
			if err != nil {
				t.Fatalf("unexpected error on empty body %q: %v", emptyBody, err)
			}
			if len(vulns) != 0 {
				t.Fatalf("expected 0 vulnerabilities, got %d", len(vulns))
			}
		})
	}
}

// Test 2.3: Edge CVSS vector strings (CVSS v3.0, v3.1, missing scores, numeric edge cases)
func TestVulnScanner_Challenge2_EdgeCVSSVectors(t *testing.T) {
	cases := []struct {
		name         string
		vuln         OSVVuln
		wantRisk     string
		wantCritical bool
	}{
		{
			name: "CVSS v3.1 Network Critical (C:H, I:H, A:H)",
			vuln: OSVVuln{
				ID: "CVE-2024-0001",
				Severity: []OSVSeverity{
					{Type: "CVSS_V3", Score: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"},
				},
			},
			wantRisk:     "CRITICAL",
			wantCritical: true,
		},
		{
			name: "CVSS v3.0 Network High (PR:N)",
			vuln: OSVVuln{
				ID: "CVE-2024-0002",
				Severity: []OSVSeverity{
					{Type: "CVSS_V3", Score: "CVSS:3.0/AV:N/AC:H/PR:N/UI:R/S:C/C:L/I:L/A:N"},
				},
			},
			wantRisk:     "HIGH",
			wantCritical: true,
		},
		{
			name: "CVSS v3.1 Local Vector (AV:L, not AV:N, severity present)",
			vuln: OSVVuln{
				ID:      "CVE-2024-0003",
				Summary: "Local buffer issue",
				Severity: []OSVSeverity{
					{Type: "CVSS_V3", Score: "CVSS:3.1/AV:L/AC:L/PR:L/UI:N/S:U/C:H/I:H/A:H"},
				},
			},
			// AV:L does not contain AV:N, Sscanf fails, falls through to len(Severity) > 0 -> HIGH
			wantRisk:     "HIGH",
			wantCritical: true,
		},
		{
			name: "Numeric Score 10.0 Max Critical",
			vuln: OSVVuln{
				ID: "CVE-2024-0004",
				Severity: []OSVSeverity{
					{Type: "CVSS_V3", Score: "10.0"},
				},
			},
			wantRisk:     "CRITICAL",
			wantCritical: true,
		},
		{
			name: "Numeric Score 0.0 Zero Low",
			vuln: OSVVuln{
				ID: "CVE-2024-0005",
				Severity: []OSVSeverity{
					{Type: "CVSS_V3", Score: "0.0"},
				},
			},
			wantRisk:     "LOW",
			wantCritical: false,
		},
		{
			name: "Empty Severity slice with RCE keyword in summary",
			vuln: OSVVuln{
				ID:       "CVE-2024-0006",
				Summary:  "Remote code execution in request parser",
				Severity: nil,
			},
			wantRisk:     "CRITICAL",
			wantCritical: true,
		},
		{
			name: "Empty Severity slice and generic summary",
			vuln: OSVVuln{
				ID:       "CVE-2024-0007",
				Summary:  "Minor cosmetic typo in error text",
				Details:  "No security impact",
				Severity: nil,
			},
			wantRisk:     "MEDIUM",
			wantCritical: false,
		},
		{
			name: "Malformed Severity Score string without vector or numeric",
			vuln: OSVVuln{
				ID: "CVE-2024-0008",
				Severity: []OSVSeverity{
					{Type: "UNKNOWN", Score: "NOT_A_SCORE"},
				},
				Summary: "General update",
			},
			// len(v.Severity) > 0 keyword heuristic triggers HIGH
			wantRisk:     "HIGH",
			wantCritical: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			risk, isCrit := EvaluateCVSS(tc.vuln)
			if risk != tc.wantRisk || isCrit != tc.wantCritical {
				t.Errorf("EvaluateCVSS() = (%s, %v), want (%s, %v)", risk, isCrit, tc.wantRisk, tc.wantCritical)
			}
		})
	}
}

// Test 2.4: Empty ID handling in ProcessVulnerabilities does not falsely match all proposals
func TestVulnScanner_Challenge2_EmptyVulnIDHandling(t *testing.T) {
	db := setupAdversarialDB(t)

	// Pre-seed an existing proposal
	preExisting := model.OpsProposal{
		ProposalID:      "prop-pre-existing-1",
		Title:           "现有提案：备份数据库",
		TargetComponent: "backup",
		Status:          "AWAITING_APPROVAL",
		CreatedAt:       time.Now(),
	}
	if err := db.Create(&preExisting).Error; err != nil {
		t.Fatalf("failed to create pre-existing proposal: %v", err)
	}

	scanner := NewVulnScanner(db, nil)

	// Process a vulnerability that has empty ID
	vulns := []OSVVuln{
		{
			ID:      "",
			Summary: "Anonymous vulnerability with empty ID",
			Details: "Should not match pre-existing proposal with LIKE %%",
		},
	}
	scanner.ProcessVulnerabilities(vulns, "test-pkg", "v1.0.0")

	// Verify that the empty ID vulnerability created a proposal and was NOT dropped
	var proposals []model.OpsProposal
	if err := db.Find(&proposals).Error; err != nil {
		t.Fatalf("failed to query proposals: %v", err)
	}

	// We expect 2 proposals: the pre-existing one + the new one
	if len(proposals) != 2 {
		t.Fatalf("expected 2 proposals (1 pre-existing + 1 new), got %d. Empty ID was falsely deduplicated!", len(proposals))
	}
}

