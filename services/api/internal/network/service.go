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

	// DirectURL is the server's IPv6 HTTP endpoint (empty if ipv6
	// is not reachable). Used by connection_manager.go.
	DirectURL string `json:"direct_url,omitempty"`

	// P2PEndpoint is the server's UDP endpoint for hole punching
	// (empty if P2P is not available).
	P2PEndpoint string `json:"p2p_endpoint,omitempty"`

	// CheckedAt is the timestamp of the last detection run.
	CheckedAt time.Time `json:"checked_at"`
}

// Service is the network capability detection service. It caches
// results for a configurable TTL to avoid hammering public STUN servers
// on every API call.
type Service struct {
	servers []STUNServer
	ttl     time.Duration

	mu     sync.RWMutex
	cached *NetworkStatus
}

// NewService creates a network service with the given STUN servers and
// cache TTL. If servers is empty, DefaultSTUNServers are used.
func NewService(servers []STUNServer, ttl time.Duration) *Service {
	if len(servers) == 0 {
		servers = DefaultSTUNServers
	}
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &Service{servers: servers, ttl: ttl}
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

	// Build the direct URL and P2P endpoint for clients to use.
	var directURL string
	var p2pEndpoint string
	if ipv6Status.Reachable && ipv6Status.Address != "" {
		// The web container (nginx) is bound to port 8088 for both
		// IPv4 and IPv6 (compose.yaml dual-stack). Clients connect
		// to this port which reverse-proxies /api/ to home-api:8080.
		directURL = fmt.Sprintf("http://[%s]:8088/", ipv6Status.Address)
	}
	if p2p.Supported && natStatus.PublicIP != "" && natStatus.PublicPort > 0 {
		p2pEndpoint = fmt.Sprintf("%s:%d", natStatus.PublicIP, natStatus.PublicPort)
	}

	log.Printf("network: ipv6=%v/%v nat=%s p2p=%v initial=%s strategy=%s quality=%d/5 direct=%s",
		ipv6Status.Enabled, ipv6Status.Reachable,
		natStatus.Type, p2p.Supported, initial, strategy, quality, directURL)

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
		CheckedAt:   time.Now(),
	}
}
