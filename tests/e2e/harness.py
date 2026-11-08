"""
E2E Test Harness & Contract Verifier
Provides reusable test fixtures, cryptographic primitives, state machines,
and mock engines for Home Datacenter E2E test tracks.
"""

import os
import sys
import time
import json
import base64
import hashlib
import hmac
import re
from typing import Dict, Any, List, Optional, Tuple

# Cryptography primitives for AES-256-GCM
try:
    from cryptography.hazmat.primitives.ciphers.aead import AESGCM
    HAS_CRYPTOGRAPHY = True
except ImportError:
    HAS_CRYPTOGRAPHY = False


class CryptoHelper:
    """Implements and verifies AES-256-GCM face vector encryption/decryption."""

    MAGIC_HEADER = b"AESGCM\x01"

    @classmethod
    def derive_key(cls, passphrase: str) -> bytes:
        if not passphrase:
            raise ValueError("Passphrase must not be empty")
        return hashlib.sha256(passphrase.encode("utf-8")).digest()

    @classmethod
    def encrypt_data(cls, plaintext: bytes, key: bytes, iv: Optional[bytes] = None) -> bytes:
        if not HAS_CRYPTOGRAPHY:
            raise RuntimeError("cryptography library required for AES-256-GCM")
        if len(key) != 32:
            raise ValueError(f"AES-256 key must be 32 bytes, got {len(key)}")
        if iv is None:
            iv = os.urandom(12)
        elif len(iv) != 12:
            raise ValueError("IV must be 12 bytes")
        
        aesgcm = AESGCM(key)
        # In AESGCM.encrypt, the tag is appended to the ciphertext
        ciphertext_and_tag = aesgcm.encrypt(iv, plaintext, cls.MAGIC_HEADER)
        return cls.MAGIC_HEADER + iv + ciphertext_and_tag

    @classmethod
    def decrypt_data(cls, payload: bytes, key: bytes) -> bytes:
        if not HAS_CRYPTOGRAPHY:
            raise RuntimeError("cryptography library required for AES-256-GCM")
        if len(key) != 32:
            raise ValueError(f"AES-256 key must be 32 bytes, got {len(key)}")
        if not payload.startswith(cls.MAGIC_HEADER):
            raise ValueError("Invalid magic header: not an encrypted payload")
        
        header_len = len(cls.MAGIC_HEADER)
        iv = payload[header_len:header_len + 12]
        ciphertext_and_tag = payload[header_len + 12:]
        if len(iv) != 12 or len(ciphertext_and_tag) < 16:
            raise ValueError("Payload too short or corrupt")

        aesgcm = AESGCM(key)
        return aesgcm.decrypt(iv, ciphertext_and_tag, cls.MAGIC_HEADER)


class JWTHelper:
    """Pure-Python HS256 JWT encoder and parser for contract verification."""

    @staticmethod
    def _b64url_encode(data: bytes) -> str:
        return base64.urlsafe_b64encode(data).rstrip(b"=").decode("utf-8")

    @staticmethod
    def _b64url_decode(s: str) -> bytes:
        padding = 4 - (len(s) % 4)
        if padding and padding != 4:
            s += "=" * padding
        return base64.urlsafe_b64decode(s.encode("utf-8"))

    @classmethod
    def create_token(cls, claims: Dict[str, Any], secret: str) -> str:
        header = {"alg": "HS256", "typ": "JWT"}
        header_bytes = json.dumps(header, separators=(",", ":")).encode("utf-8")
        payload_bytes = json.dumps(claims, separators=(",", ":")).encode("utf-8")

        p1 = cls._b64url_encode(header_bytes)
        p2 = cls._b64url_encode(payload_bytes)
        unsigned_token = f"{p1}.{p2}"

        signature = hmac.new(
            secret.encode("utf-8"),
            unsigned_token.encode("utf-8"),
            hashlib.sha256
        ).digest()
        p3 = cls._b64url_encode(signature)
        return f"{unsigned_token}.{p3}"

    @classmethod
    def verify_and_parse(cls, token: str, secret: str) -> Tuple[bool, Optional[Dict[str, Any]], str]:
        parts = token.split(".")
        if len(parts) != 3:
            return False, None, "Invalid token format: must have 3 parts"

        p1, p2, p3 = parts
        try:
            unsigned_token = f"{p1}.{p2}"
            expected_sig = hmac.new(
                secret.encode("utf-8"),
                unsigned_token.encode("utf-8"),
                hashlib.sha256
            ).digest()
            actual_sig = cls._b64url_decode(p3)

            if not hmac.compare_digest(expected_sig, actual_sig):
                return False, None, "Signature mismatch"

            payload_bytes = cls._b64url_decode(p2)
            claims = json.loads(payload_bytes.decode("utf-8"))

            # Expiration check
            if "exp" in claims and time.time() > claims["exp"]:
                return False, claims, "Token expired"

            return True, claims, "OK"
        except Exception as e:
            return False, None, f"Parse error: {e}"


class GestureMath:
    """Verifies PinchToZoom and smooth double-tap mathematics."""

    MIN_SCALE = 1.0
    MAX_SCALE = 4.0

    @classmethod
    def clamp_scale(cls, scale: float) -> float:
        return max(cls.MIN_SCALE, min(scale, cls.MAX_SCALE))

    @classmethod
    def calculate_max_translation(cls, view_width: float, view_height: float, scale: float) -> Tuple[float, float]:
        clamped_scale = cls.clamp_scale(scale)
        max_trans_x = (view_width * (clamped_scale - 1.0)) / 2.0
        max_trans_y = (view_height * (clamped_scale - 1.0)) / 2.0
        return max_trans_x, max_trans_y

    @classmethod
    def clamp_translation(cls, trans_x: float, trans_y: float, view_width: float, view_height: float, scale: float) -> Tuple[float, float]:
        max_x, max_y = cls.calculate_max_translation(view_width, view_height, scale)
        clamped_x = max(-max_x, min(trans_x, max_x))
        clamped_y = max(-max_y, min(trans_y, max_y))
        return clamped_x, clamped_y

    @classmethod
    def double_tap_target(cls, current_scale: float, tap_x: float, tap_y: float, view_w: float, view_h: float) -> Tuple[float, float, float]:
        """Returns (target_scale, target_trans_x, target_trans_y)"""
        if current_scale > 1.05:
            # Reset to 1.0x centered
            return 1.0, 0.0, 0.0
        else:
            target_scale = 2.0
            focus_x = tap_x - view_w / 2.0
            focus_y = tap_y - view_h / 2.0
            target_trans_x = -focus_x * (target_scale - 1.0)
            target_trans_y = -focus_y * (target_scale - 1.0)
            clamped_x, clamped_y = cls.clamp_translation(target_trans_x, target_trans_y, view_w, view_h, target_scale)
            return target_scale, clamped_x, clamped_y


class SDPHelper:
    """Verifies SDP candidate rewrite logic matching RewriteH3CCandidates."""

    @staticmethod
    def rewrite_h3c_candidates(sdp_answer: str, h3c_tunnel_ip: str, h3c_tunnel_port: int) -> str:
        """
        Rewrites SDP candidate port for candidates whose host matches h3c_tunnel_ip,
        mirroring services/api/internal/camera/sdp.go RewriteH3CCandidates.
        """
        lines = sdp_answer.split("\n")
        new_lines = []

        for line in lines:
            cr = line.endswith("\r")
            trimmed = line.rstrip("\r")
            if not trimmed.startswith("a=candidate:"):
                new_lines.append(line)
                continue
            
            # a=candidate:<foundation> <component> <transport> <priority> <address> <port> typ <type> ...
            fields = trimmed.split(" ")
            if len(fields) >= 6:
                host = fields[4]
                if host == h3c_tunnel_ip:
                    # Prune dead UDP candidate for TCP-only tunnel
                    if fields[2].lower() == "udp":
                        continue
                    fields[5] = str(h3c_tunnel_port)
                    trimmed = " ".join(fields)
            if cr:
                trimmed += "\r"
            new_lines.append(trimmed)

        return "\n".join(new_lines)


class H3CPingPongStateMachine:
    """
    Simulates the ping-pong zero-gap tunnel state machine:
    home_a (port 8088) <-> home_b (port 8089).
    Refresh triggered at <= 25 min remaining.
    60s dual-tunnel overlap before deleting old tunnel.
    """

    def __init__(self):
        self.active_channel = "home_a"
        self.active_internal_port = 8088
        self.active_external_port = 45001
        self.old_channel = None
        self.old_external_port = None
        self.in_overlap_window = False
        self.overlap_start_time = 0
        self.go2rtc_restart_invoked = False
        self.sdp_candidate_updated = False
        self.history = []

    def get_remaining_minutes(self, expire_timestamp: float) -> float:
        return (expire_timestamp - time.time()) / 60.0

    def trigger_refresh_if_needed(self, expire_timestamp: float, current_time: float) -> bool:
        remaining = (expire_timestamp - current_time) / 60.0
        if remaining <= 25.0:
            return self.perform_ping_pong_cutover(current_time)
        return False

    def perform_ping_pong_cutover(self, current_time: float) -> bool:
        # Step 1: determine target channel
        if self.active_channel == "home_a":
            target_channel = "home_b"
            target_port = 8089
            target_external = 45002
        else:
            target_channel = "home_a"
            target_port = 8088
            target_external = 45001

        # Step 2: create target channel without destroying active
        self.old_channel = self.active_channel
        self.old_external_port = self.active_external_port

        self.active_channel = target_channel
        self.active_internal_port = target_port
        self.active_external_port = target_external

        self.in_overlap_window = True
        self.overlap_start_time = current_time

        # Step 3: Hot reload config to go2rtc WITHOUT restart
        self.sdp_candidate_updated = True
        self.go2rtc_restart_invoked = False  # Strict requirement: no restart

        self.history.append({
            "action": "cutover_created",
            "active": self.active_channel,
            "old": self.old_channel,
            "external_port": self.active_external_port,
            "time": current_time
        })
        return True

    def tick_overlap(self, current_time: float) -> bool:
        """After 60 seconds overlap, release old channel."""
        if self.in_overlap_window and (current_time - self.overlap_start_time) >= 60.0:
            released = self.old_channel
            self.history.append({
                "action": "old_tunnel_released",
                "released": released,
                "time": current_time
            })
            self.old_channel = None
            self.old_external_port = None
            self.in_overlap_window = False
            return True
        return False


class TwoTierCostFunnel:
    """
    Simulates the two-tier cost control funnel in home-ops-agent:
    - Tier 1: Local Rule Engine (95% events, 0 tokens)
      - container crash: backoff restart
      - 5 auth failures: IP ban
      - disk > 90%: cache purge
    - Tier 2: LLM Diagnostics (5% events)
      - >= 3 consecutive crashes
      - multi-origin anomalous egress
    """

    def __init__(self):
        self.container_crash_counts = {}
        self.auth_fail_counts = {}
        self.banned_ips = set()
        self.cleaned_caches = []
        self.llm_queries = []
        self.tier1_intercept_count = 0
        self.tier2_escalate_count = 0

    def handle_auth_failure(self, ip: str) -> Dict[str, Any]:
        count = self.auth_fail_counts.get(ip, 0) + 1
        self.auth_fail_counts[ip] = count
        if count >= 5:
            self.banned_ips.add(ip)
            self.tier1_intercept_count += 1
            return {
                "handled_by": "tier1_rule_engine",
                "action": "ip_ban",
                "target_ip": ip,
                "tokens_used": 0
            }
        self.tier1_intercept_count += 1
        return {
            "handled_by": "tier1_rule_engine",
            "action": "increment_counter",
            "count": count,
            "tokens_used": 0
        }

    def handle_container_crash(self, container_name: str, exit_code: int, stderr: str) -> Dict[str, Any]:
        crashes = self.container_crash_counts.get(container_name, 0) + 1
        self.container_crash_counts[container_name] = crashes

        if crashes < 3:
            # Tier 1 rule: exponential backoff restart
            self.tier1_intercept_count += 1
            backoff_sec = 2 ** (crashes - 1)
            return {
                "handled_by": "tier1_rule_engine",
                "action": "restart_container",
                "container": container_name,
                "backoff_sec": backoff_sec,
                "tokens_used": 0
            }
        else:
            # Tier 2 rule: consecutive crash loop >= 3 -> escalate to LLM
            self.tier2_escalate_count += 1
            sanitized_log = re.sub(r"(password|token|secret)=\S+", r"\1=***", stderr)
            query = {
                "container": container_name,
                "crash_count": crashes,
                "exit_code": exit_code,
                "sanitized_log": sanitized_log
            }
            self.llm_queries.append(query)
            return {
                "handled_by": "tier2_llm",
                "action": "request_diagnostic",
                "container": container_name,
                "model_alias": "agent-fast",
                "diagnostic_advice": f"Container {container_name} is in crash loop. Recommend inspection."
            }

    def handle_disk_usage(self, disk_percent: float) -> Dict[str, Any]:
        self.tier1_intercept_count += 1
        if disk_percent >= 90.0:
            self.cleaned_caches.append(time.time())
            return {
                "handled_by": "tier1_rule_engine",
                "action": "purge_temp_cache",
                "tokens_used": 0
            }
        return {
            "handled_by": "tier1_rule_engine",
            "action": "status_nominal",
            "tokens_used": 0
        }


class ProposalLifecycleManager:
    """
    Simulates Human-in-the-Loop proposal states:
    AWAITING_APPROVAL -> APPROVED -> EXECUTING -> EXECUTED / FAILED_ROLLEDBACK
    """

    STATE_AWAITING = "AWAITING_APPROVAL"
    STATE_APPROVED = "APPROVED"
    STATE_EXECUTING = "EXECUTING"
    STATE_EXECUTED = "EXECUTED"
    STATE_FAILED_ROLLEDBACK = "FAILED_ROLLEDBACK"

    def __init__(self):
        self.proposals = {}

    def create_proposal(self, prop_id: str, title: str, risk_level: str, action: str, rollback_cmd: str) -> Dict[str, Any]:
        proposal = {
            "id": prop_id,
            "title": title,
            "risk_level": risk_level,
            "action": action,
            "rollback_cmd": rollback_cmd,
            "status": self.STATE_AWAITING,
            "created_at": time.time(),
            "approved_at": None,
            "executed_at": None
        }
        self.proposals[prop_id] = proposal
        return proposal

    def approve_proposal(self, prop_id: str, approver: str, is_admin: bool) -> Tuple[bool, str]:
        if not is_admin:
            return False, "Permission denied: admin role required"
        proposal = self.proposals.get(prop_id)
        if not proposal:
            return False, "Proposal not found"
        if proposal["status"] != self.STATE_AWAITING:
            return False, f"Invalid state transition from {proposal['status']}"

        proposal["status"] = self.STATE_APPROVED
        proposal["approved_at"] = time.time()
        proposal["approved_by"] = approver
        return True, "Proposal approved successfully"

    def execute_proposal(self, prop_id: str, simulate_failure: bool = False) -> Tuple[bool, str]:
        proposal = self.proposals.get(prop_id)
        if not proposal:
            return False, "Proposal not found"
        if proposal["status"] != self.STATE_APPROVED:
            return False, f"Cannot execute proposal in state {proposal['status']}: must be APPROVED first"

        proposal["status"] = self.STATE_EXECUTING
        if simulate_failure:
            # Trigger rollback command
            proposal["status"] = self.STATE_FAILED_ROLLEDBACK
            proposal["rollback_executed"] = True
            return False, "Execution failed: auto-rollback triggered"

        proposal["status"] = self.STATE_EXECUTED
        proposal["executed_at"] = time.time()
        return True, "Executed successfully"


class NewAPIGatewaySimulator:
    """
    Simulates New API key pool weight round-robin & model alias resolution
    via proxy 172.17.0.1:7890 (JP/US nodes).
    """

    MODEL_ALIASES = {
        "agent-fast": ["gemini-3.5-flash-lite", "gemini-3.1-flash-lite"],
        "agent-deep": ["gemini-3.8-flash", "gpt-oss-120b"],
        "agent-guard": ["qwen/qwen3.8-27b", "meta-llama/llama-prompt-guard-2-86m"],
        "agent-fallback": ["gemma-4-31b"]
    }

    def __init__(self, google_keys: List[str], groq_keys: List[str]):
        self.google_keys = google_keys
        self.groq_keys = groq_keys
        self.google_index = 0
        self.groq_index = 0
        self.key_status = {k: "healthy" for k in (google_keys + groq_keys)}

    def mark_key_rate_limited(self, key: str):
        self.key_status[key] = "rate_limited"

    def get_next_healthy_key(self, channel: str) -> Optional[str]:
        keys = self.google_keys if channel == "google" else self.groq_keys
        start_idx = self.google_index if channel == "google" else self.groq_index
        n = len(keys)

        for i in range(n):
            curr_idx = (start_idx + i) % n
            key = keys[curr_idx]
            if self.key_status[key] == "healthy":
                if channel == "google":
                    self.google_index = (curr_idx + 1) % n
                else:
                    self.groq_index = (curr_idx + 1) % n
                return key
        return None

    def resolve_model(self, alias: str) -> List[str]:
        return self.MODEL_ALIASES.get(alias, [])
