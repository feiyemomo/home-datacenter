#!/bin/bash
# check-api.sh — Diagnose why home-api is not responding.
set +e

echo "===== 1. home-api container status ====="
docker ps --format "{{.Names}}: {{.Status}}" | grep home-api
echo

echo "===== 2. home-api container ports ====="
docker port home-api 2>&1
echo

echo "===== 3. home-api last 30 log lines ====="
docker logs home-api --tail 30 2>&1
echo

echo "===== 4. home-web -> home-api upstream check ====="
docker exec home-web cat /etc/nginx/conf.d/*.conf 2>&1 | grep -A 3 "api" | head -40
echo

echo "===== 5. curl from inside home-web container ====="
docker exec home-web curl -s -o /dev/null -w "home-web -> home-api /api/v1/health: %{http_code} (time_total=%{time_total}s)\n" --max-time 5 http://home-api:8080/api/v1/health 2>&1
echo

echo "===== 6. direct curl to home-api container IP ====="
API_IP=$(docker inspect -f '{{range.NetworkSettings.Networks}}{{.IPAddress}}{{end}}' home-api 2>&1 | head -1)
echo "home-api container IP: $API_IP"
if [ -n "$API_IP" ]; then
    curl -s -o /dev/null -w "direct /api/v1/health: %{http_code} (time_total=%{time_total}s)\n" --max-time 5 "http://$API_IP:8080/api/v1/health" 2>&1
fi
echo

echo "===== DONE ====="
