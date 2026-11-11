package hostsec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"home-datacenter-api/internal/model"
)

func TestR11_FI_01_NamespaceMismatchAndBridgeBoundary(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	_, ae := setupHostsecAuditDB(t)
	defer ae.Close()

	mockRunner := &mockHostExecRunner{}
	exec := NewHostUpdateExecutor(debRoot, snapRoot, ae)
	exec.Runner = mockRunner
	exec.SetBridge(nil)

	prop := makeSignedHostUpdateProposal(t, []string{"openssh-server"})
	baseHash, _, err := exec.ComputeBaselineHash(context.Background(), []string{"openssh-server"})
	if err != nil {
		t.Fatalf("ComputeBaselineHash failed: %v", err)
	}
	prop.BaselineHash = baseHash
	prop.ApprovalDigest, err = model.ComputeProposalApprovalDigest(prop)
	if err != nil {
		t.Fatal(err)
	}

	// Cross-namespace root (debRoot != "/") without bridge MUST fail closed before runner or snapshot.
	_, err = exec.Execute(context.Background(), HostUpdateRequest{
		Proposal:             prop,
		FencingGeneration:    1,
		Packages:             []string{"openssh-server"},
		ExpectedBaselineHash: baseHash,
	})
	if !errors.Is(err, ErrContainerHostExecutionBoundary) {
		t.Fatalf("expected ErrContainerHostExecutionBoundary without bridge, got %v", err)
	}
	if len(mockRunner.calls) != 0 {
		t.Fatalf("expected 0 runner calls when bridge boundary blocks execution, got %v", mockRunner.calls)
	}

	// Container-style v1: baseline hash MUST be rejected by HostExecutionBinding.Validate.
	bridge := attachTestHMACBridge(t, debRoot, snapRoot, mockRunner, true)
	if _, bErr := bridge.ExecuteTypedAction(context.Background(), HostActionRequest{
		Action: HostActionAptUpgrade,
		Binding: HostExecutionBinding{
			ProposalID:        prop.ProposalID,
			ApprovalID:        prop.ApprovalDigest,
			OperationID:       "op-fi-01",
			BaselineHash:      "v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			FencingGeneration: 1,
		},
		Packages: []string{"openssh-server"},
	}); !errors.Is(bErr, ErrBridgeBindingInvalid) {
		t.Fatalf("expected ErrBridgeBindingInvalid for container v1: baseline on host bridge, got %v", bErr)
	}
}

func TestR11_FI_02_MissingRunnerAndMissingAuditFailClosed(t *testing.T) {
	_, ae := setupHostsecAuditDB(t)
	defer ae.Close()

	prop := makeSignedHostUpdateProposal(t, []string{"openssh-server"})
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()

	// 1. Nil AuditEngine MUST block host update execution fail-closed even with valid bridge.
	mockRunner := &mockHostExecRunner{}
	noAuditExec := NewHostUpdateExecutor(debRoot, snapRoot, nil)
	noAuditExec.SetBridge(attachTestHMACBridge(t, debRoot, snapRoot, mockRunner, true))
	baseHash, _, err := noAuditExec.ComputeBaselineHash(context.Background(), []string{"openssh-server"})
	if err != nil {
		t.Fatalf("ComputeBaselineHash: %v", err)
	}
	if _, err := noAuditExec.Execute(context.Background(), HostUpdateRequest{
		Proposal:             prop,
		FencingGeneration:    1,
		Packages:             []string{"openssh-server"},
		ExpectedBaselineHash: baseHash,
	}); err == nil {
		t.Fatalf("expected Execute to fail closed when AuditEngine is nil")
	}
	for _, call := range mockRunner.calls {
		if !strings.HasPrefix(call, "apt-cache ") {
			t.Fatalf("expected zero mutating host commands when AuditEngine is nil, got %v", mockRunner.calls)
		}
	}

	// 2. Nil Runner on direct HostUpdateExecutor (without bridge, HostRoot == "/") MUST fail closed without panic.
	directNoRunner := &HostUpdateExecutor{
		SnapshotRoot:   snapRoot,
		AuditEngine:    ae,
		RequireFencing: false,
		Detector:       &PlatformDetector{HostRoot: "/"},
	}
	if _, runErr := directNoRunner.runTypedHostAction(context.Background(), PlatformInfo{
		OSFamily:            OSFamilyDebian,
		DistroID:            "debian",
		PackageManager:      PkgManagerAptDpkg,
		HostUpdateSupported: true,
		HostRoot:            "/",
		CrossNamespaceRoot:  false,
	}, HostActionRequest{
		Action: HostActionSSHDTest,
		Binding: HostExecutionBinding{
			ProposalID:   prop.ProposalID,
			ApprovalID:   prop.ApprovalDigest,
			OperationID:  "op-fi-02",
			BaselineHash: "host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	}); runErr == nil {
		t.Fatalf("expected error when both Bridge and Runner are nil")
	}

	// 3. Bridge HTTP handler with nil Runner MUST fail closed (non-200).
	h, hErr := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot: debRoot,
		KeyID:    "bridge-k1",
		HMACKey:  []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
		Runner:   nil,
	})
	if hErr != nil {
		t.Fatalf("NewHostBridgeHTTPHandler: %v", hErr)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	clientBridge, cErr := NewHMACHostExecutionBridge(HostBridgeConfig{
		Endpoint:       srv.URL,
		KeyID:          "bridge-k1",
		HMACKey:        []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
		RequireFencing: true,
	})
	if cErr != nil {
		t.Fatal(cErr)
	}
	if _, err := clientBridge.ExecuteTypedAction(context.Background(), HostActionRequest{
		Action: HostActionSSHDTest,
		Binding: HostExecutionBinding{
			ProposalID:        "p1",
			ApprovalID:        "a1",
			OperationID:       "o1",
			BaselineHash:      "host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			FencingGeneration: 1,
		},
	}); err == nil {
		t.Fatalf("expected bridge with nil runner to fail closed")
	}
}

type mockWatchdogBackend struct {
	mu       sync.Mutex
	scheduled []string
	canceled []string
}

func (m *mockWatchdogBackend) Schedule(_ context.Context, guardID string, _ time.Duration, _ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	unit := "unit-" + guardID
	m.scheduled = append(m.scheduled, unit)
	return unit, nil
}

func (m *mockWatchdogBackend) Cancel(_ context.Context, unitName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.canceled = append(m.canceled, unitName)
	return nil
}

func TestR11_FI_03_APICrashDuringSSHUpdatePersistentRecovery(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	snapDir := filepath.Join(snapRoot, "snap-ssh-crash-1")
	backupSSH := filepath.Join(snapDir, "etc/ssh/sshd_config")
	if err := os.MkdirAll(filepath.Dir(backupSSH), 0700); err != nil {
		t.Fatal(err)
	}
	origSSH := "Port 22\nPermitRootLogin no\nPasswordAuthentication no\n"
	if err := os.WriteFile(backupSSH, []byte(origSSH), 0600); err != nil {
		t.Fatal(err)
	}
	manifestBytes, _ := json.Marshal(HostUpdateSnapshot{
		SnapshotID:      "snap-ssh-crash-1",
		SnapshotDir:     snapDir,
		PackageVersions: map[string]string{"openssh-server": "1:9.2p1-2+deb12u3"},
		CopiedFiles:     map[string]string{"etc/ssh/sshd_config": backupSSH},
	})
	if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), manifestBytes, 0600); err != nil {
		t.Fatal(err)
	}

	guardDir := t.TempDir()
	mockRunner := &mockHostExecRunner{}
	wd := &mockWatchdogBackend{}

	// Process 1 arms guard and "crashes" immediately (DisableLocalTimer=true, sshd_config corrupted).
	g1 := NewPersistentHostSSHGuard(guardDir, debRoot, snapRoot, mockRunner, nil)
	g1.DisableLocalTimer = true
	g1.ExternalWatchdog = wd
	gID, err := g1.Arm(context.Background(), snapDir, 25*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("Arm failed: %v", err)
	}
	if len(wd.scheduled) != 1 {
		t.Fatalf("expected external watchdog scheduled, got %v", wd.scheduled)
	}
	if err := os.WriteFile(filepath.Join(debRoot, "etc/ssh/sshd_config"), []byte("BROKEN_PORT_0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(45 * time.Millisecond)

	// Process 2 restarts and reconciles expired guard state from disk.
	g2 := NewPersistentHostSSHGuard(guardDir, debRoot, snapRoot, mockRunner, nil)
	g2.ExternalWatchdog = wd
	count, recErr := g2.ReconcileExpired(context.Background())
	if recErr != nil || count != 1 {
		t.Fatalf("expected ReconcileExpired=1 without error, got count=%d err=%v", count, recErr)
	}
	gotSSH, _ := os.ReadFile(filepath.Join(debRoot, "etc/ssh/sshd_config"))
	if string(gotSSH) != origSSH {
		t.Fatalf("expected sshd_config restored to %q, got %q", origSSH, string(gotSSH))
	}
	// Verify state transitioned to RECOVERED and Disarm refuses double-close.
	stateRaw, _ := os.ReadFile(filepath.Join(guardDir, gID+".json"))
	var st PersistentSSHGuardState
	if err := json.Unmarshal(stateRaw, &st); err != nil || st.Status != SSHGuardStatusRecovered {
		t.Fatalf("expected guard status RECOVERED on disk, got %+v err=%v", st, err)
	}
	if err := g2.Disarm(gID); err == nil {
		t.Fatalf("expected Disarm on already-recovered guard to return error")
	}

	// Fault: expired guard whose Fallback/Runner fails MUST record RECOVERY_FAILED and return error.
	gFail := NewPersistentHostSSHGuard(t.TempDir(), debRoot, snapRoot, nil, nil)
	gFail.DisableLocalTimer = true
	gFail.FallbackRecoverFn = func(_ context.Context, _ PersistentSSHGuardState) error {
		return errors.New("simulated sshd restart failure")
	}
	if _, err := gFail.Arm(context.Background(), snapDir, 15*time.Millisecond, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(35 * time.Millisecond)
	if cnt, rErr := gFail.ReconcileExpired(context.Background()); rErr == nil || cnt != 0 {
		t.Fatalf("expected recovery failure error and count=0, got cnt=%d err=%v", cnt, rErr)
	}
}

func TestR11_FI_04_BridgePayloadAndActionSpecTampering(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	mockRunner := &mockHostExecRunner{}
	bridgeKey := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	h, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		Runner:         mockRunner,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// 1. Unknown JSON field rejected (DisallowUnknownFields) -> 400.
	badPayload := []byte(`{"action":"ssh.config_test","binding":{"proposal_id":"p1","approval_id":"a1","operation_id":"o1","baseline_hash":"host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","fencing_generation":1},"evil_shell":"id"}`)
	ts := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	nonce := "nonce-fi-04-unknown-field"
	sig := computeBridgeRequestSignature(bridgeKey, http.MethodPost, bridgeActionHTTPPath, ts, nonce, badPayload)
	req1, _ := http.NewRequest(http.MethodPost, srv.URL+bridgeActionHTTPPath, bytes.NewReader(badPayload))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set(bridgeHeaderKeyID, "bridge-k1")
	req1.Header.Set(bridgeHeaderTimestamp, ts)
	req1.Header.Set(bridgeHeaderNonce, nonce)
	req1.Header.Set(bridgeHeaderSignature, sig)
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown JSON field, got %d", resp1.StatusCode)
	}

	// 2. Replay attack reusing valid nonce MUST fail closed (401).
	goodPayload := []byte(`{"action":"ssh.config_test","binding":{"proposal_id":"p1","approval_id":"a1","operation_id":"o1","baseline_hash":"host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","fencing_generation":1}}`)
	ts2 := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	nonce2 := "nonce-fi-04-replay-001"
	sig2 := computeBridgeRequestSignature(bridgeKey, http.MethodPost, bridgeActionHTTPPath, ts2, nonce2, goodPayload)
	sendSigned := func(tStamp, n, s string) int {
		r, _ := http.NewRequest(http.MethodPost, srv.URL+bridgeActionHTTPPath, bytes.NewReader(goodPayload))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(bridgeHeaderKeyID, "bridge-k1")
		r.Header.Set(bridgeHeaderTimestamp, tStamp)
		r.Header.Set(bridgeHeaderNonce, n)
		r.Header.Set(bridgeHeaderSignature, s)
		res, doErr := http.DefaultClient.Do(r)
		if doErr != nil {
			t.Fatal(doErr)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	if code := sendSigned(ts2, nonce2, sig2); code != http.StatusOK {
		t.Fatalf("expected initial signed request 200, got %d", code)
	}
	if code := sendSigned(ts2, nonce2, sig2); code != http.StatusUnauthorized {
		t.Fatalf("expected replayed nonce rejected with 401, got %d", code)
	}

	// 3. Stale timestamp outside replay window MUST be rejected (401).
	staleTS := strconv.FormatInt(time.Now().UTC().Add(-10*time.Minute).UnixMilli(), 10)
	staleNonce := "nonce-fi-04-stale"
	staleSig := computeBridgeRequestSignature(bridgeKey, http.MethodPost, bridgeActionHTTPPath, staleTS, staleNonce, goodPayload)
	if code := sendSigned(staleTS, staleNonce, staleSig); code != http.StatusUnauthorized {
		t.Fatalf("expected stale timestamp rejected with 401, got %d", code)
	}

	// 4. Shell metacharacter in TargetVersions rejected fail-closed.
	clientBridge, _ := NewHMACHostExecutionBridge(HostBridgeConfig{
		Endpoint:       srv.URL,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
	})
	if _, err := clientBridge.ExecuteTypedAction(context.Background(), HostActionRequest{
		Action: HostActionAptUpgrade,
		Binding: HostExecutionBinding{
			ProposalID:        "p1",
			ApprovalID:        "a1",
			OperationID:       "o1",
			BaselineHash:      "host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			FencingGeneration: 1,
		},
		Packages:       []string{"openssh-server"},
		TargetVersions: map[string]string{"openssh-server": "1:9.2p1;id"},
	}); !errors.Is(err, ErrInvalidPackageVersion) {
		t.Fatalf("expected ErrInvalidPackageVersion for shell-injected version, got %v", err)
	}
}

func TestR11_FI_05_FnOSBypassAllTiersBlocked(t *testing.T) {
	fnosRoot := writeHostFixture(t, true)
	debRoot := writeHostFixture(t, false)
	mockRunner := &mockHostExecRunner{}
	bridgeKey := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	// 1. Bridge server configured against fnOS host root MUST reject apt.upgrade with 409 Conflict.
	fnosHandler, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       fnosRoot,
		SnapshotRoot:   t.TempDir(),
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		Runner:         mockRunner,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(fnosHandler)
	defer srv.Close()
	clientBridge, _ := NewHMACHostExecutionBridge(HostBridgeConfig{
		Endpoint:       srv.URL,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
	})
	if _, err := clientBridge.ExecuteTypedAction(context.Background(), HostActionRequest{
		Action: HostActionAptUpgrade,
		Binding: HostExecutionBinding{
			ProposalID:        "prop-fnos-bridge",
			ApprovalID:        "apv-fnos-bridge",
			OperationID:       "op-fnos-bridge",
			BaselineHash:      "host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			FencingGeneration: 1,
		},
		Packages: []string{"openssh-server"},
	}); err == nil || !strings.Contains(err.Error(), "fnOS") {
		t.Fatalf("expected bridge on fnOS host root to fail closed with fnOS error, got %v", err)
	}
	if len(mockRunner.calls) != 0 {
		t.Fatalf("expected zero runner calls on fnOS bridge reject, got %v", mockRunner.calls)
	}

	// 2. Tier A execReadOnlyRunner refuses disguised mutating apt-get flags.
	for _, args := range [][]string{
		{"-s", "install", "openssh-server"},
		{"upgrade", "--only-upgrade"},
		{"remove", "openssh-server"},
	} {
		if _, _, rErr := (execReadOnlyRunner{}).Run(context.Background(), "apt-get", args...); rErr == nil {
			t.Fatalf("expected Tier A runner to reject mutating apt-get args %v", args)
		}
	}

	// 3. Even if executor has a healthy Debian bridge, fnOS PlatformDetector blocks Execute before calling bridge.
	_, ae := setupHostsecAuditDB(t)
	defer ae.Close()
	debBridge := attachTestHMACBridge(t, debRoot, t.TempDir(), mockRunner, true)
	fnosExec := NewHostUpdateExecutor(fnosRoot, t.TempDir(), ae)
	fnosExec.SetBridge(debBridge)
	prop := makeSignedHostUpdateProposal(t, []string{"openssh-server"})
	fnosBase, _, _ := fnosExec.ComputeBaselineHash(context.Background(), []string{"openssh-server"})
	if _, err := fnosExec.Execute(context.Background(), HostUpdateRequest{
		Proposal:             prop,
		FencingGeneration:    2,
		Packages:             []string{"openssh-server"},
		ExpectedBaselineHash: fnosBase,
	}); !errors.Is(err, ErrFnOSUpdateUnsupported) {
		t.Fatalf("expected ErrFnOSUpdateUnsupported even with Debian bridge, got %v", err)
	}
}

func TestR11_FI_06_ConcurrentHostUpdateAndFencingRejection(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	_, ae := setupHostsecAuditDB(t)
	defer ae.Close()

	mockRunner := &mockHostExecRunner{
		handlers: map[string]func(args []string) (string, string, error){
			"apt-cache": func(_ []string) (string, string, error) {
				return "openssh-server:\n  Installed: 1:9.2p1-2+deb12u3\n  Candidate: 1:9.2p1-2+deb12u4\n", "", nil
			},
			"dpkg-query": func(_ []string) (string, string, error) {
				return "install ok installed\t1:9.2p1-2+deb12u4\n", "", nil
			},
		},
	}
	bridge := attachTestHMACBridge(t, debRoot, snapRoot, mockRunner, true)
	exec := NewHostUpdateExecutor(debRoot, snapRoot, ae)
	exec.Runner = mockRunner
	exec.SSHProbeFn = func(context.Context, string) error { return nil }
	exec.SetBridge(bridge)

	prop := makeSignedHostUpdateProposal(t, []string{"openssh-server"})
	baseHash, _, err := exec.ComputeBaselineHash(context.Background(), []string{"openssh-server"})
	if err != nil {
		t.Fatal(err)
	}
	prop.BaselineHash = baseHash
	prop.ApprovalDigest, _ = model.ComputeProposalApprovalDigest(prop)

	// 1. Unfenced request (FencingGeneration == 0) MUST fail closed when RequireFencing is true.
	if _, err := exec.Execute(context.Background(), HostUpdateRequest{
		Proposal:             prop,
		FencingGeneration:    0,
		Packages:             []string{"openssh-server"},
		ExpectedBaselineHash: baseHash,
	}); err == nil || !errors.Is(err, ErrContainerHostExecutionBoundary) || !strings.Contains(err.Error(), ErrBridgeBindingInvalid.Error()) {
		t.Fatalf("expected ErrContainerHostExecutionBoundary wrapping %q for unfenced host update, got %v", ErrBridgeBindingInvalid.Error(), err)
	}

	// 2. Concurrent executions over shared executor/bridge/audit DB must be race-free.
	const workers = 6
	var wg sync.WaitGroup
	var okCount atomic.Int32
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			wProp := makeSignedHostUpdateProposal(t, []string{"openssh-server"})
			wProp.ProposalID = fmt.Sprintf("prop-hostupd-race-%d", idx)
			wProp.BaselineHash = baseHash
			wProp.ApprovalDigest, _ = model.ComputeProposalApprovalDigest(wProp)
			res, execErr := exec.Execute(context.Background(), HostUpdateRequest{
				Proposal:             wProp,
				FencingGeneration:    int64(idx + 1),
				Packages:             []string{"openssh-server"},
				ExpectedBaselineHash: baseHash,
			})
			if execErr == nil && res != nil && res.Verified {
				okCount.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if okCount.Load() != workers {
		t.Fatalf("expected all %d concurrent fenced mock upgrades to settle cleanly without race, got %d", workers, okCount.Load())
	}
}
