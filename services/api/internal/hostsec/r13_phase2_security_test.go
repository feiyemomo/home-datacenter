package hostsec

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"home-datacenter-api/internal/model"
)

func generateTestEd25519Key(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return pub, priv
}

// TestR13_P2_01_GrantFailClosedOnMissingKey verifies that the bridge fails closed
// when independent grant verification keys are missing under RequireGrant.
func TestR13_P2_01_GrantFailClosedOnMissingKey(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	bridgeKey := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	// Missing grant key must be rejected on initialization when RequireGrant is true
	_, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireGrant:   true,
		RequireFencing: true,
	})
	if err == nil || (!strings.Contains(err.Error(), "grant HMAC or Ed25519 key material required") && !strings.Contains(err.Error(), "independent grant verification key required")) {
		t.Fatalf("expected NewHostBridgeHTTPHandler to fail closed without grant key, got %v", err)
	}

	// When initialized with valid grant public key, missing grant on request must return 403
	pub, priv := generateTestEd25519Key(t)
	handler, hErr := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireGrant:   true,
		RequireFencing: true,
		GrantKeyID:     "grant-ed1",
		GrantPublicKey: pub,
	})
	if hErr != nil {
		t.Fatalf("failed to initialize handler with ed25519 public key: %v", hErr)
	}

	srv := httptest.NewServer(handler)
	defer srv.Close()

	binding := HostExecutionBinding{
		ProposalID:        "prop-p2-1",
		ApprovalID:        "apv-p2-1",
		OperationID:       "op-p2-1",
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:testbase",
		FencingGeneration: 1,
	}

	// Request with valid transport HMAC but missing Grant -> must be 403 Forbidden
	code, body := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-p2-1", HostActionRequest{
		Action:   HostActionAptUpgrade,
		Binding:  binding,
		Packages: []string{"openssh-server"},
		Grant:    nil,
	})
	if code != http.StatusForbidden {
		t.Fatalf("expected 403 when Grant is missing, got %d (%s)", code, string(body))
	}

	// Now issue valid Grant and verify request succeeds past grant authorization gate
	auth, aErr := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         "grant-ed1",
		PrivateKey:    priv,
		PublicKey:     pub,
		BridgeHMACKey: bridgeKey,
	})
	if aErr != nil {
		t.Fatalf("create grant authority: %v", aErr)
	}

	grantDir := filepath.Join(snapRoot, "grant-store")
	watchDir := filepath.Join(snapRoot, "ssh-watchdog")
	_ = os.MkdirAll(grantDir, 0700)
	_ = os.MkdirAll(watchDir, 0700)

	createTestSnapshotManifest(t, snapRoot, binding.OperationID)
	st := PersistentSSHGuardState{
		GuardID:      binding.OperationID,
		OperationID:  binding.OperationID,
		Status:       SSHGuardStatusArmed,
		ArmedAt:      time.Now().UTC(),
		DeadlineAt:   time.Now().UTC().Add(10 * time.Minute),
		ExternalUnit: fmt.Sprintf("pid:%d:%s", os.Getpid(), binding.OperationID),
	}
	sRaw, _ := json.Marshal(st)
	_ = os.WriteFile(filepath.Join(watchDir, binding.OperationID+".json"), sRaw, 0600)

	grant, gErr := auth.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID:        binding.ProposalID,
		ApprovalID:        binding.ApprovalID,
		OperationID:       binding.OperationID,
		ResourceScope:     binding.ResourceScope,
		BaselineHash:      binding.BaselineHash,
		FencingGeneration: binding.FencingGeneration,
		Action:            ActionHostPackageUpgrade,
		Packages:          []string{"openssh-server"},
	})
	if gErr != nil {
		t.Fatalf("issue grant: %v", gErr)
	}

	mockRunner := &mockHostExecRunner{}
	handler2, _ := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
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
		Runner:         mockRunner,
	})
	srv2 := httptest.NewServer(handler2)
	defer srv2.Close()

	// Arm watchdog step through bridge to transition state machine to WATCHDOG_ARMED
	armReq := HostActionRequest{
		Action:            HostActionWatchdogArm,
		Binding:           binding,
		GuardID:           binding.OperationID,
		SnapshotID:        binding.OperationID,
		WatchdogTimeoutMs: 30000,
		Grant:             grant,
	}
	codeArm, bodyArm := sendRawBridgeRequest(t, srv2.URL, "bridge-k1", bridgeKey, "nonce-p2-arm", armReq)
	if codeArm != http.StatusOK {
		t.Fatalf("arm watchdog step failed: %d (%s)", codeArm, string(bodyArm))
	}

	code2, body2 := sendRawBridgeRequest(t, srv2.URL, "bridge-k1", bridgeKey, "nonce-p2-2", HostActionRequest{
		Action:   HostActionAptUpgrade,
		Binding:  binding,
		Packages: []string{"openssh-server"},
		Grant:    grant,
	})
	if code2 != http.StatusOK {
		t.Fatalf("expected 200 with valid grant, got %d (%s)", code2, string(body2))
	}
}

// TestR13_P2_02_StepDigestsAndAllowedStepsTamper verifies that:
// 1. Actions not in AllowedSteps are blocked (403).
// 2. Tampered parameters mismatching StepParamDigests are blocked (403).
func TestR13_P2_02_StepDigestsAndAllowedStepsTamper(t *testing.T) {
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
		ProposalID:        "prop-p2-tamper",
		ApprovalID:        "apv-p2-tamper",
		OperationID:       "op-p2-tamper",
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:testbase",
		FencingGeneration: 1,
	}

	upgradeReq := HostActionRequest{
		Action:         HostActionAptUpgrade,
		Binding:        binding,
		Packages:       []string{"openssh-server"},
		TargetVersions: map[string]string{"openssh-server": "1:9.2p1-2+deb12u4"},
	}
	canonicalDigest := ComputeStepParamDigest(upgradeReq)

	watchDir := t.TempDir()
	createTestSnapshotManifest(t, snapRoot, binding.OperationID)
	st := PersistentSSHGuardState{
		GuardID:      binding.OperationID,
		OperationID:  binding.OperationID,
		Status:       SSHGuardStatusArmed,
		ArmedAt:      time.Now().UTC(),
		DeadlineAt:   time.Now().UTC().Add(10 * time.Minute),
		ExternalUnit: fmt.Sprintf("pid:%d:%s", os.Getpid(), binding.OperationID),
	}
	sRaw, _ := json.Marshal(st)
	_ = os.WriteFile(filepath.Join(watchDir, binding.OperationID+".json"), sRaw, 0600)

	armDigest := ComputeStepParamDigest(HostActionRequest{Action: HostActionWatchdogArm, Binding: binding, GuardID: binding.OperationID, SnapshotID: binding.OperationID, WatchdogTimeoutMs: 30000})
	disarmDigest := ComputeStepParamDigest(HostActionRequest{Action: HostActionWatchdogDisarm, Binding: binding, GuardID: binding.OperationID})

	grant, err := auth.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID:        binding.ProposalID,
		ApprovalID:        binding.ApprovalID,
		OperationID:       binding.OperationID,
		ResourceScope:     binding.ResourceScope,
		BaselineHash:      binding.BaselineHash,
		FencingGeneration: binding.FencingGeneration,
		Action:            ActionHostPackageUpgrade,
		AllowedSteps:      []string{string(HostActionWatchdogArm), string(HostActionAptCachePolicy), string(HostActionAptUpgrade), string(HostActionWatchdogDisarm)},
		StepParamDigests: map[string]string{
			string(HostActionWatchdogArm):    armDigest,
			string(HostActionAptUpgrade):     canonicalDigest,
			string(HostActionWatchdogDisarm): disarmDigest,
		},
		Packages:       []string{"openssh-server"},
		TargetVersions: map[string]string{"openssh-server": "1:9.2p1-2+deb12u4"},
	})
	if err != nil {
		t.Fatalf("issue grant: %v", err)
	}

	grantDir := filepath.Join(snapRoot, "grant-store")
	_ = os.MkdirAll(grantDir, 0700)

	mockRunner := &mockHostExecRunner{}
	handler, _ := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
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
		Runner:         mockRunner,
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Arm watchdog step to transition state to WATCHDOG_ARMED
	armReq := HostActionRequest{
		Action:            HostActionWatchdogArm,
		Binding:           binding,
		GuardID:           binding.OperationID,
		SnapshotID:        binding.OperationID,
		WatchdogTimeoutMs: 30000,
		Grant:             grant,
	}
	codeArm, bodyArm := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-p2-t0-arm", armReq)
	if codeArm != http.StatusOK {
		t.Fatalf("arm watchdog step failed: %d (%s)", codeArm, string(bodyArm))
	}

	// 1. Call unpermitted step: ssh.service_restart is NOT in AllowedSteps
	codeDisallowed, bodyDisallowed := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-p2-t1", HostActionRequest{
		Action:  HostActionSSHDRestart,
		Binding: binding,
		Grant:   grant,
	})
	if codeDisallowed != http.StatusForbidden || !strings.Contains(string(bodyDisallowed), "not in allowed_steps") {
		t.Fatalf("expected 403 not in allowed_steps for ssh.service_restart, got %d (%s)", codeDisallowed, string(bodyDisallowed))
	}

	// 2. Call apt.upgrade with tampered target version -> parameter digest mismatch
	tamperedReq := HostActionRequest{
		Action:         HostActionAptUpgrade,
		Binding:        binding,
		Packages:       []string{"openssh-server"},
		TargetVersions: map[string]string{"openssh-server": "1:9.2p1-2+deb12u3"}, // tampered!
		Grant:          grant,
	}
	codeTampered, bodyTampered := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-p2-t2", tamperedReq)
	if codeTampered != http.StatusForbidden || !strings.Contains(string(bodyTampered), "digest mismatch") {
		t.Fatalf("expected 403 digest mismatch for tampered version, got %d (%s)", codeTampered, string(bodyTampered))
	}

	// 3. Call apt.upgrade with exact untampered parameters -> must succeed
	exactReq := HostActionRequest{
		Action:         HostActionAptUpgrade,
		Binding:        binding,
		Packages:       []string{"openssh-server"},
		TargetVersions: map[string]string{"openssh-server": "1:9.2p1-2+deb12u4"},
		Grant:          grant,
	}
	codeOK, bodyOK := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-p2-t3", exactReq)
	if codeOK != http.StatusOK {
		t.Fatalf("expected 200 for untampered parameters, got %d (%s)", codeOK, string(bodyOK))
	}
}

// TestR13_P2_03_StepLevelConsumptionAndReplayDefense verifies that steps within an
// operation are consumed once, replays are blocked, and cross-operation reuse fails.
func TestR13_P2_03_StepLevelConsumptionAndReplayDefense(t *testing.T) {
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
		ProposalID:        "prop-p2-consume",
		ApprovalID:        "apv-p2-consume",
		OperationID:       "op-p2-consume-1",
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:testbase",
		FencingGeneration: 1,
	}

	watchDir := t.TempDir()
	createTestSnapshotManifest(t, snapRoot, binding.OperationID)
	st := PersistentSSHGuardState{
		GuardID:      binding.OperationID,
		OperationID:  binding.OperationID,
		Status:       SSHGuardStatusArmed,
		ArmedAt:      time.Now().UTC(),
		DeadlineAt:   time.Now().UTC().Add(10 * time.Minute),
		ExternalUnit: fmt.Sprintf("pid:%d:%s", os.Getpid(), binding.OperationID),
	}
	sRaw, _ := json.Marshal(st)
	_ = os.WriteFile(filepath.Join(watchDir, binding.OperationID+".json"), sRaw, 0600)

	grant, err := auth.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID:        binding.ProposalID,
		ApprovalID:        binding.ApprovalID,
		OperationID:       binding.OperationID,
		ResourceScope:     binding.ResourceScope,
		BaselineHash:      binding.BaselineHash,
		FencingGeneration: binding.FencingGeneration,
		Action:            ActionHostPackageUpgrade,
		Packages:          []string{"openssh-server"},
	})
	if err != nil {
		t.Fatalf("issue grant: %v", err)
	}

	grantDir := filepath.Join(snapRoot, "grant-store")
	_ = os.MkdirAll(grantDir, 0700)

	mockRunner := &mockHostExecRunner{}
	handler, _ := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
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
		Runner:         mockRunner,
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Arm watchdog step to transition state to WATCHDOG_ARMED
	armReq := HostActionRequest{
		Action:            HostActionWatchdogArm,
		Binding:           binding,
		GuardID:           binding.OperationID,
		SnapshotID:        binding.OperationID,
		WatchdogTimeoutMs: 30000,
		Grant:             grant,
	}
	codeArm, bodyArm := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-p2-t0-arm", armReq)
	if codeArm != http.StatusOK {
		t.Fatalf("arm watchdog step failed: %d (%s)", codeArm, string(bodyArm))
	}

	// 1. Consume apt.upgrade once -> 200 OK
	code1, _ := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-c1", HostActionRequest{
		Action:   HostActionAptUpgrade,
		Binding:  binding,
		Packages: []string{"openssh-server"},
		Grant:    grant,
	})
	if code1 != http.StatusOK {
		t.Fatalf("first apt.upgrade consumption failed: %d", code1)
	}

	// 2. Replay apt.upgrade with new nonce -> 403 ErrExecutionGrantConsumed
	codeReplay, bodyReplay := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-c2", HostActionRequest{
		Action:   HostActionAptUpgrade,
		Binding:  binding,
		Packages: []string{"openssh-server"},
		Grant:    grant,
	})
	if codeReplay != http.StatusForbidden || !strings.Contains(string(bodyReplay), "already consumed") {
		t.Fatalf("expected 403 already consumed on step replay, got %d (%s)", codeReplay, string(bodyReplay))
	}

	// 3. Cross-operation reuse: using same grant with different OperationID in binding
	tamperedOpBinding := binding
	tamperedOpBinding.OperationID = "op-p2-consume-OTHER"
	codeCross, bodyCross := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-c3", HostActionRequest{
		Action:   HostActionAptUpgrade,
		Binding:  tamperedOpBinding,
		Packages: []string{"openssh-server"},
		Grant:    grant,
	})
	if codeCross != http.StatusForbidden {
		t.Fatalf("expected 403 on cross-operation grant reuse, got %d (%s)", codeCross, string(bodyCross))
	}
}

// TestR13_P2_04_WatchdogOperationSnapshotBinding verifies that watchdog_arm and restore_config
// strictly enforce that SnapshotID belongs to the authorized OperationID.
func TestR13_P2_04_WatchdogOperationSnapshotBinding(t *testing.T) {
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
		ProposalID:        "prop-p2-snap",
		ApprovalID:        "apv-p2-snap",
		OperationID:       "op-p2-snap-1",
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:testbase",
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

	// Attempt watchdog_arm with a SnapshotID not belonging to op-p2-snap-1
	codeArmBad, bodyArmBad := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-snap-bad", HostActionRequest{
		Action:            HostActionWatchdogArm,
		Binding:           binding,
		SnapshotID:        "snap-other-operation",
		GuardID:           "guard-1",
		WatchdogTimeoutMs: 5000,
		Grant:             grant,
	})
	if codeArmBad != http.StatusForbidden || !strings.Contains(string(bodyArmBad), "does not match grant operation_id") {
		t.Fatalf("expected 403 on mismatched snapshot_id in watchdog_arm, got %d (%s)", codeArmBad, string(bodyArmBad))
	}

	// Attempt restore_config with mismatched snapshot ID
	codeRestoreBad, bodyRestoreBad := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-res-bad", HostActionRequest{
		Action:     HostActionRestoreConfig,
		Binding:    binding,
		SnapshotID: "snap-other-operation",
		Grant:      grant,
	})
	if codeRestoreBad != http.StatusForbidden || !strings.Contains(string(bodyRestoreBad), "does not match grant operation_id") {
		t.Fatalf("expected 403 on mismatched snapshot_id in restore_config, got %d (%s)", codeRestoreBad, string(bodyRestoreBad))
	}
}

// TestR13_P2_05_WatchdogOfflineReconcileAuditBackfill verifies that an out-of-process
// watchdog recovery that completed while the API was offline is reconciled and
// recorded into the AuditEngine upon API boot.
func TestR13_P2_05_WatchdogOfflineReconcileAuditBackfill(t *testing.T) {
	guardDir := t.TempDir()
	hostRoot := t.TempDir()
	snapRoot := t.TempDir()

	db, auditEngine := setupHostsecAuditDB(t)

	guardID := "guard-offline-1"
	now := time.Now().UTC()

	// 2. Simulate an offline recovery state file left on disk by a detached host watchdog
	state := PersistentSSHGuardState{
		GuardID:           guardID,
		OperationID:       "op-offline-1",
		ProposalID:        "prop-offline-1",
		ApprovalID:        "apv-offline-1",
		BaselineHash:      "host-v1:base",
		FencingGeneration: 1,
		HostRoot:          hostRoot,
		ArmedAt:           now.Add(-2 * time.Minute),
		DeadlineAt:        now.Add(-1 * time.Minute),
		RecoveredAt:       &now,
		Status:            SSHGuardStatusRecovered,
	}
	stateData, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(guardDir, guardID+".json"), stateData, 0600); err != nil {
		t.Fatalf("write offline guard state: %v", err)
	}

	// 3. Cold boot PersistentHostSSHGuard with AuditEngine attached
	guard := NewPersistentHostSSHGuard(guardDir, hostRoot, snapRoot, nil, nil)
	guard.AuditEngine = auditEngine

	// ReconcileExpired should detect the unsynced RECOVERED state and backfill an audit event
	recCount, rErr := guard.ReconcileExpired(context.Background())
	if rErr != nil {
		t.Fatalf("reconcile expired failed: %v", rErr)
	}
	_ = recCount

	// Verify .audit_synced marker exists
	syncPath := filepath.Join(guardDir, guardID+".audit_synced")
	if _, statErr := os.Stat(syncPath); statErr != nil {
		t.Fatalf("expected .audit_synced marker to be created: %v", statErr)
	}

	// Query events from DB
	var auditEvents []model.AgentAuditEvent
	if err := db.Where("operation_id = ?", "op-offline-1").Find(&auditEvents).Error; err != nil {
		t.Fatalf("query audit events: %v", err)
	}
	if len(auditEvents) == 0 {
		t.Fatalf("expected audit event backfilled for offline watchdog recovery, found 0")
	}
	if auditEvents[0].Action != "host.ssh_watchdog.reconcile" || auditEvents[0].Result != "WATCHDOG_RECOVERED" {
		t.Fatalf("unexpected audit event content: action=%s result=%s", auditEvents[0].Action, auditEvents[0].Result)
	}

	// 4. Verify idempotency: running ReconcileExpired again should not duplicate the audit event
	_, _ = guard.ReconcileExpired(context.Background())
	var auditEvents2 []model.AgentAuditEvent
	_ = db.Where("operation_id = ?", "op-offline-1").Find(&auditEvents2).Error
	if len(auditEvents2) != 1 {
		t.Fatalf("expected exactly 1 audit event (idempotent), got %d", len(auditEvents2))
	}
}

// TestR13_P2_06_PIDReuseProtection verifies that isProcessRecycledOrDead
// protects against killing unrelated recycled OS processes.
func TestR13_P2_06_PIDReuseProtection(t *testing.T) {
	// A non-existent PID should be identified as already dead
	deadPID := 999999
	if !isProcessRecycledOrDead(deadPID, "guard-test-xyz") {
		t.Fatalf("expected dead PID %d to be recognized as dead/recycled", deadPID)
	}

	// On Linux, /proc/<pid>/cmdline verifies process identity to defend against PID reuse
	if runtime.GOOS == "linux" {
		currentPID := os.Getpid()
		if !isProcessRecycledOrDead(currentPID, "guard-test-nonexistent-string-xyz123") {
			t.Fatalf("expected process without guardID marker in cmdline to be recognized as recycled")
		}
	}
}
