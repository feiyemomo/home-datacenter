#!/bin/bash
# check-ipv6.sh — Check current NAS IPv6 vs DDNS config.
set +e

echo "===== 1. Current IPv6 addresses on enp4s0 ====="
ip -6 addr show enp4s0 2>&1 | grep "scope global" | grep -v temporary
echo
echo "--- including temporary ---"
ip -6 addr show enp4s0 2>&1 | grep "scope global"
echo

echo "===== 2. Current IPv6 on eno1 ====="
ip -6 addr show eno1 2>&1 | grep "scope global"
echo

echo "===== 3. Default IPv6 routes ====="
ip -6 route show default 2>&1
echo

echo "===== 4. DDNS domain resolution (nas.feiyemomo.top) ====="
dig AAAA nas.feiyemomo.top +short 2>&1
echo
echo "--- using getent ---"
getent ahostsv6 nas.feiyemomo.top 2>&1 | head -3
echo

echo "===== 5. Test local IPv6 connectivity to :8088 ====="
curl -s -o /dev/null -w "curl [::1]:8088 -> %{http_code}\n" --max-time 3 "http://[::1]:8088/" 2>&1
LAN_V6=$(ip -6 addr show enp4s0 2>&1 | grep "scope global" | grep -v temporary | awk '{print $2}' | cut -d/ -f1 | head -1)
if [ -n "$LAN_V6" ]; then
    echo "Testing with local IPv6: $LAN_V6"
    curl -s -o /dev/null -w "curl [$LAN_V6]:8088 -> %{http_code} (time=%{time_total}s)\n" --max-time 5 "http://[$LAN_V6]:8088/" 2>&1
fi
echo

echo "===== 6. cloudflared / DDNS update logs ====="
docker logs home-cloudflared --tail 10 2>&1 | head -15
echo

echo "===== DONE ====="
