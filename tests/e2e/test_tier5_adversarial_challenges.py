#!/usr/bin/env python3
"""
Tier 5 Adversarial Challenges & Stress Testing Suite
Covers:
  - Challenge 2: Face Engine AES-256-GCM Cryptographic Corruption & Migration
  - Challenge 3: Vision Endpoint Authentication & RBAC Defenses
  - Challenge 4: Vision Dynamic CPU Gating Boundary Conditions & Spikes
  - Challenge 5: H3C Ping-Pong State Machine Network Timeouts, Collisions & Boundaries
"""

import os
import sys
import json
import time
import base64
import hmac
import hashlib
import tempfile
import shutil
import unittest
from unittest.mock import MagicMock, patch
from datetime import datetime, timezone, timedelta
from http.server import HTTPServer
import threading
import urllib.request
import urllib.error

# Project paths
PROJECT_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
VISION_DIR = os.path.join(PROJECT_ROOT, "services", "vision")
H3C_DIR = os.path.join(PROJECT_ROOT, "deploy", "h3c-keepalive")

# Add paths to sys.path
if VISION_DIR not in sys.path:
    sys.path.insert(0, VISION_DIR)
if H3C_DIR not in sys.path:
    sys.path.append(H3C_DIR)

import importlib.util

def load_module_from_file(module_name: str, file_path: str):
    spec = importlib.util.spec_from_file_location(module_name, file_path)
    mod = importlib.util.module_from_spec(spec)
    sys.modules[module_name] = mod
    spec.loader.exec_module(mod)
    return mod

# Load vision main explicitly
vision_main_path = os.path.join(VISION_DIR, "main.py")
vision_main = load_module_from_file("vision_service_main", vision_main_path)

import numpy as np
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from face_engine import FaceEngine
from h3c_client import PingPongRelay


# ==============================================================================
# Helper Utilities
# ==============================================================================

def make_test_jwt(secret: str, user_id: int = 1, is_admin: bool = False, expires_in: int = 3600) -> str:
    """Generates an HS256 JWT string for testing."""
    header = {"alg": "HS256", "typ": "JWT"}
    payload = {
        "user_id": user_id,
        "is_admin": is_admin,
        "exp": int(time.time()) + expires_in
    }
    h_b64 = base64.urlsafe_b64encode(json.dumps(header).encode()).decode().rstrip("=")
    p_b64 = base64.urlsafe_b64encode(json.dumps(payload).encode()).decode().rstrip("=")
    signing_input = f"{h_b64}.{p_b64}".encode("ascii")
    sig = hmac.new(secret.encode("utf-8"), signing_input, hashlib.sha256).digest()
    s_b64 = base64.urlsafe_b64encode(sig).decode().rstrip("=")
    return f"{h_b64}.{p_b64}.{s_b64}"


def create_1x1_jpeg_base64() -> str:
    """Returns a valid 1x1 base64 encoded JPEG for vision payload tests."""
    # Smallest valid 1x1 JPEG bytes
    raw_jpg = (
        b"\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01\x01\x01\x00`\x00`\x00\x00"
        b"\xff\xdb\x00C\x00\x08\x06\x06\x07\x06\x05\x08\x07\x07\x07\t\t"
        b"\x08\n\x0c\x14\r\x0c\x0b\x0b\x0c\x19\x12\x13\x0f\x14\x1d\x1a"
        b"\x1f\x1e\x1d\x1a\x1c\x1c $.' \",#\x1c\x1c(7),01444\x1f'9=82<.342"
        b"\xff\xc0\x00\x0b\x08\x00\x01\x00\x01\x01\x01\x11\x00\xff\xc4"
        b"\x00\x1f\x00\x00\x01\x05\x01\x01\x01\x01\x01\x01\x00\x00\x00"
        b"\x00\x00\x00\x00\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b"
        b"\xff\xda\x00\x08\x01\x01\x00\x00?\x00\xbf\x00\xff\xd9"
    )
    return base64.b64encode(raw_jpg).decode("ascii")


# ==============================================================================
# Challenge 2: Face Engine AES-256-GCM Cryptographic Corruption & Migration
# ==============================================================================

class TestChallenge2FaceEngineCryptographicCorruption(unittest.TestCase):
    """
    Stress-tests FaceEngine storage against cryptographic tampering, key mismatches,
    empty files, and corrupt legacy files.
    """

    def setUp(self):
        self.temp_dir = tempfile.mkdtemp(prefix="challenger_face_")
        self.model_dir = os.path.join(VISION_DIR, "models")
        os.environ["VISION_AES_KEY"] = "challenger-secure-key-256"

    def tearDown(self):
        shutil.rmtree(self.temp_dir, ignore_errors=True)

    def test_corrupted_aes_gcm_tag_handling(self):
        """
        Verify that tampering with the AES-256-GCM authentication tag causes
        clean decryption rejection without process crash.
        """
        fe = FaceEngine(model_dir=self.model_dir, data_dir=self.temp_dir)
        fe.persons.append({"name": "alice", "feature": np.ones(128, dtype=np.float32)})
        fe._save_db()

        # Read encrypted bytes: Magic (7) + Nonce (12) + Ciphertext + Tag (16)
        with open(fe.db_path, "rb") as f:
            raw = bytearray(f.read())
        self.assertTrue(raw.startswith(b"AESGCM\x01"))

        # Flip bits in the final 8 bytes (corrupting GCM authentication tag)
        for i in range(len(raw) - 8, len(raw)):
            raw[i] ^= 0xFF

        with open(fe.db_path, "wb") as f:
            f.write(raw)

        # Attempt to load the corrupted database with a fresh engine
        fe_tampered = FaceEngine(model_dir=self.model_dir, data_dir=self.temp_dir)
        # Should gracefully log error and initialize with empty persons
        self.assertEqual(len(fe_tampered.persons), 0)

    def test_wrong_key_decryption_failure(self):
        """
        Verify that decrypting with an incorrect key fails GCM authentication
        and does not corrupt database state.
        """
        os.environ["VISION_AES_KEY"] = "original-secret-key-1"
        fe = FaceEngine(model_dir=self.model_dir, data_dir=self.temp_dir)
        fe.persons.append({"name": "bob", "feature": np.zeros(128, dtype=np.float32)})
        fe._save_db()

        # Switch to unauthorized wrong key
        os.environ["VISION_AES_KEY"] = "wrong-intruder-key-2"
        fe_wrong_key = FaceEngine(model_dir=self.model_dir, data_dir=self.temp_dir)
        self.assertEqual(len(fe_wrong_key.persons), 0)

    def test_empty_faces_file_handling(self):
        """
        Verify that an empty file (0 bytes) is handled gracefully without error.
        """
        db_path = os.path.join(self.temp_dir, "faces.json")
        with open(db_path, "wb") as f:
            f.write(b"")

        fe = FaceEngine(model_dir=self.model_dir, data_dir=self.temp_dir)
        self.assertEqual(len(fe.persons), 0)

    def test_legacy_plaintext_corruption_handling(self):
        """
        Verify that non-AES files with invalid / corrupted data are rejected gracefully.
        """
        db_path = os.path.join(self.temp_dir, "faces.json")
        with open(db_path, "wb") as f:
            f.write(b"NOT_MAGIC_AND_NOT_JSON\x00\x01\xfe\xff{corrupt")

        fe = FaceEngine(model_dir=self.model_dir, data_dir=self.temp_dir)
        self.assertEqual(len(fe.persons), 0)

    def test_legacy_plaintext_migration_to_encrypted(self):
        """
        Verify that valid legacy plaintext faces.json is loaded and immediately
        auto-migrated to AES-256-GCM format.
        """
        db_path = os.path.join(self.temp_dir, "faces.json")
        legacy_data = {
            "persons": [
                {"name": "charlie", "feature": [0.5] * 128}
            ]
        }
        with open(db_path, "w", encoding="utf-8") as f:
            json.dump(legacy_data, f)

        fe = FaceEngine(model_dir=self.model_dir, data_dir=self.temp_dir)
        self.assertEqual(len(fe.persons), 1)
        self.assertEqual(fe.persons[0]["name"], "charlie")

        # Verify disk file is now fully encrypted with magic header
        with open(db_path, "rb") as f:
            on_disk = f.read()
        self.assertTrue(on_disk.startswith(b"AESGCM\x01"))
        self.assertNotIn(b"charlie", on_disk)

    def test_truncated_header_resilience(self):
        """
        Verify that truncated header files (shorter than magic + nonce) do not crash.
        """
        db_path = os.path.join(self.temp_dir, "faces.json")
        with open(db_path, "wb") as f:
            f.write(b"AESGCM\x01truncated")

        fe = FaceEngine(model_dir=self.model_dir, data_dir=self.temp_dir)
        self.assertEqual(len(fe.persons), 0)


# ==============================================================================
# Challenge 3: Vision Endpoint Authentication & RBAC Defenses
# ==============================================================================

class TestChallenge3VisionEndpointAuthAndRBAC(unittest.TestCase):
    """
    Stress-tests verify_vision_auth and HTTP endpoints for unauthenticated access,
    invalid PSK, and non-admin JWTs.
    """

    def setUp(self):
        self.jwt_secret = "test-jwt-secret-32-chars-long-vision!!"
        self.psk = "test-super-secret-psk-12345"
        os.environ["JWT_SECRET"] = self.jwt_secret
        os.environ["VISION_PSK"] = self.psk

    def test_unauthenticated_request_rejected(self):
        """Missing auth header rejected with HTTP 401."""
        authorized, code, msg = vision_main.verify_vision_auth({})
        self.assertFalse(authorized)
        self.assertEqual(code, 401)
        self.assertIn("missing", msg.lower())

    def test_invalid_psk_rejected(self):
        """Invalid PSK rejected with HTTP 401."""
        headers = {"X-Vision-PSK": "totally-wrong-psk"}
        authorized, code, _ = vision_main.verify_vision_auth(headers)
        self.assertFalse(authorized)
        self.assertEqual(code, 401)

    def test_non_admin_jwt_rejected(self):
        """Valid JWT with is_admin=False rejected with HTTP 403 Forbidden."""
        user_jwt = make_test_jwt(self.jwt_secret, user_id=42, is_admin=False)
        headers = {"Authorization": f"Bearer {user_jwt}"}
        authorized, code, msg = vision_main.verify_vision_auth(headers)
        self.assertFalse(authorized)
        self.assertEqual(code, 403)
        self.assertIn("administrator privileges required", msg)

    def test_expired_admin_jwt_rejected(self):
        """Expired admin JWT rejected with HTTP 401."""
        expired_admin_jwt = make_test_jwt(self.jwt_secret, user_id=1, is_admin=True, expires_in=-3600)
        headers = {"Authorization": f"Bearer {expired_admin_jwt}"}
        authorized, code, msg = vision_main.verify_vision_auth(headers)
        self.assertFalse(authorized)
        self.assertEqual(code, 401)
        self.assertIn("expired", msg.lower())

    def test_tampered_jwt_signature_rejected(self):
        """Admin JWT with modified signature rejected with HTTP 401."""
        admin_jwt = make_test_jwt(self.jwt_secret, user_id=1, is_admin=True)
        tampered_jwt = admin_jwt[:-5] + "XXXXX"
        headers = {"Authorization": f"Bearer {tampered_jwt}"}
        authorized, code, msg = vision_main.verify_vision_auth(headers)
        self.assertFalse(authorized)
        self.assertEqual(code, 401)
        self.assertIn("signature", msg.lower())

    def test_valid_psk_accepted(self):
        """Valid PSK passes authentication (HTTP 200)."""
        headers = {"X-Vision-PSK": self.psk}
        authorized, code, _ = vision_main.verify_vision_auth(headers)
        self.assertTrue(authorized)
        self.assertEqual(code, 200)

    def test_valid_admin_jwt_accepted(self):
        """Valid Admin JWT passes authentication (HTTP 200)."""
        admin_jwt = make_test_jwt(self.jwt_secret, user_id=1, is_admin=True)
        headers = {"Authorization": f"Bearer {admin_jwt}"}
        authorized, code, _ = vision_main.verify_vision_auth(headers)
        self.assertTrue(authorized)
        self.assertEqual(code, 200)

    def test_unsigned_jwt_rejected_when_secret_unset(self):
        """Unsigned forged JWT must be rejected with 401 when JWT_SECRET is unset."""
        os.environ.pop("JWT_SECRET", None)
        os.environ.pop("VISION_JWT_SECRET", None)
        try:
            payload = {"is_admin": True}
            b64_payload = base64.urlsafe_b64encode(json.dumps(payload).encode()).decode().rstrip("=")
            fake_token = f"eyJhbGciOiJIUzI1NiJ9.{b64_payload}.fake_signature"
            headers = {"Authorization": f"Bearer {fake_token}"}
            authorized, code, msg = vision_main.verify_vision_auth(headers)
            self.assertFalse(authorized)
            self.assertEqual(code, 401)
            self.assertIn("unavailable", msg.lower())
        finally:
            os.environ["JWT_SECRET"] = self.jwt_secret


# ==============================================================================
# Challenge 4: Vision Dynamic CPU Gating Boundary Conditions
# ==============================================================================

class TestChallenge4VisionDynamicCPUGatingBoundaries(unittest.TestCase):
    """
    Stress-tests the exact CPU boundary conditions (59.9%, 60.0%, 80.0%, 80.1%)
    and rapid CPU spikes.
    """

    @classmethod
    def setUpClass(cls):
        # Start in-process test server on dynamic port
        cls.server = HTTPServer(("127.0.0.1", 0), vision_main.VisionHandler)
        cls.port = cls.server.server_port
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def _post_analyze(self, cpu_mock_val: float) -> tuple[int, dict, dict]:
        """Sends POST /api/v1/analyze with mocked CPU value."""
        url = f"http://127.0.0.1:{self.port}/api/v1/analyze"
        payload = {
            "image": create_1x1_jpeg_base64(),
            "detect_face": True,
            "detect_pose": True
        }
        data = json.dumps(payload).encode("utf-8")
        req = urllib.request.Request(url, data=data, headers={"Content-Type": "application/json"})

        with patch.object(vision_main, "get_cpu_usage", return_value=cpu_mock_val):
            try:
                with urllib.request.urlopen(req, timeout=5) as resp:
                    resp_headers = dict(resp.headers)
                    body = json.loads(resp.read().decode())
                    return resp.status, body, resp_headers
            except urllib.error.HTTPError as e:
                resp_headers = dict(e.headers)
                body = json.loads(e.read().decode())
                return e.code, body, resp_headers

    def test_boundary_59_9_percent_is_full_mode(self):
        """CPU 59.9% (< 60.0%) must execute full mode (200 OK)."""
        code, body, _ = self._post_analyze(59.9)
        self.assertEqual(code, 200)
        self.assertEqual(body["data"]["gating_mode"], "full")
        self.assertEqual(body["data"]["cpu_usage"], 59.9)

    def test_boundary_60_0_percent_is_lightweight_mode(self):
        """CPU 60.0% (>= 60.0%) must enter lightweight mode and skip pose (200 OK)."""
        code, body, _ = self._post_analyze(60.0)
        self.assertEqual(code, 200)
        self.assertEqual(body["data"]["gating_mode"], "lightweight")
        self.assertEqual(body["data"]["poses"], [])
        self.assertEqual(body["data"]["cpu_usage"], 60.0)

    def test_boundary_80_0_percent_is_lightweight_mode(self):
        """CPU 80.0% (not > 80.0%) must remain in lightweight mode (200 OK)."""
        code, body, _ = self._post_analyze(80.0)
        self.assertEqual(code, 200)
        self.assertEqual(body["data"]["gating_mode"], "lightweight")
        self.assertEqual(body["data"]["cpu_usage"], 80.0)

    def test_boundary_80_1_percent_triggers_circuit_breaker(self):
        """CPU 80.1% (> 80.0%) must trigger 503 circuit breaker with Retry-After header."""
        code, body, headers = self._post_analyze(80.1)
        self.assertEqual(code, 503)
        self.assertEqual(body["gating_mode"], "throttled")
        self.assertEqual(body["cpu_usage"], 80.1)
        self.assertIn("Retry-After", headers)
        self.assertEqual(headers["Retry-After"], "5")

    def test_rapid_cpu_spikes_no_state_leakage(self):
        """Simulate rapid CPU fluctuations: each request is isolated and evaluated on instantaneous CPU."""
        sequence = [
            (15.0, 200, "full"),
            (95.0, 503, "throttled"),
            (45.0, 200, "full"),
            (80.5, 503, "throttled"),
            (70.0, 200, "lightweight"),
            (10.0, 200, "full"),
        ]
        for cpu_val, expected_code, expected_mode in sequence:
            code, body, _ = self._post_analyze(cpu_val)
            self.assertEqual(code, expected_code, f"Failed at CPU {cpu_val}")
            actual_mode = body.get("gating_mode") or body.get("data", {}).get("gating_mode")
            self.assertEqual(actual_mode, expected_mode, f"Failed mode at CPU {cpu_val}")


# ==============================================================================
# Challenge 5: H3C Ping-Pong State Machine Edge Cases
# ==============================================================================

class TestChallenge5H3CPingPongStateMachine(unittest.TestCase):
    """
    Stress-tests H3C PingPongRelay for:
      - Network timeout during wait_for_tunnel
      - Simultaneous port collision resolution
      - 60s overlap boundary transitions
      - Cloud deletion failure resilience
    """

    def setUp(self):
        self.client_mock = MagicMock()
        self.relay = PingPongRelay(
            client=self.client_mock,
            app_a="home_a",
            port_a=8088,
            app_b="home_b",
            port_b=8089,
            min_remaining_minutes=25,
            overlap_seconds=60
        )

    def test_network_timeout_during_wait_preserves_active_tunnel(self):
        """
        If creating the alternate tunnel times out or fails over the network,
        the active tunnel MUST remain active and pending_retire MUST NOT be set.
        """
        # Active tunnel home_a has 10 minutes left (< 25m trigger)
        active_tun = {
            "tunnelId": "tun-a-111",
            "appName": "home_a",
            "internalPort": 8088,
            "externalAddr": "154.8.195.220:18088",
            "remainingMinutes": 10.0,
            "status": "ESTABLISHED"
        }
        self.client_mock.get_established_tunnel.side_effect = lambda app: active_tun if app == "home_a" else None

        # Network timeout on creating alternate peer
        self.client_mock.create_and_wait_tunnel.side_effect = TimeoutError("H3C Cloud timeout: tunnel failed to reach ESTABLISHED")

        act, ret = self.relay.step()

        # Active tunnel remains home_a
        self.assertIsNotNone(act)
        self.assertEqual(act["appName"], "home_a")
        self.assertEqual(act["tunnelId"], "tun-a-111")
        # pending_retire is not set
        self.assertIsNone(ret)
        # delete_tunnel was NEVER invoked (prevents destruction of active connection)
        self.client_mock.delete_tunnel.assert_not_called()

    def test_simultaneous_port_collision_deterministic_resolution(self):
        """
        When both home_a and home_b exist simultaneously (e.g. from container restart during overlap),
        the relay picks the healthier tunnel and marks the other for pending retirement.
        """
        tun_a = {
            "tunnelId": "tun-a-111",
            "appName": "home_a",
            "internalPort": 8088,
            "remainingMinutes": 45.0,
            "status": "ESTABLISHED"
        }
        tun_b = {
            "tunnelId": "tun-b-222",
            "appName": "home_b",
            "internalPort": 8089,
            "remainingMinutes": 15.0,
            "status": "ESTABLISHED"
        }

        def mock_get(app):
            if app == "home_a":
                return tun_a
            if app == "home_b":
                return tun_b
            return None

        self.client_mock.get_established_tunnel.side_effect = mock_get

        act, ret = self.relay.step()

        # Healthier tunnel home_a (45m > 15m) is chosen
        self.assertEqual(act["appName"], "home_a")
        # Dying tunnel home_b (15m <= 25m) placed in pending_retire
        self.assertIsNotNone(ret)
        self.assertEqual(ret["appName"], "home_b")
        self.assertEqual(ret["tunnelId"], "tun-b-222")

    def test_60s_overlap_exact_boundary_timing(self):
        """
        Validates the 60-second overlap lifecycle:
          - T < 60s: dual tunnel coexistence, delete NOT called
          - T >= 60s: silent deletion of retired tunnel
        """
        self.relay.pending_retire = {
            "tunnelId": "tun-old-999",
            "appName": "home_a",
            "retireAt": 1000.0,
            "externalAddr": "154.8.195.220:18088"
        }

        # 1. At T=970.0 (30s remaining) -> No retirement
        with patch("time.time", return_value=970.0):
            res = self.relay.check_and_retire()
            self.assertIsNone(res)
            self.assertIsNotNone(self.relay.pending_retire)
            self.client_mock.delete_tunnel.assert_not_called()

        # 2. At T=999.9 (0.1s remaining) -> No retirement
        with patch("time.time", return_value=999.9):
            res = self.relay.check_and_retire()
            self.assertIsNone(res)
            self.assertIsNotNone(self.relay.pending_retire)
            self.client_mock.delete_tunnel.assert_not_called()

        # 3. At T=1000.0 (exact boundary reached) -> Silent retirement
        with patch("time.time", return_value=1000.0):
            res = self.relay.check_and_retire()
            self.assertIsNotNone(res)
            self.assertEqual(res["tunnelId"], "tun-old-999")
            self.assertIsNone(self.relay.pending_retire)
            self.client_mock.delete_tunnel.assert_called_once_with("tun-old-999")

    def test_h3c_cloud_delete_failure_resilience(self):
        """
        If H3C delete_tunnel API fails during retirement, the state machine logs warning
        and clears pending_retire without raising an unhandled exception.
        """
        self.relay.pending_retire = {
            "tunnelId": "tun-failing-delete",
            "appName": "home_a",
            "retireAt": 1000.0
        }
        self.client_mock.delete_tunnel.side_effect = urllib.error.HTTPError(
            url="https://ucloud.h3c.com", code=500, msg="Internal Error", hdrs={}, fp=None
        )

        with patch("time.time", return_value=1001.0):
            # Must not raise exception
            res = self.relay.check_and_retire()
            self.assertIsNotNone(res)
            self.assertIsNone(self.relay.pending_retire)


if __name__ == "__main__":
    unittest.main(verbosity=2)
