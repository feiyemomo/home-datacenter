package security

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

var (
	ErrInvalidIP     = errors.New("无效的 IP 地址格式")
	ErrLoopbackIP    = errors.New("不能封禁回环地址")
	ErrWhitelistedIP = errors.New("该 IP 在白名单中，禁止封禁")
	ErrSelfIP        = errors.New("不能封禁当前管理员操作 IP")
)

const (
	DefaultAlertCooldown = 10 * time.Minute
)

// FailedAttempt tracks the consecutive failure window for an IP.
type FailedAttempt struct {
	Count     int       `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Reason    string    `json:"reason"`
}

// BanEntry represents an actively banned IP.
type BanEntry struct {
	ID        uint           `json:"id,omitempty"`
	IP        string         `json:"ip"`
	Reason    string         `json:"reason"`
	Source    string         `json:"source"` // "auto" | "manual"
	BannedAt  time.Time      `json:"banned_at"`
	ExpiresAt time.Time      `json:"expires_at"`
	Permanent bool           `json:"permanent"`
	HitCount  int64          `json:"hit_count"`
	LastHitAt utils.NullTime `json:"last_hit_at"`
}

// PendingAttempt summarizes an IP currently accumulating failures.
type PendingAttempt struct {
	IP            string    `json:"ip"`
	Count         int       `json:"count"`
	WindowLeftSec int64     `json:"window_left_sec"`
	FirstSeen     time.Time `json:"first_seen"`
	LastSeen      time.Time `json:"last_seen"`
	Reason        string    `json:"reason"`
}

// TopAttacker summarizes the most active blocked IPs.
type TopAttacker struct {
	IP        string         `json:"ip"`
	Hits      int64          `json:"hits"`
	LastHitAt utils.NullTime `json:"last_hit_at"`
	Reason    string         `json:"reason"`
}

// DefenseConfigSummary exposes active runtime parameters.
type DefenseConfigSummary struct {
	MaxAuthFailures   int      `json:"max_auth_failures"`
	FailureWindowMins int      `json:"failure_window_mins"`
	BanDurationMins   int      `json:"ban_duration_mins"`
	Whitelist         []string `json:"whitelist"`
}

// DefenseOverview provides summary metrics for dashboard visualization.
//
// The three counters deliberately measure different things and are NEVER
// added together:
//
//	TotalAuthFailures -> detection: failed authentications that were observed.
//	TotalBlockedHits  -> enforcement: requests rejected because the source IP
//	                     was already banned at the time of the request.
//	TotalBannedCount  -> the size of the active blocklist right now.
type DefenseOverview struct {
	TotalBannedCount  int                  `json:"total_banned_count"`
	TotalBlockedHits  int64                `json:"total_blocked_hits"`
	TotalAuthFailures int64                `json:"total_auth_failures"`
	ActiveBans        []BanEntry           `json:"active_bans"`
	SMTPEnabled       bool                 `json:"smtp_enabled"`
	SenderEmail       string               `json:"sender_email"`
	SMTPHost          string               `json:"smtp_host"`
	Recipient         string               `json:"recipient"`
	Recipients        []string             `json:"recipients"`
	PendingAttempts   []PendingAttempt     `json:"pending_attempts"`
	BansToday         int64                `json:"bans_today"`
	Bans7d            int64                `json:"bans_7d"`
	AuthFails24h      []int64              `json:"auth_fails_24h"`
	BlockedHits24h    []int64              `json:"blocked_hits_24h"`
	TopAttackers      []TopAttacker        `json:"top_attackers"`
	ByType            map[string]int64     `json:"by_type"`
	Config            DefenseConfigSummary `json:"config"`
	LastEventAt       *time.Time           `json:"last_event_at"`
}

// hourlyDelta holds not-yet-flushed per-hour counters.
type hourlyDelta struct {
	AuthFails int64
	Blocked   int64
}

// DefenseManager coordinates attack detection, IP banning, persistence, and alert dispatch.
type DefenseManager struct {
	db       *gorm.DB
	cfg      config.DefenseConfig
	alertMgr *AlertManager
	bus      *eventbus.Bus

	mu             sync.RWMutex
	failures       map[string]*FailedAttempt
	banned         map[string]BanEntry
	whitelistNets  []*net.IPNet
	whitelistAddrs []net.IP
	dynamicWL      []string

	alertCooldown map[string]time.Time
	blockedHits   int64

	// hourlyMu guards hourlyStat, which buffers the current hour's counters
	// until the janitor flushes them into security_hourly_stats.
	hourlyMu   sync.Mutex
	hourlyStat map[string]*hourlyDelta
}

// NewDefenseManager initializes defense state, loads persisted bans & whitelists, and starts janitor loop.
func NewDefenseManager(db *gorm.DB, cfg config.DefenseConfig, alertMgr *AlertManager, bus *eventbus.Bus) *DefenseManager {
	dm := &DefenseManager{
		db:            db,
		cfg:           cfg,
		alertMgr:      alertMgr,
		bus:           bus,
		failures:      make(map[string]*FailedAttempt),
		banned:        make(map[string]BanEntry),
		alertCooldown: make(map[string]time.Time),
		hourlyStat:    make(map[string]*hourlyDelta),
	}

	dm.parseWhitelist(cfg.WhitelistIPs)
	dm.loadPersistedState()
	go dm.janitorLoop()
	return dm
}

func (dm *DefenseManager) loadPersistedState() {
	if dm.db == nil {
		return
	}

	// 1. Load dynamic whitelist from DB
	var dbWhitelists []model.SecurityWhitelist
	if err := dm.db.Find(&dbWhitelists).Error; err == nil {
		for _, w := range dbWhitelists {
			dm.dynamicWL = append(dm.dynamicWL, w.Entry)
		}
		dm.parseWhitelist(dm.dynamicWL)
	}

	// 2. Load unexpired or permanent bans
	now := time.Now()
	var dbBans []model.IPBan
	if err := dm.db.Where("permanent = ? OR expires_at > ?", true, now).Find(&dbBans).Error; err == nil {
		dm.mu.Lock()
		for _, b := range dbBans {
			dm.banned[b.IP] = BanEntry{
				ID:        b.ID,
				IP:        b.IP,
				Reason:    b.Reason,
				Source:    b.Source,
				BannedAt:  b.BannedAt,
				ExpiresAt: b.ExpiresAt,
				Permanent: b.Permanent,
				HitCount:  b.HitCount,
				LastHitAt: b.LastHitAt,
			}
		}
		dm.mu.Unlock()
		log.Printf("security: loaded %d active bans from database", len(dbBans))
	}

	// 3. Restore the cumulative post-ban interception counter.
	//
	// This is sourced from security_hourly_stats.blocked rather than
	// SUM(ip_bans.hit_count): the janitor deletes expired temporary bans, so
	// their hit counts would silently disappear and the cumulative number would
	// appear to go backwards.
	var totalBlocked int64
	if err := dm.db.Model(&model.SecurityHourlyStat{}).
		Select("COALESCE(SUM(blocked), 0)").Scan(&totalBlocked).Error; err == nil {
		atomic.StoreInt64(&dm.blockedHits, totalBlocked)
	}

	// 4. Seed the hourly table from historical security_events on first boot so
	// the 24h charts are not empty right after this feature lands.
	dm.backfillHourlyStats()
}

// backfillHourlyStats reconstructs the AuthFails series from security_events the
// first time the hourly table is introduced. Post-ban "blocked" counts cannot be
// reconstructed from history, so those start from zero.
func (dm *DefenseManager) backfillHourlyStats() {
	var existing int64
	if err := dm.db.Model(&model.SecurityHourlyStat{}).Count(&existing).Error; err != nil || existing > 0 {
		return
	}

	type bucketRow struct {
		Bucket string
		Count  int64
	}
	var rows []bucketRow
	// created_at is stored as "YYYY-MM-DD HH:MM:SS...+08:00"; substr() avoids
	// depending on SQLite's date parser accepting the full layout.
	if err := dm.db.Raw(`
		SELECT substr(created_at, 1, 10) || 'T' || substr(created_at, 12, 2) AS bucket,
		       COUNT(*) AS count
		FROM security_events
		WHERE type = 'auth_fail' AND created_at >= ?
		GROUP BY bucket
	`, time.Now().AddDate(0, 0, -2)).Scan(&rows).Error; err != nil {
		return
	}

	now := time.Now()
	for _, r := range rows {
		dm.db.Exec(`
			INSERT INTO security_hourly_stats (bucket, auth_fails, blocked, updated_at)
			VALUES (?, ?, 0, ?)
			ON CONFLICT(bucket) DO UPDATE SET auth_fails = excluded.auth_fails, updated_at = excluded.updated_at
		`, r.Bucket, r.Count, now)
	}
	log.Printf("security: backfilled %d hourly bucket(s) from historical security_events", len(rows))
}

// hourBucketKey renders the canonical hourly bucket identifier.
func hourBucketKey(t time.Time) string { return t.Format("2006-01-02T15") }

// bumpHourly buffers one counter increment for the given hour.
func (dm *DefenseManager) bumpHourly(now time.Time, blocked bool) {
	dm.hourlyMu.Lock()
	key := hourBucketKey(now)
	d := dm.hourlyStat[key]
	if d == nil {
		d = &hourlyDelta{}
		dm.hourlyStat[key] = d
	}
	if blocked {
		d.Blocked++
	} else {
		d.AuthFails++
	}
	dm.hourlyMu.Unlock()
}

// flushHourly moves buffered counters into SQLite. Called by the janitor.
func (dm *DefenseManager) flushHourly() {
	if dm.db == nil {
		return
	}

	dm.hourlyMu.Lock()
	pending := dm.hourlyStat
	dm.hourlyStat = make(map[string]*hourlyDelta)
	dm.hourlyMu.Unlock()

	now := time.Now()
	for key, d := range pending {
		if err := dm.db.Exec(`
			INSERT INTO security_hourly_stats (bucket, auth_fails, blocked, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(bucket) DO UPDATE SET
				auth_fails = auth_fails + excluded.auth_fails,
				blocked    = blocked + excluded.blocked,
				updated_at = excluded.updated_at
		`, key, d.AuthFails, d.Blocked, now).Error; err != nil {
			log.Printf("security: failed to flush hourly stats for %s: %v", key, err)
		}
	}
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
	ip := net.ParseIP(strings.TrimSpace(ipStr))
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

// ValidateBanTarget enforces self-protection: loopback, whitelists, and operator IP cannot be banned.
func (dm *DefenseManager) ValidateBanTarget(operatorIP, targetIPStr string) error {
	targetIPStr = strings.TrimSpace(targetIPStr)
	if targetIPStr == "" {
		return ErrInvalidIP
	}
	ip := net.ParseIP(targetIPStr)
	if ip == nil {
		return ErrInvalidIP
	}
	if ip.IsLoopback() {
		return ErrLoopbackIP
	}
	if dm.IsWhitelisted(targetIPStr) {
		return ErrWhitelistedIP
	}
	if operatorIP != "" && strings.TrimSpace(operatorIP) == targetIPStr {
		return ErrSelfIP
	}
	return nil
}

// IsBanned returns true if the IP is currently in the active blocklist.
func (dm *DefenseManager) IsBanned(ip string) (bool, BanEntry) {
	dm.mu.RLock()
	entry, ok := dm.banned[ip]
	dm.mu.RUnlock()

	if !ok {
		return false, BanEntry{}
	}

	now := time.Now()
	if !entry.Permanent && now.After(entry.ExpiresAt) {
		dm.mu.Lock()
		delete(dm.banned, ip)
		dm.mu.Unlock()
		if dm.db != nil {
			dm.db.Where("ip = ? AND permanent = ?", ip, false).Delete(&model.IPBan{})
		}
		return false, BanEntry{}
	}

	atomic.AddInt64(&dm.blockedHits, 1)

	dm.mu.Lock()
	if e, exists := dm.banned[ip]; exists {
		e.HitCount++
		e.LastHitAt = utils.NullTime{Time: now, Valid: true}
		dm.banned[ip] = e
	}
	dm.mu.Unlock()

	// This is a genuine enforcement event: the IP was already banned and its
	// request has just been rejected. It goes into the "blocked" series, never
	// into the "auth_fails" (detection) series.
	dm.bumpHourly(now, true)

	return true, entry
}

// get24hSeries returns two independent 24-bucket series aligned so that the
// last bucket is the current hour:
//
//	authFails -> detections (a failed authentication was observed)
//	blocked   -> enforcements (a request was rejected because the IP was
//	             already on the blocklist)
//
// Historical buckets come from security_hourly_stats; the current hour is
// topped up with whatever is still buffered in memory.
func (dm *DefenseManager) get24hSeries(now time.Time) (authFails, blocked []int64) {
	authFails = make([]int64, 24)
	blocked = make([]int64, 24)

	// Window start = the beginning of the hour 23 hours ago.
	base := now.Add(-23 * time.Hour)
	windowStart := time.Date(base.Year(), base.Month(), base.Day(), base.Hour(), 0, 0, 0, base.Location())

	indexOf := func(t time.Time) int {
		return int(t.Sub(windowStart) / time.Hour)
	}

	if dm.db != nil {
		var rows []model.SecurityHourlyStat
		if err := dm.db.Where("bucket >= ?", hourBucketKey(windowStart)).Find(&rows).Error; err == nil {
			for _, r := range rows {
				t, err := time.ParseInLocation("2006-01-02T15", r.Bucket, now.Location())
				if err != nil {
					continue
				}
				if i := indexOf(t); i >= 0 && i < 24 {
					authFails[i] += r.AuthFails
					blocked[i] += r.Blocked
				}
			}
		}
	}

	// Merge counters that have not been flushed yet.
	dm.hourlyMu.Lock()
	for key, d := range dm.hourlyStat {
		t, err := time.ParseInLocation("2006-01-02T15", key, now.Location())
		if err != nil {
			continue
		}
		if i := indexOf(t); i >= 0 && i < 24 {
			authFails[i] += d.AuthFails
			blocked[i] += d.Blocked
		}
	}
	dm.hourlyMu.Unlock()

	return authFails, blocked
}

// calculateEscalatedBan calculates the ban duration and escalation level for an IP based on past violations.
func (dm *DefenseManager) calculateEscalatedBan(ip string) (duration time.Duration, permanent bool, offenseCount int64) {
	baseDuration := time.Duration(dm.cfg.BanDurationMins) * time.Minute
	if baseDuration <= 0 {
		baseDuration = 30 * time.Minute
	}

	var pastCount int64
	if dm.db != nil {
		// Count previous auth_fail events for this IP within the past 30 days
		since := time.Now().AddDate(0, 0, -30)
		dm.db.Model(&model.SecurityEvent{}).
			Where("ip = ? AND type = 'auth_fail' AND created_at >= ?", ip, since).
			Count(&pastCount)
	}

	offenseCount = pastCount + 1

	switch {
	case offenseCount == 1:
		return baseDuration, false, offenseCount
	case offenseCount == 2:
		return 6 * time.Hour, false, offenseCount
	case offenseCount == 3:
		return 24 * time.Hour, false, offenseCount
	default:
		return 10 * 365 * 24 * time.Hour, true, offenseCount
	}
}

// RecordAuthFailure tallies authentication/API failures per IP, triggering ban & alert if threshold met.
func (dm *DefenseManager) RecordAuthFailure(ip, reason, userAgent string) {
	// If the IP is already banned, this is not a new detection — it is an
	// enforcement hit (the request is being rejected by the blocklist).
	// IsBanned() credits it to the "blocked" series for us.
	if banned, _ := dm.IsBanned(ip); banned {
		return
	}

	now := time.Now()
	isWhite := dm.IsWhitelisted(ip)

	// Detection counter: a failed authentication has been observed.
	dm.bumpHourly(now, false)

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
			Reason:    reason,
		}
		dm.failures[ip] = attempt
	} else {
		attempt.Count++
		attempt.LastSeen = now
		attempt.Reason = reason
	}
	currentCount := attempt.Count
	dm.mu.Unlock()

	maxFails := dm.cfg.MaxAuthFailures
	if maxFails <= 0 {
		maxFails = 5
	}

	if currentCount >= maxFails {
		if isWhite {
			// Whitelisted IP (such as Gateway). Do NOT ban, but alert and reset failure counter.
			log.Printf("security: [GATEWAY WARNING] Whitelisted IP %s triggered %d failures (reason=%s). Not banning.", ip, currentCount, reason)

			// Reset failure counter so subsequent failures in this window don't repeatedly fire alerts
			dm.mu.Lock()
			delete(dm.failures, ip)
			dm.mu.Unlock()

			if parsed := net.ParseIP(strings.TrimSpace(ip)); parsed != nil && parsed.IsLoopback() {
				return
			}

			// Throttle gateway warning alerts so we don't spam EventBus or database
			if !dm.shouldSendAlert(ip) {
				return
			}

			alertDetail := fmt.Sprintf("网关/白名单 IP %s 在 %v 内连续认证失败达到 %d 次！\nUser-Agent: %s\n触发原因: %s\n\n【注意】该 IP 为家庭网络网关/白名单设备，系统未予阻断网络。请核查路由器或跳板隧道连接！",
				ip, window, currentCount, userAgent, reason)

			dm.dispatchAlert("网关异常穿透攻击告警", ip, "gateway_warning", alertDetail, "白名单保护免封禁（请人工核查路由器）")
			return
		}

		// Non-whitelisted IP: Progressive escalation ban and alert!
		banDuration, permanent, offenseCount := dm.calculateEscalatedBan(ip)

		var reasonText string
		if permanent {
			reasonText = fmt.Sprintf("认证失败达到 %d 次 (累计第 %d 次严重违规，升格为永久黑名单)", currentCount, offenseCount)
		} else if offenseCount > 1 {
			reasonText = fmt.Sprintf("认证失败达到 %d 次 (第 %d 次再犯，阶梯递增封禁 %v)", currentCount, offenseCount, banDuration)
		} else {
			reasonText = fmt.Sprintf("认证失败达到 %d 次 (%s)", currentCount, reason)
		}

		banEntry := BanEntry{
			IP:        ip,
			Reason:    reasonText,
			Source:    "auto",
			BannedAt:  now,
			ExpiresAt: now.Add(banDuration),
			Permanent: permanent,
		}

		dm.mu.Lock()
		dm.banned[ip] = banEntry
		delete(dm.failures, ip)
		dm.mu.Unlock()

		if dm.db != nil {
			banModel := model.IPBan{
				IP:        ip,
				Reason:    banEntry.Reason,
				Source:    "auto",
				BannedAt:  now,
				ExpiresAt: banEntry.ExpiresAt,
				Permanent: permanent,
			}
			dm.db.Where("ip = ?", ip).Assign(banModel).FirstOrCreate(&banModel)
		}

		log.Printf("security: [AUTO-BAN] IP %s banned for %v (permanent=%v, offense#=%d) due to %d failures (reason=%s)",
			ip, banDuration, permanent, offenseCount, currentCount, reason)

		alertDetail := fmt.Sprintf("外部来源 IP %s 在 %v 内连续认证失败达到 %d 次！\n阶梯封禁级别: 第 %d 次违规\nUser-Agent: %s\n触发原因: %s",
			ip, window, currentCount, offenseCount, userAgent, reason)
		action := fmt.Sprintf("已自动触发第 %d 级阶梯封禁 (时长: %v)", offenseCount, banDuration)
		if permanent {
			action = fmt.Sprintf("已自动触发第 %d 级阶梯封禁 (升格为永久封禁)", offenseCount)
		}

		dm.dispatchAlert("拦截暴力破解侵入", ip, "auth_fail", alertDetail, action)
	}
}

// RecordAuthSuccess clears failed attempts counter upon successful login.
func (dm *DefenseManager) RecordAuthSuccess(ip string) {
	dm.mu.Lock()
	delete(dm.failures, ip)
	dm.mu.Unlock()
}

// ManualBan manually bans an IP with custom duration and reason, enforcing self-protection.
func (dm *DefenseManager) ManualBan(operatorIP, operatorUser, ip string, duration time.Duration, reason string) error {
	if err := dm.ValidateBanTarget(operatorIP, ip); err != nil {
		return err
	}

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
		Source:    "manual",
		BannedAt:  now,
		ExpiresAt: expires,
		Permanent: perm,
	}

	dm.mu.Lock()
	dm.banned[ip] = entry
	delete(dm.failures, ip)
	dm.mu.Unlock()

	if dm.db != nil {
		banModel := model.IPBan{
			IP:        ip,
			Reason:    reason,
			Source:    "manual",
			BannedAt:  now,
			ExpiresAt: expires,
			Permanent: perm,
		}
		dm.db.Where("ip = ?", ip).Assign(banModel).FirstOrCreate(&banModel)
	}

	action := fmt.Sprintf("管理员 %s 手动加入黑名单 (时长: %v)", operatorUser, duration)
	dm.dispatchAlert("管理员手动封禁 IP", ip, "manual_ban", fmt.Sprintf("管理员 %s 手动封禁: %s", operatorUser, reason), action)
	return nil
}

// ManualUnban removes an IP from the ban list.
func (dm *DefenseManager) ManualUnban(operatorUser, ip string) error {
	dm.mu.Lock()
	delete(dm.banned, ip)
	dm.mu.Unlock()

	if dm.db != nil {
		dm.db.Where("ip = ?", ip).Delete(&model.IPBan{})
	}

	log.Printf("security: IP %s unbanned by administrator %s", ip, operatorUser)
	action := fmt.Sprintf("管理员 %s 解除黑名单拦截", operatorUser)
	dm.dispatchAlert("管理员解封 IP", ip, "manual_unban", fmt.Sprintf("操作人: %s", operatorUser), action)
	return nil
}

// UpdateBan adjusts duration or permanent flag of an existing ban.
func (dm *DefenseManager) UpdateBan(ip string, duration time.Duration, permanent bool) error {
	dm.mu.Lock()
	entry, ok := dm.banned[ip]
	if !ok {
		dm.mu.Unlock()
		return fmt.Errorf("ip %s is not banned", ip)
	}
	now := time.Now()
	entry.Permanent = permanent
	if permanent {
		entry.ExpiresAt = now.AddDate(10, 0, 0)
	} else if duration > 0 {
		entry.ExpiresAt = now.Add(duration)
	}
	dm.banned[ip] = entry
	dm.mu.Unlock()

	if dm.db != nil {
		dm.db.Model(&model.IPBan{}).Where("ip = ?", ip).Updates(map[string]any{
			"permanent":  entry.Permanent,
			"expires_at": entry.ExpiresAt,
		})
	}
	return nil
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

// shouldSendAlert checks if an IP is within its 10-minute alert cooldown window.
func (dm *DefenseManager) shouldSendAlert(ip string) bool {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	now := time.Now()
	last, ok := dm.alertCooldown[ip]
	if ok && now.Sub(last) < DefaultAlertCooldown {
		return false
	}
	dm.alertCooldown[ip] = now
	return true
}

func (dm *DefenseManager) recordSecurityEvent(ip, eventType, detail, action string) {
	if dm.db != nil {
		event := model.SecurityEvent{
			IP:        ip,
			Type:      eventType,
			Detail:    detail,
			Action:    action,
			CreatedAt: time.Now(),
		}
		if err := dm.db.Create(&event).Error; err != nil {
			log.Printf("security: failed to record security_event: %v", err)
		}
	}
}

func (dm *DefenseManager) dispatchAlert(subject, ip, alertType, detail, action string) {
	// 1. Audit to security_events table
	dm.recordSecurityEvent(ip, alertType, detail, action)

	// 2. QQ SMTP email (throttled by 10-min cooldown per IP)
	if dm.alertMgr != nil && dm.shouldSendAlert(ip) {
		dm.alertMgr.SendSecurityEmail(subject, ip, alertType, detail, action)
	}

	// 3. Publish to EventBus for WebSocket dashboard and SystemLog persistence
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

// GetOverview returns real-time metrics, active bans, observability breakdown, and config summary.
func (dm *DefenseManager) GetOverview() DefenseOverview {
	bannedList := dm.ListBanned()
	smtpCfg := dm.alertMgr.GetConfig()
	now := time.Now()

	// 1. Pending attempts in active window
	window := time.Duration(dm.cfg.FailureWindowMins) * time.Minute
	if window <= 0 {
		window = 5 * time.Minute
	}

	dm.mu.RLock()
	pending := make([]PendingAttempt, 0, len(dm.failures))
	for ip, fa := range dm.failures {
		elapsed := now.Sub(fa.FirstSeen)
		leftSec := int64((window - elapsed).Seconds())
		if leftSec < 0 {
			leftSec = 0
		}
		pending = append(pending, PendingAttempt{
			IP:            ip,
			Count:         fa.Count,
			WindowLeftSec: leftSec,
			FirstSeen:     fa.FirstSeen,
			LastSeen:      fa.LastSeen,
			Reason:        fa.Reason,
		})
	}
	authFails24h, blocked24h := dm.get24hSeries(now)
	dm.mu.RUnlock()
	// 2. Database aggregates (today bans, 7d bans, total detections, top attackers, event breakdown)
	var bansToday int64
	var bans7d int64
	var totalAuthFailures int64
	var lastEventAt *time.Time
	byType := make(map[string]int64)
	topAttackers := make([]TopAttacker, 0)

	if dm.db != nil {
		startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		sevenDaysAgo := now.AddDate(0, 0, -7)

		dm.db.Model(&model.SecurityEvent{}).Where("type IN ('auth_fail', 'manual_ban') AND created_at >= ?", startOfDay).Count(&bansToday)
		dm.db.Model(&model.SecurityEvent{}).Where("type IN ('auth_fail', 'manual_ban') AND created_at >= ?", sevenDaysAgo).Count(&bans7d)

		// Detection total = every observed failed authentication. Sourced from the
		// per-hour counters (which increment once per failure) rather than from
		// security_events, because that table only stores ONE row per threshold
		// trip — it cannot represent the individual attempts.
		var persistedAuthFails int64
		dm.db.Model(&model.SecurityHourlyStat{}).
			Select("COALESCE(SUM(auth_fails), 0)").Scan(&persistedAuthFails)
		totalAuthFailures = persistedAuthFails

		// Top attackers aggregated from security_events for real attack attempts
		type topAgg struct {
			IP       string
			Hits     int64
			LastSeen string
			Detail   string
		}
		var taggs []topAgg
		dm.db.Model(&model.SecurityEvent{}).
			Select("ip, count(*) as hits, max(created_at) as last_seen, max(detail) as detail").
			Where("type = 'auth_fail' AND ip != '' AND ip NOT IN ('127.0.0.1', '::1')").
			Group("ip").
			Order("hits DESC").
			Limit(5).
			Scan(&taggs)

		for _, ta := range taggs {
			var t time.Time
			if ta.LastSeen != "" {
				t, _ = time.Parse(time.RFC3339Nano, ta.LastSeen)
				if t.IsZero() {
					t, _ = time.Parse("2006-01-02 15:04:05.999999999-07:00", ta.LastSeen)
				}
				if t.IsZero() {
					t, _ = time.Parse("2006-01-02 15:04:05", ta.LastSeen)
				}
			}
			topAttackers = append(topAttackers, TopAttacker{
				IP:        ta.IP,
				Hits:      ta.Hits,
				LastHitAt: utils.NullTime{Time: t, Valid: !t.IsZero()},
				Reason:    fmt.Sprintf("累计触发 %d 次暴力破解告警", ta.Hits),
			})
		}

		// Fallback to ip_bans if security_events has no auth_fail records
		if len(topAttackers) == 0 {
			var topBans []model.IPBan
			dm.db.Model(&model.IPBan{}).Order("hit_count DESC, banned_at DESC").Limit(5).Find(&topBans)
			for _, tb := range topBans {
				topAttackers = append(topAttackers, TopAttacker{
					IP:        tb.IP,
					Hits:      tb.HitCount,
					LastHitAt: tb.LastHitAt,
					Reason:    tb.Reason,
				})
			}
		}

		// By type breakdown (past 7 days)
		type typeCount struct {
			Type  string
			Count int64
		}
		var tc []typeCount
		dm.db.Model(&model.SecurityEvent{}).
			Select("type, count(*) as count").
			Where("created_at >= ?", sevenDaysAgo).
			Group("type").
			Scan(&tc)
		for _, item := range tc {
			byType[item.Type] = item.Count
		}

		// Last event timestamp
		var lastEvent model.SecurityEvent
		if err := dm.db.Order("created_at DESC").First(&lastEvent).Error; err == nil {
			lastEventAt = &lastEvent.CreatedAt
		}
	}

	// 3. Fold in detection counters that the janitor has not flushed yet.
	dm.hourlyMu.Lock()
	for _, d := range dm.hourlyStat {
		totalAuthFailures += d.AuthFails
	}
	dm.hourlyMu.Unlock()

	// 4. Combined whitelist strings
	allWhitelist := make([]string, 0, len(dm.cfg.WhitelistIPs)+len(dm.dynamicWL))
	allWhitelist = append(allWhitelist, dm.cfg.WhitelistIPs...)
	allWhitelist = append(allWhitelist, dm.dynamicWL...)

	return DefenseOverview{
		TotalBannedCount: len(bannedList),
		// Enforcement only: requests rejected *because* the source IP was
		// already blocklisted. Detection counts are reported separately below.
		TotalBlockedHits:  atomic.LoadInt64(&dm.blockedHits),
		TotalAuthFailures: totalAuthFailures,
		ActiveBans:        bannedList,
		SMTPEnabled:       smtpCfg.Enabled,
		SenderEmail:       smtpCfg.Username,
		SMTPHost:          smtpCfg.Host,
		Recipient:         smtpCfg.Recipient,
		Recipients:        dm.alertMgr.GetRecipients(),
		PendingAttempts:   pending,
		BansToday:         bansToday,
		Bans7d:            bans7d,
		AuthFails24h:      authFails24h,
		BlockedHits24h:    blocked24h,
		TopAttackers:      topAttackers,
		ByType:            byType,
		Config: DefenseConfigSummary{
			MaxAuthFailures:   dm.cfg.MaxAuthFailures,
			FailureWindowMins: dm.cfg.FailureWindowMins,
			BanDurationMins:   dm.cfg.BanDurationMins,
			Whitelist:         allWhitelist,
		},
		LastEventAt: lastEventAt,
	}
}

// ListEvents queries the security events timeline with optional filtering by type, ip, and keyword.
func (dm *DefenseManager) ListEvents(limit int, eventType, ip, keyword string) ([]model.SecurityEvent, error) {
	if dm.db == nil {
		return []model.SecurityEvent{}, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := dm.db.Model(&model.SecurityEvent{})
	if eventType != "" && eventType != "all" {
		q = q.Where("type = ?", eventType)
	}
	if ip != "" {
		q = q.Where("ip = ?", ip)
	}
	if keyword != "" {
		k := "%" + keyword + "%"
		q = q.Where("detail LIKE ? OR action LIKE ? OR ip LIKE ?", k, k, k)
	}
	var events []model.SecurityEvent
	err := q.Order("created_at DESC").Limit(limit).Find(&events).Error
	if events == nil {
		events = []model.SecurityEvent{}
	}
	return events, err
}

// PruneEvents removes security events older than the specified retention days.
func (dm *DefenseManager) PruneEvents(days int) (int64, error) {
	if dm.db == nil {
		return 0, nil
	}
	if days <= 0 {
		days = 30
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	res := dm.db.Where("created_at < ?", cutoff).Delete(&model.SecurityEvent{})
	return res.RowsAffected, res.Error
}

// RecordOperatorEvent logs an administrative audit event to the security timeline and EventBus.
func (dm *DefenseManager) RecordOperatorEvent(operatorUser, ip, eventType, detail, action string) {
	dm.dispatchAlert(fmt.Sprintf("管理员 %s: %s", operatorUser, eventType), ip, eventType, detail, action)
}

// ListWhitelist returns all whitelist records, including system-configured and dynamically added.
func (dm *DefenseManager) ListWhitelist() ([]model.SecurityWhitelist, error) {
	var list []model.SecurityWhitelist
	if dm.db != nil {
		_ = dm.db.Order("created_at DESC").Find(&list).Error
	}
	existing := make(map[string]bool)
	for _, item := range list {
		existing[item.Entry] = true
	}
	// Fold in system configured whitelist IPs
	for _, sysIP := range dm.cfg.WhitelistIPs {
		sysIP = strings.TrimSpace(sysIP)
		if sysIP != "" && !existing[sysIP] {
			list = append(list, model.SecurityWhitelist{
				Entry: sysIP,
				Note:  "系统配置内置豁免",
			})
			existing[sysIP] = true
		}
	}
	return list, nil
}

// AddWhitelist registers a dynamic whitelist exemption.
func (dm *DefenseManager) AddWhitelist(entry, note string) error {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return ErrInvalidIP
	}

	// Validate IP or CIDR
	if strings.Contains(entry, "/") {
		_, _, err := net.ParseCIDR(entry)
		if err != nil {
			return ErrInvalidIP
		}
	} else {
		if net.ParseIP(entry) == nil {
			return ErrInvalidIP
		}
	}

	if dm.db != nil {
		row := model.SecurityWhitelist{
			Entry:     entry,
			Note:      note,
			CreatedAt: time.Now(),
		}
		if err := dm.db.Create(&row).Error; err != nil {
			return err
		}
	}

	dm.mu.Lock()
	dm.dynamicWL = append(dm.dynamicWL, entry)
	dm.parseWhitelist([]string{entry})
	dm.mu.Unlock()

	dm.dispatchAlert("添加安全白名单", entry, "config_change", fmt.Sprintf("白名单添加: %s (备注: %s)", entry, note), "生效免封禁保护")
	return nil
}

// RemoveWhitelist removes a dynamic whitelist exemption.
func (dm *DefenseManager) RemoveWhitelist(entry string) error {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return ErrInvalidIP
	}

	if dm.db != nil {
		if err := dm.db.Where("entry = ?", entry).Delete(&model.SecurityWhitelist{}).Error; err != nil {
			return err
		}
	}

	dm.mu.Lock()
	newWL := make([]string, 0, len(dm.dynamicWL))
	for _, w := range dm.dynamicWL {
		if w != entry {
			newWL = append(newWL, w)
		}
	}
	dm.dynamicWL = newWL

	// Rebuild whitelist caches
	dm.whitelistAddrs = nil
	dm.whitelistNets = nil
	dm.parseWhitelist(dm.cfg.WhitelistIPs)
	dm.parseWhitelist(dm.dynamicWL)
	dm.mu.Unlock()

	dm.dispatchAlert("移除安全白名单", entry, "config_change", fmt.Sprintf("白名单移除: %s", entry), "已恢复正常安全检测")
	return nil
}

// UpdateConfig updates runtime thresholds with protective lower bounds.
func (dm *DefenseManager) UpdateConfig(maxFailures, windowMins, banDurationMins int) error {
	if maxFailures < 3 {
		return errors.New("最大失败次数下限为 3 次")
	}
	if windowMins < 1 {
		return errors.New("检测窗口最短为 1 分钟")
	}
	if banDurationMins < 1 {
		return errors.New("封禁时长最短为 1 分钟")
	}

	dm.mu.Lock()
	dm.cfg.MaxAuthFailures = maxFailures
	dm.cfg.FailureWindowMins = windowMins
	dm.cfg.BanDurationMins = banDurationMins
	dm.mu.Unlock()

	dm.dispatchAlert("更新防御中心策略配置", "系统", "config_change",
		fmt.Sprintf("阈值调整: 失败 %d 次/窗口 %d 分钟/封禁 %d 分钟", maxFailures, windowMins, banDurationMins),
		"动态热更新生效")
	return nil
}

// janitorLoop periodically sweeps expired failures, syncs hit stats, and removes expired bans.
func (dm *DefenseManager) janitorLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()

		dm.mu.Lock()
		// 1. Expire in-memory bans
		for ip, entry := range dm.banned {
			if !entry.Permanent && now.After(entry.ExpiresAt) {
				delete(dm.banned, ip)
			}
		}

		// 2. Clean expired failure windows
		window := time.Duration(dm.cfg.FailureWindowMins) * time.Minute
		if window <= 0 {
			window = 5 * time.Minute
		}
		for ip, attempt := range dm.failures {
			if now.Sub(attempt.LastSeen) > window {
				delete(dm.failures, ip)
			}
		}

		// 3. Clean alert cooldown entries older than 1 hour
		for ip, lastAlert := range dm.alertCooldown {
			if now.Sub(lastAlert) > time.Hour {
				delete(dm.alertCooldown, ip)
			}
		}

		// Copy bans for DB sync to avoid holding lock during DB I/O
		activeCopy := make([]BanEntry, 0, len(dm.banned))
		for _, b := range dm.banned {
			activeCopy = append(activeCopy, b)
		}
		dm.mu.Unlock()

		// 4. DB cleanup & hit-count flush
		if dm.db != nil {
			dm.db.Where("permanent = ? AND expires_at < ?", false, now).Delete(&model.IPBan{})
			for _, b := range activeCopy {
				if b.HitCount > 0 {
					dm.db.Model(&model.IPBan{}).Where("ip = ?", b.IP).Updates(map[string]any{
						"hit_count":   b.HitCount,
						"last_hit_at": b.LastHitAt,
					})
				}
			}

			// 5. Persist buffered per-hour detection / enforcement counters.
			dm.flushHourly()

			// 6. Drop hourly buckets older than the retention window.
			dm.db.Where("bucket < ?", hourBucketKey(now.AddDate(0, 0, -90))).
				Delete(&model.SecurityHourlyStat{})
		}
	}
}
