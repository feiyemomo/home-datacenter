package repository

import (
	"testing"

	"home-datacenter-api/internal/model"
)

func TestUserRepositoryCRUD(t *testing.T) {
	db := setupRepoDB(t)
	r := NewUserRepository(db)

	u1 := &model.User{Name: "alice", IsAdmin: true}
	u2 := &model.User{Name: "bob"}
	if err := r.Create(u1); err != nil {
		t.Fatalf("create: %v", err)
	}
	if u1.ID != 1 {
		t.Errorf("first user id = %d, want 1", u1.ID)
	}
	if err := r.Create(u2); err != nil {
		t.Fatalf("create: %v", err)
	}
	if u2.ID != 2 {
		t.Errorf("second user id = %d, want 2", u2.ID)
	}

	// GetByID.
	got, err := r.GetByID(1)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.Name != "alice" || !got.IsAdmin {
		t.Errorf("user = %+v, want alice/admin", got)
	}

	// GetByName.
	gotN, err := r.GetByName("bob")
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	if gotN.ID != 2 {
		t.Errorf("get by name returned id %d, want 2", gotN.ID)
	}

	// List.
	list, err := r.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("list len = %d, want 2", len(list))
	}

	// CountAdmins.
	n, err := r.CountAdmins()
	if err != nil {
		t.Fatalf("count admins: %v", err)
	}
	if n != 1 {
		t.Errorf("admin count = %d, want 1", n)
	}

	// CountDevicesByUser.
	if err := db.Create(&model.Device{UserID: 1, DeviceName: "d", AccessKeyHash: "hd"}).Error; err != nil {
		t.Fatalf("seed device: %v", err)
	}
	cnt, err := r.CountDevicesByUser(1)
	if err != nil {
		t.Fatalf("count devices: %v", err)
	}
	if cnt != 1 {
		t.Errorf("device count for user 1 = %d, want 1", cnt)
	}

	// Update.
	u1.IsAdmin = false
	if err := r.Update(u1); err != nil {
		t.Fatalf("update: %v", err)
	}
	got1, _ := r.GetByID(1)
	if got1.IsAdmin {
		t.Error("user 1 should no longer be admin after update")
	}

	// Delete + ID reuse.
	if err := r.Delete(2); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := r.GetByID(2); err == nil {
		t.Error("deleted user should not be found")
	}
	u3 := &model.User{Name: "carol"}
	if err := r.Create(u3); err != nil {
		t.Fatalf("create: %v", err)
	}
	if u3.ID != 2 {
		t.Errorf("freed user id 2 was not reused, got %d", u3.ID)
	}
}

func TestUserRepository_UniqueName(t *testing.T) {
	db := setupRepoDB(t)
	r := NewUserRepository(db)

	if err := r.Create(&model.User{Name: "alice"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := r.Create(&model.User{Name: "alice"}); err == nil {
		t.Error("duplicate user name should violate the unique constraint")
	}
}