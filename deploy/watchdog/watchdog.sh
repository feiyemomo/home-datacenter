#!/bin/bash
# Standalone Independent Watchdog for home-datacenter
# Probes http://127.0.0.1:8080/healthz and restarts home-api via Host Helper when dead.
# Does NOT require raw docker.sock or host root shell.

set -euo pipefail

TARGET_URL="${WATCHDOG_TARGET_URL:-http://127.0.0.1:8080/healthz}"
CHECK_INTERVAL="${WATCHDOG_CHECK_INTERVAL:-10s}"
FAIL_THRESHOLD="${WATCHDOG_FAIL_THRESHOLD:-3}"
MAX_RESTARTS_PER_HOUR="${WATCHDOG_MAX_RESTARTS:-2}"
BRIDGE_SOCKET="${WATCHDOG_BRIDGE_SOCKET:-/run/home-datacenter/host-bridge.sock}"
BRIDGE_KEY="${OPS_HOST_BRIDGE_HMAC_KEY:-default-bridge-secret-key-min-32-bytes!!}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_PATH="/usr/local/bin/home-datacenter-watchdog"

if [ ! -f "$BIN_PATH" ]; then
    echo "[Watchdog] building watchdog binary..."
    go build -o "$BIN_PATH" "${SCRIPT_DIR}/../../services/api/cmd/watchdog"
fi

exec "$BIN_PATH" \
    -target-url "$TARGET_URL" \
    -interval "$CHECK_INTERVAL" \
    -threshold "$FAIL_THRESHOLD" \
    -max-restarts-per-hour "$MAX_RESTARTS_PER_HOUR" \
    -bridge-socket "$BRIDGE_SOCKET" \
    -hmac-key "$BRIDGE_KEY"
