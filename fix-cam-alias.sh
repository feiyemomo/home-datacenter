#!/bin/bash
# fix-cam-alias.sh — Add 192.168.31.x IP alias on NAS so it can reach
# the camera at 192.168.31.100 without changing the camera's config.
set +e

echo "===== 1. Add secondary IP 192.168.31.3/24 to enp4s0 ====="
echo '@Fnos324' | sudo -S ip addr add 192.168.31.3/24 dev enp4s0 2>&1
echo "Result: $?"
echo

echo "===== 2. Verify IP alias ====="
ip -4 addr show enp4s0 2>&1
echo

echo "===== 3. Test 192.168.31.100:554 ====="
timeout 5 bash -c "echo > /dev/tcp/192.168.31.100/554" 2>/dev/null && echo "192.168.31.100:554 OPEN ✓" || echo "192.168.31.100:554 UNREACHABLE"
echo

echo "===== 4. Test RTSP via ffmpeg (quick probe) ====="
# Use docker to run ffmpeg probe (frigate container has ffmpeg)
docker exec home-frigate ffprobe -rtsp_transport tcp -timeout 5000000 -i "rtsp://admin:Haikangcam@192.168.31.100:554/Streaming/Channels/101" -show_streams -select_streams v -of json 2>&1 | head -30
echo

echo "===== 5. Restart frigate to pick up camera ====="
docker restart home-frigate 2>&1
sleep 10
echo

echo "===== 6. Check frigate logs after restart ====="
docker logs home-frigate --tail 15 2>&1
echo

echo "===== DONE ====="
