package handler

import (
	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/server"
	"home-datacenter-api/internal/utils"
)

// ServerHandler exposes the home server's identity and capability
// advertisement.
//
// Unlike most handlers in this codebase, ServerHandler's primary
// endpoint (GET /api/v1/server/info) is intentionally unauthenticated:
// a client needs to know the server's identity and capabilities
// before it can decide whether (and how) to authenticate. The
// public_key and server_id are not secrets — they exist to be
// advertised so clients can pin them against MITM.
type ServerHandler struct {
	identity *server.Identity
	caps     []server.Capability
}

// NewServerHandler creates a handler that serves the server's
// identity and capability list.
func NewServerHandler(identity *server.Identity, caps []server.Capability) *ServerHandler {
	return &ServerHandler{
		identity: identity,
		caps:     caps,
	}
}

// serverInfoResponse is the JSON shape returned by GET /server/info.
//
// The shape matches the spec in Phase 1:
//
//	{
//	  "server_id":    "550e8400-e29b-41d4-a716-446655440000",
//	  "name":         "Home Server",
//	  "version":      "1.0",
//	  "capabilities": ["ipv6", "p2p", "camera"]
//	}
//
// Plus public_key and created_at — the former lets future clients
// verify signed challenges, the latter is useful for display.
type serverInfoResponse struct {
	ServerID     string   `json:"server_id"`
	Name         string   `json:"name"`
	PublicKey    string   `json:"public_key"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
	CreatedAt    string   `json:"created_at"`
}

// Info returns the server's identity and advertised capabilities.
//
//	Route: GET /api/v1/server/info
//
// No auth required: this is the discovery endpoint a client hits
// before it has credentials. The public_key is not a secret; the
// private_key never leaves the process (Identity json:"-" tag).
func (h *ServerHandler) Info(c *gin.Context) {
	caps := make([]string, 0, len(h.caps))
	for _, cap := range h.caps {
		caps = append(caps, string(cap))
	}
	utils.Success(c, serverInfoResponse{
		ServerID:     h.identity.ServerID,
		Name:         h.identity.Name,
		PublicKey:    h.identity.PublicKey,
		Version:      h.identity.Version,
		Capabilities: caps,
		CreatedAt:    h.identity.CreatedAt.Format("2006-01-02 15:04:05"),
	})
}
