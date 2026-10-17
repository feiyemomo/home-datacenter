package camera

import (
	"net/url"
	"strings"
	"testing"

	"home-datacenter-api/internal/model"
)

func TestCodecFromProbe(t *testing.T) {
	tests := []struct {
		name          string
		probed        string
		wantCodec     string
		wantTranscode bool
	}{
		{name: "h264 stays passthrough", probed: "h264", wantCodec: "", wantTranscode: false},
		{name: "h264 uppercase", probed: "H264", wantCodec: "", wantTranscode: false},
		{name: "hevc routes to h264", probed: "hevc", wantCodec: "h264", wantTranscode: true},
		{name: "h265 routes to h264", probed: "h265", wantCodec: "h264", wantTranscode: true},
		{name: "mjpeg routes to h264", probed: "mjpeg", wantCodec: "h264", wantTranscode: true},
		{name: "empty keeps passthrough", probed: "", wantCodec: "", wantTranscode: false},
		{name: "whitespace keeps passthrough", probed: "  ", wantCodec: "", wantTranscode: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			codec, transcode := codecFromProbe(tt.probed)
			if codec != tt.wantCodec || transcode != tt.wantTranscode {
				t.Errorf("codecFromProbe(%q) = (%q, %v), want (%q, %v)",
					tt.probed, codec, transcode, tt.wantCodec, tt.wantTranscode)
			}
		})
	}
}

func TestFrigateInputs(t *testing.T) {
	r := &Registry{}
	cam := &model.Camera{
		Host:      "192.168.31.100",
		RTSPPort:  554,
		ChannelID: 101,
	}
	inputs := r.frigateInputs(cam, "admin", "pass")
	if len(inputs) != 2 {
		t.Fatalf("expected 2 inputs, got %d", len(inputs))
	}
	if inputs[0].Path != "rtsp://admin:pass@192.168.31.100:554/Streaming/Channels/101" {
		t.Errorf("unexpected input 0 path: %s", inputs[0].Path)
	}
	if len(inputs[0].Roles) != 1 || inputs[0].Roles[0] != "record" {
		t.Errorf("unexpected input 0 roles: %v", inputs[0].Roles)
	}
	if inputs[1].Path != "rtsp://admin:pass@192.168.31.100:554/Streaming/Channels/102" {
		t.Errorf("unexpected input 1 path: %s", inputs[1].Path)
	}
	if len(inputs[1].Roles) != 1 || inputs[1].Roles[0] != "detect" {
		t.Errorf("unexpected input 1 roles: %v", inputs[1].Roles)
	}
}

func TestBuildRTSPURL_SpecialCharacters(t *testing.T) {
	testCases := []struct {
		name     string
		user     string
		pass     string
		host     string
		port     int
		channel  int
		wantUser string
		wantPass string
	}{
		{
			name:     "plain credentials",
			user:     "admin",
			pass:     "pass123",
			host:     "192.168.1.100",
			port:     554,
			channel:  101,
			wantUser: "admin",
			wantPass: "pass123",
		},
		{
			name:     "password with @ symbol",
			user:     "admin",
			pass:     "p@ssword",
			host:     "192.168.1.100",
			port:     554,
			channel:  101,
			wantUser: "admin",
			wantPass: "p@ssword",
		},
		{
			name:     "password with # symbol",
			user:     "admin",
			pass:     "pass#123",
			host:     "192.168.1.100",
			port:     554,
			channel:  101,
			wantUser: "admin",
			wantPass: "pass#123",
		},
		{
			name:     "password with colons, slashes, queries, and percents",
			user:     "admin",
			pass:     "P@ss:w/o?r&d%1!#",
			host:     "192.168.1.100",
			port:     554,
			channel:  101,
			wantUser: "admin",
			wantPass: "P@ss:w/o?r&d%1!#",
		},
		{
			name:     "username and password with spaces and special chars",
			user:     "cam admin",
			pass:     "hello world @#$",
			host:     "192.168.1.100",
			port:     554,
			channel:  101,
			wantUser: "cam admin",
			wantPass: "hello world @#$",
		},
		{
			name:     "empty credentials",
			user:     "",
			pass:     "",
			host:     "192.168.1.100",
			port:     554,
			channel:  101,
			wantUser: "",
			wantPass: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := buildRTSPURL(tc.user, tc.pass, tc.host, tc.port, tc.channel)

			// Parse back with standard net/url to verify RFC 3986 compliance.
			u, err := url.Parse(result)
			if err != nil {
				t.Fatalf("buildRTSPURL(%q, %q) created invalid URL %q: %v", tc.user, tc.pass, result, err)
			}

			if tc.user == "" && tc.pass == "" {
				if u.User != nil {
					t.Errorf("expected no userinfo, got %v", u.User)
				}
				return
			}

			if u.User == nil {
				t.Fatalf("expected userinfo in %q, got nil", result)
			}

			gotUser := u.User.Username()
			gotPass, _ := u.User.Password()

			if gotUser != tc.wantUser {
				t.Errorf("user mismatch: got %q, want %q", gotUser, tc.wantUser)
			}
			if gotPass != tc.wantPass {
				t.Errorf("pass mismatch: got %q, want %q", gotPass, tc.wantPass)
			}
		})
	}
}

func TestFrigateInputs_SpecialCharacters(t *testing.T) {
	r := &Registry{}
	cam := &model.Camera{
		Host:      "192.168.31.100",
		RTSPPort:  554,
		ChannelID: 101,
	}
	inputs := r.frigateInputs(cam, "admin", "p@ss#word!")
	if len(inputs) != 2 {
		t.Fatalf("expected 2 inputs, got %d", len(inputs))
	}
	// Path must not contain raw # which would break ffmpeg fragment handling.
	if strings.Contains(inputs[0].Path, "pass#") {
		t.Errorf("path contains raw '#': %s", inputs[0].Path)
	}
	// Parsed password from Path must recover "p@ss#word!"
	u, err := url.Parse(inputs[0].Path)
	if err != nil {
		t.Fatalf("failed to parse frigate input path: %v", err)
	}
	p, _ := u.User.Password()
	if p != "p@ss#word!" {
		t.Errorf("frigate input password mismatch: got %q, want %q", p, "p@ss#word!")
	}
}

func TestBuildISAPIURL_SpecialCharacters(t *testing.T) {
	result := buildISAPIURL("admin", "p@ss#123", "192.168.1.100", 80)
	u, err := url.Parse(result)
	if err != nil {
		t.Fatalf("invalid ISAPI URL %q: %v", result, err)
	}
	p, _ := u.User.Password()
	if p != "p@ss#123" {
		t.Errorf("isapi password mismatch: got %q, want %q", p, "p@ss#123")
	}
}