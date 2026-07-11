package network

import (
	"net"
	"sync"
	"time"
)

// PeerEndpoint is a registered peer's public address, used for P2P
// UDP hole punching. The mobile app registers its STUN-discovered
// endpoint here, then looks up the server's endpoint to start
// punching.
type PeerEndpoint struct {
	// PeerID is the unique identifier for the peer. Typically the
	// device ID from the JWT.
	PeerID string `json:"peer_id"`

	// PublicIP is the peer's server-reflexive IPv4 address.
	PublicIP string `json:"public_ip"`

	// PublicPort is the peer's server-reflexive UDP port.
	PublicPort int `json:"public_port"`

	// IPv6 is the peer's public IPv6 address, if available.
	IPv6 string `json:"ipv6,omitempty"`

	// RegisteredAt is when the peer last registered.
	RegisteredAt time.Time `json:"registered_at"`

	// ExpiresAt is when the registration expires (auto-cleaned).
	ExpiresAt time.Time `json:"expires_at"`
}

// PeerRegistry is an in-memory registry of P2P peer endpoints. Peers
// register their STUN-discovered public addresses here so other peers
// (or the server itself) can look them up for UDP hole punching.
//
// The registry is in-memory and per-instance. Entries expire after
// 5 minutes if not refreshed — this matches the typical P2P session
// lifecycle and prevents stale endpoints from accumulating.
//
// This is the signaling layer only. The actual UDP hole punching is
// done by the peers themselves — the server just helps them discover
// each other's public endpoints.
type PeerRegistry struct {
	mu      sync.RWMutex
	peers   map[string]*PeerEndpoint
	ttl     time.Duration
}

// NewPeerRegistry creates a registry with a 5-minute default TTL.
func NewPeerRegistry() *PeerRegistry {
	return &PeerRegistry{
		peers: make(map[string]*PeerEndpoint),
		ttl:   5 * time.Minute,
	}
}

// Register adds or updates a peer's endpoint. The peer should call
// this periodically (before the TTL expires) to keep its endpoint alive.
func (r *PeerRegistry) Register(peerID, publicIP string, publicPort int, ipv6 string) *PeerEndpoint {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	peer := &PeerEndpoint{
		PeerID:      peerID,
		PublicIP:    publicIP,
		PublicPort:  publicPort,
		IPv6:        ipv6,
		RegisteredAt: now,
		ExpiresAt:   now.Add(r.ttl),
	}
	r.peers[peerID] = peer
	return peer
}

// Lookup returns a peer's endpoint by ID. Returns nil if the peer is
// not registered or the registration has expired.
func (r *PeerRegistry) Lookup(peerID string) *PeerEndpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()

	peer, ok := r.peers[peerID]
	if !ok {
		return nil
	}
	if time.Now().After(peer.ExpiresAt) {
		return nil
	}
	// Return a copy to prevent callers from mutating the registry's entry.
	return &PeerEndpoint{
		PeerID:       peer.PeerID,
		PublicIP:     peer.PublicIP,
		PublicPort:   peer.PublicPort,
		IPv6:         peer.IPv6,
		RegisteredAt: peer.RegisteredAt,
		ExpiresAt:    peer.ExpiresAt,
	}
}

// Unregister removes a peer from the registry (explicit logout).
func (r *PeerRegistry) Unregister(peerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.peers, peerID)
}

// List returns all non-expired peers. Used by the admin debug endpoint.
func (r *PeerRegistry) List() []*PeerEndpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now()
	var peers []*PeerEndpoint
	for _, peer := range r.peers {
		if now.Before(peer.ExpiresAt) {
			peers = append(peers, &PeerEndpoint{
				PeerID:       peer.PeerID,
				PublicIP:     peer.PublicIP,
				PublicPort:   peer.PublicPort,
				IPv6:         peer.IPv6,
				RegisteredAt: peer.RegisteredAt,
				ExpiresAt:    peer.ExpiresAt,
			})
		}
	}
	return peers
}

// Cleanup removes expired entries. Called periodically by the service.
func (r *PeerRegistry) Cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	for id, peer := range r.peers {
		if now.After(peer.ExpiresAt) {
			delete(r.peers, id)
		}
	}
}

// IsPrivateIP checks if an IP address is in a private/loopback/link-local
// range. Used to validate that the registered endpoint is a real public
// address (not a spoofed private address).
func IsPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// RFC 1918 private ranges
		switch {
		case ip4[0] == 10:
			return true
		case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
			return true
		case ip4[0] == 192 && ip4[1] == 168:
			return true
		case ip4[0] == 127:
			return true
		case ip4[0] == 169 && ip4[1] == 254:
			return true // link-local
		case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127:
			return true // CGNAT 100.64.0.0/10
		}
	}
	// IPv6 ULA: fc00::/7
	if len(ip) == 16 && (ip[0]&0xfe) == 0xfc {
		return true
	}
	return false
}
