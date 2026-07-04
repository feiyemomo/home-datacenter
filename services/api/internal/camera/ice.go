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
