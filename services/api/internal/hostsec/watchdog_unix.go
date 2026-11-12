//go:build !windows

package hostsec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DetachedProcessWatchdogBackend spawns an out-of-process, session-detached OS watchdog
// (Setsid=true) that survives API crashes and Docker container stops independently of
// Go timers or ReconcileExpired startup scans.
type DetachedProcessWatchdogBackend struct {
	GuardDir string
}

// NewDefaultHostWatchdogBackend returns the platform host-side detached watchdog backend.
func NewDefaultHostWatchdogBackend(guardDir string) ExternalWatchdogBackend {
	return &DetachedProcessWatchdogBackend{GuardDir: filepath.Clean(strings.TrimSpace(guardDir))}
}

func (b *DetachedProcessWatchdogBackend) resolveGuardDir(scriptPath string) string {
	if strings.TrimSpace(b.GuardDir) != "" && strings.TrimSpace(b.GuardDir) != "." {
		return filepath.Clean(b.GuardDir)
	}
	return filepath.Dir(filepath.Clean(scriptPath))
}

func (b *DetachedProcessWatchdogBackend) Schedule(_ context.Context, guardID string, timeout time.Duration, scriptPath string) (string, error) {
	if !reSafeSnapshotID.MatchString(strings.TrimSpace(guardID)) {
		return "", fmt.Errorf("invalid guard_id %q for detached watchdog", guardID)
	}
	guardDir := b.resolveGuardDir(scriptPath)
	if err := os.MkdirAll(guardDir, 0700); err != nil {
		return "", fmt.Errorf("create watchdog dir %q: %w", guardDir, err)
	}

	ms := timeout.Milliseconds()
	if ms < 20 {
		ms = 20
	}
	sleepArg := fmt.Sprintf("%d.%03d", ms/1000, ms%1000)

	stateFile := filepath.Join(guardDir, guardID+".json")
	disarmedFlag := filepath.Join(guardDir, guardID+".disarmed")
	recTmpl := filepath.Join(guardDir, guardID+".recovered.tmpl")
	failTmpl := filepath.Join(guardDir, guardID+".failed.tmpl")
	lockFile := filepath.Join(guardDir, ".guard.lock")
	watchdogScript := filepath.Join(guardDir, guardID+"-watchdog.sh")

	recLog := filepath.Join(guardDir, guardID+"-recovery.log")

	// Intentionally detached from request ctx so caller cancellation/exit never aborts countdown.
	scriptBody := fmt.Sprintf(`#!/bin/sh
set -eu
sleep %s 2>/dev/null || sleep 1
run_guarded() {
  if [ -f '%s' ]; then
    exit 0
  fi
  if ! grep -q '"status":[[:space:]]*"ARMED"' '%s' 2>/dev/null; then
    exit 0
  fi
  NOW_UTC="$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ 2>/dev/null || echo '1970-01-01T00:00:00Z')"
  if /bin/sh '%s' >>'%s' 2>&1; then
    if [ -f '%s' ]; then
      exit 0
    fi
    if [ -f '%s' ]; then
      sed "s/__RECOVERED_AT__/$NOW_UTC/" '%s' > '%s.wd.tmp'
      mv -f '%s.wd.tmp' '%s'
    fi
  else
    if [ -f '%s' ]; then
      exit 0
    fi
    if [ -f '%s' ]; then
      sed "s/__RECOVERED_AT__/$NOW_UTC/" '%s' > '%s.wd.tmp'
      mv -f '%s.wd.tmp' '%s'
    fi
  fi
}
if command -v flock >/dev/null 2>&1; then
  (
    flock -x 9
    run_guarded
  ) 9>>'%s'
else
  run_guarded
fi
`,
		sleepArg,
		shellEscapeSingle(disarmedFlag),
		shellEscapeSingle(stateFile),
		shellEscapeSingle(scriptPath),
		shellEscapeSingle(recLog),
		shellEscapeSingle(disarmedFlag),
		shellEscapeSingle(recTmpl),
		shellEscapeSingle(recTmpl),
		shellEscapeSingle(stateFile),
		shellEscapeSingle(stateFile),
		shellEscapeSingle(stateFile),
		shellEscapeSingle(disarmedFlag),
		shellEscapeSingle(failTmpl),
		shellEscapeSingle(failTmpl),
		shellEscapeSingle(stateFile),
		shellEscapeSingle(stateFile),
		shellEscapeSingle(stateFile),
		shellEscapeSingle(lockFile),
	)

	if err := writeFileAtomicFailClosed(watchdogScript, []byte(scriptBody), 0700); err != nil {
		return "", fmt.Errorf("write detached watchdog script: %w", err)
	}

	devNull, openErr := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if openErr != nil {
		return "", openErr
	}
	cmd := exec.Command("/bin/sh", watchdogScript)
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	startErr := cmd.Start()
	_ = devNull.Close()
	if startErr != nil {
		return "", fmt.Errorf("start detached watchdog process: %w", startErr)
	}
	pid := cmd.Process.Pid
	go func() {
		_ = cmd.Wait()
	}()
	return fmt.Sprintf("pid:%d:%s", pid, guardID), nil
}

func (b *DetachedProcessWatchdogBackend) Cancel(_ context.Context, unitName string) error {
	u := strings.TrimSpace(unitName)
	if u == "" || !strings.HasPrefix(u, "pid:") {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(u, "pid:"), ":")
	pid, err := strconv.Atoi(parts[0])
	if err != nil || pid <= 0 {
		return fmt.Errorf("invalid detached watchdog unit %q", unitName)
	}
	guardID := ""
	if len(parts) > 1 {
		guardID = strings.TrimSpace(parts[1])
	}

	// PID reuse defense: before sending SIGKILL, verify that the process is actually
	// the intended watchdog script and not an unrelated recycled OS process.
	if isProcessRecycledOrDead(pid, guardID) {
		return nil
	}

	// 1. Linux pidfd_open / pidfd_send_signal (syscall 434 / 424) eliminates PID reuse TOCTOU race
	if killedViaPidfd(pid) {
		return nil
	}

	// 2. Controlled process group kill only if pgrp matches pid, pgrp != current pgrp, and pgrp > 100
	if pgrp, pgErr := syscall.Getpgid(pid); pgErr == nil {
		if pgrp == pid && pgrp != syscall.Getpgrp() && pgrp > 100 {
			if kErr := syscall.Kill(-pid, syscall.SIGKILL); kErr == nil || errors.Is(kErr, syscall.ESRCH) {
				return nil
			}
		}
	}

	// 3. Fallback: kill individual verified PID
	if pErr := syscall.Kill(pid, syscall.SIGKILL); pErr != nil && !errors.Is(pErr, syscall.ESRCH) {
		return pErr
	}
	return nil
}

const (
	sysPidfdOpen       = 434
	sysPidfdSendSignal = 424
)

func killedViaPidfd(pid int) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	// pidfd_open(pid, 0)
	fd, _, errno := syscall.Syscall(sysPidfdOpen, uintptr(pid), 0, 0)
	if errno != 0 {
		return false
	}
	defer syscall.Close(int(fd))
	// pidfd_send_signal(fd, SIGKILL, nil, 0)
	_, _, sErrno := syscall.Syscall6(sysPidfdSendSignal, fd, uintptr(syscall.SIGKILL), 0, 0, 0, 0)
	return sErrno == 0 || errors.Is(sErrno, syscall.ESRCH)
}

func isProcessRecycledOrDead(pid int, guardID string) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true // process already exited
	}
	// On Linux, inspect /proc/<pid>/cmdline to ensure the command line corresponds to watchdog
	cmdlinePath := fmt.Sprintf("/proc/%d/cmdline", pid)
	data, err := os.ReadFile(cmdlinePath)
	if err != nil {
		if os.IsNotExist(err) {
			return true // exited between kill(0) and read
		}
		// /proc not available (e.g. non-Linux Unix or unmounted procfs), proceed with safe kill
		return false
	}
	cmdline := string(data)
	if guardID != "" {
		if !strings.Contains(cmdline, guardID) {
			// Process PID has been recycled by another command! Refuse to kill it!
			return true
		}
	} else if !strings.Contains(cmdline, "watchdog") || !strings.Contains(cmdline, ".sh") {
		return true
	}
	return false
}
