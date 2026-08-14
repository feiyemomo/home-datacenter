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

echo "[backup] rclone backup loop started (interval ${INTERVAL}s, source ${SOURCE_DIR}, dest s3://${BITIFUL_BUCKET}/home-datacenter/sqlite)"

while true; do
    echo "[backup] $(date '+%F %T') syncing SQLite backups -> Bitiful bucket"
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
        echo "[backup] $(date '+%F %T') sync OK"
    else
        echo "[backup] $(date '+%F %T') sync FAILED (will retry next interval)"
    fi
    sleep "${INTERVAL}"
done