package hostsec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
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
)

func TestR13_FI_12A_CompositeBridgeAdversarialAndTraversal(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	bridgeKey, _ := r13TestBridgeKeys()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	grantSigner, err := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:      "grant-ed-k1",
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatalf("NewHostGrantAuthority(signer): %v", err)
	}

	runner := &mockHostExecRunner{
		handlers: map[string]func(args []string) (string, string, error){
			"apt-cache": func(_ []string) (string, string, error) {
				return "openssh-server:\n  Installed: 1:9.2p1-2+deb12u3\n  Candidate: 1:9.2p1-2+deb12u4\n", "", nil
			},
		},
	}

	// Bridge holds ONLY Ed25519 PublicKey (no GrantHMACKey, no GrantPrivateKey)
	h, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		RequireGrant:   true,
		GrantKeyID:     "grant-ed-k1",
		GrantPublicKey: pub,
		Runner:         runner,
	})
	if err != nil {
		t.Fatalf("NewHostBridgeHTTPHandler(Ed25519 verify-only): %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	base := "host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	// 1. DisallowUnknownFields rejects injected argv/cmd JSON fields (HTTP 400).
	unknownBody := []byte(`{"action":"ssh.config_test","binding":{"proposal_id":"p-unk","approval_id":"a-unk","operation_id":"op-unk","baseline_hash":"` + base + `","fencing_generation":1},"argv":["/bin/sh","-c","id"]}`)
	tsUnk := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	sigUnk := computeBridgeRequestSignature(bridgeKey, http.MethodPost, bridgeActionHTTPPath, tsUnk, "nonce-unk-1", unknownBody)
	reqUnk, _ := http.NewRequest(http.MethodPost, srv.URL+bridgeActionHTTPPath, bytes.NewReader(unknownBody))
	reqUnk.Header.Set("Content-Type", "application/json")
	reqUnk.Header.Set(bridgeHeaderKeyID, "bridge-k1")
	reqUnk.Header.Set(bridgeHeaderTimestamp, tsUnk)
	reqUnk.Header.Set(bridgeHeaderNonce, "nonce-unk-1")
	reqUnk.Header.Set(bridgeHeaderSignature, "hmac-sha256="+sigUnk)
	respUnk, err := http.DefaultClient.Do(reqUnk)
	if err != nil {
		t.Fatalf("do unknown field req: %v", err)
	}
	_ = respUnk.Body.Close()
	if respUnk.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for unknown JSON fields, got %d", respUnk.StatusCode)
	}

	// 2. Oversized payload (>64KB) rejected with HTTP 413 RequestEntityTooLarge.
	hugeBody := bytes.Repeat([]byte("A"), maxBridgeRequestBodyBytes+32)
	tsHuge := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	sigHuge := computeBridgeRequestSignature(bridgeKey, http.MethodPost, bridgeActionHTTPPath, tsHuge, "nonce-huge-1", hugeBody)
	reqHuge, _ := http.NewRequest(http.MethodPost, srv.URL+bridgeActionHTTPPath, bytes.NewReader(hugeBody))
	reqHuge.Header.Set(bridgeHeaderKeyID, "bridge-k1")
	reqHuge.Header.Set(bridgeHeaderTimestamp, tsHuge)
	reqHuge.Header.Set(bridgeHeaderNonce, "nonce-huge-1")
	reqHuge.Header.Set(bridgeHeaderSignature, "hmac-sha256="+sigHuge)
	respHuge, err := http.DefaultClient.Do(reqHuge)
	if err != nil {
		t.Fatalf("do huge req: %v", err)
	}
	_ = respHuge.Body.Close()
	if respHuge.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected HTTP 413 for oversized body, got %d", respHuge.StatusCode)
	}

	// 3. Path-traversal snapshot_id & escaping manifest CopiedFiles backup path rejected on RestoreConfig.
	evilSnapID := "snap-evil-escape"
	evilSnapDir := filepath.Join(snapRoot, evilSnapID)
	if err := os.MkdirAll(evilSnapDir, 0700); err != nil {
		t.Fatal(err)
	}
	outsideSecret := filepath.Join(t.TempDir(), "outside-secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("DO_NOT_OVERWRITE_SSHD"), 0600); err != nil {
		t.Fatal(err)
	}
	mb, _ := json.Marshal(HostUpdateSnapshot{
		SnapshotID:  evilSnapID,
		SnapshotDir: evilSnapDir,
		CopiedFiles: map[string]string{
			"etc/ssh/sshd_config": outsideSecret,
		},
	})
	if err := os.WriteFile(filepath.Join(evilSnapDir, "manifest.json"), mb, 0600); err != nil {
		t.Fatal(err)
	}
	escapeBinding := HostExecutionBinding{
		ProposalID: "p-esc", ApprovalID: "a-esc", OperationID: "op-esc",
		ResourceScope: "host-os", BaselineHash: base, FencingGeneration: 1,
	}
	escapeGrant, err := grantSigner.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID:        escapeBinding.ProposalID,
		ApprovalID:        escapeBinding.ApprovalID,
		OperationID:       escapeBinding.OperationID,
		ResourceScope:     escapeBinding.ResourceScope,
		BaselineHash:      escapeBinding.BaselineHash,
		FencingGeneration: escapeBinding.FencingGeneration,
		Action:            string(HostActionRestoreConfig),
	})
	if err != nil {
		t.Fatal(err)
	}
	codeEsc, bodyEsc := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-esc-1", HostActionRequest{
		Action:     HostActionRestoreConfig,
		Binding:    escapeBinding,
		Grant:      escapeGrant,
		SnapshotID: evilSnapID,
	})
	if codeEsc != http.StatusInternalServerError || !strings.Contains(string(bodyEsc), "escapes snapshot directory") {
		t.Fatalf("expected 500 escape rejection for outside snapshot path, got %d (%s)", codeEsc, string(bodyEsc))
	}
	sshdNow, _ := os.ReadFile(filepath.Join(debRoot, "etc/ssh/sshd_config"))
	if strings.Contains(string(sshdNow), "DO_NOT_OVERWRITE_SSHD") {
		t.Fatalf("outside file leaked into host sshd_config!")
	}

	// 4. Invalid watchdog guard_id & out-of-bounds watchdog_timeout_ms rejected (400).
	codeWD1, _ := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-wd-badid", HostActionRequest{
		Action:            HostActionWatchdogArm,
		Binding:           escapeBinding,
		SnapshotID:        evilSnapID,
		GuardID:           "../escape",
		WatchdogTimeoutMs: 5000,
	})
	if codeWD1 != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for traversal guard_id, got %d", codeWD1)
	}
	codeWD2, _ := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-wd-badms", HostActionRequest{
		Action:            HostActionWatchdogArm,
		Binding:           escapeBinding,
		SnapshotID:        evilSnapID,
		GuardID:           "ssh-guard-valid",
		WatchdogTimeoutMs: maxBridgeWatchdogTimeoutMs + 1,
	})
	if codeWD2 != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for excessive watchdog_timeout_ms, got %d", codeWD2)
	}

	// 5. Genuine Ed25519 grant succeeds against verify-only bridge at higher monotonic fencing generation.
	okBinding := HostExecutionBinding{
		ProposalID: "p-ed-ok", ApprovalID: "a-ed-ok", OperationID: "op-ed-ok",
		ResourceScope: "host-os", BaselineHash: base, FencingGeneration: 2,
	}
	okGrant, err := grantSigner.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID:        okBinding.ProposalID,
		ApprovalID:        okBinding.ApprovalID,
		OperationID:       okBinding.OperationID,
		ResourceScope:     okBinding.ResourceScope,
		BaselineHash:      okBinding.BaselineHash,
		FencingGeneration: okBinding.FencingGeneration,
		Action:            string(HostActionAptCachePolicy),
		Packages:          []string{"openssh-server"},
	})
	if err != nil {
		t.Fatal(err)
	}
	codeOK, bodyOK := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-ed-ok", HostActionRequest{
		Action:   HostActionAptCachePolicy,
		Binding:  okBinding,
		Grant:    okGrant,
		Packages: []string{"openssh-server"},
	})
	if codeOK != http.StatusOK {
		t.Fatalf("expected 200 OK for valid Ed25519 grant, got %d (%s)", codeOK, string(bodyOK))
	}
	_ = context.Background()
}

func TestR13_FI_12B_ConcurrentGrantFencingNonceRace(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	bridgeKey, grantKey := r13TestBridgeKeys()

	grantAuth, err := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         "grant-race-k1",
		HMACKey:       grantKey,
		BridgeHMACKey: bridgeKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	var aptCalls atomic.Int32
	runner := &mockHostExecRunner{
		handlers: map[string]func(args []string) (string, string, error){
			"apt-get": func(_ []string) (string, string, error) {
				aptCalls.Add(1)
				return "upgraded", "", nil
			},
		},
	}

	watchDir := t.TempDir()
	grantDir := t.TempDir()
	createTestSnapshotManifest(t, snapRoot, "op-race-single")
	st := PersistentSSHGuardState{
		GuardID:      "op-race-single",
		OperationID:  "op-race-single",
		Status:       SSHGuardStatusArmed,
		ArmedAt:      time.Now().UTC(),
		DeadlineAt:   time.Now().UTC().Add(10 * time.Minute),
		ExternalUnit: fmt.Sprintf("pid:%d:op-race-single", os.Getpid()),
	}
	sRaw, _ := json.Marshal(st)
	_ = os.WriteFile(filepath.Join(watchDir, "op-race-single.json"), sRaw, 0600)

	h, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		WatchdogDir:    watchDir,
		GrantDir:       grantDir,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		RequireGrant:   true,
		GrantKeyID:     "grant-race-k1",
		GrantHMACKey:   grantKey,
		Runner:         runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	base := "host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	binding := HostExecutionBinding{
		ProposalID: "prop-race-single", ApprovalID: "apv-race-single", OperationID: "op-race-single",
		ResourceScope: "host-os", BaselineHash: base, FencingGeneration: 10,
	}
	singleGrant, err := grantAuth.IssueGrant(HostExecutionGrantIssueRequest{
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
		t.Fatal(err)
	}

	// Arm watchdog step to advance grant state machine to WATCHDOG_ARMED
	armReq := HostActionRequest{
		Action:            HostActionWatchdogArm,
		Binding:           binding,
		GuardID:           binding.OperationID,
		SnapshotID:        binding.OperationID,
		WatchdogTimeoutMs: 30000,
		Grant:             singleGrant,
	}
	codeArm, bodyArm := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-race-arm", armReq)
	if codeArm != http.StatusOK {
		t.Fatalf("arm watchdog step failed: %d (%s)", codeArm, string(bodyArm))
	}

	const workers = 12
	var wg sync.WaitGroup
	var okCount, forbidCount atomic.Int32
	startGate := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-startGate
			code, _ := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, fmt.Sprintf("nonce-race-single-%d", idx), HostActionRequest{
				Action:   HostActionAptUpgrade,
				Binding:  binding,
				Packages: []string{"openssh-server"},
				Grant:    singleGrant,
			})
			switch code {
			case http.StatusOK:
				okCount.Add(1)
			case http.StatusForbidden:
				forbidCount.Add(1)
			}
		}(i)
	}
	close(startGate)
	wg.Wait()

	if okCount.Load() != 1 || forbidCount.Load() != workers-1 || aptCalls.Load() != 1 {
		t.Fatalf("expected exactly 1 single-use grant winner and %d 403 rejections (aptCalls=1), got ok=%d forbidden=%d aptCalls=%d",
			workers-1, okCount.Load(), forbidCount.Load(), aptCalls.Load())
	}
}
