package ops

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
)

// SQLiteFaultClass classifies database failures consistently across Go and Python (R4-18).
type SQLiteFaultClass string

const (
	FaultNone                  SQLiteFaultClass = ""
	FaultDatabaseMissing       SQLiteFaultClass = "DATABASE_MISSING"
	FaultDatabaseCorrupt       SQLiteFaultClass = "DATABASE_CORRUPT"
	FaultTableMissing          SQLiteFaultClass = "TABLE_MISSING"
	FaultColumnMissing         SQLiteFaultClass = "COLUMN_MISSING"
	FaultSchemaVersionMismatch SQLiteFaultClass = "SCHEMA_VERSION_MISMATCH"
	FaultDatabaseReadonly      SQLiteFaultClass = "DATABASE_READONLY"
	FaultSQLiteBusy            SQLiteFaultClass = "SQLITE_BUSY"
	FaultSQLiteFull            SQLiteFaultClass = "SQLITE_FULL"
	FaultTransactionFailed     SQLiteFaultClass = "TRANSACTION_FAILED"
	FaultTimestampInvalid      SQLiteFaultClass = "TIMESTAMP_INVALID"
	FaultRecordInconsistent    SQLiteFaultClass = "RECORD_INCONSISTENT"
)

const (
	RequiredOpsSchemaVersion = 4
)

var (
	ErrSchemaIntegrityFailed = errors.New("ops SQLite schema integrity check failed")
	ErrSchemaTableMissing    = errors.New("required ops security table is missing on existing database")
	ErrSchemaColumnMissing   = errors.New("required ops security column is missing")
	ErrSchemaVersionMismatch = errors.New("ops schema migration version mismatch")
	ErrTimestampInvalid      = errors.New("malformed or invalid timestamp in security store")
	ErrRecordInconsistent    = errors.New("inconsistent security state record")
)

// ClassifySQLiteError maps raw GORM/SQLite errors into the canonical Round 4 SQLiteFaultClass.
func ClassifySQLiteError(err error) SQLiteFaultClass {
	if err == nil {
		return FaultNone
	}
	if errors.Is(err, ErrTimestampInvalid) {
		return FaultTimestampInvalid
	}
	if errors.Is(err, ErrRecordInconsistent) {
		return FaultRecordInconsistent
	}
	if errors.Is(err, ErrSchemaTableMissing) {
		return FaultTableMissing
	}
	if errors.Is(err, ErrSchemaColumnMissing) {
		return FaultColumnMissing
	}
	if errors.Is(err, ErrSchemaVersionMismatch) {
		return FaultSchemaVersionMismatch
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "no such table"):
		return FaultTableMissing
	case strings.Contains(msg, "no such column") || strings.Contains(msg, "has no column"):
		return FaultColumnMissing
	case strings.Contains(msg, "readonly") || strings.Contains(msg, "read-only") || strings.Contains(msg, "attempt to write a readonly database"):
		return FaultDatabaseReadonly
	case strings.Contains(msg, "database is locked") || strings.Contains(msg, "sqlite_busy") || strings.Contains(msg, "busy"):
		return FaultSQLiteBusy
	case strings.Contains(msg, "database or disk is full") || strings.Contains(msg, "sqlite_full"):
		return FaultSQLiteFull
	case strings.Contains(msg, "malformed") || strings.Contains(msg, "not a database") || strings.Contains(msg, "corrupt"):
		return FaultDatabaseCorrupt
	case strings.Contains(msg, "unable to open database file") || strings.Contains(msg, "no such file"):
		return FaultDatabaseMissing
	default:
		return FaultTransactionFailed
	}
}

func tableExists(db *gorm.DB, tableName string) (bool, error) {
	var count int64
	err := db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = ?", tableName).Scan(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func getTableColumns(db *gorm.DB, tableName string) (map[string]bool, error) {
	type pragmaCol struct {
		Name string `gorm:"column:name"`
	}
	var cols []pragmaCol
	if err := db.Raw(fmt.Sprintf("PRAGMA table_info(%s)", tableName)).Scan(&cols).Error; err != nil {
		return nil, err
	}
	res := make(map[string]bool, len(cols))
	for _, c := range cols {
		res[strings.ToLower(c.Name)] = true
	}
	return res, nil
}

func addColumnIfMissing(db *gorm.DB, tableName, colName, colDDL string) error {
	cols, err := getTableColumns(db, tableName)
	if err != nil {
		return err
	}
	if cols[strings.ToLower(colName)] {
		return nil
	}
	return db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", tableName, colName, colDDL)).Error
}

// EnsureOpsSchema runs explicit, versioned migrations for all ops security tables.
// Crucially, if an already-bootstrapped database (indicated by ops_schema_meta or
// core business tables like users/pending_proposals) has unexpectedly lost a security
// table (e.g. ops_circuit_breaker_states, ops_maintenance_locks, offline_restart_history),
// EnsureOpsSchema FAILS CLOSED instead of silently recreating an empty table that would
// erase crash/lock/restart protections (R4-14, R4-18).
func EnsureOpsSchema(db *gorm.DB) error {
	if db == nil {
		return nil
	}

	var totalTables int64
	if err := db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&totalTables).Error; err != nil {
		GlobalSecurityMetrics.IncDatabaseError()
		return fmt.Errorf("%w: failed to inspect sqlite_master (%s): %v", ErrSchemaIntegrityFailed, ClassifySQLiteError(err), err)
	}

	hasMeta, err := tableExists(db, "ops_schema_meta")
	if err != nil {
		return err
	}
	hasMigrations, err := tableExists(db, "ops_schema_migrations")
	if err != nil {
		return err
	}

	bootstrapped := false
	if hasMeta {
		var sentinelVal string
		if err := db.Raw("SELECT value FROM ops_schema_meta WHERE key = 'bootstrapped'").Scan(&sentinelVal).Error; err == nil && sentinelVal == "1" {
			bootstrapped = true
		}
	}

	// If ops_schema_migrations exists & database was already bootstrapped, check if any
	// critical security history/state table was deleted externally.
	if bootstrapped {
		criticalTables := []string{
			"ops_maintenance_locks",
			"ops_circuit_breaker_states",
			"ops_recovery_leases",
			"ops_recovery_lease_generations",
			"offline_restart_history",
			"offline_ops_audit",
			"pending_proposals",
		}
		for _, tbl := range criticalTables {
			exists, tErr := tableExists(db, tbl)
			if tErr != nil {
				GlobalSecurityMetrics.IncDatabaseError()
				return tErr
			}
			if !exists {
				GlobalSecurityMetrics.IncDatabaseError()
				return fmt.Errorf("%w: security table '%s' missing from bootstrapped database (fail-closed)", ErrSchemaTableMissing, tbl)
			}
		}
	}

	// Create migration bookkeeping tables
	if err := db.Exec(`
		CREATE TABLE IF NOT EXISTS ops_schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at_ms INTEGER NOT NULL,
			checksum TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS ops_schema_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at_ms INTEGER NOT NULL
		);
	`).Error; err != nil {
		GlobalSecurityMetrics.IncDatabaseError()
		return err
	}

	if !hasMigrations && totalTables == 0 {
		// Clean initial DB: safe to initialize all models cleanly
	}

	currentVersion := 0
	_ = db.Raw("SELECT COALESCE(MAX(version), 0) FROM ops_schema_migrations").Scan(&currentVersion).Error

	nowMs := time.Now().UTC().UnixMilli()

	// Migration V1: Base ops tables via GORM + offline tables
	if currentVersion < 1 {
		if err := db.AutoMigrate(
			&model.OpsProposal{},
			&model.OpsExecutionHistory{},
			&model.StructuredOpsAuditLog{},
			&model.OpsMaintenanceLock{},
			&model.OpsCircuitBreakerState{},
			&model.OpsChatSession{},
			&model.OpsChatMessage{},
		); err != nil {
			GlobalSecurityMetrics.IncDatabaseError()
			return fmt.Errorf("migration v1 automigrate failed: %w", err)
		}
		if err := db.Exec(`
			CREATE TABLE IF NOT EXISTS offline_restart_history (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				container_name TEXT NOT NULL,
				restart_time REAL NOT NULL DEFAULT 0,
				restart_time_ms INTEGER NOT NULL DEFAULT 0
			);
			CREATE TABLE IF NOT EXISTS offline_ops_audit (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				event_uuid TEXT UNIQUE,
				operation_id TEXT NOT NULL DEFAULT '',
				container_name TEXT NOT NULL,
				action TEXT NOT NULL,
				holder TEXT NOT NULL DEFAULT '',
				fencing_generation INTEGER NOT NULL DEFAULT 0,
				exit_code INTEGER,
				success INTEGER,
				audit_status TEXT NOT NULL DEFAULT 'AUDIT_COMMITTED',
				error_message TEXT NOT NULL DEFAULT '',
				executed_at TEXT NOT NULL,
				executed_at_ms INTEGER NOT NULL DEFAULT 0,
				synced INTEGER DEFAULT 0
			);
		`).Error; err != nil {
			GlobalSecurityMetrics.IncDatabaseError()
			return fmt.Errorf("migration v1 offline tables failed: %w", err)
		}
		if err := recordMigration(db, 1, "base_ops_schema", nowMs, "v1"); err != nil {
			return err
		}
	}

	// Migration V2: Cross-language Lease Protocol (Unix ms + fencing_generation + generation counter)
	if currentVersion < 2 {
		if err := db.Exec(`
			CREATE TABLE IF NOT EXISTS ops_recovery_leases (
				component TEXT PRIMARY KEY,
				holder TEXT NOT NULL,
				lease_token TEXT NOT NULL,
				fencing_generation INTEGER NOT NULL DEFAULT 1,
				acquired_at DATETIME,
				expires_at DATETIME,
				acquired_at_ms INTEGER NOT NULL DEFAULT 0,
				expires_at_ms INTEGER NOT NULL DEFAULT 0,
				state TEXT NOT NULL DEFAULT 'ACTIVE'
			);
			CREATE TABLE IF NOT EXISTS ops_recovery_lease_generations (
				component TEXT PRIMARY KEY,
				last_generation INTEGER NOT NULL DEFAULT 0
			);
		`).Error; err != nil {
			GlobalSecurityMetrics.IncDatabaseError()
			return fmt.Errorf("migration v2 lease tables failed: %w", err)
		}
		for _, col := range []struct{ name, ddl string }{
			{"fencing_generation", "INTEGER NOT NULL DEFAULT 1"},
			{"acquired_at_ms", "INTEGER NOT NULL DEFAULT 0"},
			{"expires_at_ms", "INTEGER NOT NULL DEFAULT 0"},
			{"state", "TEXT NOT NULL DEFAULT 'ACTIVE'"},
		} {
			if err := addColumnIfMissing(db, "ops_recovery_leases", col.name, col.ddl); err != nil {
				return err
			}
		}
		// Backfill expires_at_ms from legacy DATETIME column if present and expires_at_ms == 0
		_ = backfillLegacyLeaseMillis(db)
		_ = db.Exec("CREATE INDEX IF NOT EXISTS idx_ops_recovery_leases_expires_ms ON ops_recovery_leases (expires_at_ms)").Error

		if err := recordMigration(db, 2, "cross_language_leases_v2", nowMs, "v2"); err != nil {
			return err
		}
	}

	// Migration V3: Circuit Breaker probe persistence + Maintenance Lock ms + Proposal CAS fields
	if currentVersion < 3 {
		if err := db.AutoMigrate(&model.OpsProposal{}, &model.OpsMaintenanceLock{}, &model.OpsCircuitBreakerState{}); err != nil {
			return err
		}
		for _, col := range []struct{ tbl, col, ddl string }{
			{"ops_circuit_breaker_states", "opened_at_ms", "INTEGER NOT NULL DEFAULT 0"},
			{"ops_circuit_breaker_states", "active_probe_token", "TEXT NOT NULL DEFAULT ''"},
			{"ops_circuit_breaker_states", "active_probe_issued_ms", "INTEGER NOT NULL DEFAULT 0"},
			{"ops_circuit_breaker_states", "active_probe_expires_ms", "INTEGER NOT NULL DEFAULT 0"},
			{"ops_circuit_breaker_states", "degraded", "INTEGER NOT NULL DEFAULT 0"},
			{"ops_circuit_breaker_states", "updated_at_ms", "INTEGER NOT NULL DEFAULT 0"},
			{"ops_maintenance_locks", "expires_at_ms", "INTEGER NOT NULL DEFAULT 0"},
			{"pending_proposals", "execution_token", "TEXT NOT NULL DEFAULT ''"},
			{"pending_proposals", "execution_policy", "TEXT NOT NULL DEFAULT ''"},
			{"pending_proposals", "revision", "INTEGER NOT NULL DEFAULT 1"},
		} {
			if err := addColumnIfMissing(db, col.tbl, col.col, col.ddl); err != nil {
				return err
			}
		}
		if err := recordMigration(db, 3, "cb_probe_proposal_cas_v3", nowMs, "v3"); err != nil {
			return err
		}
	}

	// Migration V4: Offline history ms + audit metadata columns + boot sentinel
	if currentVersion < 4 {
		for _, col := range []struct{ tbl, col, ddl string }{
			{"offline_restart_history", "restart_time_ms", "INTEGER NOT NULL DEFAULT 0"},
			{"offline_ops_audit", "operation_id", "TEXT NOT NULL DEFAULT ''"},
			{"offline_ops_audit", "holder", "TEXT NOT NULL DEFAULT ''"},
			{"offline_ops_audit", "fencing_generation", "INTEGER NOT NULL DEFAULT 0"},
			{"offline_ops_audit", "audit_status", "TEXT NOT NULL DEFAULT 'AUDIT_COMMITTED'"},
			{"offline_ops_audit", "error_message", "TEXT NOT NULL DEFAULT ''"},
			{"offline_ops_audit", "executed_at_ms", "INTEGER NOT NULL DEFAULT 0"},
		} {
			if err := addColumnIfMissing(db, col.tbl, col.col, col.ddl); err != nil {
				return err
			}
		}
		if err := recordMigration(db, 4, "offline_audit_hardening_v4", nowMs, "v4"); err != nil {
			return err
		}
	}

	// Anchor boot sentinel once schema is healthy
	if err := db.Exec(`
		INSERT INTO ops_schema_meta (key, value, updated_at_ms)
		VALUES ('bootstrapped', '1', ?)
		ON CONFLICT(key) DO UPDATE SET value = '1', updated_at_ms = excluded.updated_at_ms
	`, nowMs).Error; err != nil {
		GlobalSecurityMetrics.IncDatabaseError()
		return err
	}

	return VerifyOpsSchemaIntegrity(db)
}

func recordMigration(db *gorm.DB, version int, name string, appliedAtMs int64, checksum string) error {
	return db.Exec(`
		INSERT INTO ops_schema_migrations (version, name, applied_at_ms, checksum)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(version) DO NOTHING
	`, version, name, appliedAtMs, checksum).Error
}

func backfillLegacyLeaseMillis(db *gorm.DB) error {
	type legacyLease struct {
		Component    string    `gorm:"column:component"`
		AcquiredAt   time.Time `gorm:"column:acquired_at"`
		ExpiresAt    time.Time `gorm:"column:expires_at"`
		AcquiredAtMs int64     `gorm:"column:acquired_at_ms"`
		ExpiresAtMs  int64     `gorm:"column:expires_at_ms"`
	}
	var rows []legacyLease
	if err := db.Raw("SELECT component, acquired_at, expires_at, acquired_at_ms, expires_at_ms FROM ops_recovery_leases WHERE expires_at_ms = 0").Scan(&rows).Error; err != nil {
		return err
	}
	for _, r := range rows {
		acqMs := r.AcquiredAtMs
		if acqMs == 0 && !r.AcquiredAt.IsZero() {
			acqMs = r.AcquiredAt.UTC().UnixMilli()
		}
		expMs := r.ExpiresAtMs
		if expMs == 0 && !r.ExpiresAt.IsZero() {
			expMs = r.ExpiresAt.UTC().UnixMilli()
		}
		if err := db.Exec("UPDATE ops_recovery_leases SET acquired_at_ms = ?, expires_at_ms = ? WHERE component = ? AND expires_at_ms = 0",
			acqMs, expMs, r.Component).Error; err != nil {
			return err
		}
	}
	return nil
}

// VerifyOpsSchemaIntegrity performs a strict check of SQLite file health (PRAGMA quick_check),
// migration version, and required tables & columns before exiting Degraded mode or trusting storage.
func VerifyOpsSchemaIntegrity(db *gorm.DB) error {
	if db == nil {
		return ErrLeaseDBRequired
	}

	var quickCheck string
	if err := db.Raw("PRAGMA quick_check").Scan(&quickCheck).Error; err != nil {
		GlobalSecurityMetrics.IncDatabaseError()
		return fmt.Errorf("%w: quick_check error: %v", ErrSchemaIntegrityFailed, err)
	}
	if !strings.EqualFold(strings.TrimSpace(quickCheck), "ok") {
		GlobalSecurityMetrics.IncDatabaseError()
		return fmt.Errorf("%w: PRAGMA quick_check returned %q", ErrSchemaIntegrityFailed, quickCheck)
	}

	hasMigrations, err := tableExists(db, "ops_schema_migrations")
	if err != nil {
		return err
	}
	if !hasMigrations {
		return fmt.Errorf("%w: ops_schema_migrations missing", ErrSchemaTableMissing)
	}

	var ver int
	if err := db.Raw("SELECT COALESCE(MAX(version), 0) FROM ops_schema_migrations").Scan(&ver).Error; err != nil {
		return err
	}
	if ver < RequiredOpsSchemaVersion {
		return fmt.Errorf("%w: got v%d, need >= v%d", ErrSchemaVersionMismatch, ver, RequiredOpsSchemaVersion)
	}

	requiredColumns := map[string][]string{
		"ops_recovery_leases":            {"component", "holder", "lease_token", "fencing_generation", "acquired_at_ms", "expires_at_ms", "state"},
		"ops_recovery_lease_generations": {"component", "last_generation"},
		"ops_circuit_breaker_states":     {"container", "state", "crash_timestamps", "opened_at_ms", "active_probe_token", "active_probe_expires_ms", "degraded"},
		"ops_maintenance_locks":          {"active", "locked_by", "expires_at"},
		"pending_proposals":              {"proposal_id", "status", "version", "revision", "baseline_hash", "approval_digest", "execution_token"},
		"offline_restart_history":        {"container_name", "restart_time", "restart_time_ms"},
		"offline_ops_audit":              {"event_uuid", "operation_id", "container_name", "audit_status", "fencing_generation"},
	}

	for tbl, cols := range requiredColumns {
		exists, tErr := tableExists(db, tbl)
		if tErr != nil {
			return tErr
		}
		if !exists {
			return fmt.Errorf("%w: table %s missing", ErrSchemaTableMissing, tbl)
		}
		colMap, cErr := getTableColumns(db, tbl)
		if cErr != nil {
			return cErr
		}
		for _, reqCol := range cols {
			if !colMap[reqCol] {
				return fmt.Errorf("%w: table %s missing column %s", ErrSchemaColumnMissing, tbl, reqCol)
			}
		}
	}

	return nil
}
