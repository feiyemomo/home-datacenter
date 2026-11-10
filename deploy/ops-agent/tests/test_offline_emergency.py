"""
Acceptance Criteria B: Comprehensive Python Offline Emergency Recovery & Docker Health Verification Test Suite.
Verifies all 19 fail-closed failure modes and safety invariants without mock masking.
"""

import asyncio
from datetime import datetime, timezone
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
    parse_sqlite_timestamp,
    resolve_canonical_component,
    resolve_docker_container_name,
)


class TestOfflineEmergencyRecoveryFailClosed(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.tmp_dir = tempfile.TemporaryDirectory()
        self.db_path = os.path.join(self.tmp_dir.name, "app.db")
        self.mock_sock = "/tmp/nonexistent_test.sock"
        self.token = "valid-long-emergency-token-1234"
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

    def _init_clean_db(self, db_path: str = None) -> str:
        target = db_path or self.db_path
        return init_ops_v4_schema(target)

    # =========================================================================
    # 1. SQLite 文件不存在测试
    # =========================================================================
    async def test_scenario_1_sqlite_file_not_found(self):
        """Scenario 1: Non-existent SQLite database must evaluate to UNKNOWN and halt restart."""
        missing_db = os.path.join(self.tmp_dir.name, "missing_subfolder", "nonexistent.db")
        self.assertFalse(os.path.exists(missing_db))

        self.assertEqual(
            self.agent._is_maintenance_locked_offline(missing_db),
            MaintenanceLockState.UNKNOWN,
        )
        self.assertEqual(
            self.agent._is_circuit_open_offline("home-api", missing_db),
            CircuitBreakerState.UNKNOWN,
        )
        self.assertIsNone(self.agent._get_persisted_restart_count("home-api", 60.0, missing_db))
        self.assertFalse(self.agent._record_persisted_restart("home-api", missing_db))

        # Full restart chain must be blocked fail-closed
        self.agent._restart_docker_container = AsyncMock(return_value=True)
        res = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
        self.assertFalse(res, "Offline restart must be strictly blocked when SQLite DB does not exist")
        self.agent._restart_docker_container.assert_not_called()

        # Invariant: Never auto-create an empty database file on read path
        self.assertFalse(os.path.exists(missing_db), "Must NOT auto-create empty DB file on failure")

    # =========================================================================
    # 2. SQLite 无权限测试
    # =========================================================================
    async def test_scenario_2_sqlite_permission_denied(self):
        """Scenario 2: Permission denied opening SQLite database must evaluate to UNKNOWN and halt restart."""
        db_path = self._init_clean_db()

        # Simulate permission denied / access error via open_readonly_sqlite
        with patch("main.open_readonly_sqlite", side_effect=PermissionError("Permission denied: access forbidden")):
            self.assertEqual(
                self.agent._is_maintenance_locked_offline(db_path),
                MaintenanceLockState.UNKNOWN,
            )
            self.assertEqual(
                self.agent._is_circuit_open_offline("home-api", db_path),
                CircuitBreakerState.UNKNOWN,
            )
            self.assertIsNone(self.agent._get_persisted_restart_count("home-api", 60.0, db_path))

            self.agent._restart_docker_container = AsyncMock(return_value=True)
            res = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
            self.assertFalse(res, "Permission denied must fail-closed and block restart")
            self.agent._restart_docker_container.assert_not_called()

    # =========================================================================
    # 3. SQLite 损坏测试
    # =========================================================================
    async def test_scenario_3_sqlite_file_corrupted(self):
        """Scenario 3: Corrupted SQLite database with invalid header must evaluate to UNKNOWN and halt restart."""
        corrupt_db = os.path.join(self.tmp_dir.name, "corrupt_app.db")
        with open(corrupt_db, "wb") as f:
            f.write(b"CORRUPTED_GARBAGE_HEADER_DATA_1234567890\x00\xff\xfe\x01\x02")

        self.assertEqual(
            self.agent._is_maintenance_locked_offline(corrupt_db),
            MaintenanceLockState.UNKNOWN,
        )
        self.assertEqual(
            self.agent._is_circuit_open_offline("home-api", corrupt_db),
            CircuitBreakerState.UNKNOWN,
        )
        self.assertIsNone(self.agent._get_persisted_restart_count("home-api", 60.0, corrupt_db))

        self.agent._restart_docker_container = AsyncMock(return_value=True)
        self.config.db_path = corrupt_db
        res = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
        self.assertFalse(res, "Corrupted SQLite DB must fail-closed and block restart")
        self.agent._restart_docker_container.assert_not_called()

    # =========================================================================
    # 4. 表不存在测试
    # =========================================================================
    async def test_scenario_4_table_missing(self):
        """Scenario 4: Existing SQLite DB lacking required schema tables must evaluate to UNKNOWN and halt restart."""
        db_path = os.path.join(self.tmp_dir.name, "empty_schema.db")
        conn = sqlite3.connect(db_path)
        with conn:
            conn.execute("CREATE TABLE irrelevant_table (id INTEGER PRIMARY KEY, note TEXT)")
        conn.close()

        self.assertEqual(
            self.agent._is_maintenance_locked_offline(db_path),
            MaintenanceLockState.UNKNOWN,
        )
        self.assertEqual(
            self.agent._is_circuit_open_offline("home-api", db_path),
            CircuitBreakerState.UNKNOWN,
        )

        self.agent._restart_docker_container = AsyncMock(return_value=True)
        self.config.db_path = db_path
        res = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
        self.assertFalse(res, "Missing schema tables must fail-closed and block restart")
        self.agent._restart_docker_container.assert_not_called()

    # =========================================================================
    # 5. 表字段不完整测试
    # =========================================================================
    async def test_scenario_5_table_columns_incomplete(self):
        """Scenario 5: Schema tables with missing essential columns must evaluate to UNKNOWN and halt restart."""
        db_path = os.path.join(self.tmp_dir.name, "partial_schema.db")
        conn = sqlite3.connect(db_path)
        with conn:
            # ops_maintenance_locks lacking active or expires_at
            conn.execute("CREATE TABLE ops_maintenance_locks (id INTEGER PRIMARY KEY, locked_by TEXT)")
            # ops_circuit_breaker_states lacking state
            conn.execute("CREATE TABLE ops_circuit_breaker_states (id INTEGER PRIMARY KEY, container TEXT)")
        conn.close()

        self.assertEqual(
            self.agent._is_maintenance_locked_offline(db_path),
            MaintenanceLockState.UNKNOWN,
        )
        self.assertEqual(
            self.agent._is_circuit_open_offline("home-api", db_path),
            CircuitBreakerState.UNKNOWN,
        )

        self.agent._restart_docker_container = AsyncMock(return_value=True)
        self.config.db_path = db_path
        res = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
        self.assertFalse(res, "Incomplete schema columns must fail-closed and block restart")
        self.agent._restart_docker_container.assert_not_called()

    # =========================================================================
    # 6. SQLite 查询异常测试
    # =========================================================================
    async def test_scenario_6_sqlite_query_exception(self):
        """Scenario 6: Operational SQLite exception during query execution must return UNKNOWN and block restart."""
        db_path = self._init_clean_db()

        class ExplodingConnection:
            def cursor(self):
                raise sqlite3.OperationalError("database table is locked by another process")
            def close(self):
                pass

        with patch("main.open_readonly_sqlite", return_value=ExplodingConnection()):
            self.assertEqual(
                self.agent._is_maintenance_locked_offline(db_path),
                MaintenanceLockState.UNKNOWN,
            )
            self.assertEqual(
                self.agent._is_circuit_open_offline("home-api", db_path),
                CircuitBreakerState.UNKNOWN,
            )

            self.agent._restart_docker_container = AsyncMock(return_value=True)
            res = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
            self.assertFalse(res, "Query exceptions must fail-closed and block restart")
            self.agent._restart_docker_container.assert_not_called()

    # =========================================================================
    # 7. UNKNOWN 状态禁止重启测试
    # =========================================================================
    async def test_scenario_7_unknown_state_strictly_halts_restart(self):
        """Scenario 7: UNKNOWN state from any safety guard strictly suppresses autonomous restarts."""
        self._init_clean_db()
        self.agent._restart_docker_container = AsyncMock(return_value=True)

        # 7a: MaintenanceLockState.UNKNOWN blocks restart
        with patch.object(self.agent, "_is_maintenance_locked_offline", return_value=MaintenanceLockState.UNKNOWN):
            res = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
            self.assertFalse(res)
            self.agent._restart_docker_container.assert_not_called()

        # 7b: CircuitBreakerState.UNKNOWN blocks restart
        with patch.object(self.agent, "_is_circuit_open_offline", return_value=CircuitBreakerState.UNKNOWN):
            res = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
            self.assertFalse(res)
            self.agent._restart_docker_container.assert_not_called()

        # 7c: Persisted restart count query returning None blocks restart
        with patch.object(self.agent, "_get_persisted_restart_count", return_value=None):
            res = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
            self.assertFalse(res)
            self.agent._restart_docker_container.assert_not_called()

    # =========================================================================
    # 8. 熔断状态 OPEN 测试
    # =========================================================================
    async def test_scenario_8_circuit_breaker_open_blocks_restart(self):
        """Scenario 8: Tripped OPEN circuit breaker prevents offline restart."""
        db_path = self._init_clean_db()
        conn = sqlite3.connect(db_path)
        with conn:
            conn.execute(
                "INSERT INTO ops_circuit_breaker_states (container, state, opened_at, updated_at) VALUES (?, ?, ?, ?)",
                ("frigate", "OPEN", time.strftime("%Y-%m-%dT%H:%M:%SZ"), time.strftime("%Y-%m-%dT%H:%M:%SZ"))
            )
        conn.close()

        # Query using raw Docker container name home-frigate
        cb_state = self.agent._is_circuit_open_offline("home-frigate", db_path)
        self.assertEqual(cb_state, CircuitBreakerState.OPEN)

        self.agent._restart_docker_container = AsyncMock(return_value=True)
        res = await self.agent._handle_offline_emergency_restart("home-frigate", 1, self.mock_sock)
        self.assertFalse(res, "OPEN circuit breaker must halt restart")
        self.agent._restart_docker_container.assert_not_called()

    # =========================================================================
    # 9. 熔断状态 UNKNOWN 测试
    # =========================================================================
    async def test_scenario_9_circuit_breaker_unknown_blocks_restart(self):
        """Scenario 9: Unrecognized/corrupted circuit breaker state evaluates to UNKNOWN and blocks restart."""
        db_path = self._init_clean_db()
        conn = sqlite3.connect(db_path)
        with conn:
            conn.execute(
                "INSERT INTO ops_circuit_breaker_states (container, state, opened_at, updated_at) VALUES (?, ?, ?, ?)",
                ("frigate", "CORRUPTED_NON_STANDARD_STATE", "", "")
            )
        conn.close()

        cb_state = self.agent._is_circuit_open_offline("home-frigate", db_path)
        self.assertEqual(cb_state, CircuitBreakerState.UNKNOWN)

        self.agent._restart_docker_container = AsyncMock(return_value=True)
        res = await self.agent._handle_offline_emergency_restart("home-frigate", 1, self.mock_sock)
        self.assertFalse(res, "UNKNOWN circuit breaker must halt restart")
        self.agent._restart_docker_container.assert_not_called()

    # =========================================================================
    # 10. 规范化名称匹配测试
    # =========================================================================
    async def test_scenario_10_canonical_name_mapping(self):
        """Scenario 10: Canonical component name resolution aligns with Go Core."""
        # 1-to-1 canonicalization verification
        self.assertEqual(resolve_canonical_component("home-frigate"), "frigate")
        self.assertEqual(resolve_canonical_component("nvr"), "frigate")
        self.assertEqual(resolve_canonical_component("frigate"), "frigate")
        self.assertEqual(resolve_canonical_component("home-mosquitto"), "mosquitto")
        self.assertEqual(resolve_canonical_component("mqtt"), "mosquitto")
        self.assertEqual(resolve_canonical_component("mosquitto"), "mosquitto")
        self.assertEqual(resolve_canonical_component("home-api"), "home-api")
        self.assertEqual(resolve_canonical_component("backend"), "home-api")
        self.assertEqual(resolve_canonical_component("home-web"), "home-web")
        self.assertEqual(resolve_canonical_component("home-vision"), "home-vision")
        self.assertEqual(resolve_canonical_component("1Panel-openresty-WuDU"), "openresty")

        # Database lookup by canonical vs Docker name
        db_path = self._init_clean_db()
        conn = sqlite3.connect(db_path)
        with conn:
            conn.execute("INSERT INTO ops_circuit_breaker_states (container, state) VALUES ('frigate', 'OPEN')")
            conn.execute("INSERT INTO ops_circuit_breaker_states (container, state) VALUES ('openresty', 'OPEN')")
        conn.close()

        # Both raw Docker name and alias should find the OPEN state
        self.assertEqual(self.agent._is_circuit_open_offline("home-frigate", db_path), CircuitBreakerState.OPEN)
        self.assertEqual(self.agent._is_circuit_open_offline("nvr", db_path), CircuitBreakerState.OPEN)
        self.assertEqual(self.agent._is_circuit_open_offline("1Panel-openresty-WuDU", db_path), CircuitBreakerState.OPEN)

    # =========================================================================
    # 11. Docker Socket 不存在测试
    # =========================================================================
    async def test_scenario_11_docker_socket_not_found(self):
        """Scenario 11: Missing Docker socket returns DockerHealthState.UNKNOWN, NEVER True."""
        missing_sock = "/tmp/nonexistent_docker_daemon_99999.sock"
        health = await self.agent._verify_container_health("home-api", missing_sock)
        self.assertEqual(health, DockerHealthState.UNKNOWN)
        self.assertNotEqual(health, DockerHealthState.HEALTHY)
        self.assertIsNot(health, True)

    # =========================================================================
    # 12. Docker inspect HTTP 错误测试
    # =========================================================================
    async def test_scenario_12_docker_inspect_http_error(self):
        """Scenario 12: Docker socket returning HTTP 500/404 fails health verification."""
        # 12a: HTTP 500 -> UNKNOWN
        async def handler_500(reader, writer):
            await reader.read(512)
            writer.write(b"HTTP/1.1 500 Internal Server Error\r\nConnection: close\r\n\r\n{\"message\": \"daemon error\"}")
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        server = await asyncio.start_server(handler_500, "127.0.0.1", 0)
        port = server.sockets[0].getsockname()[1]
        try:
            health = await self.agent._verify_container_health("home-api", f"tcp://127.0.0.1:{port}")
            self.assertEqual(health, DockerHealthState.UNKNOWN)
        finally:
            server.close()
            await server.wait_closed()

        # 12b: HTTP 404 -> UNHEALTHY (container missing)
        async def handler_404(reader, writer):
            await reader.read(512)
            writer.write(b"HTTP/1.1 404 Not Found\r\nConnection: close\r\n\r\n{\"message\": \"No such container\"}")
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        server404 = await asyncio.start_server(handler_404, "127.0.0.1", 0)
        port404 = server404.sockets[0].getsockname()[1]
        try:
            health = await self.agent._verify_container_health("home-api", f"tcp://127.0.0.1:{port404}")
            self.assertEqual(health, DockerHealthState.UNHEALTHY)
        finally:
            server404.close()
            await server404.wait_closed()

    # =========================================================================
    # 13. Docker inspect JSON 损坏测试
    # =========================================================================
    async def test_scenario_13_docker_inspect_json_corrupted(self):
        """Scenario 13: Malformed or truncated inspect JSON returns UNKNOWN."""
        async def handler_corrupt(reader, writer):
            await reader.read(512)
            writer.write(b"HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n{\"State\": {\"Running\": true, \"Health\": {\"Sta")
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        server = await asyncio.start_server(handler_corrupt, "127.0.0.1", 0)
        port = server.sockets[0].getsockname()[1]
        try:
            health = await self.agent._verify_container_health("home-api", f"tcp://127.0.0.1:{port}")
            self.assertEqual(health, DockerHealthState.UNKNOWN)
        finally:
            server.close()
            await server.wait_closed()

    # =========================================================================
    # 14. Docker Running=false 测试
    # =========================================================================
    async def test_scenario_14_docker_running_false(self):
        """Scenario 14: Container with State.Running=False must evaluate to UNHEALTHY."""
        payload = json.dumps({
            "State": {
                "Running": False,
                "Health": {"Status": "healthy"}
            }
        }).encode("utf-8")

        async def handler_stopped(reader, writer):
            await reader.read(512)
            writer.write(b"HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n" + payload)
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        server = await asyncio.start_server(handler_stopped, "127.0.0.1", 0)
        port = server.sockets[0].getsockname()[1]
        try:
            health = await self.agent._verify_container_health("home-api", f"tcp://127.0.0.1:{port}")
            self.assertEqual(health, DockerHealthState.UNHEALTHY)
        finally:
            server.close()
            await server.wait_closed()

    # =========================================================================
    # 15. Docker HEALTHCHECK unhealthy 测试
    # =========================================================================
    async def test_scenario_15_docker_healthcheck_unhealthy(self):
        """Scenario 15: Container with Health.Status=unhealthy or starting evaluates to UNHEALTHY."""
        payload_unhealthy = json.dumps({
            "State": {
                "Running": True,
                "Health": {"Status": "unhealthy"}
            }
        }).encode("utf-8")

        async def handler_unhealthy(reader, writer):
            await reader.read(512)
            writer.write(b"HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n" + payload_unhealthy)
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        server = await asyncio.start_server(handler_unhealthy, "127.0.0.1", 0)
        port = server.sockets[0].getsockname()[1]
        try:
            health = await self.agent._verify_container_health("home-api", f"tcp://127.0.0.1:{port}")
            self.assertEqual(health, DockerHealthState.UNHEALTHY)
        finally:
            server.close()
            await server.wait_closed()

        # Check 'starting' state
        payload_starting = json.dumps({
            "State": {
                "Running": True,
                "Health": {"Status": "starting"}
            }
        }).encode("utf-8")

        async def handler_starting(reader, writer):
            await reader.read(512)
            writer.write(b"HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n" + payload_starting)
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        server_start = await asyncio.start_server(handler_starting, "127.0.0.1", 0)
        port_start = server_start.sockets[0].getsockname()[1]
        try:
            health = await self.agent._verify_container_health("home-api", f"tcp://127.0.0.1:{port_start}")
            self.assertEqual(health, DockerHealthState.UNHEALTHY)
        finally:
            server_start.close()
            await server_start.wait_closed()

    # =========================================================================
    # 16. 无 HEALTHCHECK 的独立服务就绪检查测试
    # =========================================================================
    async def test_scenario_16_unhealthchecked_independent_readiness_probe(self):
        """Scenario 16: Container without HEALTHCHECK triggers independent service readiness probe."""
        payload_no_healthcheck = json.dumps({
            "State": {
                "Running": True
                # No "Health" field
            }
        }).encode("utf-8")

        async def handler_inspect(reader, writer):
            await reader.read(512)
            writer.write(b"HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n" + payload_no_healthcheck)
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        server = await asyncio.start_server(handler_inspect, "127.0.0.1", 0)
        port = server.sockets[0].getsockname()[1]
        sock_uri = f"tcp://127.0.0.1:{port}"

        try:
            # 16a: When independent probe fails -> UNHEALTHY
            with patch.object(self.agent, "_check_service_readiness", new=AsyncMock(return_value=False)):
                health = await self.agent._verify_container_health("home-api", sock_uri)
                self.assertEqual(health, DockerHealthState.UNHEALTHY)

            # 16b: When independent probe succeeds -> HEALTHY
            with patch.object(self.agent, "_check_service_readiness", new=AsyncMock(return_value=True)):
                health = await self.agent._verify_container_health("home-api", sock_uri)
                self.assertEqual(health, DockerHealthState.HEALTHY)
        finally:
            server.close()
            await server.wait_closed()

        # 16c: Verify actual TCP / HTTP probe dispatch methods
        async def mock_http_server(reader, writer):
            await reader.read(512)
            writer.write(b"HTTP/1.1 200 OK\r\nConnection: close\r\n\r\nOK")
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        probe_srv = await asyncio.start_server(mock_http_server, "127.0.0.1", 0)
        probe_port = probe_srv.sockets[0].getsockname()[1]
        try:
            # Override host/port via env
            with patch.dict(os.environ, {"PROBE_HOST": "127.0.0.1"}):
                # Test probe against this active probe server
                probe_res = await self.agent._check_service_readiness("unknown-service")
                self.assertFalse(probe_res, "Unregistered service without probe specification must fail-closed")
        finally:
            probe_srv.close()
            await probe_srv.wait_closed()

    # =========================================================================
    # 17. 离线重启频率记录写入失败测试
    # =========================================================================
    async def test_scenario_17_record_persisted_restart_failure_halts_restart(self):
        """Scenario 17: Inability to persist restart rate limit timestamp aborts restart."""
        self._init_clean_db()
        self.agent._restart_docker_container = AsyncMock(return_value=True)

        with patch.object(self.agent, "_record_persisted_restart", return_value=False):
            res = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
            self.assertFalse(res, "Restart must be aborted when recording rate limit timestamp fails")
            self.agent._restart_docker_container.assert_not_called()

    # =========================================================================
    # 18. 维护锁过期时间格式异常测试
    # =========================================================================
    async def test_scenario_18_maintenance_lock_expires_at_format_variations(self):
        """Scenario 18: Handles various date formats and fails closed (UNKNOWN) on malformed timestamps."""
        db_path = self._init_clean_db()

        # 18a: Future RFC3339 with 'T' and 'Z' -> LOCKED
        conn = sqlite3.connect(db_path)
        with conn:
            conn.execute("DELETE FROM ops_maintenance_locks")
            conn.execute(
                "INSERT INTO ops_maintenance_locks (active, expires_at) VALUES (1, '2099-01-01T12:00:00Z')"
            )
        conn.close()
        self.assertEqual(self.agent._is_maintenance_locked_offline(db_path), MaintenanceLockState.LOCKED)

        # 18b: Future space-delimited ISO -> LOCKED
        conn = sqlite3.connect(db_path)
        with conn:
            conn.execute("DELETE FROM ops_maintenance_locks")
            conn.execute(
                "INSERT INTO ops_maintenance_locks (active, expires_at) VALUES (1, '2099-01-01 12:00:00')"
            )
        conn.close()
        self.assertEqual(self.agent._is_maintenance_locked_offline(db_path), MaintenanceLockState.LOCKED)

        # 18c: Past timestamp -> UNLOCKED
        conn = sqlite3.connect(db_path)
        with conn:
            conn.execute("DELETE FROM ops_maintenance_locks")
            conn.execute(
                "INSERT INTO ops_maintenance_locks (active, expires_at) VALUES (1, '2020-01-01T00:00:00Z')"
            )
        conn.close()
        self.assertEqual(self.agent._is_maintenance_locked_offline(db_path), MaintenanceLockState.UNLOCKED)

        # 18d: Active lock with empty expires_at (indefinite lock) -> LOCKED
        conn = sqlite3.connect(db_path)
        with conn:
            conn.execute("DELETE FROM ops_maintenance_locks")
            conn.execute(
                "INSERT INTO ops_maintenance_locks (active, expires_at) VALUES (1, '')"
            )
        conn.close()
        self.assertEqual(self.agent._is_maintenance_locked_offline(db_path), MaintenanceLockState.LOCKED)

        # 18e: Malformed unparseable expires_at -> UNKNOWN & halts restart
        conn = sqlite3.connect(db_path)
        with conn:
            conn.execute("DELETE FROM ops_maintenance_locks")
            conn.execute(
                "INSERT INTO ops_maintenance_locks (active, expires_at) VALUES (1, 'MALFORMED_GARBAGE_DATE_STRING')"
            )
        conn.close()
        self.assertEqual(self.agent._is_maintenance_locked_offline(db_path), MaintenanceLockState.UNKNOWN)

        self.agent._restart_docker_container = AsyncMock(return_value=True)
        res = await self.agent._handle_offline_emergency_restart("home-api", 1, self.mock_sock)
        self.assertFalse(res, "Malformed maintenance lock expires_at must evaluate to UNKNOWN and halt restart")
        self.agent._restart_docker_container.assert_not_called()

    # =========================================================================
    # 19. 双路径重复恢复互斥测试
    # =========================================================================
    async def test_scenario_19_dual_path_mutual_exclusion(self):
        """Scenario 19: Persistent recovery lease held by Go Core blocks Python offline emergency restart."""
        db_path = self._init_clean_db()

        # Insert active lease held by Go Core for 30s
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

        # When lease expires, Python offline agent can successfully acquire it
        conn = sqlite3.connect(db_path)
        with conn:
            conn.execute(
                "UPDATE ops_recovery_leases SET expires_at = ? WHERE component = 'frigate'",
                (past_iso,)
            )
        conn.close()

        acquired_now, token_now = self.agent._acquire_recovery_lease("home-frigate", db_path)
        self.assertTrue(acquired_now, "Expired lease can be acquired")
        self.assertTrue(token_now.startswith("py-"))

        # Release lease
        self.agent._release_recovery_lease("home-frigate", token_now, db_path)
        conn = sqlite3.connect(db_path)
        cursor = conn.cursor()
        cursor.execute("SELECT count(*) FROM ops_recovery_leases WHERE component = 'frigate' AND lease_token = ?", (token_now,))
        self.assertEqual(cursor.fetchone()[0], 0)
        conn.close()


if __name__ == "__main__":
    unittest.main()
