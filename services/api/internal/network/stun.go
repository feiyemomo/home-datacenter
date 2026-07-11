package network

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

// STUN protocol constants (RFC 5389).
const (
	stunMagicCookie   = 0x2112A442
	stunBindingReq    = 0x0001
	stunBindingResp   = 0x0101
	attrMappedAddr    = 0x0001
	attrXorMappedAddr = 0x0020
	stunTimeout       = 5 * time.Second
)

// RefAddr is the server-reflexive (public) address discovered via STUN.
type RefAddr struct {
	IP   net.IP
	Port int
}

func (r RefAddr) String() string {
	if r.IP == nil {
		return ""
	}
	return net.JoinHostPort(r.IP.String(), fmt.Sprintf("%d", r.Port))
}

// STUNServer represents a public STUN server endpoint.
type STUNServer struct {
	Host string // e.g. "stun.l.google.com"
	Port int    // e.g. 19302
}

// StunQuery sends a STUN Binding Request from the given local UDP address
// to the STUN server and returns the server-reflexive address (the public
// IP:port as seen by the STUN server).
//
// If localAddr is nil, the OS picks an ephemeral port. Callers that need
// to compare reflexive ports across multiple STUN servers MUST pass the
// same localAddr (a fixed *net.UDPAddr) so the same NAT mapping is reused.
func StunQuery(localAddr *net.UDPAddr, server STUNServer) (*RefAddr, error) {
	// Resolve the STUN server hostname.
	serverAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", server.Host, server.Port))
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", server.Host, err)
	}

	// Bind a UDP socket. If localAddr is nil, the OS assigns an ephemeral
	// port — this is the "one-shot" mode used by simple reachability tests.
	// When localAddr is a fixed address, the same socket/port is reused
	// across calls to detect NAT mapping consistency.
	conn, err := net.ListenUDP("udp", localAddr)
	if err != nil {
		return nil, fmt.Errorf("listen udp: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(stunTimeout))

	// Build the 20-byte Binding Request (no attributes).
	//   Type:           0x0001
	//   Length:         0x0000 (no attributes)
	//   Magic Cookie:   0x2112A442
	//   Transaction ID: 12 random bytes
	var req [20]byte
	binary.BigEndian.PutUint16(req[0:], stunBindingReq)
	binary.BigEndian.PutUint16(req[2:], 0) // length = 0
	binary.BigEndian.PutUint32(req[4:], stunMagicCookie)
	txnID := make([]byte, 12)
	if _, err := rand.Read(txnID); err != nil {
		return nil, fmt.Errorf("generate txn id: %w", err)
	}
	copy(req[8:], txnID)

	if _, err := conn.WriteToUDP(req[:], serverAddr); err != nil {
		return nil, fmt.Errorf("write to %s: %w", server.Host, err)
	}

	// Read the response. STUN responses are typically <100 bytes but can
	// be larger if the server includes SOFTWARE or other attributes. 1500
	// is the safe MTU-sized buffer.
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return nil, fmt.Errorf("read from %s: %w", server.Host, err)
	}
	buf = buf[:n]

	return parseSTUNResponse(buf, txnID)
}

// parseSTUNResponse validates the STUN response header and extracts the
// reflexive address from the first XOR-MAPPED-ADDRESS or MAPPED-ADDRESS
// attribute found.
func parseSTUNResponse(buf []byte, txnID []byte) (*RefAddr, error) {
	if len(buf) < 20 {
		return nil, fmt.Errorf("stun response too short: %d bytes", len(buf))
	}

	msgType := binary.BigEndian.Uint16(buf[0:2])
	msgLen := binary.BigEndian.Uint16(buf[2:4])
	cookie := binary.BigEndian.Uint32(buf[4:8])

	if msgType != stunBindingResp {
		return nil, fmt.Errorf("unexpected stun message type: 0x%04x", msgType)
	}
	if cookie != stunMagicCookie {
		return nil, fmt.Errorf("invalid magic cookie: 0x%08x", cookie)
	}
	if len(buf) < int(20+msgLen) {
		return nil, fmt.Errorf("stun response truncated: header says %d bytes, got %d", 20+msgLen, len(buf))
	}

	// Verify transaction ID matches.
	respTxnID := buf[8:20]
	for i := 0; i < 12; i++ {
		if respTxnID[i] != txnID[i] {
			return nil, fmt.Errorf("transaction id mismatch")
		}
	}

	// Walk attributes.
	attrs := buf[20 : 20+msgLen]
	for i := 0; i+4 <= len(attrs); {
		attrType := binary.BigEndian.Uint16(attrs[i : i+2])
		attrLen := binary.BigEndian.Uint16(attrs[i+2 : i+4])
		attrVal := attrs[i+4 : i+4+int(attrLen)]
		i += 4 + int(attrLen)
		// STUN attributes are padded to 4-byte boundaries.
		if pad := int(attrLen) % 4; pad != 0 {
			i += 4 - pad
		}

		switch attrType {
		case attrXorMappedAddr:
			if r, err := parseXorMappedAddress(attrVal, txnID); err == nil {
				return r, nil
			}
		case attrMappedAddr:
			if r, err := parseMappedAddress(attrVal); err == nil {
				return r, nil
			}
		}
	}

	return nil, fmt.Errorf("no mapped address attribute in stun response")
}

// parseXorMappedAddress decodes an XOR-MAPPED-ADDRESS attribute value.
//
// Layout (RFC 5389 §15.2):
//
//	 0                   1                   2                   3
//	 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|0 0 0 0 0 0 0 0|    Family     |         X-Port                |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|   X-Address (variable)                                       |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//
// The port is XORed with the upper 16 bits of the magic cookie.
// The IPv4 address is XORed with the full 32-bit magic cookie.
// The IPv6 address is XORed with the magic cookie + transaction ID (16 bytes).
func parseXorMappedAddress(val []byte, txnID []byte) (*RefAddr, error) {
	if len(val) < 8 {
		return nil, fmt.Errorf("xor-mapped-address too short")
	}

	family := val[1]
	xPort := binary.BigEndian.Uint16(val[2:4])
	port := xPort ^ uint16(stunMagicCookie>>16)

	switch family {
	case 0x01: // IPv4
		if len(val) < 8 {
			return nil, fmt.Errorf("xor-mapped-address ipv4 too short")
		}
		xAddr := binary.BigEndian.Uint32(val[4:8])
		cookie := uint32(stunMagicCookie)
		raw := xAddr ^ cookie
		ip := net.IPv4(
			byte(raw),
			byte(raw>>8),
			byte(raw>>16),
			byte(raw>>24),
		)
		return &RefAddr{IP: ip, Port: int(port)}, nil

	case 0x02: // IPv6
		if len(val) < 20 {
			return nil, fmt.Errorf("xor-mapped-address ipv6 too short")
		}
		// XOR key = magic cookie (4 bytes) + transaction ID (12 bytes) = 16 bytes
		var key [16]byte
		binary.BigEndian.PutUint32(key[0:4], stunMagicCookie)
		copy(key[4:16], txnID)

		ip := make(net.IP, 16)
		for i := 0; i < 16; i++ {
			ip[i] = val[4+i] ^ key[i]
		}
		return &RefAddr{IP: ip, Port: int(port)}, nil

	default:
		return nil, fmt.Errorf("unknown address family: 0x%02x", family)
	}
}

// parseMappedAddress decodes a legacy MAPPED-ADDRESS attribute (RFC 5389
// §15.1). Same layout as XOR-MAPPED-ADDRESS but without the XOR obfuscation.
// Used as a fallback for older STUN servers that don't implement the XOR
// variant.
func parseMappedAddress(val []byte) (*RefAddr, error) {
	if len(val) < 8 {
		return nil, fmt.Errorf("mapped-address too short")
	}

	family := val[1]
	port := binary.BigEndian.Uint16(val[2:4])

	switch family {
	case 0x01: // IPv4
		ip := net.IPv4(val[4], val[5], val[6], val[7])
		return &RefAddr{IP: ip, Port: int(port)}, nil
	case 0x02: // IPv6
		if len(val) < 20 {
			return nil, fmt.Errorf("mapped-address ipv6 too short")
		}
		ip := make(net.IP, 16)
		copy(ip, val[4:20])
		return &RefAddr{IP: ip, Port: int(port)}, nil
	default:
		return nil, fmt.Errorf("unknown address family: 0x%02x", family)
	}
}
