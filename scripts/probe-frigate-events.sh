#!/bin/bash
# Probe Frigate for events/reviews from multiple angles to find where
# detection data actually lives in Frigate 0.17.
set -e

echo "=== /api/events (default) ==="
curl -s 'http://127.0.0.1:5000/api/events' | head -c 500
echo

echo "=== /api/events?has_clip=1 ==="
curl -s 'http://127.0.0.1:5000/api/events?has_clip=1' | head -c 500
echo

echo "=== /api/events?include_thumbnails=1 ==="
curl -s 'http://127.0.0.1:5000/api/events?include_thumbnails=1' | head -c 500
echo

echo "=== /api/review (default) ==="
curl -s 'http://127.0.0.1:5000/api/review' | head -c 500
echo

echo "=== /api/review?detections=1&alerts=1 ==="
curl -s 'http://127.0.0.1:5000/api/review?detections=1&alerts=1' | head -c 500
echo

echo "=== /api/review?include_static=1 ==="
curl -s 'http://127.0.0.1:5000/api/review?include_static=1' | head -c 500
echo

# 1 hour ago in unix seconds (GNU date)
NOW=$(date +%s)
HOUR_AGO=$((NOW - 3600))

echo "=== /api/front_door/recordings?after=$HOUR_AGO&before=$NOW ==="
curl -s "http://127.0.0.1:5000/api/front_door/recordings?after=$HOUR_AGO&before=$NOW" | head -c 500
echo

echo "=== docker logs home-frigate (last 200 lines, filtered for mqtt/event) ==="
docker logs home-frigate 2>&1 | grep -iE 'mqtt|event|publish' | tail -30
echo

echo "=== mosquitto_sub - frigate/events for 5 seconds ==="
timeout 5 docker exec home-mosquitto mosquitto_sub -h 127.0.0.1 -p 1883 -u home-datacenter -P '@Fnos324mqtt' -t 'frigate/events' || echo "(timeout, no events in 5s)"
