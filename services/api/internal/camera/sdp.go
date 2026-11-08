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
// stream interruption.
//
// Crucially, the public H3C tunnel is strictly TCP-only. Inbound UDP packets
// to the relay IP:port are silently dropped by the provider. Under RFC 8445,
// WebRTC clients prioritize UDP candidates over TCP. If a UDP candidate is
// left in the answer, clients stall on dead UDP probes until their internal
// watchdog triggers. Therefore, all UDP candidates for the relay host are
// discarded, leaving only the rewritten TCP candidate (tcptype passive).
func RewriteH3CCandidates(sdp, current string) string {
	host, port, ok := splitHostPort(current)
	if !ok {
		return sdp
	}

	lines := strings.Split(sdp, "\n")
	out := make([]string, 0, len(lines))
	changed := false

	for _, line := range lines {
		cr := strings.HasSuffix(line, "\r")
		trimmed := strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(trimmed, "a=candidate:") {
			out = append(out, line)
			continue
		}
		// a=candidate:<foundation> <component> <transport> <priority>
		//   <connection-address> <port> typ <type> ...
		fields := strings.Split(trimmed, " ")
		if len(fields) < 6 {
			out = append(out, line)
			continue
		}
		if fields[4] == host {
			// H3C Oasis tunnel is strictly TCP-only. Completely discard UDP candidate
			// for the relay host.
			if strings.EqualFold(fields[2], "udp") {
				changed = true
				continue
			}
			// Rewrite TCP candidates with the current active tunnel port.
			if strings.EqualFold(fields[2], "tcp") {
				if fields[5] != port {
					fields[5] = port
					rewritten := strings.Join(fields, " ")
					if cr {
						rewritten += "\r"
					}
					out = append(out, rewritten)
					changed = true
					continue
				}
			}
		}
		out = append(out, line)
	}

	if !changed {
		return sdp
	}
	return strings.Join(out, "\n")
}

func splitHostPort(addr string) (host, port string, ok bool) {
	h, p, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil || h == "" || p == "" {
		return "", "", false
	}
	return h, p, true
}
