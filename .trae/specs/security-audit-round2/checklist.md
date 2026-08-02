# 安全审计第二轮检查清单

- [x] WebSocket 连接 URL 中不包含 `token` 查询参数
- [x] WebSocket 通过 `Sec-WebSocket-Protocol` 头传递 JWT
- [x] 服务端从 `Sec-WebSocket-Protocol` 头提取并验证 JWT
- [x] 所有 `ShouldBindJSON` 端点受到 `MaxBytesReader` 大小限制保护
- [x] 超大请求体返回 413 状态码
- [x] `weather_handler.go` 中 `isPublicIP` 使用 `net.ParseIP` 验证 IP 格式
- [x] wttr.in 请求 URL 通过 `url.URL` 构建，非字符串拼接
- [x] `camera_handler.go` 中 go2rtc Frame 读取使用 `io.LimitReader` 限制大小
- [x] `WeatherHandler` 的 `cache` 和 `cachedAt` 字段受 `sync.RWMutex` 保护
- [x] `config.go` 包含 `SecureCookie` 配置项
- [x] `auth_handler.go` 中 `SetCookie` 的 `Secure` 参数由配置控制
- [x] `web/index.html` 中无外部 Google Fonts `<link>` 标签
- [x] 字体文件从同源加载
- [x] `nginx.conf` 包含 `Permissions-Policy` 响应头
- [x] nginx CSP `font-src` 和 `style-src` 与实际字体加载方式一致
