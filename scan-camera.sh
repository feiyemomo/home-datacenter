#!/bin/bash
# scan-camera.sh — Scan 192.168.1.0/24 for RTSP cameras (port 554)
set +e

echo "===== Scanning 192.168.1.0/24 for port 554 (RTSP) ====="
# Try nmap first
if command -v nmap &>/dev/null; then
    nmap -p 554 --open 192.168.1.0/24 2>&1 | grep -E "Nmap scan report|554/tcp"
else
    echo "nmap not available, using bash loop..."
    for i in $(seq 1 254); do
        # timeout 1s per host, check port 554
        (timeout 1 bash -c "echo > /dev/tcp/192.168.1.$i/554" 2>/dev/null && echo "192.168.1.$i:554 OPEN") &
    done
    wait
fi
echo

echo "===== Also check common camera IPs directly ====="
for ip in 192.168.1.64 192.168.1.100 192.168.1.108 192.168.1.64; do
    timeout 2 bash -c "echo > /dev/tcp/$ip/554" 2>/dev/null && echo "$ip:554 OPEN" || echo "$ip:554 closed/timeout"
done
echo

echo "===== Check ARP table for known camera MAC vendors ====="
arp -n 2>&1 | head -30
echo

echo "===== DONE ====="
