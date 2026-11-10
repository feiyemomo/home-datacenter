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

func setupIDOREmpiricalTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}

	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
		_, _ = sqlDB.Exec("PRAGMA busy_timeout = 5000;")
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

// ============================================================================
// Objective 5: Anti-IDOR Cross-Household Boundaries (User, Device, Camera)
// ============================================================================
func TestEmpiricalChallenge5_UserDeviceCameraCrossHouseholdIDOR(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupIDOREmpiricalTestDB(t)
	userRepo := repository.NewUserRepository(db)
	deviceRepo := repository.NewDeviceRepository(db)
	userSvc := service.NewUserService(userRepo, deviceRepo)
	deviceSvc := service.NewDeviceService(deviceRepo)
	bus := eventbus.New()

	userH := NewUserHandler(userSvc, deviceSvc, nil, deviceRepo, bus)
	deviceH := NewDeviceHandler(deviceSvc, userSvc, bus)
	reg := &camera.Registry{DB: db}
	camH := NewCameraHandler(reg, nil, nil, "", "", nil, bus)

	// Seed Household 1 & 2
	db.Create(&model.Household{ID: 1, Name: "Household A"})
	db.Create(&model.Household{ID: 2, Name: "Household B"})

	// Household 1 entities: user1 (admin), dev1, cam1
	db.Create(&model.User{ID: 10, HouseholdID: 1, Name: "userA", IsAdmin: true})
	db.Create(&model.Device{ID: 100, HouseholdID: 1, UserID: 10, DeviceName: "devA"})
	db.Create(&model.Camera{ID: 1, HouseholdID: 1, OwnerID: 10, Name: "camA", StreamName: "camA"})

	// Household 2 entities: user2 (admin), dev2, cam2
	db.Create(&model.User{ID: 20, HouseholdID: 2, Name: "userB", IsAdmin: true})
	db.Create(&model.Device{ID: 200, HouseholdID: 2, UserID: 20, DeviceName: "devB"})
	db.Create(&model.Camera{ID: 2, HouseholdID: 2, OwnerID: 20, Name: "camB", StreamName: "camB"})

	// Setup router acting on behalf of userB (Household 2)
	rB := gin.New()
	rB.Use(func(c *gin.Context) {
		c.Set("user_id", uint(20))
		c.Set("household_id", uint(2))
		c.Set("is_admin", true)
		c.Next()
	})

	rB.GET("/api/v1/user/:id", userH.Get)
	rB.PUT("/api/v1/user/:id", userH.Update)
	rB.DELETE("/api/v1/user/:id", userH.Delete)

	rB.DELETE("/api/v1/device/:id", deviceH.Delete)
	rB.POST("/api/v1/device/:id/rotate-token", deviceH.RotateToken)

	rB.POST("/api/v1/cameras/:id/shares", camH.ShareCamera)

	// 1. User IDOR: Household B cannot read, update, or delete User of Household A (returns 404)
	t.Run("Cross-Household User Access Returns 404", func(t *testing.T) {
		// GET
		wGet := httptest.NewRecorder()
		reqGet, _ := http.NewRequest(http.MethodGet, "/api/v1/user/10", nil)
		rB.ServeHTTP(wGet, reqGet)
		if wGet.Code != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found for cross-household user get, got %d", wGet.Code)
		}

		// PUT
		wPut := httptest.NewRecorder()
		reqPut, _ := http.NewRequest(http.MethodPut, "/api/v1/user/10", bytes.NewReader([]byte(`{"name":"tampered"}`)))
		reqPut.Header.Set("Content-Type", "application/json")
		rB.ServeHTTP(wPut, reqPut)
		if wPut.Code != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found for cross-household user update, got %d", wPut.Code)
		}

		// DELETE
		wDel := httptest.NewRecorder()
		reqDel, _ := http.NewRequest(http.MethodDelete, "/api/v1/user/10", nil)
		rB.ServeHTTP(wDel, reqDel)
		if wDel.Code != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found for cross-household user delete, got %d", wDel.Code)
		}
	})

	// 2. Device IDOR: Household B cannot delete or rotate token of Device of Household A (returns 404)
	t.Run("Cross-Household Device Access Returns 404", func(t *testing.T) {
		// DELETE
		wDel := httptest.NewRecorder()
		reqDel, _ := http.NewRequest(http.MethodDelete, "/api/v1/device/100", nil)
		rB.ServeHTTP(wDel, reqDel)
		if wDel.Code != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found for cross-household device delete, got %d", wDel.Code)
		}

		// ROTATE TOKEN
		wRot := httptest.NewRecorder()
		reqRot, _ := http.NewRequest(http.MethodPost, "/api/v1/device/100/rotate-token", nil)
		rB.ServeHTTP(wRot, reqRot)
		if wRot.Code != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found for cross-household device rotate token, got %d", wRot.Code)
		}
	})

	// 3. Camera Sharing IDOR: Attempting to share Camera 2 (Household B) with User 10 (Household A) -> Must return 400
	t.Run("Cross-Household Camera Sharing Rejected with 400", func(t *testing.T) {
		sharePayload, _ := json.Marshal(map[string]any{
			"user_id": 10, // Household 1
			"can_ptz": false,
		})
		wShare := httptest.NewRecorder()
		reqShare, _ := http.NewRequest(http.MethodPost, "/api/v1/cameras/2/shares", bytes.NewReader(sharePayload))
		reqShare.Header.Set("Content-Type", "application/json")
		rB.ServeHTTP(wShare, reqShare)

		if wShare.Code != http.StatusBadRequest {
			t.Fatalf("CRITICAL CROSS-HOUSEHOLD LEAK: expected 400 Bad Request when sharing camera across households, got %d: %s", wShare.Code, wShare.Body.String())
		}
	})

	// 4. Camera Manage Shares IDOR: Household B attempting to manage/share Camera 1 (Household A) -> Returns 404
	t.Run("Cross-Household Camera Management Returns 404", func(t *testing.T) {
		sharePayload, _ := json.Marshal(map[string]any{
			"user_id": 20,
			"can_ptz": false,
		})
		wShare := httptest.NewRecorder()
		reqShare, _ := http.NewRequest(http.MethodPost, "/api/v1/cameras/1/shares", bytes.NewReader(sharePayload))
		reqShare.Header.Set("Content-Type", "application/json")
		rB.ServeHTTP(wShare, reqShare)

		if wShare.Code != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found when attempting to access/share foreign camera, got %d: %s", wShare.Code, wShare.Body.String())
		}
	})
}
