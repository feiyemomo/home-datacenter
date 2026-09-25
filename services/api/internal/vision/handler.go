package vision

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/maintenance"
)

type Handler struct {
	client *Client
	bus    *eventbus.Bus
}

func NewHandler(client *Client, bus *eventbus.Bus) *Handler {
	return &Handler{client: client, bus: bus}
}

func (h *Handler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/status", h.GetStatus)
	r.GET("/persons", h.ListPersons)
	r.POST("/persons", h.RegisterPerson)
	r.DELETE("/persons/:name", h.DeletePerson)
	r.POST("/analyze", h.Analyze)
}

func (h *Handler) GetStatus(c *gin.Context) {
	cpuUsage := maintenance.SampleCPU()
	if h.client == nil {
		c.JSON(http.StatusOK, gin.H{
			"code": 0,
			"data": gin.H{
				"online":            false,
				"error":             "vision service not configured",
				"cpu_usage_percent": cpuUsage,
				"cpu_gate":          getGateStatus(cpuUsage),
			},
		})
		return
	}
	health, err := h.client.Health(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": 0,
			"data": gin.H{
				"online":            false,
				"error":             err.Error(),
				"cpu_usage_percent": cpuUsage,
				"cpu_gate":          getGateStatus(cpuUsage),
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"online":             true,
			"face_engine_ready":  health.FaceEngineReady,
			"pose_engine_ready":  health.PoseEngineReady,
			"registered_persons": health.RegisteredPersons,
			"cpu_usage_percent":  cpuUsage,
			"cpu_gate":           getGateStatus(cpuUsage),
		},
	})
}

func getGateStatus(cpu float64) string {
	if cpu >= 80.0 {
		return "circuit_break" // Skip all vision analysis
	}
	if cpu >= 60.0 {
		return "degraded" // Face recognition only
	}
	return "normal" // Full analysis (face + pose)
}

func (h *Handler) ListPersons(c *gin.Context) {
	persons, err := h.client.ListPersons(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"code": 502, "message": "Failed to query vision service: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": persons})
}

type RegisterRequest struct {
	Name        string `json:"name"`
	ImageBase64 string `json:"image"`
}

func (h *Handler) RegisterPerson(c *gin.Context) {
	contentType := c.ContentType()

	if contentType == "application/json" {
		var req RegisterRequest
		if err := c.ShouldBindJSON(&req); err != nil || req.Name == "" || req.ImageBase64 == "" {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "name and image base64 are required"})
			return
		}
		if err := h.client.RegisterPerson(c.Request.Context(), req.Name, req.ImageBase64); err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"code": 422, "message": err.Error()})
			return
		}
		if h.bus != nil {
			payload, _ := json.Marshal(eventbus.VisionPersonManagePayload{
				AdminID:   c.GetUint("user_id"),
				AdminName: c.GetString("user_name"),
				Name:      req.Name,
				Action:    "register",
				Ts:        time.Now().Unix(),
			})
			h.bus.Publish(eventbus.Event{
				Topic:   eventbus.TopicVisionPersonRegister,
				Payload: payload,
				Source:  eventbus.SourceSystem,
			})
		}
		c.JSON(http.StatusOK, gin.H{"code": 0, "message": "Person registered successfully"})
		return
	}

	// Multipart form upload
	name := c.PostForm("name")
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "name form field is required"})
		return
	}

	file, _, err := c.Request.FormFile("image")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "image file upload is required"})
		return
	}
	defer file.Close()

	bytesData, err := io.ReadAll(file)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": "Failed to read uploaded file"})
		return
	}

	b64 := base64.StdEncoding.EncodeToString(bytesData)
	if err := h.client.RegisterPerson(c.Request.Context(), name, b64); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"code": 422, "message": err.Error()})
		return
	}

	if h.bus != nil {
		payload, _ := json.Marshal(eventbus.VisionPersonManagePayload{
			AdminID:   c.GetUint("user_id"),
			AdminName: c.GetString("user_name"),
			Name:      name,
			Action:    "register",
			Ts:        time.Now().Unix(),
		})
		h.bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicVisionPersonRegister,
			Payload: payload,
			Source:  eventbus.SourceSystem,
		})
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "Person registered successfully"})
}

func (h *Handler) DeletePerson(c *gin.Context) {
	name := c.Param("name")
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "name is required"})
		return
	}

	if err := h.client.DeletePerson(c.Request.Context(), name); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
		return
	}

	if h.bus != nil {
		payload, _ := json.Marshal(eventbus.VisionPersonManagePayload{
			AdminID:   c.GetUint("user_id"),
			AdminName: c.GetString("user_name"),
			Name:      name,
			Action:    "delete",
			Ts:        time.Now().Unix(),
		})
		h.bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicVisionPersonDelete,
			Payload: payload,
			Source:  eventbus.SourceSystem,
		})
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "Person deleted"})
}

func (h *Handler) Analyze(c *gin.Context) {
	var req AnalyzeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "Invalid JSON body"})
		return
	}

	res, err := h.client.Analyze(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res)
}
