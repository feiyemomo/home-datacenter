package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/audit"
	"home-datacenter-api/internal/hostsec"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/security"
)

// OSVQueryRequest models OSV API query body.
type OSVQueryRequest struct {
	Commit    string        `json:"commit,omitempty"`
	Version   string        `json:"version,omitempty"`
	Package   OSVPackageRef `json:"package,omitempty"`
	PageToken string        `json:"page_token,omitempty"`
}

type OSVPackageRef struct {
	Name      string `json:"name"`
	Ecosystem string `json:"ecosystem"` // "Go", "PyPI", "Debian", "Debian:12", "Ubuntu"
}

// OSVQueryResponse models OSV API results.
type OSVQueryResponse struct {
	Vulns         []OSVVuln `json:"vulns"`
	NextPageToken string    `json:"next_page_token,omitempty"`
}

type OSVVuln struct {
	ID        string        `json:"id"` // e.g. "GO-2024-XXXX" or "CVE-2024-XXXX"
	Summary   string        `json:"summary"`
	Details   string        `json:"details"`
	Severity  []OSVSeverity `json:"severity,omitempty"`
	Aliases   []string      `json:"aliases,omitempty"`
	Published time.Time     `json:"published"`
}

type OSVSeverity struct {
	Type  string `json:"type"`  // e.g. "CVSS_V3", "CVSS_V4"
	Score string `json:"score"` // CVSS vector string or numeric score
}

// PackageTarget specifies an ecosystem package dependency to scan.
type PackageTarget struct {
	Component string `json:"component"`
	Ecosystem string `json:"ecosystem"` // "Go", "PyPI"
	Package   string `json:"package"`
	Version   string `json:"version"`
	Direct    bool   `json:"direct"` // true if direct dependency, false if indirect/transitive
}

// DefaultScanTargets lists baseline framework and server dependencies to monitor.
var DefaultScanTargets = []PackageTarget{
	{Component: "gin", Ecosystem: "Go", Package: "github.com/gin-gonic/gin", Version: "v1.9.1", Direct: true},
	{Component: "sqlite", Ecosystem: "Go", Package: "github.com/glebarez/sqlite", Version: "v1.11.0", Direct: true},
	{Component: "gorm", Ecosystem: "Go", Package: "gorm.io/gorm", Version: "v1.25.7", Direct: true},
}

// Scan status outcomes
const (
	ScanStatusSuccess        = "success"
	ScanStatusPartialFailure = "partial_failure"
	ScanStatusFailed         = "failed"
)

// VulnScanner integrates Google OSV, Debian Security Tracker (backport-aware),
// NVD, GHSA, and CISA KEV to evaluate both application dependencies and host OS assets.
type VulnScanner struct {
	db             *gorm.DB
	alertMgr       *security.AlertManager
	auditEngine    *audit.AgentAuditEngine
	baselineFn     func(string) string
	hostProvider   hostsec.HostSecurityProvider
	correlator     *hostsec.CorrelationEngine
	httpClient     *http.Client
	apiURL         string
	mu             sync.Mutex
	lastScanAt     time.Time
	lastScanStatus string // "success", "partial_failure", "failed", ""
	lastScanError  string
	lastHostReport *hostsec.CorrelationReport
}

// SetAuditEngine attaches an AgentAuditEngine to VulnScanner.
func (s *VulnScanner) SetAuditEngine(ae *audit.AgentAuditEngine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditEngine = ae
}

// SetBaselineExtractor configures live baseline fingerprint resolution for generated CVE proposals.
func (s *VulnScanner) SetBaselineExtractor(fn func(string) string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.baselineFn = fn
}

// SetHostSecurityProvider configures the read-only host asset provider for OS/OpenSSH/Kernel/Docker CVE correlation.
func (s *VulnScanner) SetHostSecurityProvider(p hostsec.HostSecurityProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hostProvider = p
}

// SetCorrelationEngine configures the multi-source CVE/backport correlation engine.
func (s *VulnScanner) SetCorrelationEngine(c *hostsec.CorrelationEngine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.correlator = c
}

// LastHostReport returns the most recent R9.2 host asset correlation report.
func (s *VulnScanner) LastHostReport() *hostsec.CorrelationReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastHostReport
}

func (s *VulnScanner) getAuditEngine() *audit.AgentAuditEngine {
	if s.auditEngine != nil {
		return s.auditEngine
	}
	if s.db != nil {
		s.auditEngine = audit.NewAgentAuditEngine(s.db, nil, audit.WithFailureCallback(GlobalSecurityMetrics.IncAuditFailed))
		return s.auditEngine
	}
	return nil
}

// NewVulnScanner creates a VulnScanner instance.
func NewVulnScanner(db *gorm.DB, alertMgr *security.AlertManager) *VulnScanner {
	httpClient := &http.Client{Timeout: 10 * time.Second}
	apiURL := "https://api.osv.dev/v1/query"
	return &VulnScanner{
		db:           db,
		alertMgr:     alertMgr,
		httpClient:   httpClient,
		apiURL:       apiURL,
		hostProvider: hostsec.NewDefaultHostSecurityProvider("", ""),
		correlator: hostsec.NewCorrelationEngine(hostsec.CorrelationConfig{
			OSVURL:     apiURL,
			HTTPClient: httpClient,
		}),
	}
}

// SetAPIURL configures a custom endpoint (e.g. httptest server for unit testing).
func (s *VulnScanner) SetAPIURL(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apiURL = url
}

// SetHTTPClient configures a custom HTTP client.
func (s *VulnScanner) SetHTTPClient(client *http.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.httpClient = client
}

// LastScanAt returns the timestamp of the last executed vulnerability scan.
func (s *VulnScanner) LastScanAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastScanAt
}

// LastScanStatus returns "success", "partial_failure", "failed", or "" depending on recent scan outcome.
func (s *VulnScanner) LastScanStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastScanStatus
}

// LastScanError returns the error string from the last failed scan.
func (s *VulnScanner) LastScanError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastScanError
}

// CVSS31Metrics models the Base Metrics of FIRST CVSS v3.1 specification.
type CVSS31Metrics struct {
	AV float64 // Attack Vector
	AC float64 // Attack Complexity
	PR float64 // Privileges Required
	UI float64 // User Interaction
	S  string  // Scope: "U" (Unchanged) or "C" (Changed)
	C  float64 // Confidentiality
	I  float64 // Integrity
	A  float64 // Availability
}

func roundUpCVSS(val float64) float64 {
	return math.Ceil(val*10.0) / 10.0
}

// ParseCVSS31Vector computes the exact FIRST CVSS v3.1 Base Score and severity risk level.
func ParseCVSS31Vector(vector string) (float64, string, error) {
	parts := strings.Split(vector, "/")
	if len(parts) == 0 || (!strings.HasPrefix(parts[0], "CVSS:3.1") && !strings.HasPrefix(parts[0], "CVSS:3.0")) {
		return 0, "", fmt.Errorf("invalid CVSS 3.x vector: %s", vector)
	}

	m := CVSS31Metrics{S: "U"}
	scopeChanged := false

	for _, p := range parts[1:] {
		kv := strings.SplitN(p, ":", 2)
		if len(kv) != 2 {
			continue
		}
		k, v := kv[0], kv[1]
		switch k {
		case "AV":
			switch v {
			case "N":
				m.AV = 0.85
			case "A":
				m.AV = 0.62
			case "L":
				m.AV = 0.55
			case "P":
				m.AV = 0.20
			}
		case "AC":
			switch v {
			case "L":
				m.AC = 0.77
			case "H":
				m.AC = 0.44
			}
		case "UI":
			switch v {
			case "N":
				m.UI = 0.85
			case "R":
				m.UI = 0.62
			}
		case "S":
			m.S = v
			if v == "C" {
				scopeChanged = true
			}
		case "C":
			switch v {
			case "H":
				m.C = 0.56
			case "L":
				m.C = 0.22
			case "N":
				m.C = 0.00
			}
		case "I":
			switch v {
			case "H":
				m.I = 0.56
			case "L":
				m.I = 0.22
			case "N":
				m.I = 0.00
			}
		case "A":
			switch v {
			case "H":
				m.A = 0.56
			case "L":
				m.A = 0.22
			case "N":
				m.A = 0.00
			}
		}
	}

	// PR depends on Scope
	for _, p := range parts[1:] {
		if strings.HasPrefix(p, "PR:") {
			v := strings.TrimPrefix(p, "PR:")
			if scopeChanged {
				switch v {
				case "N":
					m.PR = 0.85
				case "L":
					m.PR = 0.68
				case "H":
					m.PR = 0.50
				}
			} else {
				switch v {
				case "N":
					m.PR = 0.85
				case "L":
					m.PR = 0.62
				case "H":
					m.PR = 0.27
				}
			}
		}
	}

	// FIRST CVSS v3.1 Base Score Equations
	iss := 1.0 - ((1.0 - m.C) * (1.0 - m.I) * (1.0 - m.A))
	var impact float64
	if scopeChanged {
		impact = 7.52*(iss-0.029) - 3.25*math.Pow(iss-0.02, 15.0)
	} else {
		impact = 6.42 * iss
	}

	exploitability := 8.22 * m.AV * m.AC * m.PR * m.UI

	var baseScore float64
	if impact <= 0 {
		baseScore = 0.0
	} else if !scopeChanged {
		baseScore = roundUpCVSS(math.Min(impact+exploitability, 10.0))
	} else {
		baseScore = roundUpCVSS(math.Min(1.08*(impact+exploitability), 10.0))
	}

	var riskLevel string
	switch {
	case baseScore >= 9.0:
		riskLevel = "CRITICAL"
	case baseScore >= 7.0:
		riskLevel = "HIGH"
	case baseScore >= 4.0:
		riskLevel = "MEDIUM"
	default:
		riskLevel = "LOW"
	}

	return baseScore, riskLevel, nil
}

// EvaluateCVSS determines the risk level ("CRITICAL", "HIGH", "MEDIUM", "LOW")
// and whether it constitutes an emergency critical vulnerability requiring immediate alert.
func EvaluateCVSS(v OSVVuln) (riskLevel string, isCritical bool) {
	// 1. Check explicit CVSS severity records from OSV
	for _, sev := range v.Severity {
		if strings.HasPrefix(sev.Score, "CVSS:4.0") {
			upper := strings.ToUpper(sev.Score)
			if strings.Contains(upper, "AV:N") && (strings.Contains(upper, "VC:H") || strings.Contains(upper, "VI:H") || strings.Contains(upper, "VA:H")) {
				return "CRITICAL", true
			}
			return "HIGH", true
		}
		if strings.HasPrefix(sev.Score, "CVSS:3.1") {
			_, risk, err := ParseCVSS31Vector(sev.Score)
			if err == nil {
				return risk, (risk == "CRITICAL" || risk == "HIGH")
			}
		} else if strings.HasPrefix(sev.Score, "CVSS:3.0") {
			scoreStr := strings.ToUpper(sev.Score)
			if strings.Contains(scoreStr, "AV:N") {
				if strings.Contains(scoreStr, "C:H") && strings.Contains(scoreStr, "I:H") && strings.Contains(scoreStr, "A:H") {
					return "CRITICAL", true
				}
				if strings.Contains(scoreStr, "C:H") || strings.Contains(scoreStr, "PR:N") {
					return "HIGH", true
				}
			}
			_, risk, err := ParseCVSS31Vector(sev.Score)
			if err == nil {
				return risk, (risk == "CRITICAL" || risk == "HIGH")
			}
		} else if strings.Contains(sev.Score, "CVSS:3") {
			_, risk, err := ParseCVSS31Vector(sev.Score)
			if err == nil {
				return risk, (risk == "CRITICAL" || risk == "HIGH")
			}
		}
		var score float64
		if _, err := fmt.Sscanf(sev.Score, "%f", &score); err == nil {
			if score >= 9.0 {
				return "CRITICAL", true
			}
			if score >= 7.0 {
				return "HIGH", true
			}
			if score >= 4.0 {
				return "MEDIUM", false
			}
			return "LOW", false
		}
	}

	// 2. Keyword heuristic fallback if OSV returns no structured CVSS metrics
	text := strings.ToLower(v.Summary + " " + v.Details)
	if strings.Contains(text, "remote code execution") ||
		strings.Contains(text, "rce") ||
		strings.Contains(text, "arbitrary code execution") ||
		strings.Contains(text, "critical") ||
		strings.Contains(text, "sql injection") ||
		strings.Contains(text, "authentication bypass") ||
		strings.Contains(text, "privilege escalation") {
		return "CRITICAL", true
	}
	if strings.Contains(text, "denial of service") ||
		strings.Contains(text, "dos") ||
		strings.Contains(text, "memory corruption") ||
		strings.Contains(text, "high") ||
		strings.Contains(text, "buffer overflow") ||
		len(v.Severity) > 0 {
		return "HIGH", true
	}

	return "MEDIUM", false
}

// ParseGoMod parses go.mod content to dynamically extract dependency packages,
// accurately distinguishing direct from indirect (// indirect) dependencies.
func ParseGoMod(content string) ([]PackageTarget, error) {
	var targets []PackageTarget
	inRequireBlock := false

	lines := strings.Split(content, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || trimmed == "" {
			continue
		}
		if trimmed == "require (" {
			inRequireBlock = true
			continue
		}
		if inRequireBlock && trimmed == ")" {
			inRequireBlock = false
			continue
		}

		if inRequireBlock || strings.HasPrefix(trimmed, "require ") {
			raw := trimmed
			if strings.HasPrefix(raw, "require ") {
				raw = strings.TrimSpace(strings.TrimPrefix(raw, "require "))
			}
			isIndirect := strings.Contains(raw, "// indirect")
			clean := raw
			if idx := strings.Index(clean, "//"); idx != -1 {
				clean = strings.TrimSpace(clean[:idx])
			}
			fields := strings.Fields(clean)
			if len(fields) >= 2 {
				pkg := fields[0]
				ver := fields[1]
				parts := strings.Split(pkg, "/")
				comp := parts[len(parts)-1]
				targets = append(targets, PackageTarget{
					Component: comp,
					Ecosystem: "Go",
					Package:   pkg,
					Version:   ver,
					Direct:    !isIndirect,
				})
			}
		}
	}
	return targets, nil
}

// ParseRequirementsTxt parses Python requirements.txt to dynamically extract package dependencies.
// It sanitizes inline comments (#), environment markers (;), and hash requirements.
func ParseRequirementsTxt(content string) ([]PackageTarget, error) {
	var targets []PackageTarget
	lines := strings.Split(content, "\n")
	separators := []string{"==", ">=", "<=", "~=", ">", "<"}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
			continue
		}
		// Strip trailing inline comments
		if idx := strings.Index(trimmed, "#"); idx != -1 {
			trimmed = strings.TrimSpace(trimmed[:idx])
		}
		// Strip environment markers (e.g. '; python_version >= "3.8"')
		if idx := strings.Index(trimmed, ";"); idx != -1 {
			trimmed = strings.TrimSpace(trimmed[:idx])
		}

		pkg := trimmed
		ver := ""
		for _, sep := range separators {
			if idx := strings.Index(trimmed, sep); idx != -1 {
				pkg = strings.TrimSpace(trimmed[:idx])
				ver = strings.TrimSpace(trimmed[idx+len(sep):])
				break
			}
		}

		// Strip hash specifications if present
		if idx := strings.Index(ver, "--hash"); idx != -1 {
			ver = strings.TrimSpace(ver[:idx])
		}

		if pkg != "" {
			targets = append(targets, PackageTarget{
				Component: pkg,
				Ecosystem: "PyPI",
				Package:   pkg,
				Version:   ver,
				Direct:    true,
			})
		}
	}
	return targets, nil
}

// ExtractGoBuildInfoTargets reads Go module dependencies compiled into the running binary
// (useful in scratch/Alpine runtime containers where go.mod is not mounted).
func ExtractGoBuildInfoTargets() []PackageTarget {
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi == nil {
		return nil
	}
	var targets []PackageTarget
	if bi.GoVersion != "" {
		targets = append(targets, PackageTarget{
			Component: "go",
			Ecosystem: "Go",
			Package:   "stdlib",
			Version:   strings.TrimPrefix(bi.GoVersion, "go"),
			Direct:    true,
		})
	}
	for _, dep := range bi.Deps {
		if dep == nil || dep.Path == "" || dep.Version == "" {
			continue
		}
		parts := strings.Split(dep.Path, "/")
		targets = append(targets, PackageTarget{
			Component: parts[len(parts)-1],
			Ecosystem: "Go",
			Package:   dep.Path,
			Version:   dep.Version,
			Direct:    true,
		})
	}
	return targets
}

// ScanPackage queries Google OSV for vulnerabilities in a specific ecosystem package (with pagination).
func (s *VulnScanner) ScanPackage(ctx context.Context, ecosystem, pkgName, version string) ([]OSVVuln, error) {
	s.mu.Lock()
	apiURL := s.apiURL
	client := s.httpClient
	s.mu.Unlock()

	if client == nil {
		return nil, errors.New("http client is not configured")
	}

	var all []OSVVuln
	pageToken := ""
	for page := 0; page < 5; page++ {
		reqBody := OSVQueryRequest{
			Version:   version,
			PageToken: pageToken,
			Package: OSVPackageRef{
				Name:      pkgName,
				Ecosystem: ecosystem,
			},
		}
		payloadBytes, err := json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payloadBytes))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("Google OSV API request failed: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return nil, fmt.Errorf("Google OSV returned HTTP %d: %s", resp.StatusCode, string(body))
		}

		var res OSVQueryResponse
		decErr := json.NewDecoder(resp.Body).Decode(&res)
		_ = resp.Body.Close()
		if decErr != nil {
			return nil, fmt.Errorf("failed to decode OSV response: %w", decErr)
		}
		all = append(all, res.Vulns...)
		if strings.TrimSpace(res.NextPageToken) == "" {
			break
		}
		pageToken = res.NextPageToken
	}

	return all, nil
}

// ScanDefaultPackages scans all baseline datacenter dependencies against Google OSV.
func (s *VulnScanner) ScanDefaultPackages(ctx context.Context) error {
	successCount := 0
	failedCount := 0
	var lastErr error

	for _, target := range DefaultScanTargets {
		vulns, err := s.ScanPackage(ctx, target.Ecosystem, target.Package, target.Version)
		if err != nil {
			lastErr = err
			failedCount++
			log.Printf("[vuln-scanner] scan %s (%s) failed: %v", target.Component, target.Package, err)
			continue
		}
		successCount++
		if len(vulns) > 0 {
			s.ProcessVulnerabilities(vulns, target.Component, target.Version)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastScanAt = time.Now()

	total := len(DefaultScanTargets)
	if total == 0 {
		s.lastScanStatus = ScanStatusFailed
		s.lastScanError = "zero scan targets configured"
		return errors.New("zero scan targets configured")
	}

	if successCount == 0 && lastErr != nil {
		s.lastScanStatus = ScanStatusFailed
		s.lastScanError = lastErr.Error()
		return lastErr
	}

	if failedCount > 0 {
		s.lastScanStatus = ScanStatusPartialFailure
		s.lastScanError = fmt.Sprintf("%d of %d package scans failed (last: %v)", failedCount, total, lastErr)
		return nil
	}

	s.lastScanStatus = ScanStatusSuccess
	s.lastScanError = ""
	return nil
}

// ScanWorkspaceDependencies extracts dependencies dynamically from workspace go.mod and requirements.txt,
// scanning them against Google OSV and updating scanner status.
func (s *VulnScanner) ScanWorkspaceDependencies(ctx context.Context, workspaceRoot string) ([]OSVVuln, error) {
	var targets []PackageTarget

	// 1. Try parsing go.mod
	goModPaths := []string{
		filepath.Join(workspaceRoot, "services", "api", "go.mod"),
		filepath.Join(workspaceRoot, "go.mod"),
	}
	for _, p := range goModPaths {
		if content, err := os.ReadFile(p); err == nil {
			if parsed, err := ParseGoMod(string(content)); err == nil && len(parsed) > 0 {
				targets = append(targets, parsed...)
				break
			}
		}
	}

	// 2. Try parsing requirements.txt
	reqPaths := []string{
		filepath.Join(workspaceRoot, "deploy", "ops-agent", "requirements.txt"),
		filepath.Join(workspaceRoot, "requirements.txt"),
	}
	for _, p := range reqPaths {
		if content, err := os.ReadFile(p); err == nil {
			if parsed, err := ParseRequirementsTxt(string(content)); err == nil && len(parsed) > 0 {
				targets = append(targets, parsed...)
				break
			}
		}
	}

	if len(targets) == 0 {
		s.mu.Lock()
		s.lastScanAt = time.Now()
		s.lastScanStatus = ScanStatusFailed
		s.lastScanError = "no dependency files (go.mod / requirements.txt) found in workspace"
		s.mu.Unlock()
		return nil, errors.New("no dependency files (go.mod / requirements.txt) found in workspace")
	}

	var allVulns []OSVVuln
	successCount := 0
	failedCount := 0
	validTargetCount := 0
	var lastErr error

	for _, target := range targets {
		if strings.TrimSpace(target.Version) == "" || strings.TrimSpace(target.Package) == "" {
			continue
		}
		validTargetCount++
		vulns, err := s.ScanPackage(ctx, target.Ecosystem, target.Package, target.Version)
		if err != nil {
			lastErr = err
			failedCount++
			log.Printf("[vuln-scanner] scan %s (%s) failed: %v", target.Component, target.Package, err)
			continue
		}
		successCount++
		if len(vulns) > 0 {
			allVulns = append(allVulns, vulns...)
			s.ProcessVulnerabilities(vulns, target.Component, target.Version)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastScanAt = time.Now()

	if validTargetCount == 0 {
		s.lastScanStatus = ScanStatusFailed
		s.lastScanError = "zero valid dependencies found to scan"
		return nil, errors.New("zero valid dependencies found to scan")
	}

	if successCount == 0 && lastErr != nil {
		s.lastScanStatus = ScanStatusFailed
		s.lastScanError = lastErr.Error()
		return nil, fmt.Errorf("all %d dependency scans failed: %w", validTargetCount, lastErr)
	}

	if failedCount > 0 {
		s.lastScanStatus = ScanStatusPartialFailure
		s.lastScanError = fmt.Sprintf("%d of %d dependency scans failed (last: %v)", failedCount, validTargetCount, lastErr)
		return allVulns, nil
	}

	s.lastScanStatus = ScanStatusSuccess
	s.lastScanError = ""
	return allVulns, nil
}

// ScanHostAssets collects host OS/OpenSSH/Kernel/OpenSSL/Docker Engine/package inventory
// read-only and correlates against Debian Security Tracker (backport-aware), OSV, NVD, GHSA, and CISA KEV.
func (s *VulnScanner) ScanHostAssets(ctx context.Context) (*hostsec.CorrelationReport, error) {
	s.mu.Lock()
	hp := s.hostProvider
	corr := s.correlator
	s.mu.Unlock()

	if hp == nil {
		hp = hostsec.NewDefaultHostSecurityProvider("", "")
	}
	if corr == nil {
		corr = hostsec.NewCorrelationEngine(hostsec.CorrelationConfig{})
	}

	snap, err := hp.CollectAssets(ctx)
	if err != nil {
		return nil, err
	}
	report, err := corr.CorrelateAssets(ctx, snap)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.lastHostReport = report
	s.lastScanAt = time.Now()
	if len(report.Warnings) > 0 && len(report.SourcesQueried) == 0 {
		s.lastScanStatus = ScanStatusFailed
		s.lastScanError = strings.Join(report.Warnings, "; ")
	} else {
		s.lastScanStatus = ScanStatusSuccess
		s.lastScanError = ""
	}
	s.mu.Unlock()

	if len(report.VulnerableFindings) > 0 {
		s.ProcessHostFindings(report.VulnerableFindings)
	}
	return report, nil
}

// ScanHostAndDependencies performs both workspace/compiled-Go dependency scanning AND
// R9.2 host OS/OpenSSH/Kernel/OpenSSL/Docker Engine CVE correlation.
func (s *VulnScanner) ScanHostAndDependencies(ctx context.Context, workspaceRoot string) (*hostsec.CorrelationReport, []OSVVuln, error) {
	hostRep, hostErr := s.ScanHostAssets(ctx)

	appVulns, appErr := s.ScanWorkspaceDependencies(ctx, workspaceRoot)
	if appErr != nil {
		// In container runtime without go.mod, fall back to embedded binary build info
		if biTargets := ExtractGoBuildInfoTargets(); len(biTargets) > 0 {
			for _, target := range biTargets {
				if !target.Direct {
					continue
				}
				if v, err := s.ScanPackage(ctx, target.Ecosystem, target.Package, target.Version); err == nil && len(v) > 0 {
					appVulns = append(appVulns, v...)
					s.ProcessVulnerabilities(v, target.Component, target.Version)
				}
			}
			appErr = nil
		}
	}

	if hostErr != nil && appErr != nil {
		return hostRep, appVulns, fmt.Errorf("host scan error: %v; app scan error: %v", hostErr, appErr)
	}
	return hostRep, appVulns, hostErr
}

// ProcessHostFindings persists non-backported host CVE findings as proposals/audit events/alerts
// without misusing container restart actions for OS packages or enabling apt updates on fnOS.
func (s *VulnScanner) ProcessHostFindings(findings []hostsec.VulnerabilityFinding) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ae := s.getAuditEngine()
	for _, f := range findings {
		if !f.IsVulnerable {
			continue
		}
		idemKey := fmt.Sprintf("hostvuln:%s:%s:%s", f.CVEID, f.Package, f.InstalledVersion)
		if s.db != nil {
			var existing model.OpsProposal
			if err := s.db.Where("idempotency_key = ? AND status NOT IN ?", idemKey, []string{
				model.ProposalStatusRejected, model.ProposalStatusCancelled, model.ProposalStatusExpired,
			}).First(&existing).Error; err == nil {
				continue
			}

			var actionSpecBytes []byte
			var execPlan, rbPlan string
			if f.RemediationMode == "host_pkg_upgrade" {
				actionSpecBytes, _ = json.Marshal(OperationSpec{
					Action: hostsec.ActionHostPackageUpgrade,
					Target: "host-os",
					Params: map[string]interface{}{
						"packages":      []string{f.Package},
						"fixed_version": f.FixedVersion,
						"cve_id":        f.CVEID,
					},
				})
				execPlan = fmt.Sprintf("1. 校验 Debian/Ubuntu 宿主机兼容性与 APT GPG 签名\n2. 备份 SSH/Docker 配置与 dpkg 状态快照\n3. 仅升级白名单包 %s (%s -> %s)\n4. 执行 dpkg-query / sshd -t / SSH 连通性校验", f.Package, f.InstalledVersion, f.FixedVersion)
				rbPlan = fmt.Sprintf("恢复配置快照并降级 %s=%s，触发 SSH 死手恢复保护", f.Package, f.InstalledVersion)
			} else {
				execPlan = fmt.Sprintf("只读安全通告（%s）：请通过 fnOS 官方 OTA 或人工窗口升级 %s (%s -> %s)", f.RemediationMode, f.Package, f.InstalledVersion, f.FixedVersion)
				rbPlan = "无需回滚（只读通告提案，禁止自动执行 apt 变更）"
			}

			kevNote := ""
			if f.InCISAKEV {
				kevNote = fmt.Sprintf(" [CISA KEV 在列, 截止=%s]", f.KEVDueDate)
			}

			baseHash := ""
			if s.baselineFn != nil {
				baseHash = strings.TrimSpace(s.baselineFn("host-os"))
			}
			if baseHash == "" {
				baseHash = fmt.Sprintf("host:%s:%s", f.Package, f.InstalledVersion)
			}

			prop := model.OpsProposal{
				HouseholdID:      1,
				ProposalID:       fmt.Sprintf("prop-hostcve-%d", time.Now().UnixNano()),
				IdempotencyKey:   idemKey,
				Title:            fmt.Sprintf("宿主机漏洞 %s (%s %s)%s", f.CVEID, f.Package, f.InstalledVersion, kevNote),
				Description:      fmt.Sprintf("情报源 %v 关联发现宿主机组件 %s %s 存在 %s（修复版本: %s，补丁评估: %s）。\n\n%s", f.Sources, f.Package, f.InstalledVersion, f.CVEID, f.FixedVersion, f.PatchStatus, f.Summary),
				TargetComponent:  "host-os",
				ActionSpec:       string(actionSpecBytes),
				BaselineHash:     baseHash,
				RiskLevel:        f.RiskLevel,
				EstimatedSeconds: 120,
				DowntimeSeconds:  5,
				ExecutionPlan:    execPlan,
				RollbackPlan:     rbPlan,
				Status:           model.ProposalStatusAwaitingApproval,
				Version:          1,
				Revision:         1,
				CreatedAt:        time.Now().UTC(),
			}
			if err := s.db.Create(&prop).Error; err != nil {
				GlobalSecurityMetrics.IncDatabaseError()
				log.Printf("[vuln-scanner] failed to persist host CVE proposal %s: %v", f.CVEID, err)
			} else if ae != nil {
				s.recordVulnAuditEvents(ae, prop, f.CVEID, f.Package, f.InstalledVersion, f.RiskLevel, "host-os")
			}
		}

		if (f.RiskLevel == "CRITICAL" || f.RiskLevel == "HIGH" || f.InCISAKEV) && s.alertMgr != nil {
			filters := s.alertMgr.GetAlertFilters()
			if filters.CriticalVuln {
				if err := s.alertMgr.SendSecurityEmail(
					fmt.Sprintf("【宿主机高危 CVE】%s 影响 %s (%s)", f.CVEID, f.Package, f.InstalledVersion),
					"127.0.0.1",
					"宿主机 CVE 漏洞预警",
					fmt.Sprintf("组件 %s (%s) 命中 %v，修复版本 %s，CISA KEV=%v：%s", f.Package, f.InstalledVersion, f.Sources, f.FixedVersion, f.InCISAKEV, f.Summary),
					fmt.Sprintf("处置模式: %s", f.RemediationMode),
				); err != nil && !errors.Is(err, security.ErrAlertNotConfigured) {
					log.Printf("[vuln-scanner] host CVE alert email failed: %v", err)
				}
			}
		}
	}
}

// ProcessVulnerabilities evaluates scan results, escalates high/critical CVEs to proposals and email alerts.
func (s *VulnScanner) ProcessVulnerabilities(vulns []OSVVuln, component, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ae := s.getAuditEngine()
	resolvedExecComp, isKnownExecComp := mapKnownVulnComponent(component)
	targetComp := component
	if isKnownExecComp {
		targetComp = resolvedExecComp
	}

	for _, v := range vulns {
		riskLevel, isCritical := EvaluateCVSS(v)

		cveID := v.ID
		if len(v.Aliases) > 0 {
			cveID = fmt.Sprintf("%s (%s)", v.ID, v.Aliases[0])
		}

		if s.db != nil {
			if strings.TrimSpace(v.ID) != "" {
				idemKey := fmt.Sprintf("osv:%s:%s:%s", v.ID, component, version)
				var existing model.OpsProposal
				err := s.db.Where(
					"(idempotency_key = ? OR title LIKE ?) AND status NOT IN ?",
					idemKey,
					"修复 "+v.ID+" %",
					[]string{model.ProposalStatusRejected, model.ProposalStatusCancelled, model.ProposalStatusExpired},
				).First(&existing).Error
				if err == nil {
					continue // Already recorded active/pending proposal for exact CVE ID
				}
			}

			baselineHash := ""
			if s.baselineFn != nil {
				baselineHash = s.baselineFn(targetComp)
			}
			if baselineHash == "" && !IsProductionRuntime() {
				baselineHash = model.ComputeDefaultBaseline(targetComp)
			}
			actionTarget := targetComp
			if !isKnownExecComp {
				actionTarget = ComponentHomeAPI
			}
			actionSpecBytes, _ := json.Marshal(OperationSpec{
				Action: ActionDockerRestart,
				Target: actionTarget,
			})

			idemKey := ""
			if strings.TrimSpace(v.ID) != "" {
				idemKey = fmt.Sprintf("osv:%s:%s:%s", v.ID, component, version)
			}

			prop := model.OpsProposal{
				HouseholdID:      1,
				ProposalID:       fmt.Sprintf("prop-vuln-%d", time.Now().UnixNano()),
				IdempotencyKey:   idemKey,
				Title:            fmt.Sprintf("修复 %s: %s %s 漏洞基线加固", cveID, component, version),
				Description:      fmt.Sprintf("Google OSV 漏洞情报源预警（影响依赖 %s %s -> 宿主服务 %s）：%s。\n\n详情：%s", component, version, targetComp, v.Summary, v.Details),
				TargetComponent:  targetComp,
				ActionSpec:       string(actionSpecBytes),
				BaselineHash:     baselineHash,
				RiskLevel:        riskLevel,
				EstimatedSeconds: 45,
				DowntimeSeconds:  2,
				ExecutionPlan:    fmt.Sprintf("1. 备份当前 %s 镜像与数据快照\n2. 应用安全补丁版本并重启服务\n3. 执行 10 秒 /health 探针校验", targetComp),
				RollbackPlan:     fmt.Sprintf("docker rollback %s || restore-snapshot", targetComp),
				Status:           model.ProposalStatusAwaitingApproval,
				Version:          1,
				Revision:         1,
				CreatedAt:        time.Now().UTC(),
			}
			if err := s.db.Create(&prop).Error; err != nil {
				GlobalSecurityMetrics.IncDatabaseError()
				log.Printf("[vuln-scanner] failed to persist proposal for %s: %v", cveID, err)
			} else if ae != nil {
				s.recordVulnAuditEvents(ae, prop, cveID, component, version, riskLevel, targetComp)
			}
		}

		// Dispatch email alert if high/critical risk and enabled
		if isCritical && s.alertMgr != nil {
			filters := s.alertMgr.GetAlertFilters()
			if filters.CriticalVuln {
				if err := s.alertMgr.SendSecurityEmail(
					fmt.Sprintf("【高危安全预警】依赖情报源发现 %s 存在高危漏洞 %s", component, cveID),
					"127.0.0.1",
					"CVE漏洞预警",
					fmt.Sprintf("组件 %s (%s) 命中 Google OSV 漏洞库：%s。\n主动协作 Agent 已生成待批修补提案，请登录运维控制台审阅。", component, version, v.Summary),
					"已生成安全待批提案并锁定沙箱推演",
				); err != nil && !errors.Is(err, security.ErrAlertNotConfigured) {
					log.Printf("[vuln-scanner] alert email failed: %v", err)
				}
			}
		}

		log.Printf("[vuln-scanner] Detected %s for %s (%s), risk=%s, critical=%v", cveID, component, version, riskLevel, isCritical)
	}
}

func (s *VulnScanner) recordVulnAuditEvents(ae *audit.AgentAuditEngine, prop model.OpsProposal, cveID, pkg, version, riskLevel, targetComp string) {
	opID := GenerateOperationID("vuln")
	_, runCtx, rErr := ae.StartRun(context.Background(), audit.StartRunInput{
		HouseholdID:   prop.HouseholdID,
		TriggerSource: "vuln_scanner",
		ActorType:     "system_daemon",
		ActorID:       "vuln-scanner",
		Action:        "vuln.proposal.create",
		Target:        targetComp,
		Component:     targetComp,
		Decision:      model.AuditDecisionRequireApproval,
		RiskLevel:     riskLevel,
		ProposalID:    prop.ProposalID,
		OperationID:   opID,
		Summary:       prop.Title,
	})
	if rErr != nil {
		GlobalSecurityMetrics.IncAuditFailed()
		log.Printf("[vuln-scanner] audit StartRun failed for %s: %v", prop.ProposalID, rErr)
		return
	}
	if _, err := ae.EmitEvent(runCtx, audit.EmitEventInput{
		EventType:   model.AuditEventRiskAssessed,
		Action:      "vuln.cve.assessed",
		Target:      targetComp,
		Component:   targetComp,
		RiskLevel:   riskLevel,
		ProposalID:  prop.ProposalID,
		OperationID: opID,
		Evidence:    map[string]interface{}{"cve": cveID, "package": pkg, "version": version},
		Result:      riskLevel,
	}); err != nil {
		GlobalSecurityMetrics.IncAuditFailed()
		log.Printf("[vuln-scanner] audit EmitEvent(RiskAssessed) failed for %s: %v", prop.ProposalID, err)
	}
	if _, err := ae.EmitEvent(runCtx, audit.EmitEventInput{
		EventType:    model.AuditEventProposalCreated,
		Action:       "vuln.proposal.create",
		Target:       targetComp,
		Component:    targetComp,
		Decision:     model.AuditDecisionRequireApproval,
		RiskLevel:    riskLevel,
		ProposalID:   prop.ProposalID,
		OperationID:  opID,
		AfterState:   map[string]interface{}{"status": prop.Status, "baseline_hash": prop.BaselineHash},
		Result:       "CREATED",
		MirrorLegacy: true,
	}); err != nil {
		GlobalSecurityMetrics.IncAuditFailed()
		log.Printf("[vuln-scanner] audit EmitEvent(ProposalCreated) failed for %s: %v", prop.ProposalID, err)
	}
}

func mapKnownVulnComponent(pkg string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(pkg))
	if canon, err := ResolveComponent(lower); err == nil {
		return canon, true
	}
	switch {
	case strings.Contains(lower, "gin"), strings.Contains(lower, "gorm"), strings.Contains(lower, "sqlite"), strings.Contains(lower, "golang"), lower == "go":
		return ComponentHomeAPI, true
	case strings.Contains(lower, "opencv"), strings.Contains(lower, "onnx"), strings.Contains(lower, "python"), strings.Contains(lower, "vision"):
		return ComponentHomeVision, true
	case strings.Contains(lower, "react"), strings.Contains(lower, "vite"), strings.Contains(lower, "nginx"), strings.Contains(lower, "web"):
		return ComponentHomeWeb, true
	case strings.Contains(lower, "frigate"):
		return ComponentFrigate, true
	case strings.Contains(lower, "mosquitto"), strings.Contains(lower, "mqtt"):
		return ComponentMosquitto, true
	case strings.Contains(lower, "openresty"), strings.Contains(lower, "waf"):
		return ComponentOpenResty, true
	default:
		return pkg, false
	}
}
