"""
Test suite verifying that missing optional LLM token in production does not
block daemon startup or Tier-1 Rule Engine autonomous execution.
"""

import os
import sys
import unittest

CURRENT_DIR = os.path.dirname(os.path.abspath(__file__))
PARENT_DIR = os.path.dirname(CURRENT_DIR)
sys.path.insert(0, PARENT_DIR)

from config import OpsAgentConfig
from main import is_production, validate_boot_guard, HomeOpsAgent


class TestOfflineFallback(unittest.TestCase):
    def test_missing_llm_token_does_not_block_production_boot(self):
        """
        When ENVIRONMENT=production and NEW_API_TOKEN is empty,
        validate_boot_guard must NOT raise RuntimeError, but allow Tier-1 boot.
        """
        old_env = os.environ.get("ENVIRONMENT")
        old_token = os.environ.get("NEW_API_TOKEN")
        try:
            os.environ["ENVIRONMENT"] = "production"
            os.environ["NEW_API_TOKEN"] = ""
            cfg = OpsAgentConfig(
                new_api_url="http://remote-api:3000/v1",
                new_api_token=""
            )
            self.assertTrue(is_production())
            # Must not raise
            try:
                validate_boot_guard(cfg)
            except RuntimeError as e:
                self.fail(f"validate_boot_guard unexpectedly failed when optional LLM token was missing: {e}")
        finally:
            if old_env is not None:
                os.environ["ENVIRONMENT"] = old_env
            else:
                os.environ.pop("ENVIRONMENT", None)
            if old_token is not None:
                os.environ["NEW_API_TOKEN"] = old_token
            else:
                os.environ.pop("NEW_API_TOKEN", None)

    def test_weak_jwt_secret_still_fails_fast_in_production(self):
        """
        True security vulnerabilities (like a weak JWT secret in production) must still fail fast.
        """
        old_env = os.environ.get("ENVIRONMENT")
        old_jwt = os.environ.get("JWT_SECRET")
        try:
            os.environ["ENVIRONMENT"] = "production"
            os.environ["JWT_SECRET"] = "123456"
            with self.assertRaises(RuntimeError) as cm:
                validate_boot_guard(OpsAgentConfig())
            self.assertIn("JWT_SECRET is weak", str(cm.exception))
        finally:
            if old_env is not None:
                os.environ["ENVIRONMENT"] = old_env
            else:
                os.environ.pop("ENVIRONMENT", None)
            if old_jwt is not None:
                os.environ["JWT_SECRET"] = old_jwt
            else:
                os.environ.pop("JWT_SECRET", None)


if __name__ == "__main__":
    unittest.main()
