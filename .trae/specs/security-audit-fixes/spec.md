# 安全审计与加固 Spec

## Why

对 Home Datacenter 项目进行系统性安全审计，识别并修复 Go 后端和 React 前端中的安全漏洞，遵循 OWASP 安全最佳实践和 Go/React 安全规范，提升整体安全性和健壮性。

## What Changes

### 后端 (Go/Gin)

1. **[Critical] 修复 HTTP 服务器超时配置** — `r.Run(addr)` 使用默认 `http.Server`（零超时），需要显式配置 `ReadTimeout`、`WriteTimeout`、`IdleTimeout`、`MaxHeaderBytes`
2. **[High] 为 JWT Cookie 启用 HttpOnly** — `auth_handler.go` 中 `SetCookie` 的 `HttpOnly` 参数为 `false`，需改为 `true`
3. **[High] 添加 Content-Security-Policy 响应头** — 为所有 API 响应添加 CSP 头
4. **[High] 修复无大小限制的响应体读取** — `weather_handler.go:116` 和 `frigate.go:362` 中 `io.ReadAll(resp.Body)` 未限制大小
5. **[Medium] 为 nginx 添加安全响应头** — 在 `nginx.conf` 中添加 CSP、X-Content-Type-Options 等安全头
6. **[Medium] 修复 go2rtc 开放重定向** — `build-host/go2rtc` 中 `mp4.go` 存在潜在开放重定向
7. **[Low] 添加 CSRF 保护** — 对 cookie 认证的状态变更端点添加 CSRF 检查
8. **[Low] 修复 JWT 默认值** — `jwt.go` 中默认 secret 为 `PLEASE_CHANGE_TO_A_LONG_RANDOM_SECRET`（已由 config 验证缓解，但应移除默认值）

### 前端 (React/TypeScript)

9. **[High] 添加 Content-Security-Policy** — 在 nginx 层为 SPA 添加 CSP 头
10. **[Medium] 添加安全响应头** — 在 nginx 层添加 `X-Content-Type-Options`、`X-Frame-Options`、`Referrer-Policy` 等
11. **[Low] 审计 localStorage 中的 JWT 存储** — 当前设计将 JWT 存储在 `localStorage`，有 XSS 泄露风险。需确认风险并记录

### 部署/配置

12. **[Medium] 添加依赖漏洞扫描** — 在 CI 或文档中添加 `govulncheck` 和 `npm audit` 的使用说明

## Impact

- Affected specs: 安全规范、配置管理、前端安全
- Affected code:
  - `services/api/cmd/main.go`
  - `services/api/internal/handler/auth_handler.go`
  - `services/api/internal/handler/weather_handler.go`
  - `services/api/internal/camera/frigate.go`
  - `services/api/internal/utils/jwt.go`
  - `services/api/internal/utils/response.go`
  - `web/nginx.conf`
  - `build-host/go2rtc/internal/mp4/mp4.go`

## ADDED Requirements

### Requirement: HTTP 服务器超时配置
系统 SHALL 在启动 HTTP 服务器时配置合理的超时时间。

#### Scenario: 生产环境启动
- **WHEN** 服务器启动
- **THEN** 使用配置了 `ReadTimeout=15s`、`WriteTimeout=15s`、`IdleTimeout=60s`、`MaxHeaderBytes=1MB` 的 `http.Server`

### Requirement: JWT Cookie HttpOnly
系统 SHALL 在设置 JWT 认证 cookie 时启用 HttpOnly 标志。

#### Scenario: 用户登录绑定
- **WHEN** 用户成功绑定设备
- **THEN** 设置的 `home_token` cookie 包含 `HttpOnly` 属性

### Requirement: CSP 响应头
系统 SHALL 为所有 HTTP 响应添加 Content-Security-Policy 头。

#### Scenario: API 响应
- **WHEN** 服务器返回任何 API 响应
- **THEN** 响应包含 `Content-Security-Policy: default-src 'self'` 头

### Requirement: 响应体大小限制
系统 SHALL 在读取外部 HTTP 响应体时使用大小限制。

#### Scenario: 天气代理
- **WHEN** 从 wttr.in 读取天气响应
- **THEN** 使用 `io.LimitReader` 限制读取大小（如 64KB）

#### Scenario: Frigate 配置读取
- **WHEN** 从 Frigate API 读取配置
- **THEN** 使用 `io.LimitReader` 限制读取大小

### Requirement: nginx 安全头
系统 SHALL 在 nginx 配置中添加安全响应头。

#### Scenario: 浏览器请求
- **WHEN** 浏览器请求 SPA 页面
- **THEN** 响应包含 CSP、X-Content-Type-Options、X-Frame-Options、Referrer-Policy 等安全头

## MODIFIED Requirements

### Requirement: JWT Secret 默认值
`jwt.go` 中的默认 secret 值 `PLEASE_CHANGE_TO_A_LONG_RANDOM_SECRET` 应被移除或改为空字符串（由 `config.go` 的 `validateJWTSecret` 统一拒绝启动）。

## REMOVED Requirements

无