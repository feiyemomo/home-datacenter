#!/usr/bin/env python3
"""
OSV.dev Vulnerability Audit MCP Server.
Provides tools for checking project dependencies and individual packages for CVE/GHSA vulnerabilities.
"""

import json
import os
import sys
import urllib.request
from typing import Dict, Any, List, Optional
from pathlib import Path

# Add parent directory to path to allow importing scanner
sys.path.insert(0, str(Path(__file__).parent))
import scanner

try:
    from fastmcp import FastMCP
except ImportError:
    print("Error: fastmcp package is not installed. Please run `pip install fastmcp`", file=sys.stderr)
    sys.exit(1)

# Initialize MCP Server
mcp = FastMCP(
    "vulnerability-auditor",
    instructions="A security MCP server for auditing project dependencies and querying CVE/GHSA vulnerabilities from OSV.dev."
)


@mcp.tool(
    name="audit_workspace_vulnerabilities",
    description="Scan and audit all dependencies in a workspace or project directory (Go, npm, Android/Maven, PyPI) against OSV.dev vulnerability database."
)
def audit_workspace_vulnerabilities(workspace_dir: str = ".") -> str:
    """
    Scans the given directory for dependency manifests (go.mod, package-lock.json, libs.versions.toml, requirements.txt)
    and checks each package against OSV.dev. Returns a full Markdown vulnerability report.
    """
    target = Path(workspace_dir).resolve()
    if not target.exists():
        return f"Error: Directory {workspace_dir} does not exist."

    deps = scanner.collect_project_dependencies(str(target))
    if not deps:
        return f"No recognized dependency files found under {workspace_dir}."

    vulns = scanner.query_osv_batch(deps)
    return scanner.generate_markdown_report(deps, vulns)


@mcp.tool(
    name="check_package_vulnerability",
    description="Check whether a specific package and version has known vulnerabilities in OSV.dev."
)
def check_package_vulnerability(package_name: str, version: str, ecosystem: str) -> Dict[str, Any]:
    """
    Query OSV for a single package.
    :param package_name: e.g. "github.com/gin-gonic/gin", "axios", "requests", "com.google.android.exoplayer:exoplayer"
    :param version: e.g. "1.9.0", "1.7.7"
    :param ecosystem: One of "Go", "npm", "PyPI", "Maven"
    """
    clean_ver = version.lstrip("v")
    dep = scanner.Dependency(
        ecosystem=ecosystem,
        name=package_name,
        version=clean_ver,
        source_file="manual_check"
    )
    vulns = scanner.query_osv_batch([dep])
    return {
        "package": package_name,
        "version": clean_ver,
        "ecosystem": ecosystem,
        "vulnerability_count": len(vulns),
        "vulnerabilities": [scanner.asdict(v) for v in vulns]
    }


@mcp.tool(
    name="query_vulnerability_details",
    description="Retrieve full details for a specific vulnerability ID (such as GHSA-xxxx or CVE-xxxx) from OSV.dev."
)
def query_vulnerability_details(vuln_id: str) -> Dict[str, Any]:
    """
    Query OSV.dev for details of a specific vulnerability ID.
    """
    url = f"https://api.osv.dev/v1/vulns/{vuln_id}"
    req = urllib.request.Request(
        url,
        headers={"User-Agent": "OSV-MCP-Scanner/1.0"}
    )
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        return {"error": f"HTTP {e.code}: {e.reason}", "vuln_id": vuln_id}
    except Exception as e:
        return {"error": str(e), "vuln_id": vuln_id}


if __name__ == "__main__":
    # Run the FastMCP server via stdio
    mcp.run()
