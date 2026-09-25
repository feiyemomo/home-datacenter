package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/camera"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/utils"
)

type mockUserResolver struct {
	adminUsers map[uint]bool
}

func (m *mockUserResolver) GetIsAdmin(userID uint) (bool, error) {
	return m.adminUsers[userID], nil
}

func setupCameraTestDB(t *testing.T) *gorm.DB {
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
	if err := db.AutoMigrate(&model.Camera{}, &model.CameraShare{}, &model.User{}, &model.Device{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestListAlertsAuthorization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupCameraTestDB(t)

	// User 1 = Admin, User 2 = Regular
	userRepo := repository.NewUserRepository(db)
	userRepo.Create(&model.User{Name: "admin", IsAdmin: true})
	userRepo.Create(&model.User{Name: "user2", IsAdmin: false})

	userResolver := &mockUserResolver{
		adminUsers: map[uint]bool{
			1: true,
			2: false,
		},
	}

	secretBox, _ := utils.NewSecretBox("test-secret-at-least-32-bytes-long-for-testing!!")
	camReg := camera.NewRegistry(db, nil, nil, secretBox, nil, "")

	// Insert Camera 1 owned by Admin (User 1)
	cam1 := &model.Camera{
		Name:       "Living Room",
		StreamName: "living_room",
		OwnerID:    1,
		Status:     "online",
		Type:       "camera",
	}
	db.Create(cam1)

	// Insert Camera 2 owned by User 2
	cam2 := &model.Camera{
		Name:       "User2 Room",
		StreamName: "user2_room",
		OwnerID:    2,
		Status:     "online",
		Type:       "camera",
	}
	db.Create(cam2)

	camHandler := NewCameraHandler(camReg, nil, nil, "", "", userResolver, nil)

	router := gin.New()
	router.GET("/api/v1/cameras/alerts", func(c *gin.Context) {
		// Mock auth context: set user_id from query param
		if uidStr := c.Query("as_user"); uidStr == "1" {
			c.Set("user_id", uint(1))
		} else if uidStr == "2" {
			c.Set("user_id", uint(2))
		}
		camHandler.ListAlerts(c)
	})

	t.Run("non-admin querying unpermitted camera_id gets 403 Forbidden", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cameras/alerts?as_user=2&camera_id=1", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Errorf("expected 403 Forbidden, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("non-admin querying unpermitted camera slug gets 403 Forbidden", func(t *testing.T) {
		slug := camReg.FrigateSlug(cam1)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cameras/alerts?as_user=2&camera="+slug, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Errorf("expected 403 Forbidden, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("admin querying camera_id 2 is allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cameras/alerts?as_user=1&camera_id=2", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200 OK for admin, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("user 2 querying own camera_id 2 is allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cameras/alerts?as_user=2&camera_id=2", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200 OK for camera owner, got %d: %s", w.Code, w.Body.String())
		}
	})
}

func TestCanReadEventHelper(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupCameraTestDB(t)

	userResolver := &mockUserResolver{
		adminUsers: map[uint]bool{
			1: true,
			2: false,
		},
	}

	secretBox, _ := utils.NewSecretBox("test-secret-at-least-32-bytes-long-for-testing!!")
	camReg := camera.NewRegistry(db, nil, nil, secretBox, nil, "")

	// Insert Camera 1 owned by User 1 (Admin)
	cam1 := &model.Camera{
		Name:       "Private Office",
		StreamName: "private_office",
		OwnerID:    1,
		Status:     "online",
		Type:       "camera",
	}
	db.Create(cam1)

	camHandler := NewCameraHandler(camReg, nil, nil, "", "", userResolver, nil)

	t.Run("unauthenticated context returns false and 401", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "/test", nil)

		ok := camHandler.canReadEvent(c, "evt_123")
		if ok {
			t.Errorf("expected canReadEvent to return false for unauthenticated")
		}
		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected status 401, got %d", w.Code)
		}
	})

	t.Run("admin context returns true unconditionally", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "/test", nil)
		c.Set("user_id", uint(1))

		ok := camHandler.canReadEvent(c, "evt_123")
		if !ok {
			t.Errorf("expected canReadEvent to return true for admin")
		}
	})
}
