package camera

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// PTZCommand is the high-level direction the API caller wants.
// Stop is special: it sends a MoveStop instead of a ContinuousMove.
type PTZCommand string

const (
	PTZLeft    PTZCommand = "left"
	PTZRight   PTZCommand = "right"
	PTZUp      PTZCommand = "up"
	PTZDown    PTZCommand = "down"
	PTZStop    PTZCommand = "stop"
	PTZZoomIn  PTZCommand = "zoom_in"
	PTZZoomOut PTZCommand = "zoom_out"
)

// ONVIFController dispatches PTZ commands to ONVIF-capable cameras.
// It deliberately uses raw SOAP+HTTP for the two requests we need
// (ContinuousMove and Stop) — pulling in a full ONVIF library would
// double the binary size for what is, today, ~150 lines of XML.
//
// Connections are lazy and cached per camera id. A network error
// evicts the cached client so the next call reconnects.
type ONVIFController struct {
	mu     sync.Mutex
	cached map[uint]*onvifSession
	hc     *http.Client
}

// NewONVIFController returns a controller with a 5s HTTP timeout.
func NewONVIFController() *ONVIFController {
	return &ONVIFController{
		cached: map[uint]*onvifSession{},
		hc:     &http.Client{Timeout: 5 * time.Second},
	}
}

// onvifSession is a per-camera cached client state.
type onvifSession struct {
	host         string
	port         int
	user, pass   string
	profileToken string
}

// ContinuousMove is the public entry point used by the HTTP handler.
// `cmd` is one of the PTZ* constants above; `speed` is normalised to
// 0..1 by the handler before being mapped to a Velocity vector.
func (o *ONVIFController) ContinuousMove(
	ctx context.Context,
	id uint,
	host string, port int,
	user, pass, profileToken string,
	cmd PTZCommand,
	speed float64,
) error {
	if profileToken == "" {
		return fmt.Errorf("camera %d: missing onvif profile token", id)
	}

	o.mu.Lock()
	s, ok := o.cached[id]
	if !ok || s.host != host || s.port != port ||
		s.user != user || s.pass != pass ||
		s.profileToken != profileToken {
		o.cached[id] = &onvifSession{
			host: host, port: port,
			user: user, pass: pass,
			profileToken: profileToken,
		}
	}
	o.mu.Unlock()

	deviceURL := fmt.Sprintf("http://%s:%d/onvif/device_service", host, port)

	if cmd == PTZStop {
		return o.sendSoap(ctx, deviceURL, user, pass, buildStopBody(profileToken))
	}
	vx, vy, zx := vectorFor(cmd, speed)
	body := buildContinuousMoveBody(profileToken, vx, vy, zx, 2*time.Second)
	return o.sendSoap(ctx, deviceURL, user, pass, body)
}

// Forget drops the cached session for a camera. Useful on unregister.
func (o *ONVIFController) Forget(id uint) {
	o.mu.Lock()
	delete(o.cached, id)
	o.mu.Unlock()
}

// sendSoap POSTs `body` to `deviceURL` with WS-Security UsernameToken
// digest auth, then verifies the SOAP response is a fault-free 200.
func (o *ONVIFController) sendSoap(ctx context.Context, deviceURL, user, pass, body string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceURL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")
	req.SetBasicAuth(user, pass)
	resp, err := o.hc.Do(req)
	if err != nil {
		// On any transport error, drop every cached session so the
		// next call reconnects. Stale auth state is the most common
		// cause of "first call works, second call fails".
		o.mu.Lock()
		o.cached = map[uint]*onvifSession{}
		o.mu.Unlock()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("onvif http %d: %s", resp.StatusCode, string(buf))
	}
	// Soap fault: <Fault>...</Fault> in the response body.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if bytes.Contains(raw, []byte("<Fault")) {
		return fmt.Errorf("onvif fault: %s", truncate(string(raw), 256))
	}
	return nil
}

// vectorFor converts a high-level PTZCommand + speed (0..1) into the
// pan/tilt/zoom velocity triple that ContinuousMove expects.
func vectorFor(cmd PTZCommand, speed float64) (vx, vy, zx float64) {
	if speed < 0 {
		speed = 0
	}
	if speed > 1 {
		speed = 1
	}
	switch cmd {
	case PTZLeft:
		vx = -speed
	case PTZRight:
		vx = speed
	case PTZUp:
		vy = speed
	case PTZDown:
		vy = -speed
	case PTZZoomIn:
		zx = speed
	case PTZZoomOut:
		zx = -speed
	}
	return vx, vy, zx
}

// buildContinuousMoveBody returns a SOAP envelope for
// ContinuousMove with a 2-second timeout (so the camera doesn't
// keep moving forever if the Stop call is lost).
func buildContinuousMoveBody(profileToken string, vx, vy, zx float64, timeout time.Duration) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<soap:Envelope xmlns:soap="http://www.w3.org/2003/05/soap-envelope"
               xmlns:tptz="http://www.onvif.org/ver20/ptz/wsdl"
               xmlns:tt="http://www.onvif.org/ver10/schema">
  <soap:Body>
    <tptz:ContinuousMove>
      <tptz:ProfileToken>%s</tptz:ProfileToken>
      <tptz:Velocity>
        <tt:PanTilt x="%.3f" y="%.3f" xmlns="http://www.onvif.org/ver10/schema"/>
        <tt:Zoom x="%.3f" xmlns="http://www.onvif.org/ver10/schema"/>
      </tptz:Velocity>
      <tptz:Timeout>PT%.0fS</tptz:Timeout>
    </tptz:ContinuousMove>
  </soap:Body>
</soap:Envelope>`, xmlEscape(profileToken), vx, vy, zx, timeout.Seconds())
}

func buildStopBody(profileToken string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<soap:Envelope xmlns:soap="http://www.w3.org/2003/05/soap-envelope"
               xmlns:tptz="http://www.onvif.org/ver20/ptz/wsdl">
  <soap:Body>
    <tptz:Stop>
      <tptz:ProfileToken>%s</tptz:ProfileToken>
      <tptz:PanTilt>true</tptz:PanTilt>
      <tptz:Zoom>true</tptz:Zoom>
    </tptz:Stop>
  </soap:Body>
</soap:Envelope>`, xmlEscape(profileToken))
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
