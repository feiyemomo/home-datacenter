# 安全审计第三轮检查清单

## Critical — 密钥泄露
- [x] `deploy/cloudflared/credentials.json` 已从 git 追踪中移除且在 `.gitignore` 中
- [x] `deploy/mosquitto/passwd` 已从 git 追踪中移除且在 `.gitignore` 中
- [x] `deploy/frigate/config.yml` 已从 git 追踪中移除或明文密码已替换为占位符
- [x] `services/api/.dockerignore` 存在且排除敏感文件
- [x] `web/.dockerignore` 存在且排除 `node_modules/`、`dist/`
- [x] `fix_camera_password.go` 不再明文记录密码

## High — 授权与暴露
- [x] `POST /api/v1/mqtt/publish` 要求 admin 权限
- [x] `DELETE /api/v1/system/logs/:id` 要求 admin 权限
- [x] go2rtc 端口 1984 绑定到 `127.0.0.1` 而非 `0.0.0.0`
- [x] API Dockerfile 包含非 root `USER` 指令
- [x] Web Dockerfile 包含非 root `USER` 指令
- [x] compose.yaml 所有服务包含 `security_opt: ["no-new-privileges:true"]`
- [x] compose.yaml 所有服务包含 `cap_drop: ["ALL"]`

## Medium — 安全加固
- [x] `POST /api/v1/auth/logout` 端点存在且清除 cookie
- [x] 前端 logout 先调用服务端端点再清除 localStorage
- [x] `SetTrustedProxies` 配置信任 Docker 网桥网段
- [x] 非管理员查看摄像头时 `host` 返回 `"***"`
- [x] 非管理员查看摄像头时 `onvif_port`/`rtsp_port` 返回 0
- [x] API 错误响应不包含内部主机名（`home-frigate`、`home-go2rtc`）
- [x] API 错误响应不包含文件系统路径
- [x] API 错误响应不包含原始绑定错误
- [x] WebSocket `CheckOrigin` 在未配置 `allowed_origins` 时拒绝跨域连接
- [x] `auth_handler.go` 中 "NOT HttpOnly" 注释已修正

## Low — 防御加固
- [x] `UpdateCodec` 和 `UpdateAudio` 的 `callerIsAdmin` 检查包含 `isAdmin` 判断
- [x] `http.Server` 包含 `ReadHeaderTimeout` 配置
- [x] `camera_handler.go` 中 `[motion-ranges]` 调试日志已移除
