#!/usr/bin/env bash
# Shell runner for Home Datacenter E2E test track
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PYTHON_CMD="python3"

if ! command -v "$PYTHON_CMD" &> /dev/null; then
    PYTHON_CMD="python"
fi

exec "$PYTHON_CMD" "${SCRIPT_DIR}/run_e2e.py" "$@"
