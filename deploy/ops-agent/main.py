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
    cleaned = (name or "").strip().lower().lstrip("/")
    if "(" in cleaned:
        cleaned = cleaned.split("(")[0].strip()
    return CANONICAL_COMPONENT_MAP.get(cleaned, cleaned)

def resolve_docker_container_name(canonical: str) -> str:
    cleaned = resolve_canonical_component(canonical).strip().lower()
    return CANONICAL_TO_DOCKER_CONTAINER.get(cleaned, canonical)

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
    return sqlite3.connect(uri, uri=True, timeout=2.0)

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
    """Returns True if running in production mode."""
    env = os.environ.get("ENVIRONMENT", os.environ.get("APP_ENV", os.environ.get("ENV", ""))).lower().strip()
    if env in ("development", "dev", "test", "testing"):
        return False
    if env in ("production", "prod"):
        return True
    return False


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
                                logger.info(f"[Authoritative] Go Core approved restart for {container_name}. Executing restart.")
                                await self._restart_docker_container(container_name, sock_path)
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

        # Guard 2e: Dual-Path Execution Lease Mutual Exclusion (R13)
        lease_acquired, lease_token = self._acquire_recovery_lease(canonical, db_path)
        if not lease_acquired:
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
                    f"[Emergency Autonomy Blocked] Persisted restart count is UNKNOWN (query error). Halting restart."
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

            timestamps.append(now)
            logger.warning(
                f"[Emergency Autonomy Triggered] Go Core OFFLINE. Executing offline emergency restart for {container_name} "
                f"(attempt {len(timestamps)} in 10m window)"
            )
            docker_container = resolve_docker_container_name(canonical)
            success = await self._restart_docker_container(docker_container, sock_path)
            if success:
                health_state = await self._verify_container_health(docker_container, sock_path)
                if health_state != DockerHealthState.HEALTHY:
                    logger.error(f"[Emergency Recovery Failed] Post-restart health check returned {health_state} for {container_name}.")
                    success = False

            self._record_offline_audit(container_name, "emergency_restart", exit_code, success)
            return success
        finally:
            self._release_recovery_lease(canonical, lease_token, db_path)

    def _is_maintenance_locked_offline(self, db_path: str, container_name: Optional[str] = None) -> MaintenanceLockState:
        """
        Checks if system or component maintenance lock is active in local SQLite.
        Returns MaintenanceLockState: LOCKED, UNLOCKED, or UNKNOWN.
        Fails closed: missing DB, corrupted DB, missing table, incomplete columns -> UNKNOWN.
        """
        if not db_path or not os.path.isfile(db_path):
            logger.error(f"[Fail-Closed] SQLite database file missing at {db_path}. Maintenance lock state UNKNOWN.")
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

            has_component = "component" in cols or "target_component" in cols
            comp_col = "component" if "component" in cols else ("target_component" if "target_component" in cols else "")

            if has_component and container_name:
                canonical = resolve_canonical_component(container_name)
                cursor.execute(
                    f"SELECT {active_col}, expires_at FROM ops_maintenance_locks "
                    f"WHERE {comp_col} IS NULL OR {comp_col} = '' OR {comp_col} = '*' "
                    f"OR {comp_col} = ? OR {comp_col} = ? ORDER BY id DESC LIMIT 1",
                    (canonical, container_name)
                )
            else:
                cursor.execute(f"SELECT {active_col}, expires_at FROM ops_maintenance_locks ORDER BY id DESC LIMIT 1")

            row = cursor.fetchone()
            if not row:
                return MaintenanceLockState.UNLOCKED

            is_active = bool(row[0])
            if not is_active:
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
        Fails closed: missing DB, corrupted DB, missing table, incomplete columns -> UNKNOWN.
        """
        if not db_path or not os.path.isfile(db_path):
            logger.error(f"[Fail-Closed] SQLite database file missing at {db_path}. Circuit breaker state UNKNOWN.")
            return CircuitBreakerState.UNKNOWN
        canonical = resolve_canonical_component(container_name)
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

            cursor.execute(
                f"SELECT state FROM ops_circuit_breaker_states WHERE {container_col} = ? OR {container_col} = ? LIMIT 1",
                (canonical, container_name)
            )
            row = cursor.fetchone()
            if not row:
                return CircuitBreakerState.CLOSED

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
        Returns None if DB is missing, corrupted, or query fails (Fail-Closed).
        DO NOT auto-create empty DB file.
        """
        if not db_path or not os.path.isfile(db_path):
            logger.error(f"[Fail-Closed] DB file missing at {db_path}. Persisted restart count UNKNOWN.")
            return None
        canonical = resolve_canonical_component(container_name)
        conn = None
        try:
            conn = open_readonly_sqlite(db_path)
            cursor = conn.cursor()
            cursor.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='offline_restart_history'")
            if not cursor.fetchone():
                return 0
            cutoff = time.time() - window_seconds
            cursor.execute(
                "SELECT COUNT(*) FROM offline_restart_history WHERE (container_name = ? OR container_name = ?) AND restart_time >= ?",
                (canonical, container_name, cutoff)
            )
            row = cursor.fetchone()
            return row[0] if row else 0
        except Exception as e:
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
        Records restart timestamp into SQLite offline_restart_history.
        Returns True on success, False on failure.
        DO NOT auto-create empty DB file if db_path does not exist.
        """
        if not db_path or not os.path.isfile(db_path):
            logger.error(f"[Fail-Closed] Cannot record restart: DB file {db_path} does not exist.")
            return False
        canonical = resolve_canonical_component(container_name)
        conn = None
        try:
            conn = sqlite3.connect(db_path, timeout=5.0)
            with conn:
                conn.execute("""
                    CREATE TABLE IF NOT EXISTS offline_restart_history (
                        id INTEGER PRIMARY KEY AUTOINCREMENT,
                        container_name TEXT NOT NULL,
                        restart_time REAL NOT NULL
                    )
                """)
                conn.execute(
                    "INSERT INTO offline_restart_history (container_name, restart_time) VALUES (?, ?)",
                    (canonical, time.time())
                )
            return True
        except Exception as e:
            logger.error(f"[Fail-Closed] Failed to record offline restart timestamp: {e}")
            return False
        finally:
            if conn:
                try:
                    conn.close()
                except Exception:
                    pass

    def _acquire_recovery_lease(self, component: str, db_path: str, lease_duration: float = 30.0) -> Tuple[bool, str]:
        """
        Attempts to atomically acquire an exclusive persistent recovery lease in SQLite.
        Prevents dual-path race conditions between Go Core and Python Offline Emergency.
        Returns (acquired: bool, lease_token: str).
        """
        if not db_path or not os.path.isfile(db_path):
            return False, ""
        canonical = resolve_canonical_component(component)
        token = f"py-{uuid.uuid4()}"
        now_epoch = time.time()
        now_iso = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now_epoch))
        exp_iso = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now_epoch + lease_duration))
        conn = None
        try:
            conn = sqlite3.connect(db_path, timeout=5.0)
            with conn:
                conn.execute("""
                    CREATE TABLE IF NOT EXISTS ops_recovery_leases (
                        component TEXT PRIMARY KEY,
                        holder TEXT NOT NULL,
                        lease_token TEXT NOT NULL,
                        acquired_at DATETIME NOT NULL,
                        expires_at DATETIME NOT NULL
                    )
                """)
                cursor = conn.cursor()
                cursor.execute("SELECT holder, expires_at FROM ops_recovery_leases WHERE component = ?", (canonical,))
                row = cursor.fetchone()
                if row:
                    holder, exp_raw = row[0], row[1]
                    exp_ts, valid = parse_sqlite_timestamp(exp_raw)
                    if not valid or (exp_ts is not None and exp_ts > now_epoch and holder != "python-offline"):
                        logger.warning(f"[Lease Conflict] Recovery lease for {canonical} held by '{holder}' until {exp_raw}.")
                        return False, ""
                    cursor.execute("""
                        UPDATE ops_recovery_leases
                        SET holder = 'python-offline', lease_token = ?, acquired_at = ?, expires_at = ?
                        WHERE component = ? AND (holder = 'python-offline' OR expires_at = ?)
                    """, (token, now_iso, exp_iso, canonical, exp_raw))
                    if cursor.rowcount == 0:
                        return False, ""
                    return True, token
                else:
                    try:
                        conn.execute("""
                            INSERT INTO ops_recovery_leases (component, holder, lease_token, acquired_at, expires_at)
                            VALUES (?, 'python-offline', ?, ?, ?)
                        """, (canonical, token, now_iso, exp_iso))
                        return True, token
                    except sqlite3.IntegrityError:
                        return False, ""
        except Exception as e:
            logger.error(f"[Fail-Closed] Failed to acquire recovery lease for {canonical}: {e}")
            return False, ""
        finally:
            if conn:
                try:
                    conn.close()
                except Exception:
                    pass

    def _release_recovery_lease(self, component: str, lease_token: str, db_path: str) -> bool:
        """Releases recovery lease held by this agent. Returns True if released, False otherwise."""
        if not db_path or not lease_token or not os.path.isfile(db_path):
            return False
        canonical = resolve_canonical_component(component)
        conn = None
        try:
            conn = sqlite3.connect(db_path, timeout=5.0)
            with conn:
                cursor = conn.cursor()
                cursor.execute(
                    "DELETE FROM ops_recovery_leases WHERE component = ? AND lease_token = ?",
                    (canonical, lease_token)
                )
                return cursor.rowcount > 0
        except Exception as e:
            logger.debug(f"Failed to release recovery lease for {canonical}: {e}")
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
        Executes independent TCP/HTTP readiness probes for services without Docker HEALTHCHECK.
        """
        if hasattr(self, "_readiness_probe_override") and callable(self._readiness_probe_override):
            return await self._readiness_probe_override(canonical)

        probe_host = os.environ.get("PROBE_HOST", "127.0.0.1")

        async def probe_tcp(host: str, port: int, timeout: float = 2.0) -> bool:
            try:
                _, writer = await asyncio.wait_for(asyncio.open_connection(host, port), timeout=timeout)
                writer.close()
                await writer.wait_closed()
                return True
            except Exception:
                return False

        async def probe_http(host: str, port: int, path: str = "/", timeout: float = 2.0) -> bool:
            try:
                reader, writer = await asyncio.wait_for(asyncio.open_connection(host, port), timeout=timeout)
                req = f"GET {path} HTTP/1.1\r\nHost: {host}:{port}\r\nUser-Agent: home-ops-agent-probe\r\nConnection: close\r\n\r\n".encode("utf-8")
                writer.write(req)
                await writer.drain()
                resp = await asyncio.wait_for(reader.read(1024), timeout=timeout)
                writer.close()
                await writer.wait_closed()
                return (
                    b"200 OK" in resp or b"HTTP/1.1 200" in resp or b"HTTP/1.0 200" in resp or
                    b"HTTP/1.1 204" in resp or b"HTTP/1.1 3" in resp
                )
            except Exception:
                return False

        if canonical == "mosquitto":
            return await probe_tcp(probe_host, 1883)
        elif canonical == "frigate":
            ok = await probe_http(probe_host, 5000, "/")
            if not ok:
                ok = await probe_http(probe_host, 1984, "/")
            return ok
        elif canonical == "home-api":
            return await probe_http(probe_host, 8080, "/health")
        elif canonical == "home-web":
            ok = await probe_http(probe_host, 80, "/")
            if not ok:
                ok = await probe_http(probe_host, 5173, "/")
            return ok
        elif canonical == "home-vision":
            return await probe_http(probe_host, 8090, "/health")
        elif canonical == "openresty":
            return await probe_tcp(probe_host, 80)
        else:
            logger.warning(f"No independent readiness probe defined for component '{canonical}'.")
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

    def _record_offline_audit(self, container_name: str, action: str, exit_code: int, success: bool) -> str:
        """Persists offline emergency event into local SQLite offline_ops_audit table with unique UUID."""
        event_uuid = f"offline-{uuid.uuid4()}"
        now_iso = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        db_path = getattr(self.config, "db_path", "/data/sqlite/app.db")
        conn = None
        try:
            parent_dir = os.path.dirname(os.path.abspath(db_path))
            if parent_dir:
                os.makedirs(parent_dir, exist_ok=True)
            conn = sqlite3.connect(db_path)
            with conn:
                conn.execute("""
                    CREATE TABLE IF NOT EXISTS offline_ops_audit (
                        id INTEGER PRIMARY KEY AUTOINCREMENT,
                        event_uuid TEXT UNIQUE,
                        container_name TEXT NOT NULL,
                        action TEXT NOT NULL,
                        exit_code INTEGER,
                        success INTEGER,
                        executed_at TEXT NOT NULL,
                        synced INTEGER DEFAULT 0
                    )
                """)
                conn.execute("""
                    INSERT INTO offline_ops_audit (event_uuid, container_name, action, exit_code, success, executed_at, synced)
                    VALUES (?, ?, ?, ?, ?, ?, 0)
                """, (event_uuid, container_name, action, exit_code, 1 if success else 0, now_iso))
            logger.info(f"[Offline Audit] Recorded offline event {event_uuid} for container {container_name}")
            return event_uuid
        except Exception as e:
            logger.error(f"[Offline Audit] Failed to persist offline log: {e}")
            return ""
        finally:
            if conn:
                try:
                    conn.close()
                except Exception:
                    pass

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

        # 2. Query unsynced events from SQLite
        def read_unsynced():
            conn = sqlite3.connect(db_path)
            conn.row_factory = sqlite3.Row
            try:
                cursor = conn.cursor()
                cursor.execute("SELECT * FROM offline_ops_audit WHERE synced = 0 ORDER BY id ASC LIMIT 50")
                rows = cursor.fetchall()
                return [dict(r) for r in rows]
            except sqlite3.OperationalError:
                return []
            finally:
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

        # 4. Mark synced = 1
        def mark_synced():
            conn = sqlite3.connect(db_path)
            try:
                synced_ids = [r["id"] for r in records]
                placeholders = ",".join("?" * len(synced_ids))
                with conn:
                    conn.execute(f"UPDATE offline_ops_audit SET synced = 1 WHERE id IN ({placeholders})", synced_ids)
            finally:
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
                        return CoreReportingStatus.AUTHORIZED, resp_json

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
            "action": action,
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
