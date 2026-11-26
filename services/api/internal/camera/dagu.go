package camera

import (
	"bytes"
	"context"
	"crypto/aes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// DaguAESKey is the reverse-engineered fixed AES key from the original APK (XuedaAesUtil/AES.java).
var DaguAESKey = []byte("@Afwe#ofjweA$*I@")

const (
	DaguAPIBaseURL = "https://application.daguiot.com"
	DaguWSSURL     = "wss://vlink.daguipc.com:9443/cloud"
)

// DaguManager manages the signaling lifecycle, on-demand streaming sessions,
// and PTZ control for Dagu (大谷云) 4G cameras.
type DaguManager struct {
	phone    string
	password string
	deviceID string

	mu           sync.RWMutex
	accessToken  string
	refreshToken string
	tokenExpiry  time.Time

	wsConn     *websocket.Conn
	wsLock     sync.Mutex
	subscribed map[string]bool

	subListenersMu sync.Mutex
	subListeners   map[string]chan bool

	// Active play sessions: SN -> *DaguPlaySession
	sessionsMu sync.Mutex
	sessions   map[string]*DaguPlaySession

	httpClient *http.Client
	stopCh     chan struct{}

	consumerCheckFn func(streamName string) bool
}

// DaguPlaySession tracks an on-demand live stream session.
type DaguPlaySession struct {
	SN         string
	StreamName string
	StreamAddr string
	StreamID   int // 0: main, 1: sub
	RefCnt     int
	CreatedAt  time.Time
	LastActive time.Time
	readyCh    chan struct{}
}

// NewDaguManager creates a new manager for Dagu 4G cameras.
func NewDaguManager(phone, password string) *DaguManager {
	if phone == "" {
		phone = "15291472514"
	}
	if password == "" {
		password = "wm10050817"
	}
	return &DaguManager{
		phone:      phone,
		password:   password,
		deviceID:   "home-datacenter-bridge",
		subscribed:   make(map[string]bool),
		subListeners: make(map[string]chan bool),
		sessions:     make(map[string]*DaguPlaySession),
		httpClient: &http.Client{Timeout: 10 * time.Second},
		stopCh:     make(chan struct{}),
	}
}

// Global default instance for easy integration
var (
	globalDaguMgr     *DaguManager
	globalDaguMgrOnce sync.Once
)

// GetGlobalDaguManager returns the singleton DaguManager instance.
func GetGlobalDaguManager() *DaguManager {
	globalDaguMgrOnce.Do(func() {
		globalDaguMgr = NewDaguManager("15291472514", "wm10050817")
		globalDaguMgr.Start()
	})
	return globalDaguMgr
}

// EncryptPassword encrypts raw password using AES-128-ECB PKCS5Padding + Base64.
func EncryptPassword(rawPw string, key []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	data := pkcs5Padding([]byte(rawPw), block.BlockSize())
	encrypted := make([]byte, len(data))
	for bs, be := 0, block.BlockSize(); bs < len(data); bs, be = bs+block.BlockSize(), be+block.BlockSize() {
		block.Encrypt(encrypted[bs:be], data[bs:be])
	}
	return base64.StdEncoding.EncodeToString(encrypted), nil
}

func pkcs5Padding(ciphertext []byte, blockSize int) []byte {
	padding := blockSize - len(ciphertext)%blockSize
	padtext := bytes.Repeat([]byte{byte(padding)}, padding)
	return append(ciphertext, padtext...)
}

// Login authenticates with application.daguiot.com to get accessToken.
func (m *DaguManager) Login(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	encPw, err := EncryptPassword(m.password, DaguAESKey)
	if err != nil {
		return fmt.Errorf("encrypt password: %w", err)
	}

	reqBody, _ := json.Marshal(map[string]string{
		"phone":    m.phone,
		"password": encPw,
		"deviceId": m.deviceID,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, DaguAPIBaseURL+"/user/V2/login", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("login status %d: %s", resp.StatusCode, string(body))
	}

	var res struct {
		Success bool   `json:"success"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &res); err != nil {
		return fmt.Errorf("unmarshal login resp: %w", err)
	}
	if !res.Success || res.Code != 200 || res.Data.AccessToken == "" {
		return fmt.Errorf("login failed: %s (code %d)", res.Message, res.Code)
	}

	m.accessToken = res.Data.AccessToken
	m.refreshToken = res.Data.RefreshToken
	m.tokenExpiry = time.Now().Add(24 * time.Hour) // Dagu JWT token expires in ~30 days
	log.Printf("[dagu] successfully logged in, got accessToken (prefix: %s...)", m.accessToken[:15])
	return nil
}

// GetToken returns the cached accessToken or logs in if expired/empty.
func (m *DaguManager) GetToken(ctx context.Context) (string, error) {
	m.mu.RLock()
	if m.accessToken != "" && time.Now().Before(m.tokenExpiry) {
		token := m.accessToken
		m.mu.RUnlock()
		return token, nil
	}
	m.mu.RUnlock()

	if err := m.Login(ctx); err != nil {
		return "", err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.accessToken, nil
}

// Start launches the background worker maintaining WSS connection & heartbeats.
func (m *DaguManager) Start() {
	go m.maintainLoop()
}

// Close stops the manager and closes connections.
func (m *DaguManager) Close() {
	close(m.stopCh)
	m.wsLock.Lock()
	if m.wsConn != nil {
		_ = m.wsConn.Close()
	}
	m.wsLock.Unlock()
}

// SetConsumerChecker registers a callback to check whether a given streamName has active consumers in go2rtc.
func (m *DaguManager) SetConsumerChecker(fn func(streamName string) bool) {
	m.mu.Lock()
	m.consumerCheckFn = fn
	m.mu.Unlock()
}

// TouchSession refreshes the activity timestamp for a live session.
func (m *DaguManager) TouchSession(sn, streamName string) {
	m.sessionsMu.Lock()
	defer m.sessionsMu.Unlock()
	if s, ok := m.sessions[sn]; ok && s != nil {
		s.LastActive = time.Now()
		if streamName != "" {
			s.StreamName = streamName
		}
	}
}

func (m *DaguManager) maintainLoop() {
	heartbeatTicker := time.NewTicker(20 * time.Second)
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-heartbeatTicker.C:
			m.sendHeartbeat()
			m.checkIdleSessions()
		}
	}
}

func (m *DaguManager) checkIdleSessions() {
	m.sessionsMu.Lock()
	type stopTarget struct {
		sn       string
		streamID int
	}
	var toStop []stopTarget
	now := time.Now()

	m.mu.RLock()
	checker := m.consumerCheckFn
	m.mu.RUnlock()

	for sn, session := range m.sessions {
		if session == nil {
			continue
		}
		// Grace period: allow 12s for physical camera to spin up and client player to attach
		if now.Sub(session.CreatedAt) < 12*time.Second {
			continue
		}

		hasConsumers := false
		if checker != nil && session.StreamName != "" {
			hasConsumers = checker(session.StreamName)
		}

		// Condition A: consumers count is 0 and no recent activity for 10s (reduced from 30s to cut battery drain)
		// Condition B: absolute inactivity timeout (10m safety fuse against stale leaks)
		if (!hasConsumers && session.RefCnt <= 0 && now.Sub(session.LastActive) > 10*time.Second) ||
			(now.Sub(session.LastActive) > 10*time.Minute) {
			toStop = append(toStop, stopTarget{sn: sn, streamID: session.StreamID})
			delete(m.sessions, sn)
		}
	}
	m.sessionsMu.Unlock()

	for _, target := range toStop {
		m.wsLock.Lock()
		if m.wsConn != nil {
			stopCmd := map[string]any{
				"code":    22,
				"message": "videostop",
				"sn":      target.sn,
				"data": map[string]any{
					"stream": target.streamID,
				},
			}
			_ = m.wsConn.WriteJSON(stopCmd)
			log.Printf("[dagu] idle watchdog: videostop sent for idle camera %s (stream %d, power & cellular saved)", target.sn, target.streamID)
		}
		m.wsLock.Unlock()
	}
}

// GetActiveSessionAddr returns the live stream address if an active session exists.
func (m *DaguManager) GetActiveSessionAddr(sn string) (string, bool) {
	m.sessionsMu.Lock()
	defer m.sessionsMu.Unlock()
	session, ok := m.sessions[sn]
	if ok && session != nil && session.StreamAddr != "" {
		return session.StreamAddr, true
	}
	return "", false
}

func (m *DaguManager) sendHeartbeat() {
	m.wsLock.Lock()
	defer m.wsLock.Unlock()

	if m.wsConn == nil {
		return
	}

	hb := map[string]any{
		"code":      0,
		"message":   "heartbeat",
		"sn":        "6912200023929742",
		"timestamp": time.Now().UnixMilli(),
	}
	if err := m.wsConn.WriteJSON(hb); err != nil {
		log.Printf("[dagu] heartbeat failed: %v", err)
		_ = m.wsConn.Close()
		m.wsConn = nil
	}
}

// ensureWSS guarantees an active WSS connection with the specified device subscribed.
func (m *DaguManager) ensureWSS(ctx context.Context, sn string) error {
	token, err := m.GetToken(ctx)
	if err != nil {
		return err
	}

	m.wsLock.Lock()
	defer m.wsLock.Unlock()

	if m.wsConn != nil && m.subscribed[sn] {
		return nil
	}

	if m.wsConn == nil {
		headers := http.Header{}
		headers.Set("Authorization", "Bearer "+token)
		dialer := websocket.DefaultDialer
		dialer.HandshakeTimeout = 8 * time.Second

		conn, resp, err := dialer.DialContext(ctx, DaguWSSURL, headers)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusUnauthorized {
				// Token expired, clear token and retry once
				m.mu.Lock()
				m.accessToken = ""
				m.mu.Unlock()
			}
			return fmt.Errorf("dial dagu wss: %w", err)
		}
		m.wsConn = conn
		m.subscribed = make(map[string]bool)

		// Start reader goroutine
		go m.readLoop(conn)
	}

	subCh := make(chan bool, 1)
	m.subListenersMu.Lock()
	m.subListeners[sn] = subCh
	m.subListenersMu.Unlock()

	// Send subscribe message (code: 90)
	subMsg := map[string]any{
		"code": 90,
		"data": []map[string]string{
			{"sn": sn},
		},
	}
	if err := m.wsConn.WriteJSON(subMsg); err != nil {
		_ = m.wsConn.Close()
		m.wsConn = nil
		return fmt.Errorf("send subscribe msg: %w", err)
	}

	// Wait for code:90 response
	select {
	case ok := <-subCh:
		if !ok {
			return fmt.Errorf("subscription rejected for %s", sn)
		}
	case <-time.After(5 * time.Second):
		log.Printf("[dagu] warning: subscribe response timeout for %s, continuing", sn)
	case <-ctx.Done():
		return ctx.Err()
	}

	m.subscribed[sn] = true
	log.Printf("[dagu] device %s subscribed via WSS confirmed", sn)
	return nil
}

func (m *DaguManager) readLoop(conn *websocket.Conn) {
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			m.wsLock.Lock()
			if m.wsConn == conn {
				m.wsConn = nil
				m.subscribed = make(map[string]bool)
			}
			m.wsLock.Unlock()
			return
		}

		log.Printf("[dagu] raw wss message: %s", string(msg))
		var raw struct {
			Code    int             `json:"code"`
			State   int             `json:"state"`
			Message string          `json:"message"`
			SN      string          `json:"sn"`
			Data    json.RawMessage `json:"data"`
		}

		if err := json.Unmarshal(msg, &raw); err == nil {
			if raw.State == 90 || raw.Code == 90 {
				var items []struct {
					SN string `json:"sn"`
				}
				_ = json.Unmarshal(raw.Data, &items)
				m.subListenersMu.Lock()
				for _, it := range items {
					if ch, exists := m.subListeners[it.SN]; exists {
						select {
						case ch <- true:
						default:
						}
						delete(m.subListeners, it.SN)
					}
				}
				// Also if items was empty but we have waiting listener
				for snKey, ch := range m.subListeners {
					select {
					case ch <- true:
					default:
					}
					delete(m.subListeners, snKey)
				}
				m.subListenersMu.Unlock()
			}

			if raw.State == 22 || raw.Message == "videoaddr" {
				var addrObj struct {
					Addr string `json:"addr"`
				}
				_ = json.Unmarshal(raw.Data, &addrObj)
				addr := addrObj.Addr
				if addr == "" {
					var addrStr string
					if err := json.Unmarshal(raw.Data, &addrStr); err == nil {
						addr = addrStr
					}
				}

				sn := raw.SN
				m.sessionsMu.Lock()
				// If sn is empty in payload, check if we have an active waiting session
				if sn == "" && len(m.sessions) == 1 {
					for k := range m.sessions {
						sn = k
						break
					}
				}
				session, ok := m.sessions[sn]
				if ok && session != nil && addr != "" {
					session.StreamAddr = addr
					select {
					case <-session.readyCh:
					default:
						close(session.readyCh)
					}
					log.Printf("[dagu] received stream addr for %s: %s", sn, addr)
				}
				m.sessionsMu.Unlock()
			}
		}
	}
}

// AcquireStream requests on-demand streaming for a given SN, wakes the physical camera,
// and returns the live RTMP URL.
func (m *DaguManager) AcquireStream(ctx context.Context, sn string, streamID int) (string, error) {
	if err := m.ensureWSS(ctx, sn); err != nil {
		return "", err
	}

	m.sessionsMu.Lock()
	session, exists := m.sessions[sn]
	if !exists || session == nil {
		session = &DaguPlaySession{
			SN:        sn,
			StreamID:  streamID,
			CreatedAt: time.Now(),
			readyCh:   make(chan struct{}),
		}
		m.sessions[sn] = session
	}
	session.RefCnt++
	session.LastActive = time.Now()
	addr := session.StreamAddr
	readyCh := session.readyCh
	m.sessionsMu.Unlock()

	if addr != "" {
		return addr, nil
	}

	// Send videoplay command (code: 22)
	m.wsLock.Lock()
	if m.wsConn == nil {
		m.wsLock.Unlock()
		return "", errors.New("dagu wss not connected")
	}
	playCmd := map[string]any{
		"code":    22,
		"message": "videoplay",
		"sn":      sn,
		"data": map[string]any{
			"stream": streamID,
		},
	}
	err := m.wsConn.WriteJSON(playCmd)
	m.wsLock.Unlock()

	if err != nil {
		return "", fmt.Errorf("send videoplay: %w", err)
	}

	// Wait for videoaddr response with timeout
	select {
	case <-readyCh:
		m.sessionsMu.Lock()
		resAddr := session.StreamAddr
		m.sessionsMu.Unlock()
		// Allow 800ms cold start for physical device encoder
		time.Sleep(800 * time.Millisecond)
		return resAddr, nil
	case <-time.After(8 * time.Second):
		return "", errors.New("timeout waiting for dagu videoaddr")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// HasActiveSession checks if an active play session with live stream address exists.
func (m *DaguManager) HasActiveSession(sn string) bool {
	m.sessionsMu.Lock()
	defer m.sessionsMu.Unlock()
	session, ok := m.sessions[sn]
	return ok && session != nil && session.StreamAddr != ""
}

// ReleaseStream decrements reference count. The session is gracefully kept alive
// for idle timeout / consumer check in checkIdleSessions() to prevent premature videostop.
func (m *DaguManager) ReleaseStream(sn string) {
	m.sessionsMu.Lock()
	defer m.sessionsMu.Unlock()
	session, ok := m.sessions[sn]
	if !ok || session == nil {
		return
	}
	session.RefCnt--
	if session.RefCnt < 0 {
		session.RefCnt = 0
	}
	session.LastActive = time.Now()
}

// PTZ controls the physical camera motor.
// Direction: 1=UP, 2=DOWN, 3=LEFT, 0=RIGHT.
func (m *DaguManager) PTZ(ctx context.Context, sn string, direction int, action string, speed int) error {
	if err := m.ensureWSS(ctx, sn); err != nil {
		return err
	}

	m.wsLock.Lock()
	defer m.wsLock.Unlock()

	if m.wsConn == nil {
		return errors.New("dagu wss not connected")
	}

	motorMsg := map[string]any{
		"code":    10,
		"message": "motor",
		"sn":      sn,
		"data": map[string]any{
			"dir":   direction,
			"speed": 1,
		},
	}

	// Send 3 step pulses with 150ms interval for responsive physical movement
	for i := 0; i < 3; i++ {
		if err := m.wsConn.WriteJSON(motorMsg); err != nil {
			return fmt.Errorf("send motor msg: %w", err)
		}
		if i < 2 {
			time.Sleep(150 * time.Millisecond)
		}
	}
	return nil
}
