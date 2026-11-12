package hostsec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	SSHGuardStatusArmed          = "ARMED"
	SSHGuardStatusDisarmed       = "DISARMED"
	SSHGuardStatusRecovered      = "RECOVERED"
	SSHGuardStatusRecoveryFailed = "RECOVERY_FAILED"
)

// PersistentSSHGuardState is written atomically to disk inside GuardDir so SSH dead-man's-switch
// recovery survives container restarts, API crashes, or helper termination.
type PersistentSSHGuardState struct {
	GuardID           string                `json:"guard_id"`
	OperationID       string                `json:"operation_id,omitempty"`
	ProposalID        string                `json:"proposal_id,omitempty"`
	ApprovalID        string                `json:"approval_id,omitempty"`
	BaselineHash      string                `json:"baseline_hash,omitempty"`
	FencingGeneration int64                 `json:"fencing_generation,omitempty"`
	SnapshotID        string                `json:"snapshot_id,omitempty"`
	SnapshotDir       string                `json:"snapshot_dir"`
	HostRoot          string                `json:"host_root"`
	ArmedAt           time.Time             `json:"armed_at"`
	DeadlineAt        time.Time             `json:"deadline_at"`
	Packages          []string              `json:"packages,omitempty"`
	PackageVersions   map[string]string     `json:"package_versions,omitempty"`
	CopiedFiles       map[string]string     `json:"copied_files,omitempty"`
	ExternalUnit      string                `json:"external_unit,omitempty"`
	BridgeDelegated   bool                  `json:"bridge_delegated,omitempty"`
	ScriptPath        string                `json:"script_path,omitempty"`
	Status            string                `json:"status"`
	DisarmedAt        *time.Time            `json:"disarmed_at,omitempty"`
	RecoveredAt       *time.Time            `json:"recovered_at,omitempty"`
	LastError         string                `json:"last_error,omitempty"`
}

// ExternalWatchdogBackend abstracts host-level out-of-process timer scheduling (e.g., detached OS process or systemd-run)
// so countdown continues even if the Go API container crashes or stops.
type ExternalWatchdogBackend interface {
	Schedule(ctx context.Context, guardID string, timeout time.Duration, scriptPath string) (unitName string, err error)
	Cancel(ctx context.Context, unitName string) error
}

// PersistentHostSSHGuard implements a disk-persisted, crash-survivable SSH dead-man's-switch.
type PersistentHostSSHGuard struct {
	GuardDir                 string
	HostRoot                 string
	SnapshotRoot             string
	Runner                   HostExecRunner
	Bridge                   RestrictedHostExecutionBridge
	ExternalWatchdog         ExternalWatchdogBackend
	DelegateWatchdogToBridge bool
	DisableLocalTimer        bool // test hook to simulate immediate API crash after Arm
	FallbackRecoverFn        func(ctx context.Context, state PersistentSSHGuardState) error

	mu             sync.Mutex
	timers         map[string]*time.Timer
	bridgeBindings map[string]struct {
		binding HostExecutionBinding
		grant   *HostExecutionGrant
	}
}

// NewPersistentHostSSHGuard constructs a disk-backed SSH dead-man's-switch guard.
func NewPersistentHostSSHGuard(guardDir, hostRoot, snapshotRoot string, runner HostExecRunner, bridge RestrictedHostExecutionBridge) *PersistentHostSSHGuard {
	if strings.TrimSpace(guardDir) == "" {
		if strings.TrimSpace(snapshotRoot) != "" {
			guardDir = filepath.Join(snapshotRoot, "ssh-watchdog")
		} else {
			guardDir = filepath.Join(os.TempDir(), "home-datacenter-ssh-watchdog")
		}
	}
	return &PersistentHostSSHGuard{
		GuardDir:     filepath.Clean(guardDir),
		HostRoot:     hostRoot,
		SnapshotRoot: snapshotRoot,
		Runner:       runner,
		Bridge:       bridge,
		timers:       make(map[string]*time.Timer),
		bridgeBindings: make(map[string]struct {
			binding HostExecutionBinding
			grant   *HostExecutionGrant
		}),
	}
}

func (g *PersistentHostSSHGuard) lockFilePath() string {
	return filepath.Join(g.GuardDir, ".guard.lock")
}

func (g *PersistentHostSSHGuard) disarmedFlagPath(guardID string) string {
	return filepath.Join(g.GuardDir, guardID+".disarmed")
}

func (g *PersistentHostSSHGuard) recoveredTemplatePath(guardID string) string {
	return filepath.Join(g.GuardDir, guardID+".recovered.tmpl")
}

func (g *PersistentHostSSHGuard) failedTemplatePath(guardID string) string {
	return filepath.Join(g.GuardDir, guardID+".failed.tmpl")
}

func (g *PersistentHostSSHGuard) stateFilePath(guardID string) string {
	return filepath.Join(g.GuardDir, guardID+".json")
}

func (g *PersistentHostSSHGuard) scriptFilePath(guardID string) string {
	return filepath.Join(g.GuardDir, guardID+"-recover.sh")
}

func (g *PersistentHostSSHGuard) writeStateAtomic(state PersistentSSHGuardState) error {
	if err := os.MkdirAll(g.GuardDir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomicFailClosed(g.stateFilePath(state.GuardID), data, 0600)
}

func (g *PersistentHostSSHGuard) writeStatusTemplates(state PersistentSSHGuardState) error {
	sentinel := time.Date(1999, 9, 9, 9, 9, 9, 0, time.UTC)
	recState := state
	recState.Status = SSHGuardStatusRecovered
	recState.RecoveredAt = &sentinel
	recBytes, err := json.MarshalIndent(recState, "", "  ")
	if err != nil {
		return err
	}
	recContent := strings.ReplaceAll(string(recBytes), "1999-09-09T09:09:09Z", "__RECOVERED_AT__")
	if err := writeFileAtomicFailClosed(g.recoveredTemplatePath(state.GuardID), []byte(recContent), 0600); err != nil {
		return err
	}

	failState := state
	failState.Status = SSHGuardStatusRecoveryFailed
	failState.RecoveredAt = &sentinel
	failState.LastError = "detached host watchdog recovery command failed"
	failBytes, err := json.MarshalIndent(failState, "", "  ")
	if err != nil {
		return err
	}
	failContent := strings.ReplaceAll(string(failBytes), "1999-09-09T09:09:09Z", "__RECOVERED_AT__")
	return writeFileAtomicFailClosed(g.failedTemplatePath(state.GuardID), []byte(failContent), 0600)
}

func (g *PersistentHostSSHGuard) readState(guardID string) (*PersistentSSHGuardState, error) {
	b, err := os.ReadFile(g.stateFilePath(guardID))
	if err != nil {
		return nil, err
	}
	var st PersistentSSHGuardState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Arm persists guard metadata + standalone host recovery script to GuardDir before any SSH change,
// schedules an external watchdog if available, and starts an in-process deadline timer.
func (g *PersistentHostSSHGuard) Arm(
	ctx context.Context,
	snapshotDir string,
	timeout time.Duration,
	recoverFn func(context.Context) error,
) (string, error) {
	return g.ArmWithBinding(ctx, snapshotDir, timeout, HostExecutionBinding{}, nil, recoverFn)
}

// ArmWithBinding persists guard metadata, optionally delegates out-of-process watchdog scheduling
// across RestrictedHostExecutionBridge (surviving API container termination), and arms timers.
func (g *PersistentHostSSHGuard) ArmWithBinding(
	ctx context.Context,
	snapshotDir string,
	timeout time.Duration,
	binding HostExecutionBinding,
	grant *HostExecutionGrant,
	recoverFn func(context.Context) error,
) (string, error) {
	now := time.Now().UTC()
	guardID := fmt.Sprintf("ssh-guard-%d", now.UnixNano())
	return g.armWithGuardID(ctx, guardID, now, snapshotDir, timeout, binding, grant, recoverFn)
}

func (g *PersistentHostSSHGuard) armWithGuardID(
	ctx context.Context,
	guardID string,
	now time.Time,
	snapshotDir string,
	timeout time.Duration,
	binding HostExecutionBinding,
	grant *HostExecutionGrant,
	recoverFn func(context.Context) error,
) (string, error) {
	if !reSafeSnapshotID.MatchString(strings.TrimSpace(guardID)) {
		return "", fmt.Errorf("invalid guard_id %q", guardID)
	}
	if timeout <= 0 {
		timeout = DefaultSSHRecoveryWindow
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	detector := NewPlatformDetector(g.HostRoot)

	state := PersistentSSHGuardState{
		GuardID:           guardID,
		OperationID:       strings.TrimSpace(binding.OperationID),
		ProposalID:        strings.TrimSpace(binding.ProposalID),
		ApprovalID:        strings.TrimSpace(binding.ApprovalID),
		BaselineHash:      strings.TrimSpace(binding.BaselineHash),
		FencingGeneration: binding.FencingGeneration,
		SnapshotDir:       filepath.Clean(snapshotDir),
		HostRoot:          detector.HostRoot,
		ArmedAt:           now,
		DeadlineAt:        now.Add(timeout),
		ScriptPath:        g.scriptFilePath(guardID),
		Status:            SSHGuardStatusArmed,
	}

	if snapshotDir != "" {
		if mb, err := os.ReadFile(filepath.Join(snapshotDir, "manifest.json")); err == nil {
			var snap HostUpdateSnapshot
			if jErr := json.Unmarshal(mb, &snap); jErr == nil {
				if state.OperationID == "" {
					state.OperationID = snap.SnapshotID
				}
				state.SnapshotID = snap.SnapshotID
				state.PackageVersions = snap.PackageVersions
				state.CopiedFiles = snap.CopiedFiles
				for pkg := range snap.PackageVersions {
					if pkg != "" {
						state.Packages = append(state.Packages, pkg)
					}
				}
			}
		}
	}

	unlock, lErr := acquireCrossProcessFileLock(g.lockFilePath())
	if lErr != nil {
		return "", fmt.Errorf("acquire SSH guard lock: %w", lErr)
	}
	if err := removeFileFailClosed(g.disarmedFlagPath(guardID)); err != nil {
		_ = unlock()
		return "", fmt.Errorf("clear SSH guard disarmed marker: %w", err)
	}
	if err := g.writeRecoveryScript(detector, state); err != nil {
		_ = unlock()
		return "", fmt.Errorf("write persistent SSH recovery script: %w", err)
	}
	if err := g.writeStatusTemplates(state); err != nil {
		_ = unlock()
		return "", fmt.Errorf("write SSH guard state templates: %w", err)
	}
	// Persist initial ARMED state BEFORE launching out-of-process watchdog so disk state exists on wakeup.
	if err := g.writeStateAtomic(state); err != nil {
		_ = unlock()
		return "", fmt.Errorf("persist initial SSH guard state: %w", err)
	}
	if uErr := unlock(); uErr != nil {
		return "", fmt.Errorf("release SSH guard lock: %w", uErr)
	}

	if g.DelegateWatchdogToBridge && g.Bridge != nil && state.SnapshotID != "" {
		if _, err := g.Bridge.ExecuteTypedAction(ctx, HostActionRequest{
			Action:            HostActionWatchdogArm,
			Binding:           binding,
			Grant:             grant,
			SnapshotID:        state.SnapshotID,
			GuardID:           guardID,
			WatchdogTimeoutMs: timeout.Milliseconds(),
		}); err != nil {
			return "", fmt.Errorf("delegate host bridge SSH watchdog arm: %w", err)
		}
		state.BridgeDelegated = true
		g.mu.Lock()
		if g.bridgeBindings == nil {
			g.bridgeBindings = make(map[string]struct {
				binding HostExecutionBinding
				grant   *HostExecutionGrant
			})
		}
		g.bridgeBindings[guardID] = struct {
			binding HostExecutionBinding
			grant   *HostExecutionGrant
		}{binding: binding, grant: grant}
		g.mu.Unlock()
	}

	if g.ExternalWatchdog != nil {
		unitName, wErr := g.ExternalWatchdog.Schedule(ctx, guardID, timeout, state.ScriptPath)
		if wErr != nil {
			return "", fmt.Errorf("schedule external SSH watchdog: %w", wErr)
		}
		state.ExternalUnit = unitName
	}

	if state.BridgeDelegated || state.ExternalUnit != "" {
		unlock2, lErr2 := acquireCrossProcessFileLock(g.lockFilePath())
		if lErr2 != nil {
			return "", fmt.Errorf("acquire SSH guard lock for unit persist: %w", lErr2)
		}
		if latest, rErr := g.readState(guardID); rErr == nil && latest != nil {
			if state.ExternalUnit == "" && latest.ExternalUnit != "" {
				state.ExternalUnit = latest.ExternalUnit
			}
		}
		wErr := g.writeStateAtomic(state)
		uErr := unlock2()
		if wErr != nil {
			return "", fmt.Errorf("persist SSH guard unit state: %w", wErr)
		}
		if uErr != nil {
			return "", fmt.Errorf("release SSH guard lock: %w", uErr)
		}
	}

	if !g.DisableLocalTimer {
		timer := time.AfterFunc(timeout, func() {
			g.mu.Lock()
			if _, ok := g.timers[guardID]; !ok {
				g.mu.Unlock()
				return
			}
			delete(g.timers, guardID)
			g.mu.Unlock()

			rCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = g.triggerRecovery(rCtx, guardID, recoverFn)
		})
		g.mu.Lock()
		g.timers[guardID] = timer
		g.mu.Unlock()
	}

	return guardID, nil
}

// Disarm stops any local/external watchdog and records DISARMED state on disk.
// Fails if recovery already fired (even in a prior crashed process or detached watchdog).
func (g *PersistentHostSSHGuard) Disarm(guardID string) error {
	g.mu.Lock()
	if t, ok := g.timers[guardID]; ok {
		t.Stop()
		delete(g.timers, guardID)
	}
	bound, hasBound := g.bridgeBindings[guardID]
	delete(g.bridgeBindings, guardID)
	extBackend := g.ExternalWatchdog
	bridge := g.Bridge
	g.mu.Unlock()

	unlock, lErr := acquireCrossProcessFileLock(g.lockFilePath())
	if lErr != nil {
		return fmt.Errorf("acquire SSH guard lock on disarm: %w", lErr)
	}
	st, err := g.readState(guardID)
	if err != nil {
		_ = unlock()
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read SSH guard state on disarm: %w", err)
	}
	if st.Status == SSHGuardStatusRecovered || st.Status == SSHGuardStatusRecoveryFailed {
		_ = unlock()
		return fmt.Errorf("SSH recovery guard %s already triggered rollback (status=%s)", guardID, st.Status)
	}
	if st.Status == SSHGuardStatusDisarmed {
		_ = unlock()
		return nil
	}
	// Persist .disarmed marker + DISARMED state atomically BEFORE stopping external process
	if fErr := writeFileAtomicFailClosed(g.disarmedFlagPath(guardID), []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0600); fErr != nil {
		_ = unlock()
		return fmt.Errorf("persist SSH guard disarmed marker: %w", fErr)
	}
	now := time.Now().UTC()
	st.Status = SSHGuardStatusDisarmed
	st.DisarmedAt = &now
	wErr := g.writeStateAtomic(*st)
	extUnit := st.ExternalUnit
	bridgeDelegated := st.BridgeDelegated
	uErr := unlock()
	if wErr != nil {
		return wErr
	}
	if uErr != nil {
		return uErr
	}

	if extUnit != "" && extBackend != nil {
		cCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = extBackend.Cancel(cCtx, extUnit)
		cancel()
	}
	if bridgeDelegated && bridge != nil {
		bBinding := bound.binding
		var bGrant *HostExecutionGrant
		if hasBound {
			bGrant = bound.grant
		} else {
			bBinding = HostExecutionBinding{
				ProposalID:        st.ProposalID,
				ApprovalID:        st.ApprovalID,
				OperationID:       st.OperationID,
				BaselineHash:      st.BaselineHash,
				FencingGeneration: st.FencingGeneration,
			}
		}
		dCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, dErr := bridge.ExecuteTypedAction(dCtx, HostActionRequest{
			Action:  HostActionWatchdogDisarm,
			Binding: bBinding,
			Grant:   bGrant,
			GuardID: guardID,
		})
		cancel()
		if dErr != nil {
			return dErr
		}
	}
	return nil
}

// StopLocalTimers stops in-memory timers WITHOUT disarming persisted disk state,
// simulating process crash / container kill while leaving the guard ARMED on disk.
func (g *PersistentHostSSHGuard) StopLocalTimers() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, t := range g.timers {
		t.Stop()
		delete(g.timers, id)
	}
}

// ReconcileExpired inspects persistent guard files in GuardDir (typically on process/container startup)
// and recovers any still-ARMED guard whose deadline elapsed while the API was offline or crashed.
func (g *PersistentHostSSHGuard) ReconcileExpired(ctx context.Context) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	entries, err := os.ReadDir(g.GuardDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	now := time.Now().UTC()
	recoveredCount := 0
	var errs []string
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") || strings.HasSuffix(ent.Name(), ".tmp") {
			continue
		}
		guardID := strings.TrimSuffix(ent.Name(), ".json")
		st, rErr := g.readState(guardID)
		if rErr != nil || st.Status != SSHGuardStatusArmed {
			continue
		}
		if now.Before(st.DeadlineAt) {
			continue
		}
		if recErr := g.triggerRecovery(ctx, guardID, nil); recErr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", guardID, recErr))
			continue
		}
		recoveredCount++
	}
	if len(errs) > 0 {
		return recoveredCount, errors.New(strings.Join(errs, "; "))
	}
	return recoveredCount, nil
}

// LoadState returns the current on-disk state for guardID.
func (g *PersistentHostSSHGuard) LoadState(guardID string) (*PersistentSSHGuardState, error) {
	return g.readState(guardID)
}

func (g *PersistentHostSSHGuard) triggerRecovery(ctx context.Context, guardID string, liveRecoverFn func(context.Context) error) error {
	unlock, lErr := acquireCrossProcessFileLock(g.lockFilePath())
	if lErr != nil {
		return lErr
	}
	st, err := g.readState(guardID)
	if err != nil {
		_ = unlock()
		return err
	}
	if st.Status != SSHGuardStatusArmed {
		return unlock()
	}
	stateCopy := *st
	if uErr := unlock(); uErr != nil {
		return uErr
	}

	var recErr error
	switch {
	case liveRecoverFn != nil:
		recErr = liveRecoverFn(ctx)
	case g.FallbackRecoverFn != nil:
		recErr = g.FallbackRecoverFn(ctx, stateCopy)
	default:
		recErr = g.recoverFromPersistedState(ctx, stateCopy)
	}

	unlock2, lErr2 := acquireCrossProcessFileLock(g.lockFilePath())
	if lErr2 != nil {
		return lErr2
	}
	latest, lErr := g.readState(guardID)
	if lErr != nil {
		_ = unlock2()
		return lErr
	}
	if latest.Status != SSHGuardStatusArmed {
		return unlock2()
	}
	now := time.Now().UTC()
	latest.RecoveredAt = &now
	if recErr != nil {
		latest.Status = SSHGuardStatusRecoveryFailed
		latest.LastError = recErr.Error()
	} else {
		latest.Status = SSHGuardStatusRecovered
	}
	wErr := g.writeStateAtomic(*latest)
	uErr2 := unlock2()
	if wErr != nil {
		return wErr
	}
	if uErr2 != nil {
		return uErr2
	}
	return recErr
}

func (g *PersistentHostSSHGuard) recoverFromPersistedState(ctx context.Context, st PersistentSSHGuardState) error {
	detector := NewPlatformDetector(st.HostRoot)
	var errs []string

	// Restore SSH files from snapshot backup paths on disk
	if g.Bridge != nil && st.SnapshotID != "" && st.OperationID != "" {
		binding := HostExecutionBinding{
			ProposalID:  st.ProposalID,
			ApprovalID:  "persistent-ssh-guard",
			OperationID: st.OperationID,
			BaselineHash: "host-v1:guard",
		}
		if _, err := g.Bridge.ExecuteTypedAction(ctx, HostActionRequest{
			Action:     HostActionRestoreConfig,
			Binding:    binding,
			SnapshotID: st.SnapshotID,
		}); err != nil {
			errs = append(errs, err.Error())
		}
		if _, err := g.Bridge.ExecuteTypedAction(ctx, HostActionRequest{
			Action:  HostActionSSHDRestart,
			Binding: binding,
		}); err != nil {
			errs = append(errs, err.Error())
		}
	} else {
		// Direct file restore onto detector.HostRoot (when guard runs on host namespace / tests)
		for rel, backupPath := range st.CopiedFiles {
			if !isAllowedRestoreRelPath(rel) {
				continue
			}
			data, err := os.ReadFile(backupPath)
			if err != nil {
				errs = append(errs, fmt.Sprintf("read backup %s: %v", backupPath, err))
				continue
			}
			dst := detector.resolvePath(rel)
			if mkErr := os.MkdirAll(filepath.Dir(dst), 0755); mkErr != nil {
				errs = append(errs, mkErr.Error())
				continue
			}
			if wErr := os.WriteFile(dst, data, 0644); wErr != nil {
				errs = append(errs, wErr.Error())
			}
		}
		if g.Runner != nil && !detector.InDocker {
			if _, errOut, err := g.Runner.Run(ctx, nil, "systemctl", "restart", "ssh"); err != nil {
				if _, errOut2, err2 := g.Runner.Run(ctx, nil, "systemctl", "restart", "sshd"); err2 != nil {
					errs = append(errs, fmt.Sprintf("ssh restart failed: %v (%s) / %v (%s)", err, errOut, err2, errOut2))
				}
			}
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (g *PersistentHostSSHGuard) writeRecoveryScript(detector *PlatformDetector, st PersistentSSHGuardState) error {
	if err := os.MkdirAll(g.GuardDir, 0700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -eu\n")
	for rel, backupPath := range st.CopiedFiles {
		if !isAllowedRestoreRelPath(rel) {
			continue
		}
		dst := detector.resolvePath(rel)
		b.WriteString(fmt.Sprintf("if [ -f '%s' ]; then mkdir -p '%s' && cp -f '%s' '%s'; fi\n",
			shellEscapeSingle(backupPath),
			shellEscapeSingle(filepath.Dir(dst)),
			shellEscapeSingle(backupPath),
			shellEscapeSingle(dst),
		))
	}
	b.WriteString("if command -v sshd >/dev/null 2>&1; then sshd -t; fi\n")
	b.WriteString("if command -v systemctl >/dev/null 2>&1; then systemctl restart ssh || systemctl restart sshd || true; fi\n")
	return os.WriteFile(st.ScriptPath, []byte(b.String()), 0700)
}

func shellEscapeSingle(s string) string {
	return strings.ReplaceAll(s, "'", "'\\''")
}

// SystemdRunWatchdogBackend schedules an out-of-process transient systemd timer on Linux hosts.
type SystemdRunWatchdogBackend struct{}

func (SystemdRunWatchdogBackend) Schedule(ctx context.Context, guardID string, timeout time.Duration, scriptPath string) (string, error) {
	secs := int(timeout.Seconds())
	if secs < 1 {
		secs = 1
	}
	unit := "hdc-" + guardID
	cmd := exec.CommandContext(ctx, "systemd-run",
		"--unit="+unit,
		"--on-active="+fmt.Sprintf("%ds", secs),
		"--timer-property=AccuracySec=1s",
		"/bin/sh", scriptPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("systemd-run failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return unit + ".timer", nil
}

func (SystemdRunWatchdogBackend) Cancel(ctx context.Context, unitName string) error {
	if strings.TrimSpace(unitName) == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "systemctl", "stop", unitName)
	return cmd.Run()
}
