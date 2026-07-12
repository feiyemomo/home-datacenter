package handler

import (
	"net"
	"strconv"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/network"
	"home-datacenter-api/internal/utils"
)

// NetworkHandler exposes the network capability detection API.
type NetworkHandler struct {
	svc     *network.Service
	peers   *network.PeerRegistry
	puncher *network.HolePuncher // nil = P2P hole punching disabled
}

// NewNetworkHandler creates a handler for network status and P2P signaling.
// puncher may be nil if P2P hole punching is disabled (p2p_port = 0).
func NewNetworkHandler(svc *network.Service, peers *network.PeerRegistry, puncher *network.HolePuncher) *NetworkHandler {
	return &NetworkHandler{svc: svc, peers: peers, puncher: puncher}
}

// Probe is a lightweight connectivity endpoint. Clients fetch this via
// the server's IPv6 address to test if IPv6 direct is reachable.
// Returns 200 {"ok": true} — no auth required so the probe doesn't
// need a valid JWT (the client is just testing TCP connectivity).
//
//	Route: GET /api/v1/network/probe
func (h *NetworkHandler) Probe(c *gin.Context) {
	utils.Success(c, gin.H{"ok": true})
}

// Status returns the network capability report.
//
//	Route: GET /api/v1/network/status
//
// The response includes IPv6 availability, NAT type, P2P feasibility,
// relay status, and the recommended connection strategy.
//
// Pass ?refresh=true to force a fresh detection (skips the cache).
// This is useful when the network environment has changed (e.g. router
// rebooted, IPv6 newly enabled).
func (h *NetworkHandler) Status(c *gin.Context) {
	var status network.NetworkStatus
	if c.Query("refresh") == "true" {
		status = h.svc.Refresh()
	} else {
		status = h.svc.Status()
	}
	utils.Success(c, status)
}

// RegisterP2P registers the caller's public endpoint for P2P signaling.
//
//	Route: POST /api/v1/network/p2p/register
//
// The mobile app calls this after completing its own STUN discovery.
// The registered endpoint is then available for lookup by the server
// (or other peers) to initiate UDP hole punching.
//
// Body:
//
//	{
//	  "public_ip": "203.0.113.42",
//	  "public_port": 54321,
//	  "ipv6": "2001:db8::1"   // optional
//	}
//
// The peer_id is extracted from the JWT (device_id), not the body,
// to prevent impersonation.
func (h *NetworkHandler) RegisterP2P(c *gin.Context) {
	var req struct {
		PublicIP   string `json:"public_ip" binding:"required"`
		PublicPort int    `json:"public_port" binding:"required"`
		IPv6       string `json:"ipv6"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, 400, "invalid request: "+err.Error())
		return
	}

	// Validate the IP addresses.
	ip := net.ParseIP(req.PublicIP)
	if ip == nil {
		utils.Fail(c, 400, "invalid public_ip")
		return
	}
	if network.IsPrivateIP(ip) {
		utils.Fail(c, 400, "public_ip is a private/loopback address — expected a STUN-discovered public address")
		return
	}

	// Validate port range.
	if req.PublicPort < 1 || req.PublicPort > 65535 {
		utils.Fail(c, 400, "public_port must be between 1 and 65535")
		return
	}

	// Validate IPv6 if provided.
	if req.IPv6 != "" {
		ip6 := net.ParseIP(req.IPv6)
		if ip6 == nil || ip6.To4() != nil {
			utils.Fail(c, 400, "invalid ipv6 address")
			return
		}
	}

	// Extract peer ID from the JWT context (set by JWTAuth middleware).
	peerID := strconv.Itoa(c.GetInt("device_id"))
	if peerID == "0" {
		peerID = c.GetString("username")
	}

	peer := h.peers.Register(peerID, req.PublicIP, req.PublicPort, req.IPv6)

	// If the hole puncher is active, start sending hole-punching
	// packets to this peer's public endpoint. The peer should also
	// be sending packets to the server's endpoint (returned by
	// LookupServer) — when both sides are punching, the NAT holes
	// open and the UDP channel is established.
	if h.puncher != nil {
		peerAddr, err := net.ResolveUDPAddr("udp",
			net.JoinHostPort(req.PublicIP, strconv.Itoa(req.PublicPort)))
		if err == nil {
			h.puncher.StartPunching(peerID, peerAddr)
		}
	}

	utils.Success(c, gin.H{
		"peer_id":    peer.PeerID,
		"registered": true,
		"expires_at": peer.ExpiresAt,
		"p2p_active": h.puncher != nil,
	})
}

// LookupServer returns the server's own P2P endpoint (STUN-discovered
// public address). The mobile app uses this to know where to send UDP
// hole-punching packets.
//
// When the hole puncher is active, the endpoint comes from the hole
// puncher's persistent socket (NOT the NAT detection socket) — this is
// critical because the NAT mapping is per-socket. If we returned the
// NAT detection's endpoint, the peer would send packets to a port that
// the hole puncher isn't listening on.
//
//	Route: GET /api/v1/network/p2p/server-endpoint
func (h *NetworkHandler) LookupServer(c *gin.Context) {
	status := h.svc.Status()

	resp := gin.H{
		"ipv6":     status.IPv6.Address,
		"nat_type": status.NAT.Type,
		"strategy": status.Strategy,
	}

	// Prefer the hole puncher's endpoint (same socket as punching).
	if h.puncher != nil {
		if ep := h.puncher.PublicEndpoint(); ep != nil {
			resp["public_ip"] = ep.IP.String()
			resp["public_port"] = ep.Port
			resp["p2p_port"] = h.puncher.LocalPort()
			utils.Success(c, resp)
			return
		}
	}

	// Fall back to the NAT detection endpoint (different socket, may
	// not match the hole puncher's mapping — but better than nothing).
	resp["public_ip"] = status.NAT.PublicIP
	resp["public_port"] = status.NAT.PublicPort
	utils.Success(c, resp)
}

// ListSessions returns all active P2P hole-punching sessions.
// Admin-only.
//
//	Route: GET /api/v1/network/p2p/sessions
func (h *NetworkHandler) ListSessions(c *gin.Context) {
	if h.puncher == nil {
		utils.Success(c, gin.H{"sessions": []interface{}{}, "count": 0, "enabled": false})
		return
	}
	sessions := h.puncher.Sessions()
	utils.Success(c, gin.H{
		"sessions":   sessions,
		"count":      len(sessions),
		"enabled":    true,
		"local_port": h.puncher.LocalPort(),
	})
}

// LookupPeer returns a specific peer's registered endpoint.
//
//	Route: GET /api/v1/network/p2p/peers/:id
func (h *NetworkHandler) LookupPeer(c *gin.Context) {
	peerID := c.Param("id")
	peer := h.peers.Lookup(peerID)
	if peer == nil {
		utils.Fail(c, 404, "peer not found or registration expired")
		return
	}
	utils.Success(c, peer)
}

// ListPeers returns all registered peers. Admin-only.
//
//	Route: GET /api/v1/network/p2p/peers
func (h *NetworkHandler) ListPeers(c *gin.Context) {
	peers := h.peers.List()
	utils.Success(c, gin.H{
		"peers": peers,
		"count": len(peers),
	})
}

// UnregisterP2P removes the caller's endpoint from the registry.
//
//	Route: DELETE /api/v1/network/p2p/register
func (h *NetworkHandler) UnregisterP2P(c *gin.Context) {
	peerID := strconv.Itoa(c.GetInt("device_id"))
	if peerID == "0" {
		peerID = c.GetString("username")
	}
	h.peers.Unregister(peerID)
	if h.puncher != nil {
		h.puncher.StopPunching(peerID)
	}
	utils.Success(c, gin.H{"unregistered": true})
}
