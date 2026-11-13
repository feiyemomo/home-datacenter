package main

import (
	"context"
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"home-datacenter-api/internal/hostsec"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n[-] Validation failed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fmt.Println("================================================================")
	fmt.Println(" Home Datacenter R13 Stage 4: Real fnOS Host Live Validation")
	fmt.Println("================================================================")

	flagSockPath := flag.String("socket", "/run/home-datacenter/host-bridge.sock", "Path to host bridge unix socket")
	flagBridgeKey := flag.String("bridge-key-hex", "", "Hex-encoded bridge HMAC key (or via FNOS_TEST_BRIDGE_KEY_HEX)")
	flagGrantKey := flag.String("grant-key-hex", "", "Hex-encoded independent grant key (or via FNOS_TEST_GRANT_KEY_HEX)")
	flag.Parse()

	sockPath := filepath.Clean(*flagSockPath)
	bridgeKeyHex := strings.TrimSpace(*flagBridgeKey)
	if bridgeKeyHex == "" {
		bridgeKeyHex = strings.TrimSpace(os.Getenv("FNOS_TEST_BRIDGE_KEY_HEX"))
	}
	grantKeyHex := strings.TrimSpace(*flagGrantKey)
	if grantKeyHex == "" {
		grantKeyHex = strings.TrimSpace(os.Getenv("FNOS_TEST_GRANT_KEY_HEX"))
	}

	if len(bridgeKeyHex) < 64 || len(grantKeyHex) < 64 {
		return fmt.Errorf("FAIL-CLOSED: Test verification keys not provided or too short.\n" +
			"Provide non-default test keys via flags (-bridge-key-hex, -grant-key-hex) or env vars (FNOS_TEST_BRIDGE_KEY_HEX, FNOS_TEST_GRANT_KEY_HEX).\n" +
			"Keys must be independent and at least 32 bytes (64 hex characters).")
	}

	bridgeKey, bErr := hex.DecodeString(bridgeKeyHex)
	if bErr != nil {
		return fmt.Errorf("failed to hex decode bridge key: %w", bErr)
	}
	grantKey, gErr := hex.DecodeString(grantKeyHex)
	if gErr != nil {
		return fmt.Errorf("failed to hex decode grant key: %w", gErr)
	}

	if hmac.Equal(bridgeKey, grantKey) {
		return fmt.Errorf("SECURITY VIOLATION: Bridge key and Grant key must NOT be identical (key conflict)")
	}

	// Check 1: Socket & Ancestry Permissions
	fmt.Println("\n[Check 1] Auditing Socket & Ancestry Directory Permissions on fnOS...")
	if err := hostsec.AuditSocketAncestry(sockPath); err != nil {
		return fmt.Errorf("SECURITY VIOLATION: Socket ancestry failed audit: %w", err)
	}
	fi, err := os.Lstat(sockPath)
	if err != nil {
		return fmt.Errorf("failed to stat socket: %w", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s is not a unix domain socket", sockPath)
	}
	perm := fi.Mode().Perm()
	fmt.Printf("[+] Socket mode: %04o\n", perm)
	if perm&0002 != 0 {
		return fmt.Errorf("SECURITY VIOLATION: Socket is world-writable (mode %04o)", perm)
	}
	fmt.Println("[PASS] Check 1: Socket and complete directory ancestry passed security audit.")

	// Check 2: Negative test - Missing Grant rejected with 403
	fmt.Println("\n[Check 2] Testing Negative Authorization Gate (Missing Grant)...")
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
		Timeout: 5 * time.Second,
	}

	nowUnix := time.Now().Unix()
	binding := hostsec.HostExecutionBinding{
		ProposalID:        fmt.Sprintf("prop-live-fnos-%d", nowUnix),
		ApprovalID:        fmt.Sprintf("apv-live-fnos-%d", nowUnix),
		OperationID:       fmt.Sprintf("op-live-fnos-%d", nowUnix),
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:live-probe",
		FencingGeneration: nowUnix,
	}

	// 1. Missing Grant
	reqMissingGrant := hostsec.HostActionRequest{
		Action:   hostsec.HostActionAptUpgrade,
		Binding:  binding,
		Packages: []string{"openssh-server"},
	}
	code2, body2, err := sendBridgeRequest(client, "bridge-default-key", bridgeKey, "nonce-live-chk2", reqMissingGrant)
	if err != nil {
		return fmt.Errorf("bridge request failed: %w", err)
	}
	fmt.Printf("[+] Bridge response for missing grant: HTTP %d, body=%s\n", code2, body2)
	if code2 != http.StatusForbidden {
		return fmt.Errorf("expected HTTP 403 Forbidden, got %d", code2)
	}
	fmt.Println("[PASS] Check 2: Missing Grant rejected fail-closed with 403 Forbidden.")

	// Check 3: Platform Policy Hard Enforcement - fnOS 409 Conflict
	// Platform policy is evaluated at the very front of the bridge, hard-blocking any mutation-lifecycle step on fnOS.
	fmt.Println("\n[Check 3] Testing Platform Policy Hard Enforcement (fnOS 409 Conflict)...")
	grantAuth, err := hostsec.NewHostGrantAuthority(hostsec.HostGrantAuthorityConfig{
		KeyID:         "grant-k1",
		HMACKey:       grantKey,
		BridgeHMACKey: bridgeKey,
	})
	if err != nil {
		return fmt.Errorf("create grant authority failed: %w", err)
	}

	grant, err := grantAuth.IssueGrant(hostsec.HostExecutionGrantIssueRequest{
		ProposalID:        binding.ProposalID,
		ApprovalID:        binding.ApprovalID,
		OperationID:       binding.OperationID,
		ResourceScope:     binding.ResourceScope,
		BaselineHash:      binding.BaselineHash,
		FencingGeneration: binding.FencingGeneration,
		Action:            hostsec.ActionHostPackageUpgrade,
		Packages:          []string{"openssh-server"},
	})
	if err != nil {
		return fmt.Errorf("issue grant failed: %w", err)
	}

	liveSnapDir := filepath.Join("/var/lib/home-datacenter/snapshots", binding.OperationID)
	if err := os.MkdirAll(liveSnapDir, 0700); err != nil {
		return fmt.Errorf("create live snapshot dir: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(liveSnapDir)
	}()
	if err := os.WriteFile(filepath.Join(liveSnapDir, "manifest.json"), []byte(`{"snapshot_id":"`+binding.OperationID+`"}`), 0600); err != nil {
		return fmt.Errorf("write live snapshot manifest: %w", err)
	}

	// Send mutation request with valid grant -> Must be intercepted by Platform Policy with HTTP 409
	armReq := hostsec.HostActionRequest{
		Action:            hostsec.HostActionWatchdogArm,
		Binding:           binding,
		GuardID:           binding.OperationID,
		SnapshotID:        binding.OperationID,
		WatchdogTimeoutMs: 30000,
		Grant:             grant,
	}
	code3, body3, err := sendBridgeRequest(client, "bridge-default-key", bridgeKey, "nonce-live-chk3", armReq)
	if err != nil {
		return fmt.Errorf("bridge request with grant failed: %w", err)
	}
	fmt.Printf("[+] Bridge response for fnOS mutation request: HTTP %d, body=%s\n", code3, body3)
	if code3 != http.StatusConflict {
		return fmt.Errorf("expected HTTP 409 Conflict (ErrFnOSUpdateUnsupported), got %d", code3)
	}
	if !strings.Contains(body3, "host package mutation blocked on fnOS") {
		return fmt.Errorf("unexpected error message: %s", body3)
	}
	fmt.Println("[PASS] Check 3: fnOS platform policy hard-blocked mutation with 409 Conflict (zero commands dispatched to host).")

	// Check 4: Host SSH Guard / Watchdog live test on fnOS
	fmt.Println("\n[Check 4] Testing Host SSH Guard & Watchdog on fnOS host...")
	snapDir := filepath.Join("/var/lib/home-datacenter/snapshots", "live-test-snap")
	if err := os.MkdirAll(filepath.Join(snapDir, "etc/ssh"), 0700); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(snapDir)
	}()
	if err := os.WriteFile(filepath.Join(snapDir, "etc/ssh/sshd_config"), []byte("# live test backup\n"), 0600); err != nil {
		return fmt.Errorf("write test sshd_config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), []byte(`{"snapshot_id":"live-test-snap","created_at":"`+time.Now().Format(time.RFC3339)+`"}`), 0600); err != nil {
		return fmt.Errorf("write test snapshot manifest: %w", err)
	}

	watchDir := filepath.Join("/var/lib/home-datacenter/snapshots", "ssh-watchdog")
	guard := hostsec.NewPersistentHostSSHGuard(watchDir, "/", "/var/lib/home-datacenter/snapshots", nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	guardID := "guard-live-fnos"
	defer func() {
		_ = os.Remove(filepath.Join(watchDir, guardID+".json"))
		_ = os.Remove(filepath.Join(watchDir, guardID+".disarmed"))
		_ = os.Remove(filepath.Join(watchDir, guardID+"-watchdog.sh"))
		_ = os.Remove(filepath.Join(watchDir, guardID+"-recovery.log"))
	}()

	gID, err := guard.ArmWithBinding(ctx, "live-test-snap", 60*time.Second, hostsec.HostExecutionBinding{
		ProposalID:        "prop-live-fnos-1",
		ApprovalID:        "apv-live-fnos-1",
		OperationID:       guardID,
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:live-probe",
		FencingGeneration: nowUnix,
	}, nil, nil)
	if err != nil {
		return fmt.Errorf("watchdog ArmWithBinding failed: %w", err)
	}
	fmt.Printf("[+] Armed watchdog unit: %s\n", gID)

	// Verify state file on disk
	st, err := guard.LoadState(guardID)
	if err != nil || st.Status != hostsec.SSHGuardStatusArmed {
		return fmt.Errorf("failed to load armed state: %+v err=%v", st, err)
	}
	fmt.Printf("[+] Watchdog state persisted: Status=%s, ExternalUnit=%s\n", st.Status, st.ExternalUnit)

	// Disarm watchdog
	if err := guard.Disarm(guardID); err != nil {
		return fmt.Errorf("watchdog Disarm failed: %w", err)
	}
	stDisarmed, dErr := guard.LoadState(guardID)
	if dErr != nil {
		return fmt.Errorf("failed to load disarmed state: %w", dErr)
	}
	if stDisarmed.Status != hostsec.SSHGuardStatusDisarmed {
		return fmt.Errorf("expected Status DISARMED, got %s", stDisarmed.Status)
	}
	fmt.Printf("[+] Watchdog safely disarmed: Status=%s\n", stDisarmed.Status)

	// Verify detached watchdog process has cleaned up / is dead or recycled
	if strings.HasPrefix(st.ExternalUnit, "pid:") {
		parts := strings.Split(strings.TrimPrefix(st.ExternalUnit, "pid:"), ":")
		if pid, pErr := strconv.Atoi(parts[0]); pErr == nil && pid > 0 {
			deadline := time.Now().Add(1 * time.Second)
			terminated := false
			for time.Now().Before(deadline) {
				if hostsec.IsProcessRecycledOrDead(pid, guardID) {
					terminated = true
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if !terminated {
				return fmt.Errorf("SECURITY VIOLATION: watchdog process (PID %d) failed to terminate within deadline", pid)
			}
			fmt.Printf("[+] Verified watchdog process (PID %d) terminated cleanly\n", pid)
		}
	}

	fmt.Println("[PASS] Check 4: Host SSH Guard and Detached Watchdog operated cleanly on fnOS.")

	fmt.Println("\n================================================================")
	fmt.Println(" ALL 4 fnOS REAL HOST CHECKS PASSED SUCCESSFULLY!")
	fmt.Println("================================================================")
	return nil
}

func sendBridgeRequest(client *http.Client, keyID string, key []byte, noncePrefix string, req hostsec.HostActionRequest) (int, string, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return 0, "", err
	}
	ts := fmt.Sprintf("%d", time.Now().UTC().UnixMilli())
	nonce := fmt.Sprintf("%s-%d", noncePrefix, time.Now().UnixNano())
	sig := hostsec.ComputeBridgeRequestSignature(key, http.MethodPost, "/v1/host/actions", ts, nonce, raw)

	httpReq, err := http.NewRequest(http.MethodPost, "http://localhost/v1/host/actions", strings.NewReader(string(raw)))
	if err != nil {
		return 0, "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Bridge-Key-Id", keyID)
	httpReq.Header.Set("X-Bridge-Timestamp", ts)
	httpReq.Header.Set("X-Bridge-Nonce", nonce)
	httpReq.Header.Set("X-Bridge-Signature", "hmac-sha256="+sig)

	resp, err := client.Do(httpReq)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}
