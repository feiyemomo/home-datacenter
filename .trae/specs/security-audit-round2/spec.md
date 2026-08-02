# 安全审计第二轮 Spec

## Why

在第一轮安全审计修复了 HTTP 超时、CSP 头、HttpOnly cookie、响应体限制等问题后，第二轮系统性扫描发现了剩余的安全漏洞，涉及 JWT 泄露、请求体大小限制缺失、SSRF、数据竞争、第三方资源安全等方面。本 spec 旨在修复这些问题，进一步提升项目安全性和健壮性。

## What Changes

### 后端 (Go/Gin)

1. **[High] 修复 WebSocket URL 中泄露 JWT** — 前端通过 `ws://host/api/v1/ws?token=<JWT>` 传递认证令牌，导致 JWT 被记录在 nginx 访问日志、Cloudflare 日志和浏览器历史中。需改为通过 WebSocket 握手头（`Sec-WebSocket-Protocol`）传递令牌。
2. **[High] 添加请求体大小限制** — 服务器缺少 `http.MaxBytesReader`，17 个 `ShouldBindJSON` 调用无大小限制，攻击者可通过大体积 JSON 体进行 DoS。
3. **[Medium] 修复 weather handler SSRF** — `X-Forwarded-For` 头中的 IP 被直接拼接到 wttr.in URL 中，未经 `net.ParseIP` 验证或 URL 编码，存在路径注入和请求伪造风险。
4. **[Medium] 修复 go2rtc Frame 响应体无大小限制** — `camera_handler.go:777` 中 `io.ReadAll(body)` 读取 go2rtc 帧数据无大小限制，可能导致 OOM。
5. **[Medium] 修复 WeatherHandler 数据竞争** — `cache` 和 `cachedAt` 字段在并发 HTTP 请求中被读写，无 mutex 保护。
6. **[Low] Cookie `Secure` 标志可配置化** — 当前 `Secure: false` 是为 LAN HTTP 访问设计的，但应支持通过配置在纯 HTTPS 部署中启用 `Secure`。

### 前端 (React/TypeScript)

7. **[High] WebSocket 认证改为协议头传递** — `useWebSocket.ts` 中将 `?token=` 参数改为通过 `Sec-WebSocket-Protocol` 子协议头传递。
8. **[Low] 自托管 Google Fonts 或更新 CSP** — `index.html` 加载外部 Google Fonts 但 CSP 不允许 `fonts.googleapis.com`，字体实际被浏览器静默拦截。改为自托管字体或更新 CSP。
9. **[Low] 添加 Permissions-Policy 头** — nginx 缺少 `Permissions-Policy` 响应头。

### 部署/配置

10. **[Low] nginx WebSocket 日志过滤** — 在修复前端 WS 认证方式之前，先过滤 nginx 访问日志中的 `token=` 参数，防止历史泄露。

## Impact

- Affected specs: 安全规范、WebSocket 认证、请求处理、nginx 配置
- Affected code:
  - `services/api/cmd/main.go` — 添加 MaxBytesReader 中间件
  - `services/api/internal/handler/weather_handler.go` — 修复 SSRF + 数据竞争
  - `services/api/internal/handler/camera_handler.go` — 修复 go2rtc Frame 无限制读取
  - `services/api/internal/handler/ws_handler.go` — 修改 WebSocket 认证方式
  - `services/api/internal/handler/auth_handler.go` — Cookie Secure 可配置化
  - `services/api/internal/config/config.go` — 添加 SecureCookie 配置项
  - `web/src/hooks/useWebSocket.ts` — 修改 WS 认证方式
  - `web/src/api/client.ts` — Cookie 删除时同步 Secure 标志
  - `web/index.html` — 移除外部 Google Fonts 或自托管
  - `web/nginx.conf` — 添加 Permissions-Policy、更新 CSP、过滤 WS 日志

## ADDED Requirements

### Requirement: WebSocket 认证通过协议头传递
系统 SHALL 在 WebSocket 连接时通过 `Sec-WebSocket-Protocol` 子协议头传递 JWT，而非 URL 查询参数。

#### Scenario: 客户端建立 WebSocket 连接
- **WHEN** 客户端发起 WebSocket 连接
- **THEN** JWT 通过 `Sec-WebSocket-Protocol: bearer.<token>` 头传递
- **AND** URL 中不包含 token 查询参数

#### Scenario: 服务端验证 WebSocket 认证
- **WHEN** 服务端收到 WebSocket 升级请求
- **THEN** 从 `Sec-WebSocket-Protocol` 头中提取 JWT 并验证
- **AND** 验证通过后完成 WebSocket 握手

### Requirement: 请求体大小限制
系统 SHALL 对所有接受请求体的端点设置最大请求体大小限制。

#### Scenario: 客户端提交超大 JSON
- **WHEN** 客户端提交超过限制大小的 JSON 请求体
- **THEN** 服务器返回 413 Request Entity Too Large
- **AND** 请求体不会被完整读取到内存

### Requirement: Weather Handler IP 验证
系统 SHALL 使用 `net.ParseIP` 验证客户端 IP 地址，而非字符串前缀匹配。

#### Scenario: 伪造 X-Forwarded-For 头
- **WHEN** 请求包含恶意的 `X-Forwarded-For` 头
- **THEN** 服务器使用 `net.ParseIP` 验证 IP 格式
- **AND** 非 IP 格式的值被拒绝，回退到默认位置

### Requirement: Weather Handler 缓存线程安全
系统 SHALL 使用互斥锁保护 WeatherHandler 的缓存字段。

#### Scenario: 并发天气请求
- **WHEN** 多个客户端同时请求天气数据
- **THEN** 缓存读写操作通过 `sync.RWMutex` 保护
- **AND** 不存在数据竞争

### Requirement: go2rtc Frame 响应体大小限制
系统 SHALL 在读取 go2rtc 帧数据时使用 `io.LimitReader` 限制大小。

#### Scenario: go2rtc 返回超大帧数据
- **WHEN** go2rtc 返回超过限制大小的帧数据
- **THEN** 服务器只读取限制大小内的数据
- **AND** 不会因 OOM 导致服务中断

### Requirement: Permissions-Policy 头
系统 SHALL 在 nginx 响应中添加 `Permissions-Policy` 头。

#### Scenario: 浏览器请求 SPA 页面
- **WHEN** 浏览器请求页面
- **THEN** 响应包含 `Permissions-Policy` 头，禁用不需要的浏览器 API

### Requirement: Google Fonts 自托管
系统 SHALL 自托管字体资源，不依赖外部 CDN。

#### Scenario: 页面加载字体
- **WHEN** 浏览器请求页面
- **THEN** 字体资源从同源服务器加载
- **AND** 不向 `fonts.googleapis.com` 发送请求

## MODIFIED Requirements

### Requirement: Cookie Secure 标志
`auth_handler.go` 中的 Cookie `Secure` 标志 SHALL 支持通过配置项控制，在纯 HTTPS 部署中设为 `true`，在混合 HTTP/HTTPS 部署中设为 `false`。

## REMOVED Requirements

无
