package camera

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// probeVideoCodec runs ffprobe against the RTSP source and returns the
// camera's native video codec name (lowercased, e.g. "h264", "hevc").
// Returns "" when the camera is unreachable, ffprobe is missing, or no
// video track is found. Best-effort: never blocks registration, never
// panics.
//
// Auto-codec detection (v1.9.x): an HEVC/H.265 camera streamed with the
// passthrough path (`#video=copy`) breaks go2rtc's frame-grab (ffmpeg
// exit 183 → HTTP 500) and WebRTC live view (Chrome's WebRTC registry
// has no H.265 codec). Probing the native codec at registration lets us
// transparently route HEVC cameras to the h264 transcode pipeline while
// leaving H.264 cameras on the cheap passthrough path.
func probeVideoCodec(ctx context.Context, rtspURL string) string {
	if rtspURL == "" {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "ffprobe",
		"-v", "error",
		"-rtsp_transport", "tcp",
		"-rtsp_flags", "prefer_tcp",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_name",
		"-of", "csv=p=0",
		rtspURL,
	).Output()
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(out)))
}

// codecFromProbe maps a probed native video codec to the codec/transcode
// the camera should use. h264 (and unknown/empty) stay on passthrough;
// anything else (hevc, h265, mjpeg, ...) routes to h264 transcoding so
// WebRTC and frame-grab work regardless of the camera's native codec.
func codecFromProbe(probed string) (codec string, transcode bool) {
	switch strings.ToLower(strings.TrimSpace(probed)) {
	case "", "h264": // unknown or already H.264 → passthrough (cheap)
		return "", false
	default: // hevc/h265/mjpeg/... → transcode to h264 for WebRTC/frame compat
		return "h264", true
	}
}