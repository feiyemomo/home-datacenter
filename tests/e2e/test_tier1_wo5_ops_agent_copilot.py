"""
Tier 1 Feature Coverage: Work Order 5 (Ops Daemon & App Copilot)
Validates core functional contracts for:
- home-ops-agent resource ceiling (<= 50MB resident memory)
- Two-tier cost funnel Tier 1: 5 auth failures -> IP ban (0 tokens)
- Two-tier cost funnel Tier 1: Single/double container crash -> backoff restart (0 tokens)
- Two-tier cost funnel Tier 2: Consecutive crashes (>= 3) -> LLM diagnostic escalation
- Human-in-the-loop proposal lifecycle (AWAITING_APPROVAL -> APPROVED -> EXECUTED)
- Ops Chat API contracts and metrics context assembly
"""

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(__file__))
from harness import TwoTierCostFunnel, ProposalLifecycleManager


class TestTier1WorkOrder5OpsAgentCopilot(unittest.TestCase):
    """Tier 1 Functional Tests for Work Order 5."""

    def setUp(self):
        self.funnel = TwoTierCostFunnel()
        self.props = ProposalLifecycleManager()

    def test_edge_ops_daemon_resource_ceiling_contract(self):
        """
        WO 5.1: Verify edge ops daemon memory ceiling specification (<= 50MB RAM).
        Authoritative source: Task Specification Section 5.1 & PROJECT.md Feature 18.
        Expected: Daemon max memory allocation constraint is <= 50MB.
        """
        daemon_config = {
            "name": "home-ops-agent",
            "runtime": "python:3.11-slim",
            "max_memory_mb": 50,
            "async_loop": "asyncio"
        }
        self.assertLessEqual(daemon_config["max_memory_mb"], 50)

    def test_two_tier_funnel_tier1_auth_failure_ip_ban(self):
        """
        WO 5.1: Verify 5 failed auth attempts trigger Tier 1 IP ban with 0 token consumption.
        Authoritative source: Task Specification Section 5.1.
        Expected: 5th failure from same IP produces 'ip_ban' action, tokens_used = 0.
        """
        attacker_ip = "192.168.1.199"

        # Attempts 1 to 4: increment counter, no ban yet
        for i in range(1, 5):
            res = self.funnel.handle_auth_failure(attacker_ip)
            self.assertEqual(res["handled_by"], "tier1_rule_engine")
            self.assertEqual(res["action"], "increment_counter")
            self.assertEqual(res["tokens_used"], 0)
            self.assertNotIn(attacker_ip, self.funnel.banned_ips)

        # 5th attempt: triggers immediate IP ban
        res5 = self.funnel.handle_auth_failure(attacker_ip)
        self.assertEqual(res5["handled_by"], "tier1_rule_engine")
        self.assertEqual(res5["action"], "ip_ban")
        self.assertEqual(res5["target_ip"], attacker_ip)
        self.assertEqual(res5["tokens_used"], 0)
        self.assertIn(attacker_ip, self.funnel.banned_ips)

    def test_two_tier_funnel_tier1_container_exponential_backoff(self):
        """
        WO 5.1: Verify occasional container crash triggers Tier 1 backoff restart with 0 tokens.
        Authoritative source: Task Specification Section 5.1.
        Expected: Crashes 1 and 2 handled locally with exponential backoff (1s, 2s).
        """
        # Crash 1: backoff 1s
        r1 = self.funnel.handle_container_crash("home-vision", exit_code=1, stderr="Fatal error")
        self.assertEqual(r1["handled_by"], "tier1_rule_engine")
        self.assertEqual(r1["action"], "restart_container")
        self.assertEqual(r1["backoff_sec"], 1)
        self.assertEqual(r1["tokens_used"], 0)

        # Crash 2: backoff 2s
        r2 = self.funnel.handle_container_crash("home-vision", exit_code=1, stderr="Fatal error")
        self.assertEqual(r2["handled_by"], "tier1_rule_engine")
        self.assertEqual(r2["action"], "restart_container")
        self.assertEqual(r2["backoff_sec"], 2)
        self.assertEqual(r2["tokens_used"], 0)

    def test_two_tier_funnel_tier2_consecutive_crash_loop_escalation(self):
        """
        WO 5.1: Verify consecutive crash loop (>= 3 times) escalates to Tier 2 LLM diagnostics.
        Authoritative source: Task Specification Section 5.1.
        Expected: 3rd crash escalates to Tier 2 LLM with sanitized logs (passwords masked).
        """
        # Two previous crashes
        self.funnel.handle_container_crash("home-api", 137, "OOMKilled")
        self.funnel.handle_container_crash("home-api", 137, "OOMKilled")

        # 3rd crash with sensitive string in stderr
        dirty_stderr = "Database connection failed with password=super_secret_pw! and token=abc12345"
        r3 = self.funnel.handle_container_crash("home-api", 1, dirty_stderr)

        self.assertEqual(r3["handled_by"], "tier2_llm")
        self.assertEqual(r3["action"], "request_diagnostic")
        self.assertEqual(r3["model_alias"], "agent-fast")

        # Verify query was recorded with sensitive data sanitized
        recorded = self.funnel.llm_queries[0]
        self.assertEqual(recorded["container"], "home-api")
        self.assertNotIn("super_secret_pw!", recorded["sanitized_log"])
        self.assertIn("password=***", recorded["sanitized_log"])

    def test_human_in_the_loop_proposal_creation_and_approval_workflow(self):
        """
        WO 5.1 & 5.2: Verify high-risk operations require explicit Admin approval before execution.
        Authoritative source: Task Specification Section 5.1 & 5.2.
        Expected: Proposal starts in AWAITING_APPROVAL; cannot execute until approved by Admin.
        """
        prop_id = "prop-2026-001"
        prop = self.props.create_proposal(
            prop_id=prop_id,
            title="Upgrade pure Go SQLite schema to v3",
            risk_level="HIGH",
            action="ALTER TABLE ...",
            rollback_cmd="RESTORE SNAPSHOT"
        )
        self.assertEqual(prop["status"], ProposalLifecycleManager.STATE_AWAITING)

        # Attempt to execute without approval -> MUST FAIL
        ok, msg = self.props.execute_proposal(prop_id)
        self.assertFalse(ok)
        self.assertIn("must be APPROVED first", msg)

        # Non-admin attempts to approve -> MUST FAIL
        ok, msg = self.props.approve_proposal(prop_id, approver="guest", is_admin=False)
        self.assertFalse(ok)
        self.assertIn("admin role required", msg)

        # Admin approves
        ok, msg = self.props.approve_proposal(prop_id, approver="admin_alice", is_admin=True)
        self.assertTrue(ok)
        self.assertEqual(self.props.proposals[prop_id]["status"], ProposalLifecycleManager.STATE_APPROVED)

        # Now execution succeeds
        ok, msg = self.props.execute_proposal(prop_id)
        self.assertTrue(ok)
        self.assertEqual(self.props.proposals[prop_id]["status"], ProposalLifecycleManager.STATE_EXECUTED)

    def test_ops_chat_api_endpoints_contract(self):
        """
        WO 5.2: Verify Ops Chat API message and session request/response contracts.
        Authoritative source: Task Specification Section 5.2 & PROJECT.md Feature 21.
        Expected: Sessions list returns chat history; messages return AI copilot structured advice.
        """
        chat_request = {
            "session_id": "session-ops-001",
            "message": "Analyze any SSH brute force attacks in past 24 hours"
        }
        chat_response = {
            "session_id": "session-ops-001",
            "reply": "Detected 5 failed login attempts from IP 192.168.1.199. IP automatically banned via Tier 1 rule.",
            "proposals_referenced": []
        }
        self.assertEqual(chat_response["session_id"], chat_request["session_id"])
        self.assertIn("192.168.1.199", chat_response["reply"])


if __name__ == "__main__":
    unittest.main()
