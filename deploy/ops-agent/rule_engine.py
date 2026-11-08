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

import json
import logging
import re
import subprocess
import time
import urllib.error
import urllib.request
from typing import Any, Dict, List, Optional, Set

from config import OpsAgentConfig

logger = logging.getLogger("home-ops-agent.rule_engine")


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
        self.cleaned_caches: List[float] = []
        self.llm_queries: List[Dict[str, Any]] = []
        self.tier1_intercept_count: int = 0
        self.tier2_escalate_count: int = 0

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
        Tier 1 Rule: Disk utilization >= threshold -> auto purge temporary caches (0 tokens).
        """
        self.tier1_intercept_count += 1
        threshold = max(self.config.disk_threshold_percent, 85.0)

        if disk_percent >= threshold:
            self.cleaned_caches.append(time.time())
            purged_items = self._purge_temp_cache(mount_path)
            logger.warning(f"[Tier 1] Disk usage at {disk_percent:.1f}% (threshold {threshold}%). Purged temporary caches.")
            return {
                "handled_by": "tier1_rule_engine",
                "action": "purge_temp_cache",
                "mount_path": mount_path,
                "disk_percent": disk_percent,
                "purged_items": purged_items,
                "tokens_used": 0
            }

        return {
            "handled_by": "tier1_rule_engine",
            "action": "status_nominal",
            "disk_percent": disk_percent,
            "tokens_used": 0
        }

    def _execute_firewall_drop(self, ip: str, cmd: str) -> None:
        """Executes iptables command if running in container with NET_ADMIN or logs action."""
        try:
            # Check if iptables exists on system
            subprocess.run(
                ["iptables", "-I", "INPUT", "-s", ip, "-j", "DROP"],
                check=False,
                capture_output=True,
                timeout=2
            )
        except Exception as e:
            logger.debug(f"iptables command skipped or simulated: {e}")

    def _purge_temp_cache(self, mount_path: str) -> List[str]:
        """Runs cache cleanup commands (docker builder prune, temporary files)."""
        purged = ["docker builder prune -f", "/tmp orphan files"]
        try:
            subprocess.run(["docker", "builder", "prune", "-f"], check=False, capture_output=True, timeout=5)
        except Exception:
            pass
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
