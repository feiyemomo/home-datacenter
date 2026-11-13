package hostsec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"home-datacenter-api/internal/model"
)

func r13TestBridgeKeys() ([]byte, []byte) {
	bridgeKey := []byte("bridge-transport-key-0123456789abcdef0123456789abcdef")
	grantKey := []byte("grant-authority-independent-key-fedcba9876543210fedcba98")
	return bridgeKey, grantKey
}

func sendRawBridgeRequest(
	t *testing.T,
	serverURL string,
	keyID string,
	hmacKey []byte,
	nonce string,
	req HostActionRequest,
) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal HostActionRequest: %v", err)
	}
	ts := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	sig := computeBridgeRequestSignature(hmacKey, http.MethodPost, bridgeActionHTTPPath, ts, nonce, body)
	httpReq, err := http.NewRequest(http.MethodPost, serverURL+bridgeActionHTTPPath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new HTTP request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(bridgeHeaderKeyID, keyID)
	httpReq.Header.Set(bridgeHeaderTimestamp, ts)
	httpReq.Header.Set(bridgeHeaderNonce, nonce)
	httpReq.Header.Set(bridgeHeaderSignature, "hmac-sha256="+sig)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("do HTTP request: %v", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func TestR13_FI_01_NoncePersistenceFailClosed(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	bridgeKey, _ := r13TestBridgeKeys()
	mockRunner := &mockHostExecRunner{}

	// 1. Startup fail-closed when NonceDir path is blocked by an existing regular file.
	fileAsDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(fileAsDir, []byte("blocker"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:   debRoot,
		NonceDir:   fileAsDir,
		FencingDir: t.TempDir(),
		KeyID:      "bridge-k1",
		HMACKey:    bridgeKey,
		Runner:     mockRunner,
	}); err == nil {
		t.Fatalf("expected NewHostBridgeHTTPHandler to fail closed when NonceDir is a file")
	}

	// 2. Startup fail-closed when NonceDir contains a corrupted .ts file.
	corruptNonceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(corruptNonceDir, "abc123.ts"), []byte("corrupted-non-int"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:   debRoot,
		NonceDir:   corruptNonceDir,
		FencingDir: t.TempDir(),
		KeyID:      "bridge-k1",
		HMACKey:    bridgeKey,
		Runner:     mockRunner,
	}); err == nil {
		t.Fatalf("expected NewHostBridgeHTTPHandler to fail closed on corrupted .ts nonce file")
	}

	// 3. Request-time WriteFile/Rename/lock failure MUST return HTTP 500 and block runner execution.
	nonceDir := t.TempDir()
	h, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		NonceDir:       nonceDir,
		FencingDir:     t.TempDir(),
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		Runner:         mockRunner,
	})
	if err != nil {
		t.Fatalf("NewHostBridgeHTTPHandler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Pre-create a directory at the exact target nonce file path so Rename fails even as UID 0 root.
	badNonce := "nonce-write-fail-001"
	sum := sha256.Sum256([]byte(badNonce))
	targetConflictDir := filepath.Join(nonceDir, hex.EncodeToString(sum[:])+".ts")
	if err := os.MkdirAll(targetConflictDir, 0700); err != nil {
		t.Fatal(err)
	}

	code, body := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, badNonce, HostActionRequest{
		Action: HostActionSSHDTest,
		Binding: HostExecutionBinding{
			ProposalID:        "p-nonce-fail",
			ApprovalID:        "a-nonce-fail",
			OperationID:       "op-nonce-fail",
			BaselineHash:      "host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			FencingGeneration: 1,
		},
	})
	if code != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 on nonce persistence failure, got %d (%s)", code, string(body))
	}
	if len(mockRunner.calls) != 0 {
		t.Fatalf("expected 0 runner calls when nonce persistence fails, got %v", mockRunner.calls)
	}
}

func TestR13_FI_02_FencingPersistenceAndCorruptionFailClosed(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	bridgeKey, _ := r13TestBridgeKeys()
	mockRunner := &mockHostExecRunner{}

	// 1. Startup fail-closed when FencingDir is blocked by a file.
	fileAsDir := filepath.Join(t.TempDir(), "fencing-file")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:   debRoot,
		NonceDir:   t.TempDir(),
		FencingDir: fileAsDir,
		KeyID:      "bridge-k1",
		HMACKey:    bridgeKey,
		Runner:     mockRunner,
	}); err == nil {
		t.Fatalf("expected fail-closed startup when FencingDir is a file")
	}

	// 2. Startup fail-closed when FencingDir contains corrupted .gen state.
	corruptFenceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(corruptFenceDir, "deadbeef.gen"), []byte("{not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:   debRoot,
		NonceDir:   t.TempDir(),
		FencingDir: corruptFenceDir,
		KeyID:      "bridge-k1",
		HMACKey:    bridgeKey,
		Runner:     mockRunner,
	}); err == nil {
		t.Fatalf("expected fail-closed startup on corrupted .gen file")
	}

	// 3. Runtime corrupted .gen file or unwriteable .gen target -> HTTP 500, zero mutations.
	fenceDir := t.TempDir()
	h, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		NonceDir:       t.TempDir(),
		FencingDir:     fenceDir,
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

	scopeSum := sha256.Sum256([]byte("host-os"))
	genFile := filepath.Join(fenceDir, hex.EncodeToString(scopeSum[:])+".gen")
	if err := os.WriteFile(genFile, []byte("CORRUPTED_GEN_STATE"), 0600); err != nil {
		t.Fatal(err)
	}
	code, body := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-fence-corrupt-1", HostActionRequest{
		Action: HostActionSSHDTest,
		Binding: HostExecutionBinding{
			ProposalID:        "p-fence-corrupt",
			ApprovalID:        "a-fence-corrupt",
			OperationID:       "op-fence-corrupt",
			BaselineHash:      "host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			FencingGeneration: 10,
		},
	})
	if code != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 on corrupted .gen fencing state, got %d (%s)", code, string(body))
	}

	// 4. Write/Rename failure on a clean scope -> HTTP 500 fail closed.
	otherScope := "host-other"
	otherSum := sha256.Sum256([]byte(otherScope))
	otherGenConflict := filepath.Join(fenceDir, hex.EncodeToString(otherSum[:])+".gen")
	if err := os.MkdirAll(otherGenConflict, 0700); err != nil {
		t.Fatal(err)
	}
	code2, body2 := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "nonce-fence-write-fail-2", HostActionRequest{
		Action: HostActionSSHDTest,
		Binding: HostExecutionBinding{
			ProposalID:        "p-fence-wf",
			ApprovalID:        "a-fence-wf",
			OperationID:       "op-fence-wf",
			ResourceScope:     otherScope,
			BaselineHash:      "host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			FencingGeneration: 1,
		},
	})
	if code2 != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 on fencing write failure, got %d (%s)", code2, string(body2))
	}
	if len(mockRunner.calls) != 0 {
		t.Fatalf("expected 0 runner calls on fencing IO errors, got %v", mockRunner.calls)
	}
}

func TestR13_Helper_CrossProcessFileLockSubprocess(t *testing.T) {
	if os.Getenv("HDC_R13_LOCK_HELPER") != "1" {
		return
	}
	lockPath := os.Getenv("HDC_R13_LOCK_PATH")
	counterPath := os.Getenv("HDC_R13_COUNTER_PATH")
	iters, _ := strconv.Atoi(os.Getenv("HDC_R13_ITERS"))
	if iters <= 0 {
		iters = 10
	}
	for i := 0; i < iters; i++ {
		unlock, err := acquireCrossProcessFileLock(lockPath)
		if err != nil {
			t.Fatalf("helper acquire lock failed: %v", err)
		}
		raw, rErr := os.ReadFile(counterPath)
		val := 0
		if rErr == nil {
			val, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		}
		if wErr := writeFileAtomicFailClosed(counterPath, []byte(strconv.Itoa(val+1)), 0600); wErr != nil {
			_ = unlock()
			t.Fatalf("helper write counter failed: %v", wErr)
		}
		if uErr := unlock(); uErr != nil {
			t.Fatalf("helper unlock failed: %v", uErr)
		}
	}
}

func TestR13_FI_03_CrossProcessLockAndResourceScopedFencing(t *testing.T) {
	// 1. Real multi-process file lock contention verification across OS subprocesses.
	sharedDir := t.TempDir()
	lockPath := filepath.Join(sharedDir, ".test.lock")
	counterPath := filepath.Join(sharedDir, "counter.txt")
	if err := writeFileAtomicFailClosed(counterPath, []byte("0"), 0600); err != nil {
		t.Fatal(err)
	}
	const procs = 3
	const iters = 15
	var pwg sync.WaitGroup
	var pErrs atomic.Int32
	for p := 0; p < procs; p++ {
		pwg.Add(1)
		go func() {
			defer pwg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestR13_Helper_CrossProcessFileLockSubprocess$")
			cmd.Env = append(os.Environ(),
				"HDC_R13_LOCK_HELPER=1",
				"HDC_R13_LOCK_PATH="+lockPath,
				"HDC_R13_COUNTER_PATH="+counterPath,
				"HDC_R13_ITERS="+strconv.Itoa(iters),
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				pErrs.Add(1)
				t.Logf("subprocess failed: %v (%s)", err, string(out))
			}
		}()
	}
	pwg.Wait()
	if pErrs.Load() != 0 {
		t.Fatalf("expected 0 subprocess lock errors")
	}
	finalRaw, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatal(err)
	}
	finalVal, _ := strconv.Atoi(strings.TrimSpace(string(finalRaw)))
	if finalVal != procs*iters {
		t.Fatalf("cross-process file lock lost updates: expected %d, got %d", procs*iters, finalVal)
	}

	// 2. Resource-scoped monotonic fencing across distinct proposals/operations and handler restarts.
	debRoot := writeHostFixture(t, false)
	bridgeKey, _ := r13TestBridgeKeys()
	nonceDir := t.TempDir()
	fenceDir := t.TempDir()
	runner := &mockHostExecRunner{}

	h1, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		NonceDir:       nonceDir,
		FencingDir:     fenceDir,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		Runner:         runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv1 := httptest.NewServer(h1)
	defer srv1.Close()

	base := "host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// Step 1 & 2 of same active operation (prop-A, op-1, gen=5) on default "host-os" scope succeed.
	for i := 1; i <= 2; i++ {
		code, b := sendRawBridgeRequest(t, srv1.URL, "bridge-k1", bridgeKey, fmt.Sprintf("n-same-op-%d", i), HostActionRequest{
			Action: HostActionSSHDTest,
			Binding: HostExecutionBinding{
				ProposalID:        "prop-A",
				ApprovalID:        "apv-A",
				OperationID:       "op-1",
				BaselineHash:      base,
				FencingGeneration: 5,
			},
		})
		if code != http.StatusOK {
			t.Fatalf("same operation step %d expected 200, got %d (%s)", i, code, string(b))
		}
	}

	// Different operation (op-2) or different proposal (prop-B) reusing equal gen=5 on "host-os" MUST be rejected.
	for idx, tc := range []HostExecutionBinding{
		{ProposalID: "prop-B", ApprovalID: "apv-B", OperationID: "op-2", BaselineHash: base, FencingGeneration: 5},
		{ProposalID: "prop-A", ApprovalID: "apv-A", OperationID: "op-2", BaselineHash: base, FencingGeneration: 5},
		{ProposalID: "prop-C", ApprovalID: "apv-C", OperationID: "op-3", BaselineHash: base, FencingGeneration: 4},
	} {
		code, b := sendRawBridgeRequest(t, srv1.URL, "bridge-k1", bridgeKey, fmt.Sprintf("n-stale-eq-%d", idx), HostActionRequest{
			Action:  HostActionSSHDTest,
			Binding: tc,
		})
		if code != http.StatusBadRequest || !strings.Contains(string(b), "stale or non-monotonic fencing generation") {
			t.Fatalf("case %d expected 400 stale/non-monotonic fencing rejection, got %d (%s)", idx, code, string(b))
		}
	}

	// Separate handler instance (restart) reading same FencingDir rejects gen=5 for prop-D/op-4, accepts gen=6.
	h2, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		NonceDir:       nonceDir,
		FencingDir:     fenceDir,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		Runner:         runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv2 := httptest.NewServer(h2)
	defer srv2.Close()

	codeStale, _ := sendRawBridgeRequest(t, srv2.URL, "bridge-k1", bridgeKey, "n-restart-stale", HostActionRequest{
		Action: HostActionSSHDTest,
		Binding: HostExecutionBinding{
			ProposalID:        "prop-D",
			ApprovalID:        "apv-D",
			OperationID:       "op-4",
			BaselineHash:      base,
			FencingGeneration: 5,
		},
	})
	if codeStale != http.StatusBadRequest {
		t.Fatalf("expected restarted handler to reject reused fencing gen=5 across ops, got %d", codeStale)
	}
	codeNext, bodyNext := sendRawBridgeRequest(t, srv2.URL, "bridge-k1", bridgeKey, "n-restart-gen6", HostActionRequest{
		Action: HostActionSSHDTest,
		Binding: HostExecutionBinding{
			ProposalID:        "prop-D",
			ApprovalID:        "apv-D",
			OperationID:       "op-4",
			BaselineHash:      base,
			FencingGeneration: 6,
		},
	})
	if codeNext != http.StatusOK {
		t.Fatalf("expected monotonic gen=6 to succeed after restart, got %d (%s)", codeNext, string(bodyNext))
	}

	// Distinct resource scope ("host-sub-scope") maintains independent monotonic watermark starting at gen=1.
	codeOther, _ := sendRawBridgeRequest(t, srv2.URL, "bridge-k1", bridgeKey, "n-other-scope-1", HostActionRequest{
		Action: HostActionSSHDTest,
		Binding: HostExecutionBinding{
			ProposalID:        "prop-E",
			ApprovalID:        "apv-E",
			OperationID:       "op-5",
			ResourceScope:     "host-sub-scope",
			BaselineHash:      base,
			FencingGeneration: 1,
		},
	})
	if codeOther != http.StatusOK {
		t.Fatalf("expected independent scope watermark gen=1 to succeed, got %d", codeOther)
	}
}

func TestR13_FI_04_IndependentGrantKeySeparationAndForgery(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	bridgeKey, grantKey := r13TestBridgeKeys()
	runner := &mockHostExecRunner{}

	// 1. Key conflict (GrantHMACKey == BridgeHMACKey) MUST fail closed at authority & bridge construction.
	if _, err := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         "grant-k1",
		HMACKey:       bridgeKey,
		BridgeHMACKey: bridgeKey,
	}); !errors.Is(err, ErrExecutionGrantKeyConflict) {
		t.Fatalf("expected ErrExecutionGrantKeyConflict on shared bridge/grant key, got %v", err)
	}
	if _, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:     debRoot,
		SnapshotRoot: t.TempDir(),
		KeyID:        "bridge-k1",
		HMACKey:      bridgeKey,
		RequireGrant: true,
		GrantHMACKey: bridgeKey,
		Runner:       runner,
	}); !errors.Is(err, ErrExecutionGrantKeyConflict) {
		t.Fatalf("expected bridge init to reject GrantHMACKey == BridgeHMACKey, got %v", err)
	}

	// 2. Compromised container holding BridgeHMACKey CANNOT forge a valid HostExecutionGrant.
	h, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   t.TempDir(),
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		RequireGrant:   true,
		GrantKeyID:     "grant-k1",
		GrantHMACKey:   grantKey,
		Runner:         runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	forgingAuth, err := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:   "grant-k1",
		HMACKey: bridgeKey, // Attacker uses leaked container transport HMAC key
	})
	if err != nil {
		t.Fatal(err)
	}
	base := "host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	binding := HostExecutionBinding{
		ProposalID:        "prop-forge",
		ApprovalID:        "apv-forge",
		OperationID:       "op-forge",
		BaselineHash:      base,
		FencingGeneration: 1,
	}
	forgedGrant, err := forgingAuth.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID:        binding.ProposalID,
		ApprovalID:        binding.ApprovalID,
		OperationID:       binding.OperationID,
		BaselineHash:      binding.BaselineHash,
		FencingGeneration: binding.FencingGeneration,
		Action:            string(HostActionSSHDTest),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Missing grant -> 403
	codeMissing, _ := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "n-grant-missing", HostActionRequest{
		Action:  HostActionSSHDTest,
		Binding: binding,
	})
	if codeMissing != http.StatusForbidden {
		t.Fatalf("expected 403 when grant omitted under RequireGrant, got %d", codeMissing)
	}

	// Forged grant (signed by BridgeHMACKey) -> 403
	codeForged, bodyForged := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "n-grant-forged", HostActionRequest{
		Action:  HostActionSSHDTest,
		Binding: binding,
		Grant:   forgedGrant,
	})
	if codeForged != http.StatusForbidden {
		t.Fatalf("expected 403 for grant forged with bridge transport key, got %d (%s)", codeForged, string(bodyForged))
	}
	if len(runner.calls) != 0 {
		t.Fatalf("expected 0 runner executions for missing/forged grants, got %v", runner.calls)
	}

	// 3. Asymmetric Ed25519 grant verification (bridge only holds PublicKey, never PrivateKey).
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	edIssuer, err := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:      "grant-ed-k1",
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatal(err)
	}
	edHandler, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   t.TempDir(),
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		RequireGrant:   true,
		GrantKeyID:     "grant-ed-k1",
		GrantPublicKey: pub,
		Runner:         runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	edSrv := httptest.NewServer(edHandler)
	defer edSrv.Close()

	edGrant, err := edIssuer.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID:        binding.ProposalID,
		ApprovalID:        binding.ApprovalID,
		OperationID:       binding.OperationID,
		BaselineHash:      binding.BaselineHash,
		FencingGeneration: binding.FencingGeneration,
		Action:            string(HostActionSSHDTest),
	})
	if err != nil {
		t.Fatal(err)
	}
	codeEd, bodyEd := sendRawBridgeRequest(t, edSrv.URL, "bridge-k1", bridgeKey, "n-grant-ed-ok", HostActionRequest{
		Action:  HostActionSSHDTest,
		Binding: binding,
		Grant:   edGrant,
	})
	if codeEd != http.StatusOK {
		t.Fatalf("expected valid Ed25519 grant to succeed (200), got %d (%s)", codeEd, string(bodyEd))
	}
}

func TestR13_FI_05_GrantSixDimBindingTamperRejected(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	bridgeKey, grantKey := r13TestBridgeKeys()
	runner := &mockHostExecRunner{}
	grantAuth, err := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         "grant-k1",
		HMACKey:       grantKey,
		BridgeHMACKey: bridgeKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapRoot := t.TempDir()
	watchDir := filepath.Join(snapRoot, "ssh-watchdog")
	grantDir := filepath.Join(snapRoot, "grant-store")
	_ = os.MkdirAll(watchDir, 0700)
	_ = os.MkdirAll(grantDir, 0700)

	h, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		WatchdogDir:    watchDir,
		GrantDir:       grantDir,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		RequireGrant:   true,
		GrantKeyID:     "grant-k1",
		GrantHMACKey:   grantKey,
		Runner:         runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	base := "host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	validBinding := HostExecutionBinding{
		ProposalID:        "prop-6d",
		ApprovalID:        "apv-6d",
		OperationID:       "op-6d",
		ResourceScope:     "host-os",
		BaselineHash:      base,
		FencingGeneration: 3,
	}

	createTestSnapshotManifest(t, snapRoot, validBinding.OperationID)
	st := PersistentSSHGuardState{
		GuardID:      validBinding.OperationID,
		OperationID:  validBinding.OperationID,
		Status:       SSHGuardStatusArmed,
		ArmedAt:      time.Now().UTC(),
		DeadlineAt:   time.Now().UTC().Add(10 * time.Minute),
		ExternalUnit: fmt.Sprintf("pid:%d:%s", os.Getpid(), validBinding.OperationID),
	}
	sRaw, _ := json.Marshal(st)
	_ = os.WriteFile(filepath.Join(watchDir, validBinding.OperationID+".json"), sRaw, 0600)

	grant, err := grantAuth.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID:        validBinding.ProposalID,
		ApprovalID:        validBinding.ApprovalID,
		OperationID:       validBinding.OperationID,
		ResourceScope:     validBinding.ResourceScope,
		BaselineHash:      validBinding.BaselineHash,
		FencingGeneration: validBinding.FencingGeneration,
		Action:            ActionHostPackageUpgrade,
		Packages:          []string{"openssh-server"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Arm watchdog step through bridge to advance grant state machine to WATCHDOG_ARMED
	armReq := HostActionRequest{
		Action:            HostActionWatchdogArm,
		Binding:           validBinding,
		GuardID:           validBinding.OperationID,
		SnapshotID:        validBinding.OperationID,
		WatchdogTimeoutMs: 30000,
		Grant:             grant,
	}
	codeArm, bodyArm := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "n-arm-step", armReq)
	if codeArm != http.StatusOK {
		t.Fatalf("arm watchdog step failed: %d (%s)", codeArm, string(bodyArm))
	}

	tamperedCases := []struct {
		name string
		req  HostActionRequest
	}{
		{
			name: "proposal_id_mismatch",
			req: HostActionRequest{
				Action: HostActionAptUpgrade,
				Binding: HostExecutionBinding{
					ProposalID: "prop-tampered", ApprovalID: "apv-6d", OperationID: "op-6d",
					BaselineHash: base, FencingGeneration: 3,
				},
				Packages: []string{"openssh-server"},
				Grant:    grant,
			},
		},
		{
			name: "approval_id_mismatch",
			req: HostActionRequest{
				Action: HostActionAptUpgrade,
				Binding: HostExecutionBinding{
					ProposalID: "prop-6d", ApprovalID: "apv-tampered", OperationID: "op-6d",
					BaselineHash: base, FencingGeneration: 3,
				},
				Packages: []string{"openssh-server"},
				Grant:    grant,
			},
		},
		{
			name: "operation_id_mismatch",
			req: HostActionRequest{
				Action: HostActionAptUpgrade,
				Binding: HostExecutionBinding{
					ProposalID: "prop-6d", ApprovalID: "apv-6d", OperationID: "op-tampered",
					BaselineHash: base, FencingGeneration: 3,
				},
				Packages: []string{"openssh-server"},
				Grant:    grant,
			},
		},
		{
			name: "baseline_hash_mismatch",
			req: HostActionRequest{
				Action: HostActionAptUpgrade,
				Binding: HostExecutionBinding{
					ProposalID: "prop-6d", ApprovalID: "apv-6d", OperationID: "op-6d",
					BaselineHash: "host-v1:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", FencingGeneration: 3,
				},
				Packages: []string{"openssh-server"},
				Grant:    grant,
			},
		},
		{
			name: "fencing_gen_mismatch",
			req: HostActionRequest{
				Action: HostActionAptUpgrade,
				Binding: HostExecutionBinding{
					ProposalID: "prop-6d", ApprovalID: "apv-6d", OperationID: "op-6d",
					BaselineHash: base, FencingGeneration: 4,
				},
				Packages: []string{"openssh-server"},
				Grant:    grant,
			},
		},
		{
			name: "action_mismatch",
			req: HostActionRequest{
				Action:  HostActionSSHDTest,
				Binding: validBinding,
				Grant:   grant,
			},
		},
		{
			name: "package_mismatch",
			req: HostActionRequest{
				Action:   HostActionAptUpgrade,
				Binding:  validBinding,
				Packages: []string{"docker-ce"},
				Grant:    grant,
			},
		},
	}

	for i, tc := range tamperedCases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, fmt.Sprintf("n-tamper-%d", i), tc.req)
			if code != http.StatusForbidden {
				t.Fatalf("expected 403 on %s, got %d (%s)", tc.name, code, string(body))
			}
		})
	}
	if len(runner.calls) != 0 {
		t.Fatalf("expected 0 runner calls on tampered grants, got %v", runner.calls)
	}

	// Genuine matching request succeeds because rejected tamper attempts did not consume the grant!
	codeOK, bodyOK := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "n-genuine-after-tampers", HostActionRequest{
		Action:   HostActionAptUpgrade,
		Binding:  validBinding,
		Packages: []string{"openssh-server"},
		Grant:    grant,
	})
	if codeOK != http.StatusOK {
		t.Fatalf("expected genuine request after failed tamper probes to succeed (200), got %d (%s)", codeOK, string(bodyOK))
	}
}

func TestR13_FI_06_GrantExpirySingleUseReplayAndRevocation(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	bridgeKey, grantKey := r13TestBridgeKeys()
	grantAuth, err := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         "grant-k1",
		HMACKey:       grantKey,
		BridgeHMACKey: bridgeKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	runner := &mockHostExecRunner{
		handlers: map[string]func(args []string) (string, string, error){
			"apt-cache": func(_ []string) (string, string, error) {
				return "openssh-server:\n  Installed: 1:9.2p1-2+deb12u3\n  Candidate: 1:9.2p1-2+deb12u4\n", "", nil
			},
			"dpkg-query": func(_ []string) (string, string, error) {
				return "install ok installed\t1:9.2p1-2+deb12u4\n", "", nil
			},
		},
	}

	h1, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		RequireGrant:   true,
		GrantKeyID:     "grant-k1",
		GrantHMACKey:   grantKey,
		Runner:         runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv1 := httptest.NewServer(h1)
	defer srv1.Close()

	base := "host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	b1 := HostExecutionBinding{
		ProposalID: "prop-exp", ApprovalID: "apv-exp", OperationID: "op-exp",
		BaselineHash: base, FencingGeneration: 1,
	}

	// 1. Expired grant rejected (403).
	expGrant, err := grantAuth.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID: b1.ProposalID, ApprovalID: b1.ApprovalID, OperationID: b1.OperationID,
		BaselineHash: b1.BaselineHash, FencingGeneration: b1.FencingGeneration,
		Action: string(HostActionSSHDTest),
		TTL:    20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(45 * time.Millisecond)
	codeExp, bodyExp := sendRawBridgeRequest(t, srv1.URL, "bridge-k1", bridgeKey, "n-exp-1", HostActionRequest{
		Action: HostActionSSHDTest, Binding: b1, Grant: expGrant,
	})
	if codeExp != http.StatusForbidden || !strings.Contains(string(bodyExp), "expired") {
		t.Fatalf("expected 403 grant expired, got %d (%s)", codeExp, string(bodyExp))
	}

	// 2. Single-use grant replay rejected in-process AND after helper restart.
	b2 := HostExecutionBinding{
		ProposalID: "prop-1u", ApprovalID: "apv-1u", OperationID: "op-1u",
		BaselineHash: base, FencingGeneration: 2,
	}
	oneUseGrant, err := grantAuth.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID: b2.ProposalID, ApprovalID: b2.ApprovalID, OperationID: b2.OperationID,
		BaselineHash: b2.BaselineHash, FencingGeneration: b2.FencingGeneration,
		Action: string(HostActionSSHDTest),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := sendRawBridgeRequest(t, srv1.URL, "bridge-k1", bridgeKey, "n-1u-1", HostActionRequest{
		Action: HostActionSSHDTest, Binding: b2, Grant: oneUseGrant,
	}); code != http.StatusOK {
		t.Fatalf("expected first single-use grant invocation to succeed, got %d (%s)", code, string(body))
	}
	if code, body := sendRawBridgeRequest(t, srv1.URL, "bridge-k1", bridgeKey, "n-1u-2", HostActionRequest{
		Action: HostActionSSHDTest, Binding: b2, Grant: oneUseGrant,
	}); code != http.StatusForbidden || !strings.Contains(string(body), "already consumed") {
		t.Fatalf("expected replay of single-use grant to fail 403 already consumed, got %d (%s)", code, string(body))
	}

	h2, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		RequireGrant:   true,
		GrantKeyID:     "grant-k1",
		GrantHMACKey:   grantKey,
		Runner:         runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv2 := httptest.NewServer(h2)
	defer srv2.Close()

	if code, body := sendRawBridgeRequest(t, srv2.URL, "bridge-k1", bridgeKey, "n-1u-3", HostActionRequest{
		Action: HostActionSSHDTest, Binding: b2, Grant: oneUseGrant,
	}); code != http.StatusForbidden || !strings.Contains(string(body), "already consumed") {
		t.Fatalf("expected restarted bridge handler to reject consumed grant replay, got %d (%s)", code, string(body))
	}

	// 3. Persistent revocation blocks unused valid grant.
	b3 := HostExecutionBinding{
		ProposalID: "prop-rev", ApprovalID: "apv-rev", OperationID: "op-rev",
		BaselineHash: base, FencingGeneration: 3,
	}
	revGrant, err := grantAuth.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID: b3.ProposalID, ApprovalID: b3.ApprovalID, OperationID: b3.OperationID,
		BaselineHash: b3.BaselineHash, FencingGeneration: b3.FencingGeneration,
		Action: string(HostActionSSHDTest),
	})
	if err != nil {
		t.Fatal(err)
	}
	gs, err := NewPersistentGrantStore(filepath.Join(snapRoot, "bridge-grants"))
	if err != nil {
		t.Fatal(err)
	}
	if err := gs.RevokeGrant(revGrant.GrantID, "admin emergency revoke"); err != nil {
		t.Fatal(err)
	}
	if code, body := sendRawBridgeRequest(t, srv2.URL, "bridge-k1", bridgeKey, "n-rev-1", HostActionRequest{
		Action: HostActionSSHDTest, Binding: b3, Grant: revGrant,
	}); code != http.StatusForbidden || !strings.Contains(string(body), "revoked") {
		t.Fatalf("expected revoked grant to be rejected with 403, got %d (%s)", code, string(body))
	}

	// 4. End-to-end HostUpdateExecutor enforces independent ExecutionGrant and blocks operation replay.
	_, ae := setupHostsecAuditDB(t)
	defer ae.Close()
	clientBridge, err := NewHMACHostExecutionBridge(HostBridgeConfig{
		Endpoint:       srv2.URL,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		RequireGrant:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	exec := NewHostUpdateExecutor(debRoot, snapRoot, ae)
	exec.Runner = runner
	exec.GrantAuthority = grantAuth
	exec.SSHProbeFn = func(context.Context, string) error { return nil }
	exec.SetBridge(clientBridge)

	liveBase, _, err := exec.ComputeBaselineHash(context.Background(), []string{"openssh-server"})
	if err != nil {
		t.Fatal(err)
	}
	prop := makeSignedHostUpdateProposal(t, []string{"openssh-server"})
	prop.BaselineHash = liveBase
	prop.ApprovalDigest, _ = model.ComputeProposalApprovalDigest(prop)

	opID := "op-r13-e2e-1"
	e2eGrant, err := grantAuth.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID:        prop.ProposalID,
		ApprovalID:        prop.ApprovalDigest,
		OperationID:       opID,
		BaselineHash:      liveBase,
		FencingGeneration: 4,
		Action:            ActionHostPackageUpgrade,
		Packages:          []string{"openssh-server"},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := exec.Execute(context.Background(), HostUpdateRequest{
		Proposal:             prop,
		OperationID:          opID,
		FencingGeneration:    4,
		ExecutionGrant:       e2eGrant,
		Packages:             []string{"openssh-server"},
		ExpectedBaselineHash: liveBase,
	})
	if err != nil || !res.Success || !res.Verified {
		t.Fatalf("expected HostUpdateExecutor with valid HostExecutionGrant to succeed, got res=%+v err=%v", res, err)
	}
	// Replay of apt.upgrade with same operation grant MUST fail closed.
	if _, err := exec.Execute(context.Background(), HostUpdateRequest{
		Proposal:             prop,
		OperationID:          opID,
		FencingGeneration:    4,
		ExecutionGrant:       e2eGrant,
		Packages:             []string{"openssh-server"},
		ExpectedBaselineHash: liveBase,
	}); err == nil {
		t.Fatalf("expected replay of consumed HostExecutionGrant to fail closed")
	}
}

func TestR13_FI_07_DetachedWatchdog_APICrashAndDockerStopIndependence(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	snapDir := filepath.Join(snapRoot, "snap-r13-detached")
	backupSSH := filepath.Join(snapDir, "etc/ssh/sshd_config")
	if err := os.MkdirAll(filepath.Dir(backupSSH), 0700); err != nil {
		t.Fatal(err)
	}
	origSSH := "Port 22\nPermitRootLogin no\nPasswordAuthentication no\n"
	if err := os.WriteFile(backupSSH, []byte(origSSH), 0600); err != nil {
		t.Fatal(err)
	}
	manifestBytes, _ := json.Marshal(HostUpdateSnapshot{
		SnapshotID:      "snap-r13-detached",
		SnapshotDir:     snapDir,
		PackageVersions: map[string]string{"openssh-server": "1:9.2p1-2+deb12u3"},
		CopiedFiles:     map[string]string{"etc/ssh/sshd_config": backupSSH},
	})
	if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), manifestBytes, 0600); err != nil {
		t.Fatal(err)
	}

	// 1. Direct DetachedProcessWatchdogBackend survives StopLocalTimers (API crash/docker stop) WITHOUT ReconcileExpired.
	guardDir := t.TempDir()
	g := NewPersistentHostSSHGuard(guardDir, debRoot, snapRoot, nil, nil)
	g.DisableLocalTimer = true
	g.ExternalWatchdog = NewDefaultHostWatchdogBackend(guardDir)

	timeout := 120 * time.Millisecond
	waitLimit := 3 * time.Second
	if runtime.GOOS == "windows" {
		timeout = 250 * time.Millisecond
		waitLimit = 8 * time.Second
	}
	gID, err := g.Arm(context.Background(), snapDir, timeout, nil)
	if err != nil {
		t.Fatalf("Detached Arm failed: %v", err)
	}
	// Simulate immediate API / container process death and broken SSH config left behind.
	g.StopLocalTimers()
	if err := os.WriteFile(filepath.Join(debRoot, "etc/ssh/sshd_config"), []byte("BROKEN_SSHD_BY_CRASH\n"), 0644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(waitLimit)
	recovered := false
	for time.Now().Before(deadline) {
		st, lErr := g.LoadState(gID)
		raw, rErr := os.ReadFile(filepath.Join(debRoot, "etc/ssh/sshd_config"))
		if lErr == nil && st.Status == SSHGuardStatusRecovered && rErr == nil && string(raw) == origSSH {
			recovered = true
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if !recovered {
		st, _ := g.LoadState(gID)
		raw, _ := os.ReadFile(filepath.Join(debRoot, "etc/ssh/sshd_config"))
		t.Fatalf("expected detached OS watchdog to autonomously recover without ReconcileExpired: state=%+v sshd_config=%q", st, string(raw))
	}

	// 2. HostBridgeHTTPHandler Watchdog delegation (Docker container API stops, host-side detached watchdog recovers).
	debRoot2 := writeHostFixture(t, false)
	snapRoot2 := t.TempDir()
	snapDir2 := filepath.Join(snapRoot2, "snap-bridge-wd")
	backupSSH2 := filepath.Join(snapDir2, "etc/ssh/sshd_config")
	if err := os.MkdirAll(filepath.Dir(backupSSH2), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupSSH2, []byte(origSSH), 0600); err != nil {
		t.Fatal(err)
	}
	mb2, _ := json.Marshal(HostUpdateSnapshot{
		SnapshotID:      "snap-bridge-wd",
		SnapshotDir:     snapDir2,
		PackageVersions: map[string]string{"openssh-server": "1:9.2p1-2+deb12u3"},
		CopiedFiles:     map[string]string{"etc/ssh/sshd_config": backupSSH2},
	})
	if err := os.WriteFile(filepath.Join(snapDir2, "manifest.json"), mb2, 0600); err != nil {
		t.Fatal(err)
	}
	bridgeKey, _ := r13TestBridgeKeys()
	bh, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot2,
		SnapshotRoot:   snapRoot2,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		Runner:         &mockHostExecRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	bsrv := httptest.NewServer(bh)
	defer bsrv.Close()

	bridgeClient, err := NewHMACHostExecutionBridge(HostBridgeConfig{
		Endpoint:       bsrv.URL,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	containerGuard := NewPersistentHostSSHGuard(t.TempDir(), debRoot2, snapRoot2, nil, bridgeClient)
	containerGuard.DisableLocalTimer = true
	containerGuard.DelegateWatchdogToBridge = true

	gID2, err := containerGuard.ArmWithBinding(context.Background(), snapDir2, timeout, HostExecutionBinding{
		ProposalID: "prop-wd", ApprovalID: "apv-wd", OperationID: "op-wd",
		BaselineHash: "host-v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", FencingGeneration: 1,
	}, nil, nil)
	if err != nil {
		t.Fatalf("bridge ArmWithBinding failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(debRoot2, "etc/ssh/sshd_config"), []byte("CONTAINER_KILLED_CORRUPT\n"), 0644); err != nil {
		t.Fatal(err)
	}

	hostGuardView := NewPersistentHostSSHGuard(filepath.Join(snapRoot2, "ssh-watchdog"), debRoot2, snapRoot2, nil, nil)
	deadline2 := time.Now().Add(waitLimit)
	recovered2 := false
	for time.Now().Before(deadline2) {
		st, lErr := hostGuardView.LoadState(gID2)
		raw, rErr := os.ReadFile(filepath.Join(debRoot2, "etc/ssh/sshd_config"))
		if lErr == nil && st.Status == SSHGuardStatusRecovered && rErr == nil && string(raw) == origSSH {
			recovered2 = true
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if !recovered2 {
		st, _ := hostGuardView.LoadState(gID2)
		raw, _ := os.ReadFile(filepath.Join(debRoot2, "etc/ssh/sshd_config"))
		t.Fatalf("expected bridge-delegated host detached watchdog to recover independently: state=%+v ssh=%q", st, string(raw))
	}
}

func TestR13_FI_08_WatchdogRestartBeforeDeadlineDisarmPreventsRollback(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	snapDir := filepath.Join(snapRoot, "snap-r13-disarm")
	backupSSH := filepath.Join(snapDir, "etc/ssh/sshd_config")
	if err := os.MkdirAll(filepath.Dir(backupSSH), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupSSH, []byte("Port 22\nPermitRootLogin no\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mb, _ := json.Marshal(HostUpdateSnapshot{
		SnapshotID:      "snap-r13-disarm",
		SnapshotDir:     snapDir,
		PackageVersions: map[string]string{"openssh-server": "1:9.2p1-2+deb12u3"},
		CopiedFiles:     map[string]string{"etc/ssh/sshd_config": backupSSH},
	})
	if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), mb, 0600); err != nil {
		t.Fatal(err)
	}

	guardDir := t.TempDir()
	g1 := NewPersistentHostSSHGuard(guardDir, debRoot, snapRoot, nil, nil)
	g1.DisableLocalTimer = true
	g1.ExternalWatchdog = NewDefaultHostWatchdogBackend(guardDir)

	timeout := 700 * time.Millisecond
	if runtime.GOOS == "windows" {
		timeout = 1800 * time.Millisecond
	}
	gID, err := g1.Arm(context.Background(), snapDir, timeout, nil)
	if err != nil {
		t.Fatal(err)
	}
	newSSH := "Port 2222\nPermitRootLogin no\n"
	if err := os.WriteFile(filepath.Join(debRoot, "etc/ssh/sshd_config"), []byte(newSSH), 0644); err != nil {
		t.Fatal(err)
	}

	// Simulate API restart before deadline, and Disarm from new instance.
	g2 := NewPersistentHostSSHGuard(guardDir, debRoot, snapRoot, nil, nil)
	g2.ExternalWatchdog = NewDefaultHostWatchdogBackend(guardDir)
	if err := g2.Disarm(gID); err != nil {
		t.Fatalf("Disarm across restart before deadline failed: %v", err)
	}

	// Wait past original deadline and confirm detached watchdog did NOT roll back valid new config.
	time.Sleep(timeout + 250*time.Millisecond)
	raw, err := os.ReadFile(filepath.Join(debRoot, "etc/ssh/sshd_config"))
	if err != nil || string(raw) != newSSH {
		t.Fatalf("expected disarmed guard not to roll back new SSH config, got %q (err=%v)", string(raw), err)
	}
	st, err := g2.LoadState(gID)
	if err != nil || st.Status != SSHGuardStatusDisarmed {
		t.Fatalf("expected guard status DISARMED, got %+v err=%v", st, err)
	}
	if cnt, rErr := g2.ReconcileExpired(context.Background()); rErr != nil || cnt != 0 {
		t.Fatalf("expected 0 reconciled expired guards after Disarm, got cnt=%d err=%v", cnt, rErr)
	}
}

func TestR13_FI_09_QAReviewRegressions(t *testing.T) {
	bridgeKey, grantKey := r13TestBridgeKeys()
	grantAuth, err := NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         "grant-k1",
		HMACKey:       grantKey,
		BridgeHMACKey: bridgeKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Pinned TargetVersions on grant MUST reject requests omitting req.TargetVersions (403)
	// and MUST NOT consume the grant on failed probes.
	debRoot := writeHostFixture(t, false)
	snapRoot := t.TempDir()
	watchDir := filepath.Join(snapRoot, "ssh-watchdog")
	grantDir := filepath.Join(snapRoot, "grant-store")
	_ = os.MkdirAll(watchDir, 0700)
	_ = os.MkdirAll(grantDir, 0700)

	runner := &mockHostExecRunner{
		handlers: map[string]func(args []string) (string, string, error){
			"apt-cache": func(_ []string) (string, string, error) {
				return "openssh-server:\n  Installed: 1:9.2p1-2+deb12u3\n  Candidate: 1:9.2p1-2+deb12u4\n", "", nil
			},
			"dpkg-query": func(_ []string) (string, string, error) {
				return "install ok installed\t1:9.2p1-2+deb12u4\n", "", nil
			},
		},
	}
	h, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		WatchdogDir:    watchDir,
		GrantDir:       grantDir,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		RequireGrant:   true,
		GrantKeyID:     "grant-k1",
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
		ProposalID: "prop-tv", ApprovalID: "apv-tv", OperationID: "op-tv",
		ResourceScope: "host-os", BaselineHash: base, FencingGeneration: 1,
	}

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

	pinnedGrant, err := grantAuth.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID:        binding.ProposalID,
		ApprovalID:        binding.ApprovalID,
		OperationID:       binding.OperationID,
		ResourceScope:     binding.ResourceScope,
		BaselineHash:      binding.BaselineHash,
		FencingGeneration: binding.FencingGeneration,
		Action:            ActionHostPackageUpgrade,
		Packages:          []string{"openssh-server"},
		TargetVersions:    map[string]string{"openssh-server": "1:9.2p1-2+deb12u4"},
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
		Grant:             pinnedGrant,
	}
	codeArm, bodyArm := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "n-tv-arm", armReq)
	if codeArm != http.StatusOK {
		t.Fatalf("arm watchdog step failed: %d (%s)", codeArm, string(bodyArm))
	}

	codeOmit, bodyOmit := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "n-tv-omit", HostActionRequest{
		Action:   HostActionAptUpgrade,
		Binding:  binding,
		Packages: []string{"openssh-server"},
		Grant:    pinnedGrant,
	})
	if codeOmit != http.StatusForbidden {
		t.Fatalf("expected 403 when req omits pinned target_versions, got %d (%s)", codeOmit, string(bodyOmit))
	}

	codeMismatch, bodyMismatch := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "n-tv-mismatch", HostActionRequest{
		Action:         HostActionAptUpgrade,
		Binding:        binding,
		Packages:       []string{"openssh-server"},
		TargetVersions: map[string]string{"openssh-server": "1:9.2p1-2+deb12u3"},
		Grant:          pinnedGrant,
	})
	if codeMismatch != http.StatusForbidden {
		t.Fatalf("expected 403 when req mismatches pinned target_versions, got %d (%s)", codeMismatch, string(bodyMismatch))
	}

	if len(runner.calls) != 0 {
		t.Fatalf("expected 0 runner calls after rejected target_versions probes, got %v", runner.calls)
	}

	codeMatch, bodyMatch := sendRawBridgeRequest(t, srv.URL, "bridge-k1", bridgeKey, "n-tv-match", HostActionRequest{
		Action:         HostActionAptUpgrade,
		Binding:        binding,
		Packages:       []string{"openssh-server"},
		TargetVersions: map[string]string{"openssh-server": "1:9.2p1-2+deb12u4"},
		Grant:          pinnedGrant,
	})
	if codeMatch != http.StatusOK {
		t.Fatalf("expected 200 with exact pinned target_versions after rejected probes, got %d (%s)", codeMatch, string(bodyMatch))
	}

	// 2. Shared GuardDir == WatchdogDir preserves bridge-written ExternalUnit PID on ArmWithBinding.
	debRoot2 := writeHostFixture(t, false)
	snapRoot2 := t.TempDir()
	snapDir2 := filepath.Join(snapRoot2, "snap-shared-wd")
	backupSSH2 := filepath.Join(snapDir2, "etc/ssh/sshd_config")
	if err := os.MkdirAll(filepath.Dir(backupSSH2), 0700); err != nil {
		t.Fatal(err)
	}
	origSSH := "Port 22\nPermitRootLogin no\n"
	if err := os.WriteFile(backupSSH2, []byte(origSSH), 0600); err != nil {
		t.Fatal(err)
	}
	mb2, _ := json.Marshal(HostUpdateSnapshot{
		SnapshotID:      "snap-shared-wd",
		SnapshotDir:     snapDir2,
		PackageVersions: map[string]string{"openssh-server": "1:9.2p1-2+deb12u3"},
		CopiedFiles:     map[string]string{"etc/ssh/sshd_config": backupSSH2},
	})
	if err := os.WriteFile(filepath.Join(snapDir2, "manifest.json"), mb2, 0600); err != nil {
		t.Fatal(err)
	}

	bh2, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot2,
		SnapshotRoot:   snapRoot2,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		Runner:         &mockHostExecRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv2 := httptest.NewServer(bh2)
	defer srv2.Close()

	bridgeClient2, err := NewHMACHostExecutionBridge(HostBridgeConfig{
		Endpoint:       srv2.URL,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	sharedWatchdogDir := filepath.Join(snapRoot2, "ssh-watchdog")
	sharedGuard := NewPersistentHostSSHGuard(sharedWatchdogDir, debRoot2, snapRoot2, nil, bridgeClient2)
	sharedGuard.DisableLocalTimer = true
	sharedGuard.DelegateWatchdogToBridge = true

	timeout := 1500 * time.Millisecond
	if runtime.GOOS == "windows" {
		timeout = 2200 * time.Millisecond
	}
	gID2, err := sharedGuard.ArmWithBinding(context.Background(), snapDir2, timeout, HostExecutionBinding{
		ProposalID: "prop-shared", ApprovalID: "apv-shared", OperationID: "op-shared",
		BaselineHash: base, FencingGeneration: 1,
	}, nil, nil)
	if err != nil {
		t.Fatalf("shared-dir ArmWithBinding failed: %v", err)
	}
	st2, err := sharedGuard.LoadState(gID2)
	if err != nil {
		t.Fatalf("LoadState(%s) failed: %v", gID2, err)
	}
	if !st2.BridgeDelegated || !strings.HasPrefix(st2.ExternalUnit, "pid:") {
		t.Fatalf("expected BridgeDelegated=true and ExternalUnit=pid:..., got %+v", st2)
	}
	if err := sharedGuard.Disarm(gID2); err != nil {
		t.Fatalf("Disarm(%s) failed: %v", gID2, err)
	}

	// 3. fnOS bridge rejects HostActionWatchdogArm/Disarm BEFORE grant consumption & guard creation.
	fnosRoot := writeHostFixture(t, true)
	fnosSnapRoot := t.TempDir()
	fnosGrantDir := filepath.Join(fnosSnapRoot, "grant-store")
	fnosHandler, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       fnosRoot,
		SnapshotRoot:   fnosSnapRoot,
		GrantDir:       fnosGrantDir,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		RequireGrant:   true,
		GrantKeyID:     "grant-k1",
		GrantHMACKey:   grantKey,
		Runner:         &mockHostExecRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	fnosSrv := httptest.NewServer(fnosHandler)
	defer fnosSrv.Close()

	fnosBinding := HostExecutionBinding{
		ProposalID: "prop-fnos-wd", ApprovalID: "apv-fnos-wd", OperationID: "op-fnos-wd",
		ResourceScope: "host-os", BaselineHash: base, FencingGeneration: 1,
	}
	fnosGrant, err := grantAuth.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID:        fnosBinding.ProposalID,
		ApprovalID:        fnosBinding.ApprovalID,
		OperationID:       fnosBinding.OperationID,
		ResourceScope:     fnosBinding.ResourceScope,
		BaselineHash:      fnosBinding.BaselineHash,
		FencingGeneration: fnosBinding.FencingGeneration,
		Action:            string(HostActionWatchdogArm),
	})
	if err != nil {
		t.Fatal(err)
	}
	codeFnArm, bodyFnArm := sendRawBridgeRequest(t, fnosSrv.URL, "bridge-k1", bridgeKey, "n-fnos-wd-arm", HostActionRequest{
		Action:            HostActionWatchdogArm,
		Binding:           fnosBinding,
		GuardID:           "guard-fnos-1",
		SnapshotID:        "snap-unused",
		WatchdogTimeoutMs: 30000,
		Grant:             fnosGrant,
	})
	if codeFnArm != http.StatusConflict || !strings.Contains(string(bodyFnArm), "fnOS") {
		t.Fatalf("expected 409 Conflict mentioning fnOS on watchdog_arm, got %d (%s)", codeFnArm, string(bodyFnArm))
	}
	if _, statErr := os.Stat(filepath.Join(fnosSnapRoot, "ssh-watchdog", "guard-fnos-1.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected no guard state file created on fnOS block, got statErr=%v", statErr)
	}

	// Prove fnosGrant was NOT consumed by the blocked fnOS request by verifying a non-fnOS bridge
	// sharing the same GrantDir can still authorize and consume fnosGrant!
	debSnapRoot3 := t.TempDir()
	debSnapID := "snap-after-fnos"
	debSnapDir := filepath.Join(debSnapRoot3, debSnapID)
	if err := os.MkdirAll(filepath.Join(debSnapDir, "etc/ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(debSnapDir, "etc/ssh/sshd_config"), []byte(origSSH), 0600); err != nil {
		t.Fatal(err)
	}
	debMB, _ := json.Marshal(HostUpdateSnapshot{
		SnapshotID:  debSnapID,
		SnapshotDir: debSnapDir,
		CopiedFiles: map[string]string{"etc/ssh/sshd_config": filepath.Join(debSnapDir, "etc/ssh/sshd_config")},
	})
	if err := os.WriteFile(filepath.Join(debSnapDir, "manifest.json"), debMB, 0600); err != nil {
		t.Fatal(err)
	}
	debHandler, err := NewHostBridgeHTTPHandler(HostBridgeServerConfig{
		HostRoot:       debRoot2,
		SnapshotRoot:   debSnapRoot3,
		GrantDir:       fnosGrantDir,
		KeyID:          "bridge-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		RequireGrant:   true,
		GrantKeyID:     "grant-k1",
		GrantHMACKey:   grantKey,
		Runner:         &mockHostExecRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	debSrv := httptest.NewServer(debHandler)
	defer debSrv.Close()

	codeDebArm, bodyDebArm := sendRawBridgeRequest(t, debSrv.URL, "bridge-k1", bridgeKey, "n-deb-reuse-fnos-grant", HostActionRequest{
		Action:            HostActionWatchdogArm,
		Binding:           fnosBinding,
		GuardID:           "guard-deb-after-fnos",
		SnapshotID:        debSnapID,
		WatchdogTimeoutMs: int64(timeout / time.Millisecond),
		Grant:             fnosGrant,
	})
	if codeDebArm != http.StatusOK {
		t.Fatalf("expected fnOS-rejected grant to remain unconsumed and succeed on Debian bridge (200), got %d (%s)", codeDebArm, string(bodyDebArm))
	}
	_ = NewPersistentHostSSHGuard(filepath.Join(debSnapRoot3, "ssh-watchdog"), debRoot2, debSnapRoot3, nil, nil).Disarm("guard-deb-after-fnos")

	fnosDisarmGrant, err := grantAuth.IssueGrant(HostExecutionGrantIssueRequest{
		ProposalID:        fnosBinding.ProposalID,
		ApprovalID:        fnosBinding.ApprovalID,
		OperationID:       fnosBinding.OperationID,
		ResourceScope:     fnosBinding.ResourceScope,
		BaselineHash:      fnosBinding.BaselineHash,
		FencingGeneration: fnosBinding.FencingGeneration,
		Action:            string(HostActionWatchdogDisarm),
	})
	if err != nil {
		t.Fatal(err)
	}
	codeFnDisarm, bodyFnDisarm := sendRawBridgeRequest(t, fnosSrv.URL, "bridge-k1", bridgeKey, "n-fnos-wd-disarm", HostActionRequest{
		Action:  HostActionWatchdogDisarm,
		Binding: fnosBinding,
		GuardID: "guard-fnos-1",
		Grant:   fnosDisarmGrant,
	})
	if codeFnDisarm != http.StatusConflict || !strings.Contains(string(bodyFnDisarm), "fnOS") {
		t.Fatalf("expected 409 Conflict mentioning fnOS on watchdog_disarm, got %d (%s)", codeFnDisarm, string(bodyFnDisarm))
	}
}
