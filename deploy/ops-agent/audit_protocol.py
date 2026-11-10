"""
Cross-language tamper-evident AgentAuditEngine protocol for Home Datacenter Ops Agent.
Matches Go services/api/internal/audit and services/api/internal/redact byte-for-byte.
Uses Python 3.11 standard library only (zero heavy dependencies, <2MB RAM footprint).
"""

import datetime
import hashlib
import hmac
import json
import os
import re
import time
import uuid
from typing import Any, Dict, List, Optional, Tuple

AUDIT_GENESIS_HASH = "0" * 64
AUDIT_POLICY_VERSION = "v5.0"
MIN_AUDIT_HMAC_KEY_BYTES = 32
MAX_EVIDENCE_BYTES = 16 * 1024

# Canonical 24-stage lifecycle event types matching model/audit.go 1:1
EVENT_RECEIVED = "EVENT_RECEIVED"
CONTEXT_COLLECTED = "CONTEXT_COLLECTED"
ANALYSIS_COMPLETED = "ANALYSIS_COMPLETED"
RISK_ASSESSED = "RISK_ASSESSED"
PROPOSAL_CREATED = "PROPOSAL_CREATED"
POLICY_EVALUATED = "POLICY_EVALUATED"
APPROVAL_REQUESTED = "APPROVAL_REQUESTED"
APPROVAL_GRANTED = "APPROVAL_GRANTED"
APPROVAL_REJECTED = "APPROVAL_REJECTED"
APPROVAL_EXPIRED = "APPROVAL_EXPIRED"
EXECUTION_LOCK_ACQUIRED = "EXECUTION_LOCK_ACQUIRED"
RECOVERY_LEASE_ACQUIRED = "RECOVERY_LEASE_ACQUIRED"
EXECUTION_STARTED = "EXECUTION_STARTED"
EXECUTION_STEP_COMPLETED = "EXECUTION_STEP_COMPLETED"
EXECUTION_FAILED = "EXECUTION_FAILED"
HEALTH_CHECK_STARTED = "HEALTH_CHECK_STARTED"
HEALTH_CHECK_COMPLETED = "HEALTH_CHECK_COMPLETED"
ROLLBACK_STARTED = "ROLLBACK_STARTED"
ROLLBACK_COMPLETED = "ROLLBACK_COMPLETED"
RECOVERY_VERIFIED = "RECOVERY_VERIFIED"
EXECUTION_COMPLETED = "EXECUTION_COMPLETED"
EXECUTION_UNKNOWN = "EXECUTION_UNKNOWN"
AUDIT_COMMIT_FAILED = "AUDIT_COMMIT_FAILED"
EXECUTION_CANCELLED = "EXECUTION_CANCELLED"

DECISION_ALLOW = "ALLOW"
DECISION_DENY = "DENY"
DECISION_REQUIRE_APPROVAL = "REQUIRE_APPROVAL"

_SENSITIVE_KEY_WORDS = (
    "password", "passwd", "secret", "secret_key", "s3_secret", "jwt", "token",
    "apikey", "api_key", "access_token", "refresh_token", "authorization",
    "pcsrftoken", "psession", "credential", "auth_token", "cookie", "set-cookie",
    "x-internal-secret", "internal_ops_token", "emergency_override_token",
    "session_id", "hmac", "encryption_key", "bitiful", "totp", "otp", "private_key",
)

_SENSITIVE_PATTERN = (
    r"password|passwd|secret|secret_key|s3_secret|jwt|token|apiKey|api_key|"
    r"access_token|refresh_token|authorization|pcsrftoken|psession|credential|"
    r"auth_token|cookie|set-cookie|x-internal-secret|internal_ops_token|"
    r"emergency_override_token|session_id|hmac|encryption_key|bitiful|totp|otp|private_key"
)
_KV_SENSITIVE_PATTERN = (
    r"password|passwd|secret|secret_key|s3_secret|jwt|token|apiKey|api_key|"
    r"access_token|refresh_token|pcsrftoken|psession|credential|"
    r"auth_token|cookie|set-cookie|x-internal-secret|internal_ops_token|"
    r"emergency_override_token|session_id|hmac|encryption_key|bitiful|totp|otp|private_key"
)

_RE_PEM_KEY = re.compile(
    r"-----BEGIN [A-Z0-9_\- ]+PRIVATE KEY-----.*?-----END [A-Z0-9_\- ]+PRIVATE KEY-----",
    re.DOTALL,
)
_RE_URL_CREDS = re.compile(
    r"([a-zA-Z][a-zA-Z0-9+.\-]*://)([^:/\s]+):(.+?)@([a-zA-Z0-9.\-_]+(?::[0-9]+)?(?:/|\?|#|\s|$))"
)
_RE_URL_QUERY_SECRETS = re.compile(
    rf"([?&](?:{_SENSITIVE_PATTERN}|sig|signature|key|x-amz-signature|x-amz-security-token)=)([^&\s#\"']+)",
    re.IGNORECASE,
)
_RE_BEARER = re.compile(r"\b(Bearer\s+)[A-Za-z0-9\-._~+/]+=*", re.IGNORECASE)
_RE_BASIC = re.compile(r"\b(Basic\s+)[A-Za-z0-9+/]+=*", re.IGNORECASE)
_RE_JWT = re.compile(r"\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]+")
_RE_JSON_STRING = re.compile(
    rf'("(?:{_SENSITIVE_PATTERN})"\s*:\s*)"((?:\\.|[^"\\])*)"',
    re.IGNORECASE,
)
_RE_JSON_UNQUOTED = re.compile(
    rf'("(?:{_SENSITIVE_PATTERN})"\s*:\s*)([^"\s,\}}]][^\s,\}}]]*)',
    re.IGNORECASE,
)
_RE_KV_QUOTED = re.compile(
    rf'\b({_KV_SENSITIVE_PATTERN})\s*([=:])\s*(?:"((?:\\.|[^"\\])*)"|\'([^\']*)\')',
    re.IGNORECASE,
)
_RE_KV_UNQUOTED = re.compile(
    rf'\b({_KV_SENSITIVE_PATTERN})\s*([=:])\s*([^\'"\[\s,;}}&][^\s,;}}&]*)',
    re.IGNORECASE,
)


def format_rfc3339_ms(ts_ms: Optional[int] = None, dt: Optional[datetime.datetime] = None) -> str:
    """Formats timestamp in UTC with fixed millisecond precision matching Go FormatRFC3339Ms."""
    if dt is not None:
        utc_dt = dt.astimezone(datetime.timezone.utc) if dt.tzinfo else dt.replace(tzinfo=datetime.timezone.utc)
        total_ms = int(utc_dt.timestamp() * 1000)
    elif ts_ms is not None:
        total_ms = int(ts_ms)
    else:
        total_ms = int(time.time() * 1000)

    sec, ms = divmod(total_ms, 1000)
    utc_dt = datetime.datetime.fromtimestamp(sec, tz=datetime.timezone.utc)
    return utc_dt.strftime("%Y-%m-%dT%H:%M:%S") + f".{ms:03d}Z"


def canonicalize_value(val: Any) -> Any:
    """Recursively normalizes maps, lists, and whole floats matching Go model.CanonicalizeJSONValue."""
    if val is None or isinstance(val, bool):
        return val
    if isinstance(val, float):
        if val.is_integer():
            return int(val)
        return val
    if isinstance(val, dict):
        return {str(k): canonicalize_value(val[k]) for k in sorted(val.keys(), key=lambda x: str(x))}
    if isinstance(val, (list, tuple)):
        return [canonicalize_value(x) for x in val]
    return val


def canonical_json_bytes(val: Any) -> bytes:
    """Produces compact UTF-8 JSON without HTML escaping, matching Go CanonicalJSONBytes byte-for-byte."""
    if isinstance(val, str):
        try:
            decoded = json.loads(val)
            val = decoded
        except Exception:
            pass
    normalized = canonicalize_value(val)
    return json.dumps(normalized, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")


def redact_sensitive_string(text: str) -> str:
    """Applies unified Go-parity secret redaction across logs, URLs, JSON payloads, and headers."""
    if not text:
        return ""
    res = text
    res = _RE_PEM_KEY.sub("[REDACTED_PRIVATE_KEY]", res)
    res = _RE_URL_CREDS.sub(r"\1\2:[REDACTED]@\4", res)
    res = _RE_URL_QUERY_SECRETS.sub(r"\1[REDACTED]", res)
    res = _RE_BEARER.sub(r"\1[REDACTED]", res)
    res = _RE_BASIC.sub(r"\1[REDACTED]", res)
    res = _RE_JWT.sub("[REDACTED_JWT]", res)
    res = _RE_JSON_STRING.sub(r'\1"[REDACTED]"', res)
    res = _RE_JSON_UNQUOTED.sub(r'\1"[REDACTED]"', res)
    res = _RE_KV_QUOTED.sub(r'\1\2"[REDACTED]"', res)
    res = _RE_KV_UNQUOTED.sub(r"\1\2[REDACTED]", res)
    return res


def _is_sensitive_key(key: str) -> bool:
    lower = str(key or "").strip().lower()
    if not lower:
        return False
    return any(w == lower or w in lower for w in _SENSITIVE_KEY_WORDS)


def redact_value(val: Any) -> Any:
    """Recursively scrubs sensitive map keys and string values."""
    if val is None or isinstance(val, (bool, int, float)):
        return val
    if isinstance(val, str):
        return redact_sensitive_string(val)
    if isinstance(val, dict):
        out: Dict[str, Any] = {}
        for k, v in val.items():
            ks = str(k)
            out[ks] = "[REDACTED]" if _is_sensitive_key(ks) else redact_value(v)
        return out
    if isinstance(val, (list, tuple)):
        return [redact_value(x) for x in val]
    return redact_sensitive_string(str(val))


def cap_and_redact_text(raw: str, max_bytes: int = MAX_EVIDENCE_BYTES) -> str:
    if not raw:
        return ""
    sanitized = raw
    trimmed = raw.strip()
    if trimmed.startswith("{") or trimmed.startswith("["):
        try:
            parsed = json.loads(trimmed)
            sanitized = redact_sensitive_string(
                json.dumps(redact_value(parsed), separators=(",", ":"), ensure_ascii=False)
            )
        except Exception:
            sanitized = redact_sensitive_string(raw)
    else:
        sanitized = redact_sensitive_string(raw)

    raw_bytes = sanitized.encode("utf-8")
    if len(raw_bytes) <= max_bytes:
        return sanitized
    digest16 = hashlib.sha256(raw_bytes).hexdigest()[:16]
    suffix = f"...[TRUNCATED:sha256={digest16}]"
    cut = max(0, max_bytes - len(suffix.encode("utf-8")))
    truncated = raw_bytes[:cut].decode("utf-8", errors="ignore")
    return truncated + suffix


def serialize_params(params: Any) -> Tuple[str, str]:
    """Returns (parameters_digest, parameters_redacted) matching Go SerializeParams."""
    if params is None or (isinstance(params, str) and not params.strip()):
        return "", ""
    canon_bytes = canonical_json_bytes(params)
    digest = hashlib.sha256(canon_bytes).hexdigest() if canon_bytes else ""
    if isinstance(params, str):
        redacted = cap_and_redact_text(params, MAX_EVIDENCE_BYTES)
    else:
        red_bytes = canonical_json_bytes(redact_value(params))
        redacted = cap_and_redact_text(red_bytes.decode("utf-8"), MAX_EVIDENCE_BYTES)
    return digest, redacted


def serialize_evidence(val: Any) -> str:
    if val is None:
        return ""
    if isinstance(val, str):
        return cap_and_redact_text(val, MAX_EVIDENCE_BYTES)
    return cap_and_redact_text(canonical_json_bytes(redact_value(val)).decode("utf-8"), MAX_EVIDENCE_BYTES)


def canonical_event_hash_payload(ev: Dict[str, Any]) -> Dict[str, Any]:
    """Builds the exact 29-field Go/Python-identical hash input object."""
    created_at_ms = int(ev.get("created_at_ms") or 0)
    created_at_str = str(ev.get("created_at") or "").strip()
    if created_at_ms == 0 and created_at_str:
        dt = datetime.datetime.fromisoformat(created_at_str.replace("Z", "+00:00"))
        created_at_ms = int(dt.timestamp() * 1000)
    if created_at_ms > 0:
        created_at_str = format_rfc3339_ms(created_at_ms)

    return {
        "audit_id": str(ev.get("audit_id") or ""),
        "run_id": str(ev.get("run_id") or ""),
        "parent_event_id": str(ev.get("parent_event_id") or ""),
        "correlation_id": str(ev.get("correlation_id") or ""),
        "household_id": int(ev.get("household_id") or 1),
        "trigger_source": str(ev.get("trigger_source") or ""),
        "actor_type": str(ev.get("actor_type") or ""),
        "actor_id": str(ev.get("actor_id") or ""),
        "event_type": str(ev.get("event_type") or ""),
        "action": str(ev.get("action") or ""),
        "target": str(ev.get("target") or ""),
        "component": str(ev.get("component") or ""),
        "decision": str(ev.get("decision") or ""),
        "risk_level": str(ev.get("risk_level") or ""),
        "policy_version": str(ev.get("policy_version") or AUDIT_POLICY_VERSION),
        "proposal_id": str(ev.get("proposal_id") or ""),
        "approval_id": str(ev.get("approval_id") or ""),
        "operation_id": str(ev.get("operation_id") or ""),
        "fencing_generation": int(ev.get("fencing_generation") or 0),
        "before_state": str(ev.get("before_state") or ""),
        "after_state": str(ev.get("after_state") or ""),
        "parameters_digest": str(ev.get("parameters_digest") or ""),
        "parameters_redacted": str(ev.get("parameters_redacted") or ""),
        "result": str(ev.get("result") or ""),
        "error_code": str(ev.get("error_code") or ""),
        "duration_ms": int(ev.get("duration_ms") or 0),
        "evidence": str(ev.get("evidence") or ""),
        "created_at": created_at_str,
        "created_at_ms": created_at_ms,
        "is_offline": bool(ev.get("is_offline", False)),
        "previous_hash": str(ev.get("previous_hash") or AUDIT_GENESIS_HASH),
    }


def compute_event_entry_hash(ev: Dict[str, Any]) -> str:
    payload = canonical_event_hash_payload(ev)
    canon_bytes = canonical_json_bytes(payload)
    return hashlib.sha256(canon_bytes).hexdigest()


def resolve_audit_hmac_keyring() -> Tuple[Optional[str], Dict[str, str]]:
    """Loads external OPS_AUDIT_HMAC_KEY / OPS_AUDIT_HMAC_KEYS without JWT fallback."""
    ring: Dict[str, str] = {}
    active_kid = os.environ.get("OPS_AUDIT_HMAC_KEY_ID", "k1").strip() or "k1"

    raw_keys = os.environ.get("OPS_AUDIT_HMAC_KEYS", "").strip()
    if raw_keys:
        for pair in raw_keys.split(","):
            trimmed = pair.strip()
            if ":" in trimmed:
                kid, sec = trimmed.split(":", 1)
                kid = kid.strip()
                sec = sec.strip()
                if kid and sec and len(sec) >= MIN_AUDIT_HMAC_KEY_BYTES:
                    ring[kid] = sec

    primary = os.environ.get("OPS_AUDIT_HMAC_KEY", "").strip()
    if primary and len(primary) >= MIN_AUDIT_HMAC_KEY_BYTES:
        ring[active_kid] = primary

    if active_kid in ring:
        return active_kid, ring
    if ring:
        first_kid = next(iter(ring.keys()))
        return first_kid, ring
    return None, {}


def compute_event_hmac(
    kid: str,
    secret: str,
    stream_id: str,
    seq: int,
    entry_hash: str,
    created_at_ms: int,
) -> str:
    msg = f"{stream_id}|{int(seq)}|{entry_hash}|{int(created_at_ms)}".encode("utf-8")
    sig_hex = hmac.new(secret.encode("utf-8"), msg, hashlib.sha256).hexdigest()
    return f"hmac-sha256:{kid}:{sig_hex}"


def verify_event_hmac(ev: Dict[str, Any], ring: Dict[str, str], stream_id: str = "core") -> bool:
    sig = str(ev.get("hmac_signature") or "").strip()
    parts = sig.split(":", 2)
    if len(parts) != 3 or parts[0] != "hmac-sha256" or not parts[1] or not parts[2]:
        return False
    kid = parts[1]
    secret = ring.get(kid, "")
    if len(secret) < MIN_AUDIT_HMAC_KEY_BYTES:
        return False
    expected = compute_event_hmac(
        kid=kid,
        secret=secret,
        stream_id=str(ev.get("stream_id") or stream_id),
        seq=int(ev.get("seq") or 0),
        entry_hash=str(ev.get("entry_hash") or ""),
        created_at_ms=int(ev.get("created_at_ms") or 0),
    )
    return hmac.compare_digest(sig, expected)


def append_jsonl_audit_stub(archive_dir: str, filename: str, record: Dict[str, Any]) -> bool:
    """Appends an immutable JSONL line into independent local archive dir (mode 0600)."""
    if not archive_dir:
        return False
    try:
        os.makedirs(archive_dir, exist_ok=True)
        path = os.path.join(archive_dir, filename)
        line = json.dumps(record, sort_keys=True, separators=(",", ":"), ensure_ascii=False) + "\n"
        fd = os.open(path, os.O_CREAT | os.O_WRONLY | os.O_APPEND, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(line)
            fh.flush()
            os.fsync(fh.fileno())
        return True
    except Exception:
        return False


def make_deterministic_audit_id(prefix: str = "aud-py") -> str:
    return f"{prefix}-{int(time.time() * 1000)}-{uuid.uuid4().hex[:10]}"
