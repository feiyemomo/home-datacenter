package repository

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
)

// setupRepoDB opens an in-memory SQLite DB (single connection so the
// in-memory store is shared across queries) with Device + User migrated.
func setupRepoDB(t *testing.T) *gorm.DB {
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

func TestDeviceRepositoryCRUD(t *testing.T) {
	db := setupRepoDB(t)
	r := NewDeviceRepository(db)

	// Create assigns IDs 1 and 2.
	d1 := &model.Device{UserID: 1, DeviceName: "phone", AccessKeyHash: "h1"}
	if err := r.Create(d1); err != nil {
		t.Fatalf("create: %v", err)
	}
	if d1.ID != 1 {
		t.Errorf("first device id = %d, want 1", d1.ID)
	}
	d2 := &model.Device{UserID: 1, DeviceName: "tablet", AccessKeyHash: "h2"}
	if err := r.Create(d2); err != nil {
		t.Fatalf("create: %v", err)
	}
	if d2.ID != 2 {
		t.Errorf("second device id = %d, want 2", d2.ID)
	}

	// GetByID.
	got, err := r.GetByID(1)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.DeviceName != "phone" || got.AccessKeyHash != "h1" {
		t.Errorf("device = %+v", got)
	}

	// GetByUserID.
	got2, err := r.GetByUserID(1)
	if err != nil {
		t.Fatalf("get by user id: %v", err)
	}
	if len(got2) != 2 {
		t.Errorf("got %d devices for user 1, want 2", len(got2))
	}

	// GetAll ordered by id.
	got3, err := r.GetAll()
	if err != nil {
		t.Fatalf("get all: %v", err)
	}
	if len(got3) != 2 || got3[0].ID != 1 || got3[1].ID != 2 {
		t.Errorf("get all = %+v, want [1,2]", got3)
	}

	// GetByAccessKeyHash.
	got4, err := r.GetByAccessKeyHash("h2")
	if err != nil {
		t.Fatalf("get by hash: %v", err)
	}
	if got4.ID != 2 {
		t.Errorf("hash lookup returned id %d, want 2", got4.ID)
	}

	// GetByUserIDAndHash.
	got5, err := r.GetByUserIDAndHash(1, "h1")
	if err != nil {
		t.Fatalf("get by user+hash: %v", err)
	}
	if got5.ID != 1 {
		t.Errorf("user+hash lookup returned id %d, want 1", got5.ID)
	}

	// Update.
	d1.DeviceName = "phone2"
	if err := r.Update(d1); err != nil {
		t.Fatalf("update: %v", err)
	}
	got1b, _ := r.GetByID(1)
	if got1b.DeviceName != "phone2" {
		t.Errorf("updated device name = %q, want phone2", got1b.DeviceName)
	}

	// Revoke / IsRevoked.
	if err := r.Revoke(1); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	revoked, err := r.IsRevoked(1)
	if err != nil {
		t.Fatalf("is revoked: %v", err)
	}
	if !revoked {
		t.Error("device 1 should be revoked")
	}
	notRevoked, _ := r.IsRevoked(2)
	if notRevoked {
		t.Error("device 2 should not be revoked")
	}

	// IncrementTokenVersion (default TokenVersion is 1).
	if err := r.IncrementTokenVersion(1); err != nil {
		t.Fatalf("increment token version: %v", err)
	}
	got1c, _ := r.GetByID(1)
	if got1c.TokenVersion != 2 {
		t.Errorf("token_version = %d, want 2", got1c.TokenVersion)
	}

	// UpdateLastSeen.
	if err := r.UpdateLastSeen(1, "10.0.0.9"); err != nil {
		t.Fatalf("update last seen: %v", err)
	}
	got1d, _ := r.GetByID(1)
	if got1d.LastIP != "10.0.0.9" || !got1d.LastSeenAt.Valid {
		t.Errorf("last_seen not persisted: ip=%q valid=%v", got1d.LastIP, got1d.LastSeenAt.Valid)
	}

	// Delete.
	if err := r.Delete(2); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := r.GetByID(2); err == nil {
		t.Error("deleted device should not be found")
	}

	// DeleteByUser hard-cascade.
	u1 := &model.Device{UserID: 7, DeviceName: "x", AccessKeyHash: "hx"}
	u2 := &model.Device{UserID: 7, DeviceName: "y", AccessKeyHash: "hy"}
	if err := r.Create(u1); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.Create(u2); err != nil {
		t.Fatalf("create: %v", err)
	}
	n, err := r.DeleteByUser(7)
	if err != nil {
		t.Fatalf("delete by user: %v", err)
	}
	if n != 2 {
		t.Errorf("delete by user removed %d rows, want 2", n)
	}
}

func TestDeviceRepository_IDReuse(t *testing.T) {
	db := setupRepoDB(t)
	r := NewDeviceRepository(db)

	d1 := &model.Device{UserID: 1, DeviceName: "a", AccessKeyHash: "ha"}
	d2 := &model.Device{UserID: 1, DeviceName: "b", AccessKeyHash: "hb"}
	if err := r.Create(d1); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.Create(d2); err != nil {
		t.Fatalf("create: %v", err)
	}
	if d1.ID != 1 || d2.ID != 2 {
		t.Fatalf("ids = %d,%d, want 1,2", d1.ID, d2.ID)
	}

	if err := r.Delete(1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	d3 := &model.Device{UserID: 1, DeviceName: "c", AccessKeyHash: "hc"}
	if err := r.Create(d3); err != nil {
		t.Fatalf("create: %v", err)
	}
	if d3.ID != 1 {
		t.Errorf("freed id 1 was not reused, got %d", d3.ID)
	}
}