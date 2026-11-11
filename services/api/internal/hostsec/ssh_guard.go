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
	GuardID         string            `json:"guard_id"`
	OperationID     string            `json:"operation_id,omitempty"`
	ProposalID      string            `json:"proposal_id,omitempty"`
	SnapshotID      string            `json:"snapshot_id,omitempty"`
	SnapshotDir     string            `json:"snapshot_dir"`
	HostRoot        string            `json:"host_root"`
	ArmedAt         time.Time         `json:"armed_at"`
	DeadlineAt      time.Time         `json:"deadline_at"`
	Packages        []string          `json:"packages,omitempty"`
	PackageVersions map[string]string `json:"package_versions,omitempty"`
	CopiedFiles     map[string]string `json:"copied_files,omitempty"`
	ExternalUnit    string            `json:"external_unit,omitempty"`
	ScriptPath      string            `json:"script_path,omitempty"`
	Status          string            `json:"status"`
	DisarmedAt      *time.Time        `json:"disarmed_at,omitempty"`
	RecoveredAt     *time.Time        `json:"recovered_at,omitempty"`
	LastError       string            `json:"last_error,omitempty"`
}

// ExternalWatchdogBackend abstracts host-level out-of-process timer scheduling (e.g., systemd-run)
// so countdown continues even if the Go API container crashes or restarts.
type ExternalWatchdogBackend interface {
	Schedule(ctx context.Context, guardID string, timeout time.Duration, scriptPath string) (unitName string, err error)
	Cancel(ctx context.Context, unitName string) error
}

// PersistentHostSSHGuard implements a disk-persisted, crash-survivable SSH dead-man's-switch.
type PersistentHostSSHGuard struct {
	GuardDir          string
	HostRoot          string
	SnapshotRoot      string
	Runner            HostExecRunner
	Bridge            RestrictedHostExecutionBridge
	ExternalWatchdog  ExternalWatchdogBackend
	DisableLocalTimer bool // test hook to simulate immediate API crash after Arm
	FallbackRecoverFn func(ctx context.Context, state PersistentSSHGuardState) error

	mu     sync.Mutex
	timers map[string]*time.Timer
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
	}
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
	target := g.stateFilePath(state.GuardID)
	tmp := target + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, wErr := f.Write(data); wErr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return wErr
	}
	if sErr := f.Sync(); sErr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return sErr
	}
	if cErr := f.Close(); cErr != nil {
		_ = os.Remove(tmp)
		return cErr
	}
	return os.Rename(tmp, target)
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
	if timeout <= 0 {
		timeout = DefaultSSHRecoveryWindow
	}
	now := time.Now().UTC()
	guardID := fmt.Sprintf("ssh-guard-%d", now.UnixNano())
	detector := NewPlatformDetector(g.HostRoot)

	state := PersistentSSHGuardState{
		GuardID:     guardID,
		SnapshotDir: filepath.Clean(snapshotDir),
		HostRoot:    detector.HostRoot,
		ArmedAt:     now,
		DeadlineAt:  now.Add(timeout),
		ScriptPath:  g.scriptFilePath(guardID),
		Status:      SSHGuardStatusArmed,
	}

	if snapshotDir != "" {
		if mb, err := os.ReadFile(filepath.Join(snapshotDir, "manifest.json")); err == nil {
			var snap HostUpdateSnapshot
			if jErr := json.Unmarshal(mb, &snap); jErr == nil {
				state.OperationID = snap.SnapshotID
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

	if err := g.writeRecoveryScript(detector, state); err != nil {
		return "", fmt.Errorf("write persistent SSH recovery script: %w", err)
	}
	if g.ExternalWatchdog != nil {
		unitName, wErr := g.ExternalWatchdog.Schedule(ctx, guardID, timeout, state.ScriptPath)
		if wErr != nil {
			return "", fmt.Errorf("schedule external SSH watchdog: %w", wErr)
		}
		state.ExternalUnit = unitName
	}
	if err := g.writeStateAtomic(state); err != nil {
		return "", fmt.Errorf("persist SSH guard state: %w", err)
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
// Fails if recovery already fired (even in a prior crashed process).
func (g *PersistentHostSSHGuard) Disarm(guardID string) error {
	g.mu.Lock()
	if t, ok := g.timers[guardID]; ok {
		t.Stop()
		delete(g.timers, guardID)
	}
	st, err := g.readState(guardID)
	if err != nil {
		g.mu.Unlock()
		return nil
	}
	if st.Status == SSHGuardStatusRecovered || st.Status == SSHGuardStatusRecoveryFailed {
		g.mu.Unlock()
		return fmt.Errorf("SSH recovery guard %s already triggered rollback (status=%s)", guardID, st.Status)
	}
	if st.Status == SSHGuardStatusDisarmed {
		g.mu.Unlock()
		return nil
	}
	now := time.Now().UTC()
	st.Status = SSHGuardStatusDisarmed
	st.DisarmedAt = &now
	wErr := g.writeStateAtomic(*st)
	extUnit := st.ExternalUnit
	extBackend := g.ExternalWatchdog
	g.mu.Unlock()

	if extUnit != "" && extBackend != nil {
		cCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = extBackend.Cancel(cCtx, extUnit)
	}
	return wErr
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
	g.mu.Lock()
	st, err := g.readState(guardID)
	if err != nil {
		g.mu.Unlock()
		return err
	}
	if st.Status != SSHGuardStatusArmed {
		g.mu.Unlock()
		return nil
	}
	stateCopy := *st
	g.mu.Unlock()

	var recErr error
	switch {
	case liveRecoverFn != nil:
		recErr = liveRecoverFn(ctx)
	case g.FallbackRecoverFn != nil:
		recErr = g.FallbackRecoverFn(ctx, stateCopy)
	default:
		recErr = g.recoverFromPersistedState(ctx, stateCopy)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	latest, lErr := g.readState(guardID)
	if lErr != nil {
		return lErr
	}
	if latest.Status == SSHGuardStatusDisarmed {
		return nil
	}
	now := time.Now().UTC()
	latest.RecoveredAt = &now
	if recErr != nil {
		latest.Status = SSHGuardStatusRecoveryFailed
		latest.LastError = recErr.Error()
	} else {
		latest.Status = SSHGuardStatusRecovered
	}
	if wErr := g.writeStateAtomic(*latest); wErr != nil {
		return wErr
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
