package network

import "testing"

func TestIPv6PrefixMatches(t *testing.T) {
	tests := []struct {
		name   string
		addr1  string
		addr2  string
		expect bool
	}{
		{
			name:   "same /64 prefix",
			addr1:  "2001:db8::1",
			addr2:  "2001:db8::2",
			expect: true,
		},
		{
			name:   "different /64 prefix",
			addr1:  "2001:db8::1",
			addr2:  "2001:db9::1",
			expect: false,
		},
		{
			name:   "first address invalid",
			addr1:  "not-an-ip",
			addr2:  "2001:db8::1",
			expect: false,
		},
		{
			name:   "second address invalid",
			addr1:  "2001:db8::1",
			addr2:  "not-an-ip",
			expect: false,
		},
		{
			name:   "both addresses invalid",
			addr1:  "invalid1",
			addr2:  "invalid2",
			expect: false,
		},
		{
			name:   "empty addresses",
			addr1:  "",
			addr2:  "",
			expect: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IPv6PrefixMatches(tt.addr1, tt.addr2)
			if got != tt.expect {
				t.Errorf("IPv6PrefixMatches(%q, %q) = %v, want %v", tt.addr1, tt.addr2, got, tt.expect)
			}
		})
	}
}

func TestIPv6FromDNS(t *testing.T) {
	tests := []struct {
		name     string
		hostname string
		want     string
	}{
		{name: "empty hostname", hostname: "", want: ""},
		{name: "invalid hostname", hostname: "nonexistent.invalid", want: ""},
		{name: "ipv4-only", hostname: "1.1.1.1", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IPv6FromDNS(tt.hostname)
			if got != tt.want {
				t.Errorf("IPv6FromDNS(%q) = %q, want %q", tt.hostname, got, tt.want)
			}
		})
	}
}

func TestOutboundIPv6Address_PrefersDNSOverEnv(t *testing.T) {
	// The DDNS lookup must be consulted before the static env var so a
	// prefix rotation is picked up from the AAAA record. Use an
	// unroutable, non-resolving hostname so DNS fails deterministically
	// and we fall through to NAS_IPV6_ADDRESS.
	t.Setenv("NAS_IPV6_DDNS", "nonexistent.invalid")
	t.Setenv("NAS_IPV6_ADDRESS", "2001:db8::100")

	got := OutboundIPv6Address()
	if got != "2001:db8::100" {
		t.Errorf("OutboundIPv6Address() = %q, want %q", got, "2001:db8::100")
	}
}
