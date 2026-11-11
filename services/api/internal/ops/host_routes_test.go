package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/hostsec"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/security"
	"home-datacenter-api/internal/utils"
)

type stubHostROCmdRunner struct {
	mu        sync.Mutex
	responses map[string]struct {
		out string
		err string
		e   error
	}
}

func (r *stubHostROCmdRunner) Run(_ context.Context, name string, args ...string) (string, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := name + " " + strings.Join(args, " ")
	if resp, ok := r.responses[key]; ok {
		return resp.out, resp.err, resp.e
	}
	return "", "", nil
}

type stubHostExecRunner struct {
	mu       sync.Mutex
	calls    []string
	handlers map[string]func(args []string) (string, string, error)
}

func (r *stubHostExecRunner) Run(_ context.Context, _ []string, name string, args ...string) (string, string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	fn := r.handlers[name]
	r.mu.Unlock()
	if fn != nil {
		return fn(args)
	}
	return "", "", nil
}

func writeOpsHostFixture(t *testing.T, fnos bool) string {
	t.Helper()
	root := t.TempDir()
	mustWrite := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	mustWrite("etc/hostname", "nas-prod\n")
	if fnos {
		mustWrite("proc/sys/kernel/osrelease", "6.6.38-trim\n")
		mustWrite("etc/os-release", "PRETTY_NAME=\"fnOS 0.8.36 (trim)\"\nID=trim\nID_LIKE=debian\nVERSION_ID=\"12\"\nVERSION_CODENAME=bookworm\n")
		mustWrite("etc/trim-release", "0.8.36\n")
	} else {
		mustWrite("proc/sys/kernel/osrelease", "6.1.0-26-amd64\n")
		mustWrite("etc/os-release", "PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nID=debian\nVERSION_ID=\"12\"\nVERSION_CODENAME=bookworm\n")
	}
	mustWrite("proc/sys/kernel/arch", "x86_64\n")
	mustWrite("etc/ssh/sshd_config", "Port 22\nPermitRootLogin no\nPasswordAuthentication no\n")
	mustWrite("etc/apt/sources.list", "deb https://deb.debian.org/debian bookworm main\n")
	mustWrite("var/lib/dpkg/status", strings.Join([]string{
		"Package: openssh-server",
		"Status: install ok installed",
		"Architecture: amd64",
		"Source: openssh",
		"Version: 1:9.2p1-2+deb12u2",
		"",
		"Package: libssl3",
		"Status: install ok installed",
		"Architecture: amd64",
		"Source: openssl",
		"Version: 3.0.11-1~deb12u2",
		"",
	}, "\n"))
	mustWrite("etc/passwd", "root:x:0:0:root:/root:/bin/bash\nmomo:x:1000:1000::/home/momo:/bin/bash\n")
	mustWrite("etc/group", "root:x:0:\nsudo:x:27:momo\n")
	mustWrite("etc/sudoers.d/momo", "momo ALL=(ALL) ALL\n")
	mustWrite("proc/net/tcp", "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1001 1 00000000 100 0 0 10 0\n")
	return root
}

func makeHostRouter(h *OpsHandler, householdID uint, isAdmin bool, isInternal bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", uint(1))
		c.Set("user_name", "test-operator")
		c.Set("household_id", householdID)
		c.Set("is_admin", isAdmin)
		c.Set("is_internal_service", isInternal)
		c.Next()
	})
	opsGroup := r.Group("/api/v1/ops")
	h.RegisterRoutes(opsGroup)
	h.RegisterProposalRoutes(opsGroup)
	return r
}

func doHostJSONReq(r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	var bodyReader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(b)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestHostOSComponentRegistrationDoesNotWeakenContainers(t *testing.T) {
	if c := model.CanonicalizeComponent("host-os"); c != ComponentHostOS {
		t.Fatalf("expected model.CanonicalizeComponent('host-os')=%q, got %q", ComponentHostOS, c)
	}
	if c, err := ResolveComponent("host-os"); err != nil || c != ComponentHostOS {
		t.Fatalf("expected ResolveComponent('host-os')=%q, err=%v", ComponentHostOS, err)
	}
	if _, err := ResolveComponent("host-system"); !errors.Is(err, ErrComponentNotWhitelisted) {
		t.Fatalf("expected host-system to remain rejected, got err=%v", err)
	}

	fakeHostHash := "host-v1:" + strings.Repeat("a", 64)
	fakeContHash := "v1:" + strings.Repeat("b", 64)

	if !IsRealBaselineHash(fakeHostHash, ComponentHostOS) {
		t.Fatalf("expected host-v1 hash accepted for host-os")
	}
	if IsRealBaselineHash(fakeContHash, ComponentHostOS) {
		t.Fatalf("expected container v1 hash rejected for host-os")
	}
	if IsRealBaselineHash(fakeHostHash, ComponentHomeAPI) {
		t.Fatalf("container baseline check weakened: host-v1 hash must NOT validate for home-api")
	}
	if !IsRealBaselineHash(fakeContHash, ComponentHomeAPI) {
		t.Fatalf("expected v1 hash still valid for home-api")
	}
}

func TestHostExecutor_HostPkgUpgradeGuardsAndAudit(t *testing.T) {
	t.Setenv("OPS_APPROVAL_HMAC_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	db, bus, ae, _ := setupAuditR5TestEnv(t)
	defer ae.Close()

	debRoot := writeOpsHostFixture(t, false)
	prov := hostsec.NewDefaultHostSecurityProvider(debRoot, filepath.Join(t.TempDir(), "missing.sock"))
	prov.Runner = &stubHostROCmdRunner{}
	hue := hostsec.NewHostUpdateExecutor(debRoot, t.TempDir(), ae)

	exec := NewOperationExecutor(db, bus, nil, nil,
		WithAuditEngine(ae),
		WithHostSecurityProvider(prov),
		WithHostUpdateExecutor(hue),
	)

	hostBase := exec.ExtractBaseline(ComponentHostOS)
	if !IsRealBaselineHash(hostBase.Hash, ComponentHostOS) {
		t.Fatalf("expected valid host-v1 baseline from ExtractBaseline, got %+v", hostBase)
	}

	// 1. Direct Execute(host.pkg_upgrade) without approved OpsProposal MUST fail closed
	if _, err := exec.Execute(context.Background(), OperationSpec{
		Action:       ActionHostPackageUpgrade,
		Target:       ComponentHostOS,
		BaselineHash: hostBase.Hash,
		Params:       map[string]interface{}{"packages": []string{"openssh-server"}},
	}); !errors.Is(err, hostsec.ErrApprovalRequired) {
		t.Fatalf("expected direct unapproved host.pkg_upgrade to fail with ErrApprovalRequired, got %v", err)
	}

	// 2. docker.restart targeting host-os MUST fail closed
	if _, err := exec.Execute(context.Background(), OperationSpec{
		Action:       ActionDockerRestart,
		Target:       ComponentHostOS,
		BaselineHash: hostBase.Hash,
	}); !errors.Is(err, ErrActionNotSupported) {
		t.Fatalf("expected docker.restart on host-os to fail with ErrActionNotSupported, got %v", err)
	}

	// 3. host-os proposal with empty ActionSpec MUST NOT default to docker.restart
	if _, err := exec.parseProposalSpec(&model.OpsProposal{
		ProposalID:      "prop-host-empty-spec",
		TargetComponent: ComponentHostOS,
		ExecutionPlan:   "restart host",
	}); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("expected empty ActionSpec on host-os to fail with ErrInvalidSpec, got %v", err)
	}
}

func TestHostRoutes_RBAC_Isolation_FnOSFailClosed_And_DebianEndToEnd(t *testing.T) {
	t.Setenv("OPS_APPROVAL_HMAC_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	db, bus, ae, _ := setupAuditR5TestEnv(t)
	defer ae.Close()

	debRoot := writeOpsHostFixture(t, false)
	missingSock := filepath.Join(t.TempDir(), "missing.sock")

	prov := hostsec.NewDefaultHostSecurityProvider(debRoot, missingSock)
	prov.Runner = &stubHostROCmdRunner{
		responses: map[string]struct {
			out string
			err string
			e   error
		}{
			"systemctl list-units --type=service --all --no-pager --no-legend": {
				out: "ssh.service loaded active running OpenBSD Secure Shell server\n",
			},
			"ipset list -n": {
				out: "hdc_blocklist\n",
			},
			"ufw status verbose": {
				out: "Status: active\nDefault: deny (incoming), allow (outgoing), disabled (routed)\n",
			},
			"apt-get -s upgrade": {
				out: "Inst openssh-server [1:9.2p1-2+deb12u2] (1:9.2p1-2+deb12u3 Debian-Security:12/stable-security [amd64])\n",
			},
		},
	}

	debFeed, err := hostsec.ParseDebianTrackerJSON([]byte(`{
		"openssh": {
			"CVE-2024-6387": {
				"description": "regreSSHion race",
				"releases": {
					"bookworm": {"status": "resolved", "fixed_version": "1:9.2p1-2+deb12u3", "urgency": "high"}
				}
			}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}

	dm := security.NewDefenseManager(db, config.DefenseConfig{}, nil, bus)
	dm.SetAuditEngine(ae)
	mon := security.NewHostSecurityMonitor(debRoot, missingSock, dm, ae)
	mon.SetProvider(prov)

	scanner := NewVulnScanner(db, nil)
	scanner.SetAuditEngine(ae)
	scanner.SetHostSecurityProvider(prov)
	scanner.SetCorrelationEngine(hostsec.NewCorrelationEngine(hostsec.CorrelationConfig{
		OSVURL:           " ",
		StaticDebianFeed: debFeed,
	}))

	// Ephemeral SSH banner listener for post-update verification
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, aErr := ln.Accept()
			if aErr != nil {
				return
			}
			_, _ = c.Write([]byte("SSH-2.0-OpenSSH_9.2p1 Debian-2+deb12u3\r\n"))
			_ = c.Close()
		}
	}()

	debSnapRoot := t.TempDir()
	hue := hostsec.NewHostUpdateExecutor(debRoot, debSnapRoot, ae)
	stubExecRunner := &stubHostExecRunner{
		handlers: map[string]func(args []string) (string, string, error){
			"apt-cache": func(args []string) (string, string, error) {
				return "openssh-server:\n  Installed: 1:9.2p1-2+deb12u2\n  Candidate: 1:9.2p1-2+deb12u3\n", "", nil
			},
			"apt-get": func(args []string) (string, string, error) {
				return "Setting up openssh-server (1:9.2p1-2+deb12u3) ...", "", nil
			},
			"dpkg-query": func(args []string) (string, string, error) {
				return "install ok installed\t1:9.2p1-2+deb12u3", "", nil
			},
			"sshd": func(args []string) (string, string, error) {
				return "", "", nil
			},
		},
	}
	bridgeKey := []byte("ops-route-test-bridge-hmac-secret-key-32-bytes-minimum-123456")
	bridgeHTTPHandler, err := hostsec.NewHostBridgeHTTPHandler(hostsec.HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   debSnapRoot,
		KeyID:          "ops-route-k1",
		HMACKey:        bridgeKey,
		Runner:         stubExecRunner,
		RequireFencing: true,
	})
	if err != nil {
		t.Fatalf("NewHostBridgeHTTPHandler: %v", err)
	}
	bridgeSrv := httptest.NewServer(bridgeHTTPHandler)
	defer bridgeSrv.Close()
	authBridge, err := hostsec.NewHMACHostExecutionBridge(hostsec.HostBridgeConfig{
		Endpoint:       bridgeSrv.URL,
		KeyID:          "ops-route-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
	})
	if err != nil {
		t.Fatalf("NewHMACHostExecutionBridge: %v", err)
	}
	hue.RequireFencing = true
	hue.SetBridge(authBridge)

	h := NewOpsHandler(NewOnePanelClient("", "", "", ""), nil, db, bus, dm)
	h.SetAuditEngine(ae)
	h.SetVulnScanner(scanner)
	h.SetHostSecurityProvider(prov)
	h.SetHostSecurityMonitor(mon)
	h.SetHostUpdateExecutor(hue)
	scanner.SetBaselineExtractor(h.ComputeFingerprint)

	// 1. RBAC: non-admin and internal daemon rejected with 403 on all /host/* endpoints
	nonAdminRouter := makeHostRouter(h, 1, false, false)
	internalRouter := makeHostRouter(h, 1, true, true)
	for _, tc := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/ops/host/assets", nil},
		{http.MethodGet, "/api/v1/ops/host/vulns", nil},
		{http.MethodPost, "/api/v1/ops/host/vulns/scan", nil},
		{http.MethodGet, "/api/v1/ops/host/monitor", nil},
		{http.MethodPost, "/api/v1/ops/host/monitor/scan", nil},
		{http.MethodPost, "/api/v1/ops/host/update", map[string]string{"proposal_id": "prop-1"}},
	} {
		if w := doHostJSONReq(nonAdminRouter, tc.method, tc.path, tc.body); w.Code != http.StatusForbidden {
			t.Fatalf("non-admin %s %s expected 403, got %d: %s", tc.method, tc.path, w.Code, w.Body.String())
		}
		if w := doHostJSONReq(internalRouter, tc.method, tc.path, tc.body); w.Code != http.StatusForbidden {
			t.Fatalf("internal service %s %s expected 403, got %d: %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}

	adminRouter := makeHostRouter(h, 1, true, false)

	// 2. GET /host/assets returns snapshot + host-v1 baseline hash
	wAssets := doHostJSONReq(adminRouter, http.MethodGet, "/api/v1/ops/host/assets", nil)
	if wAssets.Code != http.StatusOK {
		t.Fatalf("GET /host/assets failed (%d): %s", wAssets.Code, wAssets.Body.String())
	}
	if !strings.Contains(wAssets.Body.String(), "host-v1:") || !strings.Contains(wAssets.Body.String(), "openssh-server") {
		t.Fatalf("unexpected /host/assets payload: %s", wAssets.Body.String())
	}

	// 3. POST /host/vulns/scan -> detects CVE-2024-6387 and persists host.pkg_upgrade proposal with host-v1 baseline
	wScan := doHostJSONReq(adminRouter, http.MethodPost, "/api/v1/ops/host/vulns/scan", nil)
	if wScan.Code != http.StatusOK {
		t.Fatalf("POST /host/vulns/scan failed (%d): %s", wScan.Code, wScan.Body.String())
	}
	wVulns := doHostJSONReq(adminRouter, http.MethodGet, "/api/v1/ops/host/vulns", nil)
	if wVulns.Code != http.StatusOK || !strings.Contains(wVulns.Body.String(), "CVE-2024-6387") {
		t.Fatalf("GET /host/vulns unexpected (%d): %s", wVulns.Code, wVulns.Body.String())
	}

	// 4. POST /host/monitor/scan -> baseline then detect SSH config drift on second scan
	wMon1 := doHostJSONReq(adminRouter, http.MethodPost, "/api/v1/ops/host/monitor/scan", nil)
	if wMon1.Code != http.StatusOK {
		t.Fatalf("first /host/monitor/scan failed (%d): %s", wMon1.Code, wMon1.Body.String())
	}
	if err := os.WriteFile(filepath.Join(debRoot, "etc/ssh/sshd_config"), []byte("Port 22\nPermitRootLogin yes\nPasswordAuthentication yes\n"), 0644); err != nil {
		t.Fatal(err)
	}
	wMon2 := doHostJSONReq(adminRouter, http.MethodPost, "/api/v1/ops/host/monitor/scan", nil)
	if wMon2.Code != http.StatusOK || !strings.Contains(wMon2.Body.String(), string(security.CatSSHConfigDrift)) {
		t.Fatalf("expected ssh_config_drift finding on second monitor scan (%d): %s", wMon2.Code, wMon2.Body.String())
	}
	// Restore sshd_config before baseline-bound update flow
	if err := os.WriteFile(filepath.Join(debRoot, "etc/ssh/sshd_config"), []byte("Port 22\nPermitRootLogin no\nPasswordAuthentication no\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 5. Create + Approve + Execute Debian host update proposal through HTTP endpoints
	liveBaseline := h.ComputeFingerprint(ComponentHostOS)
	specBytes, _ := json.Marshal(OperationSpec{
		Action: ActionHostPackageUpgrade,
		Target: ComponentHostOS,
		Params: map[string]interface{}{
			"packages":        []string{"openssh-server"},
			"target_versions": map[string]string{"openssh-server": "1:9.2p1-2+deb12u3"},
			"ssh_probe_addr":  ln.Addr().String(),
		},
	})
	prop := model.OpsProposal{
		HouseholdID:      1,
		ProposalID:       "prop-host-e2e-1",
		Title:            "Upgrade openssh-server on Debian",
		Description:      "Patch CVE-2024-6387",
		TargetComponent:  ComponentHostOS,
		ActionSpec:       string(specBytes),
		BaselineHash:     liveBaseline,
		RiskLevel:        model.RiskLevelHigh,
		EstimatedSeconds: 60,
		DowntimeSeconds:  5,
		ExecutionPlan:    "Upgrade openssh-server safely",
		RollbackPlan:     "Restore sshd_config and downgrade",
		Status:           model.ProposalStatusAwaitingApproval,
		Version:          1,
		Revision:         1,
		CreatedAt:        time.Now().UTC(),
	}
	if err := db.Create(&prop).Error; err != nil {
		t.Fatalf("create proposal: %v", err)
	}

	// Unapproved execution via POST /host/update MUST fail closed (403)
	if wUnapproved := doHostJSONReq(adminRouter, http.MethodPost, "/api/v1/ops/host/update", map[string]string{"proposal_id": prop.ProposalID}); wUnapproved.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for unapproved host proposal, got %d: %s", wUnapproved.Code, wUnapproved.Body.String())
	}

	// Household isolation: household 2 admin MUST receive 404
	hh2Router := makeHostRouter(h, 2, true, false)
	if wIDOR := doHostJSONReq(hh2Router, http.MethodPost, "/api/v1/ops/host/update", map[string]string{"proposal_id": prop.ProposalID}); wIDOR.Code != http.StatusNotFound {
		t.Fatalf("expected 404 cross-household isolation, got %d: %s", wIDOR.Code, wIDOR.Body.String())
	}

	// Dedicated approved proposal with tampered ApprovalDigest MUST fail closed (403)
	tamperedProp := model.OpsProposal{
		HouseholdID:      1,
		ProposalID:       "prop-host-tampered-1",
		Title:            "Tampered host upgrade digest",
		TargetComponent:  ComponentHostOS,
		ActionSpec:       string(specBytes),
		BaselineHash:     liveBaseline,
		RiskLevel:        model.RiskLevelHigh,
		EstimatedSeconds: 60,
		DowntimeSeconds:  5,
		ExecutionPlan:    "Upgrade openssh-server safely",
		RollbackPlan:     "Restore sshd_config and downgrade",
		Status:           model.ProposalStatusApproved,
		ApprovedBy:       "admin",
		ApprovedAt:       utils.NullTime{Time: time.Now().UTC().Truncate(time.Second), Valid: true},
		ApprovalDigest:   "hmac-sha256:k1:deadbeef",
		Version:          1,
		Revision:         1,
		CreatedAt:        time.Now().UTC(),
	}
	if err := db.Create(&tamperedProp).Error; err != nil {
		t.Fatalf("create tampered proposal: %v", err)
	}
	if wTampered := doHostJSONReq(adminRouter, http.MethodPost, "/api/v1/ops/host/update", map[string]string{"proposal_id": tamperedProp.ProposalID}); wTampered.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for tampered approval digest, got %d: %s", wTampered.Code, wTampered.Body.String())
	}

	// fnOS fail-closed gate: switch HostUpdateExecutor to fnOS fixture while approved proposal is signed
	fnosRoot := writeOpsHostFixture(t, true)
	fnosHue := hostsec.NewHostUpdateExecutor(fnosRoot, t.TempDir(), ae)
	fnosPropSpec, _ := json.Marshal(OperationSpec{
		Action: ActionHostPackageUpgrade,
		Target: ComponentHostOS,
		Params: map[string]interface{}{"packages": []string{"openssh-server"}},
	})
	fnosHandler := NewOpsHandler(NewOnePanelClient("", "", "", ""), nil, db, bus, dm)
	fnosHandler.SetAuditEngine(ae)
	fnosHandler.SetHostSecurityProvider(hostsec.NewDefaultHostSecurityProvider(fnosRoot, missingSock))
	fnosHandler.SetHostUpdateExecutor(fnosHue)
	fnosProp := model.OpsProposal{
		HouseholdID:      1,
		ProposalID:       "prop-host-fnos-1",
		Title:            "Attempt host upgrade on fnOS",
		TargetComponent:  ComponentHostOS,
		ActionSpec:       string(fnosPropSpec),
		BaselineHash:     fnosHandler.ComputeFingerprint(ComponentHostOS),
		RiskLevel:        model.RiskLevelHigh,
		EstimatedSeconds: 60,
		DowntimeSeconds:  5,
		ExecutionPlan:    "fnOS blocked",
		RollbackPlan:     "none",
		Status:           model.ProposalStatusApproved,
		ApprovedBy:       "admin",
		ApprovedAt:       utils.NullTime{Time: time.Now().UTC().Truncate(time.Second), Valid: true},
		Version:          1,
		Revision:         1,
		CreatedAt:        time.Now().UTC(),
	}
	fnosDigest, err := model.ComputeProposalApprovalDigest(&fnosProp)
	if err != nil {
		t.Fatal(err)
	}
	fnosProp.ApprovalDigest = fnosDigest
	if err := db.Create(&fnosProp).Error; err != nil {
		t.Fatal(err)
	}
	fnosRouter := makeHostRouter(fnosHandler, 1, true, false)
	wFnOS := doHostJSONReq(fnosRouter, http.MethodPost, "/api/v1/ops/host/update", map[string]string{"proposal_id": fnosProp.ProposalID})
	if wFnOS.Code != http.StatusConflict || !strings.Contains(wFnOS.Body.String(), "fnOS") {
		t.Fatalf("expected 409 Conflict fail-closed fnOS rejection, got %d: %s", wFnOS.Code, wFnOS.Body.String())
	}

	// Container / cross-namespace boundary gate: missing RestrictedHostExecutionBridge MUST return 409 Conflict
	noBridgeHandler := NewOpsHandler(NewOnePanelClient("", "", "", ""), nil, db, bus, dm)
	noBridgeHandler.SetAuditEngine(ae)
	noBridgeHandler.SetHostSecurityProvider(prov)
	noBridgeHue := hostsec.NewHostUpdateExecutor(debRoot, t.TempDir(), ae)
	noBridgeHue.Detector.InDocker = true
	noBridgeHandler.SetHostUpdateExecutor(noBridgeHue)
	noBridgeProp := model.OpsProposal{
		HouseholdID:      1,
		ProposalID:       "prop-host-nobridge-1",
		Title:            "Attempt host upgrade without execution bridge",
		TargetComponent:  ComponentHostOS,
		ActionSpec:       string(specBytes),
		BaselineHash:     liveBaseline,
		RiskLevel:        model.RiskLevelHigh,
		EstimatedSeconds: 60,
		DowntimeSeconds:  5,
		ExecutionPlan:    "Blocked across container boundary",
		RollbackPlan:     "none",
		Status:           model.ProposalStatusApproved,
		ApprovedBy:       "admin",
		ApprovedAt:       utils.NullTime{Time: time.Now().UTC().Truncate(time.Second), Valid: true},
		Version:          1,
		Revision:         1,
		CreatedAt:        time.Now().UTC(),
	}
	noBridgeDigest, err := model.ComputeProposalApprovalDigest(&noBridgeProp)
	if err != nil {
		t.Fatal(err)
	}
	noBridgeProp.ApprovalDigest = noBridgeDigest
	if err := db.Create(&noBridgeProp).Error; err != nil {
		t.Fatal(err)
	}
	noBridgeRouter := makeHostRouter(noBridgeHandler, 1, true, false)
	wNoBridge := doHostJSONReq(noBridgeRouter, http.MethodPost, "/api/v1/ops/host/update", map[string]string{"proposal_id": noBridgeProp.ProposalID})
	if wNoBridge.Code != http.StatusConflict || !strings.Contains(wNoBridge.Body.String(), "container namespace boundary") {
		t.Fatalf("expected 409 Conflict fail-closed container boundary rejection, got %d: %s", wNoBridge.Code, wNoBridge.Body.String())
	}

	// Execute valid signed Debian proposal end-to-end via POST /api/v1/ops/host/update
	updateProp := model.OpsProposal{
		HouseholdID:      1,
		ProposalID:       "prop-host-update-1",
		Title:            "Signed host upgrade via /host/update",
		Description:      "Patch CVE-2024-6387",
		TargetComponent:  ComponentHostOS,
		ActionSpec:       string(specBytes),
		BaselineHash:     liveBaseline,
		RiskLevel:        model.RiskLevelHigh,
		EstimatedSeconds: 60,
		DowntimeSeconds:  5,
		ExecutionPlan:    "Upgrade openssh-server safely",
		RollbackPlan:     "Restore sshd_config and downgrade",
		Status:           model.ProposalStatusApproved,
		ApprovedBy:       "admin",
		ApprovedAt:       utils.NullTime{Time: time.Now().UTC().Truncate(time.Second), Valid: true},
		Version:          1,
		Revision:         1,
		CreatedAt:        time.Now().UTC(),
	}
	updateDigest, err := model.ComputeProposalApprovalDigest(&updateProp)
	if err != nil {
		t.Fatal(err)
	}
	updateProp.ApprovalDigest = updateDigest
	if err := db.Create(&updateProp).Error; err != nil {
		t.Fatalf("create signed update proposal: %v", err)
	}
	wExec := doHostJSONReq(adminRouter, http.MethodPost, "/api/v1/ops/host/update", map[string]string{"proposal_id": updateProp.ProposalID})
	if wExec.Code != http.StatusOK {
		t.Fatalf("expected 200 for approved Debian host update, got %d: %s", wExec.Code, wExec.Body.String())
	}

	var executedUpdateProp model.OpsProposal
	if err := db.Where("proposal_id = ?", updateProp.ProposalID).First(&executedUpdateProp).Error; err != nil {
		t.Fatal(err)
	}
	if executedUpdateProp.Status != model.ProposalStatusExecuted {
		t.Fatalf("expected /host/update proposal status EXECUTED, got %s", executedUpdateProp.Status)
	}

	// Also verify POST /proposals/:id/approve?sync=true signs and synchronously executes prop-host-e2e-1
	wApprove := doHostJSONReq(adminRouter, http.MethodPost, fmt.Sprintf("/api/v1/ops/proposals/%s/approve?sync=true", prop.ProposalID), map[string]any{
		"action": "approve",
	})
	if wApprove.Code != http.StatusOK {
		t.Fatalf("POST /proposals/:id/approve?sync=true failed (%d): %s", wApprove.Code, wApprove.Body.String())
	}

	var executedProp model.OpsProposal
	if err := db.Where("proposal_id = ?", prop.ProposalID).First(&executedProp).Error; err != nil {
		t.Fatal(err)
	}
	if executedProp.Status != model.ProposalStatusExecuted {
		t.Fatalf("expected approved sync proposal status EXECUTED, got %s", executedProp.Status)
	}

	var auditEventCount int64
	db.Model(&model.AgentAuditEvent{}).Where("component = ?", ComponentHostOS).Count(&auditEventCount)
	if auditEventCount == 0 {
		t.Fatalf("expected unified AgentAuditEvent entries recorded for host-os")
	}
}
