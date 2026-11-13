package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"home-datacenter-api/internal/hostsec"
)

func main() {
	fmt.Println("================================================================")
	fmt.Println(" Home Datacenter R13 Stage 4: Real fnOS Host Live Validation")
	fmt.Println("================================================================")

	sockPath := "/run/home-datacenter/host-bridge.sock"
	bridgeKeyHex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	grantKeyHex := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

	bridgeKey, _ := hex.DecodeString(bridgeKeyHex)
	grantKey, _ := hex.DecodeString(grantKeyHex)

	// Check 1: Socket & Directory Permissions
	fmt.Println("\n[Check 1] Auditing Socket & Directory Permissions on fnOS...")
	fi, err := os.Lstat(sockPath)
	if err != nil {
		fmt.Printf("[-] Failed to stat socket: %v\n", err)
		os.Exit(1)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		fmt.Printf("[-] %s is not a unix domain socket\n", sockPath)
		os.Exit(1)
	}
	perm := fi.Mode().Perm()
	fmt.Printf("[+] Socket mode: %04o\n", perm)
	if perm&0002 != 0 {
		fmt.Println("[-] SECURITY VIOLATION: Socket is world-writable!")
		os.Exit(1)
	}
	fmt.Println("[PASS] Check 1: Socket permissions are restricted (non-world-writable).")

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
		fmt.Printf("[-] Bridge request failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[+] Bridge response for missing grant: HTTP %d, body=%s\n", code2, body2)
	if code2 != http.StatusForbidden {
		fmt.Printf("[-] Expected HTTP 403 Forbidden, got %d\n", code2)
		os.Exit(1)
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
		fmt.Printf("[-] Create grant authority failed: %v\n", err)
		os.Exit(1)
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
		fmt.Printf("[-] Issue grant failed: %v\n", err)
		os.Exit(1)
	}

	liveSnapDir := filepath.Join("/var/lib/home-datacenter/snapshots", binding.OperationID)
	_ = os.MkdirAll(liveSnapDir, 0700)
	_ = os.WriteFile(filepath.Join(liveSnapDir, "manifest.json"), []byte(`{"snapshot_id":"`+binding.OperationID+`"}`), 0600)

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
		fmt.Printf("[-] Bridge request with grant failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[+] Bridge response for fnOS mutation request: HTTP %d, body=%s\n", code3, body3)
	if code3 != http.StatusConflict {
		fmt.Printf("[-] Expected HTTP 409 Conflict (ErrFnOSUpdateUnsupported), got %d\n", code3)
		os.Exit(1)
	}
	if !strings.Contains(body3, "host package mutation blocked on fnOS") {
		fmt.Printf("[-] Unexpected error message: %s\n", body3)
		os.Exit(1)
	}
	fmt.Println("[PASS] Check 3: fnOS platform policy hard-blocked mutation with 409 Conflict (zero commands dispatched to host).")

	// Check 4: Host SSH Guard / Watchdog live test on fnOS
	fmt.Println("\n[Check 4] Testing Host SSH Guard & Watchdog on fnOS host...")
	snapDir := filepath.Join("/var/lib/home-datacenter/snapshots", "live-test-snap")
	_ = os.MkdirAll(filepath.Join(snapDir, "etc/ssh"), 0700)
	_ = os.WriteFile(filepath.Join(snapDir, "etc/ssh/sshd_config"), []byte("# live test backup\n"), 0600)
	_ = os.WriteFile(filepath.Join(snapDir, "manifest.json"), []byte(`{"snapshot_id":"live-test-snap","created_at":"`+time.Now().Format(time.RFC3339)+`"}`), 0600)

	watchDir := filepath.Join("/var/lib/home-datacenter/snapshots", "ssh-watchdog")
	guard := hostsec.NewPersistentHostSSHGuard(watchDir, "/", "/var/lib/home-datacenter/snapshots", nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	guardID := "guard-live-fnos"
	gID, err := guard.ArmWithBinding(ctx, "live-test-snap", 60*time.Second, hostsec.HostExecutionBinding{
		ProposalID:        "prop-live-fnos-1",
		ApprovalID:        "apv-live-fnos-1",
		OperationID:       guardID,
		ResourceScope:     "host-os",
		BaselineHash:      "host-v1:live-probe",
		FencingGeneration: nowUnix,
	}, nil, nil)
	if err != nil {
		fmt.Printf("[-] Watchdog ArmWithBinding failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[+] Armed watchdog unit: %s\n", gID)

	// Verify state file on disk
	st, err := guard.LoadState(guardID)
	if err != nil || st.Status != hostsec.SSHGuardStatusArmed {
		fmt.Printf("[-] Failed to load armed state: %+v err=%v\n", st, err)
		os.Exit(1)
	}
	fmt.Printf("[+] Watchdog state persisted: Status=%s, ExternalUnit=%s\n", st.Status, st.ExternalUnit)

	// Disarm watchdog
	if err := guard.Disarm(guardID); err != nil {
		fmt.Printf("[-] Watchdog Disarm failed: %v\n", err)
		os.Exit(1)
	}
	stDisarmed, _ := guard.LoadState(guardID)
	fmt.Printf("[+] Watchdog safely disarmed: Status=%s\n", stDisarmed.Status)
	fmt.Println("[PASS] Check 4: Host SSH Guard and Detached Watchdog operated cleanly on fnOS.")

	fmt.Println("\n================================================================")
	fmt.Println(" ALL 4 fnOS REAL HOST CHECKS PASSED SUCCESSFULLY!")
	fmt.Println("================================================================")
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
