package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/security"
	"home-datacenter-api/internal/utils"
)

// AllowedCoreContainers defines the security whitelist of containers that may be restarted via API.
var AllowedCoreContainers = map[string]bool{
	"home-api":          true,
	"home-web":          true,
	"home-frigate":      true,
	"home-mosquitto":    true,
	"home-vision":       true,
	"home-cloudflared":  true,
	"home-h3c-keepalive": true,
	"home-new-api":      true,
	"home-ops-agent":    true,
	"home-backup":       true,
	"mihomo":            true,
	"tailscale":         true,
}

// OpsHandler manages BFF endpoints for 1Panel and datacenter ops.
type OpsHandler struct {
	client      *OnePanelClient
	fallback    *HostFallbackProvider
	db          *gorm.DB
	bus         *eventbus.Bus
	defenseMgr  *security.DefenseManager
	executor    OperationExecutor
	vulnScanner *VulnScanner

	mu                  sync.RWMutex
	newAPIFailCount     int
	newAPICooldownUntil time.Time
}

// SetVulnScanner configures the VulnScanner for OpsHandler.
func (h *OpsHandler) SetVulnScanner(s *VulnScanner) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.vulnScanner = s
}

// GetVulnScanner returns the current VulnScanner instance.
func (h *OpsHandler) GetVulnScanner() *VulnScanner {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.vulnScanner
}

// NewOpsHandler creates a new OpsHandler.
func NewOpsHandler(
	client *OnePanelClient,
	fallback *HostFallbackProvider,
	db *gorm.DB,
	bus *eventbus.Bus,
	defenseMgr *security.DefenseManager,
) *OpsHandler {
	if client == nil {
		client = NewOnePanelClient("", "", "", "")
	}
	if fallback == nil {
		fallback = NewHostFallbackProvider("", "")
	}
	h := &OpsHandler{
		client:     client,
		fallback:   fallback,
		db:         db,
		bus:        bus,
		defenseMgr: defenseMgr,
	}
	h.executor = NewOperationExecutor(db, bus, client, fallback, WithBaselineExtractor(h.ComputeFingerprint))
	return h
}

// SetExecutor sets the OperationExecutor for OpsHandler.
func (h *OpsHandler) SetExecutor(exec OperationExecutor) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.executor = exec
}

// GetExecutor returns the current OperationExecutor.
func (h *OpsHandler) GetExecutor() OperationExecutor {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.executor
}

// RegisterRoutes registers ops endpoints under the given router group.
// Expected base path: /api/v1/ops
func (h *OpsHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/onepanel/session", h.GetOnePanelSession)
	rg.GET("/history", h.ListExecutionHistory)

	nas := rg.Group("/nas")
	{
		nas.GET("/status", h.GetNASStatus)
		nas.GET("/containers", h.ListNASContainers)
		nas.POST("/containers/:name/restart", h.RestartNASContainer)
		nas.GET("/security-summary", h.GetSecuritySummary)
	}
}

// GetOnePanelSession returns live authenticated session cookies for in-app 1Panel console access.
//
//	Route: GET /api/v1/ops/onepanel/session
func (h *OpsHandler) GetOnePanelSession(c *gin.Context) {
	if !c.GetBool("is_admin") {
		utils.Fail(c, http.StatusForbidden, "管理员权限不足，禁止获取 1Panel 运维会话")
		return
	}

	ctx, cancel := contextWithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	creds, err := h.client.GetSession(ctx)
	if err != nil {
		log.Printf("[ops] failed to fetch 1Panel session: %v", err)
		utils.Fail(c, http.StatusBadGateway, fmt.Sprintf("获取 1Panel 认证会话失败: %v", err))
		return
	}

	utils.Success(c, creds)
}

// ListExecutionHistory returns autonomous agent operation logs and execution timeline.
//
//	Route: GET /api/v1/ops/history
func (h *OpsHandler) ListExecutionHistory(c *gin.Context) {
	var histories []model.OpsExecutionHistory
	if h.db != nil {
		if err := h.db.Order("id DESC").Limit(50).Find(&histories).Error; err != nil {
			utils.Fail(c, http.StatusInternalServerError, err.Error())
			return
		}
	}

	if len(histories) == 0 {
		histories = []model.OpsExecutionHistory{}
	}

	utils.Success(c, histories)
}

// GetNASStatus returns CPU, RAM, and Disk pool watermark metrics.
//
//	Route: GET /api/v1/ops/nas/status
func (h *OpsHandler) GetNASStatus(c *gin.Context) {
	ctx, cancel := contextWithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	// 1. Attempt retrieval from 1Panel OpenAPI
	nodeMetrics, err1 := h.client.GetNodeMetrics(ctx)
	osInfo, err2 := h.client.GetOSInfo(ctx)

	if err1 == nil && err2 == nil {
		// Synthesize disk pool watermark from 1Panel OS/Node metrics
		diskTotalGB := float64(osInfo.DiskSize) / 1024 / 1024 / 1024
		hostStatus := h.fallback.GetHostStatus()

		diskUsedGB := hostStatus.DiskPool.UsedGB
		diskPct := hostStatus.DiskPool.UsedPercent
		watermark := hostStatus.DiskPool.Watermark
		if diskTotalGB > 0 && diskUsedGB > 0 {
			diskPct = roundFloat((diskUsedGB/diskTotalGB)*100, 1)
			if diskPct >= 90.0 {
				watermark = WatermarkCritical
			} else if diskPct >= 80.0 {
				watermark = WatermarkWarning
			} else {
				watermark = WatermarkNormal
			}
		}

		status := NASStatus{
			CPU: CPUMetrics{
				Percent:     roundFloat(nodeMetrics.CPUUsedPercent, 1),
				Cores:       nodeMetrics.CPUTotal,
				Model:       nodeMetrics.CPUModel,
				LoadAverage: []float64{nodeMetrics.Load1, nodeMetrics.Load5, nodeMetrics.Load15},
			},
			Memory: MemoryMetrics{
				TotalMB:     nodeMetrics.MemoryTotal / 1024 / 1024,
				UsedMB:      nodeMetrics.MemoryUsed / 1024 / 1024,
				FreeMB:      nodeMetrics.MemoryAvailable / 1024 / 1024,
				UsedPercent: roundFloat(nodeMetrics.MemoryUsedPercent, 1),
			},
			DiskPool: DiskPoolMetrics{
				Path:        hostStatus.DiskPool.Path,
				TotalGB:     roundFloat(diskTotalGB, 2),
				UsedGB:      diskUsedGB,
				FreeGB:      roundFloat(diskTotalGB-diskUsedGB, 2),
				UsedPercent: diskPct,
				Watermark:   watermark,
			},
			OS: OSInfo{
				Platform:     osInfo.Platform,
				Distro:       osInfo.PrettyDistro,
				Kernel:       osInfo.KernelVersion,
				KernelArch:   osInfo.KernelArch,
				UptimeSec:    nodeMetrics.Uptime,
				Hostname:     osInfo.Hostname,
				PanelVersion: "v2.4.0 (Enterprise)",
			},
			Source:     "1panel",
			Reachable:  true,
			SampledAt:  time.Now(),
			StatusText: "1Panel OpenAPI connected",
		}
		utils.Success(c, status)
		return
	}

	// 2. Resilient host fallback
	log.Printf("[ops] 1Panel query failed (metrics: %v, os: %v), falling back to host metrics", err1, err2)
	fallbackStatus := h.fallback.GetHostStatus()
	utils.Success(c, fallbackStatus)
}

// ListNASContainers returns core container states and resource consumption.
//
//	Route: GET /api/v1/ops/nas/containers
func (h *OpsHandler) ListNASContainers(c *gin.Context) {
	ctx, cancel := contextWithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	// 1. Try 1Panel container search
	containers, err := h.client.ListContainers(ctx)
	if err == nil && len(containers) > 0 {
		utils.Success(c, containers)
		return
	}

	// 2. Resilient Docker socket / process fallback
	log.Printf("[ops] 1Panel container list failed (%v), falling back to host Docker inspection", err)
	fallbackContainers := h.fallback.GetHostContainers(ctx)
	utils.Success(c, fallbackContainers)
}

// RestartNASContainer restarts a specific container by name (Admin only, audit logged).
//
//	Route: POST /api/v1/ops/nas/containers/:name/restart
func (h *OpsHandler) RestartNASContainer(c *gin.Context) {
	// Security check 1: Require Admin privilege
	isAdmin := c.GetBool("is_admin")
	if !isAdmin {
		utils.Fail(c, http.StatusForbidden, "管理员权限不足，无法执行容器重启")
		return
	}

	rawName := c.Param("name")
	name := strings.TrimPrefix(strings.TrimSpace(rawName), "/")

	// Security check 2: Whitelist constraint
	if !AllowedCoreContainers[name] {
		utils.Fail(c, http.StatusBadRequest, fmt.Sprintf("容器 '%s' 不在受管安全重启白名单中", name))
		return
	}

	operator := getOperatorName(c)
	clientIP := c.GetString("real_client_ip")
	if clientIP == "" {
		clientIP = c.ClientIP()
	}

	ctx, cancel := contextWithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	// 1. Attempt restart via 1Panel OpenAPI
	err := h.client.RestartContainer(ctx, name)
	if err != nil {
		log.Printf("[ops] 1Panel restart failed for %s (%v), attempting fallback Docker restart", name, err)
		err = h.fallback.RestartContainerFallback(ctx, name)
	}

	if err != nil {
		h.recordAuditLog(name, operator, clientIP, false, err.Error())
		utils.Fail(c, http.StatusInternalServerError, fmt.Sprintf("容器重启失败: %v", err))
		return
	}

	// Record audit log for successful restart
	h.recordAuditLog(name, operator, clientIP, true, "OK")

	utils.Success(c, ContainerRestartResponse{
		Name:        name,
		Success:     true,
		Message:     fmt.Sprintf("容器 %s 已成功触发安全重启", name),
		Operator:    operator,
		RestartedAt: time.Now(),
	})
}

// GetSecuritySummary aggregates WAF blocked count and S3 backup status.
//
//	Route: GET /api/v1/ops/nas/security-summary
func (h *OpsHandler) GetSecuritySummary(c *gin.Context) {
	var todayBlockedCount int64 = 0
	var totalHits int64 = 0
	var activeBanned int = 0
	var lastBlockedAt *time.Time

	// Read defense overview if configured
	if h.defenseMgr != nil {
		overview := h.defenseMgr.GetOverview()
		todayBlockedCount = overview.BansToday
		totalHits = overview.TotalBlockedHits
		activeBanned = overview.TotalBannedCount
		lastBlockedAt = overview.LastEventAt
	}

	s3Status := h.fallback.GetS3BackupStatus()

	summary := SecuritySummary{
		TodayWAFBlockedCount: todayBlockedCount,
		TotalBlockedHits:     totalHits,
		ActiveBannedIPs:      activeBanned,
		LastBlockedAt:        lastBlockedAt,
		S3Backup:             s3Status,
		Source:               "integrated_defense",
		SampledAt:            time.Now(),
	}

	utils.Success(c, summary)
}

// recordAuditLog saves container restart audit event to database and EventBus.
func (h *OpsHandler) recordAuditLog(containerName, operator, clientIP string, success bool, detail string) {
	msg := fmt.Sprintf("管理员 %s 从 IP %s 安全重启核心容器 [%s]: 状态=%v", operator, clientIP, containerName, success)
	if !success {
		msg += fmt.Sprintf(" (原因: %s)", detail)
	}

	payloadMap := map[string]any{
		"action":         "container.restart",
		"container_name": containerName,
		"operator":       operator,
		"client_ip":      clientIP,
		"success":        success,
		"detail":         detail,
		"ts":             time.Now().Unix(),
	}
	payloadBytes, _ := json.Marshal(payloadMap)

	// Publish to EventBus
	if h.bus != nil {
		severity := eventbus.SeverityWarn
		if !success {
			severity = eventbus.SeverityCritical
		}
		h.bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicSystemLog,
			Source:   eventbus.SourceSystem,
			Severity: severity,
			Payload:  payloadBytes,
		})
	}

	// Persist to SQLite system_logs table
	if h.db != nil {
		entry := model.SystemLog{
			Ts:        time.Now().Unix(),
			EventType: "ops.container.restart",
			Level:     model.LevelWarning,
			Source:    eventbus.SourceSystem,
			Context:   containerName,
			Message:   msg,
			Payload:   string(payloadBytes),
		}
		if !success {
			entry.Level = model.LevelCritical
		}
		if err := h.db.Create(&entry).Error; err != nil {
			log.Printf("[ops] failed to persist restart audit log: %v", err)
		}
	}
}

func getOperatorName(c *gin.Context) string {
	if name := c.GetString("user_name"); name != "" {
		return name
	}
	if uid := c.GetUint("user_id"); uid > 0 {
		return fmt.Sprintf("用户#%d", uid)
	}
	return "管理员"
}

func contextWithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		return context.WithTimeout(context.Background(), timeout)
	}
	return context.WithTimeout(parent, timeout)
}

// OfflineSyncItem represents a single offline audit log event.
type OfflineSyncItem struct {
	EventUUID     string `json:"event_uuid"`
	ContainerName string `json:"container_name"`
	Action        string `json:"action"`
	ExitCode      int    `json:"exit_code"`
	Success       int    `json:"success"`
	ExecutedAt    string `json:"executed_at"`
}

// OfflineSyncRequest represents the batch of offline emergency audit events.
type OfflineSyncRequest struct {
	Events []OfflineSyncItem `json:"events"`
}

// SyncOfflineAuditLogs handles POST /api/v1/ops/audit/offline-sync.
// Atomically ingests buffered offline events with idempotency deduplication by EventUUID.
func (h *OpsHandler) SyncOfflineAuditLogs(c *gin.Context) {
	var req OfflineSyncRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid payload: "+err.Error())
		return
	}

	syncedCount := 0
	for _, ev := range req.Events {
		eventUUID := strings.TrimSpace(ev.EventUUID)
		if eventUUID == "" {
			continue
		}

		// Idempotency check: if event_uuid already exists, skip insertion
		var existing model.StructuredOpsAuditLog
		if err := h.db.Where("event_uuid = ?", eventUUID).First(&existing).Error; err == nil {
			continue
		}

		createdAt := time.Now()
		if ev.ExecutedAt != "" {
			if t, err := time.Parse(time.RFC3339, ev.ExecutedAt); err == nil {
				createdAt = t
			}
		}

		auditRow := model.StructuredOpsAuditLog{
			EventUUID:       eventUUID,
			HouseholdID:     1,
			RequestID:       c.GetString("request_id"),
			ActorType:       "offline_agent",
			ActorID:         0,
			ActorName:       "home-ops-agent",
			ClientIP:        c.ClientIP(),
			TargetType:      "container",
			TargetID:        ev.ContainerName,
			Action:          "container.emergency_restart",
			ExecutionResult: fmt.Sprintf(`{"success":%v,"exit_code":%d}`, ev.Success == 1, ev.ExitCode),
			IsOfflineAction: true,
			CreatedAt:       createdAt,
		}

		// Also mirror into existing system_logs for UI dashboard visibility
		sysLog := model.SystemLog{
			HouseholdID: 1,
			Ts:          createdAt.Unix(),
			EventType:   "ops.container.offline_restart",
			Level:       model.LevelWarning,
			Source:      "ops_agent_offline",
			Context:     ev.ContainerName,
			Message:     security.RedactSensitiveInfo(fmt.Sprintf("【离线应急自愈】容器 %s 在 Go Core 离线期间执行了受控紧急重启 (状态: %v)", ev.ContainerName, ev.Success == 1)),
			Payload:     fmt.Sprintf(`{"event_uuid":"%s","exit_code":%d}`, eventUUID, ev.ExitCode),
		}

		if err := h.db.Create(&auditRow).Error; err == nil {
			_ = h.db.Create(&sysLog)
			syncedCount++
		}
	}

	utils.Success(c, gin.H{
		"synced_count": syncedCount,
		"message":      fmt.Sprintf("成功同步 %d 条离线应急自愈审计日志", syncedCount),
	})
}
