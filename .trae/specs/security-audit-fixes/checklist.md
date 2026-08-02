# 安全审计检查清单

- [x] HTTP 服务器使用配置了超时参数的 `http.Server`，非默认 `r.Run(addr)`
- [x] JWT Cookie `home_token` 包含 `HttpOnly` 属性
- [x] 所有 API 响应包含 `Content-Security-Policy` 头
- [x] `weather_handler.go` 中的 `io.ReadAll` 使用 `io.LimitReader` 限制大小
- [x] `frigate.go` 中的 `io.ReadAll` 使用 `io.LimitReader` 限制大小
- [x] nginx.conf 包含安全响应头配置（CSP、X-Content-Type-Options、X-Frame-Options、Referrer-Policy）
- [x] `jwt.go` 中 `JWTSecret` 默认值已移除，改为空字符串