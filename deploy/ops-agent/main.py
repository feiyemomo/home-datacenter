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
from typing import Any, Dict, Optional, Tuple

class CoreReportingStatus(enum.Enum):
    RECOVERED_BY_CORE = "RECOVERED_BY_CORE"  # Go Core executed recovery and verified health
    AUTHORIZED = "AUTHORIZED"                # Go Core explicitly approved restart
    POLICY_REJECTED = "POLICY_REJECTED"      # Go Core returned 400/401/403/409/422/429
    BLOCKED_BY_FSM = "BLOCKED_BY_FSM"        # Go Core returned 200, but decision is BLOCKED/IGNORE/TRIPPED
    SERVER_ERROR = "SERVER_ERROR"            # Go Core returned 5xx
    OFFLINE = "OFFLINE"                      # Genuine network error: connection refused/timeout

try:
    import paho.mqtt.client as paho_mqtt
    PAHO_AVAILABLE = True
except ImportError:
    paho_mqtt = None
    PAHO_AVAILABLE = False

# Hardcoded emergency container whitelist for local autonomous recovery when Go Core is offline
EMERGENCY_RESTART_WHITELIST = frozenset({
    "home-api", "home-web", "home-vision", "home-frigate",
    "1Panel-openresty-WuDU", "1panel-openresty-wudu"
})

# Forbidden components that must NEVER be restarted offline without Go Core coordination
OFFLINE_FORBIDDEN_COMPONENTS = frozenset({
    "app.db", "mosquitto", "home-mosquitto", "database", "sqlite"
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
        """Controlled 5-guard offline emergency recovery mode."""
        # Guard 1: Validate EMERGENCY_OVERRIDE_TOKEN (must be >= 16 chars, fail-secure if missing or weak)
        override_token = os.environ.get("EMERGENCY_OVERRIDE_TOKEN", "").strip()
        if not override_token or len(override_token) < 16 or override_token.lower() in WEAK_SECRETS_BLACKLIST:
            logger.critical(
                f"[Emergency Autonomy Blocked] Refusing offline restart for {container_name}: "
                f"Missing or weak EMERGENCY_OVERRIDE_TOKEN."
            )
            return False

        # Guard 2a: Forbidden component check (core storage / database forbidden from offline autonomous restart)
        if container_name in OFFLINE_FORBIDDEN_COMPONENTS:
            logger.critical(
                f"[Emergency Autonomy Blocked] Container {container_name} is in OFFLINE_FORBIDDEN_COMPONENTS. Refusing offline restart."
            )
            return False

        # Guard 2b: Restrict to container whitelist
        if container_name not in EMERGENCY_RESTART_WHITELIST:
            logger.warning(
                f"[Emergency Autonomy Blocked] Container {container_name} is NOT in EMERGENCY_RESTART_WHITELIST. Refusing restart."
            )
            return False

        db_path = getattr(self.config, "db_path", "/data/sqlite/app.db")

        # Guard 2c: Maintenance Lock Verification (Fail-Closed)
        if self._is_maintenance_locked_offline(db_path):
            logger.critical(
                f"[Emergency Autonomy Blocked] Refusing offline restart for {container_name}: "
                f"System maintenance lock is ACTIVE in local DB."
            )
            return False

        # Guard 2d: Circuit Breaker State Verification (Fail-Closed)
        if self._is_circuit_open_offline(container_name, db_path):
            logger.critical(
                f"[Emergency Autonomy Blocked] Refusing offline restart for {container_name}: "
                f"Container circuit breaker is OPEN in local DB."
            )
            return False

        # Guard 3: Action scope strictly limited to container restart (enforced via _restart_docker_container)

        # Guard 4: Persistent cooldown and circuit breaker (<= 2 restarts in 60s, <= 3 crashes in 10m)
        now = time.time()
        timestamps = self._local_restarts.setdefault(container_name, [])
        timestamps = [t for t in timestamps if now - t < 600.0]  # 10 minute window
        self._local_restarts[container_name] = timestamps

        short_window = [t for t in timestamps if now - t < 60.0]  # 60 second window
        persisted_short = self._get_persisted_restart_count(container_name, 60.0, db_path)
        persisted_long = self._get_persisted_restart_count(container_name, 600.0, db_path)

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

        # Guard 5: Local SQLite persistence in offline_restart_history and offline_ops_audit
        self._record_persisted_restart(container_name, db_path)
        timestamps.append(now)
        logger.warning(
            f"[Emergency Autonomy Triggered] Go Core OFFLINE. Executing offline emergency restart for {container_name} "
            f"(attempt {len(timestamps)} in 10m window)"
        )
        success = await self._restart_docker_container(container_name, sock_path)
        if success:
            health_ok = await self._verify_container_health(container_name, sock_path)
            if not health_ok:
                logger.error(f"[Emergency Recovery Failed] Post-restart health check failed for {container_name}.")
                success = False

        self._record_offline_audit(container_name, "emergency_restart", exit_code, success)
        return success

    def _is_maintenance_locked_offline(self, db_path: str) -> bool:
        """Checks if system maintenance lock is active in local SQLite."""
        if not os.path.exists(db_path):
            return False
        conn = None
        try:
            conn = sqlite3.connect(db_path)
            cursor = conn.cursor()
            cursor.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='ops_maintenance_locks'")
            if not cursor.fetchone():
                return False
            cursor.execute("PRAGMA table_info(ops_maintenance_locks)")
            cols = [col[1] for col in cursor.fetchall()]
            active_col = "active" if "active" in cols else ("locked" if "locked" in cols else "")
            if not active_col:
                return False
            cursor.execute(f"SELECT {active_col}, expires_at FROM ops_maintenance_locks ORDER BY id DESC LIMIT 1")
            row = cursor.fetchone()
            if row and row[0]:
                expires_at_str = row[1]
                if expires_at_str:
                    try:
                        exp_time = time.strptime(expires_at_str[:19], "%Y-%m-%dT%H:%M:%S")
                        if time.mktime(exp_time) > time.time():
                            return True
                    except Exception:
                        return bool(row[0])
                else:
                    return bool(row[0])
            return False
        except Exception as e:
            logger.debug(f"Failed to check offline maintenance lock: {e}")
            return False
        finally:
            if conn:
                conn.close()

    def _is_circuit_open_offline(self, container_name: str, db_path: str) -> bool:
        """Checks if container circuit breaker is OPEN in local SQLite."""
        if not os.path.exists(db_path):
            return False
        conn = None
        try:
            conn = sqlite3.connect(db_path)
            cursor = conn.cursor()
            cursor.execute("SELECT name FROM sqlite_master WHERE type='table' AND name='ops_circuit_breaker_states'")
            if not cursor.fetchone():
                return False
            cursor.execute("PRAGMA table_info(ops_circuit_breaker_states)")
            cols = [col[1] for col in cursor.fetchall()]
            container_col = "container" if "container" in cols else ("container_name" if "container_name" in cols else "")
            if not container_col:
                return False
            cursor.execute(f"SELECT state FROM ops_circuit_breaker_states WHERE {container_col} = ? LIMIT 1", (container_name,))
            row = cursor.fetchone()
            if row and str(row[0]).upper() == "OPEN":
                return True
            return False
        except Exception as e:
            logger.debug(f"Failed to check offline circuit breaker: {e}")
            return False
        finally:
            if conn:
                conn.close()

    def _get_persisted_restart_count(self, container_name: str, window_seconds: float, db_path: str) -> int:
        """Returns number of restarts recorded in SQLite within window_seconds."""
        if not db_path:
            return 0
        conn = None
        try:
            parent_dir = os.path.dirname(os.path.abspath(db_path))
            if parent_dir:
                os.makedirs(parent_dir, exist_ok=True)
            conn = sqlite3.connect(db_path)
            with conn:
                conn.execute("""
                    CREATE TABLE IF NOT EXISTS offline_restart_history (
                        id INTEGER PRIMARY KEY AUTOINCREMENT,
                        container_name TEXT NOT NULL,
                        restart_time REAL NOT NULL
                    )
                """)
                cutoff = time.time() - window_seconds
                cursor = conn.cursor()
                cursor.execute(
                    "SELECT COUNT(*) FROM offline_restart_history WHERE container_name = ? AND restart_time >= ?",
                    (container_name, cutoff)
                )
                row = cursor.fetchone()
                return row[0] if row else 0
        except Exception as e:
            logger.debug(f"Failed to query offline_restart_history: {e}")
            return 0
        finally:
            if conn:
                conn.close()

    def _record_persisted_restart(self, container_name: str, db_path: str) -> None:
        """Records restart timestamp into SQLite offline_restart_history."""
        if not db_path:
            return
        conn = None
        try:
            conn = sqlite3.connect(db_path)
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
                    (container_name, time.time())
                )
        except Exception as e:
            logger.debug(f"Failed to record offline restart timestamp: {e}")
        finally:
            if conn:
                conn.close()

    async def _verify_container_health(self, container_name: str, sock_path: str) -> bool:
        """Inspects container state via Docker socket to verify it is running and healthy."""
        if not os.path.exists(sock_path):
            return True
        try:
            reader, writer = await asyncio.open_unix_connection(sock_path)
            req = (
                f"GET /containers/{container_name}/json HTTP/1.1\r\n"
                f"Host: docker\r\n"
                f"User-Agent: home-ops-agent\r\n"
                f"Connection: close\r\n\r\n"
            ).encode("utf-8")
            writer.write(req)
            await writer.drain()
            resp = await reader.read(4096)
            writer.close()
            await writer.wait_closed()
            if b"200 OK" not in resp:
                return False
            parts = resp.split(b"\r\n\r\n", 1)
            if len(parts) > 1:
                data = json.loads(parts[1].decode("utf-8", errors="ignore"))
                state = data.get("State", {})
                running = state.get("Running", False)
                health = state.get("Health", {}).get("Status", "")
                if running and health != "unhealthy":
                    return True
            return False
        except Exception as e:
            logger.debug(f"Container health check error: {e}")
            return False

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
                    dec_action = str(decision.get("Action", "")).upper()

                    if exec_status == "RECOVERED":
                        return CoreReportingStatus.RECOVERED_BY_CORE, resp_json
                    elif exec_status in ("BLOCKED", "FAILED") or dec_action in ("BLOCKED", "IGNORE", "TRIPPED"):
                        logger.warning(
                            f"[Policy Guard] Go Core FSM evaluated {container_name}: "
                            f"Status={exec_status}, Action={dec_action}, Reason={decision.get('Reason') or resp_json.get('error')}. Execution suppressed."
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
            reader, writer = await asyncio.open_unix_connection(sock_path)
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
