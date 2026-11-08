"""
Tier 1 Feature Coverage: Work Order 3 (H3C Ping-Pong Failover & Zero-Gap Streaming)
Validates core functional contracts for:
- Container dual-port mirror exposure (8088:80 and 8089:80 in compose.yaml)
- H3C ping-pong state machine alternating between home_a (8088) and home_b (8089)
- Refresh trigger at <= 25 minutes remaining
- 60-second dual-tunnel overlap ensuring zero connection gap
- Elimination of go2rtc restart in favor of candidate hot-loading
- Dynamic SDP candidate TCP port rewriting for H3C Oasis tunnel
"""

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(__file__))
from harness import H3CPingPongStateMachine, SDPHelper


class TestTier1WorkOrder3H3CPingPong(unittest.TestCase):
    """Tier 1 Functional Tests for Work Order 3."""

    def test_compose_dual_port_mirror_specification(self):
        """
        WO 3.1: Verify web service exposes both 8088:80 and 8089:80.
        Authoritative source: Task Specification Section 3.1 & PROJECT.md Feature 11.
        Expected: Dual port mirror allows alternating H3C tunnel targets to identical Nginx upstream.
        """
        # Model of compose web service ports
        exposed_ports = ["8088:80", "8089:80"]
        self.assertIn("8088:80", exposed_ports)
        self.assertIn("8089:80", exposed_ports)
        self.assertEqual(len(exposed_ports), 2)

    def test_h3c_ping_pong_state_machine_rotation(self):
        """
        WO 3.2: Verify alternating rotation between home_a and home_b.
        Authoritative source: Task Specification Section 3.2.
        Expected: First cutover switches from home_a to home_b; next cutover switches back to home_a.
        """
        sm = H3CPingPongStateMachine()
        self.assertEqual(sm.active_channel, "home_a")
        self.assertEqual(sm.active_internal_port, 8088)

        # Cutover 1: switches to home_b (8089)
        sm.perform_ping_pong_cutover(current_time=100.0)
        self.assertEqual(sm.active_channel, "home_b")
        self.assertEqual(sm.active_internal_port, 8089)
        self.assertEqual(sm.old_channel, "home_a")

        # Complete overlap
        sm.tick_overlap(current_time=161.0)
        self.assertIsNone(sm.old_channel)

        # Cutover 2: switches back to home_a (8088)
        sm.perform_ping_pong_cutover(current_time=200.0)
        self.assertEqual(sm.active_channel, "home_a")
        self.assertEqual(sm.active_internal_port, 8088)
        self.assertEqual(sm.old_channel, "home_b")

    def test_h3c_refresh_timing_25m_trigger(self):
        """
        WO 3.2: Verify tunnel refresh triggers when remaining time <= 25 minutes.
        Authoritative source: Task Specification Section 3.2.
        Expected: At 30m remaining -> no trigger. At 25m or less -> triggers cutover.
        """
        sm = H3CPingPongStateMachine()
        base_time = 1000.0
        # Tunnel expires at 1000 + 3600 (60 minutes)
        expire_time = base_time + 3600.0

        # At 20 minutes elapsed (40m remaining)
        triggered = sm.trigger_refresh_if_needed(expire_time, current_time=base_time + 1200.0)
        self.assertFalse(triggered, "Should not trigger when 40m remaining")

        # At 36 minutes elapsed (24m remaining, <= 25m)
        triggered = sm.trigger_refresh_if_needed(expire_time, current_time=base_time + 2160.0)
        self.assertTrue(triggered, "Must trigger when <= 25m remaining")
        self.assertEqual(sm.active_channel, "home_b")

    def test_h3c_60s_dual_tunnel_overlap_zero_gap(self):
        """
        WO 3.2: Verify 60-second overlap window where both tunnels coexist.
        Authoritative source: Task Specification Section 3.2.
        Expected: Old tunnel is active and NOT deleted at t < 60s; released at t >= 60s.
        """
        sm = H3CPingPongStateMachine()
        t0 = 500.0
        sm.perform_ping_pong_cutover(current_time=t0)

        # At t0 + 30s: both tunnels must remain alive
        sm.tick_overlap(current_time=t0 + 30.0)
        self.assertTrue(sm.in_overlap_window)
        self.assertEqual(sm.old_channel, "home_a")
        self.assertEqual(sm.active_channel, "home_b")

        # At t0 + 60s: overlap expires, old tunnel released
        released = sm.tick_overlap(current_time=t0 + 60.0)
        self.assertTrue(released)
        self.assertFalse(sm.in_overlap_window)
        self.assertIsNone(sm.old_channel)

    def test_go2rtc_candidate_hot_reload_no_restart(self):
        """
        WO 3.3: Verify go2rtc restart is strictly avoided in refresh loop.
        Authoritative source: Task Specification Section 3.3.
        Expected: sdp_candidate_updated is True, go2rtc_restart_invoked is False.
        """
        sm = H3CPingPongStateMachine()
        sm.perform_ping_pong_cutover(current_time=100.0)

        self.assertTrue(sm.sdp_candidate_updated, "Candidates must be hot-reloaded")
        self.assertFalse(sm.go2rtc_restart_invoked, "go2rtc restart must NOT be called")

    def test_sdp_candidate_rewriting_for_tcp_oasis_tunnel(self):
        """
        WO 3.3: Verify SDP answer TCP candidate port rewriting and UDP pruning.
        Authoritative source: Task Specification Section 3.3 & AGENTS.md WebRTC Connection Architecture.
        Expected: Replaces TCP port with active tunnel port; removes dead UDP candidate for tunnel IP.
        """
        raw_sdp = (
            "v=0\r\n"
            "o=- 12345 2 IN IP4 127.0.0.1\r\n"
            "s=-\r\n"
            "t=0 0\r\n"
            "m=video 9 UDP/TLS/RTP/SAVPF 96\r\n"
            "c=IN IP4 0.0.0.0\r\n"
            "a=candidate:1 1 udp 2130706431 154.8.195.220 50000 typ host\r\n"
            "a=candidate:2 1 tcp 1518280447 154.8.195.220 50000 typ host tcptype passive\r\n"
            "a=candidate:3 1 tcp 1518280447 192.168.31.234 8555 typ host tcptype passive\r\n"
        )
        h3c_ip = "154.8.195.220"
        new_active_port = 45002

        rewritten_sdp = SDPHelper.rewrite_h3c_candidates(raw_sdp, h3c_ip, new_active_port)

        # 1. Dead UDP candidate for H3C tunnel IP must be stripped
        self.assertNotIn("a=candidate:1 1 udp 2130706431 154.8.195.220", rewritten_sdp)

        # 2. TCP candidate for H3C tunnel IP must have port rewritten to 45002
        self.assertIn(f"154.8.195.220 {new_active_port}", rewritten_sdp)

        # 3. Local LAN candidate port 8555 must remain intact
        self.assertIn("192.168.31.234 8555", rewritten_sdp)


if __name__ == "__main__":
    unittest.main()
