#!/bin/sh
# Home Datacenter — Frigate surveillance video archiving to Lanzou Cloud via Alist WebDAV.
#
# Syncs Frigate recordings (/recordings: ./data/frigate/recordings) to
# Lanzou Cloud via Alist WebDAV (http://home-alist:5244/dav/lanzou/Surveillance).
#
# Strategy:
#   - Runs daily at 03:00 AM (local time) to avoid daytime bandwidth usage and rate limits.
#   - Uses `rclone copy` to sync completed recordings older than ${MIN_AGE:-7d}.
#   - Frigate retains 7 days locally; recordings older than 7 days are archived to Lanzou.
#   - On-demand playback: Go backend fetches the 3.6MB slice on click for instant viewing.

set -eu

SOURCE_DIR="/recordings"
ALIST_URL="${ALIST_WEBDAV_URL:-http://home-alist:5244/dav}"
ALIST_USER="${ALIST_WEBDAV_USER:-admin}"
ALIST_PASS="${ALIST_WEBDAV_PASS:-wm10050817}"
REMOTE_PATH="${ARCHIVE_REMOTE_PATH:-${QUARK_REMOTE_PATH:-lanzou/Surveillance/Recordings}}"
MIN_AGE="${ARCHIVE_MIN_AGE:-6d}"
RETENTION_DAYS="${ARCHIVE_RETENTION_DAYS:-${QUARK_RETENTION_DAYS:-0}}"
STATE_FILE="${STATE_FILE:-/state/recordings-archive.json}"
ONCE="${ONCE:-0}"
TARGET_HOUR="${SCHEDULE_HOUR:-03}"

log() { echo "[recordings-archive] $(date '+%F %T') $*"; }

# Automatically obscure password for rclone webdav backend
if echo "$ALIST_PASS" | grep -q '^[a-zA-Z0-9_-]\{20,\}$' 2>/dev/null; then
    OBSCURED_PASS="$ALIST_PASS"
else
    OBSCURED_PASS="$(rclone obscure "$ALIST_PASS" 2>/dev/null || echo "$ALIST_PASS")"
fi

cleanup_old_recordings() {
    if [ -n "${RETENTION_DAYS:-}" ] && [ "$RETENTION_DAYS" -gt 0 ] 2>/dev/null; then
        log "Lifecycle check: pruning remote recordings older than ${RETENTION_DAYS} days..."
        rclone delete ":webdav:${REMOTE_PATH}" \
            --webdav-url "$ALIST_URL" \
            --webdav-vendor other \
            --webdav-user "$ALIST_USER" \
            --webdav-pass "$OBSCURED_PASS" \
            --min-age "${RETENTION_DAYS}d" \
            --config /dev/null \
            --verbose 2>&1 || true

        rclone rmdirs ":webdav:${REMOTE_PATH}" \
            --webdav-url "$ALIST_URL" \
            --webdav-vendor other \
            --webdav-user "$ALIST_USER" \
            --webdav-pass "$OBSCURED_PASS" \
            --leave-root \
            --config /dev/null 2>/dev/null || true
        log "Lifecycle pruning completed."
    fi
}

archive_once() {
    # Refresh dynamic configuration right before archiving
    if [ -f "/state/storage-config.json" ]; then
        DYN_MIN_AGE="$(grep -o '"archive_min_age_days":[0-9]*' /state/storage-config.json 2>/dev/null | cut -d: -f2 || true)"
        if [ -n "$DYN_MIN_AGE" ] && [ "$DYN_MIN_AGE" -gt 0 ] 2>/dev/null; then
            MIN_AGE="${DYN_MIN_AGE}d"
        fi
    fi

    log "Starting archive of recordings older than ${MIN_AGE} to ${REMOTE_PATH}..."
    ts="$(date +%s)"
    
    DAYS="$(echo "$MIN_AGE" | tr -dc '0-9')"
    [ -z "$DAYS" ] && DAYS=6

    STAGING_DIR="$SOURCE_DIR/.archive-staging"
    rm -rf "$STAGING_DIR" 2>/dev/null || true
    mkdir -p "$STAGING_DIR"

    NOW_TS="$(date +%s)"
    CUTOFF_DATE="$(date -d "@$(( NOW_TS - DAYS * 86400 ))" +%Y-%m-%d 2>/dev/null)"
    log "Evaluating recording dates older than or equal to ${CUTOFF_DATE} (cutoff DAYS=${DAYS})..."

    COUNT=0
    for date_path in "$SOURCE_DIR"/20[0-9][0-9]-[0-9][0-9]-[0-9][0-9]; do
        [ -d "$date_path" ] || continue
        DATE="$(basename "$date_path")"
        if [ "$DATE" \< "$CUTOFF_DATE" ] || [ "$DATE" = "$CUTOFF_DATE" ]; then
            log "Found eligible archive date folder: $DATE"
            TARGET_DIR="$STAGING_DIR/$DATE"
            mkdir -p "$TARGET_DIR"

            for hour_path in "$date_path"/*; do
                [ -d "$hour_path" ] || continue
                HOUR="$(basename "$hour_path")"
                for cam_path in "$hour_path"/*; do
                    [ -d "$cam_path" ] || continue
                    SLUG="$(basename "$cam_path")"
                    for f in "$cam_path"/*.mp4; do
                        [ -f "$f" ] || continue
                        FILE="$(basename "$f")"
                        TARGET_FILE="$TARGET_DIR/${SLUG}_${HOUR}_${FILE}"
                        ln -f "$f" "$TARGET_FILE" 2>/dev/null || cp "$f" "$TARGET_FILE" 2>/dev/null || true
                        COUNT=$((COUNT + 1))
                    done
                done
            done
        fi
    done

    log "Prepared ${COUNT} segment hardlinks for remote transfer (flattened 3-level format)..."

    if [ "$COUNT" -gt 0 ]; then
        if rclone copy "$STAGING_DIR" ":webdav:${REMOTE_PATH}" \
            --webdav-url "$ALIST_URL" \
            --webdav-vendor other \
            --webdav-user "$ALIST_USER" \
            --webdav-pass "$OBSCURED_PASS" \
            --ignore-size \
            --transfers 4 \
            --checkers 8 \
            --fast-list \
            --config /dev/null \
            --verbose 2>&1; then
            log "Archive completed successfully (${COUNT} segments transferred/synced)."
            tmp="${STATE_FILE}.tmp.$$"
            printf '{"ts":%s,"ok":true,"error":"","count":%d}\n' "$ts" "$COUNT" > "$tmp"
            mv -f "$tmp" "$STATE_FILE"
        else
            log "Archive encountered errors. Will retry next run."
            tmp="${STATE_FILE}.tmp.$$"
            printf '{"ts":%s,"ok":false,"error":"rclone copy failed"}\n' "$ts" > "$tmp"
            mv -f "$tmp" "$STATE_FILE"
            rm -rf "$STAGING_DIR" 2>/dev/null || true
            return 1
        fi
    else
        log "No recordings older than ${MIN_AGE} to archive."
        tmp="${STATE_FILE}.tmp.$$"
        printf '{"ts":%s,"ok":true,"error":"","count":0}\n' "$ts" > "$tmp"
        mv -f "$tmp" "$STATE_FILE"
    fi

    rm -rf "$STAGING_DIR" 2>/dev/null || true
    cleanup_old_recordings
}

log "Recordings archiver initialized (target hour: ${TARGET_HOUR}:00, min-age: ${MIN_AGE}, dest: ${ALIST_URL}/${REMOTE_PATH})"

if [ "$ONCE" = "1" ]; then
    archive_once
    exit $?
fi

# Main scheduler loop: checks every 30 seconds, runs once daily at TARGET_HOUR:00
LAST_RUN_DAY=""
while true; do
    # Check for dynamic configuration updates from API
    if [ -f "/state/storage-config.json" ]; then
        DYN_HOUR="$(grep -o '"archive_schedule_hour":[0-9]*' /state/storage-config.json 2>/dev/null | cut -d: -f2 || true)"
        if [ -n "$DYN_HOUR" ]; then
            TARGET_HOUR="$(printf "%02d" "$DYN_HOUR")"
        fi
        DYN_MIN_AGE="$(grep -o '"archive_min_age_days":[0-9]*' /state/storage-config.json 2>/dev/null | cut -d: -f2 || true)"
        if [ -n "$DYN_MIN_AGE" ] && [ "$DYN_MIN_AGE" -gt 0 ] 2>/dev/null; then
            MIN_AGE="${DYN_MIN_AGE}d"
        fi
    fi

    # Check for manual trigger file from Web/App API
    if [ -f "/state/trigger" ]; then
        log "Manual trigger detected (/state/trigger), running archive immediately..."
        rm -f "/state/trigger" 2>/dev/null || true
        archive_once || true
    fi

    CUR_DAY="$(date +%Y-%m-%d)"
    CUR_HOUR="$(date +%H)"
    CUR_MIN="$(date +%M)"

    if [ "$CUR_HOUR" = "$TARGET_HOUR" ] && [ "$CUR_MIN" = "00" ] && [ "$LAST_RUN_DAY" != "$CUR_DAY" ]; then
        log "Scheduled trigger reached at ${TARGET_HOUR}:00 on ${CUR_DAY}"
        archive_once || true
        LAST_RUN_DAY="$CUR_DAY"
        sleep 65
    else
        sleep 30
    fi
done
