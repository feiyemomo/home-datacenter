#!/bin/bash
# check-interfaces.sh — Check all network interfaces after user
# connected the old router to another port.
set +e

echo "===== 1. All network interfaces ====="
ip link show 2>&1
echo

echo "===== 2. All IPv4 addresses ====="
ip -4 addr show 2>&1 | grep -E "inet |state "
echo

echo "===== 3. NetworkManager connections + devices ====="
nmcli -t -f NAME,DEVICE,TYPE,STATE con show 2>&1
echo

echo "===== 4. Which interfaces have carrier (link detected)? ====="
for iface in $(ls /sys/class/net/); do
    carrier=$(cat /sys/class/net/$iface/carrier 2>/dev/null)
    speed=$(cat /sys/class/net/$iface/speed 2>/dev/null)
    echo "$iface: carrier=$carrier speed=$speed"
done
echo

echo "===== 5. Re-add 192.168.31.3 to enp4s0 (if not already) ====="
echo '@Fnos324' | sudo -S ip addr add 192.168.31.3/24 dev enp4s0 2>&1 || echo "(already exists)"
echo

echo "===== 6. Flush ARP cache and retry 192.168.31.100 ====="
echo '@Fnos324' | sudo -S ip neigh flush 192.168.31.100 2>&1
# Force ARP request
timeout 3 bash -c "ping -c 1 -W 2 192.168.31.100" 2>&1
ip neigh show | grep "192.168.31"
echo

echo "===== 7. Test 192.168.31.100:554 ====="
timeout 5 bash -c "echo > /dev/tcp/192.168.31.100/554" 2>/dev/null && echo "192.168.31.100:554 OPEN ✓" || echo "192.168.31.100:554 UNREACHABLE"
echo

echo "===== 8. Check if old router (192.168.31.1) is reachable ====="
timeout 3 bash -c "echo > /dev/tcp/192.168.31.1/80" 2>/dev/null && echo "192.168.31.1:80 OPEN ✓" || echo "192.168.31.1:80 UNREACHABLE"
ip neigh show | grep "192.168.31.1"
echo

echo "===== DONE ====="
