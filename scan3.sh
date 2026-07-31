#!/bin/bash
# scan3.sh — Scan for RTSP cameras using alpine container
set +e

echo "===== Scanning 192.168.1.0/24 port 554 via alpine+nmap ====="
docker run --rm --network host alpine:3.18 sh -c "
apk add --no-cache nmap >/dev/null 2>&1
nmap -p 554 --open -T4 192.168.1.0/24 2>&1
" 2>&1
echo

echo "===== Also scan common camera HTTP ports (80, 443, 8080) ====="
docker run --rm --network host alpine:3.18 sh -c "
apk add --no-cache nmap >/dev/null 2>&1
nmap -p 80,443,8080 --open -T4 192.168.1.0/24 2>&1
" 2>&1
echo

echo "===== DONE ====="
