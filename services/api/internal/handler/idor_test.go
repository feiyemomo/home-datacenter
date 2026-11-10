package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/camera"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/service"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func setupIDORTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}
	if err := db.AutoMigrate(
		&model.Household{},
		&model.User{},
		&model.Device{},
		&model.Camera{},
		&model.CameraShare{},
	); err != nil {
		t.Fatalf("failed to auto migrate: %v", err)
	}
	return db
}

func TestAntiIDOR_UserIsolation(t *testing.T) {
	db := setupIDORTestDB(t)
	userRepo := repository.NewUserRepository(db)
	deviceRepo := repository.NewDeviceRepository(db)
	userSvc := service.NewUserService(userRepo, deviceRepo)
	deviceSvc := service.NewDeviceService(deviceRepo)
	userH := NewUserHandler(userSvc, deviceSvc, nil, deviceRepo, nil)

	// Seed 2 households
	db.Create(&model.Household{ID: 1, Name: "家庭1"})
	db.Create(&model.Household{ID: 2, Name: "家庭2"})

	// Seed user in household 1 and user in household 2
	user1 := model.User{ID: 10, HouseholdID: 1, Name: "user1", IsAdmin: true}
	user2 := model.User{ID: 20, HouseholdID: 2, Name: "user2", IsAdmin: false}
	db.Create(&user1)
	db.Create(&user2)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", uint(10))
		c.Set("household_id", uint(1)) // Caller belongs to household 1
		c.Next()
	})
	r.GET("/api/v1/user/:id", userH.Get)
	r.DELETE("/api/v1/user/:id", userH.Delete)

	// Caller in household 1 attempting to GET user 20 in household 2 -> Must return 404 (anti-probing)
	wGet := httptest.NewRecorder()
	reqGet, _ := http.NewRequest(http.MethodGet, "/api/v1/user/20", nil)
	r.ServeHTTP(wGet, reqGet)

	if wGet.Code != http.StatusNotFound {
		t.Fatalf("CRITICAL IDOR VULNERABILITY: expected 404 Not Found for cross-household user read, got %d: %s", wGet.Code, wGet.Body.String())
	}

	// Caller in household 1 attempting to DELETE user 20 in household 2 -> Must return 404
	wDel := httptest.NewRecorder()
	reqDel, _ := http.NewRequest(http.MethodDelete, "/api/v1/user/20", nil)
	r.ServeHTTP(wDel, reqDel)

	if wDel.Code != http.StatusNotFound {
		t.Fatalf("CRITICAL IDOR VULNERABILITY: expected 404 Not Found for cross-household user delete, got %d: %s", wDel.Code, wDel.Body.String())
	}
}

func TestAntiIDOR_DeviceIsolation(t *testing.T) {
	db := setupIDORTestDB(t)
	userRepo := repository.NewUserRepository(db)
	deviceRepo := repository.NewDeviceRepository(db)
	userSvc := service.NewUserService(userRepo, deviceRepo)
	deviceSvc := service.NewDeviceService(deviceRepo)
	deviceH := NewDeviceHandler(deviceSvc, userSvc, nil)

	// User & Device in household 1
	db.Create(&model.User{ID: 10, HouseholdID: 1, Name: "user1", IsAdmin: true})
	db.Create(&model.Device{ID: 100, HouseholdID: 1, UserID: 10, DeviceName: "device1"})

	// User & Device in household 2
	db.Create(&model.User{ID: 20, HouseholdID: 2, Name: "user2", IsAdmin: false})
	db.Create(&model.Device{ID: 200, HouseholdID: 2, UserID: 20, DeviceName: "device2"})

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", uint(10))
		c.Set("household_id", uint(1)) // Caller is admin of household 1
		c.Next()
	})
	r.DELETE("/api/v1/device/:id", deviceH.Delete)
	r.POST("/api/v1/device/:id/rotate-token", deviceH.RotateToken)

	// Admin of household 1 attempting to delete device 200 of household 2 -> Must return 404
	wDel := httptest.NewRecorder()
	reqDel, _ := http.NewRequest(http.MethodDelete, "/api/v1/device/200", nil)
	r.ServeHTTP(wDel, reqDel)

	if wDel.Code != http.StatusNotFound {
		t.Fatalf("CRITICAL IDOR VULNERABILITY: expected 404 Not Found for cross-household device delete, got %d: %s", wDel.Code, wDel.Body.String())
	}

	// Admin of household 1 attempting to rotate token of device 200 of household 2 -> Must return 404
	wRot := httptest.NewRecorder()
	reqRot, _ := http.NewRequest(http.MethodPost, "/api/v1/device/200/rotate-token", nil)
	r.ServeHTTP(wRot, reqRot)

	if wRot.Code != http.StatusNotFound {
		t.Fatalf("CRITICAL IDOR VULNERABILITY: expected 404 Not Found for cross-household token rotation, got %d: %s", wRot.Code, wRot.Body.String())
	}
}

func TestAntiIDOR_CameraSharingCrossHouseholdRejected(t *testing.T) {
	db := setupIDORTestDB(t)
	bus := eventbus.New()
	reg := &camera.Registry{
		DB: db,
	}
	camH := NewCameraHandler(reg, nil, nil, "", "", nil, bus)

	// Household 1: user1, cam1
	db.Create(&model.User{ID: 10, HouseholdID: 1, Name: "user1", IsAdmin: true})
	cam1 := model.Camera{
		ID:          1,
		HouseholdID: 1,
		Name:        "front-door",
		OwnerID:     10,
		StreamName:  "front-door",
	}
	db.Create(&cam1)

	// Household 2: user2
	db.Create(&model.User{ID: 20, HouseholdID: 2, Name: "user2", IsAdmin: false})

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", uint(10))
		c.Set("household_id", uint(1)) // Caller belongs to household 1
		c.Next()
	})
	r.POST("/api/v1/cameras/:id/shares", camH.ShareCamera)

	// Attempting to share camera 1 (Household 1) with user 20 (Household 2) -> Must be REJECTED with 400
	shareBody, _ := json.Marshal(map[string]any{
		"user_id": 20,
		"can_ptz": false,
	})
	wShare := httptest.NewRecorder()
	reqShare, _ := http.NewRequest(http.MethodPost, "/api/v1/cameras/1/shares", bytes.NewReader(shareBody))
	reqShare.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wShare, reqShare)

	if wShare.Code != http.StatusBadRequest {
		t.Fatalf("CRITICAL IDOR VULNERABILITY: expected 400 Bad Request when sharing camera across households, got %d: %s", wShare.Code, wShare.Body.String())
	}
}
