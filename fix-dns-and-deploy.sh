#!/bin/bash
# fix-dns-and-deploy.sh — Fix DNS, configure working mirror, test pull
set -e

echo "===== 1. Configure DNS via NetworkManager ====="
# Add public DNS servers (Ali + Tencent) as fallback
echo '@Fnos324' | sudo -S nmcli con mod enp4s0 ipv4.dns "192.168.1.1 223.5.5.5 119.29.29.29" 2>&1
echo '@Fnos324' | sudo -S nmcli con mod enp4s0 ipv4.ignore-auto-dns no 2>&1
echo '@Fnos324' | sudo -S nmcli con up enp4s0 2>&1
echo "=== resolv.conf now ==="
cat /etc/resolv.conf
echo

echo "===== 2. Update daemon.json (daocloud first, no proxy) ====="
NEW_CONFIG='{"data-root":"/vol1/docker","live-restore":true,"log-driver":"json-file","log-opts":{"max-file":"5","max-size":"100m"},"registry-mirrors":["https://docker.m.daocloud.io","https://docker.xuanyuan.me","https://docker.1ms.run","https://k-docker.asia","https://docker.fnnas.com"]}'
echo "@Fnos324" | sudo -S bash -c "echo '$NEW_CONFIG' > /etc/docker/daemon.json"
cat /etc/docker/daemon.json
echo

echo "===== 3. Restart Docker ====="
echo '@Fnos324' | sudo -S systemctl restart docker 2>&1
sleep 3
systemctl is-active docker
echo

echo "===== 4. Test: docker pull alpine:3.18 ====="
timeout 60 docker pull alpine:3.18 2>&1
echo "pull exit code: $?"
echo

echo "===== 5. Verify alpine runs ====="
docker run --rm alpine:3.18 echo "alpine pull OK"
echo

echo "===== DONE ====="
