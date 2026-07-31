#!/bin/bash
# fix-eno1-ipv6.sh — Permanently disable IPv6 on eno1 (old router segment).
# eno1 only carries IPv4 for the 192.168.31.x camera segment; IPv6 on it
# creates a competing default route (pref medium > enp4s0's pref low) that
# breaks inbound IPv6 to enp4s0's stable EUI-64 address.
set +e

echo "===== 1. Find eno1's NetworkManager connection name ====="
nmcli -t -f NAME,DEVICE,TYPE,STATE con show 2>&1 | grep "eno1"
echo

CONN_NAME=$(nmcli -t -f NAME,DEVICE con show 2>&1 | grep "eno1" | head -1 | cut -d: -f1)
echo "Using connection name: $CONN_NAME"
echo

echo "===== 2. Disable IPv6 on $CONN_NAME (permanent) ====="
echo '@Fnos324' | sudo -S nmcli connection modify "$CONN_NAME" ipv6.method disabled 2>&1
echo

echo "===== 3. Apply (down + up) ====="
echo '@Fnos324' | sudo -S nmcli connection down "$CONN_NAME" 2>&1
sleep 2
echo '@Fnos324' | sudo -S nmcli connection up "$CONN_NAME" 2>&1
sleep 3
echo

echo "===== 4. Verify eno1 IPv4 still works (camera segment) ====="
ip -4 addr show eno1 2>&1 | grep "inet "
echo

echo "===== 5. Verify eno1 IPv6 is gone ====="
ip -6 addr show eno1 2>&1 | grep "scope global" || echo "(no global IPv6 on eno1 - good)"
echo

echo "===== 6. Verify IPv6 default routes (should be enp4s0 only) ====="
ip -6 route show default 2>&1
echo

echo "===== 7. Verify camera still reachable ====="
timeout 5 bash -c "echo > /dev/tcp/192.168.31.100/554" 2>/dev/null && echo "192.168.31.100:554 OPEN ✓" || echo "192.168.31.100:554 UNREACHABLE"
echo

echo "===== 8. Verify home-web IPv6 listen still works ====="
curl -s -o /dev/null -w "curl [stable EUI-64]:8088 -> %{http_code}\n" --max-time 5 \
    "http://[2409:8a70:37a4:9140:62be:b4ff:fe08:bd09]:8088/" 2>&1
echo

echo "===== DONE ====="
