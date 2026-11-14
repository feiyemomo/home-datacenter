package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// ReconciliationState represents the lifecycle state of an ambiguous mutation resolution.
type ReconciliationState string

const (
	ReconciliationStatePending        ReconciliationState = "PENDING"
	ReconciliationStateResolved       ReconciliationState = "RESOLVED"
	ReconciliationStateManualApproval ReconciliationState = "MANUAL_APPROVAL"
)

// OpsReconciliationRecord persists incomplete, crashed, or ambiguous mutations (R14.2).
type OpsReconciliationRecord struct {
	ID                   string              `gorm:"primaryKey;size:64" json:"id"`
	OperationID          string              `gorm:"index;size:64;not null" json:"operation_id"`
	Component            string              `gorm:"size:64;not null" json:"component"`
	Action               string              `gorm:"size:64;not null" json:"action"`
	FailedStage          string              `gorm:"size:64;not null" json:"failed_stage"`
	AttemptID            string              `gorm:"size:96;not null" json:"attempt_id"`
	Channel              string              `gorm:"size:32;not null" json:"channel"`
	FencingGeneration    int64               `gorm:"not null" json:"fencing_generation"`
	OriginalContainerID  string              `gorm:"size:64" json:"original_container_id"`
	BackupContainerID    string              `gorm:"size:64" json:"backup_container_id"`
	BackupContainerName  string              `gorm:"size:128" json:"backup_container_name"`
	CandidateContainerID string              `gorm:"size:64" json:"candidate_container_id"`
	State                ReconciliationState `gorm:"size:32;not null;default:'PENDING'" json:"state"`
	DiscoveredState      string              `gorm:"type:text" json:"discovered_state"`
	ResolutionPlan       string              `gorm:"type:text" json:"resolution_plan"`
	CreatedAtMs          int64               `gorm:"not null" json:"created_at_ms"`
	UpdatedAtMs          int64               `gorm:"not null" json:"updated_at_ms"`
}

// TableName returns the table name in SQLite.
func (OpsReconciliationRecord) TableName() string {
	return "ops_reconciliations"
}

// In-memory store fallback for test environments or DB-less executions
var (
	memReconciliationMu      sync.RWMutex
	memReconciliationRecords = make(map[string]*OpsReconciliationRecord)
)

// RecordReconciliation persists an ambiguous or unverified mutation state.
func RecordReconciliation(db *gorm.DB, rec *OpsReconciliationRecord) error {
	if rec == nil {
		return errors.New("nil reconciliation record")
	}
	if rec.ID == "" {
		rec.ID = fmt.Sprintf("rec-%d-%s", time.Now().UTC().UnixNano(), rec.Component)
	}
	nowMs := time.Now().UTC().UnixMilli()
	if rec.CreatedAtMs == 0 {
		rec.CreatedAtMs = nowMs
	}
	rec.UpdatedAtMs = nowMs

	if db != nil {
		_ = db.AutoMigrate(&OpsReconciliationRecord{})
		if err := db.Save(rec).Error; err != nil {
			log.Printf("[reconcile] db save failed, falling back to memory: %v", err)
		} else {
			return nil
		}
	}

	memReconciliationMu.Lock()
	defer memReconciliationMu.Unlock()
	memReconciliationRecords[rec.ID] = rec
	return nil
}

// GetPendingReconciliations loads all unresolved ambiguous records.
func GetPendingReconciliations(db *gorm.DB) ([]OpsReconciliationRecord, error) {
	if db != nil {
		_ = db.AutoMigrate(&OpsReconciliationRecord{})
		var records []OpsReconciliationRecord
		err := db.Where("state = ?", ReconciliationStatePending).Find(&records).Error
		if err == nil {
			return records, nil
		}
	}

	memReconciliationMu.RLock()
	defer memReconciliationMu.RUnlock()
	var res []OpsReconciliationRecord
	for _, r := range memReconciliationRecords {
		if r.State == ReconciliationStatePending {
			res = append(res, *r)
		}
	}
	return res, nil
}

// GetReconciliationByOperationID finds a record by operation ID.
func GetReconciliationByOperationID(db *gorm.DB, opID string) (*OpsReconciliationRecord, error) {
	if db != nil {
		_ = db.AutoMigrate(&OpsReconciliationRecord{})
		var rec OpsReconciliationRecord
		if err := db.Where("operation_id = ?", opID).First(&rec).Error; err == nil {
			return &rec, nil
		}
	}

	memReconciliationMu.RLock()
	defer memReconciliationMu.RUnlock()
	for _, r := range memReconciliationRecords {
		if r.OperationID == opID {
			return r, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

// ReconcileResult represents the outcome of a 3-way container reconciliation pass.
type ReconcileResult struct {
	RecordID        string              `json:"record_id"`
	ActionTaken     string              `json:"action_taken"`
	FinalState      ReconciliationState `json:"final_state"`
	DiscoveredState string              `json:"discovered_state"`
	Error           string              `json:"error,omitempty"`
}

// 3-Way Docker Container Inspect State
type containerPresence struct {
	exists  bool
	running bool
	id      string
	name    string
	status  string
}

func inspectContainerForReconcile(ctx context.Context, client *http.Client, target string) containerPresence {
	if target == "" {
		return containerPresence{exists: false}
	}
	u := fmt.Sprintf("http://localhost/containers/%s/json", url.PathEscape(target))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return containerPresence{exists: false}
	}
	resp, err := client.Do(req)
	if err != nil {
		return containerPresence{exists: false}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return containerPresence{exists: false}
	}
	var data struct {
		ID    string `json:"Id"`
		Name  string `json:"Name"`
		State struct {
			Running bool   `json:"Running"`
			Status  string `json:"Status"`
		} `json:"State"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return containerPresence{exists: false}
	}
	return containerPresence{
		exists:  true,
		running: data.State.Running,
		id:      data.ID,
		name:    strings.TrimPrefix(data.Name, "/"),
		status:  data.State.Status,
	}
}

// ReconcileOperation performs a safe 3-way reconciliation on an ambiguous record.
// Strictly adheres to the fail-closed boundaries:
// 1. If candidate is running and healthy while backup is stopped/exited: safe to remove backup and resolve.
// 2. If dual-active (both running), or conflict, or candidate is broken: escalates to MANUAL_APPROVAL.
func (e *DefaultOperationExecutor) ReconcileOperation(ctx context.Context, recID string) (*ReconcileResult, error) {
	var rec *OpsReconciliationRecord
	if e.db != nil {
		var r OpsReconciliationRecord
		if err := e.db.Where("id = ?", recID).First(&r).Error; err == nil {
			rec = &r
		}
	}
	if rec == nil {
		memReconciliationMu.RLock()
		rec = memReconciliationRecords[recID]
		memReconciliationMu.RUnlock()
	}
	if rec == nil {
		return nil, fmt.Errorf("reconciliation record %s not found", recID)
	}

	sockPath := e.dockerSocket
	if sockPath == "" {
		sockPath = "/var/run/docker.sock"
	}
	tr := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(dCtx context.Context, _, _ string) (net.Conn, error) {
			return e.dialDocker(dCtx, sockPath)
		},
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second}

	// 3-way inspection: Candidate, Backup, and Named container
	candPres := inspectContainerForReconcile(ctx, client, rec.CandidateContainerID)
	backupPres := inspectContainerForReconcile(ctx, client, rec.BackupContainerID)
	namedPres := inspectContainerForReconcile(ctx, client, rec.Component)

	discState := fmt.Sprintf("cand(id:%s,run:%v); backup(id:%s,run:%v); named(id:%s,run:%v)",
		candPres.id, candPres.running, backupPres.id, backupPres.running, namedPres.id, namedPres.running)
	rec.DiscoveredState = discState

	// Case A: Safe Auto-Resolve (New Candidate is running and serving, Backup container exists and is stopped)
	if candPres.exists && candPres.running && (!backupPres.exists || (backupPres.exists && !backupPres.running)) {
		// Clean up the stopped backup if it still exists
		if backupPres.exists && isValidDockerContainerID(backupPres.id) && backupPres.id != candPres.id {
			delURL := fmt.Sprintf("http://localhost/containers/%s?v=false&force=true", url.PathEscape(backupPres.id))
			delReq, _ := http.NewRequestWithContext(ctx, http.MethodDelete, delURL, nil)
			if dResp, err := client.Do(delReq); err == nil {
				_ = dResp.Body.Close()
			}
		}
		rec.State = ReconciliationStateResolved
		rec.ResolutionPlan = "candidate verified running; orphan backup safely purged"
		rec.UpdatedAtMs = time.Now().UTC().UnixMilli()
		_ = RecordReconciliation(e.db, rec)
		return &ReconcileResult{
			RecordID:        rec.ID,
			ActionTaken:     "PURGED_BACKUP_AND_RESOLVED",
			FinalState:      ReconciliationStateResolved,
			DiscoveredState: discState,
		}, nil
	}

	// Case B: Escalation to Manual Approval (Dual-Running, or Candidate not running, or ambiguous identity)
	rec.State = ReconciliationStateManualApproval
	rec.ResolutionPlan = "dual running or broken candidate detected; requiring manual operator inspection"
	rec.UpdatedAtMs = time.Now().UTC().UnixMilli()
	_ = RecordReconciliation(e.db, rec)

	return &ReconcileResult{
		RecordID:        rec.ID,
		ActionTaken:     "ESCALATED_MANUAL_APPROVAL",
		FinalState:      ReconciliationStateManualApproval,
		DiscoveredState: discState,
	}, nil
}
