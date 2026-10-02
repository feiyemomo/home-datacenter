#!/bin/sh
# Home Datacenter — Frigate surveillance video archiving to Quark Drive via Alist WebDAV (v1.8.34).
#
# Syncs Frigate recordings (/recordings: ./data/frigate/recordings) to
# Quark Drive via Alist WebDAV (http://home-alist:5244/dav/quark/Surveillance).
#
# Strategy:
#   - Uses `rclone copy` with `--min-age ${ARCHIVE_MIN_AGE:-2h}` to ensure only completed
#     MP4 segments are archived, avoiding in-progress recordings.
#   - Runs periodically every ${ARCHIVE_INTERVAL:-21600} seconds (default: 6 hours).
#   - Does not delete remote archives, allowing Quark Drive to act as cold retention.
#     Local NAS disk retention is managed by Frigate (record.continuous.days: 7).

set -eu

SOURCE_DIR="/recordings"
ALIST_URL="${ALIST_WEBDAV_URL:-http://home-alist:5244/dav}"
ALIST_USER="${ALIST_WEBDAV_USER:-admin}"
ALIST_PASS="${ALIST_WEBDAV_PASS:-b8NBAhGkRvBju0j7Tlu2bQoAlAtghQupSpA}"
REMOTE_PATH="${QUARK_REMOTE_PATH:-quark/Surveillance/Recordings}"
INTERVAL="${ARCHIVE_INTERVAL:-21600}"
MIN_AGE="${ARCHIVE_MIN_AGE:-2h}"
STATE_FILE="${STATE_FILE:-/state/recordings-archive.json}"
ONCE="${ONCE:-0}"

log() { echo "[recordings-archive] $(date '+%F %T') $*"; }

archive_once() {
    log "Starting incremental archive of recordings older than ${MIN_AGE} to Quark WebDAV..."
    ts="$(date +%s)"
    
    if rclone copy "$SOURCE_DIR" ":webdav:${REMOTE_PATH}" \
        --webdav-url "$ALIST_URL" \
        --webdav-vendor other \
        --webdav-user "$ALIST_USER" \
        --webdav-pass "$ALIST_PASS" \
        --min-age "$MIN_AGE" \
        --transfers 2 \
        --checkers 4 \
        --fast-list \
        --config /dev/null \
        --verbose 2>&1; then
        log "Archive completed successfully."
        tmp="${STATE_FILE}.tmp.$$"
        printf '{"ts":%s,"ok":true,"error":""}\n' "$ts" > "$tmp"
        mv -f "$tmp" "$STATE_FILE"
    else
        log "Archive encountered errors. Will retry next interval."
        tmp="${STATE_FILE}.tmp.$$"
        printf '{"ts":%s,"ok":false,"error":"rclone copy failed"}\n' "$ts" > "$tmp"
        mv -f "$tmp" "$STATE_FILE"
        return 1
    fi
}

log "Recordings archiver started (interval: ${INTERVAL}s, min-age: ${MIN_AGE}, dest: ${ALIST_URL}/${REMOTE_PATH})"

if [ "$ONCE" = "1" ]; then
    archive_once
    exit $?
fi

while true; do
    archive_once || true
    sleep "${INTERVAL}"
done
