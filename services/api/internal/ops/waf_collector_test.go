package ops

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/security"
)

func TestWAFCollector_FetchAndSync(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "waf_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	wafDBDir := filepath.Join(tempDir, "db", "waf")
	wafRulesDir := filepath.Join(tempDir, "rules")
	_ = os.MkdirAll(wafDBDir, 0755)
	_ = os.MkdirAll(wafRulesDir, 0755)

	// Create attack_logs.db
	attackDBPath := filepath.Join(wafDBDir, "attack_logs.db")
	db, err := sql.Open("sqlite", attackDBPath)
	if err != nil {
		t.Fatalf("failed to create sqlite db: %v", err)
	}
	_, _ = db.Exec(`CREATE TABLE attack_logs (
		id INTEGER PRIMARY KEY,
		event_id TEXT,
		website_id INTEGER,
		ip_id INTEGER,
		host_id INTEGER,
		rule_id INTEGER,
		rule_type_id INTEGER,
		match_value_id INTEGER,
		nginx_log_id INTEGER,
		action_id INTEGER,
		uri_id INTEGER,
		req_uri_id INTEGER,
		ip_location_id INTEGER,
		is_block INTEGER,
		is_attack INTEGER,
		iso_id INTEGER,
		localtime TEXT,
		time REAL
	);`)
	_, _ = db.Exec(`INSERT INTO attack_logs VALUES (1, 'ev-1', 1, 1, 1, 1, 1, 1, 1, 2, 1, 1, 1, 0, 1, 0, '2026-11-08 08:30:00', 1794098800.0);`)
	db.Close()

	// Create ips.db
	ipsDBPath := filepath.Join(wafDBDir, "ips.db")
	ipsDB, _ := sql.Open("sqlite", ipsDBPath)
	_, _ = ipsDB.Exec(`CREATE TABLE ips (id INTEGER PRIMARY KEY, value TEXT);`)
	_, _ = ipsDB.Exec(`INSERT INTO ips VALUES (1, '194.26.29.112');`)
	ipsDB.Close()

	// Create rules.db
	rulesDBPath := filepath.Join(wafDBDir, "rules.db")
	rulesDB, _ := sql.Open("sqlite", rulesDBPath)
	_, _ = rulesDB.Exec(`CREATE TABLE rules (id INTEGER PRIMARY KEY, value TEXT);`)
	_, _ = rulesDB.Exec(`INSERT INTO rules VALUES (1, 'sql_injection_defense');`)
	rulesDB.Close()

	// Create req_uris.db
	reqDBPath := filepath.Join(wafDBDir, "req_uris.db")
	reqDB, _ := sql.Open("sqlite", reqDBPath)
	_, _ = reqDB.Exec(`CREATE TABLE req_uris (id INTEGER PRIMARY KEY, value TEXT);`)
	_, _ = reqDB.Exec(`INSERT INTO req_uris VALUES (1, '/api/v1/user?id=1%27%20OR%201=1');`)
	reqDB.Close()

	// GORM DB
	gormDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to init gorm: %v", err)
	}
	_ = gormDB.AutoMigrate(&model.SecurityEvent{}, &model.IPBan{})

	bus := eventbus.New()
	defenseMgr := security.NewDefenseManager(gormDB, config.DefenseConfig{
		MaxAuthFailures:   5,
		FailureWindowMins: 10,
		BanDurationMins:   30,
	}, nil, bus)

	var alertReceived bool
	bus.Subscribe(eventbus.TopicSecurityAlert, func(e eventbus.Event) {
		alertReceived = true
	})

	collector := NewWAFCollector(gormDB, bus, defenseMgr, WAFCollectorConfig{
		WAFDataDir:   tempDir,
		PollInterval: 100 * time.Millisecond,
	})

	// Run pollOnce
	collector.pollOnce(context.Background())

	// Assert security event created in DB
	var events []model.SecurityEvent
	if err := gormDB.Find(&events).Error; err != nil || len(events) == 0 {
		t.Fatalf("expected security event inserted, found %d (err: %v)", len(events), err)
	}
	if events[0].IP != "194.26.29.112" {
		t.Errorf("expected IP 194.26.29.112, got %s", events[0].IP)
	}

	time.Sleep(50 * time.Millisecond)
	if !alertReceived {
		t.Errorf("expected eventbus TopicSecurityAlert to be fired")
	}

	// Test SyncBannedIPs
	testBans := []security.BanEntry{
		{
			IP:        "194.26.29.112",
			Reason:    "WAF Attack",
			BannedAt:  time.Now(),
			ExpiresAt: time.Now().Add(1 * time.Hour),
		},
	}
	if err := collector.SyncBannedIPs(testBans); err != nil {
		t.Errorf("SyncBannedIPs failed: %v", err)
	}

	// Verify ipBlack.json exists
	blackJSONPath := filepath.Join(wafRulesDir, "ipBlack.json")
	if _, err := os.Stat(blackJSONPath); err != nil {
		t.Errorf("expected ipBlack.json created, err: %v", err)
	}
}
