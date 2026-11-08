package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/utils"
)

func setupTestDB(t *testing.T) (*repository.DeviceRepository, *repository.UserRepository) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&model.Device{}, &model.User{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return repository.NewDeviceRepository(db), repository.NewUserRepository(db)
}

func TestJWTAuthPopulatesIdentityContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	utils.JWTSecret = "test-secret-must-be-at-least-32-chars-long"

	deviceRepo, userRepo := setupTestDB(t)

	// Create user
	user := &model.User{
		Name:    "欢迎回家",
		IsAdmin: true,
	}
	if err := userRepo.Create(user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Create device
	device := &model.Device{
		UserID:        user.ID,
		DeviceName:    "phone-device",
		AccessKeyHash: "fakehash",
		TokenVersion:  1,
	}
	if err := deviceRepo.Create(device); err != nil {
		t.Fatalf("create device: %v", err)
	}

	// Generate JWT
	token, err := utils.GenerateToken(user.ID, device.ID, 1)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	// Setup router with JWTAuth
	r := gin.New()
	var capturedUserID uint
	var capturedUserName string
	var capturedDeviceID uint
	var capturedDeviceName string
	var capturedIsAdmin bool

	r.GET("/test", JWTAuth(deviceRepo, userRepo), func(c *gin.Context) {
		capturedUserID = c.GetUint("user_id")
		capturedUserName = c.GetString("user_name")
		capturedDeviceID = c.GetUint("device_id")
		capturedDeviceName = c.GetString("device_name")
		capturedIsAdmin = c.GetBool("is_admin")
		c.Status(http.StatusOK)
	})

	// Make request
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	if capturedUserID != user.ID {
		t.Errorf("user_id = %d, want %d", capturedUserID, user.ID)
	}
	if capturedUserName != "欢迎回家" {
		t.Errorf("user_name = %q, want '欢迎回家'", capturedUserName)
	}
	if capturedDeviceID != device.ID {
		t.Errorf("device_id = %d, want %d", capturedDeviceID, device.ID)
	}
	if capturedDeviceName != "phone-device" {
		t.Errorf("device_name = %q, want 'phone-device'", capturedDeviceName)
	}
	if !capturedIsAdmin {
		t.Errorf("is_admin = false, want true")
	}
}

func TestJWTAuthRejectsMissingUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	utils.JWTSecret = "test-secret-must-be-at-least-32-chars-long"

	deviceRepo, userRepo := setupTestDB(t)

	// Create device with user_id=999 (which does not exist)
	device := &model.Device{
		UserID:        999,
		DeviceName:    "ghost-device",
		AccessKeyHash: "fakehash",
		TokenVersion:  1,
	}
	if err := deviceRepo.Create(device); err != nil {
		t.Fatalf("create device: %v", err)
	}

	token, err := utils.GenerateToken(999, device.ID, 1)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	r := gin.New()
	r.GET("/test", JWTAuth(deviceRepo, userRepo), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status code = %d, want 401", w.Code)
	}
}
