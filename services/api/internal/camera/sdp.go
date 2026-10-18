package camera

import (
	"net"
	"strings"
)

// RewriteH3CCandidates replaces the port of every ICE candidate that
// advertises the H3C relay host with the port of the tunnel that is up
// right now.
//
// Why this exists: go2rtc builds its SDP answer from the
// go2rtc.webrtc.candidates it was started with. On this deployment the
// H3C gateway hands out a *new* public port for the WebRTC (8555) tunnel
// every few hours, and the running go2rtc only picks up the new value
// when Frigate regenerates /dev/shm/go2rtc.yaml — which happens on a
// Frigate restart, not on a config push. The keepalive service does
// persist the new address to config.yml (and to go2rtc's in-memory
// config), but any later go2rtc restart silently reverts the runtime
// list to the stale file. Clients that cannot reach the NAS over the LAN
// (double-NAT setups) then sit in ICE "checking" until they time out to
// the MP4 fallback.
//
// Rewriting the answer on the way out makes every new session carry the
// tunnel address that is actually established, with no restart and no
// stream interruption. Both the UDP and the TCP candidate for that host
// are rewritten; the TCP one (tcptype passive) is the one that matters,
// because the H3C tunnel is a plain TCP forward.
func RewriteH3CCandidates(sdp, current string) string {
	host, port, ok := splitHostPort(current)
	if !ok {
		return sdp
	}

	lines := strings.Split(sdp, "\n")
	changed := false

	for i, line := range lines {
		cr := strings.HasSuffix(line, "\r")
		trimmed := strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(trimmed, "a=candidate:") {
			continue
		}
		// a=candidate:<foundation> <component> <transport> <priority>
		//   <connection-address> <port> typ <type> ...
		fields := strings.Split(trimmed, " ")
		if len(fields) < 6 {
			continue
		}
		if fields[4] != host || fields[5] == port {
			continue
		}
		fields[5] = port
		rewritten := strings.Join(fields, " ")
		if cr {
			rewritten += "\r"
		}
		lines[i] = rewritten
		changed = true
	}

	if !changed {
		return sdp
	}
	return strings.Join(lines, "\n")
}

func splitHostPort(addr string) (host, port string, ok bool) {
	h, p, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil || h == "" || p == "" {
		return "", "", false
	}
	return h, p, true
}
