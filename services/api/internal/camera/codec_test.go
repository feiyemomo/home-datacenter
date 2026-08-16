package camera

import "testing"

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