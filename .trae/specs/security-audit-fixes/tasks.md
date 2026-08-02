# Tasks

- [x] Task 1: [Critical] 配置 HTTP 服务器超时 — 用 `http.Server` 替换 `r.Run(addr)`，设置 `ReadTimeout`、`WriteTimeout`、`IdleTimeout`、`MaxHeaderBytes`
  - [x] 修改 `services/api/cmd/main.go`，创建有超时配置的 `http.Server`，使用 `s.ListenAndServe()` 替代 `r.Run(addr)`
  - [x] 验证启动后服务器正确应用超时配置

- [x] Task 2: [High] 为 JWT Cookie 启用 HttpOnly — 修改 `auth_handler.go` 中 `SetCookie` 调用
  - [x] 修改 `services/api/internal/handler/auth_handler.go` 第 104 行，将 `HttpOnly` 参数从 `false` 改为 `true`
  - [x] 验证绑定后 cookie 包含 `HttpOnly` 属性

- [x] Task 3: [High] 为 API 响应添加 CSP 头 — 在 `response.go` 中添加 `Content-Security-Policy` 头
  - [x] 修改 `services/api/internal/utils/response.go` 中的 `applySecurityHeaders` 函数，添加 CSP 头
  - [x] 验证所有 API 响应包含 CSP 头

- [x] Task 4: [High] 修复无大小限制的响应体读取 — 为 `weather_handler.go` 和 `frigate.go` 添加 `io.LimitReader`
  - [x] 修改 `services/api/internal/handler/weather_handler.go`，为 `io.ReadAll(resp.Body)` 添加 `io.LimitReader` 包装
  - [x] 修改 `services/api/internal/camera/frigate.go` 第 362 行，为 `io.ReadAll(resp.Body)` 添加 `io.LimitReader` 包装

- [x] Task 5: [Medium] 为 nginx 添加安全响应头 — 在 `nginx.conf` 中添加安全头配置
  - [x] 修改 `web/nginx.conf`，为 SPA 添加 `Content-Security-Policy`、`X-Content-Type-Options`、`X-Frame-Options`、`Referrer-Policy` 等安全头

- [x] Task 6: [Medium] 移除 JWT 默认值 — 移除 `jwt.go` 中的不安全默认值
  - [x] 修改 `services/api/internal/utils/jwt.go`，将 `JWTSecret` 的默认值改为空字符串

- [x] Task 7: [Low] 验证 `config.go` 的 `validateJWTSecret` 能正确拒绝空字符串
  - [x] 验证 `insecureSecrets` 映射中包含 `""` 空字符串键
  - [x] 确认 `validateJWTSecret("")` 返回错误

# Task Dependencies

- [Task 1] 无依赖
- [Task 2] 无依赖
- [Task 3] 无依赖
- [Task 4] 无依赖
- [Task 5] 无依赖
- [Task 6] 无依赖
- [Task 7] 依赖 [Task 6]