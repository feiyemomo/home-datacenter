package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/service"
	"home-datacenter-api/internal/utils"
)

type AuthHandler struct {
	authService *service.AuthService
}

func NewAuthHandler(
	authService *service.AuthService,
) *AuthHandler {
	return &AuthHandler{
		authService: authService,
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
// The cookie is NOT HttpOnly because the dashboard already stores
// the same JWT in localStorage (XSS-readable) — the cookie is just
// a transport for the same secret, not a fresh attack surface.
// SameSite=Lax is enough: it blocks cross-site XHR/fetch but
// allows top-level navigations (which is how the dashboard opens
// /frigate/).
func (h *AuthHandler) Bind(c *gin.Context) {

	var req BindRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		utils.Fail(c, http.StatusBadRequest, "invalid request body")
		return
	}

	token, err := h.authService.Bind(req.UserID, req.AccessKey)
	if err != nil {
		utils.Fail(c, http.StatusUnauthorized, "invalid credentials")
		return
	}

	// 365 days, matching the JWT's exp claim. SameSite=Lax is
	// the right choice: top-level navigations (which is the only
	// way the dashboard reaches /frigate/) carry the cookie,
	// while cross-site XHR/fetch (the only path an attacker would
	// use to ride the cookie) is blocked by the browser.
	const maxAge = 365 * 24 * 60 * 60
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("home_token", token, maxAge, "/", "", false, false)

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
		utils.Fail(c, http.StatusUnauthorized, "missing or invalid Authorization header")
		return
	}

	claims, err := utils.ParseToken(tokenString)
	if err != nil {
		utils.Fail(c, http.StatusUnauthorized, "invalid token")
		return
	}

	// Reuse the same revocation check as the JWT middleware. We do
	// NOT call JWTAuth() directly because the middleware writes a
	// 401 with a custom JSON body; we want a clean 200/401 contract
	// for nginx auth_request. A revoked device must be 401, not 200,
	// otherwise nginx forwards the request to go2rtc.
	device, err := h.authService.GetDeviceForAuth(claims.DeviceID)
	if err != nil {
		utils.Fail(c, http.StatusUnauthorized, "device lookup failed")
		return
	}
	if device.RevokedAt.Valid {
		utils.Fail(c, http.StatusUnauthorized, "device revoked")
		return
	}

	utils.Success(c, gin.H{
		"user_id":   claims.UserID,
		"device_id": claims.DeviceID,
		"valid":     true,
	})
}
