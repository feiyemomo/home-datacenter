#!/usr/bin/env python3
"""
Unit and Negative Tests for CI Govulncheck Security Gate (gate_govulncheck.py)
Verifies fail-closed behavior on truncated output, malformed JSON, 3rd-party module exemptions, and expired entries.
"""

import json
import os
import tempfile
import unittest
from gate_govulncheck import parse_govulncheck_json_stream, verify_govulncheck


class TestGateGovulncheck(unittest.TestCase):
    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory()
        self.allowlist_path = os.path.join(self.temp_dir.name, "allowlist.json")
        self.valid_allowlist = {
            "version": "1.0",
            "exemptions": [
                {
                    "id": "GO-2026-6859",
                    "module": "stdlib",
                    "package": "net/http",
                    "summary": "HTTP/1 server crash test",
                    "reason": "Audited and tested",
                    "risk": "MEDIUM",
                    "approved_by": "Security-Consortium",
                    "expires_at": "2030-01-01T00:00:00Z"
                },
                {
                    "id": "GO-2026-9999",
                    "module": "github.com/evil/pkg",
                    "package": "github.com/evil/pkg",
                    "summary": "Third party vuln attempting bypass",
                    "reason": "Invalid attempt",
                    "risk": "HIGH",
                    "approved_by": "Security-Consortium",
                    "expires_at": "2030-01-01T00:00:00Z"
                }
            ]
        }
        with open(self.allowlist_path, "w", encoding="utf-8") as f:
            json.dump(self.valid_allowlist, f)

    def tearDown(self):
        self.temp_dir.cleanup()

    def test_valid_stdlib_allowlisted(self):
        raw = json.dumps({
            "finding": {
                "osv": "GO-2026-6859",
                "fixed_version": "v1.26.5",
                "trace": [
                    {"module": "stdlib", "package": "net/http", "function": "ServeHTTP"},
                    {"module": "home-datacenter-api", "package": "home-datacenter-api/internal/server"}
                ]
            }
        })
        success, violations, approved = verify_govulncheck(self.allowlist_path, raw)
        self.assertTrue(success, f"Expected success, got violations: {violations}")
        self.assertEqual(len(violations), 0)
        self.assertEqual(len(approved), 1)
        self.assertEqual(approved[0][0], "GO-2026-6859")

    def test_truncated_json_fails_closed(self):
        raw = '{"finding": {"osv": "GO-2026-6859", "trace": ['
        with self.assertRaises(ValueError) as ctx:
            parse_govulncheck_json_stream(raw)
        self.assertIn("FAIL-CLOSED", str(ctx.exception))

        success, violations, _ = verify_govulncheck(self.allowlist_path, raw)
        self.assertFalse(success)
        self.assertTrue(any("FAIL-CLOSED" in v for v in violations))

    def test_malformed_json_fails_closed(self):
        raw = '{"finding": {"osv": "GO-2026-6859", invalid_json}}'
        with self.assertRaises(ValueError) as ctx:
            parse_govulncheck_json_stream(raw)
        self.assertIn("FAIL-CLOSED", str(ctx.exception))

        success, violations, _ = verify_govulncheck(self.allowlist_path, raw)
        self.assertFalse(success)

    def test_corrupted_json_fragment_fails_closed(self):
        raw = 'plain text log with broken { fragment\n{"config": {}}'
        with self.assertRaises(ValueError) as ctx:
            parse_govulncheck_json_stream(raw)
        self.assertIn("Corrupted JSON fragment", str(ctx.exception))

    def test_empty_stream_fails_closed(self):
        with self.assertRaises(ValueError) as ctx:
            parse_govulncheck_json_stream("")
        self.assertIn("empty", str(ctx.exception))

        success, violations, _ = verify_govulncheck(self.allowlist_path, "   \n  ")
        self.assertFalse(success)

    def test_third_party_module_fails_closed(self):
        raw = json.dumps({
            "finding": {
                "osv": "GO-2026-9999",
                "fixed_version": "v2.0.0",
                "trace": [
                    {"module": "github.com/evil/pkg", "package": "github.com/evil/pkg", "function": "Evil"},
                    {"module": "home-datacenter-api", "package": "home-datacenter-api/internal/ops"}
                ]
            }
        })
        success, violations, _ = verify_govulncheck(self.allowlist_path, raw)
        self.assertFalse(success)
        self.assertTrue(any("UNAUTHORIZED VULNERABILITY IN 3RD-PARTY MODULE" in v for v in violations))

    def test_disguised_third_party_module_rejected(self):
        raw = json.dumps({
            "finding": {
                "osv": "GO-2026-6859",
                "fixed_version": "v1.0.1",
                "trace": [
                    {"module": "github.com/gin-gonic/gin", "package": "github.com/gin-gonic/gin", "function": "Run"},
                    {"module": "home-datacenter-api", "package": "home-datacenter-api/cmd"}
                ]
            }
        })
        success, violations, _ = verify_govulncheck(self.allowlist_path, raw)
        self.assertFalse(success)
        self.assertTrue(any("UNAUTHORIZED VULNERABILITY IN 3RD-PARTY MODULE" in v for v in violations))

    def test_unallowlisted_stdlib_finding_fails_closed(self):
        raw = json.dumps({
            "finding": {
                "osv": "GO-2026-0000-UNAPPROVED",
                "fixed_version": "v1.26.5",
                "trace": [
                    {"module": "stdlib", "package": "net/http", "function": "Get"},
                    {"module": "home-datacenter-api", "package": "home-datacenter-api/internal/server"}
                ]
            }
        })
        success, violations, _ = verify_govulncheck(self.allowlist_path, raw)
        self.assertFalse(success)
        self.assertTrue(any("UNAUTHORIZED VULNERABILITY: GO-2026-0000-UNAPPROVED" in v for v in violations))

    def test_package_mismatch_fails_closed(self):
        raw = json.dumps({
            "finding": {
                "osv": "GO-2026-6859",
                "fixed_version": "v1.26.5",
                "trace": [
                    {"module": "stdlib", "package": "crypto/tls", "function": "Dial"},
                    {"module": "home-datacenter-api", "package": "home-datacenter-api/internal/server"}
                ]
            }
        })
        success, violations, _ = verify_govulncheck(self.allowlist_path, raw)
        self.assertFalse(success)
        self.assertTrue(any("PACKAGE MISMATCH" in v for v in violations))

    def test_expired_allowlist_entry_fails_closed(self):
        expired_allowlist_path = os.path.join(self.temp_dir.name, "expired.json")
        with open(expired_allowlist_path, "w", encoding="utf-8") as f:
            json.dump({
                "version": "1.0",
                "exemptions": [
                    {
                        "id": "GO-2020-0001",
                        "module": "stdlib",
                        "package": "net/http",
                        "summary": "Expired vuln",
                        "reason": "Old",
                        "risk": "LOW",
                        "approved_by": "Security-Consortium",
                        "expires_at": "2020-01-01T00:00:00Z"
                    }
                ]
            }, f)
        raw = json.dumps({
            "finding": {
                "osv": "GO-2020-0001",
                "fixed_version": "v1.14.0",
                "trace": [
                    {"module": "stdlib", "package": "net/http", "function": "ServeHTTP"},
                    {"module": "home-datacenter-api", "package": "home-datacenter-api/internal/server"}
                ]
            }
        })
        success, violations, _ = verify_govulncheck(expired_allowlist_path, raw)
        self.assertFalse(success)
        self.assertTrue(any("expired" in v for v in violations))

    def test_duplicate_exemption_id_fails_closed(self):
        dup_allowlist_path = os.path.join(self.temp_dir.name, "dup.json")
        with open(dup_allowlist_path, "w", encoding="utf-8") as f:
            json.dump({
                "version": "1.0",
                "exemptions": [
                    {
                        "id": "GO-2026-1111",
                        "module": "stdlib",
                        "package": "net/http",
                        "reason": "Test 1",
                        "risk": "LOW",
                        "approved_by": "sec",
                        "expires_at": "2030-01-01T00:00:00Z"
                    },
                    {
                        "id": "GO-2026-1111",
                        "module": "stdlib",
                        "package": "net/http",
                        "reason": "Test 2 dup",
                        "risk": "LOW",
                        "approved_by": "sec",
                        "expires_at": "2030-01-01T00:00:00Z"
                    }
                ]
            }, f)
        success, violations, _ = verify_govulncheck(dup_allowlist_path, '{"finding":{"osv":"GO-2026-1111"}}')
        self.assertFalse(success)
        self.assertTrue(any("Duplicate exemption ID" in v for v in violations))

    def test_missing_required_field_fails_closed(self):
        bad_path = os.path.join(self.temp_dir.name, "missing_field.json")
        with open(bad_path, "w", encoding="utf-8") as f:
            json.dump({
                "version": "1.0",
                "exemptions": [
                    {
                        "id": "GO-2026-2222",
                        "module": "stdlib",
                        "package": "net/http",
                        "reason": "", # empty reason
                        "risk": "LOW",
                        "approved_by": "sec",
                        "expires_at": "2030-01-01T00:00:00Z"
                    }
                ]
            }, f)
        success, violations, _ = verify_govulncheck(bad_path, '{"finding":{"osv":"GO-2026-2222"}}')
        self.assertFalse(success)
        self.assertTrue(any("missing required field" in v for v in violations))

    def test_non_utc_expiration_fails_closed(self):
        bad_path = os.path.join(self.temp_dir.name, "non_utc.json")
        with open(bad_path, "w", encoding="utf-8") as f:
            json.dump({
                "version": "1.0",
                "exemptions": [
                    {
                        "id": "GO-2026-3333",
                        "module": "stdlib",
                        "package": "net/http",
                        "reason": "Valid reason",
                        "risk": "LOW",
                        "approved_by": "sec",
                        "expires_at": "2030-01-01T00:00:00+08:00" # Not ending in Z
                    }
                ]
            }, f)
        success, violations, _ = verify_govulncheck(bad_path, '{"finding":{"osv":"GO-2026-3333"}}')
        self.assertFalse(success)
        self.assertTrue(any("must be UTC ISO 8601 ending with 'Z'" in v for v in violations))

    def test_spoofed_stdlib_package_fails_closed(self):
        bad_path = os.path.join(self.temp_dir.name, "spoof.json")
        with open(bad_path, "w", encoding="utf-8") as f:
            json.dump({
                "version": "1.0",
                "exemptions": [
                    {
                        "id": "GO-2026-4444",
                        "module": "stdlib",
                        "package": "github.com/gin-gonic/gin", # spoofed 3rd party in stdlib module
                        "reason": "Sneaky attempt",
                        "risk": "HIGH",
                        "approved_by": "sec",
                        "expires_at": "2030-01-01T00:00:00Z"
                    }
                ]
            }, f)
        success, violations, _ = verify_govulncheck(bad_path, '{"finding":{"osv":"GO-2026-4444"}}')
        self.assertFalse(success)
        self.assertTrue(any("Spoofed stdlib exemption" in v for v in violations))

    def test_traceless_finding_fails_closed(self):
        raw = json.dumps({
            "finding": {
                "osv": "GO-2026-9999-NOTRACE"
            }
        })
        success, violations, _ = verify_govulncheck(self.allowlist_path, raw)
        self.assertFalse(success)
        self.assertTrue(any("UNAUTHORIZED VULNERABILITY IN 3RD-PARTY MODULE" in v for v in violations))


if __name__ == "__main__":
    unittest.main()
