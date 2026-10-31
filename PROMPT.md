# Home Datacenter — 可复用提示词

> 每次开始本项目工作时，先读取此文件，再读取 `README.md` 和 `docs/ai-context.md` 了解项目背景。

## 项目概览

家庭数据中心 — Go API + React Web + MQTT + Frigate NVR 的全栈 Docker Compose 项目。
详细文档：
- `README.md` — 部署、配置、运维命令
- `docs/ai-context.md` — 项目架构、Phase 历史、已知陷阱（AI 接手必读）
- `docs/api-documentation.md` — 完整 API 规格

## 🚨 AI 安全准则与开发红线（CRITICAL SECURITY RULES）

**严禁在代码、注释、提交信息（commit message）、文档或配置文件中提交任何真实敏感凭据至 GitHub！**
1. **绝对禁止提交的内容**：
   * 真实密码、明文 AccessKey、SSH 私钥、Token、Cookie。
   * 真实云服务密钥：`CLOUDFLARE_TOKEN`、`BITIFUL_ACCESS_KEY`、`BITIFUL_SECRET_KEY`、`LOLIA_FRP_TOKEN`。
   * 真实内部服务密码：`H3C_PASSWORD`、`ALIST_WEBDAV_PASS`、`JWT_SECRET`、`MQTT_PASSWORD`。
   * 生产环境真实私有或公网 IP、动态中继端口号。
2. **规范做法**：
   * 所有机密必须存放在本地未被跟踪的 `.env` 或 `configs/config.local.yaml` 中，或统一在外部安全手册（如 `NAS服务运维指南.md`）中离线归档。
   * 代码和 `compose.yaml` 中一律使用 `${ENV_VAR}` 占位符，严禁提供含生产真实凭据的默认后备值 `${ENV_VAR:-secret}`。
   * 每次执行 `git commit` 或 `git push` 前，必须自查 `git diff` 是否有凭据泄露。

## 生产环境与网络设备资料（连接与凭据指南）

> **资料离线归档目录**：`C:\Users\Administrator\Desktop\设备资料`  
> 包含核心手册：《NAS服务运维指南.md》、《网络设备运维管理手册.md》、《运维交接与上线操作手册.md》、《安全防御中心改进方案.md》及网络架构图。

### 1. 核心网络与主机访问端点

| 维度 / 服务 | 连接地址与参数 | 认证与使用说明 | 运维说明 |
|---|---|---|---|
| **SSH 登录 (Tailscale)** | `100.90.67.71:22` 或 `ssh nas` | 用户名: `momo`，私钥: `~/.ssh/id_ed25519` | 推荐方式，原生免密，具备 sudo 权限 |
| **SSH 登录 (局域网)** | `192.168.31.234:22` 或 `ssh nas-lan` | 用户名: `momo`，凭据同上 | 局域网静态 IP 直连，低延迟（~2ms） |
| **SSH 登录 (H3C 公网穿透)** | `154.8.195.220:<动态端口>` | 查看 NAS 端 `/vol1/docker/home-datacenter/data/h3c/ssh_url.txt` 取得当前端口 | H3C Oasis 动态转发通道，用于外出无 VPN 直连 |
| **NAS 物理路径** | `/vol1/docker/home-datacenter` | 容器工作根目录（生产部署目录不含 `.git`） | 统一由 `deploy-nas.ps1` 增量同步部署 |
| **Web 控制台 (局域网)** | `http://192.168.31.234:8088/` | 局域网直连，首字节响应 ~10ms | React 18 SPA + Nginx 反向代理 |
| **Web 控制台 (H3C 高速通道)** | `https://dashboard.feiyemomo.top/fast` | 自动 302 秒级重定向至国内高带宽动态端口 | 延迟 30~50ms，免翻墙极速访问 |
| **Web 控制台 (Cloudflare)** | `https://dashboard.feiyemomo.top/` | Cloudflare Tunnel 保护入口 | 双栈不可达时的跨网穿透保底通道 |
| **API 服务端点** | `https://api.feiyemomo.top/` \| 内网: `:8080` | REST API 与 WebSocket 鉴权端点 (`/api/v1/*`) | 健康检查端点 `/health` |
| **WebRTC 穿透端口** | `154.8.195.220:<动态端口>` (TCP) | 查看 NAS 端 `/vol1/docker/home-datacenter/data/h3c/webrtc_url.txt` | 仅支持 TCP，与 go2rtc 候选地址联动 |
| **核心路由器 (UR7208-P-E)** | `192.168.77.1` 或简优云 `ucloud.h3c.com` | 账号: `admin` / 密码见运维手册 | 核心路由器 / AC 一体机 / PoE 供电管理 |
| **光猫 (创维 SK-D840N)** | `192.168.1.1` (VLAN 100, 桥接) | 超管账号: `CMCCAdmin` / 宽带账号见运维手册 | 移动 10G XG-PON 光猫，PPPoE 拨号交由 UR7208 |
| **监控摄像头 (3路 IPC)** | `192.168.31.100~102:554` | 账号: `admin` / 密码见运维手册 | 海康/皓视通 IPC，统一经 PoE 交换机供电并接入 Frigate |

---

## 🎥 WebRTC 实时流媒体架构与连接失败深度排查指南

本项目在 Web 前端 (`web/src/hooks/useWebRTCStream.ts`) 和 Android 客户端 (`Android/app/.../WebRtcClient.kt`) 中均采用 WebRTC (WHEP) 作为超低延迟（<500ms）主播放通道。在实际运行中，WebRTC 连接建立失败通常分为**信令阶段**与**媒体 ICE 连通阶段**两大类。

### 1. WebRTC 连接全流程拓扑
```
[客户端 App/Web]
   │
   ├─ 1. 信令阶段 (SDP Offer/Answer) ───────────────────────────────┐
   │     POST /api/v1/cameras/:id/webrtc (带 JWT)                   │
   │     ↓                                                          │
   │  [home-api] ──> [home-frigate:1984/api/webrtc] (go2rtc 核心)    │
   │     ↓                                                          │
   │  SDP Answer (携带所有候选地址: LAN, Tailscale, WAN, H3C)        │
   │                                                                │
   └─ 2. 媒体连通阶段 (ICE Connectivity Checks & DTLS) ─────────────┘
         客户端向各个 candidate 发送 STUN Binding Request (端口 8555)
         成功打通后建立 SRTP 视频流传输
```

### 2. WebRTC 连接建立失败的关键根本原因分析 (Root Causes)

#### 原因一：公网/外网环境下 H3C Oasis 隧道的「TCP 单向代理与 UDP 黑洞」陷阱 (P0 最核心阻断)
- **网络现状**：
  1. 家用移动宽带为运营商级 **对称型 NAT (Symmetric NAT / NAT444)**，STUN 反射候选 (`srflx`) 无法实现常规 UDP P2P 打洞。
  2. NAS 当前显式禁用了 IPv6（`.env` 中 `NAS_IPV6_DISABLED=true`），无法走 IPv6 直连。
  3. Cloudflare Tunnel (`dashboard.feiyemomo.top`) 为 HTTP/HTTPS 反向代理，**不代理 UDP 8555 媒体流量**。
- **致命陷阱**：
  H3C Oasis 穿透保活服务 (`home-h3c-keepalive`) 维护的 `154.8.195.220:<动态端口>` 是**纯 TCP 端口转发**，**不支持 UDP**。
  然而，go2rtc 生成 SDP Answer 时，会根据 `candidates` 列表同时生成**两条** `154.8.195.220` 的候选：
  - `a=candidate:... 1 udp 2130706421 154.8.195.220 <port> typ host` (UDP 候选，优先级 ~2130706421)
  - `a=candidate:... 1 tcp 1671430134 154.8.195.220 <port> typ host tcptype passive` (TCP 候选，优先级 ~1671430134)
- **后果**：
  根据 RFC 8445 规范，客户端（浏览器与 Android `stream-webrtc-android`）必定**优先尝试高优先级的 UDP 候选**。客户端将 STUN 请求发往 `154.8.195.220:<port>` UDP，全部被黑洞丢弃。客户端在指数退避等待（0.5s、1s、2s、4s...）中耗尽时间，而客户端的看门狗超时（Android 为 6 秒，Web 为 8 秒）**提前触发熔断**，直接判定 WebRTC 失败并回退到 HLS/MP4，根本来不及尝试 TCP 候选！
- **实测验证结果**：
  - `UDP 154.8.195.220:34680 failed: timed out`（彻底不可达）
  - `TCP 154.8.195.220:34680 SUCCESS!`（秒级响应 STUN 0x0101）
- **解决对策**：
  在 `services/api/internal/camera/sdp.go` 的 `RewriteH3CCandidates` 中，必须**剥离/删除发往 H3C 隧道的 UDP 候选**，仅保留 `tcp passive` 候选，使客户端跳过无效的 UDP 探测，秒级建立 TCP ICE 链路。

#### 原因二：H3C 动态中转端口漂移与配置不同步
- H3C 简优云隧道每 2.5~3 小时重新分配外网端口（如从 `58388` 变为 `34680`）。
- 若 `home-api` 容器内缓存未更新、或本地 `config.yml` 写死旧端口且 go2rtc 未热加载，SDP Answer 会返回已失效的历史端口，导致客户端连接超时。
- 动态端口排查命令：
  ```bash
  ssh nas "cat /vol1/docker/home-datacenter/data/h3c/webrtc_url.txt"
  ```

#### 原因三：摄像头原生码流编解码不匹配 (SDP 502 Bad Gateway)
- 海康摄像机主码流默认为 **H.265 (HEVC)** 视频和 **PCMA** 音频。Chrome/Firefox 等大部分浏览器原生 WebRTC 仅支持 H.264 与 Opus/AAC 音频。
- 若请求未经转码的码流，go2rtc 会直接返回 500/502：`codecs not matched: video:H265, audio:PCMA`。
- **解决规则**：
  1. 实时预览统一使用子码流或开启 VAAPI 核显硬解标签：`#video=h264#hardware=vaapi`。
  2. 针对纯视频场景，添加 `#audio=0` 避免因音频格式不支持导致协商失败。
  3. 冷流拉起超时：首次启动 ffmpeg 转码可能耗时 3~5 秒，后端需保持重试机制（已在 `camera_handler.go` 实现 2 次退避重试）。

#### 原因四：go2rtc 守护进程崩溃重启
- go2rtc 1.9.10 在部分客户端断开 MP4/HLS 流时存在空指针 panic (`github.com/AlexxIT/go2rtc/pkg/mp4.(*Consumer).AddTrack`)，会导致容器内 go2rtc 重启，瞬间中断所有现有 WebRTC 媒体通道。可通过 `docker exec home-frigate cat /dev/shm/logs/go2rtc/current` 查看是否有 panic 堆栈。

---

## 便捷脚本

项目根目录提供以下一键脚本，覆盖开发与运维高频操作：

| 脚本 | 用途 | 说明 |
|---|---|---|
| `deploy-nas.ps1` | 部署到 NAS | 构建并打包项目，scp 推送至 NAS 并 `docker compose up -d --build` |
| `test-ws.ps1` | WebSocket 连接测试 | 从 `services/api/scripts/` 迁移至根目录，验证 WS 通道是否畅通 |
| `get-token.ps1` | 获取测试 JWT | 调用 `/api/v1/auth/bind` 拿到 token 并写入 `$env:HC_TOKEN`，便于下游 API 调用 |
| `commit.ps1` | 一键 git 提交推送 | 交互式展示 `git status`、提示输入提交信息，提交后自动 `git push` |

### deploy-nas.ps1 — 部署到 NAS

```powershell
# 从项目根目录运行（密码模式，无需 SSH 密钥）
cd D:\Projects\home-datacenter
.\deploy-nas.ps1 -Password '@Fnos324'         # 构建并部署

# 仅更新 compose.yaml/.env（不重新构建镜像）
.\deploy-nas.ps1 -Password '@Fnos324' -NoBuild

# 部署后查看 api 日志
.\deploy-nas.ps1 -Password '@Fnos324' -Logs

# 预览将要打包的文件（不实际部署）
.\deploy-nas.ps1 -DryRun
```

脚本流程：tar 打包（排除 data/.git/node_modules/.env）→ scp 到 NAS → 远程 tar 解压 → 验证 .env 存在 → `docker compose up -d --build` → 打印服务状态。

### test-ws.ps1 — WebSocket 连接测试

```powershell
# 默认测试本地 (localhost:8080)
.\test-ws.ps1 -AccessKey "<key>"

# 测试 NAS 上的 WebSocket
.\test-ws.ps1 -AccessKey "<key>" -BaseUrl "http://192.168.1.3:8080"
```

### get-token.ps1 — 获取测试 JWT

```powershell
# 默认从 LAN 获取
.\get-token.ps1

# 从 Cloudflare 中继域名获取
.\get-token.ps1 -BaseUrl "https://dashboard.feiyemomo.top"

# 获取并复制到剪贴板
.\get-token.ps1 -Copy

# 设置 $env:HC_TOKEN 供后续 API 调用使用（需 dot-source 运行）
. .\get-token.ps1
```

> 注意：脚本会设置 `$env:HC_TOKEN`，但因 PowerShell 作用域限制，需 dot-source（`. .\get-token.ps1`）执行方可在当前 shell 中保留该环境变量。

### commit.ps1 — 一键 git 提交推送

```powershell
# 交互模式：显示 git status，提示输入提交信息，提交后自动 push
.\commit.ps1

# 非交互模式：直接传入提交信息
.\commit.ps1 -Message "fix: ..."

# 预览将要提交的内容（不实际提交）
.\commit.ps1 -DryRun
```

## Dashboard 测试用户

| 项 | 值 |
|---|---|
| User ID | `1` |
| Access Key | `ebc94f7fe99b497a9bdec7bd45add929360e4386be3cad96a8b3922d1d680a05` |

获取 JWT：
```powershell
$body = @{ user_id = 1; access_key = "ebc94f7fe99b497a9bdec7bd45add929360e4386be3cad96a8b3922d1d680a05" } | ConvertTo-Json
$resp = Invoke-RestMethod -Uri "http://192.168.1.3:8080/api/v1/auth/bind" -Method POST -Body $body -ContentType "application/json"
$token = $resp.data.token
```

登录 Dashboard：访问 `http://192.168.1.3/`，在登录页输入 user_id=1 和 access-key。

## 标准工作流

1. **了解项目** — 先读 `README.md` + `docs/ai-context.md`
2. **修改代码** — 在本地编辑，遵循项目已有风格（Go: gofmt；TS: 2-space 缩进，双引号）
3. **部署到 NAS** — `.\deploy-nas.ps1 -Password '@Fnos324'`
4. **验证** — 使用 chrome-devtools MCP 浏览 `http://192.168.1.3/` 或 `https://dashboard.feiyemomo.top/`，检查页面行为和 console
5. **更新文档** — 更新 `docs/ai-context.md` 的 Phase 记录和 `README.md` 的更新日志
6. **Git 提交** — `git add <files>` + `git commit -m "..."` + `git push`

## 常用验证与调试命令

```powershell
# 容器状态（直接使用 ~/.ssh/config 中配置好的别名 nas 或局域网 nas-lan）
ssh nas "cd /vol1/docker/home-datacenter && docker compose ps"

# API 健康检查
curl -s http://192.168.31.234:8080/health

# 查看 api 日志
ssh nas "cd /vol1/docker/home-datacenter && docker compose logs --tail=50 api"

# 查看 frigate / go2rtc 日志
ssh nas "cd /vol1/docker/home-datacenter && docker compose logs --tail=50 frigate"

# 查看 go2rtc 实时内部运行与流日志
ssh nas "docker exec home-frigate cat /dev/shm/logs/go2rtc/current"

# 查看当前生效的 H3C WebRTC 动态转发端口与 SSH 动态端口
ssh nas "cat /vol1/docker/home-datacenter/data/h3c/webrtc_url.txt; echo ''; cat /vol1/docker/home-datacenter/data/h3c/ssh_url.txt"
```

## 注意事项

- `.env` 文件不入 Git（每个环境独立维护 JWT_SECRET 和 MQTT_PASSWORD）
- `deploy/mosquitto/passwd` 不入 Git（环境特定）
- SSH 密码 `@Fnos324` 含特殊字符，PowerShell 中需用单引号包裹
- 部署脚本会自动排除 `data/`、`.git/`、`node_modules/`、`*.log` 等
- chrome-devtools MCP 可用于自动化浏览器验证（导航、截图、检查 console）
