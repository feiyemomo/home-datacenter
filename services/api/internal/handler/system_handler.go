package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/device"
	"home-datacenter-api/internal/mqtt"
	"home-datacenter-api/internal/utils"
	"home-datacenter-api/internal/ws"
)

// SystemHandler exposes system-level status and debug endpoints.
type SystemHandler struct {
	mqttClient *mqtt.Client
	hub        *ws.Hub
	deviceMgr  *device.Manager
	startTime  time.Time
}

// NewSystemHandler creates a handler for system status and MQTT debug.
func NewSystemHandler(
	mqttClient *mqtt.Client,
	hub *ws.Hub,
	deviceMgr *device.Manager,
) *SystemHandler {
	return &SystemHandler{
		mqttClient: mqttClient,
		hub:        hub,
		deviceMgr:  deviceMgr,
		startTime:  time.Now(),
	}
}

// Status returns real-time system metrics for the dashboard.
//
//	Route: GET /api/v1/system/status
func (h *SystemHandler) Status(c *gin.Context) {
	onlineDevices := h.deviceMgr.GetOnlineDevices()

	utils.Success(c, gin.H{
		"mqtt_connected":      h.mqttClient.IsConnected(),
		"ws_clients":          h.hub.OnlineClientCount(),
		"online_device_count": len(onlineDevices),
		"online_device_ids":   onlineDevices,
		"uptime_seconds":      int64(time.Since(h.startTime).Seconds()),
		"server_time":         time.Now().Format("2006-01-02 15:04:05"),
	})
}

// PublishRequest is the JSON body for the MQTT publish endpoint.
type PublishRequest struct {
	Topic   string `json:"topic"   binding:"required"`
	Payload string `json:"payload" binding:"required"`
	QoS     byte   `json:"qos"`
}

// Publish sends a message to an MQTT topic. Admin only.
//
//	Route: POST /api/v1/mqtt/publish
func (h *SystemHandler) Publish(c *gin.Context) {
	var req PublishRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}

	if !h.mqttClient.IsConnected() {
		utils.Fail(c, http.StatusServiceUnavailable, "mqtt not connected")
		return
	}

	// Default QoS to 1 if not specified.
	qos := req.QoS
	if qos > 2 {
		qos = 2
	}
	if qos == 0 {
		qos = 1
	}

	h.mqttClient.Handler().Publish(req.Topic, req.Payload, qos)

	utils.Success(c, gin.H{
		"topic":   req.Topic,
		"payload": req.Payload,
		"qos":     qos,
	})
}
