package hostsec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func makeSignedBridgeTestRequest(key []byte, keyID string, req HostActionRequest, nonce string, ts time.Time) (*http.Request, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	tsStr := strconv.FormatInt(ts.UTC().UnixMilli(), 10)
	sig := computeBridgeRequestSignature(key, http.MethodPost, bridgeActionHTTPPath, tsStr, nonce, body)

	httpReq := httptest.NewRequest(http.MethodPost, bridgeActionHTTPPath, bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(bridgeHeaderKeyID, keyID)
	httpReq.Header.Set(bridgeHeaderTimestamp, tsStr)
	httpReq.Header.Set(bridgeHeaderNonce, nonce)
	httpReq.Header.Set(bridgeHeaderSignature, "hmac-sha256="+sig)
	return httpReq, nil
}

func TestR12_HostBridge_CommandInjectionAndShellMetacharsRejected(t *testing.T) {
	key := []byte("r12-unit-test-hmac-bridge-key-32bytes-ok")
	keyID := "bridge-k1"
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()

	mockRunner := &mockHostExecRunner{
		handlers: map[string]func(args []string) (string, string, error){
			"dpkg-query": func(args []string) (string, string, error) {
				return "install ok installed\t1:9.2p1-2+deb12u3\n", "", nil
			},
		},
	}

	handler, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		KeyID:          keyID,
		HMACKey:        key,
		Runner:         mockRunner,
		RequireFencing: true,
	})
	if err != nil {
		t.Fatalf("NewHostBridgeHTTPHandler: %v", err)
	}

	injectionTargets := []struct {
		desc        string
		pkg         string
		action      HostActionType
		expectedErr string
	}{
		{"semicolon injection", "openssh-server; rm -rf /", HostActionAptUpgrade, "invalid debian package name"},
		{"command substitution", "openssh-server$(id)", HostActionAptUpgrade, "invalid debian package name"},
		{"pipe injection", "openssh-server | sh", HostActionAptUpgrade, "invalid debian package name"},
		{"flag injection", "--force-yes", HostActionAptUpgrade, "invalid debian package name"},
		{"path traversal in pkg", "../../bin/sh", HostActionAptUpgrade, "invalid debian package name"},
		{"whitespace injection", "openssh-server curl", HostActionAptUpgrade, "invalid debian package name"},
		{"unwhitelisted binary bash", "bash", HostActionAptUpgrade, "package is not in host update whitelist"},
		{"unwhitelisted binary ncat", "ncat", HostActionAptUpgrade, "package is not in host update whitelist"},
		{"kernel package blocked", "linux-image-amd64", HostActionAptUpgrade, "package is not in host update whitelist"},
		{"kernel generic blocked", "linux-image-6.1.0-26-amd64", HostActionAptUpgrade, "package is not in host update whitelist"},
	}

	for i, tc := range injectionTargets {
		t.Run(tc.desc, func(t *testing.T) {
			reqPayload := HostActionRequest{
				Action:   tc.action,
				Packages: []string{tc.pkg},
				TargetVersions: map[string]string{
					tc.pkg: "1:9.2p1-2+deb12u4",
				},
				Binding: HostExecutionBinding{
					ProposalID:        fmt.Sprintf("prop-r12-inject-%d", i),
					ApprovalID:        fmt.Sprintf("appr-r12-inject-%d", i),
					OperationID:       fmt.Sprintf("op-r12-inject-%d", i),
					BaselineHash:      "host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
					FencingGeneration: 1,
				},
			}
			httpReq, sErr := makeSignedBridgeTestRequest(key, keyID, reqPayload, fmt.Sprintf("nonce-inject-%d", i), time.Now())
			if sErr != nil {
				t.Fatalf("sign request: %v", sErr)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httpReq)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 Bad Request for %q, got HTTP %d: %s", tc.desc, rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), tc.expectedErr) {
				t.Fatalf("expected error body to contain %q, got %s", tc.expectedErr, rr.Body.String())
			}
		})
	}
}

func TestR12_HostBridge_HelperRestart_PersistentNonceAntiReplay(t *testing.T) {
	key := []byte("r12-unit-test-hmac-bridge-key-32bytes-ok")
	keyID := "bridge-k1"
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	nonceDir := filepath.Join(snapRoot, "custom-nonces")

	mockRunner := &mockHostExecRunner{
		handlers: map[string]func(args []string) (string, string, error){
			"dpkg-query": func(args []string) (string, string, error) {
				return "install ok installed\t1:9.2p1-2+deb12u3\n", "", nil
			},
		},
	}

	// 1. Start Helper instance 1
	handler1, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		NonceDir:       nonceDir,
		KeyID:          keyID,
		HMACKey:        key,
		Runner:         mockRunner,
		RequireFencing: true,
	})
	if err != nil {
		t.Fatalf("NewHostBridgeHTTPHandler instance 1: %v", err)
	}

	reqPayload := HostActionRequest{
		Action:   HostActionDpkgQuery,
		Packages: []string{"openssh-server"},
		Binding: HostExecutionBinding{
			ProposalID:        "prop-r12-nonce-1",
			ApprovalID:        "appr-r12-nonce-1",
			OperationID:       "op-r12-nonce-1",
			BaselineHash:      "host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			FencingGeneration: 1,
		},
	}
	fixedNonce := "fixed-nonce-replay-001"
	ts := time.Now()

	httpReq1, err := makeSignedBridgeTestRequest(key, keyID, reqPayload, fixedNonce, ts)
	if err != nil {
		t.Fatal(err)
	}
	rr1 := httptest.NewRecorder()
	handler1.ServeHTTP(rr1, httpReq1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("first request expected 200 OK, got HTTP %d: %s", rr1.Code, rr1.Body.String())
	}

	// Immediate replay against instance 1 -> rejected
	httpReq1Replay, _ := makeSignedBridgeTestRequest(key, keyID, reqPayload, fixedNonce, ts)
	rr1Replay := httptest.NewRecorder()
	handler1.ServeHTTP(rr1Replay, httpReq1Replay)
	if rr1Replay.Code != http.StatusUnauthorized || !strings.Contains(rr1Replay.Body.String(), "bridge nonce replayed") {
		t.Fatalf("expected 401 bridge nonce replayed on same instance, got %d: %s", rr1Replay.Code, rr1Replay.Body.String())
	}

	// 2. Simulate Helper Crash & Restart: initialize brand-new handler2 pointing to same NonceDir
	handler2, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		NonceDir:       nonceDir,
		KeyID:          keyID,
		HMACKey:        key,
		Runner:         mockRunner,
		RequireFencing: true,
	})
	if err != nil {
		t.Fatalf("NewHostBridgeHTTPHandler instance 2: %v", err)
	}

	// Replay intercepted request against restarted instance 2 -> MUST be rejected because nonce was persisted on disk!
	httpReq2Replay, _ := makeSignedBridgeTestRequest(key, keyID, reqPayload, fixedNonce, ts)
	rr2Replay := httptest.NewRecorder()
	handler2.ServeHTTP(rr2Replay, httpReq2Replay)
	if rr2Replay.Code != http.StatusUnauthorized || !strings.Contains(rr2Replay.Body.String(), "bridge nonce replayed") {
		t.Fatalf("expected 401 bridge nonce replayed on restarted Helper instance, got %d: %s", rr2Replay.Code, rr2Replay.Body.String())
	}
}

func TestR12_HostBridge_MonotonicFencingTokenRejection(t *testing.T) {
	key := []byte("r12-unit-test-hmac-bridge-key-32bytes-ok")
	keyID := "bridge-k1"
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	fencingDir := filepath.Join(snapRoot, "custom-fencing")

	mockRunner := &mockHostExecRunner{
		handlers: map[string]func(args []string) (string, string, error){
			"dpkg-query": func(args []string) (string, string, error) {
				return "install ok installed\t1:9.2p1-2+deb12u3\n", "", nil
			},
		},
	}

	handler1, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		FencingDir:     fencingDir,
		KeyID:          keyID,
		HMACKey:        key,
		Runner:         mockRunner,
		RequireFencing: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	proposalID := "prop-r12-fencing-mono"
	reqPayloadGen5 := HostActionRequest{
		Action:   HostActionDpkgQuery,
		Packages: []string{"openssh-server"},
		Binding: HostExecutionBinding{
			ProposalID:        proposalID,
			ApprovalID:        "appr-1",
			OperationID:       "op-1",
			BaselineHash:      "host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			FencingGeneration: 5,
		},
	}

	// 1. Initial request with FencingGeneration = 5 succeeds
	httpReqGen5, _ := makeSignedBridgeTestRequest(key, keyID, reqPayloadGen5, "nonce-fence-5", time.Now())
	rrGen5 := httptest.NewRecorder()
	handler1.ServeHTTP(rrGen5, httpReqGen5)
	if rrGen5.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for gen 5, got %d: %s", rrGen5.Code, rrGen5.Body.String())
	}

	// 2. Delayed/stale request with FencingGeneration = 2 (< 5) MUST be rejected with 400 Bad Request
	reqPayloadGen2 := reqPayloadGen5
	reqPayloadGen2.Binding.FencingGeneration = 2
	httpReqGen2, _ := makeSignedBridgeTestRequest(key, keyID, reqPayloadGen2, "nonce-fence-2", time.Now())
	rrGen2 := httptest.NewRecorder()
	handler1.ServeHTTP(rrGen2, httpReqGen2)
	if rrGen2.Code != http.StatusBadRequest || !strings.Contains(rrGen2.Body.String(), "stale or non-monotonic fencing generation") {
		t.Fatalf("expected 400 stale fencing generation for gen 2 < 5, got %d: %s", rrGen2.Code, rrGen2.Body.String())
	}

	// 3. Helper restarts: new handler with same FencingDir
	handler2, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		FencingDir:     fencingDir,
		KeyID:          keyID,
		HMACKey:        key,
		Runner:         mockRunner,
		RequireFencing: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// After restart, request with FencingGeneration = 3 (< 5) MUST STILL be rejected
	reqPayloadGen3 := reqPayloadGen5
	reqPayloadGen3.Binding.FencingGeneration = 3
	httpReqGen3, _ := makeSignedBridgeTestRequest(key, keyID, reqPayloadGen3, "nonce-fence-3", time.Now())
	rrGen3 := httptest.NewRecorder()
	handler2.ServeHTTP(rrGen3, httpReqGen3)
	if rrGen3.Code != http.StatusBadRequest || !strings.Contains(rrGen3.Body.String(), "stale or non-monotonic fencing generation") {
		t.Fatalf("expected 400 stale fencing generation across restart for gen 3 < 5, got %d: %s", rrGen3.Code, rrGen3.Body.String())
	}

	// Higher generation (FencingGeneration = 6) succeeds
	reqPayloadGen6 := reqPayloadGen5
	reqPayloadGen6.Binding.FencingGeneration = 6
	httpReqGen6, _ := makeSignedBridgeTestRequest(key, keyID, reqPayloadGen6, "nonce-fence-6", time.Now())
	rrGen6 := httptest.NewRecorder()
	handler2.ServeHTTP(rrGen6, httpReqGen6)
	if rrGen6.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for gen 6, got %d: %s", rrGen6.Code, rrGen6.Body.String())
	}
}

func TestR12_PersistentHostSSHGuard_CorruptedDiskStateSafeHandling(t *testing.T) {
	guardDir := t.TempDir()
	hostRoot := t.TempDir()
	snapRoot := t.TempDir()
	mockRunner := &mockHostExecRunner{}

	guard := NewPersistentHostSSHGuard(guardDir, hostRoot, snapRoot, mockRunner, nil)

	// Write garbage / truncated data to a .json file in GuardDir
	corruptGuardID := "sshguard-corrupted-test-01"
	corruptPath := filepath.Join(guardDir, corruptGuardID+".json")
	if err := os.WriteFile(corruptPath, []byte("{ truncated invalid json content ..."), 0600); err != nil {
		t.Fatal(err)
	}

	// LoadState should return error without panic
	st, err := guard.LoadState(corruptGuardID)
	if err == nil || st != nil {
		t.Fatalf("expected error on loading corrupted guard state, got st=%v err=%v", st, err)
	}

	// ReconcileExpired should safely ignore corrupted file without crashing
	count, recErr := guard.ReconcileExpired(context.Background())
	if count != 0 {
		t.Fatalf("expected 0 recovered on corrupted state, got %d (err: %v)", count, recErr)
	}
}

func TestR12_KernelCVE_RemediationModeAdvisoryOnly(t *testing.T) {
	// Verify that Linux kernel packages are never automatically remediated via apt-get,
	// and are strictly classified as manual_advisory.
	kernelTargets := []string{
		"linux-image-amd64",
		"linux-image-6.1.0-26-amd64",
		"linux",
		"linux-headers-amd64",
	}

	for _, target := range kernelTargets {
		mode := remediationModeForTarget("host_pkg_upgrade", target)
		if mode != "manual_advisory" {
			t.Fatalf("expected target %q to yield remediationMode=manual_advisory, got %q", target, mode)
		}
		if AllowedHostPackages[target] {
			t.Fatalf("kernel package %q MUST NOT be in AllowedHostPackages whitelist", target)
		}
	}
}

func TestR12_SSHConfigPathTraversalRejection(t *testing.T) {
	key := []byte("r12-unit-test-hmac-bridge-key-32bytes-ok")
	keyID := "bridge-k1"
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	mockRunner := &mockHostExecRunner{}

	handler, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		KeyID:          keyID,
		HMACKey:        key,
		Runner:         mockRunner,
		RequireFencing: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	traversalPayload := HostActionRequest{
		Action:     HostActionRestoreConfig,
		SnapshotID: "../../etc/passwd",
		Binding: HostExecutionBinding{
			ProposalID:        "prop-traversal-1",
			ApprovalID:        "appr-1",
			OperationID:       "op-1",
			BaselineHash:      "host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			FencingGeneration: 1,
		},
	}
	httpReq, err := makeSignedBridgeTestRequest(key, keyID, traversalPayload, "nonce-trav-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httpReq)

	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid snapshot_id") {
		t.Fatalf("expected 400 Bad Request on path traversal snapshot_id, got %d: %s", rr.Code, rr.Body.String())
	}
}
