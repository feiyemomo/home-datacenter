package ops

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/audit"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/security"

	_ "github.com/glebarez/sqlite"
)

// WAFCollectorConfig holds path configuration for 1Panel OpenResty 1pwaf integration.
type WAFCollectorConfig struct {
	WAFDataDir    string        // e.g. "/1pwaf" or "/opt/1panel/apps/openresty/openresty/1pwaf/data"
	PollInterval  time.Duration // e.g. 10 * time.Second
	AutoSyncBlack bool          // true to automatically sync defense blacklists to 1pwaf rules
}

// WAFAttackRecord represents an aggregated record from 1pwaf database.
type WAFAttackRecord struct {
	ID        int64
	EventID   string
	IP        string
	Rule      string
	ReqURI    string
	ActionID  int
	IsBlock   int
	CreatedAt time.Time
}

// WAFCollector polls 1Panel WAF SQLite databases and integrates with EventBus and DefenseManager.
type WAFCollector struct {
	db         *gorm.DB
	bus        *eventbus.Bus
	defenseMgr *security.DefenseManager
	cfg         WAFCollectorConfig
	executor    OperationExecutor
	auditEngine *audit.AgentAuditEngine

	mu           sync.Mutex
	syncFileMu   sync.Mutex
	lastSyncedID int64
	running      bool
	cancel       context.CancelFunc
}

// SetAuditEngine configures the unified AgentAuditEngine on WAFCollector.
func (c *WAFCollector) SetAuditEngine(ae *audit.AgentAuditEngine) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.auditEngine = ae
}

func (c *WAFCollector) getAuditEngine() *audit.AgentAuditEngine {
	c.mu.Lock()
	ae := c.auditEngine
	ex := c.executor
	db := c.db
	bus := c.bus
	c.mu.Unlock()
	if ae != nil {
		return ae
	}
	if defExec, ok := ex.(*DefaultOperationExecutor); ok && defExec != nil {
		if exAE := defExec.GetAuditEngine(); exAE != nil {
			c.SetAuditEngine(exAE)
			return exAE
		}
	}
	if db != nil {
		created := audit.NewAgentAuditEngine(db, bus, audit.WithFailureCallback(GlobalSecurityMetrics.IncAuditFailed))
		c.SetAuditEngine(created)
		return created
	}
	return nil
}

// SetExecutor configures the OperationExecutor for safe WAF operations.
func (c *WAFCollector) SetExecutor(exec OperationExecutor) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.executor = exec
}

// NewWAFCollector creates a new 1Panel WAF log collector and firewall syncer.
func NewWAFCollector(db *gorm.DB, bus *eventbus.Bus, defenseMgr *security.DefenseManager, cfg WAFCollectorConfig) *WAFCollector {
	if cfg.WAFDataDir == "" {
		cfg.WAFDataDir = "/1pwaf"
		if _, err := os.Stat(cfg.WAFDataDir); err != nil {
			// Fallback check on host path
			if _, errHost := os.Stat("/opt/1panel/apps/openresty/openresty/1pwaf/data"); errHost == nil {
				cfg.WAFDataDir = "/opt/1panel/apps/openresty/openresty/1pwaf/data"
			}
		}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 10 * time.Second
	}

	return &WAFCollector{
		db:         db,
		bus:        bus,
		defenseMgr: defenseMgr,
		cfg:        cfg,
	}
}

// Start begins background polling of 1Panel WAF logs.
func (c *WAFCollector) Start(ctx context.Context) {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	c.running = true
	c.mu.Unlock()

	// Anchor initial baseline watermark so restarts do not replay ancient attacks
	c.initBaselineCursor()

	go c.runLoop(ctx)
}

func (c *WAFCollector) initBaselineCursor() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.lastSyncedID > 0 {
		return
	}

	attackDBPath := filepath.Join(c.cfg.WAFDataDir, "db", "waf", "attack_logs.db")
	if _, err := os.Stat(attackDBPath); err != nil {
		return
	}

	dsn := fmt.Sprintf("file:%s?mode=ro", attackDBPath)
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return
	}
	defer sqldb.Close()

	var maxID sql.NullInt64
	row := sqldb.QueryRow("SELECT MAX(id) FROM attack_logs")
	if err := row.Scan(&maxID); err == nil && maxID.Valid {
		c.lastSyncedID = maxID.Int64
		log.Printf("[waf-collector] initialized sync watermark baseline at ID %d", c.lastSyncedID)
	}
}

// Stop stops the collector loop.
func (c *WAFCollector) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.running = false
}

func (c *WAFCollector) runLoop(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()

	// Initial immediate poll
	c.pollOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.pollOnce(ctx)
		}
	}
}

// pollOnce queries the 1pwaf SQLite database for new attack events.
func (c *WAFCollector) pollOnce(ctx context.Context) {
	attackDBPath := filepath.Join(c.cfg.WAFDataDir, "db", "waf", "attack_logs.db")
	if _, err := os.Stat(attackDBPath); err != nil {
		return // WAF not mounted or offline
	}

	records, err := c.fetchNewRecords(attackDBPath)
	if err != nil {
		log.Printf("[waf-collector] fetch error: %v", err)
		return
	}

	if len(records) == 0 {
		return
	}

	for _, rec := range records {
		c.processAttackRecord(rec)
	}
}

func (c *WAFCollector) fetchNewRecords(attackDBPath string) ([]WAFAttackRecord, error) {
	c.mu.Lock()
	lastID := c.lastSyncedID
	c.mu.Unlock()

	dsn := fmt.Sprintf("file:%s?mode=ro", attackDBPath)
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer sqldb.Close()

	ipsDBPath := filepath.Join(c.cfg.WAFDataDir, "db", "waf", "ips.db")
	rulesDBPath := filepath.Join(c.cfg.WAFDataDir, "db", "waf", "rules.db")
	reqUrisDBPath := filepath.Join(c.cfg.WAFDataDir, "db", "waf", "req_uris.db")

	// If secondary lookup DBs exist, attach them
	query := "SELECT id, event_id, ip_id, rule_id, req_uri_id, action_id, is_block, localtime FROM attack_logs WHERE id > ? ORDER BY id ASC LIMIT 100"
	rows, err := sqldb.Query(query, lastID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type rawRecord struct {
		id        int64
		eventID   string
		ipID      int64
		ruleID    int64
		reqURIID  int64
		actionID  int
		isBlock   int
		localTime string
	}

	var rawList []rawRecord
	var maxID int64 = lastID

	for rows.Next() {
		var r rawRecord
		if err := rows.Scan(&r.id, &r.eventID, &r.ipID, &r.ruleID, &r.reqURIID, &r.actionID, &r.isBlock, &r.localTime); err != nil {
			continue
		}
		rawList = append(rawList, r)
		if r.id > maxID {
			maxID = r.id
		}
	}

	if len(rawList) == 0 {
		return nil, nil
	}

	// Resolve foreign keys
	ipMap := c.queryLookupTable(ipsDBPath, "ips")
	ruleMap := c.queryLookupTable(rulesDBPath, "rules")
	uriMap := c.queryLookupTable(reqUrisDBPath, "req_uris")

	var records []WAFAttackRecord
	for _, r := range rawList {
		ip := ipMap[r.ipID]
		if ip == "" {
			ip = "unknown"
		}
		rule := ruleMap[r.ruleID]
		if rule == "" {
			rule = "waf_rule"
		}
		uri := uriMap[r.reqURIID]
		if uri == "" {
			uri = "/"
		}

		t, parseErr := time.ParseInLocation("2006-01-02 15:04:05", r.localTime, time.Local)
		if parseErr != nil {
			t = time.Now()
		}

		records = append(records, WAFAttackRecord{
			ID:        r.id,
			EventID:   r.eventID,
			IP:        ip,
			Rule:      rule,
			ReqURI:    uri,
			ActionID:  r.actionID,
			IsBlock:   r.isBlock,
			CreatedAt: t,
		})
	}

	c.mu.Lock()
	if maxID > c.lastSyncedID {
		c.lastSyncedID = maxID
	}
	c.mu.Unlock()

	return records, nil
}

func (c *WAFCollector) queryLookupTable(dbPath, tableName string) map[int64]string {
	result := make(map[int64]string)
	if _, err := os.Stat(dbPath); err != nil {
		return result
	}

	dsn := fmt.Sprintf("file:%s?mode=ro", dbPath)
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return result
	}
	defer sqldb.Close()

	query := fmt.Sprintf("SELECT id, value FROM %s", tableName)
	rows, err := sqldb.Query(query)
	if err != nil {
		return result
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var val string
		if err := rows.Scan(&id, &val); err == nil {
			result[id] = val
		}
	}
	return result
}

func (c *WAFCollector) processAttackRecord(rec WAFAttackRecord) {
	log.Printf("[waf-collector] 1Panel WAF intercepted: IP=%s Rule=%s URI=%s", rec.IP, rec.Rule, rec.ReqURI)

	// 1. Audit to security_events table in SQLite
	if c.db != nil {
		event := model.SecurityEvent{
			IP:        rec.IP,
			Type:      "waf_attack",
			Detail:    fmt.Sprintf("1Panel WAF 拦截攻击: 命中规则 [%s], 请求路径: %s", rec.Rule, rec.ReqURI),
			Action:    "WAF 403 阻断",
			CreatedAt: rec.CreatedAt,
		}
		_ = c.db.Create(&event).Error
	}

	// 2. Publish to EventBus TopicSecurityAlert
	if c.bus != nil {
		payload, _ := json.Marshal(map[string]any{
			"ip":         rec.IP,
			"rule":       rec.Rule,
			"uri":        rec.ReqURI,
			"action":     "deny_403",
			"created_at": rec.CreatedAt.Unix(),
		})
		c.bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicSecurityAlert,
			Source:   "1panel-waf",
			Severity: eventbus.SeverityWarn,
			Payload:  payload,
		})
	}

	// 3. Link with DefenseManager: only recent attacks (< 10m) trigger active brute-force failure counts
	// Ancient records loaded during catch-up are purely logged for historical auditing without triggering auto-bans.
	if c.defenseMgr != nil && rec.IP != "" && rec.IP != "127.0.0.1" && rec.IP != "unknown" {
		if time.Since(rec.CreatedAt) < 10*time.Minute {
			c.defenseMgr.RecordAuthFailure(rec.IP, fmt.Sprintf("1Panel WAF 规则 [%s] 触发", rec.Rule), "1Panel-WAF")
		} else {
			log.Printf("[waf-collector] historical WAF attack IP=%s at %v recorded as audit only", rec.IP, rec.CreatedAt)
		}
	}
}

// SyncBannedIPs safely merges active defense bans into 1pwaf ipBlack.json, preserving user custom rules.
func (c *WAFCollector) SyncBannedIPs(bannedEntries []security.BanEntry) error {
	rulesPath := filepath.Join(c.cfg.WAFDataDir, "rules", "ipBlack.json")
	if _, err := os.Stat(filepath.Dir(rulesPath)); err != nil {
		return nil // directory not present
	}

	c.syncFileMu.Lock()
	defer c.syncFileMu.Unlock()

	ae := c.getAuditEngine()
	runCtx := context.Background()
	var syncRunID string
	if ae != nil {
		rID, rCtx, sErr := ae.StartRun(runCtx, audit.StartRunInput{
			HouseholdID:   1,
			TriggerSource: "waf_collector",
			ActorType:     "system_daemon",
			ActorID:       "waf-collector",
			Action:        "waf.ipblack.sync",
			Target:        Component1PWAF,
			Component:     Component1PWAF,
			Decision:      model.AuditDecisionAllow,
			RiskLevel:     model.AuditRiskHigh,
		})
		if sErr != nil {
			return sErr
		}
		syncRunID = rID
		runCtx = rCtx
		if _, preErr := ae.RecordPreMutation(runCtx, audit.EmitEventInput{
			EventType:     model.AuditEventExecutionStarted,
			Action:        "waf.ipblack.sync",
			Target:        Component1PWAF,
			Component:     Component1PWAF,
			Decision:      model.AuditDecisionAllow,
			RiskLevel:     model.AuditRiskHigh,
			Parameters:    map[string]interface{}{"ban_count": len(bannedEntries)},
			Result:        "STARTED",
			FailurePolicy: audit.PolicyFailClosed,
		}); preErr != nil {
			return preErr
		}
	}

	type wafRuleItem struct {
		State string `json:"state"`
		Rule  string `json:"rule"`
		Name  string `json:"name,omitempty"`
	}
	type wafRuleFile struct {
		Rules []wafRuleItem `json:"rules"`
	}

	var existingFile wafRuleFile
	// Read existing rules to preserve non-AutoSync user customized rules
	if data, err := os.ReadFile(rulesPath); err == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &existingFile)
	}

	// Keep existing rules that do not start with "AutoSync-"
	mergedRules := make([]wafRuleItem, 0, len(existingFile.Rules)+len(bannedEntries))
	for _, r := range existingFile.Rules {
		if !strings.HasPrefix(r.Name, "AutoSync-") {
			mergedRules = append(mergedRules, r)
		}
	}

	// Append active system bans
	for _, entry := range bannedEntries {
		if entry.IP == "" || entry.IP == "127.0.0.1" {
			continue
		}
		mergedRules = append(mergedRules, wafRuleItem{
			State: "on",
			Rule:  entry.IP,
			Name:  fmt.Sprintf("AutoSync-%s", entry.BannedAt.Format("20060102150405")),
		})
	}

	fileContent := wafRuleFile{Rules: mergedRules}
	data, err := json.MarshalIndent(fileContent, "", "  ")
	if err != nil {
		if ae != nil {
			_, _ = ae.RecordPostMutation(runCtx, audit.EmitEventInput{
				EventType: model.AuditEventExecutionFailed, Action: "waf.ipblack.sync",
				Target: Component1PWAF, Component: Component1PWAF, RiskLevel: model.AuditRiskHigh,
				Result: "FAILED", ErrorCode: err.Error(), FinalRunStatus: model.AuditRunStatusFailed,
			}, nil)
		}
		return err
	}

	tmpPath := rulesPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		if ae != nil {
			_, _ = ae.RecordPostMutation(runCtx, audit.EmitEventInput{
				EventType: model.AuditEventExecutionFailed, Action: "waf.ipblack.sync",
				Target: Component1PWAF, Component: Component1PWAF, RiskLevel: model.AuditRiskHigh,
				Result: "FAILED", ErrorCode: err.Error(), FinalRunStatus: model.AuditRunStatusFailed,
			}, nil)
		}
		return fmt.Errorf("failed to write temp %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, rulesPath); err != nil {
		_ = os.Remove(tmpPath)
		if ae != nil {
			_, _ = ae.RecordPostMutation(runCtx, audit.EmitEventInput{
				EventType: model.AuditEventExecutionFailed, Action: "waf.ipblack.sync",
				Target: Component1PWAF, Component: Component1PWAF, RiskLevel: model.AuditRiskHigh,
				Result: "FAILED", ErrorCode: err.Error(), FinalRunStatus: model.AuditRunStatusFailed,
			}, nil)
		}
		return fmt.Errorf("failed to atomically replace %s: %w", rulesPath, err)
	}

	if ae != nil {
		_, _ = ae.EmitEvent(runCtx, audit.EmitEventInput{
			EventType:  model.AuditEventExecutionStepCompleted,
			Action:     "waf.ipblack.write",
			Target:     Component1PWAF,
			Component:  Component1PWAF,
			RiskLevel:  model.AuditRiskMedium,
			AfterState: map[string]interface{}{"rules_count": len(mergedRules)},
			Result:     "WRITTEN",
		})
	}

	// Trigger OpenResty HUP reload strictly via OperationExecutor (R4-24)
	if err := c.reloadOpenRestyWithContext(runCtx); err != nil {
		log.Printf("[waf-collector] warning: failed to reload openresty via OperationExecutor: %v", err)
		if ae != nil {
			_, _ = ae.RecordPostMutation(runCtx, audit.EmitEventInput{
				EventType:      model.AuditEventExecutionFailed,
				Action:         "waf.ipblack.sync",
				Target:         Component1PWAF,
				Component:      Component1PWAF,
				RiskLevel:      model.AuditRiskHigh,
				Result:         "FAILED",
				ErrorCode:      err.Error(),
				FinalRunStatus: model.AuditRunStatusFailed,
				RunSummary:     fmt.Sprintf("synced %d rules to disk, reload failed: %v", len(mergedRules), err),
			}, nil)
		}
	} else if ae != nil {
		_ = ae.FinishRun(syncRunID, model.AuditRunStatusCompleted, fmt.Sprintf("synced %d rules", len(mergedRules)), "")
	}
	return nil
}

func (c *WAFCollector) reloadOpenResty() error {
	return c.reloadOpenRestyWithContext(context.Background())
}

func (c *WAFCollector) reloadOpenRestyWithContext(parent context.Context) error {
	c.mu.Lock()
	exec := c.executor
	c.mu.Unlock()

	if exec == nil {
		return fmt.Errorf("operation executor is not configured on WAFCollector; refusing raw docker socket bypass")
	}

	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	_, err := exec.Execute(ctx, OperationSpec{
		Action: ActionWAFReload,
		Target: ComponentOpenResty,
	})
	return err
}
