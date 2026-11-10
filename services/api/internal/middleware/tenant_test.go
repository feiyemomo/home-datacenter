package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
)

func setupTenantDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open memory db: %v", err)
	}
	if err := db.AutoMigrate(&model.Household{}, &model.User{}); err != nil {
		t.Fatalf("failed to auto migrate: %v", err)
	}
	return db
}

func TestTenantContext_InternalService(t *testing.T) {
	db := setupTenantDB(t)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("is_internal_service", true)
		c.Next()
	})
	r.Use(TenantContext(db))

	var capturedHouseholdID uint
	r.GET("/tenant-test", func(c *gin.Context) {
		capturedHouseholdID = GetHouseholdID(c)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/tenant-test", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if capturedHouseholdID != 1 {
		t.Fatalf("expected internal service to be pinned to household 1, got %d", capturedHouseholdID)
	}
}

func TestTenantContext_UserHouseholdLookup(t *testing.T) {
	db := setupTenantDB(t)

	hh2 := model.Household{ID: 2, Name: "家庭二号"}
	db.Create(&hh2)

	user2 := model.User{
		ID:          42,
		HouseholdID: 2,
		Name:        "Alice",
	}
	db.Create(&user2)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", uint(42))
		c.Next()
	})
	r.Use(TenantContext(db))

	var capturedHouseholdID uint
	r.GET("/tenant-test", func(c *gin.Context) {
		capturedHouseholdID = GetHouseholdID(c)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/tenant-test", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if capturedHouseholdID != 2 {
		t.Fatalf("expected user 42 to resolve to household 2, got %d", capturedHouseholdID)
	}
}
