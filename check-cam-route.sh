#!/bin/bash
# check-cam-route.sh — Check if NAS can reach 192.168.31.100 across subnets
set +e

echo "===== 1. Route table ====="
ip route show 2>&1
echo

echo "===== 2. Can we reach 192.168.31.100? ====="
# Test TCP 554 directly
timeout 3 bash -c "echo > /dev/tcp/192.168.31.100/554" 2>/dev/null && echo "192.168.31.100:554 OPEN" || echo "192.168.31.100:554 UNREACHABLE"
echo

echo "===== 3. Can we reach 192.168.31.1 (old router IP)? ====="
timeout 3 bash -c "echo > /dev/tcp/192.168.31.1/80" 2>/dev/null && echo "192.168.31.1:80 OPEN" || echo "192.168.31.1:80 UNREACHABLE"
echo

echo "===== 4. Traceroute to 192.168.31.100 ====="
timeout 10 traceroute -n -m 5 192.168.31.100 2>&1 || echo "traceroute failed"
echo

echo "===== 5. Check if router has 192.168.31.x interface ====="
# Try to access router's secondary interface
curl -s -m 5 -o /dev/null -w "192.168.31.1: HTTP %{http_code}\n" "http://192.168.31.1/" 2>&1
echo

echo "===== 6. Add route to 192.168.31.0/24 via router ====="
# If the router has both 192.168.1.1 and 192.168.31.1 interfaces,
# adding a route via 192.168.1.1 should let us reach 192.168.31.x
echo '@Fnos324' | sudo -S ip route add 192.168.31.0/24 via 192.168.1.1 2>&1
echo "Route add result: $?"
ip route show 2>&1
echo

echo "===== 7. Test 192.168.31.100:554 after route ====="
timeout 5 bash -c "echo > /dev/tcp/192.168.31.100/554" 2>/dev/null && echo "192.168.31.100:554 OPEN" || echo "192.168.31.100:554 UNREACHABLE"
echo

echo "===== 8. Test RTSP via curl ====="
curl -s -m 5 -o /dev/null -w "RTSP: HTTP %{http_code} in %{time_total}s\n" "rtsp://admin:Haikangcam@192.168.31.100:554/Streaming/Channels/101" 2>&1
echo

echo "===== DONE ====="
