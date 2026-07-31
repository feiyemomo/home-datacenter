package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

// SystemLogHandler exposes the persisted SystemLog table over REST.
//
//	Route: GET /api/v1/system/logs
//
// The endpoint is JWT-protected (mounted under the same group as
// /system/status). It returns the most recent entries first so the
// dashboard's log pane can render a tail-style view without
// client-side sorting.
type SystemLogHandler struct {
	db *gorm.DB
}

// NewSystemLogHandler creates a handler bound to the given GORM DB.
func NewSystemLogHandler(db *gorm.DB) *SystemLogHandler {
	return &SystemLogHandler{db: db}
}

// List returns a page of system log entries, newest first.
//
// Query params:
//
//	limit       int   default 50, max 200
//	offset      int   default 0
//	event_type  str   optional filter (e.g. "device.status",
//	                  "user.login"); matched by exact equality on
//	                  the indexed event_type column.
//	level       str   optional filter (v1.6.36): "critical" |
//	                  "normal" | "info". Matched by exact equality
//	                  on the indexed level column.
//
// Response envelope (utils.Success):
//
//	{ "logs": [...], "total": N }
//
// where `total` is the count after applying the event_type / level
// filters (but ignoring limit/offset) so the dashboard can render
// a pager.
func (h *SystemLogHandler) List(c *gin.Context) {
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if err != nil || limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	offset, err := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if err != nil || offset < 0 {
		offset = 0
	}

	eventType := c.Query("event_type")
	level := c.Query("level")

	q := h.db.Model(&model.SystemLog{})
	if eventType != "" {
		q = q.Where("event_type = ?", eventType)
	}
	if level != "" {
		q = q.Where("level = ?", level)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to count logs")
		return
	}

	var logs []model.SystemLog
	if err := q.
		Order("ts DESC, id DESC").
		Limit(limit).
		Offset(offset).
		Find(&logs).Error; err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to list logs")
		return
	}

	// Always return a non-nil slice so the JSON serializes `[]`
	// instead of `null` when there are no rows.
	if logs == nil {
		logs = []model.SystemLog{}
	}

	utils.Success(c, gin.H{
		"logs":  logs,
		"total": total,
	})
}
