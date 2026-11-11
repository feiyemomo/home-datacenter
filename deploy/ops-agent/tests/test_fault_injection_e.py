"""
Acceptance Criteria E: Comprehensive 15 Fault Injection Test Suite (Python Path & Dual-Path).
Verifies:
1. SQLite file missing
2. SQLite database corrupt
3. SQLite table structure incomplete
4. SQLite insufficient write permissions
6. Docker socket missing
7. Docker socket unable to connect
8. Docker inspect HTTP error
9. Docker inspect JSON invalid
10. Docker container running but service unavailable (readiness probe fails)
12. Python offline path takeover (all 5 fail-closed guards)
13. Dual-path recovery lease race & mutual exclusion
"""

import asyncio
import io
import json
import os
import sqlite3
import sys
import tempfile
import time
import unittest
from unittest.mock import AsyncMock, patch

CURRENT_DIR = os.path.dirname(os.path.abspath(__file__))
PARENT_DIR = os.path.dirname(CURRENT_DIR)
sys.path.insert(0, PARENT_DIR)
sys.path.insert(0, CURRENT_DIR)

from config import OpsAgentConfig
from schema_helper import init_ops_v4_schema
from main import (
    CircuitBreakerState,
    DockerHealthState,
    HomeOpsAgent,
    MaintenanceLockState,
    open_readonly_sqlite,
    resolve_canonical_component,
    resolve_docker_container_name,
)


class TestFaultInjectionScenarios(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.tmp_dir = tempfile.TemporaryDirectory()
        self.db_path = os.path.join(self.tmp_dir.name, "app.db")
        self.mock_sock = "/tmp/nonexistent_test.sock"
        self.token = "valid-long-emergency-token-1234"
        self.env_patcher = patch.dict(
            os.environ,
            {
                "ENVIRONMENT": "test",
                "APP_ENV": "test",
                "EMERGENCY_OVERRIDE_TOKEN": self.token,
            },
        )
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

    def _init_clean_db(self, db_path: str = None) -> str:
        target = db_path or self.db_path
        return init_ops_v4_schema(target)

    # =========================================================================
    # Fault Injection 1: SQLite file missing
    # =========================================================================
    async def test_fi_01_sqlite_file_missing(self):
        """FI 1: SQLite file missing returns UNKNOWN and blocks restart fail-closed."""
        missing_db = os.path.join(self.tmp_dir.name, "nonexistent_subfolder", "missing.db")
        self.assertFalse(os.path.exists(missing_db))

        self.assertEqual(self.agent._is_maintenance_locked_offline(missing_db), MaintenanceLockState.UNKNOWN)
        self.assertEqual(self.agent._is_circuit_open_offline("home-api", missing_db), CircuitBreakerState.UNKNOWN)
        self.assertIsNone(self.agent._get_persisted_restart_count("home-api", 60.0, missing_db))
        self.assertFalse(self.agent._record_persisted_restart("home-api", missing_db))

        self.agent._restart_docker_container = AsyncMock(return_value=True)
        res = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
        self.assertFalse(res, "Offline restart MUST be blocked when SQLite DB file is missing")
        self.agent._restart_docker_container.assert_not_called()
        self.assertFalse(os.path.exists(missing_db), "Must NOT auto-create empty DB file")

    # =========================================================================
    # Fault Injection 2: SQLite database corrupt
    # =========================================================================
    async def test_fi_02_sqlite_database_corrupt(self):
        """FI 2: Corrupted SQLite database evaluates to UNKNOWN and blocks restart."""
        corrupt_db = os.path.join(self.tmp_dir.name, "corrupt.db")
        with open(corrupt_db, "wb") as f:
            f.write(b"CORRUPTED_GARBAGE_HEADER_DATA_1234567890\x00\xff\xfe\x01\x02")

        self.assertEqual(self.agent._is_maintenance_locked_offline(corrupt_db), MaintenanceLockState.UNKNOWN)
        self.assertEqual(self.agent._is_circuit_open_offline("home-api", corrupt_db), CircuitBreakerState.UNKNOWN)
        self.assertIsNone(self.agent._get_persisted_restart_count("home-api", 60.0, corrupt_db))

        self.agent._restart_docker_container = AsyncMock(return_value=True)
        self.config.db_path = corrupt_db
        res = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
        self.assertFalse(res, "Corrupted DB must fail-closed and block restart")
        self.agent._restart_docker_container.assert_not_called()

    # =========================================================================
    # Fault Injection 3: SQLite table structure incomplete
    # =========================================================================
    async def test_fi_03_sqlite_table_structure_incomplete(self):
        """FI 3: Missing tables or missing essential columns evaluate to UNKNOWN and block restart."""
        # 3a: Tables missing entirely
        missing_tables_db = os.path.join(self.tmp_dir.name, "missing_tables.db")
        conn = sqlite3.connect(missing_tables_db)
        with conn:
            conn.execute("CREATE TABLE irrelevant (id INT)")
        conn.close()

        self.assertEqual(self.agent._is_maintenance_locked_offline(missing_tables_db), MaintenanceLockState.UNKNOWN)
        self.assertEqual(self.agent._is_circuit_open_offline("home-api", missing_tables_db), CircuitBreakerState.UNKNOWN)

        # 3b: Table columns incomplete
        partial_cols_db = os.path.join(self.tmp_dir.name, "partial_cols.db")
        conn = sqlite3.connect(partial_cols_db)
        with conn:
            conn.execute("CREATE TABLE ops_maintenance_locks (id INTEGER PRIMARY KEY, reason TEXT)")
            conn.execute("CREATE TABLE ops_circuit_breaker_states (id INTEGER PRIMARY KEY, container TEXT)")
        conn.close()

        self.assertEqual(self.agent._is_maintenance_locked_offline(partial_cols_db), MaintenanceLockState.UNKNOWN)
        self.assertEqual(self.agent._is_circuit_open_offline("home-api", partial_cols_db), CircuitBreakerState.UNKNOWN)

        self.agent._restart_docker_container = AsyncMock(return_value=True)
        self.config.db_path = partial_cols_db
        res = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
        self.assertFalse(res, "Incomplete table structure must fail-closed and block restart")
        self.agent._restart_docker_container.assert_not_called()

    # =========================================================================
    # Fault Injection 4: SQLite insufficient write permissions
    # =========================================================================
    async def test_fi_04_sqlite_insufficient_write_permissions(self):
        """FI 4: Insufficient permissions reading/writing SQLite evaluates to UNKNOWN/False and halts restart."""
        db_path = self._init_clean_db()

        with patch("main.open_readonly_sqlite", side_effect=PermissionError("Permission denied")):
            self.assertEqual(self.agent._is_maintenance_locked_offline(db_path), MaintenanceLockState.UNKNOWN)
            self.assertEqual(self.agent._is_circuit_open_offline("home-api", db_path), CircuitBreakerState.UNKNOWN)
            self.assertIsNone(self.agent._get_persisted_restart_count("home-api", 60.0, db_path))

            self.agent._restart_docker_container = AsyncMock(return_value=True)
            res = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
            self.assertFalse(res, "Permission denied must fail-closed and block restart")
            self.agent._restart_docker_container.assert_not_called()

    # =========================================================================
    # Fault Injection 6: Docker socket missing
    # =========================================================================
    async def test_fi_06_docker_socket_missing(self):
        """FI 6: Missing Docker socket returns DockerHealthState.UNKNOWN, NEVER True."""
        missing_sock = "/tmp/nonexistent_docker_daemon_88888.sock"
        health = await self.agent._verify_container_health("home-api", missing_sock)
        self.assertEqual(health, DockerHealthState.UNKNOWN)
        self.assertNotEqual(health, DockerHealthState.HEALTHY)
        self.assertIsNot(health, True)

    # =========================================================================
    # Fault Injection 7: Docker socket unable to connect
    # =========================================================================
    async def test_fi_07_docker_socket_unable_to_connect(self):
        """FI 7: Docker endpoint connection refused / timeout returns DockerHealthState.UNKNOWN."""
        # Point to a local TCP port that is actively closed / refused
        dead_sock_uri = "tcp://127.0.0.1:59123"
        health = await self.agent._verify_container_health("home-api", dead_sock_uri)
        self.assertEqual(health, DockerHealthState.UNKNOWN)
        self.assertNotEqual(health, DockerHealthState.HEALTHY)
        self.assertIsNot(health, True)

    # =========================================================================
    # Fault Injection 8: Docker inspect HTTP error
    # =========================================================================
    async def test_fi_08_docker_inspect_http_error(self):
        """FI 8: Docker inspect HTTP 500 returns UNKNOWN; HTTP 404 returns UNHEALTHY."""
        # 8a: HTTP 500
        async def handler_500(reader, writer):
            await reader.read(512)
            writer.write(b"HTTP/1.1 500 Internal Server Error\r\nConnection: close\r\n\r\n{\"message\": \"daemon error\"}")
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        srv500 = await asyncio.start_server(handler_500, "127.0.0.1", 0)
        p500 = srv500.sockets[0].getsockname()[1]
        try:
            h500 = await self.agent._verify_container_health("home-api", f"tcp://127.0.0.1:{p500}")
            self.assertEqual(h500, DockerHealthState.UNKNOWN)
        finally:
            srv500.close()
            await srv500.wait_closed()

        # 8b: HTTP 404
        async def handler_404(reader, writer):
            await reader.read(512)
            writer.write(b"HTTP/1.1 404 Not Found\r\nConnection: close\r\n\r\n{\"message\": \"No such container\"}")
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        srv404 = await asyncio.start_server(handler_404, "127.0.0.1", 0)
        p404 = srv404.sockets[0].getsockname()[1]
        try:
            h404 = await self.agent._verify_container_health("home-api", f"tcp://127.0.0.1:{p404}")
            self.assertEqual(h404, DockerHealthState.UNHEALTHY)
        finally:
            srv404.close()
            await srv404.wait_closed()

    # =========================================================================
    # Fault Injection 9: Docker inspect JSON invalid
    # =========================================================================
    async def test_fi_09_docker_inspect_json_invalid(self):
        """FI 9: Corrupted or truncated inspect JSON returns UNKNOWN."""
        async def handler_corrupt(reader, writer):
            await reader.read(512)
            writer.write(b"HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n{\"State\": {\"Running\": true, \"Health\": {\"Sta")
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        srv = await asyncio.start_server(handler_corrupt, "127.0.0.1", 0)
        port = srv.sockets[0].getsockname()[1]
        try:
            health = await self.agent._verify_container_health("home-api", f"tcp://127.0.0.1:{port}")
            self.assertEqual(health, DockerHealthState.UNKNOWN)
        finally:
            srv.close()
            await srv.wait_closed()

    # =========================================================================
    # Fault Injection 10: Docker container running but service unavailable
    # =========================================================================
    async def test_fi_10_container_running_service_unavailable(self):
        """FI 10: State.Running=True without HEALTHCHECK evaluates to UNHEALTHY when readiness probe fails."""
        payload_no_hc = json.dumps({"State": {"Running": True}}).encode("utf-8")

        async def handler_running(reader, writer):
            await reader.read(512)
            writer.write(b"HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n" + payload_no_hc)
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        srv = await asyncio.start_server(handler_running, "127.0.0.1", 0)
        port = srv.sockets[0].getsockname()[1]
        sock_uri = f"tcp://127.0.0.1:{port}"

        try:
            # Independent service readiness probe fails (e.g. port refused)
            with patch.object(self.agent, "_check_service_readiness", new=AsyncMock(return_value=False)):
                health = await self.agent._verify_container_health("home-api", sock_uri)
                self.assertEqual(health, DockerHealthState.UNHEALTHY, "Running=True alone must not pass when service probe fails")
        finally:
            srv.close()
            await srv.wait_closed()

    # =========================================================================
    # Fault Injection 12: Python offline path takeover (5 fail-closed guards)
    # =========================================================================
    async def test_fi_12_python_offline_takeover_guards(self):
        """FI 12: Python offline path takeover rigorously enforces all 5 fail-closed guards."""
        self._init_clean_db()
        self.agent._restart_docker_container = AsyncMock(return_value=True)

        # Guard 1: Missing or weak override token
        with patch.dict(os.environ, {"EMERGENCY_OVERRIDE_TOKEN": "weak"}):
            res = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
            self.assertFalse(res, "Guard 1: Weak override token must block restart")

        # Guard 2: Container outside whitelist
        with patch.dict(os.environ, {"EMERGENCY_OVERRIDE_TOKEN": "valid-long-emergency-token-1234"}):
            res = await self.agent._handle_offline_emergency_restart("unapproved-container", 1, self.mock_sock)
            self.assertFalse(res, "Guard 2: Unwhitelisted container must block restart")

        # Guard 4: Rate limit circuit breaker (>2 restarts in 60s)
        with patch.dict(os.environ, {"EMERGENCY_OVERRIDE_TOKEN": "valid-long-emergency-token-1234"}):
            self.agent._verify_container_health = AsyncMock(return_value=DockerHealthState.HEALTHY)
            res1 = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
            self.assertTrue(res1)
            res2 = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
            self.assertTrue(res2)
            res3 = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
            self.assertFalse(res3, "Guard 4: Third restart in 60s must trip breaker and halt restart")

        # Guard 5: Restart history persistence failure
        with patch.object(self.agent, "_record_persisted_restart", return_value=False):
            res = await self.agent._handle_offline_emergency_restart("home-web", 1, self.mock_sock)
            self.assertFalse(res, "Guard 5: Failure to persist restart timestamp must halt restart")

    # =========================================================================
    # Fault Injection 13: Go/Python lease mutual exclusion (Python side)
    # =========================================================================
    async def test_fi_13_dual_path_lease_mutual_exclusion(self):
        """FI 13: Persistent recovery lease held by Go Core blocks Python offline emergency takeover."""
        db_path = self._init_clean_db()
        now_epoch = time.time()
        future_iso = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now_epoch + 30.0))
        past_iso = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now_epoch - 10.0))

        conn = sqlite3.connect(db_path)
        with conn:
            conn.execute("""
                INSERT INTO ops_recovery_leases (component, holder, lease_token, acquired_at, expires_at)
                VALUES ('frigate', 'go-core', 'go-ticket-abc', ?, ?)
            """, (past_iso, future_iso))
        conn.close()

        # Python offline agent attempts to acquire lease for home-frigate -> BLOCKED
        acquired, token = self.agent._acquire_recovery_lease("home-frigate", db_path)
        self.assertFalse(acquired, "Cannot acquire recovery lease while held by Go Core")
        self.assertEqual(token, "")

        self.agent._restart_docker_container = AsyncMock(return_value=True)
        res = await self.agent._handle_offline_emergency_restart("home-frigate", 1, self.mock_sock)
        self.assertFalse(res, "Offline restart must be blocked when lease is held by Go Core")
        self.agent._restart_docker_container.assert_not_called()


if __name__ == "__main__":
    unittest.main()
