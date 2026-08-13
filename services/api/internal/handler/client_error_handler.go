package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

// ClientErrorHandler ingests errors reported from the web frontend —
// uncaught JS exceptions, unhandled promise rejections, and failed
// media playback — and persists them as SystemLog rows so they surface
// in the dashboard's log pane.
//
// Why this exists: before v1.8.25 the system log only captured backend
// events (device/camera/user). Client-side failures ("视频加载失败",
// a JS null deref, a 404 fetch) were invisible to the server, so remote
// diagnosis was impossible — the operator had to SSH in and guess. This
// endpoint closes that gap: the frontend fire-and-forgets a report, and
// the operator sees a "client.error" log entry with the message, stack,
// URL and page context.
//
// Route: POST /api/v1/system/client-errors (JWT-protected)
type ClientErrorHandler struct {
	db *gorm.DB

	// mu guards the rate-limit window below.
	mu sync.Mutex
	// window is the start of the current rolling minute; count is the
	// number of client-error rows accepted in it. This is a simple
	// global limiter that stops a misbehaving page (e.g. a playback
	// retry loop) from flooding SQLite. Default cap: 60/min.
	window    time.Time
	count     int
	maxPerMin int
}

// NewClientErrorHandler creates a handler bound to the given GORM DB.
func NewClientErrorHandler(db *gorm.DB) *ClientErrorHandler {
	return &ClientErrorHandler{db: db, maxPerMin: 60}
}

// reportRequestBody is the JSON body accepted by Report.
type reportRequestBody struct {
	// Message is a short human-readable description in Chinese, e.g.
	// "视频加载失败，请检查网络或浏览器解码能力". Required, capped at 500.
	Message string `json:"message"`
	// Stack is the JS stack trace (uncaught error / rejection). Optional.
	Stack string `json:"stack"`
	// URL is the browser URL / resource URL the error occurred on.
	URL string `json:"url"`
	// Level overrides the severity. Default "normal". Use "critical"
	// for user-blocking failures like playback.
	Level string `json:"level"`
	// Context is a free-form tag describing where the error came from
	// (e.g. "recording.playback", "window.onerror", "unhandledrejection").
	Context string `json:"context"`
}

// Report persists a client-side error report as a SystemLog row.
func (h *ClientErrorHandler) Report(c *gin.Context) {
	var body reportRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid body")
		return
	}
	body.Message = strings.TrimSpace(body.Message)
	if body.Message == "" {
		utils.Fail(c, http.StatusBadRequest, "message required")
		return
	}
	if len(body.Message) > 500 {
		body.Message = body.Message[:500]
	}
	if len(body.Stack) > 2000 {
		body.Stack = body.Stack[:2000]
	}
	if len(body.Context) > 64 {
		body.Context = body.Context[:64]
	}

	level := model.LevelNormal
	switch body.Level {
	case model.LevelCritical:
		level = model.LevelCritical
	case model.LevelInfo:
		level = model.LevelInfo
	}

	if !h.allow() {
		// Rate-limited: still return 2xx with a flag so the client
		// doesn't treat a flood of reports as a new error to retry.
		utils.Success(c, gin.H{"accepted": false, "reason": "rate_limited"})
		return
	}

	// Attach the authenticated user id (set by the JWT middleware) so
	// the log entry says which user hit the error.
	userID := ""
	if raw, exists := c.Get("user_id"); exists {
		userID = fmt.Sprintf("%v", raw)
	}
	payloadJSON, _ := json.Marshal(map[string]string{
		"stack":   body.Stack,
		"url":     body.URL,
		"context": body.Context,
		"user_id": userID,
	})

	entry := &model.SystemLog{
		Ts:        time.Now().Unix(),
		EventType: "client.error",
		Level:     level,
		Source:    "web",
		Message:   body.Message,
		Payload:   string(payloadJSON),
	}
	if err := h.db.Create(entry).Error; err != nil {
		log.Printf("[handler] persist client error: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "failed to store")
		return
	}
	utils.Success(c, gin.H{"accepted": true, "id": entry.ID})
}

// allow returns true if the caller may write a client-error row this
// minute. It enforces maxPerMin accepted rows per rolling minute.
func (h *ClientErrorHandler) allow() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	if now.Sub(h.window) >= time.Minute {
		h.window = now
		h.count = 0
	}
	if h.count >= h.maxPerMin {
		return false
	}
	h.count++
	return true
}