#!/usr/bin/env python3
"""
OSV.dev Vulnerability Scanner Core Engine.
Analyzes project dependencies and queries OSV.dev database for security advisories.
"""

import json
import os
import re
import sys
import urllib.request
import urllib.error
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, asdict
from pathlib import Path
from typing import List, Dict, Any, Optional, Tuple

OSV_BATCH_URL = "https://api.osv.dev/v1/querybatch"
OSV_SINGLE_URL = "https://api.osv.dev/v1/query"

@dataclass
class Dependency:
    ecosystem: str   # "Go", "npm", "PyPI", "Maven"
    name: str
    version: str
    source_file: str

@dataclass
class Vulnerability:
    vuln_id: str
    aliases: List[str]
    summary: str
    details: str
    severity: str
    fixed_version: Optional[str]
    package_name: str
    package_version: str
    ecosystem: str
    source_file: str
    references: List[str]


def parse_go_mod(file_path: Path) -> List[Dependency]:
    """Extract Go dependencies from go.mod."""
    deps: List[Dependency] = []
    if not file_path.exists():
        return deps

    with open(file_path, "r", encoding="utf-8", errors="ignore") as f:
        content = f.read()

    # Match require (...) blocks or single require lines
    in_require = False
    for line in content.splitlines():
        line = line.strip()
        if not line or line.startswith("//"):
            continue
        if line.startswith("require ("):
            in_require = True
            continue
        if in_require and line == ")":
            in_require = False
            continue

        if in_require or line.startswith("require "):
            if line.startswith("require "):
                line = line[len("require "):].strip()
            # line looks like: github.com/gin-gonic/gin v1.12.0 // indirect
            parts = line.split()
            if len(parts) >= 2:
                pkg_name = parts[0]
                version = parts[1].lstrip("v")
                deps.append(Dependency(
                    ecosystem="Go",
                    name=pkg_name,
                    version=version,
                    source_file=str(file_path)
                ))
    return deps


def parse_package_lock(file_path: Path) -> List[Dependency]:
    """Extract npm dependencies from package-lock.json (npm v2/v3 or v1)."""
    deps: List[Dependency] = []
    if not file_path.exists():
        return deps

    try:
        with open(file_path, "r", encoding="utf-8", errors="ignore") as f:
            data = json.load(f)

        packages = data.get("packages", {})
        if packages:
            for pkg_path, meta in packages.items():
                if not pkg_path:  # root
                    continue
                version = meta.get("version")
                if not version:
                    continue
                # Extract clean package name from node_modules path
                # e.g., "node_modules/@vitejs/plugin-react" or "node_modules/axios"
                parts = pkg_path.split("node_modules/")
                pkg_name = parts[-1]
                deps.append(Dependency(
                    ecosystem="npm",
                    name=pkg_name,
                    version=version,
                    source_file=str(file_path)
                ))
        else:
            # Fallback to v1 dependencies dict
            dependencies = data.get("dependencies", {})
            for pkg_name, meta in dependencies.items():
                version = meta.get("version")
                if version:
                    deps.append(Dependency(
                        ecosystem="npm",
                        name=pkg_name,
                        version=version,
                        source_file=str(file_path)
                    ))
    except Exception as e:
        print(f"Warning: failed to parse {file_path}: {e}", file=sys.stderr)
    return deps


def parse_package_json(file_path: Path) -> List[Dependency]:
    """Fallback: extract direct dependencies from package.json."""
    deps: List[Dependency] = []
    if not file_path.exists():
        return deps

    try:
        with open(file_path, "r", encoding="utf-8") as f:
            data = json.load(f)

        for section in ["dependencies", "devDependencies"]:
            for name, ver in data.get(section, {}).items():
                clean_ver = re.sub(r"^[^\d]*", "", ver) # strip ^, ~, >=, etc.
                if clean_ver:
                    deps.append(Dependency(
                        ecosystem="npm",
                        name=name,
                        version=clean_ver,
                        source_file=str(file_path)
                    ))
    except Exception as e:
        print(f"Warning: failed to parse {file_path}: {e}", file=sys.stderr)
    return deps


def parse_gradle_libs_toml(file_path: Path) -> List[Dependency]:
    """Extract Maven/Android dependencies from gradle/libs.versions.toml."""
    deps: List[Dependency] = []
    if not file_path.exists():
        return deps

    try:
        with open(file_path, "r", encoding="utf-8", errors="ignore") as f:
            lines = f.readlines()

        versions: Dict[str, str] = {}
        section = ""
        for line in lines:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            if line.startswith("[") and line.endswith("]"):
                section = line[1:-1].strip()
                continue

            if section == "versions":
                # key = "value"
                m = re.match(r"^([\w\.-]+)\s*=\s*[\"']([^\"']+)[\"']", line)
                if m:
                    versions[m.group(1)] = m.group(2)

            elif section == "libraries":
                # name = { group = "g", name = "n", version.ref = "r" }
                # or version = "v"
                m_group = re.search(r'group\s*=\s*["\']([^"\']+)["\']', line)
                m_name = re.search(r'name\s*=\s*["\']([^"\']+)["\']', line)
                if m_group and m_name:
                    group = m_group.group(1)
                    name = m_name.group(1)
                    full_pkg = f"{group}:{name}"

                    m_ver = re.search(r'version\s*=\s*["\']([^"\']+)["\']', line)
                    m_ref = re.search(r'version\.ref\s*=\s*["\']([^"\']+)["\']', line)

                    version = None
                    if m_ver:
                        version = m_ver.group(1)
                    elif m_ref and m_ref.group(1) in versions:
                        version = versions[m_ref.group(1)]

                    if version:
                        deps.append(Dependency(
                            ecosystem="Maven",
                            name=full_pkg,
                            version=version,
                            source_file=str(file_path)
                        ))
    except Exception as e:
        print(f"Warning: failed to parse {file_path}: {e}", file=sys.stderr)
    return deps


def parse_requirements_txt(file_path: Path) -> List[Dependency]:
    """Extract Python dependencies from requirements.txt."""
    deps: List[Dependency] = []
    if not file_path.exists():
        return deps

    with open(file_path, "r", encoding="utf-8", errors="ignore") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            # e.g., requests>=2.28.0, numpy==1.24.0, opencv-python-headless
            m = re.match(r"^([A-Za-z0-9_\.-]+)(?:[=><~^!]+([\w\.-]+))?", line)
            if m:
                pkg_name = m.group(1)
                ver = m.group(2) or "0.0.0"
                deps.append(Dependency(
                    ecosystem="PyPI",
                    name=pkg_name,
                    version=ver,
                    source_file=str(file_path)
                ))
    return deps


def collect_project_dependencies(root_dir: str) -> List[Dependency]:
    """Discover and parse all known dependency manifest files in project root."""
    root = Path(root_dir)
    all_deps: List[Dependency] = []

    # 1. Go modules
    for p in root.glob("**/go.mod"):
        if "vendor" not in p.parts:
            all_deps.extend(parse_go_mod(p))

    # 2. Web npm dependencies (prefer package-lock.json if available)
    for p in root.glob("**/package.json"):
        if "node_modules" in p.parts:
            continue
        lock_p = p.parent / "package-lock.json"
        if lock_p.exists():
            all_deps.extend(parse_package_lock(lock_p))
        else:
            all_deps.extend(parse_package_json(p))

    # 3. Android Gradle dependencies
    for p in root.glob("**/gradle/libs.versions.toml"):
        all_deps.extend(parse_gradle_libs_toml(p))

    # 4. Python requirements
    for p in root.glob("**/requirements.txt"):
        all_deps.extend(parse_requirements_txt(p))

    # Deduplicate dependencies by (ecosystem, name, version)
    seen = set()
    deduped: List[Dependency] = []
    for d in all_deps:
        key = (d.ecosystem, d.name, d.version)
        if key not in seen:
            seen.add(key)
            deduped.append(d)

    return deduped


def fetch_vuln_detail(vuln_id: str) -> Tuple[str, Optional[Dict[str, Any]]]:
    """Fetch full OSV vulnerability definition for a given ID."""
    url = f"https://api.osv.dev/v1/vulns/{vuln_id}"
    req = urllib.request.Request(url, headers={"User-Agent": "OSV-MCP-Scanner/1.0"})
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            return vuln_id, json.loads(resp.read().decode("utf-8"))
    except Exception:
        return vuln_id, None


def query_osv_batch(deps: List[Dependency], batch_size: int = 500) -> List[Vulnerability]:
    """Query OSV.dev batch API for security vulnerabilities and resolve details."""
    if not deps:
        return []

    hits: List[Tuple[Dependency, str]] = []

    # 1. Query OSV batch for matches
    for i in range(0, len(deps), batch_size):
        chunk = deps[i:i + batch_size]
        queries = []
        for d in chunk:
            queries.append({
                "version": d.version,
                "package": {
                    "name": d.name,
                    "ecosystem": d.ecosystem
                }
            })

        req_body = json.dumps({"queries": queries}).encode("utf-8")
        req = urllib.request.Request(
            OSV_BATCH_URL,
            data=req_body,
            headers={"Content-Type": "application/json", "User-Agent": "OSV-MCP-Scanner/1.0"}
        )

        try:
            with urllib.request.urlopen(req, timeout=30) as resp:
                resp_data = json.loads(resp.read().decode("utf-8"))
                results = resp_data.get("results", [])

                for idx, res in enumerate(results):
                    dep = chunk[idx]
                    for v in res.get("vulns", []):
                        vid = v.get("id")
                        if vid:
                            hits.append((dep, vid))
        except Exception as e:
            print(f"Error querying OSV API for batch {i}-{i+len(chunk)}: {e}", file=sys.stderr)

    if not hits:
        return []

    # 2. Concurrently fetch details for unique vulnerability IDs
    unique_vids = list({vid for _, vid in hits})
    vuln_details_cache: Dict[str, Dict[str, Any]] = {}

    with ThreadPoolExecutor(max_workers=10) as executor:
        for vid, detail in executor.map(fetch_vuln_detail, unique_vids):
            if detail:
                vuln_details_cache[vid] = detail

    # 3. Assemble Vulnerability items
    vulns: List[Vulnerability] = []
    for dep, vid in hits:
        v = vuln_details_cache.get(vid, {})

        # Extract severity
        db_spec = v.get("database_specific", {})
        severity_score = db_spec.get("severity")
        if not severity_score:
            for s in v.get("severity", []):
                if s.get("type") in ("CVSS_V3", "CVSS_V2"):
                    severity_score = s.get("score")
                    break
        if not severity_score:
            severity_score = "MODERATE"

        # Extract summary
        summary = v.get("summary")
        if not summary:
            details = v.get("details", "").strip()
            if details:
                # remove markdown headers like ## Summary
                cleaned_lines = [l.strip("# ").strip() for l in details.splitlines() if l.strip() and not l.startswith("#")]
                summary = cleaned_lines[0] if cleaned_lines else details[:100]
            else:
                summary = "Known security advisory in OSV database"

        # Extract CVE alias
        cve_ids = [a for a in v.get("aliases", []) if a.startswith("CVE-")]
        other_aliases = [a for a in v.get("aliases", []) if not a.startswith("CVE-")]

        # Extract fixed version
        fixed_versions = []
        for affected in v.get("affected", []):
            for r in affected.get("ranges", []):
                for event in r.get("events", []):
                    if "fixed" in event:
                        fixed_versions.append(event["fixed"])
        fixed_ver = ", ".join(sorted(set(fixed_versions))) if fixed_versions else None

        # References
        refs = [r.get("url") for r in v.get("references", []) if r.get("url")]

        vulns.append(Vulnerability(
            vuln_id=vid,
            aliases=cve_ids or other_aliases,
            summary=summary,
            details=v.get("details", ""),
            severity=str(severity_score).upper(),
            fixed_version=fixed_ver,
            package_name=dep.name,
            package_version=dep.version,
            ecosystem=dep.ecosystem,
            source_file=dep.source_file,
            references=refs[:3]
        ))

    return vulns


def generate_markdown_report(deps: List[Dependency], vulns: List[Vulnerability]) -> str:
    """Format audit results as a rich Markdown document."""
    report = []
    report.append("# 🛡️ 本地项目依赖安全漏洞巡检报告\n")
    report.append(f"- **总扫描组件数**: `{len(deps)}` 个第三方依赖库")
    report.append(f"- **发现漏洞数**: `{len(vulns)}` 个安全警报")
    report.append(f"- **漏洞数据库来源**: Google OSV.dev (包含 GHSA, NVD CVE, Go VulnDB 等)\n")

    # Group by ecosystem
    by_eco: Dict[str, List[Dependency]] = {}
    for d in deps:
        by_eco.setdefault(d.ecosystem, []).append(d)

    report.append("## 📦 组件清单概览\n")
    report.append("| 生态 | 扫描依赖包数量 | 主要文件位置 |")
    report.append("| :--- | :--- | :--- |")
    for eco, eco_deps in sorted(by_eco.items()):
        sample_file = Path(eco_deps[0].source_file).name
        report.append(f"| **{eco}** | `{len(eco_deps)}` 个 | `{sample_file}` 等 |")
    report.append("\n---\n")

    if not vulns:
        report.append("## ✅ 审计结果：未发现已知安全漏洞\n")
        report.append("当前扫描的所有第三方依赖库版本均未匹配到已公开的高危 CVE / GHSA 漏洞记录，依赖状态良好。\n")
        return "\n".join(report)

    report.append("## ⚠️ 发现的安全漏洞列表\n")
    report.append("| 严重等级 / 评分 | 受影响组件 | 当前版本 | 漏洞 ID / CVE | 漏洞摘要 | 建议修复版本 |")
    report.append("| :--- | :--- | :--- | :--- | :--- | :--- |")

    # Sort vulns by severity/id
    severity_order = {"CRITICAL": 4, "HIGH": 3, "MODERATE": 2, "MEDIUM": 2, "LOW": 1}
    sorted_vulns = sorted(
        vulns,
        key=lambda x: (
            -severity_order.get(x.severity, 2 if "CVSS" not in x.severity else 3),
            x.ecosystem,
            x.package_name
        )
    )

    for v in sorted_vulns:
        cve_str = ", ".join(v.aliases) if v.aliases else v.vuln_id
        fix_str = f"`>={v.fixed_version}`" if v.fixed_version else "关注官方补丁"
        summary = v.summary.replace("\n", " ").replace("|", "\\|")
        if len(summary) > 80:
            summary = summary[:77] + "..."
        report.append(f"| **{v.severity}** | `{v.package_name}` | `{v.package_version}` | [{v.vuln_id}](https://osv.dev/vulnerability/{v.vuln_id})<br>({cve_str}) | {summary} | {fix_str} |")

    report.append("\n### 💡 修复与安全加固建议\n")
    seen_pkgs = set()
    for v in sorted_vulns:
        if v.package_name in seen_pkgs:
            continue
        seen_pkgs.add(v.package_name)
        report.append(f"1. **`{v.package_name}`** (`{v.ecosystem}`):")
        report.append(f"   - 当前版本: `{v.package_version}`")
        if v.fixed_version:
            report.append(f"   - 建议升级至: `{v.fixed_version}` 及以上版本")
        else:
            report.append(f"   - 暂无直接修复版本事件，请审查代码中是否调用了相关易受攻击的功能接口。")
        if v.references:
            report.append(f"   - 参考资料: {v.references[0]}")

    return "\n".join(report)


if __name__ == "__main__":
    try:
        sys.stdout.reconfigure(encoding="utf-8")
        sys.stderr.reconfigure(encoding="utf-8")
    except Exception:
        pass

    target_dir = sys.argv[1] if len(sys.argv) > 1 else "."
    print(f"[*] Scanning workspace: {Path(target_dir).resolve()} ...")
    dependencies = collect_project_dependencies(target_dir)
    print(f"[*] Found {len(dependencies)} unique dependencies across all ecosystems.")
    
    print("[*] Querying OSV.dev vulnerability database...")
    vulnerabilities = query_osv_batch(dependencies)
    
    print(f"[*] Done! Found {len(vulnerabilities)} vulnerabilities.")
    report_md = generate_markdown_report(dependencies, vulnerabilities)
    
    report_file = Path("audit_report.md")
    report_file.write_text(report_md, encoding="utf-8")
    print(f"[*] Report saved to: {report_file.resolve()}")
    print("\n" + report_md)
