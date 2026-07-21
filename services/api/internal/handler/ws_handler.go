package handler

import (
	"errors"
	"net/http"
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
// Origin policy: permissive (any origin). Use this for local
// development only; for production prefer NewWebSocketHandlerWithOrigins
// so CSWSH is blocked even if Cloudflare Tunnel is bypassed.
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
				return true
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
//  1. Authorization: Bearer <jwt>  (preferred — keeps token out of logs)
//  2. ?token=<jwt>                  (browser fallback)
//
// The query-param form exposes the token in URL/referer/logs; the
// no-store + no-referrer policy on API responses limits, but does not
// eliminate, that exposure. Prefer the header form where possible.
func (h *WebSocketHandler) Handle(c *gin.Context) {
	// 1. Extract JWT — try header first, then ?token= query param.
	tokenString := ""
	authHeader := c.GetHeader("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		tokenString = strings.TrimPrefix(authHeader, "Bearer ")
	} else if q := c.Query("token"); q != "" {
		tokenString = q
	}

	if tokenString == "" {
		utils.Fail(c, http.StatusUnauthorized, "missing token")
		return
	}

	// 2. Verify JWT and extract claims.
	claims, err := utils.ParseToken(tokenString)
	if err != nil {
		utils.Fail(c, http.StatusUnauthorized, "invalid token")
		return
	}

	// 3. Verify the device is still valid and not revoked.
	dev, err := h.deviceRepo.GetByID(claims.DeviceID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			utils.Fail(c, http.StatusUnauthorized, "device not found")
		} else {
			utils.Fail(c, http.StatusUnauthorized, "device lookup failed")
		}
		return
	}
	if dev.RevokedAt.Valid {
		utils.Fail(c, http.StatusUnauthorized, "device revoked")
		return
	}

	// 4. Look up admin status for routing decisions.
	isAdmin := false
	if h.userService != nil {
		isAdmin, _ = h.userService.GetIsAdmin(claims.UserID)
	}

	// 5. Upgrade to WebSocket.
	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
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
