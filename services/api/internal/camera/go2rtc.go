package camera

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Go2RTCClient is a minimal HTTP client for the go2rtc server
// bundled in the deployment. We use the upstream "streams" API:
//
//	PUT    /api/streams   { "name": "cam_1", "url": "rtsp://..." }
//	DELETE /api/streams?src=<name>
//	GET    /api/webrtc?src=<name>     (WebRTC SDP exchange)
//	GET    /api/stream.m3u8?src=<name> (HLS fallback)
//
// Reference: https://github.com/AlexxIT/go2rtc
type Go2RTCClient struct {
	Base string        // e.g. http://home-go2rtc:1984
	HC   *http.Client  // overridable for tests
}

// NewGo2RTCClient returns a client with a sensible 5s timeout.
func NewGo2RTCClient(base string) *Go2RTCClient {
	return &Go2RTCClient{
		Base: base,
		HC:   &http.Client{Timeout: 5 * time.Second},
	}
}

// AddStream registers a stream. It is idempotent: re-adding the same
// name with a different URL updates the source.
func (c *Go2RTCClient) AddStream(ctx context.Context, name, rtspURL string) error {
	body, _ := json.Marshal(map[string]any{
		"name": name,
		"url":  rtspURL,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.Base+"/api/streams", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("go2rtc add stream: %s", resp.Status)
	}
	return nil
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
func (c *Go2RTCClient) HLSURL(streamName string) string {
	return fmt.Sprintf("%s/api/stream.m3u8?src=%s", c.Base, url.QueryEscape(streamName))
}
