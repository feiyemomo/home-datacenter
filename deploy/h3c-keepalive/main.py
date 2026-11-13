import http.client
import http.server
import json
import logging
import os
import re
import socket
import sys
import threading
import time
import urllib.parse
import urllib.request
import html
from datetime import datetime
from h3c_client import H3CClient, PingPongRelay

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S"
)
logger = logging.getLogger("keepalive")

H3C_USERNAME = os.environ.get("H3C_USERNAME", "")
H3C_PASSWORD = os.environ.get("H3C_PASSWORD", "")
H3C_APP_NAMES = [a.strip() for a in os.environ.get("H3C_APP_NAMES", os.environ.get("H3C_APP_NAME", "home,webrtc_test")).split(",") if a.strip()]
CHECK_INTERVAL = int(os.environ.get("CHECK_INTERVAL", "60"))
MIN_REMAINING_MINUTES = int(os.environ.get("MIN_REMAINING_MINUTES", "25"))
OVERLAP_SECONDS = int(os.environ.get("OVERLAP_SECONDS", "60"))
HTTP_PORT = int(os.environ.get("HTTP_PORT", "8087"))
DATA_DIR = os.environ.get("DATA_DIR", "/data/h3c")
FRIGATE_URL = os.environ.get("FRIGATE_URL", "http://home-frigate:5000")
GO2RTC_URL = os.environ.get("GO2RTC_URL", "http://home-frigate:1984")

PING_PONG_APP_A = os.environ.get("PING_PONG_APP_A", "home")
PING_PONG_PORT_A = int(os.environ.get("PING_PONG_PORT_A", "8088"))
PING_PONG_APP_B = os.environ.get("PING_PONG_APP_B", "home_b")
PING_PONG_PORT_B = int(os.environ.get("PING_PONG_PORT_B", "8089"))

def detect_nas_lan_ip(client_inst=None, project_id="10233048"):
    """
    Discovers current authoritative NAS LAN IPv4 address via multi-tier fallback:
      Tier 1: Explicit env var NAS_LAN_IP (if valid and not 'auto')
      Tier 2: Shared host file (DATA_DIR/host_lan_ip.txt)
      Tier 3: Frigate /api/config go2rtc candidate inspection
      Tier 4: Cross-correlation with other H3C apps in Oasis (e.g. 'ssh' app)
      Tier 5: Fallback default (192.168.31.233)
    """
    # Tier 1: Explicit env
    env_ip = os.environ.get("NAS_LAN_IP", "").strip()
    if env_ip and env_ip.lower() != "auto":
        return env_ip

    # Tier 2: File on disk
    file_path = os.path.join(DATA_DIR, "host_lan_ip.txt")
    if os.path.exists(file_path):
        try:
            with open(file_path, "r", encoding="utf-8") as f:
                f_ip = f.read().strip()
                if f_ip and re.match(r"^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$", f_ip):
                    return f_ip
        except Exception as e:
            logger.debug(f"Could not read {file_path}: {e}")

    # Tier 3: Frigate /api/config candidates
    try:
        req = urllib.request.Request(f"{FRIGATE_URL}/api/config")
        with urllib.request.urlopen(req, timeout=3) as resp:
            cfg = json.loads(resp.read().decode())
            candidates = cfg.get("go2rtc", {}).get("webrtc", {}).get("candidates", [])
            for c in reversed(candidates):
                clean = c.split(":")[0] if ":" in c else c
                if clean.startswith("192.168.") or clean.startswith("10.") or (clean.startswith("172.") and not clean.startswith("172.18.") and not clean.startswith("172.17.")):
                    if not clean.startswith("100."):  # Exclude Tailscale
                        return clean
    except Exception as e:
        logger.debug(f"Tier 3 IP detection from Frigate failed: {e}")

    # Tier 4: Oasis other apps correlation (e.g. 'ssh' app)
    if client_inst:
        try:
            apps = client_inst.get_apps(project_id)
            for a in apps:
                if a.get("tunnelAppName") == "ssh" and a.get("internalIp"):
                    s_ip = a.get("internalIp")
                    if s_ip.startswith("192.168.") or s_ip.startswith("10."):
                        return s_ip
        except Exception as e:
            logger.debug(f"Tier 4 IP detection from Oasis failed: {e}")

    # Tier 5: Fallback default
    return "192.168.31.233"

class TunnelCircuitBreaker:
    """
    Prevents API churn, infinite failover loops, and rapid port drift by enforcing
    exponential backoff and circuit breaking when reachability fails consecutively.
    """
    def __init__(self, name="primary", max_consecutive_failures=3, base_cooldown=60, max_cooling=300):
        self.name = name
        self.max_failures = max_consecutive_failures
        self.base_cooldown = base_cooldown
        self.max_cooling = max_cooling
        self.consecutive_failures = 0
        self.cooling_until = 0
        self.lock = threading.Lock()

    def can_failover(self):
        with self.lock:
            now = time.time()
            if now < self.cooling_until:
                rem = int(self.cooling_until - now)
                logger.warning(
                    f"[CircuitBreaker:{self.name}] Cooldown active ({rem}s remaining). "
                    f"Suppressed failover to prevent H3C API rate limiting and port drift."
                )
                return False
            return True

    def record_success(self):
        with self.lock:
            if self.consecutive_failures > 0:
                logger.info(f"[CircuitBreaker:{self.name}] Probe succeeded. Resetting failure counter (was {self.consecutive_failures}).")
            self.consecutive_failures = 0
            self.cooling_until = 0

    def record_failure(self):
        with self.lock:
            self.consecutive_failures += 1
            now = time.time()
            if self.consecutive_failures >= self.max_failures:
                self.cooling_until = now + self.max_cooling
                logger.error(
                    f"[CircuitBreaker:{self.name}] Tripped! {self.consecutive_failures} consecutive failovers failed. "
                    f"Entering {self.max_cooling}s cooling period. Tunnel churn paused."
                )
            else:
                backoff = min(self.max_cooling, self.base_cooldown * (2 ** (self.consecutive_failures - 1)))
                self.cooling_until = now + backoff
                logger.warning(
                    f"[CircuitBreaker:{self.name}] Recorded failure #{self.consecutive_failures}. "
                    f"Cooldown backoff set to {backoff}s."
                )

client = H3CClient(H3C_USERNAME, H3C_PASSWORD)
ping_pong_relay = PingPongRelay(
    client=client,
    app_a=PING_PONG_APP_A,
    port_a=PING_PONG_PORT_A,
    app_b=PING_PONG_APP_B,
    port_b=PING_PONG_PORT_B,
    min_remaining_minutes=MIN_REMAINING_MINUTES,
    overlap_seconds=OVERLAP_SECONDS,
    get_lan_ip_func=lambda: detect_nas_lan_ip(client_inst=client)
)

primary_breaker = TunnelCircuitBreaker(name="primary")
aux_breakers = {}

# Global thread-safe tunnel state
state_lock = threading.Lock()
tunnel_state = {
    "status": "initializing",
    "activeApp": "",
    "externalAddr": "",
    "tunnelId": "",
    "closeTime": "",
    "remainingMinutes": 0,
    "pendingRetire": None,
    "lastChecked": "",
    "error": None,
    "lanIp": "192.168.31.233",
    "tunnels": {}
}

class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, socket_path):
        super().__init__("localhost")
        self.socket_path = socket_path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(self.socket_path)

def docker_exec_frigate(cmd):
    sock_path = "/var/run/docker.sock"
    if not os.path.exists(sock_path):
        return None
    try:
        conn = UnixHTTPConnection(sock_path)
        payload = json.dumps({"AttachStdout": True, "AttachStderr": True, "Cmd": cmd})
        conn.request("POST", "/containers/home-frigate/exec", payload, {"Content-Type": "application/json"})
        resp = conn.getresponse()
        data = json.loads(resp.read().decode())
        exec_id = data.get("Id")
        if not exec_id:
            return None
        conn.request("POST", f"/exec/{exec_id}/start", json.dumps({"Detach": False}), {"Content-Type": "application/json"})
        resp = conn.getresponse()
        return resp.read().decode(errors="ignore")
    except Exception as e:
        logger.warning(f"docker_exec_frigate failed for {cmd}: {e}")
        return None

def check_addr_reachable_once(addr, default_port=80, timeout=3.0):
    if not addr:
        return False
    try:
        lower = addr.strip().lower()
        if lower.startswith("http://") or lower.startswith("https://"):
            req = urllib.request.Request(addr.strip(), headers={"User-Agent": "H3C-Probe/1.0"})
            try:
                with urllib.request.urlopen(req, timeout=timeout) as resp:
                    return resp.status < 500
            except urllib.error.HTTPError as e:
                # 4xx client errors (401/403/404) mean the server and tunnel are alive; only 5xx is failure
                return e.code < 500
        else:
            clean = addr.strip().strip("/")
            if ":" in clean:
                host, port_str = clean.rsplit(":", 1)
                port = int(port_str)
            else:
                host = clean
                port = default_port
            with socket.create_connection((host, port), timeout=timeout):
                return True
    except Exception as e:
        logger.debug(f"Reachability probe attempt for {addr} failed: {e}")
        return False

def check_addr_reachable(addr, default_port=80, retries=3, retry_delay=1.0):
    """
    Debounced reachability probe. Requires all retries to fail before marking unreachable.
    """
    if not addr:
        return False
    for attempt in range(1, retries + 1):
        if check_addr_reachable_once(addr, default_port=default_port):
            return True
        if attempt < retries:
            time.sleep(retry_delay)
    logger.warning(f"Reachability check for {addr} failed after {retries} attempts.")
    return False

last_synced_webrtc_addr = None

def sync_webrtc_to_frigate(external_addr):
    global last_synced_webrtc_addr
    # Only sync when the tunnel address actually changed.
    if not external_addr or external_addr == last_synced_webrtc_addr:
        return
    try:
        req = urllib.request.Request(f"{FRIGATE_URL}/api/config")
        with urllib.request.urlopen(req, timeout=5) as resp:
            cfg = json.loads(resp.read().decode())
        
        ext_host = external_addr.split(":")[0] if external_addr else ""
        current_candidates = cfg.get("go2rtc", {}).get("webrtc", {}).get("candidates", [])
        new_candidates = []
        for c in current_candidates:
            if ext_host and c.startswith(ext_host + ":"):
                continue
            if c.startswith("154.8.195.220:"):
                continue
            new_candidates.append(c)
        if external_addr and external_addr not in new_candidates:
            new_candidates.append(external_addr)
        
        # LAN / Tailscale candidates are owned by home-api (BuildWebRTCCandidates)
        if "127.0.0.1:8555" not in new_candidates:
            new_candidates.insert(0, "127.0.0.1:8555")
        if "stun:8555" not in new_candidates:
            new_candidates.append("stun:8555")

        payload = {
            "requires_restart": 0,
            "update_topic": "config/webrtc-candidates",
            "config_data": {
                "go2rtc": {
                    "webrtc": {
                        "candidates": new_candidates
                    }
                }
            }
        }
        # 1. Update Frigate persistent config on disk (zero restart)
        try:
            put_req = urllib.request.Request(
                f"{FRIGATE_URL}/api/config/set",
                data=json.dumps(payload).encode("utf-8"),
                headers={"Content-Type": "application/json"},
                method="PUT"
            )
            with urllib.request.urlopen(put_req, timeout=5) as resp:
                logger.info(f"Updated Frigate WebRTC candidate to {external_addr}: {resp.read().decode()}")
        except Exception as fe:
            logger.warning(f"Could not sync WebRTC candidate to Frigate REST API: {fe}")

        # 2. Update go2rtc's in-memory config via POST /api/config (zero restart)
        try:
            go2_payload = json.dumps({"webrtc": {"candidates": new_candidates}}).encode("utf-8")
            go2_req = urllib.request.Request(
                f"{GO2RTC_URL}/api/config",
                data=go2_payload,
                headers={"Content-Type": "application/json"},
                method="POST"
            )
            with urllib.request.urlopen(go2_req, timeout=5) as resp:
                logger.info(f"Updated go2rtc active candidates: {new_candidates}")
        except Exception as ge:
            logger.warning(f"Could not reload go2rtc active config: {ge}")

        # 3. Synchronize /dev/shm/go2rtc.yaml via Docker exec (if socket mounted) WITHOUT restarting go2rtc!
        # Deliberately NO /api/restart here to preserve live WebRTC sessions and prevent blackouts.
        exec_out = docker_exec_frigate(["python3", "/usr/local/go2rtc/create_config.py"])
        if exec_out is not None:
            logger.info("Successfully regenerated /dev/shm/go2rtc.yaml via create_config.py (zero restart)")

        last_synced_webrtc_addr = external_addr
    except Exception as e:
        logger.warning(f"Could not sync WebRTC candidate: {e}")

def update_state(primary_data, all_tunnels=None, pending_retire=None, active_app=None, error=None, lan_ip=None):
    with state_lock:
        if lan_ip:
            tunnel_state["lanIp"] = lan_ip
        if all_tunnels:
            tunnel_state["tunnels"] = all_tunnels
        if not primary_data:
            tunnel_state["status"] = "error"
            tunnel_state["error"] = str(error) if error else "Primary tunnel is not established"
            tunnel_state["activeApp"] = active_app or ""
            tunnel_state["externalAddr"] = ""
            tunnel_state["tunnelId"] = ""
            tunnel_state["closeTime"] = ""
            tunnel_state["remainingMinutes"] = 0
            tunnel_state["pendingRetire"] = None
            tunnel_state["lastChecked"] = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
        else:
            tunnel_state["status"] = "online"
            tunnel_state["activeApp"] = active_app or primary_data.get("appName", "")
            tunnel_state["externalAddr"] = primary_data.get("externalAddr", "")
            tunnel_state["tunnelId"] = primary_data.get("tunnelId", "")
            tunnel_state["closeTime"] = primary_data.get("closeTime", "")
            tunnel_state["remainingMinutes"] = round(primary_data.get("remainingMinutes", 0), 1)
            tunnel_state["pendingRetire"] = pending_retire
            tunnel_state["lastChecked"] = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
            tunnel_state["error"] = None
            
            # Write state files if DATA_DIR exists
            try:
                os.makedirs(DATA_DIR, exist_ok=True)
                url_path = os.path.join(DATA_DIR, "current_url.txt")
                with open(url_path, "w", encoding="utf-8") as f:
                    f.write(tunnel_state["externalAddr"])

                html_path = os.path.join(DATA_DIR, "index.html")
                target = tunnel_state["externalAddr"]
                html_content = f"""<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <meta http-equiv="refresh" content="0; url={target}">
  <title>Redirecting to H3C Gateway...</title>
  <script>window.location.replace("{target}");</script>
</head>
<body style="background:#0f172a;color:#f8fafc;font-family:system-ui,-apple-system,sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;">
  <div style="text-align:center;padding:2rem;background:#1e293b;border-radius:1rem;box-shadow:0 10px 25px rgba(0,0,0,0.5);">
    <h2 style="margin:0 0 1rem 0;">⚡ 正在跳转至 H3C 国内高速通道</h2>
    <p style="color:#94a3b8;margin-bottom:1.5rem;">极速直连家庭数据中心</p>
    <a href="{target}" style="display:inline-block;padding:0.75rem 1.5rem;background:#38bdf8;color:#0f172a;text-decoration:none;font-weight:600;border-radius:0.5rem;">立即进入 ({target})</a>
  </div>
</body>
</html>"""
                with open(html_path, "w", encoding="utf-8") as f:
                    f.write(html_content)
            except Exception as e:
                logger.warning(f"Failed to write state files to {DATA_DIR}: {e}")

def run_check(force_renew=False):
    """
    Executes a comprehensive health check:
      1. Ping-pong relay step for primary web traffic (home_a 8088 / home_b 8089).
      2. Ensures secondary tunnels (webrtc_test).
      3. Synchronizes WebRTC candidates to Frigate dynamically without restart.
    """
    logger.info(f"Running tunnel health check (force_renew={force_renew})...")
    all_tunnels = {}
    effective_lan_ip = detect_nas_lan_ip(client_inst=client)
    logger.info(f"Detected authoritative NAS LAN IP: {effective_lan_ip}")

    # 1. Primary HTTP Ping-Pong Relay Step
    primary_res = None
    pending_retire_info = None
    try:
        primary_res, pending_retire_info = ping_pong_relay.step(force_renew=force_renew)
        if primary_res and primary_res.get("externalAddr") and not force_renew:
            if not check_addr_reachable(primary_res.get("externalAddr")):
                if primary_breaker.can_failover():
                    logger.warning(f"Active tunnel '{ping_pong_relay.active_app}' ({primary_res.get('externalAddr')}) unreachable! Forcing failover...")
                    primary_breaker.record_failure()
                    primary_res, pending_retire_info = ping_pong_relay.step(force_renew=True)
                else:
                    logger.warning(f"Active tunnel '{ping_pong_relay.active_app}' unreachable, but failover suppressed by circuit breaker.")
            else:
                primary_breaker.record_success()
        
        if primary_res:
            active_app = ping_pong_relay.active_app
            all_tunnels[active_app] = primary_res
            all_tunnels["home"] = primary_res  # Compatibility alias
            logger.info(f"Active primary tunnel '{active_app}': {primary_res.get('externalAddr')} (expires {primary_res.get('closeTime')}, remaining {primary_res.get('remainingMinutes', 0):.1f}m)")

        if pending_retire_info:
            all_tunnels[pending_retire_info["appName"]] = {
                "tunnelId": pending_retire_info["tunnelId"],
                "appName": pending_retire_info["appName"],
                "externalAddr": pending_retire_info.get("externalAddr"),
                "status": "ESTABLISHED",
                "retiring": True
            }
            logger.info(f"Overlapping retiring tunnel '{pending_retire_info['appName']}': {pending_retire_info.get('externalAddr')} (retires in <= {OVERLAP_SECONDS}s)")
    except Exception as e:
        logger.error(f"Error evaluating ping-pong relay: {e}", exc_info=True)

    # 2. Check other auxiliary tunnels (e.g. webrtc_test)
    min_min = 180 if force_renew else MIN_REMAINING_MINUTES
    for app in H3C_APP_NAMES:
        if app in ("home", PING_PONG_APP_A, PING_PONG_APP_B):
            continue
        try:
            logger.info(f"Checking auxiliary H3C tunnel status for '{app}' (force_renew={force_renew})...")
            res = client.ensure_active_tunnel(app_name=app, min_remaining_minutes=min_min, internal_ip=effective_lan_ip)
            if res.get("externalAddr") and not force_renew:
                if not check_addr_reachable(res.get("externalAddr")):
                    breaker = aux_breakers.setdefault(app, TunnelCircuitBreaker(name=app))
                    if breaker.can_failover():
                        logger.warning(f"Tunnel '{app}' ({res.get('externalAddr')}) unreachable! Forcing recreation...")
                        breaker.record_failure()
                        res = client.ensure_active_tunnel(app_name=app, min_remaining_minutes=180, internal_ip=effective_lan_ip)
                    else:
                        logger.warning(f"Tunnel '{app}' unreachable, but recreation suppressed by circuit breaker.")
                else:
                    if app in aux_breakers:
                        aux_breakers[app].record_success()
            all_tunnels[app] = res
            logger.info(f"Auxiliary tunnel '{app}': {res.get('externalAddr')} (remaining {res.get('remainingMinutes', 0):.1f}m)")

            if app == "webrtc_test" and res.get("externalAddr"):
                try:
                    os.makedirs(DATA_DIR, exist_ok=True)
                    with open(os.path.join(DATA_DIR, "webrtc_url.txt"), "w", encoding="utf-8") as f:
                        f.write(res.get("externalAddr"))
                except Exception as ex:
                    logger.warning(f"Failed to write webrtc_url.txt: {ex}")
                sync_webrtc_to_frigate(res.get("externalAddr"))
        except Exception as e:
            logger.error(f"Error checking auxiliary tunnel '{app}': {e}", exc_info=True)
            all_tunnels[app] = {"status": "error", "error": str(e)}

    # Format pending_retire for status dict
    formatted_retire = None
    if pending_retire_info:
        now = time.time()
        rem_sec = max(0.0, pending_retire_info["retireAt"] - now)
        formatted_retire = {
            "app": pending_retire_info["appName"],
            "tunnelId": pending_retire_info["tunnelId"],
            "retireAt": pending_retire_info["retireAt"],
            "externalAddr": pending_retire_info.get("externalAddr"),
            "remainingSeconds": round(rem_sec, 1)
        }

    update_state(
        primary_data=primary_res,
        all_tunnels=all_tunnels,
        pending_retire=formatted_retire,
        active_app=ping_pong_relay.active_app,
        error=None if primary_res else "Primary ping-pong relay is not established",
        lan_ip=effective_lan_ip
    )
    return primary_res

def get_sanitized_state():
    with state_lock:
        state_copy = dict(tunnel_state)
        if state_copy.get("pendingRetire"):
            now = time.time()
            rem = max(0.0, state_copy["pendingRetire"]["retireAt"] - now)
            state_copy["pendingRetire"]["remainingSeconds"] = round(rem, 1)
        return state_copy

def keepalive_worker():
    logger.info("Keepalive worker thread started.")
    run_check()
    last_full_check = time.time()

    while True:
        try:
            time.sleep(5)
            # 1. Frequently inspect overlap timer to silently delete expired tunnel after 60s
            if ping_pong_relay.pending_retire:
                retired = ping_pong_relay.check_and_retire()
                if retired:
                    logger.info(f"[PingPong] Overlap finished; retired tunnel {retired['tunnelId']} ({retired['appName']})")
                    with state_lock:
                        tunnel_state["pendingRetire"] = None
                        if retired["appName"] in tunnel_state.get("tunnels", {}):
                            del tunnel_state["tunnels"][retired["appName"]]

            # 2. Check full interval for renewal triggers
            now = time.time()
            if now - last_full_check >= CHECK_INTERVAL:
                run_check()
                last_full_check = time.time()
        except Exception as e:
            logger.error(f"Worker iteration error: {e}", exc_info=True)
            time.sleep(10)

class KeepaliveHTTPHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, format, *args):
        if args and str(args[1]) in ("404", "500", "503"):
            super().log_message(format, *args)

    def do_HEAD(self):
        self.do_GET()

    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path
        query = urllib.parse.parse_qs(parsed.query)

        if path in ("/status", "/api/status", "/api/v1/h3c/status"):
            body = json.dumps(get_sanitized_state(), ensure_ascii=False, indent=2).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Access-Control-Allow-Origin", "*")
            self.send_header("Cache-Control", "no-cache, no-store, must-revalidate")
            self.end_headers()
            self.wfile.write(body)
            return

        if path in ("/", "/redirect", "/fast", "/h3c"):
            with state_lock:
                addr = tunnel_state.get("externalAddr")
                status = tunnel_state.get("status")

            if not addr or status != "online":
                self.send_response(503)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Refresh", "3")
                init_html = "<html><head><meta http-equiv='refresh' content='3'></head><body><h3>H3C Tunnel is initializing/reconnecting...</h3><p>Please wait...</p></body></html>"
                self.wfile.write(init_html.encode("utf-8"))
                return

            dest_path = ""
            if "path" in query and query["path"]:
                clean_path = query["path"][0].replace("\r", "").replace("\n", "").strip()
                dest_path = urllib.parse.quote(clean_path, safe="/?#&=_-.!~*'")
            
            target_url = addr.rstrip("/") + ("/" + dest_path.lstrip("/") if dest_path else "/")
            if "token" in query and query["token"]:
                clean_token = query["token"][0].replace("\r", "").replace("\n", "").strip()
                target_url += f"#token={urllib.parse.quote(clean_token)}"

            escaped_url = html.escape(target_url, quote=True)

            self.send_response(302)
            self.send_header("Location", target_url)
            self.send_header("Access-Control-Allow-Origin", "*")
            self.send_header("Cache-Control", "no-cache, no-store, must-revalidate")
            self.send_header("Pragma", "no-cache")
            self.send_header("Expires", "0")
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.end_headers()
            fallback = f"<html><head><meta http-equiv='refresh' content='0;url={escaped_url}'></head><body>Redirecting to <a href='{escaped_url}'>{escaped_url}</a></body></html>"
            self.wfile.write(fallback.encode("utf-8"))
            return

        self.send_response(404)
        self.end_headers()

    def do_POST(self):
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path in ("/refresh", "/api/v1/h3c/refresh"):
            res = run_check(force_renew=True)
            body = json.dumps(get_sanitized_state(), ensure_ascii=False, indent=2).encode("utf-8")
            self.send_response(200 if res else 500)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Access-Control-Allow-Origin", "*")
            self.end_headers()
            self.wfile.write(body)
            return

        self.send_response(404)
        self.end_headers()

def main():
    logger.info(f"Starting H3C NAT Keepalive daemon on port {HTTP_PORT} (ping-pong: {PING_PONG_APP_A}:{PING_PONG_PORT_A} <-> {PING_PONG_APP_B}:{PING_PONG_PORT_B}, aux: {H3C_APP_NAMES})...")
    worker = threading.Thread(target=keepalive_worker, daemon=True)
    worker.start()

    server = http.server.ThreadingHTTPServer(("0.0.0.0", HTTP_PORT), KeepaliveHTTPHandler)
    logger.info(f"HTTP Server listening on 0.0.0.0:{HTTP_PORT}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        logger.info("Shutting down...")
        server.server_close()

if __name__ == "__main__":
    main()
