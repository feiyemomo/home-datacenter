package network

import (
	"fmt"
	"net"
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
//  2. Attempt a TCP6 dial to [2606:4700:4700::1111]:443 (Cloudflare
//     DNS-over-HTTPS) with a 3s timeout. If it connects, IPv6 is
//     "reachable" from the public internet's perspective.
//
// Stage 2 is the meaningful test — having a local IPv6 address without
// routable connectivity is useless for direct connections. The dial
// target is a well-known anycast address that is extremely unlikely to
// be down.
func CheckIPv6() IPv6Status {
	status := IPv6Status{CheckedAt: time.Now()}

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
	// We use Cloudflare's anycast DNS address (well-known, always up).
	// A 3s timeout keeps the check snappy even on slow links.
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.Dial("tcp6", "[2606:4700:4700::1111]:443")
	if err == nil {
		conn.Close()
		status.Reachable = true
		// If we didn't find a global address on the interfaces but
		// can still dial out via IPv6, the container is likely using
		// host networking or an IPv6-enabled bridge. Mark enabled.
		status.Enabled = true
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
