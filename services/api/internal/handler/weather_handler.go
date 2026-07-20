package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/utils"
)

// WeatherHandler proxies wttr.in for the Android app.
//
//	Route: GET /api/v1/weather
//
// We proxy (rather than letting the app hit wttr.in directly) so that:
//  1. The app doesn't need to know wttr.in's URL or handle HTTPS
//     cert pinning for a third-party domain.
//  2. We can fall back to a default location (陕西宝鸡) when the
//     client IP can't be geolocated — the app sees a stable
//     response shape regardless.
//  3. We can cache the response briefly server-side to avoid
//     hammering wttr.in on every app open (wttr.in is a free
//     service and rate-limits aggressive callers).
//
// The response is the wttr.in JSON format (j1) wrapped in the
// standard {code, message, data} envelope.
type WeatherHandler struct {
	// defaultLocation is used when no location can be inferred
	// from the client IP. Set to 陕西宝鸡 (Baoji, Shaanxi) per
	// operator config.
	defaultLocation string

	// httpClient has a generous timeout — wttr.in is sometimes
	// slow on cold caches.
	httpClient *http.Client

	// cache holds the last successful response. wttr.in updates
	// at most once per ~10 min, so a 5-min TTL is safe.
	cache     string
	cachedAt  time.Time
	cacheTTL  time.Duration
}

// NewWeatherHandler creates a weather proxy handler.
func NewWeatherHandler() *WeatherHandler {
	return &WeatherHandler{
		defaultLocation: "Baoji",
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
		cacheTTL: 5 * time.Minute,
	}
}

// Weather godoc
//
//	@Summary	Get current weather (proxied from wttr.in)
//	@Tags		weather
//	@Produce	json
//	@Success	200	{object}	utils.ApiResponse
//	@Router		/api/v1/weather [get]
func (h *WeatherHandler) Weather(c *gin.Context) {
	// Serve from cache if fresh — avoids hitting wttr.in on every
	// app open.
	if h.cache != "" && time.Since(h.cachedAt) < h.cacheTTL {
		var data interface{}
		if err := json.Unmarshal([]byte(h.cache), &data); err == nil {
			utils.Success(c, data)
			return
		}
	}

	// Use the client's public IP for geolocation. wttr.in reads
	// the X-Forwarded-For header (or the TCP remote addr) and
	// geolocates automatically — we just need to forward the
	// caller's IP so that LAN clients (which share the server's
	// public IP via NAT) are located correctly.
	clientIP := c.ClientIP()

	url := fmt.Sprintf("https://wttr.in/%s?format=j1", h.defaultLocation)
	if clientIP != "" && clientIP != "127.0.0.1" && clientIP != "::1" {
		// For LAN/private IPs, wttr.in would locate the server's
		// public IP anyway — so we just use the default location.
		// For public IPs, we let wttr.in geolocate.
		if isPublicIP(clientIP) {
			url = fmt.Sprintf("https://wttr.in/%s?format=j1", clientIP)
		}
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, url, nil)
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to build weather request")
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "HomeDatacenter/1.0")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		utils.Fail(c, http.StatusBadGateway, "weather service unavailable: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		utils.Fail(c, http.StatusBadGateway,
			fmt.Sprintf("wttr.in returned %d: %s", resp.StatusCode, string(body)))
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to read weather response")
		return
	}

	// Cache the raw JSON body.
	h.cache = string(body)
	h.cachedAt = time.Now()

	// Parse and re-wrap in our envelope.
	var data interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to parse weather response")
		return
	}

	utils.Success(c, data)
}

// isPublicIP reports whether ip is a public (non-RFC1918/non-loopback)
// IPv4 or IPv6 address. We only forward public IPs to wttr.in for
// geolocation — private LAN IPs would confuse wttr.in (it would
// try to locate 192.168.x.y and fall back to its own server location).
func isPublicIP(ip string) bool {
	if ip == "" {
		return false
	}
	// Quick check: reject obvious private ranges. The full check
	// is more involved, but this covers the common cases.
	switch {
	case ip == "127.0.0.1", ip == "::1":
		return false
	case len(ip) >= 8 && ip[:8] == "192.168.":
		return false
	case len(ip) >= 3 && ip[:3] == "10.":
		return false
	case len(ip) >= 7 && ip[:7] == "172.16." || len(ip) >= 7 && ip[:7] == "172.17." ||
		len(ip) >= 7 && ip[:7] == "172.18." || len(ip) >= 7 && ip[:7] == "172.19." ||
		len(ip) >= 7 && ip[:7] == "172.20." || len(ip) >= 7 && ip[:7] == "172.21." ||
		len(ip) >= 7 && ip[:7] == "172.22." || len(ip) >= 7 && ip[:7] == "172.23." ||
		len(ip) >= 7 && ip[:7] == "172.24." || len(ip) >= 7 && ip[:7] == "172.25." ||
		len(ip) >= 7 && ip[:7] == "172.26." || len(ip) >= 7 && ip[:7] == "172.27." ||
		len(ip) >= 7 && ip[:7] == "172.28." || len(ip) >= 7 && ip[:7] == "172.29." ||
		len(ip) >= 7 && ip[:7] == "172.30." || len(ip) >= 7 && ip[:7] == "172.31.":
		return false
	case len(ip) >= 5 && ip[:5] == "169.2":
		// 169.254.x.y link-local
		return false
	case len(ip) >= 4 && ip[:4] == "fc00" || len(ip) >= 4 && ip[:4] == "fd00":
		// ULA
		return false
	case len(ip) >= 4 && ip[:4] == "fe80":
		// link-local IPv6
		return false
	}
	return true
}
