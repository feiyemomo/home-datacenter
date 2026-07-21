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
| Web | React 18 + Vite + Nginx | `home-web` | `80`（仅本地） |
| MQTT Broker | Eclipse Mosquitto 2 | `home-mosquitto` | 1883（**不对外暴露**） |
| NVR / AI Detection | Frigate 0.17 (bundled go2rtc + OpenVINO) | `home-frigate` | `5000` (API) / `1984` (go2rtc)（仅本地） |

Phase 4（摄像头平台化）新增 go2rtc 桥接服务；Phase 9 升级为 **Frigate 0.17**
（`home-frigate`）——内置 go2rtc + OpenVINO AI 目标检测 + 24/7 录像。所有摄像头的
RTSP 由 `home-api` 注册并加密入 SQLite，同时推送给 Frigate 的 go2rtc（用于直播）
和 Frigate 的检测/录制管道（用于 AI 检测和录像）。前端通过 **HLS（主）** 或
**WebRTC（备）** 拉流。详见
[`docs/platformization.md`](docs/platformization.md) 和
[`docs/ai-context.md`](docs/ai-context.md) 的 Phase 9 一节。

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

Phase 7（播放面板 + 安全加固）做了三件事：
（1）前端新增 **明亮主题** 切换（`useTheme` + `data-theme` + Tailwind 颜色变量），用户可在 header 一键切换 light / dark 并跨标签页同步；
（2）播放面板新增 **WebRTC / HLS 手动切换** 控件（`LiveVideo` 头部三段式开关），偏好持久化到 `localStorage`，用于 HEVC 排障时锁定单一传输协议做对比；
（3）`/auth/bind` 加 **IP 速率限制**（token-bucket，默认 `rps=0.1, burst=5`，配置项 `auth.rate_limit.*`），429 与 401 返回相同 JSON 体避免被探测。详见
[`docs/platformization.md`](docs/platformization.md) §8、§9 和
[`docs/security.md`](docs/security.md) §13。

Phase 8（用户管理 API）补全了 **管理员对 User 的 CRUD 能力**：
`/api/v1/user` 下五条 admin-only 路由（list / create / get / update /
delete），含三个状态守卫（last-admin / self-delete / self-demote），
前端 `Users` 页面镜像相同守卫（直接禁用按钮，避免对注定被 400 的
请求做无谓 round-trip）。`DELETE /user/:id` 会级联删除该 user 名下
的 devices（但不会级联 cameras——cameras 的 `owner_id` 字段已经驱动
list / get 的 scope 过滤）。详见
[`docs/platformization.md`](docs/platformization.md) §10 和
[`docs/api-documentation.md`](docs/api-documentation.md) 的
"User Management (Admin)" 章节。

所有服务都在 `home-net` 内部 Docker 网络中互相通信。默认只把 `80` 和 `8080` 绑定到 `127.0.0.1`，避免直接对外暴露。

---

## 架构

```
   ┌──────────────┐    HTTP/WS   ┌──────────────┐   MQTT  ┌──────────────┐
   │   home-web   │ ◄──────────► │   home-api   │ ◄─────► │ home-mosquitto│
   │  (nginx SPA) │              │ (Go + Gin)   │         │   (broker)   │
   └──────────────┘              └──────┬───────┘         └──────────────┘
          │                              │ config push            │
       127.0.0.1:80                 127.0.0.1:8080              │ MQTT pub (frigate/#)
          │                              │                ┌──────▼──────┐
          └────── 外部经 Cloudflare Tunnel 暴露 ────────► │ home-frigate │
                                                           │ :5000 API   │
                                                           │ :1984 go2rtc │
                                                           │ WebRTC/HLS  │
                                                           │ AI Detection│
                                                           │ 24/7 Record │
                                                           └─────────────┘
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
| `home-frigate` | Up | `127.0.0.1:5000` / `127.0.0.1:1984` | NVR + AI 检测 + 直播（内置 go2rtc） |

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

**v1.6.0 Motion Ranges**（用于 Android 端录像 SeekBar 红标覆盖）：

```bash
# Returns motion-active time ranges (unix seconds) within [after, before)
curl -sS -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/v1/cameras/1/motion-ranges?after=1784533200&before=1784619600"
# {"code":0,"message":"success","data":{"ranges":[{"start":1784533265,"end":1784533344,"duration":79,"motion_score":469,"segment_count":8,"peak_objects":0},...],"total":109}}
```

后端实现：`internal/camera/frigate.go` 的 `ListMotionRanges` 分块查询 Frigate 录制段（每块 1h，低于 Frigate 500 段上限），用 2s gap 阈值合并相邻 motion 段（v1.6.3：从 v1.6.1 的 0s 改为 2s——0s 在 24h 内产生 ~750 个独立段过于密集；2s 是人眼"同一动作"的感知阈值，产生 ~109 段正好适合横向 chip 列表展示）。**v1.6.3：返回富结构 `[]MotionRange`**（之前是 `[][2]int64`），每个 range 含预聚合字段：`start/end/duration`（unix 秒）、`motion_score`（Frigate 段 motion 字段之和，反映 motion 强度）、`segment_count`（合并的 10s 段数）、`peak_objects`（合并段中 AI 检测到的最大对象数，>0 时 chip 显示红色）。所有聚合在后端完成，客户端零现场计算。**v1.6.3：进程内 60s TTL 缓存**——避免用户重复打开同一天的录像时反复请求 Frigate（1-2s 慢查询），缓存按 `<camera>:<after>:<before>` key 隔离，超过 32 条自动淘汰最旧的一半。

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
| `./deploy/frigate/config.yml` | `/config/config.yml` | Frigate 配置（全局设置：detectors、mqtt、go2rtc、record 保留策略；摄像头定义由 home-api 动态推送） |
| `./data/frigate` | `/media/frigate` | Frigate 数据：录制文件、frigate.db、模型缓存 |
| `./data/recordings` | `/data/recordings` | Frigate 录制文件（额外挂载点） |

> 注：go2rtc 现在内置在 Frigate 容器中，不再使用独立的 `home-go2rtc` 容器。摄像头配置由 home-api 通过 `PUT /api/config/set` 推送给 Frigate，不要手动编辑 config.yml 中的 cameras 段（会被覆盖）。

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

## 更新日志

### v1.8.3（2026-07-21）HLS 延迟提示 + Android v1.6.24 同步

- **Web 端 HLS 延迟提示徽标**：
  - 在 `LiveVideo` 直播模式 + HLS 传输路径下（用户手动选择 `hls`，或 `auto` 模式下 WebRTC 回退到 HLS 时），视频容器左上角（`absolute left-2 top-2 z-20`）显示一个"网络质量差，延迟较大"徽标。
  - 暖色液态玻璃风格：`--accent-warm` 作为图标/文字颜色、`--glass-bg` 作为半透明背景、`backdrop-blur-md` 实现毛玻璃效果。
  - 图标使用 `lucide-react` 的 `AlertTriangle`（沿用已有导入）。
  - 仅在直播模式 + HLS 路径触发，回放与其他模式不显示。
- **Android v1.6.24 — Tunnel 路径尝试 WebRTC**：
  - `CameraDetailActivity.startPlayback()` 不再在调用 `startWebRtcStream()` 前检查 `isDirectPath()`，WebRTC 现在会在所有路径（LAN / IPv6 直连 / Cloudflare Tunnel）下被尝试，只要摄像头在线且 WebRTC client 可用。
  - Tunnel 路径上 WebRTC 通常会失败（Cloudflare Tunnel 无法中继 UDP），但已有的自适应超时（5s ICE 收集、6s 连接）+ TCP candidate 启用让 STUN / P2P / IPv6 直连场景仍有机会成功。
  - 失败后走原有回退阶梯（MP4 → HLS），最终可用性不受影响。
- **Android v1.6.24 — HLS 延迟提示**：
  - 新增 `bg_hls_notice.xml` drawable + `tvHlsNotice` TextView（`activity_camera_detail.xml`），HLS 激活时显示"网络质量差，延迟较大"。
  - 暖色液态玻璃风格与 `CameraCard.kt` 调色板一致（`#F2FFFFFF` 背景、`#66FFD4B8` 桃色边框）。
  - `versionCode` 66 → 67，`versionName` "1.6.23" → "1.6.24"，构建验证通过。
- **跨端 UX 一致性**：本次变更统一了 Web 端与 Android 端的 HLS 延迟提示样式与文案，用户在任一端遇到 HLS 回退时都能得到一致的视觉提示。

### v1.8.2（2026-07-21）活动事件拉取修复 + kebab 按钮文字遮挡修复

- **活动事件返回 0 的根因修复**：
  - **axios 超时**：前端全局 axios 超时为 15 秒，但后端的 motion-ranges 端点需要将 24 小时窗口分成 24 个每小时 Frigate API 请求（每个 1-2 秒），总计 24-48 秒。15 秒超时会在后端完成前静默中断请求，导致前端缓存 null 并显示「0 事件」。为 `getMotionRanges` 单独设置 90 秒超时。
  - **无限重试循环**：原 useEffect 在请求失败时缓存 `null`，但跳过条件为 `motionCache[key] !== undefined && motionCache[key] !== null`，`null` 不被跳过，导致每次失败后立即重试——无限循环冲击后端。改为 `motionCache[key] !== undefined`（任何值都跳过，包括 null），失败后停止自动重试，用户可点击「刷新」按钮清除缓存强制重试。
  - **后端调试日志**：在 MotionRanges handler 和 ListMotionRanges 中添加临时调试日志，记录 cam_id、stream_name、slug、after/before、每块的 segment 数和 with_motion 数、最终返回的 ranges 数。便于定位 Frigate 是否返回数据、slug 是否正确、时间窗口是否匹配。
- **kebab 按钮文字被边框遮挡修复**：
  - **kebab 按钮**：从 `h-7 w-7 p-0 ring-1 variant=secondary` 改为 `size=icon variant=ghost h-8 w-8`，移除 ring 边框，使用更大的 32×32 尺寸，图标从 16px 提升到 18px。
  - **mode 标签（直播/回放）**：容器从 `h-6 text-[10px]` 提升到 `h-7 text-[11px]`，按钮从 `h-5 px-1.5 tracking-wider` 改为 `inline-flex h-6 items-center justify-center px-2`（移除 tracking-wider 避免文字溢出，添加 flex 居中确保文字垂直居中）。
  - **传输方式选择器**：容器从 `h-7` 提升到 `h-8`，按钮从 `h-6 px-1.5 tracking-wider` 改为 `inline-flex h-7 items-center justify-center px-2`。
  - **停止/录像按钮**：从 `h-6/h-7` 提升到 `h-7/h-8`，移除 `tracking-wider`。
  - **根因**：`tracking-wider` 在小按钮上导致文字宽度超出按钮可视区域；缺少 `inline-flex items-center justify-center` 导致文字未垂直居中，紧贴按钮上边框。

### v1.8.1（2026-07-21）UI 修复：可见性、z-index、汉化

- **事件带可见性修复**：原事件带使用柔和的 `--accent-warm` / `--accent-danger` CSS 变量，在奶油色背景上几乎不可见。改为饱和的 Tailwind 调色板（`bg-red-500` / `bg-amber-500`）+ 深色底带（`bg-[rgb(var(--slate-900)/0.85)]`）+ 发光阴影 + AI 事件 `animate-pulse`。高度从 14px 提升到 24px，最小宽度从 0.8% 提升到 1.2%，新增小时网格线参考。
- **LiveVideo kebab 按钮可见性**：原 `variant="outline"`（`glass-subtle`，0.35 不透明度）在浅色模式下几乎只剩一个虚框。改为 `variant="secondary"`（`glass`，0.6 不透明度），明暗两种模式下均清晰可见。
- **摄像头卡片浅色模式灰色修复**：原 Card 基类 `glass`（0.6 不透明度）在奶油色页面背景上呈现半透明灰洗外观。新增 `bg-[rgb(var(--glass-bg)/0.92)]` 提升到 0.92 不透明度，恢复卡片应有的实体感。
- **ThemeMenu 下拉框 z-index 层级修复**：原下拉框 `z-50` 但 Header 无显式 z-index，导致卡片内容（`glass-glow` 阴影 + transforms）会盖住下拉框且无法点击。建立明确层级：Header `z-40` < ThemeMenu 容器 `z-50` < 下拉框 `z-[100]`，并新增 `ring-1 ring-[rgb(var(--border)/0.4)]` 边框 + `shadow-xl`。
- **RecordingTimeline 加载逻辑优化**：学习 Android 端 `RecordingsDialog.kt` 的加载模式——motion ranges 静默加载（不显示 spinner，失败不阻塞 UI），录像列表加载显示独立的"正在读取录像列表…"状态，活动计数仅在 > 0 时显示。原本"一直转圈"的问题消除。
- **全站汉化**：所有用户可见文本中文化，包括：
  - Layout：导航、品牌、管理员徽章、角色、退出登录、主题菜单（亮色/暗色/跟随系统）
  - Login：登录卡片、表单标签、按钮、提示、错误消息
  - Cameras：标题、刷新/注册按钮、空状态、删除确认、状态徽章（在线/离线/未知）、编码选择器
  - Dashboard：StatCard 标签、网络质量、检测报警、系统快照、天气卡
  - LiveVideo：直播/回放/停止、传输方式、录像计划、PTZ 方向（上转/下转/左转/右转/停止转动）、拉近/拉远、仅观看、预览不可用、加载/错误/重试
  - RecordingTimeline：今天/昨天/前天/周X、24 小时时间轴、事件 tooltip（强度/段/个目标/时长）、空状态、速度菜单
  - Users / Devices / DeviceCreate / Network / MqttDebug / Profile：全部表单、按钮、提示、错误消息
  - 移除中文标签上的 `uppercase tracking-wider` 类（中文无大小写之分），保留 `tracking-wider`

### v1.8.0（2026-07-21）UI 精修：颜色对比、播放器合并、缓存

- **全局颜色对比度修复（明暗两种模式）**：9 个页面/组件文件中的硬编码 Tailwind 颜色（`text-slate-100/200/300/400/500`、`text-emerald-400`、`text-rose-400`、`text-amber-400`、`text-sky-300`、`bg-emerald-400`、`fill-amber-400` 等）全部替换为基于 CSS 变量的主题感知类（`text-fg`、`text-fg-muted`、`text-fg-subtle`、`text-[rgb(var(--accent-success))]`、`bg-[rgb(var(--accent-success)/0.2)]` 等）。涉及 Dashboard、Network、Users、Profile、MqttDebug、Devices、DeviceCreate、LiveVideo、RecordingTimeline。明色模式下原本"白色字在浅色背景上看不清"的问题彻底消除。
- **LiveVideo 头部精简（kebab 菜单）**：原头部在 live 模式下塞了 7+ 控件（transport 分段控件、transport 徽章、mode 标签、Stop、Rec、状态、厂商），窄屏溢出。重构后可见头部精简为：`[标题 + x264]` `[状态徽章]` `[mode 标签]` `[Stop]` `[⋮]`。Transport 选择器、录制开关、厂商信息和 last seen 移入 `⋮` 下拉菜单。
- **录像/直播播放器合并**：`RecordingTimeline` 原本在主视频区下方独立渲染一个 `aspect-video` 容器（主视频区显示"切换至下方时间轴开始播放"占位符）。现在通过 React `createPortal` 将 `<video>` + 自定义控件渲染到 `LiveVideo` 的主视频区，直播和回放共享同一物理视频面。
- **RecordingTimeline 简化（移除鱼眼，新增事件带）**：删除按 `motion_score` 取 Top 50 的鱼眼芯片滚动条，改为在 24h 时间轴上方新增显著事件带——每个 `MotionRange` 渲染为高彩色条（**红色 = 人员活动/AI**，**琥珀色 = 仅画面变动**），并附图例（带计数）。事件在一眼之间即可识别。
- **`useCachedFetch` 通用缓存 Hook**：新增 `web/src/hooks/useCachedFetch.ts`，提供 sessionStorage 缓存的 fetcher + 后台静默刷新（可选轮询）。首次加载显示 loading，之后从缓存瞬时渲染，后台静默刷新数据。Dashboard 的三个轮询组件已应用：
  - **WeatherCard**：`home.dashboard.weather`，10 分钟刷新
  - **System + Network status**：`home.dashboard.status`，5 秒刷新
  - **Alerts**：`home.dashboard.alerts`，30 秒刷新
  
  切换页面再切回 Dashboard 时，立即显示上一次的数据，而不是空白 + 转圈。

### v1.7.0（2026-07-20）Dashboard 对齐 Android 端

详细差异见 [`APP_VS_DASHBOARD_FEATURES.md`](APP_VS_DASHBOARD_FEATURES.md)。

- **天气卡片**：Dashboard 顶部新增天气卡，调用 `GET /api/v1/weather`（代理 wttr.in，5 分钟缓存），显示当前温度、体感、湿度、风速、WMO 天气代码图标。
- **LAN / Remote 路径标识**：Network Quality 卡片新增路径标识（绿点 LAN / 琥珀点 Remote），客户端通过 `window.location.hostname` 判定。
- **System 主题**：`useTheme` 新增 `"system"` 选项，跟随 OS `prefers-color-scheme`。Header 主题切换器改为三状态下拉菜单（Light / Dark / System），支持外部点击 + Escape 关闭，`applyThemeEarly()` 在 React 挂载前应用主题避免闪烁。
- **24 小时录像回放**：新增 `RecordingTimeline` 组件，替换 LiveVideo 原本的"最近录制"列表：
  - 7 天日期选择器（今天 / 昨天 / 前天 / 周X / MM-DD），匹配 Frigate 默认 7 天保留策略
  - 24 小时时间轴（1440 个分钟桶），录制区间高亮，活动区间覆盖红色（AI）或琥珀色（仅运动）
  - 点击时间轴 → 播放对应的 60 秒桶并定位到偏移
  - 活动事件鱼眼芯片（按 `motion_score` 取 Top 50），点击跳转
  - 自定义视频控件：播放/暂停、±10s 跳过、当前时间/总时长、速度下拉菜单 `[0.5, 1, 1.5, 2, 3, 5]`
  - 双击 ±10s 手势（视频左右两侧）
  - 长按 5x 倍速手势（按下时切换到 5x，松开恢复）
  - 自动续播：当前录像播放结束自动加载下一桶
  - Alert 跳转：`?time=UNIX&mode=recording` URL 参数自动选择匹配日期并播放对应桶
- **MP4 兜底中间层**：`RecordingTimeline` 使用 JWT 鉴权的 `fetch` 下载 60 秒 MP4 Blob 并通过 `URL.createObjectURL` 播放，不依赖 MSE / HEVC，在任何支持 MP4 的浏览器上都能工作。

---

## License

Private / 家庭项目，未指定开源协议。
