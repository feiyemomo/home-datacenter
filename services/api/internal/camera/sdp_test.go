package camera

import (
	"strings"
	"testing"
)

func TestRewriteH3CCandidates(t *testing.T) {
	const answer = "v=0\r\n" +
		"o=- 1 2 IN IP4 127.0.0.1\r\n" +
		"m=video 9 UDP/TLS/RTP/SAVPF 96\r\n" +
		"a=candidate:375738803 1 tcp 1671430142 127.0.0.1 8555 typ host tcptype passive\r\n" +
		"a=candidate:314293799 1 tcp 1671430140 192.168.31.234 8555 typ host tcptype passive\r\n" +
		"a=candidate:371706427 1 tcp 1671430136 154.8.195.220 32596 typ host tcptype passive\r\n" +
		"a=candidate:2882845659 1 udp 2130706423 154.8.195.220 32596 typ host\r\n" +
		"a=candidate:1726268413 1 udp 1694498815 111.18.230.17 12091 typ srflx raddr 0.0.0.0 rport 58745\r\n"

	got := RewriteH3CCandidates(answer, "154.8.195.220:52992")

	if strings.Contains(got, "154.8.195.220 32596") {
		t.Fatalf("stale relay port still present:\n%s", got)
	}
	if n := strings.Count(got, "154.8.195.220 52992"); n != 2 {
		t.Fatalf("expected both relay candidates rewritten (udp+tcp), got %d:\n%s", n, got)
	}
	// The TCP one keeps its tcptype suffix.
	if !strings.Contains(got, "a=candidate:371706427 1 tcp 1671430136 154.8.195.220 52992 typ host tcptype passive\r\n") {
		t.Fatalf("tcp candidate lost its attributes:\n%s", got)
	}
	// Unrelated candidates are untouched.
	for _, want := range []string{"127.0.0.1 8555", "192.168.31.234 8555", "111.18.230.17 12091"} {
		if !strings.Contains(got, want) {
			t.Fatalf("unrelated candidate %q was modified:\n%s", want, got)
		}
	}
	// CRLF structure must survive so the SDP stays valid.
	if !strings.HasSuffix(got, "\r\n") || strings.Contains(got, "\n\n") {
		t.Fatalf("line endings were mangled:\n%q", got)
	}
}

func TestRewriteH3CCandidatesNoChange(t *testing.T) {
	const answer = "a=candidate:1 1 tcp 100 10.0.0.5 8555 typ host tcptype passive\r\n"

	if got := RewriteH3CCandidates(answer, "10.0.0.5:8555"); got != answer {
		t.Fatalf("expected identical answer, got %q", got)
	}
	if got := RewriteH3CCandidates(answer, "not-an-address"); got != answer {
		t.Fatalf("malformed current address should be ignored, got %q", got)
	}
}
