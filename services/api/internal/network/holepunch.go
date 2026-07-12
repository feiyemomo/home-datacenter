package network

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// Hole-punching protocol constants.
//
// Packets are simple JSON messages (easy to debug with Wireshark/tcpdump).
// The magic field distinguishes hole-punching packets from STUN or random
// UDP traffic on the same port.
const (
	hpMagic      = "HP1"
	hpPunchEvery = 500 * time.Millisecond // send a punch packet this often
	hpPunchTime  = 30 * time.Second        // give up after this long
	hpSessionTTL = 5 * time.Minute         // established session expires after no activity
	hpBufSize    = 1500
)

// SessionStatus is the lifecycle state of a P2P hole-punching session.
type SessionStatus string

const (
	// SessionPunching: the server is sending hole-punching packets to
	// the peer but has not yet received a response.
	SessionPunching SessionStatus = "punching"

	// SessionEstablished: the server received a valid hole-punching
	// packet from the peer — the UDP channel is open.
	SessionEstablished SessionStatus = "established"

	// SessionFailed: the punch timeout expired without receiving a
	// response. The peer's NAT may be symmetric, or the peer's endpoint
	// changed.
	SessionFailed SessionStatus = "failed"
)

// P2PSession is a single hole-punching session with a peer.
type P2PSession struct {
	// PeerID is the peer's identifier (from JWT device_id).
	PeerID string `json:"peer_id"`

	// RemoteAddr is the peer's public UDP endpoint (from STUN).
	RemoteAddr string `json:"remote_addr"`

	// Status is the current session state.
	Status SessionStatus `json:"status"`

	// EstablishedAt is when the session became established (zero = never).
	EstablishedAt time.Time `json:"established_at,omitempty"`

	// LastPacketAt is when we last received a packet from the peer.
	LastPacketAt time.Time `json:"last_packet_at,omitempty"`

	// LastPunchAt is when we last sent a hole-punching packet.
	LastPunchAt time.Time `json:"last_punch_at,omitempty"`

	// PunchCount is how many hole-punching packets we've sent.
	PunchCount int `json:"punch_count"`

	// CreatedAt is when the session was created (first punch attempt).
	CreatedAt time.Time `json:"created_at"`
}

// hpPacket is the wire format for hole-punching messages.
type hpPacket struct {
	Magic   string `json:"m"`
	PeerID  string `json:"p"`
	TS      int64  `json:"t"`
}

// HolePuncher manages the server-side UDP socket for P2P hole punching.
//
// It binds a persistent UDP socket, discovers its own public endpoint via
// STUN (using the SAME socket — critical for NAT mapping consistency),
// and then:
//
//  1. Listens for incoming hole-punching packets from peers.
//  2. Sends hole-punching packets to registered peers (triggered by the
//     peer's POST /p2p/register call, which the PeerRegistry notifies us
//     about via the OnRegister callback).
//
// Sessions are tracked in-memory and expire after hpSessionTTL of no
// activity.
type HolePuncher struct {
	conn       *net.UDPConn
	localPort  int
	publicAddr *RefAddr // discovered via STUN from this socket

	mu       sync.RWMutex
	sessions map[string]*P2PSession // peerID -> session

	servers []STUNServer // for STUN discovery
	stop    chan struct{}
}

// NewHolePuncher creates a hole puncher that will bind to the given UDP
// port. If port is 0, the OS picks an ephemeral port. The STUN servers
// are used to discover the socket's public endpoint.
func NewHolePuncher(port int, servers []STUNServer) *HolePuncher {
	if len(servers) == 0 {
		servers = DefaultSTUNServers
	}
	return &HolePuncher{
		localPort: port,
		sessions:  make(map[string]*P2PSession),
		servers:   servers,
		stop:      make(chan struct{}),
	}
}

// Listen binds the UDP socket, discovers the public endpoint via STUN,
// and starts the receive + punch goroutines. Returns an error if the
// socket cannot be bound.
//
// If STUN discovery fails (e.g. all STUN servers are unreachable), the
// hole puncher still starts — peers just won't know the server's public
// endpoint. In practice, if STUN fails, P2P won't work anyway.
func (h *HolePuncher) Listen() error {
	addr := &net.UDPAddr{IP: net.IPv4zero, Port: h.localPort}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("holepunch: listen udp :%d: %w", h.localPort, err)
	}
	h.conn = conn
	h.localPort = conn.LocalAddr().(*net.UDPAddr).Port

	log.Printf("holepunch: UDP socket listening on :%d", h.localPort)

	// Discover public endpoint from this socket. We try each STUN
	// server until one responds.
	h.discoverPublic()

	// Start background goroutines.
	go h.receiveLoop()
	go h.punchLoop()

	return nil
}

// discoverPublic queries STUN servers from the hole-punching socket to
// find the server's public endpoint. This is essential — the public
// endpoint is what peers need to send their hole-punching packets to.
func (h *HolePuncher) discoverPublic() {
	for _, srv := range h.servers {
		ref, err := stunQueryFromConn(h.conn, srv)
		if err != nil {
			continue
		}
		h.mu.Lock()
		h.publicAddr = ref
		h.mu.Unlock()
		log.Printf("holepunch: public endpoint %s (via %s:%d)",
			ref.String(), srv.Host, srv.Port)
		return
	}
	log.Printf("holepunch: STUN discovery failed — P2P will not work")
}

// PublicEndpoint returns the server's STUN-discovered public address.
// Returns nil if discovery failed.
func (h *HolePuncher) PublicEndpoint() *RefAddr {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.publicAddr == nil {
		return nil
	}
	return &RefAddr{IP: h.publicAddr.IP, Port: h.publicAddr.Port}
}

// LocalPort returns the UDP port the hole puncher is listening on.
func (h *HolePuncher) LocalPort() int {
	return h.localPort
}

// StartPunching begins sending hole-punching packets to a peer. This is
// called when a peer registers its endpoint. The punch loop will send
// packets every hpPunchEvery until:
//   - A packet is received from the peer (session → established)
//   - hpPunchTime elapses with no response (session → failed)
//   - The session expires (hpSessionTTL of no activity)
func (h *HolePuncher) StartPunching(peerID string, peerAddr *net.UDPAddr) {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	if sess, ok := h.sessions[peerID]; ok && sess.Status == SessionEstablished {
		// Already established — just update the remote address in case
		// the peer's endpoint changed (e.g. mobile network switch).
		sess.RemoteAddr = peerAddr.String()
		return
	}

	h.sessions[peerID] = &P2PSession{
		PeerID:      peerID,
		RemoteAddr:  peerAddr.String(),
		Status:      SessionPunching,
		CreatedAt:   now,
		LastPunchAt: now,
	}
	log.Printf("holepunch: start punching peer=%s addr=%s", peerID, peerAddr.String())
}

// StopPunching removes a peer's session (called on explicit unregister
// or session expiry).
func (h *HolePuncher) StopPunching(peerID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, peerID)
}

// Sessions returns a snapshot of all active P2P sessions.
func (h *HolePuncher) Sessions() []*P2PSession {
	h.mu.RLock()
	defer h.mu.RUnlock()

	now := time.Now()
	var sessions []*P2PSession
	for _, s := range h.sessions {
		// Skip expired sessions in the snapshot.
		if now.Sub(s.LastPacketAt) > hpSessionTTL && s.Status == SessionEstablished {
			continue
		}
		sessions = append(sessions, &P2PSession{
			PeerID:        s.PeerID,
			RemoteAddr:    s.RemoteAddr,
			Status:        s.Status,
			EstablishedAt: s.EstablishedAt,
			LastPacketAt:  s.LastPacketAt,
			LastPunchAt:   s.LastPunchAt,
			PunchCount:    s.PunchCount,
			CreatedAt:     s.CreatedAt,
		})
	}
	return sessions
}

// Close stops all goroutines and closes the UDP socket.
func (h *HolePuncher) Close() {
	close(h.stop)
	if h.conn != nil {
		h.conn.Close()
	}
}

// punchLoop periodically sends hole-punching packets to all peers in
// the "punching" state. Runs until Close() is called.
func (h *HolePuncher) punchLoop() {
	ticker := time.NewTicker(hpPunchEvery)
	defer ticker.Stop()

	for {
		select {
		case <-h.stop:
			return
		case <-ticker.C:
			h.punchAll()
		}
	}
}

// punchAll sends a hole-punching packet to every peer in the "punching"
// state, and transitions timed-out sessions to "failed".
func (h *HolePuncher) punchAll() {
	h.mu.Lock()
	now := time.Now()

	for _, s := range h.sessions {
		switch s.Status {
		case SessionPunching:
			// Check timeout.
			if now.Sub(s.CreatedAt) > hpPunchTime {
				s.Status = SessionFailed
				log.Printf("holepunch: peer=%s FAILED (timeout)", s.PeerID)
				continue
			}
			// Parse the remote address and send a punch packet.
			addr, err := net.ResolveUDPAddr("udp", s.RemoteAddr)
			if err != nil {
				continue
			}
			h.sendPunch(s.PeerID, addr)
			s.LastPunchAt = now
			s.PunchCount++

		case SessionEstablished:
			// Expire sessions with no activity.
			if now.Sub(s.LastPacketAt) > hpSessionTTL {
				delete(h.sessions, s.PeerID)
				log.Printf("holepunch: peer=%s session expired", s.PeerID)
			}
		}
	}
	h.mu.Unlock()
}

// sendPunch sends a single hole-punching packet to the peer. Called
// with h.mu held by punchAll, but the actual write is non-blocking.
func (h *HolePuncher) sendPunch(peerID string, addr *net.UDPAddr) {
	pkt := hpPacket{
		Magic:  hpMagic,
		PeerID: peerID,
		TS:     time.Now().Unix(),
	}
	data, _ := json.Marshal(pkt)
	_, err := h.conn.WriteToUDP(data, addr)
	if err != nil {
		log.Printf("holepunch: write to %s: %v", addr.String(), err)
	}
}

// receiveLoop reads incoming UDP packets and processes hole-punching
// messages from peers. Runs until Close() is called.
func (h *HolePuncher) receiveLoop() {
	buf := make([]byte, hpBufSize)
	for {
		select {
		case <-h.stop:
			return
		default:
		}

		// Set a read deadline so we can check the stop channel.
		h.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, remoteAddr, err := h.conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if isClosedErr(err) {
				return
			}
			continue
		}

		h.handlePacket(buf[:n], remoteAddr)
	}
}

// handlePacket processes a single incoming UDP packet. If it's a valid
// hole-punching packet, the sender's session is marked as "established".
func (h *HolePuncher) handlePacket(data []byte, remote *net.UDPAddr) {
	var pkt hpPacket
	if err := json.Unmarshal(data, &pkt); err != nil {
		return // not a hole-punching packet (could be STUN or noise)
	}
	if pkt.Magic != hpMagic {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	sess, ok := h.sessions[pkt.PeerID]
	if !ok {
		// Unknown peer — create a session in "established" state.
		// This happens when the peer punches us before we start
		// punching them (race condition in the signaling).
		sess = &P2PSession{
			PeerID:      pkt.PeerID,
			RemoteAddr:  remote.String(),
			Status:      SessionEstablished,
			CreatedAt:   now,
		}
		h.sessions[pkt.PeerID] = sess
		log.Printf("holepunch: peer=%s ESTABLISHED (inbound first) from %s",
			pkt.PeerID, remote.String())
	} else {
		// Known peer — mark as established.
		wasPunching := sess.Status == SessionPunching
		sess.Status = SessionEstablished
		sess.RemoteAddr = remote.String()
		if sess.EstablishedAt.IsZero() {
			sess.EstablishedAt = now
		}
		if wasPunching {
			log.Printf("holepunch: peer=%s ESTABLISHED from %s (after %d punches)",
				pkt.PeerID, remote.String(), sess.PunchCount)
		}
	}
	sess.LastPacketAt = now
}

// isClosedErr returns true if the error indicates the socket was closed
// (used to exit the receive loop cleanly on shutdown).
func isClosedErr(err error) bool {
	return err.Error() == "use of closed network connection"
}
