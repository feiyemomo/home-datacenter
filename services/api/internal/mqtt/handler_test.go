package mqtt

import "testing"

func TestParseStatusPayload(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		wantStat string
		wantTS   int64
		wantOK   bool
	}{
		{"strict quoted", `{"status":"online","ts":1234567890}`, "online", 1234567890, true},
		{"strict reversed", `{"ts":42,"status":"offline"}`, "offline", 42, true},
		{"unquoted keys and values", `{status:online,ts:1234567890}`, "online", 1234567890, true},
		{"unquoted keys, quoted values", `{"status":"heartbeat","ts":99}`, "heartbeat", 99, true},
		{"bareword status only", `status=offline`, "offline", 0, true},
		{"garbage rejected", `not-json-at-all`, "", 0, false},
		{"empty rejected", ``, "", 0, false},
		{"unknown status still accepted at parse layer", `{"status":"weird"}`, "weird", 0, true},
		{"nested arrays tolerated", `{"status":"online","tags":["a","b"]}`, "online", 0, true},
		{"quoted key with embedded comma", `{"a,b":"x","status":"online"}`, "online", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStatus, gotTS, gotOK := parseStatusPayload([]byte(tc.payload))
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v (status=%q ts=%d)", gotOK, tc.wantOK, gotStatus, gotTS)
			}
			if !gotOK {
				return
			}
			if gotStatus != tc.wantStat {
				t.Errorf("status = %q, want %q", gotStatus, tc.wantStat)
			}
			if gotTS != tc.wantTS {
				t.Errorf("ts = %d, want %d", gotTS, tc.wantTS)
			}
		})
	}
}

func TestLenientJSON(t *testing.T) {
	// lenientJSON is the fallback path. When the input is already
	// well-formed JSON, it returns nil so the strict decoder above
	// owns the conversion. When the input is unquoted-keyed, it
	// rewrites it into a strict object. When the input is not an
	// object at all, it returns nil.
	cases := []struct {
		name string
		in   string
		want string // empty == expect nil result
	}{
		{"unquoted status and ts", `{status:online,ts:1234567890}`, `{"status":"online","ts":1234567890}`},
		{"already strict", `{"status":"online","ts":1}`, ""},
		{"value with spaces", `{ status : online , ts : 5 }`, `{"status":"online","ts":5}`},
		{"trailing comma on strict input", `{"status":"online","ts":1,}`, ""},
		{"empty object", `{}`, ""},
		{"non-object rejected", `[]`, ""},
		{"nested braces OK strict", `{"a":{"b":1},"status":"online"}`, ""},
		{"nested braces in unquoted", `{a:{b:1},status:online}`, `{"a":"{b:1}","status":"online"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lenientJSON([]byte(tc.in))
			if tc.want == "" {
				if got != nil {
					t.Fatalf("want nil, got %q", got)
				}
				return
			}
			if string(got) != tc.want {
				t.Errorf("lenientJSON(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
