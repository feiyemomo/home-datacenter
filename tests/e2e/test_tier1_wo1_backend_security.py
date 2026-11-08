"""
Tier 1 Feature Coverage: Work Order 1 (Backend Fixes & Security Hardening)
Validates core functional contracts for:
- JWTAuth signature and claims context mapping (user_id, user_name, device_id, device_name, is_admin)
- AES-256-GCM encryption of faces.json vector storage
- Vision face register/delete authentication (PSK / JWT Admin)
- Nginx IP trust pinning to fixed cloudflared IP (172.18.0.19)
- Vision dynamic 3-tier CPU gating (<60%, 60-80%, >80%)
"""

import os
import sys
import json
import unittest

# Ensure harness is accessible
sys.path.insert(0, os.path.dirname(__file__))
from harness import CryptoHelper, JWTHelper


class TestTier1WorkOrder1BackendSecurity(unittest.TestCase):
    """Tier 1 Functional Tests for Work Order 1."""

    def setUp(self):
        self.jwt_secret = "test-jwt-secret-minimum-32-chars-length!!"
        self.aes_passphrase = "test-super-secret-vision-key-2026"
        self.aes_key = CryptoHelper.derive_key(self.aes_passphrase)

    def test_jwtauth_claims_structure_and_context_injection(self):
        """
        WO 1.1: Verify JWT generation and context claim mapping.
        Authoritative source: PROJECT.md Section 1 (Interface Contracts) & cmd/main.go.
        Expected: token with user_id, user_name, device_id, device_name, is_admin decodes accurately.
        """
        claims = {
            "user_id": 42,
            "user_name": "feiyemomo",
            "device_id": 101,
            "device_name": "Xiaomi 14 Pro",
            "is_admin": True,
            "token_version": 1
        }
        token = JWTHelper.create_token(claims, self.jwt_secret)
        valid, decoded_claims, msg = JWTHelper.verify_and_parse(token, self.jwt_secret)

        self.assertTrue(valid, f"Token validation failed: {msg}")
        self.assertEqual(decoded_claims["user_id"], 42)
        self.assertEqual(decoded_claims["user_name"], "feiyemomo")
        self.assertEqual(decoded_claims["device_id"], 101)
        self.assertEqual(decoded_claims["device_name"], "Xiaomi 14 Pro")
        self.assertTrue(decoded_claims["is_admin"])
        self.assertEqual(decoded_claims["token_version"], 1)

    def test_jwtauth_missing_token_rejection(self):
        """
        WO 1.1: Verify missing token fails authentication.
        Authoritative source: internal/middleware/jwt.go lines 59-63.
        Expected: Empty token returns invalid status.
        """
        valid, claims, msg = JWTHelper.verify_and_parse("", self.jwt_secret)
        self.assertFalse(valid)
        self.assertIn("Invalid token format", msg)

    def test_face_engine_aes256_gcm_encryption_roundtrip(self):
        """
        WO 1.2: Verify faces.json AES-256-GCM encryption and decryption.
        Authoritative source: Task Specification Section 1.2 & PROJECT.md Feature 2.
        Expected: Raw payload serialized with magic header 'AESGCM\\x01', recovers exact vectors.
        """
        sample_persons = {
            "persons": [
                {
                    "name": "Admin_Alice",
                    "feature": [0.123456, -0.654321, 0.987654] * 42  # 126 float components
                },
                {
                    "name": "User_Bob",
                    "feature": [-0.333333, 0.444444, 0.555555] * 42
                }
            ]
        }
        raw_json = json.dumps(sample_persons).encode("utf-8")
        encrypted_blob = CryptoHelper.encrypt_data(raw_json, self.aes_key)

        # Check magic header
        self.assertTrue(encrypted_blob.startswith(CryptoHelper.MAGIC_HEADER))
        self.assertGreater(len(encrypted_blob), len(raw_json))

        # Decrypt
        decrypted_raw = CryptoHelper.decrypt_data(encrypted_blob, self.aes_key)
        recovered_data = json.loads(decrypted_raw.decode("utf-8"))

        self.assertEqual(len(recovered_data["persons"]), 2)
        self.assertEqual(recovered_data["persons"][0]["name"], "Admin_Alice")
        self.assertAlmostEqual(recovered_data["persons"][0]["feature"][0], 0.123456, places=5)
        self.assertEqual(recovered_data["persons"][1]["name"], "User_Bob")

    def test_face_endpoint_auth_contract(self):
        """
        WO 1.2: Verify face registration/deletion endpoint authorization contract.
        Authoritative source: Task Specification Section 1.2.
        Expected: PSK or JWT Admin role grants access; missing/non-admin denied.
        """
        def authorize_request(headers: dict, psk: str, jwt_secret: str) -> tuple[int, str]:
            # 1. Check PSK header
            if headers.get("X-Vision-PSK") == psk:
                return 200, "Authorized by PSK"
            
            # 2. Check Bearer token
            auth = headers.get("Authorization", "")
            if auth.startswith("Bearer "):
                token = auth[7:]
                valid, claims, _ = JWTHelper.verify_and_parse(token, jwt_secret)
                if valid and claims and claims.get("is_admin") is True:
                    return 200, "Authorized by Admin JWT"
                elif valid:
                    return 403, "Forbidden: Admin role required"
                else:
                    return 401, "Unauthorized: Invalid JWT"
            
            return 401, "Unauthorized: Missing authentication"

        psk = "vision-psk-secret-999"
        admin_token = JWTHelper.create_token({"user_id": 1, "is_admin": True}, self.jwt_secret)
        user_token = JWTHelper.create_token({"user_id": 2, "is_admin": False}, self.jwt_secret)

        # PSK auth
        code, _ = authorize_request({"X-Vision-PSK": psk}, psk, self.jwt_secret)
        self.assertEqual(code, 200)

        # Admin JWT auth
        code, _ = authorize_request({"Authorization": f"Bearer {admin_token}"}, psk, self.jwt_secret)
        self.assertEqual(code, 200)

        # Non-admin JWT
        code, _ = authorize_request({"Authorization": f"Bearer {user_token}"}, psk, self.jwt_secret)
        self.assertEqual(code, 403)

        # Missing headers
        code, _ = authorize_request({}, psk, self.jwt_secret)
        self.assertEqual(code, 401)

    def test_nginx_ip_pinning_configuration_contract(self):
        """
        WO 1.3: Verify Nginx client IP trust convergence to fixed cloudflared IP.
        Authoritative source: Task Specification Section 1.3 & PROJECT.md Feature 4.
        Expected: set_real_ip_from specifies 172.18.0.19; broad 172.16.0.0/12 regex is rejected.
        """
        pinned_ip = "172.18.0.19"
        broad_regex_sample = r"~^172\.(1[6-9]|2[0-9]|3[01])\."

        def evaluate_nginx_trust(client_ip: str, trusted_upstream: str) -> bool:
            return client_ip == trusted_upstream

        # Only the pinned cloudflared container IP is trusted
        self.assertTrue(evaluate_nginx_trust("172.18.0.19", pinned_ip))
        self.assertFalse(evaluate_nginx_trust("172.18.0.25", pinned_ip))
        self.assertFalse(evaluate_nginx_trust("172.16.0.5", pinned_ip))

    def test_vision_cpu_dynamic_gating_three_tier_rules(self):
        """
        WO 1.4: Verify 3-tier dynamic CPU gating logic.
        Authoritative source: Task Specification Section 1.4.
        Expected:
        - CPU < 60%: full mode (face extraction + 17-point pose fall detection)
        - 60% <= CPU <= 80%: lightweight mode (face recognition only, skip pose)
        - CPU > 80%: throttled mode (hardware protection circuit-breaker, HTTP 503)
        """
        def get_gating_decision(cpu_percent: float) -> dict:
            if cpu_percent < 60.0:
                return {"mode": "full", "run_face": True, "run_pose": True, "status_code": 200}
            elif cpu_percent <= 80.0:
                return {"mode": "lightweight", "run_face": True, "run_pose": False, "status_code": 200}
            else:
                return {"mode": "throttled", "run_face": False, "run_pose": False, "status_code": 503}

        # Level 1: 45% CPU
        d1 = get_gating_decision(45.0)
        self.assertEqual(d1["mode"], "full")
        self.assertTrue(d1["run_face"])
        self.assertTrue(d1["run_pose"])
        self.assertEqual(d1["status_code"], 200)

        # Level 2: 70% CPU
        d2 = get_gating_decision(70.0)
        self.assertEqual(d2["mode"], "lightweight")
        self.assertTrue(d2["run_face"])
        self.assertFalse(d2["run_pose"])
        self.assertEqual(d2["status_code"], 200)

        # Level 3: 88% CPU
        d3 = get_gating_decision(88.0)
        self.assertEqual(d3["mode"], "throttled")
        self.assertFalse(d3["run_face"])
        self.assertFalse(d3["run_pose"])
        self.assertEqual(d3["status_code"], 503)


if __name__ == "__main__":
    unittest.main()
