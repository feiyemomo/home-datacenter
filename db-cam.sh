#!/bin/bash
# db-cam.sh — Check camera config in DB + fast ARP scan
set +e

echo "===== 1. Camera config in DB ====="
cd /vol1/docker/home-datacenter
DB_PATH=$(find . -name "*.db" -type f 2>/dev/null | head -5)
echo "DB files found: $DB_PATH"

# Try via docker exec (sqlite3 may not be on host)
for db in $DB_PATH; do
    echo "--- $db ---"
    docker exec home-api sqlite3 "/app/$db" "SELECT id, name, host, rtsp_port, channel, enabled FROM cameras;" 2>&1
done

# Also try absolute path inside container
echo "--- try /app/data/home.db ---"
docker exec home-api sqlite3 /app/data/home.db "SELECT id, name, host, rtsp_port, channel, enabled FROM cameras;" 2>&1
echo

echo "===== 2. ip neigh (ARP table) ====="
ip neigh show 2>&1
echo

echo "===== 3. Quick ping sweep (parallel, 0.5s timeout) ====="
for i in $(seq 1 254); do
    ping -c 1 -W 1 192.168.1.$i &>/dev/null && echo "192.168.1.$i ALIVE" &
done
wait
echo

echo "===== DONE ====="
