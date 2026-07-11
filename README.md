# Home Datacenter

家庭数据中心 / 家庭管理应用 — 一个包含 **API（Go）**、**Web（Vue SPA）** 和 **MQTT Broker（Mosquitto）** 三件套的全栈项目，本仓库提供完整的 Docker 一键部署方案。

---

## 目录

- [项目概览](#项目概览)
- [架构](#架构)
- [前置条件](#前置条件)
- [快速开始](#快速开始)
- [配置说明](#配置说明)
- [部署步骤](#部署步骤)
- [验证](#验证)
- [常用运维命令](#常用运维命令)
- [生产环境部署（Cloudflare Tunnel）](#生产环境部署cloudflare-tunnel)
- [数据持久化与备份](#数据持久化与备份)
- [常见问题](#常见问题)

---

## 项目概览

| 组件 | 技术栈 | 容器名 | 默认端口 |
|---|---|---|---|
| API | Go 1.26 + Gin | `home-api` | `8080`（仅本地） |
| Web | Vue 3 + Vite + Nginx | `home-web` | `80`（仅本地） |
| MQTT Broker | Eclipse Mosquitto 2 | `home-mosquitto` | 1883（**不对外暴露**） |
| go2rtc | AlexxIT/go2rtc | `home-go2rtc` | `1984`（仅本地） |

Phase 4（摄像头平台化）新增 `home-go2rtc`：所有摄像头的 RTSP 由
`home-api` 注册并加密入 SQLite，再以**友好名称（如"前门"）**为名推送给
go2rtc；前端通过 **HLS（主）** 或 **WebRTC（备）** 拉流。详见
[`docs/platformization.md`](docs/platformization.md)。

Phase 5（事件驱动 + 自动化引擎）将所有 Device / Camera / MQTT 状态变化统一为
Event 进入 EventBus，再驱动 WebSocket 推送与 Automation Engine
（rule = trigger + condition + action，支持 notify / mqtt / webhook 三种动作）。
详见 [`docs/platformization.md`](docs/platformization.md) 的 Phase 5 一节与
[`docs/security.md`](docs/security.md) §11。

Phase 6（自动化运行时）将规则引擎升级为可执行的 Runtime：扩展 Condition
（`source` / `threshold` / `regex` / `any`）、Action（`timeout_ms` / `retry_max`）、
新增 Throttle（`cooldown_s` / `rate_per_min` / `dedup`）与 Metrics
（`/metrics`、`/rules/:id/metrics`、`?reset=1`、`/cooldown`），
每次 fire 还会发布 `automation.fired` 审计事件。详见
[`docs/platformization.md`](docs/platformization.md) §7。

所有服务都在 `home-net` 内部 Docker 网络中互相通信。默认只把 `80` 和 `8080` 绑定到 `127.0.0.1`，避免直接对外暴露。

---

## 架构

```
   ┌──────────────┐    HTTP/WS   ┌──────────────┐   MQTT  ┌──────────────┐
   │   home-web   │ ◄──────────► │   home-api   │ ◄─────► │ home-mosquitto│
   │  (nginx SPA) │              │ (Go + Gin)   │         │   (broker)   │
   └──────────────┘              └──────┬───────┘         └──────────────┘
          │                              │ RTSP push             │
       127.0.0.1:80                 127.0.0.1:8080              │
          │                              │                ┌──────▼──────┐
          └────── 外部经 Cloudflare Tunnel 暴露 ────────► │ home-go2rtc │
                                                           │ :1984      │
                                                           │ WebRTC/HLS │
                                                           └────────────┘
```

---

## 前置条件

1. **Docker** ≥ 20.10
2. **Docker Compose** ≥ 2.x（`docker compose` 子命令，已内置于 Docker Desktop / 较新 docker-ce）
3. **OpenSSL**（仅首次部署生成密钥时需要）
4. 端口 `80`、`8080` 未被占用（本机）
5. **HEVC-capable 浏览器**（用于查看摄像头实时画面）—— Safari / Edge / Chrome on Apple Silicon 原生支持；Chrome on Windows 11 需在 Microsoft Store 安装 [HEVC Video Extensions](https://apps.microsoft.com/detail/9n4wgh0nt6jv)；**Chrome on Linux 与 Firefox 不支持 HEVC，无法播放实时视频流**。详细说明见 [docs/platformization.md — Browser / Codec requirement](docs/platformization.md#browser--codec-requirement-hard)。

检查环境：

```bash
docker --version
docker compose version
```

---

## 快速开始（5 步）

```bash
# 1. 克隆代码
git clone <your-repo-url> home-datacenter
cd home-datacenter

# 2. 创建 .env（包含 JWT 密钥、MQTT 密码等）
cp .env.example .env

# 3. 编辑 .env，至少修改 JWT_SECRET 和 MQTT_PASSWORD
#    Linux/macOS:
#      sed -i '' "s|JWT_SECRET=.*|JWT_SECRET=$(openssl rand -hex 32)|" .env
#    Git Bash / Linux:
sed -i "s|JWT_SECRET=.*|JWT_SECRET=$(openssl rand -hex 32)|" .env
sed -i "s|MQTT_PASSWORD=.*|MQTT_PASSWORD=$(openssl rand -hex 16)|" .env

# 4. 用相同的 MQTT_PASSWORD 生成 Mosquitto 密码文件
#    Windows (Git Bash) 写法见下文的 [部署步骤]。
docker run --rm -v "$(pwd)/deploy/mosquitto:/work" \
  docker.m.daocloud.io/library/eclipse-mosquitto:2 \
  mosquitto_passwd -c -b /work/passwd home-datacenter "$(grep ^MQTT_PASSWORD= .env | cut -d= -f2)"

# 5. 启动！
docker compose up -d --build
```

启动后：

- Web 控制台：<http://localhost>
- API 健康检查：<http://localhost:8080/health>
- 首次启动会自动在 `data/sqlite/app.db` 中 bootstrap 默认管理员账号，账号信息请查看 API 启动日志。

---

## 配置说明

所有运行时配置通过 `compose.yaml` 引用的 `.env` 文件注入。`docker compose` 会自动加载同目录下的 `.env`。

### `.env` 关键变量

| 变量 | 必填 | 说明 |
|---|---|---|
| `JWT_SECRET` | ✅ | API 签发 JWT 用的密钥，**≥ 32 字符**，否则服务无法启动 |
| `MQTT_USERNAME` | ✅ | Mosquitto 用户名，默认 `home-datacenter` |
| `MQTT_PASSWORD` | ✅ | Mosquitto 密码，必须与 `deploy/mosquitto/passwd` 中的记录一致 |
| `MQTT_BROKER` | ❌ | 默认 `tcp://mosquitto:1883`（走 Docker 内部网络） |
| `MQTT_CLIENT_ID` | ❌ | 默认 `home-datacenter` |
| `SERVER_PORT` | ❌ | 默认 `8080` |
| `DB_PATH` | ❌ | 默认 `/data/sqlite/app.db`（容器内） |
| `GO2RTC_BASE_URL` | ❌ | go2rtc HTTP API（摄像头平台化用），默认 `http://home-go2rtc:1984` |
| `WEBRTC_PUBLIC_BASE` | ❌ | 浏览器拉流用的 go2rtc URL 前缀。**推荐 `/go2rtc`**（由 dashboard nginx 反代到 go2rtc，同源无 CORS）；留空 = 仅 LAN（返回 Docker 内部地址，浏览器不可达）；`https://cam.example.com` = 独立 Cloudflare Tunnel 域名 |

> ⚠️ **不要把 `.env` 提交到 Git**（已经在 `.gitignore` 中忽略）。

### `compose.yaml` 网络策略

- **只**将 `127.0.0.1:80` 和 `127.0.0.1:8080` 绑定到本机回环地址，**不**对外暴露。
- Mosquitto 默认 **不**绑定主机端口。如果需要本地物理设备连入测试，取消 `compose.yaml` 第 49 行附近的注释，并设置 `MQTT_BIND_PORT` 环境变量。
- 所有服务都在 `home-net` bridge 网络中通过服务名互通：`api → mosquitto:1883`、`web → api:8080`。

---

## 部署步骤（详细版）

### 1. 准备环境文件

```bash
cp .env.example .env
```

用编辑器打开 `.env`，**至少**替换以下两个值：

```dotenv
JWT_SECRET=请用 openssl rand -hex 32 生成
MQTT_PASSWORD=请用 openssl rand -hex 16 生成
```

### 2. 生成 Mosquitto 密码文件

Mosquitto 配置里 `allow_anonymous=false`，所以**必须**为 `home-datacenter` 用户生成密码文件，且密码必须与 `.env` 中的 `MQTT_PASSWORD` 一致。

**Linux / macOS：**

```bash
docker run --rm -v "$(pwd)/deploy/mosquitto:/work" \
  eclipse-mosquitto:2 \
  mosquitto_passwd -c -b /work/passwd home-datacenter "$(grep ^MQTT_PASSWORD= .env | cut -d= -f2)"
```

**Windows（Git Bash）：**

```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd)/deploy/mosquitto:/work" \
  docker.m.daocloud.io/library/eclipse-mosquitto:2 \
  mosquitto_passwd -c -b /work/passwd home-datacenter "$(grep ^MQTT_PASSWORD= .env | cut -d= -f2)"
```

**Windows（PowerShell）：**

```powershell
$pw = (Get-Content .env | Select-String '^MQTT_PASSWORD=').ToString().Split('=')[1]
docker run --rm -v "${PWD}\deploy\mosquitto:/work" `
  docker.m.daocloud.io/library/eclipse-mosquitto:2 `
  mosquitto_passwd -c -b /work/passwd home-datacenter $pw
```

### 3. 启动服务

```bash
# 首次或代码有改动时加 --build 重新构建镜像
docker compose up -d --build
```

输出示例：

```
[+] Running 4/4
 ✔ Network home-datacenter_home-net  Created
 ✔ Container home-mosquitto          Started
 ✔ Container home-api                Started
 ✔ Container home-web                Started
```

### 4. 查看启动日志

```bash
docker compose logs -f api
docker compose logs -f mosquitto
docker compose logs -f web
```

正常情况下你会看到：

- `mosquitto version 2.x starting` + `Config loaded from /mosquitto/config/mosquitto.conf.`
- `api`：`server started on :8080` + `mqtt connected to tcp://mosquitto:1883`
- `web`：nginx 启动（不会主动输出到 stdout，访问 `http://localhost` 验证）

---

## 验证

```bash
# 1. 容器状态
docker compose ps
# 期望：所有服务 STATUS = Up / healthy

# 2. Web 健康检查
curl -s http://localhost/health
# 期望：{"status":"ok"}

# 3. Web 前端
# 浏览器打开 http://localhost，应能看到登录页

# 4. MQTT 连通性（可选）
docker exec -it home-mosquitto mosquitto_sub -u home-datacenter -P "$MQTT_PASSWORD" -t 'home-datacenter/#' -v -C 1
```

---

## 常用运维命令

| 容器名 | 状态 | 端口 | 说明 |
|---|---|---|---|
| `home-api` | Up | `127.0.0.1:8080` | Go API（健康检查 `/health`） |
| `home-web` | Up | `127.0.0.1:80` | Dashboard SPA |
| `home-mosquitto` | Up | 仅内部 `1883` | MQTT broker |
| `home-go2rtc` | Up | `127.0.0.1:1984` | 摄像头 RTSP→WebRTC/HLS 桥 |

`/api/v1/cameras` 走完后应能看到空列表：

```bash
curl -sS -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/cameras
# {"code":0,"data":[]}
```

注册一台海康摄像头（推荐使用 Dashboard `/cameras/new` 页面，含厂商
预设、RTSP URL 实时预览、密码可见性切换；底层接口仍是下面的
`POST /api/v1/cameras`）：

```bash
curl -sS -X POST http://localhost:8080/api/v1/cameras \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
    "name":"前门",
    "vendor":"hikvision",
    "host":"192.168.31.100",
    "channel_id":101,
    "username":"admin",
    "password":"your-pass",
    "ptz": true, "audio": true, "motion": true
  }'
# → {"code":0,"data":{"id":1,"stream":{"webrtc_url":"...","hls_url":"..."}}}
```

WebRTC 拉流（浏览器侧）：

```html
<video id="v" autoplay playsinline controls muted style="width:100%"></video>
<script>
  const rtc = new RTCPeerConnection({iceServers:[{urls:"stun:stun.cloudflare.com:3478"}]});
  rtc.addTransceiver("video", {direction:"sendrecv"});
  rtc.createOffer()
    .then(o => rtc.setLocalDescription(o))
    .then(() => fetch("http://localhost:1984/api/webrtc?src=前门", {
      method:"POST", headers:{"Content-Type":"application/sdp"},
      body: rtc.localDescription.sdp
    }))
    .then(r => r.text())
    .then(a => rtc.setRemoteDescription({type:"answer", sdp:a}));
  rtc.ontrack = e => document.getElementById("v").srcObject = e.streams[0];
</script>
```

PTZ（管理员 token）：

```bash
curl -sS -X POST http://localhost:8080/api/v1/cameras/1/ptz \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"command":"left","speed":0.5}'
# 2 秒后自动停，需要立刻停：
curl -sS -X POST http://localhost:8080/api/v1/cameras/1/ptz \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"command":"stop"}'
```

> `profile_token` 可省略——首次 PTZ 调用时自动通过 ONVIF `GetProfiles` 发现并持久化，后续调用直接复用。ONVIF 认证使用 WS-Security PasswordDigest（非 HTTP Basic Auth）。

完整文档：[`docs/platformization.md`](docs/platformization.md)。

### Automation Engine（Phase 5，管理员 only）

所有 Device / Camera / MQTT 状态变化都进入 EventBus；Automation Engine 订阅
`*`，按规则（trigger + condition + action）触发动作。规则 CRUD 走
`/api/v1/automation/rules`，全部要求 admin。

创建一条规则（摄像头离线时发通知）：

```bash
curl -sS -X POST http://localhost:8080/api/v1/automation/rules \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{
    "name":"摄像头离线通知",
    "trigger":"camera.offline",
    "condition":{"payload_eq":{"source":"camera"}},
    "action":{"type":"notify","user_id":1,"title":"camera offline","body":"camera went offline"},
    "enabled": true
  }'
# → {"code":0,"data":{"id":1,...,"fire_count":0}}
```

晚间 22:00 之后任意设备事件触发 MQTT 发布（带时间窗口）：

```bash
curl -sS -X POST http://localhost:8080/api/v1/automation/rules \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{
    "name":"夜间设备事件转发",
    "trigger":"device",
    "condition":{"time_gte":"22:00","time_lte":"06:00"},
    "action":{"type":"mqtt","topic":"home-datacenter/automation/night","payload":"event fired","qos":1},
    "enabled": true
  }'
```

手动测试某条规则（不增加 `fire_count`）：

```bash
curl -sS -X POST http://localhost:8080/api/v1/automation/rules/1/test \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -d '{"payload":{"source":"camera","camera_id":1}}' \
  -H "Content-Type: application/json"
```

动作类型：

| `action.type` | 行为 | 约束 |
|---|---|---|
| `notify` | 在 EventBus 上发 `user.notification`（前端 WS 推送） | 需 `user_id` + `title` + `body` |
| `mqtt` | 向 Mosquitto 发布消息 | topic 必须在 `home-datacenter/` 命名空间下；`$SYS` 被拒 |
| `webhook` | HTTP POST 到外部 URL | host 必须是公网 IP；私网 / loopback / link-local 在 fire 时被拒（SSRF 守卫） |

> 安全细节见 [`docs/security.md`](docs/security.md) §11。

### Automation Runtime（Phase 6，管理员 only）

在 Phase 5 的基础上，规则多了 **节流（throttle）**、**超时 / 重试**、**可观测（metrics）** 与 **应急静音** 能力：

- **Condition 扩展**：`source`（精确匹配 Event 来源）、`threshold`（数值比较，如 `{"confidence":{"op":">=","val":0.8}}`）、`regex`（RE2）、`any`（OR 组合）。
- **Action 扩展**：`timeout_ms`（单次超时，默认 5000ms）、`retry_max`（webhook 专用；4xx 永久失败不重试，5xx / 网络错误指数退避 500ms×2^n，封顶 30s）。
- **Throttle**：`cooldown_s`（静默窗口）、`rate_per_min`（60s 滑窗）、`dedup`（合并相同事件）。
- **Metrics**：
  ```bash
  curl -sS "http://localhost:8080/api/v1/automation/metrics" -H "Authorization: Bearer $ADMIN_TOKEN"
  curl -sS "http://localhost:8080/api/v1/automation/metrics?reset=1" -H "Authorization: Bearer $ADMIN_TOKEN"
  curl -sS "http://localhost:8080/api/v1/automation/rules/1/metrics" -H "Authorization: Bearer $ADMIN_TOKEN"
  ```
- **应急静音**（不删除规则，仅临时压制触发）：
  ```bash
  curl -sS -X POST "http://localhost:8080/api/v1/automation/rules/1/cooldown" \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
    -d '{"seconds":3600}'
  ```
- **审计事件**：每次 fire 都会在 EventBus 上发 `automation.fired`（rule id、trigger、event id、ok/err、duration_ms），前端 WS 自动收到，可用于活动流。

示例：摄像头在夜间且置信度 ≥ 0.8 时通过 webhook 推送：

```bash
curl -sS -X POST http://localhost:8080/api/v1/automation/rules \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{
    "name":"夜间高置信度推送",
    "trigger":"camera",
    "condition":{
      "time_gte":"22:00","time_lte":"06:00",
      "source":"camera",
      "payload_eq":{"event":"motion"},
      "threshold":{"confidence":{"op":">=","val":0.8}}
    },
    "action":{
      "type":"webhook",
      "url":"https://example.com/hook",
      "method":"POST",
      "payload":"{\"event\":\"motion\"}",
      "timeout_ms":3000,
      "retry_max":2
    },
    "throttle":{"cooldown_s":30,"rate_per_min":5,"dedup":true},
    "enabled": true
  }'
```

---

## 生产环境部署（Cloudflare Tunnel）

仓库内 `deploy/cloudflared/` 已经预留了 Cloudflare Tunnel 接入脚本，思路如下：

1. **不要**把 `8080` 端口映射到 `0.0.0.0`，只保留 `127.0.0.1:80` 给 `cloudflared`。
2. 在 Cloudflare 控制台创建 Tunnel，把 `cloudflared` 容器加入 `home-net` 网络，并通过 `http://home-api:8080`、`http://home-web:80`、`http://home-go2rtc:1984` 访问后端。
3. 在 `services/api/configs/config.yaml` 的 `server.allowed_origins` 中填入生产域名，开启 WebSocket Origin 校验，防止 CSWSH。
4. **不要**把 Mosquitto 暴露到公网——Tunnel 也只代理 HTTP，不代理 MQTT。
5. 摄像头经 `cam.feiyemomo.top` 暴露，**HLS 直接走 Tunnel**（HTTP-only，无 UDP 依赖）；WebRTC 仅在 LAN/直连或 TURN 方案下使用，详见 `docs/platformization.md`。
6. 在 `config.yaml`（或 `.env`）中设置 `camera.webrtc_public_base=https://cam.feiyemomo.top`，使 API 返回浏览器可访问的 `webrtc_url` / `ice.webrtc_base`。

> 详细配置见 `deploy/cloudflared/` 中的示例。

---

## 数据持久化与备份

`compose.yaml` 中通过 bind mount 持久化以下目录，删除容器不会丢失数据：

| 主机路径 | 容器内路径 | 内容 |
|---|---|---|
| `./data/sqlite` | `/data/sqlite` | SQLite 数据库（含用户、设备、消息） |
| `./data/mosquitto` | `/mosquitto/data` | Mosquitto 持久化数据（订阅、保留消息） |
| `./deploy/mosquitto/passwd` | `/mosquitto/config/passwd` | MQTT 密码文件（**只读**） |
| `./deploy/mosquitto/mosquitto.conf` | `/mosquitto/config/mosquitto.conf` | Broker 配置（**只读**） |
| `./deploy/mosquitto/aclfile` | `/mosquitto/config/aclfile` | ACL 文件（**只读**） |
| `./services/api/configs` | `/configs` | API 配置（**只读**） |

> 注：`go2rtc.yaml` **不**通过 bind mount 挂载，而是在 `deploy/go2rtc/Dockerfile` 中 `COPY` 进镜像，以保证 go2rtc 可在运行时改写该文件（Windows Docker Desktop 的单文件 bind mount 可能导致静默写入失败）。详见 `docs/platformization.md` 的 "go2rtc API integration" 一节。

### 备份建议

```bash
# 备份 SQLite + Mosquitto 状态
tar -czf home-datacenter-$(date +%F).tar.gz \
  data/ deploy/mosquitto/passwd .env
```

把这个 tar 包存到云盘或异地即可。

### 恢复

```bash
tar -xzf home-datacenter-2026-07-04.tar.gz
docker compose up -d
```

---

## 常见问题

### Q1：`home-mosquitto` 启动失败，提示 `passwd is not a file`

`deploy/mosquitto/passwd` 是空目录或者不存在。回到 [部署步骤 2](#2-生成-mosquitto-密码文件) 重新生成。

### Q2：API 启动后日志说 `WARNING: mqtt connect failed`

通常是 `MQTT_PASSWORD` 和 Mosquitto 的 `passwd` 文件对不上。检查：
- `.env` 里的 `MQTT_PASSWORD` 是否被改过？
- `deploy/mosquitto/passwd` 是否是用同样的密码重新生成的？

### Q3：API 启动失败，提示 `jwt secret too short`

`JWT_SECRET` 长度 < 32。请用 `openssl rand -hex 32` 重新生成。

### Q4：访问 `http://localhost` 502/连接被拒

`home-api` 或 `home-web` 还没起来。`docker compose ps` 查看状态，`docker compose logs -f` 查日志。

### Q5：镜像拉取太慢

`Dockerfile` 已经使用 `docker.m.daocloud.io` 国内镜像源。如果需要切换，编辑两个 `Dockerfile`（`services/api/Dockerfile` 和 `web/Dockerfile`）以及 `compose.yaml` 中 `mosquitto` 的 `image:` 字段。

### Q6：如何在本地暴露 Mosquitto 给物理设备测试

1. 编辑 `compose.yaml`，把 `mosquitto` 服务下的 `ports:` 注释打开；
2. `.env` 中加上 `MQTT_BIND_PORT=1883`（可改成别的）；
3. `docker compose up -d`。

### Q7：彻底重置

```bash
docker compose down -v            # 删容器 + 命名卷
rm -rf data/ deploy/mosquitto/passwd
# 然后从「快速开始」第 1 步重新来
```

### Q8：Dashboard 上设备一直显示 offline，但 Mosquitto 能收到消息

`GET /api/v1/system/status` 返回 `online_device_count: 0`，
`docker logs home-api` 却能看到 `mqtt: rx home-datacenter/devices/5/status = ...`。

最常见的原因有两类：

1. **设备发的是不带引号的"伪 JSON"**（手写脚本/早期客户端常踩），
   例如 `{status:online,ts:1234567890}`。Go 的 `encoding/json` 会直
   接拒绝并打 `invalid character 's' looking for beginning of object key string`。
2. **MQTT 客户端没注册默认 publish handler**，broker 收到消息了
   但 paho 把它丢了。

现在的 `mqtt.Handler.handleStatus` 已经做了三层兜底（严格 JSON →
重新引号化 → 正则抠 `status=...`），并把解析结果以规范 JSON 重
新发到 EventBus，Dashboard 会通过 WS `device.status` 事件即时刷新，
不再依赖 5s 轮询。如果还是不对：

```bash
# 在 broker 容器里手工模拟一个标准 JSON 的 status
MSYS_NO_PATHCONV=1 docker exec home-mosquitto \
  mosquitto_pub -u home-datacenter -P "$MQTT_PASSWORD" \
    -t 'home-datacenter/devices/1/status' \
    -m '{"status":"online","ts":1234567890}'

# 5 秒内再看 /system/status
curl -s -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/system/status
# → "online_device_count": 1, "online_device_ids": [1]
```

注意：API 跟 Mosquitto 断连时会自动把所有设备置为 offline（`MarkAllOffline`），
不需要等 90s 才会变灰。

---

## License

Private / 家庭项目，未指定开源协议。
