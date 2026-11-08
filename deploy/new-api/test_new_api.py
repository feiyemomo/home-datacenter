#!/usr/bin/env python3
"""
test_new_api.py — Verification suite for New API Compose & Bootstrap Channels
"""

import os
import sys
import yaml
import sqlite3
import tempfile
import subprocess

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
COMPOSE_FILE = os.path.join(SCRIPT_DIR, "compose.yaml")
BOOTSTRAP_SCRIPT = os.path.join(SCRIPT_DIR, "bootstrap_channels.py")


def test_compose_configuration():
    print("[TEST] 1. Validating compose.yaml specifications...")
    assert os.path.exists(COMPOSE_FILE), f"compose.yaml missing at {COMPOSE_FILE}"

    with open(COMPOSE_FILE, "r", encoding="utf-8") as f:
        compose = yaml.safe_load(f)

    service = compose["services"]["new-api"]
    assert service["image"] == "channels-tool/new-api:latest", f"Unexpected image: {service['image']}"

    # Port binding check
    ports = service.get("ports", [])
    assert any("127.0.0.1:3000:3000" in str(p) for p in ports), f"Port 127.0.0.1:3000:3000 not bound: {ports}"

    # Memory limit check
    limits = service["deploy"]["resources"]["limits"]
    mem_limit = limits.get("memory", "")
    assert mem_limit in ["128M", "128MB", "100M", "64M"], f"Memory limit exceeded or invalid: {mem_limit}"

    # Proxy check
    env = service.get("environment", {})
    env_dict = {}
    if isinstance(env, list):
        for item in env:
            k, v = item.split("=", 1)
            env_dict[k] = v
    else:
        env_dict = env

    assert env_dict.get("HTTP_PROXY") == "http://172.17.0.1:7890", f"Bad HTTP_PROXY: {env_dict.get('HTTP_PROXY')}"
    assert env_dict.get("HTTPS_PROXY") == "http://172.17.0.1:7890", f"Bad HTTPS_PROXY: {env_dict.get('HTTPS_PROXY')}"
    assert "192.168.0.0/16" in env_dict.get("NO_PROXY", ""), f"Bad NO_PROXY: {env_dict.get('NO_PROXY')}"
    print("    [PASS] compose.yaml verified: image, 128MB limit, local port, Mihomo proxy")


def test_bootstrap_dry_run():
    print("[TEST] 2. Testing bootstrap_channels.py --dry-run...")
    cmd = [sys.executable, BOOTSTRAP_SCRIPT, "--dry-run"]
    res = subprocess.run(cmd, capture_output=True, text=True)
    assert res.returncode == 0, f"Dry-run failed with code {res.returncode}:\n{res.stderr}"
    stdout = res.stdout

    # Assert 11 keys loaded
    assert "5 Google AI Studio keys" in stdout, "Missing 5 Google keys in output"
    assert "6 Groq keys" in stdout, "Missing 6 Groq keys in output"
    assert "Total Active Pool Size: 11 keys" in stdout, "Total pool size != 11"

    # Assert all model mapping aliases
    assert "agent-fast" in stdout and "gemini-3.5-flash-lite,gemini-3.1-flash-lite" in stdout
    assert "agent-deep" in stdout and "gemini-3.8-flash,gpt-oss-120b" in stdout
    assert "agent-guard" in stdout and "qwen/qwen3.8-27b,meta-llama/llama-prompt-guard-2-86m" in stdout
    assert "agent-fallback" in stdout and "gemma-4-31b" in stdout

    print("    [PASS] bootstrap_channels.py dry-run: 11 real keys & 4 model aliases verified")


def test_bootstrap_db_seeding():
    print("[TEST] 3. Testing SQLite DB seeding mode...")
    with tempfile.NamedTemporaryFile(suffix=".db", delete=False) as tmp:
        db_path = tmp.name

    try:
        cmd = [sys.executable, BOOTSTRAP_SCRIPT, "--db", db_path]
        res = subprocess.run(cmd, capture_output=True, text=True)
        assert res.returncode == 0, f"DB seeding failed:\n{res.stderr}"

        conn = sqlite3.connect(db_path)
        cur = conn.cursor()
        cur.execute("SELECT name, type, key, models, model_mapping, weight FROM channels")
        rows = cur.fetchall()
        conn.close()

        assert len(rows) == 2, f"Expected 2 channels, got {len(rows)}"

        # Channel 1: Google
        google_ch = next(r for r in rows if "Google" in r[0])
        google_keys = google_ch[2].splitlines()
        assert len(google_keys) == 5, f"Expected 5 Google keys, got {len(google_keys)}"
        assert google_ch[1] == 14, f"Expected type 14 for Google Gemini, got {google_ch[1]}"
        assert google_ch[5] == 10, f"Expected weight 10, got {google_ch[5]}"
        assert "agent-fast" in google_ch[4]

        # Channel 2: Groq
        groq_ch = next(r for r in rows if "Groq" in r[0])
        groq_keys = groq_ch[2].splitlines()
        assert len(groq_keys) == 6, f"Expected 6 Groq keys, got {len(groq_keys)}"
        assert groq_ch[1] == 8, f"Expected type 8 for Groq, got {groq_ch[1]}"
        assert groq_ch[5] == 10, f"Expected weight 10, got {groq_ch[5]}"
        assert "agent-guard" in groq_ch[4]
        assert "agent-fallback" in groq_ch[4]

        print("    [PASS] SQLite seeding: 2 channels, 11 keys in round-robin, mappings confirmed")
    finally:
        if os.path.exists(db_path):
            os.remove(db_path)


def main():
    print("============================================================")
    print("         New API Suite Validation & Integration Tests       ")
    print("============================================================")
    test_compose_configuration()
    test_bootstrap_dry_run()
    test_bootstrap_db_seeding()
    print("\nALL NEW API VERIFICATION TESTS PASSED SUCCESSFULLY!")


if __name__ == "__main__":
    main()
