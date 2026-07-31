#!/bin/bash
# check-cam.sh — Verify camera live stream is healthy after routing fix.
set +e

echo "===== 1. Containers status ====="
docker ps --format "table {{.Names}}\t{{.Status}}" | grep -E "home-|NAMES"
echo

echo "===== 2. home-api health ====="
curl -s -o /dev/null -w "home-api /api/v1/health: HTTP %{http_code}\n" http://localhost:8087/api/v1/health
echo

echo "===== 3. go2rtc streams (inside frigate) ====="
docker exec home-frigate curl -s http://localhost:1984/api/streams
echo
echo

echo "===== 4. Frigate /api/config (cameras block) ====="
curl -s http://localhost:5000/api/config 2>/dev/null | python3 -c "import sys,json; c=json.load(sys.stdin); print('cameras:', list(c.get('cameras',{}).keys())); print('go2rtc streams:', list(c.get('go2rtc',{}).get('streams',{}).keys()))" 2>&1
echo

echo "===== 5. ffmpeg processes inside frigate ====="
docker exec home-frigate pgrep -af ffmpeg | head -3
echo

echo "===== 6. frigate stats (front_door camera_fps) ====="
curl -s http://localhost:5000/api/stats 2>/dev/null | python3 -c "import sys,json; s=json.load(sys.stdin); cam=s.get('cameras',{}).get('front_door',{}); print('camera_fps:', cam.get('camera_fps')); print('process_fps:', cam.get('process_fps')); print('detection_fps:', cam.get('detection_fps')); print('skipped_fps:', cam.get('skipped_fps'))" 2>&1
echo

echo "===== 7. Recording files (last 2 minutes) ====="
docker exec home-frigate bash -c 'ls -lt /tmp/cache/ 2>/dev/null | head -5'
echo

echo "===== DONE ====="
