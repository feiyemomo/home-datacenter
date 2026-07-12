package network

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// Default STUN servers used when none are configured. These are public,
// free, and have good uptime. We list 3 so NAT detection can compare
// reflexive ports across at least 2 servers even if one is down.
var DefaultSTUNServers = []STUNServer{
	{Host: "stun.l.google.com", Port: 19302},
	{Host: "stun.cloudflare.com", Port: 3478},
	{Host: "stun.miwifi.com", Port: 3478},
}

// P2PStatus describes whether P2P UDP communication is feasible.
type P2PStatus struct {
	// Supported is true if NAT type is cone (hole punching works) or
	// if IPv6 is reachable (no NAT traversal needed).
	Supported bool `json:"supported"`

	// Reason explains why P2P is or isn't supported.
	// e.g. "cone NAT — hole punching feasible"
	//      "symmetric NAT — relay required"
	//      "IPv6 direct available — P2P not needed"
	Reason string `json:"reason"`
}

// RelayStatus describes the relay fallback.
type RelayStatus struct {
	// Available is true if a relay path exists. Currently this checks
	// whether the server is reachable via its configured public URL
	// (Cloudflare Tunnel). We assume the tunnel is available if the
	// server is running behind nginx (which it always is in Docker).
	Available bool `json:"available"`

	// Type is the relay mechanism, e.g. "cloudflare_tunnel".
	Type string `json:"type"`
}

// ConnectionStrategy is the recommended connection method for clients.
type ConnectionStrategy string

const (
	// StrategyIPv6Direct: client connects directly to the server's
	// public IPv6 address. Best latency, no intermediary.
	StrategyIPv6Direct ConnectionStrategy = "ipv6_direct"

	// StrategyP2P: client uses UDP hole punching via STUN + signaling.
	// Good latency, no intermediary once established.
	StrategyP2P ConnectionStrategy = "p2p"

	// StrategyRelay: client falls back to the relay (Cloudflare Tunnel).
	// Works everywhere but adds latency.
	StrategyRelay ConnectionStrategy = "relay"
)

// NetworkStatus is the full capability report returned by the API.
type NetworkStatus struct {
	IPv6  IPv6Status  `json:"ipv6"`
	NAT   NATStatus   `json:"nat"`
	P2P   P2PStatus   `json:"p2p"`
	Relay RelayStatus `json:"relay"`

	// Initial is the recommended INITIAL connection method. Always
	// "relay" — the client connects via Cloudflare Tunnel immediately
	// (zero delay, always works), then probes the `strategy` path in
	// the background and upgrades if it becomes available. This avoids
	// the "try IPv6, wait for timeout, fall back" delay chain.
	Initial ConnectionStrategy `json:"initial"`

	// Strategy is the BEST achievable connection path after probing.
	// The client should upgrade from `initial` to this if the probe
	// succeeds. When strategy == initial (relay-only), no upgrade is
	// possible.
	Strategy ConnectionStrategy `json:"strategy"`

	// Quality is a 1-5 rating for the dashboard's star display.
	// 5 = IPv6 direct, 4 = P2P, 3 = relay, 1-2 = limited connectivity.
	Quality int `json:"quality"`

	// DirectURL is the IPv6 direct connection URL. The client probes
	// this URL to test if IPv6 direct is reachable. Empty when IPv6
	// is unavailable or direct_port is not configured.
	// e.g. "http://[2001:db8::1]:8080"
	DirectURL string `json:"direct_url,omitempty"`

	// P2PEndpoint is the server's UDP endpoint for hole punching.
	// Empty when P2P is disabled (p2p_port = 0). The mobile app sends
	// its hole-punching packets to this address.
	// e.g. "203.0.113.42:19800"
	P2PEndpoint string `json:"p2p_endpoint,omitempty"`

	// P2PSessions is the list of active hole-punching sessions.
	// Empty when P2P is disabled or no peers are punching.
	P2PSessions []*P2PSession `json:"p2p_sessions,omitempty"`

	// CheckedAt is the timestamp of the last detection run.
	CheckedAt time.Time `json:"checked_at"`
}

// Service is the network capability detection service. It caches
// results for a configurable TTL to avoid hammering public STUN servers
// on every API call.
type Service struct {
	servers     []STUNServer
	ttl         time.Duration
	directPort  int          // 0 = IPv6 direct disabled
	publicIPv6  string       // manual override; empty = auto-detect
	holePuncher *HolePuncher // nil = P2P hole punching disabled

	mu     sync.RWMutex
	cached *NetworkStatus
}

// NewService creates a network service with the given STUN servers and
// cache TTL. If servers is empty, DefaultSTUNServers are used.
// directPort is the TCP port for IPv6 direct (0 = disabled).
// publicIPv6 is a manual override for the server's public IPv6 address
// (empty = auto-detect via echo services; needed on Docker Desktop
// where container IPv6 NAT is unavailable).
// holePuncher is the optional P2P hole punching server (nil = disabled).
func NewService(servers []STUNServer, ttl time.Duration, directPort int, publicIPv6 string, holePuncher *HolePuncher) *Service {
	if len(servers) == 0 {
		servers = DefaultSTUNServers
	}
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &Service{
		servers:     servers,
		ttl:         ttl,
		directPort:  directPort,
		publicIPv6:  publicIPv6,
		holePuncher: holePuncher,
	}
}

// Status returns the cached network status, running a fresh detection
// if the cache is empty or expired. The detection involves UDP round-trips
// to public STUN servers (up to 10s total), so caching is essential.
func (s *Service) Status() NetworkStatus {
	s.mu.RLock()
	if s.cached != nil && time.Since(s.cached.CheckedAt) < s.ttl {
		status := *s.cached
		s.mu.RUnlock()
		return status
	}
	s.mu.RUnlock()

	// Run detection. This is best-effort — if STUN is blocked, we still
	// return a status with NAT type "unknown".
	status := s.detect()

	s.mu.Lock()
	s.cached = &status
	s.mu.Unlock()

	return status
}

// Refresh forces a fresh detection, ignoring the cache. Called when the
// client passes ?refresh=true.
func (s *Service) Refresh() NetworkStatus {
	status := s.detect()
	s.mu.Lock()
	s.cached = &status
	s.mu.Unlock()
	return status
}

// StartBackground runs periodic detection in a goroutine so the cache
// is always warm. This is optional — the first Status() call will
// detect on demand if the background loop isn't running.
func (s *Service) StartBackground(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(s.ttl)
		defer ticker.Stop()

		// Run once immediately so the cache is warm at startup.
		status := s.detect()
		s.mu.Lock()
		s.cached = &status
		s.mu.Unlock()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				status := s.detect()
				s.mu.Lock()
				s.cached = &status
				s.mu.Unlock()
			}
		}
	}()
}

// detect runs all checks and assembles the combined status.
func (s *Service) detect() NetworkStatus {
	// Run IPv6 and NAT detection in parallel — they're independent
	// and each can take a few seconds.
	ipv6Ch := make(chan IPv6Status, 1)
	natCh := make(chan NATStatus, 1)

	go func() {
		ipv6Ch <- CheckIPv6()
	}()
	go func() {
		natCh <- DetectNAT(s.servers)
	}()

	ipv6Status := <-ipv6Ch
	natStatus := <-natCh

	// Apply manual public_ipv6 override. This is needed on Docker
	// Desktop (Windows) where the container can't do outbound IPv6
	// (WSL2 has no IPv6 NAT), so auto-detection always fails. The
	// admin sets the host's public IPv6 address here; the client's
	// probe verifies actual reachability.
	if s.publicIPv6 != "" {
		ipv6Status.Enabled = true
		ipv6Status.Reachable = true
		ipv6Status.Address = s.publicIPv6
	}

	// Determine P2P feasibility.
	p2p := P2PStatus{}
	switch {
	case ipv6Status.Reachable:
		p2p.Supported = true
		p2p.Reason = "IPv6 direct available — P2P not needed but supported"
	case natStatus.Type == NATCone:
		p2p.Supported = true
		p2p.Reason = "cone NAT — UDP hole punching feasible"
	case natStatus.Type == NATSymmetric:
		p2p.Supported = false
		p2p.Reason = "symmetric NAT — relay required for P2P"
	default:
		p2p.Supported = false
		p2p.Reason = "STUN unreachable — cannot determine NAT type"
	}

	// Relay is always available via Cloudflare Tunnel (the server is
	// always behind nginx → Cloudflare Tunnel in production).
	relay := RelayStatus{
		Available: true,
		Type:      "cloudflare_tunnel",
	}

	// Initial strategy is always relay — connect immediately via
	// Cloudflare Tunnel, then probe for a better path in the background.
	initial := StrategyRelay

	// Strategy is the best achievable upgrade target. If IPv6 or P2P
	// is available, the client probes it after the initial relay
	// connection and upgrades if the probe succeeds.
	strategy := StrategyRelay
	switch {
	case ipv6Status.Reachable:
		strategy = StrategyIPv6Direct
	case p2p.Supported:
		strategy = StrategyP2P
	}

	// Compute quality rating (1-5) based on the best achievable path.
	quality := 1
	switch strategy {
	case StrategyIPv6Direct:
		quality = 5
	case StrategyP2P:
		quality = 4
	case StrategyRelay:
		quality = 3
	}

	// Compute the IPv6 direct URL — only when IPv6 is reachable AND
	// a direct_port is configured. The client probes this URL to test
	// if IPv6 direct is feasible.
	directURL := ""
	if ipv6Status.Reachable && s.directPort > 0 && ipv6Status.Address != "" {
		directURL = fmt.Sprintf("http://[%s]:%d", ipv6Status.Address, s.directPort)
	}

	// Compute the P2P endpoint — the server's public UDP address for
	// hole punching. This comes from the HolePuncher's STUN discovery
	// (which uses the same socket as the hole punching, ensuring NAT
	// mapping consistency).
	p2pEndpoint := ""
	var p2pSessions []*P2PSession
	if s.holePuncher != nil {
		if ep := s.holePuncher.PublicEndpoint(); ep != nil {
			p2pEndpoint = ep.String()
		}
		p2pSessions = s.holePuncher.Sessions()
	}

	log.Printf("network: ipv6=%v/%v nat=%s p2p=%v initial=%s strategy=%s quality=%d/5 direct=%s p2p_ep=%s sessions=%d",
		ipv6Status.Enabled, ipv6Status.Reachable,
		natStatus.Type, p2p.Supported, initial, strategy, quality,
		directURL, p2pEndpoint, len(p2pSessions))

	return NetworkStatus{
		IPv6:        ipv6Status,
		NAT:         natStatus,
		P2P:         p2p,
		Relay:       relay,
		Initial:     initial,
		Strategy:    strategy,
		Quality:     quality,
		DirectURL:   directURL,
		P2PEndpoint: p2pEndpoint,
		P2PSessions: p2pSessions,
		CheckedAt:   time.Now(),
	}
}
