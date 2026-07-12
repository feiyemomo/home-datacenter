package network

import (
	"fmt"
	"time"
)

// ManagedConnection represents the result of a Connect() call.
// It tells the client which transport to use and provides the
// endpoint information needed to establish the connection.
type ManagedConnection struct {
	// ConnectionType is the selected transport: "ipv6_direct" | "p2p" | "relay".
	ConnectionType ConnectionStrategy `json:"connection_type"`

	// Status is "connected" when the endpoint is ready, "unavailable"
	// when the selected path cannot be used (all transports exhausted).
	Status string `json:"status"`

	// IPv6DirectURL is the server's IPv6 HTTP endpoint (empty if ipv6
	// direct is unavailable). e.g. "http://[2409:8a70:37af:7e30::81e]:8080"
	IPv6DirectURL string `json:"ipv6_direct_url,omitempty"`

	// P2PEndpoint is the server's UDP endpoint for hole punching
	// (empty if P2P is unavailable). e.g. "203.0.113.42:19800"
	P2PEndpoint string `json:"p2p_endpoint,omitempty"`

	// RelayEndpoint is the Cloudflare Tunnel fallback URL (always set).
	RelayEndpoint string `json:"relay_endpoint,omitempty"`

	// Latency is an estimated round-trip time in milliseconds. -1 means
	// "not measured yet" — the client should measure via its own probe.
	Latency int `json:"latency"`

	// Reason explains why this connection type was chosen (or why all
	// failed). Useful for debugging.
	Reason string `json:"reason"`

	// EstablishedAt is when Connect() was called.
	EstablishedAt time.Time `json:"established_at"`
}

// ConnectionManager provides a unified connection interface that
// abstracts IPv6 Direct, P2P UDP, and Relay (Cloudflare Tunnel)
// behind a single Connect() call.
//
// The manager wraps the existing NetworkService (capability detection),
// HolePuncher (server-side UDP hole punching), and PeerRegistry
// (P2P signaling) and selects the best transport for a given peer.
//
// Selection order (prefer lower latency):
//  1. IPv6 Direct   — both sides have public IPv6
//  2. P2P UDP       — server has cone NAT, peer registered an endpoint
//  3. Relay         — Cloudflare Tunnel (always available)
type ConnectionManager struct {
	netService  *Service
	holePuncher *HolePuncher
	peers       *PeerRegistry
	relayURL    string // e.g. "https://dashboard.feiyemomo.top"
}

// NewConnectionManager creates a connection manager backed by the
// existing network stack.
//
// relayURL is the public Cloudflare Tunnel endpoint, e.g.
// "https://dashboard.feiyemomo.top". Empty = relay-only.
func NewConnectionManager(
	netService *Service,
	holePuncher *HolePuncher,
	peers *PeerRegistry,
	relayURL string,
) *ConnectionManager {
	return &ConnectionManager{
		netService:  netService,
		holePuncher: holePuncher,
		peers:       peers,
		relayURL:    relayURL,
	}
}

// Connect evaluates the server's current network posture and returns
// the best connection path for a client.
//
// If peerID is non-empty, the manager also checks whether the peer has
// registered a P2P endpoint. For IPv6 Direct and Relay, the peerID is
// not required — those transports work without per-peer registration.
//
// The returned ManagedConnection.Status is always "connected" when at
// least one transport is available (relay is always present). The
// client should probe the selected path and switch to the fallbacks if
// it times out.
func (m *ConnectionManager) Connect(peerID string) ManagedConnection {
	now := time.Now()
	status := m.netService.Status()

	// Build the result with defaults from the current network state.
	result := ManagedConnection{
		RelayEndpoint: m.relayURL,
		Latency:       -1, // client measures
		EstablishedAt: now,
	}

	// 1. IPv6 Direct: best latency, no intermediary, but requires both
	//    server and client to have public IPv6.
	if status.IPv6.Reachable && status.DirectURL != "" {
		result.ConnectionType = StrategyIPv6Direct
		result.Status = "connected"
		result.IPv6DirectURL = status.DirectURL
		result.Reason = "IPv6 direct: both server and client have public IPv6"
		return result
	}

	// 2. P2P UDP: works through cone NAT, requires the server to have
	//    its hole-punching socket open and the peer to register.
	if m.holePuncher != nil && status.P2P.Supported {
		if peerID != "" {
			// Check if the peer has already registered a P2P endpoint.
			if peer := m.peers.Lookup(peerID); peer != nil {
				result.ConnectionType = StrategyP2P
				result.Status = "connected"
				result.P2PEndpoint = fmt.Sprintf("%s:%d", status.P2PEndpoint, 0)
				// The real port is the holepunch socket port; get it
				// from the status if the port was parsed correctly.
				if status.P2PEndpoint != "" {
					result.P2PEndpoint = status.P2PEndpoint
				}
				result.Reason = fmt.Sprintf("P2P: peer %s registered at %s:%d",
					peerID, peer.PublicIP, peer.PublicPort)
				return result
			}
			// Peer not registered yet — still recommend P2P but note
			// that registration is needed.
			result.ConnectionType = StrategyP2P
			result.Status = "connecting"
			result.P2PEndpoint = status.P2PEndpoint
			result.Reason = fmt.Sprintf("P2P: peer %s must register first", peerID)
			return result
		}
		// No peerID: recommend P2P but the client needs to register.
		result.ConnectionType = StrategyP2P
		result.Status = "connecting"
		result.P2PEndpoint = status.P2PEndpoint
		result.Reason = "P2P: register your endpoint at /api/v1/network/p2p/register"
		return result
	}

	// 3. Relay: always available via Cloudflare Tunnel.
	result.ConnectionType = StrategyRelay
	result.Status = "connected"
	result.Reason = fmt.Sprintf("Relay (Cloudflare Tunnel): %s", m.relayURL)
	return result
}
