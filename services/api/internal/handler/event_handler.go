package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/utils"
)

// EventHandler exposes the persisted event history API.
type EventHandler struct {
	repo *repository.EventRepository
}

// NewEventHandler creates a handler backed by the event repository.
func NewEventHandler(repo *repository.EventRepository) *EventHandler {
	return &EventHandler{repo: repo}
}

// eventItem is the JSON shape returned in event lists. Payload is
// kept as a flexible map so the frontend can render different event
// types without a hardcoded schema.
type eventItem struct {
	ID        uint                   `json:"id"`
	Type      string                 `json:"type"`
	Source    string                 `json:"source"`
	Severity  string                 `json:"severity"`
	Payload   map[string]interface{} `json:"payload"`
	Status    string                 `json:"status"`
	Timestamp string                 `json:"timestamp"`
}

// eventListResponse is the paginated list response shape.
type eventListResponse struct {
	Items []eventItem `json:"items"`
	Total int64       `json:"total"`
}

// toEventItem converts a DB row to the API response shape. The
// Payload JSON string is unmarshalled into a flexible map so the
// frontend can render it without knowing the full schema ahead of
// time.
func toEventItem(ev model.StoredEvent) eventItem {
	var payload map[string]interface{}
	if ev.Payload != "" && ev.Payload != "{}" {
		_ = json.Unmarshal([]byte(ev.Payload), &payload)
	}
	if payload == nil {
		payload = map[string]interface{}{}
	}
	return eventItem{
		ID:        ev.ID,
		Type:      ev.Topic,
		Source:    ev.Source,
		Severity:  ev.Severity,
		Payload:   payload,
		Status:    string(ev.Status),
		Timestamp: ev.Timestamp.Format("2006-01-02 15:04:05"),
	}
}

// List queries the event history with optional filters.
//
//	Route: GET /api/v1/events
//
// Query params:
//
//	page   int     (default 1)
//	limit  int     (default 20, max 100)
//	type   string  (exact topic match, e.g. "camera.motion")
//	source string  (e.g. "camera")
//	since  string  (RFC3339, events after this time)
//	before string  (RFC3339, events before this time)
func (h *EventHandler) List(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	params := repository.EventListParams{
		Topic:  c.Query("type"),
		Source: c.Query("source"),
		Limit:  limit,
		Offset: (page - 1) * limit,
	}

	if s := c.Query("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			utils.Fail(c, http.StatusBadRequest, "invalid since format (use RFC3339)")
			return
		}
		params.Since = &t
	}
	if s := c.Query("before"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			utils.Fail(c, http.StatusBadRequest, "invalid before format (use RFC3339)")
			return
		}
		params.Before = &t
	}

	rows, total, err := h.repo.List(params)
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to query events")
		return
	}

	items := make([]eventItem, 0, len(rows))
	for _, ev := range rows {
		items = append(items, toEventItem(ev))
	}

	utils.Success(c, eventListResponse{
		Items: items,
		Total: total,
	})
}

// Get returns a single event by ID.
//
//	Route: GET /api/v1/events/:id
func (h *EventHandler) Get(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid event id")
		return
	}

	ev, err := h.repo.GetByID(uint(id))
	if err != nil {
		utils.Fail(c, http.StatusNotFound, "event not found")
		return
	}

	utils.Success(c, toEventItem(*ev))
}

// Delete removes an event by ID.
//
//	Route: DELETE /api/v1/events/:id
func (h *EventHandler) Delete(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid event id")
		return
	}

	if err := h.repo.Delete(uint(id)); err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to delete event")
		return
	}

	utils.Success(c, nil)
}
