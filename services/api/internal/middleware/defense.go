package middleware

import (
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/security"
	"home-datacenter-api/internal/utils"
)

// ExtractRealClientIP resolves client IP respecting proxies and edge headers safely.
func ExtractRealClientIP(c *gin.Context) string {
	// 1. Direct Cloudflare header if present (Tunnel / Proxy)
	cfIP := strings.TrimSpace(c.GetHeader("CF-Connecting-IP"))
	if cfIP != "" && net.ParseIP(cfIP) != nil {
		return cfIP
	}

	// 2. Verified X-Real-IP set by nginx reverse proxy
	xri := strings.TrimSpace(c.GetHeader("X-Real-IP"))
	if xri != "" && net.ParseIP(xri) != nil {
		return xri
	}

	// 3. Fall back to Gin's trusted client IP (respects SetTrustedProxies)
	clientIP := c.ClientIP()
	if clientIP != "" {
		return clientIP
	}

	// 4. Fall back to X-Forwarded-For if populated
	xff := c.GetHeader("X-Forwarded-For")
	if xff != "" {
		parts := strings.Split(xff, ",")
		for _, part := range parts {
			ip := strings.TrimSpace(part)
			parsed := net.ParseIP(ip)
			if parsed != nil {
				return ip
			}
		}
	}

	return ""
}

// SecurityDefenseMiddleware enforces active IP blocklists across endpoints.
func SecurityDefenseMiddleware(dm *security.DefenseManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := ExtractRealClientIP(c)
		c.Set("real_client_ip", ip)

		if dm != nil {
			if banned, entry := dm.IsBanned(ip); banned {
				utils.FailWithCode(c, http.StatusForbidden, 40301,
					"您的 IP ("+ip+") 触发高频异常已被安全引擎拦截，原因: "+entry.Reason)
				c.Abort()
				return
			}
		}

		c.Next()
	}
}
