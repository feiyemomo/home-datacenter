package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
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
		defaultLocation: "Chencang",
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
	// Weather target location: prefer fixed Chencang (陈仓区) to avoid
	// ISP carrier IP geolocation drifting to nearby township tags (e.g. Mayingchen).
	target := h.defaultLocation
	loc := strings.TrimSpace(c.Query("location"))
	if loc != "" {
		target = loc
	}
	isDefault := loc == "" || strings.EqualFold(loc, h.defaultLocation)

	// Serve from cache if fresh — only for default location to avoid
	// location cross-contamination between different queries.
	if isDefault {
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
	}

	u := &url.URL{
		Scheme:   "https",
		Host:     "wttr.in",
		Path:     "/" + url.PathEscape(target),
		// lang=zh asks wttr.in to localise weatherDesc / lang_zh
		// fields. The app UI is Chinese, so without it the dashboard
		// mixed an English "Clear " summary into an otherwise Chinese
		// card. wttr.in falls back to English for unknown locales.
		RawQuery: "format=j1&lang=zh",
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
		if isDefault {
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
		}
		utils.Fail(c, http.StatusBadGateway, "weather service unavailable")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		log.Printf("[handler] weather service returned status %d: %s", resp.StatusCode, string(body))
		if isDefault {
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

	// Cache only default location results.
	if isDefault {
		h.mu.Lock()
		h.cache = string(body)
		h.cachedAt = time.Now()
		h.mu.Unlock()
	}

	// Parse and re-wrap in our envelope.
	var data interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		utils.Fail(c, http.StatusInternalServerError, "failed to parse weather response")
		return
	}

	utils.Success(c, data)
}
