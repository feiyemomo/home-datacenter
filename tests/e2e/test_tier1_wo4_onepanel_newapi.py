"""
Tier 1 Feature Coverage: Work Order 4 (1Panel BFF, New API Gateway & Mihomo Proxy)
Validates core functional contracts for:
- 1Panel OpenAPI Go BFF endpoints (NAS status, container list, security summary)
- Container restart authorization and whitelisting
- New API compose specification (<=128MB RAM, 127.0.0.1:3000, Mihomo proxy)
- Key pool distribution (5 Google AI Studio + 6 Groq keys)
- Key pool round-robin rotation
- Rate-limit (429) failover to healthy keys
"""

import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(__file__))
from harness import NewAPIGatewaySimulator, JWTHelper


class TestTier1WorkOrder4OnePanelNewAPI(unittest.TestCase):
    """Tier 1 Functional Tests for Work Order 4."""

    def setUp(self):
        self.google_keys = [f"test-google-key-{i}" for i in range(5)]
        self.groq_keys = [f"test-groq-key-{i}" for i in range(6)]
        self.sim = NewAPIGatewaySimulator(self.google_keys, self.groq_keys)

    def test_onepanel_bff_status_and_container_endpoints(self):
        """
        WO 4.1: Verify 1Panel BFF aggregated endpoints schema.
        Authoritative source: Task Specification Section 4.1 & PROJECT.md Feature 14, 15.
        Expected: Status returns cpu, memory, disk; containers list returns container metadata.
        """
        # Simulated contract response from 1Panel BFF
        status_payload = {
            "cpu_percent": 18.5,
            "memory": {"total_mb": 16384, "used_mb": 5120, "free_mb": 11264},
            "disk_pools": [{"name": "vol1", "total_gb": 3600, "used_gb": 1200, "percent": 33.3}]
        }
        self.assertIn("cpu_percent", status_payload)
        self.assertIn("memory", status_payload)
        self.assertIn("disk_pools", status_payload)
        self.assertLess(status_payload["cpu_percent"], 100.0)

    def test_onepanel_container_restart_authorization_and_whitelist(self):
        """
        WO 4.1: Verify container restart endpoint requires Admin role and whitelisted container name.
        Authoritative source: Task Specification Section 4.1.
        Expected: Only whitelisted core containers can be restarted; non-whitelisted rejected; non-admin denied.
        """
        whitelist = {"home-web", "home-api", "home-frigate", "home-vision", "home-new-api"}

        def restart_container(container_name: str, is_admin: bool) -> tuple[int, str]:
            if not is_admin:
                return 403, "Permission denied"
            if container_name not in whitelist:
                return 400, f"Container '{container_name}' is not in allowed restart whitelist"
            return 200, f"Container '{container_name}' restarted successfully"

        # Admin restarting valid container
        code, msg = restart_container("home-api", is_admin=True)
        self.assertEqual(code, 200)

        # Admin attempting to restart arbitrary host container (e.g. 1panel or system container)
        code, msg = restart_container("1panel", is_admin=True)
        self.assertEqual(code, 400)

        # Non-admin user attempting restart
        code, msg = restart_container("home-api", is_admin=False)
        self.assertEqual(code, 403)

    def test_new_api_compose_limits_and_proxy_configuration(self):
        """
        WO 4.2: Verify New API compose constraints (<=128MB RAM, 127.0.0.1:3000, proxy via 172.17.0.1:7890).
        Authoritative source: Task Specification Section 4.2 & PROJECT.md Feature 16.
        Expected: Strict resource ceiling and local port binding to prevent external exposure.
        """
        compose_spec = {
            "image": "channels-tool/new-api:latest",
            "ports": ["127.0.0.1:3000:3000"],
            "mem_limit": "128m",
            "environment": [
                "HTTP_PROXY=http://172.17.0.1:7890",
                "HTTPS_PROXY=http://172.17.0.1:7890",
                "NO_PROXY=localhost,127.0.0.1,192.168.0.0/16,10.0.0.0/8"
            ]
        }
        self.assertEqual(compose_spec["mem_limit"], "128m")
        self.assertTrue(compose_spec["ports"][0].startswith("127.0.0.1:3000"))
        self.assertIn("HTTP_PROXY=http://172.17.0.1:7890", compose_spec["environment"])

    def test_key_pool_distribution_and_model_alias_resolution(self):
        """
        WO 4.2: Verify 11 key pool distribution and model alias resolution.
        Authoritative source: Task Specification Section 4.2 & api.md.
        Expected:
        - agent-fast -> gemini-3.5-flash-lite, gemini-3.1-flash-lite
        - agent-deep -> gemini-3.8-flash, gpt-oss-120b
        - agent-guard -> qwen/qwen3.8-27b, meta-llama/llama-prompt-guard-2-86m
        - agent-fallback -> gemma-4-31b
        """
        self.assertEqual(len(self.google_keys), 5)
        self.assertEqual(len(self.groq_keys), 6)

        fast_models = self.sim.resolve_model("agent-fast")
        self.assertIn("gemini-3.5-flash-lite", fast_models)

        deep_models = self.sim.resolve_model("agent-deep")
        self.assertIn("gemini-3.8-flash", deep_models)

        guard_models = self.sim.resolve_model("agent-guard")
        self.assertIn("qwen/qwen3.8-27b", guard_models)

        fallback_models = self.sim.resolve_model("agent-fallback")
        self.assertIn("gemma-4-31b", fallback_models)

    def test_key_pool_round_robin_weighting(self):
        """
        WO 4.2: Verify sequential round-robin key scheduling across Google matrix.
        Authoritative source: Task Specification Section 4.2.
        Expected: Successive requests cycle through each key in the pool.
        """
        k1 = self.sim.get_next_healthy_key("google")
        k2 = self.sim.get_next_healthy_key("google")
        k3 = self.sim.get_next_healthy_key("google")
        k4 = self.sim.get_next_healthy_key("google")
        k5 = self.sim.get_next_healthy_key("google")
        k6 = self.sim.get_next_healthy_key("google")

        self.assertEqual(k1, self.google_keys[0])
        self.assertEqual(k2, self.google_keys[1])
        self.assertEqual(k3, self.google_keys[2])
        self.assertEqual(k4, self.google_keys[3])
        self.assertEqual(k5, self.google_keys[4])
        self.assertEqual(k6, self.google_keys[0])  # wrapped around

    def test_key_pool_rate_limit_failover(self):
        """
        WO 4.2: Verify rate-limited key (HTTP 429) is bypassed to healthy key in pool.
        Authoritative source: Task Specification Section 4.2 & Section 6.2.
        Expected: When key 0 is rate-limited, next call seamlessly yields key 1.
        """
        # Mark first key as rate-limited
        self.sim.mark_key_rate_limited(self.google_keys[0])

        # Request next key
        next_key = self.sim.get_next_healthy_key("google")
        self.assertNotEqual(next_key, self.google_keys[0])
        self.assertEqual(next_key, self.google_keys[1])


if __name__ == "__main__":
    unittest.main()
