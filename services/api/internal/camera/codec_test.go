package camera

import (
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