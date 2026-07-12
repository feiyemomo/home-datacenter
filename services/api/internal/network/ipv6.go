package network

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// IPv6Status is the result of the IPv6 capability check.
type IPv6Status struct {
	// Enabled is true if the host has at least one non-loopback,
	// non-link-local IPv6 address on any interface, OR if outbound
	// IPv6 connectivity is proven via an external echo service.
	Enabled bool `json:"enabled"`

	// Reachable is true if the host can establish an outbound IPv6
	// connection to a public service AND receive a response. This
	// proves end-to-end IPv6 routing, not just local interface config.
	Reachable bool `json:"reachable"`

	// Address is the PUBLIC IPv6 address as seen from the internet.
	// This is discovered via an external IPv6 echo service, so it
	// works correctly even inside a Docker container (where the
	// container's local interface address is a private ULA, not the
	// host's public address). This is the address clients use for
	// IPv6 direct mode.
	Address string `json:"address,omitempty"`

	// CheckedAt is when the check was last run.
	CheckedAt time.Time `json:"checked_at"`
}

// IPv6-only echo services that return the caller's public IPv6 address
// as plain text. These domains have AAAA records but NO A records, so
// the HTTP client is forced to use IPv6. Listed in order of preference;
// the first one that responds wins.
//
// We list multiple because some may be blocked in certain regions
// (e.g. api6.ipify.org is sometimes unreachable from China).
var ipv6EchoServices = []string{
	"https://api6.ipify.org",
	"https://ipv6.icanhazip.com",
	"https://6.ipw.cn", // China-accessible IPv6 echo service
}	

// CheckIPv6 tests IPv6 availability and public reachability.
//
// The check has two stages:
//  1. HTTP GET to an IPv6-only echo service (e.g. api6.ipify.org).
//     If it responds, we know IPv6 is reachable AND we get the public
//     address as seen from the internet. This works inside Docker
//     containers where the local interface address is a private ULA.
//  2. Fallback: scan local interfaces for a global IPv6 address and
//     TCP6-dial Cloudflare DNS to test reachability without address
//     discovery.
//
// Stage 1 is preferred because it discovers the PUBLIC address (what
// clients actually need), not the container's internal address.
func CheckIPv6() IPv6Status {
	status := IPv6Status{CheckedAt: time.Now()}

	// Stage 1: query an external IPv6 echo service. This proves
	// outbound IPv6 connectivity AND discovers the public address.
	client := &http.Client{Timeout: 4 * time.Second}
	for _, url := range ipv6EchoServices {
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		if err != nil {
			continue
		}
		ipStr := strings.TrimSpace(string(body))
		// Validate that the response is actually an IPv6 address.
		ip := net.ParseIP(ipStr)
		if ip == nil || ip.To4() != nil || ip.To16() == nil {
			continue
		}
		status.Enabled = true
		status.Reachable = true
		status.Address = ipStr
		return status
	}

	// Stage 2 (fallback): the echo services are unreachable. This
	// could mean IPv6 is down, OR the echo services are blocked.
	// Fall back to local interface scan + TCP6 dial for a best-effort
	// answer.

	// Scan interfaces for a global IPv6 address.
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
			status.Enabled = true
			status.Address = ip.String()
			break
		}
	}

	// TCP6 dial to Cloudflare's anycast DNS — tests reachability
	// without address discovery.
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.Dial("tcp6", "[2606:4700:4700::1111]:443")
	if err == nil {
		conn.Close()
		status.Reachable = true
		status.Enabled = true
	}

	return status
}

// LocalIPv6Address returns the first global-scope IPv6 address on any
// interface, or "" if none. Note: in a Docker container this returns
// the container's internal address, NOT the host's public address.
// For the public address, use CheckIPv6().Address instead.
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
