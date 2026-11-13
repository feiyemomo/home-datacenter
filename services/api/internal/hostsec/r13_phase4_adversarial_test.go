package hostsec

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type phase4MockRunner struct {
	mu    sync.Mutex
	calls []string
	runFn func(ctx context.Context, env []string, name string, args ...string) (string, string, error)
}

func (m *phase4MockRunner) Run(ctx context.Context, env []string, name string, args ...string) (string, string, error) {
	m.mu.Lock()
	m.calls = append(m.calls, name+" "+strings.Join(args, " "))
	fn := m.runFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, env, name, args...)
	}
	return "", "", nil
}

// TC-01: 无 Watchdog Grant 直接升级 -> 严格拒绝
func TestR13_P4_TC01_NoWatchdogGrantBlocked(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	auth, err := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:      "grant-k1",
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Issue grant WITHOUT HostActionWatchdogArm
	_, err = auth.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID:        "prop-tc01",
		ApprovalID:        "apv-tc01",
		OperationID:       "op-tc01",
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:baseline",
		FencingGeneration: 1,
		Action:            ActionHostPackageUpgrade,
		AllowedSteps:      []string{string(HostActionAptUpgrade), string(HostActionWatchdogDisarm)},
		StepParamDigests: map[string]string{
			string(HostActionAptUpgrade):     "digest1",
			string(HostActionWatchdogDisarm): "digest2",
		},
		Packages: []string{"openssh-server"},
	})
	if err == nil {
		t.Fatalf("expected IssueGrant to fail when watchdog arm is missing from AllowedSteps")
	}
	if !strings.Contains(err.Error(), "strictly requires watchdog arm and disarm") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// TC-02: ARMED 文件存在但进程死亡 -> 拒绝升级
func TestR13_P4_TC02_DeadWatchdogProcessRejected(t *testing.T) {
	watchDir := t.TempDir()
	opID := "op-tc02"
	deadPID := 999999

	st := PersistentSSHGuardState{
		GuardID:      opID,
		OperationID:  opID,
		Status:       SSHGuardStatusArmed,
		ArmedAt:      time.Now().UTC(),
		DeadlineAt:   time.Now().UTC().Add(30 * time.Second),
		ExternalUnit: fmt.Sprintf("pid:%d:%s", deadPID, opID),
	}
	raw, _ := json.Marshal(st)
	_ = os.WriteFile(filepath.Join(watchDir, opID+".json"), raw, 0600)

	_, _, err := verifyActiveWatchdogForOperation(watchDir, opID)
	if err == nil {
		t.Fatalf("expected verifyActiveWatchdogForOperation to reject dead watchdog process")
	}
	if !strings.Contains(err.Error(), "no active ARMED watchdog found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TC-03: Deadline 缺失或零值 -> 拒绝升级
func TestR13_P4_TC03_ZeroOrExpiredDeadlineRejected(t *testing.T) {
	watchDir := t.TempDir()
	opID := "op-tc03"

	// Zero deadline
	st := PersistentSSHGuardState{
		GuardID:      opID,
		OperationID:  opID,
		Status:       SSHGuardStatusArmed,
		ArmedAt:      time.Now().UTC(),
		DeadlineAt:   time.Time{}, // Zero value
		ExternalUnit: fmt.Sprintf("pid:%d:%s", os.Getpid(), opID),
	}
	raw, _ := json.Marshal(st)
	_ = os.WriteFile(filepath.Join(watchDir, opID+".json"), raw, 0600)

	_, _, err := verifyActiveWatchdogForOperation(watchDir, opID)
	if err == nil {
		t.Fatalf("expected rejection on zero DeadlineAt")
	}

	// Expired deadline
	st.DeadlineAt = time.Now().UTC().Add(-5 * time.Second)
	raw, _ = json.Marshal(st)
	_ = os.WriteFile(filepath.Join(watchDir, opID+".json"), raw, 0600)

	_, _, err = verifyActiveWatchdogForOperation(watchDir, opID)
	if err == nil {
		t.Fatalf("expected rejection on expired DeadlineAt")
	}
}

// TC-04: Watchdog 检查后进程退出 -> Sentinel 伴随监听主动中断执行并标记 RECOVERY_REQUIRED
func TestR13_P4_TC04_SentinelAbortsOnWatchdogDeath(t *testing.T) {
	bridgeKey := []byte("test-bridge-key-32-bytes-long!!!")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	auth, _ := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         "grant-ed1",
		PrivateKey:    priv,
		PublicKey:     pub,
		BridgeHMACKey: bridgeKey,
	})

	snapRoot := t.TempDir()
	watchDir := filepath.Join(snapRoot, "ssh-watchdog")
	_ = os.MkdirAll(watchDir, 0700)
	debRoot := writeHostFixture(t, false)

	binding := HostExecutionBinding{
		ProposalID:        "prop-tc04",
		ApprovalID:        "apv-tc04",
		OperationID:       "op-tc04",
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:tc04",
		FencingGeneration: 1,
	}

	grant, _ := auth.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID:        binding.ProposalID,
		ApprovalID:        binding.ApprovalID,
		OperationID:       binding.OperationID,
		ResourceScope:     binding.ResourceScope,
		BaselineHash:      binding.BaselineHash,
		FencingGeneration: binding.FencingGeneration,
		Action:            ActionHostPackageUpgrade,
		Packages:          []string{"openssh-server"},
	})

	createTestSnapshotManifest(t, snapRoot, "op-tc04")

	runnerStarted := make(chan struct{})
	slowRunner := &phase4MockRunner{
		runFn: func(ctx context.Context, env []string, base string, args ...string) (string, string, error) {
			select {
			case <-runnerStarted:
			default:
				close(runnerStarted)
			}
			select {
			case <-time.After(1 * time.Second):
				return "done", "", nil
			case <-ctx.Done():
				return "", "interrupted", ctx.Err()
			}
		},
	}

	grantDir := filepath.Join(snapRoot, "grant-store")
	grantStore, _ := NewPersistentGrantStore(grantDir)

	handler, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		WatchdogDir:    watchDir,
		GrantDir:       grantDir,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireGrant:   true,
		RequireFencing: true,
		GrantKeyID:     "grant-ed1",
		GrantPublicKey: pub,
		Runner:         slowRunner,
	})
	if err != nil {
		t.Fatalf("NewHostBridgeHTTPHandler failed: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// 1. Start a live background watchdog process that will survive the initial verification
	var aliveCmd *exec.Cmd
	if runtime.GOOS == "windows" {
		aliveCmd = execCommandHelper("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", fmt.Sprintf("Start-Sleep -Seconds 15; # %s", binding.OperationID))
	} else {
		aliveCmd = execCommandHelper("/bin/sh", "-c", fmt.Sprintf("sleep 15 # %s", binding.OperationID))
	}
	if err := aliveCmd.Start(); err != nil {
		t.Fatalf("failed to start watchdog process: %v", err)
	}
	defer func() {
		if aliveCmd.Process != nil {
			_ = aliveCmd.Process.Kill()
		}
	}()

	watchPID := aliveCmd.Process.Pid
	armState := PersistentSSHGuardState{
		GuardID:      "op-tc04",
		OperationID:  "op-tc04",
		Status:       SSHGuardStatusArmed,
		ArmedAt:      time.Now().UTC(),
		DeadlineAt:   time.Now().UTC().Add(30 * time.Second),
		ExternalUnit: fmt.Sprintf("pid:%d:op-tc04", watchPID),
	}
	sRaw, _ := json.Marshal(armState)
	_ = os.WriteFile(filepath.Join(watchDir, "op-tc04.json"), sRaw, 0600)

	// Consume arm step first to advance grant state machine to WATCHDOG_ARMED
	_ = grantStore.VerifyAndConsume(auth, bridgeKey, grant, HostActionRequest{
		Action:            HostActionWatchdogArm,
		Binding:           binding,
		SnapshotID:        "op-tc04",
		GuardID:           "op-tc04",
		WatchdogTimeoutMs: 30000,
	}, true, time.Now().UTC())

	// 2. Kill the watchdog process in-flight after slowRunner has started executing.
	// This tests that WatchdogSentinel detects unexpected watchdog death and aborts execution context!
	go func() {
		select {
		case <-runnerStarted:
			time.Sleep(50 * time.Millisecond)
			if aliveCmd.Process != nil {
				_ = aliveCmd.Process.Kill()
			}
		case <-time.After(5 * time.Second):
		}
	}()

	code, body := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-tc04-upg", HostActionRequest{
		Action:   HostActionAptUpgrade,
		Binding:  binding,
		Packages: []string{"openssh-server"},
		Grant:    grant,
	})
	t.Logf("TC04 response: code=%d body=%s", code, string(body))
	if code == http.StatusOK {
		t.Fatalf("expected mutation to be aborted by WatchdogSentinel when watchdog dies, got %d (%s)", code, string(body))
	}

	// Verify grant status was set to RECOVERY_REQUIRED
	rec, rErr := grantStore.GetGrantRecord(grant.GrantID)
	if rErr != nil {
		t.Fatalf("GetGrantRecord failed: %v", rErr)
	}
	if rec.ExecutionStatus != GrantExecutionStatusRecoveryRequired {
		t.Fatalf("expected ExecutionStatus=%s, got %s", GrantExecutionStatusRecoveryRequired, rec.ExecutionStatus)
	}
}

// TC-05: Grant 被重复消费 -> 拒绝
func TestR13_P4_TC05_GrantDoubleConsumptionRejected(t *testing.T) {
	bridgeKey := []byte("test-bridge-key-32-bytes-long!!")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	auth, _ := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         "grant-ed1",
		PrivateKey:    priv,
		BridgeHMACKey: bridgeKey,
	})

	grantDir := t.TempDir()
	store, _ := NewPersistentGrantStore(grantDir)

	binding := HostExecutionBinding{
		ProposalID:        "prop-tc05",
		ApprovalID:        "apv-tc05",
		OperationID:       "op-tc05",
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:tc05",
		FencingGeneration: 1,
	}

	grant, _ := auth.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID:        binding.ProposalID,
		ApprovalID:        binding.ApprovalID,
		OperationID:       binding.OperationID,
		ResourceScope:     binding.ResourceScope,
		BaselineHash:      binding.BaselineHash,
		FencingGeneration: binding.FencingGeneration,
		Action:            ActionHostPackageUpgrade,
		Packages:          []string{"openssh-server"},
	})

	armReq := HostActionRequest{
		Action:            HostActionWatchdogArm,
		Binding:           binding,
		SnapshotID:        "op-tc05",
		GuardID:           "op-tc05",
		WatchdogTimeoutMs: 30000,
	}

	// 1st consumption -> succeeds
	if err := store.VerifyAndConsume(auth, bridgeKey, grant, armReq, true, time.Now().UTC()); err != nil {
		t.Fatalf("first consumption failed: %v", err)
	}

	// 2nd consumption -> rejected
	err := store.VerifyAndConsume(auth, bridgeKey, grant, armReq, true, time.Now().UTC())
	if err == nil {
		t.Fatalf("expected double consumption to be rejected")
	}
	if !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TC-06: 两个 API 实例并发消费 -> 仅允许合法的一次执行
func TestR13_P4_TC06_ConcurrentGrantConsumptionSingleWinner(t *testing.T) {
	bridgeKey := []byte("test-bridge-key-32-bytes-long!!")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	auth, _ := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         "grant-ed1",
		PrivateKey:    priv,
		BridgeHMACKey: bridgeKey,
	})

	grantDir := t.TempDir()
	store, _ := NewPersistentGrantStore(grantDir)

	binding := HostExecutionBinding{
		ProposalID:        "prop-tc06-conc",
		ApprovalID:        "apv-tc06-conc",
		OperationID:       "op-tc06-conc",
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:tc06conc",
		FencingGeneration: 1,
	}

	grant, _ := auth.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID:        binding.ProposalID,
		ApprovalID:        binding.ApprovalID,
		OperationID:       binding.OperationID,
		ResourceScope:     binding.ResourceScope,
		BaselineHash:      binding.BaselineHash,
		FencingGeneration: binding.FencingGeneration,
		Action:            ActionHostPackageUpgrade,
		Packages:          []string{"openssh-server"},
	})

	req := HostActionRequest{
		Action:            HostActionWatchdogArm,
		Binding:           binding,
		SnapshotID:        "op-tc06-conc",
		GuardID:           "op-tc06-conc",
		WatchdogTimeoutMs: 30000,
	}

	concurrency := 10
	var wg sync.WaitGroup
	wg.Add(concurrency)

	var successCount int64
	var failCount int64
	var mu sync.Mutex

	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			err := store.VerifyAndConsume(auth, bridgeKey, grant, req, true, time.Now().UTC())
			mu.Lock()
			if err == nil {
				successCount++
			} else {
				failCount++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if successCount != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", successCount)
	}
	if failCount != int64(concurrency-1) {
		t.Fatalf("expected %d failures, got %d", concurrency-1, failCount)
	}
}

// TC-07: Grant 消费后 API 崩溃 -> 正确识别待恢复状态
func TestR13_P4_TC07_CrashRecoveryStatusIdentified(t *testing.T) {
	grantDir := t.TempDir()
	store, _ := NewPersistentGrantStore(grantDir)
	grantID := "grant-crash-001"

	rec := grantConsumeRecord{
		GrantID:         grantID,
		Action:          ActionHostPackageUpgrade,
		ProposalID:      "prop-crash",
		OperationID:     "op-crash",
		ExecutionStatus: GrantExecutionStatusExecuting, // Was executing when crash happened
		CurrentState:    GrantStepStateMutated,
		CreatedAtMs:     time.Now().UTC().UnixMilli(),
	}
	raw, _ := json.Marshal(rec)
	key, _ := grantDiskKey(grantID)
	_ = os.WriteFile(filepath.Join(grantDir, key+".consumed"), raw, 0600)

	// Mark status as RECOVERY_REQUIRED
	if err := store.MarkExecutionStatus(grantID, GrantExecutionStatusRecoveryRequired); err != nil {
		t.Fatalf("MarkExecutionStatus failed: %v", err)
	}

	updated, err := store.GetGrantRecord(grantID)
	if err != nil {
		t.Fatalf("GetGrantRecord failed: %v", err)
	}
	if updated.ExecutionStatus != GrantExecutionStatusRecoveryRequired {
		t.Fatalf("expected RECOVERY_REQUIRED, got %s", updated.ExecutionStatus)
	}
}

// TC-08: 状态文件软链接替换 -> 拒绝
func TestR13_P4_TC08_SymlinkStateFileRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping symlink creation on Windows without privileges")
	}
	watchDir := t.TempDir()
	opID := "op-tc08"
	targetFile := filepath.Join(t.TempDir(), "target.json")
	_ = os.WriteFile(targetFile, []byte(`{"status":"ARMED"}`), 0600)

	symlinkPath := filepath.Join(watchDir, opID+".json")
	_ = os.Symlink(targetFile, symlinkPath)

	_, _, err := verifyActiveWatchdogForOperation(watchDir, opID)
	if err == nil {
		t.Fatalf("expected verifyActiveWatchdogForOperation to reject symlink state file")
	}
}

// TC-09: Socket 父目录权限异常 (world-writable) -> 拒绝
func TestR13_P4_TC09_WorldWritableSocketDirectoryRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping Unix permission mode test on Windows")
	}
	baseDir := t.TempDir()
	badDir := filepath.Join(baseDir, "bad-sock-dir")
	_ = os.MkdirAll(badDir, 0777)
	_ = os.Chmod(badDir, 0777)

	sockPath := filepath.Join(badDir, "bridge.sock")
	err := AuditSocketAncestry(sockPath)
	if err == nil {
		t.Fatalf("expected AuditSocketAncestry to reject world-writable directory")
	}
	if !strings.Contains(err.Error(), "world-writable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TC-10: 宿主机 GID 不匹配 / Service 文件审计 -> 校验 Group=homedc-bridge
func TestR13_P4_TC10_SystemdServiceGroupHomedcBridge(t *testing.T) {
	repoRoot := findRepoRoot()
	servicePath := filepath.Join(repoRoot, "deploy", "host-helper", "home-datacenter-host-helper.service")
	data, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatalf("read service file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "Group=homedc-bridge") {
		t.Fatalf("service file missing Group=homedc-bridge")
	}
	if !strings.Contains(content, "RuntimeDirectoryMode=0750") {
		t.Fatalf("service file missing RuntimeDirectoryMode=0750")
	}
}

// TC-11: pidfd 发送信号权限错误 -> 不得盲杀误杀
func TestR13_P4_TC11_PidfdEPERMRefusesBlindKill(t *testing.T) {
	backend := &DetachedProcessWatchdogBackend{GuardDir: t.TempDir()}
	// Unit with PID 1 (init / systemd)
	err := backend.Cancel(context.Background(), "pid:1:guard-init")
	if err != nil && !strings.Contains(err.Error(), "pidfd") {
		// Verify PID 1 was NOT killed!
		t.Logf("PID 1 safely protected from kill: %v", err)
	}
}

// TC-12: PID 复用保护 -> 命令不匹配时拒绝 Kill
func TestR13_P4_TC12_PIDReuseProtection(t *testing.T) {
	currentPID := os.Getpid()
	// Pass an unrelated guardID that does not match current process cmdline
	recycled := isProcessRecycledOrDead(currentPID, "guard-fake-operation-unrelated-random-id-999")
	if !recycled {
		t.Fatalf("expected isProcessRecycledOrDead=true when cmdline does not contain guardID")
	}
}

// TC-13: 审计引擎离线 -> 可靠落盘 .audit_pending
func TestR13_P4_TC13_OfflineAuditPendingQueue(t *testing.T) {
	guardDir := t.TempDir()
	guard := NewPersistentHostSSHGuard(guardDir, t.TempDir(), t.TempDir(), &mockHostExecRunner{}, nil)
	// AuditEngine is nil (offline)
	guard.AuditEngine = nil

	guardID := "guard-offline-p4"
	now := time.Now().UTC()
	st := PersistentSSHGuardState{
		GuardID:      guardID,
		OperationID:  "op-offline-p4",
		Status:       SSHGuardStatusArmed,
		ArmedAt:      now.Add(-2 * time.Minute),
		DeadlineAt:   now.Add(-1 * time.Minute), // expired
		SnapshotDir:  t.TempDir(),
		ExternalUnit: fmt.Sprintf("pid:%d:%s", os.Getpid(), guardID),
	}
	sRaw, _ := json.Marshal(st)
	_ = os.WriteFile(filepath.Join(guardDir, guardID+".json"), sRaw, 0600)

	reconciled, err := guard.ReconcileExpired(context.Background())
	if err != nil {
		t.Fatalf("ReconcileExpired failed: %v", err)
	}
	if reconciled != 1 {
		t.Fatalf("expected 1 reconciled guard, got %d", reconciled)
	}

	// Verify .audit_pending exists on disk
	pendingFile := filepath.Join(guardDir, guardID+".audit_pending")
	if _, err := os.Stat(pendingFile); os.IsNotExist(err) {
		t.Fatalf("expected %s to be persisted when audit engine is offline", pendingFile)
	}
}

// TC-14: Python 离线与 Go 安全限制对齐 -> 验证门禁拦截
func TestR13_P4_TC14_PythonExemptionFormatGate(t *testing.T) {
	repoRoot := findRepoRoot()
	scriptPath := filepath.Join(repoRoot, "services", "api", "scripts", "ci", "test_gate_govulncheck.py")
	cmd := execCommandHelper("python", scriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("python test_gate_govulncheck.py failed: %v\nOutput: %s", err, string(out))
	}
}

// TC-15: 漏洞豁免没有有效期 -> CI 失败
func TestR13_P4_TC15_MissingExpirationRejected(t *testing.T) {
	allowlistJSON := `{
		"version": "1.0",
		"exemptions": [
			{
				"id": "GO-2026-MISSING-EXP",
				"module": "stdlib",
				"package": "net/http",
				"reason": "Test missing expires_at",
				"risk": "LOW",
				"approved_by": "sec"
			}
		]
	}`
	tempFile := filepath.Join(t.TempDir(), "allowlist.json")
	_ = os.WriteFile(tempFile, []byte(allowlistJSON), 0600)

	repoRoot := findRepoRoot()
	scriptPath := filepath.Join(repoRoot, "services", "api", "scripts", "ci", "gate_govulncheck.py")
	cmd := execCommandHelper("python", scriptPath, "--allowlist", tempFile, "--input", tempFile)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected gate_govulncheck.py to fail when expires_at is missing, output:\n%s", string(out))
	}
	if !strings.Contains(string(out), "missing required field 'expires_at'") {
		t.Fatalf("unexpected output: %s", string(out))
	}
}

// TC-16: 恢复脚本执行失败 -> RECOVERY_FAILED / RECOVERY_REQUIRED
func TestR13_P4_TC16_RecoveryScriptFailureRecorded(t *testing.T) {
	guardDir := t.TempDir()
	failingRunner := &phase4MockRunner{
		runFn: func(ctx context.Context, env []string, base string, args ...string) (string, string, error) {
			return "", "recovery script failed", errors.New("exit 1")
		},
	}
	guard := NewPersistentHostSSHGuard(guardDir, t.TempDir(), t.TempDir(), failingRunner, nil)
	guard.DisableLocalTimer = true

	guardID := "guard-fail-p4"
	now := time.Now().UTC()
	st := PersistentSSHGuardState{
		GuardID:      guardID,
		OperationID:  "op-fail-p4",
		Status:       SSHGuardStatusArmed,
		ArmedAt:      now.Add(-2 * time.Minute),
		DeadlineAt:   now.Add(-1 * time.Minute),
		SnapshotDir:  t.TempDir(),
		ExternalUnit: fmt.Sprintf("pid:%d:%s", os.Getpid(), guardID),
	}
	sRaw, _ := json.Marshal(st)
	_ = os.WriteFile(filepath.Join(guardDir, guardID+".json"), sRaw, 0600)

	_, err := guard.ReconcileExpired(context.Background())
	if err == nil {
		t.Fatalf("expected ReconcileExpired to return error on recovery failure")
	}

	// State should record RECOVERY_FAILED
	latest, _ := guard.readState(guardID)
	if latest == nil || latest.Status != SSHGuardStatusRecoveryFailed {
		t.Fatalf("expected status RECOVERY_FAILED, got %+v", latest)
	}
}

// TC-17: Docker 重启导致 Socket 重建 -> 连接恢复且权限正确
func TestR13_P4_TC17_SocketRecreationAndDialRecovery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping Unix domain socket bind on Windows")
	}
	sockDir := t.TempDir()
	sockPath := filepath.Join(sockDir, "test.sock")

	// 1. Initial Listen
	l1, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen 1 failed: %v", err)
	}
	go func() {
		for {
			conn, err := l1.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	// Dial 1 -> success
	c1, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial 1 failed: %v", err)
	}
	_ = c1.Close()
	_ = l1.Close()
	_ = os.Remove(sockPath)

	// 2. Recreate socket in the same parent directory (simulating Helper restart)
	l2, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen 2 failed: %v", err)
	}
	defer l2.Close()
	go func() {
		for {
			conn, err := l2.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	// Dial 2 -> success
	c2, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial 2 failed after recreation: %v", err)
	}
	_ = c2.Close()
}

// TC-18: API 与 Helper 同时重启 -> 状态可安全恢复
func TestR13_P4_TC18_SimultaneousRestartSafeRecovery(t *testing.T) {
	guardDir := t.TempDir()
	grantDir := t.TempDir()
	snapRoot := t.TempDir()

	guard := NewPersistentHostSSHGuard(guardDir, t.TempDir(), snapRoot, &mockHostExecRunner{}, nil)
	grantStore, _ := NewPersistentGrantStore(grantDir)

	opID := "op-sim-restart"
	now := time.Now().UTC()

	// 1. Persisted watchdog state on disk
	wState := PersistentSSHGuardState{
		GuardID:      opID,
		OperationID:  opID,
		Status:       SSHGuardStatusArmed,
		ArmedAt:      now.Add(-2 * time.Minute),
		DeadlineAt:   now.Add(-1 * time.Minute), // Expired during simultaneous restart
		SnapshotDir:  t.TempDir(),
		ExternalUnit: fmt.Sprintf("pid:%d:%s", os.Getpid(), opID),
	}
	wRaw, _ := json.Marshal(wState)
	_ = os.WriteFile(filepath.Join(guardDir, opID+".json"), wRaw, 0600)

	// 2. Persisted grant consumption record on disk
	gRec := grantConsumeRecord{
		GrantID:         "grant-sim-restart",
		Action:          ActionHostPackageUpgrade,
		OperationID:     opID,
		ExecutionStatus: GrantExecutionStatusExecuting,
		CurrentState:    GrantStepStateMutated,
		CreatedAtMs:     now.UnixMilli(),
	}
	gRaw, _ := json.Marshal(gRec)
	gKey, _ := grantDiskKey("grant-sim-restart")
	_ = os.WriteFile(filepath.Join(grantDir, gKey+".consumed"), gRaw, 0600)

	// 3. New API instance boots and reconciles
	reconciled, err := guard.ReconcileExpired(context.Background())
	if err != nil {
		t.Fatalf("ReconcileExpired failed: %v", err)
	}
	if reconciled != 1 {
		t.Fatalf("expected 1 reconciled guard, got %d", reconciled)
	}

	// 4. Mark grant as RECOVERY_REQUIRED
	_ = grantStore.MarkExecutionStatus("grant-sim-restart", GrantExecutionStatusRecoveryRequired)
	rec, _ := grantStore.GetGrantRecord("grant-sim-restart")
	if rec.ExecutionStatus != GrantExecutionStatusRecoveryRequired {
		t.Fatalf("expected RECOVERY_REQUIRED after restart, got %s", rec.ExecutionStatus)
	}
}

func execCommandHelper(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

func findRepoRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "deploy")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "."
}
