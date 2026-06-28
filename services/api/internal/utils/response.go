package utils

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

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
	c.JSON(status, Response{
		Code:    status,
		Message: message,
		Data:    nil,
	})
}
