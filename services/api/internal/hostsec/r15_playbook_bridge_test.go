package hostsec

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

type mockPlaybookRunner struct {
	calls []string
	err   error
	out   string
}

func (m *mockPlaybookRunner) Run(ctx context.Context, env []string, name string, args ...string) (string, string, error) {
	cmd := name + " " + strings.Join(args, " ")
	m.calls = append(m.calls, cmd)
	return m.out, "", m.err
}

func TestHostBridge_Playbook_PB01_ContainerRestart(t *testing.T) {
	runner := &mockPlaybookRunner{out: "restarted"}
	req := HostActionRequest{
		Action:         HostActionContainerRestart,
		ServiceName:    "home-vision",
		TimeoutSeconds: 15,
		Binding: HostExecutionBinding{
			ProposalID:        "prop-1",
			ApprovalID:        "app-1",
			OperationID:       "op-1",
			BaselineHash:      "host-v1:test-baseline",
			FencingGeneration: 1,
		},
	}

	// 1. Validation test: Whitelisted service succeeds
	if err := ValidateHostActionRequest(req, nil, true); err != nil {
		t.Fatalf("validation failed for valid container restart: %v", err)
	}

	// 2. Execution test
	res, err := dispatchTypedBridgeAction(context.Background(), runner, nil, "/tmp", req)
	if err != nil {
		t.Fatalf("dispatch container restart error: %v", err)
	}
	if !strings.Contains(res.Stdout, "restarted") {
		t.Fatalf("expected output, got: %v", res.Stdout)
	}
	if len(runner.calls) != 1 || runner.calls[0] != "docker restart -t 15 home-vision" {
		t.Fatalf("unexpected runner call: %v", runner.calls)
	}

	// 3. Unauthorized service rejected
	reqBad := req
	reqBad.ServiceName = "malicious-container"
	if err := ValidateHostActionRequest(reqBad, nil, true); !errors.Is(err, ErrBridgeActionRejected) {
		t.Fatalf("expected ErrBridgeActionRejected for unauthorized service, got: %v", err)
	}
}

func TestHostBridge_Playbook_PB02_ZombieReap(t *testing.T) {
	runner := &mockPlaybookRunner{out: "killed"}
	req := HostActionRequest{
		Action:      HostActionZombieReap,
		ServiceName: "home-vision",
		TargetPID:   1234,
		Binding: HostExecutionBinding{
			ProposalID:        "prop-1",
			ApprovalID:        "app-1",
			OperationID:       "op-1",
			BaselineHash:      "host-v1:test-baseline",
			FencingGeneration: 1,
		},
	}

	// 1. Validation test
	if err := ValidateHostActionRequest(req, nil, true); err != nil {
		t.Fatalf("validation failed for valid zombie reap: %v", err)
	}

	// 2. Reject PID 1
	reqPID1 := req
	reqPID1.TargetPID = 1
	if err := ValidateHostActionRequest(reqPID1, nil, true); !errors.Is(err, ErrBridgeActionRejected) {
		t.Fatalf("expected ErrBridgeActionRejected for PID 1, got: %v", err)
	}

	// 3. Execution test
	res, err := dispatchTypedBridgeAction(context.Background(), runner, nil, "/tmp", req)
	if err != nil {
		t.Fatalf("dispatch zombie reap error: %v", err)
	}
	if !strings.Contains(res.Stdout, "killed") {
		t.Fatalf("expected output, got: %v", res.Stdout)
	}
	if len(runner.calls) != 1 || runner.calls[0] != "kill -9 1234" {
		t.Fatalf("unexpected runner call: %v", runner.calls)
	}

	// 4. Cgroup Fail-Closed and Strict Matching Verification
	origCgroupReader := readProcessCgroup
	defer func() { readProcessCgroup = origCgroupReader }()

	// Test A: Fail-Closed on cgroup read error
	readProcessCgroup = func(pid int) ([]byte, error) {
		return nil, errors.New("permission denied")
	}
	// Temporarily simulate Linux environment test
	if _, err := dispatchTypedBridgeAction(context.Background(), runner, nil, "/tmp", req); runtime.GOOS == "linux" && !errors.Is(err, ErrBridgeActionRejected) {
		t.Fatalf("expected ErrBridgeActionRejected on Linux when cgroup read fails, got: %v", err)
	}

	// Test B: Generic 'docker' string in cgroup should NOT match unrelated container
	readProcessCgroup = func(pid int) ([]byte, error) {
		return []byte("0::/system.slice/docker-unrelated123.scope\n"), nil
	}
	mockInspectRunner := &mockPlaybookRunner{out: "targetcontainer456789"}
	if _, err := dispatchTypedBridgeAction(context.Background(), mockInspectRunner, nil, "/tmp", req); runtime.GOOS == "linux" && !errors.Is(err, ErrBridgeActionRejected) {
		t.Fatalf("expected ErrBridgeActionRejected when container ID does not match, got: %v", err)
	}

	// Test C: Matching cgroups v2 scope succeeds
	readProcessCgroup = func(pid int) ([]byte, error) {
		return []byte("0::/system.slice/docker-targetcontainer456789.scope\n"), nil
	}
	mockInspectRunnerMatch := &mockPlaybookRunner{out: "targetcontainer456789"}
	if _, err := dispatchTypedBridgeAction(context.Background(), mockInspectRunnerMatch, nil, "/tmp", req); err != nil && runtime.GOOS == "linux" {
		t.Fatalf("expected matching cgroup v2 scope to succeed, got: %v", err)
	}
}

func TestHostBridge_Playbook_PB03_NetworkReset(t *testing.T) {
	// 1. WAN check failure halts mutation
	wanFailRunner := &mockPlaybookRunner{err: errors.New("ping timeout")}
	reqWan := HostActionRequest{
		Action:     HostActionNetworkReset,
		ResetScope: "wan_check",
		Binding: HostExecutionBinding{
			ProposalID:        "prop-1",
			ApprovalID:        "app-1",
			OperationID:       "op-1",
			BaselineHash:      "host-v1:test-baseline",
			FencingGeneration: 1,
		},
	}

	if err := ValidateHostActionRequest(reqWan, nil, true); err != nil {
		t.Fatalf("validate wan_check: %v", err)
	}

	_, err := dispatchTypedBridgeAction(context.Background(), wanFailRunner, nil, "/tmp", reqWan)
	if err == nil || !strings.Contains(err.Error(), "WAN connectivity check failed") {
		t.Fatalf("expected WAN connectivity failure to stop mutation, got: %v", err)
	}

	// 2. DNS flush and Proxy reload
	dnsRunner := &mockPlaybookRunner{out: "flushed"}
	reqDNS := reqWan
	reqDNS.ResetScope = "dns_flush"
	res, err := dispatchTypedBridgeAction(context.Background(), dnsRunner, nil, "/tmp", reqDNS)
	if err != nil {
		t.Fatalf("dispatch dns_flush: %v", err)
	}
	if res.Stdout != "flushed" {
		t.Fatalf("expected flushed output, got: %v", res.Stdout)
	}

	proxyRunner := &mockPlaybookRunner{out: "proxy_restarted"}
	reqProxy := reqWan
	reqProxy.ResetScope = "proxy_reload"
	resProxy, err := dispatchTypedBridgeAction(context.Background(), proxyRunner, nil, "/tmp", reqProxy)
	if err != nil {
		t.Fatalf("dispatch proxy_reload: %v", err)
	}
	if resProxy.Stdout != "proxy_restarted" {
		t.Fatalf("expected proxy_restarted output, got: %v", resProxy.Stdout)
	}

	// 3. Reject invalid scope
	reqBadScope := reqWan
	reqBadScope.ResetScope = "arbitrary_iptables_flush"
	if err := ValidateHostActionRequest(reqBadScope, nil, true); !errors.Is(err, ErrBridgeActionRejected) {
		t.Fatalf("expected ErrBridgeActionRejected for invalid reset_scope, got: %v", err)
	}
}

type mockDegradedRunner struct {
	calls []string
}

func (m *mockDegradedRunner) Run(ctx context.Context, env []string, name string, args ...string) (string, string, error) {
	cmd := name + " " + strings.Join(args, " ")
	m.calls = append(m.calls, cmd)
	if strings.Contains(cmd, "127.0.0.1") {
		return "1 packets transmitted, 1 received", "", nil // LAN/loopback up!
	}
	if strings.Contains(cmd, "223.5.5.5") {
		return "", "ping: connect: Network is unreachable", errors.New("ping timeout") // WAN DNS down
	}
	return "ok", "", nil
}

func TestHostBridge_Playbook_PB03_NetworkReset_DegradedPass(t *testing.T) {
	runner := &mockDegradedRunner{}
	req := HostActionRequest{
		Action:     HostActionNetworkReset,
		ResetScope: "wan_check",
		Binding: HostExecutionBinding{
			ProposalID:        "prop-1",
			ApprovalID:        "app-1",
			OperationID:       "op-1",
			BaselineHash:      "host-v1:test-baseline",
			FencingGeneration: 1,
		},
	}

	res, err := dispatchTypedBridgeAction(context.Background(), runner, nil, "/tmp", req)
	if err != nil {
		t.Fatalf("expected nil error on degraded WAN (since local stack is up), got: %v", err)
	}
	if !strings.Contains(res.Stdout, "LAN_UP_WAN_DEGRADED") {
		t.Fatalf("expected LAN_UP_WAN_DEGRADED in stdout, got: %s", res.Stdout)
	}
}

func TestHostBridge_Playbook_PB04_OrphanPrune(t *testing.T) {
	runner := &mockPlaybookRunner{out: "removed"}
	req := HostActionRequest{
		Action:      HostActionOrphanPrune,
		PruneFilter: "hdc-tmp-container-123",
		Binding: HostExecutionBinding{
			ProposalID:        "prop-1",
			ApprovalID:        "app-1",
			OperationID:       "op-1",
			BaselineHash:      "host-v1:test-baseline",
			FencingGeneration: 1,
		},
	}

	// 1. Whitelisted temporary prefix succeeds
	if err := ValidateHostActionRequest(req, nil, true); err != nil {
		t.Fatalf("validate orphan prune: %v", err)
	}

	res, err := dispatchTypedBridgeAction(context.Background(), runner, nil, "/tmp", req)
	if err != nil {
		t.Fatalf("dispatch orphan prune: %v", err)
	}
	if !strings.Contains(res.Stdout, "removed") {
		t.Fatalf("expected removed output, got: %v", res.Stdout)
	}
	if len(runner.calls) != 1 || runner.calls[0] != "docker rm -f hdc-tmp-container-123" {
		t.Fatalf("unexpected runner call: %v", runner.calls)
	}

	// 2. Dangerous wildcard or production container rejected
	reqDangerous := req
	reqDangerous.PruneFilter = "home-api"
	if err := ValidateHostActionRequest(reqDangerous, nil, true); !errors.Is(err, ErrBridgeActionRejected) {
		t.Fatalf("expected ErrBridgeActionRejected for non-tmp container filter, got: %v", err)
	}

	reqWildcard := req
	reqWildcard.PruneFilter = "*"
	if err := ValidateHostActionRequest(reqWildcard, nil, true); !errors.Is(err, ErrBridgeActionRejected) {
		t.Fatalf("expected ErrBridgeActionRejected for wildcard, got: %v", err)
	}
}
