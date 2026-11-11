"""
Comprehensive Unit Tests for R2: Edge Agent Execution Boundaries & Communication Hardening.
Verifies HTTP status code classification, suppression of local restart on policy rejection/FSM blocks,
5-guard offline emergency restart, and offline SQLite audit log synchronization.
"""

import asyncio
import io
import json
import os
import sqlite3
import sys
import tempfile
import unittest
import urllib.error
from unittest.mock import AsyncMock, MagicMock, patch

CURRENT_DIR = os.path.dirname(os.path.abspath(__file__))
PARENT_DIR = os.path.dirname(CURRENT_DIR)
sys.path.insert(0, PARENT_DIR)
sys.path.insert(0, CURRENT_DIR)

from config import OpsAgentConfig
from schema_helper import init_ops_v4_schema
from main import (
    CoreReportingStatus,
    DockerHealthState,
    EMERGENCY_RESTART_WHITELIST,
    WEAK_SECRETS_BLACKLIST,
    HomeOpsAgent,
)


class TestAgentBoundaries(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.tmp_dir = tempfile.TemporaryDirectory()
        self.db_path = os.path.join(self.tmp_dir.name, "test_app.db")
        init_ops_v4_schema(self.db_path)
        self.config = OpsAgentConfig(
            api_base_url="http://127.0.0.1:8080",
            internal_token="test-internal-token-32-chars-long",
            db_path=self.db_path,
        )
        self.agent = HomeOpsAgent(self.config)

    def tearDown(self):
        import gc
        gc.collect()
        try:
            self.tmp_dir.cleanup()
        except Exception:
            pass

    # -------------------------------------------------------------------------
    # 1. Config Hardening: No JWT_SECRET fallback
    # -------------------------------------------------------------------------
    def test_internal_token_does_not_fallback_to_jwt_secret(self):
        old_internal = os.environ.get("INTERNAL_OPS_TOKEN")
        old_jwt = os.environ.get("JWT_SECRET")
        try:
            os.environ.pop("INTERNAL_OPS_TOKEN", None)
            os.environ["JWT_SECRET"] = "some-long-jwt-secret-that-must-not-leak"
            cfg = OpsAgentConfig.load()
            self.assertEqual(
                cfg.internal_token,
                "",
                "INTERNAL_OPS_TOKEN must NOT fallback to JWT_SECRET",
            )
        finally:
            if old_internal is not None:
                os.environ["INTERNAL_OPS_TOKEN"] = old_internal
            else:
                os.environ.pop("INTERNAL_OPS_TOKEN", None)
            if old_jwt is not None:
                os.environ["JWT_SECRET"] = old_jwt
            else:
                os.environ.pop("JWT_SECRET", None)

    # -------------------------------------------------------------------------
    # 2. HTTP Status Code Classification in _report_event_to_go_core
    # -------------------------------------------------------------------------
    async def test_report_event_authorized_when_restart(self):
        mock_resp = MagicMock()
        mock_resp.status = 200
        mock_resp.read.return_value = json.dumps({
            "decision": {"Action": "RESTART", "Reason": "Healthy crash recovery approved"}
        }).encode("utf-8")

        with patch("urllib.request.urlopen") as mock_urlopen:
            mock_urlopen.return_value.__enter__.return_value = mock_resp
            status, data = await self.agent._report_event_to_go_core(
                "home-api", 1, "die", "Error test"
            )
            self.assertEqual(status, CoreReportingStatus.AUTHORIZED)
            self.assertEqual(data["decision"]["Action"], "RESTART")

    async def test_report_event_blocked_by_fsm(self):
        for action in ("BLOCKED", "IGNORE", "TRIPPED"):
            mock_resp = MagicMock()
            mock_resp.status = 200
            mock_resp.read.return_value = json.dumps({
                "decision": {"Action": action, "Reason": f"Maintenance active: {action}"}
            }).encode("utf-8")

            with patch("urllib.request.urlopen") as mock_urlopen:
                mock_urlopen.return_value.__enter__.return_value = mock_resp
                status, data = await self.agent._report_event_to_go_core(
                    "home-api", 1, "die", "Error test"
                )
                self.assertEqual(
                    status,
                    CoreReportingStatus.BLOCKED_BY_FSM,
                    f"Action {action} must be classified as BLOCKED_BY_FSM",
                )

    async def test_report_event_policy_rejected_on_4xx(self):
        for code in (400, 401, 403, 409, 422, 429):
            err_file = io.BytesIO(b'{"error":"Policy violation / Unauthorized"}')
            http_err = urllib.error.HTTPError(
                url="http://127.0.0.1:8080/api/v1/ops/fsm/event",
                code=code,
                msg="Client Error",
                hdrs={},
                fp=err_file,
            )
            with patch("urllib.request.urlopen", side_effect=http_err):
                status, data = await self.agent._report_event_to_go_core(
                    "home-api", 1, "die", "Error test"
                )
                self.assertEqual(
                    status,
                    CoreReportingStatus.POLICY_REJECTED,
                    f"HTTP {code} must be classified as POLICY_REJECTED",
                )
                self.assertEqual(data["code"], code)

    async def test_report_event_server_error_on_5xx(self):
        for code in (500, 502, 503):
            err_file = io.BytesIO(b'{"error":"Internal server error"}')
            http_err = urllib.error.HTTPError(
                url="http://127.0.0.1:8080/api/v1/ops/fsm/event",
                code=code,
                msg="Server Error",
                hdrs={},
                fp=err_file,
            )
            with patch("urllib.request.urlopen", side_effect=http_err):
                status, data = await self.agent._report_event_to_go_core(
                    "home-api", 1, "die", "Error test"
                )
                self.assertEqual(
                    status,
                    CoreReportingStatus.SERVER_ERROR,
                    f"HTTP {code} must be classified as SERVER_ERROR",
                )

    async def test_report_event_offline_on_network_errors(self):
        for exc in (
            urllib.error.URLError("Connection refused"),
            TimeoutError("timed out"),
            ConnectionRefusedError("connection refused"),
            OSError("network unreachable"),
        ):
            with patch("urllib.request.urlopen", side_effect=exc):
                status, data = await self.agent._report_event_to_go_core(
                    "home-api", 1, "die", "Error test"
                )
                self.assertEqual(
                    status,
                    CoreReportingStatus.OFFLINE,
                    f"Exception {exc} must be classified as OFFLINE",
                )

    # -------------------------------------------------------------------------
    # 3. Decision Engine: Strictly Forbid Local Fallback on Policy Rejection / FSM Blocks
    # -------------------------------------------------------------------------
    async def test_processor_strictly_forbids_local_restart_on_policy_rejection(self):
        """When Go Core returns HTTP 403 / 401 / 400, local restart must NEVER be invoked!"""
        self.agent._report_event_to_go_core = AsyncMock(
            return_value=(CoreReportingStatus.POLICY_REJECTED, {"code": 403})
        )
        self.agent._restart_docker_container = AsyncMock(return_value=True)
        self.agent._handle_offline_emergency_restart = AsyncMock(return_value=True)

        event = {
            "Action": "die",
            "Actor": {"Attributes": {"name": "home-api", "exitCode": "1"}},
        }
        await self.agent._docker_event_queue.put(event)

        # Run processor for one iteration
        self.agent.running = True
        proc_task = asyncio.create_task(self.agent._docker_event_processor())
        await asyncio.sleep(0.05)
        self.agent.running = False
        proc_task.cancel()
        try:
            await proc_task
        except asyncio.CancelledError:
            pass

        self.agent._restart_docker_container.assert_not_called()
        self.agent._handle_offline_emergency_restart.assert_not_called()

    async def test_processor_strictly_forbids_local_restart_on_fsm_block(self):
        """When Go Core returns Action=BLOCKED, local restart must NEVER be invoked!"""
        self.agent._report_event_to_go_core = AsyncMock(
            return_value=(CoreReportingStatus.BLOCKED_BY_FSM, {"decision": {"Action": "BLOCKED"}})
        )
        self.agent._restart_docker_container = AsyncMock(return_value=True)
        self.agent._handle_offline_emergency_restart = AsyncMock(return_value=True)

        event = {
            "Action": "die",
            "Actor": {"Attributes": {"name": "home-api", "exitCode": "1"}},
        }
        await self.agent._docker_event_queue.put(event)

        self.agent.running = True
        proc_task = asyncio.create_task(self.agent._docker_event_processor())
        await asyncio.sleep(0.05)
        self.agent.running = False
        proc_task.cancel()
        try:
            await proc_task
        except asyncio.CancelledError:
            pass

        self.agent._restart_docker_container.assert_not_called()
        self.agent._handle_offline_emergency_restart.assert_not_called()

    # -------------------------------------------------------------------------
    # 4. Controlled 5-Guard Offline Emergency Mode
    # -------------------------------------------------------------------------
    async def test_offline_guard_1_fails_when_override_token_missing_or_weak(self):
        """Guard 1: Missing or weak EMERGENCY_OVERRIDE_TOKEN suppresses restart."""
        self.agent._restart_docker_container = AsyncMock(return_value=True)

        for weak_token in ("", "123456", "admin", "short", "password", "feiyemomo"):
            with patch.dict(os.environ, {"EMERGENCY_OVERRIDE_TOKEN": weak_token}):
                res = await self.agent._handle_offline_emergency_restart(
                    "home-api", 1, "/var/run/docker.sock"
                )
                self.assertFalse(res)
                self.agent._restart_docker_container.assert_not_called()

    async def test_offline_guard_2_fails_when_container_not_whitelisted(self):
        """Guard 2: Container outside whitelist is rejected."""
        self.agent._restart_docker_container = AsyncMock(return_value=True)
        with patch.dict(os.environ, {"EMERGENCY_OVERRIDE_TOKEN": "valid-long-emergency-token-1234"}):
            res = await self.agent._handle_offline_emergency_restart(
                "malicious-or-unknown-container", 1, "/var/run/docker.sock"
            )
            self.assertFalse(res)
            self.agent._restart_docker_container.assert_not_called()

    async def test_offline_guard_4_circuit_breaker_trips(self):
        """Guard 4: Cooldown & circuit breaker (>2 restarts in 60s or >=3 in 10m)."""
        self.agent._restart_docker_container = AsyncMock(return_value=True)
        self.agent._verify_container_health = AsyncMock(return_value=DockerHealthState.HEALTHY)
        token = "valid-long-emergency-token-1234"

        with patch.dict(os.environ, {"EMERGENCY_OVERRIDE_TOKEN": token}):
            # Attempt 1 -> OK
            res1 = await self.agent._handle_offline_emergency_restart("home-api", 1, "/var/run/docker.sock")
            self.assertTrue(res1)

            # Attempt 2 -> OK
            res2 = await self.agent._handle_offline_emergency_restart("home-api", 1, "/var/run/docker.sock")
            self.assertTrue(res2)

            # Attempt 3 within 60s -> Tripped!
            res3 = await self.agent._handle_offline_emergency_restart("home-api", 1, "/var/run/docker.sock")
            self.assertFalse(res3, "Third restart within 60s must trip circuit breaker")
            self.assertEqual(self.agent._restart_docker_container.call_count, 2)

    async def test_offline_guard_5_sqlite_audit_persistence(self):
        """Guard 5: Successful offline restart creates an offline_ops_audit row in SQLite."""
        self.agent._restart_docker_container = AsyncMock(return_value=True)
        self.agent._verify_container_health = AsyncMock(return_value=DockerHealthState.HEALTHY)
        token = "valid-long-emergency-token-1234"

        with patch.dict(os.environ, {"EMERGENCY_OVERRIDE_TOKEN": token}):
            res = await self.agent._handle_offline_emergency_restart("home-vision", 1, "/var/run/docker.sock")
            self.assertTrue(res)

            conn = sqlite3.connect(self.db_path)
            conn.row_factory = sqlite3.Row
            cursor = conn.cursor()
            cursor.execute("SELECT * FROM offline_ops_audit WHERE container_name = 'home-vision' ORDER BY id ASC")
            rows = cursor.fetchall()
            self.assertGreaterEqual(len(rows), 1)
            if "event_type" in rows[0].keys() and len(rows) > 1:
                self.assertEqual(
                    [r["event_type"] for r in rows],
                    [
                        "EVENT_RECEIVED",
                        "RECOVERY_LEASE_ACQUIRED",
                        "EXECUTION_STARTED",
                        "HEALTH_CHECK_COMPLETED",
                        "EXECUTION_COMPLETED",
                    ],
                )
            row = rows[-1]
            self.assertTrue(row["event_uuid"].startswith("offline-"))
            self.assertEqual(row["action"], "emergency_restart")
            self.assertEqual(row["success"], 1)
            self.assertEqual(row["synced"], 0)
            conn.close()

    # -------------------------------------------------------------------------
    # 5. Offline Audit Log Synchronization
    # -------------------------------------------------------------------------
    async def test_sync_offline_audit_logs_pushes_and_marks_synced(self):
        """When Go Core recovers, buffered offline events are sent to /audit/offline-sync and marked synced."""
        # Prepopulate SQLite with 2 offline events
        self.agent._record_offline_audit("home-api", "emergency_restart", 137, True)
        self.agent._record_offline_audit("home-web", "emergency_restart", 1, True)

        conn = sqlite3.connect(self.db_path)
        cursor = conn.cursor()
        cursor.execute("SELECT count(*) FROM offline_ops_audit WHERE synced = 0")
        self.assertEqual(cursor.fetchone()[0], 2)
        conn.close()

        # Mock /health -> 200 OK
        # Mock /audit/offline-sync -> 200 OK
        mock_health_resp = MagicMock()
        mock_health_resp.status = 200
        mock_health_resp.__enter__.return_value = mock_health_resp

        mock_sync_resp = MagicMock()
        mock_sync_resp.status = 200
        mock_sync_resp.__enter__.return_value = mock_sync_resp

        def fake_urlopen(req, timeout=None):
            url = req.full_url if hasattr(req, "full_url") else req
            if "/health" in str(url):
                return mock_health_resp
            if "/audit/offline-sync" in str(url):
                # Verify that X-Internal-Secret was attached
                hdr = req.get_header("X-internal-secret") or req.headers.get("X-Internal-Secret")
                self.assertEqual(hdr, self.config.internal_token)
                return mock_sync_resp
            raise ValueError(f"Unexpected URL: {url}")

        with patch("urllib.request.urlopen", side_effect=fake_urlopen):
            synced_count = await self.agent.sync_offline_audit_logs()
            self.assertEqual(synced_count, 2)

        # Verify synced flag in DB
        conn = sqlite3.connect(self.db_path)
        cursor = conn.cursor()
        cursor.execute("SELECT count(*) FROM offline_ops_audit WHERE synced = 1")
        self.assertEqual(cursor.fetchone()[0], 2)
        conn.close()


if __name__ == "__main__":
    unittest.main()
