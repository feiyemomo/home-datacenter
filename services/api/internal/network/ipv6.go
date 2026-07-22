package network

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// IPv6Status is the result of the IPv6 capability check.
type IPv6Status struct {
	// Enabled is true if the host has at least one non-loopback,
	// non-link-local IPv6 address on any interface.
	Enabled bool `json:"enabled"`

	// Reachable is true if the host can establish a TCP6 connection
	// to a known public IPv6 service (Cloudflare DNS). This proves
	// end-to-end IPv6 routing, not just local interface config.
	Reachable bool `json:"reachable"`

	// Address is the first global-scope IPv6 address found on the
	// host's interfaces. Empty if none. This is the address the
	// mobile app would connect to for IPv6 direct mode.
	Address string `json:"address,omitempty"`

	// CheckedAt is when the check was last run.
	CheckedAt time.Time `json:"checked_at"`
}

// CheckIPv6 tests IPv6 availability and public reachability.
//
// The check has two stages:
//  1. Scan net.Interfaces() for a global-scope IPv6 address (not ::1,
//     not fe80::). If found, IPv6 is "enabled" locally.
//  2. Attempt a TCP6 dial to one of several well-known IPv6 endpoints
//     with a 3s timeout. If any connects, IPv6 is "reachable" from
//     the public internet's perspective.
//
// Stage 2 is the meaningful test — having a local IPv6 address without
// routable connectivity is useless for direct connections.
//
// v1.6.22: replaced Cloudflare DNS (2606:4700:4700::1111) with a
// multi-target list including China-accessible endpoints. The
// Cloudflare IPv6 prefix is blocked by Chinese carriers (confirmed
// by ping6 100% loss + curl timeout), so the previous check always
// returned reachable=false even when the host had working IPv6 to
// domestic peers. AliDNS (2400:3200::1) and TUNA mirrors6 are both
// confirmed reachable from Chinese mobile IPv6 networks.
func CheckIPv6() IPv6Status {
	status := IPv6Status{CheckedAt: time.Now()}

	// v1.6.22: short-circuit — if NAS_IPV6_ADDRESS env var is set,
	// trust it as the authoritative IPv6 address. The home-api
	// container runs on the home-net docker bridge which has no IPv6
	// subnet enabled, so the container cannot perform outbound IPv6
	// probes itself. Instead, the operator sets NAS_IPV6_ADDRESS in
	// compose.yaml to the host's public IPv6 address (stable SLAAC
	// EUI-64 address; only the /64 prefix rotates on ISP DHCPv6-PD
	// renewal). When set, we report enabled+reachable=true with the
	// given address — this is consistent with the go2rtc config.yml
	// webrtc.candidates entry which advertises the same address for
	// WebRTC IPv6 direct mode.
	if envAddr := os.Getenv("NAS_IPV6_ADDRESS"); envAddr != "" {
		// Validate it parses as an IPv6 address.
		if ip := net.ParseIP(envAddr); ip != nil && ip.To4() == nil {
			status.Enabled = true
			status.Reachable = true
			status.Address = envAddr
			return status
		}
	}

	// Stage 1: scan interfaces for a global IPv6 address.
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP == nil {
				continue
			}
			ip := ipNet.IP
			if ip.To4() != nil {
				continue // IPv4
			}
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}
			// This is a global IPv6 address.
			status.Enabled = true
			status.Address = ip.String()
			break
		}
	}

	// Stage 2: test public reachability via TCP6 dial.
	// v1.6.22: try multiple China-accessible IPv6 endpoints. The
	// Cloudflare DNS anycast (2606:4700:4700::1111) is blocked by
	// Chinese carriers — using it as the only target caused every
	// CheckIPv6() call to return reachable=false on Chinese networks.
	// AliDNS and TUNA mirrors6 are both operated on Chinese IPv6
	// backbones and are reliable liveness targets.
	targets := []string{
		"[2400:3200::1]:443",         // AliDNS (Alibaba public DNS)
		"[2402:f000:1:816::10]:443",  // TUNA mirrors6 (Tsinghua)
		"[2606:4700:4700::1111]:443", // Cloudflare DNS (fallback for non-China networks)
	}
	dialer := net.Dialer{Timeout: 3 * time.Second}
	for _, target := range targets {
		conn, err := dialer.Dial("tcp6", target)
		if err == nil {
			conn.Close()
			status.Reachable = true
			// If we didn't find a global address on the interfaces but
			// can still dial out via IPv6, the container is likely using
			// host networking or an IPv6-enabled bridge. Mark enabled.
			status.Enabled = true
			break
		}
	}

	return status
}

// LocalIPv6Address returns the first global-scope IPv6 address on any
// interface, or "" if none. This is exposed separately for callers that
// only need the address without running the full reachability check.
func LocalIPv6Address() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP == nil {
			continue
		}
		ip := ipNet.IP
		if ip.To4() != nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		return ip.String()
	}
	return ""
}

// IPv6ReachableURL returns the direct IPv6 URL for the given port,
// or "" if no global IPv6 address is available.
func IPv6ReachableURL(port int) string {
	ip := LocalIPv6Address()
	if ip == "" {
		return ""
	}
	return fmt.Sprintf("http://[%s]:%d", ip, port)
}

// OutboundIPv6Address returns the current outbound IPv6 address of the
// host by querying an external echo service (ident.me, with api64.ipify.org
// as fallback). ident.me responds with the raw IP as plain text.
//
// If the NAS_IPV6_ADDRESS environment variable is set, it is returned
// directly without any network probe. This short-circuit mirrors
// CheckIPv6(): the home-api container runs on a docker bridge without
// an IPv6 subnet, so an in-container HTTP probe to ident.me would
// always time out. The env var is the authoritative source in the
// deployment and is the path that actually takes effect.
//
// 3s timeout per request. Returns "" on failure — never panics, never
// blocks the caller.
func OutboundIPv6Address() string {
	if envAddr := os.Getenv("NAS_IPV6_ADDRESS"); envAddr != "" {
		if ip := net.ParseIP(envAddr); ip != nil && ip.To4() == nil {
			return envAddr
		}
	}

	client := &http.Client{Timeout: 3 * time.Second}
	for _, url := range []string{"https://ident.me", "https://api64.ipify.org"} {
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		if err != nil {
			continue
		}
		addr := strings.TrimSpace(string(body))
		if ip := net.ParseIP(addr); ip != nil && ip.To4() == nil {
			return addr
		}
	}
	return ""
}

// IPv6PrefixMatches reports whether two IPv6 addresses share the same
// /64 prefix. This is used by the PrefixWatcher to detect ISP DHCPv6-PD
// prefix rotations: the host's interface ID (lower 64 bits) stays
// stable across rotations, but the network prefix (upper 64 bits)
// changes.
//
// Returns false if either address fails to parse as IPv6.
func IPv6PrefixMatches(addr1, addr2 string) bool {
	ip1 := net.ParseIP(addr1)
	ip2 := net.ParseIP(addr2)
	if ip1 == nil || ip2 == nil {
		return false
	}
	b1 := ip1.To16()
	b2 := ip2.To16()
	if b1 == nil || b2 == nil {
		return false
	}
	// Compare the first 8 bytes (64 bits) — the /64 network prefix.
	for i := 0; i < 8; i++ {
		if b1[i] != b2[i] {
			return false
		}
	}
	return true
}

// OutboundIPv6Status is the cached result of the outbound IPv6 address
// probe. The PrefixWatcher populates this on each check cycle and the
// /api/v1/network/ipv6 endpoint serves it to the mobile app.
type OutboundIPv6Status struct {
	OutboundAddress   string    `json:"outbound_address"`
	ConfiguredAddress string    `json:"configured_address"`
	PrefixRotated     bool      `json:"prefix_rotated"`
	LastChecked       time.Time `json:"last_checked"`
}
