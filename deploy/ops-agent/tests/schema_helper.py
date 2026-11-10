"""
Shared official Round 4 SQLite v4 schema bootstrapper for Python ops-agent tests.
Mirrors services/api/internal/ops/migrations.go 1-to-1 so tests exercise real
versioned schema & integrity checks without relying on runtime CREATE TABLE DDL.
"""

import sqlite3
import time


def init_ops_v4_schema(db_path: str) -> str:
    """Initializes the complete v4 ops SQLite schema on db_path in WAL mode."""
    now_ms = int(time.time() * 1000)
    conn = sqlite3.connect(db_path, timeout=5.0)
    with conn:
        conn.execute("PRAGMA journal_mode=WAL;")
        conn.execute("PRAGMA busy_timeout=5000;")
        conn.execute("""
            CREATE TABLE IF NOT EXISTS ops_schema_migrations (
                version INTEGER PRIMARY KEY,
                name TEXT NOT NULL,
                applied_at_ms INTEGER NOT NULL,
                checksum TEXT NOT NULL
            )
        """)
        conn.execute("""
            CREATE TABLE IF NOT EXISTS ops_schema_meta (
                key TEXT PRIMARY KEY,
                value TEXT NOT NULL,
                updated_at_ms INTEGER NOT NULL
            )
        """)
        conn.execute("""
            CREATE TABLE IF NOT EXISTS ops_maintenance_locks (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                active INTEGER NOT NULL DEFAULT 0,
                locked_by TEXT,
                reason TEXT,
                locked_at DATETIME,
                expires_at DATETIME,
                expires_at_ms INTEGER NOT NULL DEFAULT 0,
                degraded INTEGER NOT NULL DEFAULT 0,
                updated_at DATETIME
            )
        """)
        conn.execute("""
            CREATE TABLE IF NOT EXISTS ops_circuit_breaker_states (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                container TEXT UNIQUE NOT NULL,
                state TEXT NOT NULL DEFAULT 'CLOSED',
                opened_at DATETIME,
                opened_at_ms INTEGER NOT NULL DEFAULT 0,
                crash_timestamps TEXT,
                active_probe_token TEXT DEFAULT '',
                active_probe_issued_ms INTEGER NOT NULL DEFAULT 0,
                active_probe_expires_ms INTEGER NOT NULL DEFAULT 0,
                degraded INTEGER NOT NULL DEFAULT 0,
                updated_at DATETIME,
                updated_at_ms INTEGER NOT NULL DEFAULT 0
            )
        """)
        conn.execute("""
            CREATE TABLE IF NOT EXISTS ops_recovery_leases (
                component TEXT PRIMARY KEY,
                holder TEXT NOT NULL,
                lease_token TEXT NOT NULL,
                fencing_generation INTEGER NOT NULL DEFAULT 1,
                acquired_at DATETIME NOT NULL,
                expires_at DATETIME NOT NULL,
                acquired_at_ms INTEGER NOT NULL DEFAULT 0,
                expires_at_ms INTEGER NOT NULL DEFAULT 0,
                state TEXT NOT NULL DEFAULT 'ACTIVE'
            )
        """)
        conn.execute("""
            CREATE TABLE IF NOT EXISTS ops_recovery_lease_generations (
                component TEXT PRIMARY KEY,
                last_generation INTEGER NOT NULL DEFAULT 0
            )
        """)
        conn.execute("""
            CREATE TABLE IF NOT EXISTS offline_restart_history (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                container_name TEXT NOT NULL,
                restart_time REAL NOT NULL DEFAULT 0,
                restart_time_ms INTEGER NOT NULL DEFAULT 0
            )
        """)
        conn.execute("""
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
                synced INTEGER DEFAULT 0,
                run_id TEXT NOT NULL DEFAULT '',
                correlation_id TEXT NOT NULL DEFAULT '',
                event_type TEXT NOT NULL DEFAULT 'EXECUTION_COMPLETED',
                payload_json TEXT NOT NULL DEFAULT '',
                offline_seq INTEGER NOT NULL DEFAULT 0,
                local_prev_hash TEXT NOT NULL DEFAULT '',
                entry_hash TEXT NOT NULL DEFAULT '',
                hmac_signature TEXT NOT NULL DEFAULT ''
            )
        """)
        conn.execute("""
            CREATE TABLE IF NOT EXISTS pending_proposals (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                proposal_id TEXT UNIQUE,
                idempotency_key TEXT DEFAULT '',
                household_id INTEGER DEFAULT 1,
                title TEXT NOT NULL,
                description TEXT,
                target_component TEXT,
                risk_level TEXT NOT NULL,
                estimated_seconds INTEGER DEFAULT 30,
                downtime_seconds INTEGER DEFAULT 0,
                execution_plan TEXT NOT NULL DEFAULT '',
                rollback_plan TEXT NOT NULL DEFAULT '',
                status TEXT NOT NULL DEFAULT 'AWAITING_APPROVAL',
                scheduled_at TEXT,
                approved_at DATETIME,
                approved_by TEXT DEFAULT '',
                executed_at DATETIME,
                reconciled_at DATETIME,
                execution_log TEXT DEFAULT '',
                baseline_hash TEXT DEFAULT '',
                action_spec TEXT DEFAULT '',
                execution_policy TEXT DEFAULT '',
                approval_digest TEXT DEFAULT '',
                execution_token TEXT DEFAULT '',
                version INTEGER NOT NULL DEFAULT 1,
                revision INTEGER NOT NULL DEFAULT 1,
                created_at TEXT NOT NULL,
                updated_at TEXT
            )
        """)
        for ver, name in (
            (1, "base_ops_schema"),
            (2, "cross_language_lease_protocol"),
            (3, "circuit_breaker_lock_persistence"),
            (4, "proposal_cas_revision_token_policy"),
        ):
            conn.execute(
                "INSERT OR IGNORE INTO ops_schema_migrations (version, name, applied_at_ms, checksum) VALUES (?, ?, ?, ?)",
                (ver, name, now_ms, f"v{ver}"),
            )
        conn.execute(
            "INSERT OR REPLACE INTO ops_schema_meta (key, value, updated_at_ms) VALUES ('bootstrapped', '1', ?)",
            (now_ms,),
        )
    conn.close()
    return db_path


def init_ops_v5_schema(db_path: str) -> str:
    """Initializes the complete v5 ops + AgentAuditEngine schema matching Go EnsureOpsSchema v5."""
    init_ops_v4_schema(db_path)
    now_ms = int(time.time() * 1000)
    conn = sqlite3.connect(db_path, timeout=5.0)
    with conn:
        conn.execute("""
            CREATE TABLE IF NOT EXISTS agent_audit_runs (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                run_id TEXT UNIQUE NOT NULL,
                correlation_id TEXT NOT NULL,
                household_id INTEGER NOT NULL DEFAULT 1,
                trigger_source TEXT NOT NULL,
                actor_type TEXT NOT NULL,
                actor_id TEXT NOT NULL,
                action TEXT NOT NULL,
                target TEXT DEFAULT '',
                component TEXT DEFAULT '',
                decision TEXT DEFAULT '',
                risk_level TEXT DEFAULT 'MEDIUM',
                status TEXT NOT NULL,
                proposal_id TEXT DEFAULT '',
                approval_id TEXT DEFAULT '',
                operation_id TEXT DEFAULT '',
                summary TEXT DEFAULT '',
                error_code TEXT DEFAULT '',
                is_offline INTEGER NOT NULL DEFAULT 0,
                synced INTEGER NOT NULL DEFAULT 1,
                event_count INTEGER NOT NULL DEFAULT 0,
                first_seq INTEGER NOT NULL DEFAULT 0,
                last_seq INTEGER NOT NULL DEFAULT 0,
                last_audit_id TEXT DEFAULT '',
                last_entry_hash TEXT DEFAULT '',
                integrity_status TEXT NOT NULL DEFAULT 'CHAIN_INTACT_LOCAL_ONLY',
                started_at DATETIME NOT NULL,
                started_at_ms INTEGER NOT NULL,
                ended_at DATETIME,
                ended_at_ms INTEGER NOT NULL DEFAULT 0,
                duration_ms INTEGER NOT NULL DEFAULT 0
            )
        """)
        conn.execute("""
            CREATE TABLE IF NOT EXISTS agent_audit_events (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                stream_id TEXT NOT NULL DEFAULT 'core',
                seq INTEGER NOT NULL,
                audit_id TEXT UNIQUE NOT NULL,
                run_id TEXT NOT NULL,
                parent_event_id TEXT DEFAULT '',
                correlation_id TEXT NOT NULL,
                household_id INTEGER NOT NULL DEFAULT 1,
                trigger_source TEXT NOT NULL,
                actor_type TEXT NOT NULL,
                actor_id TEXT NOT NULL,
                event_type TEXT NOT NULL,
                action TEXT NOT NULL,
                target TEXT DEFAULT '',
                component TEXT DEFAULT '',
                decision TEXT DEFAULT '',
                risk_level TEXT DEFAULT 'MEDIUM',
                policy_version TEXT DEFAULT 'v5.0',
                proposal_id TEXT DEFAULT '',
                approval_id TEXT DEFAULT '',
                operation_id TEXT DEFAULT '',
                fencing_generation INTEGER NOT NULL DEFAULT 0,
                before_state TEXT DEFAULT '',
                after_state TEXT DEFAULT '',
                parameters_digest TEXT DEFAULT '',
                parameters_redacted TEXT DEFAULT '',
                result TEXT DEFAULT '',
                error_code TEXT DEFAULT '',
                duration_ms INTEGER NOT NULL DEFAULT 0,
                evidence TEXT DEFAULT '',
                created_at DATETIME NOT NULL,
                created_at_ms INTEGER NOT NULL,
                is_offline INTEGER NOT NULL DEFAULT 0,
                synced INTEGER NOT NULL DEFAULT 1,
                origin_stream_id TEXT DEFAULT '',
                origin_seq INTEGER NOT NULL DEFAULT 0,
                origin_entry_hash TEXT DEFAULT '',
                previous_hash TEXT NOT NULL,
                entry_hash TEXT UNIQUE NOT NULL,
                hmac_signature TEXT NOT NULL,
                UNIQUE(stream_id, seq)
            )
        """)
        conn.execute("""
            CREATE TABLE IF NOT EXISTS agent_audit_checkpoints (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                checkpoint_id TEXT UNIQUE NOT NULL,
                stream_id TEXT NOT NULL DEFAULT 'core',
                from_seq INTEGER NOT NULL,
                to_seq INTEGER NOT NULL,
                event_count INTEGER NOT NULL,
                head_entry_hash TEXT NOT NULL,
                tail_entry_hash TEXT NOT NULL,
                chain_digest TEXT NOT NULL,
                prev_chain_digest TEXT NOT NULL,
                key_id TEXT NOT NULL,
                hmac_signature TEXT NOT NULL,
                archive_file TEXT DEFAULT '',
                created_at DATETIME NOT NULL,
                created_at_ms INTEGER NOT NULL
            )
        """)
        conn.execute("""
            CREATE TRIGGER IF NOT EXISTS trg_agent_audit_events_no_update
            BEFORE UPDATE ON agent_audit_events
            BEGIN
                SELECT RAISE(ABORT, 'agent_audit_events is append-only');
            END;
        """)
        conn.execute("""
            CREATE TRIGGER IF NOT EXISTS trg_agent_audit_events_no_delete
            BEFORE DELETE ON agent_audit_events
            BEGIN
                SELECT RAISE(ABORT, 'agent_audit_events is append-only');
            END;
        """)
        conn.execute("""
            CREATE TRIGGER IF NOT EXISTS trg_agent_audit_checkpoints_no_update
            BEFORE UPDATE ON agent_audit_checkpoints
            BEGIN
                SELECT RAISE(ABORT, 'agent_audit_checkpoints is append-only');
            END;
        """)
        conn.execute("""
            CREATE TRIGGER IF NOT EXISTS trg_agent_audit_checkpoints_no_delete
            BEFORE DELETE ON agent_audit_checkpoints
            BEGIN
                SELECT RAISE(ABORT, 'agent_audit_checkpoints is append-only');
            END;
        """)
        conn.execute(
            "INSERT OR IGNORE INTO ops_schema_migrations (version, name, applied_at_ms, checksum) VALUES (5, 'agent_audit_engine_v5', ?, 'v5')",
            (now_ms,),
        )
    conn.close()
    return db_path
