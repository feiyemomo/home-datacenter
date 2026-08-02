# Tasks

- [x] Task 1: [Critical] 移除仓库中的已提交密钥 + 添加 .gitignore
  - [x] 将 `deploy/cloudflared/credentials.json` 添加到 `.gitignore` 并从 git 追踪中移除（`git rm --cached`）
  - [x] 将 `deploy/mosquitto/passwd` 添加到 `.gitignore` 并从 git 追踪中移除
  - [x] 将 `deploy/frigate/config.yml` 添加到 `.gitignore` 并从 git 追踪中移除（或将其中的明文密码替换为占位符）
  - [x] 添加 `services/api/.dockerignore` 排除 `configs/config.local.yaml`、`data/`、`.env`、`*.db`
  - [x] 添加 `web/.dockerignore` 排除 `node_modules/`、`dist/`、`.env`

- [x] Task 2: [Critical] 修复脚本中的明文密码日志
  - [x] 修改 `services/api/scripts/fix_camera_password.go` 第 185-186 行，使用 `maskSecret()` 脱敏 `oldPass` 和 `*newPass`
  - [x] 移除第 35 行的默认密码 `"Haikangcam"`，改为空字符串或必填参数

- [x] Task 3: [High] MQTT publish 和审计日志删除添加 admin 权限校验
  - [x] 修改 `services/api/cmd/main.go`，在 `/mqtt` 路由组添加 `middleware.RequireAdmin(database.DB)`
  - [x] 修改 `services/api/cmd/main.go`，将 `DELETE /system/logs/:id` 路由移到 admin 子组或添加 RequireAdmin

- [x] Task 4: [High] go2rtc 端口绑定到 127.0.0.1 + Docker 容器安全加固
  - [x] 修改 `compose.yaml`，将 go2rtc 的 `0.0.0.0:1984:1984` 改为 `127.0.0.1:1984:1984`
  - [x] 修改 `services/api/Dockerfile`，添加非 root `USER` 指令
  - [x] 修改 `web/Dockerfile`，确保 nginx 以非 root 运行（或添加 USER nginx）
  - [x] 修改 `compose.yaml`，为所有服务添加 `security_opt: ["no-new-privileges:true"]` 和 `cap_drop: ["ALL"]`

- [x] Task 5: [Medium] 添加服务端 logout 端点
  - [x] 修改 `services/api/internal/handler/auth_handler.go`，添加 `Logout` handler 清除 cookie（`Max-Age=-1`）
  - [x] 修改 `services/api/cmd/main.go`，注册 `POST /api/v1/auth/logout` 路由
  - [x] 修改 `web/src/api/client.ts`，`clearTokenAndRedirect` 先 POST 到 `/auth/logout` 再清除 localStorage
  - [x] 修复 `auth_handler.go` 第 60-62 行过时的 "NOT HttpOnly" 注释

- [x] Task 6: [Medium] 修复 SetTrustedProxies 使速率限制器生效
  - [x] 修改 `services/api/cmd/main.go`，将 `SetTrustedProxies(nil)` 改为信任 Docker 网桥网段 `["172.16.0.0/12", "192.168.0.0/16", "10.0.0.0/8"]`
  - [x] 确保 nginx 已设置 `X-Forwarded-For` / `X-Real-IP`（验证 nginx.conf 中 proxy_set_header）

- [x] Task 7: [Medium] 摄像头 API 对非管理员隐藏内部 host/port
  - [x] 修改 `services/api/internal/handler/camera_handler.go` 中的 `cameraView` 函数，接受 `isPrivileged bool` 参数
  - [x] 非管理员、非所有者时 `host` 返回 `"***"`，`onvif_port` 和 `rtsp_port` 返回 0
  - [x] 更新所有调用 `cameraView` 的位置传入权限信息

- [x] Task 8: [Medium] 修复 API 响应中的内部信息泄露
  - [x] 修改 `camera_handler.go` 中 46+ 处 `err.Error()` 直接返回的位置，改为通用消息 + 服务端日志
  - [x] 修改 `weather_handler.go` 中 `err.Error()` 和上游响应体直接返回的位置
  - [x] 修改 `automation/handler.go` 中 `err.Error()` 直接返回的位置
  - [x] 修改 `network_handler.go` 中 `err.Error()` 直接返回的位置
  - [x] 修改 `release_handler.go` 中 `err.Error()` 直接返回的位置

- [x] Task 9: [Medium] WebSocket CheckOrigin 默认改为严格模式
  - [x] 修改 `services/api/internal/handler/ws_handler.go`，当 `allowed_origins` 为空时，`CheckOrigin` 仅接受同源请求

- [x] Task 10: [Low] 修复 callerIsAdmin 守卫 + ReadHeaderTimeout + 移除调试日志
  - [x] 修改 `camera_handler.go` 中 `UpdateCodec` 和 `UpdateAudio`，改为 `if _, isAdmin, ok := h.callerIsAdmin(c); !ok || !isAdmin`
  - [x] 修改 `cmd/main.go` 的 `http.Server` 添加 `ReadHeaderTimeout: 10 * time.Second`
  - [x] 移除 `camera_handler.go` 中的 `[motion-ranges]` 调试日志

# Task Dependencies

- [Task 1] 无依赖，可先执行
- [Task 2] 无依赖
- [Task 3] 无依赖
- [Task 4] 无依赖
- [Task 5] 依赖 [Task 3]（auth_handler.go 修改需协调），但可并行
- [Task 6] 无依赖
- [Task 7] 无依赖
- [Task 8] 无依赖
- [Task 9] 无依赖
- [Task 10] 无依赖
- 所有任务均可并行执行，除 [Task 5] 和 [Task 3] 需协调 auth_handler.go 的修改
