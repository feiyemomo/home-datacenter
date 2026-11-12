#!/usr/bin/env python3
"""
Home Datacenter CI Security Gate: Govulncheck Audited Exemption Verifier
Fails closed if any unallowlisted or expired vulnerability is detected.
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
    Returns a dict mapping OSV ID -> finding details.
    """
    dec = json.JSONDecoder()
    pos = 0
    active_findings = {}

    idx = stream_text.find('{')
    if idx == -1:
        return active_findings
    text = stream_text[idx:]

    while pos < len(text):
        chunk = text[pos:].lstrip()
        if not chunk:
            break
        pos += len(text[pos:]) - len(chunk)
        try:
            obj, end = dec.raw_decode(text[pos:])
            pos += end
            if "finding" in obj:
                fnd = obj["finding"]
                osv_id = fnd.get("osv")
                traces = fnd.get("trace", [])
                # Only consider findings where code actually traces back to our root module
                has_root_trace = any(
                    t.get("module") == "home-datacenter-api" or
                    (t.get("package", "").startswith("home-datacenter-api/"))
                    for t in traces
                )
                if osv_id and (has_root_trace or not traces):
                    modules = set()
                    packages = set()
                    for t in traces:
                        if t.get("module"):
                            modules.add(t.get("module"))
                        if t.get("package"):
                            packages.add(t.get("package"))
                    if osv_id not in active_findings:
                        active_findings[osv_id] = {
                            "modules": set(),
                            "packages": set(),
                            "fixed_version": fnd.get("fixed_version", ""),
                        }
                    active_findings[osv_id]["modules"].update(modules)
                    active_findings[osv_id]["packages"].update(packages)
        except Exception:
            pos += 1

    return active_findings


def main():
    parser = argparse.ArgumentParser(description="Govulncheck Audited Exemption Gate")
    parser.add_argument("--allowlist", default=".govulncheck-allowlist.json", help="Path to allowlist JSON file")
    parser.add_argument("--input", default="", help="Path to pre-captured govulncheck JSON output (optional, runs tool if omitted)")
    args = parser.parse_args()

    allowlist_path = os.path.abspath(args.allowlist)
    if not os.path.isfile(allowlist_path):
        print(f"ERROR: Govulncheck allowlist file not found: {allowlist_path}", file=sys.stderr)
        sys.exit(1)

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
                    print(f"ERROR: Allowlist exemption for {osv_id} expired at {exp_str}!", file=sys.stderr)
                    sys.exit(1)
            except ValueError as e:
                print(f"ERROR: Invalid expiration format for {osv_id}: {e}", file=sys.stderr)
                sys.exit(1)

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

    active_findings = parse_govulncheck_json_stream(raw_output)
    print(f"Found {len(active_findings)} active vulnerability findings called by code.")

    violations = []
    approved = []

    for osv_id, details in sorted(active_findings.items()):
        if osv_id not in exemptions:
            violations.append(f"UNAUTHORIZED VULNERABILITY: {osv_id} (modules: {sorted(list(details['modules']))})")
            continue

        exemption = exemptions[osv_id]
        # Only stdlib vulnerabilities may be exempted under this policy
        if exemption.get("module") != "stdlib":
            violations.append(f"FORBIDDEN NON-STDLIB EXEMPTION: {osv_id} module={exemption.get('module')}")
            continue

        approved.append((osv_id, exemption["package"], exemption.get("reason", "")))

    if approved:
        print("\n--- Approved Audited Stdlib Exemptions ---")
        for osv_id, pkg, reason in approved:
            print(f"  [EXEMPTED] {osv_id} in {pkg}: {reason}")

    if violations:
        print("\n================ SECURITY GATE FAILED ================", file=sys.stderr)
        for v in violations:
            print(f"  {v}", file=sys.stderr)
        print("======================================================", file=sys.stderr)
        sys.exit(1)

    print(f"\nSUCCESS: All {len(approved)} vulnerabilities verified against audited allowlist. 0 unauthorized vulnerabilities.")
    sys.exit(0)


if __name__ == "__main__":
    main()
