#!/bin/sh
# Home Datacenter — off-NAS backup entrypoint (v1.8.29).
#
# Runs in the `backup` container (rclone/rclone image). Mounts the
# host's SQLite backup dir (./data/sqlite/backups) read-only at
# /backups and syncs it to the Bitiful (亿安云) S3-compatible bucket
# every BITIFUL_SYNC_INTERVAL seconds.
#
# rclone is configured inline via CLI flags (--s3-*), so credentials
# come from .env and never touch the repo. The "Other" S3 provider +
# custom endpoint is exactly what Bitiful documents
# (https://s3.bitiful.net / cn-east-1).
#
# `sync` (not `copy`) is intentional: it mirrors the local backup
# dir, deleting remote files that the maintenance loop already pruned
# locally, so the bucket never grows stale. `--config /dev/null`
# tells rclone to ignore any default config file.
#
# On failure we log and keep looping — a transient network hiccup
# must not kill the container; the next interval retries.
#
# Status file (v1.8.29): after every sync we `rclone size` the remote
# and write an atomic JSON status to /state/last.json (bind-mounted
# from ./data/backup-state, shared read-only with the api container).
# The api's BackupMonitor reads this to alert on failed/stalled syncs
# and on bucket retention thresholds. Written as temp+rename so a
# reader never sees a half-written file.

set -eu

# Bail out (exit 1) if the required credentials are missing. Compose
# keeps the container running otherwise; better to fail loudly so the
# operator sees it in `docker ps`/logs.
if [ -z "${BITIFUL_ACCESS_KEY:-}" ] || [ -z "${BITIFUL_SECRET_KEY:-}" ]; then
    echo "[backup] ERROR: BITIFUL_ACCESS_KEY / BITIFUL_SECRET_KEY not set. Set them in .env."
    exit 1
fi

# The bind mount is always at /backups regardless of the host path.
SOURCE_DIR="/backups"
DEST=":s3:${BITIFUL_BUCKET}/home-datacenter/sqlite"
INTERVAL="${BITIFUL_SYNC_INTERVAL:-21600}"
STATE_FILE="${STATE_FILE:-/state/last.json}"

# ONCE=1 runs a single sync + status write then exits — used by the
# restore-dry-run script and manual triggers. The container main loop
# calls this in a while loop instead.
once="${ONCE:-0}"

log() { echo "[backup] $(date '+%F %T') $*"; }

# write_status atomically writes the state JSON. $1 = ok("1"/"0"),
# $2 = error message (optional).
write_status() {
    ok="$1"
    err="${2:-}"
    ts="$(date +%s)"
    files="0"
    bytes="0"
    if [ "$ok" = "1" ]; then
        # Parse rclone size output, e.g.:
        #   Total objects: 1
        #   Total size: 700 KiB (716800 Byte)
        # We take the exact byte count from the parenthesised value.
        size_out="$(rclone size "$DEST" \
            --s3-provider Other \
            --s3-access-key-id "${BITIFUL_ACCESS_KEY}" \
            --s3-secret-access-key "${BITIFUL_SECRET_KEY}" \
            --s3-endpoint "${BITIFUL_ENDPOINT}" \
            --s3-region "${BITIFUL_REGION}" \
            --config /dev/null 2>/dev/null || true)"
        files="$(printf '%s\n' "$size_out" | sed -n 's/^Total objects:[[:space:]]*\([0-9][0-9]*\).*/\1/p' | head -n1)"
        bytes="$(printf '%s\n' "$size_out" | sed -n 's/.*(\([0-9][0-9]*\)[[:space:]]*Byte).*/\1/p' | head -n1)"
        [ -n "$files" ] || files="0"
        [ -n "$bytes" ] || bytes="0"
    fi
    # ok is a JSON boolean (true/false), not 0/1 — the api's BackupMonitor
    # unmarshals it into a Go bool.
    if [ "$ok" = "1" ]; then
        ok_bool="true"
    else
        ok_bool="false"
    fi
    tmp="${STATE_FILE}.tmp.$$"
    printf '{"ts":%s,"ok":%s,"error":"%s","remote_files":%s,"remote_bytes":%s}\n' \
        "$ts" "$ok_bool" "$err" "$files" "$bytes" > "$tmp"
    mv -f "$tmp" "$STATE_FILE"
    log "status written: ok=$ok_bool files=$files bytes=$bytes"
}

sync_once() {
    log "syncing SQLite backups -> Bitiful bucket"
    if rclone sync "$SOURCE_DIR" "$DEST" \
        --s3-provider Other \
        --s3-access-key-id "${BITIFUL_ACCESS_KEY}" \
        --s3-secret-access-key "${BITIFUL_SECRET_KEY}" \
        --s3-endpoint "${BITIFUL_ENDPOINT}" \
        --s3-region "${BITIFUL_REGION}" \
        --config /dev/null \
        --transfers 4 \
        --checkers 8 \
        --verbose 2>&1; then
        log "sync OK"
        write_status 1
    else
        log "sync FAILED"
        write_status 0 "rclone sync failed"
        return 1
    fi
}

log "rclone backup loop started (interval ${INTERVAL}s, source ${SOURCE_DIR}, dest s3://${BITIFUL_BUCKET}/home-datacenter/sqlite)"

if [ "$once" = "1" ]; then
    sync_once
    exit $?
fi

while true; do
    sync_once || true
    sleep "${INTERVAL}"
done