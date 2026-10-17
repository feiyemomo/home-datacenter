package middleware

import (
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/security"
	"home-datacenter-api/internal/utils"
)

// ExtractRealClientIP resolves the client IP that rate limiting and the
// intrusion-defense blocklist key on.
//
// It deliberately delegates to gin's Context.ClientIP, which walks
// X-Forwarded-For right-to-left through the chain and stops at the first
// hop that is not one of the trusted proxies configured via
// SetTrustedProxies (see cmd/main.go). Headers are only consulted at all
// when the immediate peer is a trusted proxy.
//
// Earlier revisions read CF-Connecting-IP, X-Real-IP and the left-most
// X-Forwarded-For entry unconditionally, before any trust check. Because
// the origin is also reachable directly over the H3C relay tunnel, an
// attacker could send "CF-Connecting-IP: <anything>" (or a forged
// X-Forwarded-For) and pick a brand-new rate-limit bucket for every
// attempt, defeating the /auth/bind throttling entirely — detail.txt
// finding #59. nginx now normalises those headers to a value derived
// from its own TCP peer, and this function relies on gin's trust logic
// rather than re-implementing it.
func ExtractRealClientIP(c *gin.Context) string {
	if ip := c.ClientIP(); ip != "" {
		return ip
	}
	// ClientIP only returns "" when RemoteAddr cannot be parsed.
	if host, _, err := net.SplitHostPort(c.Request.RemoteAddr); err == nil {
		return strings.TrimSpace(host)
	}
	return strings.TrimSpace(c.Request.RemoteAddr)
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
