package middleware

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

// TestJWTAdversarial_TamperedSignature verifies that tampering with any character
// in the signature part of a JWT causes immediate rejection (HTTP 401).
func TestJWTAdversarial_TamperedSignature(t *testing.T) {
	gin.SetMode(gin.TestMode)
	utils.JWTSecret = "test-secret-must-be-at-least-32-chars-long"

	deviceRepo, userRepo := setupTestDB(t)
	user := &model.User{Name: "alice", IsAdmin: true}
	_ = userRepo.Create(user)
	device := &model.Device{UserID: user.ID, DeviceName: "dev1", TokenVersion: 1}
	_ = deviceRepo.Create(device)

	validToken, err := utils.GenerateToken(user.ID, device.ID, 1)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	// Corrupt the signature by modifying characters
	tamperedToken := validToken[:len(validToken)-6] + "xxxxxx"

	r := gin.New()
	r.GET("/api/test", JWTAuth(deviceRepo, userRepo), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Bearer "+tamperedToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401 for tampered signature, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestJWTAdversarial_AlgNone verifies that 'alg': 'none' attack is strictly rejected.
func TestJWTAdversarial_AlgNone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	utils.JWTSecret = "test-secret-must-be-at-least-32-chars-long"

	deviceRepo, userRepo := setupTestDB(t)
	user := &model.User{Name: "alice", IsAdmin: true}
	_ = userRepo.Create(user)
	device := &model.Device{UserID: user.ID, DeviceName: "dev1", TokenVersion: 1}
	_ = deviceRepo.Create(device)

	// Craft unsigned "alg": "none" token
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claimsJSON, _ := json.Marshal(utils.Claims{
		UserID:       user.ID,
		DeviceID:     device.ID,
		Username:     "alice",
		TokenVersion: 1,
	})
	payload := base64.RawURLEncoding.EncodeToString(claimsJSON)
	noneToken := header + "." + payload + "."

	r := gin.New()
	r.GET("/api/test", JWTAuth(deviceRepo, userRepo), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Bearer "+noneToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401 for 'alg:none' token, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestJWTAdversarial_WrongSecret verifies that token signed with a different key is rejected.
func TestJWTAdversarial_WrongSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)
	utils.JWTSecret = "correct-secret-must-be-at-least-32-chars-long"

	deviceRepo, userRepo := setupTestDB(t)
	user := &model.User{Name: "alice", IsAdmin: true}
	_ = userRepo.Create(user)
	device := &model.Device{UserID: user.ID, DeviceName: "dev1", TokenVersion: 1}
	_ = deviceRepo.Create(device)

	// Sign with attacker secret
	claims := utils.Claims{
		UserID:       user.ID,
		DeviceID:     device.ID,
		TokenVersion: 1,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
		},
	}
	tokenObj := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	foreignToken, _ := tokenObj.SignedString([]byte("attacker-secret-must-be-at-least-32-bytes-long"))

	r := gin.New()
	r.GET("/api/test", JWTAuth(deviceRepo, userRepo), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Bearer "+foreignToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401 for token signed with wrong secret, got %d", w.Code)
	}
}

// TestJWTAdversarial_ExpiredToken verifies that an expired token is rejected.
func TestJWTAdversarial_ExpiredToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	utils.JWTSecret = "test-secret-must-be-at-least-32-chars-long"

	deviceRepo, userRepo := setupTestDB(t)
	user := &model.User{Name: "alice", IsAdmin: true}
	_ = userRepo.Create(user)
	device := &model.Device{UserID: user.ID, DeviceName: "dev1", TokenVersion: 1}
	_ = deviceRepo.Create(device)

	// Issue expired token (expired 2 hours ago)
	claims := utils.Claims{
		UserID:       user.ID,
		DeviceID:     device.ID,
		TokenVersion: 1,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-5 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
		},
	}
	tokenObj := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	expiredToken, _ := tokenObj.SignedString([]byte(utils.JWTSecret))

	r := gin.New()
	r.GET("/api/test", JWTAuth(deviceRepo, userRepo), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Bearer "+expiredToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401 for expired token, got %d", w.Code)
	}
}

// TestJWTAdversarial_TokenVersionMismatch verifies that stale tokens after device rotation are rejected.
func TestJWTAdversarial_TokenVersionMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	utils.JWTSecret = "test-secret-must-be-at-least-32-chars-long"

	deviceRepo, userRepo := setupTestDB(t)
	user := &model.User{Name: "alice", IsAdmin: true}
	_ = userRepo.Create(user)

	// Device has rotated to TokenVersion = 3
	device := &model.Device{UserID: user.ID, DeviceName: "dev1", TokenVersion: 3}
	_ = deviceRepo.Create(device)

	// 1. Stale token with TokenVersion = 2 must fail
	staleToken, _ := utils.GenerateToken(user.ID, device.ID, 2)

	r := gin.New()
	r.GET("/api/test", JWTAuth(deviceRepo, userRepo), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Bearer "+staleToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401 for stale TokenVersion=2 vs device TokenVersion=3, got %d", w.Code)
	}

	// 2. Exact token with TokenVersion = 3 must pass
	freshToken, _ := utils.GenerateToken(user.ID, device.ID, 3)
	req2 := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req2.Header.Set("Authorization", "Bearer "+freshToken)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 for exact TokenVersion=3, got %d", w2.Code)
	}
}

// TestJWTAdversarial_RevokedDevice verifies that revoked devices are rejected immediately.
func TestJWTAdversarial_RevokedDevice(t *testing.T) {
	gin.SetMode(gin.TestMode)
	utils.JWTSecret = "test-secret-must-be-at-least-32-chars-long"

	deviceRepo, userRepo := setupTestDB(t)
	user := &model.User{Name: "alice", IsAdmin: true}
	_ = userRepo.Create(user)

	device := &model.Device{UserID: user.ID, DeviceName: "dev1", TokenVersion: 1}
	_ = deviceRepo.Create(device)

	token, _ := utils.GenerateToken(user.ID, device.ID, 1)

	// Revoke the device
	_ = deviceRepo.Revoke(device.ID)

	r := gin.New()
	r.GET("/api/test", JWTAuth(deviceRepo, userRepo), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401 for revoked device, got %d", w.Code)
	}
}

// TestJWTAdversarial_UserDeviceMismatch verifies cross-user device impersonation defense.
func TestJWTAdversarial_UserDeviceMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	utils.JWTSecret = "test-secret-must-be-at-least-32-chars-long"

	deviceRepo, userRepo := setupTestDB(t)
	userAlice := &model.User{Name: "alice"}
	_ = userRepo.Create(userAlice)
	userBob := &model.User{Name: "bob"}
	_ = userRepo.Create(userBob)

	// Device belongs to Alice
	deviceAlice := &model.Device{UserID: userAlice.ID, DeviceName: "alice-phone", TokenVersion: 1}
	_ = deviceRepo.Create(deviceAlice)

	// Attacker crafts token claiming userBob but using alice's deviceID
	tamperedClaimsToken, _ := utils.GenerateToken(userBob.ID, deviceAlice.ID, 1)

	r := gin.New()
	r.GET("/api/test", JWTAuth(deviceRepo, userRepo), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Bearer "+tamperedClaimsToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401 for device user mismatch, got %d", w.Code)
	}
}
