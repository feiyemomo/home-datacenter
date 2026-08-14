#!/bin/sh
# Home Datacenter — off-NAS backup restore drill (v1.8.29).
#
# Verifies the off-NAS (Bitiful / 亿安云) backup is actually recoverable
# by pulling the latest snapshot from the bucket into a temp dir and
# checking it with sqlite. This is the "restore SOP" — run it regularly
# (e.g. monthly) to prove that a NAS disk failure would NOT lose the
# database.
#
# It does NOT touch the live DB or the maintenance backups; it only
# reads from the bucket and writes to a throwaway temp dir. It is
# safe to run any time.
#
# Usage:
#   ./scripts/restore-dry-run.sh            # on the NAS host (docker + python3)
#   NAS=<host> ./scripts/restore-dry-run.sh # from a machine with ssh access
#
# Exit codes: 0 = healthy backup confirmed, 1 = something failed.
#
# Dependencies on the NAS:
#   - the `backup` container (home-backup) with rclone image
#   - python3 (for sqlite integrity check) — present on fnOS

set -eu

# --- Configuration -----------------------------------------------------------
# Directory where the project lives on the NAS. Override with PROJ_DIR.
PROJ_DIR="${PROJ_DIR:-/vol1/docker/home-datacenter}"
BACKUP_CONTAINER="${BACKUP_CONTAINER:-home-backup}"
TMP_BASE="${TMP_BASE:-/tmp/restore-drill}"
REMOTE_DIR="home-datacenter/sqlite"

# --- Remote execution helper -------------------------------------------------
# If invoked on the NAS directly, run commands locally. Otherwise ssh.
run() {
    if [ -z "${NAS:-}" ]; then
        "$@"
    else
        ssh -o BatchMode=yes -o ConnectTimeout=10 "${NAS}" "$@"
    fi
}

echo "== restore drill: fetch latest backup from Bitiful bucket =="

# 1. List what's in the bucket (via the backup container, which holds
#    the rclone image + credentials as env vars).
echo "--- remote objects ---"
run docker exec "${BACKUP_CONTAINER}" sh -c "rclone lsl \":s3:\${BITIFUL_BUCKET}/${REMOTE_DIR}\" --s3-provider Other --s3-access-key-id \"\$BITIFUL_ACCESS_KEY\" --s3-secret-access-key \"\$BITIFUL_SECRET_KEY\" --s3-endpoint \"\$BITIFUL_ENDPOINT\" --s3-region \"\$BITIFUL_REGION\" --config /dev/null"

# 2. Pull the newest app-*.db snapshot into the container's temp dir.
echo "--- fetch newest app-*.db ---"
run docker exec "${BACKUP_CONTAINER}" sh -c "mkdir -p ${TMP_BASE} && rclone copy \":s3:\${BITIFUL_BUCKET}/${REMOTE_DIR}/\" ${TMP_BASE} --s3-provider Other --s3-access-key-id \"\$BITIFUL_ACCESS_KEY\" --s3-secret-access-key \"\$BITIFUL_SECRET_KEY\" --s3-endpoint \"\$BITIFUL_ENDPOINT\" --s3-region \"\$BITIFUL_REGION\" --config /dev/null --include 'app-*.db'"

# 3. Copy the newest file out of the container to the host temp dir.
run docker cp "${BACKUP_CONTAINER}:${TMP_BASE}" "${TMP_BASE}"

# 4. Pick the newest app-*.db.
RESTORED="$(run sh -c "ls -t ${TMP_BASE}/app-*.db | head -n1")"
RESTORED="$(basename "${RESTORED}")"
echo "== restored: ${RESTORED} =="

# 5. Integrity check with python3 sqlite module.
echo "--- integrity_check ---"
run python3 -c "
import sqlite3, glob, os
dbs = sorted(glob.glob('${TMP_BASE}/app-*.db'))
assert dbs, 'no app-*.db restored'
db = dbs[-1]
con = sqlite3.connect(db)
ok = con.execute('PRAGMA integrity_check;').fetchone()[0]
print('file:', os.path.basename(db), 'size:', os.path.getsize(db), 'integrity:', ok)
assert ok == 'ok', 'integrity check FAILED'
users = con.execute('SELECT COUNT(*) FROM users;').fetchone()[0]
cams  = con.execute('SELECT COUNT(*) FROM cameras;').fetchone()[0]
print('users:', users, 'cameras:', cams)
con.close()
print('RESTORE OK')
"

# 6. Clean up the temp copies.
run rm -rf "${TMP_BASE}"
echo "== drill complete: backup is recoverable =="