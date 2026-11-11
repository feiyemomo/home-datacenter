"""
Phase 2 Hardening Verification Suite for Python Edge Ops Agent (R5, R6, R7, R8):
- R5/R8: Baseline Protocol v1 byte-for-byte golden vectors (11 vectors), duplicate container
  ambiguity rejection, container Name mismatch rejection, /images/{id}/json RepoDigests
  enrichment, and AUTHORIZED branch live/trusted baseline enforcement in production.
- R6: AsyncLeaseGuard background renewal, mid-execution task cancellation on lease loss,
  EXECUTION_UNKNOWN on Docker timeout/EOF post-send, ambiguous operation lock, and
  X-Ops-Fencing-Generation / X-Ops-Operation-ID headers.
- R7: Chained 5-stage SQLite lifecycle audit events, AUTHORIZED branch audit fail-closed,
  audit_protocol redaction/NaN/chain verification, and sync_offline_audit_logs local tamper
  quarantine + missing accepted_uuids fail-closed + permanent rejected[] quarantine.
- R5 Proposal & MQTT Hardening: DB-authoritative proposal reload, memory-vs-DB mismatch
  rejection, CAS revision enforcement, and MQTT malformed/spoofed IP rejection.
"""

import asyncio
import json
import os
import sqlite3
import sys
import tempfile
import unittest
from unittest.mock import AsyncMock, MagicMock, patch

CURRENT_DIR = os.path.dirname(os.path.abspath(__file__))
AGENT_DIR = os.path.dirname(CURRENT_DIR)
REPO_ROOT = os.path.dirname(os.path.dirname(AGENT_DIR))
sys.path.insert(0, AGENT_DIR)
sys.path.insert(0, CURRENT_DIR)

from audit_protocol import (
    EXECUTION_COMPLETED,
    EXECUTION_UNKNOWN,
    canonical_json_bytes,
    cap_and_redact_text,
    redact_sensitive_string,
    resolve_audit_hmac_keyring,
    verify_offline_audit_chain,
)
from config import (
    OpsAgentConfig,
    compute_container_baseline_details_from_inspect,
    is_real_baseline_hash,
    is_real_baseline_hash_v1,
)
from main import AuditStatus, CoreReportingStatus, DockerHealthState, HomeOpsAgent
from proposal import ProposalLifecycleManager
from rule_engine import TwoTierRuleEngine
from schema_helper import init_ops_v5_schema


def _valid_v1_sample_inspect(container_name: str = "/home-web") -> dict:
    return {
        "Id": "a1b2c3d4e5f6" + ("0" * 52),
        "Name": container_name,
        "Image": "sha256:" + ("1" * 64),
        "Created": "2026-11-10T01:02:03.456789123Z",
        "RestartCount": 0,
        "Config": {"Image": "nginx:1.25-alpine"},
        "State": {
            "Status": "running",
            "Running": True,
            "StartedAt": "2026-11-10T01:02:05Z",
        },
        "HostConfig": {"Privileged": False, "ReadonlyRootfs": True},
    }


class TestPhase2BaselineAndDockerR5R8(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.tmp_dir = tempfile.TemporaryDirectory()
        self.db_path = os.path.join(self.tmp_dir.name, "r5r8.db")
        init_ops_v5_schema(self.db_path)
        self.archive_dir = os.path.join(self.tmp_dir.name, "archive")
        os.environ["OPS_AUDIT_HMAC_KEY_ID"] = "k1"
        os.environ["OPS_AUDIT_HMAC_KEY"] = "super-secret-audit-signing-key-32bytes-minimum-test"
        os.environ["OPS_AUDIT_ARCHIVE_DIR"] = self.archive_dir
        os.environ["EMERGENCY_OVERRIDE_TOKEN"] = "emergency-override-strong-secret-token-32bytes"
        self.config = OpsAgentConfig(
            api_base_url="http://127.0.0.1:8080",
            internal_token="t" * 32,
            db_path=self.db_path,
            docker_socket=os.path.join(self.tmp_dir.name, "missing.sock"),
        )
        self.agent = HomeOpsAgent(self.config)

    def tearDown(self):
        for k in ("OPS_AUDIT_HMAC_KEY_ID", "OPS_AUDIT_HMAC_KEY", "OPS_AUDIT_ARCHIVE_DIR", "EMERGENCY_OVERRIDE_TOKEN"):
            os.environ.pop(k, None)
        try:
            self.tmp_dir.cleanup()
        except Exception:
            pass

    def test_baseline_protocol_v1_golden_vectors_exact_parity(self):
        gv_path = os.path.join(REPO_ROOT, "testdata", "baseline_golden_vectors.json")
        with open(gv_path, "r", encoding="utf-8") as fh:
            data = json.load(fh)

        vectors = data["vectors"]
        self.assertGreaterEqual(len(vectors), 11)
        for vec in vectors:
            details = compute_container_baseline_details_from_inspect(vec["component"], vec["inspect"])
            if not vec["expected_valid"]:
                self.assertEqual(details, {}, f"Expected invalid vector {vec['name']} to fail closed")
                continue
            self.assertEqual(details.get("canonical_json"), vec["expected_canonical_json"], f"canonical_json mismatch on {vec['name']}")
            self.assertEqual(details.get("baseline_hash"), vec["expected_baseline_hash"], f"baseline_hash mismatch on {vec['name']}")
            self.assertEqual(details.get("identity_hash"), vec["expected_identity_hash"], f"identity_hash mismatch on {vec['name']}")
            self.assertEqual(details.get("security_hash"), vec["expected_security_hash"], f"security_hash mismatch on {vec['name']}")
            self.assertEqual(details.get("state_hash"), vec["expected_state_hash"], f"state_hash mismatch on {vec['name']}")
            self.assertTrue(is_real_baseline_hash_v1(details["baseline_hash"], vec["component"]))

    async def test_duplicate_container_and_name_mismatch_and_image_repodigest_enrichment(self):
        state = {
            "containers": [
                {"Id": "111", "Names": ["/home-web"]},
                {"Id": "222", "Names": ["/web"]},
            ],
            "inspect": _valid_v1_sample_inspect("/home-web"),
            "image": {"RepoDigests": ["nginx@sha256:" + ("9" * 64)]},
        }

        async def mock_docker(reader, writer):
            req = (await reader.read(1024)).decode("utf-8", errors="ignore")
            first_line = req.splitlines()[0] if req else ""
            path = first_line.split()[1] if len(first_line.split()) >= 2 else ""
            if path.startswith("/containers/json"):
                body = json.dumps(state["containers"]).encode("utf-8")
            elif path.startswith("/images/"):
                body = json.dumps(state["image"]).encode("utf-8")
            elif path.startswith("/containers/") and path.endswith("/json"):
                body = json.dumps(state["inspect"]).encode("utf-8")
            else:
                writer.write(b"HTTP/1.1 404 Not Found\r\nConnection: close\r\n\r\n{}")
                await writer.drain()
                writer.close()
                await writer.wait_closed()
                return
            writer.write(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n" + body)
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        srv = await asyncio.start_server(mock_docker, "127.0.0.1", 0)
        port = srv.sockets[0].getsockname()[1]
        sock = f"tcp://127.0.0.1:{port}"
        try:
            # 1. Ambiguous duplicate containers must fail closed
            self.assertEqual(await self.agent._compute_container_baseline_hash("home-web", sock), "")

            # 2. Single container + Name mismatch must fail closed
            state["containers"] = [{"Id": "111", "Names": ["/home-web"]}]
            state["inspect"] = _valid_v1_sample_inspect("/evil-spoofed-container")
            self.assertEqual(await self.agent._compute_container_baseline_hash("home-web", sock), "")

            # 3. Valid single container without RepoDigests fetches /images/{Image}/json automatically
            state["inspect"] = _valid_v1_sample_inspect("/home-web")
            live_bh = await self.agent._compute_container_baseline_hash("home-web", sock)
            self.assertTrue(is_real_baseline_hash_v1(live_bh, "home-web"))

            # Verify it equals inspect with RepoDigests already embedded (matches Go byte-for-byte)
            enriched = _valid_v1_sample_inspect("/home-web")
            enriched["RepoDigests"] = state["image"]["RepoDigests"]
            expected = compute_container_baseline_details_from_inspect("home-web", enriched)["baseline_hash"]
            self.assertEqual(live_bh, expected)
        finally:
            srv.close()
            await srv.wait_closed()

    async def test_authorized_branch_requires_live_baseline_in_production_and_checks_audit_commit(self):
        self.agent.rule_engine.handle_container_crash = MagicMock(
            return_value={"action": "restart_container", "backoff_sec": 0}
        )
        self.agent._report_event_to_go_core = AsyncMock(
            return_value=(CoreReportingStatus.AUTHORIZED, {"decision": {"Action": "RESTART"}, "operation_id": "op-prod-1"})
        )
        self.agent._restart_docker_container = AsyncMock(return_value=True)
        self.agent._verify_container_health = AsyncMock(return_value=DockerHealthState.HEALTHY)

        # In production without live Docker baseline, AUTHORIZED without baseline_hash must fail closed
        with patch.dict(os.environ, {"ENVIRONMENT": "production"}):
            await self.agent._docker_event_queue.put({
                "Action": "die",
                "Actor": {"Attributes": {"name": "home-web", "exitCode": "137"}},
            })
            self.agent.running = True
            t = asyncio.create_task(self.agent._docker_event_processor())
            await asyncio.sleep(0.08)
            self.agent.running = False
            t.cancel()
            try:
                await t
            except asyncio.CancelledError:
                pass
            self.agent._restart_docker_container.assert_not_called()

        # When live baseline succeeds, if terminal audit commit fails, _execute_guarded_restart returns False
        lease_ok, lease_tok, f_gen = self.agent._acquire_recovery_lease_grant("home-web", self.db_path)
        self.assertTrue(lease_ok)
        call_count = {"n": 0}
        orig_rec = self.agent._record_offline_audit_strict

        def fail_terminal(*args, **kwargs):
            call_count["n"] += 1
            if kwargs.get("event_type") == EXECUTION_COMPLETED:
                return AuditStatus.AUDIT_FAILED, ""
            return orig_rec(*args, **kwargs)

        with patch.object(self.agent, "_record_offline_audit_strict", side_effect=fail_terminal):
            ok = await self.agent._execute_guarded_restart(
                canonical="home-web",
                audit_container_name="home-web",
                docker_container="home-web",
                sock_path=self.config.docker_socket,
                db_path=self.db_path,
                action_name="authorized_restart",
                exit_code=137,
                lease_token=lease_tok,
                fencing_gen=f_gen,
                run_id="run-auth-fail-audit",
                corr_id="corr-1",
                op_id="op-auth-fail-audit",
                trigger_source="go_core_authorized",
                actor_type="internal_service",
                actor_id="home-ops-agent",
            )
            self.assertFalse(ok)


class TestPhase2LeaseGuardAndAuditR6R7(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.tmp_dir = tempfile.TemporaryDirectory()
        self.db_path = os.path.join(self.tmp_dir.name, "r6r7.db")
        init_ops_v5_schema(self.db_path)
        self.archive_dir = os.path.join(self.tmp_dir.name, "archive")
        os.environ["OPS_AUDIT_HMAC_KEY_ID"] = "k1"
        os.environ["OPS_AUDIT_HMAC_KEY"] = "super-secret-audit-signing-key-32bytes-minimum-test"
        os.environ["OPS_AUDIT_HMAC_KEYS"] = "k1:super-secret-audit-signing-key-32bytes-minimum-test"
        os.environ["OPS_AUDIT_ARCHIVE_DIR"] = self.archive_dir
        os.environ["EMERGENCY_OVERRIDE_TOKEN"] = "emergency-override-strong-secret-token-32bytes"
        self.config = OpsAgentConfig(
            api_base_url="http://127.0.0.1:8080",
            internal_token="t" * 32,
            db_path=self.db_path,
        )
        self.agent = HomeOpsAgent(self.config)

    def tearDown(self):
        for k in (
            "OPS_AUDIT_HMAC_KEY_ID",
            "OPS_AUDIT_HMAC_KEY",
            "OPS_AUDIT_HMAC_KEYS",
            "OPS_AUDIT_ARCHIVE_DIR",
            "EMERGENCY_OVERRIDE_TOKEN",
            "OPS_LEASE_TTL_SEC",
            "OPS_LEASE_RENEW_INTERVAL_SEC",
        ):
            os.environ.pop(k, None)
        try:
            self.tmp_dir.cleanup()
        except Exception:
            pass

    async def test_async_lease_guard_renews_and_cancels_on_stolen_lease(self):
        os.environ["OPS_LEASE_TTL_SEC"] = "6.0"
        os.environ["OPS_LEASE_RENEW_INTERVAL_SEC"] = "0.08"

        # Case 1: Long restart succeeds with >=2 background lease renewals
        async def slow_ok_restart(container_name, sock_path, fencing_generation=0, operation_id=""):
            self.assertGreater(fencing_generation, 0)
            self.assertTrue(operation_id)
            await asyncio.sleep(0.25)
            return True

        self.agent._restart_docker_container = slow_ok_restart
        self.agent._verify_container_health = AsyncMock(return_value=DockerHealthState.HEALTHY)
        ok = await self.agent._handle_offline_emergency_restart("home-web", 137, "/var/run/docker.sock")
        self.assertTrue(ok)

        conn = sqlite3.connect(self.db_path)
        conn.row_factory = sqlite3.Row
        rows = conn.execute("SELECT * FROM offline_ops_audit ORDER BY offline_seq ASC").fetchall()
        conn.close()
        self.assertEqual(len(rows), 5)
        self.assertEqual([r["event_type"] for r in rows], [
            "EVENT_RECEIVED",
            "RECOVERY_LEASE_ACQUIRED",
            "EXECUTION_STARTED",
            "HEALTH_CHECK_COMPLETED",
            "EXECUTION_COMPLETED",
        ])
        ev_completed = json.loads(rows[-1]["payload_json"])
        self.assertGreaterEqual(json.loads(ev_completed["evidence"])["lease_renewals"], 2)

        # Case 2: Lease stolen mid-flight -> AsyncLeaseGuard cancels execution & records EXECUTION_UNKNOWN
        async def stalled_restart_stolen(container_name, sock_path, fencing_generation=0, operation_id=""):
            # Simulate external process stealing/deleting lease row
            conn2 = sqlite3.connect(self.db_path)
            conn2.execute("UPDATE ops_recovery_leases SET lease_token = 'stolen-by-peer' WHERE component = 'home-web'")
            conn2.commit()
            conn2.close()
            await asyncio.sleep(5.0)
            return True

        self.agent._restart_docker_container = stalled_restart_stolen
        ok2 = await self.agent._handle_offline_emergency_restart("home-web", 137, "/var/run/docker.sock")
        self.assertFalse(ok2)
        self.assertIn("home-web", self.agent._ambiguous_operations)

        # Ambiguous lock prevents premature retry
        ok3 = await self.agent._handle_offline_emergency_restart("home-web", 137, "/var/run/docker.sock")
        self.assertFalse(ok3)

    async def test_docker_timeout_post_send_marks_execution_unknown_and_sends_fencing_headers(self):
        received_headers = {}

        async def slow_eof_docker(reader, writer):
            data = (await reader.read(2048)).decode("utf-8", errors="ignore")
            for line in data.splitlines()[1:]:
                if ":" in line:
                    k, v = line.split(":", 1)
                    received_headers[k.strip()] = v.strip()
            # Close without sending HTTP status -> post-send EOF / ambiguous outcome
            writer.close()
            await writer.wait_closed()

        srv = await asyncio.start_server(slow_eof_docker, "127.0.0.1", 0)
        port = srv.sockets[0].getsockname()[1]
        try:
            res = await self.agent._handle_offline_emergency_restart("frigate", 137, f"tcp://127.0.0.1:{port}")
            self.assertFalse(res)
            self.assertEqual(received_headers.get("X-Ops-Fencing-Generation"), "1")
            self.assertTrue(received_headers.get("X-Ops-Operation-ID", "").startswith("pyop-"))
            self.assertIn("frigate", self.agent._ambiguous_operations)
            conn = sqlite3.connect(self.db_path)
            last_ev = conn.execute("SELECT event_type FROM offline_ops_audit ORDER BY id DESC LIMIT 1").fetchone()[0]
            conn.close()
            self.assertEqual(last_ev, EXECUTION_UNKNOWN)
        finally:
            srv.close()
            await srv.wait_closed()

    async def test_sync_detects_local_tamper_and_quarantines_permanent_rejects_without_stalling(self):
        _, u1 = self.agent._record_offline_audit_strict("home-web", "emergency_restart", 1, True, operation_id="op-1", run_id="r-1")
        _, u2 = self.agent._record_offline_audit_strict("home-web", "emergency_restart", 1, True, operation_id="op-2", run_id="r-2")
        _, u3 = self.agent._record_offline_audit_strict("home-web", "emergency_restart", 1, True, operation_id="op-3", run_id="r-3")
        self.assertTrue(u1 and u2 and u3)

        # Tamper u1 container_name directly in SQLite -> local hash chain verification must catch it!
        conn = sqlite3.connect(self.db_path)
        conn.execute("UPDATE offline_ops_audit SET container_name = 'tampered-target' WHERE event_uuid = ?", (u1,))
        conn.commit()
        conn.close()

        # Server responds missing accepted_uuids first -> fail-closed (keeps u2, u3 unsynced)
        def _resp_with(obj):
            m = MagicMock()
            m.status = 200
            m.read.return_value = json.dumps(obj).encode("utf-8")
            m.__enter__.return_value = m
            m.__exit__.return_value = False
            return m

        health_resp = _resp_with({"status": "ok"})
        bad_sync_resp = _resp_with({"status": "ok"})  # Missing accepted_uuids

        with patch("urllib.request.urlopen", side_effect=[health_resp, bad_sync_resp]):
            cnt = await self.agent.sync_offline_audit_logs()
            self.assertEqual(cnt, 0)

        conn = sqlite3.connect(self.db_path)
        rows = {r[0]: (r[1], r[2]) for r in conn.execute("SELECT event_uuid, synced, audit_status FROM offline_ops_audit").fetchall()}
        conn.close()
        self.assertEqual(rows[u1], (-1, "AUDIT_TAMPERED"))
        self.assertEqual(rows[u2][0], 0)
        self.assertEqual(rows[u3][0], 0)

        # Now server accepts u3 and rejects u2 -> u3 becomes synced=1 and u2 quarantined synced=-1 (no stall)
        good_sync_resp = _resp_with({
            "data": {
                "accepted_uuids": [u3],
                "rejected": [{"event_uuid": u2, "code": "CHAIN_FORK", "reason": "broken prev hash"}],
            }
        })
        with patch("urllib.request.urlopen", side_effect=[health_resp, good_sync_resp]):
            cnt2 = await self.agent.sync_offline_audit_logs()
            self.assertEqual(cnt2, 1)

        conn = sqlite3.connect(self.db_path)
        rows2 = {r[0]: (r[1], r[2]) for r in conn.execute("SELECT event_uuid, synced, audit_status FROM offline_ops_audit").fetchall()}
        conn.close()
        self.assertEqual(rows2[u2], (-1, "AUDIT_REJECTED"))
        self.assertEqual(rows2[u3][0], 1)

    def test_audit_protocol_redaction_and_nan_and_proposal_and_mqtt_hardening(self):
        # 1. OPS_AUDIT_HMAC_KEYS redaction + unquoted JSON password redaction
        s = redact_sensitive_string(
            'OPS_AUDIT_HMAC_KEYS=k1:secretval12345,k2:anothersecret {password: mysecretpass}'
        )
        self.assertNotIn("secretval12345", s)
        self.assertNotIn("anothersecret", s)
        self.assertNotIn("mysecretpass", s)

        # 2. cap_and_redact_text produces deterministic sorted keys
        red_json = cap_and_redact_text('{"z_key":1,"a_password":"leak","m_key":2}')
        self.assertEqual(red_json, '{"a_password":"[REDACTED]","m_key":2,"z_key":1}')

        # 3. NaN / Inf rejected strictly
        with self.assertRaises(ValueError):
            canonical_json_bytes({"bad": float("nan")})

        # 4. Proposal DB-authoritative reload catches memory tamper & CAS conflict
        bh = compute_container_baseline_details_from_inspect("home-web", _valid_v1_sample_inspect())["baseline_hash"]
        mgr = ProposalLifecycleManager(self.db_path)
        mgr.create_proposal("prop-cas-1", "Restart Web", "HIGH", "docker restart home-web", "logs", target_component="home-web", baseline_hash=bh)
        # Mutate in-memory proposal silently -> approve must reload DB & detect mismatch
        mgr.proposals["prop-cas-1"]["baseline_hash"] = "v1:" + ("f" * 64)
        ok_tamper, msg_tamper = mgr.approve_proposal("prop-cas-1", "admin", is_admin=True)
        self.assertFalse(ok_tamper)
        self.assertIn("disagrees", msg_tamper.lower())

        # Reload cleanly via fresh manager, approve, then test CAS revision conflict
        mgr2 = ProposalLifecycleManager(self.db_path)
        ok_app, _ = mgr2.approve_proposal("prop-cas-1", "admin", is_admin=True)
        self.assertTrue(ok_app)
        # Concurrent revision bump in SQLite must cause execute_proposal CAS failure
        conn = sqlite3.connect(self.db_path)
        conn.execute("UPDATE pending_proposals SET revision = revision + 1 WHERE proposal_id = 'prop-cas-1'")
        conn.commit()
        conn.close()
        ok_ex, msg_ex = mgr2.execute_proposal("prop-cas-1")
        self.assertFalse(ok_ex)
        self.assertIn("cas", msg_ex.lower())

        # 5. MQTT & RuleEngine reject spoofed/loopback/invalid/unauthenticated IPs without mutating state
        re_eng = TwoTierRuleEngine(self.config)
        self.assertEqual(re_eng.handle_auth_failure("127.0.0.1", source="mqtt", authenticated=True)["action"], "rejected_protected_ip")
        self.assertEqual(re_eng.handle_auth_failure("not-an-ip", source="mqtt", authenticated=True)["action"], "rejected_invalid_ip")
        self.assertEqual(re_eng.handle_auth_failure("203.0.113.10", source="mqtt", authenticated=False)["action"], "rejected_unauthenticated")
        self.assertEqual(len(re_eng.auth_fail_counts), 0)

        self.agent._process_mqtt_security_message("security/auth_fail", '{"ip":"192.0.2.55"}')
        self.assertIn("192.0.2.55", self.agent.rule_engine.auth_fail_counts)
        self.assertTrue(os.path.isfile(os.path.join(self.archive_dir, "python-mqtt-audit.jsonl")))

        # Spoofed/loopback MQTT message must NOT emit MQTT audit or mutate counters
        self.agent._process_mqtt_security_message("security/auth_fail", '{"ip":"127.0.0.1"}')
        self.assertNotIn("127.0.0.1", self.agent.rule_engine.auth_fail_counts)


if __name__ == "__main__":
    unittest.main()
