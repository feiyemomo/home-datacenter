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
                has_root_trace = any(
                    t.get("module") == "home-datacenter-api" or
                    (t.get("package", "").startswith("home-datacenter-api/"))
                    for t in traces
                )
                if osv_id and (has_root_trace or not traces):
                    modules = set()
                    packages = set()
                    symbols = set()
                    for t in traces:
                        m = t.get("module")
                        p = t.get("package", "")
                        f = t.get("function")
                        # Separate vulnerable module/package from our root application callers
                        if m and m != "home-datacenter-api":
                            modules.add(m)
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

    exemptions = {item["id"]: item for item in allowlist_data.get("exemptions", [])}
    now = datetime.datetime.now(datetime.timezone.utc)

    # Validate expiration of all entries in allowlist
    for osv_id, item in exemptions.items():
        exp_str = item.get("expires_at", "")
        if exp_str:
            try:
                exp_dt = datetime.datetime.fromisoformat(exp_str.replace("Z", "+00:00"))
                if now > exp_dt:
                    return False, [f"Allowlist exemption for {osv_id} expired at {exp_str}!"], []
            except ValueError as e:
                return False, [f"Invalid expiration format for {osv_id}: {e}"], []

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
