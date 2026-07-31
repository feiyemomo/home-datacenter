#!/bin/bash
# diag-ipv6-docker.sh — Deep diagnose Docker IPv6 port forwarding + dual-iface routing.
set +e

echo "===== 1. Which process listens on [::]:8088 (need sudo for pid) ====="
echo '@Fnos324' | sudo -S ss -tlnp 2>&1 | grep ":8088"
echo

echo "===== 2. docker-proxy processes ====="
ps aux 2>&1 | grep -E "docker-proxy.*8088" | grep -v grep
echo

echo "===== 3. ip6tables FORWARD chain (Docker relies on this) ====="
echo '@Fnos324' | sudo -S ip6tables -L FORWARD -n -v 2>&1 | head -25
echo

echo "===== 4. ip6tables nat - PREROUTING (DNAT for external) ====="
echo '@Fnos324' | sudo -S ip6tables -t nat -L PREROUTING -n -v 2>&1 | head -10
echo

echo "===== 5. ip6tables nat - DOCKER chain (should have DNAT rules) ====="
echo '@Fnos324' | sudo -S ip6tables -t nat -L DOCKER -n -v 2>&1
echo

echo "===== 6. sysctl IPv6 forwarding ====="
sysctl net.ipv6.conf.all.forwarding 2>&1
sysctl net.ipv6.conf.enp4s0.forwarding 2>&1
sysctl net.ipv6.conf.eno1.forwarding 2>&1
echo

echo "===== 7. Dual-interface IPv6 default routes (SUSPECT) ====="
ip -6 route show default 2>&1
echo
echo "--- enp4s0 IPv6 (光猫, prefix 37a4:9140) ---"
ip -6 addr show enp4s0 2>&1 | grep "scope global" | grep -v temporary
echo "--- eno1 IPv6 (旧路由, prefix 37a4:9141) ---"
ip -6 addr show eno1 2>&1 | grep "scope global"
echo

echo "===== 8. rp_filter for IPv6 (reverse path filter) ====="
sysctl net.ipv6.conf.all.rp_filter 2>&1
sysctl net.ipv6.conf.enp4s0.rp_filter 2>&1
sysctl net.ipv6.conf.eno1.rp_filter 2>&1
echo

echo "===== 9. Test: curl from enp4s0 source IPv6 (模拟外部) ====="
ENP4_V6=$(ip -6 addr show enp4s0 2>&1 | grep "scope global" | grep "mngtmpaddr" | awk '{print $2}' | cut -d/ -f1)
echo "enp4s0 stable IPv6: $ENP4_V6"
if [ -n "$ENP4_V6" ]; then
    curl -s -o /dev/null -w "curl --interface enp4s0 [stable]:8088 -> %{http_code} (time=%{time_total}s)\n" \
        --max-time 5 --interface enp4s0 "http://[$ENP4_V6]:8088/" 2>&1
fi
echo

echo "===== 10. tcpdump test (capture 8088 on enp4s0 for 3s) ====="
echo "Will capture enp4s0 port 8088 for 3 seconds..."
echo '@Fnos324' | sudo -S timeout 3 tcpdump -i enp4s0 -n -c 5 'tcp port 8088' 2>&1 | head -10
echo

echo "===== DONE ====="
