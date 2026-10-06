package camera

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
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

// randomHex returns n random bytes hex-encoded, used to build the
// Digest `cnonce` value.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// audioElementRe matches the <Audio> block of a StreamingChannel document.
// The namespace/version attributes live on the ROOT element, so the block
// itself carries no attributes and this pattern is safe to reuse verbatim.
var audioElementRe = regexp.MustCompile(`(?is)<Audio\b[^>]*>.*?</Audio>`)

// audioEnabledInnerRe matches the <enabled> child of an <Audio> block,
// tolerating surrounding whitespace/newlines and any attributes.
var audioEnabledInnerRe = regexp.MustCompile(`(?is)<enabled\b[^>]*>\s*(?:true|false)\s*</enabled>`)

// SetCameraMicEnabled enables or disables the onboard microphone (audio
// pickup) of a Hikvision-compatible camera over ISAPI.
//
// Why this GET-merge-PUT dance: the obvious-looking write endpoints do
// NOT exist on Hikvision firmware. Verified against live DS-2CD units
// (2026-10-06):
//
//	PUT /ISAPI/Streaming/channels/101/audio -> 403 "Invalid Operation"
//	PUT /ISAPI/System/Audio/channels/1      -> 403 "Invalid Operation"
//
// Both read fine but reject writes. The ONLY writable surface is the
// FULL StreamingChannel document:
//
//	GET /ISAPI/Streaming/channels/{id}  -> full <StreamingChannel> XML
//	PUT /ISAPI/Streaming/channels/{id}  -> 200 <statusCode>1</statusCode>
//
// so we read the channel document, flip `<Audio><enabled>` inside it,
// and write the whole document back. Writing a partial document is
// rejected outright, and switching `<enabled>` off removes the audio
// track from the RTSP stream (verified with ffprobe: a PCMA audio track
// disappears within ~1s), which is what actually silences both the live
// preview and Frigate's recorder.
//
// The former implementation silently did nothing: it PUT a 2-field
// AudioChannel to two endpoints that reject writes and a third that does
// not exist, so the camera kept its microphone live while the dashboard
// reported "拾音已关闭" — the exact "turning pickup off still records
// complete audio" symptom.
//
// Channel IDs: a Hikvision device exposes the main stream as
// (cameraChannel+1)*100+1 (101) and the sub stream as +2 (102). Every
// channel that carries an <Audio> element gets the change, because live
// playback may use either channel depending on the camera's
// transcode-use-substream flag.
func SetCameraMicEnabled(ctx context.Context, host string, port int, user, pass string, enabled bool, channel int) {
	if host == "" || user == "" {
		return
	}
	if port <= 0 {
		port = 80
	}
	if channel <= 0 {
		channel = 1
	}
	mainID := channel*100 + 1
	if channel > 100 {
		mainID = channel // already a full streaming-channel ID (e.g. 101)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	base := fmt.Sprintf("http://%s:%d", host, port)

	// Discover the streaming channels actually exposed by this camera.
	ids := isapiStreamingChannelIDs(ctx, client, base, user, pass, mainID)
	applied := 0
	for _, id := range ids {
		if isapiSetChannelAudio(ctx, client, base, user, pass, id, enabled) {
			applied++
		}
	}

	// Also flip the device-level audio input channel. This is often
	// read-only, so a failure is logged at debug level only.
	isapiSetSystemAudioEnabled(ctx, client, base, user, pass, enabled)

	if applied == 0 {
		log.Printf("isapi: camera %s:%d: mic %t: no streaming channel accepted the change", host, port, enabled)
		return
	}
	log.Printf("isapi: camera %s:%d: mic (audio pickup) %s on %d channel(s) %v", host, port,
		map[bool]string{true: "enabled", false: "disabled"}[enabled], applied, ids)
}

// isapiStreamingChannelIDs lists the streaming channels of a camera via
// /ISAPI/Streaming/channels, falling back to the conventional
// main/sub pair (e.g. 101 and 102) when discovery is unavailable.
func isapiStreamingChannelIDs(ctx context.Context, client *http.Client, base, user, pass string, mainID int) []int {
	body, status, err := isapiRequest(ctx, client, http.MethodGet, base+"/ISAPI/Streaming/channels", user, pass, "")
	if err == nil && status < 300 {
		ids := make([]int, 0, 2)
		for _, m := range regexp.MustCompile(`(?is)<id>\s*(\d+)\s*</id>`).FindAllStringSubmatch(body, -1) {
			if id, cerr := strconv.Atoi(m[1]); cerr == nil {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			return ids
		}
	}
	ids := []int{mainID}
	if mainID%10 == 1 {
		ids = append(ids, mainID+1)
	}
	return ids
}

// isapiSetChannelAudio flips <Audio><enabled> on one streaming channel
// using the GET-merge-PUT flow. Returns true when the device confirmed
// the write.
func isapiSetChannelAudio(ctx context.Context, client *http.Client, base, user, pass string, id int, enabled bool) bool {
	path := fmt.Sprintf("/ISAPI/Streaming/channels/%d", id)
	body, status, err := isapiRequest(ctx, client, http.MethodGet, base+path, user, pass, "")
	if err != nil || status >= 300 {
		log.Printf("isapi: camera %s%s GET failed: status=%d err=%v", base, path, status, err)
		return false
	}
	if !audioElementRe.MatchString(body) {
		// No audio block on this channel (e.g. a video-only channel).
		return false
	}
	mutated, changed := isapiApplyAudioEnabled(body, enabled)
	if !changed {
		log.Printf("isapi: camera %s%s has no <enabled> inside <Audio>; skipped", base, path)
		return false
	}
	respBody, status, err := isapiRequest(ctx, client, http.MethodPut, base+path, user, pass, mutated)
	if err != nil || status >= 300 || !strings.Contains(respBody, "<statusCode>1</statusCode>") {
		log.Printf("isapi: camera %s%s PUT audio=%t rejected: status=%d err=%v body=%s",
			base, path, enabled, status, err, truncateForLog(respBody))
		return false
	}
	return true
}

// isapiApplyAudioEnabled rewrites the <enabled> child of the <Audio>
// block in a StreamingChannel XML document. Pure function so it can be
// unit-tested without a camera.
func isapiApplyAudioEnabled(doc string, enabled bool) (string, bool) {
	want := "false"
	if enabled {
		want = "true"
	}
	loc := audioElementRe.FindStringIndex(doc)
	if loc == nil {
		return doc, false
	}
	block := doc[loc[0]:loc[1]]
	if !audioEnabledInnerRe.MatchString(block) {
		return doc, false
	}
	newBlock := audioEnabledInnerRe.ReplaceAllStringFunc(block, func(string) string {
		return "<enabled>" + want + "</enabled>"
	})
	return doc[:loc[0]] + newBlock + doc[loc[1]:], true
}

// isapiSetSystemAudioEnabled attempts to disable the device-level audio
// input channel. Best-effort: many firmwares answer 403 here, in which
// case the per-streaming-channel flag above is the effective control.
func isapiSetSystemAudioEnabled(ctx context.Context, client *http.Client, base, user, pass string, enabled bool) {
	payload := fmt.Sprintf(`<AudioChannel xmlns="http://www.hikvision.com/ver20/XMLSchema" version="2.0"><id>1</id><enabled>%t</enabled></AudioChannel>`, enabled)
	_, status, err := isapiRequest(ctx, client, http.MethodPut, base+"/ISAPI/System/Audio/channels/1", user, pass, payload)
	if err != nil || status >= 300 {
		log.Printf("isapi: camera %s system audio channel is read-only (status=%d err=%v)", base, status, err)
	}
}

// isapiRequest performs one authenticated ISAPI call, retrying once with
// a Digest Authorization header when the camera answers 401 with a
// Digest challenge. Returns the body, the HTTP status and a transport
// error (if any).
func isapiRequest(ctx context.Context, client *http.Client, method, target, user, pass, body string) (string, int, error) {
	req, err := newISAPIRequest(ctx, method, target, user, pass, body, "")
	if err != nil {
		return "", 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		return string(raw), resp.StatusCode, nil
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(challenge)), "digest ") {
		return string(raw), resp.StatusCode, nil
	}
	digestHeader := buildDigestAuthHeader(method, targetPath(target), user, pass, challenge)
	if digestHeader == "" {
		return string(raw), resp.StatusCode, nil
	}
	req2, err := newISAPIRequest(ctx, method, target, user, pass, body, digestHeader)
	if err != nil {
		return string(raw), resp.StatusCode, err
	}
	resp2, err := client.Do(req2)
	if err != nil {
		return "", 0, err
	}
	raw2, _ := io.ReadAll(io.LimitReader(resp2.Body, 1<<20))
	resp2.Body.Close()
	return string(raw2), resp2.StatusCode, nil
}

func newISAPIRequest(ctx context.Context, method, target, user, pass, body, authHeader string) (*http.Request, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/xml")
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	} else {
		req.SetBasicAuth(user, pass)
	}
	return req, nil
}

// targetPath returns the path portion of an absolute URL for use as the
// Digest `uri` value.
func targetPath(target string) string {
	if u, err := url.Parse(target); err == nil && u.Path != "" {
		if u.RawQuery != "" {
			return u.Path + "?" + u.RawQuery
		}
		return u.Path
	}
	return target
}

func truncateForLog(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
