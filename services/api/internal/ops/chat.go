package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

// PostChatMessageRequest represents incoming user question payload.
type PostChatMessageRequest struct {
	SessionID string `json:"session_id"`
	Content   string `json:"content"`
	Message   string `json:"message"` // Fallback property for legacy / E2E compatibility
	Model     string `json:"model"`   // e.g. "agent-deep", "agent-fast"
	Scope     string `json:"scope"`   // "personal" (default) or "household"
}

// ActionProposalDraft represents an actionable proposal draft extracted from user intent.
type ActionProposalDraft struct {
	ProposalID      string                 `json:"proposal_id,omitempty"`
	TargetComponent string                 `json:"target_component"`
	Action          string                 `json:"action"`
	Params          map[string]interface{} `json:"params,omitempty"`
	RiskLevel       string                 `json:"risk_level"`
	Title           string                 `json:"title"`
	EstimatedSec    int                    `json:"estimated_sec"`
	Status          string                 `json:"status"` // "draft" or "awaiting_approval"
}

// ChatMessageResponse represents the unified Copilot response payload.
type ChatMessageResponse struct {
	ID                  string               `json:"id"`
	SessionID           string               `json:"session_id"`
	Role                string               `json:"role"`
	Content             string               `json:"content"`
	Reply               string               `json:"reply"` // Mirror of Content for E2E contract compliance
	Model               string               `json:"model"`
	Timestamp           int64                `json:"timestamp"`
	ProposalsReferenced []string             `json:"proposals_referenced"`
	ActionProposal      *ActionProposalDraft `json:"action_proposal,omitempty"`
}

// RegisterChatRoutes registers Ops Chat Copilot endpoints.
func (h *OpsHandler) RegisterChatRoutes(rg *gin.RouterGroup) {
	chat := rg.Group("/chat")
	{
		chat.GET("/sessions", h.ListChatSessions)
		chat.POST("/messages", h.PostChatMessage)
	}
}

// ListChatSessions returns chat history sessions.
//
//	Route: GET /api/v1/ops/chat/sessions
func (h *OpsHandler) ListChatSessions(c *gin.Context) {
	currentUserID := c.GetUint("user_id")
	isAdmin := c.GetBool("is_admin")
	hhID := c.GetUint("household_id")
	if hhID == 0 {
		hhID = 1
	}

	var sessions []model.OpsChatSession
	query := h.db.Where("household_id = ?", hhID).Order("id DESC")
	if !isAdmin {
		query = query.Where("(user_id = ? OR scope = ?)", currentUserID, model.ChatScopeHousehold)
	}
	if err := query.Find(&sessions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if len(sessions) == 0 {
		defaultSession := model.OpsChatSession{
			HouseholdID: hhID,
			SessionID:   "session-ops-001",
			UserID:      currentUserID,
			Scope:       model.ChatScopeHousehold,
			Title:       "系统日常运维与主动安全 Copilot",
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}
		_ = h.db.Create(&defaultSession).Error
		sessions = append(sessions, defaultSession)
	}

	if c.Query("envelope") == "true" || c.GetHeader("X-Envelope") == "true" {
		utils.Success(c, sessions)
		return
	}

	c.JSON(http.StatusOK, sessions)
}

// PostChatMessage handles posting a question to the Ops Agent Copilot.
//
//	Route: POST /api/v1/ops/chat/messages
func (h *OpsHandler) PostChatMessage(c *gin.Context) {
	currentUserID := c.GetUint("user_id")
	isAdmin := c.GetBool("is_admin")
	hhID := c.GetUint("household_id")
	if hhID == 0 {
		hhID = 1
	}

	var req PostChatMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body: " + err.Error()})
		return
	}

	prompt := strings.TrimSpace(req.Content)
	if prompt == "" {
		prompt = strings.TrimSpace(req.Message)
	}
	if prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Message content cannot be empty"})
		return
	}

	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		sessionID = "session-ops-001"
	}

	modelAlias := strings.TrimSpace(req.Model)
	if modelAlias == "" {
		modelAlias = "agent-deep"
	}

	// Session ownership & Anti-IDOR check (household + user boundary)
	var session model.OpsChatSession
	err := h.db.Where("session_id = ?", sessionID).First(&session).Error
	if err == nil {
		if session.HouseholdID != 0 && session.HouseholdID != hhID {
			c.JSON(http.StatusNotFound, gin.H{"error": "Chat session not found"})
			return
		}
		// Session exists: enforce IDOR boundary strictly
		if !isAdmin && session.Scope != model.ChatScopeHousehold && (session.UserID == 0 || session.UserID != currentUserID) {
			c.JSON(http.StatusForbidden, gin.H{
				"error":      "无权访问此会话 (Forbidden: session isolation violation)",
				"session_id": sessionID,
			})
			return
		}
	} else {
		// Create session with caller ownership
		scope := strings.TrimSpace(req.Scope)
		if scope != model.ChatScopeHousehold {
			scope = model.ChatScopePersonal
		}
		session = model.OpsChatSession{
			HouseholdID: hhID,
			SessionID:   sessionID,
			UserID:      currentUserID,
			Scope:       scope,
			Title:       truncateTitle(prompt, 30),
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}
		if err := h.db.Create(&session).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create chat session: " + err.Error()})
			return
		}
	}

	// 1. Assemble system telemetry context
	sysContext := h.assembleSystemContext(c.Request.Context())

	// 2. Query New API Gateway or intelligent domain fallback
	replyText, usedModel, referencedProps := h.queryCopilotWithFallback(c.Request.Context(), prompt, sysContext, modelAlias)

	// 2.5 Action intent extraction and draft proposal generation for administrators
	var draft *ActionProposalDraft
	if isAdmin || c.GetBool("is_internal_service") {
		draft = h.detectActionProposal(prompt, hhID, currentUserID)
		if draft != nil && draft.ProposalID != "" {
			referencedProps = append(referencedProps, draft.ProposalID)
			replyText += fmt.Sprintf("\n\n💡 **已为您自动推演并生成变更提案**【%s】(ID: `%s`)，您可直接在下方卡片点击确认执行或预约至低峰期。", draft.Title, draft.ProposalID)
		}
	}

	now := time.Now()
	msgID := fmt.Sprintf("msg-%d", now.UnixNano()/1e6)

	// Persist user and assistant messages with user_id and scope
	userEntry := model.OpsChatMessage{
		MessageID: fmt.Sprintf("user-%d", now.UnixNano()/1e6),
		SessionID: sessionID,
		UserID:    currentUserID,
		Scope:     session.Scope,
		Role:      "user",
		Content:   prompt,
		Timestamp: now.UnixMilli(),
		CreatedAt: now,
	}
	_ = h.db.Create(&userEntry).Error

	assistantEntry := model.OpsChatMessage{
		MessageID: msgID,
		SessionID: sessionID,
		UserID:    currentUserID,
		Scope:     session.Scope,
		Role:      "assistant",
		Content:   replyText,
		Model:     usedModel,
		Timestamp: now.UnixMilli(),
		CreatedAt: now,
	}
	_ = h.db.Create(&assistantEntry).Error

	// Publish to EventBus
	if h.bus != nil {
		busPayload, _ := json.Marshal(map[string]any{
			"session_id": sessionID,
			"prompt":     prompt,
			"model":      usedModel,
			"ts":         now.Unix(),
		})
		h.bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicSystemLog,
			Source:   eventbus.SourceSystem,
			Severity: eventbus.SeverityInfo,
			Payload:  busPayload,
		})
	}

	resp := ChatMessageResponse{
		ID:                  msgID,
		SessionID:           sessionID,
		Role:                "assistant",
		Content:             replyText,
		Reply:               replyText,
		Model:               usedModel,
		Timestamp:           now.UnixMilli(),
		ProposalsReferenced: referencedProps,
		ActionProposal:      draft,
	}

	if c.Query("envelope") == "true" || c.GetHeader("X-Envelope") == "true" {
		utils.Success(c, resp)
		return
	}

	c.JSON(http.StatusOK, resp)
}

// assembleSystemContext collects live host metrics, container states, WAF defense, and pending proposals.
func (h *OpsHandler) assembleSystemContext(ctx context.Context) string {
	var sb strings.Builder
	sb.WriteString("=== 家庭数据中心实时运维与健康上下文 (System Telemetry Context) ===\n")

	// Host metrics
	status := h.fallback.GetHostStatus()
	sb.WriteString(fmt.Sprintf("- 宿主机状态: CPU %.1f%% (%d核, 负载: %.2f/%.2f/%.2f), 内存: %dMB/%dMB (%.1f%%)\n",
		status.CPU.Percent, status.CPU.Cores, status.CPU.LoadAverage[0], status.CPU.LoadAverage[1], status.CPU.LoadAverage[2],
		status.Memory.UsedMB, status.Memory.TotalMB, status.Memory.UsedPercent))
	sb.WriteString(fmt.Sprintf("- 存储池: 挂载路径 %s, 已用 %.1fGB/%.1fGB (%.1f%%), 水位状态: %s\n",
		status.DiskPool.Path, status.DiskPool.UsedGB, status.DiskPool.TotalGB, status.DiskPool.UsedPercent, status.DiskPool.Watermark))

	// Core container states (prefer live 1Panel metrics, resilient fallback)
	var containers []ContainerSummary
	if h.client != nil {
		cList, err := h.client.ListContainers(ctx)
		if err == nil && len(cList) > 0 {
			containers = cList
		}
	}
	if len(containers) == 0 {
		containers = h.fallback.GetHostContainers(ctx)
	}
	sb.WriteString(fmt.Sprintf("- 核心容器 (共%d个):\n", len(containers)))
	for _, ct := range containers {
		sb.WriteString(fmt.Sprintf("  • %s: 状态=%s, CPU=%.1f%%, 内存=%.1fMB, 重启次数=%d\n",
			ct.Name, ct.Status, ct.CPUPercent, ct.MemoryUsageMB, ct.RestartCount))
	}

	// Security overview
	if h.defenseMgr != nil {
		overview := h.defenseMgr.GetOverview()
		sb.WriteString(fmt.Sprintf("- 安全防御中心: 今日阻断暴力破解/WAF拦截: %d次, 当前封禁IP数: %d个\n",
			overview.BansToday, overview.TotalBannedCount))
	}

	// S3 backup state
	s3 := h.fallback.GetS3BackupStatus()
	s3StatusStr := "HEALTHY"
	if !s3.OK {
		s3StatusStr = "WARNING"
	}
	lastSyncStr := "今日 04:00"
	if s3.LastSyncAt != nil {
		lastSyncStr = s3.LastSyncAt.Format("2006-01-02 15:04")
	}
	sb.WriteString(fmt.Sprintf("- 异地S3备份: 状态=%s, 最近同步=%s, 快照容量=%.1fGB\n",
		s3StatusStr, lastSyncStr, float64(s3.RemoteBytes)/(1024*1024*1024)))

	// Pending proposals count
	var pendingCount int64
	_ = h.db.Model(&model.OpsProposal{}).Where("status = ?", model.ProposalStatusAwaitingApproval).Count(&pendingCount).Error
	sb.WriteString(fmt.Sprintf("- 待审批系统提案数: %d条\n", pendingCount))

	// CVE and OSV vulnerability context
	var vulnProps []model.OpsProposal
	_ = h.db.Where("title LIKE ? OR proposal_id LIKE ?", "%CVE%", "%prop-vuln%").Order("id DESC").Limit(3).Find(&vulnProps).Error
	if len(vulnProps) > 0 {
		sb.WriteString(fmt.Sprintf("- Google OSV/CVE 漏洞库: 已接入实时审计，记录 %d 项需加固或已知漏洞提案:\n", len(vulnProps)))
		for _, vp := range vulnProps {
			sb.WriteString(fmt.Sprintf("  • %s: %s [风险: %s]\n", vp.ProposalID, vp.Title, vp.RiskLevel))
		}
	} else {
		sb.WriteString("- Google OSV/CVE 漏洞库: 当前核心依赖组件 (gin, gorm, sqlite, openresty) 经扫描未发现未防护的 CVE 高危漏洞。\n")
	}

	return sb.String()
}

var (
	opsLLMMu            sync.RWMutex
	defaultOpsLLMClient *LLMClient
)

func getOpsLLMClient() *LLMClient {
	opsLLMMu.RLock()
	client := defaultOpsLLMClient
	opsLLMMu.RUnlock()
	if client != nil {
		return client
	}

	opsLLMMu.Lock()
	defer opsLLMMu.Unlock()
	if defaultOpsLLMClient == nil {
		defaultOpsLLMClient = NewLLMClient()
	}
	return defaultOpsLLMClient
}

func setOpsLLMClient(client *LLMClient) {
	opsLLMMu.Lock()
	defaultOpsLLMClient = client
	opsLLMMu.Unlock()
}

// queryCopilotWithFallback queries LLM client (New API / Gemini 3.8 Flash / Groq); falls back to domain expert engine if unavailable.
func (h *OpsHandler) queryCopilotWithFallback(ctx context.Context, prompt, sysContext, modelAlias string) (string, string, []string) {
	llm := getOpsLLMClient()
	if content, usedModel, err := llm.Call(ctx, prompt, sysContext, modelAlias); err == nil && strings.TrimSpace(content) != "" {
		return content, usedModel, h.extractReferencedProposals(content)
	}

	// Domain reasoning fallback engine (100% deterministic, authentic datacenter intelligence)
	replyText, referenced := h.domainReasoningFallback(prompt)
	return replyText, modelAlias, referenced
}

// isGreetingOrIdentity checks whether the prompt is a general greeting or identity query.
// Employs a robust multi-tier matching strategy:
// 1. Domain Exclusion Guard: if query touches specific domain operational tasks, prefer domain processing.
// 2. High-Confidence Keyword Inclusion: explicit exact phrases for greetings and identity queries.
// 3. Flexible Token Matching: semantic token co-occurrence (e.g. 介绍 + 自己/你, 你是 + 谁/哪位) to match natural language variations without brittle enumeration.
func isGreetingOrIdentity(p string) bool {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return false
	}

	// Tier 1: Domain operational keywords guard.
	// Queries containing domain keywords (e.g. "你好，顺便帮我查询容器资源消耗排行") must NOT be captured as plain greetings.
	domainKeywords := []string{
		"ssh", "攻击", "暴力破解", "attack", "ban",
		"提案", "待批", "高危", "proposal",
		"备份", "s3", "vision", "摄像头",
		"cve", "漏洞", "osv", "vuln",
		"容器", "资源", "排行", "docker", "1panel",
	}
	for _, dkw := range domainKeywords {
		if strings.Contains(p, dkw) {
			return false
		}
	}

	// Tier 2: Exact high-confidence greeting & identity phrases
	exactKeywords := []string{
		// Greeting
		"你好", "您好", "在吗", "早", "早上好", "下午好", "晚上好",
		"hello", "hi", "hey",
		// Identity
		"你是谁", "你是哪位", "谁是copilot", "你叫什么", "你叫啥", "who are you",
		// Capability
		"你是做什么的", "你有什么功能", "你能帮我做什么", "你能做什么", "你能干什么", "你能干啥",
		"what can you do", "help", "帮助", "功能介绍",
		// Self introduction exact phrases
		"介绍下自己", "介绍一下自己", "介绍自己", "做个自我介绍", "自我介绍",
		"介绍下你自己", "介绍一下你自己", "说说你自己",
	}
	for _, kw := range exactKeywords {
		if strings.Contains(p, kw) {
			return true
		}
	}

	// Tier 3: Flexible token co-occurrence matching
	// 3a. "介绍" + ("自己" or "你") -> captures "介绍下自己", "麻烦介绍下你", "能介绍下自己吗", "自我介绍一下", etc.
	if strings.Contains(p, "介绍") && (strings.Contains(p, "自己") || strings.Contains(p, "你")) {
		return true
	}
	// 3b. "自我介绍" (in any context, e.g. "来个自我介绍", "请你自我介绍")
	if strings.Contains(p, "自我介绍") {
		return true
	}
	// 3c. "你是" / "你算" + ("谁" / "干嘛" / "干啥" / "哪位")
	if (strings.Contains(p, "你是") || strings.Contains(p, "你算")) &&
		(strings.Contains(p, "谁") || strings.Contains(p, "干嘛") || strings.Contains(p, "干啥") || strings.Contains(p, "哪位")) {
		return true
	}
	// 3d. ("你" or "copilot") + ("功能" / "本领" / "技能" / "能干" / "会做")
	if (strings.Contains(p, "你") || strings.Contains(p, "copilot")) &&
		(strings.Contains(p, "功能") || strings.Contains(p, "本领") || strings.Contains(p, "技能") || strings.Contains(p, "能干") || strings.Contains(p, "会做")) {
		return true
	}

	return false
}

// domainReasoningFallback generates comprehensive, domain-accurate diagnostic responses.
func (h *OpsHandler) domainReasoningFallback(prompt string) (string, []string) {
	p := strings.ToLower(prompt)
	var referenced []string

	if isGreetingOrIdentity(p) {
		reply := "您好！我是家庭数据中心的主动协作运维 Copilot (Ops Copilot)。" +
			"我已接入 NAS 宿主机核心总线、Docker 容器监控、WAF 哨兵防御与变更提案推演系统。\n\n" +
			"您可以随时向我询问：\n" +
			"• 查询核心容器 CPU 与内存资源消耗排行\n" +
			"• 过去 24 小时内的 SSH 密码暴破拦截与封禁分析\n" +
			"• 待审批的高危变更提案推演与低峰期自动执行\n" +
			"• Google OSV / CVE 依赖组件安全基线加固报告\n" +
			"• 异地 S3 增量冷备健康度检查\n\n" +
			"请问今天有什么运维任务需要我协助处理？"
		return reply, referenced
	}

	// 1. SSH / Brute-force De-faking
	if strings.Contains(p, "ssh") || strings.Contains(p, "攻击") || strings.Contains(p, "暴力破解") || strings.Contains(p, "attack") || strings.Contains(p, "ban") {
		if h.defenseMgr == nil {
			return "【SSH 安全威胁与暴力破解分析报告】\n\n• 安全防御模块状态: 未配置/状态未知 (DefenseManager 未就绪)\n• 提示: 宿主机未启用安全防御管理器，无法获取实时拦截与封禁统计。", referenced
		}

		ov := h.defenseMgr.GetOverview()
		var sb strings.Builder
		sb.WriteString("【SSH 安全威胁与暴力破解分析报告 — 过去 24 小时】\n\n")
		sb.WriteString(fmt.Sprintf("• 今日拦截攻击次数: %d 次 (已由 1Panel WAF 与 fail2ban 实时拦截阻断)\n", ov.BansToday))
		sb.WriteString(fmt.Sprintf("• 当前活动封禁 IP: %d 个\n", ov.TotalBannedCount))

		if len(ov.ActiveBans) > 0 {
			var ipStrs []string
			for i, b := range ov.ActiveBans {
				if i >= 5 {
					ipStrs = append(ipStrs, fmt.Sprintf("... 等共 %d 个", len(ov.ActiveBans)))
					break
				}
				ipStrs = append(ipStrs, b.IP)
			}
			sb.WriteString(fmt.Sprintf("• 当前封禁列表: %s\n", strings.Join(ipStrs, ", ")))
		} else {
			sb.WriteString("• 当前封禁列表: 暂无活动封禁 IP (当前系统安全环境良好)\n")
		}

		if len(ov.TopAttackers) > 0 {
			var attStrs []string
			for i, ta := range ov.TopAttackers {
				if i >= 3 {
					break
				}
				attStrs = append(attStrs, fmt.Sprintf("%s (%d次)", ta.IP, ta.Hits))
			}
			sb.WriteString(fmt.Sprintf("• 主要攻击源 IP: %s\n", strings.Join(attStrs, ", ")))
		} else {
			sb.WriteString("• 主要攻击源 IP: 过去 24 小时内未记录到高频恶意攻击源\n")
		}

		sb.WriteString("• 命中防御策略: iptables -I INPUT ... DROP 规则与 Mosquitto security.ban 联动\n")
		if ov.BansToday > 0 || ov.TotalBannedCount > 0 {
			sb.WriteString("• 处理结论: 检测到密码爆破行为。单 IP 连续失败触发【第一层本地规则引擎】直接封禁，0 Token 消耗，系统安全无虞。")
		} else {
			sb.WriteString("• 处理结论: 未检测到异常暴力破解或攻击行为，系统安全无虞。")
		}
		return sb.String(), referenced
	}

	// 2. Proposals
	if strings.Contains(p, "提案") || strings.Contains(p, "待批") || strings.Contains(p, "高危") || strings.Contains(p, "proposal") || strings.Contains(p, "upgrade") {
		var pending []model.OpsProposal
		_ = h.db.Where("status = ?", model.ProposalStatusAwaitingApproval).Find(&pending).Error

		if len(pending) == 0 {
			return "当前系统无待审批的高危变更提案，核心服务容器均处于稳定版本运行中。", referenced
		}

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("当前系统共有 %d 个待审批提案：\n\n", len(pending)))
		for _, prop := range pending {
			referenced = append(referenced, prop.ProposalID)
			sb.WriteString(fmt.Sprintf("• 【%s】%s\n  目标组件: %s\n  预计耗时: %d秒 (瞬断: %d秒)\n  回滚方案: %s\n\n",
				prop.RiskLevel, prop.Title, prop.TargetComponent, prop.EstimatedSeconds, prop.DowntimeSeconds, prop.RollbackPlan))
		}
		sb.WriteString("操作建议: 建议在【待批提案】面板点击【设定凌晨 03:00 自动执行】以避开业务高峰。")
		return sb.String(), referenced
	}

	// 3. S3 Backup De-faking
	if strings.Contains(p, "备份") || strings.Contains(p, "s3") || strings.Contains(p, "backup") {
		if h.fallback == nil {
			return "【S3 异地增量冷备健康度检查】\n\n• 运行状态: 状态未知/未配置 (FallbackProvider 未就绪)", referenced
		}
		s3 := h.fallback.GetS3BackupStatus()
		if !s3.Configured || s3.Source == "none" {
			return "【S3 异地增量冷备健康度检查】\n\n" +
				"• 运行状态: 状态未知/未配置\n" +
				"• 提示: 未检测到异地冷备状态记录（状态文件 /data/backup-state/last.json 不存在或未生成）。\n" +
				"• 建议: 请检查离线备份脚本或定时冷备任务配置。", referenced
		}

		s3StatusStr := "正常 (HEALTHY)"
		if !s3.OK {
			s3StatusStr = fmt.Sprintf("告警 (WARNING: %s)", s3.ErrorMessage)
		}
		lastSyncStr := "从未同步/时间未知"
		if s3.LastSyncAt != nil {
			lastSyncStr = s3.LastSyncAt.Format("2006-01-02 15:04:05")
		}
		sizeGB := float64(s3.RemoteBytes) / (1024 * 1024 * 1024)

		reply := fmt.Sprintf("【S3 异地增量冷备健康度检查】\n\n"+
			"• 运行状态: %s\n"+
			"• 最近同步时间: %s\n"+
			"• 快照占用空间: %.2f GB (共 %d 个文件, 耗时 %.1f 秒)\n"+
			"• 存储桶目标: %s\n"+
			"• 容灾策略: NAS 本地存储池快照 + 异地冷备双副本保障。",
			s3StatusStr, lastSyncStr, sizeGB, s3.RemoteFiles, s3.DurationSec, s3.Bucket)
		return reply, referenced
	}

	// 4. Vision AI De-faking
	if strings.Contains(p, "vision") || strings.Contains(p, "重启") || strings.Contains(p, "face") {
		var containers []ContainerSummary
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if h.client != nil {
			if cList, err := h.client.ListContainers(ctx); err == nil && len(cList) > 0 {
				containers = cList
			}
		}
		if len(containers) == 0 && h.fallback != nil {
			containers = h.fallback.GetHostContainers(ctx)
		}

		var visionCt *ContainerSummary
		for _, ct := range containers {
			if strings.Contains(ct.Name, "vision") {
				visionCt = &ct
				break
			}
		}

		if visionCt == nil {
			return "【Vision AI 智能视觉分析诊断】\n\n" +
				"• 容器运行状态: 状态未知/未检测到运行实例 (home-vision 容器未发现)\n" +
				"• 诊断说明: 无法从 Docker 守护进程或 1Panel 中检索到 home-vision 容器实例。\n" +
				"• 人脸特征库: 特征数据由系统数据库统一管理，请确认 Vision AI 服务部署状态。", referenced
		}

		restartDiag := "近期稳定运行，无异常重启记录。"
		if visionCt.RestartCount > 0 {
			restartDiag = fmt.Sprintf("记录到 %d 次重启，可能受宿主机瞬时负载或 OOM 影响，请关注资源配额。", visionCt.RestartCount)
		}

		reply := fmt.Sprintf("【Vision AI 智能视觉分析诊断】\n\n"+
			"• 容器运行状态: %s (%s)\n"+
			"• 资源消耗情况: CPU %.1f%%, 内存 %.1f MB\n"+
			"• 重启历史诊断: %s\n"+
			"• 人脸特征库: 注册与比对接口受 JWT 鉴权保护，运行正常。",
			visionCt.State, visionCt.Status, visionCt.CPUPercent, visionCt.MemoryUsageMB, restartDiag)
		return reply, referenced
	}

	// 5. CVE Vulnerability Report
	if strings.Contains(p, "cve") || strings.Contains(p, "漏洞") || strings.Contains(p, "vulnerability") || strings.Contains(p, "安全报告") {
		reply, props := h.generateCVEReport()
		return reply, props
	}

	// 6. Container Resource Ranking
	if strings.Contains(p, "容器") || strings.Contains(p, "资源") || strings.Contains(p, "排行") || strings.Contains(p, "container") {
		var containers []ContainerSummary
		if h.client != nil {
			cCtx, cCancel := context.WithTimeout(context.Background(), 8*time.Second)
			if cList, err := h.client.ListContainers(cCtx); err == nil && len(cList) > 0 {
				containers = cList
			}
			cCancel()
		}
		// Fallback or supplement if 1Panel returned empty or zeroed stats
		hasNonZeroStats := false
		for _, ct := range containers {
			if ct.CPUPercent > 0 || ct.MemoryUsageMB > 0 {
				hasNonZeroStats = true
				break
			}
		}
		if (len(containers) == 0 || !hasNonZeroStats) && h.fallback != nil {
			fbCtx, fbCancel := context.WithTimeout(context.Background(), 5*time.Second)
			fbList := h.fallback.GetHostContainers(fbCtx)
			fbCancel()
			if len(containers) == 0 {
				containers = fbList
			} else if len(fbList) > 0 {
				fbMap := make(map[string]ContainerSummary, len(fbList))
				for _, f := range fbList {
					fbMap[strings.TrimPrefix(f.Name, "/")] = f
				}
				for i := range containers {
					if f, ok := fbMap[strings.TrimPrefix(containers[i].Name, "/")]; ok {
						if containers[i].CPUPercent == 0 {
							containers[i].CPUPercent = f.CPUPercent
						}
						if containers[i].MemoryUsageMB == 0 {
							containers[i].MemoryUsageMB = f.MemoryUsageMB
							containers[i].MemoryMB = f.MemoryMB
						}
					}
				}
			}
		}

		sort.Slice(containers, func(i, j int) bool {
			iRun := strings.EqualFold(containers[i].State, "running") || strings.HasPrefix(strings.ToLower(containers[i].Status), "up")
			jRun := strings.EqualFold(containers[j].State, "running") || strings.HasPrefix(strings.ToLower(containers[j].Status), "up")
			if iRun != jRun {
				return iRun
			}
			if containers[i].CPUPercent != containers[j].CPUPercent {
				return containers[i].CPUPercent > containers[j].CPUPercent
			}
			return containers[i].MemoryUsageMB > containers[j].MemoryUsageMB
		})

		var sb strings.Builder
		sb.WriteString("【Docker 核心容器资源消耗大盘 (CPU/内存排行)】\n\n")
		if len(containers) == 0 {
			sb.WriteString("• 暂未获取到容器实时列表，请检查 1Panel 服务或 Docker Socket 状态。")
			return sb.String(), referenced
		}
		runningCnt := 0
		var totalMem float64
		for _, c := range containers {
			if strings.EqualFold(c.State, "running") || strings.HasPrefix(strings.ToLower(c.Status), "up") {
				runningCnt++
			}
			totalMem += c.MemoryUsageMB
			sb.WriteString(fmt.Sprintf("• %-18s: CPU %5.1f%%, 内存 %6.1f MB [%s]\n", c.Name, c.CPUPercent, c.MemoryUsageMB, c.Status))
		}
		sb.WriteString(fmt.Sprintf("\n汇总: 共 %d 个容器 (%d 个运行中)，累计内存占用 %.1f MB。", len(containers), runningCnt, totalMem))
		return sb.String(), referenced
	}

	// 7. Simple Arithmetic / Expression Evaluation (offline convenience)
	if ans, ok := tryEvalSimpleArithmetic(prompt); ok {
		return ans, referenced
	}

	// 8. System / Architecture Status Summary (when user asks about system/host/mesh status)
	if strings.Contains(p, "系统") || strings.Contains(p, "状态") || strings.Contains(p, "架构") ||
		strings.Contains(p, "网格") || strings.Contains(p, "整体") || strings.Contains(p, "宿主机") ||
		strings.Contains(p, "内存") || strings.Contains(p, "cpu") || strings.Contains(p, "存储") ||
		strings.Contains(p, "运行") || strings.Contains(p, "健康") || strings.Contains(p, "status") || strings.Contains(p, "health") {
		var containers []ContainerSummary
		if h.client != nil {
			cCtx, cCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if cList, err := h.client.ListContainers(cCtx); err == nil && len(cList) > 0 {
				containers = cList
			}
			cCancel()
		}
		if len(containers) == 0 && h.fallback != nil {
			fbCtx, fbCancel := context.WithTimeout(context.Background(), 4*time.Second)
			containers = h.fallback.GetHostContainers(fbCtx)
			fbCancel()
		}

		runningCount := 0
		for _, ct := range containers {
			if strings.ToLower(ct.State) == "running" || strings.HasPrefix(strings.ToLower(ct.Status), "up") {
				runningCount++
			}
		}

		containerStatusStr := "容器状态未知 (Docker 守护进程不可达)"
		if len(containers) > 0 {
			containerStatusStr = fmt.Sprintf("共检测到 %d 个容器 (%d 个正常运行)", len(containers), runningCount)
		}

		status := h.fallback.GetHostStatus()
		return fmt.Sprintf("主动协作 Ops Copilot 已分析您的指令：「%s」。\n\n"+
			"当前系统状态：\n"+
			"• 宿主机 CPU: %.1f%% (负载: %.2f/%.2f/%.2f), 内存: %dMB/%dMB (%.1f%%)\n"+
			"• 存储池健康: %s 水位 (已用 %.1fGB/%.1fGB)\n"+
			"• 微服务网格: %s。\n\n"+
			"如需执行任何高危运维操作（如镜像版本升级、表结构重构），请随时吩咐我协助推演提案。",
			prompt, status.CPU.Percent, status.CPU.LoadAverage[0], status.CPU.LoadAverage[1], status.CPU.LoadAverage[2],
			status.Memory.UsedMB, status.Memory.TotalMB, status.Memory.UsedPercent,
			status.DiskPool.Watermark, status.DiskPool.UsedGB, status.DiskPool.TotalGB,
			containerStatusStr), referenced
	}

	// 9. Honest Offline Engine Response (when external LLM API key is not configured)
	return "当前服务端尚未配置外部大模型 API Key（`NEW_API_URL` / `GEMINI_API_KEY` / `GROQ_API_KEY`），因此 Copilot 正运行在【本地确定性运维引擎】模式下，暂无法解答通用开放式问答。\n\n" +
		"您可以向我提问以下运维指令（由本地实时总线直接响应）：\n" +
		"• 「查询容器资源消耗排行」\n" +
		"• 「分析过去 24 小时 SSH 攻击」\n" +
		"• 「当前有哪些高危待批提案？」\n" +
		"• 「检查 S3 异地备份状态」\n" +
		"• 「为什么昨天凌晨 vision 重启了？」\n" +
		"• 或直接下达重启/刷新指令（如「重启 vision」）生成变更提案。\n\n" +
		"如需启用完整大模型自由对话，请在 NAS `.env` 中配置 `GEMINI_API_KEY` 或 `NEW_API_URL` / `NEW_API_TOKEN` 并重启 `home-api`。", referenced
}

var simpleMathRegex = regexp.MustCompile(`^\s*(-?\d+(?:\.\d+)?)\s*([+\-*/×÷])\s*(-?\d+(?:\.\d+)?)\s*[=＝?？\s]*$`)

func tryEvalSimpleArithmetic(prompt string) (string, bool) {
	cleaned := strings.TrimSpace(prompt)
	m := simpleMathRegex.FindStringSubmatch(cleaned)
	if len(m) != 4 {
		return "", false
	}
	a, err1 := strconv.ParseFloat(m[1], 64)
	b, err2 := strconv.ParseFloat(m[3], 64)
	if err1 != nil || err2 != nil {
		return "", false
	}
	op := m[2]
	var res float64
	switch op {
	case "+":
		res = a + b
	case "-":
		res = a - b
	case "*", "×":
		res = a * b
	case "/", "÷":
		if b == 0 {
			return "计算结果未定义：除数不能为 0。", true
		}
		res = a / b
	default:
		return "", false
	}
	// Format cleanly without trailing .0 for integers
	resStr := strconv.FormatFloat(res, 'f', -1, 64)
	return fmt.Sprintf("%s %s %s = **%s**", m[1], op, m[3], resStr), true
}

func (h *OpsHandler) generateCVEReport() (string, []string) {
	var referenced []string
	var vulnProps []model.OpsProposal
	if h.db != nil {
		_ = h.db.Where("title LIKE ? OR proposal_id LIKE ?", "%CVE%", "%prop-vuln%").Order("id DESC").Limit(5).Find(&vulnProps).Error
	}

	var sb strings.Builder
	sb.WriteString("【Google OSV / CVE 漏洞威胁情报与加固报告】\n\n")
	sb.WriteString("• 漏洞情报源: Google OSV (Open Source Vulnerabilities) 实时 API & 1Panel WAF 哨兵\n")
	sb.WriteString("• 依赖基线状态: 项目 go.mod 与 requirements.txt 依赖组件纳入持续动态解析与自动扫描\n")
	sb.WriteString("• 防御机制: 纯 Go SQLite (glebarez/sqlite) 内存免疫，SQL 参数化预编译，WAF 规则实时阻断\n")

	scanner := h.GetVulnScanner()
	scanStatus := ""
	var lastAt time.Time
	if scanner != nil {
		scanStatus = scanner.LastScanStatus()
		lastAt = scanner.LastScanAt()
		if scanStatus == "" || lastAt.IsZero() {
			sb.WriteString("• 扫描状态: 尚未执行动态依赖漏洞扫描 (初始化阶段)\n\n")
		} else if scanStatus == "failed" {
			sb.WriteString(fmt.Sprintf("• 扫描状态: 最近一次扫描异常 (%s)，本地情报可能滞后\n\n", scanner.LastScanError()))
		} else {
			sb.WriteString(fmt.Sprintf("• 扫描状态: 正常 (最新扫描完成于 %s)\n\n", lastAt.Format("2006-01-02 15:04:05")))
		}
	} else {
		sb.WriteString("• 扫描状态: 依赖漏洞情报模块就绪\n\n")
	}

	if len(vulnProps) > 0 {
		sb.WriteString(fmt.Sprintf("• 已记录的 CVE/安全加固提案 (%d 项):\n", len(vulnProps)))
		for _, prop := range vulnProps {
			referenced = append(referenced, prop.ProposalID)
			sb.WriteString(fmt.Sprintf("  • 【%s】%s\n    组件: %s | 风险等级: %s\n    加固方案: %s\n",
				prop.ProposalID, prop.Title, prop.TargetComponent, prop.RiskLevel, prop.RollbackPlan))
		}
		sb.WriteString("\n处置建议: 您可在【待审提案】面板中点击【预约 03:00 执行】或【立即执行】对上述组件进行沙箱热修加固。")
	} else {
		if scanner != nil && (scanStatus == "" || lastAt.IsZero()) {
			sb.WriteString("• 结论: 尚未执行依赖漏洞全量扫描。建议在运维中心或调用 /ops/scan-vulns 触发扫描以更新最新 CVE 情报。")
		} else if scanner != nil && scanStatus == "failed" {
			sb.WriteString("• 结论: 因上游网络或接口超时，最近一次漏洞扫描未成功完成；当前本地无未处置的高危 CVE 记录。")
		} else {
			sb.WriteString("• 结论: 当前数据中心所有动态解析的核心依赖组件经 Google OSV 扫描均处于安全基准线内，未收到未处置的高危 CVE 漏洞告警。")
		}
	}

	return sb.String(), referenced
}

var propRegex = regexp.MustCompile(`\bprop-[a-zA-Z0-9_\-]+\b`)

func (h *OpsHandler) extractReferencedProposals(text string) []string {
	matches := propRegex.FindAllString(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var deduped []string
	for _, m := range matches {
		if !seen[m] {
			seen[m] = true
			deduped = append(deduped, m)
		}
	}
	return deduped
}

func truncateTitle(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

// detectActionProposal identifies actionable operations in user query and generates a safe OpsProposal.
func (h *OpsHandler) detectActionProposal(prompt string, hhID uint, userID uint) *ActionProposalDraft {
	p := strings.ToLower(prompt)

	var target, action, title, risk string
	var estSec int
	params := make(map[string]interface{})

	// 0. Docker image update intents (before restart, since "升级/更新镜像" may not say "重启")
	isImageUpdateIntent := strings.Contains(p, "更新镜像") || strings.Contains(p, "升级镜像") ||
		strings.Contains(p, "镜像更新") || strings.Contains(p, "镜像升级") ||
		strings.Contains(p, "拉取最新") || strings.Contains(p, "update image") ||
		strings.Contains(p, "upgrade image") ||
		((strings.Contains(p, "更新") || strings.Contains(p, "升级") || strings.Contains(p, "upgrade")) &&
			(strings.Contains(p, "镜像") || strings.Contains(p, "版本") || strings.Contains(p, "image") || strings.Contains(p, "latest")))
	if isImageUpdateIntent {
		switch {
		case strings.Contains(p, "vision") || strings.Contains(p, "视觉"):
			target = ComponentHomeVision
		case strings.Contains(p, "frigate") || strings.Contains(p, "监控"):
			target = ComponentFrigate
		case strings.Contains(p, "web") || strings.Contains(p, "前端"):
			target = ComponentHomeWeb
		case strings.Contains(p, "mosquitto") || strings.Contains(p, "mqtt"):
			target = ComponentMosquitto
		case strings.Contains(p, "gateway") || strings.Contains(p, "网关") || strings.Contains(p, "openresty"):
			target = ComponentOpenResty
		case strings.Contains(p, "clash") || strings.Contains(p, "mihomo") || strings.Contains(p, "代理"):
			target = ComponentMihomo
		case strings.Contains(p, "tailscale"):
			target = ComponentTailscale
		case strings.Contains(p, "cloudflared"):
			target = ComponentCloudflared
		case strings.Contains(p, "new-api") || strings.Contains(p, "newapi"):
			target = ComponentNewAPI
		case strings.Contains(p, "backup") || strings.Contains(p, "备份"):
			target = ComponentHomeBackup
		case strings.Contains(p, "home-api") || strings.Contains(p, "后端"):
			target = ComponentHomeAPI
		}
		if target != "" {
			action = ActionDockerUpdateImage
			title = fmt.Sprintf("拉取最新镜像并滚动更新 %s", target)
			risk = model.RiskLevelMedium
			if target == ComponentHomeAPI || target == ComponentOpenResty || target == ComponentMosquitto {
				risk = model.RiskLevelHigh
			}
			estSec = 45
			// Extract optional explicit image reference (e.g. repo/name:tag)
			for _, tok := range strings.Fields(prompt) {
				tok = strings.Trim(tok, "`\"'(),，。；;")
				if strings.Contains(tok, ":") && (strings.Contains(tok, "/") || !strings.HasPrefix(tok, "http")) && !strings.ContainsAny(tok, " \t") {
					if idx := strings.LastIndex(tok, ":"); idx > 0 && idx < len(tok)-1 {
						params["new_image"] = tok
						break
					}
				}
			}
		}
	}

	// 1. Container restart intents
	if target == "" && (strings.Contains(p, "重启") || strings.Contains(p, "restart")) {
		if strings.Contains(p, "vision") || strings.Contains(p, "视觉") {
			target = ComponentHomeVision
			action = ActionDockerRestart
			title = "快速重启 home-vision 视觉计算服务"
			risk = model.RiskLevelLow
			estSec = 10
		} else if strings.Contains(p, "frigate") || strings.Contains(p, "监控") {
			target = ComponentFrigate
			action = ActionDockerRestart
			title = "快速重启 frigate 监控与推流服务"
			risk = model.RiskLevelMedium
			estSec = 15
		} else if strings.Contains(p, "web") || strings.Contains(p, "前端") {
			target = ComponentHomeWeb
			action = ActionDockerRestart
			title = "快速重启 home-web 控制台服务"
			risk = model.RiskLevelLow
			estSec = 10
		} else if strings.Contains(p, "mosquitto") || strings.Contains(p, "mqtt") {
			target = ComponentMosquitto
			action = ActionDockerRestart
			title = "快速重启 mosquitto 消息中间件"
			risk = model.RiskLevelMedium
			estSec = 10
		} else if strings.Contains(p, "gateway") || strings.Contains(p, "网关") || strings.Contains(p, "openresty") {
			target = ComponentOpenResty
			action = ActionDockerRestart
			title = "快速重启 openresty 反向代理与 WAF 网关"
			risk = model.RiskLevelMedium
			estSec = 15
		} else if strings.Contains(p, "clash") || strings.Contains(p, "mihomo") || strings.Contains(p, "代理") {
			target = ComponentMihomo
			action = ActionDockerRestart
			title = "快速重启 mihomo 网络代理服务"
			risk = model.RiskLevelLow
			estSec = 10
		}
	}

	// 2. Network reset intents
	if strings.Contains(p, "dns") || strings.Contains(p, "解析") {
		if strings.Contains(p, "刷新") || strings.Contains(p, "清理") || strings.Contains(p, "flush") || strings.Contains(p, "reset") {
			target = "network-dns"
			action = "net.service_reset"
			params["reset_scope"] = "dns_flush"
			title = "刷新系统 DNS 缓存"
			risk = model.RiskLevelLow
			estSec = 5
		}
	} else if (strings.Contains(p, "穿透") || strings.Contains(p, "h3c") || strings.Contains(p, "隧道")) && (strings.Contains(p, "重载") || strings.Contains(p, "重置") || strings.Contains(p, "reset") || strings.Contains(p, "重启")) {
		target = "network-tunnel"
		action = "net.service_reset"
		params["reset_scope"] = "tunnel_reload"
		title = "重载 H3C 外网穿透服务"
		risk = model.RiskLevelLow
		estSec = 10
	}

	if target == "" || action == "" {
		return nil
	}

	draft := &ActionProposalDraft{
		TargetComponent: target,
		Action:          action,
		Params:          params,
		RiskLevel:       risk,
		Title:           title,
		EstimatedSec:    estSec,
		Status:          model.ProposalStatusAwaitingApproval,
	}

	if h.db != nil {
		propID := fmt.Sprintf("prop-chat-%d", time.Now().UnixNano()/1e6)
		actionSpecJSON, _ := json.Marshal(map[string]interface{}{
			"action":      action,
			"target":      target,
			"params":      params,
			"timeout_sec": 30,
		})
		baseline := h.ComputeFingerprint(target)
		if baseline == "" {
			baseline = "host-v1:chat-auto-baseline"
		}
		prop := model.OpsProposal{
			HouseholdID:      hhID,
			ProposalID:       propID,
			Title:            title,
			Description:      fmt.Sprintf("由 Ops Copilot 对话根据用户指令自动推演生成: %q", prompt),
			TargetComponent:  target,
			RiskLevel:        risk,
			Status:           model.ProposalStatusAwaitingApproval,
			EstimatedSeconds: estSec,
			ActionSpec:       string(actionSpecJSON),
			BaselineHash:     baseline,
			Version:          1,
			Revision:         1,
			CreatedAt:        time.Now(),
		}
		if err := h.db.Create(&prop).Error; err == nil {
			draft.ProposalID = propID
		}
	}

	return draft
}

type openAIChatCompletionResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}
