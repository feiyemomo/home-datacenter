"""
Acceptance Criteria M4: Dual-Path Persistent Lease & Protocol Hardening Test Suite.
Verifies:
1. Canonical Component ID Unification across Go Core and Python
2. Dual-Path Persistent Execution Lease (ops_recovery_leases) Mutual Exclusion and Expiration
3. Offline Emergency Recovery Fail-Closed when Lease is held by Go Core
4. Go/Python Protocol Key Casing Handling (Action/action, Reason/reason)
"""

import asyncio
from datetime import datetime, timezone
import io
import json
import os
import sqlite3
import sys
import tempfile
import time
import unittest
from unittest.mock import AsyncMock, MagicMock, patch
import urllib.request
import urllib.error

CURRENT_DIR = os.path.dirname(os.path.abspath(__file__))
PARENT_DIR = os.path.dirname(CURRENT_DIR)
sys.path.insert(0, PARENT_DIR)
sys.path.insert(0, CURRENT_DIR)

from config import OpsAgentConfig
from schema_helper import init_ops_v4_schema
from main import (
    CircuitBreakerState,
    CoreReportingStatus,
    DockerHealthState,
    HomeOpsAgent,
    MaintenanceLockState,
    resolve_canonical_component,
    resolve_docker_container_name,
)


class TestDualPathCanonicalIDs(unittest.TestCase):
    """Verifies that Python resolve_canonical_component and resolve_docker_container_name match Go Core rules."""

    def test_canonical_component_normalization(self):
        cases = [
            ("1Panel-openresty-WuDU", "openresty"),
            ("openresty-waf", "openresty"),
            ("home-gateway (Edge Reverse Proxy)", "openresty"),
            ("1panel-openresty-wudu", "openresty"),
            ("/home-api", "home-api"),
            ("home-frigate", "frigate"),
            ("go2rtc", "frigate"),
            ("frigate (NVR Service)", "frigate"),
            ("home-api (Backend)", "home-api"),
            ("home-web (Front End)", "home-web"),
            ("mosquitto-broker", "mosquitto"),
            ("home-datacenter-mosquitto", "mosquitto"),
            ("home-datacenter-frigate", "frigate"),
            ("home-datacenter-api", "home-api"),
            ("home-datacenter-web", "home-web"),
            ("home-datacenter-gateway", "openresty"),
            ("app.db", "app.db"),
            ("database", "app.db"),
            ("sqlite", "app.db"),
        ]
        for raw_input, expected_canonical in cases:
            with self.subTest(raw_input=raw_input):
                actual = resolve_canonical_component(raw_input)
                self.assertEqual(
                    actual,
                    expected_canonical,
                    f"Expected canonical ID '{expected_canonical}' for '{raw_input}', got '{actual}'",
                )

    def test_docker_container_resolution(self):
        cases = [
            ("openresty", "1Panel-openresty-WuDU"),
            ("1panel-openresty-wudu", "1Panel-openresty-WuDU"),
            ("frigate", "home-frigate"),
            ("home-frigate", "home-frigate"),
            ("mosquitto", "home-mosquitto"),
            ("home-mosquitto", "home-mosquitto"),
            ("home-api", "home-api"),
            ("home-web", "home-web"),
            ("home-vision", "home-vision"),
        ]
        for raw_input, expected_docker in cases:
            with self.subTest(raw_input=raw_input):
                actual = resolve_docker_container_name(raw_input)
                self.assertEqual(
                    actual,
                    expected_docker,
                    f"Expected Docker container '{expected_docker}' for '{raw_input}', got '{actual}'",
                )


class TestDualPathLeaseMutualExclusion(unittest.TestCase):
    """Verifies that Go Core and Python Ops Agent enforce mutual exclusion via ops_recovery_leases."""

    def setUp(self):
        self.tmp_dir = tempfile.TemporaryDirectory()
        self.db_path = os.path.join(self.tmp_dir.name, "app.db")
        self._init_db(self.db_path)

        self.config = OpsAgentConfig(
            api_base_url="http://127.0.0.1:8080",
            internal_token="test-internal-token-32-chars-long",
            db_path=self.db_path,
        )
        self.agent = HomeOpsAgent(self.config)

    def tearDown(self):
        try:
            self.tmp_dir.cleanup()
        except Exception:
            pass

    def _init_db(self, db_path: str):
        init_ops_v4_schema(db_path)

    def test_python_fails_to_acquire_when_go_core_holds_active_lease(self):
        """When Go Core holds an active lease, Python must be blocked from acquiring."""
        now = time.time()
        now_iso = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now))
        future_iso = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now + 60.0))

        conn = sqlite3.connect(self.db_path)
        with conn:
            conn.execute(
                "INSERT INTO ops_recovery_leases (component, holder, lease_token, acquired_at, expires_at) "
                "VALUES ('home-api', 'go-core', 'go-lease-token-1234', ?, ?)",
                (now_iso, future_iso)
            )
        conn.close()

        acquired, token = self.agent._acquire_recovery_lease("home-api", self.db_path)
        self.assertFalse(acquired, "Python should not acquire lease when held by go-core")
        self.assertEqual(token, "")

        # Alias check: home-datacenter-api resolves to home-api
        acquired_alias, token_alias = self.agent._acquire_recovery_lease("home-datacenter-api", self.db_path)
        self.assertFalse(acquired_alias, "Python should not acquire lease for aliased component held by go-core")
        self.assertEqual(token_alias, "")

    def test_python_acquires_lease_when_go_core_lease_expired(self):
        """When Go Core lease has expired, Python can take over the lease atomically."""
        now = time.time()
        past_iso = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now - 10.0))
        very_past_iso = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now - 40.0))

        conn = sqlite3.connect(self.db_path)
        with conn:
            conn.execute(
                "INSERT INTO ops_recovery_leases (component, holder, lease_token, acquired_at, expires_at) "
                "VALUES ('home-web', 'go-core', 'expired-go-token', ?, ?)",
                (very_past_iso, past_iso)
            )
        conn.close()

        acquired, token = self.agent._acquire_recovery_lease("home-web", self.db_path)
        self.assertTrue(acquired, "Python should acquire lease when previous lease expired")
        self.assertTrue(token.startswith("py-"), f"Expected token starting with 'py-', got '{token}'")

        # Verify DB state
        conn = sqlite3.connect(self.db_path)
        cursor = conn.cursor()
        cursor.execute("SELECT holder, lease_token FROM ops_recovery_leases WHERE component = 'home-web'")
        row = cursor.fetchone()
        conn.close()

        self.assertIsNotNone(row)
        self.assertEqual(row[0], "python-offline")
        self.assertEqual(row[1], token)

    def test_lease_release_with_token(self):
        """Releasing with invalid token fails; releasing with correct token succeeds."""
        acquired, token = self.agent._acquire_recovery_lease("mosquitto", self.db_path)
        self.assertTrue(acquired)

        # Attempt release with wrong token
        rel_fail = self.agent._release_recovery_lease("mosquitto", "invalid-token", self.db_path)
        self.assertFalse(rel_fail, "Release with wrong token must fail")

        # Attempt release with valid token
        rel_ok = self.agent._release_recovery_lease("mosquitto", token, self.db_path)
        self.assertTrue(rel_ok, "Release with matching token must succeed")

        # Verify lease is removed
        conn = sqlite3.connect(self.db_path)
        cursor = conn.cursor()
        cursor.execute("SELECT * FROM ops_recovery_leases WHERE component = 'mosquitto'")
        row = cursor.fetchone()
        conn.close()
        self.assertIsNone(row, "Lease row should be deleted upon clean release")


class TestOfflineEmergencyWithLease(unittest.IsolatedAsyncioTestCase):
    """Verifies that offline emergency restart aborts immediately if recovery lease cannot be acquired."""

    def setUp(self):
        self.tmp_dir = tempfile.TemporaryDirectory()
        self.db_path = os.path.join(self.tmp_dir.name, "app.db")
        self.token = "valid-emergency-override-token-32-chars"
        self.env_patcher = patch.dict(
            os.environ,
            {
                "ENVIRONMENT": "test",
                "APP_ENV": "test",
                "EMERGENCY_OVERRIDE_TOKEN": self.token,
            },
        )
        self.env_patcher.start()

        self._init_healthy_db(self.db_path)

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

    def _init_healthy_db(self, db_path: str):
        init_ops_v4_schema(db_path)
        conn = sqlite3.connect(db_path)
        with conn:
            conn.execute(
                "INSERT INTO ops_circuit_breaker_states (container, state) VALUES ('home-web', 'CLOSED')"
            )
        conn.close()

    async def test_offline_emergency_restart_aborts_when_lease_held(self):
        """If Go Core holds the lease, offline emergency restart must fail-closed without restarting container."""
        now = time.time()
        now_iso = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now))
        future_iso = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now + 60.0))

        conn = sqlite3.connect(self.db_path)
        with conn:
            conn.execute(
                "INSERT INTO ops_recovery_leases (component, holder, lease_token, acquired_at, expires_at) "
                "VALUES ('home-web', 'go-core', 'go-active-lease', ?, ?)",
                (now_iso, future_iso)
            )
        conn.close()

        with patch.object(self.agent, "_restart_docker_container", new_callable=AsyncMock) as mock_restart:
            success = await self.agent._handle_offline_emergency_restart(
                "home-web", 1, "/tmp/fake.sock"
            )
            self.assertFalse(success, "Emergency restart must fail if lease is held by another path")
            mock_restart.assert_not_called()

    async def test_offline_emergency_restart_succeeds_and_releases_lease(self):
        """When lease is available, emergency restart acquires lease, runs restart, and releases lease in finally."""
        with patch.object(self.agent, "_restart_docker_container", new_callable=AsyncMock) as mock_restart, \
             patch.object(self.agent, "_verify_container_health", new_callable=AsyncMock) as mock_health:
            mock_restart.return_value = True
            mock_health.return_value = DockerHealthState.HEALTHY

            success = await self.agent._handle_offline_emergency_restart(
                "home-web", 1, "/tmp/fake.sock"
            )
            self.assertTrue(success)
            mock_restart.assert_called_once()
            mock_health.assert_called_once()

            # Lease should be cleanly released after completion
            conn = sqlite3.connect(self.db_path)
            cursor = conn.cursor()
            cursor.execute("SELECT * FROM ops_recovery_leases WHERE component = 'home-web'")
            row = cursor.fetchone()
            conn.close()
            self.assertIsNone(row, "Lease must be released in finally block")


class TestGoPythonProtocolKeyCasing(unittest.IsolatedAsyncioTestCase):
    """Verifies that _report_event_to_go_core accepts both camel/upper and lower case keys."""

    def setUp(self):
        self.config = OpsAgentConfig(
            api_base_url="http://127.0.0.1:8080",
            internal_token="test-token-32-chars-long",
        )
        self.agent = HomeOpsAgent(self.config)

    def _make_mock_response(self, json_dict: dict):
        body_bytes = json.dumps(json_dict).encode("utf-8")
        mock_resp = MagicMock()
        mock_resp.read.return_value = body_bytes
        mock_resp.__enter__.return_value = mock_resp
        mock_resp.__exit__.return_value = None
        return mock_resp

    async def test_action_uppercase_blocked(self):
        """Go Core returns {"decision": {"Action": "BLOCKED", "Reason": "circuit open"}}"""
        mock_resp = self._make_mock_response({
            "decision": {"Action": "BLOCKED", "Reason": "circuit breaker OPEN"}
        })
        with patch("urllib.request.urlopen", return_value=mock_resp):
            status, data = await self.agent._report_event_to_go_core(
                "home-web", 1, "die", "crash"
            )
            self.assertEqual(status, CoreReportingStatus.BLOCKED_BY_FSM)

    async def test_action_lowercase_blocked(self):
        """Go Core returns {"decision": {"action": "BLOCKED", "reason": "rate limit"}}"""
        mock_resp = self._make_mock_response({
            "decision": {"action": "BLOCKED", "reason": "rate limit"}
        })
        with patch("urllib.request.urlopen", return_value=mock_resp):
            status, data = await self.agent._report_event_to_go_core(
                "home-web", 1, "die", "crash"
            )
            self.assertEqual(status, CoreReportingStatus.BLOCKED_BY_FSM)

    async def test_action_tripped(self):
        """Go Core returns {"decision": {"action": "TRIPPED"}}"""
        mock_resp = self._make_mock_response({
            "decision": {"action": "TRIPPED", "reason": "5 crashes in 60s"}
        })
        with patch("urllib.request.urlopen", return_value=mock_resp):
            status, data = await self.agent._report_event_to_go_core(
                "home-web", 1, "die", "crash"
            )
            self.assertEqual(status, CoreReportingStatus.BLOCKED_BY_FSM)

    async def test_action_restart_unexecuted(self):
        """Go Core returns {"decision": {"action": "RESTART"}, "executed": false}"""
        mock_resp = self._make_mock_response({
            "decision": {"action": "RESTART", "reason": "healthy"},
            "executed": False,
        })
        with patch("urllib.request.urlopen", return_value=mock_resp):
            status, data = await self.agent._report_event_to_go_core(
                "home-web", 1, "die", "crash"
            )
            self.assertEqual(status, CoreReportingStatus.AUTHORIZED)

    async def test_action_restart_executed_by_core(self):
        """Go Core returns {"status": "RECOVERED"} or {"decision": {"Action": "RESTART"}, "executed": true}"""
        mock_resp = self._make_mock_response({
            "status": "RECOVERED",
            "decision": {"Action": "RESTART"},
            "executed": True,
        })
        with patch("urllib.request.urlopen", return_value=mock_resp):
            status, data = await self.agent._report_event_to_go_core(
                "home-web", 1, "die", "crash"
            )
            self.assertEqual(status, CoreReportingStatus.RECOVERED_BY_CORE)


if __name__ == "__main__":
    unittest.main()
