package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/service"
	"home-datacenter-api/internal/utils"
)

func setupTestDB(t *testing.T) *gorm.DB {
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
	return db
}

func TestAuthVerifyRoleAndAdminEnforcement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	utils.JWTSecret = "test-secret-at-least-32-bytes-long-for-testing!!"

	db := setupTestDB(t)
	userRepo := repository.NewUserRepository(db)
	deviceRepo := repository.NewDeviceRepository(db)
	authSvc := service.NewAuthService(userRepo, deviceRepo)
	authH := NewAuthHandler(authSvc, nil)

	// Create an admin user and a regular user
	adminUser := &model.User{Name: "admin_user", IsAdmin: true}
	if err := userRepo.Create(adminUser); err != nil {
		t.Fatalf("create admin user: %v", err)
	}
	regularUser := &model.User{Name: "regular_user", IsAdmin: false}
	if err := userRepo.Create(regularUser); err != nil {
		t.Fatalf("create regular user: %v", err)
	}

	// Create devices for both users
	adminDev := &model.Device{UserID: adminUser.ID, DeviceName: "admin_dev", TokenVersion: 1}
	if err := deviceRepo.Create(adminDev); err != nil {
		t.Fatalf("create admin device: %v", err)
	}
	regularDev := &model.Device{UserID: regularUser.ID, DeviceName: "reg_dev", TokenVersion: 1}
	if err := deviceRepo.Create(regularDev); err != nil {
		t.Fatalf("create regular device: %v", err)
	}

	adminToken, err := utils.GenerateToken(adminUser.ID, adminDev.ID, adminDev.TokenVersion)
	if err != nil {
		t.Fatalf("generate admin token: %v", err)
	}
	regularToken, err := utils.GenerateToken(regularUser.ID, regularDev.ID, regularDev.TokenVersion)
	if err != nil {
		t.Fatalf("generate regular token: %v", err)
	}

	router := gin.New()
	router.GET("/api/v1/auth/verify", authH.Verify)

	t.Run("regular user standard verify sets viewer role", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/verify", nil)
		req.Header.Set("Authorization", "Bearer "+regularToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("X-Auth-User"); got != "regular_user" {
			t.Errorf("X-Auth-User = %q, want 'regular_user'", got)
		}
		if got := w.Header().Get("X-Auth-Role"); got != "viewer" {
			t.Errorf("X-Auth-Role = %q, want 'viewer'", got)
		}

		var resp struct {
			Data struct {
				Role     string `json:"role"`
				UserName string `json:"user_name"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if resp.Data.Role != "viewer" || resp.Data.UserName != "regular_user" {
			t.Errorf("unexpected body payload: %+v", resp.Data)
		}
	})

	t.Run("admin user standard verify sets admin role", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/verify", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("X-Auth-User"); got != "admin_user" {
			t.Errorf("X-Auth-User = %q, want 'admin_user'", got)
		}
		if got := w.Header().Get("X-Auth-Role"); got != "admin" {
			t.Errorf("X-Auth-Role = %q, want 'admin'", got)
		}
	})

	t.Run("regular user fails verify when require_admin is true", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/verify?require_admin=true", nil)
		req.Header.Set("Authorization", "Bearer "+regularToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Errorf("expected 403 Forbidden for non-admin on require_admin=true, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("admin user passes verify when require_admin is true", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/verify?require_admin=true", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200 OK for admin on require_admin=true, got %d: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("X-Auth-Role"); got != "admin" {
			t.Errorf("X-Auth-Role = %q, want 'admin'", got)
		}
	})
}
