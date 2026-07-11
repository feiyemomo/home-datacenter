package network

import (
	"fmt"
	"net"
	"time"
)

// NATType classifies the NAT behaviour between the host and the public
// internet. This determines whether P2P UDP hole punching is feasible.
type NATType string

const (
	// NATCone: the NAT maps the same internal (ip, port) to the same
	// public port regardless of destination. Any external host can send
	// UDP to the mapped port. P2P hole punching works reliably.
	NATCone NATType = "cone"

	// NATSymmetric: the NAT assigns a different public port for each
	// destination. The port discovered via STUN is useless to the peer
	// because the NAT will reject packets from a different destination
	// on that port. P2P hole punching typically fails without a TURN
	// relay.
	NATSymmetric NATType = "symmetric"

	// NATUnknown: the check could not determine the NAT type — either
	// STUN is blocked, or fewer than 2 STUN servers responded.
	NATUnknown NATType = "unknown"
)

// NATStatus is the result of the NAT detection check.
type NATStatus struct {
	// Type is the detected NAT type (cone / symmetric / unknown).
	Type NATType `json:"type"`

	// PublicIP is the server-reflexive IPv4 address discovered via STUN.
	// Empty if STUN failed.
	PublicIP string `json:"public_ip,omitempty"`

	// PublicPort is the server-reflexive UDP port discovered via STUN.
	// 0 if STUN failed.
	PublicPort int `json:"public_port,omitempty"`

	// CheckedAt is when the check was last run.
	CheckedAt time.Time `json:"checked_at"`
}

// DetectNAT determines the NAT type by comparing the server-reflexive
// ports reported by two different STUN servers when queried from the
// same local UDP port.
//
// Algorithm (simplified RFC 5780):
//  1. Bind a fixed local UDP port.
//  2. Send a STUN Binding Request to server A → get (IP_A, Port_A).
//  3. Send a STUN Binding Request to server B → get (IP_B, Port_B).
//  4. If Port_A == Port_B → cone NAT (the NAT reuses the same mapping).
//  5. If Port_A != Port_B → symmetric NAT (different mapping per dest).
//
// If fewer than 2 STUN servers respond, we return NATUnknown — we can't
// distinguish cone from symmetric with a single server.
//
// The fixed local port is ephemeral — we let the OS assign one and reuse
// it for both queries. This is critical: if we used different sockets,
// the NAT would create new mappings and the port comparison would be
// meaningless.
func DetectNAT(servers []STUNServer) NATStatus {
	status := NATStatus{Type: NATUnknown, CheckedAt: time.Now()}

	if len(servers) == 0 {
		return status
	}

	// Bind a fixed local UDP port. We use ":0" to let the OS pick an
	// ephemeral port, then reuse the same *net.UDPAddr for both queries.
	// StunQuery re-binds the socket each time, but since we pass the
	// same localAddr, the OS reuses the same port (SO_REUSEADDR is
	// implicit in Go's ListenUDP when the socket is closed and reopened
	// quickly enough — and even if the port changes, the comparison is
	// still valid because both queries go through the same NAT mapping
	// for the same local endpoint).
	//
	// Actually, there's a subtlety: StunQuery opens a new socket each
	// time, so the local port might change between calls. To properly
	// test NAT behaviour, we need to use the SAME socket for both STUN
	// queries. We'll do that here directly.

	localAddr := &net.UDPAddr{IP: net.IPv4zero, Port: 0}
	conn, err := net.ListenUDP("udp", localAddr)
	if err != nil {
		return status
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(10 * time.Second))

	localPort := conn.LocalAddr().(*net.UDPAddr).Port

	// Query each STUN server from the same socket.
	var results []*RefAddr
	for _, srv := range servers {
		ref, err := stunQueryFromConn(conn, srv)
		if err != nil {
			continue
		}
		results = append(results, ref)
		if len(results) >= 2 {
			break // we only need 2 to compare
		}
	}

	if len(results) == 0 {
		// STUN is completely blocked. NAT type is unknown; P2P is
		// not possible without a relay.
		return status
	}

	// Record the first reflexive address as the "public" address.
	status.PublicIP = results[0].IP.String()
	status.PublicPort = results[0].Port

	if len(results) < 2 {
		// Only one STUN server responded. We can record the public
		// address but can't determine the NAT type.
		return status
	}

	// Compare ports from the two servers.
	if results[0].Port == results[1].Port {
		status.Type = NATCone
	} else {
		status.Type = NATSymmetric
	}

	// Also verify the IPs match (they should for any NAT type).
	if !results[0].IP.Equal(results[1].IP) {
		// Different public IPs from the same socket is unusual —
		// could be a dual-homed NAT or a CGNAT. Treat as symmetric
		// (worst case for P2P).
		status.Type = NATSymmetric
	}

	_ = localPort // used implicitly via conn; kept for debug logging
	return status
}

// stunQueryFromConn sends a STUN Binding Request using an existing UDP
// connection (so the same local port is reused for NAT detection).
func stunQueryFromConn(conn *net.UDPConn, server STUNServer) (*RefAddr, error) {
	serverAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", server.Host, server.Port))
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", server.Host, err)
	}

	var req [20]byte
	req[0], req[1] = 0x00, 0x01 // Binding Request
	req[2], req[3] = 0x00, 0x00 // Length = 0
	req[4], req[5], req[6], req[7] = 0x21, 0x12, 0xA4, 0x42 // Magic Cookie
	txnID := make([]byte, 12)
	if _, err := readRandom(txnID); err != nil {
		return nil, err
	}
	copy(req[8:], txnID)

	if _, err := conn.WriteToUDP(req[:], serverAddr); err != nil {
		return nil, fmt.Errorf("write to %s: %w", server.Host, err)
	}

	buf := make([]byte, 1500)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return nil, fmt.Errorf("read from %s: %w", server.Host, err)
	}

	return parseSTUNResponse(buf[:n], txnID)
}
