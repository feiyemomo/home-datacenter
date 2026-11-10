package sso

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/ops"
	"home-datacenter-api/internal/utils"
)

// OnePanelSessionProvider abstracts 1Panel session acquisition for decoupling and testing.
type OnePanelSessionProvider interface {
	GetSession(ctx context.Context) (*ops.SessionCredentials, error)
}

// AdapterHandler handles ticket exchange and sets secure cookies for 1Panel authentication.
type AdapterHandler struct {
	broker         *Broker
	onePanelClient OnePanelSessionProvider
	secureCookie   bool
	allowedOrigins []string
}

// NewAdapterHandler creates a new AdapterHandler.
func NewAdapterHandler(broker *Broker, client OnePanelSessionProvider, secureCookie bool) *AdapterHandler {
	return &AdapterHandler{
		broker:         broker,
		onePanelClient: client,
		secureCookie:   secureCookie,
		allowedOrigins: []string{
			"https://dashboard.feiyemomo.top",
			"http://127.0.0.1",
			"http://localhost",
		},
	}
}

// SetAllowedOrigins customizes the allowed origins whitelist.
func (h *AdapterHandler) SetAllowedOrigins(origins []string) {
	h.allowedOrigins = origins
}

// ExchangeTicket handles Form POST or JSON exchange, validates ticket, acquires 1Panel session,
// sets HttpOnly/SameSite cookies, and issues HTTP 302 redirect.
func (h *AdapterHandler) ExchangeTicket(c *gin.Context) {
	// 1. Origin and Host Header Defense
	if !ValidateOrigin(c.Request, h.allowedOrigins) {
		utils.Fail(c, http.StatusForbidden, "跨站请求来源非法 (CORS/Origin Rejected)")
		return
	}

	// 2. Extract Ticket from Form Body or JSON (strictly avoiding query param)
	ticket := strings.TrimSpace(c.PostForm("ticket"))
	redirectURL := strings.TrimSpace(c.PostForm("redirect_url"))

	if ticket == "" && strings.Contains(c.GetHeader("Content-Type"), "application/json") {
		var req struct {
			Ticket      string `json:"ticket"`
			RedirectURL string `json:"redirect_url"`
		}
		if err := c.ShouldBindJSON(&req); err == nil {
			ticket = strings.TrimSpace(req.Ticket)
			if redirectURL == "" {
				redirectURL = strings.TrimSpace(req.RedirectURL)
			}
		}
	}

	if ticket == "" {
		utils.Fail(c, http.StatusBadRequest, "缺少一次性单点登录票据 (Missing SSO Ticket)")
		return
	}

	// 3. Atomically verify and consume the ticket
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	ticketRecord, err := h.broker.VerifyAndConsume(ctx, ticket, c.ClientIP())
	if err != nil {
		log.Printf("[sso-adapter] ticket verification failed for IP %s: %v", c.ClientIP(), err)
		utils.Fail(c, http.StatusUnauthorized, fmt.Sprintf("票据验证失败: %v", err))
		return
	}

	// 4. Contact 1Panel Daemon over internal Docker network to acquire genuine session
	if h.onePanelClient == nil {
		log.Printf("[sso-adapter] 1panel client not configured")
		utils.Fail(c, http.StatusBadGateway, "1Panel 后端客户端未配置")
		return
	}

	sessionCtx, sessionCancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer sessionCancel()

	creds, err := h.onePanelClient.GetSession(sessionCtx)
	if err != nil {
		log.Printf("[sso-adapter] 1panel session generation failed: %v", err)
		utils.Fail(c, http.StatusBadGateway, "1Panel 后端会话建立失败")
		return
	}

	// 5. Calculate Cookie Security Attributes
	isHTTPS := c.Request.TLS != nil ||
		c.GetHeader("X-Forwarded-Proto") == "https" ||
		h.secureCookie

	targetURL := ValidateRedirectTarget(redirectURL)

	// Set psession (HttpOnly=true, SameSite=Lax, Path=/)
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(
		"psession",
		creds.PSession,
		1800, // 30 minutes
		"/",
		"",
		isHTTPS,
		true, // HttpOnly: script cannot access session token
	)

	// Set pcsrftoken (HttpOnly=false, SameSite=Lax, Path=/)
	c.SetCookie(
		"pcsrftoken",
		creds.PCSRFToken,
		1800,
		"/",
		"",
		isHTTPS,
		false, // Non-HttpOnly: 1Panel frontend needs this for X-CSRF-Token header
	)

	log.Printf("[sso-adapter] successfully authenticated admin '%s' (User %d) -> redirecting to %s",
		ticketRecord.Username, ticketRecord.UserID, targetURL)

	// 6. HTTP 302 Found redirect
	c.Redirect(http.StatusFound, targetURL)
}
