# Tasks

- [x] Task 1: [High] WebSocket 认证改为 Sec-WebSocket-Protocol 头传递
  - [x] 修改 `services/api/internal/handler/ws_handler.go`，从 `Sec-WebSocket-Protocol` 头提取 JWT（格式 `bearer.<token>`），移除从 URL query 参数读取 token 的逻辑
  - [x] 修改 `web/src/hooks/useWebSocket.ts`，将 `new WebSocket(url)` 改为 `new WebSocket(url, ["bearer." + token])`
  - [x] 验证 WebSocket 连接正常建立且认证通过

- [x] Task 2: [High] 添加全局请求体大小限制中间件
  - [x] 在 `services/api/cmd/main.go` 中添加 Gin 中间件，使用 `http.MaxBytesReader` 包装 `c.Request.Body`，限制为 1MB
  - [x] 对特殊端点（如有文件上传）设置更大限制或排除
  - [x] 验证超大请求体被拒绝并返回 413

- [x] Task 3: [Medium] 修复 weather handler SSRF — 使用 net.ParseIP 替换字符串前缀匹配
  - [x] 修改 `services/api/internal/handler/weather_handler.go` 中的 `isPublicIP` 函数，使用 `net.ParseIP` 验证 IP 格式，拒绝非 IP 字符串
  - [x] 使用 `url.URL` 构建 wttr.in 请求 URL，避免字符串拼接注入
  - [x] 验证伪造的 X-Forwarded-For 值被正确拒绝

- [x] Task 4: [Medium] 修复 go2rtc Frame 响应体无大小限制
  - [x] 修改 `services/api/internal/handler/camera_handler.go:777`，为 `io.ReadAll(body)` 添加 `io.LimitReader` 包装（限制 10MB）
  - [x] 验证 go2rtc 帧获取功能正常

- [x] Task 5: [Medium] 修复 WeatherHandler 数据竞争
  - [x] 修改 `services/api/internal/handler/weather_handler.go`，为 `WeatherHandler` 添加 `sync.RWMutex`
  - [x] 在缓存读取和写入操作中加锁/解锁
  - [x] 验证并发请求下无数据竞争

- [x] Task 6: [Low] Cookie Secure 标志可配置化
  - [x] 在 `services/api/internal/config/config.go` 中添加 `SecureCookie bool` 配置项（默认 false）
  - [x] 修改 `services/api/internal/handler/auth_handler.go` 中 `SetCookie` 调用，使用配置值控制 `Secure` 参数
  - [x] 修改 `web/src/api/client.ts` 中 cookie 删除逻辑，同步添加 `Secure` 标志（当适用时）
  - [x] 验证配置为 true 时 cookie 包含 Secure 属性

- [x] Task 7: [Low] 自托管 Google Fonts 并修复 CSP 不一致
  - [x] 下载 Inter 和 JetBrains Mono 字体文件到 `web/src/assets/fonts/`
  - [x] 在 CSS 中通过 `@font-face` 引用本地字体文件
  - [x] 移除 `web/index.html` 中的 Google Fonts `<link>` 标签
  - [x] 验证字体从同源加载，浏览器 console 无 CSP 拦截错误

- [x] Task 8: [Low] 添加 Permissions-Policy 头到 nginx
  - [x] 修改 `web/nginx.conf`，添加 `add_header Permissions-Policy "geolocation=(), microphone=(), camera=(), payment=(), usb=()" always;`
  - [x] 验证响应包含 Permissions-Policy 头

# Task Dependencies

- [Task 1] 前后端需同步修改，无外部依赖
- [Task 2] 无依赖
- [Task 3] 无依赖
- [Task 4] 无依赖
- [Task 5] 无依赖
- [Task 6] 无依赖
- [Task 7] 无依赖
- [Task 8] 无依赖
- [Task 1] 和 [Task 7] 可并行
- [Task 2] [Task 3] [Task 4] [Task 5] [Task 6] [Task 8] 均可并行
