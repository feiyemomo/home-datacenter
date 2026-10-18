package security

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/eventbus"
)

// FailedAttempt tracks the consecutive failure window for an IP.
type FailedAttempt struct {
	Count     int
	FirstSeen time.Time
	LastSeen  time.Time
}

// BanEntry represents an actively banned IP.
type BanEntry struct {
	IP        string    `json:"ip"`
	Reason    string    `json:"reason"`
	BannedAt  time.Time `json:"banned_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Permanent bool      `json:"permanent"`
}

// DefenseOverview provides summary metrics for dashboard visualization.
type DefenseOverview struct {
	TotalBannedCount int        `json:"total_banned_count"`
	TotalBlockedHits int64      `json:"total_blocked_hits"`
	ActiveBans       []BanEntry `json:"active_bans"`
	SMTPEnabled      bool       `json:"smtp_enabled"`
	Recipient        string     `json:"recipient"`
}

// DefenseManager coordinates attack detection, IP banning, and alert dispatch.
type DefenseManager struct {
	cfg      config.DefenseConfig
	alertMgr *AlertManager
	bus      *eventbus.Bus

	mu             sync.RWMutex
	failures       map[string]*FailedAttempt
	banned         map[string]BanEntry
	whitelistNets  []*net.IPNet
	whitelistAddrs []net.IP

	blockedHits int64
}

// NewDefenseManager initializes defense state, parsing whitelists and starting janitor loop.
func NewDefenseManager(cfg config.DefenseConfig, alertMgr *AlertManager, bus *eventbus.Bus) *DefenseManager {
	dm := &DefenseManager{
		cfg:      cfg,
		alertMgr: alertMgr,
		bus:      bus,
		failures: make(map[string]*FailedAttempt),
		banned:   make(map[string]BanEntry),
	}

	dm.parseWhitelist(cfg.WhitelistIPs)
	go dm.janitorLoop()
	return dm
}

func (dm *DefenseManager) parseWhitelist(ips []string) {
	for _, item := range ips {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if strings.Contains(item, "/") {
			_, ipNet, err := net.ParseCIDR(item)
			if err == nil {
				dm.whitelistNets = append(dm.whitelistNets, ipNet)
			}
		} else {
			ip := net.ParseIP(item)
			if ip != nil {
				dm.whitelistAddrs = append(dm.whitelistAddrs, ip)
			}
		}
	}
}

// IsWhitelisted checks whether the given IP is immune to automated blocking.
func (dm *DefenseManager) IsWhitelisted(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, single := range dm.whitelistAddrs {
		if single.Equal(ip) {
			return true
		}
	}
	for _, network := range dm.whitelistNets {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// IsBanned returns true if the IP is currently in the active blocklist.
func (dm *DefenseManager) IsBanned(ip string) (bool, BanEntry) {
	dm.mu.RLock()
	entry, ok := dm.banned[ip]
	dm.mu.RUnlock()

	if !ok {
		return false, BanEntry{}
	}

	if !entry.Permanent && time.Now().After(entry.ExpiresAt) {
		dm.mu.Lock()
		delete(dm.banned, ip)
		dm.mu.Unlock()
		return false, BanEntry{}
	}

	atomic.AddInt64(&dm.blockedHits, 1)
	return true, entry
}

// RecordAuthFailure tallies authentication/API failures per IP, triggering ban & alert if threshold met.
func (dm *DefenseManager) RecordAuthFailure(ip, reason, userAgent string) {
	now := time.Now()
	isWhite := dm.IsWhitelisted(ip)

	dm.mu.Lock()
	attempt, ok := dm.failures[ip]
	window := time.Duration(dm.cfg.FailureWindowMins) * time.Minute
	if window <= 0 {
		window = 5 * time.Minute
	}

	if !ok || now.Sub(attempt.FirstSeen) > window {
		attempt = &FailedAttempt{
			Count:     1,
			FirstSeen: now,
			LastSeen:  now,
		}
		dm.failures[ip] = attempt
	} else {
		attempt.Count++
		attempt.LastSeen = now
	}
	currentCount := attempt.Count
	dm.mu.Unlock()

	maxFails := dm.cfg.MaxAuthFailures
	if maxFails <= 0 {
		maxFails = 5
	}

	if currentCount >= maxFails {
		// Threshold reached!
		if isWhite {
			// Whitelisted IP (such as Gateway 192.168.31.1). Do NOT ban network interface, but dispatch URGENT alert!
			log.Printf("security: [GATEWAY WARNING] Whitelisted IP %s triggered %d failures (reason=%s). Not banning.", ip, currentCount, reason)

			// Loopback failures can only originate on this host — the
			// healthcheck, deploy scripts, a CLI curl, s6 probes. They
			// are not the "H3C jump host is compromised" scenario this
			// alert exists for, so keep them in the log without paging
			// (and without flooding the in-app security feed).
			if parsed := net.ParseIP(strings.TrimSpace(ip)); parsed != nil && parsed.IsLoopback() {
				return
			}

			alertDetail := fmt.Sprintf("网关/白名单 IP %s 在 %v 内连续认证失败达到 %d 次！\nUser-Agent: %s\n触发原因: %s\n\n【注意】该 IP 为家庭网络网关/路由器，系统未予阻断网络。极有可能为外部渗透者经由 H3C 简优云隧道或路由器代理发起的穿透探测攻击，请立即登录路由器检查连接！",
				ip, window, currentCount, userAgent, reason)

			dm.dispatchAlert("网关异常穿透攻击告警", ip, "网关跳板暴破探测", alertDetail, "白名单保护免封禁（请人工核查路由器）")
			return
		}

		// Non-whitelisted IP: Ban and alert!
		banDuration := time.Duration(dm.cfg.BanDurationMins) * time.Minute
		if banDuration <= 0 {
			banDuration = 30 * time.Minute
		}

		banEntry := BanEntry{
			IP:        ip,
			Reason:    fmt.Sprintf("认证失败达到 %d 次 (%s)", currentCount, reason),
			BannedAt:  now,
			ExpiresAt: now.Add(banDuration),
			Permanent: false,
		}

		dm.mu.Lock()
		dm.banned[ip] = banEntry
		// Reset failure counter after ban
		delete(dm.failures, ip)
		dm.mu.Unlock()

		log.Printf("security: [AUTO-BAN] IP %s banned for %v due to %d failures (reason=%s)", ip, banDuration, currentCount, reason)

		alertDetail := fmt.Sprintf("外部来源 IP %s 在 %v 内连续认证失败达到 %d 次！\nUser-Agent: %s\n触发原因: %s",
			ip, window, currentCount, userAgent, reason)
		action := fmt.Sprintf("已自动加入黑名单，临时封禁 %v", banDuration)

		dm.dispatchAlert("拦截暴力破解侵入", ip, "连续认证失败爆破", alertDetail, action)
	}
}

// RecordAuthSuccess clears failed attempts counter upon successful login.
func (dm *DefenseManager) RecordAuthSuccess(ip string) {
	dm.mu.Lock()
	delete(dm.failures, ip)
	dm.mu.Unlock()
}

// ManualBan manually bans an IP with custom duration and reason.
func (dm *DefenseManager) ManualBan(ip string, duration time.Duration, reason string) {
	now := time.Now()
	perm := false
	expires := now.Add(duration)
	if duration <= 0 {
		perm = true
		expires = now.AddDate(10, 0, 0)
	}

	entry := BanEntry{
		IP:        ip,
		Reason:    reason,
		BannedAt:  now,
		ExpiresAt: expires,
		Permanent: perm,
	}

	dm.mu.Lock()
	dm.banned[ip] = entry
	dm.mu.Unlock()

	dm.dispatchAlert("管理员手动封禁 IP", ip, "手动安全处置", fmt.Sprintf("管理员手动封禁: %s", reason), "已加入全局拦截黑名单")
}

// ManualUnban lifts the ban from the given IP.
func (dm *DefenseManager) ManualUnban(ip string) {
	dm.mu.Lock()
	delete(dm.banned, ip)
	dm.mu.Unlock()
	log.Printf("security: IP %s unbanned by administrator", ip)
}

// ListBanned returns a copy of all actively banned IPs.
func (dm *DefenseManager) ListBanned() []BanEntry {
	dm.mu.RLock()
	defer dm.mu.RUnlock()

	list := make([]BanEntry, 0, len(dm.banned))
	now := time.Now()
	for _, entry := range dm.banned {
		if entry.Permanent || entry.ExpiresAt.After(now) {
			list = append(list, entry)
		}
	}
	return list
}

// GetOverview returns summary metrics and active bans for dashboard.
func (dm *DefenseManager) GetOverview() DefenseOverview {
	bannedList := dm.ListBanned()
	smtpCfg := dm.alertMgr.GetConfig()

	return DefenseOverview{
		TotalBannedCount: len(bannedList),
		TotalBlockedHits: atomic.LoadInt64(&dm.blockedHits),
		ActiveBans:       bannedList,
		SMTPEnabled:      smtpCfg.Enabled,
		Recipient:        smtpCfg.Recipient,
	}
}

func (dm *DefenseManager) dispatchAlert(subject, ip, alertType, detail, action string) {
	// 1. Send via QQ SMTP email
	if dm.alertMgr != nil {
		dm.alertMgr.SendSecurityEmail(subject, ip, alertType, detail, action)
	}

	// 2. Publish to EventBus for WebSocket dashboard and SystemLog persistence
	if dm.bus != nil {
		payload, _ := json.Marshal(eventbus.SecurityAlertPayload{
			IP:        ip,
			AlertType: alertType,
			Message:   fmt.Sprintf("[安全防御] %s (来源 IP: %s) - %s", subject, ip, action),
			Action:    action,
			Detail:    detail,
			Ts:        time.Now().Unix(),
		})
		dm.bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicSecurityAlert,
			Source:  eventbus.SourceSystem,
			Payload: payload,
		})
	}
}

// janitorLoop periodically sweeps expired failures and bans.
func (dm *DefenseManager) janitorLoop() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		dm.mu.Lock()
		for ip, entry := range dm.banned {
			if !entry.Permanent && now.After(entry.ExpiresAt) {
				delete(dm.banned, ip)
			}
		}
		window := time.Duration(dm.cfg.FailureWindowMins) * time.Minute
		if window <= 0 {
			window = 5 * time.Minute
		}
		for ip, attempt := range dm.failures {
			if now.Sub(attempt.LastSeen) > window {
				delete(dm.failures, ip)
			}
		}
		dm.mu.Unlock()
	}
}
