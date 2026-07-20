#!/bin/bash
# Probe the running Frigate config (full cameras section).
set -e

echo '--- Frigate running config (cameras section) ---'
curl -s 'http://127.0.0.1:5000/api/config' > /tmp/frigate-config.json
python3 << 'PYEOF'
import json
with open('/tmp/frigate-config.json') as f:
    cfg = json.load(f)
cams = cfg.get("cameras", {})
print(f"Camera count: {len(cams)}")
for k, v in cams.items():
    print(f"  - key={k}")
    print(f"    name={v.get('name', '')}")
    print(f"    enabled={v.get('enabled', False)}")
    ff = v.get("ffmpeg", {})
    inputs = ff.get("inputs", [])
    for inp in inputs:
        print(f"    input.path={inp.get('path', '')}")
        print(f"    input.roles={inp.get('roles', [])}")
    detect = v.get("detect", {})
    print(f"    detect.enabled={detect.get('enabled', False)}")
    print(f"    detect.fps={detect.get('fps', 0)}")
    rec = v.get("record", {})
    print(f"    record.enabled={rec.get('enabled', False)}")
PYEOF

echo
echo '--- Frigate /api/cameras/list ---'
curl -s 'http://127.0.0.1:5000/api/cameras/list' | python3 -m json.tool 2>&1 | head -50
echo
echo '--- home-api logs (last 80 lines mentioning frigate/config push) ---'
docker logs home-api 2>&1 | tail -500 | grep -iE 'frigate|config|push|record' | tail -80
echo
echo '--- home-api logs (last 20 lines) ---'
docker logs home-api 2>&1 | tail -20
