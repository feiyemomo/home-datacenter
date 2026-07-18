package camera

import (
	"encoding/json"
	"net/url"
	"strings"
)

// IceServer mirrors the browser's RTCIceServer shape so the JSON
// we hand to the front-end is a drop-in for `new RTCPeerConnection({iceServers: [...]})`.
type IceServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// UnmarshalJSON accepts both the single-string form
// (`{"urls":"stun:..."}`) and the array form
// (`{"urls":["stun:...","turn:..."]}`) for `urls`, matching the
// W3C RTCIceServer spec. The config file commonly uses the
// single-string form, while the array form is used for multi-URL
// TURN servers.
func (s *IceServer) UnmarshalJSON(b []byte) error {
	type alias struct {
		URLs       json.RawMessage `json:"urls"`
		Username   string          `json:"username"`
		Credential string          `json:"credential"`
	}
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	s.Username = a.Username
	s.Credential = a.Credential
	if len(a.URLs) == 0 {
		return nil
	}
	// Try array first (most common in our codebase), then fall
	// back to single string. A quoted string fails to unmarshal
	// into []string, so we use the failure as the signal.
	var arr []string
	if err := json.Unmarshal(a.URLs, &arr); err == nil {
		s.URLs = arr
		return nil
	}
	var single string
	if err := json.Unmarshal(a.URLs, &single); err == nil {
		s.URLs = []string{single}
		return nil
	}
	return nil
}

// IceConfig is what the handler returns from
// GET /api/v1/cameras/ice.
//
//	{ "ice_servers": [...], "webrtc_base": "https://cam.example.com" }
//
// The front-end combines `webrtc_base` with the per-stream
// `?src=cam_N` it already received in /cameras/:id.
type IceConfig struct {
	ICEServers []IceServer `json:"ice_servers"`
	WebRTCBase string      `json:"webrtc_base"`
}

// BuildIceConfig parses the (possibly empty) JSON blob from
// `camera.ice_servers` and combines it with `webrtc_public_base`.
// A blank `webrtc_public_base` means "use the LAN in-network URL".
func BuildIceConfig(rawIce, publicBase, lanBase string) IceConfig {
	cfg := IceConfig{WebRTCBase: chooseBase(publicBase, lanBase)}
	if strings.TrimSpace(rawIce) == "" {
		return cfg
	}
	var list []IceServer
	if err := json.Unmarshal([]byte(rawIce), &list); err != nil {
		return cfg
	}
	// Drop empty URLs defensively.
	clean := make([]IceServer, 0, len(list))
	for _, s := range list {
		good := s.URLs[:0]
		for _, u := range s.URLs {
			if _, err := url.Parse(u); err == nil {
				good = append(good, u)
			}
		}
		if len(good) > 0 {
			s.URLs = good
			clean = append(clean, s)
		}
	}
	cfg.ICEServers = clean
	return cfg
}

func chooseBase(public, lan string) string {
	public = strings.TrimRight(strings.TrimSpace(public), "/")
	if public != "" {
		return public
	}
	return strings.TrimRight(strings.TrimSpace(lan), "/")
}
