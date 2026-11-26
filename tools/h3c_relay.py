#!/usr/bin/env python3
"""
h3c_relay.py — H3C Oasis High-Speed Relay Manager & Dynamic Endpoint Resolver.

Provides automatic 3-tier endpoint resolution for deployment and publishing scripts:
  Tier 1: Local Gigabit LAN (192.168.31.234:22) — instantaneous (< 1s probe)
  Tier 2: H3C Oasis Domestic High-Speed Relay (154.8.195.220:<dynamic_port>)
          Automatically inspects/creates a 1-hour SSH tunnel, providing 2-10 MB/s WAN transfer.
  Tier 3: Tailscale Overlay (100.90.67.71:22) — reliable fallback if Oasis is unreachable.

Usage as CLI:
  python h3c_relay.py acquire --json
  python h3c_relay.py release <tunnel_id>
  python h3c_relay.py status

Usage as Python Module:
  from tools.h3c_relay import H3CRelayContext, resolve_best_endpoint
  with H3CRelayContext(app_name="ssh", auto_cleanup=True) as ep:
      print(f"Connecting to {ep.host}:{ep.port} ({ep.tier})...")
"""

import argparse
import json
import logging
import os
import socket
import sys
import time

# Ensure deploy/h3c-keepalive is in sys.path to leverage production H3CClient
_REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
_KEEPALIVE_DIR = os.path.join(_REPO_ROOT, "deploy", "h3c-keepalive")
if _KEEPALIVE_DIR not in sys.path:
    sys.path.insert(0, _KEEPALIVE_DIR)

from h3c_client import H3CClient

# Default configurations
DEFAULT_H3C_USER = os.environ.get("H3C_USERNAME", "momo324")
DEFAULT_H3C_PASS = os.environ.get("H3C_PASSWORD", "@Momo324")
DEFAULT_PROJECT_ID = os.environ.get("H3C_PROJECT_ID", "10233048")
DEFAULT_NAS_LAN_IP = os.environ.get("NAS_LAN_IP", "192.168.31.234")
DEFAULT_TAILSCALE_IP = os.environ.get("NAS_TAILSCALE_IP", "100.90.67.71")
DEFAULT_SSH_USER = os.environ.get("NAS_USER", "momo")

logger = logging.getLogger("h3c_relay")


def probe_tcp_port(host, port, timeout_sec=0.8, is_ssh=True):
    """
    Returns True if host:port is reachable. If is_ssh is True, validates
    that the endpoint returns the standard SSH identification banner (RFC 4253 §4.2)
    to prevent false positives from local proxy/TUN synthetic sockets.
    """
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.settimeout(timeout_sec)
        s.connect((host, int(port)))
        if is_ssh:
            banner = s.recv(64)
            s.close()
            return banner.startswith(b"SSH-")
        s.close()
        return True
    except Exception:
        return False


class RelayEndpoint:
    def __init__(self, tier, host, port, user=DEFAULT_SSH_USER, tunnel_id=None, created_new=False):
        self.tier = tier                  # "lan" | "h3c" | "tailscale"
        self.host = host
        self.port = int(port)
        self.user = user
        self.tunnel_id = tunnel_id
        self.created_new = created_new

    def to_dict(self):
        return {
            "tier": self.tier,
            "host": self.host,
            "port": self.port,
            "user": self.user,
            "tunnel_id": self.tunnel_id,
            "created_new": self.created_new,
            "endpoint": f"{self.host}:{self.port}"
        }


def resolve_best_endpoint(app_name="ssh", internal_port=22, min_remaining_minutes=5.0, prefer_h3c=True, force_h3c=False):
    """
    Resolves the fastest available endpoint across LAN, H3C Oasis, and Tailscale.
    """
    is_ssh = (internal_port == 22 or app_name == "ssh")

    # 1. Tier 1: Probe LAN (unless force_h3c is set)
    if not force_h3c and probe_tcp_port(DEFAULT_NAS_LAN_IP, internal_port, timeout_sec=0.8, is_ssh=is_ssh):
        return RelayEndpoint(
            tier="lan",
            host=DEFAULT_NAS_LAN_IP,
            port=internal_port,
            user=DEFAULT_SSH_USER,
            tunnel_id=None,
            created_new=False
        )

    # 2. Tier 2: H3C Oasis Relay
    if prefer_h3c:
        try:
            client = H3CClient(DEFAULT_H3C_USER, DEFAULT_H3C_PASS)
            existing = client.get_established_tunnel(app_name)
            if existing and existing.get("remainingMinutes", 0) > min_remaining_minutes:
                ext = existing.get("externalAddr", "")
                if ":" in ext:
                    h, p = ext.split(":")
                    if probe_tcp_port(h, int(p), timeout_sec=2.0, is_ssh=is_ssh):
                        return RelayEndpoint(
                            tier="h3c",
                            host=h,
                            port=int(p),
                            user=DEFAULT_SSH_USER,
                            tunnel_id=existing.get("tunnelId"),
                            created_new=False
                        )

            # Need to create/renew tunnel
            app_info = client.find_app(DEFAULT_PROJECT_ID, app_name)
            if app_info:
                # If old expired tunnel exists, delete it first
                old_id = (existing.get("tunnelId") if existing else None) or app_info.get("tunnelId")
                if old_id:
                    try:
                        client.delete_tunnel(old_id)
                        time.sleep(1)
                    except Exception:
                        pass

                st = client.create_and_wait_tunnel(
                    app_name=app_name,
                    internal_port=internal_port,
                    project_id=DEFAULT_PROJECT_ID,
                    valid_time=1,
                    timeout_sec=30,
                    internal_ip=DEFAULT_NAS_LAN_IP
                )
                ext = st.get("externalAddr", "")
                if ":" in ext:
                    h, p = ext.split(":")
                    if probe_tcp_port(h, int(p), timeout_sec=3.0, is_ssh=is_ssh):
                        return RelayEndpoint(
                            tier="h3c",
                            host=h,
                            port=int(p),
                            user=DEFAULT_SSH_USER,
                            tunnel_id=st.get("tunnelId"),
                            created_new=True
                        )
                    logger.warning(f"H3C Oasis tunnel {h}:{p} established in control plane but failed data-plane probe; falling back.")
        except Exception as e:
            logger.warning(f"H3C Oasis tunnel acquisition failed: {e}")

    # 3. Tier 3: Tailscale fallback
    return RelayEndpoint(
        tier="tailscale",
        host=DEFAULT_TAILSCALE_IP,
        port=internal_port,
        user=DEFAULT_SSH_USER,
        tunnel_id=None,
        created_new=False
    )


def release_tunnel(tunnel_id):
    """Safely releases/deletes an H3C Oasis relay tunnel by ID."""
    if not tunnel_id:
        return False
    try:
        client = H3CClient(DEFAULT_H3C_USER, DEFAULT_H3C_PASS)
        client.delete_tunnel(tunnel_id)
        return True
    except Exception as e:
        logger.warning(f"Failed to release tunnel {tunnel_id}: {e}")
        return False


class H3CRelayContext:
    """
    Context manager that acquires the fastest endpoint and safely cleans up temporary H3C tunnels.
    """
    def __init__(self, app_name="ssh", auto_cleanup=True, force_h3c=False):
        self.app_name = app_name
        self.auto_cleanup = auto_cleanup
        self.force_h3c = force_h3c
        self.endpoint = None

    def __enter__(self):
        self.endpoint = resolve_best_endpoint(app_name=self.app_name, force_h3c=self.force_h3c)
        return self.endpoint

    def __exit__(self, exc_type, exc_val, exc_tb):
        if self.auto_cleanup and self.endpoint and self.endpoint.created_new and self.endpoint.tunnel_id:
            release_tunnel(self.endpoint.tunnel_id)


def main():
    parser = argparse.ArgumentParser(description="H3C Oasis High-Speed Relay Manager")
    subparsers = parser.add_subparsers(dest="action", help="Action to perform")

    p_acquire = subparsers.add_parser("acquire", help="Acquire fastest available endpoint")
    p_acquire.add_argument("--json", action="store_true", help="Output JSON object")
    p_acquire.add_argument("--force-h3c", action="store_true", help="Bypass LAN probe and force H3C Oasis relay")
    p_acquire.add_argument("--app", default="ssh", help="Oasis application name")
    p_acquire.add_argument("--port", type=int, default=22, help="Internal port")

    p_release = subparsers.add_parser("release", help="Release an active tunnel")
    p_release.add_argument("tunnel_id", help="Tunnel ID to release")

    subparsers.add_parser("status", help="List active Oasis tunnels")

    args = parser.parse_args()

    if args.action == "acquire":
        ep = resolve_best_endpoint(app_name=args.app, internal_port=args.port, force_h3c=args.force_h3c)
        if args.json:
            print(json.dumps(ep.to_dict()))
        else:
            print(f"{ep.tier} {ep.host}:{ep.port}")
    elif args.action == "release":
        client = H3CClient(DEFAULT_H3C_USER, DEFAULT_H3C_PASS)
        client.delete_tunnel(args.tunnel_id)
        print(f"Released tunnel: {args.tunnel_id}")
    elif args.action == "status":
        client = H3CClient(DEFAULT_H3C_USER, DEFAULT_H3C_PASS)
        tunnels = client.get_tunnels()
        print(json.dumps(tunnels, indent=2, ensure_ascii=False))
    else:
        parser.print_help()


if __name__ == "__main__":
    main()
