"""
R11 Verification Suite for Python Edge Agent Hardening:
- R7 Offline Audit Chain Continuity (genesis & confirmed stream_tips), ring=None never bypasses HMAC,
  downgrade rejection, deleted-predecessor SEQ_GAP detection, and DUPLICATE_ID_CONFLICT error_code quarantine.
- R5 Strict Production Baseline Protocol v1 Enforcement (rejects legacy sha256:<hex> in production)
  and Docker empty-Name mismatch fail-closed check.
- R6 AsyncLeaseGuard mid-flight cancellation aborts stalled Docker HTTP sockets promptly and escalates
  post-send cancellation / timeout / EOF to EXECUTION_UNKNOWN.
"""

import asyncio
import json
import os
import sqlite3
import sys
import tempfile
import time
import unittest
from unittest.mock import MagicMock, patch

CURRENT_DIR = os.path.dirname(os.path.abspath(__file__))
PARENT_DIR = os.path.dirname(CURRENT_DIR)
sys.path.insert(0, PARENT_DIR)
sys.path.insert(0, CURRENT_DIR)

from audit_protocol import (
    EXECUTION_UNKNOWN,
    compute_event_entry_hash,
    compute_event_hmac,
    format_rfc3339_ms,
    verify_offline_audit_chain,
)
from config import (
    OpsAgentConfig,
    compute_container_baseline_from_inspect,
    is_real_baseline_hash,
    is_real_baseline_hash_v1,
)
from main import DockerHealthState, HomeOpsAgent
from proposal import ProposalLifecycleManager
from schema_helper import init_ops_v5_schema


def _sample_v1_inspect(name: str = "/home-web") -> dict:
    return {
        "Id": "1" * 64,
        "Name": name,
        "Image": "sha256:" + ("2" * 64),
        "Created": "2026-11-11T01:00:00Z",
        "RestartCount": 0,
        "RepoDigests": ["nginx@sha256:" + ("3" * 64)],
        "Config": {
            "Image": "nginx:1.27-alpine",
            "User": "101",
            "Entrypoint": ["/docker-entrypoint.sh"],
            "Cmd": ["nginx", "-g", "daemon off;"],
            "Env": ["PATH=/usr/local/sbin:/usr/local/bin"],
        },
        "HostConfig": {
            "Privileged": False,
            "ReadonlyRootfs": True,
            "NetworkMode": "bridge",
            "RestartPolicy": {"Name": "unless-stopped", "MaximumRetryCount": 0},
            "Memory": 536870912,
            "NanoCpus": 1000000000,
            "PidsLimit": 256,
        },
        "State": {
            "Status": "running",
            "Running": True,
            "StartedAt": "2026-11-11T01:00:05Z",
        },
    }


def _fetch_all_audit_dicts(db_path: str) -> list[dict]:
    conn = sqlite3.connect(db_path)
    conn.row_factory = sqlite3.Row
    rows = conn.execute("SELECT * FROM offline_ops_audit ORDER BY offline_seq ASC, id ASC").fetchall()
    conn.close()
    out = []
    for r in rows:
        d = dict(r)
        meta = json.loads(d["payload_json"])
        for k, v in meta.items():
            if k not in d or not d[k]:
                d[k] = v
        d["audit_id"] = d["event_uuid"]
        d["target"] = d["container_name"]
        out.append(d)
    return out


class TestR11Hardening(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.tmp_dir = tempfile.TemporaryDirectory()
        self.db_path = os.path.join(self.tmp_dir.name, "r11.db")
        init_ops_v5_schema(self.db_path)
        self.archive_dir = os.path.join(self.tmp_dir.name, "archive")
        self.hmac_key = "super-secret-audit-signing-key-32bytes-minimum-test"
        self.env_patcher = patch.dict(
            os.environ,
            {
                "ENVIRONMENT": "test",
                "APP_ENV": "test",
                "OPS_AUDIT_HMAC_KEY_ID": "k1",
                "OPS_AUDIT_HMAC_KEY": self.hmac_key,
                "OPS_AUDIT_HMAC_KEYS": f"k1:{self.hmac_key}",
                "OPS_AUDIT_ARCHIVE_DIR": self.archive_dir,
                "EMERGENCY_OVERRIDE_TOKEN": "emergency-override-strong-secret-token-32bytes",
            },
        )
        self.env_patcher.start()
        self.config = OpsAgentConfig(
            api_base_url="http://127.0.0.1:8080",
            internal_token="t" * 32,
            db_path=self.db_path,
        )
        self.agent = HomeOpsAgent(self.config)

    def tearDown(self):
        self.env_patcher.stop()
        for k in (
            "OPS_AUDIT_HMAC_KEY_ID",
            "OPS_AUDIT_HMAC_KEY",
            "OPS_AUDIT_HMAC_KEYS",
            "OPS_AUDIT_ARCHIVE_DIR",
            "EMERGENCY_OVERRIDE_TOKEN",
            "OPS_STRICT_BASELINE",
            "OPS_LEASE_TTL_SEC",
            "OPS_LEASE_RENEW_INTERVAL_SEC",
        ):
            os.environ.pop(k, None)
        try:
            self.tmp_dir.cleanup()
        except Exception:
            pass

    def test_verify_offline_audit_chain_rejects_none_ring_and_enforces_continuity(self):
        self.agent._record_offline_audit_strict("home-web", "emergency_restart", 1, True, operation_id="op-1", run_id="r-1")
        self.agent._record_offline_audit_strict("home-web", "emergency_restart", 1, True, operation_id="op-2", run_id="r-2")
        self.agent._record_offline_audit_strict("home-web", "emergency_restart", 1, True, operation_id="op-3", run_id="r-3")
        recs = _fetch_all_audit_dicts(self.db_path)
        ring = {"k1": self.hmac_key}

        # Valid full chain from genesis
        ok, reason = verify_offline_audit_chain(recs, ring=ring)
        self.assertTrue(ok, reason)

        # ring=None or empty ring must NEVER bypass HMAC
        ok_none, msg_none = verify_offline_audit_chain(recs, ring=None)
        self.assertFalse(ok_none)
        self.assertIn("HMAC", msg_none)
        ok_empty, _ = verify_offline_audit_chain(recs, ring={})
        self.assertFalse(ok_empty)

        # Starting at seq=2 without confirmed stream tip -> SEQ_GAP
        ok_no_tip, msg_no_tip = verify_offline_audit_chain(recs[1:], ring=ring)
        self.assertFalse(ok_no_tip)
        self.assertIn("SEQ_GAP", msg_no_tip)

        # Starting at seq=2 WITH confirmed stream tip -> PASS
        ok_with_tip, tip_reason = verify_offline_audit_chain(
            recs[1:],
            ring=ring,
            stream_tips={"offline-agent": (1, recs[0]["entry_hash"])},
        )
        self.assertTrue(ok_with_tip, tip_reason)

        # Wrong tip hash -> CHAIN_FORK
        ok_bad_tip, msg_bad_tip = verify_offline_audit_chain(
            recs[1:],
            ring=ring,
            stream_tips={"offline-agent": (1, "f" * 64)},
        )
        self.assertFalse(ok_bad_tip)
        self.assertIn("CHAIN_FORK", msg_bad_tip)

        # Middle sequence gap [u1, u3] -> SEQ_GAP
        ok_mid_gap, msg_mid_gap = verify_offline_audit_chain([recs[0], recs[2]], ring=ring)
        self.assertFalse(ok_mid_gap)
        self.assertIn("SEQ_GAP", msg_mid_gap)

        # Unsigned/zeroed downgrade record -> rejected
        downgraded = dict(recs[0])
        downgraded["offline_seq"] = 0
        downgraded["entry_hash"] = ""
        downgraded["hmac_signature"] = ""
        ok_down, _ = verify_offline_audit_chain([downgraded], ring=ring)
        self.assertFalse(ok_down)

    async def test_sync_quarantines_duplicate_id_conflict_and_fails_closed_without_key_or_tip(self):
        _, u1 = self.agent._record_offline_audit_strict("home-web", "emergency_restart", 1, True, operation_id="op-1", run_id="r-1")
        _, u2 = self.agent._record_offline_audit_strict("home-web", "emergency_restart", 1, True, operation_id="op-2", run_id="r-2")
        _, u3 = self.agent._record_offline_audit_strict("home-web", "emergency_restart", 1, True, operation_id="op-3", run_id="r-3")
        _, u4 = self.agent._record_offline_audit_strict("home-web", "emergency_restart", 1, True, operation_id="op-4", run_id="r-4")

        def _resp_with(obj):
            m = MagicMock()
            m.status = 200
            m.read.return_value = json.dumps(obj).encode("utf-8")
            m.__enter__.return_value = m
            m.__exit__.return_value = False
            return m

        health_resp = _resp_with({"status": "ok"})

        # 1. Missing HMAC key must fail closed WITHOUT falsely quarantining queued signed records
        saved_key = os.environ.pop("OPS_AUDIT_HMAC_KEY", None)
        saved_keys = os.environ.pop("OPS_AUDIT_HMAC_KEYS", None)
        try:
            with patch("urllib.request.urlopen", return_value=health_resp):
                cnt_no_key = await self.agent.sync_offline_audit_logs()
                self.assertEqual(cnt_no_key, 0)
            conn = sqlite3.connect(self.db_path)
            unsynced_cnt = conn.execute("SELECT COUNT(*) FROM offline_ops_audit WHERE synced = 0").fetchone()[0]
            conn.close()
            self.assertEqual(unsynced_cnt, 4)
        finally:
            if saved_key is not None:
                os.environ["OPS_AUDIT_HMAC_KEY"] = saved_key
            if saved_keys is not None:
                os.environ["OPS_AUDIT_HMAC_KEYS"] = saved_keys

        # 2. First sync: u1 accepted, u2 permanently rejected with Go error_code=DUPLICATE_ID_CONFLICT,
        #    u3 transiently rejected with error_code=RATE_LIMITED -> u1=1, u2=-1, u3=0, u4=0
        resp_batch1 = _resp_with({
            "data": {
                "accepted_uuids": [u1],
                "rejected": [
                    {"event_uuid": u2, "error_code": "DUPLICATE_ID_CONFLICT", "reason": "replay hash mismatch"},
                    {"event_uuid": u3, "error_code": "RATE_LIMITED", "reason": "retry later"},
                ],
            }
        })
        # Force u4 to stay synced=0 on batch 1 by keeping it out of accepted/rejected above
        with patch("urllib.request.urlopen", side_effect=[health_resp, resp_batch1]):
            cnt1 = await self.agent.sync_offline_audit_logs()
            self.assertEqual(cnt1, 1)

        conn = sqlite3.connect(self.db_path)
        st_map = {
            r[0]: (r[1], r[2])
            for r in conn.execute("SELECT event_uuid, synced, audit_status FROM offline_ops_audit").fetchall()
        }
        conn.close()
        self.assertEqual(st_map[u1][0], 1)
        self.assertEqual(st_map[u2], (-1, "AUDIT_REJECTED"))
        self.assertEqual(st_map[u3][0], 0)
        self.assertEqual(st_map[u4][0], 0)

        # 3. Second sync: u2 quarantine must NOT stall u3, u4 (using confirmed SQLite stream tips)
        resp_batch2 = _resp_with({
            "data": {
                "accepted_uuids": [u3, u4],
                "rejected": [],
            }
        })
        with patch("urllib.request.urlopen", side_effect=[health_resp, resp_batch2]):
            cnt2 = await self.agent.sync_offline_audit_logs()
            self.assertEqual(cnt2, 2)

        conn = sqlite3.connect(self.db_path)
        st_map2 = {
            r[0]: r[1]
            for r in conn.execute("SELECT event_uuid, synced FROM offline_ops_audit").fetchall()
        }
        conn.close()
        self.assertEqual(st_map2[u3], 1)
        self.assertEqual(st_map2[u4], 1)

        # 4. Deleted predecessor row -> SEQ_GAP blocks sync of orphaned record
        self.agent._record_offline_audit_strict("home-web", "emergency_restart", 1, True, operation_id="op-5", run_id="r-5")
        _, u6 = self.agent._record_offline_audit_strict("home-web", "emergency_restart", 1, True, operation_id="op-6", run_id="r-6")
        conn = sqlite3.connect(self.db_path)
        conn.execute("DELETE FROM offline_ops_audit WHERE operation_id = 'op-5'")
        conn.commit()
        conn.close()
        with patch("urllib.request.urlopen", return_value=health_resp):
            cnt_gap = await self.agent.sync_offline_audit_logs()
            self.assertEqual(cnt_gap, 0)
        conn = sqlite3.connect(self.db_path)
        row_u6 = conn.execute("SELECT synced FROM offline_ops_audit WHERE event_uuid = ?", (u6,)).fetchone()
        conn.close()
        self.assertEqual(row_u6[0], -1)

    async def test_production_baseline_v1_rejects_legacy_sha256_and_empty_name(self):
        legacy_sha = "sha256:" + ("a" * 64)
        v1_bh = compute_container_baseline_from_inspect("home-web", _sample_v1_inspect())
        self.assertTrue(is_real_baseline_hash_v1(v1_bh, "home-web"))

        # Non-prod allows legacy sha256 for backward compatibility
        with patch.dict(os.environ, {"ENVIRONMENT": "development"}):
            self.assertTrue(is_real_baseline_hash(legacy_sha, "home-web"))
            self.assertTrue(is_real_baseline_hash(v1_bh, "home-web"))

        # Production strictly rejects legacy sha256 and requires v1:<64hex>
        with patch.dict(os.environ, {"ENVIRONMENT": "production"}):
            self.assertFalse(is_real_baseline_hash(legacy_sha, "home-web"))
            self.assertTrue(is_real_baseline_hash(v1_bh, "home-web"))

            mgr = ProposalLifecycleManager(self.db_path, baseline_verifier=lambda _c: v1_bh)
            with self.assertRaises(ValueError):
                mgr.create_proposal(
                    "prop-legacy-prod",
                    "Legacy",
                    "HIGH",
                    "restart",
                    "logs",
                    target_component="home-web",
                    baseline_hash=legacy_sha,
                )

            ok_v, _, reason_v = await self.agent._verify_trusted_baseline(
                "home-web",
                legacy_sha,
                self.db_path,
                self.config.docker_socket,
            )
            self.assertFalse(ok_v)
            self.assertIn("invalid or placeholder", reason_v)

        # Empty inspect Name when /containers/json?all=1 succeeds must fail closed
        async def mock_docker_empty_name(reader, writer):
            raw = (await reader.read(4096)).decode("utf-8", errors="ignore")
            req_line = raw.splitlines()[0] if raw else ""
            path = req_line.split()[1] if len(req_line.split()) >= 2 else "/"
            if path.startswith("/containers/json"):
                body = json.dumps([{"Id": "1" * 64, "Names": ["/home-web"]}]).encode("utf-8")
            elif path.startswith("/containers/home-web/json"):
                ins = _sample_v1_inspect("")
                ins["Name"] = ""
                body = json.dumps(ins).encode("utf-8")
            else:
                body = json.dumps({}).encode("utf-8")
            writer.write(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n" + body)
            await writer.drain()
            writer.close()
            await writer.wait_closed()

        srv = await asyncio.start_server(mock_docker_empty_name, "127.0.0.1", 0)
        port = srv.sockets[0].getsockname()[1]
        try:
            self.assertEqual(
                await self.agent._compute_container_baseline_hash("home-web", f"tcp://127.0.0.1:{port}"),
                "",
            )
        finally:
            srv.close()
            await srv.wait_closed()

    async def test_r6_lease_loss_aborts_inflight_docker_socket_and_records_execution_unknown(self):
        os.environ["OPS_LEASE_TTL_SEC"] = "6.0"
        os.environ["OPS_LEASE_RENEW_INTERVAL_SEC"] = "0.06"
        request_received = asyncio.Event()
        socket_closed_by_client = asyncio.Event()

        async def stalled_docker_restart(reader, writer):
            try:
                data = await reader.read(4096)
                if b"POST /containers/home-web/restart" in data:
                    request_received.set()
                    # Hold connection open without replying; when AsyncLeaseGuard steals lease and
                    # cancels _restart_docker_container, client must abort socket promptly (<1s).
                    while True:
                        chunk = await reader.read(64)
                        if not chunk:
                            socket_closed_by_client.set()
                            break
            except Exception:
                socket_closed_by_client.set()
            finally:
                try:
                    writer.close()
                except Exception:
                    pass

        srv = await asyncio.start_server(stalled_docker_restart, "127.0.0.1", 0)
        port = srv.sockets[0].getsockname()[1]
        sock = f"tcp://127.0.0.1:{port}"

        async def _steal_lease_once_sent():
            await request_received.wait()
            conn = sqlite3.connect(self.db_path)
            conn.execute("UPDATE ops_recovery_leases SET lease_token = 'stolen-r11-peer' WHERE component = 'home-web'")
            conn.commit()
            conn.close()

        stealer = asyncio.create_task(_steal_lease_once_sent())
        try:
            started = time.monotonic()
            with patch.object(self.agent, "_verify_trusted_baseline", return_value=(True, "v1:" + ("4" * 64), "ok")):
                ok = await self.agent._handle_offline_emergency_restart("home-web", 137, sock)
            elapsed = time.monotonic() - started

            self.assertFalse(ok)
            self.assertLess(elapsed, 2.0, "Lease cancellation must abort Docker socket promptly instead of waiting 15s")
            await asyncio.wait_for(socket_closed_by_client.wait(), timeout=1.0)
            self.assertIn("home-web", self.agent._ambiguous_operations)

            conn = sqlite3.connect(self.db_path)
            last_ev = conn.execute("SELECT event_type FROM offline_ops_audit ORDER BY id DESC LIMIT 1").fetchone()[0]
            conn.close()
            self.assertEqual(last_ev, EXECUTION_UNKNOWN)
        finally:
            if not stealer.done():
                stealer.cancel()
            srv.close()
            await srv.wait_closed()


if __name__ == "__main__":
    unittest.main()
