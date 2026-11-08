package ops

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/config"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/security"
)

func setupFSMTestDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.OpsExecutionHistory{},
		&model.OpsProposal{},
		&model.SecurityAlertFilter{},
		&model.SecurityRecipient{},
	); err != nil {
		t.Fatalf("failed to auto-migrate test tables: %v", err)
	}
	return db
}

func TestSafetyFSM_SuccessFlow(t *testing.T) {
	db := setupFSMTestDB(t)
	backupDir := t.TempDir()

	// Mock health probe returning 200 OK
	probeCalls := 0
	probeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCalls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer probeSrv.Close()

	alertMgr := security.NewAlertManager(config.AlertConfig{Enabled: false}, db)
	fsm := NewSafetyFSM(db, nil, alertMgr, backupDir, probeSrv.URL)

	applied := false
	rolledBack := false

	applyFn := func() error {
		applied = true
		return nil
	}
	rollbackFn := func() error {
		rolledBack = true
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := fsm.ExecuteSafeUpdate(ctx, "home-api", "v1.17.0", "v1.18.0", applyFn, rollbackFn)
	if err != nil {
		t.Fatalf("expected successful update, got error: %v", err)
	}

	if !applied {
		t.Fatalf("expected applyFn to be called")
	}
	if rolledBack {
		t.Fatalf("expected rollbackFn NOT to be called")
	}
	if fsm.CurrentState() != StateIdle {
		t.Fatalf("expected FSM state to return to IDLE, got %s", fsm.CurrentState())
	}
	if probeCalls == 0 {
		t.Fatalf("expected health probe to be called at least once")
	}

	// Verify history persisted in SQLite
	var history model.OpsExecutionHistory
	if err := db.Where("component = ? AND status = ?", "home-api", "success").First(&history).Error; err != nil {
		t.Fatalf("failed to find success history in db: %v", err)
	}
	if history.OldVersion != "v1.17.0" || history.NewVersion != "v1.18.0" {
		t.Fatalf("history version mismatch: %+v", history)
	}

	// Verify physical snapshot was created by SQLite VACUUM INTO
	snapshots, err := os.ReadDir(backupDir)
	if err != nil || len(snapshots) == 0 {
		t.Fatalf("expected physical snapshot file in backupDir, found: %v, err: %v", snapshots, err)
	}
}

func TestSafetyFSM_ProbeFailure_RollsBack(t *testing.T) {
	db := setupFSMTestDB(t)
	backupDir := t.TempDir()

	// Mock health probe returning 500 error
	probeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"error"}`))
	}))
	defer probeSrv.Close()

	alertMgr := security.NewAlertManager(config.AlertConfig{Enabled: false}, db)
	// Enable AutoRollback filter
	filter := alertMgr.GetAlertFilters()
	filter.AutoRollback = true
	alertMgr.UpdateAlertFilters(filter)

	fsm := NewSafetyFSM(db, nil, alertMgr, backupDir, probeSrv.URL)

	applied := false
	rolledBack := false

	applyFn := func() error {
		applied = true
		return nil
	}
	rollbackFn := func() error {
		rolledBack = true
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := fsm.ExecuteSafeUpdate(ctx, "home-api", "v1.17.0", "v1.18.0", applyFn, rollbackFn)
	if err == nil {
		t.Fatalf("expected error from failed probe, got nil")
	}

	if !applied {
		t.Fatalf("expected applyFn to have run")
	}
	if !rolledBack {
		t.Fatalf("expected rollbackFn to be invoked on health probe failure")
	}
	if fsm.CurrentState() != StateIdle {
		t.Fatalf("expected FSM state to return to IDLE after deferred cleanup, got %s", fsm.CurrentState())
	}

	// Verify history record with rolled_back status
	var history model.OpsExecutionHistory
	if err := db.Where("component = ? AND status = ?", "home-api", "rolled_back").First(&history).Error; err != nil {
		t.Fatalf("failed to find rolled_back history in db: %v", err)
	}
	if history.Component != "home-api" {
		t.Fatalf("history component mismatch: %+v", history)
	}
}

func TestSafetyFSM_AutoRollbackFilterDisabled(t *testing.T) {
	db := setupFSMTestDB(t)
	backupDir := t.TempDir()

	// Failing probe
	probeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer probeSrv.Close()

	alertMgr := security.NewAlertManager(config.AlertConfig{Enabled: false}, db)
	// Disable AutoRollback alert filter
	filter := alertMgr.GetAlertFilters()
	filter.AutoRollback = false
	alertMgr.UpdateAlertFilters(filter)

	fsm := NewSafetyFSM(db, nil, alertMgr, backupDir, probeSrv.URL)

	rolledBack := false
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = fsm.ExecuteSafeUpdate(ctx, "home-api", "v1.17.0", "v1.18.0",
		func() error { return nil },
		func() error {
			rolledBack = true
			return nil
		},
	)

	if !rolledBack {
		t.Fatalf("expected rollbackFn to execute even when notification is disabled")
	}
}

func TestSafetyFSM_BusyStateGuard(t *testing.T) {
	db := setupFSMTestDB(t)
	backupDir := t.TempDir()

	fsm := NewSafetyFSM(db, nil, nil, backupDir, "http://127.0.0.1:9999/health")
	fsm.mu.Lock()
	fsm.currentState = StateApplying
	fsm.mu.Unlock()

	err := fsm.ExecuteSafeUpdate(context.Background(), "home-api", "v1", "v2", nil, nil)
	if err == nil {
		t.Fatalf("expected error when FSM is busy, got nil")
	}
}

func TestSafetyFSM_IsOffPeak(t *testing.T) {
	peakTime := time.Date(2026, 11, 8, 14, 30, 0, 0, time.UTC)
	if IsOffPeak(peakTime) {
		t.Fatalf("expected 14:30 to be peak time")
	}

	offPeakTime := time.Date(2026, 11, 8, 3, 15, 0, 0, time.UTC)
	if !IsOffPeak(offPeakTime) {
		t.Fatalf("expected 03:15 to be off-peak time")
	}

	// Boundary test: 02:00 is off-peak
	boundary2AM := time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC)
	if !IsOffPeak(boundary2AM) {
		t.Fatalf("expected 02:00 to be off-peak")
	}

	// Boundary test: 05:00 is not off-peak (window is [2, 5))
	boundary5AM := time.Date(2026, 11, 8, 5, 0, 0, 0, time.UTC)
	if IsOffPeak(boundary5AM) {
		t.Fatalf("expected 05:00 to be outside off-peak window")
	}
}
