#!/bin/sh
# Retry build up to 5 times because daocloud/tencent mirrors are
# flaky today (intermittent EOF on layer pulls).
LOGDIR=d:/Projects/home-datacenter
for i in 1 2 3 4 5; do
  echo "=== try $i ==="
  if docker compose -f d:/Projects/home-datacenter/compose.yaml build go2rtc > "$LOGDIR/build_$i.log" 2>&1; then
    echo "OK on try $i"
    exit 0
  fi
  echo "FAIL on try $i (last error line:)"
  grep -E "ERROR" "$LOGDIR/build_$i.log" | head -3
  sleep 5
done
exit 1
