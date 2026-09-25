package automation

import (
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

// Handler exposes CRUD endpoints for automation rules.
//
// All endpoints are admin-only (the route group applies middleware.RequireAdmin).
//
//	GET    /api/v1/automation/rules          List all rules
//	POST   /api/v1/automation/rules          Create a rule
//	GET    /api/v1/automation/rules/:id      Fetch one rule
//	PUT    /api/v1/automation/rules/:id      Update a rule
//	DELETE /api/v1/automation/rules/:id      Delete a rule
//	POST   /api/v1/automation/rules/:id/test Manually fire a rule
type Handler struct {
	DB     *gorm.DB
	Engine *Engine
	Bus    *eventbus.Bus
}

// NewHandler creates a Handler wired to the given DB and Engine.
func NewHandler(db *gorm.DB, eng *Engine, bus *eventbus.Bus) *Handler {
	return &Handler{DB: db, Engine: eng, Bus: bus}
}

// ruleResponse is the JSON shape returned by all rule endpoints. It
// mirrors model.Rule but guarantees Condition/Action/Throttle are
// always well-formed objects (never null).
type ruleResponse struct {
	ID         uint   `json:"id"`
	Name       string `json:"name"`
	Trigger    string `json:"trigger"`
	Condition  model.Condition `json:"condition"`
	Action     model.Action    `json:"action"`
	Throttle   model.Throttle  `json:"throttle"`
	Enabled    bool   `json:"enabled"`
	FireCount  uint64 `json:"fire_count"`
	LastFireAt *int64 `json:"last_fire_at,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

func toResponse(r model.Rule) ruleResponse {
	var lastFire *int64
	if r.LastFireAt != nil {
		v := r.LastFireAt.Unix()
		lastFire = &v
	}
	return ruleResponse{
		ID:         r.ID,
		Name:       r.Name,
		Trigger:    r.Trigger,
		Condition:  r.Condition,
		Action:     r.Action,
		Throttle:   r.Throttle,
		Enabled:    r.Enabled,
		FireCount:  r.FireCount,
		LastFireAt: lastFire,
		CreatedAt:  r.CreatedAt.Unix(),
		UpdatedAt:  r.UpdatedAt.Unix(),
	}
}

// createRuleRequest is the JSON body for POST /rules.
type createRuleRequest struct {
	Name      string          `json:"name"`
	Trigger   string          `json:"trigger"`
	Condition model.Condition `json:"condition"`
	Action    model.Action    `json:"action"`
	Throttle  model.Throttle  `json:"throttle"`
	Enabled   *bool           `json:"enabled"`
}

// updateRuleRequest is the JSON body for PUT /rules/:id.
// Fields use pointers so omitted fields (e.g. only toggling enabled)
// do not overwrite existing Condition, Action, or Throttle with zero values.
type updateRuleRequest struct {
	Name      *string          `json:"name"`
	Trigger   *string          `json:"trigger"`
	Condition *model.Condition `json:"condition"`
	Action    *model.Action    `json:"action"`
	Throttle  *model.Throttle  `json:"throttle"`
	Enabled   *bool            `json:"enabled"`
}

// List returns all rules, newest first.
//
//	GET /api/v1/automation/rules
func (h *Handler) List(c *gin.Context) {
	var rules []model.Rule
	if err := h.DB.Order("id DESC").Find(&rules).Error; err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to list rules")
		return
	}
	out := make([]ruleResponse, 0, len(rules))
	for _, r := range rules {
		out = append(out, toResponse(r))
	}
	utils.Success(c, gin.H{"rules": out, "total": len(out)})
}

// Create adds a new rule and reloads the engine cache.
//
//	POST /api/v1/automation/rules
func (h *Handler) Create(c *gin.Context) {
	var req createRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		utils.Fail(c, http.StatusBadRequest, "name is required")
		return
	}
	if req.Trigger == "" {
		utils.Fail(c, http.StatusBadRequest, "trigger is required")
		return
	}
	if req.Action.Type == "" {
		utils.Fail(c, http.StatusBadRequest, "action.type is required")
		return
	}
	if err := validateAction(req.Action); err != nil {
		log.Printf("[handler] invalid action: %v", err)
		utils.Fail(c, http.StatusBadRequest, "invalid action")
		return
	}

	r := model.Rule{
		Name:      req.Name,
		Trigger:   req.Trigger,
		Condition: req.Condition,
		Action:    req.Action,
		Throttle:  req.Throttle,
		Enabled:   true,
	}
	if req.Enabled != nil {
		r.Enabled = *req.Enabled
	}
	if err := h.DB.Create(&r).Error; err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to create rule")
		return
	}
	_ = h.Engine.Reload()
	// v1.8.22: audit-trail event for rule creation.
	if h.Bus != nil {
		payload, _ := json.Marshal(eventbus.AutomationManagePayload{
			AdminID:  c.GetUint("user_id"),
			RuleID:   r.ID,
			RuleName: r.Name,
			Action:   "create",
			Ts:       time.Now().Unix(),
		})
		h.Bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicAutomationCreate,
			Payload: payload,
			Source:  eventbus.SourceSystem,
		})
	}
	utils.Success(c, toResponse(r))
}

// Get fetches one rule by ID.
//
//	GET /api/v1/automation/rules/:id
func (h *Handler) Get(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	var r model.Rule
	if err := h.DB.First(&r, id).Error; err != nil {
		utils.Fail(c, http.StatusNotFound, "rule not found")
		return
	}
	utils.Success(c, toResponse(r))
}

// Update modifies an existing rule.
//
//	PUT /api/v1/automation/rules/:id
func (h *Handler) Update(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	var req updateRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Action != nil && req.Action.Type != "" {
		if err := validateAction(*req.Action); err != nil {
			log.Printf("[handler] invalid action: %v", err)
			utils.Fail(c, http.StatusBadRequest, "invalid action")
			return
		}
	}

	var r model.Rule
	if err := h.DB.First(&r, id).Error; err != nil {
		utils.Fail(c, http.StatusNotFound, "rule not found")
		return
	}
	if req.Name != nil && *req.Name != "" {
		r.Name = *req.Name
	}
	if req.Trigger != nil && *req.Trigger != "" {
		r.Trigger = *req.Trigger
	}
	if req.Condition != nil {
		r.Condition = *req.Condition
	}
	if req.Action != nil && req.Action.Type != "" {
		r.Action = *req.Action
	}
	if req.Throttle != nil {
		r.Throttle = *req.Throttle
	}
	if req.Enabled != nil {
		r.Enabled = *req.Enabled
	}
	if err := h.DB.Save(&r).Error; err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to update rule")
		return
	}
	_ = h.Engine.Reload()
	// v1.8.22: audit-trail event for rule update with settings detail.
	if h.Bus != nil {
		detail := "修改规则配置"
		if req.Enabled != nil {
			if *req.Enabled {
				detail = "启用规则"
			} else {
				detail = "停用规则"
			}
		}
		payload, _ := json.Marshal(eventbus.AutomationManagePayload{
			AdminID:  c.GetUint("user_id"),
			RuleID:   r.ID,
			RuleName: r.Name,
			Action:   "update",
			Detail:   detail,
			Ts:       time.Now().Unix(),
		})
		h.Bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicAutomationUpdate,
			Payload: payload,
			Source:  eventbus.SourceSystem,
		})
	}
	utils.Success(c, toResponse(r))
}

// Delete removes a rule (soft delete via gorm.DeletedAt).
//
//	DELETE /api/v1/automation/rules/:id
func (h *Handler) Delete(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	// v1.8.22: snapshot the rule name BEFORE the soft-delete so
	// the audit-log event carries a friendly label (gorm's soft
	// delete keeps the row, but a future hard-delete would not).
	var snap model.Rule
	_ = h.DB.Select("name").First(&snap, id).Error
	if err := h.DB.Delete(&model.Rule{}, id).Error; err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to delete rule")
		return
	}
	_ = h.Engine.Reload()
	// v1.8.22: audit-trail event for rule deletion.
	if h.Bus != nil {
		payload, _ := json.Marshal(eventbus.AutomationManagePayload{
			AdminID:  c.GetUint("user_id"),
			RuleID:   uint(id),
			RuleName: snap.Name,
			Action:   "delete",
			Ts:       time.Now().Unix(),
		})
		h.Bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicAutomationDelete,
			Payload: payload,
			Source:  eventbus.SourceSystem,
		})
	}
	utils.Success(c, gin.H{"id": id, "deleted": true})
}

// Test manually fires a rule with a synthetic event so the user can
// verify the action works without waiting for a real trigger.
//
//	POST /api/v1/automation/rules/:id/test
func (h *Handler) Test(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	var r model.Rule
	if err := h.DB.First(&r, id).Error; err != nil {
		utils.Fail(c, http.StatusNotFound, "rule not found")
		return
	}

	// Synthesize an event that matches the rule's trigger.
	ev := eventbus.Event{
		Topic:    r.Trigger,
		Source:   "test",
		Severity: eventbus.SeverityInfo,
		Payload:  []byte(`{}`),
	}
	// Run the action synchronously so we can report success/failure
	// back to the caller.
	if err := h.Engine.executeAction(r.Action, ev); err != nil {
		log.Printf("[handler] action failed: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "action failed")
		return
	}
	utils.Success(c, gin.H{
		"id":        r.ID,
		"name":      r.Name,
		"action":    r.Action.Type,
		"test_fired": true,
	})
}

// validateAction returns an error if the action is missing required
// fields for its type. This is a fail-fast check at CRUD time so the
// user gets immediate feedback; the engine re-checks at fire time.
func validateAction(a model.Action) error {
	switch a.Type {
	case "notify":
		// UserID == 0 means "broadcast to admins", which is allowed.
		if a.Title == "" && a.Body == "" {
			return errAction("notify action requires title or body")
		}
	case "mqtt":
		if a.Topic == "" {
			return errAction("mqtt action requires topic")
		}
		if !isAllowedMQTTTopic(a.Topic) {
			return errAction("mqtt topic must be within home-datacenter/ namespace")
		}
	case "webhook":
		if a.URL == "" {
			return errAction("webhook action requires url")
		}
		u, err := url.Parse(a.URL)
		if err != nil {
			return errAction("webhook url is invalid")
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return errAction("webhook url scheme must be http or https")
		}
		if u.Host == "" {
			return errAction("webhook url missing host")
		}
	default:
		return errAction("unknown action type: " + a.Type)
	}
	return nil
}

type errAction string

func (e errAction) Error() string { return string(e) }

// Metrics returns the global engine counters + per-rule stats.
//
//	GET /api/v1/automation/metrics        Snapshot of the engine
//	GET /api/v1/automation/metrics?reset=1  Reset all counters (admin-only)
//
// Per-rule stats are keyed by rule ID; pair them with the rule
// name from /api/v1/automation/rules for human-friendly output.
func (h *Handler) Metrics(c *gin.Context) {
	if c.Query("reset") == "1" {
		h.Engine.Metrics().Reset()
		utils.Success(c, gin.H{"reset": true})
		return
	}
	utils.Success(c, h.Engine.Metrics().Snapshot())
}

// RuleMetrics returns the per-rule slice of metrics, augmented with
// the rule's name and last_fire timestamp from the DB.
//
//	GET /api/v1/automation/rules/:id/metrics
func (h *Handler) RuleMetrics(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	var r model.Rule
	if err := h.DB.First(&r, id).Error; err != nil {
		utils.Fail(c, http.StatusNotFound, "rule not found")
		return
	}
	snap := h.Engine.Metrics().Snapshot()
	rm := snap.PerRule[r.ID]
	if rm == nil {
		// Rule exists in DB but has never fired. Surface an
		// empty stats object so the UI doesn't have to handle
		// the missing case.
		rm = &RuleMetrics{}
	}
	out := gin.H{
		"id":         r.ID,
		"name":       r.Name,
		"trigger":    r.Trigger,
		"enabled":    r.Enabled,
		"fire_count": r.FireCount, // persistent total
		"runtime":    rm,          // in-memory slice
	}
	if r.LastFireAt != nil {
		out["last_fire_at"] = r.LastFireAt.Unix()
	}
	utils.Success(c, out)
}

// Cooldown temporarily pins a rule's last-fire timestamp to "now",
// effectively silencing it for the requested number of seconds.
// This is an admin escape hatch for silencing a misbehaving rule
// without deleting it.
//
//	POST /api/v1/automation/rules/:id/cooldown
//	body: {"seconds": 3600}
func (h *Handler) Cooldown(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	var body struct {
		Seconds int `json:"seconds"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Seconds <= 0 || body.Seconds > 86400 {
		utils.Fail(c, http.StatusBadRequest, "seconds must be between 1 and 86400")
		return
	}
	if err := h.Engine.PinCooldown(uint(id), time.Duration(body.Seconds)*time.Second); err != nil {
		log.Printf("[handler] failed to set cooldown: %v", err)
		utils.Fail(c, http.StatusInternalServerError, "failed to set cooldown")
		return
	}
	utils.Success(c, gin.H{"id": id, "cooldown_s": body.Seconds})
}
