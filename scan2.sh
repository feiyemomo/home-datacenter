#!/bin/bash
# scan2.sh — Better camera scan using ip neigh + ping sweep + docker nmap
set +e

echo "===== 1. ip neigh (ARP table) ====="
ip neigh show 2>&1
echo

echo "===== 2. Ping sweep 192.168.1.0/24 (fast, 0.1s timeout each) ====="
for i in $(seq 1 254); do
    ping -c 1 -W 1 192.168.1.$i &>/dev/null && echo "192.168.1.$i ALIVE" &
done
wait
echo

echo "===== 3. Check port 554 on all alive hosts ====="
# Get alive hosts from step 2 and check 554
for ip in $(ip neigh show | grep -i "REACHABLE\|STALE" | awk '{print $1}' | grep "^192.168.1."); do
    timeout 2 bash -c "echo > /dev/tcp/$ip/554" 2>/dev/null && echo "$ip:554 OPEN (RTSP!)" || echo "$ip:554 closed"
done
echo

echo "===== 4. Try nmap via docker ====="
docker run --rm --network host instrumentisto/nmap:latest -p 554 --open 192.168.1.0/24 2>&1 | grep -E "Nmap scan report|554/tcp|Host"
echo

echo "===== 5. Check if camera might be on OLD subnet (192.168.31.x) ====="
# Maybe the router still has a 192.168.31.x interface?
ip route show 2>&1
echo

echo "===== 6. Check current camera config in DB ====="
cd /vol1/docker/home-datacenter
# Try sqlite directly
if [ -f data/home.db ]; then
    sqlite3 data/home.db "SELECT id, name, host, rtsp_port, channel FROM cameras;" 2>&1
elif docker exec home-api ls /app/data/home.db 2>/dev/null; then
    docker exec home-api sqlite3 /app/data/home.db "SELECT id, name, host, rtsp_port, channel FROM cameras;" 2>&1
else
    echo "DB not found at data/home.db, searching..."
    find /vol1/docker/home-datacenter -name "*.db" -type f 2>&1 | head -10
fi
echo

echo "===== DONE ====="
