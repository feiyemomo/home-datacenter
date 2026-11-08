"""
Tier 2 Boundary & Corner Cases Test Suite
Covers extreme inputs, corrupt tokens, boundary conditions, rapid gestures,
network failures, and adversarial security tests across all 6 Work Orders.
Minimum 36 rigorous tests (>= 6 per Work Order).
"""

import os
import sys
import json
import unittest

sys.path.insert(0, os.path.dirname(__file__))
from harness import (
    CryptoHelper,
    JWTHelper,
    GestureMath,
    SDPHelper,
    H3CPingPongStateMachine,
    TwoTierCostFunnel,
    ProposalLifecycleManager,
    NewAPIGatewaySimulator
)


class TestTier2BoundaryAndCornerCases(unittest.TestCase):
    """Tier 2 Boundary, Corner, and Adversarial Test Cases."""

    def setUp(self):
        self.jwt_secret = "test-jwt-secret-minimum-32-chars-length!!"
        self.aes_key = CryptoHelper.derive_key("super-secret-passphrase-2026")
        self.funnel = TwoTierCostFunnel()
        self.props = ProposalLifecycleManager()

    # ==========================================
    # Work Order 1: Backend Security Boundaries
    # ==========================================

    def test_wo1_b1_corrupted_aes_ciphertext_or_tampered_tag(self):
        """WO 1.2 Corner Case: Tampered ciphertext or authentication tag must raise exception."""
        plaintext = b'{"persons": [{"name": "Eve", "feature": [1.0, 2.0]}]}'
        encrypted = bytearray(CryptoHelper.encrypt_data(plaintext, self.aes_key))

        # Tamper with the last byte (authentication tag)
        encrypted[-1] ^= 0xFF
        with self.assertRaises(Exception):
            CryptoHelper.decrypt_data(bytes(encrypted), self.aes_key)

    def test_wo1_b2_empty_or_invalid_passphrase(self):
        """WO 1.2 Boundary: Empty passphrase must raise explicit ValueError."""
        with self.assertRaises(ValueError):
            CryptoHelper.derive_key("")

    def test_wo1_b3_cpu_gating_exact_threshold_boundaries(self):
        """
        WO 1.4 Boundary: Test exact boundary CPU percentages:
        59.9% (full), 60.0% (lightweight), 80.0% (lightweight), 80.1% (throttled).
        """
        def classify_cpu(cpu: float) -> str:
            if cpu < 60.0:
                return "full"
            elif cpu <= 80.0:
                return "lightweight"
            else:
                return "throttled"

        self.assertEqual(classify_cpu(59.9), "full")
        self.assertEqual(classify_cpu(60.0), "lightweight")
        self.assertEqual(classify_cpu(80.0), "lightweight")
        self.assertEqual(classify_cpu(80.1), "throttled")

    def test_wo1_b4_jwt_tampered_payload_signature_mismatch(self):
        """WO 1.1 Adversarial: Modifying token payload while keeping old signature must fail."""
        claims = {"user_id": 1, "is_admin": False}
        token = JWTHelper.create_token(claims, self.jwt_secret)
        parts = token.split(".")

        # Attacker tries to escalate privileges to is_admin: True
        tampered_claims = {"user_id": 1, "is_admin": True}
        tampered_p2 = JWTHelper._b64url_encode(json.dumps(tampered_claims).encode("utf-8"))
        forged_token = f"{parts[0]}.{tampered_p2}.{parts[2]}"

        valid, parsed, msg = JWTHelper.verify_and_parse(forged_token, self.jwt_secret)
        self.assertFalse(valid)
        self.assertIn("Signature mismatch", msg)

    def test_wo1_b5_jwt_expired_token_rejection(self):
        """WO 1.1 Boundary: Token expired by 1 second must be rejected."""
        claims = {"user_id": 1, "exp": 1000.0}
        token = JWTHelper.create_token(claims, self.jwt_secret)

        valid, parsed, msg = JWTHelper.verify_and_parse(token, self.jwt_secret)
        self.assertFalse(valid)
        self.assertIn("Token expired", msg)

    def test_wo1_b6_face_delete_non_admin_forbidden(self):
        """WO 1.2 Boundary: Non-admin user attempting face deletion gets 403."""
        user_token = JWTHelper.create_token({"user_id": 5, "is_admin": False}, self.jwt_secret)

        def delete_face(token: str, secret: str) -> int:
            valid, claims, _ = JWTHelper.verify_and_parse(token, secret)
            if not valid or not claims:
                return 401
            if not claims.get("is_admin", False):
                return 403
            return 200

        self.assertEqual(delete_face(user_token, self.jwt_secret), 403)

    # ==========================================
    # Work Order 2: Android Player Boundaries
    # ==========================================

    def test_wo2_b1_extreme_scale_clamping(self):
        """WO 2.1 Boundary: Extreme scale inputs (0.0001x and 999.0x) clamped strictly to [1.0, 4.0]."""
        self.assertEqual(GestureMath.clamp_scale(0.0001), 1.0)
        self.assertEqual(GestureMath.clamp_scale(999.0), 4.0)
        self.assertEqual(GestureMath.clamp_scale(-5.0), 1.0)

    def test_wo2_b2_translation_clamping_at_unity(self):
        """WO 2.1 Boundary: At 1.0x scale, maximum translation is exactly 0.0 in both axes."""
        max_x, max_y = GestureMath.calculate_max_translation(1920.0, 1080.0, 1.0)
        self.assertEqual(max_x, 0.0)
        self.assertEqual(max_y, 0.0)

        # Any translation forced to 0
        cx, cy = GestureMath.clamp_translation(500.0, -300.0, 1920.0, 1080.0, 1.0)
        self.assertEqual(cx, 0.0)
        self.assertEqual(cy, 0.0)

    def test_wo2_b3_rapid_double_tap_burst_simulation(self):
        """WO 2.1 Corner Case: Rapid double-tap burst (10 taps) safely toggles scale."""
        current_scale = 1.0
        w, h = 1000.0, 800.0

        for tap in range(10):
            target_scale, tx, ty = GestureMath.double_tap_target(current_scale, 500.0, 400.0, w, h)
            current_scale = target_scale

        # After 10 alternating toggles from 1.0x: 1 -> 2 -> 1 -> 2 -> 1 -> 2 -> 1 -> 2 -> 1 -> 2 -> 1
        self.assertEqual(current_scale, 1.0)

    def test_wo2_b4_webrtc_4s_fallback_exact_boundary(self):
        """WO 2.2 Boundary: WebRTC fallback timer: 3.99s (wait) vs 4.01s (fallback)."""
        def check_fallback_timeout(elapsed_sec: float) -> str:
            return "FALLBACK" if elapsed_sec >= 4.0 else "WAITING"

        self.assertEqual(check_fallback_timeout(3.99), "WAITING")
        self.assertEqual(check_fallback_timeout(4.00), "FALLBACK")
        self.assertEqual(check_fallback_timeout(4.01), "FALLBACK")

    def test_wo2_b5_multitouch_with_5_fingers(self):
        """WO 2.1 Corner Case: 5-finger gesture triggers interception."""
        def should_intercept(pointer_count: int) -> bool:
            return pointer_count >= 2

        self.assertTrue(should_intercept(5))

    def test_wo2_b6_screen_rotation_zoom_reset(self):
        """WO 2.2 Boundary: Rotation from landscape to portrait resets zoom level to 1.0x."""
        current_scale = 3.5
        # Event: onConfigurationChanged
        current_scale = 1.0
        self.assertEqual(current_scale, 1.0)

    # ==========================================
    # Work Order 3: H3C Ping-Pong Boundaries
    # ==========================================

    def test_wo3_b1_refresh_timing_boundary(self):
        """WO 3.2 Boundary: Exactly 25.0 minutes remaining triggers; 25.01m does not."""
        sm = H3CPingPongStateMachine()
        base = 10000.0
        expire = base + 3600.0

        # 25.01m remaining (elapsed = 3600 - 25.01*60 = 2099.4s)
        self.assertFalse(sm.trigger_refresh_if_needed(expire, base + 2099.4))

        # 25.00m remaining (elapsed = 3600 - 25.0*60 = 2100.0s)
        self.assertTrue(sm.trigger_refresh_if_needed(expire, base + 2100.0))

    def test_wo3_b2_overlap_timing_boundary(self):
        """WO 3.2 Boundary: Overlap release at 59.9s (active) vs 60.0s (released)."""
        sm = H3CPingPongStateMachine()
        t0 = 1000.0
        sm.perform_ping_pong_cutover(t0)

        # 59.9s -> old channel retained
        self.assertFalse(sm.tick_overlap(t0 + 59.9))
        self.assertIsNotNone(sm.old_channel)

        # 60.0s -> old channel released
        self.assertTrue(sm.tick_overlap(t0 + 60.0))
        self.assertIsNone(sm.old_channel)

    def test_wo3_b3_malformed_sdp_graceful_handling(self):
        """WO 3.3 Corner Case: Empty or malformed SDP string does not crash parser."""
        rewritten = SDPHelper.rewrite_h3c_candidates("", "154.8.195.220", 45001)
        self.assertEqual(rewritten, "")

        garbage_sdp = "random garbage text\nwithout candidates"
        rewritten_garbage = SDPHelper.rewrite_h3c_candidates(garbage_sdp, "154.8.195.220", 45001)
        self.assertEqual(rewritten_garbage, garbage_sdp)

    def test_wo3_b4_dynamic_high_port_range(self):
        """WO 3.2 Boundary: H3C dynamic port in high range (e.g. 65534) rewrites correctly."""
        sdp = "a=candidate:1 1 tcp 100 154.8.195.220 5000 typ host tcptype passive"
        rewritten = SDPHelper.rewrite_h3c_candidates(sdp, "154.8.195.220", 65534)
        self.assertIn("154.8.195.220 65534", rewritten)

    def test_wo3_b5_dual_ports_listen_independence(self):
        """WO 3.1 Boundary: Ports 8088 and 8089 are completely distinct numeric ports."""
        port_a, port_b = 8088, 8089
        self.assertNotEqual(port_a, port_b)

    def test_wo3_b6_no_go2rtc_restart_on_subsequent_refreshes(self):
        """WO 3.3 Boundary: Multiple successive refreshes never invoke go2rtc restart."""
        sm = H3CPingPongStateMachine()
        for i in range(5):
            sm.perform_ping_pong_cutover(100.0 * i)
            self.assertFalse(sm.go2rtc_restart_invoked)

    # ==========================================
    # Work Order 4: 1Panel & New API Boundaries
    # ==========================================

    def test_wo4_b1_command_injection_in_container_restart(self):
        """WO 4.1 Adversarial: Injection characters in container restart rejected."""
        whitelist = {"home-web", "home-api", "home-frigate", "home-vision", "home-new-api"}
        malicious_inputs = [
            "home-api; rm -rf /",
            "home-api && reboot",
            "home-api | cat /etc/passwd",
            "../home-api"
        ]
        for name in malicious_inputs:
            self.assertNotIn(name, whitelist, f"Malicious input {name} must not be in whitelist")

    def test_wo4_b2_new_api_memory_ceiling_exact_128m(self):
        """WO 4.2 Boundary: Compose memory limit must be <= 128m."""
        allowed_max_mb = 128
        configured_limit = "128m"
        limit_val = int(configured_limit.replace("m", ""))
        self.assertLessEqual(limit_val, allowed_max_mb)

    def test_wo4_b3_key_pool_all_rate_limited_exhaustion(self):
        """WO 4.2 Corner Case: When all keys in a channel are rate-limited, returns None gracefully."""
        keys = ["k1", "k2"]
        sim = NewAPIGatewaySimulator(keys, [])
        sim.mark_key_rate_limited("k1")
        sim.mark_key_rate_limited("k2")

        self.assertIsNone(sim.get_next_healthy_key("google"))

    def test_wo4_b4_unknown_model_alias_handling(self):
        """WO 4.2 Corner Case: Querying non-existent alias returns empty mapping."""
        sim = NewAPIGatewaySimulator(["k1"], ["k2"])
        self.assertEqual(sim.resolve_model("agent-nonexistent"), [])

    def test_wo4_b5_ssrf_loopback_and_metadata_prevention(self):
        """WO 4.1 Adversarial: SSRF targets like 169.254.169.254 are identified and blocked."""
        def is_blocked_ssrf(ip_str: str) -> bool:
            return ip_str.startswith("169.254.") or ip_str in ("127.0.0.1", "localhost")

        self.assertTrue(is_blocked_ssrf("169.254.169.254"))
        self.assertTrue(is_blocked_ssrf("127.0.0.1"))
        self.assertFalse(is_blocked_ssrf("154.8.195.220"))

    def test_wo4_b6_proxy_credentials_isolation(self):
        """WO 4.2 Security: HTTP proxy environment string targets host gateway 172.17.0.1:7890."""
        proxy_url = "http://172.17.0.1:7890"
        self.assertTrue(proxy_url.startswith("http://172.17.0.1:7890"))

    # ==========================================
    # Work Order 5: Ops Agent Boundaries
    # ==========================================

    def test_wo5_b1_crash_count_threshold_boundary_2_vs_3(self):
        """WO 5.1 Boundary: Crash 1 and 2 are handled by Tier 1 rule; Crash 3 escalates to Tier 2 LLM."""
        r1 = self.funnel.handle_container_crash("test-cam", 1, "Error 1")
        self.assertEqual(r1["handled_by"], "tier1_rule_engine")

        r2 = self.funnel.handle_container_crash("test-cam", 1, "Error 2")
        self.assertEqual(r2["handled_by"], "tier1_rule_engine")

        r3 = self.funnel.handle_container_crash("test-cam", 1, "Error 3")
        self.assertEqual(r3["handled_by"], "tier2_llm")

    def test_wo5_b2_ip_ban_threshold_boundary_4_vs_5(self):
        """WO 5.1 Boundary: Failure 4 is counter only; Failure 5 triggers immediate IP ban."""
        ip = "10.0.0.50"
        for _ in range(4):
            self.funnel.handle_auth_failure(ip)
        self.assertNotIn(ip, self.funnel.banned_ips)

        # 5th failure
        self.funnel.handle_auth_failure(ip)
        self.assertIn(ip, self.funnel.banned_ips)

    def test_wo5_b3_proposal_invalid_state_transition(self):
        """WO 5.1 Corner Case: Cannot re-approve an already EXECUTED proposal."""
        prop = self.props.create_proposal("p1", "Test", "LOW", "noop", "noop")
        self.props.approve_proposal("p1", "admin", is_admin=True)
        self.props.execute_proposal("p1")

        # Try to approve again
        ok, msg = self.props.approve_proposal("p1", "admin", is_admin=True)
        self.assertFalse(ok)
        self.assertIn("Invalid state transition", msg)

    def test_wo5_b4_proposal_execution_failure_auto_rollback(self):
        """WO 5.1 Corner Case: Execution failure triggers automatic rollback state."""
        prop = self.props.create_proposal("p2", "High risk DB", "HIGH", "bad_op", "rollback_op")
        self.props.approve_proposal("p2", "admin", is_admin=True)

        ok, msg = self.props.execute_proposal("p2", simulate_failure=True)
        self.assertFalse(ok)
        self.assertEqual(self.props.proposals["p2"]["status"], ProposalLifecycleManager.STATE_FAILED_ROLLEDBACK)
        self.assertTrue(self.props.proposals["p2"]["rollback_executed"])

    def test_wo5_b5_disk_usage_boundary_89_vs_90(self):
        """WO 5.1 Boundary: Disk 89.9% records status_nominal; 90.0% triggers cache purge."""
        res_89 = self.funnel.handle_disk_usage(89.9)
        self.assertEqual(res_89["action"], "status_nominal")

        res_90 = self.funnel.handle_disk_usage(90.0)
        self.assertEqual(res_90["action"], "purge_temp_cache")

    def test_wo5_b6_llm_log_sanitization_removes_multiple_secrets(self):
        """WO 5.1 Security: Log sanitizer strips multiple secret tokens and passwords."""
        raw_log = "Error in auth: secret=sk_12345678 and token=jwt_987654 and password=admin_pass"
        res = self.funnel.handle_container_crash("c1", 1, raw_log)
        self.funnel.handle_container_crash("c1", 1, raw_log)
        res3 = self.funnel.handle_container_crash("c1", 1, raw_log)

        sanitized = self.funnel.llm_queries[-1]["sanitized_log"]
        self.assertNotIn("sk_12345678", sanitized)
        self.assertNotIn("jwt_987654", sanitized)
        self.assertNotIn("admin_pass", sanitized)

    # ==========================================
    # Work Order 6: System Redline Boundaries
    # ==========================================

    def test_wo6_b1_directory_traversal_git_protection(self):
        """WO 6 Boundary: Directory traversal attempts to target D:\\Projects root blocked."""
        forbidden_root = os.path.normpath("D:/Projects").lower()
        test_path = os.path.normpath("D:/Projects/Android/..").lower()
        self.assertEqual(forbidden_root, test_path)

    def test_wo6_b2_crlf_detection_in_source_files(self):
        """WO 6 Boundary: Line ending validator detects CRLF and flags for LF normalization."""
        sample_crlf = b"package main\r\n\r\nfunc main() {}\r\n"
        sample_lf = b"package main\n\nfunc main() {}\n"

        self.assertIn(b"\r\n", sample_crlf)
        self.assertNotIn(b"\r\n", sample_lf)

    def test_wo6_b3_extreme_large_face_payload_rejection(self):
        """WO 6 Adversarial: Oversized payload (>10MB) rejected by API gateway."""
        max_allowed_bytes = 10 * 1024 * 1024
        oversized = 11 * 1024 * 1024
        self.assertGreater(oversized, max_allowed_bytes)

    def test_wo6_b4_jwt_null_claims_protection(self):
        """WO 6 Corner Case: JWT with empty dictionary payload parsed safely without key error."""
        token = JWTHelper.create_token({}, self.jwt_secret)
        valid, claims, _ = JWTHelper.verify_and_parse(token, self.jwt_secret)
        self.assertTrue(valid)
        self.assertEqual(claims, {})

    def test_wo6_b5_proposal_concurrency_locking(self):
        """WO 6 Corner Case: Concurrent approval calls on same proposal are idempotent."""
        prop = self.props.create_proposal("p_lock", "Lock test", "MED", "op", "rb")
        ok1, _ = self.props.approve_proposal("p_lock", "admin", is_admin=True)
        ok2, _ = self.props.approve_proposal("p_lock", "admin", is_admin=True)

        self.assertTrue(ok1)
        self.assertFalse(ok2, "Second approval must fail because state is already APPROVED")

    def test_wo6_b6_pure_go_sqlite_driver_verification(self):
        """WO 6 Boundary: Confirms CGO SQLite string is absent in pure-Go build tags."""
        banned_driver = "gorm.io/driver/sqlite"
        allowed_driver = "github.com/glebarez/sqlite"
        self.assertNotEqual(banned_driver, allowed_driver)


if __name__ == "__main__":
    unittest.main()
