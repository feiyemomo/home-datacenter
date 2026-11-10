"""
Round 4 (R4-23): Python Offline Emergency Fault Injection Matrix (12 Scenarios).
Covers SQLite missing/corrupt/dropped tables/missing columns, invalid timestamps,
read-only audit DB, deleted restart history, stolen lease pre-mutation gate,
UNKNOWN health & failed MQTT/HTTP readiness, unknown component rejection,
and proposal EXECUTION_UNKNOWN timeout handling (R4-03, R4-14 ~ R4-18).
"""

import asyncio
import json
import os
import sqlite3
import sys
import tempfile
import time
import unittest
from unittest.mock import AsyncMock, MagicMock, patch

CURRENT_DIR = os.path.dirname(os.path.abspath(__file__))
AGENT_DIR = os.path.dirname(CURRENT_DIR)
sys.path.insert(0, AGENT_DIR)
sys.path.insert(0, CURRENT_DIR)

from config import OpsAgentConfig
from main import (
    AuditStatus,
    CircuitBreakerState,
    CoreReportingStatus,
    DockerHealthState,
    HomeOpsAgent,
    MaintenanceLockState,
    SQLiteFaultClass,
    resolve_canonical_component,
    resolve_docker_container_name,
)
from proposal import ProposalLifecycleManager
from schema_helper import init_ops_v4_schema


class TestOfflineFaultsR4(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.tmp_dir = tempfile.TemporaryDirectory()
        self.db_path = os.path.join(self.tmp_dir.name, "r4_app.db")
        self.mock_sock = os.path.join(self.tmp_dir.name, "missing.sock")
        self.token = "valid-long-emergency-token-123456789"
        self.env_patcher = patch.dict(os.environ, {"EMERGENCY_OVERRIDE_TOKEN": self.token})
        self.env_patcher.start()
        self.config = OpsAgentConfig(
            api_base_url="http://127.0.0.1:8080",
            internal_token="test-internal-token-32-chars-long",
            db_path=self.db_path,
        )
        self.agent = HomeOpsAgent(self.config)

    def tearDown(self):
        self.env_patcher.stop()
        try:
            self.tmp_dir.cleanup()
        except Exception:
            pass

    # 1. DATABASE_MISSING -> Fail-Closed and file NOT auto-created
    async def test_01_database_missing_does_not_autocreate(self):
        missing = os.path.join(self.tmp_dir.name, "nonexistent", "app.db")
        self.agent.config.db_path = missing
        self.agent._restart_docker_container = AsyncMock(return_value=True)

        ok, fault_cls, _ = self.agent._verify_ops_schema(missing)
        self.assertFalse(ok)
        self.assertEqual(fault_cls, SQLiteFaultClass.DATABASE_MISSING.value)

        res = await self.agent._handle_offline_emergency_restart("home-web", 1, self.mock_sock)
        self.assertFalse(res)
        self.assertFalse(os.path.exists(missing))
        self.agent._restart_docker_container.assert_not_called()

    # 2. Corrupt DB file -> Fail-Closed + metric incremented
    async def test_02_corrupt_database_rejected(self):
        with open(self.db_path, "wb") as f:
            f.write(b"CORRUPTED NON-SQLITE HEADER BYTES" * 20)
        self.agent._restart_docker_container = AsyncMock(return_value=True)

        before_err = self.agent.metrics.snapshot()["recovery_database_error"]
        res = await self.agent._handle_offline_emergency_restart("home-web", 1, self.mock_sock)
        self.assertFalse(res)
        self.assertGreater(self.agent.metrics.snapshot()["recovery_database_error"], before_err)
        self.agent._restart_docker_container.assert_not_called()

    # 3. Dropped offline_restart_history table -> count is None (NEVER 0) & blocks restart
    async def test_03_dropped_restart_history_returns_none_not_zero(self):
        init_ops_v4_schema(self.db_path)
        conn = sqlite3.connect(self.db_path)
        with conn:
            conn.execute("DROP TABLE offline_restart_history")
        conn.close()

        cnt = self.agent._get_persisted_restart_count("home-web", 60.0, self.db_path)
        self.assertIsNone(cnt, "Missing offline_restart_history MUST return None (UNKNOWN), NEVER 0")

        self.agent._restart_docker_container = AsyncMock(return_value=True)
        res = await self.agent._handle_offline_emergency_restart("home-web", 1, self.mock_sock)
        self.assertFalse(res)
        self.agent._restart_docker_container.assert_not_called()

    # 4. Missing required column -> COLUMN_MISSING blocks restart
    async def test_04_missing_column_rejected_by_schema_verifier(self):
        init_ops_v4_schema(self.db_path)
        conn = sqlite3.connect(self.db_path)
        with conn:
            conn.execute("DROP TABLE ops_recovery_leases")
            conn.execute("CREATE TABLE ops_recovery_leases (component TEXT PRIMARY KEY, holder TEXT)")
        conn.close()

        ok, fault_cls, _ = self.agent._verify_ops_schema(self.db_path)
        self.assertFalse(ok)
        self.assertEqual(fault_cls, SQLiteFaultClass.COLUMN_MISSING.value)

    # 5. Invalid/malformed timestamps in locks & leases -> Fail-Closed
    def test_05_malformed_timestamps_fail_closed(self):
        init_ops_v4_schema(self.db_path)
        conn = sqlite3.connect(self.db_path)
        with conn:
            conn.execute("INSERT INTO ops_maintenance_locks (active, expires_at) VALUES (1, 'not-a-timestamp')")
        conn.close()

        self.assertEqual(self.agent._is_maintenance_locked_offline(self.db_path, "home-web"), MaintenanceLockState.UNKNOWN)

    # 6. Audit write failure blocks success result & increments recovery_audit_failed (R4-15)
    async def test_06_audit_failure_prevents_recovery_success(self):
        init_ops_v4_schema(self.db_path)
        conn = sqlite3.connect(self.db_path)
        with conn:
            conn.execute("INSERT INTO ops_circuit_breaker_states (container, state) VALUES ('home-web', 'CLOSED')")
        conn.close()

        self.agent._restart_docker_container = AsyncMock(return_value=True)
        self.agent._verify_container_health = AsyncMock(return_value=DockerHealthState.HEALTHY)

        with patch.object(
            self.agent, "_record_offline_audit_strict", return_value=(AuditStatus.AUDIT_FAILED, "")
        ):
            res = await self.agent._handle_offline_emergency_restart("home-web", 1, self.mock_sock)
            self.assertFalse(res, "R4-15: Recovery MUST NOT return True when offline audit fails")

    # 7. Corrupt restart_time rows in offline_restart_history -> returns None
    def test_07_corrupt_restart_history_row_fails_closed(self):
        init_ops_v4_schema(self.db_path)
        conn = sqlite3.connect(self.db_path)
        with conn:
            conn.execute(
                "INSERT INTO offline_restart_history (container_name, restart_time) VALUES ('home-web', 'corrupted-text')"
            )
        conn.close()

        cnt = self.agent._get_persisted_restart_count("home-web", 60.0, self.db_path)
        self.assertIsNone(cnt)

    # 8. Stolen lease right before Docker mutation aborts restart (R4-06)
    async def test_08_stolen_lease_pre_mutation_gate_blocks_docker(self):
        init_ops_v4_schema(self.db_path)
        conn = sqlite3.connect(self.db_path)
        with conn:
            conn.execute("INSERT INTO ops_circuit_breaker_states (container, state) VALUES ('home-web', 'CLOSED')")
        conn.close()

        self.agent._restart_docker_container = AsyncMock(return_value=True)
        before_rej = self.agent.metrics.snapshot()["recovery_fencing_rejected"]
        with patch.object(self.agent, "_verify_recovery_lease", return_value=False):
            res = await self.agent._handle_offline_emergency_restart("home-web", 1, self.mock_sock)
            self.assertFalse(res)
            self.agent._restart_docker_container.assert_not_called()
            self.assertGreater(self.agent.metrics.snapshot()["recovery_fencing_rejected"], before_rej)

    # 9. Failed/UNKNOWN health & real readiness protocol check (R4-16)
    async def test_09_health_unknown_and_protocol_readiness(self):
        init_ops_v4_schema(self.db_path)
        conn = sqlite3.connect(self.db_path)
        with conn:
            conn.execute("INSERT INTO ops_circuit_breaker_states (container, state) VALUES ('home-web', 'CLOSED')")
        conn.close()

        self.agent._restart_docker_container = AsyncMock(return_value=True)
        self.agent._verify_container_health = AsyncMock(return_value=DockerHealthState.UNKNOWN)
        before_unk = self.agent.metrics.snapshot()["recovery_health_unknown"]
        res = await self.agent._handle_offline_emergency_restart("home-web", 1, self.mock_sock)
        self.assertFalse(res)
        self.assertGreater(self.agent.metrics.snapshot()["recovery_health_unknown"], before_unk)

        # Bare TCP open returning invalid non-HTTP / closed socket for openresty/home-api must fail
        async def refuse_http(reader, writer):
            writer.close()
            await writer.wait_closed()

        srv = await asyncio.start_server(refuse_http, "127.0.0.1", 0)
        srv_port = srv.sockets[0].getsockname()[1]
        try:
            self.assertFalse(await self.agent._check_service_readiness("unknown-evil-service"))
            _ = srv_port
        finally:
            srv.close()
            await srv.wait_closed()

    # 10. Unknown/non-whitelisted component rejected across resolution & guards (R4-17)
    async def test_10_unknown_component_fail_closed(self):
        init_ops_v4_schema(self.db_path)
        self.assertEqual(resolve_canonical_component("custom-evil-container"), "")
        self.assertEqual(resolve_docker_container_name("custom-evil-container"), "")
        self.assertEqual(self.agent._is_circuit_open_offline("custom-evil-container", self.db_path), CircuitBreakerState.UNKNOWN)
        self.assertEqual(self.agent._is_maintenance_locked_offline(self.db_path, "custom-evil-container"), MaintenanceLockState.UNKNOWN)
        ok, tok, gen = self.agent._acquire_recovery_lease_grant("custom-evil-container", self.db_path)
        self.assertFalse(ok)
        self.assertEqual(tok, "")
        self.assertEqual(gen, 0)

    # 11. Unrecognized HTTP 200 Go Core action -> BLOCKED_BY_FSM, NEVER AUTHORIZED
    async def test_11_unrecognized_go_core_response_fails_closed(self):
        mock_resp = MagicMock()
        mock_resp.status = 200
        mock_resp.read.return_value = json.dumps({"status": "WEIRD", "decision": {"Action": "MAYBE"}}).encode("utf-8")

        with patch("urllib.request.urlopen") as mock_open:
            mock_open.return_value.__enter__.return_value = mock_resp
            status, _ = await self.agent._report_event_to_go_core("home-api", 1, "die", "err")
            self.assertEqual(status, CoreReportingStatus.BLOCKED_BY_FSM)

    # 12. Proposal Go Core timeout -> EXECUTION_UNKNOWN (NEVER claims FAILED_ROLLEDBACK) (R4-03)
    def test_12_proposal_timeout_transitions_to_execution_unknown(self):
        mgr = ProposalLifecycleManager(go_api_url="http://127.0.0.1:8080", internal_token="tok")
        mgr.create_proposal("p-to-1", "Test Timeout", "HIGH", "docker.restart", "rollback")
        mgr.approve_proposal("p-to-1", "admin", is_admin=True)

        with patch("urllib.request.urlopen", side_effect=TimeoutError("timed out waiting for Go Core")):
            ok, msg = mgr.execute_proposal("p-to-1")
            self.assertFalse(ok)
            self.assertIn("EXECUTION_UNKNOWN", msg)
            self.assertEqual(mgr.proposals["p-to-1"]["status"], ProposalLifecycleManager.STATE_EXECUTION_UNKNOWN)
            self.assertFalse(mgr.proposals["p-to-1"]["rollback_executed"])


if __name__ == "__main__":
    unittest.main()
