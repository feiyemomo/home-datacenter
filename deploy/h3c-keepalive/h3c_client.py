import http.cookiejar
import json
import logging
import re
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

    def delete_tunnel(self, tunnel_id):
        """Deletes/stops an active tunnel."""
        logger.info(f"Deleting tunnel {tunnel_id}...")
        res = self._request("/v3/ace/oasis/tunnelmgr/tunnel/tunneldelete", method="POST", data={"tunnelId": tunnel_id})
        logger.info(f"Delete response: {res}")
        return res

    def create_tunnel(self, app_info, valid_time=3):
        """Creates/starts a tunnel for a registered app."""
        payload = {
            "userName": self.username,
            "projectId": str(app_info["projectId"]),
            "projectName": app_info.get("projectName", "home"),
            "internalIp": app_info.get("internalIp", "192.168.31.234"),
            "internalPort": int(app_info.get("internalPort", 8088)),
            "proxyType": app_info.get("proxyType", "HTTP"),
            "tunnelAppId": app_info["tunnelAppId"],
            "tunnelAppName": app_info["tunnelAppName"],
            "validTime": valid_time
        }
        logger.info(f"Creating tunnel for app '{app_info['tunnelAppName']}' ({payload['internalIp']}:{payload['internalPort']})...")
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

    def ensure_active_tunnel(self, app_name="home", min_remaining_minutes=25):
        """
        Ensures a tunnel for app_name is active with at least min_remaining_minutes.
        Returns dict with tunnel details and externalAddr.
        """
        tunnels = self.get_tunnels()
        app_tunnel = None
        for t in tunnels:
            if t.get("tunnelAppName") == app_name and t.get("tunnelStatus") == "ESTABLISHED":
                app_tunnel = t
                break

        now_utc = datetime.now(timezone.utc)
        renew_needed = True

        if app_tunnel:
            raw_time = app_tunnel.get("closeTime") or app_tunnel.get("finishTime")
            close_time = self.parse_iso_time(raw_time)
            if close_time:
                remaining_sec = (close_time - now_utc).total_seconds()
                remaining_min = remaining_sec / 60.0
                logger.info(f"Existing tunnel {app_tunnel.get('tunnelId')} has {remaining_min:.1f} minutes remaining (expires {raw_time}).")
                if remaining_min > min_remaining_minutes:
                    # Healthy, no renew needed
                    return {
                        "tunnelId": app_tunnel.get("tunnelId"),
                        "externalAddr": app_tunnel.get("externalAddr"),
                        "closeTime": raw_time,
                        "remainingMinutes": remaining_min,
                        "status": "ESTABLISHED"
                    }
                else:
                    logger.info(f"Remaining time ({remaining_min:.1f}m) <= {min_remaining_minutes}m, renewing...")
            else:
                logger.info("Existing tunnel has no closeTime or finishTime, renewing...")

        # If we need to renew or create:
        project_id = app_tunnel.get("projectId") if app_tunnel else "10233048"
        apps = self.get_apps(project_id)
        target_app = None
        for a in apps:
            if a.get("tunnelAppName") == app_name:
                target_app = a
                break

        if not target_app:
            raise RuntimeError(f"App '{app_name}' not found in Oasis project {project_id}")

        # Delete existing tunnel if still present
        existing_id = (app_tunnel.get("tunnelId") if app_tunnel else None) or target_app.get("tunnelId")
        if existing_id:
            try:
                self.delete_tunnel(existing_id)
                time.sleep(2)
            except Exception as e:
                logger.warning(f"Could not delete old tunnel {existing_id}: {e}")

        # Create new tunnel
        new_tunnel_id = self.create_tunnel(target_app, valid_time=3)
        status_data = self.wait_for_tunnel(new_tunnel_id)
        
        close_time = self.parse_iso_time(status_data.get("closeTime"))
        remaining_min = (close_time - now_utc).total_seconds() / 60.0 if close_time else 180.0
        
        return {
            "tunnelId": new_tunnel_id,
            "externalAddr": status_data.get("externalAddr"),
            "closeTime": status_data.get("closeTime"),
            "remainingMinutes": remaining_min,
            "status": status_data.get("tunnelStatus")
        }
