"""
Two-Tier Funnel Cost Control Rule Engine for home-ops-agent:
- Tier 1: Local deterministic rule engine (95% routine events, 0 token consumption)
  - Container exit: exponential backoff restart (attempt 1: 1s, attempt 2: 2s)
  - Auth failures >= 5 times: auto ban IP via iptables -I INPUT -s <ip> -j DROP
  - Disk threshold full: auto purge temporary caches
- Tier 2: AI Diagnosis engine (5% complex events)
  - Consecutive crashes >= 3 times, unusual outbound connections, or pre-upgrade checks
  - Context sanitization (passwords, tokens, keys masked)
  - New API gateway querying (http://127.0.0.1:3000/v1) using model agent-fast / agent-deep
"""

import ipaddress
import json
import logging
import os
import re
import shutil
import sqlite3
import subprocess
import time
import urllib.error
import urllib.request
from typing import Any, Dict, List, Optional, Set

from audit_protocol import (
    EXECUTION_COMPLETED,
    EXECUTION_STARTED,
    append_jsonl_audit_stub,
    format_rfc3339_ms,
    redact_sensitive_string,
)
from config import OpsAgentConfig

logger = logging.getLogger("home-ops-agent.rule_engine")

PROTECTED_VOLUME_PREFIXES = (
    "/data",
    "/vol1",
    "/media",
    "/mnt",
    "/var/lib/docker/volumes",
    "/etc",
    "/root",
    "/home",
    "/boot",
    "/sys",
    "/proc",
    "/dev",
)

PROTECTED_VOLUMES = {
    "data-sqlite",
    "data-recordings",
    "backup-state",
}

SAFE_TEMP_CACHE_DIRS = [
    "/tmp/buildcache",
    "/var/tmp/buildcache",
    "/tmp/docker-build",
]


def sanitize_log(text: str) -> str:
    """
    Sanitizes logs by masking sensitive credentials, passwords, tokens, and keys.
    """
    if not text:
        return ""
    # Mask key=value pairs (password=xyz, token=xyz, secret=xyz)
    sanitized = re.sub(
        r"(?i)(password|token|secret|jwt|key|apikey|pass)=([^\s;&]+)",
        r"\1=***",
        text
    )
    # Mask Bearer tokens
    sanitized = re.sub(
        r"(?i)Bearer\s+[A-Za-z0-9._~+/-]+",
        "Bearer ***",
        sanitized
    )
    # Mask private keys and certificates
    sanitized = re.sub(
        r"-----BEGIN [A-Z ]+ PRIVATE KEY-----.*?-----END [A-Z ]+ PRIVATE KEY-----",
        "[REDACTED_PRIVATE_KEY]",
        sanitized,
        flags=re.DOTALL
    )
    return sanitized


class TwoTierRuleEngine:
    """
    Manages Tier 1 local rule automation and Tier 2 LLM escalation.
    """

    def __init__(self, config: Optional[OpsAgentConfig] = None):
        self.config = config or OpsAgentConfig.load()
        self.container_crash_counts: Dict[str, int] = {}
        self.container_crash_timestamps: Dict[str, List[float]] = {}
        self.auth_fail_counts: Dict[str, int] = {}
        self.auth_fail_timestamps: Dict[str, List[float]] = {}
        self.banned_ips: Set[str] = set()
        self.cleaned_caches: List[Any] = []
        self.last_disk_cleanup_time: float = 0.0
        self.llm_queries: List[Dict[str, Any]] = []
        self.tier1_intercept_count: int = 0
        self.tier2_escalate_count: int = 0

    @staticmethod
    def is_protected_volume(path: str) -> bool:
        """
        Validates if target path belongs to a protected persistent volume.
        Protects /data, /vol1, /media, Docker volumes, and system root directories.
        """
        if not path:
            return False
        if path in PROTECTED_VOLUMES:
            return True
        for name in PROTECTED_VOLUMES:
            if name in path:
                return True

        # Normalize slashes and strip Windows drive letter if present
        clean_path = path.replace("\\", "/").strip()
        if len(clean_path) >= 2 and clean_path[1] == ":" and clean_path[0].isalpha():
            clean_path = clean_path[2:]
        if not clean_path.startswith("/"):
            clean_path = "/" + clean_path

        for prefix in PROTECTED_VOLUME_PREFIXES:
            p = prefix.rstrip("/")
            if clean_path == p or clean_path.startswith(p + "/"):
                return True
        return False

    def handle_auth_failure(self, ip: str) -> Dict[str, Any]:
        """
        Tier 1 Rule: 5 failed attempts from same IP within window -> IP ban (0 token consumption).
        """
        now = time.time()
        timestamps = self.auth_fail_timestamps.setdefault(ip, [])
        # Prune older than window
        cutoff = now - self.config.auth_fail_window_sec
        timestamps = [t for t in timestamps if t >= cutoff]
        timestamps.append(now)
        self.auth_fail_timestamps[ip] = timestamps

        count = len(timestamps)
        self.auth_fail_counts[ip] = count
        self.tier1_intercept_count += 1

        if count >= self.config.auth_fail_threshold:
            self.banned_ips.add(ip)
            ban_cmd = f"iptables -I INPUT -s {ip} -j DROP"
            self._execute_firewall_drop(ip, ban_cmd)
            logger.warning(f"[Tier 1] IP {ip} banned after {count} failed auth attempts: {ban_cmd}")
            return {
                "handled_by": "tier1_rule_engine",
                "action": "ip_ban",
                "target_ip": ip,
                "count": count,
                "ban_command": ban_cmd,
                "tokens_used": 0
            }

        return {
            "handled_by": "tier1_rule_engine",
            "action": "increment_counter",
            "count": count,
            "tokens_used": 0
        }

    def handle_container_crash(self, container_name: str, exit_code: int, stderr: str) -> Dict[str, Any]:
        """
        Tier 1 & Tier 2:
        - Crashes < 3: Tier 1 backoff restart (0 token consumption)
        - Crashes >= 3: Tier 2 LLM diagnostic escalation with sanitized context
        """
        now = time.time()
        timestamps = self.container_crash_timestamps.setdefault(container_name, [])
        cutoff = now - self.config.crash_window_sec
        timestamps = [t for t in timestamps if t >= cutoff]
        timestamps.append(now)
        self.container_crash_timestamps[container_name] = timestamps

        crashes = len(timestamps)
        self.container_crash_counts[container_name] = crashes

        if crashes < self.config.crash_escalation_threshold:
            # Tier 1: Exponential backoff restart: 1s, 2s
            self.tier1_intercept_count += 1
            backoff_sec = 2 ** (crashes - 1)
            logger.info(f"[Tier 1] Container {container_name} crashed (exit {exit_code}, #{crashes}). Restarting with {backoff_sec}s backoff.")
            return {
                "handled_by": "tier1_rule_engine",
                "action": "restart_container",
                "container": container_name,
                "backoff_sec": backoff_sec,
                "exit_code": exit_code,
                "tokens_used": 0
            }

        # Tier 2: Consecutive crashes >= 3 -> Escalate to LLM
        self.tier2_escalate_count += 1
        sanitized = sanitize_log(stderr)
        query = {
            "container": container_name,
            "crash_count": crashes,
            "exit_code": exit_code,
            "sanitized_log": sanitized,
            "timestamp": now
        }
        self.llm_queries.append(query)
        logger.warning(f"[Tier 2] Consecutive crash loop on {container_name} ({crashes} crashes). Escalating to LLM.")

        diagnostic_result = self._query_new_api_diagnostic(container_name, crashes, exit_code, sanitized)

        return {
            "handled_by": "tier2_llm",
            "action": "request_diagnostic",
            "container": container_name,
            "model_alias": self.config.new_api_model,
            "sanitized_log": sanitized,
            "diagnostic_advice": diagnostic_result.get("advice", f"Container {container_name} is in crash loop. Recommend inspection."),
            "suggested_plan": diagnostic_result.get("plan", []),
            "rollback_plan": diagnostic_result.get("rollback", []),
            "risk_level": diagnostic_result.get("risk_level", "HIGH")
        }

    def handle_disk_usage(self, disk_percent: float, mount_path: str = "/") -> Dict[str, Any]:
        """
        Tier 1 Rule: Safe Disk Cleanup
        - Enforces 1-hour cooldown period between cleanups to prevent churn.
        - Strictly protects persistent volumes (/data, /vol1) from pruning.
        - Measures before/after disk free space delta (freed_bytes).
        """
        self.tier1_intercept_count += 1
        threshold = max(self.config.disk_threshold_percent, 85.0)

        if disk_percent < threshold:
            return {
                "handled_by": "tier1_rule_engine",
                "action": "status_nominal",
                "disk_percent": disk_percent,
                "tokens_used": 0
            }

        now = time.time()
        elapsed = now - self.last_disk_cleanup_time
        cooldown_sec = getattr(self.config, "disk_cleanup_cooldown_sec", 3600.0)

        # 1. Enforce 1-hour cooldown period
        if elapsed < cooldown_sec:
            remaining = int(cooldown_sec - elapsed)
            logger.info(
                f"[Tier 1] Disk usage at {disk_percent:.1f}% >= {threshold}%, "
                f"but cooldown is active ({remaining}s remaining). Skipping cleanup."
            )
            return {
                "handled_by": "tier1_rule_engine",
                "action": "cooldown_active",
                "mount_path": mount_path,
                "disk_percent": disk_percent,
                "cooldown_remaining_sec": remaining,
                "tokens_used": 0
            }

        # 2. Before delta measurement
        try:
            usage_before = shutil.disk_usage(mount_path)
            free_before = usage_before.free
        except Exception:
            free_before = 0

        # 3. Execute safe pruning of build caches
        purged_items = self._purge_temp_cache(mount_path)
        self.last_disk_cleanup_time = now

        # 4. After delta measurement
        try:
            usage_after = shutil.disk_usage(mount_path)
            free_after = usage_after.free
            freed_bytes = max(0, free_after - free_before)
            disk_percent_after = round((usage_after.used / usage_after.total) * 100.0, 2)
        except Exception:
            free_after = free_before
            freed_bytes = 0
            disk_percent_after = disk_percent

        freed_mb = round(freed_bytes / (1024 * 1024), 2)
        cleanup_record = {
            "timestamp": now,
            "mount_path": mount_path,
            "disk_percent_before": disk_percent,
            "disk_percent_after": disk_percent_after,
            "free_before_bytes": free_before,
            "free_after_bytes": free_after,
            "freed_bytes": freed_bytes,
            "freed_mb": freed_mb,
            "purged_items": purged_items
        }
        self.cleaned_caches.append(cleanup_record)

        logger.warning(
            f"[Tier 1] Disk usage at {disk_percent:.1f}% >= {threshold}%. "
            f"Purged temporary build caches. Freed {freed_mb:.2f}MB ({freed_bytes} bytes). "
            f"Cooldown of {int(cooldown_sec)}s active."
        )

        return {
            "handled_by": "tier1_rule_engine",
            "action": "purge_temp_cache",
            "mount_path": mount_path,
            "disk_percent": disk_percent,
            "disk_percent_before": disk_percent,
            "disk_percent_after": disk_percent_after,
            "free_before_bytes": free_before,
            "free_after_bytes": free_after,
            "freed_bytes": freed_bytes,
            "freed_mb": freed_mb,
            "purged_items": purged_items,
            "tokens_used": 0
        }

    def _execute_firewall_drop(self, ip: str, cmd: str) -> None:
        """
        Records or executes validated IP drop; strictly prevents self-banning loopback,
        unspecified, or link-local addresses and gates direct host iptables behind explicit opt-in (R4-24).
        """
        try:
            addr = ipaddress.ip_address((ip or "").strip())
        except ValueError:
            logger.warning(f"[Rule Engine] Rejected malformed IP '{ip}' from firewall drop.")
            return

        if addr.is_loopback or addr.is_unspecified or addr.is_link_local or addr.is_multicast:
            logger.warning(f"[Rule Engine] Refused self-ban of protected local/gateway address {addr}.")
            return

        if os.environ.get("ENABLE_LOCAL_IPTABLES", "").strip() != "1":
            logger.info(f"[Rule Engine] Recorded ban intent for {addr} (delegated to Go Core WAF/DefenseManager).")
            return

        try:
            proc = subprocess.run(
                ["iptables", "-I", "INPUT", "-s", str(addr), "-j", "DROP"],
                check=False,
                capture_output=True,
                timeout=2
            )
            archive_dir = os.environ.get("OPS_AUDIT_ARCHIVE_DIR", "").strip()
            if archive_dir:
                append_jsonl_audit_stub(archive_dir, "python-rule-audit.jsonl", {
                    "event_type": EXECUTION_COMPLETED,
                    "action": "firewall.iptables.drop",
                    "target": str(addr),
                    "returncode": proc.returncode,
                    "executed_at": format_rfc3339_ms(),
                })
        except Exception as e:
            logger.debug(f"iptables command skipped or simulated: {e}")

    def _is_maintenance_locked_for_prune(self) -> bool:
        """Fail-closed maintenance check before destructive local docker prune."""
        db_path = getattr(self.config, "db_path", "/data/sqlite/app.db")
        if not db_path or not os.path.isfile(db_path):
            return True
        conn = None
        try:
            conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True, timeout=2.0)
            row = conn.execute("SELECT active FROM ops_maintenance_locks ORDER BY id DESC LIMIT 1").fetchone()
            return bool(row and row[0])
        except Exception:
            return True
        finally:
            if conn:
                conn.close()

    def _purge_temp_cache(self, mount_path: str) -> List[str]:
        """
        Executes safe cache cleanup commands.
        Guards unapproved destructive docker prune behind explicit opt-in and maintenance lock awareness,
        while safely pruning whitelisted ephemeral cache files only (R4-24).
        """
        purged = []
        if os.environ.get("ENABLE_LOCAL_DOCKER_PRUNE", "").strip() == "1":
            if self._is_maintenance_locked_for_prune():
                logger.warning("[Rule Engine] Refused local docker prune while maintenance lock active/unknown.")
                purged.append("docker_prune_blocked_by_maintenance_lock")
            else:
                for docker_args, label in (
                    (["docker", "builder", "prune", "-f"], "docker builder prune -f"),
                    (["docker", "image", "prune", "-f", "--filter", "dangling=true"], "docker image prune -f --filter dangling=true"),
                ):
                    try:
                        res = subprocess.run(docker_args, check=False, capture_output=True, timeout=10)
                        if res.returncode == 0:
                            purged.append(label)
                    except Exception:
                        pass

        for temp_dir in SAFE_TEMP_CACHE_DIRS:
            if not self.is_protected_volume(temp_dir) and os.path.exists(temp_dir):
                try:
                    for entry in os.scandir(temp_dir):
                        if entry.is_file() or entry.is_symlink():
                            os.unlink(entry.path)
                    purged.append(f"cleaned {temp_dir}")
                except Exception:
                    pass

        archive_dir = os.environ.get("OPS_AUDIT_ARCHIVE_DIR", "").strip()
        if archive_dir and purged:
            append_jsonl_audit_stub(archive_dir, "python-rule-audit.jsonl", {
                "event_type": EXECUTION_COMPLETED,
                "action": "disk.temp_cache.purge",
                "target": redact_sensitive_string(mount_path),
                "purged_items": purged,
                "executed_at": format_rfc3339_ms(),
            })

        if not purged:
            purged.append("docker builder prune -f (simulated)")

        return purged

    def _query_new_api_diagnostic(self, container: str, crashes: int, exit_code: int, sanitized_log: str) -> Dict[str, Any]:
        """Calls local New API gateway with sanitized context."""
        endpoint = f"{self.config.new_api_url.rstrip('/')}/chat/completions"
        prompt = (
            f"Container {container} exited with code {exit_code} consecutively {crashes} times.\n"
            f"Sanitized log excerpt:\n{sanitized_log}\n"
            "Provide root cause analysis, risk level (LOW/MEDIUM/HIGH), suggested fix command, and rollback command."
        )

        payload = {
            "model": self.config.new_api_model,
            "messages": [
                {"role": "system", "content": "You are Home Datacenter Ops Diagnostic Copilot. Output concise JSON advice."},
                {"role": "user", "content": prompt}
            ],
            "temperature": 0.1,
            "max_tokens": 400
        }

        try:
            req_data = json.dumps(payload).encode("utf-8")
            req = urllib.request.Request(
                endpoint,
                data=req_data,
                headers={"Content-Type": "application/json"}
            )
            if self.config.new_api_token:
                req.add_header("Authorization", f"Bearer {self.config.new_api_token}")

            with urllib.request.urlopen(req, timeout=1) as resp:
                if resp.status == 200:
                    data = json.loads(resp.read().decode("utf-8"))
                    content = data.get("choices", [{}])[0].get("message", {}).get("content", "")
                    return {
                        "advice": content,
                        "risk_level": "HIGH",
                        "plan": [f"docker restart {container}"],
                        "rollback": [f"docker logs {container}"]
                    }
        except Exception as e:
            logger.debug(f"New API query fallback: {e}")

        return {
            "advice": f"Container {container} is in crash loop (crashed {crashes} times). Recommend inspection.",
            "risk_level": "HIGH",
            "plan": [f"docker restart {container}"],
            "rollback": [f"docker logs {container}"]
        }
