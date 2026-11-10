"""
home-ops-agent — Autonomous Edge Operations Daemon.
Python 3.11 asynchronous daemon with strict <= 50MB resident RAM constraint.
"""

import asyncio
import json
import logging
import enum
import os
import shutil
import signal
import socket
import sqlite3
import sys
import time
import urllib.error
import urllib.request
import uuid
from typing import Any, Dict, Optional, Tuple, Callable, Awaitable

class CoreReportingStatus(enum.Enum):
    RECOVERED_BY_CORE = "RECOVERED_BY_CORE"  # Go Core executed recovery and verified health
    AUTHORIZED = "AUTHORIZED"                # Go Core explicitly approved restart
    POLICY_REJECTED = "POLICY_REJECTED"      # Go Core returned 400/401/403/409/422/429
    BLOCKED_BY_FSM = "BLOCKED_BY_FSM"        # Go Core returned 200, but decision is BLOCKED/IGNORE/TRIPPED
    SERVER_ERROR = "SERVER_ERROR"            # Go Core returned 5xx
    OFFLINE = "OFFLINE"                      # Genuine network error: connection refused/timeout

class MaintenanceLockState(str, enum.Enum):
    LOCKED = "LOCKED"
    UNLOCKED = "UNLOCKED"
    UNKNOWN = "UNKNOWN"

class CircuitBreakerState(str, enum.Enum):
    OPEN = "OPEN"
    HALF_OPEN = "HALF_OPEN"
    CLOSED = "CLOSED"
    UNKNOWN = "UNKNOWN"

class DockerHealthState(str, enum.Enum):
    HEALTHY = "HEALTHY"
    UNHEALTHY = "UNHEALTHY"
    RUNNING_UNHEALTHCHECKED = "RUNNING_UNHEALTHCHECKED"
    UNKNOWN = "UNKNOWN"

class SQLiteFaultClass(str, enum.Enum):
    DATABASE_MISSING = "DATABASE_MISSING"
    DATABASE_CORRUPT = "DATABASE_CORRUPT"
    TABLE_MISSING = "TABLE_MISSING"
    COLUMN_MISSING = "COLUMN_MISSING"
    SCHEMA_VERSION_MISMATCH = "SCHEMA_VERSION_MISMATCH"
    DATABASE_READONLY = "DATABASE_READONLY"
    SQLITE_BUSY = "SQLITE_BUSY"
    SQLITE_FULL = "SQLITE_FULL"
    TRANSACTION_FAILED = "TRANSACTION_FAILED"
    TIMESTAMP_INVALID = "TIMESTAMP_INVALID"
    RECORD_INCONSISTENT = "RECORD_INCONSISTENT"

class AuditStatus(str, enum.Enum):
    AUDIT_COMMITTED = "AUDIT_COMMITTED"
    AUDIT_PENDING = "AUDIT_PENDING"
    AUDIT_FAILED = "AUDIT_FAILED"
    AUDIT_UNKNOWN = "AUDIT_UNKNOWN"

REQUIRED_OPS_SCHEMA_VERSION = 4

def classify_sqlite_error(exc: Exception) -> SQLiteFaultClass:
    msg = str(exc).lower()
    if isinstance(exc, FileNotFoundError) or "unable to open database" in msg or "no such file" in msg:
        return SQLiteFaultClass.DATABASE_MISSING
    if "no such table" in msg:
        return SQLiteFaultClass.TABLE_MISSING
    if "no such column" in msg or "has no column named" in msg:
        return SQLiteFaultClass.COLUMN_MISSING
    if "readonly" in msg or "read-only" in msg or "attempt to write a readonly database" in msg:
        return SQLiteFaultClass.DATABASE_READONLY
    if "locked" in msg or "busy" in msg:
        return SQLiteFaultClass.SQLITE_BUSY
    if "disk full" in msg or "database or disk is full" in msg:
        return SQLiteFaultClass.SQLITE_FULL
    if "not a database" in msg or "malformed" in msg or "corrupt" in msg:
        return SQLiteFaultClass.DATABASE_CORRUPT
    return SQLiteFaultClass.TRANSACTION_FAILED

class SecurityMetricsCollector:
    """In-process telemetry counters and security event emitter (R4-25)."""

    def __init__(self):
        self.counters: Dict[str, int] = {
            "recovery_lease_active": 0,
            "recovery_lease_expired": 0,
            "recovery_lease_renewal_failed": 0,
            "recovery_fencing_rejected": 0,
            "recovery_execution_unknown": 0,
            "recovery_audit_failed": 0,
            "circuit_breaker_degraded": 0,
            "maintenance_lock_degraded": 0,
            "proposal_cas_conflict": 0,
            "proposal_digest_mismatch": 0,
            "recovery_health_unknown": 0,
            "recovery_database_error": 0,
        }

    def inc(self, name: str, delta: int = 1) -> None:
        self.counters[name] = self.counters.get(name, 0) + delta

    def set_gauge(self, name: str, val: int) -> None:
        self.counters[name] = val

    def snapshot(self) -> Dict[str, int]:
        return dict(self.counters)

# Canonical Component Resolution (1-to-1 with Go Core ResolveTopologyComponent)
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

    "home-ops-agent": "home-ops-agent",
    "ops-agent": "home-ops-agent",
    "home-datacenter-ops-agent": "home-ops-agent",

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
    "home-ops-agent": "home-ops-agent",
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

def open_readonly_sqlite(db_path: str) -> sqlite3.Connection:
    """
    Opens a SQLite database strictly in read-only mode (URI mode=ro).
    Will NEVER create the database file if it does not exist.
    Raises FileNotFoundError if file is missing or not a file.
    """
    if not db_path or not os.path.isfile(db_path):
        raise FileNotFoundError(f"SQLite database file not found or is not a file: {db_path}")
    import pathlib
    posix_path = pathlib.Path(os.path.abspath(db_path)).as_posix()
    uri = f"file:{posix_path}?mode=ro"
    conn = sqlite3.connect(uri, uri=True, timeout=5.0)
    conn.execute("PRAGMA busy_timeout=5000;")
    return conn

def open_readwrite_sqlite(db_path: str) -> sqlite3.Connection:
    """
    Opens an EXISTING SQLite database in read-write mode (URI mode=rw) with manual transaction
    control so callers can issue BEGIN IMMEDIATE for cross-process serialization.
    NEVER auto-creates a missing database file (R4-14, R4-15, R4-18).
    """
    if not db_path or not os.path.isfile(db_path):
        raise FileNotFoundError(f"SQLite database file not found or is not a file: {db_path}")
    import pathlib
    posix_path = pathlib.Path(os.path.abspath(db_path)).as_posix()
    uri = f"file:{posix_path}?mode=rw"
    conn = sqlite3.connect(uri, uri=True, timeout=5.0, isolation_level=None)
    conn.execute("PRAGMA busy_timeout=5000;")
    return conn

def parse_sqlite_timestamp(val: Any) -> Tuple[Optional[float], bool]:
    """
    Parses datetime values stored in SQLite by GORM / pure-Go SQLite / Python.
    Returns (timestamp_epoch, is_valid).
    If val is None or empty string -> (None, True) (represents no expiration).
    If val is a valid timestamp -> (timestamp_float, True).
    If val is malformed/unparseable -> (None, False).
    """
    if val is None:
        return None, True
    if isinstance(val, (int, float)):
        return float(val), True
    if isinstance(val, str):
        s = val.strip()
        if not s:
            return None, True
        try:
            return float(s), True
        except ValueError:
            pass
        from datetime import datetime, timezone
        try:
            dt = datetime.fromisoformat(s)
            if dt.tzinfo is None:
                dt = dt.replace(tzinfo=timezone.utc)
            return dt.timestamp(), True
        except (ValueError, TypeError):
            pass
        for fmt in (
            "%Y-%m-%d %H:%M:%S",
            "%Y-%m-%dT%H:%M:%S",
            "%Y-%m-%d %H:%M:%S.%f",
            "%Y-%m-%dT%H:%M:%S.%f",
        ):
            try:
                dt = datetime.strptime(s[:26], fmt).replace(tzinfo=timezone.utc)
                return dt.timestamp(), True
            except (ValueError, TypeError):
                continue
        return None, False
    return None, False

try:
    import paho.mqtt.client as paho_mqtt
    PAHO_AVAILABLE = True
except ImportError:
    paho_mqtt = None
    PAHO_AVAILABLE = False

# Hardcoded emergency container whitelist for local autonomous recovery when Go Core is offline
EMERGENCY_RESTART_WHITELIST = frozenset({
    "home-api", "home-web", "home-vision", "home-frigate",
    "1Panel-openresty-WuDU", "1panel-openresty-wudu",
    "api", "web", "vision", "frigate", "openresty"
})

# Forbidden components that must NEVER be restarted offline without Go Core coordination
OFFLINE_FORBIDDEN_COMPONENTS = frozenset({
    "app.db", "mosquitto", "home-mosquitto", "database", "sqlite", "db", "mqtt"
})

from config import OpsAgentConfig
from proposal import ProposalLifecycleManager
from rule_engine import TwoTierRuleEngine, sanitize_log

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] [home-ops-agent] %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S"
)
logger = logging.getLogger("home-ops-agent")


class SensitiveInfoFilter(logging.Filter):
    """Logging filter ensuring passwords, tokens, and keys are scrubbed before output."""

    def filter(self, record: logging.LogRecord) -> bool:
        if isinstance(record.msg, str):
            record.msg = sanitize_log(record.msg)
        if record.args:
            if isinstance(record.args, dict):
                record.args = {k: sanitize_log(str(v)) if isinstance(v, str) else v for k, v in record.args.items()}
            elif isinstance(record.args, tuple):
                record.args = tuple(sanitize_log(str(a)) if isinstance(a, str) else a for a in record.args)
        return True


sensitive_filter = SensitiveInfoFilter()
logging.getLogger().addFilter(sensitive_filter)
logger.addFilter(sensitive_filter)

WEAK_SECRETS_BLACKLIST = {
    "", "123456", "12345678", "123456789", "@momo324", "password", "admin",
    "btpanel", "feiyemomo", "root", "qwerty", "pass123", "default", "your-secret-key",
    "change-me", "please_change_to_a_long_random_secret", "homedatacenter", "token", "secret"
}


def is_production() -> bool:
    """Returns True if running in production mode (explicit env or Docker container without dev override)."""
    env = os.environ.get("ENVIRONMENT", os.environ.get("APP_ENV", os.environ.get("ENV", ""))).lower().strip()
    if env in ("development", "dev", "test", "testing"):
        return False
    if env in ("production", "prod"):
        return True
    return os.path.exists("/.dockerenv")


def validate_boot_guard(config: Optional[OpsAgentConfig] = None) -> None:
    """
    Enforces security boot guard in production mode.
    Fails fast if critical secrets or tokens are missing, placeholders, or weak.
    Permits basic Tier-1 Rule Engine execution when optional LLM token is absent.
    """
    if not is_production():
        return

    cfg = config or OpsAgentConfig.load()
    errors = []

    # 1. NEW_API_TOKEN check (if present, must be strong; if absent, log warning and degrade to Tier-1)
    if cfg.new_api_url and "localhost" not in cfg.new_api_url and "127.0.0.1" not in cfg.new_api_url:
        token = cfg.new_api_token.strip()
        if token:
            if token.lower() in WEAK_SECRETS_BLACKLIST or len(token) < 8:
                errors.append(f"NEW_API_TOKEN is weak or placeholder ({token[:4]}...)")
        else:
            logger.warning(
                "NEW_API_TOKEN is not configured in production: "
                "Tier-2 LLM diagnostic reasoning disabled, auto-degrading to Tier-1 Rule Engine autonomous recovery."
            )

    # 2. ONEPANEL_PASSWORD check (if present in environment or configured)
    onepanel_pass = os.environ.get("ONEPANEL_PASSWORD", "").strip()
    if onepanel_pass:
        if onepanel_pass.lower() in WEAK_SECRETS_BLACKLIST or len(onepanel_pass) < 8:
            errors.append("ONEPANEL_PASSWORD is weak or placeholder")

    # 3. JWT_SECRET check (if present in environment or configured)
    jwt_secret = os.environ.get("JWT_SECRET", "").strip()
    if jwt_secret:
        if jwt_secret.lower() in WEAK_SECRETS_BLACKLIST or len(jwt_secret) < 32:
            errors.append("JWT_SECRET is weak or less than 32 characters")

    # 4. MQTT_PASSWORD check (if remote broker and password required)
    mqtt_pass = os.environ.get("MQTT_PASSWORD", "").strip()
    if mqtt_pass and (mqtt_pass.lower() in WEAK_SECRETS_BLACKLIST or len(mqtt_pass) < 6):
        errors.append("MQTT_PASSWORD is weak or placeholder")

    if errors:
        msg = f"FAIL-FAST BOOT GUARD: {'; '.join(errors)}. Refusing to boot in production."
        logger.critical(msg)
        raise RuntimeError(msg)


class HomeOpsAgent:
    """
    Main asynchronous daemon orchestrating Docker socket events, MQTT security alerts,
    disk self-healing, and low-peak human-in-the-loop proposal executions.
    """

    def __init__(self, config: Optional[OpsAgentConfig] = None):
        self.config = config or OpsAgentConfig.load()
        validate_boot_guard(self.config)
        self.rule_engine = TwoTierRuleEngine(self.config)
        self.proposal_manager = ProposalLifecycleManager(
            db_path=self.config.db_path,
            go_api_url=self.config.api_base_url,
            internal_token=self.config.internal_token
        )
        self.running = False
        self._tasks: list[asyncio.Task] = []
        self._mqtt_client: Optional[Any] = None
        self._mqtt_queue: asyncio.Queue = asyncio.Queue()
        self._docker_event_queue: asyncio.Queue = asyncio.Queue(maxsize=500)
        self._last_docker_event_time: int = int(time.time())
        self._mqtt_connected: bool = False
        self._local_restarts: dict[str, list[float]] = {}
        self.metrics = SecurityMetricsCollector()
        self._last_audit_status: AuditStatus = AuditStatus.AUDIT_UNKNOWN
        self._active_lease_generations: dict[str, int] = {}

    def _create_paho_client(self, loop: asyncio.AbstractEventLoop) -> Optional[Any]:
        """Creates and configures standard paho-mqtt client with auth, TLS, LWT, and QoS 1."""
        if not PAHO_AVAILABLE or paho_mqtt is None:
            logger.warning("paho-mqtt library is not installed; MQTT listener will run in fallback mode")
            return None

        client_id = self.config.mqtt_client_id or f"home-ops-agent-{os.getpid()}"
        if hasattr(paho_mqtt, "CallbackAPIVersion"):
            client = paho_mqtt.Client(paho_mqtt.CallbackAPIVersion.VERSION2, client_id=client_id)
        else:
            client = paho_mqtt.Client(client_id=client_id)

        # 1. Credentials Authentication
        if self.config.mqtt_user or self.config.mqtt_password:
            client.username_pw_set(self.config.mqtt_user, self.config.mqtt_password)

        # 2. TLS Encryption Support
        if self.config.mqtt_tls:
            import ssl
            client.tls_set(cert_reqs=ssl.CERT_REQUIRED, tls_version=ssl.PROTOCOL_TLS_CLIENT)

        # 3. Last Will and Testament (LWT)
        lwt_payload = json.dumps({
            "status": "offline",
            "agent": self.config.name,
            "reason": "unexpected_disconnect",
            "timestamp": time.time()
        })
        client.will_set(self.config.mqtt_lwt_topic, payload=lwt_payload, qos=self.config.mqtt_qos, retain=True)

        # 4. Lifecycle Callbacks
        def on_connect(cl, userdata, flags, rc, properties=None):
            code = rc.value if hasattr(rc, "value") else rc
            if code == 0:
                self._mqtt_connected = True
                logger.info(f"Connected to Mosquitto MQTT broker at {self.config.mqtt_broker}:{self.config.mqtt_port}")
                cl.subscribe(self.config.mqtt_topic, qos=self.config.mqtt_qos)
                logger.info(f"Subscribed to topic '{self.config.mqtt_topic}' with QoS {self.config.mqtt_qos}")
                online_payload = json.dumps({
                    "status": "online",
                    "agent": self.config.name,
                    "timestamp": time.time()
                })
                cl.publish(self.config.mqtt_lwt_topic, payload=online_payload, qos=self.config.mqtt_qos, retain=True)
            else:
                self._mqtt_connected = False
                logger.error(f"Mosquitto connection rejected with return code {code}")

        def on_disconnect(cl, userdata, rc, properties=None):
            self._mqtt_connected = False
            code = rc.value if hasattr(rc, "value") else rc
            if code != 0:
                logger.warning(f"Unexpected MQTT disconnect (rc={code}). Automatic reconnect scheduled.")

        def on_message(cl, userdata, msg):
            try:
                topic = msg.topic
                payload_str = msg.payload.decode("utf-8", errors="ignore")
                if loop and loop.is_running():
                    loop.call_soon_threadsafe(self._mqtt_queue.put_nowait, (topic, payload_str))
            except Exception as e:
                logger.debug(f"Error buffering incoming MQTT message: {e}")

        client.on_connect = on_connect
        client.on_disconnect = on_disconnect
        client.on_message = on_message

        # Exponential backoff reconnection configuration
        client.reconnect_delay_set(min_delay=1, max_delay=60)
        return client

    async def start(self) -> None:
        """Starts all daemon background event loops."""
        self.running = True
        logger.info(f"Starting {self.config.name} (Resident RAM constraint: <= {self.config.max_memory_mb}MB)")

        # Register asyncio tasks
        self._tasks = [
            asyncio.create_task(self._docker_event_listener(), name="docker_events"),
            asyncio.create_task(self._docker_event_processor(), name="docker_processor"),
            asyncio.create_task(self._mqtt_listener(), name="mqtt_security"),
            asyncio.create_task(self._disk_monitor_loop(), name="disk_monitor"),
            asyncio.create_task(self._memory_watchdog_loop(), name="memory_watchdog"),
            asyncio.create_task(self._sync_offline_audit_logs_loop(), name="sync_offline_audit"),
        ]

        try:
            await asyncio.gather(*self._tasks)
        except asyncio.CancelledError:
            logger.info("Daemon tasks cancelled, initiating graceful shutdown")

    async def stop(self) -> None:
        """Stops the daemon cleanly, publishing clean offline LWT message."""
        self.running = False
        if self._mqtt_client:
            try:
                offline_payload = json.dumps({
                    "status": "offline",
                    "agent": self.config.name,
                    "reason": "clean_shutdown",
                    "timestamp": time.time()
                })
                self._mqtt_client.publish(self.config.mqtt_lwt_topic, payload=offline_payload, qos=1, retain=True)
                self._mqtt_client.loop_stop()
                self._mqtt_client.disconnect()
            except Exception as e:
                logger.debug(f"Error shutting down MQTT client: {e}")

        for task in self._tasks:
            task.cancel()
        logger.info(f"{self.config.name} stopped successfully")

    async def _docker_event_listener(self) -> None:
        """
        Connects to /var/run/docker.sock and streams container lifecycle events.
        Decoupled: only ingests and enqueues events without blocking I/O or inline sleep.
        Correctly decodes HTTP Chunked transfer encoding and line-delimited JSON.
        """
        sock_path = self.config.docker_socket
        while self.running:
            if not os.path.exists(sock_path):
                logger.debug(f"Docker socket {sock_path} not found. Retrying in 10s...")
                await asyncio.sleep(10)
                continue

            try:
                reader, writer = await asyncio.open_unix_connection(sock_path)
                since_time = self._last_docker_event_time
                req = (
                    f"GET /events?filters=%7B%22type%22%3A%5B%22container%22%5D%7D&since={since_time} HTTP/1.1\r\n"
                    f"Host: docker\r\n"
                    f"User-Agent: home-ops-agent\r\n"
                    f"Connection: close\r\n\r\n"
                ).encode("utf-8")
                writer.write(req)
                await writer.drain()

                logger.info(f"Connected to Docker events stream via {sock_path} (since={since_time})")

                # 1. Consume HTTP response headers until CRLF CRLF
                while self.running:
                    header_line = await reader.readline()
                    if not header_line or header_line in (b"\r\n", b"\n"):
                        break

                # 2. Ingest stream of events (HTTP Chunked or line-delimited JSON)
                while self.running:
                    line = await reader.readline()
                    if not line:
                        break
                    raw_line = line.strip()
                    if not raw_line:
                        continue

                    chunk_len = -1
                    try:
                        chunk_header = raw_line.split(b';')[0].strip()
                        chunk_len = int(chunk_header, 16)
                    except ValueError:
                        chunk_len = -1

                    event_bytes = None
                    if chunk_len > 0:
                        event_bytes = await reader.readexactly(chunk_len)
                        # Consume trailing CRLF
                        await reader.readline()
                    elif chunk_len == 0:
                        # End of chunks
                        break
                    else:
                        event_bytes = raw_line

                    if event_bytes:
                        try:
                            event = json.loads(event_bytes.decode("utf-8", errors="ignore"))
                            if isinstance(event, dict):
                                self._last_docker_event_time = int(time.time())
                                try:
                                    self._docker_event_queue.put_nowait(event)
                                except asyncio.QueueFull:
                                    logger.warning("Docker event queue full (500), dropping oldest event")
                                    try:
                                        self._docker_event_queue.get_nowait()
                                    except Exception:
                                        pass
                                    self._docker_event_queue.put_nowait(event)
                        except json.JSONDecodeError:
                            continue
            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.debug(f"Docker socket connection notice: {e}. Reconnecting in 5s...")
                await asyncio.sleep(5)

    async def _docker_event_processor(self) -> None:
        """
        Consumes events from queue, executes local two-tier rule filtering,
        and coordinates authoritative self-healing through Go Core API.
        """
        sock_path = self.config.docker_socket
        while self.running:
            try:
                event = await self._docker_event_queue.get()
                action = event.get("Action", "")
                status = event.get("status", "")
                actor = event.get("Actor", {})
                attributes = actor.get("Attributes", {})
                container_name = attributes.get("name", actor.get("ID", "")[:12])

                if action in ("die", "kill", "oom") or status in ("die", "kill"):
                    exit_code = int(attributes.get("exitCode", 1))
                    if exit_code != 0:
                        logger.warning(f"Detected container crash: {container_name} (exit {exit_code})")
                        stderr_snippet = f"Container {container_name} exited with status {exit_code}"
                        result = self.rule_engine.handle_container_crash(container_name, exit_code, stderr_snippet)

                        if result.get("action") == "restart_container":
                            backoff = result.get("backoff_sec", 1)
                            await asyncio.sleep(backoff)

                            # 1. First attempt authoritative recovery via Go Core FSM
                            status, resp_data = await self._report_event_to_go_core(
                                container_name, exit_code, action, stderr_snippet
                            )

                            # 2. Strict status-based decision
                            if status == CoreReportingStatus.RECOVERED_BY_CORE:
                                logger.info(
                                    f"[Closed-Loop Verified] Go Core successfully executed physical recovery and verified health for {container_name}. "
                                    f"Status: RECOVERED. Skipping duplicate local restart."
                                )
                                continue

                            if status == CoreReportingStatus.AUTHORIZED:
                                canonical_auth = resolve_canonical_component(container_name)
                                if not canonical_auth:
                                    logger.critical(
                                        f"[Security Block] Refusing Go Core AUTHORIZED restart for non-whitelisted container '{container_name}'."
                                    )
                                    continue
                                db_path_auth = getattr(self.config, "db_path", "/data/sqlite/app.db")
                                lease_ok, lease_tok, fencing_gen = self._acquire_recovery_lease_grant(canonical_auth, db_path_auth)
                                if not lease_ok:
                                    logger.critical(
                                        f"[Lease Block] Failed to acquire recovery lease for AUTHORIZED container {canonical_auth}. Halting restart."
                                    )
                                    continue
                                try:
                                    if not self._verify_recovery_lease(canonical_auth, lease_tok, fencing_gen, db_path_auth):
                                        self.metrics.inc("recovery_fencing_rejected")
                                        logger.critical(f"[Fencing Block] Lease invalid before Docker restart for {canonical_auth}.")
                                        continue
                                    logger.info(
                                        f"[Authoritative] Go Core approved restart for {canonical_auth} (gen={fencing_gen}). Executing guarded restart."
                                    )
                                    docker_name = resolve_docker_container_name(canonical_auth)
                                    if not docker_name:
                                        logger.critical(f"[Security Block] Cannot resolve docker container for {canonical_auth}.")
                                        continue
                                    restarted = await self._restart_docker_container(docker_name, sock_path)
                                    if restarted:
                                        h_state = await self._verify_container_health(docker_name, sock_path)
                                        if h_state != DockerHealthState.HEALTHY:
                                            logger.error(f"[Post-Restart Failed] Health check returned {h_state} for {canonical_auth}.")
                                finally:
                                    self._release_recovery_lease(canonical_auth, lease_tok, db_path_auth, fencing_gen=fencing_gen)
                                continue

                            if status in (CoreReportingStatus.POLICY_REJECTED, CoreReportingStatus.BLOCKED_BY_FSM):
                                logger.warning(
                                    f"[Execution Prohibited] Go Core rejected or blocked recovery for {container_name}. "
                                    f"Status: {status.value}. Local Docker execution strictly cancelled."
                                )
                                continue

                            if status == CoreReportingStatus.SERVER_ERROR:
                                logger.error(f"[Execution Halted] Go Core returned internal error. Suppressing autonomous restart.")
                                continue

                            # 3. Only if confirmed OFFLINE, trigger controlled 5-guard offline emergency restart
                            if status == CoreReportingStatus.OFFLINE:
                                logger.warning(
                                    f"[Offline Fallback] Go Core confirmed offline. Evaluating 5-guard offline emergency restart for {container_name}..."
                                )
                                await self._handle_offline_emergency_restart(container_name, exit_code, sock_path)

                        elif result.get("action") == "request_diagnostic":
                            prop_id = f"prop-crash-{container_name}-{int(time.time())}"
                            await self._submit_proposal_to_go_core(
                                prop_id=prop_id,
                                title=f"【高危告警】{container_name} 连续崩溃自愈推演方案",
                                risk_level="HIGH",
                                action=f"docker restart {container_name}",
                                rollback_cmd=f"docker logs {container_name}",
                                target_component=container_name,
                                estimated_seconds=30
                            )
            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.debug(f"Docker event processing error: {e}")

    async def _handle_offline_emergency_restart(self, container_name: str, exit_code: int, sock_path: str) -> bool:
        """
        Controlled 5-guard offline emergency recovery mode with fail-closed semantics.
        All safety checks must be explicitly confirmed safe. Any UNKNOWN or failure halts restart.
        """
        # Guard 1: Validate EMERGENCY_OVERRIDE_TOKEN (must be >= 16 chars, fail-secure if missing or weak)
        override_token = os.environ.get("EMERGENCY_OVERRIDE_TOKEN", "").strip()
        if not override_token or len(override_token) < 16 or override_token.lower() in WEAK_SECRETS_BLACKLIST:
            logger.critical(
                f"[Emergency Autonomy Blocked] Refusing offline restart for {container_name}: "
                f"Missing or weak EMERGENCY_OVERRIDE_TOKEN."
            )
            return False

        canonical = resolve_canonical_component(container_name)
        if not canonical:
            logger.critical(
                f"[Emergency Autonomy Blocked] Unknown or non-whitelisted component '{container_name}' (R4-17). Refusing offline restart."
            )
            return False

        # Guard 2a: Forbidden component check (core storage / database forbidden from offline autonomous restart)
        if container_name in OFFLINE_FORBIDDEN_COMPONENTS or canonical in OFFLINE_FORBIDDEN_COMPONENTS:
            logger.critical(
                f"[Emergency Autonomy Blocked] Container {container_name} ({canonical}) is in OFFLINE_FORBIDDEN_COMPONENTS. Refusing offline restart."
            )
            return False

        # Guard 2b: Restrict to container whitelist
        if container_name not in EMERGENCY_RESTART_WHITELIST and canonical not in EMERGENCY_RESTART_WHITELIST:
            logger.warning(
                f"[Emergency Autonomy Blocked] Container {container_name} ({canonical}) is NOT in EMERGENCY_RESTART_WHITELIST. Refusing restart."
            )
            return False

        db_path = getattr(self.config, "db_path", "/data/sqlite/app.db")

        # Guard 2b-pre: Pre-flight schema & storage integrity check (R4-14, R4-18)
        schema_ok, fault_cls, schema_reason = self._verify_ops_schema(db_path)
        if not schema_ok:
            self.metrics.inc("recovery_database_error")
            self.metrics.set_gauge("circuit_breaker_degraded", 1)
            self.metrics.set_gauge("maintenance_lock_degraded", 1)
            logger.critical(
                f"[Emergency Autonomy Blocked] Storage schema verification failed [{fault_cls}]: {schema_reason}. Fail-Closed."
            )
            return False

        # Guard 2c: Maintenance Lock Verification (Fail-Closed)
        lock_state = self._is_maintenance_locked_offline(db_path, container_name)
        if lock_state == MaintenanceLockState.LOCKED:
            logger.critical(
                f"[Emergency Autonomy Blocked] Refusing offline restart for {container_name}: "
                f"System maintenance lock is ACTIVE in local DB."
            )
            return False
        elif lock_state == MaintenanceLockState.UNKNOWN:
            logger.critical(
                f"[Emergency Autonomy Blocked] Refusing offline restart for {container_name}: "
                f"Maintenance lock state is UNKNOWN (DB missing, unreadable, or schema invalid). Fail-Closed."
            )
            return False
        elif lock_state != MaintenanceLockState.UNLOCKED:
            logger.critical(
                f"[Emergency Autonomy Blocked] Refusing offline restart for {container_name}: "
                f"Unexpected maintenance lock state {lock_state}. Fail-Closed."
            )
            return False

        # Guard 2d: Circuit Breaker State Verification (Fail-Closed)
        cb_state = self._is_circuit_open_offline(container_name, db_path)
        if cb_state == CircuitBreakerState.OPEN:
            logger.critical(
                f"[Emergency Autonomy Blocked] Refusing offline restart for {container_name}: "
                f"Container circuit breaker is OPEN in local DB."
            )
            return False
        elif cb_state == CircuitBreakerState.HALF_OPEN:
            logger.critical(
                f"[Emergency Autonomy Blocked] Refusing offline restart for {container_name}: "
                f"Container circuit breaker is HALF_OPEN. Offline emergency agent forbidden to probe HALF_OPEN."
            )
            return False
        elif cb_state == CircuitBreakerState.UNKNOWN:
            logger.critical(
                f"[Emergency Autonomy Blocked] Refusing offline restart for {container_name}: "
                f"Container circuit breaker state is UNKNOWN (DB error or schema invalid). Fail-Closed."
            )
            return False
        elif cb_state != CircuitBreakerState.CLOSED:
            logger.critical(
                f"[Emergency Autonomy Blocked] Refusing offline restart for {container_name}: "
                f"Unexpected circuit breaker state {cb_state}. Fail-Closed."
            )
            return False

        # Guard 2e: Dual-Path Execution Lease Mutual Exclusion with Monotonic Fencing Generation (R4-04 ~ R4-07)
        lease_acquired, lease_token, fencing_gen = self._acquire_recovery_lease_grant(canonical, db_path)
        if not lease_acquired:
            self.metrics.inc("recovery_fencing_rejected")
            logger.critical(
                f"[Emergency Autonomy Blocked] Unable to acquire recovery lease for {canonical}. "
                f"Lease held by another path or DB error. Halting offline restart."
            )
            return False

        try:
            # Guard 4: Persistent cooldown and circuit breaker (<= 2 restarts in 60s, <= 3 crashes in 10m)
            now = time.time()
            timestamps = self._local_restarts.setdefault(container_name, [])
            timestamps = [t for t in timestamps if now - t < 600.0]  # 10 minute window
            self._local_restarts[container_name] = timestamps

            short_window = [t for t in timestamps if now - t < 60.0]  # 60 second window
            persisted_short = self._get_persisted_restart_count(container_name, 60.0, db_path)
            persisted_long = self._get_persisted_restart_count(container_name, 600.0, db_path)

            if persisted_short is None or persisted_long is None:
                logger.critical(
                    f"[Emergency Autonomy Blocked] Persisted restart count is UNKNOWN (query error or missing table). Halting restart."
                )
                return False

            if len(short_window) >= 2 or persisted_short >= 2:
                logger.error(
                    f"[Emergency Autonomy Breaker] Tripped for {container_name}: {max(len(short_window), persisted_short)} restarts in 60s. Halted."
                )
                return False

            if len(timestamps) >= 3 or persisted_long >= 3:
                logger.critical(
                    f"[Emergency Autonomy Breaker] 10-minute circuit tripped for {container_name}: {max(len(timestamps), persisted_long)} crashes. Halted."
                )
                return False

            # Guard 5: Local SQLite persistence in offline_restart_history
            rec_ok = self._record_persisted_restart(container_name, db_path)
            if not rec_ok:
                logger.critical(
                    f"[Emergency Autonomy Blocked] Failed to write persisted restart record for {container_name}. Halting restart."
                )
                return False

            # R4-06: Pre-mutation lease & fencing gate right before Docker socket dispatch
            if not self._verify_recovery_lease(canonical, lease_token, fencing_gen, db_path):
                self.metrics.inc("recovery_fencing_rejected")
                logger.critical(
                    f"[Fencing Gate] Lease token or fencing generation ({fencing_gen}) became invalid right before Docker restart for {canonical}. Aborting."
                )
                return False

            timestamps.append(now)
            op_id = f"pyop-{int(now * 1000)}-{uuid.uuid4().hex[:8]}"
            logger.warning(
                f"[Emergency Autonomy Triggered] Go Core OFFLINE. Executing offline emergency restart for {container_name} "
                f"(attempt {len(timestamps)} in 10m window, gen={fencing_gen}, op={op_id})"
            )
            docker_container = resolve_docker_container_name(canonical)
            if not docker_container:
                logger.critical(f"[Emergency Autonomy Blocked] Cannot map canonical '{canonical}' to Docker container.")
                return False
            success = await self._restart_docker_container(docker_container, sock_path)
            if success:
                health_state = await self._verify_container_health(docker_container, sock_path)
                if health_state == DockerHealthState.UNKNOWN:
                    self.metrics.inc("recovery_health_unknown")
                if health_state != DockerHealthState.HEALTHY:
                    logger.error(f"[Emergency Recovery Failed] Post-restart health check returned {health_state} for {container_name}.")
                    success = False

            audit_status, _ = self._record_offline_audit_strict(
                container_name,
                "emergency_restart",
                exit_code,
                success,
                operation_id=op_id,
                fencing_generation=fencing_gen,
            )
            if audit_status != AuditStatus.AUDIT_COMMITTED:
                logger.critical(
                    f"[Audit Integrity Failure] Offline audit persistence returned {audit_status} for op={op_id}. Cannot report recovery success."
                )
                return False
            return success
        finally:
            released = self._release_recovery_lease(canonical, lease_token, db_path, fencing_gen=fencing_gen)
            if not released:
                self.metrics.inc("recovery_database_error")
                logger.critical(f"[Lease Release Failed] Failed to cleanly release recovery lease for {canonical} (gen={fencing_gen}).")

    def _verify_ops_schema(self, db_path: str) -> Tuple[bool, str, str]:
        """
        Pre-flight SQLite storage integrity, version, and security table/column verification (R4-14, R4-18).
        Returns (ok, fault_class, reason). Fails closed on missing/corrupt DB, missing migrations, or dropped tables.
        """
        if not db_path or not os.path.isfile(db_path):
            return False, SQLiteFaultClass.DATABASE_MISSING.value, f"SQLite file missing: {db_path}"
        conn = None
        try:
            conn = open_readonly_sqlite(db_path)
            cur = conn.cursor()
            cur.execute("PRAGMA quick_check(1)")
            qc = cur.fetchone()
            if not qc or str(qc[0]).lower() != "ok":
                return False, SQLiteFaultClass.DATABASE_CORRUPT.value, f"PRAGMA quick_check failed: {qc}"

            cur.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='ops_schema_migrations'")
            if not cur.fetchone():
                return False, SQLiteFaultClass.TABLE_MISSING.value, "table ops_schema_migrations missing"

            cur.execute("SELECT COALESCE(MAX(version), 0) FROM ops_schema_migrations")
            ver_row = cur.fetchone()
            schema_ver = int(ver_row[0]) if ver_row and ver_row[0] is not None else 0
            if schema_ver < REQUIRED_OPS_SCHEMA_VERSION:
                return False, SQLiteFaultClass.SCHEMA_VERSION_MISMATCH.value, (
                    f"schema version {schema_ver} < required {REQUIRED_OPS_SCHEMA_VERSION}"
                )

            cur.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='ops_schema_meta'")
            if not cur.fetchone():
                return False, SQLiteFaultClass.TABLE_MISSING.value, "table ops_schema_meta missing"
            cur.execute("SELECT value FROM ops_schema_meta WHERE key='bootstrapped'")
            meta_row = cur.fetchone()
            if not meta_row or str(meta_row[0]) != "1":
                return False, SQLiteFaultClass.SCHEMA_VERSION_MISMATCH.value, "ops_schema_meta bootstrapped sentinel missing"

            required_cols = {
                "ops_maintenance_locks": {"active", "expires_at"},
                "ops_circuit_breaker_states": {"container", "state"},
                "ops_recovery_leases": {
                    "component", "holder", "lease_token", "fencing_generation",
                    "acquired_at_ms", "expires_at_ms", "state",
                },
                "ops_recovery_lease_generations": {"component", "last_generation"},
                "offline_restart_history": {"container_name", "restart_time"},
                "offline_ops_audit": {"event_uuid", "container_name", "action", "success", "executed_at", "synced"},
            }
            for tbl, req in required_cols.items():
                cur.execute("SELECT name FROM sqlite_master WHERE type='table' AND name=?", (tbl,))
                if not cur.fetchone():
                    return False, SQLiteFaultClass.TABLE_MISSING.value, f"required table '{tbl}' missing"
                cur.execute(f"PRAGMA table_info({tbl})")
                cols = {str(col[1]).lower() for col in cur.fetchall()}
                missing = req - cols
                if missing:
                    return False, SQLiteFaultClass.COLUMN_MISSING.value, f"table '{tbl}' missing columns {sorted(missing)}"

            return True, "", "ok"
        except Exception as e:
            return False, classify_sqlite_error(e).value, str(e)
        finally:
            if conn:
                try:
                    conn.close()
                except Exception:
                    pass

    def _is_maintenance_locked_offline(self, db_path: str, container_name: Optional[str] = None) -> MaintenanceLockState:
        """
        Checks if system or component maintenance lock is active in local SQLite.
        Returns MaintenanceLockState: LOCKED, UNLOCKED, or UNKNOWN.
        Fails closed: missing DB, corrupted DB, missing table, incomplete columns, invalid component -> UNKNOWN.
        """
        if not db_path or not os.path.isfile(db_path):
            logger.error(f"[Fail-Closed] SQLite database file missing at {db_path}. Maintenance lock state UNKNOWN.")
            return MaintenanceLockState.UNKNOWN
        canonical = ""
        if container_name:
            canonical = resolve_canonical_component(container_name)
            if not canonical:
                logger.error(f"[Fail-Closed] Unknown component '{container_name}' in maintenance lock check. State UNKNOWN.")
                return MaintenanceLockState.UNKNOWN
        conn = None
        try:
            conn = open_readonly_sqlite(db_path)
            cursor = conn.cursor()
            cursor.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='ops_maintenance_locks'")
            if not cursor.fetchone():
                logger.error(f"[Fail-Closed] Table ops_maintenance_locks missing in {db_path}. State UNKNOWN.")
                return MaintenanceLockState.UNKNOWN
            cursor.execute("PRAGMA table_info(ops_maintenance_locks)")
            cols = {col[1] for col in cursor.fetchall()}
            active_col = "active" if "active" in cols else ("locked" if "locked" in cols else "")
            if not active_col or "expires_at" not in cols:
                logger.error(f"[Fail-Closed] Table ops_maintenance_locks missing required columns. State UNKNOWN.")
                return MaintenanceLockState.UNKNOWN

            has_degraded = "degraded" in cols
            has_exp_ms = "expires_at_ms" in cols
            extra_sel = ""
            if has_degraded:
                extra_sel += ", degraded"
            if has_exp_ms:
                extra_sel += ", expires_at_ms"

            has_component = "component" in cols or "target_component" in cols
            comp_col = "component" if "component" in cols else ("target_component" if "target_component" in cols else "")

            if has_component and container_name:
                cursor.execute(
                    f"SELECT {active_col}, expires_at{extra_sel} FROM ops_maintenance_locks "
                    f"WHERE {comp_col} IS NULL OR {comp_col} = '' OR {comp_col} = '*' "
                    f"OR {comp_col} = ? OR {comp_col} = ? ORDER BY id DESC LIMIT 1",
                    (canonical, container_name)
                )
            else:
                cursor.execute(f"SELECT {active_col}, expires_at{extra_sel} FROM ops_maintenance_locks ORDER BY id DESC LIMIT 1")

            row = cursor.fetchone()
            if not row:
                return MaintenanceLockState.UNLOCKED

            idx = 2
            if has_degraded:
                if bool(row[idx]):
                    self.metrics.set_gauge("maintenance_lock_degraded", 1)
                    logger.critical("[Fail-Closed] ops_maintenance_locks row flagged degraded=true. State UNKNOWN.")
                    return MaintenanceLockState.UNKNOWN
                idx += 1

            exp_ms_val = row[idx] if has_exp_ms else None

            is_active = bool(row[0])
            if not is_active:
                return MaintenanceLockState.UNLOCKED

            if exp_ms_val is not None and isinstance(exp_ms_val, (int, float)) and exp_ms_val > 0:
                if float(exp_ms_val) > time.time() * 1000.0:
                    return MaintenanceLockState.LOCKED
                return MaintenanceLockState.UNLOCKED

            expires_at_raw = row[1]
            exp_time, is_valid = parse_sqlite_timestamp(expires_at_raw)
            if not is_valid:
                logger.critical(f"[Fail-Closed] Malformed expires_at timestamp: '{expires_at_raw}'. State UNKNOWN.")
                return MaintenanceLockState.UNKNOWN

            if exp_time is None:
                return MaintenanceLockState.LOCKED

            if exp_time > time.time():
                return MaintenanceLockState.LOCKED

            return MaintenanceLockState.UNLOCKED
        except Exception as e:
            self.metrics.inc("recovery_database_error")
            logger.error(f"[Fail-Closed] Error querying offline maintenance lock: {e}. State UNKNOWN.")
            return MaintenanceLockState.UNKNOWN
        finally:
            if conn:
                try:
                    conn.close()
                except Exception:
                    pass

    def _is_circuit_open_offline(self, container_name: str, db_path: str) -> CircuitBreakerState:
        """
        Checks if container circuit breaker is OPEN, HALF_OPEN, or CLOSED in local SQLite.
        Returns CircuitBreakerState: OPEN, HALF_OPEN, CLOSED, or UNKNOWN.
        Fails closed: missing DB, corrupted DB, missing table, incomplete columns, degraded flag -> UNKNOWN.
        """
        if not db_path or not os.path.isfile(db_path):
            logger.error(f"[Fail-Closed] SQLite database file missing at {db_path}. Circuit breaker state UNKNOWN.")
            return CircuitBreakerState.UNKNOWN
        canonical = resolve_canonical_component(container_name)
        if not canonical:
            logger.error(f"[Fail-Closed] Unknown component '{container_name}' in circuit breaker check. State UNKNOWN.")
            return CircuitBreakerState.UNKNOWN
        conn = None
        try:
            conn = open_readonly_sqlite(db_path)
            cursor = conn.cursor()
            cursor.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='ops_circuit_breaker_states'")
            if not cursor.fetchone():
                logger.error(f"[Fail-Closed] Table ops_circuit_breaker_states missing in {db_path}. State UNKNOWN.")
                return CircuitBreakerState.UNKNOWN
            cursor.execute("PRAGMA table_info(ops_circuit_breaker_states)")
            cols = {col[1] for col in cursor.fetchall()}
            container_col = "container" if "container" in cols else ("container_name" if "container_name" in cols else "")
            if not container_col or "state" not in cols:
                logger.error(f"[Fail-Closed] Table ops_circuit_breaker_states schema incomplete. State UNKNOWN.")
                return CircuitBreakerState.UNKNOWN

            has_degraded = "degraded" in cols
            has_ts = "crash_timestamps" in cols
            sel_cols = "state"
            if has_degraded:
                sel_cols += ", degraded"
            if has_ts:
                sel_cols += ", crash_timestamps"

            cursor.execute(
                f"SELECT {sel_cols} FROM ops_circuit_breaker_states WHERE {container_col} = ? OR {container_col} = ? LIMIT 1",
                (canonical, container_name)
            )
            row = cursor.fetchone()
            if not row:
                return CircuitBreakerState.CLOSED

            idx = 1
            if has_degraded:
                if bool(row[idx]):
                    self.metrics.set_gauge("circuit_breaker_degraded", 1)
                    logger.critical(f"[Fail-Closed] Circuit breaker for '{canonical}' marked degraded=true. State UNKNOWN.")
                    return CircuitBreakerState.UNKNOWN
                idx += 1
            if has_ts:
                ts_raw = row[idx]
                if ts_raw is not None and str(ts_raw).strip() != "":
                    try:
                        parsed_ts = json.loads(str(ts_raw))
                        if not isinstance(parsed_ts, list):
                            return CircuitBreakerState.UNKNOWN
                    except Exception:
                        logger.critical(f"[Fail-Closed] Malformed crash_timestamps JSON for '{canonical}'. State UNKNOWN.")
                        return CircuitBreakerState.UNKNOWN

            state_str = str(row[0]).strip().upper()
            if state_str == "OPEN":
                return CircuitBreakerState.OPEN
            elif state_str == "HALF_OPEN":
                return CircuitBreakerState.HALF_OPEN
            elif state_str == "CLOSED":
                return CircuitBreakerState.CLOSED
            else:
                logger.error(f"[Fail-Closed] Unrecognized circuit breaker state '{state_str}'. State UNKNOWN.")
                return CircuitBreakerState.UNKNOWN
        except Exception as e:
            self.metrics.inc("recovery_database_error")
            logger.error(f"[Fail-Closed] Error querying offline circuit breaker: {e}. State UNKNOWN.")
            return CircuitBreakerState.UNKNOWN
        finally:
            if conn:
                try:
                    conn.close()
                except Exception:
                    pass

    def _get_persisted_restart_count(self, container_name: str, window_seconds: float, db_path: str) -> Optional[int]:
        """
        Returns number of restarts recorded in SQLite within window_seconds.
        Returns None if DB is missing, corrupted, table missing, or component invalid (Fail-Closed: NEVER 0 on missing table) (R4-14).
        """
        if not db_path or not os.path.isfile(db_path):
            logger.error(f"[Fail-Closed] DB file missing at {db_path}. Persisted restart count UNKNOWN.")
            return None
        canonical = resolve_canonical_component(container_name)
        if not canonical:
            logger.error(f"[Fail-Closed] Unknown component '{container_name}'. Persisted restart count UNKNOWN.")
            return None
        conn = None
        try:
            conn = open_readonly_sqlite(db_path)
            cursor = conn.cursor()
            cursor.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='offline_restart_history'")
            if not cursor.fetchone():
                logger.critical(f"[Fail-Closed] Table offline_restart_history missing in {db_path}. Persisted restart count UNKNOWN.")
                return None
            # Reject corrupt/non-numeric restart_time values in history table
            cursor.execute(
                "SELECT restart_time FROM offline_restart_history WHERE container_name = ? OR container_name = ?",
                (canonical, container_name)
            )
            cutoff = time.time() - window_seconds
            count = 0
            for (rt_val,) in cursor.fetchall():
                if not isinstance(rt_val, (int, float)):
                    logger.critical(f"[Fail-Closed] Corrupt restart_time value '{rt_val}' in offline_restart_history.")
                    return None
                if float(rt_val) >= cutoff:
                    count += 1
            return count
        except Exception as e:
            self.metrics.inc("recovery_database_error")
            logger.error(f"[Fail-Closed] Failed to query offline_restart_history: {e}")
            return None
        finally:
            if conn:
                try:
                    conn.close()
                except Exception:
                    pass

    def _record_persisted_restart(self, container_name: str, db_path: str) -> bool:
        """
        Records restart timestamp into EXISTING SQLite offline_restart_history table using BEGIN IMMEDIATE.
        NEVER auto-creates database files or tables at runtime (R4-14).
        """
        if not db_path or not os.path.isfile(db_path):
            logger.error(f"[Fail-Closed] Cannot record restart: DB file {db_path} does not exist.")
            return False
        canonical = resolve_canonical_component(container_name)
        if not canonical:
            logger.error(f"[Fail-Closed] Cannot record restart for unknown component '{container_name}'.")
            return False
        conn = None
        try:
            conn = open_readwrite_sqlite(db_path)
            cur = conn.cursor()
            cur.execute("BEGIN IMMEDIATE")
            cur.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='offline_restart_history'")
            if not cur.fetchone():
                cur.execute("ROLLBACK")
                logger.critical(f"[Fail-Closed] Table offline_restart_history missing in {db_path}. Refusing runtime DDL.")
                return False
            cur.execute("PRAGMA table_info(offline_restart_history)")
            cols = {str(c[1]).lower() for c in cur.fetchall()}
            now_sec = time.time()
            now_ms = int(now_sec * 1000)
            if "restart_time_ms" in cols:
                cur.execute(
                    "INSERT INTO offline_restart_history (container_name, restart_time, restart_time_ms) VALUES (?, ?, ?)",
                    (canonical, now_sec, now_ms)
                )
            else:
                cur.execute(
                    "INSERT INTO offline_restart_history (container_name, restart_time) VALUES (?, ?)",
                    (canonical, now_sec)
                )
            cur.execute("COMMIT")
            return True
        except Exception as e:
            if conn:
                try:
                    conn.execute("ROLLBACK")
                except Exception:
                    pass
            self.metrics.inc("recovery_database_error")
            logger.error(f"[Fail-Closed] Failed to record offline restart timestamp: {e}")
            return False
        finally:
            if conn:
                try:
                    conn.close()
                except Exception:
                    pass

    def _acquire_recovery_lease_grant(
        self, component: str, db_path: str, lease_duration: float = 30.0
    ) -> Tuple[bool, str, int]:
        """
        Atomically acquires an exclusive persistent recovery lease in SQLite using BEGIN IMMEDIATE,
        integer UTC milliseconds (acquired_at_ms, expires_at_ms), and monotonic fencing_generation.
        Strictly forbids same-holder re-entry while active (R4-04 ~ R4-07).
        Returns (acquired: bool, lease_token: str, fencing_generation: int).
        """
        if not db_path or not os.path.isfile(db_path) or lease_duration <= 0:
            return False, "", 0
        canonical = resolve_canonical_component(component)
        if not canonical:
            logger.error(f"[Fail-Closed] Cannot acquire lease for unknown component '{component}'.")
            return False, "", 0
        token = f"py-{uuid.uuid4()}"
        now_epoch = time.time()
        now_ms = int(now_epoch * 1000)
        exp_ms = now_ms + int(lease_duration * 1000)
        if now_ms <= 0 or exp_ms <= now_ms:
            return False, "", 0
        now_iso = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now_epoch))
        exp_iso = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now_epoch + lease_duration))

        conn = None
        try:
            conn = open_readwrite_sqlite(db_path)
            cur = conn.cursor()
            cur.execute("BEGIN IMMEDIATE")

            cur.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='ops_recovery_leases'")
            if not cur.fetchone():
                cur.execute("ROLLBACK")
                logger.critical("[Fail-Closed] Table ops_recovery_leases missing. Refusing runtime DDL.")
                return False, "", 0
            cur.execute("PRAGMA table_info(ops_recovery_leases)")
            cols = {str(c[1]).lower() for c in cur.fetchall()}
            required_lease_cols = {
                "component", "holder", "lease_token", "fencing_generation",
                "acquired_at_ms", "expires_at_ms", "state",
            }
            if not required_lease_cols.issubset(cols):
                cur.execute("ROLLBACK")
                logger.critical("[Fail-Closed] Table ops_recovery_leases missing v4 columns. Fail-Closed.")
                return False, "", 0

            cur.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='ops_recovery_lease_generations'")
            if not cur.fetchone():
                cur.execute("ROLLBACK")
                logger.critical("[Fail-Closed] Table ops_recovery_lease_generations missing. Fail-Closed.")
                return False, "", 0

            cur.execute(
                "SELECT holder, lease_token, fencing_generation, acquired_at_ms, expires_at_ms, state, expires_at "
                "FROM ops_recovery_leases WHERE component = ?",
                (canonical,)
            )
            row = cur.fetchone()
            exist_gen = 0
            if row:
                holder, _tok, exist_gen_raw, acq_ms_raw, exp_ms_raw, state_raw, exp_raw = row
                try:
                    exist_gen = int(exist_gen_raw or 0)
                    acq_ms = int(acq_ms_raw or 0)
                    exp_ms_val = int(exp_ms_raw or 0)
                except (TypeError, ValueError):
                    cur.execute("ROLLBACK")
                    self.metrics.inc("recovery_database_error")
                    logger.critical(f"[Fail-Closed] Non-integer lease fields for {canonical}.")
                    return False, "", 0

                state_str = str(state_raw or "ACTIVE").strip().upper()
                if state_str not in ("ACTIVE", "RELEASED", "EXPIRED"):
                    cur.execute("ROLLBACK")
                    self.metrics.inc("recovery_database_error")
                    logger.critical(f"[Fail-Closed] Invalid lease state '{state_str}' for {canonical}.")
                    return False, "", 0

                if exp_ms_val > 0:
                    if acq_ms <= 0 or exp_ms_val <= acq_ms:
                        cur.execute("ROLLBACK")
                        self.metrics.inc("recovery_database_error")
                        logger.critical(f"[Fail-Closed] Malformed lease ms timestamps acq={acq_ms} exp={exp_ms_val}.")
                        return False, "", 0
                    eff_exp_ms = exp_ms_val
                else:
                    exp_ts, ts_ok = parse_sqlite_timestamp(exp_raw)
                    if not ts_ok or exp_ts is None:
                        cur.execute("ROLLBACK")
                        self.metrics.inc("recovery_database_error")
                        logger.critical(f"[Fail-Closed] Missing/malformed expires_at '{exp_raw}' for {canonical}.")
                        return False, "", 0
                    eff_exp_ms = int(exp_ts * 1000)

                if state_str == "ACTIVE" and eff_exp_ms > now_ms:
                    cur.execute("ROLLBACK")
                    logger.warning(
                        f"[Lease Conflict] Recovery lease for {canonical} actively held by '{holder}' "
                        f"(gen={exist_gen}, exp_ms={eff_exp_ms} > now_ms={now_ms})."
                    )
                    return False, "", 0
                if state_str == "ACTIVE" and eff_exp_ms <= now_ms:
                    self.metrics.inc("recovery_lease_expired")

            cur.execute(
                "SELECT last_generation FROM ops_recovery_lease_generations WHERE component = ?",
                (canonical,)
            )
            gen_row = cur.fetchone()
            persisted_gen = int(gen_row[0]) if gen_row and gen_row[0] is not None else 0
            next_gen = max(exist_gen, persisted_gen) + 1

            cur.execute(
                """
                INSERT INTO ops_recovery_lease_generations (component, last_generation) VALUES (?, ?)
                ON CONFLICT(component) DO UPDATE SET last_generation = excluded.last_generation
                WHERE excluded.last_generation > ops_recovery_lease_generations.last_generation
                """,
                (canonical, next_gen),
            )

            if row:
                cur.execute(
                    """
                    UPDATE ops_recovery_leases
                    SET holder = 'python-offline', lease_token = ?, fencing_generation = ?,
                        acquired_at = ?, expires_at = ?, acquired_at_ms = ?, expires_at_ms = ?, state = 'ACTIVE'
                    WHERE component = ?
                    """,
                    (token, next_gen, now_iso, exp_iso, now_ms, exp_ms, canonical),
                )
            else:
                cur.execute(
                    """
                    INSERT INTO ops_recovery_leases (
                        component, holder, lease_token, fencing_generation,
                        acquired_at, expires_at, acquired_at_ms, expires_at_ms, state
                    ) VALUES (?, 'python-offline', ?, ?, ?, ?, ?, ?, 'ACTIVE')
                    """,
                    (canonical, token, next_gen, now_iso, exp_iso, now_ms, exp_ms),
                )

            cur.execute("COMMIT")
            self.metrics.inc("recovery_lease_active")
            self._active_lease_generations[token] = next_gen
            return True, token, next_gen
        except Exception as e:
            if conn:
                try:
                    conn.execute("ROLLBACK")
                except Exception:
                    pass
            self.metrics.inc("recovery_database_error")
            logger.error(f"[Fail-Closed] Failed to acquire recovery lease for {canonical}: {e}")
            return False, "", 0
        finally:
            if conn:
                try:
                    conn.close()
                except Exception:
                    pass

    def _acquire_recovery_lease(self, component: str, db_path: str, lease_duration: float = 30.0) -> Tuple[bool, str]:
        """Backward-compatible wrapper returning (acquired, lease_token)."""
        ok, token, _gen = self._acquire_recovery_lease_grant(component, db_path, lease_duration=lease_duration)
        return ok, token

    def _renew_recovery_lease(
        self, component: str, lease_token: str, fencing_gen: int, db_path: str, lease_duration: float = 30.0
    ) -> Tuple[bool, int]:
        """Atomically extends active lease ONLY for matching holder token, generation, and unexpired state."""
        if not db_path or not lease_token or fencing_gen <= 0 or not os.path.isfile(db_path):
            self.metrics.inc("recovery_lease_renewal_failed")
            return False, 0
        canonical = resolve_canonical_component(component)
        if not canonical or lease_duration <= 0:
            self.metrics.inc("recovery_lease_renewal_failed")
            return False, 0
        now_epoch = time.time()
        now_ms = int(now_epoch * 1000)
        new_exp_ms = now_ms + int(lease_duration * 1000)
        new_exp_iso = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now_epoch + lease_duration))
        conn = None
        try:
            conn = open_readwrite_sqlite(db_path)
            cur = conn.cursor()
            cur.execute("BEGIN IMMEDIATE")
            cur.execute(
                """
                UPDATE ops_recovery_leases
                SET expires_at = ?, expires_at_ms = ?
                WHERE component = ? AND lease_token = ? AND fencing_generation = ?
                  AND state = 'ACTIVE' AND expires_at_ms > ?
                """,
                (new_exp_iso, new_exp_ms, canonical, lease_token, fencing_gen, now_ms),
            )
            if cur.rowcount != 1:
                cur.execute("ROLLBACK")
                self.metrics.inc("recovery_lease_renewal_failed")
                return False, 0
            cur.execute("COMMIT")
            return True, new_exp_ms
        except Exception as e:
            if conn:
                try:
                    conn.execute("ROLLBACK")
                except Exception:
                    pass
            self.metrics.inc("recovery_lease_renewal_failed")
            logger.error(f"[Lease Renew Failed] {canonical}: {e}")
            return False, 0
        finally:
            if conn:
                try:
                    conn.close()
                except Exception:
                    pass

    def _verify_recovery_lease(self, component: str, lease_token: str, fencing_gen: int, db_path: str) -> bool:
        """Verifies right before external Docker mutation that this token & fencing generation still own the lease (R4-06)."""
        if not db_path or not lease_token or fencing_gen <= 0 or not os.path.isfile(db_path):
            return False
        canonical = resolve_canonical_component(component)
        if not canonical:
            return False
        now_ms = int(time.time() * 1000)
        conn = None
        try:
            conn = open_readonly_sqlite(db_path)
            cur = conn.cursor()
            cur.execute(
                "SELECT lease_token, fencing_generation, expires_at_ms, state FROM ops_recovery_leases WHERE component = ?",
                (canonical,),
            )
            row = cur.fetchone()
            if not row:
                return False
            db_tok, db_gen, db_exp_ms, db_state = row
            if str(db_tok) != lease_token or int(db_gen or 0) != fencing_gen:
                return False
            if str(db_state or "").upper() != "ACTIVE" or int(db_exp_ms or 0) <= now_ms:
                return False
            return True
        except Exception as e:
            self.metrics.inc("recovery_database_error")
            logger.error(f"[Fencing Verify Error] {canonical}: {e}")
            return False
        finally:
            if conn:
                try:
                    conn.close()
                except Exception:
                    pass

    def _release_recovery_lease(self, component: str, lease_token: str, db_path: str, fencing_gen: int = 0) -> bool:
        """Releases recovery lease held by this agent (verifying token and generation). Returns True only if released."""
        if not db_path or not lease_token or not os.path.isfile(db_path):
            return False
        canonical = resolve_canonical_component(component)
        if not canonical:
            return False
        conn = None
        try:
            conn = open_readwrite_sqlite(db_path)
            cur = conn.cursor()
            cur.execute("BEGIN IMMEDIATE")
            if fencing_gen > 0:
                cur.execute(
                    "DELETE FROM ops_recovery_leases WHERE component = ? AND lease_token = ? AND fencing_generation = ?",
                    (canonical, lease_token, fencing_gen)
                )
            else:
                cur.execute(
                    "DELETE FROM ops_recovery_leases WHERE component = ? AND lease_token = ?",
                    (canonical, lease_token)
                )
            released = cur.rowcount > 0
            cur.execute("COMMIT")
            if released:
                self._active_lease_generations.pop(lease_token, None)
                cur_active = self.metrics.counters.get("recovery_lease_active", 0)
                if cur_active > 0:
                    self.metrics.set_gauge("recovery_lease_active", cur_active - 1)
            return released
        except Exception as e:
            if conn:
                try:
                    conn.execute("ROLLBACK")
                except Exception:
                    pass
            self.metrics.inc("recovery_database_error")
            logger.error(f"Failed to release recovery lease for {canonical}: {e}")
            return False
        finally:
            if conn:
                try:
                    conn.close()
                except Exception:
                    pass

    async def _connect_docker(self, sock_path: str) -> Tuple[asyncio.StreamReader, asyncio.StreamWriter]:
        """Connects to Docker socket via Unix domain socket or TCP endpoint."""
        if not sock_path:
            raise FileNotFoundError("Docker socket path is empty")
        if sock_path.startswith("tcp://"):
            host_port = sock_path[6:]
            host, port = host_port.split(":")
            return await asyncio.open_connection(host, int(port))
        elif ":" in sock_path and not os.path.exists(sock_path):
            parts = sock_path.split(":")
            return await asyncio.open_connection(parts[0], int(parts[1]))
        else:
            if not os.path.exists(sock_path):
                raise FileNotFoundError(f"Docker socket not found: {sock_path}")
            if hasattr(asyncio, "open_unix_connection"):
                return await asyncio.open_unix_connection(sock_path)
            if hasattr(socket, "AF_UNIX"):
                s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                s.connect(sock_path)
                return await asyncio.open_connection(sock=s)
            raise OSError(f"Unix sockets unsupported on {sys.platform}")

    async def _query_docker_inspect(self, container_name: str, sock_path: str) -> Tuple[DockerHealthState, Optional[Dict[str, Any]]]:
        """Queries Docker /containers/{container_name}/json and returns (health_state, inspect_data)."""
        try:
            reader, writer = await self._connect_docker(sock_path)
        except FileNotFoundError:
            logger.error(f"[Fail-Closed] Docker socket not found: {sock_path}")
            return DockerHealthState.UNKNOWN, None
        except Exception as e:
            logger.error(f"[Fail-Closed] Docker socket connection error: {e}")
            return DockerHealthState.UNKNOWN, None

        try:
            req = (
                f"GET /containers/{container_name}/json HTTP/1.1\r\n"
                f"Host: docker\r\n"
                f"User-Agent: home-ops-agent\r\n"
                f"Connection: close\r\n\r\n"
            ).encode("utf-8")
            writer.write(req)
            await writer.drain()

            raw_chunks = []
            while True:
                chunk = await asyncio.wait_for(reader.read(4096), timeout=5.0)
                if not chunk:
                    break
                raw_chunks.append(chunk)
            writer.close()
            await writer.wait_closed()
            resp = b"".join(raw_chunks)
        except Exception as e:
            logger.error(f"[Fail-Closed] Error reading Docker inspect response: {e}")
            return DockerHealthState.UNKNOWN, None

        if not resp:
            return DockerHealthState.UNKNOWN, None

        parts = resp.split(b"\r\n\r\n", 1)
        header_block = parts[0]
        body = parts[1] if len(parts) > 1 else b""

        status_line = header_block.split(b"\r\n", 1)[0].decode("utf-8", errors="ignore")
        if not (status_line.startswith("HTTP/1.1 200") or status_line.startswith("HTTP/1.0 200")):
            logger.error(f"[Fail-Closed] Docker inspect returned non-200 status: {status_line}")
            if "404" in status_line:
                return DockerHealthState.UNHEALTHY, None
            return DockerHealthState.UNKNOWN, None

        if not body:
            logger.error("[Fail-Closed] Docker inspect returned empty body.")
            return DockerHealthState.UNKNOWN, None

        try:
            data = json.loads(body.decode("utf-8", errors="ignore"))
            return DockerHealthState.HEALTHY, data
        except Exception as e:
            logger.error(f"[Fail-Closed] Docker inspect JSON decode error: {e}")
            return DockerHealthState.UNKNOWN, None

    async def _check_service_readiness(self, canonical: str, container_name: str = "") -> bool:
        """
        Executes 5-tier protocol-level readiness probes for services without Docker HEALTHCHECK (R4-16):
        - mosquitto: real MQTT v3.1.1 CONNECT + CONNACK (0x20 .. 0x00)
        - home-api / home-vision: HTTP 200 GET /health with valid JSON payload
        - frigate: HTTP < 400 GET /api/config or /
        - home-web / openresty: HTTP < 400 GET / with non-empty body (never bare TCP connect)
        """
        if hasattr(self, "_readiness_probe_override") and callable(self._readiness_probe_override):
            return await self._readiness_probe_override(canonical)

        canon = resolve_canonical_component(canonical)
        if not canon:
            logger.warning(f"[Fail-Closed] Readiness probe rejected unknown component '{canonical}'.")
            return False

        probe_host = os.environ.get("PROBE_HOST", "127.0.0.1")

        async def probe_mqtt(host: str, port: int, timeout: float = 2.5) -> bool:
            writer = None
            try:
                reader, writer = await asyncio.wait_for(asyncio.open_connection(host, port), timeout=timeout)
                client_id = b"ops-probe"
                var_header = bytearray([0x00, 0x04, ord("M"), ord("Q"), ord("T"), ord("T"), 0x04, 0x02, 0x00, 0x05])
                payload = bytearray([0x00, len(client_id)]) + client_id
                rem_len = len(var_header) + len(payload)
                pkt = bytearray([0x10, rem_len]) + var_header + payload
                writer.write(pkt)
                await asyncio.wait_for(writer.drain(), timeout=timeout)
                connack = await asyncio.wait_for(reader.readexactly(4), timeout=timeout)
                return len(connack) == 4 and connack[0] == 0x20 and connack[3] == 0x00
            except Exception:
                return False
            finally:
                if writer is not None:
                    try:
                        writer.close()
                        await writer.wait_closed()
                    except Exception:
                        pass

        async def probe_http(host: str, port: int, path: str = "/", require_json: bool = False, timeout: float = 2.5) -> bool:
            writer = None
            try:
                reader, writer = await asyncio.wait_for(asyncio.open_connection(host, port), timeout=timeout)
                req = (
                    f"GET {path} HTTP/1.1\r\n"
                    f"Host: {host}:{port}\r\n"
                    f"User-Agent: home-ops-agent-probe\r\n"
                    f"Connection: close\r\n\r\n"
                ).encode("utf-8")
                writer.write(req)
                await asyncio.wait_for(writer.drain(), timeout=timeout)
                raw = await asyncio.wait_for(reader.read(8192), timeout=timeout)
                if not raw:
                    return False
                head, _, body = raw.partition(b"\r\n\r\n")
                status_line = head.split(b"\r\n", 1)[0].decode("ascii", errors="ignore")
                parts = status_line.split()
                if len(parts) < 2 or not parts[1].isdigit():
                    return False
                status_code = int(parts[1])
                if not (200 <= status_code < 400):
                    return False
                if require_json:
                    try:
                        data = json.loads(body.decode("utf-8", errors="ignore"))
                        if not isinstance(data, dict):
                            return False
                        st = str(data.get("status", "ok")).lower().strip()
                        return st in ("ok", "healthy", "up", "ready")
                    except Exception:
                        return False
                return len(body.strip()) > 0 or status_code == 204
            except Exception:
                return False
            finally:
                if writer is not None:
                    try:
                        writer.close()
                        await writer.wait_closed()
                    except Exception:
                        pass

        if canon == "mosquitto":
            return await probe_mqtt(probe_host, 1883)
        elif canon == "frigate":
            ok = await probe_http(probe_host, 5000, "/api/config", require_json=False)
            if not ok:
                ok = await probe_http(probe_host, 5000, "/", require_json=False)
            if not ok:
                ok = await probe_http(probe_host, 1984, "/", require_json=False)
            return ok
        elif canon == "home-api":
            return await probe_http(probe_host, 8080, "/health", require_json=True)
        elif canon == "home-web":
            ok = await probe_http(probe_host, 80, "/", require_json=False)
            if not ok:
                ok = await probe_http(probe_host, 5173, "/", require_json=False)
            return ok
        elif canon == "home-vision":
            return await probe_http(probe_host, 8090, "/health", require_json=True)
        elif canon == "openresty":
            return await probe_http(probe_host, 80, "/", require_json=False)
        else:
            logger.warning(f"No independent readiness probe defined for component '{canon}'.")
            return False

    async def _verify_container_health(self, container_name: str, sock_path: str) -> DockerHealthState:
        """
        Inspects container state via Docker socket to verify it is running and healthy.
        Distinguishes HEALTHY, UNHEALTHY, RUNNING_UNHEALTHCHECKED, and UNKNOWN.
        For containers without HEALTHCHECK, executes independent service readiness probes.
        """
        if not sock_path or (not sock_path.startswith("tcp://") and ":" not in sock_path and not os.path.exists(sock_path)):
            logger.error(f"[Fail-Closed] Docker socket missing at {sock_path}. Health state UNKNOWN.")
            return DockerHealthState.UNKNOWN

        inspect_status, data = await self._query_docker_inspect(container_name, sock_path)
        if inspect_status != DockerHealthState.HEALTHY or not data:
            return inspect_status

        state = data.get("State", {})
        if not state.get("Running", False):
            logger.warning(f"Container {container_name} State.Running is False. State UNHEALTHY.")
            return DockerHealthState.UNHEALTHY

        health_obj = state.get("Health")
        if health_obj is not None and isinstance(health_obj, dict):
            status = str(health_obj.get("Status", "")).lower().strip()
            if status == "healthy":
                return DockerHealthState.HEALTHY
            elif status == "unhealthy":
                logger.warning(f"Container {container_name} Health.Status is unhealthy.")
                return DockerHealthState.UNHEALTHY
            elif status == "starting":
                logger.warning(f"Container {container_name} Health.Status is starting. Not yet healthy.")
                return DockerHealthState.UNHEALTHY
            else:
                logger.error(f"Container {container_name} Health.Status unrecognized: {status}.")
                return DockerHealthState.UNKNOWN

        # No HEALTHCHECK configured: classify as RUNNING_UNHEALTHCHECKED and run independent readiness probe
        logger.info(f"Container {container_name} lacks Docker HEALTHCHECK (RUNNING_UNHEALTHCHECKED). Running readiness probe...")
        canonical = resolve_canonical_component(container_name)
        probe_ok = await self._check_service_readiness(canonical, container_name)
        if probe_ok:
            logger.info(f"Independent readiness probe for {canonical} passed. State HEALTHY.")
            return DockerHealthState.HEALTHY
        else:
            logger.warning(f"Independent readiness probe for {canonical} failed. State UNHEALTHY.")
            return DockerHealthState.UNHEALTHY

    def _record_offline_audit_strict(
        self,
        container_name: str,
        action: str,
        exit_code: int,
        success: bool,
        operation_id: str = "",
        fencing_generation: int = 0,
    ) -> Tuple[AuditStatus, str]:
        """
        Persists offline emergency event into EXISTING SQLite offline_ops_audit table using BEGIN IMMEDIATE.
        NEVER calls os.makedirs or CREATE TABLE IF NOT EXISTS at runtime (R4-15).
        Returns (AuditStatus, event_uuid).
        """
        event_uuid = f"offline-{uuid.uuid4()}"
        now_sec = time.time()
        now_ms = int(now_sec * 1000)
        now_iso = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now_sec))
        op_id = operation_id or event_uuid
        db_path = getattr(self.config, "db_path", "/data/sqlite/app.db")
        if not db_path or not os.path.isfile(db_path):
            self.metrics.inc("recovery_audit_failed")
            self._last_audit_status = AuditStatus.AUDIT_FAILED
            logger.critical(f"[Offline Audit Failed] Database missing at {db_path}; refusing runtime creation.")
            return AuditStatus.AUDIT_FAILED, ""

        conn = None
        try:
            conn = open_readwrite_sqlite(db_path)
            cur = conn.cursor()
            cur.execute("BEGIN IMMEDIATE")
            cur.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='offline_ops_audit'")
            if not cur.fetchone():
                cur.execute("ROLLBACK")
                self.metrics.inc("recovery_audit_failed")
                self._last_audit_status = AuditStatus.AUDIT_FAILED
                logger.critical("[Offline Audit Failed] Table offline_ops_audit missing; refusing runtime DDL.")
                return AuditStatus.AUDIT_FAILED, ""

            cur.execute("PRAGMA table_info(offline_ops_audit)")
            cols = {str(c[1]).lower() for c in cur.fetchall()}

            if {"operation_id", "holder", "fencing_generation", "audit_status", "executed_at_ms"}.issubset(cols):
                cur.execute(
                    """
                    INSERT INTO offline_ops_audit (
                        event_uuid, operation_id, container_name, action, holder, fencing_generation,
                        exit_code, success, audit_status, executed_at, executed_at_ms, synced
                    ) VALUES (?, ?, ?, ?, 'python-offline', ?, ?, ?, ?, ?, ?, 0)
                    """,
                    (
                        event_uuid,
                        op_id,
                        container_name,
                        action,
                        fencing_generation,
                        exit_code,
                        1 if success else 0,
                        AuditStatus.AUDIT_COMMITTED.value,
                        now_iso,
                        now_ms,
                    ),
                )
            else:
                cur.execute(
                    """
                    INSERT INTO offline_ops_audit (event_uuid, container_name, action, exit_code, success, executed_at, synced)
                    VALUES (?, ?, ?, ?, ?, ?, 0)
                    """,
                    (event_uuid, container_name, action, exit_code, 1 if success else 0, now_iso),
                )
            cur.execute("COMMIT")
            self._last_audit_status = AuditStatus.AUDIT_COMMITTED
            logger.info(f"[Offline Audit] Recorded offline event {event_uuid} (op={op_id}, gen={fencing_generation}) for container {container_name}")
            return AuditStatus.AUDIT_COMMITTED, event_uuid
        except Exception as e:
            if conn:
                try:
                    conn.execute("ROLLBACK")
                except Exception:
                    pass
            self.metrics.inc("recovery_audit_failed")
            self._last_audit_status = AuditStatus.AUDIT_FAILED
            logger.critical(f"[Offline Audit Failed] Failed to persist offline audit log: {e}")
            return AuditStatus.AUDIT_FAILED, ""
        finally:
            if conn:
                try:
                    conn.close()
                except Exception:
                    pass

    def _record_offline_audit(self, container_name: str, action: str, exit_code: int, success: bool) -> str:
        """Backward-compatible wrapper returning event_uuid string (or '' on failure)."""
        _st, event_uuid = self._record_offline_audit_strict(container_name, action, exit_code, success)
        return event_uuid

    async def sync_offline_audit_logs(self) -> int:
        """Probes Go Core /health, and if alive, pushes buffered offline audit logs to Go Core."""
        db_path = getattr(self.config, "db_path", "/data/sqlite/app.db")
        if not os.path.exists(db_path):
            return 0

        loop = asyncio.get_running_loop()

        # 1. Probe Go Core
        probe_url = f"{self.config.api_base_url.rstrip('/')}/health"
        def check_alive() -> bool:
            try:
                req = urllib.request.Request(probe_url, headers={"User-Agent": "home-ops-agent"})
                with urllib.request.urlopen(req, timeout=2.0) as resp:
                    return resp.status == 200
            except Exception:
                return False

        is_alive = await loop.run_in_executor(None, check_alive)
        if not is_alive:
            return 0

        # 2. Query unsynced events from SQLite (never auto-create DB)
        def read_unsynced():
            if not db_path or not os.path.isfile(db_path):
                return []
            conn = None
            try:
                conn = open_readonly_sqlite(db_path)
                conn.row_factory = sqlite3.Row
                cursor = conn.cursor()
                cursor.execute("SELECT * FROM offline_ops_audit WHERE synced = 0 ORDER BY id ASC LIMIT 50")
                rows = cursor.fetchall()
                return [dict(r) for r in rows]
            except sqlite3.OperationalError:
                return []
            finally:
                if conn:
                    conn.close()

        records = await loop.run_in_executor(None, read_unsynced)
        if not records:
            return 0

        # 3. Push to Go Core
        sync_url = f"{self.config.api_base_url.rstrip('/')}/api/v1/ops/audit/offline-sync"
        headers = {
            "Content-Type": "application/json"
        }
        if getattr(self.config, "internal_token", None):
            headers["X-Internal-Secret"] = self.config.internal_token
        sync_payload = json.dumps({"events": records}).encode("utf-8")

        def do_sync() -> bool:
            try:
                sync_req = urllib.request.Request(sync_url, data=sync_payload, headers=headers)
                with urllib.request.urlopen(sync_req, timeout=5.0) as resp:
                    return resp.status in (200, 201)
            except Exception as err:
                logger.debug(f"[Offline Audit Sync] Request failed: {err}")
                return False

        sync_ok = await loop.run_in_executor(None, do_sync)
        if not sync_ok:
            return 0

        # 4. Mark synced = 1 via read-write BEGIN IMMEDIATE transaction
        def mark_synced():
            if not db_path or not os.path.isfile(db_path):
                return
            conn = None
            try:
                conn = open_readwrite_sqlite(db_path)
                synced_ids = [r["id"] for r in records]
                placeholders = ",".join("?" * len(synced_ids))
                conn.execute("BEGIN IMMEDIATE")
                conn.execute(f"UPDATE offline_ops_audit SET synced = 1 WHERE id IN ({placeholders})", synced_ids)
                conn.execute("COMMIT")
            except Exception as err:
                if conn:
                    try:
                        conn.execute("ROLLBACK")
                    except Exception:
                        pass
                logger.error(f"[Offline Audit Sync] Failed to mark synced: {err}")
            finally:
                if conn:
                    conn.close()

        await loop.run_in_executor(None, mark_synced)
        logger.info(f"[Offline Audit Sync] Successfully synchronized {len(records)} offline events to Go Core.")
        return len(records)

    async def _sync_offline_audit_logs_loop(self) -> None:
        """Background task: periodically probes Go Core /health, and syncs buffered events to Go Core."""
        while self.running:
            try:
                await asyncio.sleep(30)
                if not self.running:
                    break
                await self.sync_offline_audit_logs()
            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.debug(f"[Offline Audit Sync Loop] Exception: {e}")

    async def _report_event_to_go_core(
        self, container_name: str, exit_code: int, action: str, error_msg: str
    ) -> Tuple[CoreReportingStatus, Dict[str, Any]]:
        """
        Reports crash event to Go Active Core API for authoritative policy verification and execution.
        Distinguishes network errors/timeouts from HTTP status codes and FSM policy blocks.
        """
        api_url = f"{self.config.api_base_url.rstrip('/')}/api/v1/ops/fsm/event"
        payload = {
            "component": container_name,
            "container_name": container_name,
            "action": action,
            "exit_code": exit_code,
            "oom_killed": (action == "oom" or exit_code == 137),
            "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "attributes": {"error": error_msg[:500]}
        }
        headers = {"Content-Type": "application/json"}
        if getattr(self.config, "internal_token", None):
            headers["X-Internal-Secret"] = self.config.internal_token

        loop = asyncio.get_running_loop()

        def _do_http_call() -> Tuple[CoreReportingStatus, Dict[str, Any]]:
            try:
                req_data = json.dumps(payload).encode("utf-8")
                req = urllib.request.Request(api_url, data=req_data, headers=headers)
                with urllib.request.urlopen(req, timeout=25.0) as resp:
                    resp_body = resp.read().decode("utf-8", errors="ignore")
                    resp_json = json.loads(resp_body) if resp_body else {}

                    exec_status = str(resp_json.get("status", "")).upper()
                    decision = resp_json.get("decision", {})
                    dec_action = str(decision.get("Action") or decision.get("action") or "").upper()
                    dec_reason = decision.get("Reason") or decision.get("reason") or resp_json.get("error") or ""

                    if exec_status == "RECOVERED":
                        return CoreReportingStatus.RECOVERED_BY_CORE, resp_json
                    elif exec_status in ("BLOCKED", "FAILED") or dec_action in ("BLOCKED", "IGNORE", "TRIPPED"):
                        logger.warning(
                            f"[Policy Guard] Go Core FSM evaluated {container_name}: "
                            f"Status={exec_status}, Action={dec_action}, Reason={dec_reason}. Execution suppressed."
                        )
                        return CoreReportingStatus.BLOCKED_BY_FSM, resp_json
                    elif dec_action == "RESTART":
                        if resp_json.get("executed"):
                            return CoreReportingStatus.RECOVERED_BY_CORE, resp_json
                        return CoreReportingStatus.AUTHORIZED, resp_json
                    else:
                        logger.warning(
                            f"[Fail-Closed Policy Guard] Unrecognized Go Core response status='{exec_status}' action='{dec_action}' for {container_name}; suppressing local restart."
                        )
                        return CoreReportingStatus.BLOCKED_BY_FSM, resp_json

            except urllib.error.HTTPError as http_err:
                status_code = http_err.code
                err_body = http_err.read().decode("utf-8", errors="ignore")

                # Explicit policy rejection and auth failure: strictly prohibit local fallback!
                if status_code in (400, 401, 403, 409, 422, 429):
                    logger.error(
                        f"[Security Alert] Go Core rejected event for {container_name} "
                        f"with HTTP {status_code}: {err_body}. FORBIDDEN to fallback locally."
                    )
                    return CoreReportingStatus.POLICY_REJECTED, {"code": status_code, "body": err_body}

                # Server errors (500, 502, 503, 504)
                logger.error(f"[Go Core Error] Server returned HTTP {status_code}: {err_body}")
                return CoreReportingStatus.SERVER_ERROR, {"code": status_code, "body": err_body}

            except (urllib.error.URLError, socket.timeout, TimeoutError, ConnectionRefusedError, OSError) as net_err:
                # Genuinely unreachable / offline
                logger.warning(f"[Network] Go Core unreachable at {api_url}: {net_err}")
                return CoreReportingStatus.OFFLINE, {"error": str(net_err)}

            except Exception as unk_err:
                logger.error(f"[Unexpected] Error contacting Go Core: {unk_err}")
                return CoreReportingStatus.SERVER_ERROR, {"error": str(unk_err)}

        return await loop.run_in_executor(None, _do_http_call)

    async def _submit_proposal_to_go_core(self, prop_id: str, title: str, risk_level: str, action: str, rollback_cmd: str, target_component: str, estimated_seconds: int) -> bool:
        """Submits high-risk diagnostic proposal to Go Core for human review."""
        api_url = f"{self.config.api_base_url.rstrip('/')}/api/v1/ops/proposals"
        payload = {
            "proposal_id": prop_id,
            "title": title,
            "risk_level": risk_level,
            "execution_plan": action,
            "rollback_plan": rollback_cmd,
            "target_component": target_component,
            "estimated_seconds": estimated_seconds
        }
        headers = {"Content-Type": "application/json"}
        if getattr(self.config, "internal_token", None):
            headers["X-Internal-Secret"] = self.config.internal_token
        try:
            req_data = json.dumps(payload).encode("utf-8")
            req = urllib.request.Request(api_url, data=req_data, headers=headers)
            loop = asyncio.get_running_loop()
            def do_req():
                with urllib.request.urlopen(req, timeout=3) as resp:
                    return resp.status in (200, 201)
            res = await loop.run_in_executor(None, do_req)
            if res:
                logger.info(f"Submitted proposal {prop_id} to Go Core successfully")
                return True
        except Exception as e:
            logger.debug(f"Submit proposal to Go Core failed ({api_url}): {e}")

        # Local tracking fallback
        self.proposal_manager.create_proposal(
            prop_id=prop_id,
            title=title,
            risk_level=risk_level,
            action=action,
            rollback_cmd=rollback_cmd,
            target_component=target_component,
            estimated_seconds=estimated_seconds
        )
        return False

    async def _restart_docker_container(self, container_name: str, sock_path: str) -> bool:
        """Sends POST /containers/{name}/restart to Docker socket."""
        try:
            reader, writer = await self._connect_docker(sock_path)
            req = (
                f"POST /containers/{container_name}/restart?t=5 HTTP/1.1\r\n"
                f"Host: docker\r\n"
                f"User-Agent: home-ops-agent\r\n"
                f"Content-Length: 0\r\n"
                f"Connection: close\r\n\r\n"
            ).encode("utf-8")
            writer.write(req)
            await writer.drain()
            resp = await reader.read(256)
            writer.close()
            await writer.wait_closed()
            logger.info(f"[Tier 1] Issued restart for container {container_name}")
            return b"204 No Content" in resp or b"200 OK" in resp
        except Exception as e:
            logger.error(f"Failed to restart container {container_name} via Docker socket: {e}")
            return False

    async def _mqtt_listener(self) -> None:
        """
        Subscribes to Mosquitto security topics via paho-mqtt client with QoS 1.
        Processes auth failures and brute-force events with Tier 1 auto-ban.
        """
        loop = asyncio.get_running_loop()
        self._mqtt_client = self._create_paho_client(loop)

        if self._mqtt_client:
            try:
                self._mqtt_client.connect_async(
                    self.config.mqtt_broker,
                    self.config.mqtt_port,
                    keepalive=60
                )
                self._mqtt_client.loop_start()
            except Exception as e:
                logger.warning(f"Initial async connect to MQTT broker failed: {e}. paho-mqtt will retry.")

            while self.running:
                try:
                    # Wait for messages dispatched from paho thread
                    topic, payload_str = await self._mqtt_queue.get()
                    if "auth_fail" in topic or "security" in topic:
                        try:
                            data = json.loads(payload_str)
                            ip = data.get("ip") or data.get("client_ip")
                            if ip:
                                res = self.rule_engine.handle_auth_failure(ip)
                                logger.info(f"MQTT security event from {ip}: action={res.get('action')}")
                        except Exception:
                            pass
                    self._mqtt_queue.task_done()
                except asyncio.CancelledError:
                    break
                except Exception as e:
                    logger.debug(f"MQTT worker loop error: {e}")
            return

        # Fallback raw socket listener when paho-mqtt is unavailable
        while self.running:
            try:
                reader, writer = await asyncio.open_connection(
                    self.config.mqtt_broker,
                    self.config.mqtt_port
                )
                logger.info(f"Connected to MQTT broker (fallback) at {self.config.mqtt_broker}:{self.config.mqtt_port}")

                # Minimal MQTT v3.1.1 CONNECT packet
                client_id = f"ops-agent-{os.getpid()}".encode("utf-8")
                connect_payload = bytearray([
                    0x10, len(client_id) + 12,
                    0x00, 0x04, ord('M'), ord('Q'), ord('T'), ord('T'),
                    0x04, 0x02, 0x00, 0x3C,
                    0x00, len(client_id)
                ]) + client_id
                writer.write(connect_payload)
                await writer.drain()

                connack = await reader.read(4)
                if len(connack) >= 4 and connack[3] == 0:
                    topic = b"security/#"
                    sub_pkt = bytearray([
                        0x82, len(topic) + 5,
                        0x00, 0x01,
                        0x00, len(topic)
                    ]) + topic + bytearray([0x00])
                    writer.write(sub_pkt)
                    await writer.drain()
                    logger.info("Subscribed to MQTT topic security/# (fallback)")

                while self.running:
                    header = await reader.read(2)
                    if not header or len(header) < 2:
                        break
                    pkt_type = header[0] >> 4
                    rem_len = header[1]
                    body = await reader.read(rem_len)

                    # PUBLISH packet handling
                    if pkt_type == 3 and len(body) >= 2:
                        topic_len = (body[0] << 8) | body[1]
                        msg_topic = body[2:2 + topic_len].decode("utf-8", errors="ignore")
                        payload_data = body[2 + topic_len:].decode("utf-8", errors="ignore")

                        if "auth_fail" in msg_topic or "security" in msg_topic:
                            try:
                                data = json.loads(payload_data)
                                ip = data.get("ip") or data.get("client_ip")
                                if ip:
                                    res = self.rule_engine.handle_auth_failure(ip)
                                    logger.info(f"MQTT security event from {ip}: action={res.get('action')}")
                            except Exception:
                                pass
            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.debug(f"MQTT connection notice: {e}. Retrying in 10s...")
                await asyncio.sleep(10)

    async def _disk_monitor_loop(self) -> None:
        """Periodically checks host disk watermark (every 300s)."""
        while self.running:
            try:
                usage = shutil.disk_usage("/")
                pct = (usage.used / usage.total) * 100.0
                self.rule_engine.handle_disk_usage(pct, mount_path="/")
            except Exception as e:
                logger.debug(f"Disk check error: {e}")
            await asyncio.sleep(300)

    async def _memory_watchdog_loop(self) -> None:
        """Monitors daemon's own resident memory usage (<= 50MB ceiling)."""
        while self.running:
            try:
                # Read RSS from /proc/self/status on Linux
                if os.path.exists("/proc/self/status"):
                    with open("/proc/self/status", "r") as f:
                        for line in f:
                            if line.startswith("VmRSS:"):
                                rss_kb = int(line.split()[1])
                                rss_mb = rss_kb / 1024.0
                                if rss_mb > self.config.max_memory_mb:
                                    logger.warning(f"Memory warning: RSS {rss_mb:.1f}MB exceeds ceiling {self.config.max_memory_mb}MB")
                                break
            except Exception:
                pass
            await asyncio.sleep(120)


async def main() -> None:
    try:
        validate_boot_guard()
    except RuntimeError as e:
        logger.critical(str(e))
        sys.exit(1)

    agent = HomeOpsAgent()

    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(sig, lambda: asyncio.create_task(agent.stop()))
        except (NotImplementedError, RuntimeError):
            # Windows signal handler fallback
            pass

    try:
        await agent.start()
    except KeyboardInterrupt:
        await agent.stop()


if __name__ == "__main__":
    asyncio.run(main())
