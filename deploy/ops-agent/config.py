"""
Configuration module for home-ops-agent.
Adheres strictly to the <= 50MB resident RAM constraint.
"""

import hashlib
import os
import re
from dataclasses import dataclass
from typing import Any, Dict, Optional


OFFLINE_PLACEHOLDER_PREFIX = "offline-placeholder:"
_SHA256_RE = re.compile(r"^sha256:[0-9a-f]{64}$")

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
    Empty strings, 'offline*' prefixes, non-sha256 tokens, and static fallback hashes are all treated as non-real.
    """
    val = (baseline_hash or "").strip().lower()
    if not val:
        return True
    if val.startswith(OFFLINE_PLACEHOLDER_PREFIX) or val.startswith("offline:"):
        return True
    if not _SHA256_RE.match(val):
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
    """Returns True ONLY if baseline_hash is a valid real resource sha256 fingerprint (never a placeholder)."""
    if component and not resolve_canonical_component(component):
        return False
    return not is_placeholder_baseline_hash(baseline_hash, component)


def compute_container_baseline_from_inspect(component: str, inspect_data: Optional[Dict[str, Any]]) -> str:
    """
    Computes a deterministic real resource fingerprint bound to container identity, image,
    state, and resource version info from Docker /containers/{name}/json inspect data (R5).
    Fail-Closed: returns '' on unknown component, missing inspect data, or unknown state.
    """
    canonical = resolve_canonical_component(component)
    if not canonical or not isinstance(inspect_data, dict) or not inspect_data:
        return ""

    container_id = str(inspect_data.get("Id") or "").strip()
    if not container_id:
        return ""

    image_id = str(inspect_data.get("Image") or "").strip()
    cfg_obj = inspect_data.get("Config") if isinstance(inspect_data.get("Config"), dict) else {}
    image_ref = str(cfg_obj.get("Image") or "").strip()
    if not image_id and not image_ref:
        return ""

    state_obj = inspect_data.get("State") if isinstance(inspect_data.get("State"), dict) else {}
    state_status = str(state_obj.get("Status") or "").strip().lower()
    if not state_status and isinstance(state_obj.get("Running"), bool):
        state_status = "running" if state_obj["Running"] else "exited"
    if not state_status or state_status == "unknown":
        return ""

    created_at = str(inspect_data.get("Created") or "").strip()
    started_at = str(state_obj.get("StartedAt") or "").strip()
    try:
        restart_count = int(inspect_data.get("RestartCount") or 0)
    except (TypeError, ValueError):
        return ""

    if not created_at and not started_at and not image_id:
        return ""

    raw = (
        f"v1|{canonical}|{container_id}|{image_id}|{image_ref}|"
        f"{state_status}|{created_at}|{started_at}|{restart_count}"
    )
    digest = hashlib.sha256(raw.encode("utf-8")).hexdigest()
    return f"sha256:{digest}"


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
