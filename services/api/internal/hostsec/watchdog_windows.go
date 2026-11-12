//go:build windows

package hostsec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	winCreateNewProcessGroup = 0x00000200
	winCreateNoWindow        = 0x08000000
)

// DetachedProcessWatchdogBackend spawns an out-of-process detached OS watchdog
// that survives API process termination independently of Go timers or ReconcileExpired.
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

func psEscapeSingle(s string) string {
	return strings.ReplaceAll(s, "'", "''")
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

	stateFile := filepath.Join(guardDir, guardID+".json")
	disarmedFlag := filepath.Join(guardDir, guardID+".disarmed")
	watchdogPS1 := filepath.Join(guardDir, guardID+"-watchdog.ps1")

	psBody := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
Start-Sleep -Milliseconds %d
$stateFile = '%s'
$disarmedFlag = '%s'
if (Test-Path -LiteralPath $disarmedFlag) { exit 0 }
if (-not (Test-Path -LiteralPath $stateFile)) { exit 0 }
$raw = Get-Content -LiteralPath $stateFile -Raw -Encoding UTF8
$st = $raw | ConvertFrom-Json
if ($st.status -ne 'ARMED') { exit 0 }
$failed = $false
if ($null -ne $st.copied_files) {
  foreach ($prop in $st.copied_files.PSObject.Properties) {
    $rel = $prop.Name
    $backup = $prop.Value
    if ($rel -ne 'etc/ssh/sshd_config' -and -not $rel.StartsWith('etc/ssh/sshd_config.d/') -and $rel -ne 'etc/docker/daemon.json') {
      continue
    }
    $dst = Join-Path $st.host_root ($rel -replace '/', '\')
    $dstDir = Split-Path -Parent $dst
    if (-not (Test-Path -LiteralPath $dstDir)) {
      New-Item -ItemType Directory -Path $dstDir -Force | Out-Null
    }
    if (Test-Path -LiteralPath $backup) {
      Copy-Item -LiteralPath $backup -Destination $dst -Force
    } else {
      $failed = $true
    }
  }
}
if (Test-Path -LiteralPath $disarmedFlag) { exit 0 }
$nowUtc = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
$st | Add-Member -MemberType NoteProperty -Name 'recovered_at' -Value $nowUtc -Force
if ($failed) {
  $st | Add-Member -MemberType NoteProperty -Name 'status' -Value 'RECOVERY_FAILED' -Force
} else {
  $st | Add-Member -MemberType NoteProperty -Name 'status' -Value 'RECOVERED' -Force
}
$tmp = $stateFile + '.wd.tmp'
[System.IO.File]::WriteAllText($tmp, ($st | ConvertTo-Json -Depth 6), (New-Object System.Text.UTF8Encoding($false)))
Move-Item -LiteralPath $tmp -Destination $stateFile -Force
`, ms, psEscapeSingle(stateFile), psEscapeSingle(disarmedFlag))

	if err := writeFileAtomicFailClosed(watchdogPS1, []byte(psBody), 0700); err != nil {
		return "", fmt.Errorf("write detached watchdog ps1: %w", err)
	}

	cmd := exec.Command(
		"powershell.exe",
		"-NoProfile",
		"-NonInteractive",
		"-WindowStyle", "Hidden",
		"-ExecutionPolicy", "Bypass",
		"-File", watchdogPS1,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: winCreateNewProcessGroup | winCreateNoWindow,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start detached watchdog powershell: %w", err)
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
		guardID = parts[1]
	}
	if isProcessRecycledOrDead(pid, guardID) {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	if kErr := proc.Kill(); kErr != nil && !errors.Is(kErr, os.ErrProcessDone) {
		return nil
	}
	return nil
}

func isProcessRecycledOrDead(pid int, guardID string) bool {
	if pid <= 0 {
		return true
	}
	const processQueryLimitedInfo = 0x1000
	h, err := syscall.OpenProcess(processQueryLimitedInfo, false, uint32(pid))
	if err != nil {
		return true // process does not exist or exited
	}
	_ = syscall.CloseHandle(h)
	return false
}
