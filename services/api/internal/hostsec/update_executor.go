package hostsec

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"home-datacenter-api/internal/audit"
	"home-datacenter-api/internal/model"
)

const (
	ActionHostPackageUpgrade = "host.pkg_upgrade"

	DefaultUpdateTimeout     = 180 * time.Second
	MaxUpdateTimeout         = 600 * time.Second
	DefaultSSHRecoveryWindow = 30 * time.Second
)

var (
	ErrPackageNotWhitelisted = errors.New("package is not in host update whitelist")
	ErrInvalidPackageName    = errors.New("invalid debian package name")
	ErrInvalidPackageVersion = errors.New("invalid debian package version")
	ErrApprovalRequired      = errors.New("verified human-approved OpsProposal is required for host update")
	ErrBaselineDrift         = errors.New("host update baseline hash mismatch (TOCTOU drift detected)")
	ErrInsecureAptSource     = errors.New("apt repository security/signature check failed")
	ErrSnapshotFailed        = errors.New("pre-update host snapshot failed")
	ErrPostVerifyFailed      = errors.New("post-update verification failed")
	ErrRollbackAttempted     = errors.New("post-update verification failed; rollback executed")
)

// AllowedHostPackages defines the strict closed whitelist of OS packages eligible
// for controlled HostUpdateExecutor upgrades on supported Debian/Ubuntu hosts.
var AllowedHostPackages = map[string]bool{
	"openssh-server":  true,
	"openssh-client":  true,
	"openssl":         true,
	"libssl3":         true,
	"libssl1.1":       true,
	"ca-certificates": true,
	"curl":            true,
	"libcurl4":        true,
	"sudo":            true,
	"tzdata":          true,
	"zlib1g":          true,
	"docker-ce":       true,
	"docker-ce-cli":   true,
	"containerd.io":   true,
	"iptables":        true,
	"ipset":           true,
	"ufw":             true,
	"nftables":        true,
}

var (
	reDebianPkgName = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{0,127}$`)
	reDebianPkgVer  = regexp.MustCompile(`^[0-9][0-9a-zA-Z.+:~-]{0,127}$`)
)

// ValidatePackageName enforces Debian package naming syntax and rejects flags/shell metachars.
func ValidatePackageName(pkg string) error {
	pkg = strings.TrimSpace(pkg)
	if pkg == "" || strings.HasPrefix(pkg, "-") || !reDebianPkgName.MatchString(pkg) {
		return fmt.Errorf("%w: %q", ErrInvalidPackageName, pkg)
	}
	return nil
}

// ValidatePackageVersion enforces Debian version syntax without shell metacharacters.
func ValidatePackageVersion(ver string) error {
	ver = strings.TrimSpace(ver)
	if ver == "" {
		return nil
	}
	if strings.ContainsAny(ver, ";|&$`><\n\r \t/\\") || !reDebianPkgVer.MatchString(ver) {
		return fmt.Errorf("%w: %q", ErrInvalidPackageVersion, ver)
	}
	return nil
}

// HostExecRunner abstracts privileged host command execution for testability and strict argv enforcement.
type HostExecRunner interface {
	Run(ctx context.Context, env []string, name string, args ...string) (stdout string, stderr string, err error)
}

type realHostExecRunner struct{}

func (realHostExecRunner) Run(ctx context.Context, env []string, name string, args ...string) (string, string, error) {
	base := filepath.Base(strings.TrimSpace(name))
	if detectInContainer() {
		return "", "", fmt.Errorf("%w: direct %s execution inside container namespace is forbidden", ErrContainerHostExecutionBoundary, base)
	}
	allowedBins := map[string]bool{
		"apt-get":    true,
		"apt-cache":  true,
		"dpkg-query": true,
		"sshd":       true,
		"systemctl":  true,
	}
	if !allowedBins[base] {
		return "", "", fmt.Errorf("host update runner refused non-allowlisted binary: %s", base)
	}
	cmd := exec.CommandContext(ctx, base, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// SSHRecoveryGuard manages a persistent local dead-man's-switch that restores SSH config
// and restarts sshd if post-update verification or disarm fails after touching SSH/TLS.
type SSHRecoveryGuard interface {
	Arm(ctx context.Context, snapshotDir string, timeout time.Duration, recoverFn func(context.Context) error) (guardID string, err error)
	Disarm(guardID string) error
}

// HostUpdateRequest defines a human-approved host package upgrade operation.
type HostUpdateRequest struct {
	Proposal             *model.OpsProposal  `json:"proposal"`
	ApprovalID           string              `json:"approval_id,omitempty"`
	OperationID          string              `json:"operation_id,omitempty"`
	ResourceScope        string              `json:"resource_scope,omitempty"`
	FencingGeneration    int64               `json:"fencing_generation,omitempty"`
	ExecutionGrant       *HostExecutionGrant `json:"execution_grant,omitempty"`
	Packages             []string            `json:"packages"`
	TargetVersions       map[string]string   `json:"target_versions,omitempty"` // optional pinned versions
	ExpectedBaselineHash string              `json:"expected_baseline_hash"`
	WatchdogWindow       time.Duration       `json:"watchdog_window,omitempty"`
	Timeout              time.Duration       `json:"timeout,omitempty"`
	SSHProbeAddr         string              `json:"ssh_probe_addr,omitempty"` // default "127.0.0.1:22"
}

// HostUpdateSnapshot records pre-update package versions and backed-up config files.
type HostUpdateSnapshot struct {
	SnapshotID      string            `json:"snapshot_id"`
	SnapshotDir     string            `json:"snapshot_dir"`
	PlatformDigest  string            `json:"platform_digest"`
	PackageVersions map[string]string `json:"package_versions"`
	ConfigHashes    map[string]string `json:"config_hashes"`
	CopiedFiles     map[string]string `json:"copied_files"` // original rel path -> backup path
	CreatedAt       time.Time         `json:"created_at"`
}

// HostUpdateResult summarizes the outcome of HostUpdateExecutor.Execute.
type HostUpdateResult struct {
	Success        bool              `json:"success"`
	OperationID    string            `json:"operation_id"`
	Platform       PlatformInfo      `json:"platform"`
	BeforeVersions map[string]string `json:"before_versions,omitempty"`
	AfterVersions  map[string]string `json:"after_versions,omitempty"`
	SnapshotPath   string            `json:"snapshot_path,omitempty"`
	Verified       bool              `json:"verified"`
	RolledBack     bool              `json:"rolled_back"`
	Error          string            `json:"error,omitempty"`
	DurationMs     int64             `json:"duration_ms"`
}

// HostUpdateExecutor performs fail-closed, audited, rollback-capable host OS package upgrades (R9.4).
type HostUpdateExecutor struct {
	Detector              *PlatformDetector
	Runner                HostExecRunner
	Bridge                RestrictedHostExecutionBridge
	GrantAuthority        *HostGrantAuthority
	GrantRevocationDir    string
	RequireFencing        bool
	RequireExecutionGrant bool
	AuditEngine           *audit.AgentAuditEngine
	SnapshotRoot          string
	AllowedPackages       map[string]bool
	SSHGuard              SSHRecoveryGuard
	SSHRecoveryWindow     time.Duration
	SSHProbeFn            func(ctx context.Context, addr string) error

	grantConfigErr error
}

// NewHostUpdateExecutor constructs a HostUpdateExecutor with safe fail-closed defaults,
// persistent disk-backed SSH dead-man's-switch guard, and optional env-configured HMAC bridge.
func NewHostUpdateExecutor(hostRoot, snapshotRoot string, auditEngine *audit.AgentAuditEngine) *HostUpdateExecutor {
	if strings.TrimSpace(snapshotRoot) == "" {
		snapshotRoot = filepath.Join(os.TempDir(), "home-datacenter-host-snapshots")
	}
	allowedCopy := make(map[string]bool, len(AllowedHostPackages))
	for k, v := range AllowedHostPackages {
		allowedCopy[k] = v
	}
	detector := NewPlatformDetector(hostRoot)
	var bridge RestrictedHostExecutionBridge
	if envBridge := NewHMACHostExecutionBridgeFromEnv(); envBridge != nil {
		bridge = envBridge
	}
	grantAuth, grantErr := NewHostGrantAuthorityFromEnv()
	reqGrant := (bridge != nil && bridge.BridgeInfo().RequireGrant)
	if envRG := strings.TrimSpace(os.Getenv("OPS_HOST_BRIDGE_REQUIRE_GRANT")); envRG != "" {
		reqGrant = envRG != "0" && !strings.EqualFold(envRG, "false")
	}
	guardDir := strings.TrimSpace(os.Getenv("HOST_SSH_WATCHDOG_DIR"))
	if guardDir == "" {
		guardDir = filepath.Join(snapshotRoot, "ssh-watchdog")
	}
	runner := realHostExecRunner{}
	sshGuard := NewPersistentHostSSHGuard(guardDir, detector.HostRoot, snapshotRoot, runner, bridge)
	sshGuard.AuditEngine = auditEngine
	if bridge != nil {
		sshGuard.DelegateWatchdogToBridge = true
	}
	return &HostUpdateExecutor{
		Detector:              detector,
		Runner:                runner,
		Bridge:                bridge,
		GrantAuthority:        grantAuth,
		GrantRevocationDir:    filepath.Join(snapshotRoot, "bridge-grants"),
		RequireExecutionGrant: reqGrant,
		AuditEngine:           auditEngine,
		SnapshotRoot:          snapshotRoot,
		AllowedPackages:       allowedCopy,
		SSHGuard:              sshGuard,
		SSHRecoveryWindow:     DefaultSSHRecoveryWindow,
		SSHProbeFn:            defaultSSHProbe,
		grantConfigErr:        grantErr,
	}
}

// SetBridge configures the authenticated RestrictedHostExecutionBridge and updates SSHGuard.
func (e *HostUpdateExecutor) SetBridge(bridge RestrictedHostExecutionBridge) {
	e.Bridge = bridge
	if bridge != nil {
		e.RequireExecutionGrant = bridge.BridgeInfo().RequireGrant
	}
	if pg, ok := e.SSHGuard.(*PersistentHostSSHGuard); ok && pg != nil {
		pg.Bridge = bridge
		if bridge != nil {
			pg.DelegateWatchdogToBridge = true
		}
	}
}

// ReconcilePendingSSHGuards recovers any expired ARMED SSH dead-man's-switch guards persisted
// across prior container restarts or API crashes.
func (e *HostUpdateExecutor) ReconcilePendingSSHGuards(ctx context.Context) (int, error) {
	if pg, ok := e.SSHGuard.(*PersistentHostSSHGuard); ok && pg != nil {
		return pg.ReconcileExpired(ctx)
	}
	return 0, nil
}

func (e *HostUpdateExecutor) resolvePath(rel string) string {
	if e.Detector == nil {
		return filepath.Join("/", rel)
	}
	return e.Detector.resolvePath(rel)
}

// ComputeBaselineHash computes a deterministic SHA-256 fingerprint over host platform,
// target package versions, and critical SSH/Docker config files (anti-TOCTOU).
func (e *HostUpdateExecutor) ComputeBaselineHash(ctx context.Context, packages []string) (string, map[string]string, error) {
	if e.Detector == nil {
		e.Detector = NewPlatformDetector("")
	}
	platform := e.Detector.Detect(ctx)
	var sortedPkgs []string
	if len(packages) > 0 {
		sortedPkgs = append([]string{}, packages...)
	} else {
		pkgSource := e.AllowedPackages
		if len(pkgSource) == 0 {
			pkgSource = AllowedHostPackages
		}
		sortedPkgs = make([]string, 0, len(pkgSource))
		for k, allowed := range pkgSource {
			if allowed {
				sortedPkgs = append(sortedPkgs, k)
			}
		}
	}
	sort.Strings(sortedPkgs)

	versions := make(map[string]string, len(sortedPkgs))
	if rawDpkg, err := os.ReadFile(e.resolvePath("var/lib/dpkg/status")); err == nil {
		pkgList := ParseDpkgStatus(string(rawDpkg))
		pkgMap := make(map[string]string, len(pkgList))
		for _, p := range pkgList {
			pkgMap[p.Name] = p.Version
		}
		for _, name := range sortedPkgs {
			versions[name] = pkgMap[name]
		}
	} else if e.Runner != nil {
		for _, name := range sortedPkgs {
			out, _, qErr := e.Runner.Run(ctx, nil, "dpkg-query", "-W", "-f=${Version}", name)
			if qErr == nil {
				versions[name] = strings.TrimSpace(out)
			}
		}
	}

	cfgHashes := make(map[string]string)
	for _, rel := range []string{"etc/ssh/sshd_config", "etc/docker/daemon.json"} {
		if b, err := os.ReadFile(e.resolvePath(rel)); err == nil {
			sum := sha256.Sum256(b)
			cfgHashes[rel] = fmt.Sprintf("%x", sum[:])
		}
	}

	payload, _ := json.Marshal(struct {
		PlatformDigest string            `json:"platform_digest"`
		Packages       map[string]string `json:"packages"`
		ConfigHashes   map[string]string `json:"config_hashes"`
	}{
		PlatformDigest: platform.Digest(),
		Packages:       versions,
		ConfigHashes:   cfgHashes,
	})
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("host-v1:%x", sum[:]), versions, nil
}

// AuthorizeMutation enforces Tier E privilege isolation:
// tiers A (assets), B (CVE scan), C (proposal), D (approval) do NOT grant mutation capability
// if platform policy (fnOS fail-closed), fencing, or container execution boundary rejects it.
func (e *HostUpdateExecutor) AuthorizeMutation(ctx context.Context, platform PlatformInfo, binding HostExecutionBinding) error {
	if polErr := platform.HostUpdatePolicyError(); polErr != nil {
		return polErr
	}
	if e.RequireFencing && binding.FencingGeneration <= 0 {
		return fmt.Errorf("%w: fencing_generation (> 0) required for host update", ErrBridgeBindingInvalid)
	}
	if platform.RequiresHostExecutionBridge() {
		if e.Bridge == nil {
			return fmt.Errorf("%w: direct host mutation forbidden (InContainer=%v, HostRoot=%q)", ErrContainerHostExecutionBoundary, platform.InContainer, platform.HostRoot)
		}
		if err := e.Bridge.VerifyBinding(ctx, binding); err != nil {
			return fmt.Errorf("%w: %v", ErrContainerHostExecutionBoundary, err)
		}
	}
	return nil
}

func (e *HostUpdateExecutor) runTypedHostAction(ctx context.Context, platform PlatformInfo, req HostActionRequest) (HostActionResult, error) {
	if e.Bridge != nil {
		return e.Bridge.ExecuteTypedAction(ctx, req)
	}
	if platform.RequiresHostExecutionBridge() {
		return HostActionResult{}, fmt.Errorf("%w: refusing direct %s across namespace boundary without RestrictedHostExecutionBridge", ErrContainerHostExecutionBoundary, req.Action)
	}
	if err := ValidateHostActionRequest(req, e.AllowedPackages, e.RequireFencing); err != nil {
		return HostActionResult{}, err
	}
	if e.Runner == nil {
		return HostActionResult{}, errors.New("host command runner is not configured")
	}
	return dispatchTypedBridgeAction(ctx, e.Runner, e.Detector, e.SnapshotRoot, req)
}

// Execute validates OS compatibility (failing closed on fnOS), package whitelist,
// human approval HMAC, container namespace execution boundary, baseline hash, apt repository
// GPG trust, takes a pre-update snapshot, arms a persistent crash-survivable SSH guard,
// runs strictly typed non-interactive upgrade, verifies post-update health & SSH,
// and triggers automatic rollback + SSH recovery on failure.
func (e *HostUpdateExecutor) Execute(ctx context.Context, req HostUpdateRequest) (*HostUpdateResult, error) {
	start := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	if e.Detector == nil {
		e.Detector = NewPlatformDetector("")
	}
	if e.Runner == nil {
		e.Runner = realHostExecRunner{}
	}
	if e.SSHGuard == nil {
		e.SSHGuard = NewPersistentHostSSHGuard("", e.Detector.HostRoot, e.SnapshotRoot, e.Runner, e.Bridge)
	}
	if e.SSHProbeFn == nil {
		e.SSHProbeFn = defaultSSHProbe
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultUpdateTimeout
	}
	if timeout > MaxUpdateTimeout {
		timeout = MaxUpdateTimeout
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	opID := strings.TrimSpace(req.OperationID)
	if opID == "" {
		opID = fmt.Sprintf("hostupd-%d", time.Now().UnixNano())
	} else if !reSafeSnapshotID.MatchString(opID) {
		return &HostUpdateResult{OperationID: opID}, fmt.Errorf("%w: invalid operation_id %q", ErrBridgeBindingInvalid, req.OperationID)
	}
	platform := e.Detector.Detect(execCtx)

	res := &HostUpdateResult{
		OperationID: opID,
		Platform:    platform,
	}

	var propID, approvalID, actorID string
	var hhID uint = 1
	if req.Proposal != nil {
		propID = req.Proposal.ProposalID
		approvalID = strings.TrimSpace(req.ApprovalID)
		if approvalID == "" {
			approvalID = req.Proposal.ApprovalDigest
		}
		actorID = req.Proposal.ApprovedBy
		if req.Proposal.HouseholdID > 0 {
			hhID = req.Proposal.HouseholdID
		}
	}
	if actorID == "" {
		actorID = "admin"
	}

	_, hasParentSession := audit.SessionFromContext(execCtx)
	runCtx := execCtx
	if e.AuditEngine != nil && !hasParentSession {
		_, rCtx, sErr := e.AuditEngine.StartRun(execCtx, audit.StartRunInput{
			HouseholdID:       hhID,
			TriggerSource:     "host_update_executor",
			ActorType:         "user",
			ActorID:           actorID,
			Action:            ActionHostPackageUpgrade,
			Target:            strings.Join(req.Packages, ","),
			Component:         "host-os",
			Decision:          model.AuditDecisionAllow,
			RiskLevel:         model.AuditRiskCritical,
			ProposalID:        propID,
			ApprovalID:        approvalID,
			OperationID:       opID,
			FencingGeneration: req.FencingGeneration,
			Summary:           fmt.Sprintf("Host OS package update (%v) on %s", req.Packages, platform.PrettyName),
		})
		if sErr != nil {
			res.Error = sErr.Error()
			res.DurationMs = time.Since(start).Milliseconds()
			return res, fmt.Errorf("%w: %v", audit.ErrAuditCommitFailed, sErr)
		}
		runCtx = rCtx
	}

	emitTerminalOrStep := func(in audit.EmitEventInput) error {
		if e.AuditEngine == nil {
			return nil
		}
		if hasParentSession {
			in.FinalRunStatus = ""
			in.MirrorLegacy = false
			_, err := e.AuditEngine.EmitEvent(runCtx, in)
			return err
		}
		_, err := e.AuditEngine.RecordPostMutation(runCtx, in, nil)
		return err
	}

	failAudit := func(code string, cause error) (*HostUpdateResult, error) {
		res.Success = false
		res.Error = cause.Error()
		res.DurationMs = time.Since(start).Milliseconds()
		_ = emitTerminalOrStep(audit.EmitEventInput{
			EventType:         model.AuditEventExecutionFailed,
			Action:            ActionHostPackageUpgrade,
			Target:            strings.Join(req.Packages, ","),
			Component:         "host-os",
			Decision:          model.AuditDecisionDeny,
			RiskLevel:         model.AuditRiskCritical,
			ProposalID:        propID,
			ApprovalID:        approvalID,
			OperationID:       opID,
			FencingGeneration: req.FencingGeneration,
			BeforeState:       res.BeforeVersions,
			AfterState:        res.AfterVersions,
			Result:            code,
			ErrorCode:         cause.Error(),
			DurationMs:        res.DurationMs,
			FinalRunStatus:    model.AuditRunStatusBlocked,
			MirrorLegacy:      true,
		})
		return res, cause
	}

	// 1. fnOS / Unsupported OS Fail-Closed Compatibility Gate (R9.5)
	if polErr := platform.HostUpdatePolicyError(); polErr != nil {
		return failAudit("BLOCKED_UNSUPPORTED_OS", polErr)
	}

	// 2. Validate package list, naming regex, and closed whitelist (deduplicated)
	if len(req.Packages) == 0 {
		return failAudit("INVALID_PACKAGES", fmt.Errorf("%w: empty package list", ErrInvalidPackageName))
	}
	cleanPkgs := make([]string, 0, len(req.Packages))
	seenPkgs := make(map[string]bool, len(req.Packages))
	touchesSSH := false
	for _, rawPkg := range req.Packages {
		pkg := strings.TrimSpace(rawPkg)
		if err := ValidatePackageName(pkg); err != nil {
			return failAudit("INVALID_PACKAGE_NAME", err)
		}
		if !e.AllowedPackages[pkg] {
			return failAudit("PACKAGE_NOT_WHITELISTED", fmt.Errorf("%w: %s", ErrPackageNotWhitelisted, pkg))
		}
		if ver, ok := req.TargetVersions[pkg]; ok {
			if err := ValidatePackageVersion(ver); err != nil {
				return failAudit("INVALID_PACKAGE_VERSION", err)
			}
		}
		if pkg == "openssh-server" || pkg == "openssh-client" || pkg == "openssl" || pkg == "libssl3" || pkg == "libssl1.1" {
			touchesSSH = true
		}
		if !seenPkgs[pkg] {
			seenPkgs[pkg] = true
			cleanPkgs = append(cleanPkgs, pkg)
		}
	}

	// 3. Human approval verification (HMAC-SHA256) + ActionSpec package binding
	if req.Proposal == nil {
		return failAudit("APPROVAL_MISSING", ErrApprovalRequired)
	}
	if req.Proposal.Status != model.ProposalStatusApproved && req.Proposal.Status != model.ProposalStatusExecuting {
		return failAudit("APPROVAL_STATUS_INVALID", fmt.Errorf("%w: proposal status is %s", ErrApprovalRequired, req.Proposal.Status))
	}
	if err := model.VerifyProposalApprovalDigest(req.Proposal); err != nil {
		return failAudit("APPROVAL_HMAC_INVALID", fmt.Errorf("%w: %v", ErrApprovalRequired, err))
	}
	if err := verifyProposalAuthorizesHostPackages(req.Proposal, cleanPkgs); err != nil {
		return failAudit("APPROVAL_SCOPE_MISMATCH", err)
	}

	// 4. Container & cross-namespace host execution boundary gate (fail-closed if bridge missing)
	if platform.RequiresHostExecutionBridge() && e.Bridge == nil {
		return failAudit("CONTAINER_EXECUTION_BOUNDARY", fmt.Errorf("%w: InContainer=%v HostRoot=%q", ErrContainerHostExecutionBoundary, platform.InContainer, platform.HostRoot))
	}

	// 5. Baseline verification (anti-TOCTOU)
	liveBaseline, beforeVersions, err := e.ComputeBaselineHash(execCtx, cleanPkgs)
	if err != nil {
		return failAudit("BASELINE_ERROR", err)
	}
	res.BeforeVersions = beforeVersions
	expectedBase := strings.TrimSpace(req.ExpectedBaselineHash)
	baselineMatched := expectedBase != "" && expectedBase == liveBaseline
	if !baselineMatched && expectedBase != "" {
		if hostWideBaseline, _, hbErr := e.ComputeBaselineHash(execCtx, nil); hbErr == nil && expectedBase == hostWideBaseline {
			baselineMatched = true
		}
	}
	if !baselineMatched {
		return failAudit("BASELINE_DRIFT", fmt.Errorf("%w: expected=%q got=%q", ErrBaselineDrift, req.ExpectedBaselineHash, liveBaseline))
	}

	binding := HostExecutionBinding{
		ProposalID:        propID,
		ApprovalID:        approvalID,
		OperationID:       opID,
		ResourceScope:     strings.TrimSpace(req.ResourceScope),
		BaselineHash:      liveBaseline,
		FencingGeneration: req.FencingGeneration,
		HouseholdID:       hhID,
		ActorID:           actorID,
	}
	if authErr := e.AuthorizeMutation(execCtx, platform, binding); authErr != nil {
		return failAudit("AUTHORIZATION_BOUNDARY_DENIED", authErr)
	}
	if e.grantConfigErr != nil {
		return failAudit("GRANT_CONFIG_ERROR", e.grantConfigErr)
	}
	if e.RequireExecutionGrant || (e.Bridge != nil && e.Bridge.BridgeInfo().RequireGrant) || req.ExecutionGrant != nil {
		if req.ExecutionGrant == nil {
			return failAudit("EXECUTION_GRANT_MISSING", ErrExecutionGrantRequired)
		}
		if e.GrantAuthority != nil {
			grantCheckReq := HostActionRequest{
				Action:         HostActionAptUpgrade,
				Binding:        binding,
				Packages:       cleanPkgs,
				TargetVersions: req.TargetVersions,
			}
			requireFence := e.RequireFencing || (e.Bridge != nil && e.Bridge.BridgeInfo().Fenced)
			if gErr := e.GrantAuthority.VerifyGrant(req.ExecutionGrant, grantCheckReq, nil, requireFence, time.Now().UTC()); gErr != nil {
				return failAudit("EXECUTION_GRANT_DENIED", gErr)
			}
		} else if e.Bridge == nil {
			return failAudit("EXECUTION_GRANT_AUTHORITY_MISSING", fmt.Errorf("%w: grant authority required to verify grant offline", ErrExecutionGrantInvalid))
		}
		if e.GrantRevocationDir != "" {
			if _, statErr := os.Stat(e.GrantRevocationDir); statErr == nil {
				gs, gsErr := NewPersistentGrantStore(e.GrantRevocationDir)
				if gsErr != nil {
					return failAudit("GRANT_STORE_ERROR", gsErr)
				}
				revoked, rErr := gs.IsRevoked(req.ExecutionGrant.GrantID)
				if rErr != nil {
					return failAudit("GRANT_REVOCATION_CHECK_FAILED", rErr)
				}
				if revoked {
					return failAudit("EXECUTION_GRANT_REVOKED", fmt.Errorf("%w: grant %s", ErrExecutionGrantRevoked, req.ExecutionGrant.GrantID))
				}
			}
		}
	}

	// 6. Repository source & GPG signature policy verification
	if err := e.verifyAptSecurity(execCtx, platform, binding, req.ExecutionGrant, cleanPkgs); err != nil {
		return failAudit("APT_SOURCE_UNTRUSTED", err)
	}

	grantID := ""
	grantKeyID := ""
	if req.ExecutionGrant != nil {
		grantID = req.ExecutionGrant.GrantID
		grantKeyID = req.ExecutionGrant.KeyID
	}
	execAuditParams := map[string]interface{}{
		"packages":           cleanPkgs,
		"target_versions":    req.TargetVersions,
		"baseline_hash":      liveBaseline,
		"fencing_generation": req.FencingGeneration,
		"grant_id":           grantID,
		"grant_key_id":       grantKeyID,
	}

	// 7. Fail-Closed Pre-Mutation Audit Gate (HC-1)
	if e.AuditEngine != nil && !hasParentSession {
		if _, preErr := e.AuditEngine.RecordPreMutation(runCtx, audit.EmitEventInput{
			EventType:         model.AuditEventExecutionStarted,
			Action:            ActionHostPackageUpgrade,
			Target:            strings.Join(cleanPkgs, ","),
			Component:         "host-os",
			Decision:          model.AuditDecisionAllow,
			RiskLevel:         model.AuditRiskCritical,
			ProposalID:        propID,
			ApprovalID:        approvalID,
			OperationID:       opID,
			FencingGeneration: req.FencingGeneration,
			Parameters:         execAuditParams,
			DecisionReason:     "host package upgrade preflight and grant verified",
			BaselineBeforeHash: liveBaseline,
			BeforeState:        beforeVersions,
			Result:             "STARTED",
			FailurePolicy:      audit.PolicyFailClosed,
		}); preErr != nil {
			res.Error = preErr.Error()
			res.DurationMs = time.Since(start).Milliseconds()
			return res, fmt.Errorf("%w: %v", audit.ErrAuditCommitFailed, preErr)
		}
	} else if e.AuditEngine != nil && hasParentSession {
		if _, preErr := e.AuditEngine.EmitEvent(runCtx, audit.EmitEventInput{
			EventType:          model.AuditEventExecutionStepCompleted,
			Action:             ActionHostPackageUpgrade + ".preflight",
			Target:             strings.Join(cleanPkgs, ","),
			Component:          "host-os",
			Decision:           model.AuditDecisionAllow,
			RiskLevel:          model.AuditRiskCritical,
			ProposalID:         propID,
			ApprovalID:         approvalID,
			OperationID:        opID,
			FencingGeneration:  req.FencingGeneration,
			Parameters:         execAuditParams,
			DecisionReason:     "host package upgrade preflight and grant verified",
			BaselineBeforeHash: liveBaseline,
			BeforeState:        beforeVersions,
			Result:             "PREFLIGHT_VERIFIED",
			FailurePolicy:      audit.PolicyFailClosed,
		}); preErr != nil {
			res.Error = preErr.Error()
			res.DurationMs = time.Since(start).Milliseconds()
			return res, fmt.Errorf("%w: %v", audit.ErrAuditCommitFailed, preErr)
		}
	}

	// 8. Pre-update configuration & package snapshot
	snap, snapErr := e.createPreUpdateSnapshot(opID, platform, beforeVersions)
	if snapErr != nil {
		res.Error = snapErr.Error()
		res.DurationMs = time.Since(start).Milliseconds()
		_ = emitTerminalOrStep(audit.EmitEventInput{
			EventType:         model.AuditEventExecutionFailed,
			Action:            ActionHostPackageUpgrade,
			Target:            strings.Join(cleanPkgs, ","),
			Component:         "host-os",
			RiskLevel:         model.AuditRiskCritical,
			ProposalID:        propID,
			ApprovalID:        approvalID,
			OperationID:       opID,
			FencingGeneration: req.FencingGeneration,
			Result:            "SNAPSHOT_FAILED",
			ErrorCode:         snapErr.Error(),
			DurationMs:        res.DurationMs,
			FinalRunStatus:    model.AuditRunStatusFailed,
		})
		return res, fmt.Errorf("%w: %v", ErrSnapshotFailed, snapErr)
	}
	res.SnapshotPath = snap.SnapshotDir

	if e.AuditEngine != nil {
		if _, evErr := e.AuditEngine.EmitEvent(runCtx, audit.EmitEventInput{
			EventType:         model.AuditEventExecutionStepCompleted,
			Action:            "host.pkg_upgrade.snapshot",
			Target:            strings.Join(cleanPkgs, ","),
			Component:         "host-os",
			RiskLevel:         model.AuditRiskMedium,
			ProposalID:        propID,
			ApprovalID:        approvalID,
			OperationID:       opID,
			FencingGeneration: req.FencingGeneration,
			AfterState:        map[string]interface{}{"snapshot_dir": snap.SnapshotDir, "before_versions": beforeVersions},
			Result:            "SNAPSHOT_CREATED",
		}); evErr != nil {
			res.Error = evErr.Error()
			res.DurationMs = time.Since(start).Milliseconds()
			return res, evErr
		}
	}

	// 9. Arm persistent disk-backed SSH-lockout recovery dead-man's-switch if touching SSH/OpenSSL
	var guardID string
	if touchesSSH {
		win := req.WatchdogWindow
		if win <= 0 {
			if req.ExecutionGrant != nil && len(req.ExecutionGrant.StepParamDigests) > 0 {
				armDigest := req.ExecutionGrant.StepParamDigests[string(HostActionWatchdogArm)]
				if armDigest != "" {
					testDef := ComputeStepParamDigest(HostActionRequest{
						Action:            HostActionWatchdogArm,
						SnapshotID:        snap.SnapshotID,
						GuardID:           snap.SnapshotID,
						WatchdogTimeoutMs: DefaultSSHRecoveryWindow.Milliseconds(),
					})
					if testDef == armDigest {
						win = DefaultSSHRecoveryWindow
					} else if e.SSHRecoveryWindow > 0 {
						testWin := ComputeStepParamDigest(HostActionRequest{
							Action:            HostActionWatchdogArm,
							SnapshotID:        snap.SnapshotID,
							GuardID:           snap.SnapshotID,
							WatchdogTimeoutMs: e.SSHRecoveryWindow.Milliseconds(),
						})
						if testWin == armDigest {
							win = e.SSHRecoveryWindow
						}
					}
				}
			}
		}
		if win <= 0 {
			win = e.SSHRecoveryWindow
		}
		if win <= 0 {
			win = DefaultSSHRecoveryWindow
		}
		recoverFn := func(rCtx context.Context) error {
			return e.rollbackFromSnapshot(rCtx, platform, binding, req.ExecutionGrant, snap, cleanPkgs)
		}
		var gID string
		var gErr error
		if pg, ok := e.SSHGuard.(*PersistentHostSSHGuard); ok && pg != nil {
			gID, gErr = pg.ArmWithBinding(execCtx, snap.SnapshotDir, win, binding, req.ExecutionGrant, recoverFn)
		} else {
			gID, gErr = e.SSHGuard.Arm(execCtx, snap.SnapshotDir, win, recoverFn)
		}
		if gErr != nil {
			res.Error = gErr.Error()
			res.DurationMs = time.Since(start).Milliseconds()
			return res, fmt.Errorf("failed to arm SSH lockout guard: %w", gErr)
		}
		guardID = gID
	}

	// 10. Execute strictly typed upgrade action via RestrictedHostExecutionBridge (never shell or container apt-get)
	upgradeRes, aptErr := e.runTypedHostAction(execCtx, platform, HostActionRequest{
		Action:         HostActionAptUpgrade,
		Binding:        binding,
		Grant:          req.ExecutionGrant,
		Packages:       cleanPkgs,
		TargetVersions: req.TargetVersions,
	})
	if aptErr != nil {
		rbErr := e.rollbackFromSnapshot(context.Background(), platform, binding, req.ExecutionGrant, snap, cleanPkgs)
		if guardID != "" {
			_ = e.SSHGuard.Disarm(guardID)
		}
		res.RolledBack = rbErr == nil
		errMsg := fmt.Sprintf("apt-get upgrade failed: %v (%s)", aptErr, strings.TrimSpace(upgradeRes.Stderr))
		if rbErr != nil {
			errMsg = fmt.Sprintf("%s; rollback error: %v", errMsg, rbErr)
		}
		res.Error = errMsg
		res.DurationMs = time.Since(start).Milliseconds()
		finalStatus := model.AuditRunStatusRolledBack
		failEventType := model.AuditEventRollbackCompleted
		rollbackResult := "ROLLED_BACK_TO_SNAPSHOT"
		if rbErr != nil {
			finalStatus = model.AuditRunStatusRollbackFailed
			failEventType = model.AuditEventRollbackFailed
			rollbackResult = "ROLLBACK_FAILED: " + rbErr.Error()
		}
		_ = emitTerminalOrStep(audit.EmitEventInput{
			EventType:          failEventType,
			Action:             ActionHostPackageUpgrade,
			Target:             strings.Join(cleanPkgs, ","),
			Component:          "host-os",
			RiskLevel:          model.AuditRiskCritical,
			ProposalID:         propID,
			ApprovalID:         approvalID,
			OperationID:        opID,
			FencingGeneration:  req.FencingGeneration,
			Parameters:         map[string]interface{}{"grant_id": grantID, "rolled_back": res.RolledBack, "fencing_generation": req.FencingGeneration},
			BaselineBeforeHash: liveBaseline,
			RollbackResult:     rollbackResult,
			BeforeState:        beforeVersions,
			Result:             strings.ToUpper(string(finalStatus)),
			ErrorCode:          errMsg,
			DurationMs:         res.DurationMs,
			FinalRunStatus:     finalStatus,
			MirrorLegacy:       true,
		})
		return res, fmt.Errorf("%w: %s", ErrRollbackAttempted, errMsg)
	}

	// 11. Post-update verification (installed version + sshd -t + SSH connectivity)
	afterVersions, verifyErr := e.verifyPostUpdate(execCtx, platform, binding, req.ExecutionGrant, cleanPkgs, req.TargetVersions, touchesSSH, req.SSHProbeAddr)
	res.AfterVersions = afterVersions
	if verifyErr != nil {
		rbErr := e.rollbackFromSnapshot(context.Background(), platform, binding, req.ExecutionGrant, snap, cleanPkgs)
		if guardID != "" {
			_ = e.SSHGuard.Disarm(guardID)
		}
		res.RolledBack = rbErr == nil
		errMsg := fmt.Sprintf("%v: %s", ErrPostVerifyFailed, verifyErr.Error())
		if rbErr != nil {
			errMsg = fmt.Sprintf("%s; rollback error: %v", errMsg, rbErr)
		}
		res.Error = errMsg
		res.DurationMs = time.Since(start).Milliseconds()
		finalStatus := model.AuditRunStatusRolledBack
		rbEventType := model.AuditEventRollbackCompleted
		rollbackResult := "ROLLED_BACK_TO_SNAPSHOT"
		if rbErr != nil {
			finalStatus = model.AuditRunStatusRollbackFailed
			rbEventType = model.AuditEventRollbackFailed
			rollbackResult = "ROLLBACK_FAILED: " + rbErr.Error()
		}
		_ = emitTerminalOrStep(audit.EmitEventInput{
			EventType:          rbEventType,
			Action:             ActionHostPackageUpgrade,
			Target:             strings.Join(cleanPkgs, ","),
			Component:          "host-os",
			RiskLevel:          model.AuditRiskCritical,
			ProposalID:         propID,
			ApprovalID:         approvalID,
			OperationID:        opID,
			FencingGeneration:  req.FencingGeneration,
			Parameters:         map[string]interface{}{"grant_id": grantID, "rolled_back": res.RolledBack, "fencing_generation": req.FencingGeneration},
			BaselineBeforeHash: liveBaseline,
			RollbackResult:     rollbackResult,
			BeforeState:        beforeVersions,
			AfterState:         afterVersions,
			Result:             strings.ToUpper(string(finalStatus)),
			ErrorCode:          errMsg,
			DurationMs:         res.DurationMs,
			FinalRunStatus:     finalStatus,
			MirrorLegacy:       true,
		})
		return res, fmt.Errorf("%w: %s", ErrRollbackAttempted, errMsg)
	}

	if guardID != "" {
		if dErr := e.SSHGuard.Disarm(guardID); dErr != nil {
			res.RolledBack = true
			res.Error = dErr.Error()
			res.DurationMs = time.Since(start).Milliseconds()
			return res, dErr
		}
	}

	res.Success = true
	res.Verified = true
	res.DurationMs = time.Since(start).Milliseconds()
	afterBaseline, _, _ := e.ComputeBaselineHash(execCtx, cleanPkgs)

	if postErr := emitTerminalOrStep(audit.EmitEventInput{
		EventType:          model.AuditEventExecutionCompleted,
		Action:             ActionHostPackageUpgrade,
		Target:             strings.Join(cleanPkgs, ","),
		Component:          "host-os",
		Decision:           model.AuditDecisionAllow,
		RiskLevel:          model.AuditRiskCritical,
		ProposalID:         propID,
		ApprovalID:         approvalID,
		OperationID:        opID,
		FencingGeneration:  req.FencingGeneration,
		Parameters:         execAuditParams,
		DecisionReason:     "host package upgrade verified cleanly",
		BaselineBeforeHash: liveBaseline,
		BaselineAfterHash:  afterBaseline,
		RollbackResult:     "NOT_NEEDED",
		BeforeState:        beforeVersions,
		AfterState:         afterVersions,
		Result:            "SUCCESS",
		DurationMs:        res.DurationMs,
		FinalRunStatus:    model.AuditRunStatusCompleted,
		MirrorLegacy:      true,
	}); postErr != nil {
		res.Success = false
		res.Error = postErr.Error()
		return res, fmt.Errorf("%w: %v", audit.ErrAuditCommitFailed, postErr)
	}

	return res, nil
}

func (e *HostUpdateExecutor) verifyAptSecurity(ctx context.Context, platform PlatformInfo, binding HostExecutionBinding, grant *HostExecutionGrant, pkgs []string) error {
	// Reject explicitly unsigned/trusted=yes APT source overrides
	sourceFiles := []string{e.resolvePath("etc/apt/sources.list")}
	if entries, err := os.ReadDir(e.resolvePath("etc/apt/sources.list.d")); err == nil {
		for _, ent := range entries {
			if !ent.IsDir() {
				sourceFiles = append(sourceFiles, e.resolvePath(filepath.Join("etc/apt/sources.list.d", ent.Name())))
			}
		}
	}
	for _, sf := range sourceFiles {
		b, err := os.ReadFile(sf)
		if err != nil {
			continue
		}
		lower := strings.ToLower(string(b))
		if strings.Contains(lower, "trusted=yes") {
			return fmt.Errorf("%w: insecure [trusted=yes] override found in %s", ErrInsecureAptSource, sf)
		}
	}

	// Verify apt-cache policy shows a signed candidate origin (not purely local/unsigned)
	for _, pkg := range pkgs {
		actRes, err := e.runTypedHostAction(ctx, platform, HostActionRequest{
			Action:   HostActionAptCachePolicy,
			Binding:  binding,
			Grant:    grant,
			Packages: []string{pkg},
		})
		if err != nil {
			return fmt.Errorf("%w: apt-cache policy %s failed: %v (%s)", ErrInsecureAptSource, pkg, err, strings.TrimSpace(actRes.Stderr))
		}
		if strings.TrimSpace(actRes.Stdout) == "" || strings.Contains(actRes.Stdout, "Candidate: (none)") {
			return fmt.Errorf("%w: no upgrade candidate for %s", ErrInsecureAptSource, pkg)
		}
	}
	return nil
}

func (e *HostUpdateExecutor) createPreUpdateSnapshot(opID string, platform PlatformInfo, beforeVersions map[string]string) (*HostUpdateSnapshot, error) {
	snapDir := filepath.Join(e.SnapshotRoot, opID)
	if err := os.MkdirAll(snapDir, 0700); err != nil {
		return nil, err
	}
	snap := &HostUpdateSnapshot{
		SnapshotID:      opID,
		SnapshotDir:     snapDir,
		PlatformDigest:  platform.Digest(),
		PackageVersions: beforeVersions,
		ConfigHashes:    make(map[string]string),
		CopiedFiles:     make(map[string]string),
		CreatedAt:       time.Now().UTC(),
	}

	filesToBackup := []string{"etc/ssh/sshd_config", "etc/docker/daemon.json", "var/lib/dpkg/status"}
	if entries, err := os.ReadDir(e.resolvePath("etc/ssh/sshd_config.d")); err == nil {
		for _, ent := range entries {
			if !ent.IsDir() {
				filesToBackup = append(filesToBackup, filepath.Join("etc/ssh/sshd_config.d", ent.Name()))
			}
		}
	}

	for _, rel := range filesToBackup {
		src := e.resolvePath(rel)
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		snap.ConfigHashes[rel] = fmt.Sprintf("%x", sum[:])
		dst := filepath.Join(snapDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, data, 0600); err != nil {
			return nil, err
		}
		snap.CopiedFiles[rel] = dst
	}

	manifestBytes, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), manifestBytes, 0600); err != nil {
		return nil, err
	}
	return snap, nil
}

func (e *HostUpdateExecutor) verifyPostUpdate(
	ctx context.Context,
	platform PlatformInfo,
	binding HostExecutionBinding,
	grant *HostExecutionGrant,
	pkgs []string,
	targetVersions map[string]string,
	touchesSSH bool,
	sshProbeAddr string,
) (map[string]string, error) {
	after := make(map[string]string, len(pkgs))
	for _, pkg := range pkgs {
		actRes, err := e.runTypedHostAction(ctx, platform, HostActionRequest{
			Action:   HostActionDpkgQuery,
			Binding:  binding,
			Grant:    grant,
			Packages: []string{pkg},
		})
		if err != nil {
			return after, fmt.Errorf("dpkg-query failed for %s: %v (%s)", pkg, err, strings.TrimSpace(actRes.Stderr))
		}
		parts := strings.SplitN(strings.TrimSpace(actRes.Stdout), "\t", 2)
		if len(parts) != 2 || !strings.Contains(parts[0], "install ok installed") {
			return after, fmt.Errorf("package %s not cleanly installed: %q", pkg, strings.TrimSpace(actRes.Stdout))
		}
		ver := strings.TrimSpace(parts[1])
		after[pkg] = ver
		if expected := strings.TrimSpace(targetVersions[pkg]); expected != "" && ver != expected {
			return after, fmt.Errorf("package %s version mismatch after update: expected %s got %s", pkg, expected, ver)
		}
	}

	if touchesSSH {
		if actRes, err := e.runTypedHostAction(ctx, platform, HostActionRequest{
			Action:  HostActionSSHDTest,
			Binding: binding,
			Grant:   grant,
		}); err != nil {
			return after, fmt.Errorf("sshd -t config check failed after update: %v (%s)", err, strings.TrimSpace(actRes.Stderr))
		}
		addr := strings.TrimSpace(sshProbeAddr)
		if addr == "" {
			addr = "127.0.0.1:22"
		}
		if err := e.SSHProbeFn(ctx, addr); err != nil {
			return after, fmt.Errorf("post-update SSH reachability probe failed (%s): %w", addr, err)
		}
	}
	return after, nil
}

func (e *HostUpdateExecutor) rollbackFromSnapshot(ctx context.Context, platform PlatformInfo, binding HostExecutionBinding, grant *HostExecutionGrant, snap *HostUpdateSnapshot, pkgs []string) error {
	if snap == nil {
		return errors.New("rollback snapshot is nil")
	}
	var errs []string
	// 1. Restore backed-up config files (SSH config first to prevent lockout)
	if e.Bridge != nil {
		if _, err := e.runTypedHostAction(ctx, platform, HostActionRequest{
			Action:     HostActionRestoreConfig,
			Binding:    binding,
			Grant:      grant,
			SnapshotID: snap.SnapshotID,
		}); err != nil {
			errs = append(errs, fmt.Sprintf("bridge restore config failed: %v", err))
		}
	} else {
		for rel, backupPath := range snap.CopiedFiles {
			if rel == "var/lib/dpkg/status" {
				continue
			}
			data, err := os.ReadFile(backupPath)
			if err != nil {
				errs = append(errs, fmt.Sprintf("read backup %s: %v", backupPath, err))
				continue
			}
			dst := e.resolvePath(rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				errs = append(errs, fmt.Sprintf("mkdir %s: %v", dst, err))
				continue
			}
			if err := os.WriteFile(dst, data, 0644); err != nil {
				errs = append(errs, fmt.Sprintf("restore %s: %v", dst, err))
			}
		}
	}

	// 2. Downgrade modified packages back to pre-update versions when known
	var downgradePkgs []string
	downgradeVersions := make(map[string]string)
	for _, pkg := range pkgs {
		if oldVer := strings.TrimSpace(snap.PackageVersions[pkg]); oldVer != "" {
			if ValidatePackageName(pkg) == nil && ValidatePackageVersion(oldVer) == nil {
				downgradePkgs = append(downgradePkgs, pkg)
				downgradeVersions[pkg] = oldVer
			}
		}
	}
	if len(downgradePkgs) > 0 {
		if actRes, err := e.runTypedHostAction(ctx, platform, HostActionRequest{
			Action:         HostActionAptDowngrade,
			Binding:        binding,
			Grant:          grant,
			Packages:       downgradePkgs,
			TargetVersions: downgradeVersions,
		}); err != nil {
			errs = append(errs, fmt.Sprintf("package downgrade failed: %v (%s)", err, strings.TrimSpace(actRes.Stderr)))
		}
	}

	// 3. Reload/restart sshd if SSH config was part of snapshot
	if _, hasSSH := snap.CopiedFiles["etc/ssh/sshd_config"]; hasSSH {
		if actRes, err := e.runTypedHostAction(ctx, platform, HostActionRequest{
			Action:  HostActionSSHDRestart,
			Binding: binding,
			Grant:   grant,
		}); err != nil {
			errs = append(errs, fmt.Sprintf("ssh restart failed: %v (%s)", err, strings.TrimSpace(actRes.Stderr)))
		}
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func verifyProposalAuthorizesHostPackages(p *model.OpsProposal, requested []string) error {
	if p == nil || strings.TrimSpace(p.ActionSpec) == "" {
		return fmt.Errorf("%w: proposal missing host.pkg_upgrade action_spec", ErrApprovalRequired)
	}
	var spec struct {
		Action string                 `json:"action"`
		Params map[string]interface{} `json:"params"`
	}
	if err := json.Unmarshal([]byte(p.ActionSpec), &spec); err != nil {
		return fmt.Errorf("%w: invalid action_spec JSON: %v", ErrApprovalRequired, err)
	}
	if spec.Action != ActionHostPackageUpgrade {
		return fmt.Errorf("%w: proposal action %q != %q", ErrApprovalRequired, spec.Action, ActionHostPackageUpgrade)
	}
	rawPkgs, ok := spec.Params["packages"].([]interface{})
	if !ok || len(rawPkgs) == 0 {
		return fmt.Errorf("%w: proposal action_spec missing params.packages", ErrApprovalRequired)
	}
	allowedInProposal := make(map[string]bool, len(rawPkgs))
	for _, item := range rawPkgs {
		if s, ok := item.(string); ok {
			allowedInProposal[strings.TrimSpace(s)] = true
		}
	}
	for _, reqPkg := range requested {
		if !allowedInProposal[reqPkg] {
			return fmt.Errorf("%w: package %q not authorized in approved proposal", ErrApprovalRequired, reqPkg)
		}
	}
	return nil
}

func defaultSSHProbe(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil && err != io.EOF {
		return err
	}
	if n == 0 || !strings.HasPrefix(string(buf[:n]), "SSH-") {
		return fmt.Errorf("unexpected SSH banner from %s: %q", addr, string(buf[:n]))
	}
	return nil
}
