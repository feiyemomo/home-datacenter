package camera

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// TestRewriteH3CCandidates_AdversarialStress tests RewriteH3CCandidates with edge cases:
// empty string, malformed strings, no candidates, multiple relay lines, IPv6, fuzzing.
func TestRewriteH3CCandidates_AdversarialStress(t *testing.T) {
	relayHost := "154.8.195.220"
	targetPort := "52992"
	targetAddr := relayHost + ":" + targetPort

	t.Run("EmptySDP", func(t *testing.T) {
		got := RewriteH3CCandidates("", targetAddr)
		if got != "" {
			t.Errorf("expected empty string, got %q", got)
		}
	})

	t.Run("NoCandidatesInSDP", func(t *testing.T) {
		sdp := "v=0\r\no=- 12345 67890 IN IP4 127.0.0.1\r\ns=Session\r\nt=0 0\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\n"
		got := RewriteH3CCandidates(sdp, targetAddr)
		if got != sdp {
			t.Errorf("SDP without candidates should not be altered, got:\n%s", got)
		}
	})

	t.Run("MalformedSDPLines", func(t *testing.T) {
		malformedInputs := []string{
			"a=candidate:\r\n",
			"a=candidate:   \r\n",
			"a=candidate:1\r\n",
			"a=candidate:1 1 tcp\r\n",
			"a=candidate:1 1 tcp 100\r\n",
			"a=candidate:1 1 tcp 100 154.8.195.220\r\n", // only 5 fields
			"a=candidate:1 1 tcp 100 154.8.195.220 \r\n",
			"a=candidate:not_enough_fields\r\n",
			"a=candidate:foo bar baz qux 154.8.195.220 8080 typ host\r\n", // fields[2] neither udp nor tcp
			"random non-sdp line without candidate prefix\r\n",
			"\r\n\r\n\r\n",
			"a=candidate:371706427 1 tcp 1671430136 154.8.195.220 32596 typ host tcptype passive", // no trailing newline
		}

		for _, input := range malformedInputs {
			// Must never panic
			_ = RewriteH3CCandidates(input, targetAddr)
			_ = RewriteH3CCandidates(input, "")
			_ = RewriteH3CCandidates(input, "invalid_address")
			_ = RewriteH3CCandidates(input, ":")
			_ = RewriteH3CCandidates(input, "154.8.195.220:")
		}
	})

	t.Run("MultipleRelayCandidates_MixedUDPAndTCP", func(t *testing.T) {
		sdp := strings.Join([]string{
			"v=0\r\n",
			"o=- 1 2 IN IP4 127.0.0.1\r\n",
			"m=video 9 UDP/TLS/RTP/SAVPF 96\r\n",
			// 3 UDP relay lines with different ports and foundations
			"a=candidate:udp1 1 udp 2130706423 154.8.195.220 32596 typ host\r\n",
			"a=candidate:udp2 2 udp 2130706422 154.8.195.220 32597 typ host\r\n",
			"a=candidate:udp3 1 udp 2130706421 154.8.195.220 12345 typ srflx raddr 0.0.0.0 rport 0\r\n",
			// 2 TCP relay lines with different ports
			"a=candidate:tcp1 1 tcp 1671430136 154.8.195.220 32596 typ host tcptype passive\r\n",
			"a=candidate:tcp2 2 tcp 1671430135 154.8.195.220 32597 typ host tcptype passive\r\n",
			// Other unrelated candidates (UDP and TCP on LAN / local loopback)
			"a=candidate:lan1 1 udp 2130706420 192.168.31.234 8555 typ host\r\n",
			"a=candidate:lan2 1 tcp 1671430130 192.168.31.234 8555 typ host tcptype passive\r\n",
			"a=candidate:loop1 1 tcp 1671430120 127.0.0.1 8555 typ host tcptype passive\r\n",
		}, "")

		got := RewriteH3CCandidates(sdp, targetAddr)

		// 1. ALL UDP candidates for 154.8.195.220 MUST be completely dropped!
		if strings.Contains(got, "udp1") || strings.Contains(got, "udp2") || strings.Contains(got, "udp3") {
			t.Errorf("found UDP relay candidate in rewritten SDP:\n%s", got)
		}
		if strings.Contains(got, "udp 2130706423 154.8.195.220") || strings.Contains(got, "udp 2130706422 154.8.195.220") {
			t.Errorf("found UDP relay candidate matching relay IP in rewritten SDP:\n%s", got)
		}

		// 2. Both TCP relay lines MUST have port rewritten to 52992
		if !strings.Contains(got, "a=candidate:tcp1 1 tcp 1671430136 154.8.195.220 52992 typ host tcptype passive\r\n") {
			t.Errorf("tcp1 relay line not rewritten correctly:\n%s", got)
		}
		if !strings.Contains(got, "a=candidate:tcp2 2 tcp 1671430135 154.8.195.220 52992 typ host tcptype passive\r\n") {
			t.Errorf("tcp2 relay line not rewritten correctly:\n%s", got)
		}

		// 3. Stale relay port 32596 / 32597 on 154.8.195.220 must not exist
		if strings.Contains(got, "154.8.195.220 32596") || strings.Contains(got, "154.8.195.220 32597") {
			t.Errorf("stale relay port remains in rewritten SDP:\n%s", got)
		}

		// 4. LAN candidates MUST be preserved untouched
		if !strings.Contains(got, "a=candidate:lan1 1 udp 2130706420 192.168.31.234 8555 typ host\r\n") {
			t.Errorf("lan1 candidate was incorrectly dropped or altered:\n%s", got)
		}
		if !strings.Contains(got, "a=candidate:lan2 1 tcp 1671430130 192.168.31.234 8555 typ host tcptype passive\r\n") {
			t.Errorf("lan2 candidate was incorrectly dropped or altered:\n%s", got)
		}
		if !strings.Contains(got, "a=candidate:loop1 1 tcp 1671430120 127.0.0.1 8555 typ host tcptype passive\r\n") {
			t.Errorf("loop1 candidate was incorrectly dropped or altered:\n%s", got)
		}
	})

	t.Run("FuzzingRandomLines", func(t *testing.T) {
		r := rand.New(rand.NewSource(42))
		for i := 0; i < 500; i++ {
			var sb strings.Builder
			lineCount := r.Intn(20)
			for j := 0; j < lineCount; j++ {
				switch r.Intn(5) {
				case 0:
					sb.WriteString(fmt.Sprintf("a=candidate:%d 1 udp %d %s %d typ host\r\n", r.Intn(99999), r.Intn(99999), relayHost, r.Intn(65535)))
				case 1:
					sb.WriteString(fmt.Sprintf("a=candidate:%d 1 tcp %d %s %d typ host tcptype passive\r\n", r.Intn(99999), r.Intn(99999), relayHost, r.Intn(65535)))
				case 2:
					sb.WriteString(fmt.Sprintf("a=candidate:%d 1 udp %d 192.168.1.%d 8555 typ host\r\n", r.Intn(99999), r.Intn(99999), r.Intn(254)))
				case 3:
					sb.WriteString(fmt.Sprintf("garbage line %d with spaces and symbols !@#$%%\r\n", r.Intn(1000)))
				case 4:
					sb.WriteString("\r\n")
				}
			}

			// Must never panic
			res := RewriteH3CCandidates(sb.String(), targetAddr)

			// Invariant: No line with "udp" AND "154.8.195.220" should survive if it is a candidate line
			lines := strings.Split(res, "\n")
			for _, l := range lines {
				trimmed := strings.TrimSpace(l)
				if strings.HasPrefix(trimmed, "a=candidate:") && strings.Contains(trimmed, relayHost) {
					fields := strings.Split(trimmed, " ")
					if len(fields) >= 5 && fields[4] == relayHost {
						if strings.EqualFold(fields[2], "udp") {
							t.Fatalf("fuzz invariant violated: found UDP candidate for relay host: %s", trimmed)
						}
						if strings.EqualFold(fields[2], "tcp") && len(fields) >= 6 {
							if fields[5] != targetPort {
								t.Fatalf("fuzz invariant violated: TCP candidate port %s != target %s: %s", fields[5], targetPort, trimmed)
							}
						}
					}
				}
			}
		}
	})
}
