#!/usr/bin/env python3
"""
Home Datacenter Unified E2E Test Runner
Executes requirement-driven opaque-box test suites across Tiers 1-4 and Work Orders 1-6.
Supports filtering by Tier (--tier), Work Order (--wo), and generating JSON/Markdown reports.
"""

import os
import sys
import time
import json
import argparse
import unittest
from typing import List, Dict, Any

# Ensure tests/e2e is on path
CURRENT_DIR = os.path.dirname(os.path.abspath(__file__))
if CURRENT_DIR not in sys.path:
    sys.path.insert(0, CURRENT_DIR)

import test_tier1_wo1_backend_security
import test_tier1_wo2_android_player
import test_tier1_wo3_h3c_pingpong
import test_tier1_wo4_onepanel_newapi
import test_tier1_wo5_ops_agent_copilot
import test_tier1_wo6_quality_gates
import test_tier2_boundary_corner_cases
import test_tier3_cross_feature
import test_tier4_real_world_workloads


TEST_MODULE_MAPPING = {
    "tier1_wo1": (1, 1, test_tier1_wo1_backend_security.TestTier1WorkOrder1BackendSecurity),
    "tier1_wo2": (1, 2, test_tier1_wo2_android_player.TestTier1WorkOrder2AndroidPlayer),
    "tier1_wo3": (1, 3, test_tier1_wo3_h3c_pingpong.TestTier1WorkOrder3H3CPingPong),
    "tier1_wo4": (1, 4, test_tier1_wo4_onepanel_newapi.TestTier1WorkOrder4OnePanelNewAPI),
    "tier1_wo5": (1, 5, test_tier1_wo5_ops_agent_copilot.TestTier1WorkOrder5OpsAgentCopilot),
    "tier1_wo6": (1, 6, test_tier1_wo6_quality_gates.TestTier1WorkOrder6QualityGates),
    "tier2": (2, None, test_tier2_boundary_corner_cases.TestTier2BoundaryAndCornerCases),
    "tier3": (3, None, test_tier3_cross_feature.TestTier3CrossFeatureInteractions),
    "tier4": (4, None, test_tier4_real_world_workloads.TestTier4RealWorldWorkloads),
}


class E2ETestResult(unittest.TestResult):
    """Custom TestResult collecting detailed execution metrics for reporting."""

    def __init__(self):
        super().__init__()
        self.test_records = []
        self._start_times = {}

    def startTest(self, test):
        super().startTest(test)
        self._start_times[test.id()] = time.time()

    def addSuccess(self, test):
        super().addSuccess(test)
        duration = time.time() - self._start_times.get(test.id(), time.time())
        self.test_records.append({
            "id": test.id(),
            "name": test._testMethodName,
            "doc": (test._testMethodDoc or "").strip().split("\n")[0],
            "status": "PASSED",
            "duration_sec": round(duration, 4),
            "error": None
        })

    def addFailure(self, test, err):
        super().addFailure(test, err)
        duration = time.time() - self._start_times.get(test.id(), time.time())
        self.test_records.append({
            "id": test.id(),
            "name": test._testMethodName,
            "doc": (test._testMethodDoc or "").strip().split("\n")[0],
            "status": "FAILED",
            "duration_sec": round(duration, 4),
            "error": self._exc_info_to_string(err, test)
        })

    def addError(self, test, err):
        super().addError(test, err)
        duration = time.time() - self._start_times.get(test.id(), time.time())
        self.test_records.append({
            "id": test.id(),
            "name": test._testMethodName,
            "doc": (test._testMethodDoc or "").strip().split("\n")[0],
            "status": "ERROR",
            "duration_sec": round(duration, 4),
            "error": self._exc_info_to_string(err, test)
        })


def build_suite(filter_tier: int = None, filter_wo: int = None) -> unittest.TestSuite:
    suite = unittest.TestSuite()
    loader = unittest.TestLoader()

    for key, (tier, wo, test_class) in TEST_MODULE_MAPPING.items():
        if filter_tier is not None and tier != filter_tier:
            continue
        if filter_wo is not None:
            if tier == 1 and wo != filter_wo:
                continue
            if tier == 2:
                # Filter tier 2 test methods by work order prefix (e.g. test_wo1_*)
                wo_prefix = f"test_wo{filter_wo}_"
                for method_name in loader.getTestCaseNames(test_class):
                    if method_name.startswith(wo_prefix):
                        suite.addTest(test_class(method_name))
                continue
            if tier in (3, 4):
                # Tiers 3 & 4 are cross-feature/system workloads
                continue

        suite.addTests(loader.loadTestsFromTestCase(test_class))

    return suite


def run_e2e(tier: int = None, wo: int = None, verbose: bool = False, json_out: str = None, md_out: str = None) -> int:
    suite = build_suite(filter_tier=tier, filter_wo=wo)
    total_tests = suite.countTestCases()

    print("======================================================================")
    print("      HOME DATACENTER SUPER ARCHITECTURE E2E TEST RUNNER")
    print("======================================================================")
    filter_info = []
    if tier:
        filter_info.append(f"Tier={tier}")
    if wo:
        filter_info.append(f"WorkOrder={wo}")
    filters_str = ", ".join(filter_info) if filter_info else "All Tiers (1-4) & Work Orders (1-6)"
    print(f"Scope: {filters_str}")
    print(f"Total Test Cases Queued: {total_tests}")
    print("----------------------------------------------------------------------")

    result = E2ETestResult()
    start_time = time.time()
    suite.run(result)
    total_duration = time.time() - start_time

    passed = len(result.test_records) - len(result.failures) - len(result.errors)
    failed = len(result.failures)
    errors = len(result.errors)

    if verbose:
        print("\n--- Test Execution Details ---")
        for rec in result.test_records:
            icon = "[PASS]" if rec["status"] == "PASSED" else "[FAIL]"
            print(f"{icon} {rec['name']} ({rec['duration_sec']}s) - {rec['doc']}")

    print("======================================================================")
    print(f"Test Run Completed in {total_duration:.3f}s")
    print(f"Summary: Total: {total_tests} | Passed: {passed} | Failed: {failed} | Errors: {errors}")
    print(f"Success Rate: {(passed / total_tests * 100.0) if total_tests else 0.0:.1f}%")
    print("======================================================================")

    if result.failures or result.errors:
        print("\nFailures / Errors:")
        for rec in result.test_records:
            if rec["status"] in ("FAILED", "ERROR"):
                print(f"\n[-] {rec['name']}:\n{rec['error']}")

    # Generate JSON report if requested
    if json_out:
        report_data = {
            "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "total": total_tests,
            "passed": passed,
            "failed": failed,
            "errors": errors,
            "duration_sec": round(total_duration, 4),
            "records": result.test_records
        }
        with open(json_out, "w", encoding="utf-8") as f:
            json.dump(report_data, f, indent=2, ensure_ascii=False)
        print(f"JSON report saved to: {json_out}")

    # Generate Markdown report if requested
    if md_out:
        with open(md_out, "w", encoding="utf-8") as f:
            f.write(f"# E2E Test Execution Report\n\n")
            f.write(f"- **Executed At**: {time.strftime('%Y-%m-%d %H:%M:%S UTC', time.gmtime())}\n")
            f.write(f"- **Total Tests**: {total_tests}\n")
            f.write(f"- **Passed**: {passed}\n")
            f.write(f"- **Failed**: {failed}\n")
            f.write(f"- **Success Rate**: {(passed / total_tests * 100.0) if total_tests else 0.0:.1f}%\n")
            f.write(f"- **Duration**: {total_duration:.3f} seconds\n\n")
            f.write("## Test Cases Summary\n\n")
            f.write("| Status | Test Name | Duration | Description |\n")
            f.write("| :--- | :--- | :--- | :--- |\n")
            for rec in result.test_records:
                status_badge = "✅ PASS" if rec["status"] == "PASSED" else "❌ FAIL"
                f.write(f"| {status_badge} | `{rec['name']}` | {rec['duration_sec']}s | {rec['doc']} |\n")
        print(f"Markdown report saved to: {md_out}")

    return 0 if (failed == 0 and errors == 0) else 1


def main():
    parser = argparse.ArgumentParser(description="Home Datacenter E2E Test Suite Runner")
    parser.add_argument("--tier", type=int, choices=[1, 2, 3, 4], help="Filter execution by test tier (1, 2, 3, 4)")
    parser.add_argument("--wo", type=int, choices=[1, 2, 3, 4, 5, 6], help="Filter execution by work order (1-6)")
    parser.add_argument("-v", "--verbose", action="store_true", help="Print verbose test results")
    parser.add_argument("--json-report", type=str, help="Path to write JSON execution report")
    parser.add_argument("--md-report", type=str, help="Path to write Markdown execution report")
    args = parser.parse_args()

    exit_code = run_e2e(
        tier=args.tier,
        wo=args.wo,
        verbose=args.verbose,
        json_out=args.json_report,
        md_out=args.md_report
    )
    sys.exit(exit_code)


if __name__ == "__main__":
    main()
