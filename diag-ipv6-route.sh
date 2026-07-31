#!/bin/bash
# diag-ipv6-route.sh — Verify which interface is used for IPv6 egress.
set +e

echo "===== 1. IPv6 route to external (Google DNS) ====="
ip -6 route get 2001:4860:4860::8888 2>&1
echo

echo "===== 2. IPv6 route to test-ipv6.com ====="
ip -6 route get 2606:4700:4700::1111 2>&1
echo

echo "===== 3. Current IPv6 default routes with pref ====="
ip -6 route show default 2>&1
echo

echo "===== 4. eno1 accept_ra setting ====="
cat /proc/sys/net/ipv6/conf/eno1/accept_ra 2>&1
cat /proc/sys/net/ipv6/conf/enp4s0/accept_ra 2>&1
echo

echo "===== 5. Disable eno1 IPv6 RA acceptance (TEST) ====="
echo "Disabling eno1 accept_ra and flushing its IPv6 default routes..."
echo '@Fnos324' | sudo -S sh -c 'echo 0 > /proc/sys/net/ipv6/conf/eno1/accept_ra' 2>&1
echo '@Fnos324' | sudo -S sh -c 'echo 1 > /proc/sys/net/ipv6/conf/eno1/disable_ipv6' 2>&1
sleep 2
echo

echo "===== 6. After disable - IPv6 default routes ====="
ip -6 route show default 2>&1
echo

echo "===== 7. After disable - IPv6 route to external ====="
ip -6 route get 2001:4860:4860::8888 2>&1
echo

echo "===== 8. After disable - eno1 still has IPv4 (camera) ====="
ip -4 addr show eno1 2>&1 | grep "inet "
echo

echo "===== 9. Test local IPv6 curl after disable ====="
curl -s -o /dev/null -w "curl [stable EUI-64]:8088 -> %{http_code} (time=%{time_total}s)\n" --max-time 5 \
    "http://[2409:8a70:37a4:9140:62be:b4ff:fe08:bd09]:8088/" 2>&1
echo

echo "===== DONE - Now test from browser: nas.feiyemomo.top:8088 ====="
