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

	"home-datacenter-api/internal/audit"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/redact"
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
	auditEngine *audit.AgentAuditEngine

	mu                  sync.RWMutex
	newAPIFailCount     int
	newAPICooldownUntil time.Time
}

// SetAuditEngine configures the unified AgentAuditEngine on OpsHandler and its executor.
func (h *OpsHandler) SetAuditEngine(ae *audit.AgentAuditEngine) {
	h.mu.Lock()
	h.auditEngine = ae
	exec := h.executor
	h.mu.Unlock()
	if defExec, ok := exec.(*DefaultOperationExecutor); ok && defExec != nil {
		defExec.SetAuditEngine(ae)
	}
}

func (h *OpsHandler) getAuditEngine() *audit.AgentAuditEngine {
	h.mu.RLock()
	ae := h.auditEngine
	exec := h.executor
	h.mu.RUnlock()
	if ae != nil {
		return ae
	}
	if defExec, ok := exec.(*DefaultOperationExecutor); ok && defExec != nil {
		if exAE := defExec.GetAuditEngine(); exAE != nil {
			h.mu.Lock()
			if h.auditEngine == nil {
				h.auditEngine = exAE
			}
			ae = h.auditEngine
			h.mu.Unlock()
			return ae
		}
	}
	if h.db != nil {
		created := audit.NewAgentAuditEngine(h.db, h.bus, audit.WithFailureCallback(GlobalSecurityMetrics.IncAuditFailed))
		h.SetAuditEngine(created)
		return created
	}
	return nil
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
	if defExec, ok := h.executor.(*DefaultOperationExecutor); ok && defExec != nil {
		h.auditEngine = defExec.GetAuditEngine()
	}
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
	if ae := h.getAuditEngine(); ae != nil {
		resStr := "SUCCESS"
		errStr := ""
		if err != nil {
			resStr = "FAILED"
			errStr = err.Error()
		}
		hhID := c.GetUint("household_id")
		if hhID == 0 {
			hhID = 1
		}
		_, _ = ae.EmitEvent(c.Request.Context(), audit.EmitEventInput{
			HouseholdID:    hhID,
			TriggerSource:  "api_manual",
			ActorType:      "user",
			ActorID:        getOperatorName(c),
			EventType:      model.AuditEventExecutionCompleted,
			Action:         "onepanel.session.fetch",
			Target:         "1panel",
			Component:      "1panel",
			Decision:       model.AuditDecisionAllow,
			RiskLevel:      model.AuditRiskMedium,
			Result:         resStr,
			ErrorCode:      errStr,
			Evidence:       map[string]interface{}{"client_ip": c.ClientIP()},
			FinalRunStatus: model.AuditRunStatusCompleted,
		})
	}
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
	if !c.GetBool("is_admin") && !c.GetBool("is_internal_service") {
		utils.Fail(c, http.StatusForbidden, "管理员权限不足，禁止读取运维执行历史")
		return
	}
	hhID := c.GetUint("household_id")
	if hhID == 0 {
		hhID = 1
	}

	var histories []model.OpsExecutionHistory
	if h.db != nil {
		if err := h.db.Where("household_id = ?", hhID).Order("id DESC").Limit(50).Find(&histories).Error; err != nil {
			utils.Fail(c, http.StatusInternalServerError, err.Error())
			return
		}
	}

	for i := range histories {
		histories[i].HealthOutput = redact.SensitiveString(histories[i].HealthOutput)
		histories[i].Detail = redact.SensitiveString(histories[i].Detail)
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

// RestartNASContainer restarts a specific container through OperationExecutor (Admin only, leased & audited).
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

	// Security check 2: Unified whitelist & canonical resolution (no OperationExecutor bypass)
	canonical, resolveErr := ResolveComponent(name)
	if resolveErr != nil || !AllowedCoreContainers[name] {
		utils.Fail(c, http.StatusBadRequest, fmt.Sprintf("容器 '%s' 不在受管安全重启白名单中", name))
		return
	}

	operator := getOperatorName(c)
	clientIP := c.GetString("real_client_ip")
	if clientIP == "" {
		clientIP = c.ClientIP()
	}

	exec := h.GetExecutor()
	if exec == nil {
		h.recordAuditLog(name, operator, clientIP, false, "operation executor not configured")
		utils.Fail(c, http.StatusServiceUnavailable, "OperationExecutor 未配置，拒绝绕过安全执行器")
		return
	}

	ctx, cancel := contextWithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	if ae := h.getAuditEngine(); ae != nil {
		hhID := c.GetUint("household_id")
		if hhID == 0 {
			hhID = 1
		}
		_, runCtx, rErr := ae.StartRun(ctx, audit.StartRunInput{
			HouseholdID:   hhID,
			TriggerSource: "api_manual",
			ActorType:     "user",
			ActorID:       operator,
			Action:        ActionDockerRestart,
			Target:        canonical,
			Component:     canonical,
			Decision:      model.AuditDecisionAllow,
			RiskLevel:     model.AuditRiskHigh,
			Summary:       fmt.Sprintf("Manual admin container restart by %s (%s)", operator, clientIP),
		})
		if rErr != nil {
			h.recordAuditLog(name, operator, clientIP, false, rErr.Error())
			utils.Fail(c, http.StatusInternalServerError, fmt.Sprintf("审计落盘失败，Fail-Closed 阻断重启: %v", rErr))
			return
		}
		ctx = runCtx
	}

	res, err := exec.Execute(ctx, OperationSpec{
		Action: ActionDockerRestart,
		Target: canonical,
	})
	if err != nil || (res != nil && !res.Success) {
		detail := ""
		if err != nil {
			detail = err.Error()
		} else if res != nil {
			detail = res.Error
		}
		h.recordAuditLog(name, operator, clientIP, false, detail)
		utils.Fail(c, http.StatusInternalServerError, fmt.Sprintf("容器重启失败: %s", detail))
		return
	}

	// Record audit log for successful restart
	h.recordAuditLog(name, operator, clientIP, true, "OK")

	utils.Success(c, ContainerRestartResponse{
		Name:        name,
		Success:     true,
		Message:     fmt.Sprintf("容器 %s 已通过安全执行器完成重启与健康验证", name),
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
	safeDetail := redact.SensitiveString(detail)
	msg := redact.SensitiveString(fmt.Sprintf("管理员 %s 从 IP %s 安全重启核心容器 [%s]: 状态=%v", operator, clientIP, containerName, success))
	if !success {
		msg += fmt.Sprintf(" (原因: %s)", safeDetail)
	}

	payloadMap := map[string]any{
		"action":         "container.restart",
		"container_name": containerName,
		"operator":       operator,
		"client_ip":      clientIP,
		"success":        success,
		"detail":         safeDetail,
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

// OfflineSyncItem is preserved for type compatibility with tests/clients.
type OfflineSyncItem = audit.OfflineSyncEvent

// OfflineSyncRequest represents the batch of offline emergency audit events.
type OfflineSyncRequest struct {
	Events []audit.OfflineSyncEvent `json:"events"`
}

// SyncOfflineAuditLogs handles POST /api/v1/ops/audit/offline-sync.
// Atomically ingests buffered offline events with signature/hash verification,
// idempotent EventUUID deduplication, and explicit accepted_uuids[] acking.
func (h *OpsHandler) SyncOfflineAuditLogs(c *gin.Context) {
	if h.db == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "ops database unavailable for offline audit sync")
		return
	}
	var req OfflineSyncRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid payload: "+err.Error())
		return
	}

	ae := h.getAuditEngine()
	if ae == nil {
		utils.Fail(c, http.StatusServiceUnavailable, "audit engine unavailable")
		return
	}
	hhID := c.GetUint("household_id")
	if hhID == 0 {
		hhID = 1
	}

	res, err := ae.IngestOfflineEvents(c.Request.Context(), req.Events, c.GetString("request_id"), c.ClientIP(), hhID)
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, fmt.Sprintf("offline audit sync failed: %v", err))
		return
	}

	utils.Success(c, gin.H{
		"synced_count":   res.SyncedCount,
		"accepted_uuids": res.AcceptedUUIDs,
		"rejected":       res.Rejected,
		"anchor_seq":     res.AnchorSeq,
		"message":        fmt.Sprintf("成功同步 %d 条离线应急自愈审计日志", res.SyncedCount),
	})
}
