#!/bin/bash
set -e
exec > /tmp/cutover.log 2>&1

echo "[$(date '+%F %T')] Auto cutover script started, monitoring rsync PID 29434..."

while kill -0 29434 2>/dev/null; do
    sleep 10
done

echo "[$(date '+%F %T')] Initial rsync completed! Verifying copied data..."
du -sh /mnt/recordings

cd /vol1/docker/home-datacenter

echo "[$(date '+%F %T')] Step 1: Stopping frigate and api for safe atomic cutover..."
docker compose stop frigate api

echo "[$(date '+%F %T')] Step 2: Final delta sync for segments created during copy..."
rsync -a --delete data/frigate/recordings/ /mnt/recordings/

echo "[$(date '+%F %T')] Step 3: Swapping directories on /vol1..."
mv data/frigate/recordings data/frigate/recordings_orig
mkdir -p data/frigate/recordings
chmod 755 data/frigate/recordings

echo "[$(date '+%F %T')] Step 4: Binding /mnt/recordings to data/frigate/recordings..."
mount --bind /mnt/recordings data/frigate/recordings

echo "[$(date '+%F %T')] Step 5: Updating /etc/fstab for persistence across reboots..."
if ! grep -q "/mnt/recordings /vol1/docker/home-datacenter/data/frigate/recordings" /etc/fstab; then
    echo "/mnt/recordings /vol1/docker/home-datacenter/data/frigate/recordings none bind,nofail 0 0" >> /etc/fstab
fi

echo "[$(date '+%F %T')] Step 6: Restarting frigate and api containers..."
docker compose start frigate api

echo "[$(date '+%F %T')] Waiting 5s for health check..."
sleep 5
curl -s http://127.0.0.1:8080/health || true
echo ""

echo "[$(date '+%F %T')] Step 7: Removing old recordings on /vol1 to reclaim 380GB..."
rm -rf data/frigate/recordings_orig

echo "[$(date '+%F %T')] Cutover finished successfully!"
df -h /vol1 /mnt/recordings
