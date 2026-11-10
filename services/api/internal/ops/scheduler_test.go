package ops

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
)

type mockSchedulerExecutor struct {
	mu           sync.Mutex
	executeCalls []string
	executeCount int32
}

func (m *mockSchedulerExecutor) Execute(ctx context.Context, spec OperationSpec) (*ExecutionResult, error) {
	return &ExecutionResult{Success: true}, nil
}

func (m *mockSchedulerExecutor) ExecuteRollback(ctx context.Context, spec OperationSpec) error {
	return nil
}

func (m *mockSchedulerExecutor) ExecuteProposal(ctx context.Context, proposal *model.OpsProposal) (*ExecutionResult, error) {
	m.mu.Lock()
	m.executeCalls = append(m.executeCalls, proposal.ProposalID)
	m.mu.Unlock()
	atomic.AddInt32(&m.executeCount, 1)
	return &ExecutionResult{
		Success: true,
		Output:  "executed mock",
	}, nil
}

func (m *mockSchedulerExecutor) ValidateComponent(target string) (string, error) {
	return target, nil
}

func (m *mockSchedulerExecutor) CheckHealth(ctx context.Context, target string) error {
	return nil
}

func setupSchedulerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbName := fmt.Sprintf("file:sched_test_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test sqlite: %v", err)
	}
	err = db.AutoMigrate(
		&model.OpsProposal{},
		&model.OpsExecutionHistory{},
		&model.SystemLog{},
	)
	if err != nil {
		t.Fatalf("failed to auto migrate: %v", err)
	}
	return db
}

func TestProposalScheduler_Lifecycle(t *testing.T) {
	db := setupSchedulerTestDB(t)
	mockExec := &mockSchedulerExecutor{}
	sched := NewProposalScheduler(db, mockExec)
	sched.SetInterval(20 * time.Millisecond)

	if sched.IsRunning() {
		t.Fatalf("scheduler should not be running before Start")
	}

	sched.Start()
	if !sched.IsRunning() {
		t.Fatalf("scheduler should be running after Start")
	}

	// Idempotent start
	sched.Start()
	if !sched.IsRunning() {
		t.Fatalf("scheduler should still be running after second Start")
	}

	sched.Stop()
	if sched.IsRunning() {
		t.Fatalf("scheduler should not be running after Stop")
	}

	// Idempotent stop
	sched.Stop()
	if sched.IsRunning() {
		t.Fatalf("scheduler should remain stopped after second Stop")
	}
}

func TestProposalScheduler_Tick_CASClaim(t *testing.T) {
	db := setupSchedulerTestDB(t)
	mockExec := &mockSchedulerExecutor{}
	sched := NewProposalScheduler(db, mockExec)

	pastTime := time.Now().Add(-10 * time.Minute).Format(time.RFC3339)
	prop := model.OpsProposal{
		ProposalID:      "sched-prop-001",
		Title:           "Scheduled Restart",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusScheduled,
		ScheduledAt:     &pastTime,
		CreatedAt:       time.Now(),
	}
	if err := db.Create(&prop).Error; err != nil {
		t.Fatalf("failed to create proposal: %v", err)
	}

	// Run Tick
	sched.Tick()

	// Wait briefly for asynchronous goroutine dispatch
	time.Sleep(100 * time.Millisecond)

	var updated model.OpsProposal
	if err := db.Where("id = ?", prop.ID).First(&updated).Error; err != nil {
		t.Fatalf("failed to retrieve updated proposal: %v", err)
	}

	if updated.Status != model.ProposalStatusExecuting {
		t.Fatalf("expected status %s, got %s", model.ProposalStatusExecuting, updated.Status)
	}
	if !updated.ExecutedAt.Valid {
		t.Fatalf("expected executed_at to be valid NullTime")
	}

	if atomic.LoadInt32(&mockExec.executeCount) != 1 {
		t.Fatalf("expected 1 execution call, got %d", mockExec.executeCount)
	}
}

func TestProposalScheduler_Tick_FutureWindowSkipped(t *testing.T) {
	db := setupSchedulerTestDB(t)
	mockExec := &mockSchedulerExecutor{}
	sched := NewProposalScheduler(db, mockExec)

	futureTime := time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	prop := model.OpsProposal{
		ProposalID:      "sched-prop-002",
		Title:           "Future Scheduled Restart",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusScheduled,
		ScheduledAt:     &futureTime,
		CreatedAt:       time.Now(),
	}
	if err := db.Create(&prop).Error; err != nil {
		t.Fatalf("failed to create proposal: %v", err)
	}

	sched.Tick()
	time.Sleep(50 * time.Millisecond)

	var unchanged model.OpsProposal
	if err := db.Where("id = ?", prop.ID).First(&unchanged).Error; err != nil {
		t.Fatalf("failed to retrieve proposal: %v", err)
	}

	if unchanged.Status != model.ProposalStatusScheduled {
		t.Fatalf("expected proposal to remain %s, got %s", model.ProposalStatusScheduled, unchanged.Status)
	}
	if unchanged.ExecutedAt.Valid {
		t.Fatalf("executed_at should not be set for skipped proposal")
	}

	if atomic.LoadInt32(&mockExec.executeCount) != 0 {
		t.Fatalf("expected 0 execution calls for future proposal, got %d", mockExec.executeCount)
	}
}

func TestProposalScheduler_Tick_CASConflict(t *testing.T) {
	db := setupSchedulerTestDB(t)
	mockExec := &mockSchedulerExecutor{}
	sched := NewProposalScheduler(db, mockExec)

	pastTime := time.Now().Add(-5 * time.Minute).Format(time.RFC3339)
	prop := model.OpsProposal{
		ProposalID:      "sched-prop-003",
		Title:           "Concurrent Scheduled Task",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusScheduled,
		ScheduledAt:     &pastTime,
		CreatedAt:       time.Now(),
	}
	if err := db.Create(&prop).Error; err != nil {
		t.Fatalf("failed to create proposal: %v", err)
	}

	// Simulate concurrent transition: another agent or user cancelled or claimed it right before Tick's CAS
	db.Model(&model.OpsProposal{}).Where("id = ?", prop.ID).Update("status", model.ProposalStatusCancelled)

	sched.Tick()
	time.Sleep(50 * time.Millisecond)

	var res model.OpsProposal
	if err := db.Where("id = ?", prop.ID).First(&res).Error; err != nil {
		t.Fatalf("failed to query proposal: %v", err)
	}

	if res.Status != model.ProposalStatusCancelled {
		t.Fatalf("expected proposal to remain CANCELLED, got %s", res.Status)
	}
	if atomic.LoadInt32(&mockExec.executeCount) != 0 {
		t.Fatalf("cancelled proposal should not have been dispatched")
	}
}
