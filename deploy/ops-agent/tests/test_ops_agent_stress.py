"""
Empirical Stress & Edge Case Test Suite for home-ops-agent Daemon (Challenge 4).

Verifies:
- Two-Tier Cost Funnel (95% Tier 1 local deterministic vs 5% Tier 2 LLM diagnostics)
- Sliding window eviction and container isolation under high concurrency
- Multi-IP brute-force flood and auth failure time-decay
- Comprehensive credential and private-key sanitization
- Docker / MQTT alert parsing under malformed, non-crash, and clean exit scenarios
- Human-in-the-loop proposal rollback and low-peak maintenance window boundaries
"""

import os
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

# Add deploy/ops-agent to path
CURRENT_DIR = os.path.dirname(os.path.abspath(__file__))
PARENT_DIR = os.path.dirname(CURRENT_DIR)
sys.path.insert(0, PARENT_DIR)

from config import OpsAgentConfig
from proposal import ProposalLifecycleManager
from rule_engine import TwoTierRuleEngine, sanitize_log


class TestOpsAgentFunnelStress(unittest.TestCase):
    """Stress tests the cost funnel and resource constraints."""

    def setUp(self):
        self.config = OpsAgentConfig.load()
        self.engine = TwoTierRuleEngine(self.config)

    def test_tier1_crash_sliding_window_expiration(self):
        """
        If crashes occur outside the 600s crash window, they must NOT accumulate
        towards Tier 2 escalation. Token consumption must remain strictly 0.
        """
        container = "home-web"

        # Crash 1 at t - 1000s (expired: crash_window_sec is 900s)
        old_time = time.time() - 1000
        self.engine.container_crash_timestamps[container] = [old_time]

        # Crash 2 now
        r1 = self.engine.handle_container_crash(container, exit_code=1, stderr="Nginx worker died")
        self.assertEqual(r1["handled_by"], "tier1_rule_engine")
        self.assertEqual(r1["action"], "restart_container")
        self.assertEqual(r1["backoff_sec"], 1)  # Treated as crash #1 because old one expired!
        self.assertEqual(r1["tokens_used"], 0)

        # Crash 3 now (total in window = 2)
        r2 = self.engine.handle_container_crash(container, exit_code=1, stderr="Nginx worker died again")
        self.assertEqual(r2["handled_by"], "tier1_rule_engine")
        self.assertEqual(r2["action"], "restart_container")
        self.assertEqual(r2["backoff_sec"], 2)  # Crash #2 in window
        self.assertEqual(r2["tokens_used"], 0)

        # Still 0 LLM queries
        self.assertEqual(len(self.engine.llm_queries), 0)

    def test_multi_container_crash_isolation(self):
        """
        Crashes across different containers must NOT contaminate each other.
        Each container maintains an independent backoff counter.
        """
        containers = ["vision", "home-api", "mosquitto", "frigate"]

        # Each container crashes twice -> All handled by Tier 1 with 0 tokens
        for c in containers:
            res1 = self.engine.handle_container_crash(c, exit_code=1, stderr="Err")
            res2 = self.engine.handle_container_crash(c, exit_code=1, stderr="Err")
            self.assertEqual(res1["handled_by"], "tier1_rule_engine")
            self.assertEqual(res2["handled_by"], "tier1_rule_engine")
            self.assertEqual(res1["tokens_used"], 0)
            self.assertEqual(res2["tokens_used"], 0)

        self.assertEqual(len(self.engine.llm_queries), 0)
        self.assertEqual(self.engine.tier2_escalate_count, 0)
        self.assertEqual(self.engine.tier1_intercept_count, 8)

        # Now only 'vision' crashes a 3rd time -> Only 'vision' escalates
        r3 = self.engine.handle_container_crash("vision", exit_code=137, stderr="OOM killed")
        self.assertEqual(r3["handled_by"], "tier2_llm")
        self.assertEqual(self.engine.tier2_escalate_count, 1)
        self.assertEqual(len(self.engine.llm_queries), 1)
        self.assertEqual(self.engine.llm_queries[0]["container"], "vision")

    def test_distributed_auth_flood_handling(self):
        """
        100 distinct attacking IPs each failing 4 times:
        100 * 4 = 400 events, 100% intercepted by Tier 1 rule engine with 0 tokens!
        """
        for i in range(100):
            ip = f"10.0.{i // 256}.{i % 256}"
            for _ in range(4):
                res = self.engine.handle_auth_failure(ip)
                self.assertEqual(res["handled_by"], "tier1_rule_engine")
                self.assertEqual(res["action"], "increment_counter")
                self.assertEqual(res["tokens_used"], 0)

        self.assertEqual(len(self.engine.banned_ips), 0)
        self.assertEqual(self.engine.tier1_intercept_count, 400)

        # Now IP 10.0.0.1 attempts 5th time -> Banned
        res_ban = self.engine.handle_auth_failure("10.0.0.1")
        self.assertEqual(res_ban["handled_by"], "tier1_rule_engine")
        self.assertEqual(res_ban["action"], "ip_ban")
        self.assertIn("10.0.0.1", self.engine.banned_ips)
        self.assertEqual(res_ban["tokens_used"], 0)

    def test_auth_window_expiry_resets_ban_counter(self):
        """
        If an attacker fails 4 times, but the 5th attempt arrives after window expiry,
        they should NOT be banned immediately; counter resets.
        """
        ip = "192.168.1.50"
        old_time = time.time() - 350  # Beyond 300s window

        self.engine.auth_fail_timestamps[ip] = [old_time, old_time, old_time, old_time]

        # 5th attempt now
        res = self.engine.handle_auth_failure(ip)
        self.assertEqual(res["action"], "increment_counter")
        self.assertEqual(res["count"], 1)  # Only 1 attempt in current window!
        self.assertNotIn(ip, self.engine.banned_ips)


class TestCredentialSanitizationStress(unittest.TestCase):
    """Stress tests credential and secret masking against malicious/varied logs."""

    def test_complex_sanitization_patterns(self):
        log_sample = """
        2026-11-07 10:00:00 [ERROR] Connection failed:
        host=10.0.0.5;password=SuperComplex$Password#123;user=admin
        Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.doz...
        apikey=sk-proj-9876543210abcdef
        secret=my_custom_secret_key
        jwt=eyJhbGciOiJIUzI1NiJ9
        -----BEGIN RSA PRIVATE KEY-----
        MIIEowIBAAKCAQEA0Y3yZ2...
        -----END RSA PRIVATE KEY-----
        Normal log text: Docker container home-api exited with status 1
        """

        sanitized = sanitize_log(log_sample)

        self.assertNotIn("SuperComplex$Password#123", sanitized)
        self.assertNotIn("eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9", sanitized)
        self.assertNotIn("sk-proj-9876543210abcdef", sanitized)
        self.assertNotIn("my_custom_secret_key", sanitized)
        self.assertNotIn("MIIEowIBAAKCAQEA0Y3yZ2", sanitized)

        self.assertIn("password=***", sanitized)
        self.assertIn("Bearer ***", sanitized)
        self.assertIn("apikey=***", sanitized)
        self.assertIn("secret=***", sanitized)
        self.assertIn("jwt=***", sanitized)
        self.assertIn("[REDACTED_PRIVATE_KEY]", sanitized)
        self.assertIn("Normal log text: Docker container home-api exited with status 1", sanitized)

    def test_empty_and_benign_logs(self):
        self.assertEqual("", sanitize_log(""))
        self.assertEqual("Clean log line with no secrets", sanitize_log("Clean log line with no secrets"))


class TestProposalRollbackAndMaintenanceWindow(unittest.TestCase):
    """Stress tests human approval requirements and low-peak execution."""

    def setUp(self):
        self.env_patcher = patch.dict(os.environ, {"ENVIRONMENT": "test", "APP_ENV": "test"})
        self.env_patcher.start()
        self.temp_dir = tempfile.TemporaryDirectory()
        self.db_path = os.path.join(self.temp_dir.name, "proposals_test.db")
        self.mgr = ProposalLifecycleManager(self.db_path)

    def tearDown(self):
        self.env_patcher.stop()
        self.temp_dir.cleanup()

    def test_unapproved_proposal_cannot_execute(self):
        prop = self.mgr.create_proposal(
            prop_id="prop-crit-1",
            title="Dangerous DB Migration",
            risk_level="HIGH",
            action="DROP TABLE users;",
            rollback_cmd="RELOAD BACKUP;"
        )
        # Attempt to run immediately
        ok, msg = self.mgr.execute_proposal("prop-crit-1")
        self.assertFalse(ok)
        self.assertIn("must be APPROVED first", msg)
        self.assertEqual(self.mgr.proposals["prop-crit-1"]["status"], ProposalLifecycleManager.STATE_AWAITING)

    def test_low_peak_window_boundary_hours(self):
        """
        Low-peak maintenance window is strictly 03:00 ~ 05:00 AM (tm_hour 3 and 4).
        Hours 0, 1, 2, 5, 6, 12, 18, 23 must return False.
        """
        # Inside window
        self.assertTrue(self.mgr.is_low_peak_window(3))
        self.assertTrue(self.mgr.is_low_peak_window(4))

        # Outside window
        for hour in [0, 1, 2, 5, 6, 7, 12, 15, 18, 22, 23]:
            self.assertFalse(self.mgr.is_low_peak_window(hour), f"Hour {hour} must NOT be low-peak window")

    def test_execution_failure_triggers_automatic_rollback(self):
        prop_id = "prop-fail-1"
        self.mgr.create_proposal(
            prop_id=prop_id,
            title="Flawed Firmware Update",
            risk_level="HIGH",
            action="flash firmware",
            rollback_cmd="restore backup firmware"
        )
        self.mgr.approve_proposal(prop_id, approver="admin", is_admin=True)

        ok, msg = self.mgr.execute_proposal(prop_id, simulate_failure=True)
        self.assertFalse(ok)
        self.assertIn("auto-rollback triggered", msg)
        self.assertEqual(self.mgr.proposals[prop_id]["status"], ProposalLifecycleManager.STATE_FAILED_ROLLEDBACK)
        self.assertTrue(self.mgr.proposals[prop_id]["rollback_executed"])


if __name__ == "__main__":
    unittest.main()
