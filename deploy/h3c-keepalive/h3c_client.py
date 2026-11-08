import http.cookiejar
import json
import logging
import os
import re
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone

logger = logging.getLogger("h3c_client")

class H3CClient:
    CAS_LOGIN_URL = "https://ucloud.h3c.com/cas/login?service=https%3A%2F%2Fucloud.h3c.com%2Foasis6%2Fstatic"
    BASE_URL = "https://ucloud.h3c.com"

    def __init__(self, username, password):
        self.username = username
        self.password = password
        self.cj = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.cj))
        self.logged_in = False
        self.last_login_time = 0

    def login(self):
        logger.info(f"Authenticating to H3C Oasis CAS as {self.username}...")
        self.cj.clear()
        req = urllib.request.Request(self.CAS_LOGIN_URL, headers={
            "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
        })
        res = self.opener.open(req)
        html = res.read().decode("utf-8")

        lt_match = re.search(r'name="lt"\s+value="([^"]+)"', html)
        exec_match = re.search(r'name="execution"\s+value="([^"]+)"', html)
        lt = lt_match.group(1) if lt_match else ""
        execution = exec_match.group(1) if exec_match else "e1s1"

        form_data = {
            "username": self.username,
            "password": self.password,
            "lt": lt,
            "execution": execution,
            "_eventId": "submit",
            "captcha": "",
            "submit": "LOGIN"
        }
        encoded = urllib.parse.urlencode(form_data).encode("utf-8")
        post_req = urllib.request.Request(
            self.CAS_LOGIN_URL,
            data=encoded,
            headers={
                "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
                "Referer": self.CAS_LOGIN_URL
            }
        )
        post_res = self.opener.open(post_req)
        final_url = post_res.geturl()
        if "oasis6" not in final_url:
            raise RuntimeError(f"Login failed: redirected to {final_url}")
        
        self.logged_in = True
        self.last_login_time = time.time()
        logger.info("Login succeeded.")
        return True

    def _request(self, endpoint, method="GET", data=None, retry_auth=True):
        if not self.logged_in or time.time() - self.last_login_time > 3600:
            self.login()

        url = f"{self.BASE_URL}{endpoint}"
        headers = {
            "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
            "Referer": "https://ucloud.h3c.com/oasis6/static/",
            "Accept": "application/json, text/plain, */*"
        }
        encoded = None
        if data is not None:
            encoded = json.dumps(data).encode("utf-8")
            headers["Content-Type"] = "application/json;charset=UTF-8"

        req = urllib.request.Request(url, data=encoded, headers=headers, method=method)
        try:
            res = self.opener.open(req)
            body = res.read().decode("utf-8")
            data = json.loads(body)
            # Check if CAS redirected or session expired
            if isinstance(data, dict) and data.get("code") in (401, 403, 1000):
                if retry_auth:
                    logger.warning(f"Session expired (code {data.get('code')}), re-authenticating...")
                    self.login()
                    return self._request(endpoint, method, data, retry_auth=False)
            return data
        except urllib.error.HTTPError as e:
            if e.code in (401, 403) and retry_auth:
                logger.warning(f"HTTP {e.code}, re-authenticating...")
                self.login()
                return self._request(endpoint, method, data, retry_auth=False)
            err_msg = e.read().decode("utf-8", errors="ignore")
            logger.error(f"HTTP Error {e.code} for {endpoint}: {err_msg}")
            raise

    def get_tunnels(self):
        """Returns list of active tunnels."""
        res = self._request(f"/v3/ace/oasis/tunnelmgr/tunnel/tunnellist?userName={self.username}")
        if res and res.get("code") == 0:
            return res.get("data", {}).get("list", [])
        return []

    def get_apps(self, project_id):
        """Returns list of configured tunnel apps for a project."""
        res = self._request(f"/v3/ace/oasis/tunnelmgr/tunnelapp/applist?userName={self.username}&projectId={project_id}")
        if res and res.get("code") == 0:
            return res.get("data", {}).get("list", [])
        return []

    def find_app(self, project_id, app_name):
        """
        Finds registered app by app_name. If app_name is not configured directly
        (e.g. home_a / home_b), adapts template from base 'home' app.
        """
        apps = self.get_apps(project_id)
        if not apps:
            logger.warning(f"No apps returned for project {project_id}")
            return None

        for a in apps:
            if a.get("tunnelAppName") == app_name:
                return a

        # If app_name is home_a, check for base "home"
        if app_name == "home_a":
            for a in apps:
                if a.get("tunnelAppName") == "home":
                    return a

        # Fallback to first app
        first = apps[0]
        logger.info(f"App '{app_name}' not found; using '{first.get('tunnelAppName')}' as template")
        adapted = dict(first)
        adapted["tunnelAppName"] = app_name
        return adapted

    def delete_tunnel(self, tunnel_id):
        """Deletes/stops an active tunnel."""
        logger.info(f"Deleting tunnel {tunnel_id}...")
        res = self._request("/v3/ace/oasis/tunnelmgr/tunnel/tunneldelete", method="POST", data={"tunnelId": tunnel_id})
        logger.info(f"Delete response: {res}")
        return res

    def create_tunnel(self, app_info, valid_time=3, internal_port=None, app_name=None):
        """Creates/starts a tunnel for a registered app."""
        target_port = internal_port if internal_port is not None else app_info.get("internalPort", 8088)
        target_name = app_name if app_name is not None else app_info.get("tunnelAppName", "home")
        payload = {
            "userName": self.username,
            "projectId": str(app_info["projectId"]),
            "projectName": app_info.get("projectName", "home"),
            "internalIp": app_info.get("internalIp") or os.environ.get("NAS_LAN_IP") or "192.168.31.234",
            "internalPort": int(target_port),
            "proxyType": app_info.get("proxyType", "HTTP"),
            "tunnelAppId": app_info["tunnelAppId"],
            "tunnelAppName": target_name,
            "validTime": valid_time
        }
        logger.info(f"Creating tunnel for app '{target_name}' ({payload['internalIp']}:{payload['internalPort']})...")
        res = self._request("/v3/ace/oasis/tunnelmgr/tunnelapp/tunnelcreate", method="POST", data=payload)
        logger.info(f"Create response: {res}")
        if res and res.get("code") == 0:
            return res.get("data", {}).get("tunnelId")
        raise RuntimeError(f"Failed to create tunnel: {res}")

    def get_tunnel_status(self, tunnel_id):
        """Polls tunnel status."""
        res = self._request(f"/v3/ace/oasis/tunnelmgr/tunnel/tunnelstatus?tunnelId={tunnel_id}")
        if res and res.get("code") == 0:
            return res.get("data", {})
        return {}

    def wait_for_tunnel(self, tunnel_id, timeout_sec=30):
        """Polls tunnelstatus until ESTABLISHED or timeout."""
        start = time.time()
        while time.time() - start < timeout_sec:
            status_data = self.get_tunnel_status(tunnel_id)
            status = status_data.get("tunnelStatus")
            logger.info(f"Tunnel {tunnel_id} status: {status}")
            if status == "ESTABLISHED":
                return status_data
            time.sleep(2)
        raise TimeoutError(f"Tunnel {tunnel_id} did not establish within {timeout_sec}s")

    @staticmethod
    def parse_iso_time(iso_str):
        if not iso_str:
            return None
        # Handle '2026-10-02T12:07:56.253Z'
        clean = iso_str.replace("Z", "+00:00")
        try:
            return datetime.fromisoformat(clean)
        except Exception:
            return None

    def get_established_tunnel(self, app_name):
        """Finds established tunnel for app_name and calculates remainingMinutes."""
        tunnels = self.get_tunnels()
        now_utc = datetime.now(timezone.utc)
        for t in tunnels:
            if t.get("tunnelAppName") == app_name and t.get("tunnelStatus") == "ESTABLISHED":
                raw_time = t.get("closeTime") or t.get("finishTime")
                close_time = self.parse_iso_time(raw_time)
                remaining_min = (close_time - now_utc).total_seconds() / 60.0 if close_time else 0.0
                return {
                    "tunnelId": t.get("tunnelId"),
                    "appName": app_name,
                    "internalPort": t.get("internalPort"),
                    "externalAddr": t.get("externalAddr"),
                    "closeTime": raw_time,
                    "remainingMinutes": remaining_min,
                    "status": "ESTABLISHED",
                    "projectId": t.get("projectId", "10233048")
                }
        return None

    def create_and_wait_tunnel(self, app_name, internal_port=8088, project_id="10233048", valid_time=3, timeout_sec=30):
        """
        Creates a tunnel without destroying any other tunnel and waits until ESTABLISHED.
        Returns the established tunnel dictionary.
        """
        app_info = self.find_app(project_id, app_name)
        if not app_info:
            raise RuntimeError(f"App configuration not found for '{app_name}' in project {project_id}")

        new_tunnel_id = self.create_tunnel(
            app_info,
            valid_time=valid_time,
            internal_port=internal_port,
            app_name=app_name
        )
        status_data = self.wait_for_tunnel(new_tunnel_id, timeout_sec=timeout_sec)

        now_utc = datetime.now(timezone.utc)
        close_time = self.parse_iso_time(status_data.get("closeTime"))
        remaining_min = (close_time - now_utc).total_seconds() / 60.0 if close_time else 180.0

        return {
            "tunnelId": new_tunnel_id,
            "appName": app_name,
            "internalPort": internal_port,
            "externalAddr": status_data.get("externalAddr"),
            "closeTime": status_data.get("closeTime"),
            "remainingMinutes": remaining_min,
            "status": status_data.get("tunnelStatus")
        }

    def ensure_active_tunnel(self, app_name="home", min_remaining_minutes=25):
        """
        Ensures a standalone tunnel for app_name is active with at least min_remaining_minutes.
        Used for non-ping-pong tunnels (such as webrtc_test and nas_ssh).
        """
        existing = self.get_established_tunnel(app_name)
        now_utc = datetime.now(timezone.utc)

        if existing:
            remaining_min = existing.get("remainingMinutes", 0)
            logger.info(f"Existing tunnel {existing.get('tunnelId')} ({app_name}) has {remaining_min:.1f} minutes remaining.")
            if remaining_min > min_remaining_minutes:
                return existing
            logger.info(f"Tunnel {app_name} remaining time ({remaining_min:.1f}m) <= {min_remaining_minutes}m, renewing...")

        project_id = existing.get("projectId") if existing else "10233048"
        app_info = self.find_app(project_id, app_name)
        if not app_info:
            raise RuntimeError(f"App '{app_name}' not found in Oasis project {project_id}")

        existing_id = (existing.get("tunnelId") if existing else None) or app_info.get("tunnelId")
        if existing_id:
            try:
                self.delete_tunnel(existing_id)
                time.sleep(1)
            except Exception as e:
                logger.warning(f"Could not delete old tunnel {existing_id}: {e}")

        new_tunnel_id = self.create_tunnel(app_info, valid_time=3)
        status_data = self.wait_for_tunnel(new_tunnel_id)
        close_time = self.parse_iso_time(status_data.get("closeTime"))
        remaining_min = (close_time - now_utc).total_seconds() / 60.0 if close_time else 180.0

        return {
            "tunnelId": new_tunnel_id,
            "appName": app_name,
            "externalAddr": status_data.get("externalAddr"),
            "closeTime": status_data.get("closeTime"),
            "remainingMinutes": remaining_min,
            "status": status_data.get("tunnelStatus")
        }


class PingPongRelay:
    """
    Manages zero-downtime ping-pong tunnel relay (先生后死乒乓接力)
    between home_a (port 8088) and home_b (port 8089).

    Lifecycle Guarantee:
      1. One tunnel is active and serving traffic.
      2. When remaining time <= min_remaining_minutes (25m) or force_renew is triggered:
         - Keep active tunnel running.
         - Request H3C to create alternate peer tunnel on the opposite port.
         - Wait until alternate tunnel status becomes ESTABLISHED.
         - Update active endpoint to the new tunnel immediately for all new requests.
         - Retain the old tunnel in 'pending_retire' state for overlap_seconds (default 60s).
         - Both tunnels coexist and handle traffic during the 60s overlap.
      3. When the 60s overlap expires:
         - Silently delete the old expired tunnel.
         - Clear pending_retire state.
      4. On next rotation cycle, symmetrically alternate back to the other tunnel.
    """
    def __init__(self, client, app_a="home_a", port_a=8088, app_b="home_b", port_b=8089,
                 min_remaining_minutes=25, overlap_seconds=60, project_id="10233048"):
        self.client = client
        self.app_a = app_a
        self.port_a = port_a
        self.app_b = app_b
        self.port_b = port_b
        self.min_remaining_minutes = min_remaining_minutes
        self.overlap_seconds = overlap_seconds
        self.project_id = project_id

        self.active_app = None       # "home_a" or "home_b"
        self.active_tunnel = None    # dict of active tunnel details
        self.pending_retire = None   # dict: {"tunnelId", "appName", "retireAt", "externalAddr"}
        self.last_switch_time = 0
        self.lock = threading.Lock()

    def get_peer_target(self, current_app):
        if current_app == self.app_a:
            return self.app_b, self.port_b
        return self.app_a, self.port_a

    def check_and_retire(self):
        """
        Checks if pending_retire tunnel has exceeded overlap_seconds.
        If so, silently deletes it and clears pending_retire.
        Returns the retired tunnel dict, or None.
        """
        with self.lock:
            if not self.pending_retire:
                return None
            
            now = time.time()
            if now >= self.pending_retire["retireAt"]:
                tunnel_id = self.pending_retire["tunnelId"]
                app_name = self.pending_retire["appName"]
                logger.info(f"[PingPong] Overlap window ({self.overlap_seconds}s) ended for {app_name}. Silently deleting tunnel {tunnel_id}...")
                try:
                    self.client.delete_tunnel(tunnel_id)
                except Exception as e:
                    logger.warning(f"[PingPong] Error deleting retired tunnel {tunnel_id}: {e}")
                
                retired = dict(self.pending_retire)
                self.pending_retire = None
                return retired
            else:
                rem = self.pending_retire["retireAt"] - now
                logger.debug(f"[PingPong] Dual tunnel overlap active: {rem:.1f}s remaining before retiring {self.pending_retire['appName']}")
                return None

    def step(self, force_renew=False):
        """
        Executes one evaluation step of the ping-pong state machine.
        Returns tuple (active_tunnel, pending_retire).
        """
        with self.lock:
            # 1. Process retirement of previous overlap tunnel if due
            now = time.time()
            if self.pending_retire and now >= self.pending_retire["retireAt"]:
                self._do_retire(self.pending_retire)
                self.pending_retire = None

            # 2. Check current established tunnels in H3C
            tun_a = self.client.get_established_tunnel(self.app_a)
            tun_b = self.client.get_established_tunnel(self.app_b)

            if tun_a and tun_b:
                if self.pending_retire:
                    retiring_app = self.pending_retire["appName"]
                    current_active = tun_b if retiring_app == self.app_a else tun_a
                    current_app = self.app_b if retiring_app == self.app_a else self.app_a
                else:
                    rem_a = tun_a.get("remainingMinutes", 0)
                    rem_b = tun_b.get("remainingMinutes", 0)
                    if rem_a >= rem_b:
                        current_active = tun_a
                        current_app = self.app_a
                        if rem_b <= self.min_remaining_minutes:
                            self.pending_retire = {
                                "tunnelId": tun_b["tunnelId"],
                                "appName": self.app_b,
                                "retireAt": now + self.overlap_seconds,
                                "externalAddr": tun_b.get("externalAddr")
                            }
                    else:
                        current_active = tun_b
                        current_app = self.app_b
                        if rem_a <= self.min_remaining_minutes:
                            self.pending_retire = {
                                "tunnelId": tun_a["tunnelId"],
                                "appName": self.app_a,
                                "retireAt": now + self.overlap_seconds,
                                "externalAddr": tun_a.get("externalAddr")
                            }
                self.active_app = current_app
                self.active_tunnel = current_active
            elif tun_a:
                current_active = tun_a
                current_app = self.app_a
                self.active_app = current_app
                self.active_tunnel = current_active
            elif tun_b:
                current_active = tun_b
                current_app = self.app_b
                self.active_app = current_app
                self.active_tunnel = current_active
            else:
                current_active = None
                current_app = None

            # Scenario A: Neither tunnel is established -> Cold start
            if not current_active:
                logger.info(f"[PingPong] No active tunnel found for {self.app_a} or {self.app_b}. Initializing {self.app_a} (port {self.port_a})...")
                new_tun = self.client.create_and_wait_tunnel(
                    self.app_a,
                    internal_port=self.port_a,
                    project_id=self.project_id
                )
                self.active_app = self.app_a
                self.active_tunnel = new_tun
                self.pending_retire = None
                return self.active_tunnel, self.pending_retire

            # Scenario B: Active tunnel exists. Check if rotation is needed.
            rem_minutes = current_active.get("remainingMinutes", 0)
            logger.info(f"[PingPong] Active tunnel '{current_app}' has {rem_minutes:.1f}m remaining (expires {current_active.get('closeTime')}).")

            if rem_minutes > self.min_remaining_minutes and not force_renew:
                return self.active_tunnel, self.pending_retire

            # Scenario C: Remaining minutes <= min_remaining_minutes (or force_renew) -> Trigger Relay!
            target_app, target_port = self.get_peer_target(current_app)
            logger.info(f"[PingPong] Active tunnel '{current_app}' requires rotation (remaining={rem_minutes:.1f}m <= {self.min_remaining_minutes}m or force={force_renew}).")
            logger.info(f"[PingPong] [Create-Before-Destroy] Creating alternate peer tunnel '{target_app}' (internalPort={target_port}) while keeping '{current_app}' alive...")

            try:
                new_tun = self.client.create_and_wait_tunnel(
                    target_app,
                    internal_port=target_port,
                    project_id=self.project_id
                )
            except Exception as e:
                logger.error(f"[PingPong] Failed to establish alternate peer tunnel '{target_app}': {e}", exc_info=True)
                # Fail-safe: current_active is STILL alive! Keep serving traffic.
                return self.active_tunnel, self.pending_retire

            # Alternate peer tunnel is now ESTABLISHED!
            logger.info(f"[PingPong] Alternate peer tunnel '{target_app}' is ESTABLISHED -> {new_tun.get('externalAddr')}.")
            logger.info(f"[PingPong] Starting {self.overlap_seconds}s dual-tunnel overlap window. Both '{target_app}' and '{current_app}' active.")

            self.pending_retire = {
                "tunnelId": current_active["tunnelId"],
                "appName": current_app,
                "retireAt": time.time() + self.overlap_seconds,
                "externalAddr": current_active.get("externalAddr")
            }
            self.active_app = target_app
            self.active_tunnel = new_tun
            self.last_switch_time = time.time()

            return self.active_tunnel, self.pending_retire

    def _do_retire(self, retire_info):
        tunnel_id = retire_info["tunnelId"]
        app_name = retire_info["appName"]
        logger.info(f"[PingPong] Overlap window ({self.overlap_seconds}s) ended. Silently deleting old expired tunnel {tunnel_id} ({app_name})...")
        try:
            self.client.delete_tunnel(tunnel_id)
        except Exception as e:
            logger.warning(f"[PingPong] Error deleting retired tunnel {tunnel_id}: {e}")

    def get_status_dict(self):
        with self.lock:
            retire_info = None
            if self.pending_retire:
                now = time.time()
                rem_sec = max(0.0, self.pending_retire["retireAt"] - now)
                retire_info = {
                    "app": self.pending_retire["appName"],
                    "tunnelId": self.pending_retire["tunnelId"],
                    "retireAt": self.pending_retire["retireAt"],
                    "externalAddr": self.pending_retire.get("externalAddr"),
                    "remainingSeconds": round(rem_sec, 1)
                }
            return {
                "activeApp": self.active_app,
                "activeTunnel": self.active_tunnel,
                "pendingRetire": retire_info,
                "overlapSeconds": self.overlap_seconds,
                "minRemainingMinutes": self.min_remaining_minutes
            }
