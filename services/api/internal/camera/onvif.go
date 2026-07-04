package camera

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
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
	// PTZGotoPreset targets a named ONVIF preset (see GotoPreset).
	PTZGotoPreset PTZCommand = "goto_preset"
)

// Profile is the trimmed projection of an ONVIF media profile —
// only what we need (token + name) is kept.
type Profile struct {
	Token string `json:"token"`
	Name  string `json:"name"`
}

// Preset is an ONVIF PTZ preset.
type Preset struct {
	Token string `json:"token"`
	Name  string `json:"name"`
}

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

// DiscoverProfiles hits ONVIF GetProfiles and returns the list of
// media profiles the camera exposes. Used by Registry.Register to
// auto-pick a profile token when the caller didn't supply one.
//
// We parse the SOAP response with regex (instead of pulling in
// encoding/xml round-trips for a four-line structure) to keep the
// dependency surface flat. The trade-off is brittleness against
// exotic namespaces — Hik/Dahua/Uniview all use the canonical
// ONVIF namespace, so this works for the 90% case.
func (o *ONVIFController) DiscoverProfiles(
	ctx context.Context,
	host string, port int, user, pass string,
) ([]Profile, error) {
	deviceURL := fmt.Sprintf("http://%s:%d/onvif/device_service", host, port)
	body := `<?xml version="1.0" encoding="UTF-8"?>
<soap:Envelope xmlns:soap="http://www.w3.org/2003/05/soap-envelope"
               xmlns:trt="http://www.onvif.org/ver10/media/wsdl">
  <soap:Body>
    <trt:GetProfiles/>
  </soap:Body>
</soap:Envelope>`
	body = strings.Replace(body, "<soap:Body>", wsseHeader(user, pass)+"<soap:Body>", 1)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")
	resp, err := o.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("onvif getprofiles http %d: %s", resp.StatusCode, string(buf))
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if bytes.Contains(raw, []byte("<Fault")) {
		return nil, fmt.Errorf("onvif getprofiles fault: %s", truncate(string(raw), 256))
	}
	return parseProfiles(string(raw)), nil
}

// DiscoverPresets hits ONVIF GetPresets for a given media profile.
// The caller is expected to have already called DiscoverProfiles to
// learn a valid profile token.
func (o *ONVIFController) DiscoverPresets(
	ctx context.Context,
	host string, port int, user, pass, profileToken string,
) ([]Preset, error) {
	deviceURL := fmt.Sprintf("http://%s:%d/onvif/device_service", host, port)
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<soap:Envelope xmlns:soap="http://www.w3.org/2003/05/soap-envelope"
               xmlns:tptz="http://www.onvif.org/ver20/ptz/wsdl">
  <soap:Body>
    <tptz:GetPresets>
      <tptz:ProfileToken>%s</tptz:ProfileToken>
    </tptz:GetPresets>
  </soap:Body>
</soap:Envelope>`, xmlEscape(profileToken))
	body = strings.Replace(body, "<soap:Body>", wsseHeader(user, pass)+"<soap:Body>", 1)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")
	resp, err := o.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("onvif getpresets http %d: %s", resp.StatusCode, string(buf))
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if bytes.Contains(raw, []byte("<Fault")) {
		return nil, fmt.Errorf("onvif getpresets fault: %s", truncate(string(raw), 256))
	}
	return parsePresets(string(raw)), nil
}

// GotoPreset issues an ONVIF GotoPreset request. The preset token
// must be known to the camera (i.e. set up in the camera's own UI
// first; ONVIF has no API to *create* a preset on most firmware).
func (o *ONVIFController) GotoPreset(
	ctx context.Context,
	id uint,
	host string, port int, user, pass, profileToken, presetToken string,
	speed float64,
) error {
	deviceURL := fmt.Sprintf("http://%s:%d/onvif/device_service", host, port)
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<soap:Envelope xmlns:soap="http://www.w3.org/2003/05/soap-envelope"
               xmlns:tptz="http://www.onvif.org/ver20/ptz/wsdl">
  <soap:Body>
    <tptz:GotoPreset>
      <tptz:ProfileToken>%s</tptz:ProfileToken>
      <tptz:PresetToken>%s</tptz:PresetToken>
      <tptz:Speed>
        <tt:PanTilt x="%.3f" y="%.3f" xmlns:tt="http://www.onvif.org/ver10/schema"/>
        <tt:Zoom x="%.3f" xmlns:tt="http://www.onvif.org/ver10/schema"/>
      </tptz:Speed>
    </tptz:GotoPreset>
  </soap:Body>
</soap:Envelope>`,
		xmlEscape(profileToken), xmlEscape(presetToken),
		clamp(speed), 0.0, clamp(speed))
	body = strings.Replace(body, "<soap:Body>", wsseHeader(user, pass)+"<soap:Body>", 1)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceURL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")
	resp, err := o.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("onvif gotopreset http %d: %s", resp.StatusCode, string(buf))
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if bytes.Contains(raw, []byte("<Fault")) {
		return fmt.Errorf("onvif gotopreset fault: %s", truncate(string(raw), 256))
	}
	// refresh cached session so the profile we just used is recorded
	o.mu.Lock()
	s, ok := o.cached[id]
	if ok {
		s.profileToken = profileToken
	}
	o.mu.Unlock()
	return nil
}

// sendSoap POSTs `body` to `deviceURL` with WS-Security UsernameToken
// auth, then verifies the SOAP response is a fault-free 200.
func (o *ONVIFController) sendSoap(ctx context.Context, deviceURL, user, pass, body string) error {
	// Inject WS-Security header before <soap:Body>.
	body = strings.Replace(body, "<soap:Body>", wsseHeader(user, pass)+"<soap:Body>", 1)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceURL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")
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

// wsseHeader returns a SOAP <soap:Header> block containing a
// WS-Security UsernameToken with PasswordDigest. ONVIF spec requires
// WS-Security for all authenticated requests. Most Hikvision / Dahua
// firmware rejects PasswordText and requires the digest form:
//
//	digest = Base64( SHA1( nonce + created + password ) )
//
// The nonce is random 16 bytes; the created timestamp is UTC ISO-8601.
func wsseHeader(user, pass string) string {
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	created := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	hasher := sha1.New()
	hasher.Write(nonce)
	hasher.Write([]byte(created))
	hasher.Write([]byte(pass))
	digest := base64.StdEncoding.EncodeToString(hasher.Sum(nil))
	nonceB64 := base64.StdEncoding.EncodeToString(nonce)
	return fmt.Sprintf(`<soap:Header>
  <wsse:Security xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd" xmlns:wsu="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">
    <wsse:UsernameToken>
      <wsse:Username>%s</wsse:Username>
      <wsse:Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">%s</wsse:Password>
      <wsse:Nonce EncodingType="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary">%s</wsse:Nonce>
      <wsu:Created>%s</wsu:Created>
    </wsse:UsernameToken>
  </wsse:Security>
</soap:Header>
  `, xmlEscape(user), digest, nonceB64, created)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
