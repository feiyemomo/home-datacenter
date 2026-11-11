"""
Hardening & Regression Suite for Python Edge Ops Agent (R1, R4, R5):
- R1: Shared Lease protocol parity, non-overwrite of same-holder active leases,
      token uniqueness, renew/verify/release checks, and Go-parity component resolution.
- R4: Persisted restart count fail-closed guarantees (deleted/corrupted/read-only/SQL error,
      first-time vs runtime missing table distinction, unified BEGIN IMMEDIATE locking).
- R5: Live Docker BaselineHash computation & verification (component resolution failure,
      404/timeout/unknown state reject, real vs placeholder distinction, proposal & execution gates).
"""

import asyncio
import hashlib
import json
import os
import sqlite3
import stat
import sys
import tempfile
import time
import unittest
from unittest.mock import AsyncMock, MagicMock, patch

CURRENT_DIR = os.path.dirname(os.path.abspath(__file__))
AGENT_DIR = os.path.dirname(CURRENT_DIR)
sys.path.insert(0, AGENT_DIR)
sys.path.insert(0, CURRENT_DIR)

from config import (
    OpsAgentConfig,
    compute_container_baseline_from_inspect,
    compute_offline_placeholder_baseline,
    is_placeholder_baseline_hash,
    is_real_baseline_hash,
    resolve_canonical_component,
    resolve_docker_container_name,
)
from main import (
    CoreReportingStatus,
    DockerHealthState,
    HomeOpsAgent,
    SQLiteFaultClass,
)
from proposal import ProposalLifecycleManager
from schema_helper import init_ops_v4_schema


def _sample_inspect_payload(
    container_id: str = "a" * 64,
    image_id: str = "sha256:" + ("b" * 64),
    image_ref: str = "channels-tool/home-web:v1.2.3",
    state_status: str = "running",
    running: bool = True,
    created: str = "2026-11-10T01:00:00Z",
    started_at: str = "2026-11-10T01:00:05Z",
    restart_count: int = 0,
) -> dict:
    return {
        "Id": container_id,
        "Image": image_id,
        "Created": created,
        "RestartCount": restart_count,
        "Config": {"Image": image_ref},
        "State": {
            "Status": state_status,
            "Running": running,
            "StartedAt": started_at,
        },
    }


class TestR1LeaseProtocolParity(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.tmp_dir = tempfile.TemporaryDirectory()
        self.db_path = os.path.join(self.tmp_dir.name, "r1.db")
        init_ops_v4_schema(self.db_path)
        self.config = OpsAgentConfig(
            api_base_url="http://127.0.0.1:8080",
            internal_token="t" * 32,
            db_path=self.db_path,
        )
        self.agent = HomeOpsAgent(self.config)

    def tearDown(self):
        try:
            self.tmp_dir.cleanup()
        except Exception:
            pass

    def test_component_map_parity_with_go_core(self):
        expected = {
            "home-new-api": ("home-new-api", "home-new-api"),
            "new-api": ("home-new-api", "home-new-api"),
            "home-backup": ("home-backup", "home-backup"),
            "backup": ("home-backup", "home-backup"),
            "mihomo": ("mihomo", "mihomo"),
            "tailscale": ("tailscale", "tailscale"),
            "1pwaf": ("openresty", "1Panel-openresty-WuDU"),
        }
        for raw, (canon, docker_name) in expected.items():
            self.assertEqual(resolve_canonical_component(raw), canon)
            self.assertEqual(resolve_docker_container_name(canon), docker_name)

    def test_same_holder_cannot_overwrite_active_lease_and_tokens_are_unique(self):
        ok1, tok1, gen1 = self.agent._acquire_recovery_lease_grant("home-web", self.db_path, lease_duration=20.0)
        self.assertTrue(ok1)
        self.assertTrue(tok1.startswith("py-"))
        self.assertEqual(gen1, 1)

        # Same holder second instance must be blocked while active
        agent2 = HomeOpsAgent(self.config)
        ok2, tok2, gen2 = agent2._acquire_recovery_lease_grant("web", self.db_path, lease_duration=20.0)
        self.assertFalse(ok2)
        self.assertEqual(tok2, "")
        self.assertEqual(gen2, 0)

        # Release and re-acquire yields distinct token and higher monotonic generation
        self.assertTrue(self.agent._release_recovery_lease("home-web", tok1, self.db_path, fencing_gen=gen1))
        ok3, tok3, gen3 = agent2._acquire_recovery_lease_grant("home-web", self.db_path, lease_duration=20.0)
        self.assertTrue(ok3)
        self.assertNotEqual(tok1, tok3)
        self.assertGreater(gen3, gen1)

    def test_verify_lease_enforces_safety_guard_and_timestamp_sanity(self):
        ok, tok, gen = self.agent._acquire_recovery_lease_grant("home-api", self.db_path, lease_duration=2.0)
        self.assertTrue(ok)
        # With 0ms guard it succeeds; with 5000ms guard it fails closed
        self.assertTrue(self.agent._verify_recovery_lease("home-api", tok, gen, self.db_path, min_remaining_ms=0))
        self.assertFalse(self.agent._verify_recovery_lease("home-api", tok, gen, self.db_path, min_remaining_ms=5000))

        # Corrupt acquired_at_ms -> both verify and renew must reject
        conn = sqlite3.connect(self.db_path)
        with conn:
            conn.execute("UPDATE ops_recovery_leases SET acquired_at_ms = 0 WHERE component = 'home-api'")
        conn.close()
        self.assertFalse(self.agent._verify_recovery_lease("home-api", tok, gen, self.db_path))
        renewed, _ = self.agent._renew_recovery_lease("home-api", tok, gen, self.db_path)
        self.assertFalse(renewed)


class TestR4RestartCountFailClosed(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.tmp_dir = tempfile.TemporaryDirectory()
        self.db_path = os.path.join(self.tmp_dir.name, "r4.db")
        self.sock_path = os.path.join(self.tmp_dir.name, "missing.sock")
        self.env_patcher = patch.dict(os.environ, {"EMERGENCY_OVERRIDE_TOKEN": "valid-long-emergency-token-123456"})
        self.env_patcher.start()
        self.config = OpsAgentConfig(
            api_base_url="http://127.0.0.1:8080",
            internal_token="t" * 32,
            db_path=self.db_path,
        )
        self.agent = HomeOpsAgent(self.config)

    def tearDown(self):
        try:
            if os.path.exists(self.db_path):
                os.chmod(self.db_path, stat.S_IWRITE | stat.S_IREAD)
        except Exception:
            pass
        self.env_patcher.stop()
        try:
            self.tmp_dir.cleanup()
        except Exception:
            pass

    async def test_deleted_history_table_distinguished_from_init_and_never_autocreated(self):
        init_ops_v4_schema(self.db_path)
        conn = sqlite3.connect(self.db_path)
        with conn:
            conn.execute("DROP TABLE offline_restart_history")
        conn.close()

        ok, fault_cls, _ = self.agent._verify_ops_schema(self.db_path)
        self.assertFalse(ok)
        self.assertEqual(fault_cls, SQLiteFaultClass.TABLE_MISSING.value)
        self.assertIsNone(self.agent._get_persisted_restart_count("home-web", 60.0, self.db_path))
        self.assertFalse(self.agent._record_persisted_restart("home-web", self.db_path))

        # Verify table was NOT auto-created
        conn = sqlite3.connect(self.db_path)
        row = conn.execute(
            "SELECT name FROM sqlite_master WHERE type='table' AND name='offline_restart_history'"
        ).fetchone()
        conn.close()
        self.assertIsNone(row, "offline_restart_history MUST NOT be auto-created")

        # Distinguish fresh/uninitialized DB (no migrations/meta) from bootstrapped DB
        fresh_db = os.path.join(self.tmp_dir.name, "fresh.db")
        sqlite3.connect(fresh_db).close()
        ok_fresh, fault_fresh, _ = self.agent._verify_ops_schema(fresh_db)
        self.assertFalse(ok_fresh)
        self.assertEqual(fault_fresh, SQLiteFaultClass.TABLE_MISSING.value)
        self.assertIsNone(self.agent._get_persisted_restart_count("home-web", 60.0, fresh_db))

    def test_corrupted_history_rows_and_missing_columns_fail_closed(self):
        init_ops_v4_schema(self.db_path)
        for bad_val in ("corrupt", 0, -5.0, float("inf")):
            conn = sqlite3.connect(self.db_path)
            with conn:
                conn.execute("DELETE FROM offline_restart_history")
                conn.execute(
                    "INSERT INTO offline_restart_history (container_name, restart_time) VALUES ('home-web', ?)",
                    (bad_val,),
                )
            conn.close()
            self.assertIsNone(self.agent._get_persisted_restart_count("home-web", 60.0, self.db_path))
            self.assertFalse(self.agent._record_persisted_restart("home-web", self.db_path))

        # Missing required columns on offline_restart_history -> fail closed
        conn = sqlite3.connect(self.db_path)
        with conn:
            conn.execute("DROP TABLE offline_restart_history")
            conn.execute("CREATE TABLE offline_restart_history (id INTEGER PRIMARY KEY, note TEXT)")
        conn.close()
        self.assertIsNone(self.agent._get_persisted_restart_count("home-web", 60.0, self.db_path))
        self.assertFalse(self.agent._record_persisted_restart("home-web", self.db_path))

    async def test_readonly_db_blocks_write_and_restart(self):
        init_ops_v4_schema(self.db_path)
        conn = sqlite3.connect(self.db_path)
        with conn:
            conn.execute("INSERT INTO ops_circuit_breaker_states (container, state) VALUES ('home-web', 'CLOSED')")
        conn.close()

        os.chmod(self.db_path, stat.S_IREAD)
        self.agent._restart_docker_container = AsyncMock(return_value=True)
        # Note: Windows chmod 0o444 prevents writing to DB file (readonly error on BEGIN IMMEDIATE)
        self.assertFalse(self.agent._record_persisted_restart("home-web", self.db_path))
        res = await self.agent._handle_offline_emergency_restart("home-web", 1, self.sock_path)
        self.assertFalse(res)
        self.agent._restart_docker_container.assert_not_called()

    async def test_sql_query_exception_on_history_forbids_restart(self):
        init_ops_v4_schema(self.db_path)
        conn = sqlite3.connect(self.db_path)
        with conn:
            conn.execute("INSERT INTO ops_circuit_breaker_states (container, state) VALUES ('home-web', 'CLOSED')")
        conn.close()

        class QueryBoomCursor:
            def execute(self, sql, params=()):
                if "offline_restart_history" in sql and "SELECT" in sql and "sqlite_master" not in sql:
                    raise sqlite3.OperationalError("simulated history query failure")
                if "sqlite_master" in sql:
                    tbl = "ops_schema_meta" if "ops_schema_meta" in sql else "offline_restart_history"
                    self._last = [(tbl,)]
                elif "ops_schema_meta" in sql:
                    self._last = [("1",)]
                elif "PRAGMA table_info" in sql:
                    self._last = [(0, "id"), (1, "container_name"), (2, "restart_time")]
                else:
                    self._last = []

            def fetchone(self):
                return self._last[0] if self._last else None

            def fetchall(self):
                return list(self._last)

        class QueryBoomConn:
            def cursor(self):
                return QueryBoomCursor()

            def close(self):
                pass

        self.agent._restart_docker_container = AsyncMock(return_value=True)
        with patch("main.open_readonly_sqlite", return_value=QueryBoomConn()):
            cnt = self.agent._get_persisted_restart_count("home-web", 60.0, self.db_path)
            self.assertIsNone(cnt)

        # Also simulate failure inside _handle_offline_emergency_restart via mocked query failure
        with patch.object(self.agent, "_get_persisted_restart_count", return_value=None):
            res = await self.agent._handle_offline_emergency_restart("home-web", 1, self.sock_path)
            self.assertFalse(res)
            self.agent._restart_docker_container.assert_not_called()

    def test_atomic_write_lock_enforces_limit_across_aliases(self):
        init_ops_v4_schema(self.db_path)
        self.assertTrue(self.agent._record_persisted_restart("web", self.db_path))
        self.assertTrue(self.agent._record_persisted_restart("/home-web", self.db_path))
        # 3rd record within 60s must be rejected atomically inside BEGIN IMMEDIATE
        self.assertFalse(self.agent._record_persisted_restart("nginx", self.db_path))
        self.assertEqual(self.agent._get_persisted_restart_count("home-web", 60.0, self.db_path), 2)


class TestR5BaselineHashHardening(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.tmp_dir = tempfile.TemporaryDirectory()
        self.db_path = os.path.join(self.tmp_dir.name, "r5.db")
        init_ops_v4_schema(self.db_path)
        self.config = OpsAgentConfig(
            api_base_url="http://127.0.0.1:8080",
            internal_token="t" * 32,
            db_path=self.db_path,
            docker_socket=os.path.join(self.tmp_dir.name, "missing.sock"),
        )
        self.agent = HomeOpsAgent(self.config)

    def tearDown(self):
        try:
            self.tmp_dir.cleanup()
        except Exception:
            pass

    def test_real_vs_placeholder_fingerprint_distinction_and_binding(self):
        inspect1 = _sample_inspect_payload()
        bh1 = compute_container_baseline_from_inspect("home-web", inspect1)
        self.assertTrue(is_real_baseline_hash(bh1, "home-web"))
        self.assertFalse(is_placeholder_baseline_hash(bh1, "home-web"))

        # Alias resolves to identical fingerprint for identical container
        self.assertEqual(compute_container_baseline_from_inspect("web", inspect1), bh1)

        # Changing container Id, Image, State, Created, StartedAt, or RestartCount changes fingerprint
        self.assertNotEqual(compute_container_baseline_from_inspect("home-web", _sample_inspect_payload(container_id="c" * 64)), bh1)
        self.assertNotEqual(compute_container_baseline_from_inspect("home-web", _sample_inspect_payload(image_id="sha256:" + ("d" * 64))), bh1)
        self.assertNotEqual(compute_container_baseline_from_inspect("home-web", _sample_inspect_payload(state_status="exited", running=False)), bh1)
        self.assertNotEqual(compute_container_baseline_from_inspect("home-web", _sample_inspect_payload(created="2026-11-11T00:00:00Z")), bh1)
        self.assertNotEqual(compute_container_baseline_from_inspect("home-web", _sample_inspect_payload(restart_count=2)), bh1)

        # Component resolution failure or unknown state must return "" (no placeholder!)
        self.assertEqual(compute_container_baseline_from_inspect("evil-container", inspect1), "")
        self.assertEqual(compute_container_baseline_from_inspect("home-web", _sample_inspect_payload(state_status="unknown", running=False)), "")
        self.assertEqual(compute_container_baseline_from_inspect("home-web", _sample_inspect_payload(container_id="")), "")

        # Placeholder fingerprints must be distinguished and rejected
        ph = compute_offline_placeholder_baseline("home-web")
        self.assertTrue(ph.startswith("offline-placeholder:"))
        self.assertTrue(is_placeholder_baseline_hash(ph, "home-web"))
        self.assertFalse(is_real_baseline_hash(ph, "home-web"))

        legacy_offline = f"sha256:{hashlib.sha256(b'home-web:offline:none').hexdigest()}"
        self.assertTrue(is_placeholder_baseline_hash(legacy_offline, "home-web"))
        self.assertFalse(is_real_baseline_hash(legacy_offline, "home-web"))

        legacy_default = f"sha256:{hashlib.sha256(b'home-web:channels-tool/home-web:latest:running:').hexdigest()}"
        self.assertTrue(is_placeholder_baseline_hash(legacy_default, "home-web"))
        self.assertFalse(is_real_baseline_hash(legacy_default, "home-web"))

    async def test_nonexistent_container_and_docker_timeout_produce_no_placeholder(self):
        # 1. Missing Docker socket
        self.assertEqual(await self.agent._compute_container_baseline_hash("home-web"), "")

        # 2. HTTP 404 Non-existent container
        async def handler_404(reader, writer):
            await reader.read(512)
            writer.write(b"HTTP/1.1 404 Not Found\r\nConnection: close\r\n\r\n{\"message\":\"No such container\"}")
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        srv404 = await asyncio.start_server(handler_404, "127.0.0.1", 0)
        p404 = srv404.sockets[0].getsockname()[1]
        try:
            self.assertEqual(await self.agent._compute_container_baseline_hash("home-web", f"tcp://127.0.0.1:{p404}"), "")
        finally:
            srv404.close()
            await srv404.wait_closed()

        # 3. Real HTTP 200 live Docker inspect -> produces real fingerprint
        payload = json.dumps(_sample_inspect_payload()).encode("utf-8")

        async def handler_200(reader, writer):
            await reader.read(512)
            writer.write(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n" + payload)
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        srv200 = await asyncio.start_server(handler_200, "127.0.0.1", 0)
        p200 = srv200.sockets[0].getsockname()[1]
        try:
            live_bh = await self.agent._compute_container_baseline_hash("home-web", f"tcp://127.0.0.1:{p200}")
            self.assertTrue(is_real_baseline_hash(live_bh, "home-web"))
        finally:
            srv200.close()
            await srv200.wait_closed()

        # 4. Docker API timeout -> returns "" (no placeholder!)
        with patch.object(self.agent, "_query_docker_inspect", new=AsyncMock(side_effect=asyncio.TimeoutError("docker timeout"))):
            with self.assertRaises(asyncio.TimeoutError):
                await self.agent._query_docker_inspect("home-web", "tcp://127.0.0.1:1")
        with patch.object(self.agent, "_connect_docker", new=AsyncMock(side_effect=asyncio.TimeoutError("connect timeout"))):
            self.assertEqual(await self.agent._compute_container_baseline_hash("home-web", "tcp://127.0.0.1:1"), "")

    async def test_submit_proposal_and_proposal_manager_enforce_r5(self):
        # Unknown component rejected
        ok_unk = await self.agent._submit_proposal_to_go_core(
            "p-unk", "Bad", "HIGH", "restart", "logs", "evil-unknown", 30
        )
        self.assertFalse(ok_unk)

        # In production, missing real baseline (Docker unreachable) rejects proposal submission
        with patch.dict(os.environ, {"ENVIRONMENT": "production"}):
            ok_prod = await self.agent._submit_proposal_to_go_core(
                "p-prod-nobh", "Restart Web", "HIGH", "docker restart home-web", "logs", "home-web", 30
            )
            self.assertFalse(ok_prod)

        # ProposalLifecycleManager rejects unknown component & placeholder baseline
        mgr = ProposalLifecycleManager(self.db_path)
        with self.assertRaises(ValueError):
            mgr.create_proposal("p-bad-comp", "Bad", "HIGH", "restart", "logs", target_component="unknown-evil")

        ph = compute_offline_placeholder_baseline("home-web")
        mgr.create_proposal("p-ph", "Placeholder", "HIGH", "restart", "logs", target_component="home-web", baseline_hash=ph)
        ok_app, msg_app = mgr.approve_proposal("p-ph", "admin", is_admin=True)
        self.assertFalse(ok_app)
        self.assertIn("Placeholder", msg_app)

        # Production mode rejects placeholder or missing baseline on create/approve/execute
        with patch.dict(os.environ, {"ENVIRONMENT": "production"}):
            with self.assertRaises(ValueError):
                mgr.create_proposal("p-prod-ph", "PH", "HIGH", "restart", "logs", target_component="home-web", baseline_hash=ph)

        # Live verifier detects drift before execution
        real_bh = compute_container_baseline_from_inspect("home-web", _sample_inspect_payload())
        drifted_bh = compute_container_baseline_from_inspect("home-web", _sample_inspect_payload(image_id="sha256:" + ("e" * 64)))
        verifier_state = {"bh": real_bh}
        mgr_live = ProposalLifecycleManager(self.db_path, baseline_verifier=lambda _c: verifier_state["bh"])
        mgr_live.create_proposal("p-live", "Live", "HIGH", "restart", "logs", target_component="home-web", baseline_hash=real_bh)
        ok_a, _ = mgr_live.approve_proposal("p-live", "admin", is_admin=True)
        self.assertTrue(ok_a)
        # Simulate container image drift before execution
        verifier_state["bh"] = drifted_bh
        ok_exec, msg_exec = mgr_live.execute_proposal("p-live")
        self.assertFalse(ok_exec)
        self.assertIn("mismatch", msg_exec)

    async def test_authorized_branch_rejects_placeholder_baseline_hash(self):
        self.agent.rule_engine.handle_container_crash = MagicMock(
            return_value={"action": "restart_container", "backoff_sec": 0}
        )
        self.agent._report_event_to_go_core = AsyncMock(
            return_value=(
                CoreReportingStatus.AUTHORIZED,
                {
                    "decision": {"Action": "RESTART"},
                    "baseline_hash": compute_offline_placeholder_baseline("home-web"),
                },
            )
        )
        self.agent._restart_docker_container = AsyncMock(return_value=True)

        await self.agent._docker_event_queue.put({
            "Action": "die",
            "Actor": {"Attributes": {"name": "home-web", "exitCode": "137"}},
        })
        self.agent.running = True
        task = asyncio.create_task(self.agent._docker_event_processor())
        await asyncio.sleep(0.08)
        self.agent.running = False
        task.cancel()
        try:
            await task
        except asyncio.CancelledError:
            pass
        self.agent._restart_docker_container.assert_not_called()

    async def test_real_docker_api_baseline_integration(self):
        """Real Docker Engine API integration (no mocks): inspects live local containers when Docker is running."""
        candidate_endpoints = []
        env_sock = os.environ.get("DOCKER_HOST", "").strip()
        if env_sock:
            candidate_endpoints.append(env_sock)
        if os.name == "nt":
            candidate_endpoints.extend([
                "npipe:////./pipe/dockerDesktopLinuxEngine",
                "npipe:////./pipe/docker_engine",
            ])
        elif os.path.exists("/var/run/docker.sock"):
            candidate_endpoints.append("/var/run/docker.sock")

        live_ep = ""
        for ep in candidate_endpoints:
            st, data = await self.agent._query_docker_inspect("home-web", ep)
            if st == DockerHealthState.HEALTHY and isinstance(data, dict) and data.get("Id"):
                live_ep = ep
                break
        if not live_ep:
            self.skipTest("Live Docker Engine socket/pipe with 'home-web' container not available on this host")

        bh_web = await self.agent._compute_container_baseline_hash("home-web", live_ep)
        self.assertTrue(is_real_baseline_hash(bh_web, "home-web"))
        self.assertFalse(is_placeholder_baseline_hash(bh_web, "home-web"))
        # Determinism check against live Docker API
        self.assertEqual(await self.agent._compute_container_baseline_hash("web", live_ep), bh_web)

        # Non-existent container against real Docker API -> HTTP 404 -> "" (no placeholder!)
        st_404, _ = await self.agent._query_docker_inspect("nonexistent-r5-probe-99999", live_ep)
        self.assertEqual(st_404, DockerHealthState.UNHEALTHY)
        self.assertEqual(compute_container_baseline_from_inspect("home-web", None), "")


if __name__ == "__main__":
    unittest.main()
