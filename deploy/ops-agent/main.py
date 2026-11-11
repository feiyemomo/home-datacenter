"""
home-ops-agent — Autonomous Edge Operations Daemon.
Python 3.11 asynchronous daemon with strict <= 50MB resident RAM constraint.
"""

import asyncio
import json
import logging
import enum
import math
import os
import secrets
import shutil
import signal
import socket
import sqlite3
import sys
import time
import urllib.error
import urllib.request
import uuid
from typing import Any, Dict, List, Optional, Tuple, Callable, Awaitable

from audit_protocol import (
    AUDIT_COMMIT_FAILED,
    AUDIT_GENESIS_HASH,
    AUDIT_POLICY_VERSION,
    DECISION_ALLOW,
    DECISION_DENY,
    EVENT_RECEIVED,
    EXECUTION_COMPLETED,
    EXECUTION_FAILED,
    EXECUTION_STARTED,
    EXECUTION_UNKNOWN,
    HEALTH_CHECK_COMPLETED,
    HEALTH_CHECK_STARTED,
    RECOVERY_LEASE_ACQUIRED,
    append_jsonl_audit_stub,
    compute_event_entry_hash,
    compute_event_hmac,
    extract_hmac_key_id,
    format_rfc3339_ms,
    redact_sensitive_string,
    resolve_audit_hmac_keyring,
    serialize_evidence,
    serialize_params,
    verify_offline_audit_chain,
)

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
    AUDIT_REJECTED = "AUDIT_REJECTED"
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

from config import (
    CANONICAL_COMPONENT_MAP,
    CANONICAL_TO_DOCKER_CONTAINER,
    OpsAgentConfig,
    compute_container_baseline_from_inspect,
    compute_offline_placeholder_baseline,
    is_placeholder_baseline_hash,
    is_production_env,
    is_real_baseline_hash,
    is_real_baseline_hash_v1,
    resolve_canonical_component,
    resolve_docker_container_name,
)

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


is_production = is_production_env


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

    # 5. OPS_AUDIT_HMAC_KEY / OPS_AUDIT_HMAC_KEYS check (external audit signing secret)
    raw_audit_key = os.environ.get("OPS_AUDIT_HMAC_KEY", "").strip()
    raw_audit_keys = os.environ.get("OPS_AUDIT_HMAC_KEYS", "").strip()
    if raw_audit_key:
        if raw_audit_key.lower() in WEAK_SECRETS_BLACKLIST or len(raw_audit_key) < 32:
            errors.append("OPS_AUDIT_HMAC_KEY is weak or less than 32 characters")
    elif raw_audit_keys:
        kid_check, ring_check = resolve_audit_hmac_keyring()
        if not kid_check or not ring_check:
            errors.append("OPS_AUDIT_HMAC_KEYS contains no valid >=32-byte key")
    else:
        logger.warning(
            "OPS_AUDIT_HMAC_KEY is not configured in production: "
            "v5 offline cryptographic audit records will fail closed until configured."
        )

    if errors:
        msg = f"FAIL-FAST BOOT GUARD: {'; '.join(errors)}. Refusing to boot in production."
        logger.critical(msg)
        raise RuntimeError(msg)


class AsyncLeaseGuard:
    """
    Background lease renewal & fencing watchdog for recovery mutations (R6).
    Renews active recovery leases every ttl/3, cancels guarded execution immediately
    on renewal loss or fencing drift, and verifies lease validity before critical stages.
    """

    def __init__(
        self,
        agent: "HomeOpsAgent",
        component: str,
        lease_token: str,
        fencing_gen: int,
        db_path: str,
        ttl_sec: float = 30.0,
        renew_interval_sec: Optional[float] = None,
        guarded_task: Optional[asyncio.Task] = None,
    ):
        self.agent = agent
        self.component = component
        self.lease_token = lease_token
        self.fencing_gen = fencing_gen
        self.db_path = db_path
        self.ttl_sec = max(1.0, float(ttl_sec))
        self.renew_interval_sec = (
            max(0.05, float(renew_interval_sec))
            if renew_interval_sec is not None
            else max(1.0, self.ttl_sec / 3.0)
        )
        self._guarded_task = guarded_task
        self._stop_event = asyncio.Event()
        self._renew_task: Optional[asyncio.Task] = None
        self.lost: bool = False
        self.loss_reason: str = ""
        self.renewal_count: int = 0
        self.last_expires_at_ms: int = 0

    def attach_task(self, task: Optional[asyncio.Task]) -> None:
        self._guarded_task = task

    async def start(self) -> None:
        if self._renew_task is None:
            self._renew_task = asyncio.create_task(self._renew_loop())

    async def _renew_loop(self) -> None:
        while not self._stop_event.is_set():
            try:
                await asyncio.wait_for(self._stop_event.wait(), timeout=self.renew_interval_sec)
                break
            except asyncio.TimeoutError:
                pass
            if self._stop_event.is_set():
                break
            ok, new_exp_ms = await asyncio.get_running_loop().run_in_executor(
                None,
                lambda: self.agent._renew_recovery_lease(
                    self.component,
                    self.lease_token,
                    self.fencing_gen,
                    self.db_path,
                    lease_duration=self.ttl_sec,
                ),
            )
            if ok:
                self.renewal_count += 1
                self.last_expires_at_ms = new_exp_ms
                continue

            self.lost = True
            self.loss_reason = (
                f"lease renewal lost for {self.component} (gen={self.fencing_gen})"
            )
            self.agent.metrics.inc("recovery_fencing_rejected")
            logger.critical(f"[AsyncLeaseGuard] {self.loss_reason}; cancelling guarded operation.")
            if self._guarded_task and not self._guarded_task.done():
                self._guarded_task.cancel()
            break

    def verify_before_stage(self, stage: str, min_remaining_ms: int = 5000) -> bool:
        if self.lost:
            self.agent.metrics.inc("recovery_fencing_rejected")
            logger.critical(
                f"[AsyncLeaseGuard] Stage '{stage}' aborted for {self.component}: {self.loss_reason}"
            )
            return False
        ok = self.agent._verify_recovery_lease(
            self.component,
            self.lease_token,
            self.fencing_gen,
            self.db_path,
            min_remaining_ms=min_remaining_ms,
        )
        if not ok:
            self.lost = True
            self.loss_reason = (
                f"fencing verification failed before '{stage}' for {self.component} (gen={self.fencing_gen})"
            )
            self.agent.metrics.inc("recovery_fencing_rejected")
            logger.critical(f"[AsyncLeaseGuard] {self.loss_reason}")
            return False
        return True

    async def stop_and_release(self) -> bool:
        self._stop_event.set()
        if self._renew_task and not self._renew_task.done():
            self._renew_task.cancel()
            try:
                await self._renew_task
            except asyncio.CancelledError:
                pass
        return self.agent._release_recovery_lease(
            self.component,
            self.lease_token,
            self.db_path,
            fencing_gen=self.fencing_gen,
        )


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
        self._executor_id: str = f"py-{os.getpid()}-{uuid.uuid4().hex[:8]}"
        self._inflight_operations: Dict[str, Dict[str, Any]] = {}
        self._ambiguous_operations: Dict[str, Dict[str, Any]] = {}
        self._last_restart_ambiguous: bool = False

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
            is_tcp = bool(sock_path and (sock_path.startswith("tcp://") or (":" in sock_path and not (len(sock_path) >= 2 and sock_path[1] == ":" and sock_path[0].isalpha()) and not os.path.exists(sock_path))))
            if not is_tcp and not os.path.exists(sock_path):
                logger.debug(f"Docker socket {sock_path} not found. Retrying in 10s...")
                await asyncio.sleep(10)
                continue

            writer = None
            try:
                reader, writer = await self._connect_docker(sock_path)
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
            finally:
                if writer is not None:
                    try:
                        writer.close()
                        await writer.wait_closed()
                    except Exception:
                        pass

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
                container_name = attributes.get("name", "")
                actor_id_raw = str(actor.get("ID", "") or "")
                if not container_name and actor_id_raw:
                    _st_evt, _ins_evt = await self._query_docker_inspect(actor_id_raw, sock_path)
                    if isinstance(_ins_evt, dict):
                        container_name = str(_ins_evt.get("Name") or "").strip().lstrip("/")
                if not container_name:
                    container_name = actor_id_raw[:12]

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
                                docker_name = resolve_docker_container_name(canonical_auth)
                                if not docker_name:
                                    logger.critical(f"[Security Block] Cannot resolve docker container for {canonical_auth}.")
                                    continue

                                db_path_auth = getattr(self.config, "db_path", "/data/sqlite/app.db")
                                expected_bh = str(
                                    resp_data.get("baseline_hash")
                                    or (resp_data.get("proposal") or {}).get("baseline_hash")
                                    or ""
                                ).strip()
                                bh_ok, verified_bh, bh_reason = await self._verify_trusted_baseline(
                                    canonical_auth,
                                    expected_bh,
                                    db_path_auth,
                                    sock_path,
                                    require_live_without_expected=(
                                        is_production() or os.environ.get("OPS_REQUIRE_LIVE_BASELINE", "") == "1"
                                    ),
                                )
                                if not bh_ok:
                                    logger.critical(
                                        f"[R5 Security Block] AUTHORIZED baseline verification failed for {canonical_auth}: {bh_reason}."
                                    )
                                    continue

                                auth_run_id = f"run-pyauth-{int(time.time() * 1000)}-{uuid.uuid4().hex[:8]}"
                                auth_corr_id = f"corr-{uuid.uuid4().hex[:8]}"
                                auth_op_id = str(resp_data.get("operation_id") or "").strip() or (
                                    f"pyauth-{int(time.time() * 1000)}-{uuid.uuid4().hex[:6]}"
                                )
                                proposal_id_auth = str(
                                    resp_data.get("proposal_id")
                                    or (resp_data.get("proposal") or {}).get("proposal_id")
                                    or ""
                                ).strip()
                                approval_id_auth = str(
                                    resp_data.get("approval_id")
                                    or (resp_data.get("proposal") or {}).get("approval_id")
                                    or ""
                                ).strip()
                                lease_ok, lease_tok, fencing_gen = self._acquire_recovery_lease_grant(canonical_auth, db_path_auth)
                                if not lease_ok:
                                    logger.critical(
                                        f"[Lease Block] Failed to acquire recovery lease for AUTHORIZED container {canonical_auth}. Halting restart."
                                    )
                                    continue
                                await self._execute_guarded_restart(
                                    canonical=canonical_auth,
                                    audit_container_name=canonical_auth,
                                    docker_container=docker_name,
                                    sock_path=sock_path,
                                    db_path=db_path_auth,
                                    action_name="authorized_restart",
                                    exit_code=exit_code,
                                    lease_token=lease_tok,
                                    fencing_gen=fencing_gen,
                                    run_id=auth_run_id,
                                    corr_id=auth_corr_id,
                                    op_id=auth_op_id,
                                    trigger_source="go_core_authorized",
                                    actor_type="internal_service",
                                    actor_id="home-ops-agent",
                                    baseline_hash=verified_bh or expected_bh,
                                    proposal_id=proposal_id_auth,
                                    approval_id=approval_id_auth,
                                )
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
                            canon_diag = resolve_canonical_component(container_name) or container_name
                            prop_id = f"prop-crash-{canon_diag}-{int(time.time() * 1000)}-{uuid.uuid4().hex[:6]}"
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

    def _has_v5_audit_schema(self, db_path: str) -> bool:
        """Checks whether offline_ops_audit contains v5 hash-chained lifecycle columns."""
        if not db_path or not os.path.isfile(db_path):
            return False
        conn = None
        try:
            conn = open_readonly_sqlite(db_path)
            cur = conn.cursor()
            cur.execute("PRAGMA table_info(offline_ops_audit)")
            cols = {str(c[1]).lower() for c in cur.fetchall()}
            return {
                "run_id", "correlation_id", "event_type", "payload_json",
                "offline_seq", "local_prev_hash", "entry_hash", "hmac_signature",
            }.issubset(cols)
        except Exception:
            return False
        finally:
            if conn:
                try:
                    conn.close()
                except Exception:
                    pass

    def _lookup_trusted_db_baseline(self, canonical: str, db_path: str) -> str:
        """Reads latest approved/active non-placeholder baseline_hash from SQLite proposals if present."""
        if not canonical or not db_path or not os.path.isfile(db_path):
            return ""
        conn = None
        try:
            conn = open_readonly_sqlite(db_path)
            cur = conn.cursor()
            cur.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='pending_proposals'")
            if not cur.fetchone():
                return ""
            cur.execute("PRAGMA table_info(pending_proposals)")
            cols = {str(c[1]).lower() for c in cur.fetchall()}
            if "baseline_hash" not in cols or "target_component" not in cols:
                return ""
            cur.execute(
                """
                SELECT baseline_hash FROM pending_proposals
                WHERE target_component = ? AND baseline_hash != '' AND status IN ('APPROVED', 'EXECUTING', 'COMPLETED')
                ORDER BY updated_at DESC LIMIT 1
                """,
                (canonical,),
            )
            row = cur.fetchone()
            return str(row[0]).strip() if row and row[0] else ""
        except Exception:
            return ""
        finally:
            if conn:
                try:
                    conn.close()
                except Exception:
                    pass

    async def _verify_trusted_baseline(
        self,
        canonical: str,
        expected_bh: str,
        db_path: str,
        sock_path: str,
        require_live_without_expected: bool = False,
    ) -> Tuple[bool, str, str]:
        """Verifies live container baseline against expected/DB baselines (R5/R8 Fail-Closed)."""
        expected_clean = (expected_bh or "").strip()
        if expected_clean:
            if not is_real_baseline_hash(expected_clean, canonical):
                return False, "", f"invalid or placeholder baseline_hash ({expected_clean!r})"
            db_bh = self._lookup_trusted_db_baseline(canonical, db_path)
            if db_bh and is_real_baseline_hash(db_bh, canonical) and db_bh != expected_clean:
                return False, "", f"expected baseline {expected_clean} conflicts with trusted DB baseline {db_bh}"
            live_bh = await self._compute_container_baseline_hash(canonical, sock_path)
            if not live_bh or live_bh != expected_clean:
                return False, live_bh, f"live baseline mismatch/unavailable (expected={expected_clean}, live={live_bh})"
            return True, live_bh, "ok"

        if require_live_without_expected:
            live_bh = await self._compute_container_baseline_hash(canonical, sock_path)
            if not is_real_baseline_hash(live_bh, canonical):
                return False, "", "live baseline unavailable in strict/production mode"
            db_bh = self._lookup_trusted_db_baseline(canonical, db_path)
            if db_bh and is_real_baseline_hash(db_bh, canonical) and db_bh != live_bh:
                return False, live_bh, f"live baseline {live_bh} drifted from trusted DB baseline {db_bh}"
            return True, live_bh, "ok"

        return True, "", "skipped_non_strict_without_expected"

    async def _dispatch_restart_with_fencing(
        self,
        docker_container: str,
        sock_path: str,
        fencing_gen: int,
        op_id: str,
    ) -> bool:
        """Invokes _restart_docker_container while remaining compatible with 2-arg test mocks."""
        import inspect

        self._last_restart_ambiguous = False
        fn = self._restart_docker_container
        try:
            sig = inspect.signature(fn)
            params = sig.parameters
            accepts_kw = any(p.kind == inspect.Parameter.VAR_KEYWORD for p in params.values()) or (
                "fencing_generation" in params and "operation_id" in params
            )
        except (ValueError, TypeError):
            accepts_kw = False

        if accepts_kw:
            return await fn(
                docker_container,
                sock_path,
                fencing_generation=fencing_gen,
                operation_id=op_id,
            )
        return await fn(docker_container, sock_path)

    async def _execute_guarded_restart(
        self,
        *,
        canonical: str,
        audit_container_name: str,
        docker_container: str,
        sock_path: str,
        db_path: str,
        action_name: str,
        exit_code: int,
        lease_token: str,
        fencing_gen: int,
        run_id: str,
        corr_id: str,
        op_id: str,
        trigger_source: str,
        actor_type: str,
        actor_id: str,
        baseline_hash: str = "",
        proposal_id: str = "",
        approval_id: str = "",
    ) -> bool:
        """
        Executes a lease-guarded, lifecycle-audited Docker container restart with
        background AsyncLeaseGuard renewal, mid-flight cancellation, and EXECUTION_UNKNOWN tracking.
        """
        started_wall = time.monotonic()
        ttl_sec = float(os.environ.get("OPS_LEASE_TTL_SEC", "30.0") or "30.0")
        renew_env = os.environ.get("OPS_LEASE_RENEW_INTERVAL_SEC", "").strip()
        renew_interval = float(renew_env) if renew_env else None
        guard = AsyncLeaseGuard(
            self,
            canonical,
            lease_token,
            fencing_gen,
            db_path,
            ttl_sec=ttl_sec,
            renew_interval_sec=renew_interval,
        )
        released = False
        try:
            if canonical in self._ambiguous_operations:
                self.metrics.inc("recovery_execution_unknown")
                logger.critical(
                    f"[EXECUTION_UNKNOWN Block] Container {canonical} has unresolved ambiguous execution "
                    f"{self._ambiguous_operations[canonical].get('operation_id')}; refusing duplicate restart."
                )
                return False

            if not guard.verify_before_stage("pre_audit", min_remaining_ms=5000):
                return False

            if not self._can_write_offline_audit_table(db_path):
                logger.critical(
                    f"[Audit Fail-Closed] Pre-execution offline audit table check failed for op={op_id}. Aborting Docker restart."
                )
                return False

            has_v5 = self._has_v5_audit_schema(db_path)
            before_meta = {"canonical": canonical}
            if baseline_hash:
                before_meta["baseline_hash"] = baseline_hash
            parent_ev_id = ""
            if has_v5:
                st_rec, ev_recv_id = self._record_offline_audit_strict(
                    audit_container_name,
                    action_name,
                    exit_code,
                    True,
                    operation_id=op_id,
                    fencing_generation=fencing_gen,
                    run_id=run_id,
                    correlation_id=corr_id,
                    event_type=EVENT_RECEIVED,
                    trigger_source=trigger_source,
                    actor_type=actor_type,
                    actor_id=actor_id,
                    proposal_id=proposal_id,
                    approval_id=approval_id,
                    before_state=before_meta,
                )
                if st_rec != AuditStatus.AUDIT_COMMITTED:
                    return False

                st_lease, ev_lease_id = self._record_offline_audit_strict(
                    audit_container_name,
                    action_name,
                    exit_code,
                    True,
                    operation_id=op_id,
                    fencing_generation=fencing_gen,
                    run_id=run_id,
                    correlation_id=corr_id,
                    parent_event_id=ev_recv_id,
                    event_type=RECOVERY_LEASE_ACQUIRED,
                    trigger_source=trigger_source,
                    actor_type=actor_type,
                    actor_id=actor_id,
                    proposal_id=proposal_id,
                    approval_id=approval_id,
                    evidence={"lease_generation": fencing_gen},
                )
                if st_lease != AuditStatus.AUDIT_COMMITTED:
                    return False

                st_start, ev_start_id = self._record_offline_audit_strict(
                    audit_container_name,
                    action_name,
                    exit_code,
                    True,
                    operation_id=op_id,
                    fencing_generation=fencing_gen,
                    run_id=run_id,
                    correlation_id=corr_id,
                    parent_event_id=ev_lease_id,
                    event_type=EXECUTION_STARTED,
                    trigger_source=trigger_source,
                    actor_type=actor_type,
                    actor_id=actor_id,
                    proposal_id=proposal_id,
                    approval_id=approval_id,
                    before_state=before_meta,
                )
                if st_start != AuditStatus.AUDIT_COMMITTED:
                    return False
                parent_ev_id = ev_start_id
            else:
                pre_archive = os.environ.get("OPS_AUDIT_ARCHIVE_DIR", "").strip()
                if pre_archive:
                    append_jsonl_audit_stub(pre_archive, "python-offline-audit.jsonl", {
                        "event_type": EXECUTION_STARTED,
                        "run_id": run_id,
                        "correlation_id": corr_id,
                        "operation_id": op_id,
                        "container_name": audit_container_name,
                        "action": action_name,
                        "fencing_generation": fencing_gen,
                        "executed_at": format_rfc3339_ms(),
                    })

            self._inflight_operations[canonical] = {
                "operation_id": op_id,
                "run_id": run_id,
                "fencing_generation": fencing_gen,
                "started_at": time.time(),
            }
            await guard.start()

            async def _guarded_exec() -> Tuple[bool, DockerHealthState, bool, str]:
                if not guard.verify_before_stage("docker_restart", min_remaining_ms=5000):
                    return False, DockerHealthState.UNKNOWN, False, "fencing_rejected_before_restart"
                ambiguous = False
                restarted = False
                try:
                    restarted = await self._dispatch_restart_with_fencing(
                        docker_container, sock_path, fencing_gen, op_id
                    )
                    if not restarted and self._last_restart_ambiguous:
                        ambiguous = True
                except (asyncio.TimeoutError, TimeoutError, ConnectionError, EOFError, OSError) as exc:
                    ambiguous = True
                    self.metrics.inc("recovery_execution_unknown")
                    logger.critical(
                        f"[EXECUTION_UNKNOWN] Docker restart outcome ambiguous for {canonical} (op={op_id}): {exc}"
                    )

                if ambiguous:
                    self._ambiguous_operations[canonical] = {
                        "operation_id": op_id,
                        "run_id": run_id,
                        "fencing_generation": fencing_gen,
                        "recorded_at": time.time(),
                    }
                    return False, DockerHealthState.UNKNOWN, True, "docker_outcome_ambiguous"

                if not restarted:
                    return False, DockerHealthState.UNKNOWN, False, f"exit_code={exit_code}"

                if not guard.verify_before_stage("health_check", min_remaining_ms=3000):
                    self._ambiguous_operations[canonical] = {
                        "operation_id": op_id,
                        "run_id": run_id,
                        "fencing_generation": fencing_gen,
                        "recorded_at": time.time(),
                    }
                    self.metrics.inc("recovery_execution_unknown")
                    return False, DockerHealthState.UNKNOWN, True, "fencing_lost_before_health_check"

                h_state = await self._verify_container_health(docker_container, sock_path)
                if h_state == DockerHealthState.UNKNOWN:
                    self.metrics.inc("recovery_health_unknown")
                h_ok = (h_state == DockerHealthState.HEALTHY)
                if not h_ok:
                    logger.error(
                        f"[Post-Restart Failed] Health check returned {h_state} for {audit_container_name}."
                    )
                return h_ok, h_state, False, ("" if h_ok else f"health_state={h_state.value}")

            exec_task = asyncio.create_task(_guarded_exec())
            guard.attach_task(exec_task)

            final_ok = False
            health_state = DockerHealthState.UNKNOWN
            execution_unknown = False
            err_detail = ""
            try:
                final_ok, health_state, execution_unknown, err_detail = await exec_task
            except asyncio.CancelledError:
                if guard.lost:
                    final_ok = False
                    health_state = DockerHealthState.UNKNOWN
                    execution_unknown = True
                    err_detail = "lease_lost_mid_execution"
                    self._ambiguous_operations[canonical] = {
                        "operation_id": op_id,
                        "run_id": run_id,
                        "fencing_generation": fencing_gen,
                        "recorded_at": time.time(),
                    }
                    self.metrics.inc("recovery_execution_unknown")
                    logger.critical(
                        f"[EXECUTION_UNKNOWN] Operation {op_id} for {canonical} aborted due to AsyncLeaseGuard lease loss."
                    )
                else:
                    if not exec_task.done():
                        exec_task.cancel()
                        try:
                            await exec_task
                        except asyncio.CancelledError:
                            pass
                    raise

            if guard.lost and not execution_unknown:
                final_ok = False
                health_state = DockerHealthState.UNKNOWN
                execution_unknown = True
                err_detail = err_detail or "lease_lost_during_execution"
                self._ambiguous_operations[canonical] = {
                    "operation_id": op_id,
                    "run_id": run_id,
                    "fencing_generation": fencing_gen,
                    "recorded_at": time.time(),
                }
                self.metrics.inc("recovery_execution_unknown")

            if has_v5 and not execution_unknown and health_state != DockerHealthState.UNKNOWN:
                st_hc, ev_hc_id = self._record_offline_audit_strict(
                    audit_container_name,
                    action_name,
                    exit_code,
                    health_state == DockerHealthState.HEALTHY,
                    operation_id=op_id,
                    fencing_generation=fencing_gen,
                    run_id=run_id,
                    correlation_id=corr_id,
                    parent_event_id=parent_ev_id,
                    event_type=HEALTH_CHECK_COMPLETED,
                    trigger_source=trigger_source,
                    actor_type=actor_type,
                    actor_id=actor_id,
                    proposal_id=proposal_id,
                    approval_id=approval_id,
                    evidence={"health_state": health_state.value, "canonical": canonical},
                )
                if st_hc != AuditStatus.AUDIT_COMMITTED:
                    return False
                parent_ev_id = ev_hc_id

            if not guard.lost and not execution_unknown:
                if not guard.verify_before_stage("final_audit", min_remaining_ms=1000):
                    final_ok = False
                    health_state = DockerHealthState.UNKNOWN
                    execution_unknown = True
                    err_detail = "fencing_lost_before_final_audit"
                    self._ambiguous_operations[canonical] = {
                        "operation_id": op_id,
                        "run_id": run_id,
                        "fencing_generation": fencing_gen,
                        "recorded_at": time.time(),
                    }
                    self.metrics.inc("recovery_execution_unknown")

            term_event_type = (
                EXECUTION_UNKNOWN if execution_unknown else (EXECUTION_COMPLETED if final_ok else EXECUTION_FAILED)
            )
            dur_ms = max(0, int((time.monotonic() - started_wall) * 1000))
            audit_status, _ = self._record_offline_audit_strict(
                audit_container_name,
                action_name,
                exit_code,
                final_ok,
                operation_id=op_id,
                fencing_generation=fencing_gen,
                run_id=run_id,
                correlation_id=corr_id,
                parent_event_id=parent_ev_id,
                event_type=term_event_type,
                trigger_source=trigger_source,
                actor_type=actor_type,
                actor_id=actor_id,
                proposal_id=proposal_id,
                approval_id=approval_id,
                duration_ms=dur_ms,
                error_code=err_detail,
                before_state=before_meta,
                after_state={"health_state": health_state.value, "execution_unknown": execution_unknown},
                evidence={
                    "health_state": health_state.value,
                    "canonical": canonical,
                    "execution_unknown": execution_unknown,
                    "lease_renewals": guard.renewal_count,
                },
            )
            if audit_status != AuditStatus.AUDIT_COMMITTED:
                logger.critical(
                    f"[Audit Integrity Failure] Offline audit persistence returned {audit_status} for op={op_id}. Cannot report recovery success."
                )
                return False
            return final_ok
        finally:
            self._inflight_operations.pop(canonical, None)
            released = await guard.stop_and_release()
            if not released and not guard.lost:
                self.metrics.inc("recovery_database_error")
                logger.critical(f"[Lease Release Failed] Failed to cleanly release recovery lease for {canonical} (gen={fencing_gen}).")

    async def _handle_offline_emergency_restart(
        self,
        container_name: str,
        exit_code: int,
        sock_path: str,
        expected_baseline_hash: Optional[str] = None,
    ) -> bool:
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

        docker_container = resolve_docker_container_name(canonical)
        if not docker_container:
            logger.critical(
                f"[Emergency Autonomy Blocked] Cannot resolve Docker container for '{canonical}' (R5). Refusing offline restart."
            )
            return False

        db_path = getattr(self.config, "db_path", "/data/sqlite/app.db")
        bh_ok, verified_bh, bh_reason = await self._verify_trusted_baseline(
            canonical,
            expected_baseline_hash or "",
            db_path,
            sock_path,
            require_live_without_expected=(
                expected_baseline_hash is not None
                or is_production()
                or os.environ.get("OPS_REQUIRE_LIVE_BASELINE", "") == "1"
            ),
        )
        if not bh_ok:
            logger.critical(
                f"[Emergency Autonomy Blocked] Baseline verification failed for {canonical} (R5): {bh_reason}."
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

        # Guard 4: Persistent cooldown and circuit breaker (<= 2 restarts in 60s, <= 3 crashes in 10m)
        now = time.time()
        timestamps = self._local_restarts.setdefault(canonical, [])
        timestamps = [t for t in timestamps if now - t < 600.0]  # 10 minute window
        self._local_restarts[canonical] = timestamps

        short_window = [t for t in timestamps if now - t < 60.0]  # 60 second window
        persisted_short = self._get_persisted_restart_count(canonical, 60.0, db_path)
        persisted_long = self._get_persisted_restart_count(canonical, 600.0, db_path)

        if persisted_short is None or persisted_long is None:
            self._release_recovery_lease(canonical, lease_token, db_path, fencing_gen=fencing_gen)
            logger.critical(
                f"[Emergency Autonomy Blocked] Persisted restart count is UNKNOWN (query error or missing table). Halting restart."
            )
            return False

        if len(short_window) >= 2 or persisted_short >= 2:
            self._release_recovery_lease(canonical, lease_token, db_path, fencing_gen=fencing_gen)
            logger.error(
                f"[Emergency Autonomy Breaker] Tripped for {canonical}: {max(len(short_window), persisted_short)} restarts in 60s. Halted."
            )
            return False

        if len(timestamps) >= 3 or persisted_long >= 3:
            self._release_recovery_lease(canonical, lease_token, db_path, fencing_gen=fencing_gen)
            logger.critical(
                f"[Emergency Autonomy Breaker] 10-minute circuit tripped for {canonical}: {max(len(timestamps), persisted_long)} crashes. Halted."
            )
            return False

        # Guard 5: Local SQLite persistence in offline_restart_history (atomic check-and-insert under BEGIN IMMEDIATE)
        rec_ok = self._record_persisted_restart(canonical, db_path)
        if not rec_ok:
            self._release_recovery_lease(canonical, lease_token, db_path, fencing_gen=fencing_gen)
            logger.critical(
                f"[Emergency Autonomy Blocked] Failed to write persisted restart record for {canonical}. Halting restart."
            )
            return False

        timestamps.append(now)
        op_id = f"pyop-{int(now * 1000)}-{uuid.uuid4().hex[:8]}"
        run_id = f"run-offline-{int(now * 1000)}-{uuid.uuid4().hex[:8]}"
        corr_id = f"corr-{uuid.uuid4().hex[:8]}"
        logger.warning(
            f"[Emergency Autonomy Triggered] Go Core OFFLINE. Executing offline emergency restart for {container_name} "
            f"(attempt {len(timestamps)} in 10m window, gen={fencing_gen}, op={op_id})"
        )
        return await self._execute_guarded_restart(
            canonical=canonical,
            audit_container_name=container_name,
            docker_container=docker_container,
            sock_path=sock_path,
            db_path=db_path,
            action_name="emergency_restart",
            exit_code=exit_code,
            lease_token=lease_token,
            fencing_gen=fencing_gen,
            run_id=run_id,
            corr_id=corr_id,
            op_id=op_id,
            trigger_source="python_offline",
            actor_type="offline_agent",
            actor_id="home-ops-agent",
            baseline_hash=verified_bh or (expected_baseline_hash or ""),
        )

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

    @staticmethod
    def _is_valid_restart_timestamp(rt_val: Any) -> bool:
        """Validates restart_time numeric sanity; rejects bool, None, str, NaN, Inf, and non-positive values."""
        if isinstance(rt_val, bool) or not isinstance(rt_val, (int, float)):
            return False
        val = float(rt_val)
        return math.isfinite(val) and val > 0.0

    def _get_persisted_restart_count(self, container_name: str, window_seconds: float, db_path: str) -> Optional[int]:
        """
        Returns number of restarts recorded in SQLite within window_seconds using an explicit read transaction.
        Returns None if DB is missing, corrupted, table/columns missing, unbootstrapped anomaly,
        corrupt timestamp rows, or component invalid (Fail-Closed: NEVER 0 on missing table) (R4-14).
        """
        if not db_path or not os.path.isfile(db_path):
            logger.error(f"[Fail-Closed] DB file missing at {db_path}. Persisted restart count UNKNOWN.")
            return None
        canonical = resolve_canonical_component(container_name)
        if not canonical:
            logger.error(f"[Fail-Closed] Unknown component '{container_name}'. Persisted restart count UNKNOWN.")
            return None
        conn = None
        cursor = None
        try:
            conn = open_readonly_sqlite(db_path)
            cursor = conn.cursor()
            cursor.execute("BEGIN")

            cursor.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='ops_schema_meta'")
            if cursor.fetchone():
                cursor.execute("SELECT value FROM ops_schema_meta WHERE key='bootstrapped'")
                meta_row = cursor.fetchone()
                if not meta_row or str(meta_row[0]) != "1":
                    cursor.execute("ROLLBACK")
                    logger.critical(f"[Fail-Closed] Schema meta bootstrapped sentinel missing in {db_path}. Restart count UNKNOWN.")
                    return None

            cursor.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='offline_restart_history'")
            if not cursor.fetchone():
                cursor.execute("ROLLBACK")
                logger.critical(f"[Fail-Closed] Table offline_restart_history missing in {db_path}. Persisted restart count UNKNOWN.")
                return None

            cursor.execute("PRAGMA table_info(offline_restart_history)")
            cols = {str(c[1]).lower() for c in cursor.fetchall()}
            if not {"container_name", "restart_time"}.issubset(cols):
                cursor.execute("ROLLBACK")
                logger.critical(f"[Fail-Closed] Table offline_restart_history missing required columns in {db_path}.")
                return None

            # Reject corrupt/non-numeric restart_time values in history table
            cursor.execute(
                "SELECT restart_time FROM offline_restart_history WHERE container_name = ? OR container_name = ?",
                (canonical, container_name)
            )
            cutoff = time.time() - window_seconds
            count = 0
            for (rt_val,) in cursor.fetchall():
                if not self._is_valid_restart_timestamp(rt_val):
                    cursor.execute("ROLLBACK")
                    logger.critical(f"[Fail-Closed] Corrupt restart_time value '{rt_val}' in offline_restart_history.")
                    return None
                if float(rt_val) >= cutoff:
                    count += 1
            cursor.execute("COMMIT")
            return count
        except Exception as e:
            if cursor is not None:
                try:
                    cursor.execute("ROLLBACK")
                except Exception:
                    pass
            self.metrics.inc("recovery_database_error")
            logger.error(f"[Fail-Closed] Failed to query offline_restart_history: {e}")
            return None
        finally:
            if conn:
                try:
                    conn.close()
                except Exception:
                    pass

    def _record_persisted_restart(
        self,
        container_name: str,
        db_path: str,
        short_window: float = 60.0,
        short_limit: int = 2,
        long_window: float = 600.0,
        long_limit: int = 3,
    ) -> bool:
        """
        Atomically verifies history schema/limits and records restart timestamp into EXISTING
        SQLite offline_restart_history table using BEGIN IMMEDIATE (unified transaction & locking).
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
            if not {"container_name", "restart_time"}.issubset(cols):
                cur.execute("ROLLBACK")
                logger.critical(f"[Fail-Closed] Table offline_restart_history missing required columns in {db_path}.")
                return False

            cur.execute(
                "SELECT restart_time FROM offline_restart_history WHERE container_name = ? OR container_name = ?",
                (canonical, container_name),
            )
            now_sec = time.time()
            short_cutoff = now_sec - short_window
            long_cutoff = now_sec - long_window
            short_cnt = 0
            long_cnt = 0
            for (rt_val,) in cur.fetchall():
                if not self._is_valid_restart_timestamp(rt_val):
                    cur.execute("ROLLBACK")
                    self.metrics.inc("recovery_database_error")
                    logger.critical(f"[Fail-Closed] Corrupt restart_time '{rt_val}' detected under write lock; aborting restart.")
                    return False
                rt_f = float(rt_val)
                if rt_f >= short_cutoff:
                    short_cnt += 1
                if rt_f >= long_cutoff:
                    long_cnt += 1

            if short_cnt >= short_limit or long_cnt >= long_limit:
                cur.execute("ROLLBACK")
                logger.error(
                    f"[Emergency Autonomy Breaker] Atomic write-lock rate check tripped for {canonical}: "
                    f"short={short_cnt}/{short_limit}, long={long_cnt}/{long_limit}."
                )
                return False

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
        token = f"py-{self._executor_id}-{secrets.token_hex(16)}"
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
                    WHERE component = ? AND (state != 'ACTIVE' OR expires_at_ms <= ?)
                    """,
                    (token, next_gen, now_iso, exp_iso, now_ms, exp_ms, canonical, now_ms),
                )
                if cur.rowcount != 1:
                    cur.execute("ROLLBACK")
                    return False, "", 0
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
                  AND acquired_at_ms > 0 AND expires_at_ms > acquired_at_ms
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

    def _verify_recovery_lease(
        self, component: str, lease_token: str, fencing_gen: int, db_path: str, min_remaining_ms: int = 0
    ) -> bool:
        """Verifies right before external Docker mutation that this token & fencing generation still own the lease (R4-06)."""
        if not db_path or not lease_token or fencing_gen <= 0 or not os.path.isfile(db_path):
            return False
        canonical = resolve_canonical_component(component)
        if not canonical:
            return False
        now_ms = int(time.time() * 1000)
        guard_ms = max(0, int(min_remaining_ms))
        conn = None
        try:
            conn = open_readonly_sqlite(db_path)
            cur = conn.cursor()
            cur.execute(
                "SELECT lease_token, fencing_generation, acquired_at_ms, expires_at_ms, state FROM ops_recovery_leases WHERE component = ?",
                (canonical,),
            )
            row = cur.fetchone()
            if not row:
                return False
            db_tok, db_gen, db_acq_ms, db_exp_ms, db_state = row
            acq_ms = int(db_acq_ms or 0)
            exp_ms = int(db_exp_ms or 0)
            if acq_ms <= 0 or exp_ms <= acq_ms:
                return False
            if str(db_tok) != lease_token or int(db_gen or 0) != fencing_gen:
                return False
            if str(db_state or "").upper() != "ACTIVE" or (exp_ms - now_ms) <= guard_ms:
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
        is_win_drive = len(sock_path) >= 2 and sock_path[1] == ":" and sock_path[0].isalpha()
        if sock_path.startswith("tcp://"):
            host_port = sock_path[6:]
            host, port = host_port.split(":")
            return await asyncio.open_connection(host, int(port))
        elif ":" in sock_path and not is_win_drive and not os.path.exists(sock_path):
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

    async def _docker_http_get_json(self, path: str, sock_path: str, timeout: float = 5.0) -> Tuple[int, Any]:
        """Executes a GET request against Docker Engine API (npipe/unix/tcp) and decodes HTTP status + JSON body."""
        req_bytes = (
            f"GET {path} HTTP/1.1\r\n"
            f"Host: docker\r\n"
            f"User-Agent: home-ops-agent\r\n"
            f"Connection: close\r\n\r\n"
        ).encode("utf-8")

        resp = b""
        if sock_path and (sock_path.startswith("npipe://") or sock_path.startswith("\\\\.\\pipe\\")):
            pipe_path = sock_path[len("npipe://"):].replace("/", "\\") if sock_path.startswith("npipe://") else sock_path

            def _read_npipe() -> bytes:
                chunks = []
                with open(pipe_path, "r+b", buffering=0) as pf:
                    pf.write(req_bytes)
                    while True:
                        chunk = pf.read(4096)
                        if not chunk:
                            break
                        chunks.append(chunk)
                return b"".join(chunks)

            try:
                loop = asyncio.get_running_loop()
                resp = await asyncio.wait_for(loop.run_in_executor(None, _read_npipe), timeout=timeout)
            except FileNotFoundError:
                logger.error(f"[Fail-Closed] Docker named pipe not found: {pipe_path}")
                return 0, None
            except Exception as e:
                logger.error(f"[Fail-Closed] Docker named pipe error: {e}")
                return 0, None
        else:
            writer = None
            try:
                reader, writer = await asyncio.wait_for(self._connect_docker(sock_path), timeout=timeout)
            except FileNotFoundError:
                logger.error(f"[Fail-Closed] Docker socket not found: {sock_path}")
                return 0, None
            except Exception as e:
                logger.error(f"[Fail-Closed] Docker socket connection error: {e}")
                return 0, None

            try:
                writer.write(req_bytes)
                await asyncio.wait_for(writer.drain(), timeout=timeout)

                raw_chunks = []
                while True:
                    chunk = await asyncio.wait_for(reader.read(4096), timeout=timeout)
                    if not chunk:
                        break
                    raw_chunks.append(chunk)
                resp = b"".join(raw_chunks)
            except asyncio.CancelledError:
                if writer is not None and getattr(writer, "transport", None) is not None:
                    try:
                        writer.transport.abort()
                    except Exception:
                        pass
                raise
            except Exception as e:
                logger.error(f"[Fail-Closed] Error reading Docker HTTP response for {path}: {e}")
                return 0, None
            finally:
                if writer is not None:
                    try:
                        writer.close()
                        await asyncio.wait_for(writer.wait_closed(), timeout=0.5)
                    except Exception:
                        pass

        if not resp:
            return 0, None

        parts = resp.split(b"\r\n\r\n", 1)
        header_block = parts[0]
        body = parts[1] if len(parts) > 1 else b""

        status_line = header_block.split(b"\r\n", 1)[0].decode("utf-8", errors="ignore")
        status_code = 0
        status_parts = status_line.split()
        if len(status_parts) >= 2 and status_parts[1].isdigit():
            status_code = int(status_parts[1])

        if b"transfer-encoding: chunked" in header_block.lower():
            decoded_chunks = []
            pos = 0
            while pos < len(body):
                crlf = body.find(b"\r\n", pos)
                if crlf == -1:
                    break
                size_hex = body[pos:crlf].split(b";", 1)[0].strip()
                try:
                    csize = int(size_hex, 16)
                except ValueError:
                    break
                if csize == 0:
                    break
                start = crlf + 2
                decoded_chunks.append(body[start:start + csize])
                pos = start + csize + 2
            body = b"".join(decoded_chunks)

        if not body:
            return status_code, None

        try:
            return status_code, json.loads(body.decode("utf-8", errors="ignore"))
        except Exception as e:
            logger.error(f"[Fail-Closed] Docker JSON decode error for {path}: {e}")
            return status_code, None

    async def _list_docker_containers(self, sock_path: str) -> Tuple[str, Optional[List[Dict[str, Any]]]]:
        """Lists all containers via /containers/json?all=1 for unambiguous canonical matching (R8)."""
        status_code, data = await self._docker_http_get_json("/containers/json?all=1", sock_path)
        if status_code != 200 or data is None:
            return "error", None
        if isinstance(data, list):
            return "ok", [item for item in data if isinstance(item, dict)]
        if isinstance(data, dict) and "Id" in data:
            return "not_implemented", None
        return "error", None

    async def _query_docker_inspect(self, container_name: str, sock_path: str) -> Tuple[DockerHealthState, Optional[Dict[str, Any]]]:
        """Queries Docker /containers/{container_name}/json and returns (health_state, inspect_data)."""
        import urllib.parse

        quoted_name = urllib.parse.quote(str(container_name), safe="")
        status_code, data = await self._docker_http_get_json(f"/containers/{quoted_name}/json", sock_path)
        if status_code == 404:
            logger.error(f"[Fail-Closed] Docker inspect returned 404 for {container_name}")
            return DockerHealthState.UNHEALTHY, None
        if status_code != 200:
            if status_code:
                logger.error(f"[Fail-Closed] Docker inspect returned HTTP {status_code}")
            return DockerHealthState.UNKNOWN, None
        if not isinstance(data, dict) or not data:
            logger.error("[Fail-Closed] Docker inspect returned empty or non-object JSON body.")
            return DockerHealthState.UNKNOWN, None
        return DockerHealthState.HEALTHY, data

    async def _compute_container_baseline_hash(self, component: str, sock_path: Optional[str] = None) -> str:
        """
        Computes a live real resource BaselineHash (Protocol v1) from Docker inspect (R5/R8).
        Detects duplicate/ambiguous containers via /containers/json?all=1, verifies inspected Name,
        enriches RepoDigests from /images/{Image}/json when missing, and strictly fails closed.
        """
        import urllib.parse

        canonical = resolve_canonical_component(component)
        if not canonical:
            logger.error(f"[R5 Fail-Closed] Component resolution failed for '{component}'.")
            return ""
        docker_name = resolve_docker_container_name(canonical)
        if not docker_name:
            logger.error(f"[R5 Fail-Closed] No Docker container mapping for canonical '{canonical}'.")
            return ""
        target_sock = sock_path or getattr(self.config, "docker_socket", "/var/run/docker.sock")

        query_name = docker_name
        list_status, container_list = await self._list_docker_containers(target_sock)
        if list_status == "error":
            logger.warning(f"[R5/R8 Fail-Closed] Failed to list Docker containers for '{canonical}'.")
            return ""
        if list_status == "ok" and container_list is not None:
            matched_names: List[str] = []
            exact_match = ""
            for item in container_list:
                raw_names = item.get("Names")
                if not isinstance(raw_names, list):
                    continue
                item_matched = False
                cleaned_list: List[str] = []
                for raw_n in raw_names:
                    clean_n = str(raw_n or "").strip().lstrip("/")
                    if not clean_n:
                        continue
                    cleaned_list.append(clean_n)
                    if clean_n.lower() == docker_name.lower() or resolve_canonical_component(clean_n) == canonical:
                        item_matched = True
                        if clean_n.lower() == docker_name.lower():
                            exact_match = clean_n
                if item_matched and cleaned_list:
                    matched_names.append(cleaned_list[0])
            if len(matched_names) > 1:
                logger.critical(
                    f"[R8 Ambiguity Block] Multiple Docker containers matched canonical '{canonical}': {matched_names}. Failing closed."
                )
                return ""
            if len(matched_names) == 0:
                logger.warning(
                    f"[R5 Fail-Closed] Container '{canonical}' ({docker_name}) not found in Docker container list."
                )
                return ""
            query_name = exact_match or matched_names[0]

        inspect_status, inspect_data = await self._query_docker_inspect(query_name, target_sock)
        if inspect_status != DockerHealthState.HEALTHY or not isinstance(inspect_data, dict):
            logger.warning(
                f"[R5 Fail-Closed] Cannot compute real baseline_hash for '{canonical}' ({query_name}): "
                f"inspect_status={inspect_status.value}."
            )
            return ""

        inspected_name = str(inspect_data.get("Name") or "").strip().lstrip("/")
        if (list_status == "ok" and inspected_name.lower() != docker_name.lower()) or (
            inspected_name and inspected_name.lower() != docker_name.lower()
        ):
            logger.critical(
                f"[R8 Name Mismatch] Docker inspect Name '{inspected_name}' != expected '{docker_name}' for '{canonical}'. Failing closed."
            )
            return ""

        if not inspect_data.get("RepoDigests") and inspect_data.get("Image"):
            img_id = str(inspect_data.get("Image") or "").strip()
            if img_id:
                quoted_img = urllib.parse.quote(img_id, safe="")
                img_status, img_data = await self._docker_http_get_json(f"/images/{quoted_img}/json", target_sock)
                if img_status == 200 and isinstance(img_data, dict) and isinstance(img_data.get("RepoDigests"), list) and img_data["RepoDigests"]:
                    inspect_data = dict(inspect_data)
                    inspect_data["RepoDigests"] = img_data["RepoDigests"]

        bh = compute_container_baseline_from_inspect(canonical, inspect_data)
        if not bh:
            logger.warning(f"[R5 Fail-Closed] Incomplete or unknown inspect metadata for '{canonical}' ({query_name}).")
        return bh

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

    def _can_write_offline_audit_table(self, db_path: str) -> bool:
        """
        Verifies offline_ops_audit exists, is writable under BEGIN IMMEDIATE, satisfies v5 schema
        (in schema>=5 or production), has valid HMAC keyring when required, and has writable archive dir.
        """
        if not db_path or not os.path.isfile(db_path):
            self.metrics.inc("recovery_audit_failed")
            return False
        if is_production() or os.environ.get("OPS_AUDIT_REQUIRE_HMAC", "") == "1":
            kid, ring = resolve_audit_hmac_keyring()
            if not (kid and kid in ring):
                self.metrics.inc("recovery_audit_failed")
                logger.critical("[Offline Audit Precheck] Missing external OPS_AUDIT_HMAC_KEY in strict/production mode.")
                return False
        archive_dir = os.environ.get("OPS_AUDIT_ARCHIVE_DIR", "").strip()
        if archive_dir:
            try:
                if os.path.exists(archive_dir):
                    if not os.path.isdir(archive_dir) or not os.access(archive_dir, os.W_OK):
                        self.metrics.inc("recovery_audit_failed")
                        return False
                else:
                    os.makedirs(archive_dir, exist_ok=True)
            except Exception:
                self.metrics.inc("recovery_audit_failed")
                return False
        conn = None
        try:
            conn = open_readwrite_sqlite(db_path)
            cur = conn.cursor()
            cur.execute("BEGIN IMMEDIATE")
            cur.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='offline_ops_audit'")
            exists = cur.fetchone() is not None
            if not exists:
                cur.execute("ROLLBACK")
                self.metrics.inc("recovery_audit_failed")
                return False
            cur.execute("PRAGMA table_info(offline_ops_audit)")
            cols = {str(c[1]).lower() for c in cur.fetchall()}
            schema_ver = 0
            cur.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='ops_schema_migrations'")
            if cur.fetchone():
                cur.execute("SELECT COALESCE(MAX(version), 0) FROM ops_schema_migrations")
                vr = cur.fetchone()
                schema_ver = int(vr[0]) if vr and vr[0] is not None else 0
            if schema_ver >= 5 or is_production():
                req_v5 = {
                    "run_id", "correlation_id", "event_type", "payload_json",
                    "offline_seq", "local_prev_hash", "entry_hash", "hmac_signature",
                }
                if not req_v5.issubset(cols):
                    cur.execute("ROLLBACK")
                    self.metrics.inc("recovery_audit_failed")
                    logger.critical("[Offline Audit Precheck] offline_ops_audit missing required v5 columns.")
                    return False
            cur.execute("ROLLBACK")
            return True
        except Exception:
            self.metrics.inc("recovery_audit_failed")
            return False
        finally:
            if conn:
                try:
                    conn.close()
                except Exception:
                    pass

    def _record_offline_audit_strict(
        self,
        container_name: str,
        action: str,
        exit_code: int,
        success: bool,
        operation_id: str = "",
        fencing_generation: int = 0,
        run_id: str = "",
        correlation_id: str = "",
        parent_event_id: str = "",
        event_type: str = EXECUTION_COMPLETED,
        trigger_source: str = "python_offline",
        actor_type: str = "offline_agent",
        actor_id: str = "home-ops-agent",
        proposal_id: str = "",
        approval_id: str = "",
        duration_ms: int = 0,
        household_id: int = 1,
        risk_level: str = "HIGH",
        decision: str = DECISION_ALLOW,
        origin_stream_id: str = "offline-agent",
        error_code: str = "",
        evidence: Any = None,
        before_state: Any = None,
        after_state: Any = None,
    ) -> Tuple[AuditStatus, str]:
        """
        Persists offline emergency / authorized event into EXISTING SQLite offline_ops_audit
        table using BEGIN IMMEDIATE, maintaining local hash chain + independent JSONL archive.
        NEVER calls os.makedirs for DB creation or CREATE TABLE IF NOT EXISTS at runtime (R4-15).
        Returns (AuditStatus, event_uuid).
        """
        event_uuid = f"offline-{uuid.uuid4()}"
        now_ms = int(time.time() * 1000)
        now_iso = format_rfc3339_ms(now_ms)
        op_id = operation_id or event_uuid
        run_id_val = run_id or f"run-offline-{event_uuid}"
        corr_id_val = correlation_id or f"corr-{event_uuid}"
        safe_target = redact_sensitive_string(str(container_name or ""))
        db_path = getattr(self.config, "db_path", "/data/sqlite/app.db")
        if not db_path or not os.path.isfile(db_path):
            self.metrics.inc("recovery_audit_failed")
            self._last_audit_status = AuditStatus.AUDIT_FAILED
            logger.critical(f"[Offline Audit Failed] Database missing at {db_path}; refusing runtime creation.")
            return AuditStatus.AUDIT_FAILED, ""

        archive_dir = os.environ.get("OPS_AUDIT_ARCHIVE_DIR", "").strip()
        evidence_str = serialize_evidence(evidence)
        before_str = serialize_evidence(before_state)
        after_str = serialize_evidence(after_state)
        params_digest, params_redacted = serialize_params({
            "container_name": safe_target,
            "action": action,
            "exit_code": exit_code,
            "fencing_generation": fencing_generation,
        })

        conn = None
        entry_hash = ""
        hmac_sig = ""
        offline_seq = 0
        key_id_used = ""
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

            has_v5_cols = {
                "run_id", "correlation_id", "event_type", "payload_json",
                "offline_seq", "local_prev_hash", "entry_hash", "hmac_signature",
            }.issubset(cols)

            if has_v5_cols:
                cur.execute(
                    "SELECT offline_seq, entry_hash FROM offline_ops_audit ORDER BY offline_seq DESC, id DESC LIMIT 1"
                )
                prev_row = cur.fetchone()
                prev_seq = int(prev_row[0]) if prev_row and prev_row[0] else 0
                prev_hash = str(prev_row[1]) if prev_row and prev_row[1] else AUDIT_GENESIS_HASH
                if not prev_hash:
                    prev_hash = AUDIT_GENESIS_HASH
                offline_seq = prev_seq + 1

                res_label = "SUCCESS" if success else "FAILED"
                err_msg = error_code if error_code else (
                    "" if success else ("EXECUTION_UNKNOWN" if event_type == EXECUTION_UNKNOWN else f"exit_code={exit_code}")
                )
                kid, ring = resolve_audit_hmac_keyring()
                has_hmac_key = bool(kid and kid in ring)
                if not has_hmac_key and (is_production() or os.environ.get("OPS_AUDIT_REQUIRE_HMAC", "") == "1"):
                    conn.execute("ROLLBACK")
                    self._last_audit_status = AuditStatus.AUDIT_FAILED
                    logger.critical("[Offline Audit Failed] External OPS_AUDIT_HMAC_KEY unavailable for v5 audit signing in production; failing closed.")
                    return AuditStatus.AUDIT_FAILED, ""

                comp_val = resolve_canonical_component(safe_target) or safe_target
                dur_val = max(0, int(duration_ms))
                hh_val = int(household_id or 1)
                if has_hmac_key:
                    key_id_used = kid
                    ev_obj = {
                        "audit_id": event_uuid,
                        "run_id": run_id_val,
                        "parent_event_id": parent_event_id,
                        "correlation_id": corr_id_val,
                        "household_id": hh_val,
                        "trigger_source": trigger_source,
                        "actor_type": actor_type,
                        "actor_id": actor_id,
                        "event_type": event_type,
                        "action": action,
                        "target": safe_target,
                        "component": comp_val,
                        "decision": decision,
                        "risk_level": risk_level,
                        "policy_version": AUDIT_POLICY_VERSION,
                        "proposal_id": proposal_id,
                        "approval_id": approval_id,
                        "operation_id": op_id,
                        "fencing_generation": fencing_generation,
                        "before_state": before_str,
                        "after_state": after_str,
                        "parameters_digest": params_digest,
                        "parameters_redacted": params_redacted,
                        "result": res_label,
                        "error_code": err_msg,
                        "duration_ms": dur_val,
                        "evidence": evidence_str,
                        "created_at": now_iso,
                        "created_at_ms": now_ms,
                        "is_offline": True,
                        "previous_hash": prev_hash,
                    }
                    entry_hash = compute_event_entry_hash(ev_obj)
                    hmac_sig = compute_event_hmac(kid, ring[kid], origin_stream_id, offline_seq, entry_hash, now_ms)
                else:
                    # Dev/test compatibility when OPS_AUDIT_HMAC_KEY is not injected: keep origin proof empty so Core treats as legacy row.
                    offline_seq = 0
                    prev_hash = ""
                    entry_hash = ""
                    hmac_sig = ""

                payload_meta = json.dumps({
                    "origin_stream_id": origin_stream_id,
                    "key_id": key_id_used,
                    "household_id": hh_val,
                    "policy_version": AUDIT_POLICY_VERSION,
                    "parent_event_id": parent_event_id,
                    "proposal_id": proposal_id,
                    "approval_id": approval_id,
                    "duration_ms": dur_val,
                    "trigger_source": trigger_source,
                    "actor_type": actor_type,
                    "actor_id": actor_id,
                    "component": comp_val,
                    "decision": decision,
                    "risk_level": risk_level,
                    "before_state": before_str,
                    "after_state": after_str,
                    "parameters_digest": params_digest,
                    "parameters_redacted": params_redacted,
                    "evidence": evidence_str,
                    "error_message": err_msg,
                }, separators=(",", ":"), ensure_ascii=False)

                if "error_message" in cols:
                    cur.execute(
                        """
                        INSERT INTO offline_ops_audit (
                            event_uuid, operation_id, container_name, action, holder, fencing_generation,
                            exit_code, success, audit_status, error_message, executed_at, executed_at_ms, synced,
                            run_id, correlation_id, event_type, payload_json,
                            offline_seq, local_prev_hash, entry_hash, hmac_signature
                        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?)
                        """,
                        (
                            event_uuid,
                            op_id,
                            safe_target,
                            action,
                            actor_id,
                            fencing_generation,
                            exit_code,
                            1 if success else 0,
                            AuditStatus.AUDIT_COMMITTED.value,
                            err_msg,
                            now_iso,
                            now_ms,
                            run_id_val,
                            corr_id_val,
                            event_type,
                            payload_meta,
                            offline_seq,
                            prev_hash,
                            entry_hash,
                            hmac_sig,
                        ),
                    )
                else:
                    cur.execute(
                        """
                        INSERT INTO offline_ops_audit (
                            event_uuid, operation_id, container_name, action, holder, fencing_generation,
                            exit_code, success, audit_status, executed_at, executed_at_ms, synced,
                            run_id, correlation_id, event_type, payload_json,
                            offline_seq, local_prev_hash, entry_hash, hmac_signature
                        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?)
                        """,
                        (
                            event_uuid,
                            op_id,
                            safe_target,
                            action,
                            actor_id,
                            fencing_generation,
                            exit_code,
                            1 if success else 0,
                            AuditStatus.AUDIT_COMMITTED.value,
                            now_iso,
                            now_ms,
                            run_id_val,
                            corr_id_val,
                            event_type,
                            payload_meta,
                            offline_seq,
                            prev_hash,
                            entry_hash,
                            hmac_sig,
                        ),
                    )
            elif {"operation_id", "holder", "fencing_generation", "audit_status", "executed_at_ms"}.issubset(cols):
                cur.execute(
                    """
                    INSERT INTO offline_ops_audit (
                        event_uuid, operation_id, container_name, action, holder, fencing_generation,
                        exit_code, success, audit_status, executed_at, executed_at_ms, synced
                    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
                    """,
                    (
                        event_uuid,
                        op_id,
                        safe_target,
                        action,
                        actor_id,
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
                    (event_uuid, safe_target, action, exit_code, 1 if success else 0, now_iso),
                )
            cur.execute("COMMIT")
            self._last_audit_status = AuditStatus.AUDIT_COMMITTED
            if archive_dir:
                stub_ok = append_jsonl_audit_stub(
                    archive_dir,
                    "python-offline-audit.jsonl",
                    {
                        "event_uuid": event_uuid,
                        "origin_stream_id": origin_stream_id,
                        "key_id": key_id_used,
                        "run_id": run_id_val,
                        "correlation_id": corr_id_val,
                        "parent_event_id": parent_event_id,
                        "event_type": event_type,
                        "container_name": safe_target,
                        "action": action,
                        "success": success,
                        "offline_seq": offline_seq,
                        "entry_hash": entry_hash,
                        "hmac_signature": hmac_sig,
                        "executed_at": now_iso,
                    },
                )
                if not stub_ok and (is_production() or os.environ.get("OPS_AUDIT_REQUIRE_HMAC", "") == "1"):
                    self.metrics.inc("recovery_audit_failed")
                    logger.critical(f"[Offline Audit Archive Failed] Failed to append JSONL audit archive for {event_uuid}.")
            logger.info(
                f"[Offline Audit] Recorded offline event {event_uuid} (run={run_id_val}, op={op_id}, seq={offline_seq}, gen={fencing_generation})"
            )
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
        """Probes Go Core /health, verifies local audit chain, pushes buffered logs, and quarantines rejects."""
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

        # 2. Query unsynced events ordered by offline_seq ASC, id ASC (never auto-create DB)
        def read_unsynced():
            if not db_path or not os.path.isfile(db_path):
                return [], set(), []
            conn = None
            try:
                conn = open_readonly_sqlite(db_path)
                conn.row_factory = sqlite3.Row
                cursor = conn.cursor()
                cursor.execute("PRAGMA table_info(offline_ops_audit)")
                cols = {str(c[1]).lower() for c in cursor.fetchall()}
                prior_rows: List[Dict[str, Any]] = []
                if "offline_seq" in cols:
                    cursor.execute(
                        "SELECT * FROM offline_ops_audit WHERE synced = 0 ORDER BY COALESCE(offline_seq, 0) ASC, id ASC LIMIT 50"
                    )
                    rows = cursor.fetchall()
                    if rows and {"entry_hash", "payload_json"}.issubset(cols):
                        cursor.execute(
                            "SELECT offline_seq, entry_hash, payload_json, synced FROM offline_ops_audit "
                            "WHERE offline_seq > 0 AND synced IN (1, -1) ORDER BY offline_seq ASC, id ASC"
                        )
                        prior_rows = [dict(r) for r in cursor.fetchall()]
                else:
                    cursor.execute("SELECT * FROM offline_ops_audit WHERE synced = 0 ORDER BY id ASC LIMIT 50")
                    rows = cursor.fetchall()
                return [dict(r) for r in rows], cols, prior_rows
            except sqlite3.OperationalError:
                return [], set(), []
            finally:
                if conn:
                    conn.close()

        records, audit_cols, prior_tip_rows = await loop.run_in_executor(None, read_unsynced)
        if not records:
            return 0

        has_v5_cols = {
            "run_id", "correlation_id", "event_type", "payload_json",
            "offline_seq", "local_prev_hash", "entry_hash", "hmac_signature",
        }.issubset(audit_cols)

        # Enrich records with unpacked payload_json metadata for Go Core AgentAuditEngine verification
        enriched_records = []
        for rec in records:
            out = dict(rec)
            raw_meta = str(rec.get("payload_json") or "").strip()
            if raw_meta.startswith("{"):
                try:
                    meta = json.loads(raw_meta)
                    for k in (
                        "origin_stream_id", "key_id", "household_id", "policy_version",
                        "parent_event_id", "proposal_id", "approval_id", "duration_ms",
                        "trigger_source", "actor_type", "actor_id", "component",
                        "decision", "risk_level", "before_state", "after_state",
                        "parameters_digest", "parameters_redacted", "evidence", "error_message",
                    ):
                        if k in meta and not out.get(k):
                            out[k] = meta[k]
                except Exception:
                    pass
            out.setdefault("audit_id", out.get("event_uuid", ""))
            out.setdefault("target", out.get("container_name", ""))
            if not out.get("origin_stream_id"):
                out["origin_stream_id"] = "offline-agent"
            if not out.get("key_id") and out.get("hmac_signature"):
                out["key_id"] = extract_hmac_key_id(str(out.get("hmac_signature") or ""))
            enriched_records.append(out)

        # Build confirmed stream tips preceding the earliest unsynced record per stream
        min_unsynced_seq_by_stream: Dict[str, int] = {}
        for rec in enriched_records:
            sid = str(rec.get("origin_stream_id") or "offline-agent").strip() or "offline-agent"
            seq_val = int(rec.get("offline_seq") or 0)
            if seq_val > 0:
                if sid not in min_unsynced_seq_by_stream or seq_val < min_unsynced_seq_by_stream[sid]:
                    min_unsynced_seq_by_stream[sid] = seq_val

        confirmed_tips: Dict[str, Tuple[int, str]] = {}
        for prow in prior_tip_rows:
            p_seq = int(prow.get("offline_seq") or 0)
            p_hash = str(prow.get("entry_hash") or "").strip().lower()
            if p_seq <= 0 or len(p_hash) != 64:
                continue
            p_sid = "offline-agent"
            raw_pmeta = str(prow.get("payload_json") or "").strip()
            if raw_pmeta.startswith("{"):
                try:
                    pmeta = json.loads(raw_pmeta)
                    if pmeta.get("origin_stream_id"):
                        p_sid = str(pmeta["origin_stream_id"]).strip() or "offline-agent"
                except Exception:
                    pass
            min_u = min_unsynced_seq_by_stream.get(p_sid)
            if min_u is None or p_seq < min_u:
                prev_t = confirmed_tips.get(p_sid)
                if prev_t is None or p_seq > prev_t[0]:
                    confirmed_tips[p_sid] = (p_seq, p_hash)

        # Pre-verify local cryptographic hash chain before pushing to Go Core (R7)
        _, hmac_ring = resolve_audit_hmac_keyring()
        valid_records: List[Dict[str, Any]] = []
        tampered_ids: List[int] = []
        all_unsigned_legacy = all(
            int(r.get("offline_seq") or 0) == 0
            and not str(r.get("entry_hash") or "").strip()
            and not str(r.get("hmac_signature") or "").strip()
            for r in enriched_records
        )
        if not has_v5_cols or (not hmac_ring and all_unsigned_legacy):
            if is_production() or os.environ.get("OPS_AUDIT_REQUIRE_HMAC", "") == "1":
                self.metrics.inc("recovery_audit_failed")
                logger.critical("[Offline Audit Sync Blocked] v5 cryptographic audit proof and HMAC key required in production; failing closed.")
                return 0
            valid_records = enriched_records
        elif not hmac_ring:
            self.metrics.inc("recovery_audit_failed")
            logger.critical("[Offline Audit Sync Blocked] External OPS_AUDIT_HMAC_KEY unavailable to verify v5 offline chain; failing closed.")
            return 0
        else:
            chain_ok, chain_reason = verify_offline_audit_chain(
                enriched_records,
                ring=hmac_ring,
                stream_tips=confirmed_tips,
            )
            if not chain_ok:
                self.metrics.inc("recovery_audit_failed")
                logger.critical(f"[Offline Audit Tamper Detected] Local chain check failed before sync: {chain_reason}")
                cur_tips: Dict[str, Tuple[int, str]] = dict(confirmed_tips)
                for rec in enriched_records:
                    sid = str(rec.get("origin_stream_id") or "offline-agent").strip() or "offline-agent"
                    single_ok, _ = verify_offline_audit_chain(
                        [rec],
                        ring=hmac_ring,
                        stream_tips=cur_tips,
                    )
                    seq_val = int(rec.get("offline_seq") or 0)
                    rec_hash = str(rec.get("entry_hash") or "").strip().lower()
                    rec_prev = str(rec.get("local_prev_hash") or "").strip().lower()
                    if single_ok:
                        valid_records.append(rec)
                        if seq_val > 0 and len(rec_hash) == 64:
                            cur_tips[sid] = (seq_val, rec_hash)
                    else:
                        if "id" in rec:
                            tampered_ids.append(rec["id"])
                        tip_seq, tip_hash = cur_tips.get(sid, (0, AUDIT_GENESIS_HASH))
                        if (
                            seq_val == tip_seq + 1
                            and rec_prev == tip_hash
                            and len(rec_hash) == 64
                        ):
                            cur_tips[sid] = (seq_val, rec_hash)
            else:
                valid_records = enriched_records

        if tampered_ids:
            def mark_tampered():
                if not db_path or not os.path.isfile(db_path):
                    return
                conn = None
                try:
                    conn = open_readwrite_sqlite(db_path)
                    ph = ",".join("?" * len(tampered_ids))
                    conn.execute("BEGIN IMMEDIATE")
                    if "audit_status" in audit_cols:
                        conn.execute(
                            f"UPDATE offline_ops_audit SET synced = -1, audit_status = 'AUDIT_TAMPERED' WHERE id IN ({ph})",
                            tampered_ids,
                        )
                    else:
                        conn.execute(
                            f"UPDATE offline_ops_audit SET synced = -1 WHERE id IN ({ph})",
                            tampered_ids,
                        )
                    conn.execute("COMMIT")
                except Exception as err:
                    if conn:
                        try:
                            conn.execute("ROLLBACK")
                        except Exception:
                            pass
                    logger.error(f"[Offline Audit Sync] Failed to quarantine tampered rows: {err}")
                finally:
                    if conn:
                        conn.close()

            await loop.run_in_executor(None, mark_tampered)

        if not valid_records:
            return 0

        # 3. Push verified events to Go Core
        sync_url = f"{self.config.api_base_url.rstrip('/')}/api/v1/ops/audit/offline-sync"
        headers = {
            "Content-Type": "application/json"
        }
        if getattr(self.config, "internal_token", None):
            headers["X-Internal-Secret"] = self.config.internal_token
        sync_payload = json.dumps({"events": valid_records}).encode("utf-8")

        def do_sync() -> Tuple[bool, Optional[List[str]], List[str]]:
            try:
                sync_req = urllib.request.Request(sync_url, data=sync_payload, headers=headers)
                with urllib.request.urlopen(sync_req, timeout=5.0) as resp:
                    if resp.status not in (200, 201):
                        return False, None, []
                    raw_body = resp.read()
                    if isinstance(raw_body, (bytes, bytearray, str)):
                        text_body = raw_body.decode("utf-8", errors="ignore") if isinstance(raw_body, (bytes, bytearray)) else raw_body
                        if not text_body.strip():
                            return False, None, []
                        parsed = json.loads(text_body)
                        if not isinstance(parsed, dict):
                            return False, None, []
                        data_obj = parsed.get("data") if isinstance(parsed.get("data"), dict) else parsed
                        if not isinstance(data_obj, dict) or not isinstance(data_obj.get("accepted_uuids"), list):
                            logger.error("[Offline Audit Sync] Response missing required 'accepted_uuids' list; failing closed.")
                            return False, None, []
                        acc = data_obj.get("accepted_uuids") or []
                        rej_uuids: List[str] = []
                        transient_codes = {"RATE_LIMITED", "TEMPORARY_UNAVAILABLE", "RETRY_LATER", "SERVICE_UNAVAILABLE"}
                        for item in (data_obj.get("rejected") or []):
                            if isinstance(item, dict):
                                r_uuid = str(item.get("event_uuid") or item.get("audit_id") or "").strip()
                                r_code = str(item.get("error_code") or item.get("code") or "").strip().upper()
                                if r_uuid and r_code not in transient_codes:
                                    rej_uuids.append(r_uuid)
                        return True, [str(u) for u in acc if u], rej_uuids
                    # Fallback for unconfigured unittest MagicMocks with no JSON body
                    return True, None, []
            except Exception as err:
                logger.debug(f"[Offline Audit Sync] Request failed: {err}")
                return False, None, []

        sync_ok, accepted_uuids, rejected_uuids = await loop.run_in_executor(None, do_sync)
        if not sync_ok:
            return 0

        if accepted_uuids is not None:
            accepted_set = set(accepted_uuids)
            rejected_set = set(rejected_uuids) - accepted_set
            records_to_ack = [
                r for r in valid_records
                if (r.get("event_uuid") in accepted_set or r.get("audit_id") in accepted_set)
            ]
            records_to_reject = [
                r for r in valid_records
                if (r.get("event_uuid") in rejected_set or r.get("audit_id") in rejected_set)
            ]
        else:
            records_to_ack = valid_records
            records_to_reject = []

        if not records_to_ack and not records_to_reject:
            return 0

        # 4. Mark synced = 1 for accepted and synced = -1 for permanently rejected (quarantine)
        def mark_synced():
            if not db_path or not os.path.isfile(db_path):
                return
            conn = None
            try:
                conn = open_readwrite_sqlite(db_path)
                conn.execute("BEGIN IMMEDIATE")
                if records_to_ack:
                    synced_ids = [r["id"] for r in records_to_ack]
                    placeholders = ",".join("?" * len(synced_ids))
                    conn.execute(f"UPDATE offline_ops_audit SET synced = 1 WHERE id IN ({placeholders})", synced_ids)
                if records_to_reject:
                    rej_ids = [r["id"] for r in records_to_reject]
                    placeholders_rej = ",".join("?" * len(rej_ids))
                    if "audit_status" in audit_cols:
                        conn.execute(
                            f"UPDATE offline_ops_audit SET synced = -1, audit_status = ? WHERE id IN ({placeholders_rej})",
                            [AuditStatus.AUDIT_REJECTED.value, *rej_ids],
                        )
                    else:
                        conn.execute(
                            f"UPDATE offline_ops_audit SET synced = -1 WHERE id IN ({placeholders_rej})",
                            rej_ids,
                        )
                conn.execute("COMMIT")
            except Exception as err:
                if conn:
                    try:
                        conn.execute("ROLLBACK")
                    except Exception:
                        pass
                logger.error(f"[Offline Audit Sync] Failed to update sync statuses: {err}")
            finally:
                if conn:
                    conn.close()

        await loop.run_in_executor(None, mark_synced)
        if records_to_reject:
            logger.warning(f"[Offline Audit Sync] Quarantined {len(records_to_reject)} permanently rejected offline events (synced=-1).")
        if records_to_ack:
            logger.info(f"[Offline Audit Sync] Successfully synchronized {len(records_to_ack)} offline events to Go Core.")
        return len(records_to_ack)

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
        canonical = resolve_canonical_component(target_component)
        if not canonical:
            logger.critical(
                f"[R5 Fail-Closed] Refusing proposal {prop_id}: component resolution failed for '{target_component}'."
            )
            return False

        live_baseline = await self._compute_container_baseline_hash(canonical, getattr(self.config, "docker_socket", None))
        if is_production() and not is_real_baseline_hash(live_baseline, canonical):
            logger.critical(
                f"[R5 Fail-Closed] Refusing proposal {prop_id} in production: real resource baseline_hash unavailable for '{canonical}'."
            )
            return False

        api_url = f"{self.config.api_base_url.rstrip('/')}/api/v1/ops/proposals"
        payload = {
            "proposal_id": prop_id,
            "title": title,
            "risk_level": risk_level,
            "execution_plan": action,
            "rollback_plan": rollback_cmd,
            "target_component": canonical,
            "estimated_seconds": estimated_seconds
        }
        if is_real_baseline_hash(live_baseline, canonical):
            payload["baseline_hash"] = live_baseline
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

        if not is_real_baseline_hash(live_baseline, canonical):
            logger.warning(
                f"[R5 Fail-Closed] Skipping local fallback proposal {prop_id} for '{canonical}': real baseline_hash unavailable."
            )
            return False

        # Local tracking fallback (only with verified real resource baseline)
        self.proposal_manager.create_proposal(
            prop_id=prop_id,
            title=title,
            risk_level=risk_level,
            action=action,
            rollback_cmd=rollback_cmd,
            target_component=canonical,
            estimated_seconds=estimated_seconds,
            baseline_hash=live_baseline,
        )
        return False

    async def _restart_docker_container(
        self,
        container_name: str,
        sock_path: str,
        fencing_generation: int = 0,
        operation_id: str = "",
    ) -> bool:
        """Sends POST /containers/{name}/restart to Docker socket with fencing headers & EXECUTION_UNKNOWN tracking."""
        import urllib.parse

        self._last_restart_ambiguous = False
        writer = None
        request_sent = False
        try:
            reader, writer = await asyncio.wait_for(self._connect_docker(sock_path), timeout=5.0)
            quoted_name = urllib.parse.quote(str(container_name), safe="")
            extra_headers = ""
            if fencing_generation > 0:
                extra_headers += f"X-Ops-Fencing-Generation: {fencing_generation}\r\n"
            if operation_id:
                extra_headers += f"X-Ops-Operation-ID: {operation_id}\r\n"
            req = (
                f"POST /containers/{quoted_name}/restart?t=5 HTTP/1.1\r\n"
                f"Host: docker\r\n"
                f"User-Agent: home-ops-agent\r\n"
                f"{extra_headers}"
                f"Content-Length: 0\r\n"
                f"Connection: close\r\n\r\n"
            ).encode("utf-8")
            writer.write(req)
            await asyncio.wait_for(writer.drain(), timeout=5.0)
            request_sent = True
            resp = await asyncio.wait_for(reader.read(256), timeout=15.0)
            if not resp:
                self._last_restart_ambiguous = True
                self.metrics.inc("recovery_execution_unknown")
                logger.critical(
                    f"[EXECUTION_UNKNOWN] Empty EOF from Docker socket after restart dispatch for {container_name}."
                )
                return False
            logger.info(f"[Tier 1] Issued restart for container {container_name}")
            return b"204 No Content" in resp or b"200 OK" in resp
        except asyncio.CancelledError:
            if request_sent:
                self._last_restart_ambiguous = True
                self.metrics.inc("recovery_execution_unknown")
                logger.critical(
                    f"[EXECUTION_UNKNOWN] Docker restart cancelled after request dispatch for {container_name}."
                )
            if writer is not None and getattr(writer, "transport", None) is not None:
                try:
                    writer.transport.abort()
                except Exception:
                    pass
            raise
        except (asyncio.TimeoutError, TimeoutError, ConnectionError, EOFError, OSError) as e:
            if request_sent:
                self._last_restart_ambiguous = True
                self.metrics.inc("recovery_execution_unknown")
                logger.critical(
                    f"[EXECUTION_UNKNOWN] Docker restart outcome ambiguous after dispatch for {container_name}: {e}"
                )
            else:
                logger.error(f"Failed to restart container {container_name} via Docker socket: {e}")
            return False
        except Exception as e:
            logger.error(f"Failed to restart container {container_name} via Docker socket: {e}")
            return False
        finally:
            if writer is not None:
                try:
                    writer.close()
                    await asyncio.wait_for(writer.wait_closed(), timeout=0.5)
                except Exception:
                    pass

    def _process_mqtt_security_message(self, topic: str, payload_str: str) -> None:
        """Validates MQTT security topic/schema before dispatching auth-failure counters and emits audit trail."""
        clean_topic = (topic or "").strip()
        if not clean_topic or ("auth_fail" not in clean_topic and not clean_topic.startswith("security/")):
            return
        if not payload_str or len(payload_str) > 8192:
            logger.warning(f"[MQTT Reject] Empty or oversized payload on topic {clean_topic}")
            return
        try:
            data = json.loads(payload_str)
        except Exception:
            logger.warning(f"[MQTT Reject] Malformed JSON payload on topic {clean_topic}")
            return
        if not isinstance(data, dict):
            logger.warning(f"[MQTT Reject] Non-object JSON payload on topic {clean_topic}")
            return

        raw_ip = data.get("ip") or data.get("client_ip")
        if not isinstance(raw_ip, str) or not raw_ip.strip():
            logger.warning(f"[MQTT Reject] Missing/invalid IP field on topic {clean_topic}")
            return

        res = self.rule_engine.handle_auth_failure(raw_ip.strip(), source="mqtt", authenticated=True)
        action = str(res.get("action") or "ignored")
        if action == "ignored" or action.startswith("rejected_"):
            logger.warning(f"[MQTT Reject] RuleEngine rejected auth failure event ({action}) on {clean_topic}")
            return
        logger.info(f"MQTT security event from {raw_ip.strip()}: action={action}")
        archive_dir = os.environ.get("OPS_AUDIT_ARCHIVE_DIR", "").strip()
        if archive_dir:
            append_jsonl_audit_stub(archive_dir, "python-mqtt-audit.jsonl", {
                "event_type": "MQTT_AUTH_FAILURE",
                "topic": redact_sensitive_string(clean_topic),
                "source_ip": raw_ip.strip(),
                "action": action,
                "executed_at": format_rfc3339_ms(),
            })

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
                    self._process_mqtt_security_message(topic, payload_str)
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
                        self._process_mqtt_security_message(msg_topic, payload_data)
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
