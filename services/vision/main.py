import os
import sys
import json
import base64
import logging
import urllib.request
import urllib.parse
from http.server import HTTPServer, BaseHTTPRequestHandler
from socketserver import ThreadingMixIn
import numpy as np
import cv2

from face_engine import FaceEngine
from pose_engine import PoseEngine

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(name)s: %(message)s")
logger = logging.getLogger("vision_service")

MODEL_DIR = os.environ.get("MODEL_DIR", os.path.join(os.path.dirname(__file__), "models"))
DATA_DIR = os.environ.get("DATA_DIR", "/data/vision" if os.path.exists("/data") else os.path.join(os.path.dirname(__file__), "data"))
PORT = int(os.environ.get("PORT", "8090"))

face_engine = FaceEngine(model_dir=MODEL_DIR, data_dir=DATA_DIR)
pose_engine = PoseEngine(model_dir=MODEL_DIR)

def is_safe_url(url_str, allow_loopback=False):
    try:
        parsed = urllib.parse.urlparse(url_str)
        # Strictly require http or https
        if parsed.scheme not in ("http", "https"):
            logger.warning(f"SSRF blocked: invalid scheme '{parsed.scheme}' in {url_str}")
            return False
        hostname = parsed.hostname
        if not hostname:
            return False

        if not allow_loopback and os.environ.get("VISION_ALLOW_LOOPBACK", "false").lower() in ("true", "1"):
            allow_loopback = True

        # Resolve IP and check if it's private or reserved
        import ipaddress
        import socket
        try:
            ip_obj = ipaddress.ip_address(hostname)
            resolved_ips = [ip_obj]
        except ValueError:
            # Domain name: resolve all addresses
            addr_info = socket.getaddrinfo(hostname, None)
            if not addr_info:
                return False
            resolved_ips = [ipaddress.ip_address(item[4][0]) for item in addr_info]

        for ip in resolved_ips:
            # Block cloud metadata (e.g. 169.254.169.254) and link-local
            if ip.is_link_local or str(ip).startswith("169.254."):
                logger.warning(f"SSRF blocked: cloud metadata / link-local target {ip}")
                return False

            # Check loopback
            if ip.is_loopback or hostname.lower() in ("localhost", "127.0.0.1", "::1"):
                if not allow_loopback:
                    logger.warning(f"SSRF blocked: loopback target {hostname} ({ip})")
                    return False
            elif ip.is_reserved:
                logger.warning(f"SSRF blocked: reserved IP target {ip}")
                return False
            elif ip.is_private:
                # Block all private IPs by default to prevent internal Docker/subnet SSRF (Finding #55)
                logger.warning(f"SSRF blocked: private network IP {ip}")
                return False

        return True
    except Exception as e:
        logger.warning(f"SSRF check exception for {url_str}: {e}")
        return False

def decode_image(payload):
    if "image" in payload and payload["image"]:
        b64_str = payload["image"]
        if "," in b64_str:
            b64_str = b64_str.split(",", 1)[1]
        img_bytes = base64.b64decode(b64_str)
        nparr = np.frombuffer(img_bytes, np.uint8)
        return cv2.imdecode(nparr, cv2.IMREAD_COLOR)

    if "image_url" in payload and payload["image_url"]:
        url = payload["image_url"]
        if not is_safe_url(url):
            logger.warning(f"Rejected unsafe image_url: {url}")
            return None
        req = urllib.request.Request(url, headers={"User-Agent": "HomeDatacenter-Vision/1.0"})
        with urllib.request.urlopen(req, timeout=10) as resp:
            img_bytes = resp.read()
            nparr = np.frombuffer(img_bytes, np.uint8)
            return cv2.imdecode(nparr, cv2.IMREAD_COLOR)

    return None

class ThreadedHTTPServer(ThreadingMixIn, HTTPServer):
    daemon_threads = True

class VisionHandler(BaseHTTPRequestHandler):
    def send_json(self, status_code, data):
        body = json.dumps(data, ensure_ascii=False).encode("utf-8")
        self.send_response(status_code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Headers", "Content-Type, Authorization")
        self.send_header("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
        self.end_headers()
        self.wfile.write(body)

    def do_OPTIONS(self):
        self.send_response(204)
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Headers", "Content-Type, Authorization")
        self.send_header("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
        self.end_headers()

    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path

        if path == "/health":
            self.send_json(200, {
                "status": "ok",
                "face_engine_ready": face_engine.detector is not None,
                "pose_engine_ready": pose_engine.net is not None,
                "registered_persons": len(face_engine.persons)
            })
            return

        if path == "/api/v1/face/list" or path == "/api/v1/vision/persons":
            self.send_json(200, {
                "code": 0,
                "data": face_engine.list_persons()
            })
            return

        self.send_json(404, {"code": 404, "message": "Not Found"})

    def do_POST(self):
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path
        content_length = int(self.headers.get("Content-Length", 0))
        raw_body = self.rfile.read(content_length).decode("utf-8", errors="replace")

        try:
            payload = json.loads(raw_body) if raw_body else {}
        except Exception:
            self.send_json(400, {"code": 400, "message": "Invalid JSON body"})
            return

        if path == "/api/v1/face/register" or path == "/api/v1/vision/persons":
            name = payload.get("name", "").strip()
            if not name:
                self.send_json(400, {"code": 400, "message": "name is required"})
                return

            img = decode_image(payload)
            if img is None:
                self.send_json(400, {"code": 400, "message": "Valid image or image_url is required"})
                return

            try:
                face_engine.register_person(name, img)
                self.send_json(200, {
                    "code": 0,
                    "message": f"Person '{name}' registered successfully"
                })
            except Exception as e:
                self.send_json(422, {"code": 422, "message": str(e)})
            return

        if path == "/api/v1/analyze" or path == "/api/v1/vision/analyze":
            img = decode_image(payload)
            if img is None:
                self.send_json(400, {"code": 400, "message": "Valid image or image_url is required"})
                return

            detect_face = payload.get("detect_face", True)
            detect_pose = payload.get("detect_pose", True)

            faces = []
            poses = []

            if detect_face:
                try:
                    faces = face_engine.recognize(img)
                except Exception as e:
                    logger.error(f"Face recognition error: {e}")

            if detect_pose:
                try:
                    poses = pose_engine.detect(img)
                except Exception as e:
                    logger.error(f"Pose detection error: {e}")

            matched_persons = [f["name"] for f in faces if f.get("matched") and f["name"] != "unknown"]
            has_fall = any(p.get("is_fall") for p in poses)

            self.send_json(200, {
                "code": 0,
                "data": {
                    "faces": faces,
                    "poses": poses,
                    "has_fall": has_fall,
                    "matched_persons": matched_persons
                }
            })
            return

        self.send_json(404, {"code": 404, "message": "Not Found"})

    def do_DELETE(self):
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path

        if path.startswith("/api/v1/face/") or path.startswith("/api/v1/vision/persons/"):
            parts = path.strip("/").split("/")
            name = urllib.parse.unquote(parts[-1])
            deleted = face_engine.delete_person(name)
            if deleted:
                self.send_json(200, {"code": 0, "message": f"Person '{name}' deleted"})
            else:
                self.send_json(404, {"code": 404, "message": f"Person '{name}' not found"})
            return

        self.send_json(404, {"code": 404, "message": "Not Found"})

def run():
    server_address = ("0.0.0.0", PORT)
    httpd = ThreadedHTTPServer(server_address, VisionHandler)
    logger.info(f"Vision service running on port {PORT}...")
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        logger.info("Stopping vision service...")
        httpd.server_close()

if __name__ == "__main__":
    run()
