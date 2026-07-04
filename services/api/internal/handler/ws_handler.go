package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"home-datacenter-api/internal/device"
	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/utils"
	"home-datacenter-api/internal/ws"
)

// WebSocketHandler handles the HTTP → WebSocket upgrade.
//
// Auth model (hybrid):
//   - The initial HTTP request must carry a valid 365-day JWT in
//     the Authorization: Bearer header (same as REST endpoints).
//   - After upgrade, the connection is kept alive by ping/pong.
//   - The JWT's (user_id, device_id) claims identify the connection.
type WebSocketHandler struct {
	hub         *ws.Hub
	upgrader    websocket.Upgrader
	deviceRepo  *repository.DeviceRepository
	deviceMgr   *device.Manager
	userService UserService
}

// UserService is a minimal interface to avoid a circular import
// with the service package. The concrete *service.UserService
// satisfies it.
type UserService interface {
	GetIsAdmin(userID uint) (bool, error)
}

// NewWebSocketHandler creates a handler for the /api/v1/ws endpoint.
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
			// Allow any origin in the home network. Cloudflare Tunnel
			// handles origin validation at the edge.
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}
}

// Handle is the gin handler for GET /api/v1/ws.
//
// Query-parameter auth alternative:
//
//	ws://host/api/v1/ws?token=<jwt>
//
// is supported for browsers / clients that cannot set Authorization
// headers on WebSocket upgrades.
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
		utils.Fail(c, http.StatusUnauthorized, "device not found")
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
	h.hub.Register(client)

	// 7. Push initial online device list to the new client.
	onlineIDs := h.deviceMgr.GetOnlineDevices()
	h.hub.PushOnlineList(client, onlineIDs)

	// 8. Launch read/write pumps. These run until the connection closes.
	go client.WritePump()
	go client.ReadPump()
}
