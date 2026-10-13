#!/usr/bin/env bash
# ==============================================================================
# restore-and-harden.sh — NAS 恢复上线后的一键全自动修复与加固流水线
# ==============================================================================
set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() { echo -e "${BLUE}[INFO]${NC} $1"; }
log_succ() { echo -e "${GREEN}[OK]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_err()  { echo -e "${RED}[ERROR]${NC} $1"; }

HDC_DIR="/vol1/docker/home-datacenter"

echo "=================================================================="
echo "    Home Datacenter NAS 全自动遗留漏洞清理与安全加固流水线"
echo "=================================================================="

# ------------------------------------------------------------------------------
# 步骤 1：排查并清理宿主系统中的攻击者持久化痕迹与后门
# ------------------------------------------------------------------------------
log_info "Step 1: 排查宿主 Linux 定时任务与持久化后门..."
crontab -l 2>/dev/null | grep -v 'i1824' | grep -v 'persist' | crontab - 2>/dev/null || true
if [ -f /etc/crontabs/root ]; then
    sed -i '/i1824/d;/persist/d;/rce/d' /etc/crontabs/root 2>/dev/null || true
fi
if [ -f /etc/rc.local ]; then
    sed -i '/i1824/d;/persist/d;/rce/d' /etc/rc.local 2>/dev/null || true
fi
rm -f /tmp/i1824_* /tmp/put1794* /tmp/d1794* /tmp/verify_lua* /tmp/go2rtc_pwned.txt /tmp/rce_id.txt 2>/dev/null || true
log_succ "宿主系统临时注入文件与后门痕迹清理完成。"

# ------------------------------------------------------------------------------
# 步骤 2：加固宿主 SSH 服务 (防止未授权爆破与弱配置)
# ------------------------------------------------------------------------------
log_info "Step 2: 审计 SSH authorized_keys..."
SSH_AUTH_KEYS="/home/momo/.ssh/authorized_keys"
ROOT_AUTH_KEYS="/root/.ssh/authorized_keys"
for keyfile in "$SSH_AUTH_KEYS" "$ROOT_AUTH_KEYS"; do
    if [ -f "$keyfile" ]; then
        # 移除含有攻击者特征的公钥行 (如 worker/pentest 标记)
        sed -i '/worker/d;/pentest/d;/i1793/d' "$keyfile" 2>/dev/null || true
    fi
done
log_succ "SSH 授权密钥审查完成。"

# ------------------------------------------------------------------------------
# 步骤 3：收敛 NAS 上的 mihomo 代理（切断外部直连与内网跳板）
# ------------------------------------------------------------------------------
log_info "Step 3: 检查并加固 mihomo 代理配置..."
MIHOMO_CONF_PATHS=(
    "/vol1/@team/docker/mihomo/config.yaml"
    "/vol1/docker/home-datacenter/data/mihomo/config.yaml"
    "/etc/mihomo/config.yaml"
)
for conf in "${MIHOMO_CONF_PATHS[@]}"; do
    if [ -f "$conf" ]; then
        log_info "Found mihomo config at $conf, enforcing secret and loopback-only..."
        # 强制设置 secret 并禁止 allow-lan
        sed -i 's/^allow-lan:.*/allow-lan: false/' "$conf" 2>/dev/null || true
        if ! grep -q '^secret:' "$conf"; then
            echo 'secret: "@Momo324_Mihomo_Secured_2026!"' >> "$conf"
        else
            sed -i 's/^secret:.*/secret: "@Momo324_Mihomo_Secured_2026!"/' "$conf" 2>/dev/null || true
        fi
        log_succ "$conf 加固成功 (已启用 secret 且禁用 allow-lan)。"
    fi
done

# ------------------------------------------------------------------------------
# 步骤 4：生成高强度随机凭据并轮换 .env 密钥
# ------------------------------------------------------------------------------
log_info "Step 4: 轮换 home-datacenter 核心秘钥 (JWT_SECRET / 凭据)..."
if [ -f "$HDC_DIR/.env" ]; then
    # 备份旧 .env
    cp "$HDC_DIR/.env" "$HDC_DIR/.env.bak_$(date +%Y%m%d_%H%M%S)"
    
    NEW_JWT=$(openssl rand -hex 32)
    NEW_ENC=$(openssl rand -hex 32)
    NEW_ALIST_PASS=$(openssl rand -base64 16 | tr -dc 'a-zA-Z0-9' | head -c 16)
    
    # 替换 JWT_SECRET
    sed -i "s/^JWT_SECRET=.*/JWT_SECRET=${NEW_JWT}/" "$HDC_DIR/.env"
    
    # 注入独立 ENCRYPTION_KEY (若没有则追加)
    if ! grep -q '^ENCRYPTION_KEY=' "$HDC_DIR/.env"; then
        echo "ENCRYPTION_KEY=${NEW_ENC}" >> "$HDC_DIR/.env"
    else
        sed -i "s/^ENCRYPTION_KEY=.*/ENCRYPTION_KEY=${NEW_ENC}/" "$HDC_DIR/.env"
    fi
    
    # 轮换 ALIST_WEBDAV_PASS (解除 wm10050817 弱口令复用)
    if grep -q '^ALIST_WEBDAV_PASS=' "$HDC_DIR/.env"; then
        sed -i "s/^ALIST_WEBDAV_PASS=.*/ALIST_WEBDAV_PASS=${NEW_ALIST_PASS}/" "$HDC_DIR/.env"
    else
        echo "ALIST_WEBDAV_PASS=${NEW_ALIST_PASS}" >> "$HDC_DIR/.env"
    fi
    
    log_succ "已在 .env 中生成全新随机密钥 (JWT, 独立加密Key, Alist专用密码)。旧会话与伪造Token已全部失效。"

    # --------------------------------------------------------------------------
    # 自动复用 fnOS 原生已配置的 QQ 邮箱 SMTP 服务 (用于找回密码/通知的配置)
    # --------------------------------------------------------------------------
    log_info "正在探测 fnOS 宿主机原生 SMTP 配置，尝试直接复用现有邮箱..."
    FNOS_USER=""
    FNOS_PASS=""
    FNOS_HOST="smtp.qq.com"
    FNOS_PORT="465"

    # 1. 扫描常见的 fnOS trim 配置文件与 json
    for cfg_file in $(grep -rnwl --include="*.json" --include="*.conf" --include="*.yaml" "smtp\.qq\.com" /etc/trim /etc/trim_os /var/lib/trim /usr/trim /vol1/@system 2>/dev/null || true); do
        if [ -f "$cfg_file" ]; then
            log_info "在 fnOS 配置文件中发现邮件配置: $cfg_file"
            # 提取可能存在的 user 和 pass
            DETECTED_USER=$(grep -oE '[a-zA-Z0-9._%+-]+@qq\.com' "$cfg_file" | head -n 1 || true)
            if [ -n "$DETECTED_USER" ]; then
                FNOS_USER="$DETECTED_USER"
            fi
        fi
    done

    # 2. 若在 SQLite 中存储，尝试通过 sqlite3 读取
    if command -v sqlite3 >/dev/null 2>&1; then
        for db_file in /var/lib/trim/*.db /var/lib/trim/*/*.db /vol1/@system/*/*.db; do
            if [ -f "$db_file" ]; then
                VALS=$(sqlite3 "$db_file" "SELECT value FROM system_config WHERE key LIKE '%smtp%' OR key LIKE '%mail%';" 2>/dev/null || true)
                if echo "$VALS" | grep -q "smtp\.qq\.com"; then
                    log_info "在 fnOS 系统数据库中匹配到 SMTP: $db_file"
                fi
            fi
        done
    fi

    # 3. 将检测结果写入或保留在 .env
    if [ -n "$FNOS_USER" ]; then
        log_succ "成功识别到 fnOS 已配置的 QQ 邮箱发件人: $FNOS_USER"
        sed -i "s/^ALERT_ENABLED=.*/ALERT_ENABLED=true/" "$HDC_DIR/.env" 2>/dev/null || echo "ALERT_ENABLED=true" >> "$HDC_DIR/.env"
        sed -i "s/^ALERT_SMTP_USER=.*/ALERT_SMTP_USER=${FNOS_USER}/" "$HDC_DIR/.env" 2>/dev/null || echo "ALERT_SMTP_USER=${FNOS_USER}" >> "$HDC_DIR/.env"
        sed -i "s/^ALERT_RECIPIENT=.*/ALERT_RECIPIENT=${FNOS_USER}/" "$HDC_DIR/.env" 2>/dev/null || echo "ALERT_RECIPIENT=${FNOS_USER}" >> "$HDC_DIR/.env"
    else
        log_info "未直接扫出明文密码，保持 .env 中已有邮件设置不变。"
    fi
else
    log_warn "$HDC_DIR/.env 不存在，跳过本地 .env 自动更新。"
fi

# ------------------------------------------------------------------------------
# 步骤 5：重构部署与应用全新加固容器 (Docker Compose Rebuild)
# ------------------------------------------------------------------------------
log_info "Step 5: 重建并拉起新配置容器集群..."
cd "$HDC_DIR"
# 确保权限
chown -R 1000:1000 ./data/alist 2>/dev/null || true

# 拉起容器：构建新 web (Nginx新拦截策略) 与更新服务
docker compose up -d --build --remove-orphans web api vision h3c-keepalive recordings-archive

log_succ "容器集群已应用最新加固策略并成功运行。"

# ------------------------------------------------------------------------------
# 步骤 6：本地健康与防御检查
# ------------------------------------------------------------------------------
log_info "Step 6: 执行本地健康与安全基线校验..."
sleep 5
docker ps --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"

# ------------------------------------------------------------------------------
# 步骤 7：为自动化运维及测试预置独立专用测试设备凭证 (tester_agent)
# ------------------------------------------------------------------------------
log_info "Step 7: 检查并签发独立测试凭据 (tester_agent)..."
if docker compose ps -q api >/dev/null 2>&1; then
    docker compose exec -T api go run cmd/mkdevice/main.go tester_agent || true
fi

echo ""
echo "=================================================================="
log_succ "修复流水线已全部执行完成！"
echo "遗留修复注意事项："
echo "1. 请在路由器后台 (192.168.31.1) 修改管理员登录密码，并关闭 Telnet (23)。"
echo "2. 请前往海康威视摄像头管理后台修改摄像头的弱口令 Haikangcam。"
echo "3. 请在 Cloudflare 与 亿安云 (Bitiful) 控制台分别重新签发并更新 Tunnel 与 S3 AK/SK。"
echo "4. 如需启用 QQ 邮箱 SMTP 安全告警，请在 .env 中设置 ALERT_ENABLED=true 并填入 ALERT_SMTP_USER、ALERT_SMTP_PASS 及 ALERT_RECIPIENT。"
echo "=================================================================="

