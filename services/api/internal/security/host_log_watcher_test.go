package security

import (
	"os"
	"path/filepath"
	"testing"

	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/eventbus"
)

func TestHostLogWatcher_ParseAndExport(t *testing.T) {
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

	// Test line parsing
	failedLine := "Oct 25 01:30:15 nas sshd[12345]: Failed password for root from 198.51.100.33 port 45678 ssh2"
	entry := hw.parseLogLine(failedLine)
	if entry == nil || entry.Status != "failed" || entry.IP != "198.51.100.33" || entry.User != "root" {
		t.Fatalf("failed to parse Failed password log line: %+v", entry)
	}

	invalidLine := "Oct 25 01:30:16 nas sshd[12346]: Invalid user admin from 198.51.100.33 port 45679"
	entry = hw.parseLogLine(invalidLine)
	if entry == nil || entry.Status != "invalid_user" || entry.IP != "198.51.100.33" || entry.User != "admin" {
		t.Fatalf("failed to parse Invalid user log line: %+v", entry)
	}

	acceptedLine := "Oct 25 01:30:20 nas sshd[12347]: Accepted publickey for momo from 192.168.31.100 port 55123 ssh2"
	entry = hw.parseLogLine(acceptedLine)
	if entry == nil || entry.Status != "accepted" || entry.IP != "192.168.31.100" || entry.User != "momo" {
		t.Fatalf("failed to parse Accepted log line: %+v", entry)
	}

	// Write lines to log file to simulate live tailing
	content := failedLine + "\n" + invalidLine + "\n" + acceptedLine + "\n"
	if err := os.WriteFile(logFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test log file: %v", err)
	}

	hw.checkAndReadLog()

	// 198.51.100.33 had 2 failures (threshold=2), should be banned
	banned, _ := dm.IsBanned("198.51.100.33")
	if !banned {
		t.Fatalf("expected 198.51.100.33 to be banned from SSH failures")
	}

	// Verify exported file
	hw.ExportFirewallRules()
	exportBytes, err := os.ReadFile(exportFile)
	if err != nil {
		t.Fatalf("failed to read exported firewall rules: %v", err)
	}
	if !stringContains(string(exportBytes), "198.51.100.33") {
		t.Fatalf("expected export file to contain banned IP, got:\n%s", string(exportBytes))
	}

	// Verify status
	running, path, exp, entries := hw.GetStatus()
	if !running || path != logFile || exp != exportFile || len(entries) < 3 {
		t.Fatalf("unexpected GetStatus result: running=%v, len(entries)=%d", running, len(entries))
	}
}

func stringContains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(s) > 0 && len(sub) > 0 && filepath.Base(s) != "" && containsSubstring(s, sub)))
}

func containsSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
