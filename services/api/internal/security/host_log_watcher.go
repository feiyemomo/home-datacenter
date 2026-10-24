package security

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// HostAuthLogEntry represents a parsed SSH/system authentication event from the NAS host.
type HostAuthLogEntry struct {
	Timestamp  time.Time `json:"timestamp"`
	IP         string    `json:"ip"`
	User       string    `json:"user"`
	Port       string    `json:"port"`
	Status     string    `json:"status"` // "failed" | "accepted" | "invalid_user"
	Service    string    `json:"service"` // "sshd"
	RawMessage string    `json:"raw_message"`
}

// HostLogWatcher coordinates real-time monitoring of host OS authentication logs
// and exports active blocklists for host-level firewall (iptables/ipset) enforcement.
type HostLogWatcher struct {
	dm             *DefenseManager
	logPath        string
	exportPath     string
	mu             sync.RWMutex
	recentEntries  []HostAuthLogEntry
	maxEntries     int
	lastOffset     int64
	isRunning      bool
	stopChan       chan struct{}
}

// Regular expressions for OpenSSH log parsing
var (
	// Failed password for invalid user admin from 198.51.100.22 port 52341 ssh2
	// Failed password for root from 203.0.113.10 port 44321 ssh2
	reFailedPassword = regexp.MustCompile(`sshd\[\d+\]: Failed password for (?:invalid user )?(\S+) from (\S+) port (\d+)`)
	// Invalid user test from 198.51.100.22 port 49123
	reInvalidUser = regexp.MustCompile(`sshd\[\d+\]: Invalid user (\S+) from (\S+) port (\d+)`)
	// Accepted password for momo from 192.168.31.100 port 55123 ssh2
	// Accepted publickey for momo from 192.168.31.100 port 55123 ssh2
	reAcceptedAuth = regexp.MustCompile(`sshd\[\d+\]: Accepted (?:password|publickey) for (\S+) from (\S+) port (\d+)`)
)

// NewHostLogWatcher creates and starts a host OS log monitoring service.
func NewHostLogWatcher(dm *DefenseManager, logPath, exportPath string) *HostLogWatcher {
	if logPath == "" {
		logPath = os.Getenv("HOST_AUTH_LOG_PATH")
	}
	if logPath == "" {
		logPath = "/host/log/auth.log"
	}
	if exportPath == "" {
		exportPath = "/data/security/blocked_ips.txt"
	}

	hw := &HostLogWatcher{
		dm:            dm,
		logPath:       logPath,
		exportPath:    exportPath,
		maxEntries:    100,
		recentEntries: make([]HostAuthLogEntry, 0, 100),
		stopChan:      make(chan struct{}),
	}

	go hw.startWatcher()
	return hw
}

// GetStatus returns the operational status and recent host authentication logs.
func (hw *HostLogWatcher) GetStatus() (bool, string, string, []HostAuthLogEntry) {
	hw.mu.RLock()
	defer hw.mu.RUnlock()

	entries := make([]HostAuthLogEntry, len(hw.recentEntries))
	copy(entries, hw.recentEntries)
	return hw.isRunning, hw.logPath, hw.exportPath, entries
}

func (hw *HostLogWatcher) startWatcher() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	// Initial export of firewall rules
	hw.ExportFirewallRules()

	for {
		select {
		case <-hw.stopChan:
			return
		case <-ticker.C:
			hw.checkAndReadLog()
		}
	}
}

func (hw *HostLogWatcher) checkAndReadLog() {
	fi, err := os.Stat(hw.logPath)
	if err != nil {
		hw.mu.Lock()
		hw.isRunning = false
		hw.mu.Unlock()
		return
	}

	hw.mu.Lock()
	hw.isRunning = true
	size := fi.Size()

	// Log rotation detected (file shrank)
	if size < hw.lastOffset {
		hw.lastOffset = 0
	}

	if size == hw.lastOffset {
		hw.mu.Unlock()
		return
	}

	file, err := os.Open(hw.logPath)
	if err != nil {
		hw.mu.Unlock()
		return
	}
	defer file.Close()

	if hw.lastOffset > 0 {
		_, _ = file.Seek(hw.lastOffset, io.SeekStart)
	}

	scanner := bufio.NewScanner(file)
	var newEntries []HostAuthLogEntry

	for scanner.Scan() {
		line := scanner.Text()
		if entry := hw.parseLogLine(line); entry != nil {
			newEntries = append(newEntries, *entry)
		}
	}

	newOffset, err := file.Seek(0, io.SeekCurrent)
	if err == nil {
		hw.lastOffset = newOffset
	}

	// Prepend new entries (newest first)
	if len(newEntries) > 0 {
		// Reverse newEntries so the very newest is at index 0
		for i, j := 0, len(newEntries)-1; i < j; i, j = i+1, j-1 {
			newEntries[i], newEntries[j] = newEntries[j], newEntries[i]
		}
		combined := append(newEntries, hw.recentEntries...)
		if len(combined) > hw.maxEntries {
			combined = combined[:hw.maxEntries]
		}
		hw.recentEntries = combined
	}
	hw.mu.Unlock()

	// Process actions outside lock
	for _, entry := range newEntries {
		hw.handleAuthEntry(entry)
	}
}

func (hw *HostLogWatcher) parseLogLine(line string) *HostAuthLogEntry {
	line = strings.TrimSpace(line)
	if line == "" || !strings.Contains(line, "sshd") {
		return nil
	}

	now := time.Now()

	// 1. Check for failed password
	if matches := reFailedPassword.FindStringSubmatch(line); len(matches) == 4 {
		return &HostAuthLogEntry{
			Timestamp:  now,
			User:       matches[1],
			IP:         matches[2],
			Port:       matches[3],
			Status:     "failed",
			Service:    "sshd",
			RawMessage: line,
		}
	}

	// 2. Check for invalid user probe
	if matches := reInvalidUser.FindStringSubmatch(line); len(matches) == 4 {
		return &HostAuthLogEntry{
			Timestamp:  now,
			User:       matches[1],
			IP:         matches[2],
			Port:       matches[3],
			Status:     "invalid_user",
			Service:    "sshd",
			RawMessage: line,
		}
	}

	// 3. Check for successful authentication
	if matches := reAcceptedAuth.FindStringSubmatch(line); len(matches) == 4 {
		return &HostAuthLogEntry{
			Timestamp:  now,
			User:       matches[1],
			IP:         matches[2],
			Port:       matches[3],
			Status:     "accepted",
			Service:    "sshd",
			RawMessage: line,
		}
	}

	return nil
}

func (hw *HostLogWatcher) handleAuthEntry(entry HostAuthLogEntry) {
	if hw.dm == nil {
		return
	}

	switch entry.Status {
	case "failed", "invalid_user":
		reason := fmt.Sprintf("NAS 宿主机 SSH 爆破探测 (目标用户: %s)", entry.User)
		hw.dm.RecordAuthFailure(entry.IP, reason, "OpenSSH-Host")
		// Update exported rules whenever new failure might cause a ban
		hw.ExportFirewallRules()

	case "accepted":
		action := fmt.Sprintf("NAS 宿主机 SSH 登录成功 (用户: %s, 端口: %s)", entry.User, entry.Port)
		hw.dm.RecordOperatorEvent(entry.User, entry.IP, "host_ssh_login", action, "宿主操作系统安全审计")
	}
}

// ExportFirewallRules exports active banned IPs for host iptables/ipset synchronization.
func (hw *HostLogWatcher) ExportFirewallRules() {
	if hw.dm == nil || hw.exportPath == "" {
		return
	}

	bannedList := hw.dm.ListBanned()
	dir := filepath.Dir(hw.exportPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Home Datacenter Firewall Sync List\n# Generated: %s\n# Total Banned IPs: %d\n\n",
		time.Now().Format("2006-01-02 15:04:05"), len(bannedList)))

	for _, b := range bannedList {
		sb.WriteString(b.IP)
		sb.WriteString("\n")
	}

	_ = os.WriteFile(hw.exportPath, []byte(sb.String()), 0644)

	// Also generate an automated sync script for host iptables/ipset
	scriptPath := filepath.Join(dir, "sync_host_firewall.sh")
	scriptContent := fmt.Sprintf(`#!/usr/bin/env bash
# ==============================================================================
# sync_host_firewall.sh — Synchronize Home Datacenter IP Blacklist to Host iptables
# Run via host root crontab: */2 * * * * %s
# ==============================================================================
IPSET_NAME="hdc_blocklist"
IP_FILE="%s"

if [ ! -f "$IP_FILE" ]; then
    exit 0
fi

# Ensure ipset exists
if ! ipset list "$IPSET_NAME" >/dev/null 2>&1; then
    ipset create "$IPSET_NAME" hash:ip hashsize 4096 maxelem 65536
    iptables -I INPUT -m set --match-set "$IPSET_NAME" src -j DROP
fi

# Flush and reload set
ipset flush "$IPSET_NAME"
while read -r ip; do
    # Skip comments and blank lines
    [[ "$ip" =~ ^#.*$ ]] && continue
    [[ -z "$ip" ]] && continue
    ipset add "$IPSET_NAME" "$ip" 2>/dev/null || true
done < "$IP_FILE"
`, scriptPath, hw.exportPath)

	_ = os.WriteFile(scriptPath, []byte(scriptContent), 0755)
}
