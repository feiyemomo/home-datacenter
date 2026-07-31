#!/bin/bash
# check-arp.sh — Check ARP table for 192.168.31.100 to determine
# if the camera is physically reachable at Layer 2.
set +e

echo "===== 1. ARP table for 192.168.31.x ====="
ip neigh show | grep "192.168.31"
echo

echo "===== 2. Try arping 192.168.31.100 (Layer 2 only) ====="
echo '@Fnos324' | sudo -S arping -c 3 -I enp4s0 192.168.31.100 2>&1
echo

echo "===== 3. Full ARP table (all neighbors) ====="
ip neigh show 2>&1
echo

echo "===== 4. Check if old router (192.168.31.1) is visible at L2 ====="
echo '@Fnos324' | sudo -S arping -c 2 -I enp4s0 192.168.31.1 2>&1
echo

echo "===== 5. Check network switch ports ====="
# List all MAC addresses the NAS can see
ip neigh show | awk '{print $5}' | sort -u | head -20
echo

echo "===== DONE ====="
