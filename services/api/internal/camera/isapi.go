package camera

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// BoostSpeakerVolume attempts to set the hardware speaker volume of a
// Hikvision or compatible ISAPI camera to 100% (maximum hardware amplifier gain).
func BoostSpeakerVolume(ctx context.Context, host string, port int, user, pass string) {
	if host == "" || user == "" {
		return
	}
	if port <= 0 {
		port = 80
	}

	endpoints := []struct {
		path string
		body string
	}{
		{
			path: "/ISAPI/System/TwoWayAudio/channels/1",
			body: `<TwoWayAudioChannel xmlns="http://www.hikvision.com/ver20/XMLSchema" version="2.0"><id>1</id><speakerVolume>100</speakerVolume></TwoWayAudioChannel>`,
		},
		{
			path: "/ISAPI/System/Audio/channels/1",
			body: `<AudioChannel xmlns="http://www.hikvision.com/ver20/XMLSchema" version="2.0"><id>1</id><audioOutputVolume>100</audioOutputVolume></AudioChannel>`,
		},
	}

	client := &http.Client{Timeout: 5 * time.Second}

	for _, ep := range endpoints {
		urlStr := fmt.Sprintf("http://%s:%d%s", host, port, ep.path)
		go func(targetURL, reqBody, path string) {
			reqCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			req, err := http.NewRequestWithContext(reqCtx, http.MethodPut, targetURL, strings.NewReader(reqBody))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/xml")

			// First request: can attempt Basic or trigger 401 Digest challenge
			req.SetBasicAuth(user, pass)
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
				log.Printf("isapi: camera %s:%d %s speakerVolume set to 100 (HTTP %d)", host, port, path, resp.StatusCode)
				return
			}

			if resp.StatusCode == http.StatusUnauthorized {
				authHeader := resp.Header.Get("WWW-Authenticate")
				if strings.HasPrefix(strings.ToLower(authHeader), "digest ") {
					digestHeader := buildDigestAuthHeader(http.MethodPut, path, user, pass, authHeader)
					if digestHeader != "" {
						req2, err := http.NewRequestWithContext(reqCtx, http.MethodPut, targetURL, strings.NewReader(reqBody))
						if err == nil {
							req2.Header.Set("Content-Type", "application/xml")
							req2.Header.Set("Authorization", digestHeader)
							resp2, err := client.Do(req2)
							if err == nil {
								defer resp2.Body.Close()
								if resp2.StatusCode == http.StatusOK || resp2.StatusCode == http.StatusNoContent {
									log.Printf("isapi: camera %s:%d %s speakerVolume set to 100 via Digest (HTTP %d)", host, port, path, resp2.StatusCode)
								}
							}
						}
					}
				}
			}
		}(urlStr, ep.body, ep.path)
	}
}

func buildDigestAuthHeader(method, uri, user, pass, challenge string) string {
	parts := parseDigestChallenge(challenge)
	realm := parts["realm"]
	nonce := parts["nonce"]
	qop := parts["qop"]
	opaque := parts["opaque"]
	algorithm := strings.ToUpper(parts["algorithm"])

	if realm == "" || nonce == "" {
		return ""
	}

	ha1 := md5Hex(fmt.Sprintf("%s:%s:%s", user, realm, pass))
	ha2 := md5Hex(fmt.Sprintf("%s:%s", method, uri))

	var response string
	nc := "00000001"
	cnonce := randomHex(8)

	if strings.Contains(qop, "auth") {
		response = md5Hex(fmt.Sprintf("%s:%s:%s:%s:auth:%s", ha1, nonce, nc, cnonce, ha2))
		header := fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", qop=auth, nc=%s, cnonce="%s", response="%s"`,
			user, realm, nonce, uri, nc, cnonce, response)
		if opaque != "" {
			header += fmt.Sprintf(`, opaque="%s"`, opaque)
		}
		if algorithm != "" {
			header += fmt.Sprintf(`, algorithm=%s`, algorithm)
		}
		return header
	}

	response = md5Hex(fmt.Sprintf("%s:%s:%s", ha1, nonce, ha2))
	header := fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`,
		user, realm, nonce, uri, response)
	if opaque != "" {
		header += fmt.Sprintf(`, opaque="%s"`, opaque)
	}
	return header
}

func parseDigestChallenge(header string) map[string]string {
	res := make(map[string]string)
	header = strings.TrimPrefix(header, "Digest ")
	header = strings.TrimPrefix(header, "digest ")
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 {
			k := strings.TrimSpace(kv[0])
			v := strings.Trim(strings.TrimSpace(kv[1]), `"`)
			res[k] = v
		}
	}
	return res
}

func md5Hex(data string) string {
	h := md5.Sum([]byte(data))
	return hex.EncodeToString(h[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SetCameraMicEnabled attempts to enable or disable the onboard microphone (audio pickup)
// on Hikvision or compatible ISAPI cameras via hardware REST calls.
func SetCameraMicEnabled(ctx context.Context, host string, port int, user, pass string, enabled bool) {
	if host == "" || user == "" {
		return
	}
	if port <= 0 {
		port = 80
	}

	vol := 100
	if !enabled {
		vol = 0
	}

	endpoints := []struct {
		path string
		body string
	}{
		{
			path: "/ISAPI/System/Audio/channels/1",
			body: fmt.Sprintf(`<AudioChannel xmlns="http://www.hikvision.com/ver20/XMLSchema" version="2.0"><id>1</id><enabled>%t</enabled><audioInputVolume>%d</audioInputVolume></AudioChannel>`, enabled, vol),
		},
		{
			path: "/ISAPI/Streaming/channels/101/audio",
			body: fmt.Sprintf(`<AudioChannel xmlns="http://www.hikvision.com/ver20/XMLSchema" version="2.0"><enabled>%t</enabled></AudioChannel>`, enabled),
		},
	}

	client := &http.Client{Timeout: 5 * time.Second}

	for _, ep := range endpoints {
		urlStr := fmt.Sprintf("http://%s:%d%s", host, port, ep.path)
		go func(targetURL, reqBody, path string) {
			reqCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			req, err := http.NewRequestWithContext(reqCtx, http.MethodPut, targetURL, strings.NewReader(reqBody))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/xml")
			req.SetBasicAuth(user, pass)
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
				log.Printf("isapi: camera %s:%d %s micEnabled set to %t (HTTP %d)", host, port, path, enabled, resp.StatusCode)
				return
			}

			if resp.StatusCode == http.StatusUnauthorized {
				authHeader := resp.Header.Get("WWW-Authenticate")
				if strings.HasPrefix(strings.ToLower(authHeader), "digest ") {
					digestHeader := buildDigestAuthHeader(http.MethodPut, path, user, pass, authHeader)
					if digestHeader != "" {
						req2, err := http.NewRequestWithContext(reqCtx, http.MethodPut, targetURL, strings.NewReader(reqBody))
						if err == nil {
							req2.Header.Set("Content-Type", "application/xml")
							req2.Header.Set("Authorization", digestHeader)
							resp2, err := client.Do(req2)
							if err == nil {
								defer resp2.Body.Close()
								if resp2.StatusCode == http.StatusOK || resp2.StatusCode == http.StatusNoContent {
									log.Printf("isapi: camera %s:%d %s micEnabled set to %t via Digest (HTTP %d)", host, port, path, enabled, resp2.StatusCode)
								}
							}
						}
					}
				}
			}
		}(urlStr, ep.body, ep.path)
	}
}
