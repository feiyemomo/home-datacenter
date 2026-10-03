package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
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
	cache    string
	cachedAt time.Time
	cacheTTL time.Duration

	// mu protects cache and cachedAt, which are read and written
	// by concurrent HTTP handlers.
	mu sync.RWMutex
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
	h.mu.RLock()
	cached := h.cache
	cachedAt := h.cachedAt
	h.mu.RUnlock()
	if cached != "" && time.Since(cachedAt) < h.cacheTTL {
		var data interface{}
		if err := json.Unmarshal([]byte(cached), &data); err == nil {
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

	// Build the wttr.in URL. We use url.URL + url.PathEscape so a
	// spoofed X-Forwarded-For value (which c.ClientIP() trusts) can't
	// inject path components or query strings into the request.
	target := h.defaultLocation
	if isPublicIP(clientIP) {
		target = clientIP
	}
	u := &url.URL{
		Scheme:   "https",
		Host:     "wttr.in",
		Path:     "/" + url.PathEscape(target),
		RawQuery: "format=j1",
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to build weather request")
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "HomeDatacenter/1.0")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		log.Printf("[handler] weather service unavailable: %v", err)
		h.mu.RLock()
		cached := h.cache
		h.mu.RUnlock()
		if cached != "" {
			var data interface{}
			if err := json.Unmarshal([]byte(cached), &data); err == nil {
				utils.Success(c, data)
				return
			}
		}
		utils.Fail(c, http.StatusBadGateway, "weather service unavailable")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		log.Printf("[handler] weather service returned status %d: %s", resp.StatusCode, string(body))
		h.mu.RLock()
		cached := h.cache
		h.mu.RUnlock()
		if cached != "" {
			var data interface{}
			if err := json.Unmarshal([]byte(cached), &data); err == nil {
				utils.Success(c, data)
				return
			}
		}
		utils.Fail(c, http.StatusBadGateway,
			fmt.Sprintf("weather service returned status %d", resp.StatusCode))
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to read weather response")
		return
	}

	// Cache the raw JSON body.
	h.mu.Lock()
	h.cache = string(body)
	h.cachedAt = time.Now()
	h.mu.Unlock()

	// Parse and re-wrap in our envelope.
	var data interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to parse weather response")
		return
	}

	utils.Success(c, data)
}

// isPublicIP reports whether ipStr is a public (non-RFC1918/non-loopback/
// non-link-local/non-unspecified) IPv4 or IPv6 address. We only forward
// public IPs to wttr.in for geolocation — private LAN IPs would confuse
// wttr.in (it would try to locate 192.168.x.y and fall back to its own
// server location).
//
// Unlike naive string-prefix matching, net.ParseIP fully validates the
// address and rejects anything that isn't a well-formed IP literal, so
// a spoofed X-Forwarded-For value like "192.168.1.1/../../etc" can't
// sneak past the check.
func isPublicIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return false
	}
	return true
}
