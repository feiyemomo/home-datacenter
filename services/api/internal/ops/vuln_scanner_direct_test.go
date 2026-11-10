package ops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// 1. Test ParseGoMod with direct vs indirect dependencies
func TestParseGoMod_DirectVsIndirect(t *testing.T) {
	goModContent := `module test-project

go 1.22

require (
	github.com/gin-gonic/gin v1.9.1
	gorm.io/gorm v1.25.7
	golang.org/x/crypto v0.21.0 // indirect
	golang.org/x/net v0.22.0 // indirect; some comment
)

require github.com/stretchr/testify v1.8.4
require github.com/davecgh/go-spew v1.1.1 // indirect
`

	targets, err := ParseGoMod(goModContent)
	if err != nil {
		t.Fatalf("unexpected error parsing go.mod: %v", err)
	}

	expected := map[string]struct {
		version string
		direct  bool
	}{
		"github.com/gin-gonic/gin":    {version: "v1.9.1", direct: true},
		"gorm.io/gorm":                {version: "v1.25.7", direct: true},
		"golang.org/x/crypto":         {version: "v0.21.0", direct: false},
		"golang.org/x/net":            {version: "v0.22.0", direct: false},
		"github.com/stretchr/testify": {version: "v1.8.4", direct: true},
		"github.com/davecgh/go-spew":  {version: "v1.1.1", direct: false},
	}

	if len(targets) != len(expected) {
		t.Fatalf("expected %d targets, got %d: %+v", len(expected), len(targets), targets)
	}

	for _, tgt := range targets {
		exp, ok := expected[tgt.Package]
		if !ok {
			t.Errorf("unexpected package found: %s", tgt.Package)
			continue
		}
		if tgt.Version != exp.version {
			t.Errorf("package %s: expected version %s, got %s", tgt.Package, exp.version, tgt.Version)
		}
		if tgt.Direct != exp.direct {
			t.Errorf("package %s: expected Direct=%v, got Direct=%v", tgt.Package, exp.direct, tgt.Direct)
		}
		if tgt.Ecosystem != "Go" {
			t.Errorf("package %s: expected Ecosystem Go, got %s", tgt.Package, tgt.Ecosystem)
		}
	}
}

// 2. Test ParseRequirementsTxt sanitization (comments, env markers, hashes)
func TestParseRequirementsTxt_Sanitization(t *testing.T) {
	reqContent := `# Requirements for ops agent
requests==2.31.0 # HTTP library for API sync
paho-mqtt>=1.6.1,<2.0.0
pywin32>=306 ; sys_platform == 'win32'
urllib3==2.0.7 \
    --hash=sha256:1234567890abcdef
cryptography==41.0.5; python_version >= '3.8' # security critical

# Empty line above and invalid lines below
-r other-requirements.txt
--index-url https://pypi.org/simple
`

	targets, err := ParseRequirementsTxt(reqContent)
	if err != nil {
		t.Fatalf("unexpected error parsing requirements.txt: %v", err)
	}

	expected := map[string]string{
		"requests":     "2.31.0",
		"paho-mqtt":    "1.6.1,<2.0.0",
		"pywin32":      "306",
		"urllib3":      "2.0.7 \\",
		"cryptography": "41.0.5",
	}

	if len(targets) != len(expected) {
		t.Fatalf("expected %d targets, got %d: %+v", len(expected), len(targets), targets)
	}

	for _, tgt := range targets {
		expVer, ok := expected[tgt.Package]
		if !ok {
			t.Errorf("unexpected target parsed: %s (%s)", tgt.Package, tgt.Version)
			continue
		}
		if tgt.Version != expVer {
			t.Errorf("package %s: expected version %s, got %s", tgt.Package, expVer, tgt.Version)
		}
		if !tgt.Direct {
			t.Errorf("requirements.txt dependencies should be marked Direct=true, got false for %s", tgt.Package)
		}
		if tgt.Ecosystem != "PyPI" {
			t.Errorf("expected PyPI ecosystem, got %s", tgt.Ecosystem)
		}
	}
}

// 3. Test ScanWorkspaceDependencies strict status and no silent fallback
func TestScanWorkspaceDependencies_StrictNoSilentFallback(t *testing.T) {
	scanner := NewVulnScanner(nil, nil)

	// Mock OSV server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(OSVQueryResponse{Vulns: nil})
	}))
	defer server.Close()
	scanner.SetAPIURL(server.URL)

	// Test Empty Workspace: should fail strictly with ScanStatusFailed and NOT fall back to DefaultScanTargets
	emptyDir := t.TempDir()
	vulns, err := scanner.ScanWorkspaceDependencies(context.Background(), emptyDir)
	if err == nil {
		t.Fatalf("expected error when scanning empty directory without manifests")
	}
	if len(vulns) != 0 {
		t.Errorf("expected 0 vulns for empty directory, got: %d", len(vulns))
	}
	if scanner.LastScanStatus() != ScanStatusFailed {
		t.Errorf("expected ScanStatusFailed for empty directory, got: %s", scanner.LastScanStatus())
	}

	// Test Valid Workspace: should succeed with ScanStatusSuccess
	validDir := t.TempDir()
	apiDir := filepath.Join(validDir, "services", "api")
	_ = os.MkdirAll(apiDir, 0755)
	_ = os.WriteFile(filepath.Join(apiDir, "go.mod"), []byte("module myapp\n\ngo 1.22\n\nrequire github.com/gin-gonic/gin v1.9.1\n"), 0644)

	resValid, err := scanner.ScanWorkspaceDependencies(context.Background(), validDir)
	if err != nil {
		t.Fatalf("unexpected error scanning valid workspace: %v", err)
	}
	if scanner.LastScanStatus() != ScanStatusSuccess {
		t.Errorf("expected ScanStatusSuccess, got: %s", scanner.LastScanStatus())
	}
	if len(resValid) != 0 {
		t.Errorf("expected 0 vulnerabilities found in clean mock, got %d", len(resValid))
	}
}
