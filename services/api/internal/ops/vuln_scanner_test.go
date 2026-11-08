package ops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/security"
)

func setupVulnTestDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.OpsProposal{},
		&model.SecurityAlertFilter{},
		&model.SecurityRecipient{},
	); err != nil {
		t.Fatalf("failed to auto-migrate vuln test tables: %v", err)
	}
	return db
}

func TestVulnScanner_ScanPackageMock(t *testing.T) {
	mockResp := OSVQueryResponse{
		Vulns: []OSVVuln{
			{
				ID:        "GO-2024-2611",
				Summary:   "Improper input validation in net/http",
				Details:   "A critical remote code execution flaw in HTTP request processing allows arbitrary code execution.",
				Aliases:   []string{"CVE-2024-24790"},
				Published: time.Now(),
				Severity: []OSVSeverity{
					{
						Type:  "CVSS_V3",
						Score: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H",
					},
				},
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST method, got %s", r.Method)
		}
		var req OSVQueryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}
		if req.Package.Name != "net/http" || req.Package.Ecosystem != "Go" {
			t.Errorf("unexpected query package: %+v", req)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mockResp)
	}))
	defer server.Close()

	scanner := NewVulnScanner(nil, nil)
	scanner.SetAPIURL(server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	vulns, err := scanner.ScanPackage(ctx, "Go", "net/http", "v1.21.0")
	if err != nil {
		t.Fatalf("ScanPackage failed: %v", err)
	}

	if len(vulns) != 1 {
		t.Fatalf("expected 1 vuln, got %d", len(vulns))
	}
	if vulns[0].ID != "GO-2024-2611" {
		t.Fatalf("expected ID GO-2024-2611, got %s", vulns[0].ID)
	}
	if len(vulns[0].Aliases) == 0 || vulns[0].Aliases[0] != "CVE-2024-24790" {
		t.Fatalf("expected alias CVE-2024-24790, got %+v", vulns[0].Aliases)
	}
}

func TestVulnScanner_CVSS_Evaluation(t *testing.T) {
	// 1. Critical vector
	criticalVuln := OSVVuln{
		ID:      "CVE-2024-9999",
		Summary: "Critical RCE",
		Details: "Remote code execution vulnerability in core library",
		Severity: []OSVSeverity{
			{Type: "CVSS_V3", Score: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"},
		},
	}
	risk, isCrit := EvaluateCVSS(criticalVuln)
	if risk != "CRITICAL" || !isCrit {
		t.Fatalf("expected CRITICAL risk, got %s (isCritical=%v)", risk, isCrit)
	}

	// 2. High numeric score
	highVuln := OSVVuln{
		ID:      "CVE-2024-8888",
		Summary: "Privilege escalation",
		Details: "Allows local privilege escalation",
		Severity: []OSVSeverity{
			{Type: "CVSS_V3", Score: "7.8"},
		},
	}
	riskHigh, isCritHigh := EvaluateCVSS(highVuln)
	if riskHigh != "HIGH" || !isCritHigh {
		t.Fatalf("expected HIGH risk, got %s (isCritical=%v)", riskHigh, isCritHigh)
	}

	// 3. Medium numeric score
	medVuln := OSVVuln{
		ID:      "CVE-2024-5555",
		Summary: "Minor information disclosure",
		Details: "Leaking internal timestamp in debug logs",
		Severity: []OSVSeverity{
			{Type: "CVSS_V3", Score: "5.3"},
		},
	}
	riskMed, isCritMed := EvaluateCVSS(medVuln)
	if riskMed != "MEDIUM" || isCritMed {
		t.Fatalf("expected MEDIUM risk and not critical, got %s (isCritical=%v)", riskMed, isCritMed)
	}

	// 4. Keyword heuristic
	keywordVuln := OSVVuln{
		ID:      "CVE-2024-4444",
		Summary: "SQL Injection in parameter handling",
		Details: "Unsanitized user input leading to sql injection",
	}
	riskKey, isCritKey := EvaluateCVSS(keywordVuln)
	if riskKey != "CRITICAL" || !isCritKey {
		t.Fatalf("expected CRITICAL from sql injection keyword, got %s", riskKey)
	}
}

func TestVulnScanner_ProcessVulnerabilities_CreatesProposalAndDeduplicates(t *testing.T) {
	db := setupVulnTestDB(t)
	alertMgr := security.NewAlertManager(config.AlertConfig{Enabled: false}, db)

	scanner := NewVulnScanner(db, alertMgr)

	testVulns := []OSVVuln{
		{
			ID:        "GO-2024-3001",
			Summary:   "Buffer overflow in compression engine",
			Details:   "Remote attackers can trigger buffer overflow via crafted archive",
			Aliases:   []string{"CVE-2024-3001"},
			Published: time.Now(),
			Severity: []OSVSeverity{
				{Type: "CVSS_V3", Score: "8.5"},
			},
		},
	}

	// First pass: creates proposal
	scanner.ProcessVulnerabilities(testVulns, "archive-lib", "v2.0.0")

	var prop model.OpsProposal
	if err := db.Where("target_component = ?", "archive-lib").First(&prop).Error; err != nil {
		t.Fatalf("expected proposal to be created: %v", err)
	}
	if prop.Status != "AWAITING_APPROVAL" {
		t.Fatalf("expected status AWAITING_APPROVAL, got %s", prop.Status)
	}
	if prop.RiskLevel != "HIGH" {
		t.Fatalf("expected RiskLevel HIGH, got %s", prop.RiskLevel)
	}

	// Second pass: deduplication
	scanner.ProcessVulnerabilities(testVulns, "archive-lib", "v2.0.0")

	var count int64
	db.Model(&model.OpsProposal{}).Where("target_component = ?", "archive-lib").Count(&count)
	if count != 1 {
		t.Fatalf("expected exactly 1 proposal after duplicate scan, got %d", count)
	}
}

func TestVulnScanner_CriticalAlertFilter(t *testing.T) {
	db := setupVulnTestDB(t)
	alertMgr := security.NewAlertManager(config.AlertConfig{Enabled: false}, db)

	// Filter disabled: CriticalVuln = false
	filter := alertMgr.GetAlertFilters()
	filter.CriticalVuln = false
	alertMgr.UpdateAlertFilters(filter)

	scanner := NewVulnScanner(db, alertMgr)

	testVuln := []OSVVuln{
		{
			ID:        "GO-2024-9000",
			Summary:   "RCE flaw",
			Details:   "arbitrary code execution",
			Published: time.Now(),
		},
	}

	// Should not panic or fail, proposal is created
	scanner.ProcessVulnerabilities(testVuln, "core-api", "v1.0.0")

	var prop model.OpsProposal
	if err := db.Where("target_component = ?", "core-api").First(&prop).Error; err != nil {
		t.Fatalf("expected proposal created even when alert is disabled: %v", err)
	}
}

func TestVulnScanner_ScanDefaultPackages(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(OSVQueryResponse{Vulns: nil})
	}))
	defer server.Close()

	scanner := NewVulnScanner(nil, nil)
	scanner.SetAPIURL(server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := scanner.ScanDefaultPackages(ctx)
	if err != nil {
		t.Fatalf("ScanDefaultPackages failed: %v", err)
	}

	if requestCount != len(DefaultScanTargets) {
		t.Fatalf("expected %d requests for default targets, got %d", len(DefaultScanTargets), requestCount)
	}
	if scanner.LastScanAt().IsZero() {
		t.Fatalf("expected LastScanAt to be updated")
	}
}
