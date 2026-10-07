package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/service"
	"home-datacenter-api/internal/utils"
)

type AuthHandler struct {
	authService *service.AuthService
	bus         *eventbus.Bus
}

func NewAuthHandler(
	authService *service.AuthService,
	bus *eventbus.Bus,
) *AuthHandler {
	return &AuthHandler{
		authService: authService,
		bus:         bus,
	}
}

type BindRequest struct {
	UserID    uint   `json:"user_id" binding:"required"`
	AccessKey string `json:"access_key" binding:"required"`
}

// Bind exchanges (user_id, access_key) for a long-lived JWT.
//
//	Route: POST /api/v1/auth/bind
//
// Security: deliberately returns a generic "invalid credentials" for
// all bind failures (bad user_id, wrong key, revoked device). A
// distinct message per failure would let an attacker enumerate which
// user IDs exist and which keys are valid.
//
// On success the response is twofold:
//
//  1. JSON body with the token (dashboard stores it in localStorage
//     for `Authorization: Bearer <jwt>` on /api/ calls).
//  2. Set-Cookie: home_token=<jwt> (the browser auto-sends it on
//     same-origin navigations to /frigate/, /go2rtc/, etc., which
//     are gated by nginx's `auth_request /api/v1/auth/verify`
//     subrequest. The subrequest only sees the original request's
//     headers — Authorization is added by the SPA's axios/fetch
//     interceptors, but a raw browser navigation to /frigate/
//     carries no Authorization. The cookie bridges that gap so the
//     operator can click a sidebar link to open Frigate's UI
//     without re-logging in.)
//
// The cookie is HttpOnly (prevents XSS theft) and serves as a secondary
// auth channel for sub-resource requests (e.g. <img> tags, /go2rtc/, /frigate/).
// The primary token store is localStorage + Authorization: Bearer header.
// SameSite=Lax is enough: it blocks cross-site XHR/fetch but
// allows top-level navigations (which is how the dashboard opens
// /frigate/).
func (h *AuthHandler) Bind(c *gin.Context) {

	var req BindRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}

	token, device, err := h.authService.Bind(req.UserID, req.AccessKey)
	if err != nil {
		utils.FailWithCode(c, http.StatusUnauthorized, utils.ErrAuthInvalidCredentials, "invalid credentials")
		return
	}

	// Publish user.login so the log subscriber can persist a
	// human-readable "用户 admin 登录" entry and the WS Hub can
	// push it to connected dashboards.
	if h.bus != nil && device != nil {
		payload, _ := json.Marshal(eventbus.UserLoginPayload{
			UserID:     req.UserID,
			DeviceID:   device.ID,
			DeviceName: device.DeviceName,
			Ts:         time.Now().Unix(),
		})
		h.bus.Publish(eventbus.Event{
			Topic:   eventbus.TopicUserLogin,
			Payload: payload,
			Source:  eventbus.SourceSystem,
		})
	}

	// 365 days, matching the JWT's exp claim. SameSite=Lax is
	// the right choice: top-level navigations (which is the only
	// way the dashboard reaches /frigate/) carry the cookie,
	// while cross-site XHR/fetch (the only path an attacker would
	// use to ride the cookie) is blocked by the browser.
	// The cookie is also HttpOnly — localStorage is the primary
	// token store and the cookie is a secondary channel.
	//
	// The Secure flag is configurable (server.secure_cookie). It
	// defaults to false because the LAN dashboard is served over
	// plain HTTP, where a Secure cookie would be silently dropped
	// by the browser and break the /frigate/ & /go2rtc/
	// auth_request navigations. HTTPS-only deployments (e.g.
	// Cloudflare Tunnel) opt in to prevent the cookie from ever
	// being sent over an unencrypted hop — that's the right
	// trade-off once TLS terminates the connection end-to-end.
	const maxAge = 365 * 24 * 60 * 60
	secureCookie := false
	if config.AppConfig != nil {
		secureCookie = config.AppConfig.Server.SecureCookie
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("home_token", token, maxAge, "/", "", secureCookie, true)

	utils.Success(c, gin.H{
		"token": token,
	})
}

// Verify validates a JWT without exposing user details.
//
//	Route: GET /api/v1/auth/verify
//
// This endpoint exists so the dashboard's nginx fronting the go2rtc
// reverse-proxy (/go2rtc/) can perform an `auth_request` sub-call
// before letting a request through. Without it, the /go2rtc/
// location is wide-open: any browser pointed at
// https://cam.feiyemomo.top/go2rtc/api/streams could list every
// camera and pull live frames, because go2rtc itself has no auth.
//
// The verify call is cheap (single-row lookup) and runs in-process,
// so it's safe to gate every go2rtc sub-request on it.
//
// Returns:
//
//	200 OK with {"user_id":N,"device_id":M,"valid":true}  on success
//	401 Unauthorized                                         on bad/expired token
//	401 Unauthorized                                         on revoked device
//
// We deliberately do NOT set Cache-Control on the success response.
// nginx auth_request can cache a 200 for a sub-second window with
// `proxy_cache_valid`, but a revoked device that flipped state would
// stay valid for that window — and on a home camera the window is
// the only thing between "the user just revoked their stolen
// laptop" and "the thief keeps watching". Leaving cache headers off
// is the safe default.
func (h *AuthHandler) Verify(c *gin.Context) {
	// nginx's `auth_request` directive sub-calls this endpoint
	// with the ORIGINAL request's method, headers, AND
	// Content-Length — but the body itself is dropped before
	// the sub-request goes out. The result is a malformed
	// HTTP request: a GET with `Content-Length: 367` and an
	// immediately-EOF body. Go's net/http strictly honors
	// Content-Length and blocks the read for the full 60s
	// keepalive window before returning `unexpected EOF`,
	// which surfaces to the client as a 60s `auth_request`
	// 500. (Verified by adding timing logs in this handler:
	// `enter method=GET cl=367` followed by
	// `drain done in 1m0s (n=0 err=unexpected EOF)`.)
	//
	// The fix: discard the body entirely. The /auth/verify
	// handler only needs the Authorization header and a
	// single device row, so any forwarded body is by
	// definition garbage from a misbehaving sub-request
	// machinery. Setting `Body = http.NoBody` skips both
	// the read AND the keepalive wait.
	c.Request.Body = http.NoBody

	// Token resolution order:
	//  1. `Authorization: Bearer <jwt>` (the SPA's axios/fetch
	//     interceptors add this on /api/ calls).
	//  2. `Cookie: home_token=<jwt>` (the browser auto-sends
	//     this on top-level navigations like clicking a link to
	//     /frigate/ or /go2rtc/. Without this fallback a raw
	//     navigation to /frigate/ would 401 because the
	//     Authorization header is only added by the SPA's JS,
	//     and the auth_request subrequest is plain nginx —
	//     no JS hooks. See Bind's comment for the full
	//     rationale.)
	//
	// The two paths are kept separate so we can later add
	// per-source revocation (e.g. "force re-auth on cookie
	// but keep Authorization valid for service-to-service
	// calls"). For now both yield the same token and the
	// downstream checks are identical.
	tokenString := ""
	if authHeader := c.GetHeader("Authorization"); authHeader != "" && strings.HasPrefix(authHeader, "Bearer ") {
		tokenString = strings.TrimPrefix(authHeader, "Bearer ")
	}
	if tokenString == "" {
		if cookie, err := c.Cookie("home_token"); err == nil {
			tokenString = cookie
		}
	}
	if tokenString == "" {
		utils.FailWithCode(c, http.StatusUnauthorized, utils.ErrAuthMissing, "missing or invalid Authorization header")
		return
	}

	claims, err := utils.ParseToken(tokenString)
	if err != nil {
		utils.FailWithCode(c, http.StatusUnauthorized, utils.ErrAuthTokenInvalid, "invalid token")
		return
	}

	// Reuse the same revocation check as the JWT middleware. We do
	// NOT call JWTAuth() directly because the middleware writes a
	// 401 with a custom JSON body; we want a clean 200/401 contract
	// for nginx auth_request. A revoked device must be 401, not 200,
	// otherwise nginx forwards the request to go2rtc.
	device, err := h.authService.GetDeviceForAuth(claims.DeviceID)
	if err != nil {
		utils.FailWithCode(c, http.StatusUnauthorized, utils.ErrAuthDeviceLookupFailed, "device lookup failed")
		return
	}
	if device.RevokedAt.Valid {
		utils.FailWithCode(c, http.StatusUnauthorized, utils.ErrAuthDeviceRevoked, "device revoked")
		return
	}

	// Token version check: if the admin has rotated the token, reject
	// old tokens here too (mirrors the JWTAuth middleware check).
	if claims.TokenVersion < device.TokenVersion {
		utils.FailWithCode(c, http.StatusUnauthorized, utils.ErrAuthTokenVersionMismatch, "token version mismatch")
		return
	}

	// Look up the user to retrieve their username and admin status.
	role := "viewer"
	userName := ""
	user, err := h.authService.GetUserByID(claims.UserID)
	if err != nil || user == nil {
		utils.FailWithCode(c, http.StatusUnauthorized, utils.ErrAuthUserNotFound, "user lookup failed")
		return
	}
	userName = user.Name
	if user.IsAdmin {
		role = "admin"
	}

	// Echo response headers for nginx auth_request to forward to upstream
	// proxies (e.g. Frigate Remote-User and Remote-Role).
	c.Header("X-Auth-User", userName)
	c.Header("X-Auth-Role", role)

	// Optional require_admin check for nginx auth_request locations.
	if c.Query("require_admin") == "true" || c.Query("require_admin") == "1" {
		if !user.IsAdmin {
			utils.Fail(c, http.StatusForbidden, "admin privileges required")
			return
		}
	}

	utils.Success(c, gin.H{
		"user_id":   claims.UserID,
		"device_id": claims.DeviceID,
		"user_name": userName,
		"role":      role,
		"valid":     true,
	})
}

// Logout clears the home_token cookie server-side.
// The cookie is HttpOnly, so JavaScript cannot delete it — the frontend
// must call this endpoint to ensure the cookie is properly expired.
//
//	Route: POST /api/v1/auth/logout
func (h *AuthHandler) Logout(c *gin.Context) {
	secureCookie := false
	if config.AppConfig != nil {
		secureCookie = config.AppConfig.Server.SecureCookie
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("home_token", "", -1, "/", "", secureCookie, true)
	utils.Success(c, gin.H{"logged_out": true})
}

// Refresh re-issues a fresh long-lived JWT for the currently authenticated client,
// updating its last_login_at timestamp and refreshing the auth cookie.
//
//	Route: POST /api/v1/auth/refresh
func (h *AuthHandler) Refresh(c *gin.Context) {
	userID := c.GetUint("user_id")
	deviceID := c.GetUint("device_id")
	if userID == 0 || deviceID == 0 {
		utils.FailWithCode(c, http.StatusUnauthorized, utils.ErrAuthMissing, "unauthorized")
		return
	}

	token, device, err := h.authService.Refresh(userID, deviceID)
	if err != nil {
		// Map service errors to stable error codes. Unknown errors are
		// treated as transient (lookup failed) so clients never log out
		// on a server-side hiccup.
		msg := err.Error()
		errCode := utils.ErrAuthDeviceLookupFailed
		switch {
		case msg == "device revoked":
			errCode = utils.ErrAuthDeviceRevoked
		case msg == "device does not belong to user", strings.Contains(msg, "record not found"):
			errCode = utils.ErrAuthDeviceNotFound
		}
		utils.FailWithCode(c, http.StatusUnauthorized, errCode, msg)
		return
	}

	const maxAge = 365 * 24 * 60 * 60
	secureCookie := false
	if config.AppConfig != nil {
		secureCookie = config.AppConfig.Server.SecureCookie
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("home_token", token, maxAge, "/", "", secureCookie, true)

	utils.Success(c, gin.H{
		"token":     token,
		"device_id": device.ID,
	})
}
