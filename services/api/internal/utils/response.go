package utils

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Common security headers applied to every API response.
//
// These are intentionally modest because Home Datacenter sits behind
// Cloudflare Tunnel + nginx, but defence-in-depth still pays off:
//
//   - X-Content-Type-Options: nosniff   — stop MIME sniffing on JSON
//   - X-Frame-Options: DENY              — prevent clickjacking via iframe
//   - Referrer-Policy: no-referrer      — avoid leaking the dashboard URL
//   - Cache-Control: no-store           — never cache authenticated JSON
//
// The web SPA is served by nginx (separate service) with its own caching
// policy for hashed static assets, so no-store here only affects API JSON.
const securityHeaders = "no-store"

func applySecurityHeaders(c *gin.Context) {
	h := c.Writer.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", securityHeaders)
}

// Response is the unified API envelope used by every /api/v1/* handler.
//
//	{
//	  "code": 0,            // 0 = success, non-zero = error (HTTP status)
//	  "message": "success", // human-readable status
//	  "data": {}            // payload on success, null on error
//	}
//
// /health is intentionally excluded — it stays as {"status":"ok"} so
// Docker / Cloudflare Tunnel health probes keep working.
type Response struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data"`
}

// Success sends HTTP 200 with { code: 0, message: "success", data: <data> }.
// Pass nil for endpoints that return no payload (e.g. DELETE).
func Success(c *gin.Context, data interface{}) {
	applySecurityHeaders(c)
	c.JSON(http.StatusOK, Response{
		Code:    0,
		Message: "success",
		Data:    data,
	})
}

// Fail sends the given HTTP status with
// { code: <status>, message: <message>, data: null }.
//
// The HTTP status is reused as the business code so the client can
// branch on either the status code or the JSON code field.
func Fail(c *gin.Context, status int, message string) {
	applySecurityHeaders(c)
	c.JSON(status, Response{
		Code:    status,
		Message: message,
		Data:    nil,
	})
}
