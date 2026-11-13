#!/usr/bin/env python3
"""
Home Datacenter CI Security Gate: Govulncheck Audited Exemption Verifier
Fails closed if any unallowlisted, non-stdlib, expired, truncated, or malformed vulnerability finding is detected.
"""

import argparse
import datetime
import json
import os
import subprocess
import sys


def parse_govulncheck_json_stream(stream_text: str):
    """
    Parses a multiline JSON stream output by govulncheck -format json.
    Fails closed if the stream is empty, truncated, or contains malformed JSON.
    Returns a dict mapping OSV ID -> finding details.
    """
    if not stream_text or not stream_text.strip():
        raise ValueError("FAIL-CLOSED: Govulncheck output stream is empty")

    lines = stream_text.strip().splitlines()
    active_findings = {}
    parsed_any = False

    for line_num, line in enumerate(lines, 1):
        line_str = line.strip()
        if not line_str:
            continue
        if line_str.startswith('{'):
            try:
                obj = json.loads(line_str)
            except json.JSONDecodeError as err:
                raise ValueError(f"FAIL-CLOSED: Malformed or truncated JSON on line {line_num}: {err}") from err

            parsed_any = True
            if "finding" in obj:
                fnd = obj["finding"]
                osv_id = fnd.get("osv")
                traces = fnd.get("trace", [])
                if not traces:
                    # Vulnerabilities without trace cannot be assumed safe; flag as missing_trace
                    if osv_id:
                        if osv_id not in active_findings:
                            active_findings[osv_id] = {
                                "modules": {"missing_trace"},
                                "packages": set(),
                                "symbols": set(),
                                "fixed_version": fnd.get("fixed_version", ""),
                            }
                else:
                    has_root_trace = any(
                        t.get("module") == "home-datacenter-api" or
                        (t.get("package", "").startswith("home-datacenter-api/"))
                        for t in traces
                    )
                    if osv_id and has_root_trace:
                        modules = set()
                        packages = set()
                        symbols = set()
                        # In govulncheck protocol, traces[0] is the vulnerable leaf frame
                        if traces and len(traces) > 0:
                            leaf_mod = traces[0].get("module")
                            if leaf_mod and leaf_mod != "home-datacenter-api":
                                modules.add(leaf_mod)
                        for t in traces:
                            p = t.get("package", "")
                            f = t.get("function")
                            if p and not p.startswith("home-datacenter-api"):
                                packages.add(p)
                            if f:
                                symbols.add(f)
                        if osv_id not in active_findings:
                            active_findings[osv_id] = {
                                "modules": set(),
                                "packages": set(),
                                "symbols": set(),
                                "fixed_version": fnd.get("fixed_version", ""),
                            }
                        active_findings[osv_id]["modules"].update(modules)
                        active_findings[osv_id]["packages"].update(packages)
                        active_findings[osv_id]["symbols"].update(symbols)
        else:
            # Lines not starting with '{' should only be pure textual build logs (e.g. "go: downloading...")
            # If they contain JSON delimiter braces, it indicates a corrupted or broken JSON fragment
            if '{' in line_str or '}' in line_str:
                raise ValueError(f"FAIL-CLOSED: Corrupted JSON fragment on line {line_num}: {line_str}")

    if not parsed_any:
        raise ValueError("FAIL-CLOSED: No valid JSON records found in govulncheck output")

    return active_findings


def verify_govulncheck(allowlist_path: str, raw_output: str) -> tuple[bool, list[str], list[tuple[str, str, str]]]:
    """
    Verifies govulncheck raw output against allowlist_path.
    Returns (success, violations, approved).
    """
    if not os.path.isfile(allowlist_path):
        return False, [f"Govulncheck allowlist file not found: {allowlist_path}"], []

    with open(allowlist_path, "r", encoding="utf-8") as f:
        allowlist_data = json.load(f)

    raw_exemptions = allowlist_data.get("exemptions", [])
    exemptions = {}
    seen_ids = set()
    now = datetime.datetime.now(datetime.timezone.utc)

    # Validate schema, mandatory fields, anti-duplicate, and expiration for all entries
    for item in raw_exemptions:
        osv_id = item.get("id")
        if not osv_id or not str(osv_id).strip():
            return False, ["Allowlist exemption entry missing required 'id'"], []
        if osv_id in seen_ids:
            return False, [f"Duplicate exemption ID in allowlist: {osv_id}"], []
        seen_ids.add(osv_id)

        for req_field in ("module", "package", "reason", "risk", "approved_by", "expires_at"):
            val = item.get(req_field)
            if not val or not str(val).strip():
                return False, [f"Allowlist exemption for {osv_id} missing required field '{req_field}'"], []

        risk_val = item.get("risk")
        if risk_val not in ("LOW", "MEDIUM", "HIGH", "CRITICAL"):
            return False, [f"Allowlist exemption for {osv_id} has invalid risk '{risk_val}'"], []

        # Strict UTC ISO 8601 validation (must end in 'Z')
        exp_str = item.get("expires_at", "")
        if not exp_str.endswith("Z"):
            return False, [f"Allowlist exemption for {osv_id} expiration must be UTC ISO 8601 ending with 'Z': {exp_str}"], []
        try:
            exp_dt = datetime.datetime.fromisoformat(exp_str[:-1] + "+00:00")
            if now > exp_dt:
                return False, [f"Allowlist exemption for {osv_id} expired at {exp_str}!"], []
        except ValueError as e:
            return False, [f"Invalid expiration format for {osv_id}: {e}"], []

        # Anti-spoofing: stdlib packages cannot have third-party module prefixes
        if item.get("module") == "stdlib":
            pkg = item.get("package", "")
            if "." in pkg.split("/")[0] or pkg.startswith("github.com") or pkg.startswith("golang.org/x/"):
                return False, [f"Spoofed stdlib exemption for {osv_id}: package {pkg} is a third-party dependency"], []

        exemptions[osv_id] = item

    try:
        active_findings = parse_govulncheck_json_stream(raw_output)
    except ValueError as parse_err:
        return False, [str(parse_err)], []

    violations = []
    approved = []

    for osv_id, details in sorted(active_findings.items()):
        actual_modules = details["modules"]

        # Bidirectional check: actual_modules must ONLY contain "stdlib"
        if not actual_modules or actual_modules != {"stdlib"}:
            violations.append(
                f"UNAUTHORIZED VULNERABILITY IN 3RD-PARTY MODULE: {osv_id} "
                f"(actual_modules: {sorted(list(actual_modules))}, packages: {sorted(list(details['packages']))})"
            )
            continue

        if osv_id not in exemptions:
            violations.append(f"UNAUTHORIZED VULNERABILITY: {osv_id} (packages: {sorted(list(details['packages']))})")
            continue

        exemption = exemptions[osv_id]
        # Only stdlib vulnerabilities may be exempted under this policy
        if exemption.get("module") != "stdlib":
            violations.append(f"FORBIDDEN NON-STDLIB EXEMPTION: {osv_id} module={exemption.get('module')}")
            continue

        # Package validation
        ex_pkg = exemption.get("package")
        if ex_pkg and ex_pkg not in details["packages"]:
            violations.append(
                f"PACKAGE MISMATCH FOR EXEMPTION: {osv_id} expected {ex_pkg} but found {sorted(list(details['packages']))}"
            )
            continue

        approved.append((osv_id, exemption["package"], exemption.get("reason", "")))

    success = len(violations) == 0
    return success, violations, approved


def main():
    parser = argparse.ArgumentParser(description="Govulncheck Audited Exemption Gate")
    parser.add_argument("--allowlist", default=".govulncheck-allowlist.json", help="Path to allowlist JSON file")
    parser.add_argument("--input", default="", help="Path to pre-captured govulncheck JSON output (optional, runs tool if omitted)")
    args = parser.parse_args()

    allowlist_path = os.path.abspath(args.allowlist)

    if args.input:
        with open(args.input, "r", encoding="utf-8") as f:
            raw_output = f.read()
    else:
        cmd = ["go", "run", "golang.org/x/vuln/cmd/govulncheck@latest", "-format", "json", "./internal/...", "./cmd/..."]
        print(f"Executing: {' '.join(cmd)}")
        proc = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        raw_output = proc.stdout
        if proc.returncode not in (0, 3):
            print(f"ERROR: govulncheck execution failed (exit code {proc.returncode}):\n{proc.stderr}", file=sys.stderr)
            sys.exit(1)

    success, violations, approved = verify_govulncheck(allowlist_path, raw_output)

    if approved:
        print("\n--- Approved Audited Stdlib Exemptions ---")
        for osv_id, pkg, reason in approved:
            print(f"  [EXEMPTED] {osv_id} in {pkg}: {reason}")

    if not success or violations:
        print("\n================ SECURITY GATE FAILED ================", file=sys.stderr)
        for v in violations:
            print(f"  {v}", file=sys.stderr)
        print("======================================================", file=sys.stderr)
        sys.exit(1)

    print(f"\nSUCCESS: All {len(approved)} vulnerabilities verified against audited allowlist. 0 unauthorized vulnerabilities.")
    sys.exit(0)


if __name__ == "__main__":
    main()
