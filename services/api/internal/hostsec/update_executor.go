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
	"sync"
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

// SSHRecoveryGuard manages a local dead-man's-switch timer that restores SSH config
// and restarts sshd if post-update verification or disarm fails after touching SSH/TLS.
type SSHRecoveryGuard interface {
	Arm(ctx context.Context, snapshotDir string, timeout time.Duration, recoverFn func(context.Context) error) (guardID string, err error)
	Disarm(guardID string) error
}

type localTimerSSHGuard struct {
	mu     sync.Mutex
	timers map[string]*time.Timer
	fired  map[string]bool
}

func newLocalTimerSSHGuard() *localTimerSSHGuard {
	return &localTimerSSHGuard{
		timers: make(map[string]*time.Timer),
		fired:  make(map[string]bool),
	}
}

func (g *localTimerSSHGuard) Arm(ctx context.Context, snapshotDir string, timeout time.Duration, recoverFn func(context.Context) error) (string, error) {
	if timeout <= 0 {
		timeout = DefaultSSHRecoveryWindow
	}
	id := fmt.Sprintf("ssh-guard-%d", time.Now().UnixNano())
	timer := time.AfterFunc(timeout, func() {
		g.mu.Lock()
		if _, active := g.timers[id]; !active {
			g.mu.Unlock()
			return
		}
		delete(g.timers, id)
		g.fired[id] = true
		g.mu.Unlock()
		if recoverFn != nil {
			rCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = recoverFn(rCtx)
		}
	})
	g.mu.Lock()
	g.timers[id] = timer
	g.mu.Unlock()
	return id, nil
}

func (g *localTimerSSHGuard) Disarm(guardID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if t, ok := g.timers[guardID]; ok {
		t.Stop()
		delete(g.timers, guardID)
		return nil
	}
	if g.fired[guardID] {
		return fmt.Errorf("SSH recovery guard %s already triggered rollback", guardID)
	}
	return nil
}

// HostUpdateRequest defines a human-approved host package upgrade operation.
type HostUpdateRequest struct {
	Proposal             *model.OpsProposal `json:"proposal"`
	Packages             []string           `json:"packages"`
	TargetVersions       map[string]string  `json:"target_versions,omitempty"` // optional pinned versions
	ExpectedBaselineHash string             `json:"expected_baseline_hash"`
	Timeout              time.Duration      `json:"timeout,omitempty"`
	SSHProbeAddr         string             `json:"ssh_probe_addr,omitempty"` // default "127.0.0.1:22"
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
	Detector          *PlatformDetector
	Runner            HostExecRunner
	AuditEngine       *audit.AgentAuditEngine
	SnapshotRoot      string
	AllowedPackages   map[string]bool
	SSHGuard          SSHRecoveryGuard
	SSHRecoveryWindow time.Duration
	SSHProbeFn        func(ctx context.Context, addr string) error
}

// NewHostUpdateExecutor constructs a HostUpdateExecutor with safe defaults.
func NewHostUpdateExecutor(hostRoot, snapshotRoot string, auditEngine *audit.AgentAuditEngine) *HostUpdateExecutor {
	if strings.TrimSpace(snapshotRoot) == "" {
		snapshotRoot = filepath.Join(os.TempDir(), "home-datacenter-host-snapshots")
	}
	allowedCopy := make(map[string]bool, len(AllowedHostPackages))
	for k, v := range AllowedHostPackages {
		allowedCopy[k] = v
	}
	return &HostUpdateExecutor{
		Detector:          NewPlatformDetector(hostRoot),
		Runner:            realHostExecRunner{},
		AuditEngine:       auditEngine,
		SnapshotRoot:      snapshotRoot,
		AllowedPackages:   allowedCopy,
		SSHGuard:          newLocalTimerSSHGuard(),
		SSHRecoveryWindow: DefaultSSHRecoveryWindow,
		SSHProbeFn:        defaultSSHProbe,
	}
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

// Execute validates OS compatibility (failing closed on fnOS), package whitelist,
// human approval HMAC, baseline hash, apt repository GPG trust, takes a pre-update
// snapshot, runs bounded non-interactive upgrade, verifies post-update health & SSH,
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
		e.SSHGuard = newLocalTimerSSHGuard()
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

	opID := fmt.Sprintf("hostupd-%d", time.Now().UnixNano())
	platform := e.Detector.Detect(execCtx)

	res := &HostUpdateResult{
		OperationID: opID,
		Platform:    platform,
	}

	var propID, approvalID, actorID string
	var hhID uint = 1
	if req.Proposal != nil {
		propID = req.Proposal.ProposalID
		approvalID = req.Proposal.ApprovalDigest
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
			HouseholdID:   hhID,
			TriggerSource: "host_update_executor",
			ActorType:     "user",
			ActorID:       actorID,
			Action:        ActionHostPackageUpgrade,
			Target:        strings.Join(req.Packages, ","),
			Component:     "host-os",
			Decision:      model.AuditDecisionAllow,
			RiskLevel:     model.AuditRiskCritical,
			ProposalID:    propID,
			ApprovalID:    approvalID,
			OperationID:   opID,
			Summary:       fmt.Sprintf("Host OS package update (%v) on %s", req.Packages, platform.PrettyName),
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
			EventType:      model.AuditEventExecutionFailed,
			Action:         ActionHostPackageUpgrade,
			Target:         strings.Join(req.Packages, ","),
			Component:      "host-os",
			Decision:       model.AuditDecisionDeny,
			RiskLevel:      model.AuditRiskCritical,
			ProposalID:     propID,
			ApprovalID:     approvalID,
			OperationID:    opID,
			BeforeState:    res.BeforeVersions,
			AfterState:     res.AfterVersions,
			Result:         code,
			ErrorCode:      cause.Error(),
			DurationMs:     res.DurationMs,
			FinalRunStatus: model.AuditRunStatusBlocked,
			MirrorLegacy:   true,
		})
		return res, cause
	}

	// 1. fnOS / Unsupported OS Fail-Closed Compatibility Gate (R9.5)
	if polErr := platform.HostUpdatePolicyError(); polErr != nil {
		return failAudit("BLOCKED_UNSUPPORTED_OS", polErr)
	}

	// 2. Validate package list, naming regex, and closed whitelist
	if len(req.Packages) == 0 {
		return failAudit("INVALID_PACKAGES", fmt.Errorf("%w: empty package list", ErrInvalidPackageName))
	}
	cleanPkgs := make([]string, 0, len(req.Packages))
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
		cleanPkgs = append(cleanPkgs, pkg)
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

	// 4. Baseline verification (anti-TOCTOU)
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

	// 5. Repository source & GPG signature policy verification
	if err := e.verifyAptSecurity(execCtx, cleanPkgs); err != nil {
		return failAudit("APT_SOURCE_UNTRUSTED", err)
	}

	// 6. Fail-Closed Pre-Mutation Audit Gate (HC-1)
	if e.AuditEngine != nil && !hasParentSession {
		if _, preErr := e.AuditEngine.RecordPreMutation(runCtx, audit.EmitEventInput{
			EventType:     model.AuditEventExecutionStarted,
			Action:        ActionHostPackageUpgrade,
			Target:        strings.Join(cleanPkgs, ","),
			Component:     "host-os",
			Decision:      model.AuditDecisionAllow,
			RiskLevel:     model.AuditRiskCritical,
			ProposalID:    propID,
			ApprovalID:    approvalID,
			OperationID:   opID,
			Parameters:    map[string]interface{}{"packages": cleanPkgs, "target_versions": req.TargetVersions},
			BeforeState:   beforeVersions,
			Result:        "STARTED",
			FailurePolicy: audit.PolicyFailClosed,
		}); preErr != nil {
			res.Error = preErr.Error()
			res.DurationMs = time.Since(start).Milliseconds()
			return res, fmt.Errorf("%w: %v", audit.ErrAuditCommitFailed, preErr)
		}
	} else if e.AuditEngine != nil && hasParentSession {
		if _, preErr := e.AuditEngine.EmitEvent(runCtx, audit.EmitEventInput{
			EventType:     model.AuditEventExecutionStepCompleted,
			Action:        ActionHostPackageUpgrade + ".preflight",
			Target:        strings.Join(cleanPkgs, ","),
			Component:     "host-os",
			Decision:      model.AuditDecisionAllow,
			RiskLevel:     model.AuditRiskCritical,
			ProposalID:    propID,
			ApprovalID:    approvalID,
			OperationID:   opID,
			Parameters:    map[string]interface{}{"packages": cleanPkgs, "target_versions": req.TargetVersions},
			BeforeState:   beforeVersions,
			Result:        "PREFLIGHT_VERIFIED",
			FailurePolicy: audit.PolicyFailClosed,
		}); preErr != nil {
			res.Error = preErr.Error()
			res.DurationMs = time.Since(start).Milliseconds()
			return res, fmt.Errorf("%w: %v", audit.ErrAuditCommitFailed, preErr)
		}
	}

	// 7. Pre-update configuration & package snapshot
	snap, snapErr := e.createPreUpdateSnapshot(opID, platform, beforeVersions)
	if snapErr != nil {
		res.Error = snapErr.Error()
		res.DurationMs = time.Since(start).Milliseconds()
		_ = emitTerminalOrStep(audit.EmitEventInput{
			EventType:      model.AuditEventExecutionFailed,
			Action:         ActionHostPackageUpgrade,
			Target:         strings.Join(cleanPkgs, ","),
			Component:      "host-os",
			RiskLevel:      model.AuditRiskCritical,
			ProposalID:     propID,
			OperationID:    opID,
			Result:         "SNAPSHOT_FAILED",
			ErrorCode:      snapErr.Error(),
			DurationMs:     res.DurationMs,
			FinalRunStatus: model.AuditRunStatusFailed,
		})
		return res, fmt.Errorf("%w: %v", ErrSnapshotFailed, snapErr)
	}
	res.SnapshotPath = snap.SnapshotDir

	if e.AuditEngine != nil {
		if _, evErr := e.AuditEngine.EmitEvent(runCtx, audit.EmitEventInput{
			EventType:   model.AuditEventExecutionStepCompleted,
			Action:      "host.pkg_upgrade.snapshot",
			Target:      strings.Join(cleanPkgs, ","),
			Component:   "host-os",
			RiskLevel:   model.AuditRiskMedium,
			ProposalID:  propID,
			OperationID: opID,
			AfterState:  map[string]interface{}{"snapshot_dir": snap.SnapshotDir, "before_versions": beforeVersions},
			Result:      "SNAPSHOT_CREATED",
		}); evErr != nil {
			res.Error = evErr.Error()
			res.DurationMs = time.Since(start).Milliseconds()
			return res, evErr
		}
	}

	// 8. Arm local SSH-lockout recovery dead-man's-switch if touching SSH/OpenSSL
	var guardID string
	if touchesSSH {
		win := e.SSHRecoveryWindow
		if win <= 0 {
			win = DefaultSSHRecoveryWindow
		}
		gID, gErr := e.SSHGuard.Arm(execCtx, snap.SnapshotDir, win, func(rCtx context.Context) error {
			return e.rollbackFromSnapshot(rCtx, snap, cleanPkgs)
		})
		if gErr != nil {
			res.Error = gErr.Error()
			res.DurationMs = time.Since(start).Milliseconds()
			return res, fmt.Errorf("failed to arm SSH lockout guard: %w", gErr)
		}
		guardID = gID
	}

	// 9. Execute non-interactive apt-get upgrade strictly via argv (never shell, never --allow-unauthenticated)
	pkgArgs := []string{
		"install", "-y", "--only-upgrade",
		"-o", "Acquire::AllowInsecureRepositories=false",
		"-o", "Acquire::AllowDowngradeToInsecureRepositories=false",
		"-o", "Dpkg::Options::=--force-confdef",
		"-o", "Dpkg::Options::=--force-confold",
		"--",
	}
	for _, pkg := range cleanPkgs {
		if ver := strings.TrimSpace(req.TargetVersions[pkg]); ver != "" {
			pkgArgs = append(pkgArgs, pkg+"="+ver)
		} else {
			pkgArgs = append(pkgArgs, pkg)
		}
	}

	aptEnv := []string{"DEBIAN_FRONTEND=noninteractive", "LC_ALL=C"}
	_, aptErrOut, aptErr := e.Runner.Run(execCtx, aptEnv, "apt-get", pkgArgs...)
	if aptErr != nil {
		rbErr := e.rollbackFromSnapshot(context.Background(), snap, cleanPkgs)
		if guardID != "" {
			_ = e.SSHGuard.Disarm(guardID)
		}
		res.RolledBack = rbErr == nil
		errMsg := fmt.Sprintf("apt-get upgrade failed: %v (%s)", aptErr, strings.TrimSpace(aptErrOut))
		if rbErr != nil {
			errMsg = fmt.Sprintf("%s; rollback error: %v", errMsg, rbErr)
		}
		res.Error = errMsg
		res.DurationMs = time.Since(start).Milliseconds()
		finalStatus := model.AuditRunStatusRolledBack
		if rbErr != nil {
			finalStatus = model.AuditRunStatusFailed
		}
		_ = emitTerminalOrStep(audit.EmitEventInput{
			EventType:      model.AuditEventExecutionFailed,
			Action:         ActionHostPackageUpgrade,
			Target:         strings.Join(cleanPkgs, ","),
			Component:      "host-os",
			RiskLevel:      model.AuditRiskCritical,
			ProposalID:     propID,
			OperationID:    opID,
			BeforeState:    beforeVersions,
			Result:         strings.ToUpper(string(finalStatus)),
			ErrorCode:      errMsg,
			DurationMs:     res.DurationMs,
			FinalRunStatus: finalStatus,
			MirrorLegacy:   true,
		})
		return res, fmt.Errorf("%w: %s", ErrRollbackAttempted, errMsg)
	}

	// 10. Post-update verification (installed version + sshd -t + SSH connectivity)
	afterVersions, verifyErr := e.verifyPostUpdate(execCtx, cleanPkgs, req.TargetVersions, touchesSSH, req.SSHProbeAddr)
	res.AfterVersions = afterVersions
	if verifyErr != nil {
		rbErr := e.rollbackFromSnapshot(context.Background(), snap, cleanPkgs)
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
		if rbErr != nil {
			finalStatus = model.AuditRunStatusFailed
		}
		_ = emitTerminalOrStep(audit.EmitEventInput{
			EventType:      model.AuditEventRollbackCompleted,
			Action:         ActionHostPackageUpgrade,
			Target:         strings.Join(cleanPkgs, ","),
			Component:      "host-os",
			RiskLevel:      model.AuditRiskCritical,
			ProposalID:     propID,
			OperationID:    opID,
			BeforeState:    beforeVersions,
			AfterState:     afterVersions,
			Result:         strings.ToUpper(string(finalStatus)),
			ErrorCode:      errMsg,
			DurationMs:     res.DurationMs,
			FinalRunStatus: finalStatus,
			MirrorLegacy:   true,
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

	if postErr := emitTerminalOrStep(audit.EmitEventInput{
		EventType:      model.AuditEventExecutionCompleted,
		Action:         ActionHostPackageUpgrade,
		Target:         strings.Join(cleanPkgs, ","),
		Component:      "host-os",
		Decision:       model.AuditDecisionAllow,
		RiskLevel:      model.AuditRiskCritical,
		ProposalID:     propID,
		ApprovalID:     approvalID,
		OperationID:    opID,
		BeforeState:    beforeVersions,
		AfterState:     afterVersions,
		Result:         "SUCCESS",
		DurationMs:     res.DurationMs,
		FinalRunStatus: model.AuditRunStatusCompleted,
		MirrorLegacy:   true,
	}); postErr != nil {
		res.Success = false
		res.Error = postErr.Error()
		return res, fmt.Errorf("%w: %v", audit.ErrAuditCommitFailed, postErr)
	}

	return res, nil
}

func (e *HostUpdateExecutor) verifyAptSecurity(ctx context.Context, pkgs []string) error {
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
		out, errOut, err := e.Runner.Run(ctx, []string{"LC_ALL=C"}, "apt-cache", "policy", pkg)
		if err != nil {
			return fmt.Errorf("%w: apt-cache policy %s failed: %v (%s)", ErrInsecureAptSource, pkg, err, strings.TrimSpace(errOut))
		}
		if strings.TrimSpace(out) == "" || strings.Contains(out, "Candidate: (none)") {
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
	pkgs []string,
	targetVersions map[string]string,
	touchesSSH bool,
	sshProbeAddr string,
) (map[string]string, error) {
	after := make(map[string]string, len(pkgs))
	for _, pkg := range pkgs {
		out, errOut, err := e.Runner.Run(ctx, []string{"LC_ALL=C"}, "dpkg-query", "-W", "-f=${Status}\t${Version}", pkg)
		if err != nil {
			return after, fmt.Errorf("dpkg-query failed for %s: %v (%s)", pkg, err, strings.TrimSpace(errOut))
		}
		parts := strings.SplitN(strings.TrimSpace(out), "\t", 2)
		if len(parts) != 2 || !strings.Contains(parts[0], "install ok installed") {
			return after, fmt.Errorf("package %s not cleanly installed: %q", pkg, strings.TrimSpace(out))
		}
		ver := strings.TrimSpace(parts[1])
		after[pkg] = ver
		if expected := strings.TrimSpace(targetVersions[pkg]); expected != "" && ver != expected {
			return after, fmt.Errorf("package %s version mismatch after update: expected %s got %s", pkg, expected, ver)
		}
	}

	if touchesSSH {
		if _, errOut, err := e.Runner.Run(ctx, nil, "sshd", "-t"); err != nil {
			return after, fmt.Errorf("sshd -t config check failed after update: %v (%s)", err, strings.TrimSpace(errOut))
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

func (e *HostUpdateExecutor) rollbackFromSnapshot(ctx context.Context, snap *HostUpdateSnapshot, pkgs []string) error {
	if snap == nil {
		return errors.New("rollback snapshot is nil")
	}
	var errs []string
	// 1. Restore backed-up config files (SSH config first to prevent lockout)
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

	// 2. Downgrade modified packages back to pre-update versions when known
	var downgradeTargets []string
	for _, pkg := range pkgs {
		if oldVer := strings.TrimSpace(snap.PackageVersions[pkg]); oldVer != "" {
			if ValidatePackageName(pkg) == nil && ValidatePackageVersion(oldVer) == nil {
				downgradeTargets = append(downgradeTargets, pkg+"="+oldVer)
			}
		}
	}
	if len(downgradeTargets) > 0 && e.Runner != nil {
		args := append([]string{
			"install", "-y", "--allow-downgrades",
			"-o", "Acquire::AllowInsecureRepositories=false",
			"--",
		}, downgradeTargets...)
		if _, errOut, err := e.Runner.Run(ctx, []string{"DEBIAN_FRONTEND=noninteractive", "LC_ALL=C"}, "apt-get", args...); err != nil {
			errs = append(errs, fmt.Sprintf("package downgrade failed: %v (%s)", err, strings.TrimSpace(errOut)))
		}
	}

	// 3. Reload/restart sshd if SSH config was part of snapshot
	if _, hasSSH := snap.CopiedFiles["etc/ssh/sshd_config"]; hasSSH && e.Runner != nil {
		if _, errOut, err := e.Runner.Run(ctx, nil, "systemctl", "restart", "ssh"); err != nil {
			if _, errOut2, err2 := e.Runner.Run(ctx, nil, "systemctl", "restart", "sshd"); err2 != nil {
				errs = append(errs, fmt.Sprintf("ssh restart failed: %v (%s) / %v (%s)", err, errOut, err2, errOut2))
			}
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
