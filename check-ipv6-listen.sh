#!/bin/bash
# check-ipv6-listen.sh — Diagnose why IPv6 :8088 is unreachable from outside.
set +e

echo "===== 1. Actual listen sockets on :8088 ====="
ss -tlnp 2>&1 | grep -E ":8088|:8080|:5000" | head -10
echo

echo "===== 2. Docker port bindings for home-web ====="
docker inspect home-web --format '{{json .NetworkSettings.Ports}}' 2>&1
echo
echo "--- formatted ---"
docker inspect home-web --format '{{range $k, $v := .NetworkSettings.Ports}}{{$k}}: {{range $v}}{{.HostIp}}:{{.HostPort}}{{end}}{{println}}{{end}}' 2>&1
echo

echo "===== 3. IPv6 listen sockets only ====="
ss -tlnp6 2>&1 | head -15
echo

echo "===== 4. ip6tables filter (INPUT chain) ====="
echo '@Fnos324' | sudo -S ip6tables -L INPUT -n -v 2>&1 | head -30
echo

echo "===== 5. ip6tables nat (to verify Docker IPv6 port forwarding) ====="
echo '@Fnos324' | sudo -S ip6tables -t nat -L DOCKER -n -v 2>&1 | head -20
echo

echo "===== 6. Test IPv6 from NAS itself (stable EUI-64 addr) ====="
curl -s -o /dev/null -w "curl [stable EUI-64]:8088 -> %{http_code} (time=%{time_total}s)\n" --max-time 5 \
    "http://[2409:8a70:37a4:9140:62be:b4ff:fe08:bd09]:8088/" 2>&1
echo

echo "===== 7. Docker daemon IPv6 config ====="
cat /etc/docker/daemon.json 2>&1
echo

echo "===== 8. compose.yaml home-web section (ports) ====="
grep -B 2 -A 10 "home-web:" /home/fnos-momo/home-datacenter/compose.yaml 2>&1 | head -30
echo

echo "===== 9. UFW status (if active) ====="
echo '@Fnos324' | sudo -S ufw status 2>&1 | head -10
echo

echo "===== DONE ====="
