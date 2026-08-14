package middleware

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// statusRecorder captures the response status code so downstream
// middleware can inspect it after the handler runs. Defaults to 200;
// WriteHeader overrides it.
type statusRecorder struct {
	gin.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// ErrorLogMiddleware writes a SystemLog row (event_type "server.error")
// for every >=500 response so backend failures surface in the dashboard
// log pane instead of only in container stderr. It is rate-limited
// (default 30/min, process-wide) and deduplicated by (method+path+status)
// within a window so a fault loop yields one row with a bumping count,
// not a wall of duplicates.
//
// Panics are handled by Recovery (which records its own "server.panic"
// row) and do not reach here — the status recorder stays 200 because
// no normal response was written.
func ErrorLogMiddleware(db *gorm.DB, bus *eventbus.Bus) gin.HandlerFunc {
	var (
		mu     sync.Mutex
		window time.Time
		count  int
	)
	const maxPerMin = 30
	const dedupeWindow = 10 * time.Minute

	return func(c *gin.Context) {
		w := &statusRecorder{ResponseWriter: c.Writer, status: http.StatusOK}
		c.Writer = w
		c.Next()

		if w.status < 500 {
			return
		}

		// Rate limit (best-effort, process-wide).
		mu.Lock()
		now := time.Now()
		if now.Sub(window) >= time.Minute {
			window = now
			count = 0
		}
		if count >= maxPerMin {
			mu.Unlock()
			return
		}
		count++
		mu.Unlock()

		msg := http.StatusText(w.status)
		if msg == "" {
			msg = "error"
		}
		message := c.Request.Method + " " + c.Request.URL.Path + " → " + msg
		level := model.LevelWarning
		if w.status >= 500 {
			level = model.LevelCritical
		}

		// Dedup: bump the count on an identical recent row.
		cutoff := time.Now().Add(-dedupeWindow).Unix()
		var existing model.SystemLog
		if err := db.Where("event_type = ? AND message = ? AND ts > ?",
			"server.error", message, cutoff).
			Order("ts DESC").First(&existing).Error; err == nil {
			payloadMap := map[string]any{"count": 1}
			var prev map[string]any
			if json.Unmarshal([]byte(existing.Payload), &prev) == nil {
				if n, ok := prev["count"].(float64); ok {
					payloadMap["count"] = int(n) + 1
				}
			}
			pj, _ := json.Marshal(payloadMap)
			_ = db.Model(&model.SystemLog{}).Where("id = ?", existing.ID).
				Updates(map[string]any{"ts": time.Now().Unix(), "payload": string(pj)}).Error
			return
		}

		pj, _ := json.Marshal(map[string]any{"count": 1})
		entry := &model.SystemLog{
			Ts:        time.Now().Unix(),
			EventType: "server.error",
			Level:     level,
			Source:    "server",
			Message:   message,
			Payload:   string(pj),
		}
		if err := db.Create(entry).Error; err != nil {
			log.Printf("errorlog: persist failed: %v", err)
			return
		}
		if bus != nil {
			if eb, err := json.Marshal(entry); err == nil {
				bus.Publish(eventbus.Event{
					Topic:   eventbus.TopicSystemLog,
					Source:  eventbus.SourceSystem,
					Payload: eb,
				})
			}
		}
	}
}