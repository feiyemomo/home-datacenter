package security

import (
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

func setupTestDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open in-memory sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.IPBan{}, &model.SecurityEvent{}, &model.SecurityWhitelist{}, &model.SecurityHourlyStat{}); err != nil {
		t.Fatalf("failed to migrate schema: %v", err)
	}
	return db
}

func TestDefenseManager_AutoBanAndPersistence(t *testing.T) {
	db := setupTestDB(t)
	bus := eventbus.New()
	alertMgr := NewAlertManager(config.AlertConfig{Enabled: false})

	defenseCfg := config.DefenseConfig{
		MaxAuthFailures:   3,
		FailureWindowMins: 5,
		BanDurationMins:   10,
		WhitelistIPs:      []string{"127.0.0.1", "192.168.31.0/24"},
	}

	dm := NewDefenseManager(db, defenseCfg, alertMgr, bus)

	testExternalIP := "203.0.113.50"
	testGatewayIP := "192.168.31.1"

	// 1. External IP failures
	dm.RecordAuthFailure(testExternalIP, "test fail 1", "curl/7.0")
	dm.RecordAuthFailure(testExternalIP, "test fail 2", "curl/7.0")

	banned, _ := dm.IsBanned(testExternalIP)
	if banned {
		t.Fatalf("expected IP %s not to be banned after 2 attempts", testExternalIP)
	}

	// 3rd failure reaches threshold
	dm.RecordAuthFailure(testExternalIP, "test fail 3", "curl/7.0")

	banned, entry := dm.IsBanned(testExternalIP)
	if !banned {
		t.Fatalf("expected IP %s to be banned after 3 attempts", testExternalIP)
	}
	if entry.IP != testExternalIP {
		t.Fatalf("expected entry IP %s, got %s", testExternalIP, entry.IP)
	}

	// Verify persistence in DB
	var dbBan model.IPBan
	if err := db.Where("ip = ?", testExternalIP).First(&dbBan).Error; err != nil {
		t.Fatalf("expected ban for %s to be persisted in DB, got error: %v", testExternalIP, err)
	}
	if dbBan.IP != testExternalIP || dbBan.Source != "auto" {
		t.Fatalf("persisted ban mismatch: %+v", dbBan)
	}

	// 2. Restart simulation: create a new DefenseManager instance on the same DB
	dm2 := NewDefenseManager(db, defenseCfg, alertMgr, bus)
	banned2, entry2 := dm2.IsBanned(testExternalIP)
	if !banned2 {
		t.Fatalf("expected IP %s to still be banned after DM restart", testExternalIP)
	}
	if entry2.IP != testExternalIP {
		t.Fatalf("expected restarted entry IP %s, got %s", testExternalIP, entry2.IP)
	}

	// 3. Gateway IP (whitelisted)
	dm.RecordAuthFailure(testGatewayIP, "gateway probe 1", "python/3.10")
	dm.RecordAuthFailure(testGatewayIP, "gateway probe 2", "python/3.10")
	dm.RecordAuthFailure(testGatewayIP, "gateway probe 3", "python/3.10")
	dm.RecordAuthFailure(testGatewayIP, "gateway probe 4", "python/3.10")

	gwBanned, _ := dm.IsBanned(testGatewayIP)
	if gwBanned {
		t.Fatalf("gateway IP %s is whitelisted and MUST NEVER be banned", testGatewayIP)
	}

	// 4. Manual unban
	if err := dm.ManualUnban("admin", testExternalIP); err != nil {
		t.Fatalf("failed manual unban: %v", err)
	}
	bannedAfterUnban, _ := dm.IsBanned(testExternalIP)
	if bannedAfterUnban {
		t.Fatalf("expected IP %s to be unbanned after ManualUnban", testExternalIP)
	}
	// Verify deleted from DB
	var count int64
	db.Model(&model.IPBan{}).Where("ip = ?", testExternalIP).Count(&count)
	if count != 0 {
		t.Fatalf("expected ban row to be deleted from DB, found %d", count)
	}

	// 5. Overview check
	overview := dm.GetOverview()
	if overview.BansToday < 1 {
		t.Fatalf("expected bans_today >= 1, got %d", overview.BansToday)
	}
}

func TestDefenseManager_SelfProtection(t *testing.T) {
	db := setupTestDB(t)
	bus := eventbus.New()
	alertMgr := NewAlertManager(config.AlertConfig{Enabled: false})

	defenseCfg := config.DefenseConfig{
		MaxAuthFailures:   3,
		FailureWindowMins: 5,
		BanDurationMins:   10,
		WhitelistIPs:      []string{"127.0.0.1", "192.168.31.1"},
	}

	dm := NewDefenseManager(db, defenseCfg, alertMgr, bus)

	operatorIP := "192.168.31.100"

	// 1. Loopback ban attempt
	err := dm.ManualBan(operatorIP, "admin", "127.0.0.1", 30*time.Minute, "test loopback")
	if !errors.Is(err, ErrLoopbackIP) {
		t.Fatalf("expected ErrLoopbackIP, got %v", err)
	}

	// 2. Whitelist ban attempt
	err = dm.ManualBan(operatorIP, "admin", "192.168.31.1", 30*time.Minute, "test gateway")
	if !errors.Is(err, ErrWhitelistedIP) {
		t.Fatalf("expected ErrWhitelistedIP, got %v", err)
	}

	// 3. Self-ban attempt
	err = dm.ManualBan(operatorIP, "admin", operatorIP, 30*time.Minute, "test self")
	if !errors.Is(err, ErrSelfIP) {
		t.Fatalf("expected ErrSelfIP, got %v", err)
	}

	// 4. Valid external ban
	err = dm.ManualBan(operatorIP, "admin", "198.51.100.22", 60*time.Minute, "test valid")
	if err != nil {
		t.Fatalf("expected valid ban to succeed, got %v", err)
	}
	banned, entry := dm.IsBanned("198.51.100.22")
	if !banned || entry.Source != "manual" {
		t.Fatalf("expected manual ban to be active, got %+v", entry)
	}
}

func TestDefenseManager_AlertCooldownAndWhitelist(t *testing.T) {
	db := setupTestDB(t)
	bus := eventbus.New()
	alertMgr := NewAlertManager(config.AlertConfig{Enabled: false})

	defenseCfg := config.DefenseConfig{
		MaxAuthFailures:   3,
		FailureWindowMins: 5,
		BanDurationMins:   10,
		WhitelistIPs:      []string{"127.0.0.1"},
	}

	dm := NewDefenseManager(db, defenseCfg, alertMgr, bus)

	testIP := "198.51.100.88"

	// Check alert cooldown
	if !dm.shouldSendAlert(testIP) {
		t.Fatalf("first alert should be allowed")
	}
	if dm.shouldSendAlert(testIP) {
		t.Fatalf("second alert within 10 minutes should be throttled")
	}

	// Dynamic whitelist add and remove
	if err := dm.AddWhitelist("10.0.0.0/8", "internal VPN"); err != nil {
		t.Fatalf("failed to add whitelist: %v", err)
	}
	if !dm.IsWhitelisted("10.1.2.3") {
		t.Fatalf("expected 10.1.2.3 to be whitelisted under 10.0.0.0/8")
	}

	// List dynamic whitelist (includes 1 system + 1 dynamic)
	wls, err := dm.ListWhitelist()
	if err != nil || len(wls) != 2 {
		t.Fatalf("expected 2 whitelist entries (1 system + 1 dynamic), got %d, err: %v", len(wls), err)
	}

	// Remove whitelist
	if err := dm.RemoveWhitelist("10.0.0.0/8"); err != nil {
		t.Fatalf("failed to remove whitelist: %v", err)
	}
	if dm.IsWhitelisted("10.1.2.3") {
		t.Fatalf("expected 10.1.2.3 not to be whitelisted after removal")
	}
	wlsAfter, _ := dm.ListWhitelist()
	if len(wlsAfter) != 1 {
		t.Fatalf("expected 1 system whitelist entry after removal, got %d", len(wlsAfter))
	}

	// Dynamic config update
	if err := dm.UpdateConfig(10, 15, 60); err != nil {
		t.Fatalf("failed to update config: %v", err)
	}
	ov := dm.GetOverview()
	if ov.Config.MaxAuthFailures != 10 || ov.Config.FailureWindowMins != 15 || ov.Config.BanDurationMins != 60 {
		t.Fatalf("overview config mismatch: %+v", ov.Config)
	}
}

func TestDefenseManager_ProgressiveBanAndPrune(t *testing.T) {
	db := setupTestDB(t)
	bus := eventbus.New()
	alertMgr := NewAlertManager(config.AlertConfig{Enabled: false})

	defenseCfg := config.DefenseConfig{
		MaxAuthFailures:   2,
		FailureWindowMins: 5,
		BanDurationMins:   30,
	}

	dm := NewDefenseManager(db, defenseCfg, alertMgr, bus)
	testIP := "198.51.100.99"

	// 1st offense -> 30 minutes
	dm.RecordAuthFailure(testIP, "fail 1", "curl/7.0")
	dm.RecordAuthFailure(testIP, "fail 2", "curl/7.0")
	banned, entry := dm.IsBanned(testIP)
	if !banned || entry.Permanent {
		t.Fatalf("expected 1st offense to be temporary ban")
	}

	// Simulate ban expired / unbanned, then 2nd offense
	dm.ManualUnban("admin", testIP)
	dm.RecordAuthFailure(testIP, "fail 3", "curl/7.0")
	dm.RecordAuthFailure(testIP, "fail 4", "curl/7.0")

	// Verify 3rd offense calculation
	dur, perm, count := dm.calculateEscalatedBan(testIP)
	if count != 3 || dur != 24*time.Hour || perm {
		t.Fatalf("expected 3rd offense calculation, got count=%d dur=%v perm=%v", count, dur, perm)
	}

	// 4th offense -> permanent
	db.Create(&model.SecurityEvent{IP: testIP, Type: "auth_fail", CreatedAt: time.Now()})
	dur, perm, count = dm.calculateEscalatedBan(testIP)
	if !perm || count < 4 {
		t.Fatalf("expected 4th offense to escalate to permanent, got count=%d perm=%v", count, perm)
	}

	// Test Pruning
	pruned, err := dm.PruneEvents(0) // Default 30 days
	if err != nil {
		t.Fatalf("failed to prune events: %v", err)
	}
	if pruned != 0 {
		t.Fatalf("expected 0 events pruned (created just now)")
	}
}

// TestDefenseManager_DetectionVsEnforcementSeparated is a regression guard for
// the dashboard bug where "累计阻断拦截" was computed as
// auth_fail-events + post-ban hits. Those are two different measurements:
//
//	detection  -> a failed authentication was observed (bounded: one per attempt
//	              until the threshold trips)
//	enforcement-> a request was rejected because the IP was ALREADY banned
//	              (unbounded: grows with every further attempt)
//
// Folding them together made a single brute-force campaign look like thousands
// of "blocks" while the real post-ban counter was still zero.
func TestDefenseManager_DetectionVsEnforcementSeparated(t *testing.T) {
	db := setupTestDB(t)
	bus := eventbus.New()
	alertMgr := NewAlertManager(config.AlertConfig{Enabled: false})

	defenseCfg := config.DefenseConfig{
		MaxAuthFailures:   3,
		FailureWindowMins: 5,
		BanDurationMins:   30,
	}

	dm := NewDefenseManager(db, defenseCfg, alertMgr, bus)
	attacker := "198.51.100.201"

	// Three failed authentications trip the threshold and ban the IP.
	dm.RecordAuthFailure(attacker, "fail 1", "ssh")
	dm.RecordAuthFailure(attacker, "fail 2", "ssh")
	dm.RecordAuthFailure(attacker, "fail 3", "ssh")

	if !isBannedIn(activeBanIPs(dm), attacker) {
		t.Fatalf("expected %s to be banned after 3 failures", attacker)
	}
	for i := 0; i < 10; i++ {
		dm.flushHourly()
	}

	// Post-ban traffic: every subsequent attempt is an enforcement hit.
	// RecordAuthFailure must short-circuit into IsBanned, which credits the
	// "blocked" series rather than the detection series.
	for i := 0; i < 7; i++ {
		dm.RecordAuthFailure(attacker, "fail after ban", "ssh")
	}
	dm.flushHourly()

	ov := dm.GetOverview()

	// Detection count reflects the 3 recorded auth_fail detections.
	if ov.TotalAuthFailures != 3 {
		t.Fatalf("expected total_auth_failures=3, got %d", ov.TotalAuthFailures)
	}

	// Enforcement count is independent: exactly the 7 post-ban rejections.
	if ov.TotalBlockedHits != 7 {
		t.Fatalf("expected total_blocked_hits=7 (post-ban only), got %d", ov.TotalBlockedHits)
	}

	if ov.TotalBlockedHits == ov.TotalAuthFailures {
		t.Fatalf("detection and enforcement counters must not be conflated")
	}

	// The two 24h series must stay separate too.
	sumAuth, sumBlocked := int64(0), int64(0)
	for _, v := range ov.AuthFails24h {
		sumAuth += v
	}
	for _, v := range ov.BlockedHits24h {
		sumBlocked += v
	}
	if sumAuth != 3 {
		t.Fatalf("expected auth_fails_24h to sum to 3, got %d", sumAuth)
	}
	if sumBlocked < 7 {
		t.Fatalf("expected blocked_hits_24h to sum to >=7, got %d", sumBlocked)
	}
}

// activeBanIPs snapshots the blocklist WITHOUT going through IsBanned(), which
// would itself credit a post-ban interception hit and skew the counters under
// assertion.
func activeBanIPs(dm *DefenseManager) []string {
	list := dm.ListBanned()
	ips := make([]string, 0, len(list))
	for _, b := range list {
		ips = append(ips, b.IP)
	}
	return ips
}

func isBannedIn(ips []string, target string) bool {
	for _, ip := range ips {
		if ip == target {
			return true
		}
	}
	return false
}
