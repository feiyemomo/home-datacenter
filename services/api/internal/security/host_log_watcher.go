package security

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// HostAuthLogEntry represents a parsed SSH/system authentication or privilege event from the NAS host.
type HostAuthLogEntry struct {
	Timestamp  time.Time `json:"timestamp"`
	IP         string    `json:"ip"`
	User       string    `json:"user"`
	Port       string    `json:"port"`
	Status     string    `json:"status"` // "failed" | "accepted" | "invalid_user" | "sudo_exec" | "sudo_failed" | "su_event" | "user_change" | "sshd_config_event"
	Service    string    `json:"service"` // "sshd" | "sudo" | "su" | "account_mgmt"
	Severity   string    `json:"severity,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	RawMessage string    `json:"raw_message"`
}

// HostLogWatcher coordinates real-time monitoring of host OS authentication logs
// and exports active blocklists for host-level firewall (iptables/ipset/nftables) enforcement.
type HostLogWatcher struct {
	dm            *DefenseManager
	logPath       string
	exportPath    string
	mu            sync.RWMutex
	recentEntries []HostAuthLogEntry
	maxEntries    int
	lastOffset    int64
	isRunning     bool
	stopChan      chan struct{}
	stopOnce      sync.Once
	knownLoginIPs map[string]bool
}

// Regular expressions for OpenSSH, sudo, su, and account management log parsing.
var (
	// Failed password for invalid user admin from 198.51.100.22 port 52341 ssh2
	reFailedPassword = regexp.MustCompile(`sshd\[\d+\]: Failed (?:password|keyboard-interactive(?:/pam)?|publickey) for (?:invalid user )?(\S+) from (\S+) port (\d+)`)
	// Invalid user test from 198.51.100.22 port 49123
	reInvalidUser = regexp.MustCompile(`sshd\[\d+\]: Invalid user (\S+) from (\S+) port (\d+)`)
	// Connection closed by authenticating user admin 198.51.100.22 port 49123 [preauth]
	rePreauthClosed = regexp.MustCompile(`sshd\[\d+\]: Connection (?:closed|reset) by (?:authenticating|invalid) user (\S+) (\S+) port (\d+)`)
	// Accepted password/publickey/keyboard-interactive for momo from 192.168.31.100 port 55123 ssh2
	reAcceptedAuth = regexp.MustCompile(`sshd\[\d+\]: Accepted (?:password|publickey|keyboard-interactive(?:/pam)?|gssapi-with-mic) for (\S+) from (\S+) port (\d+)`)
	// Received SIGHUP; restarting / error in /etc/ssh/sshd_config
	reSSHDConfigEvent = regexp.MustCompile(`sshd\[\d+\]: (Received SIGHUP; restarting\.|.*sshd_config.*)`)
	// sudo: momo : TTY=pts/0 ; PWD=/home/momo ; USER=root ; COMMAND=/usr/bin/apt-get ...
	reSudoExec = regexp.MustCompile(`sudo(?:\[\d+\])?:\s+(\S+)\s*:.*USER=(\S+)\s*;\s*COMMAND=(.+)$`)
	// sudo: pam_unix(sudo:auth): authentication failure; ... user=momo
	reSudoFail = regexp.MustCompile(`sudo(?:\[\d+\])?:.*(?:authentication failure|NOT in sudoers|incorrect password).*?(?:user=(\S+)|^\S+:\s+(\S+))`)
	// su: (to root) momo on pts/0  OR  FAILED SU (to root) momo on pts/0
	reSuEvent = regexp.MustCompile(`su(?:\[\d+\])?:\s+(FAILED SU|\(to [^)]+\)|pam_unix\(su[^)]*\): authentication failure).*`)
	// useradd / usermod / userdel / groupadd / passwd
	reAccountMgmt = regexp.MustCompile(`(useradd|usermod|userdel|groupadd|groupmod|deluser|adduser|chpasswd)\[\d+\]:\s+(.+)$`)
	// Syslog timestamp prefix e.g. "Oct 25 01:30:15"
	reSyslogTS = regexp.MustCompile(`^([A-Z][a-z]{2}\s+\d{1,2}\s+\d{2}:\d{2}:\d{2})\s+`)
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

	var initialOffset int64
	if fi, err := os.Stat(logPath); err == nil {
		initialOffset = fi.Size()
	}

	hw := &HostLogWatcher{
		dm:            dm,
		logPath:       logPath,
		exportPath:    exportPath,
		maxEntries:    100,
		recentEntries: make([]HostAuthLogEntry, 0, 100),
		lastOffset:    initialOffset,
		stopChan:      make(chan struct{}),
		knownLoginIPs: make(map[string]bool),
	}

	go hw.startWatcher()
	return hw
}

// Stop gracefully shuts down the log watcher goroutine.
func (hw *HostLogWatcher) Stop() {
	if hw == nil {
		return
	}
	hw.stopOnce.Do(func() {
		close(hw.stopChan)
	})
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

func parseLogLineTimestamp(line string) time.Time {
	// 1. Check RFC3339 / ISO-8601 prefix (journald / rsyslog modern format)
	if firstSpace := strings.IndexByte(line, ' '); firstSpace > 10 {
		tok := line[:firstSpace]
		if t, err := time.Parse(time.RFC3339Nano, tok); err == nil {
			return t
		}
		if t, err := time.Parse(time.RFC3339, tok); err == nil {
			return t
		}
	}
	// 2. Traditional syslog timestamp: "Oct 25 01:30:15"
	if m := reSyslogTS.FindStringSubmatch(line); len(m) == 2 {
		now := time.Now()
		normalized := strings.Join(strings.Fields(m[1]), " ")
		if t, err := time.ParseInLocation("Jan 2 15:04:05", normalized, now.Location()); err == nil {
			return time.Date(now.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, now.Location())
		}
	}
	return time.Now()
}

func (hw *HostLogWatcher) parseLogLine(line string) *HostAuthLogEntry {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}

	ts := parseLogLineTimestamp(line)

	if strings.Contains(line, "sshd") {
		if matches := reFailedPassword.FindStringSubmatch(line); len(matches) == 4 {
			return &HostAuthLogEntry{
				Timestamp:  ts,
				User:       matches[1],
				IP:         matches[2],
				Port:       matches[3],
				Status:     "failed",
				Service:    "sshd",
				Severity:   "warning",
				RawMessage: line,
			}
		}
		if matches := reInvalidUser.FindStringSubmatch(line); len(matches) == 4 {
			return &HostAuthLogEntry{
				Timestamp:  ts,
				User:       matches[1],
				IP:         matches[2],
				Port:       matches[3],
				Status:     "invalid_user",
				Service:    "sshd",
				Severity:   "warning",
				RawMessage: line,
			}
		}
		if matches := rePreauthClosed.FindStringSubmatch(line); len(matches) == 4 {
			return &HostAuthLogEntry{
				Timestamp:  ts,
				User:       matches[1],
				IP:         matches[2],
				Port:       matches[3],
				Status:     "failed",
				Service:    "sshd",
				Severity:   "warning",
				Detail:     "preauth connection closed/reset",
				RawMessage: line,
			}
		}
		if matches := reAcceptedAuth.FindStringSubmatch(line); len(matches) == 4 {
			sev := "info"
			if matches[1] == "root" {
				sev = "critical"
			}
			return &HostAuthLogEntry{
				Timestamp:  ts,
				User:       matches[1],
				IP:         matches[2],
				Port:       matches[3],
				Status:     "accepted",
				Service:    "sshd",
				Severity:   sev,
				RawMessage: line,
			}
		}
		if matches := reSSHDConfigEvent.FindStringSubmatch(line); len(matches) == 2 {
			return &HostAuthLogEntry{
				Timestamp:  ts,
				IP:         "127.0.0.1",
				Status:     "sshd_config_event",
				Service:    "sshd",
				Severity:   "warning",
				Detail:     matches[1],
				RawMessage: line,
			}
		}
		return nil
	}

	if strings.Contains(line, "sudo") {
		if matches := reSudoExec.FindStringSubmatch(line); len(matches) == 4 {
			return &HostAuthLogEntry{
				Timestamp:  ts,
				IP:         "127.0.0.1",
				User:       matches[1],
				Status:     "sudo_exec",
				Service:    "sudo",
				Severity:   "info",
				Detail:     fmt.Sprintf("target_user=%s cmd=%s", matches[2], strings.TrimSpace(matches[3])),
				RawMessage: line,
			}
		}
		if matches := reSudoFail.FindStringSubmatch(line); len(matches) >= 2 {
			u := matches[1]
			if u == "" && len(matches) > 2 {
				u = matches[2]
			}
			return &HostAuthLogEntry{
				Timestamp:  ts,
				IP:         "127.0.0.1",
				User:       u,
				Status:     "sudo_failed",
				Service:    "sudo",
				Severity:   "warning",
				Detail:     "sudo authentication/policy failure",
				RawMessage: line,
			}
		}
		return nil
	}

	if reSuEvent.MatchString(line) {
		sev := "info"
		if strings.Contains(line, "FAILED") || strings.Contains(line, "authentication failure") {
			sev = "warning"
		}
		return &HostAuthLogEntry{
			Timestamp:  ts,
			IP:         "127.0.0.1",
			Status:     "su_event",
			Service:    "su",
			Severity:   sev,
			Detail:     line,
			RawMessage: line,
		}
	}

	if matches := reAccountMgmt.FindStringSubmatch(line); len(matches) == 3 {
		return &HostAuthLogEntry{
			Timestamp:  ts,
			IP:         "127.0.0.1",
			Status:     "user_change",
			Service:    "account_mgmt",
			Severity:   "critical",
			Detail:     fmt.Sprintf("%s: %s", matches[1], matches[2]),
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
		ipStr := strings.TrimSpace(entry.IP)
		parsedIP := net.ParseIP(ipStr)
		if parsedIP == nil || parsedIP.IsLoopback() {
			return
		}
		if banned, _ := hw.dm.IsBanned(ipStr); banned {
			return
		}
		reason := fmt.Sprintf("NAS 宿主机 SSH 爆破探测 (目标用户: %s)", entry.User)
		hw.dm.RecordAuthFailure(ipStr, reason, "OpenSSH-Host")
		hw.ExportFirewallRules()

	case "accepted":
		ipStr := strings.TrimSpace(entry.IP)
		parsedIP := net.ParseIP(ipStr)
		if parsedIP == nil || parsedIP.IsLoopback() {
			return
		}
		hw.mu.Lock()
		isNewIP := len(hw.knownLoginIPs) > 0 && !hw.knownLoginIPs[ipStr]
		hw.knownLoginIPs[ipStr] = true
		hw.mu.Unlock()

		if entry.User == "root" {
			hw.dm.RecordOperatorEvent(entry.User, ipStr, "host_ssh_root_login",
				fmt.Sprintf("警告: 宿主机 root 用户 SSH 直接登录成功 (来源 IP: %s, 端口: %s)", ipStr, entry.Port),
				"高危宿主机特权登录告警")
		} else if isNewIP {
			hw.dm.RecordOperatorEvent(entry.User, ipStr, "host_ssh_anomalous_login",
				fmt.Sprintf("宿主机 SSH 从新来源 IP 登录成功 (用户: %s, IP: %s, 端口: %s)", entry.User, ipStr, entry.Port),
				"异常来源 SSH 登录审计")
		} else {
			action := fmt.Sprintf("NAS 宿主机 SSH 登录成功 (用户: %s, 端口: %s)", entry.User, entry.Port)
			hw.dm.RecordOperatorEvent(entry.User, ipStr, "host_ssh_login", action, "宿主操作系统安全审计")
		}

	case "sudo_failed":
		hw.dm.RecordOperatorEvent(entry.User, "127.0.0.1", "host_sudo_failure",
			fmt.Sprintf("宿主机 sudo 提权认证失败 (用户: %s): %s", entry.User, entry.Detail),
			"特权提权异常审计")

	case "su_event":
		if entry.Severity == "warning" {
			hw.dm.RecordOperatorEvent("system", "127.0.0.1", "host_su_failure",
				fmt.Sprintf("宿主机 su 切换失败: %s", entry.Detail), "特权切换异常审计")
		}

	case "user_change":
		hw.dm.RecordOperatorEvent("system", "127.0.0.1", "host_user_change",
			fmt.Sprintf("宿主机账号/用户组变更事件: %s", entry.Detail), "宿主机账号完整性告警")

	case "sshd_config_event":
		hw.dm.RecordOperatorEvent("system", "127.0.0.1", "host_sshd_reload",
			fmt.Sprintf("宿主机 sshd 配置重载或异常: %s", entry.Detail), "SSH 服务配置监控")
	}
}

// ExportFirewallRules exports active banned IPs (validated IPv4/IPv6/CIDR)
// for host iptables/ip6tables/ipset synchronization atomically.
func (hw *HostLogWatcher) ExportFirewallRules() {
	if hw.dm == nil || hw.exportPath == "" {
		return
	}

	bannedList := hw.dm.ListBanned()
	dir := filepath.Dir(hw.exportPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}

	var validIPs []string
	for _, b := range bannedList {
		raw := strings.TrimSpace(b.IP)
		if raw == "" || strings.ContainsAny(raw, "\r\n\t ;|&$`") {
			continue
		}
		if strings.Contains(raw, "/") {
			if ip, ipNet, err := net.ParseCIDR(raw); err == nil && !ip.IsLoopback() {
				validIPs = append(validIPs, ipNet.String())
			}
			continue
		}
		if ip := net.ParseIP(raw); ip != nil && !ip.IsLoopback() {
			validIPs = append(validIPs, ip.String())
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Home Datacenter Firewall Sync List\n# Generated: %s\n# Total Banned IPs: %d\n\n",
		time.Now().Format("2006-01-02 15:04:05"), len(validIPs)))

	for _, ip := range validIPs {
		sb.WriteString(ip)
		sb.WriteString("\n")
	}

	tmpExport := hw.exportPath + ".tmp"
	if err := os.WriteFile(tmpExport, []byte(sb.String()), 0644); err == nil {
		_ = os.Rename(tmpExport, hw.exportPath)
	}

	// Generate dual-stack (IPv4 + IPv6 + CIDR hash:net) sync script
	scriptPath := filepath.Join(dir, "sync_host_firewall.sh")
	scriptContent := fmt.Sprintf(`#!/usr/bin/env bash
# ==============================================================================
# sync_host_firewall.sh — Synchronize Home Datacenter IP Blacklist to Host Firewall
# Supports IPv4 + IPv6 + CIDR via hash:net ipset + iptables/ip6tables
# Run via host root crontab: */2 * * * * %s
# ==============================================================================
set -euo pipefail
IPSET_V4="hdc_blocklist"
IPSET_V6="hdc_blocklist_v6"
IP_FILE="%s"

if [ ! -f "$IP_FILE" ]; then
    exit 0
fi

if ! ipset list "$IPSET_V4" >/dev/null 2>&1; then
    ipset create "$IPSET_V4" hash:net family inet hashsize 4096 maxelem 65536
fi
if ! iptables -C INPUT -m set --match-set "$IPSET_V4" src -j DROP >/dev/null 2>&1; then
    iptables -I INPUT -m set --match-set "$IPSET_V4" src -j DROP
fi

if command -v ip6tables >/dev/null 2>&1; then
    if ! ipset list "$IPSET_V6" >/dev/null 2>&1; then
        ipset create "$IPSET_V6" hash:net family inet6 hashsize 4096 maxelem 65536
    fi
    if ! ip6tables -C INPUT -m set --match-set "$IPSET_V6" src -j DROP >/dev/null 2>&1; then
        ip6tables -I INPUT -m set --match-set "$IPSET_V6" src -j DROP
    fi
fi

ipset flush "$IPSET_V4"
ipset flush "$IPSET_V6" 2>/dev/null || true
while read -r ip; do
    [[ "$ip" =~ ^#.*$ ]] && continue
    [[ -z "$ip" ]] && continue
    if [[ "$ip" == *:* ]]; then
        ipset add "$IPSET_V6" "$ip" 2>/dev/null || true
    else
        ipset add "$IPSET_V4" "$ip" 2>/dev/null || true
    fi
done < "$IP_FILE"
`, scriptPath, hw.exportPath)

	tmpScript := scriptPath + ".tmp"
	if err := os.WriteFile(tmpScript, []byte(scriptContent), 0755); err == nil {
		_ = os.Rename(tmpScript, scriptPath)
	}
}
