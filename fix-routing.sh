#!/bin/bash
# fix-routing.sh — Remove conflicting route/IP alias, ensure 192.168.31.0/24
# routes via eno1 (which has 192.168.31.235/24 from the old router's DHCP).
set +e

echo "===== 1. Remove conflicting 192.168.31.3/24 from enp4s0 ====="
echo '@Fnos324' | sudo -S ip addr del 192.168.31.3/24 dev enp4s0 2>&1
echo

echo "===== 2. Remove static route 192.168.31.0/24 via 192.168.1.1 ====="
echo '@Fnos324' | sudo -S ip route del 192.168.31.0/24 via 192.168.1.1 2>&1
echo

echo "===== 3. Verify routing table ====="
ip route show 2>&1
echo

echo "===== 4. Verify eno1 has 192.168.31.235/24 ====="
ip -4 addr show eno1 2>&1
echo

echo "===== 5. Flush ARP cache for 192.168.31.x ====="
echo '@Fnos324' | sudo -S ip neigh flush 192.168.31.0/24 dev enp4s0 2>&1
echo '@Fnos324' | sudo -S ip neigh flush 192.168.31.0/24 dev eno1 2>&1
echo

echo "===== 6. Test 192.168.31.1 (old router) ====="
timeout 3 bash -c "echo > /dev/tcp/192.168.31.1/80" 2>/dev/null && echo "192.168.31.1:80 OPEN ✓" || echo "192.168.31.1:80 UNREACHABLE"
ip neigh show | grep "192.168.31.1"
echo

echo "===== 7. Test 192.168.31.100:554 (camera) ====="
# Force traffic via eno1 by specifying source IP
timeout 5 bash -c "echo > /dev/tcp/192.168.31.100/554" 2>/dev/null && echo "192.168.31.100:554 OPEN ✓ ✓ ✓" || echo "192.168.31.100:554 UNREACHABLE"
ip neigh show | grep "192.168.31.100"
echo

echo "===== 8. If camera reachable, restart frigate ====="
if timeout 5 bash -c "echo > /dev/tcp/192.168.31.100/554" 2>/dev/null; then
    echo "Camera is reachable! Restarting frigate..."
    docker restart home-frigate 2>&1
    sleep 15
    echo "=== Frigate logs ==="
    docker logs home-frigate --tail 20 2>&1
else
    echo "Camera still unreachable. Checking ARP..."
    ip neigh show | grep "192.168.31"
fi
echo

echo "===== DONE ====="
