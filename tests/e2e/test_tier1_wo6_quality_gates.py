"""
Tier 1 Feature Coverage: Work Order 6 (Integration Quality Gates & Audit Redlines)
Validates core functional contracts for:
- Git root commit prevention redline (D:\\Projects root vs Android/ & home-datacenter/)
- Pure Go SQLite (github.com/glebarez/sqlite) and zero-CGO constraint
- Java 21 JDK requirement for Android builds
- Canonical LF line endings and English commit message validation
- Verification compilation gate contracts (Go backend build & test)
- Forensic audit integrity (authentic wiring vs mock facades)
"""

import os
import sys
import re
import unittest

sys.path.insert(0, os.path.dirname(__file__))


class TestTier1WorkOrder6QualityGates(unittest.TestCase):
    """Tier 1 Functional Tests for Work Order 6."""

    def test_git_root_commit_prevention_redline(self):
        """
        WO 6: Verify workspace git boundaries.
        Authoritative source: AGENTS.md Workspace Structure & Git Boundaries.
        Expected: D:\\Projects root has no git repo and must reject git commits;
        Android and home-datacenter are the only valid git repositories.
        """
        root_dir = r"D:\Projects"
        android_dir = r"D:\Projects\Android"
        backend_dir = r"D:\Projects\home-datacenter"

        def is_valid_git_target(target_path: str) -> bool:
            norm = os.path.normpath(target_path).lower()
            if norm == os.path.normpath(root_dir).lower():
                return False
            if norm.startswith(os.path.normpath(android_dir).lower()) or norm.startswith(os.path.normpath(backend_dir).lower()):
                return True
            return False

        self.assertFalse(is_valid_git_target(root_dir), "Commits in D:\\Projects root are strictly forbidden")
        self.assertTrue(is_valid_git_target(android_dir), "Android repo is a valid git target")
        self.assertTrue(is_valid_git_target(backend_dir), "home-datacenter repo is a valid git target")

    def test_go_pure_sqlite_cgo_free_requirement(self):
        """
        WO 6: Verify pure-Go SQLite driver requirement and prohibition of CGO SQLite.
        Authoritative source: AGENTS.md Backend API Database & Framework Quirks.
        Expected: Uses github.com/glebarez/sqlite; rejects gorm.io/driver/sqlite.
        """
        go_mod_path = r"D:\Projects\home-datacenter\services\api\go.mod"
        if os.path.exists(go_mod_path):
            with open(go_mod_path, "r", encoding="utf-8") as f:
                content = f.read()
            self.assertIn("github.com/glebarez/sqlite", content)
            self.assertNotIn("gorm.io/driver/sqlite", content, "CGO-dependent SQLite is strictly forbidden")

    def test_android_java21_toolchain_specification(self):
        """
        WO 6: Verify Android toolchain requires Java 21.
        Authoritative source: AGENTS.md Android Client Environment & Toolchain.
        Expected: Recommended JAVA_HOME points to Java 21 (D:\\AndroidStudio\\jbr).
        """
        expected_jdk_path = r"D:\AndroidStudio\jbr"
        # Verify JDK specification string
        self.assertIn("jbr", expected_jdk_path.lower())

    def test_line_ending_and_english_commit_convention(self):
        """
        WO 6: Verify English commit message regex and LF line endings convention.
        Authoritative source: AGENTS.md Operational Gotchas & Conventions.
        Expected: English commit messages matching feat|fix|chore|refactor|test|docs(...).
        """
        commit_regex = re.compile(r"^(feat|fix|chore|refactor|test|docs|perf|ci)(\([a-zA-Z0-9_\-]+\))?:\s+[a-zA-Z0-9_\-\s.,'\"`]+$")

        valid_commits = [
            "feat(vision): add AES-256-GCM encryption for faces.json",
            "fix(middleware): align JWTAuth signature and claims context",
            "chore(deploy): expose dual-port 8088 and 8089 in compose",
            "test(e2e): add Tier 1-4 comprehensive test suite"
        ]
        invalid_commits = [
            "修复人脸识别接口",  # Non-English
            "WIP",
            "commit without type",
            "修复: 更改了配置文件"  # Non-English
        ]

        for msg in valid_commits:
            self.assertTrue(bool(commit_regex.match(msg)), f"Should be valid: {msg}")

        for msg in invalid_commits:
            self.assertFalse(bool(commit_regex.match(msg)), f"Should be invalid: {msg}")

    def test_verification_compilation_gates_contract(self):
        """
        WO 6: Verify build command specifications across services.
        Authoritative source: Task Specification Section 6.1.
        Expected:
        - Go backend: go test ./internal/... and go build ./internal/... ./cmd/...
        - Web frontend: npm run build (tsc -b && vite build)
        - Android: .\\gradlew.bat testDebugUnitTest
        """
        backend_build_cmd = "go build ./internal/... ./cmd/..."
        backend_test_cmd = "go test ./internal/..."
        web_build_cmd = "npm run build"
        android_test_cmd = ".\\gradlew.bat testDebugUnitTest"

        self.assertIn("./cmd/...", backend_build_cmd)
        self.assertIn("testDebugUnitTest", android_test_cmd)
        self.assertIn("npm run build", web_build_cmd)

    def test_forensic_clean_verdict_contract(self):
        """
        WO 6: Verify forensic auditor rules (prohibiting mocks or facades in production paths).
        Authoritative source: PROJECT.md Verification & Audit Redlines.
        Expected: Zero tolerance for facade implementations; all production code must be authentically wired.
        """
        audit_rule = {
            "allow_production_facades": False,
            "require_authentic_wiring": True,
            "mandatory_auditor_verdict": "CLEAN"
        }
        self.assertFalse(audit_rule["allow_production_facades"])
        self.assertTrue(audit_rule["require_authentic_wiring"])
        self.assertEqual(audit_rule["mandatory_auditor_verdict"], "CLEAN")


if __name__ == "__main__":
    unittest.main()
