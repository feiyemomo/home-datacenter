import os
import sys
import time
import unittest
from datetime import datetime, timezone, timedelta
from unittest.mock import MagicMock, patch

# Ensure keepalive module can be imported
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))

from h3c_client import H3CClient, PingPongRelay
import main


class MockH3CClient:
    def __init__(self):
        self.tunnels = {}  # tunnel_id -> tunnel dict
        self.deleted_tunnels = []
        self.created_tunnels = []
        self.create_fail = False

    def get_tunnels(self):
        return list(self.tunnels.values())

    def get_established_tunnel(self, app_name):
        for t in self.tunnels.values():
            if t.get("appName") == app_name and t.get("status") == "ESTABLISHED":
                return dict(t)
        return None

    def create_and_wait_tunnel(self, app_name, internal_port=8088, project_id="10233048", valid_time=3, timeout_sec=30, internal_ip=None, **kwargs):
        if self.create_fail:
            raise RuntimeError(f"Simulated network timeout creating tunnel for {app_name}")
        
        tun_id = f"tun_{app_name}_{len(self.created_tunnels) + 1}"
        close_iso = (datetime.now(timezone.utc) + timedelta(minutes=180)).isoformat()
        info = {
            "tunnelId": tun_id,
            "appName": app_name,
            "internalPort": internal_port,
            "internalIp": internal_ip or "192.168.31.233",
            "externalAddr": f"154.8.195.220:{30000 + internal_port}",
            "closeTime": close_iso,
            "remainingMinutes": 180.0,
            "status": "ESTABLISHED",
            "projectId": project_id
        }
        self.tunnels[tun_id] = info
        self.created_tunnels.append(info)
        return dict(info)

    def delete_tunnel(self, tunnel_id):
        self.deleted_tunnels.append(tunnel_id)
        if tunnel_id in self.tunnels:
            del self.tunnels[tunnel_id]
        return {"code": 0, "msg": "deleted"}


class TestPingPongRelay(unittest.TestCase):
    def setUp(self):
        self.mock_client = MockH3CClient()
        self.relay = PingPongRelay(
            client=self.mock_client,
            app_a="home_a",
            port_a=8088,
            app_b="home_b",
            port_b=8089,
            min_remaining_minutes=25,
            overlap_seconds=60
        )

    def test_cold_start_creates_home_a(self):
        """On cold start when no tunnel exists, home_a on port 8088 is created."""
        active, pending_retire = self.relay.step()

        self.assertIsNotNone(active)
        self.assertEqual(active["appName"], "home_a")
        self.assertEqual(active["internalPort"], 8088)
        self.assertEqual(self.relay.active_app, "home_a")
        self.assertIsNone(pending_retire)
        self.assertEqual(len(self.mock_client.created_tunnels), 1)
        self.assertEqual(len(self.mock_client.deleted_tunnels), 0)

    def test_healthy_tunnel_does_not_rotate(self):
        """When active tunnel has > 25m remaining, no rotation occurs."""
        self.relay.step()
        self.assertEqual(len(self.mock_client.created_tunnels), 1)

        # Set remaining time to 120 minutes (> 25)
        for t in self.mock_client.tunnels.values():
            t["remainingMinutes"] = 120.0

        active, pending_retire = self.relay.step()
        self.assertEqual(active["appName"], "home_a")
        self.assertIsNone(pending_retire)
        # Verify no new tunnel was created
        self.assertEqual(len(self.mock_client.created_tunnels), 1)
        self.assertEqual(len(self.mock_client.deleted_tunnels), 0)

    def test_25m_trigger_creates_home_b_with_overlap(self):
        """
        When active tunnel drops to <= 25m:
        1. home_b (8089) is created before home_a is touched ('先生后死').
        2. home_a is NOT deleted immediately.
        3. Both tunnels coexist simultaneously during 60s overlap.
        """
        self.relay.step()
        initial_tun_id = self.relay.active_tunnel["tunnelId"]

        # Simulate remaining time dropping to 24.0 minutes (<= 25m)
        self.mock_client.tunnels[initial_tun_id]["remainingMinutes"] = 24.0

        now = 1000000.0
        with patch("time.time", return_value=now):
            active, pending_retire = self.relay.step()

        # home_b is now the active tunnel
        self.assertEqual(active["appName"], "home_b")
        self.assertEqual(active["internalPort"], 8089)
        self.assertEqual(self.relay.active_app, "home_b")

        # home_a is NOT deleted yet!
        self.assertNotIn(initial_tun_id, self.mock_client.deleted_tunnels)

        # pending_retire is set with 60s overlap expiration
        self.assertIsNotNone(pending_retire)
        self.assertEqual(pending_retire["appName"], "home_a")
        self.assertEqual(pending_retire["tunnelId"], initial_tun_id)
        self.assertEqual(pending_retire["retireAt"], now + 60.0)

        # Both tunnels are present in H3C
        self.assertEqual(len(self.mock_client.tunnels), 2)
        self.assertIn(initial_tun_id, self.mock_client.tunnels)

    def test_overlap_grace_period_and_expiration(self):
        """Tests that retirement occurs only after the 60s overlap window has fully passed."""
        self.relay.step()
        old_id = self.relay.active_tunnel["tunnelId"]
        self.mock_client.tunnels[old_id]["remainingMinutes"] = 20.0

        t0 = 1000000.0
        with patch("time.time", return_value=t0):
            self.relay.step()

        # At t0 + 30s: 30s remaining in overlap window, old tunnel must NOT be deleted
        with patch("time.time", return_value=t0 + 30.0):
            retired = self.relay.check_and_retire()
            self.assertIsNone(retired)
            self.assertNotIn(old_id, self.mock_client.deleted_tunnels)
            self.assertIsNotNone(self.relay.pending_retire)

        # At t0 + 61s: 60s overlap elapsed, old tunnel must be silently deleted
        with patch("time.time", return_value=t0 + 61.0):
            retired = self.relay.check_and_retire()
            self.assertIsNotNone(retired)
            self.assertEqual(retired["tunnelId"], old_id)
            self.assertEqual(retired["appName"], "home_a")
            self.assertIn(old_id, self.mock_client.deleted_tunnels)
            self.assertIsNone(self.relay.pending_retire)

        # Only home_b remains in H3C tunnels
        self.assertEqual(len(self.mock_client.tunnels), 1)
        self.assertEqual(self.relay.active_app, "home_b")

    def test_symmetric_rotation_from_b_back_to_a(self):
        """Tests that on the next cycle, home_b rotates back to home_a symmetrically."""
        # 1. Start on home_a
        self.relay.step()
        id_a1 = self.relay.active_tunnel["tunnelId"]

        # 2. Trigger rotation to home_b at <= 25m
        self.mock_client.tunnels[id_a1]["remainingMinutes"] = 20.0
        t0 = 1000000.0
        with patch("time.time", return_value=t0):
            self.relay.step()
        
        # 3. Complete overlap and retire home_a
        with patch("time.time", return_value=t0 + 65.0):
            self.relay.check_and_retire()

        self.assertEqual(self.relay.active_app, "home_b")
        id_b = self.relay.active_tunnel["tunnelId"]

        # 4. Now home_b drops to <= 25m remaining -> Trigger symmetric rotation to home_a (8088)
        self.mock_client.tunnels[id_b]["remainingMinutes"] = 15.0
        t1 = t0 + 3600.0
        with patch("time.time", return_value=t1):
            active, pending_retire = self.relay.step()

        self.assertEqual(active["appName"], "home_a")
        self.assertEqual(active["internalPort"], 8088)
        self.assertEqual(self.relay.active_app, "home_a")
        self.assertEqual(pending_retire["appName"], "home_b")
        self.assertEqual(pending_retire["tunnelId"], id_b)

        # 5. Advance 61 seconds -> home_b is retired
        with patch("time.time", return_value=t1 + 61.0):
            retired = self.relay.check_and_retire()
            self.assertIsNotNone(retired)
            self.assertEqual(retired["appName"], "home_b")
            self.assertIn(id_b, self.mock_client.deleted_tunnels)
            self.assertIsNone(self.relay.pending_retire)

    def test_alternate_creation_failure_preserves_active_tunnel(self):
        """
        If creating the alternate tunnel fails (e.g. Oasis API error),
        the active tunnel MUST NOT be deleted. It has remaining time to serve traffic.
        """
        self.relay.step()
        initial_id = self.relay.active_tunnel["tunnelId"]
        self.mock_client.tunnels[initial_id]["remainingMinutes"] = 22.0

        # Simulate network error on peer creation
        self.mock_client.create_fail = True

        active, pending_retire = self.relay.step()

        # Current active tunnel is still home_a
        self.assertEqual(active["tunnelId"], initial_id)
        self.assertEqual(active["appName"], "home_a")
        self.assertNotIn(initial_id, self.mock_client.deleted_tunnels)
        self.assertIsNone(pending_retire)
        self.assertIn(initial_id, self.mock_client.tunnels)

    def test_force_renew_triggers_immediate_rotation(self):
        """force_renew=True triggers immediate failover even if remaining time > 25m."""
        self.relay.step()
        id_a = self.relay.active_tunnel["tunnelId"]
        self.mock_client.tunnels[id_a]["remainingMinutes"] = 175.0  # plenty of time

        t0 = 2000000.0
        with patch("time.time", return_value=t0):
            active, pending_retire = self.relay.step(force_renew=True)

        self.assertEqual(active["appName"], "home_b")
        self.assertEqual(self.relay.active_app, "home_b")
        self.assertIsNotNone(pending_retire)
        self.assertEqual(pending_retire["tunnelId"], id_a)
        self.assertNotIn(id_a, self.mock_client.deleted_tunnels)


class TestMainIntegrationAndSanitization(unittest.TestCase):
    def test_get_sanitized_state(self):
        """get_sanitized_state() must return clean state dictionary with remainingSeconds."""
        with main.state_lock:
            main.tunnel_state["activeApp"] = "home_b"
            main.tunnel_state["externalAddr"] = "154.8.195.220:41234"
            main.tunnel_state["status"] = "online"
            main.tunnel_state["tunnels"] = {
                "home_a": {"status": "ESTABLISHED", "externalAddr": "154.8.195.220:40001"},
                "home_b": {"status": "ESTABLISHED", "externalAddr": "154.8.195.220:41234"},
                "home": {"status": "ESTABLISHED", "externalAddr": "154.8.195.220:41234"},
                "webrtc_test": {"status": "ESTABLISHED", "externalAddr": "154.8.195.220:40002"}
            }

        sanitized = main.get_sanitized_state()
        self.assertIn("home_a", sanitized["tunnels"])
        self.assertIn("home_b", sanitized["tunnels"])
        self.assertIn("webrtc_test", sanitized["tunnels"])
        self.assertEqual(sanitized["activeApp"], "home_b")
        self.assertEqual(sanitized["externalAddr"], "154.8.195.220:41234")

    @patch("urllib.request.urlopen")
    @patch("main.docker_exec_frigate")
    def test_sync_webrtc_zero_restart(self, mock_docker_exec, mock_urlopen):
        """
        sync_webrtc_to_frigate must update config via REST API but NEVER call
        docker_exec_frigate with 'api/restart'.
        """
        # Mock Frigate GET /api/config response
        mock_resp = MagicMock()
        mock_resp.read.return_value = b'{"go2rtc": {"webrtc": {"candidates": ["127.0.0.1:8555", "154.8.195.220:30000"]}}}'
        mock_resp.__enter__.return_value = mock_resp
        mock_resp.__exit__.return_value = None
        mock_urlopen.return_value = mock_resp

        # Mock docker_exec_frigate for create_config.py
        mock_docker_exec.return_value = "config regenerated"

        main.last_synced_webrtc_addr = None
        main.sync_webrtc_to_frigate("154.8.195.220:45000")

        # Verify docker_exec_frigate was NEVER called with restart
        for call_args in mock_docker_exec.call_args_list:
            cmd = call_args[0][0]
            self.assertFalse(
                any("restart" in str(part) for part in cmd),
                f"docker_exec_frigate was violently called with restart: {cmd}"
            )


class TestLANIPDetection(unittest.TestCase):
    def test_tier1_explicit_env(self):
        with patch.dict(os.environ, {"NAS_LAN_IP": "192.168.31.200"}):
            ip = main.detect_nas_lan_ip()
            self.assertEqual(ip, "192.168.31.200")

    def test_tier1_auto_env_falls_through(self):
        with patch.dict(os.environ, {"NAS_LAN_IP": "auto"}):
            ip = main.detect_nas_lan_ip()
            self.assertNotEqual(ip, "auto")
            self.assertTrue(ip.startswith("192.168.") or ip.startswith("10."))

    @patch("urllib.request.urlopen")
    def test_tier3_frigate_candidates(self, mock_urlopen):
        with patch.dict(os.environ, {"NAS_LAN_IP": ""}), \
             patch("os.path.exists", return_value=False):
            mock_resp = MagicMock()
            mock_resp.read.return_value = b'{"go2rtc":{"webrtc":{"candidates":["127.0.0.1:8555", "192.168.31.233:8555", "100.90.67.71:8555"]}}}'
            mock_resp.__enter__.return_value = mock_resp
            mock_resp.__exit__.return_value = None
            mock_urlopen.return_value = mock_resp

            ip = main.detect_nas_lan_ip()
            self.assertEqual(ip, "192.168.31.233")


class TestCircuitBreaker(unittest.TestCase):
    def test_backoff_and_circuit_tripping(self):
        cb = main.TunnelCircuitBreaker(name="test", max_consecutive_failures=3, base_cooldown=10, max_cooling=60)
        self.assertTrue(cb.can_failover())

        t0 = 1000.0
        with patch("time.time", return_value=t0):
            cb.record_failure()
            self.assertEqual(cb.consecutive_failures, 1)
            # Cooling for base_cooldown = 10s
            self.assertFalse(cb.can_failover())

        # After 11s, can failover again
        with patch("time.time", return_value=t0 + 11.0):
            self.assertTrue(cb.can_failover())
            cb.record_failure()
            self.assertEqual(cb.consecutive_failures, 2)
            # Cooling for 20s
            self.assertFalse(cb.can_failover())

        # After 32s, third failure trips circuit breaker into max_cooling (60s)
        with patch("time.time", return_value=t0 + 32.0):
            self.assertTrue(cb.can_failover())
            cb.record_failure()
            self.assertEqual(cb.consecutive_failures, 3)
            self.assertFalse(cb.can_failover())

        # During 60s cooling, must remain tripped
        with patch("time.time", return_value=t0 + 70.0):
            self.assertFalse(cb.can_failover())

        # On recovery probe success
        cb.record_success()
        self.assertEqual(cb.consecutive_failures, 0)
        self.assertTrue(cb.can_failover())


class TestAppUpdateOasisSync(unittest.TestCase):
    def test_create_tunnel_auto_syncs_oasis_when_ip_diverges(self):
        client = H3CClient("test_user", "test_pass")
        client._request = MagicMock()
        client._request.return_value = {"code": 0, "msg": "ok", "data": {"tunnelId": "tun_123"}}

        app_info = {
            "projectId": "10233048",
            "projectName": "home",
            "tunnelAppId": "app_999",
            "tunnelAppName": "home",
            "internalIp": "192.168.31.234",  # Old IP
            "internalPort": 8088,
            "proxyType": "HTTP"
        }

        # Call create_tunnel with new internal_ip 192.168.31.233
        tun_id = client.create_tunnel(app_info, internal_ip="192.168.31.233")
        self.assertEqual(tun_id, "tun_123")

        # Verify appupdate was called to correct Oasis cloud config
        update_calls = [
            c for c in client._request.call_args_list
            if "/v3/ace/oasis/tunnelmgr/tunnelapp/appupdate" in str(c)
        ]
        self.assertEqual(len(update_calls), 1)
        update_payload = update_calls[0][1]["data"]
        self.assertEqual(update_payload["internalIp"], "192.168.31.233")
        self.assertEqual(app_info["internalIp"], "192.168.31.233")


if __name__ == "__main__":
    unittest.main()
