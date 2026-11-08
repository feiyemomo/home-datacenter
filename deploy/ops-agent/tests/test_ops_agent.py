"""
Unit test suite for home-ops-agent daemon, two-tier cost funnel, and proposal lifecycle.
"""

import os
import sys
import tempfile
import unittest

# Ensure deploy/ops-agent is on path
CURRENT_DIR = os.path.dirname(os.path.abspath(__file__))
PARENT_DIR = os.path.dirname(CURRENT_DIR)
sys.path.insert(0, PARENT_DIR)

from config import OpsAgentConfig
from proposal import ProposalLifecycleManager
from rule_engine import TwoTierRuleEngine, sanitize_log
from main import HomeOpsAgent


class TestOpsAgentConfig(unittest.TestCase):
    """Verifies resource constraint and configuration contracts."""

    def test_memory_ceiling_constraint(self):
        """WO 5.1: Daemon resident memory limit constraint must be <= 50MB."""
        cfg = OpsAgentConfig.load()
        self.assertLessEqual(cfg.max_memory_mb, 50)
        self.assertEqual(cfg.name, "home-ops-agent")
        self.assertEqual(cfg.runtime, "python:3.11-slim")
        self.assertEqual(cfg.async_loop, "asyncio")


class TestRuleEngineSanitization(unittest.TestCase):
    """Tests log sanitization functionality."""

    def test_sanitize_log_removes_sensitive_data(self):
        dirty = "Error: password=my_secret_pw! token=xyz123 secret=very_secret Bearer eyJhbGciOiJIUzI1NiI"
        clean = sanitize_log(dirty)

        self.assertNotIn("my_secret_pw!", clean)
        self.assertNotIn("xyz123", clean)
        self.assertNotIn("very_secret", clean)
        self.assertIn("password=***", clean)
        self.assertIn("token=***", clean)
        self.assertIn("secret=***", clean)
        self.assertIn("Bearer ***", clean)


class TestTwoTierRuleEngine(unittest.TestCase):
    """Tests the Two-Tier Cost Funnel (95% Tier 1 local, 5% Tier 2 LLM)."""

    def setUp(self):
        self.engine = TwoTierRuleEngine()

    def test_tier1_auth_failure_counter_and_ban(self):
        attacker_ip = "192.168.1.199"

        # Attempts 1 to 4: increment counter, 0 tokens, not banned
        for i in range(1, 5):
            res = self.engine.handle_auth_failure(attacker_ip)
            self.assertEqual(res["handled_by"], "tier1_rule_engine")
            self.assertEqual(res["action"], "increment_counter")
            self.assertEqual(res["tokens_used"], 0)
            self.assertNotIn(attacker_ip, self.engine.banned_ips)

        # 5th attempt: immediate ban
        res5 = self.engine.handle_auth_failure(attacker_ip)
        self.assertEqual(res5["handled_by"], "tier1_rule_engine")
        self.assertEqual(res5["action"], "ip_ban")
        self.assertEqual(res5["target_ip"], attacker_ip)
        self.assertEqual(res5["tokens_used"], 0)
        self.assertIn(attacker_ip, self.engine.banned_ips)

    def test_tier1_container_exponential_backoff(self):
        # 1st crash: backoff 1s, 0 tokens
        r1 = self.engine.handle_container_crash("home-vision", exit_code=1, stderr="Error")
        self.assertEqual(r1["handled_by"], "tier1_rule_engine")
        self.assertEqual(r1["action"], "restart_container")
        self.assertEqual(r1["backoff_sec"], 1)
        self.assertEqual(r1["tokens_used"], 0)

        # 2nd crash: backoff 2s, 0 tokens
        r2 = self.engine.handle_container_crash("home-vision", exit_code=1, stderr="Error")
        self.assertEqual(r2["handled_by"], "tier1_rule_engine")
        self.assertEqual(r2["action"], "restart_container")
        self.assertEqual(r2["backoff_sec"], 2)
        self.assertEqual(r2["tokens_used"], 0)

    def test_tier2_consecutive_crash_escalation(self):
        # 2 crashes first
        self.engine.handle_container_crash("home-api", 137, "OOM")
        self.engine.handle_container_crash("home-api", 137, "OOM")

        # 3rd crash with sensitive information in log
        dirty_log = "Database connection failed with password=super_secret_pw! and token=abc12345"
        r3 = self.engine.handle_container_crash("home-api", 1, dirty_log)

        self.assertEqual(r3["handled_by"], "tier2_llm")
        self.assertEqual(r3["action"], "request_diagnostic")
        self.assertEqual(r3["model_alias"], "agent-fast")

        # Verify query recorded with sanitized log
        recorded = self.engine.llm_queries[0]
        self.assertEqual(recorded["container"], "home-api")
        self.assertNotIn("super_secret_pw!", recorded["sanitized_log"])
        self.assertIn("password=***", recorded["sanitized_log"])

    def test_tier1_disk_threshold_and_cache_purging(self):
        # Nominal usage (75%) -> no purge
        r_nominal = self.engine.handle_disk_usage(75.0)
        self.assertEqual(r_nominal["handled_by"], "tier1_rule_engine")
        self.assertEqual(r_nominal["action"], "status_nominal")
        self.assertEqual(r_nominal["tokens_used"], 0)

        # High usage (92%) -> trigger cache purge
        r_purge = self.engine.handle_disk_usage(92.0)
        self.assertEqual(r_purge["handled_by"], "tier1_rule_engine")
        self.assertEqual(r_purge["action"], "purge_temp_cache")
        self.assertEqual(r_purge["tokens_used"], 0)


class TestProposalLifecycleManager(unittest.TestCase):
    """Tests the Human-in-the-Loop proposal lifecycle."""

    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory()
        self.db_path = os.path.join(self.temp_dir.name, "test_app.db")
        self.mgr = ProposalLifecycleManager(self.db_path)

    def tearDown(self):
        self.temp_dir.cleanup()

    def test_proposal_lifecycle_workflow(self):
        prop_id = "prop-2026-001"
        prop = self.mgr.create_proposal(
            prop_id=prop_id,
            title="Upgrade pure Go SQLite schema to v3",
            risk_level="HIGH",
            action="ALTER TABLE ...",
            rollback_cmd="RESTORE SNAPSHOT"
        )
        self.assertEqual(prop["status"], ProposalLifecycleManager.STATE_AWAITING)

        # 1. Attempt execute without approval -> FAILS
        ok, msg = self.mgr.execute_proposal(prop_id)
        self.assertFalse(ok)
        self.assertIn("must be APPROVED first", msg)

        # 2. Non-admin attempts approval -> FAILS
        ok, msg = self.mgr.approve_proposal(prop_id, approver="guest", is_admin=False)
        self.assertFalse(ok)
        self.assertIn("admin role required", msg)

        # 3. Admin approves -> SUCCEEDS
        ok, msg = self.mgr.approve_proposal(prop_id, approver="admin_alice", is_admin=True)
        self.assertTrue(ok)
        self.assertEqual(self.mgr.proposals[prop_id]["status"], ProposalLifecycleManager.STATE_APPROVED)

        # 4. Now execution succeeds
        ok, msg = self.mgr.execute_proposal(prop_id)
        self.assertTrue(ok)
        self.assertEqual(self.mgr.proposals[prop_id]["status"], ProposalLifecycleManager.STATE_EXECUTED)

    def test_proposal_failure_auto_rollback(self):
        prop_id = "prop-2026-002"
        self.mgr.create_proposal(
            prop_id=prop_id,
            title="Dangerous Kernel Module Reload",
            risk_level="HIGH",
            action="modprobe broken_module",
            rollback_cmd="modprobe -r broken_module"
        )
        self.mgr.approve_proposal(prop_id, approver="admin", is_admin=True)

        # Simulate execution failure
        ok, msg = self.mgr.execute_proposal(prop_id, simulate_failure=True)
        self.assertFalse(ok)
        self.assertIn("auto-rollback triggered", msg)
        self.assertEqual(self.mgr.proposals[prop_id]["status"], ProposalLifecycleManager.STATE_FAILED_ROLLEDBACK)
        self.assertTrue(self.mgr.proposals[prop_id]["rollback_executed"])

    def test_low_peak_window_scheduling(self):
        prop_id = "prop-2026-003"
        self.mgr.create_proposal(
            prop_id=prop_id,
            title="Base Image Alpine Upgrade",
            risk_level="HIGH",
            action="docker-compose pull && docker-compose up -d",
            rollback_cmd="docker-compose rollback"
        )

        # Schedule for low-peak window
        ok, msg = self.mgr.approve_proposal(prop_id, approver="admin", is_admin=True, schedule_at="03:00")
        self.assertTrue(ok)
        self.assertEqual(self.mgr.proposals[prop_id]["status"], ProposalLifecycleManager.STATE_SCHEDULED)

        # Outside low-peak window (e.g. 14:00) -> not executed
        executed = self.mgr.check_and_run_scheduled_proposals(current_hour=14)
        self.assertEqual(len(executed), 0)
        self.assertEqual(self.mgr.proposals[prop_id]["status"], ProposalLifecycleManager.STATE_SCHEDULED)

        # Inside low-peak window (04:00 AM) -> auto executed
        executed = self.mgr.check_and_run_scheduled_proposals(current_hour=4)
        self.assertEqual(len(executed), 1)
        self.assertEqual(self.mgr.proposals[prop_id]["status"], ProposalLifecycleManager.STATE_EXECUTED)


class TestHomeOpsAgentDaemon(unittest.TestCase):
    """Tests the daemon initialization and lifecycle."""

    def test_agent_initialization(self):
        agent = HomeOpsAgent()
        self.assertEqual(agent.config.name, "home-ops-agent")
        self.assertFalse(agent.running)
        self.assertIsNotNone(agent.rule_engine)
        self.assertIsNotNone(agent.proposal_manager)


if __name__ == "__main__":
    unittest.main()
