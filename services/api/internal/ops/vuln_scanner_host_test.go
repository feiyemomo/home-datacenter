package ops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"home-datacenter-api/internal/hostsec"
	"home-datacenter-api/internal/model"
)

type stubHostProvider struct {
	snap *hostsec.HostAssetSnapshot
}

func (s *stubHostProvider) CollectAssets(_ context.Context) (*hostsec.HostAssetSnapshot, error) {
	return s.snap, nil
}

func TestVulnScanner_ScanPackagePaginationAndCVSS40(t *testing.T) {
	var callNum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callNum.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_ = json.NewEncoder(w).Encode(OSVQueryResponse{
				Vulns: []OSVVuln{{
					ID:       "CVE-2024-1",
					Summary:  "First page vuln",
					Severity: []OSVSeverity{{Type: "CVSS_V4", Score: "CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:H/VI:H/VA:H"}},
				}},
				NextPageToken: "page-2",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(OSVQueryResponse{
			Vulns: []OSVVuln{{
				ID:      "CVE-2024-10",
				Summary: "Second page vuln",
			}},
		})
	}))
	defer server.Close()

	scanner := NewVulnScanner(nil, nil)
	scanner.SetAPIURL(server.URL)

	vulns, err := scanner.ScanPackage(context.Background(), "Go", "github.com/gin-gonic/gin", "v1.9.1")
	if err != nil {
		t.Fatalf("ScanPackage failed: %v", err)
	}
	if len(vulns) != 2 {
		t.Fatalf("expected 2 paginated vulns, got %d", len(vulns))
	}
	risk, crit := EvaluateCVSS(vulns[0])
	if risk != "CRITICAL" || !crit {
		t.Fatalf("expected CVSS 4.0 AV:N/VC:H to be CRITICAL, got %s (crit=%v)", risk, crit)
	}
}

func TestVulnScanner_NoSubstringCollisionDedup(t *testing.T) {
	db := setupVulnTestDB(t)
	scanner := NewVulnScanner(db, nil)

	// Process CVE-2024-10 first, then CVE-2024-1 — must NOT suppress CVE-2024-1
	scanner.ProcessVulnerabilities([]OSVVuln{{ID: "CVE-2024-10", Summary: "Vuln 10"}}, "gin", "v1.9.1")
	scanner.ProcessVulnerabilities([]OSVVuln{{ID: "CVE-2024-1", Summary: "Vuln 1"}}, "gin", "v1.9.1")

	var count int64
	db.Model(&model.OpsProposal{}).Count(&count)
	if count != 2 {
		t.Fatalf("expected 2 distinct proposals for CVE-2024-10 and CVE-2024-1, got %d", count)
	}
}

func TestVulnScanner_ScanHostAssets_DebianVsFnOSProposals(t *testing.T) {
	debFeed, err := hostsec.ParseDebianTrackerJSON([]byte(`{
		"openssh": {
			"CVE-2024-6387": {
				"description": "regreSSHion",
				"releases": {
					"bookworm": {"status": "resolved", "fixed_version": "1:9.2p1-2+deb12u3", "urgency": "high"}
				}
			}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}

	// 1. Debian host with unpatched openssh-server -> creates host.pkg_upgrade proposal
	dbDeb := setupVulnTestDB(t)
	scannerDeb := NewVulnScanner(dbDeb, nil)
	scannerDeb.SetHostSecurityProvider(&stubHostProvider{
		snap: &hostsec.HostAssetSnapshot{
			Platform: hostsec.PlatformInfo{
				OSFamily:            hostsec.OSFamilyDebian,
				VersionID:           "12",
				Codename:            "bookworm",
				PackageManager:      hostsec.PkgManagerAptDpkg,
				HostUpdateSupported: true,
			},
			Packages: []hostsec.OSPackage{
				{Name: "openssh-server", SourceName: "openssh", Version: "1:9.2p1-2+deb12u2"},
			},
		},
	})
	scannerDeb.SetCorrelationEngine(hostsec.NewCorrelationEngine(hostsec.CorrelationConfig{
		OSVURL:           " ",
		StaticDebianFeed: debFeed,
	}))

	repDeb, err := scannerDeb.ScanHostAssets(context.Background())
	if err != nil {
		t.Fatalf("ScanHostAssets Debian failed: %v", err)
	}
	if len(repDeb.VulnerableFindings) != 1 {
		t.Fatalf("expected 1 vulnerable finding, got %d", len(repDeb.VulnerableFindings))
	}
	var debProp model.OpsProposal
	if err := dbDeb.First(&debProp).Error; err != nil {
		t.Fatalf("expected host CVE proposal on Debian: %v", err)
	}
	var spec OperationSpec
	if err := json.Unmarshal([]byte(debProp.ActionSpec), &spec); err != nil {
		t.Fatalf("expected valid ActionSpec on Debian host proposal: %v", err)
	}
	if spec.Action != hostsec.ActionHostPackageUpgrade {
		t.Fatalf("expected action %s, got %s", hostsec.ActionHostPackageUpgrade, spec.Action)
	}

	// 2. fnOS host with unpatched openssh-server -> creates read-only advisory proposal (NO ActionDockerRestart / NO host.pkg_upgrade)
	dbFn := setupVulnTestDB(t)
	scannerFn := NewVulnScanner(dbFn, nil)
	scannerFn.SetHostSecurityProvider(&stubHostProvider{
		snap: &hostsec.HostAssetSnapshot{
			Platform: hostsec.PlatformInfo{
				OSFamily:            hostsec.OSFamilyFnOS,
				IsFnOS:              true,
				VersionID:           "12",
				Codename:            "bookworm",
				PackageManager:      hostsec.PkgManagerTrimManaged,
				HostUpdateSupported: false,
			},
			Packages: []hostsec.OSPackage{
				{Name: "openssh-server", SourceName: "openssh", Version: "1:9.2p1-2+deb12u2"},
			},
		},
	})
	scannerFn.SetCorrelationEngine(hostsec.NewCorrelationEngine(hostsec.CorrelationConfig{
		OSVURL:           " ",
		StaticDebianFeed: debFeed,
	}))

	if _, err := scannerFn.ScanHostAssets(context.Background()); err != nil {
		t.Fatalf("ScanHostAssets fnOS failed: %v", err)
	}
	var fnProp model.OpsProposal
	if err := dbFn.First(&fnProp).Error; err != nil {
		t.Fatalf("expected advisory proposal on fnOS: %v", err)
	}
	if fnProp.ActionSpec != "" {
		t.Fatalf("expected empty ActionSpec (read-only advisory) on fnOS, got %s", fnProp.ActionSpec)
	}
}
