"""
Configuration module for home-ops-agent.
Adheres strictly to the <= 50MB resident RAM constraint.
"""

import hashlib
import json
import os
import re
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from typing import Any, Dict, List, Optional, Tuple


OFFLINE_PLACEHOLDER_PREFIX = "offline-placeholder:"
BASELINE_PROTOCOL_VERSION = "v1"
_SHA256_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
_V1_BASELINE_HASH_RE = re.compile(r"^v1:[0-9a-f]{64}$")
_RFC3339_STRICT_RE = re.compile(
    r"^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|([+-])(\d{2}):(\d{2}))$"
)

# Canonical Component Resolution (1-to-1 with Go Core ResolveTopologyComponent / executor.go)
CANONICAL_COMPONENT_MAP = {
    "home-mosquitto": "mosquitto",
    "mosquitto": "mosquitto",
    "mqtt": "mosquitto",
    "broker": "mosquitto",
    "mosquitto-broker": "mosquitto",
    "home-datacenter-mosquitto": "mosquitto",

    "home-frigate": "frigate",
    "nvr": "frigate",
    "frigate": "frigate",
    "go2rtc": "frigate",
    "home-datacenter-frigate": "frigate",

    "app.db": "app.db",
    "database": "app.db",
    "sqlite": "app.db",
    "db": "app.db",

    "home-api": "home-api",
    "api": "home-api",
    "backend": "home-api",
    "go-api": "home-api",
    "home-datacenter-api": "home-api",

    "home-web": "home-web",
    "web": "home-web",
    "frontend": "home-web",
    "nginx": "home-web",
    "home-datacenter-web": "home-web",

    "home-vision": "home-vision",
    "vision": "home-vision",
    "ai-vision": "home-vision",
    "home-datacenter-vision": "home-vision",

    "home-cloudflared": "home-cloudflared",
    "cloudflared": "home-cloudflared",
    "home-datacenter-cloudflared": "home-cloudflared",

    "home-h3c-keepalive": "home-h3c-keepalive",
    "h3c": "home-h3c-keepalive",
    "home-datacenter-h3c-keepalive": "home-h3c-keepalive",

    "home-new-api": "home-new-api",
    "new-api": "home-new-api",
    "home-datacenter-new-api": "home-new-api",

    "home-ops-agent": "home-ops-agent",
    "ops-agent": "home-ops-agent",
    "home-datacenter-ops-agent": "home-ops-agent",

    "home-backup": "home-backup",
    "backup": "home-backup",
    "home-datacenter-backup": "home-backup",

    "mihomo": "mihomo",
    "tailscale": "tailscale",

    "1panel-openresty-wudu": "openresty",
    "1panel-openresty": "openresty",
    "openresty": "openresty",
    "nginx-proxy": "openresty",
    "1pwaf": "openresty",
    "waf": "openresty",
    "openresty-waf": "openresty",
    "home-gateway": "openresty",
    "gateway": "openresty",
    "home-datacenter-gateway": "openresty",
}

CANONICAL_TO_DOCKER_CONTAINER = {
    "frigate": "home-frigate",
    "mosquitto": "home-mosquitto",
    "home-api": "home-api",
    "home-web": "home-web",
    "home-vision": "home-vision",
    "openresty": "1Panel-openresty-WuDU",
    "home-cloudflared": "home-cloudflared",
    "home-h3c-keepalive": "home-h3c-keepalive",
    "home-new-api": "home-new-api",
    "home-ops-agent": "home-ops-agent",
    "home-backup": "home-backup",
    "mihomo": "mihomo",
    "tailscale": "tailscale",
}


def resolve_canonical_component(name: str) -> str:
    """Normalizes known component names and aliases; returns '' (Fail-Closed) on unknown (R4-17)."""
    cleaned = (name or "").strip().lower().lstrip("/")
    if "(" in cleaned:
        prefix = cleaned.split("(")[0].strip()
        if prefix:
            cleaned = prefix
    if not cleaned:
        return ""
    return CANONICAL_COMPONENT_MAP.get(cleaned, "")


def resolve_docker_container_name(canonical: str) -> str:
    """Resolves canonical component name to Docker container name; returns '' on unknown (R4-17)."""
    cleaned = resolve_canonical_component(canonical)
    if not cleaned:
        return ""
    return CANONICAL_TO_DOCKER_CONTAINER.get(cleaned, "")


def is_production_env() -> bool:
    """Returns True when running in production mode."""
    env = os.environ.get("ENVIRONMENT", os.environ.get("APP_ENV", os.environ.get("ENV", ""))).lower().strip()
    if env in ("development", "dev", "test", "testing"):
        return False
    if env in ("production", "prod"):
        return True
    return os.path.exists("/.dockerenv")


def _known_legacy_placeholder_hashes() -> set[str]:
    """Computes known static placeholder hashes that must never pass production validation."""
    hashes: set[str] = set()
    candidates = set(CANONICAL_COMPONENT_MAP.values()) | {"1pwaf"}
    for canonical in candidates:
        hashes.add(f"sha256:{hashlib.sha256(f'{canonical}:offline:none'.encode('utf-8')).hexdigest()}")
        docker_name = CANONICAL_TO_DOCKER_CONTAINER.get(canonical, canonical)
        if canonical == "1pwaf":
            docker_name = "1Panel-openresty-WuDU"
        hashes.add(
            f"sha256:{hashlib.sha256(f'{canonical}:channels-tool/{docker_name}:latest:running:'.encode('utf-8')).hexdigest()}"
        )
    return hashes


_LEGACY_PLACEHOLDER_HASHES = _known_legacy_placeholder_hashes()


def compute_offline_placeholder_baseline(component: str) -> str:
    """
    Returns an explicit non-production offline placeholder marker (distinct from real sha256 fingerprints).
    Returns '' if component resolution fails.
    """
    canonical = resolve_canonical_component(component)
    if not canonical:
        return ""
    return f"{OFFLINE_PLACEHOLDER_PREFIX}{canonical}"


def is_placeholder_baseline_hash(baseline_hash: str, component: str = "") -> bool:
    """
    Distinguishes offline/static placeholder fingerprints from real resource fingerprints (R5).
    Empty strings, 'offline*' prefixes, non-(v1|sha256) tokens, and static fallback hashes are rejected.
    """
    val = (baseline_hash or "").strip().lower()
    if not val:
        return True
    if val.startswith(OFFLINE_PLACEHOLDER_PREFIX) or val.startswith("offline:"):
        return True
    if not (_V1_BASELINE_HASH_RE.match(val) or _SHA256_RE.match(val)):
        return True
    if val in _LEGACY_PLACEHOLDER_HASHES:
        return True
    if component:
        canonical = resolve_canonical_component(component)
        if canonical:
            legacy_offline = f"sha256:{hashlib.sha256(f'{canonical}:offline:none'.encode('utf-8')).hexdigest()}"
            if val == legacy_offline:
                return True
    return False


def is_real_baseline_hash(baseline_hash: str, component: str = "") -> bool:
    """
    Returns True ONLY if baseline_hash is a valid real resource fingerprint.
    In production (is_production_env() or OPS_STRICT_BASELINE=1), strictly requires
    Baseline Protocol v1 ('v1:<64-hex>') and rejects legacy 'sha256:<hex>' formats.
    """
    if component and not resolve_canonical_component(component):
        return False
    if is_placeholder_baseline_hash(baseline_hash, component):
        return False
    if is_production_env() or os.environ.get("OPS_STRICT_BASELINE", "") == "1":
        return bool(_V1_BASELINE_HASH_RE.match((baseline_hash or "").strip()))
    return True


def is_real_baseline_hash_v1(baseline_hash: str, component: str = "") -> bool:
    """Returns True ONLY if baseline_hash strictly matches Baseline Protocol v1 ('v1:<64-hex>') and is non-placeholder."""
    val = (baseline_hash or "").strip()
    if not _V1_BASELINE_HASH_RE.match(val):
        return False
    return is_real_baseline_hash(val, component)


def _baseline_canonical_json_bytes(obj: Any) -> bytes:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False, allow_nan=False).encode("utf-8")


def _baseline_sha256_hex(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _baseline_str(mapping: Optional[Dict[str, Any]], key: str) -> str:
    if not isinstance(mapping, dict):
        return ""
    v = mapping.get(key)
    if v is None:
        return ""
    if isinstance(v, str):
        return v.strip()
    return str(v).strip()


def _baseline_map(mapping: Optional[Dict[str, Any]], key: str) -> Dict[str, Any]:
    if not isinstance(mapping, dict):
        return {}
    v = mapping.get(key)
    return v if isinstance(v, dict) else {}


def _baseline_bool(mapping: Optional[Dict[str, Any]], key: str) -> Tuple[bool, bool]:
    if not isinstance(mapping, dict):
        return False, True
    if key not in mapping or mapping[key] is None:
        return False, True
    v = mapping[key]
    if isinstance(v, bool):
        return v, True
    return False, False


def _baseline_int64(mapping: Optional[Dict[str, Any]], key: str) -> Tuple[int, bool]:
    if not isinstance(mapping, dict):
        return 0, True
    if key not in mapping or mapping[key] is None:
        return 0, True
    v = mapping[key]
    if isinstance(v, bool):
        return 0, False
    if isinstance(v, int):
        return int(v), True
    if isinstance(v, float):
        if int(v) != v:
            return 0, False
        return int(v), True
    return 0, False


def _baseline_trimmed_str_list(raw: Any) -> List[str]:
    if not isinstance(raw, list):
        return []
    out: List[str] = []
    for item in raw:
        if item is None:
            continue
        s = item.strip() if isinstance(item, str) else str(item).strip()
        if s:
            out.append(s)
    return out


def _baseline_raw_str_list(raw: Any) -> List[str]:
    if not isinstance(raw, list):
        return []
    out: List[str] = []
    for item in raw:
        if isinstance(item, str) and item != "":
            out.append(item)
    return out


def _baseline_sorted_unique_strings(raw: Any, upper: bool = False) -> List[str]:
    seen: set[str] = set()
    out: List[str] = []
    for s in _baseline_trimmed_str_list(raw):
        val = s.upper() if upper else s
        if val not in seen:
            seen.add(val)
            out.append(val)
    out.sort()
    return out


def _normalize_baseline_timestamp(raw: Any) -> Tuple[str, bool]:
    if raw is None:
        return "", True
    s = raw.strip() if isinstance(raw, str) else str(raw).strip()
    if not s:
        return "", True
    m = _RFC3339_STRICT_RE.match(s)
    if not m:
        return "", False
    year, month, day, hour, minute, second, frac, tz_part, tz_sign, tz_h, tz_m = m.groups()
    try:
        y_i, mo_i, d_i = int(year), int(month), int(day)
        h_i, mi_i, s_i = int(hour), int(minute), int(second)
        if tz_part == "Z":
            tz = timezone.utc
        else:
            tzh_i, tzm_i = int(tz_h), int(tz_m)
            if tzh_i > 23 or tzm_i > 59:
                return "", False
            offset_delta = timedelta(hours=tzh_i, minutes=tzm_i)
            if tz_sign == "-":
                offset_delta = -offset_delta
            tz = timezone(offset_delta)
        dt = datetime(y_i, mo_i, d_i, h_i, mi_i, s_i, tzinfo=tz)
        micro = int((frac or "").ljust(6, "0")[:6])
        dt = dt + timedelta(microseconds=micro)
        utc_dt = dt.astimezone(timezone.utc)
    except (ValueError, OverflowError):
        return "", False
    if utc_dt.year <= 1:
        return "", True
    ms = utc_dt.microsecond // 1000
    return f"{utc_dt.strftime('%Y-%m-%dT%H:%M:%S')}.{ms:03d}Z", True


def _has_ro_mode_token(mode: str) -> bool:
    for tok in mode.lower().split(","):
        if tok.strip() == "ro":
            return True
    return False


def _parse_bind_string(bind: str) -> Optional[Dict[str, Any]]:
    trimmed = bind.strip()
    if not trimmed:
        return None
    parts = trimmed.split(":")
    if len(parts) >= 3 and len(parts[0]) == 1 and (parts[1].startswith("\\") or parts[1].startswith("/")):
        parts = [parts[0] + ":" + parts[1]] + parts[2:]
    if len(parts) < 2:
        return None
    src = parts[0].strip()
    dst = parts[1].strip()
    mode = ":".join(parts[2:]).strip() if len(parts) >= 3 else ""
    return {
        "destination": dst,
        "mode": mode,
        "rw": not _has_ro_mode_token(mode),
        "source": src,
        "type": "bind",
    }


def _normalize_baseline_mounts(inspect_data: Dict[str, Any], host_cfg: Dict[str, Any]) -> List[Dict[str, Any]]:
    seen: set[str] = set()
    out: List[Dict[str, Any]] = []

    def _add_mount(m: Dict[str, Any]) -> None:
        key = f"{m['type']}|{m['source']}|{m['destination']}|{m['mode']}|{str(m['rw']).lower()}"
        if key in seen:
            return
        seen.add(key)
        out.append(m)

    raw_mounts = inspect_data.get("Mounts")
    if isinstance(raw_mounts, list) and len(raw_mounts) > 0:
        for item in raw_mounts:
            if not isinstance(item, dict):
                continue
            src = _baseline_str(item, "Source") or _baseline_str(item, "Name")
            dst = _baseline_str(item, "Destination")
            typ = _baseline_str(item, "Type").lower()
            mode = _baseline_str(item, "Mode")
            rw = not _has_ro_mode_token(mode)
            if "RW" in item and isinstance(item["RW"], bool):
                rw = item["RW"]
            _add_mount({
                "destination": dst,
                "mode": mode,
                "rw": rw,
                "source": src,
                "type": typ,
            })
    elif host_cfg:
        for b in _baseline_trimmed_str_list(host_cfg.get("Binds")):
            parsed = _parse_bind_string(b)
            if parsed is not None:
                _add_mount(parsed)

    out.sort(key=lambda m: (m["destination"], m["source"], m["type"], m["mode"], m["rw"]))
    return out


def _normalize_port_bindings(host_cfg: Dict[str, Any]) -> List[str]:
    pb_map = _baseline_map(host_cfg, "PortBindings")
    if not pb_map:
        return []
    seen: set[str] = set()
    out: List[str] = []

    def _add(val: str) -> None:
        if val not in seen:
            seen.add(val)
            out.append(val)

    for port_proto, raw_list in pb_map.items():
        clean_port = str(port_proto).strip()
        if not isinstance(raw_list, list) or len(raw_list) == 0:
            _add(f"{clean_port}->")
            continue
        for b in raw_list:
            if not isinstance(b, dict):
                continue
            host_ip = _baseline_str(b, "HostIp")
            host_port = _baseline_str(b, "HostPort")
            _add(f"{clean_port}->{host_ip}:{host_port}")
    out.sort()
    return out


def _normalize_devices(host_cfg: Dict[str, Any]) -> List[str]:
    raw_devices = host_cfg.get("Devices") if isinstance(host_cfg, dict) else None
    if not isinstance(raw_devices, list) or len(raw_devices) == 0:
        return []
    seen: set[str] = set()
    out: List[str] = []
    for d in raw_devices:
        if not isinstance(d, dict):
            continue
        spec = (
            f"{_baseline_str(d, 'PathOnHost')}:"
            f"{_baseline_str(d, 'PathInContainer')}:"
            f"{_baseline_str(d, 'CgroupPermissions')}"
        )
        if spec not in seen:
            seen.add(spec)
            out.append(spec)
    out.sort()
    return out


def _normalize_env_digest(cfg: Dict[str, Any]) -> str:
    raw_env = _baseline_raw_str_list(cfg.get("Env"))
    if not raw_env:
        return ""
    seen: set[str] = set()
    hashed: List[str] = []
    for entry in raw_env:
        if "=" in entry:
            k, v = entry.split("=", 1)
        else:
            k, v = entry, ""
        token = f"{k}:{_baseline_sha256_hex(v.encode('utf-8'))}"
        if token not in seen:
            seen.add(token)
            hashed.append(token)
    hashed.sort()
    return f"sha256:{_baseline_sha256_hex(_baseline_canonical_json_bytes(hashed))}"


def _normalize_image_digest(inspect_data: Dict[str, Any], image_ref: str) -> str:
    seen: set[str] = set()
    digests: List[str] = []
    for raw in _baseline_trimmed_str_list(inspect_data.get("RepoDigests")):
        d = raw.rsplit("@", 1)[1].strip() if "@" in raw else raw
        if d and d not in seen:
            seen.add(d)
            digests.append(d)
    digests.sort()
    if digests:
        return ",".join(digests)
    if "@sha256:" in image_ref:
        return image_ref.rsplit("@sha256:", 1)[1].strip() and f"sha256:{image_ref.rsplit('@sha256:', 1)[1].strip()}"
    return ""


def compute_container_baseline_details_from_inspect(
    component: str, inspect_data: Optional[Dict[str, Any]]
) -> Dict[str, Any]:
    """
    Computes the Baseline Protocol v1 document, canonical JSON, sub-hashes
    (identity_hash, security_hash, state_hash), and baseline_hash ('v1:<64-hex>')
    from Docker /containers/{name}/json inspect data, matching Go Core byte-for-byte.
    Fail-Closed: returns {} on unknown component, missing/invalid metadata, or bad timestamps.
    """
    canonical = resolve_canonical_component(component)
    if not canonical or not isinstance(inspect_data, dict) or not inspect_data:
        return {}

    container_id = _baseline_str(inspect_data, "Id")
    if not container_id:
        return {}
    container_name = _baseline_str(inspect_data, "Name").lstrip("/")

    cfg = _baseline_map(inspect_data, "Config")
    image_id = _baseline_str(inspect_data, "Image")
    image_ref = _baseline_str(cfg, "Image")
    if not image_id and not image_ref:
        return {}
    image_digest = _normalize_image_digest(inspect_data, image_ref)

    state_obj = _baseline_map(inspect_data, "State")
    container_state = _baseline_str(state_obj, "Status").lower()
    if not container_state and isinstance(state_obj.get("Running"), bool):
        container_state = "running" if state_obj["Running"] else "exited"
    if not container_state or container_state == "unknown":
        return {}

    created_at, created_ok = _normalize_baseline_timestamp(inspect_data.get("Created"))
    if not created_ok:
        return {}
    started_at, started_ok = _normalize_baseline_timestamp(state_obj.get("StartedAt"))
    if not started_ok:
        return {}
    if not created_at and not started_at and not image_id:
        return {}

    restart_count, rc_ok = _baseline_int64(inspect_data, "RestartCount")
    if not rc_ok or restart_count < 0:
        return {}

    host_cfg = _baseline_map(inspect_data, "HostConfig")
    privileged, ok_priv = _baseline_bool(host_cfg, "Privileged")
    readonly_rootfs, ok_ro = _baseline_bool(host_cfg, "ReadonlyRootfs")
    mem_limit, ok_mem = _baseline_int64(host_cfg, "Memory")
    nano_cpus, ok_cpu = _baseline_int64(host_cfg, "NanoCpus")
    pids_limit, ok_pids = _baseline_int64(host_cfg, "PidsLimit")
    if not (ok_priv and ok_ro and ok_mem and ok_cpu and ok_pids):
        return {}

    ep_list = _baseline_raw_str_list(cfg.get("Entrypoint"))
    entrypoint_digest = f"sha256:{_baseline_sha256_hex(_baseline_canonical_json_bytes(ep_list))}" if ep_list else ""
    cmd_list = _baseline_raw_str_list(cfg.get("Cmd"))
    command_digest = f"sha256:{_baseline_sha256_hex(_baseline_canonical_json_bytes(cmd_list))}" if cmd_list else ""
    env_digest = _normalize_env_digest(cfg)

    restart_policy = ""
    rp = _baseline_map(host_cfg, "RestartPolicy")
    if rp:
        rp_name = _baseline_str(rp, "Name")
        if rp_name:
            max_retry, rp_ok = _baseline_int64(rp, "MaximumRetryCount")
            if not rp_ok:
                return {}
            restart_policy = f"{rp_name}:{max_retry}" if max_retry > 0 else rp_name

    cgroup_mode = _baseline_str(host_cfg, "CgroupnsMode") or _baseline_str(host_cfg, "CgroupMode")
    app_armor = _baseline_str(inspect_data, "AppArmorProfile")
    proc_label = _baseline_str(inspect_data, "ProcessLabel")
    if app_armor and proc_label:
        runtime_profile = f"{app_armor}|{proc_label}"
    else:
        runtime_profile = app_armor or proc_label

    identity = {
        "canonical_component": canonical,
        "container_id": container_id,
        "container_name": container_name,
        "image_digest": image_digest,
        "image_id": image_id,
        "image_reference": image_ref,
    }
    security = {
        "cap_add": _baseline_sorted_unique_strings(host_cfg.get("CapAdd"), upper=True),
        "cap_drop": _baseline_sorted_unique_strings(host_cfg.get("CapDrop"), upper=True),
        "cgroup_mode": cgroup_mode,
        "cgroup_parent": _baseline_str(host_cfg, "CgroupParent"),
        "command_digest": command_digest,
        "devices": _normalize_devices(host_cfg),
        "entrypoint_digest": entrypoint_digest,
        "env_digest": env_digest,
        "ipc_mode": _baseline_str(host_cfg, "IpcMode"),
        "memory_limit": mem_limit,
        "mounts": _normalize_baseline_mounts(inspect_data, host_cfg),
        "nano_cpus": nano_cpus,
        "network_mode": _baseline_str(host_cfg, "NetworkMode"),
        "pid_mode": _baseline_str(host_cfg, "PidMode"),
        "pids_limit": pids_limit,
        "port_bindings": _normalize_port_bindings(host_cfg),
        "privileged": privileged,
        "readonly_rootfs": readonly_rootfs,
        "restart_policy": restart_policy,
        "runtime": _baseline_str(host_cfg, "Runtime"),
        "runtime_security_profile": runtime_profile,
        "security_opt": _baseline_sorted_unique_strings(host_cfg.get("SecurityOpt"), upper=False),
        "user": _baseline_str(cfg, "User"),
        "uts_mode": _baseline_str(host_cfg, "UTSMode"),
    }
    state = {
        "container_state": container_state,
        "created_at": created_at,
        "restart_count": restart_count,
        "started_at": started_at,
    }
    doc = {
        "identity": identity,
        "protocol_version": BASELINE_PROTOCOL_VERSION,
        "security": security,
        "state": state,
    }

    try:
        canonical_bytes = _baseline_canonical_json_bytes(doc)
        id_bytes = _baseline_canonical_json_bytes(identity)
        sec_bytes = _baseline_canonical_json_bytes(security)
        st_bytes = _baseline_canonical_json_bytes(state)
    except (TypeError, ValueError):
        return {}

    return {
        "protocol_version": BASELINE_PROTOCOL_VERSION,
        "baseline_hash": f"v1:{_baseline_sha256_hex(canonical_bytes)}",
        "identity_hash": f"sha256:{_baseline_sha256_hex(id_bytes)}",
        "security_hash": f"sha256:{_baseline_sha256_hex(sec_bytes)}",
        "state_hash": f"sha256:{_baseline_sha256_hex(st_bytes)}",
        "canonical_json": canonical_bytes.decode("utf-8"),
        "document": doc,
    }


def compute_container_baseline_from_inspect(component: str, inspect_data: Optional[Dict[str, Any]]) -> str:
    """
    Computes the Baseline Protocol v1 fingerprint ('v1:<64-hex>') bound to container identity,
    all 24 runtime security posture fields, and state from Docker inspect data (R5/R8).
    Fail-Closed: returns '' on unknown component, missing inspect data, bad timestamps, or unknown state.
    """
    details = compute_container_baseline_details_from_inspect(component, inspect_data)
    return str(details.get("baseline_hash") or "")


@dataclass
class OpsAgentConfig:
    name: str = "home-ops-agent"
    runtime: str = "python:3.11-slim"
    max_memory_mb: int = 50
    async_loop: str = "asyncio"

    # Infrastructure endpoints
    docker_socket: str = os.getenv("DOCKER_SOCKET", "/var/run/docker.sock")
    mqtt_broker: str = os.getenv("MQTT_BROKER", "home-mosquitto")
    mqtt_port: int = int(os.getenv("MQTT_PORT", "1883"))
    mqtt_topic: str = os.getenv("MQTT_TOPIC", "security/#")
    mqtt_user: str = os.getenv("MQTT_USER", "")
    mqtt_password: str = os.getenv("MQTT_PASSWORD", "")
    mqtt_tls: bool = os.getenv("MQTT_TLS", "false").lower() in ("true", "1", "yes")
    mqtt_lwt_topic: str = os.getenv("MQTT_LWT_TOPIC", "home-datacenter/ops-agent/status")
    mqtt_client_id: str = os.getenv("MQTT_CLIENT_ID", "")
    mqtt_qos: int = int(os.getenv("MQTT_QOS", "1"))
    api_base_url: str = os.getenv("API_BASE_URL", "http://home-api:8080")
    internal_token: str = os.getenv("INTERNAL_OPS_TOKEN", "")
    new_api_url: str = os.getenv("NEW_API_URL", "http://home-new-api:3000/v1")
    new_api_token: str = os.getenv("NEW_API_TOKEN", "")
    new_api_model: str = os.getenv("NEW_API_MODEL", "agent-fast")
    db_path: str = os.getenv("DB_PATH", "/data/sqlite/app.db")

    # Tier 1 Thresholds
    auth_fail_threshold: int = 5
    auth_fail_window_sec: float = 300.0  # 5 minutes
    crash_escalation_threshold: int = 3
    crash_window_sec: float = 900.0      # 15 minutes
    disk_threshold_percent: float = 85.0
    disk_cleanup_cooldown_sec: float = float(os.getenv("DISK_CLEANUP_COOLDOWN_SEC", "3600.0"))  # 1 hour cooldown

    # Low peak scheduling window (03:00 to 05:00 AM)
    low_peak_start_hour: int = 3
    low_peak_end_hour: int = 5

    @classmethod
    def load(cls) -> "OpsAgentConfig":
        return cls()
