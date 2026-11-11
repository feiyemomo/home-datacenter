package hostsec

import (
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

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/audit"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

func setupHostsecAuditDB(t *testing.T) (*gorm.DB, *audit.AgentAuditEngine) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	ae := audit.NewAgentAuditEngine(db, nil, audit.WithArchiveDir(t.TempDir()))
	return db, ae
}

func makeSignedHostUpdateProposal(t *testing.T, pkgs []string) *model.OpsProposal {
	t.Helper()
	t.Setenv("OPS_APPROVAL_HMAC_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	t.Setenv("OPS_AUDIT_HMAC_KEY", "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")

	approvedAt := time.Now().UTC().Truncate(time.Second)
	specBytes, _ := json.Marshal(map[string]any{
		"action": ActionHostPackageUpgrade,
		"target": "host-os",
		"params": map[string]any{"packages": pkgs},
	})
	prop := &model.OpsProposal{
		ProposalID:      "prop-hostupd-1",
		HouseholdID:     1,
		Title:           "Upgrade OpenSSH security patch",
		TargetComponent: "host-os",
		ActionSpec:      string(specBytes),
		RiskLevel:       model.RiskLevelHigh,
		ExecutionPlan:   "Snapshot -> apt-get upgrade -> sshd -t & SSH probe",
		RollbackPlan:    "Restore sshd_config + downgrade package",
		Status:          model.ProposalStatusApproved,
		ApprovedBy:      "admin",
		ApprovedAt:      utils.NullTime{Time: approvedAt, Valid: true},
		BaselineHash:    "host-v1:test",
		Version:         1,
	}
	digest, err := model.ComputeProposalApprovalDigest(prop)
	if err != nil {
		t.Fatalf("ComputeProposalApprovalDigest: %v", err)
	}
	prop.ApprovalDigest = digest
	return prop
}

type mockROCommandRunner struct {
	responses map[string]struct {
		out string
		err string
		e   error
	}
}

func (m *mockROCommandRunner) Run(_ context.Context, name string, args ...string) (string, string, error) {
	key := name + " " + strings.Join(args, " ")
	if r, ok := m.responses[key]; ok {
		return r.out, r.err, r.e
	}
	return "", "", fmt.Errorf("mock command not registered: %s", key)
}

type mockHostExecRunner struct {
	mu       sync.Mutex
	calls    []string
	handlers map[string]func(args []string) (string, string, error)
}

func (m *mockHostExecRunner) Run(_ context.Context, _ []string, name string, args ...string) (string, string, error) {
	m.mu.Lock()
	m.calls = append(m.calls, name+" "+strings.Join(args, " "))
	fn := m.handlers[name]
	m.mu.Unlock()
	if fn != nil {
		return fn(args)
	}
	return "", "", nil
}

func writeHostFixture(t *testing.T, fnos bool) string {
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
	} else {
		mustWrite("proc/sys/kernel/osrelease", "6.1.0-26-amd64\n")
	}
	mustWrite("proc/sys/kernel/arch", "x86_64\n")
	mustWrite("etc/ssh/sshd_config", "Port 22\nPermitRootLogin no\nPasswordAuthentication no\n")
	mustWrite("etc/apt/sources.list", "deb https://deb.debian.org/debian bookworm main\n")

	if fnos {
		mustWrite("etc/os-release", "PRETTY_NAME=\"fnOS 0.8.36 (trim)\"\nID=trim\nID_LIKE=debian\nVERSION_ID=\"12\"\nVERSION_CODENAME=bookworm\n")
		mustWrite("etc/trim-release", "0.8.36\n")
	} else {
		mustWrite("etc/os-release", "PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nID=debian\nVERSION_ID=\"12\"\nVERSION_CODENAME=bookworm\n")
	}

	mustWrite("var/lib/dpkg/status", strings.Join([]string{
		"Package: openssh-server",
		"Status: install ok installed",
		"Architecture: amd64",
		"Source: openssh",
		"Version: 1:9.2p1-2+deb12u3",
		"",
		"Package: libssl3",
		"Status: install ok installed",
		"Architecture: amd64",
		"Source: openssl",
		"Version: 3.0.11-1~deb12u2",
		"",
	}, "\n"))

	mustWrite("etc/passwd", "root:x:0:0:root:/root:/bin/bash\nmomo:x:1000:1000::/home/momo:/bin/bash\nwww-data:x:33:33::/var/www:/usr/sbin/nologin\n")
	mustWrite("etc/group", "root:x:0:\nsudo:x:27:momo\ndocker:x:999:momo\n")
	mustWrite("etc/sudoers.d/momo", "momo ALL=(ALL) ALL\n")

	// /proc/net/tcp: 0.0.0.0:22 (00000000:0016) LISTEN (0A) and 127.0.0.1:8080 (0100007F:1F90) LISTEN (0A)
	mustWrite("proc/net/tcp", strings.Join([]string{
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode",
		"   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1001 1 00000000 100 0 0 10 0",
		"   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00001000     0        0 1002 1 00000000 100 0 0 10 0",
	}, "\n"))

	return root
}

func TestPlatformDetector_FnOSvsDebianFailClosed(t *testing.T) {
	fnosRoot := writeHostFixture(t, true)
	fnosInfo := NewPlatformDetector(fnosRoot).Detect(context.Background())
	if !fnosInfo.IsFnOS || fnosInfo.OSFamily != OSFamilyFnOS || fnosInfo.HostUpdateSupported {
		t.Fatalf("expected fnOS read-only non-updatable detection, got %+v", fnosInfo)
	}
	if err := fnosInfo.HostUpdatePolicyError(); !errors.Is(err, ErrFnOSUpdateUnsupported) {
		t.Fatalf("expected ErrFnOSUpdateUnsupported, got %v", err)
	}

	debRoot := writeHostFixture(t, false)
	debInfo := NewPlatformDetector(debRoot).Detect(context.Background())
	if debInfo.IsFnOS || debInfo.OSFamily != OSFamilyDebian || !debInfo.HostUpdateSupported {
		t.Fatalf("expected Debian apt-dpkg updatable detection, got %+v", debInfo)
	}
	if err := debInfo.HostUpdatePolicyError(); err != nil {
		t.Fatalf("expected nil update policy error for Debian, got %v", err)
	}

	// Unknown / empty OS rootfs must fail closed
	emptyRoot := t.TempDir()
	unkInfo := NewPlatformDetector(emptyRoot).Detect(context.Background())
	if err := unkInfo.HostUpdatePolicyError(); !errors.Is(err, ErrUnsupportedHostOS) {
		t.Fatalf("expected ErrUnsupportedHostOS, got %v", err)
	}
}

func TestHostSecurityProvider_CollectAssetsReadOnly(t *testing.T) {
	debRoot := writeHostFixture(t, false)
	prov := NewDefaultHostSecurityProvider(debRoot, filepath.Join(t.TempDir(), "missing.sock"))
	prov.Runner = &mockROCommandRunner{
		responses: map[string]struct {
			out string
			err string
			e   error
		}{
			"systemctl list-units --type=service --all --no-pager --no-legend": {
				out: "ssh.service loaded active running OpenBSD Secure Shell server\ndocker.service loaded active running Docker Application Container Engine\n",
			},
			"ipset list -n": {
				out: "hdc_blocklist\n",
			},
			"ufw status verbose": {
				out: "Status: active\nDefault: deny (incoming), allow (outgoing), disabled (routed)\n22/tcp ALLOW IN Anywhere\n",
			},
			"apt-get -s upgrade": {
				out: "Inst libssl3 [3.0.11-1~deb12u2] (3.0.14-1~deb12u1 Debian-Security:12/stable-security [amd64])\n",
			},
		},
	}

	snap, err := prov.CollectAssets(context.Background())
	if err != nil {
		t.Fatalf("CollectAssets failed: %v", err)
	}
	if snap.Hostname != "nas-prod" || snap.KernelVersion != "6.1.0-26-amd64" {
		t.Fatalf("unexpected host identity: %+v", snap)
	}
	if snap.OpenSSH == nil || snap.OpenSSH.Version != "1:9.2p1-2+deb12u3" {
		t.Fatalf("expected OpenSSH dpkg version, got %+v", snap.OpenSSH)
	}
	if snap.Firewall == nil || !snap.Firewall.Active || !snap.Firewall.BlocklistSetPresent || snap.Firewall.DefaultInputPolicy != "DROP" {
		t.Fatalf("unexpected firewall status: %+v", snap.Firewall)
	}
	if len(snap.ListeningPorts) != 2 || snap.ListeningPorts[0].LocalPort != 22 || !snap.ListeningPorts[0].IsWildcard {
		t.Fatalf("unexpected listening ports: %+v", snap.ListeningPorts)
	}
	if snap.SecurityUpdates == nil || snap.SecurityUpdates.SecurityCount != 1 {
		t.Fatalf("expected 1 security update pending, got %+v", snap.SecurityUpdates)
	}

	foundMomo := false
	for _, u := range snap.Users {
		if u.Username == "momo" && u.HasSudo && u.HasLoginShell {
			foundMomo = true
		}
	}
	if !foundMomo {
		t.Fatalf("expected sudoer momo in users list: %+v", snap.Users)
	}
}

func TestCompareDpkgVersions_BackportSemantics(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1:9.2p1-2+deb12u3", "1:9.2p1-2+deb12u2", 1},
		{"1:9.2p1-2+deb12u3", "1:9.2p1-2+deb12u3", 0},
		{"3.0.11-1~deb12u2", "3.0.11-1", -1},
		{"1:8.9p1-3", "9.8p1-1", 1}, // epoch 1 > epoch 0
		{"24.0.9-1", "26.1.3-1", -1},
	}
	for _, tc := range cases {
		got := CompareDpkgVersions(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("CompareDpkgVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCorrelationEngine_BackportSuppressionAndCISAKEV(t *testing.T) {
	kev := NewCISAKEVCatalog()
	if err := kev.LoadJSON([]byte(`{
		"vulnerabilities": [
			{"cveID": "CVE-2024-6387", "dateAdded": "2024-07-01", "dueDate": "2024-07-22", "requiredAction": "Apply updates"}
		]
	}`)); err != nil {
		t.Fatal(err)
	}

	// OSV mock returns a paginated vuln for libssl3
	osvSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		pkgMap, _ := req["package"].(map[string]any)
		pkgName, _ := pkgMap["name"].(string)
		if pkgName == "libssl3" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"vulns": []OSVAdvisory{
					{
						ID:      "DEBIAN-CVE-2024-5535",
						Aliases: []string{"CVE-2024-5535"},
						Summary: "OpenSSL SSL_select_next_proto buffer overread",
						Severity: []OSVSeverity{
							{Type: "CVSS_V3", Score: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:N/A:H"},
						},
						Affected: []OSVAffected{
							{
								Package: struct {
									Name      string `json:"name"`
									Ecosystem string `json:"ecosystem"`
								}{Name: "libssl3", Ecosystem: "Debian:12"},
								Ranges: []struct {
									Type   string `json:"type"`
									Events []struct {
										Introduced string `json:"introduced,omitempty"`
										Fixed      string `json:"fixed,omitempty"`
									} `json:"events"`
								}{
									{Type: "ECOSYSTEM", Events: []struct {
										Introduced string `json:"introduced,omitempty"`
										Fixed      string `json:"fixed,omitempty"`
									}{{Introduced: "0"}, {Fixed: "3.0.14-1~deb12u1"}}},
								},
							},
						},
					},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"vulns": []any{}})
	}))
	defer osvSrv.Close()

	debFeed, err := ParseDebianTrackerJSON([]byte(`{
		"openssh": {
			"CVE-2024-6387": {
				"description": "regreSSHion signal handler race",
				"releases": {
					"bookworm": {"status": "resolved", "fixed_version": "1:9.2p1-2+deb12u3", "urgency": "high"}
				}
			}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}

	engine := NewCorrelationEngine(CorrelationConfig{
		OSVURL:           osvSrv.URL,
		StaticDebianFeed: debFeed,
		StaticKEV:        kev,
	})

	// 1. Installed 1:9.2p1-2+deb12u3 -> patched via backport!
	snap := &HostAssetSnapshot{
		Platform: PlatformInfo{OSFamily: OSFamilyFnOS, IsFnOS: true, VersionID: "12", Codename: "bookworm"},
		Packages: []OSPackage{
			{Name: "openssh-server", SourceName: "openssh", Version: "1:9.2p1-2+deb12u3"},
			{Name: "libssl3", SourceName: "openssl", Version: "3.0.11-1~deb12u2"},
		},
	}

	rep, err := engine.CorrelateAssets(context.Background(), snap)
	if err != nil {
		t.Fatalf("CorrelateAssets failed: %v", err)
	}
	if len(rep.SuppressedBackport) != 1 || rep.SuppressedBackport[0].CVEID != "CVE-2024-6387" {
		t.Fatalf("expected CVE-2024-6387 suppressed by backport check, got %+v", rep.SuppressedBackport)
	}
	if len(rep.VulnerableFindings) != 1 || rep.VulnerableFindings[0].CVEID != "CVE-2024-5535" {
		t.Fatalf("expected CVE-2024-5535 vulnerable finding, got %+v", rep.VulnerableFindings)
	}
	if rep.VulnerableFindings[0].RemediationMode != "fnos_vendor_ota" {
		t.Fatalf("expected fnos_vendor_ota remediation mode on fnOS, got %s", rep.VulnerableFindings[0].RemediationMode)
	}

	// 2. Downgrade openssh-server to unpatched u2 -> CVE-2024-6387 flagged AND tagged InCISAKEV!
	snap.Packages[0].Version = "1:9.2p1-2+deb12u2"
	rep2, err := engine.CorrelateAssets(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.KEVMatches != 1 {
		t.Fatalf("expected 1 CISA KEV match, got %d", rep2.KEVMatches)
	}
	if rep2.VulnerableFindings[0].CVEID != "CVE-2024-6387" || !rep2.VulnerableFindings[0].InCISAKEV {
		t.Fatalf("expected KEV-prioritized CVE-2024-6387 first, got %+v", rep2.VulnerableFindings[0])
	}
}

func TestHostUpdateExecutor_SecurityGatesAndRollback(t *testing.T) {
	// 1. fnOS fail-closed test
	fnosRoot := writeHostFixture(t, true)
	_, ae := setupHostsecAuditDB(t)
	fnosExec := NewHostUpdateExecutor(fnosRoot, t.TempDir(), ae)
	prop := makeSignedHostUpdateProposal(t, []string{"openssh-server"})
	baseHash, _, _ := fnosExec.ComputeBaselineHash(context.Background(), []string{"openssh-server"})

	_, err := fnosExec.Execute(context.Background(), HostUpdateRequest{
		Proposal:             prop,
		Packages:             []string{"openssh-server"},
		ExpectedBaselineHash: baseHash,
	})
	if !errors.Is(err, ErrFnOSUpdateUnsupported) {
		t.Fatalf("expected ErrFnOSUpdateUnsupported on fnOS, got %v", err)
	}

	// 2. Whitelist, injection, approval, baseline, and insecure apt source gates on Debian
	debRoot := writeHostFixture(t, false)
	debExec := NewHostUpdateExecutor(debRoot, t.TempDir(), ae)
	debBase, _, _ := debExec.ComputeBaselineHash(context.Background(), []string{"openssh-server"})

	if _, err := debExec.Execute(context.Background(), HostUpdateRequest{
		Proposal:             prop,
		Packages:             []string{"openssh-server;rm -rf /"},
		ExpectedBaselineHash: debBase,
	}); !errors.Is(err, ErrInvalidPackageName) {
		t.Fatalf("expected ErrInvalidPackageName, got %v", err)
	}

	if _, err := debExec.Execute(context.Background(), HostUpdateRequest{
		Proposal:             prop,
		Packages:             []string{"vim"},
		ExpectedBaselineHash: debBase,
	}); !errors.Is(err, ErrPackageNotWhitelisted) {
		t.Fatalf("expected ErrPackageNotWhitelisted, got %v", err)
	}

	if _, err := debExec.Execute(context.Background(), HostUpdateRequest{
		Proposal:             prop,
		Packages:             []string{"openssh-server"},
		ExpectedBaselineHash: "host-v1:tampered",
	}); !errors.Is(err, ErrBaselineDrift) {
		t.Fatalf("expected ErrBaselineDrift, got %v", err)
	}

	badProp := makeSignedHostUpdateProposal(t, []string{"openssh-server"})
	badProp.ApprovalDigest = "hmac-sha256:k1:deadbeef"
	if _, err := debExec.Execute(context.Background(), HostUpdateRequest{
		Proposal:             badProp,
		Packages:             []string{"openssh-server"},
		ExpectedBaselineHash: debBase,
	}); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("expected ErrApprovalRequired on tampered digest, got %v", err)
	}

	// 3. Successful upgrade + post-update SSH banner verification + audit trail
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("SSH-2.0-OpenSSH_9.2p1 Debian-2+deb12u4\r\n"))
			_ = c.Close()
		}
	}()

	mockRunner := &mockHostExecRunner{
		handlers: map[string]func(args []string) (string, string, error){
			"apt-cache": func(args []string) (string, string, error) {
				return "openssh-server:\n  Installed: 1:9.2p1-2+deb12u3\n  Candidate: 1:9.2p1-2+deb12u4\n", "", nil
			},
			"apt-get": func(args []string) (string, string, error) {
				return "Setting up openssh-server (1:9.2p1-2+deb12u4) ...", "", nil
			},
			"dpkg-query": func(args []string) (string, string, error) {
				return "install ok installed\t1:9.2p1-2+deb12u4", "", nil
			},
			"sshd": func(args []string) (string, string, error) {
				return "", "", nil
			},
		},
	}
	debExec.Runner = mockRunner
	res, err := debExec.Execute(context.Background(), HostUpdateRequest{
		Proposal:             prop,
		Packages:             []string{"openssh-server"},
		TargetVersions:       map[string]string{"openssh-server": "1:9.2p1-2+deb12u4"},
		ExpectedBaselineHash: debBase,
		SSHProbeAddr:         ln.Addr().String(),
	})
	if err != nil || !res.Success || !res.Verified {
		t.Fatalf("expected successful verified host update, got res=%+v err=%v", res, err)
	}

	// 4. Failed SSH verification triggers rollback & restores sshd_config
	sshFailExec := NewHostUpdateExecutor(debRoot, t.TempDir(), ae)
	sshFailBase, _, _ := sshFailExec.ComputeBaselineHash(context.Background(), []string{"openssh-server"})
	sshFailExec.Runner = &mockHostExecRunner{
		handlers: map[string]func(args []string) (string, string, error){
			"apt-cache": func(args []string) (string, string, error) {
				return "Candidate: 1:9.2p1-2+deb12u4\n", "", nil
			},
			"apt-get": func(args []string) (string, string, error) {
				for _, a := range args {
					if a == "--only-upgrade" {
						// Simulate forward upgrade corrupting sshd_config before verification catches it
						_ = os.WriteFile(filepath.Join(debRoot, "etc/ssh/sshd_config"), []byte("BROKEN_DIRECTIVE yes\n"), 0644)
					}
				}
				return "ok", "", nil
			},
			"dpkg-query": func(args []string) (string, string, error) {
				return "install ok installed\t1:9.2p1-2+deb12u4", "", nil
			},
			"sshd": func(args []string) (string, string, error) {
				return "", "bad configuration option: BROKEN_DIRECTIVE", errors.New("exit 255")
			},
			"systemctl": func(args []string) (string, string, error) {
				return "", "", nil
			},
		},
	}
	rbRes, rbErr := sshFailExec.Execute(context.Background(), HostUpdateRequest{
		Proposal:             prop,
		Packages:             []string{"openssh-server"},
		ExpectedBaselineHash: sshFailBase,
	})
	if !errors.Is(rbErr, ErrRollbackAttempted) || !rbRes.RolledBack {
		t.Fatalf("expected rollback on sshd -t failure, got res=%+v err=%v", rbRes, rbErr)
	}
	restoredCfg, _ := os.ReadFile(filepath.Join(debRoot, "etc/ssh/sshd_config"))
	if strings.Contains(string(restoredCfg), "BROKEN_DIRECTIVE") || !strings.Contains(string(restoredCfg), "PermitRootLogin no") {
		t.Fatalf("expected sshd_config to be restored on rollback, got %q", string(restoredCfg))
	}

	// 5. Proposal scope mismatch (cannot upgrade sudo using openssh-server proposal)
	sudoBase, _, _ := debExec.ComputeBaselineHash(context.Background(), []string{"sudo"})
	if _, err := debExec.Execute(context.Background(), HostUpdateRequest{
		Proposal:             prop,
		Packages:             []string{"sudo"},
		ExpectedBaselineHash: sudoBase,
	}); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("expected ErrApprovalRequired on scope mismatch, got %v", err)
	}

	// 6. Insecure [trusted=yes] apt source rejected fail-closed
	insRoot := writeHostFixture(t, false)
	_ = os.WriteFile(filepath.Join(insRoot, "etc/apt/sources.list"), []byte("deb [trusted=yes] http://insecure.example/debian bookworm main\n"), 0644)
	insExec := NewHostUpdateExecutor(insRoot, t.TempDir(), ae)
	insBase, _, _ := insExec.ComputeBaselineHash(context.Background(), []string{"openssh-server"})
	if _, err := insExec.Execute(context.Background(), HostUpdateRequest{
		Proposal:             prop,
		Packages:             []string{"openssh-server"},
		ExpectedBaselineHash: insBase,
	}); !errors.Is(err, ErrInsecureAptSource) {
		t.Fatalf("expected ErrInsecureAptSource, got %v", err)
	}

	// 7. SSH dead-man's-switch local recovery guard triggers automatically if not disarmed
	recovered := make(chan bool, 1)
	guard := newLocalTimerSSHGuard()
	gID, err := guard.Arm(context.Background(), t.TempDir(), 40*time.Millisecond, func(_ context.Context) error {
		recovered <- true
		return nil
	})
	if err != nil || gID == "" {
		t.Fatalf("Arm failed: %v", err)
	}
	select {
	case <-recovered:
	case <-time.After(1 * time.Second):
		t.Fatalf("expected SSH dead-man's-switch recovery guard to fire automatically")
	}
	if err := guard.Disarm(gID); err == nil {
		t.Fatalf("expected Disarm after guard already fired to return error")
	}
}

func TestCorrelationEngine_NVDAndGHSAEnrichment(t *testing.T) {
	nvdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"vulnerabilities": []map[string]any{
				{
					"cve": map[string]any{
						"id": "CVE-2025-1111",
						"descriptions": []map[string]string{
							{"lang": "en", "value": "Docker Engine daemon privilege escalation"},
						},
						"metrics": map[string]any{
							"cvssMetricV31": []map[string]any{
								{"cvssData": map[string]any{"baseScore": 8.8, "baseSeverity": "HIGH", "vectorString": "CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:H/A:H"}},
							},
						},
					},
				},
			},
		})
	}))
	defer nvdSrv.Close()

	ghsaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"ghsa_id":  "GHSA-xxxx-yyyy-zzzz",
				"cve_id":   "CVE-2025-2222",
				"summary":  "OpenSSH proxy vulnerability",
				"severity": "critical",
				"cvss":     map[string]any{"score": 9.1, "vector_string": "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:N"},
			},
		})
	}))
	defer ghsaSrv.Close()

	eng := NewCorrelationEngine(CorrelationConfig{
		OSVURL:  " ",
		NVDURL:  nvdSrv.URL,
		GHSAURL: ghsaSrv.URL,
	})
	rep, err := eng.CorrelateAssets(context.Background(), &HostAssetSnapshot{
		Platform:     PlatformInfo{OSFamily: OSFamilyDebian, VersionID: "12", Codename: "bookworm", PackageManager: PkgManagerAptDpkg, HostUpdateSupported: true},
		DockerEngine: &DockerEngineInfo{Available: true, Version: "24.0.9"},
		Packages:     []OSPackage{{Name: "openssh-server", SourceName: "openssh", Version: "1:9.2p1-2+deb12u2"}},
	})
	if err != nil {
		t.Fatalf("CorrelateAssets failed: %v", err)
	}
	if len(rep.VulnerableFindings) < 2 {
		t.Fatalf("expected NVD + GHSA findings, got %+v", rep.VulnerableFindings)
	}
}
