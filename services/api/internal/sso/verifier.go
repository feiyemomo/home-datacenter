package sso

import (
	"crypto/subtle"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// InternalVerifierHandler handles internal machine-to-machine ticket verification.
// Accessible strictly via internal Docker network with dedicated INTERNAL_SSO_SECRET.
type InternalVerifierHandler struct {
	broker *Broker
}

// NewInternalVerifierHandler creates an internal ticket verifier handler.
func NewInternalVerifierHandler(broker *Broker) *InternalVerifierHandler {
	return &InternalVerifierHandler{broker: broker}
}

func isTrustedInternalSubnet(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	// Docker default bridge 172.17.0.0/16 and home-net 172.18.0.0/16
	_, subnet1, _ := net.ParseCIDR("172.17.0.0/16")
	_, subnet2, _ := net.ParseCIDR("172.18.0.0/16")
	return subnet1.Contains(ip) || subnet2.Contains(ip)
}

// VerifyTicketInternal handles POST /api/v1/internal/sso/verify
func (h *InternalVerifierHandler) VerifyTicketInternal(c *gin.Context) {
	// 1. Fail-Secure Check on Missing / Weak Secret
	internalSecret := os.Getenv("INTERNAL_SSO_SECRET")
	if strings.TrimSpace(internalSecret) == "" || len(internalSecret) < 32 {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"valid": false,
			"error": "INTERNAL_SSO_SECRET is not configured or insufficient entropy (>= 32 chars required)",
		})
		return
	}

	// 2. Secret Verification with Constant-Time Comparison
	reqSecret := c.GetHeader("X-Internal-SSO-Secret")
	if subtle.ConstantTimeCompare([]byte(reqSecret), []byte(internalSecret)) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{
			"valid": false,
			"error": "unauthorized internal secret",
		})
		return
	}

	// 3. Network Isolation Verification
	if !isTrustedInternalSubnet(c.ClientIP()) {
		c.JSON(http.StatusForbidden, gin.H{
			"valid": false,
			"error": "forbidden: endpoint restricted to internal docker network",
		})
		return
	}

	var req struct {
		Ticket string `json:"ticket"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Ticket) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"valid": false, "error": "invalid request body: missing ticket"})
		return
	}

	ticket, err := h.broker.VerifyAndConsume(c.Request.Context(), strings.TrimSpace(req.Ticket), c.ClientIP())
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"valid": false,
			"error": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"valid":     true,
		"user_id":   ticket.UserID,
		"username":  ticket.Username,
		"is_admin":  true,
		"issued_at": ticket.CreatedAt.Unix(),
	})
}
