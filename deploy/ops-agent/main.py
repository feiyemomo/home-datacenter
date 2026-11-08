"""
home-ops-agent — Autonomous Edge Operations Daemon.
Python 3.11 asynchronous daemon with strict <= 50MB resident RAM constraint.
"""

import asyncio
import json
import logging
import os
import shutil
import signal
import sys
import time
from typing import Any, Optional

try:
    import paho.mqtt.client as paho_mqtt
    PAHO_AVAILABLE = True
except ImportError:
    paho_mqtt = None
    PAHO_AVAILABLE = False

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
        self.proposal_manager = ProposalLifecycleManager(self.config.db_path)
        self.running = False
        self._tasks: list[asyncio.Task] = []
        self._mqtt_client: Optional[Any] = None
        self._mqtt_queue: asyncio.Queue = asyncio.Queue()
        self._docker_event_queue: asyncio.Queue = asyncio.Queue(maxsize=500)
        self._last_docker_event_time: int = int(time.time())
        self._mqtt_connected: bool = False

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
            asyncio.create_task(self._scheduled_proposals_loop(), name="scheduled_proposals"),
            asyncio.create_task(self._memory_watchdog_loop(), name="memory_watchdog"),
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
                            recovered = await self._report_event_to_go_core(container_name, exit_code, action, stderr_snippet)
                            if not recovered:
                                # 2. Fallback to emergency local socket restart if Go Core is unreachable
                                logger.info(f"[Emergency Autonomy] Go Core unreachable or offline. Executing direct socket restart for {container_name}")
                                await self._restart_docker_container(container_name, sock_path)

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

    async def _report_event_to_go_core(self, container_name: str, exit_code: int, action: str, error_msg: str) -> bool:
        """Reports crash event to Go Active Core API for authoritative policy verification and execution."""
        api_url = f"{self.config.api_base_url.rstrip('/')}/api/v1/ops/fsm/event"
        payload = {
            "component": container_name,
            "container_name": container_name,
            "action": action,
            "exit_code": exit_code,
            "oom_killed": (action == "oom" or exit_code == 137),
            "error_message": error_msg,
            "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        }
        try:
            req_data = json.dumps(payload).encode("utf-8")
            req = urllib.request.Request(api_url, data=req_data, headers={"Content-Type": "application/json"})
            loop = asyncio.get_running_loop()
            def do_req():
                with urllib.request.urlopen(req, timeout=3) as resp:
                    return resp.status in (200, 201)
            return await loop.run_in_executor(None, do_req)
        except Exception as e:
            logger.debug(f"Report event to Go Core failed ({api_url}): {e}")
            return False

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
        try:
            req_data = json.dumps(payload).encode("utf-8")
            req = urllib.request.Request(api_url, data=req_data, headers={"Content-Type": "application/json"})
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

    async def _scheduled_proposals_loop(self) -> None:
        """Checks for and executes approved low-peak proposals (every 60s)."""
        while self.running:
            try:
                if self.proposal_manager.is_low_peak_window():
                    executed = self.proposal_manager.check_and_run_scheduled_proposals()
                    if executed:
                        logger.info(f"Low-peak window: Executed {len(executed)} scheduled proposals")
            except Exception as e:
                logger.debug(f"Scheduled proposals error: {e}")
            await asyncio.sleep(60)

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
