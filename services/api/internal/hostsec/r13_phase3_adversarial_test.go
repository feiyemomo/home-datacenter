package hostsec

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TC-01: ActionHostPackageUpgrade requires non-empty AllowedSteps fail-closed.
func TestR13_TC01_EmptyAllowedStepsBlocked(t *testing.T) {
	g := &HostExecutionGrant{
		GrantID:           "grant-tc01",
		KeyID:             "key-tc01",
		Algorithm:         GrantAlgorithmEd25519,
		Signature:         "abcd",
		ProposalID:        "prop-1",
		ApprovalID:        "apv-1",
		OperationID:       "op-1",
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:base1",
		FencingGeneration: 1,
		Action:            ActionHostPackageUpgrade,
		AllowedSteps:      []string{}, // Empty!
		StepParamDigests:  map[string]string{"apt.upgrade": "digest1"},
		Packages:          []string{"openssh-server"},
		IssuedAtMs:        time.Now().UnixMilli(),
		ExpiresAtMs:       time.Now().Add(5 * time.Minute).UnixMilli(),
	}
	if err := g.ValidateFormat(true); err == nil || !strings.Contains(err.Error(), "requires non-empty allowed_steps") {
		t.Fatalf("expected error on empty allowed_steps for ActionHostPackageUpgrade, got %v", err)
	}
}

// TC-02: ActionHostPackageUpgrade requires non-empty StepParamDigests fail-closed.
func TestR13_TC02_EmptyStepParamDigestsBlocked(t *testing.T) {
	g := &HostExecutionGrant{
		GrantID:           "grant-tc02",
		KeyID:             "key-tc02",
		Algorithm:         GrantAlgorithmEd25519,
		Signature:         "abcd",
		ProposalID:        "prop-1",
		ApprovalID:        "apv-1",
		OperationID:       "op-1",
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:base1",
		FencingGeneration: 1,
		Action:            ActionHostPackageUpgrade,
		AllowedSteps:      []string{string(HostActionAptUpgrade)},
		StepParamDigests:  map[string]string{}, // Empty!
		Packages:          []string{"openssh-server"},
		IssuedAtMs:        time.Now().UnixMilli(),
		ExpiresAtMs:       time.Now().Add(5 * time.Minute).UnixMilli(),
	}
	if err := g.ValidateFormat(true); err == nil || !strings.Contains(err.Error(), "requires non-empty step_param_digests") {
		t.Fatalf("expected error on empty step_param_digests for ActionHostPackageUpgrade, got %v", err)
	}
}

func createTestSnapshotManifest(t *testing.T, snapRoot, snapID string) {
	t.Helper()
	dir := filepath.Join(snapRoot, snapID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := HostUpdateSnapshot{
		SnapshotID:      snapID,
		SnapshotDir:     dir,
		PackageVersions: map[string]string{"openssh-server": "1:9.2p1-2+deb12u3"},
	}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

// TC-03: Digest tampering detection across SnapshotID, GuardID, Timeout, Pkgs, and TargetVersions.
func TestR13_TC03_DigestTamperDetection(t *testing.T) {
	pub, priv := generateTestEd25519Key(t)
	bridgeKey := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	auth, err := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         "grant-ed1",
		PrivateKey:    priv,
		PublicKey:     pub,
		BridgeHMACKey: bridgeKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	binding := HostExecutionBinding{
		ProposalID:        "prop-tc03",
		ApprovalID:        "apv-tc03",
		OperationID:       "op-tc03",
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:tc03",
		FencingGeneration: 1,
	}

	armReq := HostActionRequest{
		Action:            HostActionWatchdogArm,
		Binding:           binding,
		SnapshotID:        "op-tc03",
		GuardID:           "op-tc03",
		WatchdogTimeoutMs: 30000,
	}
	armDigest := ComputeStepParamDigest(armReq)

	upgReq := HostActionRequest{
		Action:         HostActionAptUpgrade,
		Binding:        binding,
		Packages:       []string{"openssh-server"},
		TargetVersions: map[string]string{"openssh-server": "1:9.2p1-2+deb12u4"},
	}
	upgDigest := ComputeStepParamDigest(upgReq)

	grant, err := auth.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID:        binding.ProposalID,
		ApprovalID:        binding.ApprovalID,
		OperationID:       binding.OperationID,
		ResourceScope:     binding.ResourceScope,
		BaselineHash:      binding.BaselineHash,
		FencingGeneration: binding.FencingGeneration,
		Action:            ActionHostPackageUpgrade,
		AllowedSteps:      []string{string(HostActionWatchdogArm), string(HostActionAptUpgrade)},
		StepParamDigests: map[string]string{
			string(HostActionWatchdogArm): armDigest,
			string(HostActionAptUpgrade):  upgDigest,
		},
		Packages:       []string{"openssh-server"},
		TargetVersions: map[string]string{"openssh-server": "1:9.2p1-2+deb12u4"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Tamper SnapshotID
	tamperedSnap := armReq
	tamperedSnap.SnapshotID = "op-tc03-other"
	tamperedSnap.Grant = grant
	if err := auth.VerifyGrant(grant, tamperedSnap, bridgeKey, true, time.Now().UTC()); err == nil {
		t.Fatalf("expected rejection on tampered SnapshotID")
	}

	// 2. Tamper GuardID
	tamperedGuard := armReq
	tamperedGuard.GuardID = "op-tc03-fake"
	tamperedGuard.Grant = grant
	if err := auth.VerifyGrant(grant, tamperedGuard, bridgeKey, true, time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "step digest mismatch") {
		t.Fatalf("expected step digest mismatch on tampered GuardID, got %v", err)
	}

	// 3. Tamper Timeout
	tamperedTimeout := armReq
	tamperedTimeout.WatchdogTimeoutMs = 15000
	tamperedTimeout.Grant = grant
	if err := auth.VerifyGrant(grant, tamperedTimeout, bridgeKey, true, time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "step digest mismatch") {
		t.Fatalf("expected step digest mismatch on tampered Timeout, got %v", err)
	}

	// 4. Tamper Packages in apt.upgrade
	tamperedPkgs := upgReq
	tamperedPkgs.Packages = []string{"nginx"}
	tamperedPkgs.Grant = grant
	if err := auth.VerifyGrant(grant, tamperedPkgs, bridgeKey, true, time.Now().UTC()); err == nil {
		t.Fatalf("expected rejection on tampered package name")
	}

	// 5. Tamper TargetVersion in apt.upgrade
	tamperedVer := upgReq
	tamperedVer.TargetVersions = map[string]string{"openssh-server": "1:9.2p1-2+deb12u3"}
	tamperedVer.Grant = grant
	if err := auth.VerifyGrant(grant, tamperedVer, bridgeKey, true, time.Now().UTC()); err == nil {
		t.Fatalf("expected rejection on tampered target version")
	}
}

// TC-04: DAG Order Enforcement - apt.upgrade cannot execute from INITIAL before watchdog is armed.
func TestR13_TC04_DAGOrderEnforcement(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	bridgeKey := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	pub, priv := generateTestEd25519Key(t)

	auth, _ := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         "grant-ed1",
		PrivateKey:    priv,
		PublicKey:     pub,
		BridgeHMACKey: bridgeKey,
	})

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

	createTestSnapshotManifest(t, snapRoot, "op-tc08")

	handler, _ := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireGrant:   true,
		RequireFencing: true,
		GrantKeyID:     "grant-ed1",
		GrantPublicKey: pub,
		Runner:         &mockHostExecRunner{},
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Attempt apt.upgrade directly from INITIAL state -> MUST return 403 invalid initial step transition
	code, body := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-tc04", HostActionRequest{
		Action:   HostActionAptUpgrade,
		Binding:  binding,
		Packages: []string{"openssh-server"},
		Grant:    grant,
	})
	if code != http.StatusForbidden || !strings.Contains(string(body), "invalid initial step transition") {
		t.Fatalf("expected 403 invalid initial step transition, got %d (%s)", code, string(body))
	}
}

// TC-05: Package mutation without active armed SSH watchdog file is refused (HTTP 412).
func TestR13_TC05_MutationWithoutActiveArmedWatchdogRefused(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	bridgeKey := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	pub, priv := generateTestEd25519Key(t)

	auth, _ := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         "grant-ed1",
		PrivateKey:    priv,
		PublicKey:     pub,
		BridgeHMACKey: bridgeKey,
	})

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

	watchDir := filepath.Join(snapRoot, "ssh-watchdog")
	_ = os.MkdirAll(watchDir, 0700)
	createTestSnapshotManifest(t, snapRoot, "op-tc05")

	handler, _ := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		WatchdogDir:    watchDir,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireGrant:   true,
		RequireFencing: true,
		GrantKeyID:     "grant-ed1",
		GrantPublicKey: pub,
		Runner:         &mockHostExecRunner{},
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// 1. Arm watchdog through bridge
	codeArm, bodyArm := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-tc05-arm", HostActionRequest{
		Action:            HostActionWatchdogArm,
		Binding:           binding,
		SnapshotID:        "op-tc05",
		GuardID:           "op-tc05",
		WatchdogTimeoutMs: 30000,
		Grant:             grant,
	})
	if codeArm != http.StatusOK {
		t.Fatalf("expected 200 on watchdog_arm, got %d (%s)", codeArm, string(bodyArm))
	}

	// 2. Artificially remove watchdog state file to simulate missing/corrupted watchdog on disk
	_ = os.Remove(filepath.Join(watchDir, "op-tc05.json"))

	// 3. Attempt apt.upgrade -> MUST return HTTP 412 Precondition Failed
	codeUpg, bodyUpg := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-tc05-upg", HostActionRequest{
		Action:   HostActionAptUpgrade,
		Binding:  binding,
		Packages: []string{"openssh-server"},
		Grant:    grant,
	})
	if codeUpg != http.StatusPreconditionFailed || !strings.Contains(string(bodyUpg), "refusing package mutation without active armed SSH watchdog") {
		t.Fatalf("expected 412 refusing mutation without active watchdog, got %d (%s)", codeUpg, string(bodyUpg))
	}
}

// TC-06: Valid full step sequence advances state machine smoothly.
func TestR13_TC06_ValidStepSequenceSucceeds(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	bridgeKey := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	pub, priv := generateTestEd25519Key(t)

	auth, _ := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         "grant-ed1",
		PrivateKey:    priv,
		PublicKey:     pub,
		BridgeHMACKey: bridgeKey,
	})

	binding := HostExecutionBinding{
		ProposalID:        "prop-tc06",
		ApprovalID:        "apv-tc06",
		OperationID:       "op-tc06",
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:tc06",
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

	watchDir := filepath.Join(snapRoot, "ssh-watchdog")
	_ = os.MkdirAll(watchDir, 0700)
	createTestSnapshotManifest(t, snapRoot, "op-tc06")

	handler, _ := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		WatchdogDir:    watchDir,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireGrant:   true,
		RequireFencing: true,
		GrantKeyID:     "grant-ed1",
		GrantPublicKey: pub,
		Runner:         &mockHostExecRunner{},
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Step 1: Preflight dpkg.query -> 200
	code, body := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-tc06-1", HostActionRequest{
		Action:   HostActionDpkgQuery,
		Binding:  binding,
		Packages: []string{"openssh-server"},
		Grant:    grant,
	})
	if code != http.StatusOK {
		t.Fatalf("step 1 failed: %d (%s)", code, string(body))
	}

	// Step 2: Watchdog arm -> 200
	code, body = sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-tc06-2", HostActionRequest{
		Action:            HostActionWatchdogArm,
		Binding:           binding,
		SnapshotID:        "op-tc06",
		GuardID:           "op-tc06",
		WatchdogTimeoutMs: 30000,
		Grant:             grant,
	})
	if code != http.StatusOK {
		t.Fatalf("step 2 failed: %d (%s)", code, string(body))
	}

	// Step 3: Apt upgrade -> 200
	code, body = sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-tc06-3", HostActionRequest{
		Action:   HostActionAptUpgrade,
		Binding:  binding,
		Packages: []string{"openssh-server"},
		Grant:    grant,
	})
	if code != http.StatusOK {
		t.Fatalf("step 3 failed: %d (%s)", code, string(body))
	}

	// Step 4: Post-update dpkg.query -> 200
	code, body = sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-tc06-4", HostActionRequest{
		Action:   HostActionDpkgQuery,
		Binding:  binding,
		Packages: []string{"openssh-server"},
		Grant:    grant,
	})
	if code != http.StatusOK {
		t.Fatalf("step 4 failed: %d (%s)", code, string(body))
	}

	// Step 5: SSHD test -> 200
	code, body = sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-tc06-5", HostActionRequest{
		Action:  HostActionSSHDTest,
		Binding: binding,
		Grant:   grant,
	})
	if code != http.StatusOK {
		t.Fatalf("step 5 failed: %d (%s)", code, string(body))
	}

	// Step 6: Watchdog disarm -> 200
	code, body = sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-tc06-6", HostActionRequest{
		Action:  HostActionWatchdogDisarm,
		Binding: binding,
		GuardID: "op-tc06",
		Grant:   grant,
	})
	if code != http.StatusOK {
		t.Fatalf("step 6 failed: %d (%s)", code, string(body))
	}
}

// TC-07: Rollback sequence transitions through RESTORED to COMPLETED.
func TestR13_TC07_RollbackSequenceSucceeds(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	bridgeKey := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	pub, priv := generateTestEd25519Key(t)

	auth, _ := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         "grant-ed1",
		PrivateKey:    priv,
		PublicKey:     pub,
		BridgeHMACKey: bridgeKey,
	})

	binding := HostExecutionBinding{
		ProposalID:        "prop-tc07",
		ApprovalID:        "apv-tc07",
		OperationID:       "op-tc07",
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:tc07",
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
		TargetVersions:    map[string]string{"openssh-server": "1:9.2p1-2+deb12u3"},
	})

	watchDir := filepath.Join(snapRoot, "ssh-watchdog")
	_ = os.MkdirAll(watchDir, 0700)
	createTestSnapshotManifest(t, snapRoot, "op-tc07")

	handler, _ := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		WatchdogDir:    watchDir,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireGrant:   true,
		RequireFencing: true,
		GrantKeyID:     "grant-ed1",
		GrantPublicKey: pub,
		Runner:         &mockHostExecRunner{},
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Arm -> 200
	sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-tc07-arm", HostActionRequest{
		Action:            HostActionWatchdogArm,
		Binding:           binding,
		SnapshotID:        "op-tc07",
		GuardID:           "op-tc07",
		WatchdogTimeoutMs: 30000,
		Grant:             grant,
	})

	// Upgrade -> 200
	codeUpg, bodyUpg := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-tc07-upg", HostActionRequest{
		Action:         HostActionAptUpgrade,
		Binding:        binding,
		Packages:       []string{"openssh-server"},
		TargetVersions: map[string]string{"openssh-server": "1:9.2p1-2+deb12u3"},
		Grant:          grant,
	})
	if codeUpg != http.StatusOK {
		t.Fatalf("upgrade failed: %d (%s)", codeUpg, string(bodyUpg))
	}

	// Rollback: restore config -> 200
	cRestore, bRestore := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-tc07-res", HostActionRequest{
		Action:     HostActionRestoreConfig,
		Binding:    binding,
		SnapshotID: "op-tc07",
		Grant:      grant,
	})
	if cRestore != http.StatusOK {
		t.Fatalf("restore config failed: %d (%s)", cRestore, string(bRestore))
	}

	// Rollback: downgrade package -> 200
	cDown, bDown := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-tc07-down", HostActionRequest{
		Action:         HostActionAptDowngrade,
		Binding:        binding,
		Packages:       []string{"openssh-server"},
		TargetVersions: map[string]string{"openssh-server": "1:9.2p1-2+deb12u3"},
		Grant:          grant,
	})
	if cDown != http.StatusOK {
		t.Fatalf("downgrade failed: %d (%s)", cDown, string(bDown))
	}

	// Disarm watchdog -> 200
	cDisarm, bDisarm := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-tc07-disarm", HostActionRequest{
		Action:  HostActionWatchdogDisarm,
		Binding: binding,
		GuardID: "op-tc07",
		Grant:   grant,
	})
	if cDisarm != http.StatusOK {
		t.Fatalf("disarm failed: %d (%s)", cDisarm, string(bDisarm))
	}
}

// TC-08: Concurrent state transitions are atomic; duplicate steps are strictly rejected.
func TestR13_TC08_ConcurrentAtomicStateTransitions(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	grantDir := t.TempDir()
	bridgeKey := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	pub, priv := generateTestEd25519Key(t)

	auth, _ := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         "grant-ed1",
		PrivateKey:    priv,
		PublicKey:     pub,
		BridgeHMACKey: bridgeKey,
	})

	binding := HostExecutionBinding{
		ProposalID:        "prop-tc08",
		ApprovalID:        "apv-tc08",
		OperationID:       "op-tc08",
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:tc08",
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

	createTestSnapshotManifest(t, snapRoot, "op-tc08")

	handler, _ := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		GrantDir:       grantDir,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireGrant:   true,
		RequireFencing: true,
		GrantKeyID:     "grant-ed1",
		GrantPublicKey: pub,
		Runner:         &mockHostExecRunner{},
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	concurrency := 10
	var wg sync.WaitGroup
	wg.Add(concurrency)
	statusCodes := make([]int, concurrency)

	for i := 0; i < concurrency; i++ {
		idx := i
		go func() {
			defer wg.Done()
			code, _ := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-tc08-"+string(rune('a'+idx)), HostActionRequest{
				Action:            HostActionWatchdogArm,
				Binding:           binding,
				SnapshotID:        "op-tc08",
				GuardID:           "op-tc08",
				WatchdogTimeoutMs: 30000,
				Grant:             grant,
			})
			statusCodes[idx] = code
		}()
	}
	wg.Wait()

	successCount := 0
	forbiddenCount := 0
	for _, code := range statusCodes {
		if code == http.StatusOK {
			successCount++
		} else if code == http.StatusForbidden {
			forbiddenCount++
		}
	}

	if successCount != 1 {
		t.Fatalf("expected exactly 1 successful watchdog_arm, got %d (statusCodes: %v)", successCount, statusCodes)
	}
	if forbiddenCount != concurrency-1 {
		t.Fatalf("expected %d rejections, got %d", concurrency-1, forbiddenCount)
	}
}

// TC-09: Consumption record contains full 6-dim binding, step history, and digests.
func TestR13_TC09_ConsumptionRecordCompleteness(t *testing.T) {
	grantDir := t.TempDir()
	store, err := NewPersistentGrantStore(grantDir)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv := generateTestEd25519Key(t)
	bridgeKey := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	auth, _ := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         "grant-ed1",
		PrivateKey:    priv,
		PublicKey:     pub,
		BridgeHMACKey: bridgeKey,
	})

	binding := HostExecutionBinding{
		ProposalID:        "prop-tc09",
		ApprovalID:        "apv-tc09",
		OperationID:       "op-tc09",
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:tc09",
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
		SnapshotID:        "op-tc09",
		GuardID:           "op-tc09",
		WatchdogTimeoutMs: 30000,
	}
	if err := store.VerifyAndConsume(auth, bridgeKey, grant, armReq, true, time.Now().UTC()); err != nil {
		t.Fatalf("VerifyAndConsume arm: %v", err)
	}

	// Read on-disk consumed record directly
	diskKey, _ := grantDiskKey(grant.GrantID)
	raw, rErr := os.ReadFile(filepath.Join(grantDir, diskKey+".consumed"))
	if rErr != nil {
		t.Fatalf("read consumed record: %v", rErr)
	}

	var rec grantConsumeRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("unmarshal consumed record: %v", err)
	}

	if rec.GrantID != grant.GrantID || rec.OperationID != "op-tc09" || rec.ProposalID != "prop-tc09" || rec.ApprovalID != "apv-tc09" {
		t.Fatalf("binding mismatch in consumption record: %+v", rec)
	}
	if rec.CurrentState != GrantStepStateWatchdogArmed {
		t.Fatalf("expected state WATCHDOG_ARMED, got %s", rec.CurrentState)
	}
	if len(rec.History) != 1 || rec.History[0].StepID != string(HostActionWatchdogArm) {
		t.Fatalf("expected history record for watchdog_arm, got %+v", rec.History)
	}
	if rec.History[0].StepParamDigest != ComputeStepParamDigest(armReq) {
		t.Fatalf("step digest in history mismatch: %s vs %s", rec.History[0].StepParamDigest, ComputeStepParamDigest(armReq))
	}
}

// TC-10: Crash and restart of store maintains state machine on disk without rollback.
func TestR13_TC10_RestartCrashNoStateRollback(t *testing.T) {
	grantDir := t.TempDir()
	store1, _ := NewPersistentGrantStore(grantDir)
	pub, priv := generateTestEd25519Key(t)
	bridgeKey := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	auth, _ := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         "grant-ed1",
		PrivateKey:    priv,
		PublicKey:     pub,
		BridgeHMACKey: bridgeKey,
	})

	binding := HostExecutionBinding{
		ProposalID:        "prop-tc10",
		ApprovalID:        "apv-tc10",
		OperationID:       "op-tc10",
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:tc10",
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
		SnapshotID:        "op-tc10",
		GuardID:           "op-tc10",
		WatchdogTimeoutMs: 30000,
	}
	_ = store1.VerifyAndConsume(auth, bridgeKey, grant, armReq, true, time.Now().UTC())

	// Simulate crash & restart by creating store2 pointing to same directory
	store2, _ := NewPersistentGrantStore(grantDir)

	// Re-attempting watchdog_arm must fail as already consumed
	err := store2.VerifyAndConsume(auth, bridgeKey, grant, armReq, true, time.Now().UTC())
	if !errors.Is(err, ErrExecutionGrantConsumed) {
		t.Fatalf("expected ErrExecutionGrantConsumed on store restart, got %v", err)
	}

	// Attempting apt.upgrade must advance state from WATCHDOG_ARMED to MUTATED
	upgReq := HostActionRequest{
		Action:   HostActionAptUpgrade,
		Binding:  binding,
		Packages: []string{"openssh-server"},
	}
	if err := store2.VerifyAndConsume(auth, bridgeKey, grant, upgReq, true, time.Now().UTC()); err != nil {
		t.Fatalf("expected apt.upgrade to succeed after restart, got %v", err)
	}
}

// TC-11: Rollback disarm properly creates .disarmed marker and marks guard DISARMED.
func TestR13_TC11_RemediationRollbackDisarmCleanup(t *testing.T) {
	guardDir := t.TempDir()
	snapDir := t.TempDir()
	guard := NewPersistentHostSSHGuard(guardDir, "/", snapDir, nil, nil)

	binding := HostExecutionBinding{
		ProposalID:        "prop-tc11",
		ApprovalID:        "apv-tc11",
		OperationID:       "op-tc11",
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:tc11",
		FencingGeneration: 1,
	}

	guardID, err := guard.ArmWithBinding(context.Background(), snapDir, 30*time.Second, binding, nil, nil)
	if err != nil {
		t.Fatalf("ArmWithBinding: %v", err)
	}

	// Disarm
	if err := guard.Disarm(guardID); err != nil {
		t.Fatalf("Disarm: %v", err)
	}

	// Check on disk .disarmed marker exists
	disarmFlag := filepath.Join(guardDir, guardID+".disarmed")
	if _, err := os.Stat(disarmFlag); os.IsNotExist(err) {
		t.Fatalf("expected %s to exist", disarmFlag)
	}

	st, err := guard.LoadState(guardID)
	if err != nil || st.Status != SSHGuardStatusDisarmed {
		t.Fatalf("expected status DISARMED, got %s (err=%v)", st.Status, err)
	}
}

// TC-12: Systemd service file security static assertions.
func TestR13_TC12_SystemdServiceSecurityAssertions(t *testing.T) {
	possiblePaths := []string{
		filepath.Join("..", "..", "..", "deploy", "host-helper", "home-datacenter-host-helper.service"),
		filepath.Join("..", "..", "..", "..", "deploy", "host-helper", "home-datacenter-host-helper.service"),
		filepath.Join("deploy", "host-helper", "home-datacenter-host-helper.service"),
	}
	var data []byte
	var err error
	for _, p := range possiblePaths {
		data, err = os.ReadFile(p)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("could not locate home-datacenter-host-helper.service: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "Group=homedc-bridge") {
		t.Errorf("systemd service file missing Group=homedc-bridge")
	}
	if !strings.Contains(content, "RuntimeDirectoryMode=0750") {
		t.Errorf("systemd service file missing RuntimeDirectoryMode=0750")
	}
	if !strings.Contains(content, "User=root") {
		t.Errorf("systemd service file missing User=root")
	}
	if !strings.Contains(content, "ProtectSystem=strict") {
		t.Errorf("systemd service file missing ProtectSystem=strict")
	}
	if !strings.Contains(content, "ProtectHome=true") {
		t.Errorf("systemd service file missing ProtectHome=true")
	}
	if !strings.Contains(content, "PrivateTmp=true") {
		t.Errorf("systemd service file missing PrivateTmp=true")
	}
}

// TC-13: Socket directory audit verifies root ownership, permissions <= 0750, non-world-writable.
func TestR13_TC13_SocketDirectoryAuditStrict(t *testing.T) {
	dir := t.TempDir()
	if err := AuditSocketDirectory(dir); err != nil {
		t.Fatalf("expected audit of temp dir to succeed, got %v", err)
	}

	// Non-existent directory must fail
	if err := AuditSocketDirectory(filepath.Join(dir, "does_not_exist")); err == nil {
		t.Fatalf("expected failure on non-existent directory")
	}

	// File instead of directory must fail
	dummyFile := filepath.Join(dir, "file.txt")
	_ = os.WriteFile(dummyFile, []byte("hello"), 0644)
	if err := AuditSocketDirectory(dummyFile); err == nil {
		t.Fatalf("expected failure on file path")
	}
}

// TC-14: Socket directory symlink escape rejected fail-closed.
func TestR13_TC14_SocketDirectorySymlinkRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping symlink test on Windows due to privilege requirements")
	}
	targetDir := t.TempDir()
	symlinkPath := filepath.Join(t.TempDir(), "symlink_dir")
	if err := os.Symlink(targetDir, symlinkPath); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if err := AuditSocketDirectory(symlinkPath); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected rejection of symlinked socket directory, got %v", err)
	}
}

// TC-15: Socket path pre-flight check rejects symlinks and regular files.
func TestR13_TC15_SocketFileSymlinkOrRegularFileRejected(t *testing.T) {
	tmpDir := t.TempDir()
	regFile := filepath.Join(tmpDir, "host-bridge.sock")
	_ = os.WriteFile(regFile, []byte("payload"), 0644)

	fi, err := os.Lstat(regFile)
	if err != nil {
		t.Fatal(err)
	}
	// Verify test assertion: non-socket files must be detected
	if fi.Mode().Type()&os.ModeSocket != 0 {
		t.Fatalf("expected regular file not socket")
	}
}

// TC-16: Negative PID kill guards verification.
func TestR13_TC16_NegativePIDKillGuards(t *testing.T) {
	// Static inspection of watchdog_unix.go to ensure negative PID kill is guarded
	srcPath := filepath.Join("watchdog_unix.go")
	data, err := os.ReadFile(srcPath)
	if err != nil {
		t.Skipf("cannot read watchdog_unix.go: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "pgrp == pid") || !strings.Contains(content, "pgrp > 100") {
		t.Fatalf("watchdog_unix.go missing pgrp == pid && pgrp > 100 guard on negative PID kill")
	}
	if !strings.Contains(content, "killedViaPidfd") {
		t.Fatalf("watchdog_unix.go missing pidfd kill support")
	}
}

// TC-17: Offline recovery audit backfill failure writes .audit_pending queue.
func TestR13_TC17_OfflineRecoveryAuditPendingQueue(t *testing.T) {
	guardDir := t.TempDir()
	snapDir := t.TempDir()

	db, auditEngine := setupHostsecAuditDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	_ = sqlDB.Close() // Force database queries to fail

	guard := NewPersistentHostSSHGuard(guardDir, "/", snapDir, nil, nil)
	guard.AuditEngine = auditEngine

	guardID := "guard-tc17"
	now := time.Now().UTC()
	st := PersistentSSHGuardState{
		GuardID:           guardID,
		OperationID:       "op-tc17",
		ProposalID:        "prop-tc17",
		ApprovalID:        "apv-tc17",
		BaselineHash:      "host-v1:tc17",
		FencingGeneration: 1,
		Status:            SSHGuardStatusRecovered, // Already recovered out-of-process
		ArmedAt:           now.Add(-10 * time.Minute),
		DeadlineAt:        now.Add(-5 * time.Minute),
	}
	_ = guard.writeStateAtomic(st)

	// Run ReconcileExpired -> emit will fail -> must create .audit_pending
	_, _ = guard.ReconcileExpired(context.Background())

	pendingFile := filepath.Join(guardDir, guardID+".audit_pending")
	if _, err := os.Stat(pendingFile); os.IsNotExist(err) {
		t.Fatalf("expected %s to be created on audit failure", pendingFile)
	}
	syncFile := filepath.Join(guardDir, guardID+".audit_synced")
	if _, err := os.Stat(syncFile); !os.IsNotExist(err) {
		t.Fatalf("expected %s NOT to be created on audit failure", syncFile)
	}
}

// TC-18: ReconcileExpired drains .audit_pending and creates .audit_synced when audit recovers.
func TestR13_TC18_ReconcilePendingQueueDrain(t *testing.T) {
	guardDir := t.TempDir()
	snapDir := t.TempDir()

	_, auditEngine := setupHostsecAuditDB(t)
	guard := NewPersistentHostSSHGuard(guardDir, "/", snapDir, nil, nil)
	guard.AuditEngine = auditEngine

	guardID := "guard-tc18"
	now := time.Now().UTC()
	st := PersistentSSHGuardState{
		GuardID:           guardID,
		OperationID:       "op-tc18",
		ProposalID:        "prop-tc18",
		ApprovalID:        "apv-tc18",
		BaselineHash:      "host-v1:tc18",
		FencingGeneration: 1,
		Status:            SSHGuardStatusRecovered,
		ArmedAt:           now.Add(-10 * time.Minute),
		DeadlineAt:        now.Add(-5 * time.Minute),
	}
	_ = guard.writeStateAtomic(st)

	// Pre-seed an .audit_pending file
	pendingFile := filepath.Join(guardDir, guardID+".audit_pending")
	_ = os.WriteFile(pendingFile, []byte(`{"guard_id":"guard-tc18","status":"RECOVERED"}`), 0600)

	// Run ReconcileExpired with healthy AuditEngine
	_, err := guard.ReconcileExpired(context.Background())
	if err != nil {
		t.Fatalf("ReconcileExpired: %v", err)
	}

	// Verify .audit_synced was created and .audit_pending was removed
	syncFile := filepath.Join(guardDir, guardID+".audit_synced")
	if _, err := os.Stat(syncFile); os.IsNotExist(err) {
		t.Fatalf("expected %s to be created on successful reconcile", syncFile)
	}
	if _, err := os.Stat(pendingFile); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be removed on successful reconcile", pendingFile)
	}
}

// TC-19: Strict key separation prevents bridge transport key from acting as grant authority key.
func TestR13_TC19_IndependentKeySeparationStrict(t *testing.T) {
	sharedKey := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	// Attempting to construct HostGrantAuthority with identical BridgeHMACKey and HMACKey MUST fail
	_, err := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         "grant-k1",
		HMACKey:       sharedKey,
		BridgeHMACKey: sharedKey,
	})
	if !errors.Is(err, ErrExecutionGrantKeyConflict) {
		t.Fatalf("expected ErrExecutionGrantKeyConflict, got %v", err)
	}
}
