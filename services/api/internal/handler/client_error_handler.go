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
// v1.8.26 aggregation/dedup: a playback retry loop or a noisy page
// previously flooded the log with N identical rows for the same error.
// Now reports are deduplicated by (context + message) within a window:
// the first report creates a row, and repeats bump a count on the
// existing row instead of inserting N copies. The operator sees one
// row per error "signature" with a count, not a wall of duplicates.
// The signature is (context, message) — stack/URL vary with the page
// and are not part of the key.
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

	// dedupeWindow is how far back to look for an identical
	// (context, message) row to merge a repeat report into.
	dedupeWindow time.Duration
}

// NewClientErrorHandler creates a handler bound to the given GORM DB.
func NewClientErrorHandler(db *gorm.DB) *ClientErrorHandler {
	return &ClientErrorHandler{db: db, maxPerMin: 60, dedupeWindow: 10 * time.Minute}
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
	payloadMap := map[string]interface{}{
		"stack":   body.Stack,
		"url":     body.URL,
		"context": body.Context,
		"user_id": userID,
		"count":   1,
	}

	// v1.8.26: dedup — if an identical (context, message) row exists
	// within the window, bump its count and refresh its timestamp
	// instead of inserting a duplicate. This keeps a retry loop from
	// flooding the log with N copies of the same error.
	if existingID, ok := h.findDuplicate(body.Context, body.Message); ok {
		payloadJSON, _ := json.Marshal(payloadMap)
		// Load the existing row's count and increment it.
		var existing model.SystemLog
		if err := h.db.First(&existing, existingID).Error; err == nil {
			var prev map[string]interface{}
			if json.Unmarshal([]byte(existing.Payload), &prev) == nil {
				if n, ok := prev["count"].(float64); ok {
					payloadMap["count"] = int(n) + 1
				}
			}
			payloadJSON, _ = json.Marshal(payloadMap)
			// Refresh timestamp so the row stays near the top and the
			// dedup window keeps sliding while the error is recurring.
			updates := map[string]interface{}{
				"ts":      time.Now().Unix(),
				"payload": string(payloadJSON),
			}
			if err := h.db.Model(&model.SystemLog{}).Where("id = ?", existingID).Updates(updates).Error; err != nil {
				log.Printf("[handler] update client error count: %v", err)
			}
		}
		utils.Success(c, gin.H{"accepted": true, "id": existingID, "deduped": true, "count": payloadMap["count"]})
		return
	}

	payloadJSON, _ := json.Marshal(payloadMap)

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

// findDuplicate looks for an existing client-error row with the same
// context and message within the dedup window. It returns the row's ID
// and whether one was found. context is matched via a LIKE on the
// payload JSON (it is not a column), message is a plain column.
func (h *ClientErrorHandler) findDuplicate(context, message string) (uint, bool) {
	cutoff := time.Now().Add(-h.dedupeWindow).Unix()
	// Escape the context for a JSON string literal so the LIKE pattern
	// matches the "context":"..." key value pair.
	ctxJSON, _ := json.Marshal(context)
	like := "%\"context\":" + string(ctxJSON) + "%"
	var row model.SystemLog
	err := h.db.Where("event_type = ? AND source = ? AND message = ? AND ts > ? AND payload LIKE ?",
		"client.error", "web", message, cutoff, like).
		Order("ts DESC").First(&row).Error
	if err != nil {
		return 0, false
	}
	return row.ID, true
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