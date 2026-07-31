#!/bin/bash
# db-cam2.sh — Test 192.168.1.2:554 + read DB via python3
set +e

echo "===== 1. Test 192.168.1.2:554 (RTSP) ====="
timeout 3 bash -c "echo > /dev/tcp/192.168.1.2/554" 2>/dev/null && echo "192.168.1.2:554 OPEN" || echo "192.168.1.2:554 closed"
echo

echo "===== 2. Test RTSP URL directly ====="
curl -s -m 5 -o /dev/null -w "RTSP test: HTTP %{http_code} in %{time_total}s\n" "rtsp://admin:Haikangcam@192.168.1.2:554/Streaming/Channels/101" 2>&1
echo

echo "===== 3. Read camera config from DB via python3 ====="
python3 -c "
import sqlite3, json
conn = sqlite3.connect('/vol1/docker/home-datacenter/data/sqlite/app.db')
conn.row_factory = sqlite3.Row
cur = conn.cursor()
# List all tables
cur.execute(\"SELECT name FROM sqlite_master WHERE type='table'\")
tables = [r[0] for r in cur.fetchall()]
print('Tables:', tables)
print()
# Find camera table
for t in tables:
    if 'camera' in t.lower():
        cur.execute(f'SELECT * FROM {t}')
        cols = [d[0] for d in cur.description]
        print(f'--- {t} (columns: {cols}) ---')
        for row in cur.fetchall():
            print(json.dumps(dict(row), ensure_ascii=False, default=str))
        print()
conn.close()
" 2>&1
echo

echo "===== DONE ====="
