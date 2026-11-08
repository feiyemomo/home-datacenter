"""
Adversarial Stress Test for Ops-Agent Milestone M4:
- Safe Disk Cleanup: 1-hour cooldown enforcement under rapid fire
- Protected volume whitelist and directory traversal resistance
- Free space delta calculation under edge conditions (e.g. concurrent writes)
"""

import os
import shutil
import sys
import time
import unittest
from unittest.mock import MagicMock, patch

# Ensure deploy/ops-agent is on path
CURRENT_DIR = os.path.dirname(os.path.abspath(__file__))
PARENT_DIR = os.path.dirname(CURRENT_DIR)
sys.path.insert(0, PARENT_DIR)

from config import OpsAgentConfig
from rule_engine import TwoTierRuleEngine, SAFE_TEMP_CACHE_DIRS


class TestOpsAgentM4Adversarial(unittest.TestCase):

    def setUp(self):
        self.config = OpsAgentConfig.load()
        self.config.disk_threshold_percent = 85.0
        self.config.disk_cleanup_cooldown_sec = 3600.0
        self.engine = TwoTierRuleEngine(self.config)

    def test_rapid_fire_cooldown_rejection(self):
        """
        Stress test: 10 rapid-fire calls after initial purge must ALL be blocked by cooldown.
        Zero disk pruning commands must be re-executed during cooldown.
        """
        # Call 1: triggers purge
        res1 = self.engine.handle_disk_usage(90.0)
        self.assertEqual(res1["action"], "purge_temp_cache")
        self.assertIn("freed_bytes", res1)
        self.assertGreaterEqual(res1["freed_bytes"], 0)
        self.assertEqual(len(self.engine.cleaned_caches), 1)

        # Calls 2..11: Rapid fire within 1 second -> all rejected
        for i in range(10):
            res = self.engine.handle_disk_usage(90.0 + (i * 0.5))
            self.assertEqual(
                res["action"],
                "cooldown_active",
                f"Call {i+2} failed: cooldown should be active"
            )
            self.assertIn("cooldown_remaining_sec", res)
            self.assertGreaterEqual(res["cooldown_remaining_sec"], 3590)
            self.assertLessEqual(res["cooldown_remaining_sec"], 3600)

        # Assert no additional entries were added to cleaned_caches during cooldown
        self.assertEqual(len(self.engine.cleaned_caches), 1)

        # Advance time by 1800s (30 minutes) -> still blocked
        self.engine.last_disk_cleanup_time = time.time() - 1800
        res_mid = self.engine.handle_disk_usage(92.0)
        self.assertEqual(res_mid["action"], "cooldown_active")
        self.assertGreaterEqual(res_mid["cooldown_remaining_sec"], 1790)

        # Advance time by 3605s (> 1 hour) -> allowed
        self.engine.last_disk_cleanup_time = time.time() - 3605
        res_after = self.engine.handle_disk_usage(92.0)
        self.assertEqual(res_after["action"], "purge_temp_cache")
        self.assertEqual(len(self.engine.cleaned_caches), 2)

    def test_nominal_disk_usage_no_action(self):
        """Usage below threshold (84.9%) must return status_nominal with zero pruning."""
        res = self.engine.handle_disk_usage(84.9)
        self.assertEqual(res["action"], "status_nominal")
        self.assertEqual(res["tokens_used"], 0)
        self.assertEqual(len(self.engine.cleaned_caches), 0)

    def test_protected_volumes_whitelist_and_traversal(self):
        """
        Verify persistent storage paths are protected against accidental pruning.
        """
        protected_paths = [
            "/data",
            "/data/sqlite/app.db",
            "/vol1",
            "/vol1/docker/home-datacenter",
            "/media",
            "/media/frigate/recordings",
            "/mnt",
            "/mnt/nas_backup",
            "/var/lib/docker/volumes",
            "/etc",
            "/etc/nginx/conf.d",
            "/root",
            "/home/user",
            "data-sqlite",
            "data-recordings",
            "backup-state",
            # Windows drive letter paths
            "C:\\data\\app.db",
            "D:/vol1/docker/compose.yaml",
            "C:/etc/passwd",
        ]

        for p in protected_paths:
            self.assertTrue(
                TwoTierRuleEngine.is_protected_volume(p),
                f"Path {p} MUST be recognized as protected persistent volume!"
            )

        # Temporary cache folders must NOT be protected
        unprotected_paths = [
            "/tmp/buildcache",
            "/var/tmp/buildcache",
            "/tmp/docker-build",
        ]
        for p in unprotected_paths:
            self.assertFalse(
                TwoTierRuleEngine.is_protected_volume(p),
                f"Path {p} should be pruneable, not protected!"
            )

    def test_space_delta_under_concurrent_writes(self):
        """
        If concurrent disk write occurs during pruning causing free_after < free_before,
        freed_bytes must clamp to 0 and not produce negative integers.
        """
        mock_usage_before = MagicMock(free=1000000000, used=9000000000, total=10000000000)
        mock_usage_after = MagicMock(free=900000000, used=9100000000, total=10000000000) # Free space decreased!

        with patch("shutil.disk_usage", side_effect=[mock_usage_before, mock_usage_after]):
            res = self.engine.handle_disk_usage(90.0)
            self.assertEqual(res["action"], "purge_temp_cache")
            self.assertEqual(res["freed_bytes"], 0)
            self.assertEqual(res["freed_mb"], 0.0)
            self.assertEqual(res["free_before_bytes"], 1000000000)
            self.assertEqual(res["free_after_bytes"], 900000000)


if __name__ == "__main__":
    unittest.main()
