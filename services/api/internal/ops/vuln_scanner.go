package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/security"
)

// OSVQueryRequest models OSV API query body.
type OSVQueryRequest struct {
	Commit  string        `json:"commit,omitempty"`
	Version string        `json:"version,omitempty"`
	Package OSVPackageRef `json:"package,omitempty"`
}

type OSVPackageRef struct {
	Name      string `json:"name"`
	Ecosystem string `json:"ecosystem"` // "Go", "PyPI", "Debian"
}

// OSVQueryResponse models OSV API results.
type OSVQueryResponse struct {
	Vulns []OSVVuln `json:"vulns"`
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
	Type  string `json:"type"`  // e.g. "CVSS_V3"
	Score string `json:"score"` // CVSS vector string or numeric score
}

// PackageTarget specifies an ecosystem package dependency to scan.
type PackageTarget struct {
	Component string
	Ecosystem string
	Package   string
	Version   string
}

// DefaultScanTargets lists baseline framework and server dependencies to monitor.
var DefaultScanTargets = []PackageTarget{
	{Component: "gin", Ecosystem: "Go", Package: "github.com/gin-gonic/gin", Version: "v1.9.1"},
	{Component: "sqlite", Ecosystem: "Go", Package: "github.com/glebarez/sqlite", Version: "v1.11.0"},
	{Component: "gorm", Ecosystem: "Go", Package: "gorm.io/gorm", Version: "v1.25.7"},
}

// VulnScanner integrates with Google OSV (Open Source Vulnerabilities) API
// to fetch zero-auth, real-time vulnerability intelligence for project dependencies.
type VulnScanner struct {
	db         *gorm.DB
	alertMgr   *security.AlertManager
	httpClient *http.Client
	apiURL     string
	mu         sync.Mutex
	lastScanAt time.Time
}

// NewVulnScanner creates a VulnScanner instance.
func NewVulnScanner(db *gorm.DB, alertMgr *security.AlertManager) *VulnScanner {
	return &VulnScanner{
		db:         db,
		alertMgr:   alertMgr,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		apiURL:     "https://api.osv.dev/v1/query",
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

// EvaluateCVSS determines the risk level ("CRITICAL", "HIGH", "MEDIUM", "LOW")
// and whether it constitutes an emergency critical vulnerability requiring immediate alert.
func EvaluateCVSS(v OSVVuln) (riskLevel string, isCritical bool) {
	// 1. Check explicit CVSS severity records from OSV
	for _, sev := range v.Severity {
		scoreStr := strings.ToUpper(sev.Score)
		// Check for CVSS v3/v4 vector
		if strings.Contains(scoreStr, "AV:N") { // Network accessible
			if strings.Contains(scoreStr, "C:H") && strings.Contains(scoreStr, "I:H") && strings.Contains(scoreStr, "A:H") {
				return "CRITICAL", true
			}
			if strings.Contains(scoreStr, "C:H") || strings.Contains(scoreStr, "PR:N") {
				return "HIGH", true
			}
		}
		// Try parsing numeric score if present
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

	// 2. Keyword heuristic on Summary and Details
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

// ScanPackage queries Google OSV for vulnerabilities in a specific ecosystem package.
func (s *VulnScanner) ScanPackage(ctx context.Context, ecosystem, pkgName, version string) ([]OSVVuln, error) {
	reqBody := OSVQueryRequest{
		Version: version,
		Package: OSVPackageRef{
			Name:      pkgName,
			Ecosystem: ecosystem,
		},
	}
	payloadBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Google OSV API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Google OSV returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var res OSVQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("failed to decode OSV response: %w", err)
	}

	return res.Vulns, nil
}

// ScanDefaultPackages scans all baseline datacenter dependencies against Google OSV.
func (s *VulnScanner) ScanDefaultPackages(ctx context.Context) error {
	for _, target := range DefaultScanTargets {
		vulns, err := s.ScanPackage(ctx, target.Ecosystem, target.Package, target.Version)
		if err != nil {
			log.Printf("[vuln-scanner] scan %s (%s) failed: %v", target.Component, target.Package, err)
			continue
		}
		if len(vulns) > 0 {
			s.ProcessVulnerabilities(vulns, target.Component, target.Version)
		}
	}
	s.mu.Lock()
	s.lastScanAt = time.Now()
	s.mu.Unlock()
	return nil
}

// ProcessVulnerabilities evaluates scan results, escalates high/critical CVEs to proposals and email alerts.
func (s *VulnScanner) ProcessVulnerabilities(vulns []OSVVuln, component, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, v := range vulns {
		riskLevel, isCritical := EvaluateCVSS(v)

		cveID := v.ID
		if len(v.Aliases) > 0 {
			cveID = fmt.Sprintf("%s (%s)", v.ID, v.Aliases[0])
		}

		// Check if proposal already exists
		if s.db != nil {
			if v.ID != "" {
				var existing model.OpsProposal
				if err := s.db.Where("title LIKE ?", "%"+v.ID+"%").First(&existing).Error; err == nil {
					continue // Already recorded
				}
			}

			// Create OpsProposal
			prop := model.OpsProposal{
				ProposalID:       fmt.Sprintf("prop-vuln-%d", time.Now().UnixNano()),
				Title:            fmt.Sprintf("修复 %s: %s %s 漏洞基线加固", cveID, component, version),
				Description:      fmt.Sprintf("Google OSV 漏洞情报源预警：%s。\n\n详情：%s", v.Summary, v.Details),
				TargetComponent:  component,
				RiskLevel:        riskLevel,
				EstimatedSeconds: 45,
				DowntimeSeconds:  2,
				ExecutionPlan:    fmt.Sprintf("1. 备份当前 %s 镜像与数据快照\n2. 拉取安全补丁版本并就地更新\n3. 执行 10 秒 /health 探针校验，若失败立即回滚", component),
				RollbackPlan:     fmt.Sprintf("docker rollback %s || restore-snapshot", component),
				Status:           "AWAITING_APPROVAL",
				CreatedAt:        time.Now(),
			}
			_ = s.db.Create(&prop).Error
		}

		// Dispatch email alert if high/critical risk and enabled
		if isCritical && s.alertMgr != nil {
			filters := s.alertMgr.GetAlertFilters()
			if filters.CriticalVuln {
				_ = s.alertMgr.SendSecurityEmail(
					fmt.Sprintf("【高危安全预警】依赖情报源发现 %s 存在高危漏洞 %s", component, cveID),
					"127.0.0.1",
					"CVE漏洞预警",
					fmt.Sprintf("组件 %s (%s) 命中 Google OSV 漏洞库：%s。\n主动协作 Agent 已生成待批修补提案，请登录运维控制台审阅。", component, version, v.Summary),
					"已生成安全待批提案并锁定沙箱推演",
				)
			}
		}

		log.Printf("[vuln-scanner] Detected %s for %s (%s), risk=%s, critical=%v", cveID, component, version, riskLevel, isCritical)
	}
}
