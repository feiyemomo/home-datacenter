"""
Round 5 Verification Suite for Python Edge AgentAuditEngine Protocol Parity,
V5 Offline Hash Chain, Pre-Mutation Fail-Closed, B4 AUTHORIZED Auditing,
Strict accepted_uuids Partial ACK, and RuleEngine Security Guards.
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
PARENT_DIR = os.path.dirname(CURRENT_DIR)
sys.path.insert(0, PARENT_DIR)
sys.path.insert(0, CURRENT_DIR)

from audit_protocol import (
    AUDIT_GENESIS_HASH,
    canonical_json_bytes,
    compute_event_entry_hash,
    compute_event_hmac,
    format_rfc3339_ms,
    redact_sensitive_string,
    verify_event_hmac,
)
from config import OpsAgentConfig
from main import AuditStatus, CoreReportingStatus, DockerHealthState, HomeOpsAgent
from rule_engine import TwoTierRuleEngine
from schema_helper import init_ops_v5_schema


class TestAuditProtocolR5(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.tmp_dir = tempfile.TemporaryDirectory()
        self.db_path = os.path.join(self.tmp_dir.name, "test_v5.db")
        self.archive_dir = os.path.join(self.tmp_dir.name, "archive")
        init_ops_v5_schema(self.db_path)

        os.environ["OPS_AUDIT_HMAC_KEY_ID"] = "k1"
        os.environ["OPS_AUDIT_HMAC_KEY"] = "super-secret-audit-signing-key-32bytes-minimum-test"
        os.environ["OPS_AUDIT_HMAC_KEYS"] = (
            "k1:super-secret-audit-signing-key-32bytes-minimum-test,"
            "k2:rotated-secret-audit-signing-key-32bytes-test-v2"
        )
        os.environ["OPS_AUDIT_ARCHIVE_DIR"] = self.archive_dir
        os.environ["EMERGENCY_OVERRIDE_TOKEN"] = "emergency-override-strong-secret-token-32bytes"

        self.config = OpsAgentConfig(
            api_base_url="http://127.0.0.1:8080",
            internal_token="test-internal-token-32-chars-long",
            db_path=self.db_path,
        )
        self.agent = HomeOpsAgent(self.config)

    def tearDown(self):
        import gc
        gc.collect()
        for key in (
            "OPS_AUDIT_HMAC_KEY_ID",
            "OPS_AUDIT_HMAC_KEY",
            "OPS_AUDIT_HMAC_KEYS",
            "OPS_AUDIT_ARCHIVE_DIR",
            "ENABLE_LOCAL_DOCKER_PRUNE",
            "EMERGENCY_OVERRIDE_TOKEN",
        ):
            os.environ.pop(key, None)
        try:
            self.tmp_dir.cleanup()
        except Exception:
            pass

    def test_golden_vectors_parity_with_go(self):
        vec_path = os.path.abspath(os.path.join(PARENT_DIR, "..", "..", "testdata", "audit_golden_vectors.json"))
        with open(vec_path, "r", encoding="utf-8") as fh:
            doc = json.load(fh)

        keyring = doc["keyring"]
        for idx, cv in enumerate(doc["canonical_json_vectors"]):
            got = canonical_json_bytes(cv["input"]).decode("utf-8")
            self.assertEqual(got, cv["expected_json"], f"canonical mismatch at index {idx}")

        for idx, tv in enumerate(doc["timestamp_vectors"]):
            got = format_rfc3339_ms(tv["ms"])
            self.assertEqual(got, tv["expected_rfc3339_ms"], f"timestamp mismatch at index {idx}")

        for idx, ev_vec in enumerate(doc["event_vectors"]):
            ev = ev_vec["event"]
            got_hash = compute_event_entry_hash(ev)
            self.assertEqual(got_hash, ev_vec["expected_entry_hash"], f"entry_hash mismatch at index {idx}")

            kid = ev_vec["key_id"]
            got_hmac = compute_event_hmac(
                kid=kid,
                secret=keyring[kid],
                stream_id=ev_vec["stream_id"],
                seq=ev_vec["seq"],
                entry_hash=got_hash,
                created_at_ms=ev["created_at_ms"],
            )
            self.assertEqual(got_hmac, ev_vec["expected_hmac"], f"hmac mismatch at index {idx}")

            ev_check = dict(ev)
            ev_check["stream_id"] = ev_vec["stream_id"]
            ev_check["seq"] = ev_vec["seq"]
            ev_check["entry_hash"] = got_hash
            ev_check["hmac_signature"] = got_hmac
            self.assertTrue(verify_event_hmac(ev_check, keyring))

    def test_redaction_preserves_bearer_prefix_and_redacts_secrets(self):
        sample = 'Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.aaaa.bbbb token=topsecret http://admin:pass123@nas.local/?sig=abc'
        redacted = redact_sensitive_string(sample)
        self.assertIn("Authorization: Bearer [REDACTED]", redacted)
        self.assertNotIn("topsecret", redacted)
        self.assertNotIn("pass123", redacted)
        self.assertNotIn("sig=abc", redacted)

    def test_v5_offline_hash_chain_across_multiple_records(self):
        st1, uuid1 = self.agent._record_offline_audit_strict(
            container_name="home-web",
            action="offline_restart",
            exit_code=137,
            success=True,
            operation_id="op-chain-1",
            fencing_generation=1,
            run_id="run-chain-1",
            event_type="EXECUTION_COMPLETED",
        )
        st2, uuid2 = self.agent._record_offline_audit_strict(
            container_name="frigate",
            action="offline_restart",
            exit_code=1,
            success=False,
            operation_id="op-chain-2",
            fencing_generation=2,
            run_id="run-chain-2",
            event_type="EXECUTION_FAILED",
            evidence={"error": "probe failed"},
        )
        self.assertEqual(st1, AuditStatus.AUDIT_COMMITTED)
        self.assertEqual(st2, AuditStatus.AUDIT_COMMITTED)
        self.assertTrue(bool(uuid1) and bool(uuid2))

        conn = sqlite3.connect(self.db_path)
        conn.row_factory = sqlite3.Row
        rows = conn.execute("SELECT * FROM offline_ops_audit ORDER BY id ASC").fetchall()
        conn.close()

        self.assertEqual(len(rows), 2)
        self.assertEqual(rows[0]["offline_seq"], 1)
        self.assertEqual(rows[0]["local_prev_hash"], AUDIT_GENESIS_HASH)
        self.assertEqual(rows[0]["error_message"], "")
        self.assertEqual(len(rows[0]["entry_hash"]), 64)
        self.assertTrue(rows[0]["hmac_signature"].startswith("hmac-sha256:k1:"))
        self.assertEqual(rows[1]["offline_seq"], 2)
        self.assertEqual(rows[1]["local_prev_hash"], rows[0]["entry_hash"])
        self.assertEqual(rows[1]["error_message"], "exit_code=1")
        self.assertNotEqual(rows[1]["entry_hash"], rows[0]["entry_hash"])

        jsonl_path = os.path.join(self.archive_dir, "python-offline-audit.jsonl")
        self.assertTrue(os.path.isfile(jsonl_path))

    def test_v5_missing_hmac_key_fails_closed(self):
        saved = {
            k: os.environ.pop(k, None)
            for k in ("OPS_AUDIT_HMAC_KEY", "OPS_AUDIT_HMAC_KEYS", "ENVIRONMENT", "OPS_AUDIT_REQUIRE_HMAC")
        }
        try:
            os.environ["ENVIRONMENT"] = "production"
            os.environ["OPS_AUDIT_REQUIRE_HMAC"] = "1"
            st, uid = self.agent._record_offline_audit_strict(
                container_name="home-web",
                action="offline_restart",
                exit_code=1,
                success=False,
                operation_id="op-no-key",
                run_id="run-no-key",
            )
            self.assertEqual(st, AuditStatus.AUDIT_FAILED)
            self.assertEqual(uid, "")
        finally:
            os.environ.pop("ENVIRONMENT", None)
            os.environ.pop("OPS_AUDIT_REQUIRE_HMAC", None)
            for k, v in saved.items():
                if v is not None:
                    os.environ[k] = v

    async def test_pre_mutation_audit_table_broken_blocks_offline_restart(self):
        # Drop offline_ops_audit table to prove fail-closed pre-mutation audit blocks docker restart
        conn = sqlite3.connect(self.db_path)
        conn.execute("DROP TABLE offline_ops_audit")
        conn.commit()
        conn.close()

        self.agent._restart_docker_container = AsyncMock(return_value=True)
        result = await self.agent._handle_offline_emergency_restart("home-web", 137, "/var/run/docker.sock")
        self.assertFalse(result)
        self.agent._restart_docker_container.assert_not_called()

    async def test_b4_authorized_branch_audits_local_execution(self):
        self.agent.rule_engine.handle_container_crash = MagicMock(
            return_value={"action": "restart_container", "backoff_sec": 0}
        )
        self.agent._report_event_to_go_core = AsyncMock(
            return_value=(CoreReportingStatus.AUTHORIZED, {"decision": {"Action": "RESTART"}, "operation_id": "op-auth-1"})
        )
        self.agent._restart_docker_container = AsyncMock(return_value=True)
        self.agent._verify_container_health = AsyncMock(return_value=DockerHealthState.HEALTHY)

        event = {
            "Action": "die",
            "Actor": {"Attributes": {"name": "home-web", "exitCode": "137"}},
        }
        await self.agent._docker_event_queue.put(event)

        self.agent.running = True
        proc_task = asyncio.create_task(self.agent._docker_event_processor())
        await asyncio.sleep(0.08)
        self.agent.running = False
        proc_task.cancel()
        try:
            await proc_task
        except asyncio.CancelledError:
            pass

        self.agent._restart_docker_container.assert_called_once()
        conn = sqlite3.connect(self.db_path)
        rows = conn.execute("SELECT event_type, container_name, action FROM offline_ops_audit").fetchall()
        conn.close()
        self.assertGreaterEqual(len(rows), 1)
        self.assertEqual(rows[0][1], "home-web")
        self.assertEqual(rows[0][2], "authorized_restart")

    async def test_strict_accepted_uuids_partial_ack_sync(self):
        self.agent._record_offline_audit_strict(
            container_name="home-web",
            action="offline_restart",
            exit_code=0,
            success=True,
            operation_id="op-sync-1",
            run_id="run-sync-1",
        )
        self.agent._record_offline_audit_strict(
            container_name="frigate",
            action="offline_restart",
            exit_code=0,
            success=True,
            operation_id="op-sync-2",
            run_id="run-sync-2",
        )
        conn = sqlite3.connect(self.db_path)
        uuids = [r[0] for r in conn.execute("SELECT event_uuid FROM offline_ops_audit ORDER BY id ASC").fetchall()]
        conn.close()
        self.assertEqual(len(uuids), 2)

        resp_body = json.dumps({
            "data": {
                "accepted_uuids": [uuids[0]],
                "rejected": [{"event_uuid": uuids[1], "reason": "bad hmac"}],
            }
        }).encode("utf-8")
        mock_resp = MagicMock()
        mock_resp.status = 200
        mock_resp.read.return_value = resp_body
        mock_resp.__enter__.return_value = mock_resp
        mock_resp.__exit__.return_value = False

        with patch("urllib.request.urlopen", return_value=mock_resp):
            await self.agent.sync_offline_audit_logs()

        conn = sqlite3.connect(self.db_path)
        statuses = {r[0]: r[1] for r in conn.execute("SELECT event_uuid, synced FROM offline_ops_audit").fetchall()}
        conn.close()
        self.assertEqual(statuses[uuids[0]], 1)
        self.assertEqual(statuses[uuids[1]], 0)

    def test_rule_engine_maintenance_lock_blocks_docker_prune_and_emits_audit(self):
        os.environ["ENABLE_LOCAL_DOCKER_PRUNE"] = "1"
        conn = sqlite3.connect(self.db_path)
        conn.execute("INSERT INTO ops_maintenance_locks (active, locked_by, reason, expires_at, updated_at) VALUES (1, 'admin', 'upgrade', '2099-01-01T00:00:00Z', '2026-11-10T00:00:00Z')")
        conn.commit()
        conn.close()

        re_engine = TwoTierRuleEngine(self.config)
        with patch("subprocess.run") as mock_sub:
            purged = re_engine._purge_temp_cache(self.tmp_dir.name)
            mock_sub.assert_not_called()
        self.assertIn("docker_prune_blocked_by_maintenance_lock", purged)

        rule_jsonl = os.path.join(self.archive_dir, "python-rule-audit.jsonl")
        self.assertTrue(os.path.isfile(rule_jsonl))


if __name__ == "__main__":
    unittest.main()
