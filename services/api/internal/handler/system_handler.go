package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/device"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/maintenance"
	"home-datacenter-api/internal/mqtt"
	"home-datacenter-api/internal/utils"
	"home-datacenter-api/internal/ws"
)

// SystemHandler exposes system-level status and debug endpoints.
type SystemHandler struct {
	mqttClient    *mqtt.Client
	hub           *ws.Hub
	deviceMgr     *device.Manager
	startTime     time.Time
	diskPath      string
	recordingsDir string
	quotaBytes    uint64
	bus           *eventbus.Bus
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

// ConfigureMetrics sets up the filesystem and recording paths for telemetry.
func (h *SystemHandler) ConfigureMetrics(diskPath, recordingsDir string, quotaBytes uint64, bus *eventbus.Bus) {
	h.diskPath = diskPath
	h.recordingsDir = recordingsDir
	h.quotaBytes = quotaBytes
	h.bus = bus
}

// Status returns real-time system metrics for the dashboard.
//
//	Route: GET /api/v1/system/status
func (h *SystemHandler) Status(c *gin.Context) {
	onlineDevices := h.deviceMgr.GetOnlineDevices()
	metrics := maintenance.CollectMetrics(h.diskPath, h.recordingsDir, h.quotaBytes)

	utils.Success(c, gin.H{
		"mqtt_connected":      h.mqttClient.IsConnected(),
		"ws_clients":          h.hub.OnlineClientCount(),
		"online_device_count": len(onlineDevices),
		"online_device_ids":   onlineDevices,
		"uptime_seconds":      int64(time.Since(h.startTime).Seconds()),
		"server_time":         time.Now().Format("2006-01-02 15:04:05"),
		"metrics":             metrics,
	})
}

// CleanCache clears the transcode cache under recordings directory.
//
//	Route: POST /api/v1/system/clean-cache
func (h *SystemHandler) CleanCache(c *gin.Context) {
	reclaimed, count, err := maintenance.CleanTranscodeCache(h.recordingsDir)
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to clean transcode cache")
		return
	}

	if h.bus != nil {
		payload, _ := json.Marshal(map[string]any{
			"reclaimed_bytes": reclaimed,
			"deleted_files":   count,
			"user_id":         c.GetUint("user_id"),
			"ts":              time.Now().Unix(),
		})
		h.bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicSystemLog,
			Source:   eventbus.SourceSystem,
			Severity: eventbus.SeverityInfo,
			Payload:  payload,
		})
	}

	utils.Success(c, gin.H{
		"reclaimed_bytes": reclaimed,
		"deleted_files":   count,
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
//
// Security: the topic is restricted to the home-datacenter namespace
// (prefix "home-datacenter/"). This prevents a compromised admin token
// from publishing to arbitrary broker topics — e.g. retained messages
// on $SYS or third-party plugin topics that other devices on the
// broker might consume.
func (h *SystemHandler) Publish(c *gin.Context) {
	var req PublishRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}

	// Restrict publishes to the home-datacenter topic namespace.
	if !isAllowedTopic(req.Topic) {
		utils.Fail(c, http.StatusBadRequest, "topic must be within the home-datacenter/ namespace")
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

// mqttPrefix is the root namespace all server-managed topics share.
const mqttPrefix = "home-datacenter/"

// isAllowedTopic reports whether a publish target is inside the
// home-datacenter namespace and is not a broker control topic ($SYS,
// $SHARE). Retained-flag abuse on control topics is the main risk we
// guard against here.
func isAllowedTopic(topic string) bool {
	if topic == "" {
		return false
	}
	if strings.HasPrefix(topic, "$") {
		return false
	}
	return strings.HasPrefix(topic, mqttPrefix)
}

type StorageConfig struct {
	QuotaGB              int    `json:"quota_gb"`
	RetentionDays        int    `json:"retention_days"`
	ReducedRetentionDays int    `json:"reduced_retention_days"`
	ArchiveScheduleHour  int    `json:"archive_schedule_hour"`
	ArchiveMinAgeDays    int    `json:"archive_min_age_days"`
	ArchiveRetentionDays int    `json:"archive_retention_days"`
	LastSyncTimestamp    int64  `json:"last_sync_timestamp"`
	LastSyncOK           bool   `json:"last_sync_ok"`
	LastSyncError        string `json:"last_sync_error"`
}

const (
	storageConfigFile  = "/data/backup-state/storage-config.json"
	archiveStateFile   = "/data/backup-state/recordings-archive.json"
	archiveTriggerFile = "/data/backup-state/trigger"
)

// GetStorageConfig — GET /api/v1/system/storage/config
func (h *SystemHandler) GetStorageConfig(c *gin.Context) {
	cfg := StorageConfig{
		QuotaGB:              int(h.quotaBytes / (1024 * 1024 * 1024)),
		RetentionDays:        7,
		ReducedRetentionDays: 3,
		ArchiveScheduleHour:  3,
		ArchiveMinAgeDays:    7,
		ArchiveRetentionDays: 0,
	}
	if cfg.QuotaGB <= 0 {
		cfg.QuotaGB = 400
	}

	if data, err := os.ReadFile(storageConfigFile); err == nil {
		_ = json.Unmarshal(data, &cfg)
	}

	if data, err := os.ReadFile(archiveStateFile); err == nil {
		var state struct {
			TS    int64  `json:"ts"`
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(data, &state); err == nil {
			cfg.LastSyncTimestamp = state.TS
			cfg.LastSyncOK = state.OK
			cfg.LastSyncError = state.Error
		}
	}

	utils.Success(c, cfg)
}

// UpdateStorageConfig — PUT /api/v1/system/storage/config
func (h *SystemHandler) UpdateStorageConfig(c *gin.Context) {
	var req struct {
		QuotaGB              int `json:"quota_gb"`
		ArchiveScheduleHour  int `json:"archive_schedule_hour"`
		ArchiveMinAgeDays    int `json:"archive_min_age_days"`
		ArchiveRetentionDays int `json:"archive_retention_days"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.QuotaGB < 50 || req.QuotaGB > 5000 {
		utils.Fail(c, http.StatusBadRequest, "quota_gb must be between 50 and 5000")
		return
	}
	if req.ArchiveScheduleHour < 0 || req.ArchiveScheduleHour > 23 {
		utils.Fail(c, http.StatusBadRequest, "archive_schedule_hour must be between 0 and 23")
		return
	}
	if req.ArchiveMinAgeDays < 1 || req.ArchiveMinAgeDays > 365 {
		req.ArchiveMinAgeDays = 7
	}

	// Update in-memory metrics quota
	h.quotaBytes = uint64(req.QuotaGB) * 1024 * 1024 * 1024

	cfg := StorageConfig{
		QuotaGB:              req.QuotaGB,
		RetentionDays:        7,
		ReducedRetentionDays: 3,
		ArchiveScheduleHour:  req.ArchiveScheduleHour,
		ArchiveMinAgeDays:    req.ArchiveMinAgeDays,
		ArchiveRetentionDays: req.ArchiveRetentionDays,
	}

	data, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(storageConfigFile, data, 0644)

	if h.bus != nil {
		payload, _ := json.Marshal(map[string]any{
			"quota_gb":              req.QuotaGB,
			"archive_schedule_hour": req.ArchiveScheduleHour,
			"archive_min_age_days":  req.ArchiveMinAgeDays,
			"user_id":               c.GetUint("user_id"),
			"ts":                    time.Now().Unix(),
		})
		h.bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicSystemLog,
			Source:   eventbus.SourceSystem,
			Severity: eventbus.SeverityInfo,
			Payload:  payload,
		})
	}

	utils.Success(c, cfg)
}

// TriggerArchiveSync — POST /api/v1/system/storage/sync-archive
func (h *SystemHandler) TriggerArchiveSync(c *gin.Context) {
	if err := os.WriteFile(archiveTriggerFile, []byte(fmt.Sprintf("%d", time.Now().Unix())), 0644); err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to create trigger file")
		return
	}
	utils.Success(c, gin.H{
		"message": "archive sync triggered",
		"ts":      time.Now().Unix(),
	})
}
