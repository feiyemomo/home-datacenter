package camera

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Go2RTCClient is a minimal HTTP client for the go2rtc server
// bundled in the deployment. We use the upstream "streams" API:
//
//	PUT    /api/streams?src=<rtsp_url>&name=<stream_name>   (create)
//	PATCH  /api/streams?src=<rtsp_url>&name=<stream_name>   (update)
//	DELETE /api/streams?src=<stream_name>
//	GET    /api/webrtc?src=<name>      (WebRTC SDP exchange)
//	GET    /api/stream.m3u8?src=<name>  (HLS fallback)
//
// go2rtc's PUT /api/streams uses QUERY PARAMETERS, not a JSON body.
// Sending a JSON body like {"cam_1": "rtsp://..."} (the old shape)
// is silently accepted with HTTP 200, but go2rtc ignores the body
// entirely -- the stream is never created because the required src
// query parameter is missing. This is the root cause of the
// "registered a camera but go2rtc's stream list is empty" symptom.
//
// Reference: https://go2rtc.org/api/  (OpenAPI spec)
type Go2RTCClient struct {
	Base string       // e.g. http://home-go2rtc:1984
	HC   *http.Client // overridable for tests
}

// NewGo2RTCClient returns a client with a sensible 5s timeout.
func NewGo2RTCClient(base string) *Go2RTCClient {
	return &Go2RTCClient{
		Base: base,
		HC:   &http.Client{Timeout: 5 * time.Second},
	}
}

// Alive reports whether go2rtc's HTTP API is reachable. Used by
// BootReplay to decide whether to attempt stream registration or
// wait. A simple GET /api/streams with a short timeout is enough —
// go2rtc responds with 200 once its API server is up.
func (c *Go2RTCClient) Alive(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/api/streams", nil)
	if err != nil {
		return false
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 500
}

// AddStream registers a stream. It is idempotent: re-adding the same
// name with a different URL updates the source.
//
// go2rtc's PUT /api/streams expects two query parameters:
//
//	src  = the stream source URI (e.g. rtsp://user:pass@host/path)
//	name = the stream name (e.g. cam_1)
//
// Both values are URL-escaped so special characters in RTSP URLs
// (notably @ and : in credentials) don't break the query string.
//
// Known go2rtc quirk: after adding the stream to its in-memory state,
// go2rtc tries to persist the config to /etc/go2rtc.yaml. If the
// RTSP URL contains characters that go2rtc's YAML serializer doesn't
// quote properly (notably : inside the URL), the save fails and
// go2rtc returns HTTP 400 -- even though the stream IS live in memory.
// We work around this by doing a GET /api/streams after a failed PUT:
// if the stream exists, we treat it as success.
func (c *Go2RTCClient) AddStream(ctx context.Context, name, rtspURL string) error {
	u := fmt.Sprintf("%s/api/streams?src=%s&name=%s",
		c.Base,
		url.QueryEscape(rtspURL),
		url.QueryEscape(name),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 300 {
		return nil
	}
	// go2rtc may have added the stream to memory but failed to save
	// the config file. Verify with a GET before returning an error.
	if c.streamExists(ctx, name) {
		return nil
	}
	return fmt.Errorf("go2rtc add stream: %s", resp.Status)
}

// streamExists checks whether a stream with the given name is
// registered in go2rtc's in-memory state via GET /api/streams.
func (c *Go2RTCClient) streamExists(ctx context.Context, name string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/api/streams", nil)
	if err != nil {
		return false
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false
	}
	var streams map[string]json.RawMessage
	if err := json.Unmarshal(raw, &streams); err != nil {
		return false
	}
	_, ok := streams[name]
	return ok
}

// RemoveStream deletes a stream by name. A 404 is treated as success
// (idempotent unregister).
func (c *Go2RTCClient) RemoveStream(ctx context.Context, name string) error {
	u := fmt.Sprintf("%s/api/streams?src=%s", c.Base, url.QueryEscape(name))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("go2rtc remove stream: %s", resp.Status)
}

// WebRTCURL is the SDP endpoint the browser POSTs to. The client
// exchanges its SDP offer here and receives an SDP answer back as
// the HTTP body.
func (c *Go2RTCClient) WebRTCURL(streamName string) string {
	return fmt.Sprintf("%s/api/webrtc?src=%s", c.Base, url.QueryEscape(streamName))
}

// HLSURL is the iOS-Safari fallback (HLS, not WebRTC).
//
// The `mp4=` query parameter switches go2rtc from the default
// MPEG-TS container (segment.ts) to fragmented MP4 (segment.m4s).
// hls.js's TS demuxer has weak HEVC support and will silently drop
// frames on the path back to MSE, which manifests as a "playback
// starts, gets a frame or two, then stalls" symptom (the browser
// has fresh data in the buffer but no decoded frame reaches the
// compositor, so `<video>` never fires `playing`). fMP4 sidesteps
// the demuxer problem entirely and is the recommended container
// for HEVC over HLS in 2024+.
//
// See: build-host/go2rtc/internal/hls/hls.go (uses
// mp4.ParseQuery to choose between `mp4.NewConsumer` and
// `mpegts.NewConsumer`).
func (c *Go2RTCClient) HLSURL(streamName string) string {
	return fmt.Sprintf("%s/api/stream.m3u8?src=%s&mp4=", c.Base, url.QueryEscape(streamName))
}
