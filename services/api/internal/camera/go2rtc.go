package camera

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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
	HC   *http.Client // short-timeout client for stream CRUD + health
	// SDPHC is a long-timeout client used exclusively for WebRTC SDP
	// exchange. go2rtc needs to connect to the RTSP source (and start
	// an ffmpeg transcoder when a codec directive is present) before
	// it can generate the SDP answer, which can take 10-20s on the
	// first request. The default 5s HC timeout causes "context
	// deadline exceeded" 502s.
	SDPHC *http.Client
}

// NewGo2RTCClient returns a client with a sensible 5s timeout for
// stream management and a 30s timeout for WebRTC SDP exchange.
func NewGo2RTCClient(base string) *Go2RTCClient {
	return &Go2RTCClient{
		Base:  base,
		HC:    &http.Client{Timeout: 5 * time.Second},
		SDPHC: &http.Client{Timeout: 30 * time.Second},
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

// ExchangeSDP forwards the browser's SDP offer to go2rtc and
// returns the SDP answer verbatim.
//
// Why this exists: the dashboard was originally proxying the
// browser's POST /api/webrtc through nginx, gating it with
// auth_request /api/v1/auth/verify. nginx's auth_request
// sub-request machinery reads (and discards) the request body
// while it's being formed for the sub-call, so when the upstream
// proxy_pass tried to forward the same body to go2rtc the body
// was already gone — the upstream connection sat idle waiting
// for bytes that would never come, hit the 60s
// proxy_send_timeout, and the browser saw 504/500.
//
// Bypassing nginx for the SDP exchange fixes that: the front-end
// POSTs the SDP directly to home-api (which already speaks
// JWT-auth), and home-api acts as the thin reverse-proxy to
// go2rtc. The SDP body is read once, in a Go handler, and
// forwarded once. The JWT is validated by the existing
// camGroup.Use(middleware.JWTAuth) middleware, so we don't need
// any new auth surface.
//
// Returns the SDP answer bytes (no parsing — go2rtc hands us the
// answer SDP as the response body, and we hand it back to the
// browser as our response body). go2rtc 5xx is bubbled up as
// an error; the handler turns that into a 502 BadGateway so the
// front-end can keep its existing SDP-error fallback path.
func (c *Go2RTCClient) ExchangeSDP(ctx context.Context, streamName string, sdpOffer []byte) ([]byte, error) {
	if len(sdpOffer) == 0 {
		return nil, fmt.Errorf("empty SDP offer")
	}
	u := c.WebRTCURL(streamName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(sdpOffer)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/sdp")

	// Use the long-timeout client (SDPHC) instead of the default
	// 5s HC. go2rtc must connect to the RTSP source and possibly
	// start an ffmpeg transcoder before it can produce the SDP
	// answer — the first request for a cold stream can take 10-20s.
	hc := c.SDPHC
	if hc == nil {
		hc = c.HC // fallback for tests that only set HC
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Read the full body so the connection can be returned to the
	// pool (go2rtc speaks HTTP/1.1 keep-alive). Cap at 64 KiB
	// — a normal SDP answer is well under 4 KiB; this is just a
	// guard against a misbehaving upstream that streams forever.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, fmt.Errorf("read sdp answer: %w", err)
	}

	if resp.StatusCode >= 300 {
		return body, fmt.Errorf("go2rtc webrtc: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
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

// Frame fetches a single JPEG frame from the go2rtc stream. Used by
// the dashboard's camera card to show a static preview before the
// operator clicks Play (avoids spinning up a WebRTC/HLS connection
// for every camera on the page — a 50KB JPEG is ~100x cheaper than
// a live stream).
//
// go2rtc exposes GET /api/frame.jpeg?src=<name> which grabs the
// next keyframe from the source. The first call on a cold stream
// may take 1-2s while go2rtc connects to the RTSP source.
func (c *Go2RTCClient) Frame(ctx context.Context, streamName string) (io.ReadCloser, string, error) {
	u := fmt.Sprintf("%s/api/frame.jpeg?src=%s", c.Base, url.QueryEscape(streamName))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	// Use the SDP client (longer timeout) because the first frame
	// on a cold stream can take a few seconds while go2rtc connects
	// to the RTSP source.
	hc := c.SDPHC
	if hc == nil {
		hc = c.HC
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return nil, "", fmt.Errorf("go2rtc frame: %s: %s", resp.Status, string(raw))
	}
	return resp.Body, resp.Header.Get("Content-Type"), nil
}
