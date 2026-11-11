package ops

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	ErrOnePanelUnreachable = errors.New("1panel daemon unreachable")
	ErrOnePanelAuthFailed  = errors.New("1panel authentication failed")
	ErrInvalidContainer    = errors.New("invalid or unauthorized container name")
)

// OnePanelClient connects to 1Panel Enterprise / Community OpenAPI.
type OnePanelClient struct {
	baseURL    string
	entrance   string
	username   string
	password   string
	httpClient *http.Client

	mu          sync.RWMutex
	psession    string
	pcsrftoken  string
	lastLoginAt time.Time
	loginTTL    time.Duration
}

// NewOnePanelClient creates a configured 1Panel API client.
func NewOnePanelClient(baseURL, entrance, username, password string) *OnePanelClient {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		baseURL = "http://172.18.0.1:10000"
	}
	if entrance == "" {
		entrance = "btpanel"
	}
	// Fallback credentials purged for security: username and password must be explicitly provided.

	return &OnePanelClient{
		baseURL:  baseURL,
		entrance: entrance,
		username: username,
		password: password,
		httpClient: &http.Client{
			Timeout: 8 * time.Second,
		},
		loginTTL: 30 * time.Minute,
	}
}

// BaseURL returns the configured base URL.
func (c *OnePanelClient) BaseURL() string {
	return c.baseURL
}

// SessionCredentials contains live 1Panel session cookies for Web direct access.
type SessionCredentials struct {
	BaseURL    string    `json:"base_url"`
	Entrance   string    `json:"entrance"`
	EntryURL   string    `json:"entry_url"`
	PSession   string    `json:"psession"`
	PCSRFToken string    `json:"pcsrftoken"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// GetSession returns valid session credentials, executing handshake login if needed.
func (c *OnePanelClient) GetSession(ctx context.Context) (*SessionCredentials, error) {
	if err := c.EnsureAuthenticated(ctx); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	entryURL := fmt.Sprintf("%s/%s", c.baseURL, strings.TrimPrefix(c.entrance, "/"))
	if c.entrance == "" {
		entryURL = c.baseURL
	}

	return &SessionCredentials{
		BaseURL:    c.baseURL,
		Entrance:   c.entrance,
		EntryURL:   entryURL,
		PSession:   c.psession,
		PCSRFToken: c.pcsrftoken,
		ExpiresAt:  c.lastLoginAt.Add(c.loginTTL),
	}, nil
}

// EnsureAuthenticated performs login if session is empty or expired.
func (c *OnePanelClient) EnsureAuthenticated(ctx context.Context) error {
	c.mu.RLock()
	valid := c.psession != "" && time.Since(c.lastLoginAt) < c.loginTTL
	c.mu.RUnlock()

	if valid {
		return nil
	}
	return c.Login(ctx)
}

// Login executes the 1Panel v2 hybrid RSA+AES authentication handshake.
func (c *OnePanelClient) Login(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-checked locking to avoid concurrent login thundering herd
	if c.psession != "" && time.Since(c.lastLoginAt) < c.loginTTL {
		return nil
	}

	entranceB64 := base64.StdEncoding.EncodeToString([]byte(c.entrance))

	// Step 1: Probe entrance or login endpoint to acquire panel_public_key cookie
	probeURL := fmt.Sprintf("%s/api/v2/core/auth/login", c.baseURL)
	emptyReqBody, _ := json.Marshal(map[string]string{
		"name":     "",
		"password": "",
		"language": "zh",
	})
	probeReq, err := http.NewRequestWithContext(ctx, http.MethodPost, probeURL, bytes.NewReader(emptyReqBody))
	if err != nil {
		return fmt.Errorf("%w: failed to create probe request: %v", ErrOnePanelUnreachable, err)
	}
	probeReq.Header.Set("Content-Type", "application/json")
	probeReq.Header.Set("EntranceCode", entranceB64)

	probeResp, err := c.httpClient.Do(probeReq)
	if err != nil {
		return fmt.Errorf("%w: probe failed: %v", ErrOnePanelUnreachable, err)
	}
	defer probeResp.Body.Close()

	var pubKeyPEM string
	for _, cookie := range probeResp.Cookies() {
		if cookie.Name == "panel_public_key" {
			val, _ := url.QueryUnescape(cookie.Value)
			decoded, err := base64.StdEncoding.DecodeString(val)
			if err == nil {
				pubKeyPEM = string(decoded)
			}
			break
		}
	}

	if pubKeyPEM == "" {
		// Fallback: try fetching from GET /btpanel directly
		getURL := fmt.Sprintf("%s/%s", c.baseURL, c.entrance)
		getReq, getErr := http.NewRequestWithContext(ctx, http.MethodGet, getURL, nil)
		if getErr == nil {
			getReq.Header.Set("EntranceCode", entranceB64)
			if getResp, getRespErr := c.httpClient.Do(getReq); getRespErr == nil {
				defer getResp.Body.Close()
				for _, cookie := range getResp.Cookies() {
					if cookie.Name == "panel_public_key" {
						val, _ := url.QueryUnescape(cookie.Value)
						if decoded, err := base64.StdEncoding.DecodeString(val); err == nil {
							pubKeyPEM = string(decoded)
						}
						break
					}
				}
			}
		}
	}

	if pubKeyPEM == "" {
		return fmt.Errorf("%w: failed to obtain panel_public_key cookie", ErrOnePanelAuthFailed)
	}

	// Step 2: Encrypt password using hybrid RSA + AES-128-CBC
	encryptedPassword, err := encryptHybridPassword(pubKeyPEM, c.password)
	if err != nil {
		return fmt.Errorf("%w: hybrid encryption failed: %v", ErrOnePanelAuthFailed, err)
	}

	// Step 3: Perform login
	loginURL := fmt.Sprintf("%s/api/v2/core/auth/login", c.baseURL)
	loginPayload := map[string]string{
		"name":       c.username,
		"password":   encryptedPassword,
		"language":   "zh",
		"authMethod": "session",
	}
	payloadBytes, _ := json.Marshal(loginPayload)

	loginReq, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return fmt.Errorf("%w: failed to create login request: %v", ErrOnePanelUnreachable, err)
	}
	loginReq.Header.Set("Content-Type", "application/json")
	loginReq.Header.Set("EntranceCode", entranceB64)

	loginResp, err := c.httpClient.Do(loginReq)
	if err != nil {
		return fmt.Errorf("%w: login request failed: %v", ErrOnePanelUnreachable, err)
	}
	defer loginResp.Body.Close()

	bodyBytes, _ := io.ReadAll(loginResp.Body)
	var respData onePanelResponse
	if err := json.Unmarshal(bodyBytes, &respData); err != nil {
		return fmt.Errorf("%w: unexpected login response: %s", ErrOnePanelAuthFailed, string(bodyBytes))
	}

	if respData.Code != 200 {
		return fmt.Errorf("%w: code %d: %s", ErrOnePanelAuthFailed, respData.Code, respData.Message)
	}

	// Step 4: Extract psession & pcsrftoken
	for _, cookie := range loginResp.Cookies() {
		if cookie.Name == "psession" {
			c.psession = cookie.Value
		}
		if cookie.Name == "pcsrftoken" {
			c.pcsrftoken = cookie.Value
		}
	}

	c.lastLoginAt = time.Now()
	return nil
}

// encryptHybridPassword implements 1Panel's client-side hybrid password encryption.
func encryptHybridPassword(pubKeyPEM, rawPassword string) (string, error) {
	// Parse RSA Public Key
	block, _ := pem.Decode([]byte(pubKeyPEM))
	if block == nil {
		return "", errors.New("failed to parse PEM block containing public key")
	}
	pubInterface, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("failed to parse PKIX public key: %w", err)
	}
	rsaPub, ok := pubInterface.(*rsa.PublicKey)
	if !ok {
		return "", errors.New("key is not an RSA public key")
	}

	// Generate 16 random bytes as a 32-character hex AES key
	aesKeyRaw := make([]byte, 16)
	if _, err := rand.Read(aesKeyRaw); err != nil {
		return "", err
	}
	aesKeyHex := hex.EncodeToString(aesKeyRaw)

	// RSA-encrypt the 32-char hex string using PKCS#1 v1.5
	rsaEncryptedKey, err := rsa.EncryptPKCS1v15(rand.Reader, rsaPub, []byte(aesKeyHex))
	if err != nil {
		return "", fmt.Errorf("RSA PKCS1v15 encrypt failed: %w", err)
	}
	rsaEncB64 := base64.StdEncoding.EncodeToString(rsaEncryptedKey)

	// AES-128-CBC encrypt rawPassword using aesKeyHex as key bytes
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}

	aesBlock, err := aes.NewCipher([]byte(aesKeyHex))
	if err != nil {
		return "", fmt.Errorf("AES cipher creation failed: %w", err)
	}

	// Apply PKCS#7 padding
	padLen := aes.BlockSize - (len(rawPassword) % aes.BlockSize)
	padded := append([]byte(rawPassword), bytes.Repeat([]byte{byte(padLen)}, padLen)...)

	ciphertext := make([]byte, len(padded))
	mode := cipher.NewCBCEncrypter(aesBlock, iv)
	mode.CryptBlocks(ciphertext, padded)

	ivB64 := base64.StdEncoding.EncodeToString(iv)
	cipherB64 := base64.StdEncoding.EncodeToString(ciphertext)

	return fmt.Sprintf("%s:%s:%s", rsaEncB64, ivB64, cipherB64), nil
}

// doRequest performs an authenticated API call with automatic 401 retry.
func (c *OnePanelClient) doRequest(ctx context.Context, method, path string, body any) ([]byte, error) {
	if err := c.EnsureAuthenticated(ctx); err != nil {
		return nil, err
	}

	res, err := c.executeRequest(ctx, method, path, body)
	if err != nil && errors.Is(err, ErrOnePanelAuthFailed) {
		// Attempt re-login once
		if loginErr := c.Login(ctx); loginErr == nil {
			return c.executeRequest(ctx, method, path, body)
		}
	}
	return res, err
}

func (c *OnePanelClient) executeRequest(ctx context.Context, method, path string, body any) ([]byte, error) {
	c.mu.RLock()
	psession := c.psession
	csrfToken := c.pcsrftoken
	entrance := c.entrance
	c.mu.RUnlock()

	reqURL := fmt.Sprintf("%s%s", c.baseURL, path)
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to create request: %v", ErrOnePanelUnreachable, err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("EntranceCode", base64.StdEncoding.EncodeToString([]byte(entrance)))
	if csrfToken != "" {
		req.Header.Set("X-CSRF-Token", csrfToken)
	}
	if psession != "" {
		req.AddCookie(&http.Cookie{Name: "psession", Value: psession})
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: request error: %v", ErrOnePanelUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrOnePanelAuthFailed
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to read response: %v", ErrOnePanelUnreachable, err)
	}
	return data, nil
}

// GetNodeMetrics fetches live CPU, memory, and load statistics from 1Panel.
func (c *OnePanelClient) GetNodeMetrics(ctx context.Context) (*onePanelNodeMetrics, error) {
	data, err := c.doRequest(ctx, http.MethodGet, "/api/v2/dashboard/current/node", nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Code    int                 `json:"code"`
		Message string              `json:"message"`
		Data    onePanelNodeMetrics `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("%w: invalid node metrics json: %v", ErrOnePanelUnreachable, err)
	}
	if resp.Code != 200 {
		return nil, fmt.Errorf("%w: 1panel returned code %d: %s", ErrOnePanelUnreachable, resp.Code, resp.Message)
	}
	return &resp.Data, nil
}

// GetOSInfo fetches OS, architecture, kernel, and disk capacity.
func (c *OnePanelClient) GetOSInfo(ctx context.Context) (*onePanelOSInfo, error) {
	data, err := c.doRequest(ctx, http.MethodGet, "/api/v2/dashboard/base/os", nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Code    int            `json:"code"`
		Message string         `json:"message"`
		Data    onePanelOSInfo `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("%w: invalid os info json: %v", ErrOnePanelUnreachable, err)
	}
	if resp.Code != 200 {
		return nil, fmt.Errorf("%w: 1panel returned code %d: %s", ErrOnePanelUnreachable, resp.Code, resp.Message)
	}
	return &resp.Data, nil
}

// ListContainers queries container statuses from 1Panel with full pagination.
func (c *OnePanelClient) ListContainers(ctx context.Context) ([]ContainerSummary, error) {
	const pageSize = 50
	const maxPages = 20

	var allItems []onePanelContainerItem
	for page := 1; page <= maxPages; page++ {
		reqPayload := map[string]any{
			"page":     page,
			"pageSize": pageSize,
			"state":    "all",
			"orderBy":  "name",
			"order":    "descending",
		}

		data, err := c.doRequest(ctx, http.MethodPost, "/api/v2/containers/search", reqPayload)
		if err != nil {
			return nil, err
		}

		var resp struct {
			Code    int                   `json:"code"`
			Message string                `json:"message"`
			Data    onePanelContainerPage `json:"data"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("%w: invalid container search json: %v", ErrOnePanelUnreachable, err)
		}
		if resp.Code != 200 {
			return nil, fmt.Errorf("%w: container search code %d: %s", ErrOnePanelUnreachable, resp.Code, resp.Message)
		}

		allItems = append(allItems, resp.Data.Items...)
		if len(resp.Data.Items) == 0 || len(resp.Data.Items) < pageSize || (resp.Data.Total > 0 && len(allItems) >= resp.Data.Total) {
			break
		}
	}

	// Fetch real-time container metrics from dedicated 1Panel stats endpoint
	statsMap := make(map[string]onePanelStatsItem)
	if statsData, sErr := c.doRequest(ctx, http.MethodGet, "/api/v2/containers/list/stats", nil); sErr == nil {
		var statsResp struct {
			Code int                 `json:"code"`
			Data []onePanelStatsItem `json:"data"`
		}
		if json.Unmarshal(statsData, &statsResp) == nil && (statsResp.Code == 200 || statsResp.Code == 0) {
			for _, st := range statsResp.Data {
				statsMap[st.ContainerID] = st
			}
		}
	}

	var summaries []ContainerSummary
	coreContainers := map[string]bool{
		"home-api":          true,
		"home-web":          true,
		"home-frigate":      true,
		"home-mosquitto":    true,
		"home-vision":       true,
		"home-cloudflared":  true,
		"home-h3c-keepalive": true,
		"home-new-api":      true,
		"home-ops-agent":    true,
		"home-backup":       true,
		"mihomo":            true,
		"tailscale":         true,
	}

	for _, item := range allItems {
		cleanName := strings.TrimPrefix(item.Name, "/")
		cpu := item.CPUPerc
		memMB := float64(item.MemUsage) / 1024 / 1024
		memLimitMB := float64(item.MemLimit) / 1024 / 1024

		if st, ok := statsMap[item.ContainerID]; ok {
			cpu = st.CPUPercent
			memMB = float64(st.MemoryUsage) / 1024 / 1024
			memLimitMB = float64(st.MemoryLimit) / 1024 / 1024
		}

		summaries = append(summaries, ContainerSummary{
			ContainerID:   item.ContainerID,
			Name:          cleanName,
			State:         item.State,
			Status:        item.Status,
			Image:         item.Image,
			CPUPercent:    roundFloat(cpu, 1),
			MemoryUsageMB: roundFloat(memMB, 1),
			MemoryMB:      roundFloat(memMB, 1),
			MemoryLimitMB: roundFloat(memLimitMB, 1),
			IsCore:        coreContainers[cleanName],
			CreatedAt:     item.Created,
		})
	}
	return summaries, nil
}

// RestartContainer calls 1Panel to restart a container by name.
func (c *OnePanelClient) RestartContainer(ctx context.Context, name string) error {
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	if name == "" || strings.ContainsAny(name, "/\\?# \t\r\n") {
		return ErrInvalidContainer
	}

	payload := map[string]any{
		"names":     []string{name},
		"operation": "restart",
	}

	data, err := c.doRequest(ctx, http.MethodPost, "/api/v2/containers/operate", payload)
	if err != nil {
		return err
	}

	var resp onePanelResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("%w: invalid restart response: %v", ErrOnePanelUnreachable, err)
	}
	if resp.Code != 200 {
		return fmt.Errorf("%w: 1panel restart failed (code %d): %s", ErrOnePanelUnreachable, resp.Code, resp.Message)
	}
	return nil
}
