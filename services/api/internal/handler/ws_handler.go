package handler

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"

	"home-datacenter-api/internal/device"
	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/utils"
	"home-datacenter-api/internal/ws"
)

// WebSocketHandler handles the HTTP → WebSocket upgrade.
//
// Auth model:
//   - The initial HTTP request must carry a valid JWT via the
//     Authorization: Bearer header (preferred) or a ?token= query
//     parameter (for browsers that cannot set headers on upgrades).
//   - After upgrade, the connection is kept alive by ping/pong.
//   - The JWT's (user_id, device_id) claims identify the connection.
type WebSocketHandler struct {
	hub         *ws.Hub
	upgrader    websocket.Upgrader
	deviceRepo  *repository.DeviceRepository
	deviceMgr   *device.Manager
	userService UserService

	// allowedOrigins is the allowlist of hostnames that may open a
	// WebSocket against /api/v1/ws. Empty = allow all (local dev).
	// In production, populate with the dashboard hostname(s) via
	// NewWebSocketHandlerWithOrigins so cross-site WebSocket
	// hijacking (CSWSH) is blocked at the application layer too.
	allowedOrigins map[string]struct{}
}

// UserService is a minimal interface to avoid a circular import
// with the service package. The concrete *service.UserService
// satisfies it.
type UserService interface {
	GetIsAdmin(userID uint) (bool, error)
}

// NewWebSocketHandler creates a handler for the /api/v1/ws endpoint.
//
// Origin policy: same-origin by default. Requests without an Origin
// header (non-browser clients such as curl, the Android app, or CLI
// tools) are allowed. Browser requests must have an Origin host that
// matches the request's Host header, which blocks cross-site WebSocket
// hijacking (CSWSH) from malicious websites. For an explicit allowlist
// (e.g. when behind a Cloudflare Tunnel with a known dashboard
// hostname), prefer NewWebSocketHandlerWithOrigins.
func NewWebSocketHandler(
	hub *ws.Hub,
	deviceRepo *repository.DeviceRepository,
	deviceMgr *device.Manager,
	userService UserService,
) *WebSocketHandler {
	return &WebSocketHandler{
		hub:         hub,
		deviceRepo:  deviceRepo,
		deviceMgr:   deviceMgr,
		userService: userService,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				// Default to same-origin check when no allowlist is
				// configured. This prevents CSWSH attacks from
				// malicious websites.
				origin := r.Header.Get("Origin")
				if origin == "" {
					// Non-browser clients (curl, CLI) don't send
					// Origin — allow them.
					return true
				}
				u, err := url.Parse(origin)
				if err != nil {
					return false
				}
				return u.Host == r.Host
			},
		},
	}
}

// NewWebSocketHandlerWithOrigins creates a handler that only accepts
// WebSocket upgrades whose Origin host is in allowlist.
//
// Pass the dashboard's public hostname(s), e.g. {"dashboard.feiyemomo.top"}.
// Cloudflare Tunnel validates origin at the edge, but checking it here
// too prevents CSWSH if a tunnel misconfiguration ever exposes the
// origin directly.
func NewWebSocketHandlerWithOrigins(
	hub *ws.Hub,
	deviceRepo *repository.DeviceRepository,
	deviceMgr *device.Manager,
	userService UserService,
	allowlist []string,
) *WebSocketHandler {
	h := &WebSocketHandler{
		hub:            hub,
		deviceRepo:     deviceRepo,
		deviceMgr:      deviceMgr,
		userService:    userService,
		allowedOrigins: make(map[string]struct{}, len(allowlist)),
	}
	for _, o := range allowlist {
		h.allowedOrigins[strings.ToLower(stripScheme(o))] = struct{}{}
	}
	h.upgrader = websocket.Upgrader{
		CheckOrigin: h.checkOrigin,
	}
	return h
}

// checkOrigin returns true only when the request's Origin host is in
// the allowlist. Only active when allowlist is non-empty.
func (h *WebSocketHandler) checkOrigin(r *http.Request) bool {
	if len(h.allowedOrigins) == 0 {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	host := strings.ToLower(stripScheme(origin))
	_, ok := h.allowedOrigins[host]
	return ok
}

// stripScheme removes the http(s)/ws(s):// prefix from a URL string.
func stripScheme(s string) string {
	for _, p := range []string{"https://", "http://", "wss://", "ws://"} {
		if strings.HasPrefix(strings.ToLower(s), p) {
			return s[len(p):]
		}
	}
	return s
}

// Handle is the gin handler for GET /api/v1/ws.
//
// Token sources (in priority order):
//  1. Sec-WebSocket-Protocol: bearer.<jwt>  (preferred for browsers —
//     browsers cannot set custom headers on a WS upgrade, so the JWT is
//     carried as a subprotocol entry. Keeps the token out of the URL,
//     server logs, referer headers, and browser history.)
//  2. Authorization: Bearer <jwt>            (non-browser clients)
//  3. ?token=<jwt>                            (legacy fallback for older
//     clients, e.g. the Android app before it migrates to the subprotocol
//     form. Exposes the token in URL/referer/logs.)
//
// Security note: Token passed via Sec-WebSocket-Protocol to avoid leakage
// in server logs and browser history. The query-param form is retained
// only for backward compatibility.
func (h *WebSocketHandler) Handle(c *gin.Context) {
	// 1. Extract JWT — prefer Sec-WebSocket-Protocol, then Authorization
	//    header, then ?token= query param (legacy fallback).
	//
	// Browsers cannot set custom headers on a WebSocket upgrade request,
	// so the token is carried as a subprotocol entry of the form
	// "bearer.<jwt>". The selected subprotocol must be echoed back in the
	// 101 response's Sec-WebSocket-Protocol header, otherwise the browser
	// rejects the connection.
	//
	// Token passed via Sec-WebSocket-Protocol to avoid leakage in server
	// logs and browser history.
	tokenString := ""
	subprotocol := ""

	// The Sec-WebSocket-Protocol header may appear multiple times and each
	// value may itself be a comma-separated list (RFC 6455 §4.1). Walk all
	// entries looking for a "bearer.<token>" entry.
	for _, raw := range c.Request.Header["Sec-WebSocket-Protocol"] {
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if strings.HasPrefix(p, "bearer.") {
				tokenString = strings.TrimPrefix(p, "bearer.")
				subprotocol = p
				break
			}
		}
		if subprotocol != "" {
			break
		}
	}

	// Non-browser clients can still use the Authorization header.
	if tokenString == "" {
		authHeader := c.GetHeader("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			tokenString = strings.TrimPrefix(authHeader, "Bearer ")
		}
	}

	// Backward-compat fallback: older clients (e.g. the Android app) still
	// pass the token via ?token=. Keep this until all clients migrate to
	// the Sec-WebSocket-Protocol form.
	if tokenString == "" {
		tokenString = c.Query("token")
	}

	if tokenString == "" {
		utils.FailWithCode(c, http.StatusUnauthorized, utils.ErrAuthMissing, "missing token")
		return
	}

	// 2. Verify JWT and extract claims.
	claims, err := utils.ParseToken(tokenString)
	if err != nil {
		utils.FailWithCode(c, http.StatusUnauthorized, utils.ErrAuthTokenInvalid, "invalid token")
		return
	}

	// 3. Verify the device is still valid and not revoked.
	dev, err := h.deviceRepo.GetByID(claims.DeviceID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			utils.FailWithCode(c, http.StatusUnauthorized, utils.ErrAuthDeviceNotFound, "device not found")
		} else {
			utils.FailWithCode(c, http.StatusUnauthorized, utils.ErrAuthDeviceLookupFailed, "device lookup failed")
		}
		return
	}
	if dev.RevokedAt.Valid {
		utils.FailWithCode(c, http.StatusUnauthorized, utils.ErrAuthDeviceRevoked, "device revoked")
		return
	}

	// 4. Look up admin status for routing decisions.
	isAdmin := false
	if h.userService != nil {
		isAdmin, _ = h.userService.GetIsAdmin(claims.UserID)
	}

	// 5. Upgrade to WebSocket.
	//
	// Use a per-request copy of the upgrader so we can set Subprotocols
	// (the selected subprotocol to echo back in the handshake response)
	// without racing other in-flight upgrades that share h.upgrader. If a
	// "bearer.<token>" subprotocol was supplied, echoing it back is
	// required — the browser aborts the connection otherwise.
	upgrader := h.upgrader
	if subprotocol != "" {
		upgrader.Subprotocols = []string{subprotocol}
	}
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade already wrote an error response; just log.
		return
	}

	// 6. Create client and register with hub.
	client := ws.NewClient(h.hub, conn, claims.UserID, claims.DeviceID, isAdmin)
	// v1.6.16: wire WS client lifecycle into device.Manager so that
	// Android app clients (which connect via WS, not MQTT) are counted
	// as online devices. Previously only MQTT-publishing devices were
	// tracked, so the dashboard always showed "0 online" even when the
	// user was actively using the app.
	//
	// onHeartbeat: called every time the client sends a WS heartbeat
	//   message — refreshes the device's LastSeen so the manager's
	//   90s sweep loop keeps it marked online.
	// onDisconnect: intentionally NOT wired to SetOffline. The sweep
	//   loop will mark the device offline after 90s without a heartbeat,
	//   which correctly handles the case where the user has multiple
	//   WS connections (e.g. app in foreground + background briefly).
	//   Calling SetOffline here would prematurely flip a device that
	//   still has another live connection.
	client.SetLifecycleCallbacks(
		func(deviceID uint) {
			h.deviceMgr.Heartbeat(deviceID)
		},
		nil,
	)
	h.hub.Register(client)

	// Immediately mark the device online — the WS connection is the
	// strongest signal that the user is actively using the app.
	h.deviceMgr.SetOnline(claims.DeviceID, c.ClientIP())

	// 7. Push initial online device list to the new client.
	onlineIDs := h.deviceMgr.GetOnlineDevices()
	h.hub.PushOnlineList(client, onlineIDs)

	// 8. Launch read/write pumps. These run until the connection closes.
	go client.WritePump()
	go client.ReadPump()
}
