# 安全审计第三轮 Spec

## Why

前两轮审计修复了 HTTP 超时、CSP、WebSocket 认证、请求体限制等问题后，第三轮深度扫描覆盖了 Docker/容器安全、密钥泄露、MQTT 安全、授权绕过、速率限制、信息泄露等领域，发现了多个 Critical 和 High 级别漏洞，包括已提交到仓库的 Cloudflare Tunnel 密钥和摄像头明文密码、授权绕过、容器以 root 运行、go2rtc 无认证暴露等。

## What Changes

### Critical — 密钥泄露修复

1. **[Critical] 移除已提交到仓库的 Cloudflare Tunnel 密钥** — `deploy/cloudflared/credentials.json` 包含 TunnelSecret，已提交到版本控制。需从当前树中移除、添加到 `.gitignore`、并文档化轮换需求。
2. **[Critical] 移除 Frigate 配置中的明文摄像头密码** — `deploy/frigate/config.yml` 包含 RTSP 明文密码 `Haikangcam`。需从配置中移除或占位符化、添加到 `.gitignore`。
3. **[Critical] 修复脚本中的明文密码日志** — `scripts/fix_camera_password.go` 将新旧密码明文记录到日志。需使用 `redactPass()` 脱敏。

### High — 授权与暴露修复

4. **[High] MQTT publish 端点添加 admin 权限校验** — `POST /mqtt/publish` 仅有 JWTAuth，缺少 RequireAdmin，任何认证用户可向控制平面注入消息。
5. **[High] 审计日志删除端点添加 admin 权限校验** — `DELETE /system/logs/:id` 仅有 JWTAuth，任何认证用户可删除审计记录。
6. **[High] go2rtc 端口绑定到 127.0.0.1** — `compose.yaml` 中 go2rtc 1984 端口绑定到 `0.0.0.0`，LAN 上任何设备可无认证访问摄像头流。
7. **[High] Docker 容器以非 root 用户运行** — API 和 Web Dockerfile 缺少 `USER` 指令，compose.yaml 缺少 `security_opt` 和 `cap_drop`。

### Medium — 安全加固

8. **[Medium] 添加服务端 logout 端点清除 HttpOnly Cookie** — 前端 `document.cookie` 无法删除 HttpOnly cookie，logout 后 cookie 仍有效 365 天。需添加 `POST /api/v1/auth/logout`。
9. **[Medium] 修复 SetTrustedProxies 使速率限制器生效** — `SetTrustedProxies(nil)` 导致 `c.ClientIP()` 返回 nginx IP，速率限制器对所有用户共享同一桶。
10. **[Medium] 摄像头 API 对非管理员隐藏内部 host/port** — `cameraView` 向有共享读权限的非管理员用户暴露摄像头 LAN IP 和端口。
11. **[Medium] 修复 API 响应中的内部信息泄露** — 46+ 处 `err.Error()` 直接返回给客户端，泄露内部主机名（`home-frigate`、`home-go2rtc`）和文件路径。
12. **[Medium] WebSocket CheckOrigin 默认改为严格模式** — `allowed_origins` 为空时 `CheckOrigin` 返回 `true`，存在 CSWSH 风险。
13. **[Medium] 添加 .dockerignore 文件** — 两个构建上下文缺少 `.dockerignore`，可能将本地密钥文件打包进镜像层。
14. **[Medium] MQTT 密码文件添加到 .gitignore** — `deploy/mosquitto/passwd` 已提交到仓库，需移除并 gitignore。

### Low — 防御加固

15. **[Low] 修复 callerIsAdmin 守卫逻辑** — `UpdateCodec`/`UpdateAudio` 中 `callerIsAdmin` 只检查 `ok` 不检查 `isAdmin`，存在潜在授权绕过。
16. **[Low] 添加 ReadHeaderTimeout** — `http.Server` 缺少 `ReadHeaderTimeout`，slowloris 防御不够精准。
17. **[Low] 移除调试日志** — `camera_handler.go` 中 `[motion-ranges]` 调试日志应移除。
18. **[Low] 修复过时的 cookie 注释** — `auth_handler.go` 中 "NOT HttpOnly" 注释与实际代码矛盾。

## Impact

- Affected specs: 安全规范、授权模型、Docker 部署、WebSocket 安全
- Affected code:
  - `deploy/cloudflared/credentials.json` — 移除
  - `deploy/frigate/config.yml` — 移除明文密码
  - `deploy/mosquitto/passwd` — 移除
  - `.gitignore` — 添加密钥文件路径
  - `services/api/scripts/fix_camera_password.go` — 脱敏密码日志
  - `services/api/cmd/main.go` — RequireAdmin 中间件、SetTrustedProxies、ReadHeaderTimeout
  - `services/api/internal/handler/auth_handler.go` — logout 端点、修复注释
  - `services/api/internal/handler/camera_handler.go` — 信息脱敏、移除调试日志
  - `services/api/internal/handler/ws_handler.go` — CheckOrigin 默认严格
  - `services/api/Dockerfile` — 添加 USER
  - `web/Dockerfile` — 添加 USER
  - `compose.yaml` — go2rtc 端口绑定、security_opt、cap_drop
  - `web/src/api/client.ts` — logout 调用服务端
  - `web/src/context/AuthContext.tsx` — logout 流程

## ADDED Requirements

### Requirement: 服务端 Logout 端点
系统 SHALL 提供 `POST /api/v1/auth/logout` 端点，通过设置 `Max-Age=-1` 清除 HttpOnly cookie。

#### Scenario: 用户登出
- **WHEN** 用户点击登出
- **THEN** 前端 POST 到 `/api/v1/auth/logout`
- **AND** 服务端清除 `home_token` cookie
- **AND** 前端清除 localStorage 中的 token

### Requirement: 内部错误信息不泄露
系统 SHALL 在 API 响应中返回通用错误消息，将详细错误记录到服务端日志。

#### Scenario: 上游服务不可用
- **WHEN** go2rtc/Frigate 返回错误
- **THEN** 客户端收到 `"upstream unavailable"` 等通用消息
- **AND** 详细错误记录在服务端日志中

### Requirement: 摄像头内部信息对非管理员脱敏
系统 SHALL 对非管理员、非所有者的用户隐藏摄像头 `host`、`onvif_port`、`rtsp_port` 字段。

#### Scenario: 共享用户查看摄像头
- **WHEN** 非管理员共享用户请求摄像头详情
- **THEN** `host` 返回 `"***"`
- **AND** `onvif_port` 和 `rtsp_port` 返回 0

### Requirement: WebSocket Origin 默认严格
系统 SHALL 在 `allowed_origins` 未配置时拒绝跨域 WebSocket 连接。

#### Scenario: 未配置 allowed_origins
- **WHEN** `allowed_origins` 为空
- **THEN** WebSocket 仅接受同源连接
- **AND** 跨域连接被拒绝

### Requirement: Docker 容器非 root 运行
系统 SHALL 在所有 Dockerfile 中使用非 root 用户运行服务。

#### Scenario: 容器启动
- **WHEN** 容器启动
- **THEN** 进程以非 root 用户（如 `app`/`nginx`）运行
- **AND** compose.yaml 包含 `security_opt: ["no-new-privileges:true"]` 和 `cap_drop: ["ALL"]`

## MODIFIED Requirements

### Requirement: MQTT publish 权限
`POST /api/v1/mqtt/publish` SHALL 要求 admin 权限，非 admin 用户返回 403。

### Requirement: 审计日志删除权限
`DELETE /api/v1/system/logs/:id` SHALL 要求 admin 权限，非 admin 用户返回 403。

### Requirement: 速率限制器 IP 识别
速率限制器 SHALL 通过配置 `SetTrustedProxies` 信任 Docker 网桥网段，正确解析 `X-Forwarded-For` 中的真实客户端 IP。

## REMOVED Requirements

无
