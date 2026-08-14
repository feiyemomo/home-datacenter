package middleware

import (
	"log"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/utils"
)

// PanicSink receives a recovered panic so the caller can persist or
// alert on it (e.g. write a SystemLog row). Set to nil to only log to
// stderr.
type PanicSink func(c *gin.Context, panicVal any, stack []byte)

// Recovery replaces gin.Recovery(). It captures panics raised anywhere
// in the handler chain, logs the stack, optionally sinks them (e.g. to
// SystemLog for the dashboard), and returns a unified
// {code:500, data:null} envelope instead of an empty body. Install it
// FIRST via gin.New() so it wraps every other middleware.
func Recovery(sink PanicSink) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				stack := debug.Stack()
				log.Printf("[recovery] panic serving %s %s: %v\n%s",
					c.Request.Method, c.Request.URL.Path, r, stack)
				if sink != nil {
					sink(c, r, stack)
				}
				c.AbortWithStatusJSON(http.StatusInternalServerError, utils.Response{
					Code:    http.StatusInternalServerError,
					Message: "internal server error",
					Data:    nil,
				})
			}
		}()
		c.Next()
	}
}