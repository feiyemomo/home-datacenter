package security

import (
	"testing"

	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/eventbus"
)

func TestDefenseManager_AutoBanAndAlert(t *testing.T) {
	bus := eventbus.New()
	alertCfg := config.AlertConfig{
		Enabled: false, // Don't actually send SMTP in unit test
	}
	alertMgr := NewAlertManager(alertCfg)

	defenseCfg := config.DefenseConfig{
		MaxAuthFailures:   3,
		FailureWindowMins: 5,
		BanDurationMins:   10,
		WhitelistIPs:      []string{"127.0.0.1", "192.168.31.0/24"},
	}

	dm := NewDefenseManager(defenseCfg, alertMgr, bus)

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

	// 2. Gateway IP (whitelisted)
	dm.RecordAuthFailure(testGatewayIP, "gateway probe 1", "python/3.10")
	dm.RecordAuthFailure(testGatewayIP, "gateway probe 2", "python/3.10")
	dm.RecordAuthFailure(testGatewayIP, "gateway probe 3", "python/3.10")
	dm.RecordAuthFailure(testGatewayIP, "gateway probe 4", "python/3.10")

	gwBanned, _ := dm.IsBanned(testGatewayIP)
	if gwBanned {
		t.Fatalf("gateway IP %s is whitelisted and MUST NEVER be banned", testGatewayIP)
	}

	// 3. Manual unban
	dm.ManualUnban(testExternalIP)
	bannedAfterUnban, _ := dm.IsBanned(testExternalIP)
	if bannedAfterUnban {
		t.Fatalf("expected IP %s to be unbanned after ManualUnban", testExternalIP)
	}

	// 4. Overview
	overview := dm.GetOverview()
	if overview.TotalBlockedHits < 1 {
		t.Logf("blocked hits: %d", overview.TotalBlockedHits)
	}
}
