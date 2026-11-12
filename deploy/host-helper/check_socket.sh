#!/usr/bin/env bash
# check_socket.sh - Security and connectivity audit for Home Datacenter Host Bridge Unix Socket
# Usage: ./check_socket.sh [/run/home-datacenter/host-bridge.sock]

set -euo pipefail

SOCKET_PATH="${1:-/run/home-datacenter/host-bridge.sock}"
SOCKET_DIR="$(dirname "$SOCKET_PATH")"
EXIT_CODE=0

echo "=== Home Datacenter Host Helper Socket Security Audit ==="
echo "Target Socket: $SOCKET_PATH"
echo "Target Directory: $SOCKET_DIR"
echo ""

# 1. Audit Socket Parent Directory
if [ ! -e "$SOCKET_DIR" ]; then
    echo "[FAIL] Socket directory $SOCKET_DIR does not exist."
    EXIT_CODE=1
elif [ -L "$SOCKET_DIR" ]; then
    echo "[FAIL] SECURITY VIOLATION: Socket directory $SOCKET_DIR is a symbolic link."
    EXIT_CODE=1
elif [ ! -d "$SOCKET_DIR" ]; then
    echo "[FAIL] Socket directory path $SOCKET_DIR is not a directory."
    EXIT_CODE=1
else
    DIR_PERM=$(stat -c "%a" "$SOCKET_DIR" 2>/dev/null || stat -f "%Op" "$SOCKET_DIR" 2>/dev/null || echo "unknown")
    DIR_UID=$(stat -c "%u" "$SOCKET_DIR" 2>/dev/null || echo "unknown")
    DIR_GID=$(stat -c "%g" "$SOCKET_DIR" 2>/dev/null || echo "unknown")
    DIR_GROUP=$(stat -c "%G" "$SOCKET_DIR" 2>/dev/null || echo "unknown")

    echo "[INFO] Directory stats: mode=$DIR_PERM, uid=$DIR_UID, gid=$DIR_GID ($DIR_GROUP)"

    # Must be owned by root
    if [ "$DIR_UID" != "0" ]; then
        echo "[FAIL] Socket directory $SOCKET_DIR must be owned by root (UID 0), got UID $DIR_UID."
        EXIT_CODE=1
    else
        echo "[PASS] Directory owner is root (UID 0)."
    fi

    # Check other-writable
    OTHER_BIT="${DIR_PERM: -1}"
    if [ "$OTHER_BIT" -ge 2 ]; then
        echo "[FAIL] SECURITY VIOLATION: Socket directory $SOCKET_DIR is other-writable (mode $DIR_PERM)."
        EXIT_CODE=1
    elif [ "$DIR_PERM" -gt 755 ]; then
        echo "[FAIL] Socket directory $SOCKET_DIR permissions $DIR_PERM exceed maximum 0755."
        EXIT_CODE=1
    else
        echo "[PASS] Directory permissions ($DIR_PERM) are secure (<= 0755, non-world-writable)."
    fi
fi

# 2. Audit Socket File
if [ ! -e "$SOCKET_PATH" ]; then
    echo "[FAIL] Socket file $SOCKET_PATH does not exist (is host-helper running?)."
    EXIT_CODE=1
elif [ -L "$SOCKET_PATH" ]; then
    echo "[FAIL] SECURITY VIOLATION: Socket path $SOCKET_PATH is a symbolic link."
    EXIT_CODE=1
elif [ ! -S "$SOCKET_PATH" ]; then
    echo "[FAIL] Path $SOCKET_PATH exists but is not a UNIX domain socket."
    EXIT_CODE=1
else
    SOCK_PERM=$(stat -c "%a" "$SOCKET_PATH" 2>/dev/null || echo "unknown")
    SOCK_UID=$(stat -c "%u" "$SOCKET_PATH" 2>/dev/null || echo "unknown")
    SOCK_GID=$(stat -c "%g" "$SOCKET_PATH" 2>/dev/null || echo "unknown")
    SOCK_GROUP=$(stat -c "%G" "$SOCKET_PATH" 2>/dev/null || echo "unknown")

    echo "[INFO] Socket file stats: mode=$SOCK_PERM, uid=$SOCK_UID, gid=$SOCK_GID ($SOCK_GROUP)"

    # Must not be world-writable or world-readable
    SOCK_OTHER="${SOCK_PERM: -1}"
    if [ "$SOCK_OTHER" -ne 0 ]; then
        echo "[FAIL] SECURITY VIOLATION: Socket $SOCKET_PATH is accessible to others (mode $SOCK_PERM, other=$SOCK_OTHER)."
        EXIT_CODE=1
    elif [ "$SOCK_PERM" != "660" ] && [ "$SOCK_PERM" != "600" ]; then
        echo "[WARN] Expected socket mode 0660 or 0600, got $SOCK_PERM."
    else
        echo "[PASS] Socket permissions ($SOCK_PERM) are secure."
    fi
fi

# 3. Test Bridge Connectivity and Fail-Closed Authentication
if [ -S "$SOCKET_PATH" ]; then
    if command -v curl >/dev/null 2>&1; then
        HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" --max-time 3 --unix-socket "$SOCKET_PATH" "http://localhost/" || echo "ERR")
        if [ "$HTTP_CODE" = "401" ] || [ "$HTTP_CODE" = "403" ]; then
            echo "[PASS] Bridge socket reachable and enforcing fail-closed auth (HTTP $HTTP_CODE on unauthenticated request)."
        elif [ "$HTTP_CODE" = "ERR" ]; then
            echo "[FAIL] Failed to connect to Unix socket via curl."
            EXIT_CODE=1
        else
            echo "[INFO] Bridge responded with HTTP $HTTP_CODE."
        fi
    else
        echo "[SKIP] curl not found, skipping HTTP probe."
    fi
fi

# 4. Check Container Volume Mount Recommendation
if [ -f "docker-compose.yml" ]; then
    if grep -q "${SOCKET_PATH}:${SOCKET_PATH}" docker-compose.yml 2>/dev/null; then
        echo "[WARN] docker-compose.yml mounts direct socket file. Recommended to mount parent directory $SOCKET_DIR to prevent inode invalidation across restarts."
    fi
fi

echo ""
if [ "$EXIT_CODE" -eq 0 ]; then
    echo "=== RESULT: ALL CHECKS PASSED ==="
else
    echo "=== RESULT: SECURITY AUDIT FAILED (code $EXIT_CODE) ==="
fi

exit "$EXIT_CODE"
