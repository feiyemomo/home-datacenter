package hostsec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AdvisorySource names the intelligence provider backing a CVE finding.
type AdvisorySource string

const (
	SourceOSV           AdvisorySource = "OSV"
	SourceDebianTracker AdvisorySource = "DebianSecurityTracker"
	SourceNVD           AdvisorySource = "NVD"
	SourceGHSA          AdvisorySource = "GHSA"
	SourceCISAKEV       AdvisorySource = "CISA_KEV"
)

// VulnerabilityFinding represents a correlated CVE/advisory evaluation against a host asset.
type VulnerabilityFinding struct {
	FindingID          string           `json:"finding_id"`
	CVEID              string           `json:"cve_id"`
	AdvisoryID         string           `json:"advisory_id"`
	Aliases            []string         `json:"aliases,omitempty"`
	Sources            []AdvisorySource `json:"sources"`
	Component          string           `json:"component"`
	Package            string           `json:"package"`
	SourcePackage      string           `json:"source_package,omitempty"`
	Ecosystem          string           `json:"ecosystem"`
	InstalledVersion   string           `json:"installed_version"`
	FixedVersion       string           `json:"fixed_version,omitempty"`
	DistroRelease      string           `json:"distro_release,omitempty"`
	BackportEvaluated  bool             `json:"backport_evaluated"`
	PatchStatus        string           `json:"patch_status"` // "vulnerable", "patched_by_backport", "unaffected", "unverified"
	IsVulnerable       bool             `json:"is_vulnerable"`
	CVSSScore          float64          `json:"cvss_score,omitempty"`
	CVSSVector         string           `json:"cvss_vector,omitempty"`
	RiskLevel          string           `json:"risk_level"` // "CRITICAL", "HIGH", "MEDIUM", "LOW"
	InCISAKEV          bool             `json:"in_cisa_kev"`
	KEVDateAdded       string           `json:"kev_date_added,omitempty"`
	KEVDueDate         string           `json:"kev_due_date,omitempty"`
	KEVRequiredAction  string           `json:"kev_required_action,omitempty"`
	Summary            string           `json:"summary"`
	RemediationMode    string           `json:"remediation_mode"` // "host_pkg_upgrade", "fnos_vendor_ota", "manual_advisory"
	EvaluatedAt        time.Time        `json:"evaluated_at"`
}

// CorrelationReport summarizes a full R9.2 host + dependency correlation pass.
type CorrelationReport struct {
	Platform           PlatformInfo           `json:"platform"`
	EvaluatedAssets    int                    `json:"evaluated_assets"`
	VulnerableFindings []VulnerabilityFinding `json:"vulnerable_findings"`
	SuppressedBackport []VulnerabilityFinding `json:"suppressed_backport_findings,omitempty"`
	KEVMatches         int                    `json:"kev_matches"`
	SourcesQueried     []AdvisorySource       `json:"sources_queried"`
	Warnings           []string               `json:"warnings,omitempty"`
	GeneratedAt        time.Time              `json:"generated_at"`
}

// CriticalHostPackages defines core security-sensitive OS packages prioritized during host correlation.
var CriticalHostPackages = map[string]bool{
	"openssh-server": true,
	"openssh-client": true,
	"openssh":        true,
	"openssl":        true,
	"libssl3":        true,
	"libssl1.1":      true,
	"linux-image-amd64": true,
	"linux-image-arm64": true,
	"linux":          true,
	"docker-ce":      true,
	"docker-ce-cli":  true,
	"docker.io":      true,
	"containerd.io":  true,
	"libc6":          true,
	"glibc":          true,
	"curl":           true,
	"libcurl4":       true,
	"sudo":           true,
	"systemd":        true,
	"zlib1g":         true,
	"ca-certificates": true,
}

// ============================================================================
// Pure-Go Debian Policy 5.6.12 Version Comparator (Backport-Aware)
// ============================================================================

type dpkgVer struct {
	epoch    int
	upstream string
	revision string
}

func parseDpkgVersion(v string) dpkgVer {
	v = strings.TrimSpace(v)
	var out dpkgVer
	if colonIdx := strings.IndexByte(v, ':'); colonIdx > 0 {
		if ep, err := strconv.Atoi(v[:colonIdx]); err == nil && ep >= 0 {
			out.epoch = ep
			v = v[colonIdx+1:]
		}
	}
	if dashIdx := strings.LastIndexByte(v, '-'); dashIdx >= 0 {
		out.upstream = v[:dashIdx]
		out.revision = v[dashIdx+1:]
	} else {
		out.upstream = v
	}
	return out
}

// CompareDpkgVersions compares two Debian/Ubuntu package versions according to
// Debian Policy Section 5.6.12 ([epoch:]upstream_version[-debian_revision]).
// Returns -1 if a < b, 0 if a == b, +1 if a > b.
func CompareDpkgVersions(a, b string) int {
	va := parseDpkgVersion(a)
	vb := parseDpkgVersion(b)

	if va.epoch < vb.epoch {
		return -1
	}
	if va.epoch > vb.epoch {
		return 1
	}
	if cmp := compareDpkgPart(va.upstream, vb.upstream); cmp != 0 {
		return cmp
	}
	return compareDpkgPart(va.revision, vb.revision)
}

func dpkgCharOrder(c byte) int {
	if c == '~' {
		return -1
	}
	if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
		return int(c)
	}
	// Non-letters (except ~) sort after all letters in Debian Policy 5.6.12
	return int(c) + 256
}

func compareDpkgPart(a, b string) int {
	ia, ib := 0, 0
	for ia < len(a) || ib < len(b) {
		// 1. Compare non-digit prefix lexically
		for (ia < len(a) && !isDigit(a[ia])) || (ib < len(b) && !isDigit(b[ib])) {
			var ca, cb int
			if ia < len(a) && !isDigit(a[ia]) {
				ca = dpkgCharOrder(a[ia])
				ia++
			}
			if ib < len(b) && !isDigit(b[ib]) {
				cb = dpkgCharOrder(b[ib])
				ib++
			}
			if ca != cb {
				if ca < cb {
					return -1
				}
				return 1
			}
		}

		// 2. Compare digit sequence numerically (skipping leading zeroes)
		for ia < len(a) && a[ia] == '0' {
			ia++
		}
		for ib < len(b) && b[ib] == '0' {
			ib++
		}
		startA, startB := ia, ib
		for ia < len(a) && isDigit(a[ia]) {
			ia++
		}
		for ib < len(b) && isDigit(b[ib]) {
			ib++
		}
		lenA := ia - startA
		lenB := ib - startB
		if lenA != lenB {
			if lenA < lenB {
				return -1
			}
			return 1
		}
		if lenA > 0 {
			subA := a[startA:ia]
			subB := b[startB:ib]
			if subA < subB {
				return -1
			}
			if subA > subB {
				return 1
			}
		}
	}
	return 0
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

// ============================================================================
// Multi-Source Intelligence Providers (OSV, Debian Tracker, NVD, GHSA, CISA KEV)
// ============================================================================

// CISAKEVEntry models an entry from the CISA Known Exploited Vulnerabilities catalog.
type CISAKEVEntry struct {
	CVEID                      string `json:"cveID"`
	VendorProject              string `json:"vendorProject"`
	Product                    string `json:"product"`
	VulnerabilityName          string `json:"vulnerabilityName"`
	DateAdded                  string `json:"dateAdded"`
	ShortDescription           string `json:"shortDescription"`
	RequiredAction             string `json:"requiredAction"`
	DueDate                    string `json:"dueDate"`
	KnownRansomwareCampaignUse string `json:"knownRansomwareCampaignUse"`
}

// CISAKEVCatalog indexes CISA KEV entries by uppercase CVE ID.
type CISAKEVCatalog struct {
	mu      sync.RWMutex
	entries map[string]CISAKEVEntry
}

// NewCISAKEVCatalog creates an empty KEV catalog.
func NewCISAKEVCatalog() *CISAKEVCatalog {
	return &CISAKEVCatalog{entries: make(map[string]CISAKEVEntry)}
}

// LoadJSON loads a CISA KEV JSON catalog payload.
func (k *CISAKEVCatalog) LoadJSON(data []byte) error {
	var payload struct {
		Vulnerabilities []CISAKEVEntry `json:"vulnerabilities"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, v := range payload.Vulnerabilities {
		id := strings.ToUpper(strings.TrimSpace(v.CVEID))
		if id != "" {
			k.entries[id] = v
		}
	}
	return nil
}

// Fetch loads CISA KEV from a URL.
func (k *CISAKEVCatalog) Fetch(ctx context.Context, client *http.Client, feedURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("CISA KEV HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	if err != nil {
		return err
	}
	return k.LoadJSON(b)
}

// Lookup checks if a CVE ID is in CISA KEV.
func (k *CISAKEVCatalog) Lookup(cveID string) (CISAKEVEntry, bool) {
	if k == nil {
		return CISAKEVEntry{}, false
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	e, ok := k.entries[strings.ToUpper(strings.TrimSpace(cveID))]
	return e, ok
}

// DebianTrackerRelease models a single Debian release status for a CVE.
type DebianTrackerRelease struct {
	Status       string `json:"status"`        // "resolved", "open", "undetermined"
	FixedVersion string `json:"fixed_version"` // e.g. "1:9.2p1-2+deb12u3"
	Urgency      string `json:"urgency"`       // "high", "medium", "low", "unimportant"
}

// DebianTrackerIssue models a CVE record in Debian Security Tracker JSON format.
type DebianTrackerIssue struct {
	Description string                          `json:"description"`
	Releases    map[string]DebianTrackerRelease `json:"releases"`
}

// DebianTrackerFeed maps source_package -> cve_id -> DebianTrackerIssue.
type DebianTrackerFeed map[string]map[string]DebianTrackerIssue

// ParseDebianTrackerJSON decodes Debian Security Tracker JSON.
func ParseDebianTrackerJSON(data []byte) (DebianTrackerFeed, error) {
	var feed DebianTrackerFeed
	if err := json.Unmarshal(data, &feed); err != nil {
		return nil, err
	}
	return feed, nil
}

// OSVAdvisory models a normalized OSV vulnerability record with affected ranges.
type OSVAdvisory struct {
	ID        string        `json:"id"`
	Summary   string        `json:"summary"`
	Details   string        `json:"details"`
	Aliases   []string      `json:"aliases,omitempty"`
	Severity  []OSVSeverity `json:"severity,omitempty"`
	Affected  []OSVAffected `json:"affected,omitempty"`
	Published time.Time     `json:"published"`
}

type OSVSeverity struct {
	Type  string `json:"type"`
	Score string `json:"score"`
}

type OSVAffected struct {
	Package struct {
		Name      string `json:"name"`
		Ecosystem string `json:"ecosystem"`
	} `json:"package"`
	Ranges []struct {
		Type   string `json:"type"` // "ECOSYSTEM", "SEMVER", "GIT"
		Events []struct {
			Introduced string `json:"introduced,omitempty"`
			Fixed      string `json:"fixed,omitempty"`
		} `json:"events"`
	} `json:"ranges,omitempty"`
	Versions []string `json:"versions,omitempty"`
}

// CorrelationConfig configures advisory feed URLs and optional offline/cached feeds.
type CorrelationConfig struct {
	OSVURL           string
	DebianTrackerURL string
	NVDURL           string
	NVDAPIKey        string
	GHSAURL          string
	GHSAToken        string
	CISAKEVURL       string
	HTTPClient       *http.Client
	StaticDebianFeed DebianTrackerFeed
	StaticKEV        *CISAKEVCatalog
}

// CorrelationEngine evaluates host assets and OS/runtime packages against
// OSV, Debian Security Tracker (backport-aware), NVD, GHSA, and CISA KEV.
type CorrelationEngine struct {
	cfg CorrelationConfig
	mu  sync.RWMutex
	kev *CISAKEVCatalog
}

// NewCorrelationEngine initializes a multi-source CVE correlation engine.
func NewCorrelationEngine(cfg CorrelationConfig) *CorrelationEngine {
	if cfg.OSVURL == "" {
		cfg.OSVURL = "https://api.osv.dev/v1/query"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	kev := cfg.StaticKEV
	if kev == nil {
		kev = NewCISAKEVCatalog()
	}
	return &CorrelationEngine{
		cfg: cfg,
		kev: kev,
	}
}

// QueryOSV queries OSV with pagination (next_page_token) for a package and ecosystem.
func (e *CorrelationEngine) QueryOSV(ctx context.Context, ecosystem, pkgName, version string) ([]OSVAdvisory, error) {
	e.mu.RLock()
	apiURL := e.cfg.OSVURL
	client := e.cfg.HTTPClient
	e.mu.RUnlock()

	if apiURL == "" || client == nil {
		return nil, fmt.Errorf("OSV endpoint not configured")
	}

	var all []OSVAdvisory
	pageToken := ""
	for page := 0; page < 5; page++ {
		reqBody := map[string]any{
			"package": map[string]string{
				"name":      pkgName,
				"ecosystem": ecosystem,
			},
			"version": version,
		}
		if pageToken != "" {
			reqBody["page_token"] = pageToken
		}
		payload, err := json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("OSV returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		var parsed struct {
			Vulns         []OSVAdvisory `json:"vulns"`
			NextPageToken string        `json:"next_page_token"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, err
		}
		all = append(all, parsed.Vulns...)
		if strings.TrimSpace(parsed.NextPageToken) == "" {
			break
		}
		pageToken = parsed.NextPageToken
	}
	return all, nil
}

// QueryNVD queries NVD v2.0 API (when NVDURL is configured) for a keyword/CPE term.
func (e *CorrelationEngine) QueryNVD(ctx context.Context, keyword string) ([]VulnerabilityFinding, error) {
	e.mu.RLock()
	nvdURL := e.cfg.NVDURL
	apiKey := e.cfg.NVDAPIKey
	client := e.cfg.HTTPClient
	e.mu.RUnlock()

	if strings.TrimSpace(nvdURL) == "" {
		return nil, nil
	}
	u, err := url.Parse(nvdURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("keywordSearch", keyword)
	q.Set("resultsPerPage", "20")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("apiKey", apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("NVD returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Vulnerabilities []struct {
			CVE struct {
				ID           string `json:"id"`
				Descriptions []struct {
					Lang  string `json:"lang"`
					Value string `json:"value"`
				} `json:"descriptions"`
				Metrics struct {
					CvssMetricV31 []struct {
						CvssData struct {
							BaseScore    float64 `json:"baseScore"`
							BaseSeverity string  `json:"baseSeverity"`
							VectorString string  `json:"vectorString"`
						} `json:"cvssData"`
					} `json:"cvssMetricV31"`
				} `json:"metrics"`
			} `json:"cve"`
		} `json:"vulnerabilities"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024*1024)).Decode(&payload); err != nil {
		return nil, err
	}
	var out []VulnerabilityFinding
	for _, item := range payload.Vulnerabilities {
		desc := ""
		for _, d := range item.CVE.Descriptions {
			if d.Lang == "en" {
				desc = d.Value
				break
			}
		}
		score := 0.0
		risk := "MEDIUM"
		vec := ""
		if len(item.CVE.Metrics.CvssMetricV31) > 0 {
			m := item.CVE.Metrics.CvssMetricV31[0].CvssData
			score = m.BaseScore
			risk = strings.ToUpper(m.BaseSeverity)
			vec = m.VectorString
		}
		out = append(out, VulnerabilityFinding{
			CVEID:       item.CVE.ID,
			AdvisoryID:  item.CVE.ID,
			Sources:     []AdvisorySource{SourceNVD},
			RiskLevel:   risk,
			CVSSScore:   score,
			CVSSVector:  vec,
			Summary:     desc,
			IsVulnerable: true,
			PatchStatus: "unverified",
		})
	}
	return out, nil
}

// QueryGHSA queries GitHub Security Advisories API (when GHSAURL is configured).
func (e *CorrelationEngine) QueryGHSA(ctx context.Context, ecosystem, pkgName string) ([]VulnerabilityFinding, error) {
	e.mu.RLock()
	ghsaURL := e.cfg.GHSAURL
	token := e.cfg.GHSAToken
	client := e.cfg.HTTPClient
	e.mu.RUnlock()

	if strings.TrimSpace(ghsaURL) == "" {
		return nil, nil
	}
	u, err := url.Parse(ghsaURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	if ecosystem != "" {
		q.Set("ecosystem", strings.ToLower(ecosystem))
	}
	if pkgName != "" {
		q.Set("affects", pkgName)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GHSA returned HTTP %d", resp.StatusCode)
	}
	var advisories []struct {
		GHSAID   string `json:"ghsa_id"`
		CVEID    string `json:"cve_id"`
		Summary  string `json:"summary"`
		Severity string `json:"severity"`
		CVSS     struct {
			Score        float64 `json:"score"`
			VectorString string  `json:"vector_string"`
		} `json:"cvss"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024*1024)).Decode(&advisories); err != nil {
		return nil, err
	}
	var out []VulnerabilityFinding
	for _, a := range advisories {
		cve := a.CVEID
		if cve == "" {
			cve = a.GHSAID
		}
		risk := strings.ToUpper(strings.TrimSpace(a.Severity))
		if risk == "" {
			risk = "MEDIUM"
		}
		out = append(out, VulnerabilityFinding{
			CVEID:        cve,
			AdvisoryID:   a.GHSAID,
			Sources:      []AdvisorySource{SourceGHSA},
			Package:      pkgName,
			Ecosystem:    ecosystem,
			RiskLevel:    risk,
			CVSSScore:    a.CVSS.Score,
			CVSSVector:   a.CVSS.VectorString,
			Summary:      a.Summary,
			IsVulnerable: true,
			PatchStatus:  "unverified",
		})
	}
	return out, nil
}

// CorrelateAssets evaluates a HostAssetSnapshot across Debian Security Tracker, OSV, NVD, GHSA, and CISA KEV.
func (e *CorrelationEngine) CorrelateAssets(ctx context.Context, snap *HostAssetSnapshot) (*CorrelationReport, error) {
	if snap == nil {
		return nil, fmt.Errorf("host asset snapshot is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	report := &CorrelationReport{
		Platform:    snap.Platform,
		GeneratedAt: time.Now().UTC(),
	}

	// Refresh CISA KEV if feed URL configured
	if strings.TrimSpace(e.cfg.CISAKEVURL) != "" {
		if err := e.kev.Fetch(ctx, e.cfg.HTTPClient, e.cfg.CISAKEVURL); err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("CISA KEV fetch failed: %v", err))
		} else {
			report.SourcesQueried = append(report.SourcesQueried, SourceCISAKEV)
		}
	}

	// Load Debian Security Tracker feed if static or URL configured
	debianFeed := e.cfg.StaticDebianFeed
	if debianFeed == nil && strings.TrimSpace(e.cfg.DebianTrackerURL) != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.cfg.DebianTrackerURL, nil)
		if err == nil {
			if resp, rErr := e.cfg.HTTPClient.Do(req); rErr == nil {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					if parsed, pErr := ParseDebianTrackerJSON(b); pErr == nil {
						debianFeed = parsed
					}
				}
			}
		}
	}
	if debianFeed != nil {
		report.SourcesQueried = append(report.SourcesQueried, SourceDebianTracker)
	}

	releaseCodename := resolveDebianRelease(snap.Platform)
	osvEcosystem := resolveOSVEcosystem(snap.Platform)

	// Build asset list from critical OS packages + OpenSSH + Kernel + Docker Engine
	targets := selectHostCorrelationTargets(snap)
	report.EvaluatedAssets = len(targets)

	seenKey := make(map[string]bool)
	remediationMode := resolveRemediationMode(snap.Platform)

	// 1. Evaluate against Debian Security Tracker (authoritative backport check)
	if debianFeed != nil {
		for _, tgt := range targets {
			srcPkg := tgt.SourceName
			if srcPkg == "" {
				srcPkg = tgt.Name
			}
			issues, ok := debianFeed[srcPkg]
			if !ok && tgt.Name != srcPkg {
				issues, ok = debianFeed[tgt.Name]
			}
			if !ok {
				continue
			}
			for cveID, issue := range issues {
				rel, hasRel := issue.Releases[releaseCodename]
				if !hasRel && releaseCodename == "" && len(issue.Releases) == 1 {
					for k, v := range issue.Releases {
						releaseCodename = k
						rel = v
						hasRel = true
					}
				}
				if !hasRel {
					continue
				}

				finding := VulnerabilityFinding{
					FindingID:         fmt.Sprintf("%s:%s:%s", cveID, tgt.Name, tgt.Version),
					CVEID:             cveID,
					AdvisoryID:        cveID,
					Sources:           []AdvisorySource{SourceDebianTracker},
					Component:         tgt.Name,
					Package:           tgt.Name,
					SourcePackage:     srcPkg,
					Ecosystem:         osvEcosystem,
					InstalledVersion:  tgt.Version,
					FixedVersion:      rel.FixedVersion,
					DistroRelease:     releaseCodename,
					BackportEvaluated: true,
					RiskLevel:         urgencyToRiskLevel(rel.Urgency),
					Summary:           issue.Description,
					RemediationMode:   remediationModeForTarget(remediationMode, tgt.Name),
					EvaluatedAt:       time.Now().UTC(),
				}

				if strings.EqualFold(rel.Status, "resolved") && rel.FixedVersion != "" {
					if CompareDpkgVersions(tgt.Version, rel.FixedVersion) >= 0 {
						finding.IsVulnerable = false
						finding.PatchStatus = "patched_by_backport"
						e.annotateKEV(&finding)
						report.SuppressedBackport = append(report.SuppressedBackport, finding)
						seenKey[cveID+"|"+tgt.Name] = true
						continue
					}
					finding.IsVulnerable = true
					finding.PatchStatus = "vulnerable"
				} else if strings.EqualFold(rel.Status, "not-affected") {
					finding.IsVulnerable = false
					finding.PatchStatus = "unaffected"
					report.SuppressedBackport = append(report.SuppressedBackport, finding)
					seenKey[cveID+"|"+tgt.Name] = true
					continue
				} else {
					finding.IsVulnerable = true
					finding.PatchStatus = "vulnerable"
				}

				e.annotateKEV(&finding)
				if finding.InCISAKEV {
					report.KEVMatches++
				}
				seenKey[cveID+"|"+tgt.Name] = true
				report.VulnerableFindings = append(report.VulnerableFindings, finding)
			}
		}
	}

	// 2. Evaluate against OSV ecosystem (with Debian fixed-version backport comparison)
	if osvEcosystem != "" && strings.TrimSpace(e.cfg.OSVURL) != "" {
		osvQueried := false
		for _, tgt := range targets {
			vulns, err := e.QueryOSV(ctx, osvEcosystem, tgt.Name, tgt.Version)
			if err != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("OSV query %s/%s@%s failed: %v", osvEcosystem, tgt.Name, tgt.Version, err))
				continue
			}
			osvQueried = true
			for _, v := range vulns {
				cveID := v.ID
				for _, al := range v.Aliases {
					if strings.HasPrefix(strings.ToUpper(al), "CVE-") {
						cveID = strings.ToUpper(al)
						break
					}
				}
				if seenKey[cveID+"|"+tgt.Name] {
					continue
				}

				fixedVer, isPatched := evaluateOSVAffectedDpkg(v, tgt.Name, tgt.Version)
				finding := VulnerabilityFinding{
					FindingID:         fmt.Sprintf("%s:%s:%s", v.ID, tgt.Name, tgt.Version),
					CVEID:             cveID,
					AdvisoryID:        v.ID,
					Aliases:           v.Aliases,
					Sources:           []AdvisorySource{SourceOSV},
					Component:         tgt.Name,
					Package:           tgt.Name,
					SourcePackage:     tgt.SourceName,
					Ecosystem:         osvEcosystem,
					InstalledVersion:  tgt.Version,
					FixedVersion:      fixedVer,
					DistroRelease:     releaseCodename,
					BackportEvaluated: fixedVer != "",
					Summary:           v.Summary,
					RemediationMode:   remediationModeForTarget(remediationMode, tgt.Name),
					EvaluatedAt:       time.Now().UTC(),
				}
				finding.CVSSScore, finding.CVSSVector, finding.RiskLevel = evaluateOSVSeverity(v)
				e.annotateKEV(&finding)

				if isPatched {
					finding.IsVulnerable = false
					finding.PatchStatus = "patched_by_backport"
					report.SuppressedBackport = append(report.SuppressedBackport, finding)
					seenKey[cveID+"|"+tgt.Name] = true
					continue
				}
				finding.IsVulnerable = true
				finding.PatchStatus = "vulnerable"
				if finding.InCISAKEV {
					report.KEVMatches++
				}
				seenKey[cveID+"|"+tgt.Name] = true
				report.VulnerableFindings = append(report.VulnerableFindings, finding)
			}
		}
		if osvQueried {
			report.SourcesQueried = append(report.SourcesQueried, SourceOSV)
		}
	}

	// 3. Optional NVD & GHSA enrichment (e.g. Docker Engine, OpenSSH)
	if strings.TrimSpace(e.cfg.NVDURL) != "" {
		report.SourcesQueried = append(report.SourcesQueried, SourceNVD)
		for _, tgt := range targets {
			if tgt.Name != "openssh-server" && tgt.Name != "docker-ce" && tgt.Name != "linux" {
				continue
			}
			nvdFindings, err := e.QueryNVD(ctx, tgt.Name+" "+tgt.Version)
			if err != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("NVD query %s failed: %v", tgt.Name, err))
				continue
			}
			for _, nf := range nvdFindings {
				if seenKey[nf.CVEID+"|"+tgt.Name] {
					continue
				}
				nf.FindingID = fmt.Sprintf("%s:%s:%s", nf.CVEID, tgt.Name, tgt.Version)
				nf.Component = tgt.Name
				nf.Package = tgt.Name
				nf.InstalledVersion = tgt.Version
				nf.RemediationMode = remediationModeForTarget(remediationMode, tgt.Name)
				nf.EvaluatedAt = time.Now().UTC()
				e.annotateKEV(&nf)
				if nf.InCISAKEV {
					report.KEVMatches++
				}
				seenKey[nf.CVEID+"|"+tgt.Name] = true
				report.VulnerableFindings = append(report.VulnerableFindings, nf)
			}
		}
	}
	if strings.TrimSpace(e.cfg.GHSAURL) != "" {
		report.SourcesQueried = append(report.SourcesQueried, SourceGHSA)
		for _, tgt := range targets {
			ghsaFindings, err := e.QueryGHSA(ctx, "", tgt.Name)
			if err != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("GHSA query %s failed: %v", tgt.Name, err))
				continue
			}
			for _, gf := range ghsaFindings {
				if seenKey[gf.CVEID+"|"+tgt.Name] {
					continue
				}
				gf.FindingID = fmt.Sprintf("%s:%s:%s", gf.CVEID, tgt.Name, tgt.Version)
				gf.Component = tgt.Name
				gf.InstalledVersion = tgt.Version
				gf.RemediationMode = remediationModeForTarget(remediationMode, tgt.Name)
				gf.EvaluatedAt = time.Now().UTC()
				e.annotateKEV(&gf)
				if gf.InCISAKEV {
					report.KEVMatches++
				}
				seenKey[gf.CVEID+"|"+tgt.Name] = true
				report.VulnerableFindings = append(report.VulnerableFindings, gf)
			}
		}
	}

	sort.Slice(report.VulnerableFindings, func(i, j int) bool {
		if report.VulnerableFindings[i].InCISAKEV != report.VulnerableFindings[j].InCISAKEV {
			return report.VulnerableFindings[i].InCISAKEV
		}
		ri := riskRank(report.VulnerableFindings[i].RiskLevel)
		rj := riskRank(report.VulnerableFindings[j].RiskLevel)
		if ri != rj {
			return ri > rj
		}
		return report.VulnerableFindings[i].CVEID < report.VulnerableFindings[j].CVEID
	})

	return report, nil
}

func (e *CorrelationEngine) annotateKEV(f *VulnerabilityFinding) {
	if e.kev == nil || f == nil {
		return
	}
	candidates := append([]string{f.CVEID}, f.Aliases...)
	for _, c := range candidates {
		if entry, ok := e.kev.Lookup(c); ok {
			f.InCISAKEV = true
			f.KEVDateAdded = entry.DateAdded
			f.KEVDueDate = entry.DueDate
			f.KEVRequiredAction = entry.RequiredAction
			f.Sources = appendUniqueSource(f.Sources, SourceCISAKEV)
			if riskRank(f.RiskLevel) < riskRank("HIGH") {
				f.RiskLevel = "HIGH"
			}
			return
		}
	}
}

func appendUniqueSource(list []AdvisorySource, s AdvisorySource) []AdvisorySource {
	for _, cur := range list {
		if cur == s {
			return list
		}
	}
	return append(list, s)
}

func selectHostCorrelationTargets(snap *HostAssetSnapshot) []OSPackage {
	seen := make(map[string]bool)
	var out []OSPackage
	for _, pkg := range snap.Packages {
		if CriticalHostPackages[pkg.Name] || strings.HasPrefix(pkg.Name, "linux-image-") || strings.HasPrefix(pkg.Name, "openssh-") {
			seen[pkg.Name] = true
			out = append(out, pkg)
		}
	}
	if snap.OpenSSH != nil && !seen["openssh-server"] && !seen["openssh"] {
		out = append(out, OSPackage{Name: "openssh-server", SourceName: "openssh", Version: snap.OpenSSH.Version})
		seen["openssh-server"] = true
	}
	if snap.OpenSSL != nil && !seen["openssl"] && !seen["libssl3"] {
		out = append(out, OSPackage{Name: "openssl", SourceName: "openssl", Version: snap.OpenSSL.Version})
		seen["openssl"] = true
	}
	if snap.DockerEngine != nil && snap.DockerEngine.Available && snap.DockerEngine.Version != "" && !seen["docker-ce"] && !seen["docker.io"] {
		out = append(out, OSPackage{Name: "docker-ce", SourceName: "docker-ce", Version: snap.DockerEngine.Version})
		seen["docker-ce"] = true
	}
	if snap.KernelVersion != "" && !seen["linux"] {
		hasKernelPkg := false
		for k := range seen {
			if strings.HasPrefix(k, "linux-image-") {
				hasKernelPkg = true
				break
			}
		}
		if !hasKernelPkg {
			out = append(out, OSPackage{Name: "linux", SourceName: "linux", Version: snap.KernelVersion})
		}
	}
	return out
}

func resolveDebianRelease(p PlatformInfo) string {
	if p.Codename != "" {
		return p.Codename
	}
	switch p.VersionID {
	case "12":
		return "bookworm"
	case "11":
		return "bullseye"
	case "13":
		return "trixie"
	case "24.04":
		return "noble"
	case "22.04":
		return "jammy"
	default:
		if p.IsFnOS {
			return "bookworm"
		}
		return ""
	}
}

func resolveOSVEcosystem(p PlatformInfo) string {
	switch p.OSFamily {
	case OSFamilyDebian, OSFamilyFnOS:
		if p.VersionID != "" {
			return "Debian:" + p.VersionID
		}
		return "Debian:12"
	case OSFamilyUbuntu:
		if p.VersionID != "" {
			return "Ubuntu:" + p.VersionID
		}
		return "Ubuntu"
	default:
		return ""
	}
}

func resolveRemediationMode(p PlatformInfo) string {
	if p.IsFnOS || p.OSFamily == OSFamilyFnOS {
		return "fnos_vendor_ota"
	}
	if p.HostUpdateSupported && p.PackageManager == PkgManagerAptDpkg {
		return "host_pkg_upgrade"
	}
	return "manual_advisory"
}

func remediationModeForTarget(baseMode, pkgName string) string {
	if baseMode != "host_pkg_upgrade" {
		return baseMode
	}
	if AllowedHostPackages[pkgName] {
		return "host_pkg_upgrade"
	}
	return "manual_advisory"
}

func evaluateOSVAffectedDpkg(v OSVAdvisory, pkgName, installedVer string) (fixedVersion string, patched bool) {
	for _, aff := range v.Affected {
		if aff.Package.Name != "" && !strings.EqualFold(aff.Package.Name, pkgName) {
			continue
		}
		for _, r := range aff.Ranges {
			for _, ev := range r.Events {
				if ev.Fixed != "" {
					fixedVersion = ev.Fixed
					if CompareDpkgVersions(installedVer, ev.Fixed) >= 0 {
						return fixedVersion, true
					}
				}
			}
		}
	}
	return fixedVersion, false
}

func evaluateOSVSeverity(v OSVAdvisory) (score float64, vector string, risk string) {
	for _, s := range v.Severity {
		vector = s.Score
		if val, err := strconv.ParseFloat(s.Score, 64); err == nil {
			score = val
			return score, vector, scoreToRisk(score)
		}
		if strings.HasPrefix(s.Score, "CVSS:3.") {
			if sc, rk, err := ParseCVSS3xVector(s.Score); err == nil {
				return sc, s.Score, rk
			}
		}
		if strings.HasPrefix(s.Score, "CVSS:4.0") {
			// CVSS 4.0 vector fallback: inspect impact/exploitability metrics cleanly
			upper := strings.ToUpper(s.Score)
			if strings.Contains(upper, "AV:N") && (strings.Contains(upper, "VC:H") || strings.Contains(upper, "VI:H") || strings.Contains(upper, "VA:H")) {
				return 9.0, s.Score, "CRITICAL"
			}
			return 7.5, s.Score, "HIGH"
		}
	}
	text := strings.ToLower(v.Summary + " " + v.Details)
	if strings.Contains(text, "remote code execution") || strings.Contains(text, "arbitrary code execution") || strings.Contains(text, "privilege escalation") {
		return 9.0, "", "CRITICAL"
	}
	return 5.5, "", "MEDIUM"
}

func urgencyToRiskLevel(urgency string) string {
	switch strings.ToLower(strings.TrimSpace(urgency)) {
	case "high":
		return "HIGH"
	case "medium":
		return "MEDIUM"
	case "low", "unimportant":
		return "LOW"
	default:
		return "MEDIUM"
	}
}

func scoreToRisk(score float64) string {
	switch {
	case score >= 9.0:
		return "CRITICAL"
	case score >= 7.0:
		return "HIGH"
	case score >= 4.0:
		return "MEDIUM"
	default:
		return "LOW"
	}
}

func riskRank(risk string) int {
	switch strings.ToUpper(risk) {
	case "CRITICAL":
		return 4
	case "HIGH":
		return 3
	case "MEDIUM":
		return 2
	case "LOW":
		return 1
	default:
		return 0
	}
}

// ParseCVSS3xVector computes FIRST CVSS v3.0/v3.1 Base Score and severity.
func ParseCVSS3xVector(vector string) (float64, string, error) {
	parts := strings.Split(vector, "/")
	if len(parts) == 0 || (!strings.HasPrefix(parts[0], "CVSS:3.1") && !strings.HasPrefix(parts[0], "CVSS:3.0")) {
		return 0, "", fmt.Errorf("invalid CVSS 3.x vector: %s", vector)
	}
	var av, ac, pr, ui, c, i, a float64
	scopeChanged := false
	for _, p := range parts[1:] {
		kv := strings.SplitN(p, ":", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "AV":
			switch kv[1] {
			case "N": av = 0.85
			case "A": av = 0.62
			case "L": av = 0.55
			case "P": av = 0.20
			}
		case "AC":
			switch kv[1] {
			case "L": ac = 0.77
			case "H": ac = 0.44
			}
		case "UI":
			switch kv[1] {
			case "N": ui = 0.85
			case "R": ui = 0.62
			}
		case "S":
			if kv[1] == "C" {
				scopeChanged = true
			}
		case "C":
			switch kv[1] {
			case "H": c = 0.56
			case "L": c = 0.22
			}
		case "I":
			switch kv[1] {
			case "H": i = 0.56
			case "L": i = 0.22
			}
		case "A":
			switch kv[1] {
			case "H": a = 0.56
			case "L": a = 0.22
			}
		}
	}
	for _, p := range parts[1:] {
		if strings.HasPrefix(p, "PR:") {
			v := strings.TrimPrefix(p, "PR:")
			if scopeChanged {
				switch v {
				case "N": pr = 0.85
				case "L": pr = 0.68
				case "H": pr = 0.50
				}
			} else {
				switch v {
				case "N": pr = 0.85
				case "L": pr = 0.62
				case "H": pr = 0.27
				}
			}
		}
	}
	iss := 1.0 - ((1.0 - c) * (1.0 - i) * (1.0 - a))
	if iss <= 0 {
		return 0, "LOW", nil
	}
	var impact float64
	if scopeChanged {
		// Standard approximation of 7.52*(iss-0.029) - 3.25*(iss-0.02)^15
		pow := 1.0
		base := iss - 0.02
		for k := 0; k < 15; k++ {
			pow *= base
		}
		impact = 7.52*(iss-0.029) - 3.25*pow
	} else {
		impact = 6.42 * iss
	}
	exploitability := 8.22 * av * ac * pr * ui
	var raw float64
	if impact <= 0 {
		raw = 0
	} else if !scopeChanged {
		raw = impact + exploitability
	} else {
		raw = 1.08 * (impact + exploitability)
	}
	if raw > 10.0 {
		raw = 10.0
	}
	// Round up to 1 decimal place per FIRST spec
	rounded := float64(int(raw*10.0+0.999999999)) / 10.0
	if float64(int(raw*10.0)) == raw*10.0 {
		rounded = raw
	}
	return rounded, scoreToRisk(rounded), nil
}
