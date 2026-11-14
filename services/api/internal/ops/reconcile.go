package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		if err := db.AutoMigrate(&OpsReconciliationRecord{}); err != nil {
			log.Printf("[reconcile] db automigrate failed: %v", err)
			return fmt.Errorf("failed to automigrate reconciliation record table: %w", err)
		}
		if err := db.Save(rec).Error; err != nil {
			log.Printf("[reconcile] db save failed: %v", err)
			return fmt.Errorf("failed to persist reconciliation record to database: %w", err)
		}
		return nil
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

// PresenceState represents the 3-state Docker container inspect outcome (R14.2).
type PresenceState string

const (
	PresencePresent PresenceState = "PRESENT"
	PresenceAbsent  PresenceState = "ABSENT"
	PresenceUnknown PresenceState = "UNKNOWN"
)

// ContainerPresence models the tri-state presence and health of a container in Docker (R14.2).
type ContainerPresence struct {
	State   PresenceState
	Running bool
	ID      string
	Name    string
	Status  string
	Err     error
}

func inspectContainerForReconcile(ctx context.Context, client *http.Client, target string) ContainerPresence {
	if target == "" {
		return ContainerPresence{State: PresenceAbsent}
	}
	u := fmt.Sprintf("http://localhost/containers/%s/json", url.PathEscape(target))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ContainerPresence{State: PresenceUnknown, Err: err}
	}
	resp, err := client.Do(req)
	if err != nil {
		return ContainerPresence{State: PresenceUnknown, Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ContainerPresence{State: PresenceAbsent}
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return ContainerPresence{
			State: PresenceUnknown,
			Err:   fmt.Errorf("inspect %s returned status %d: %s", target, resp.StatusCode, string(b)),
		}
	}

	var data struct {
		ID    string `json:"Id"`
		Name  string `json:"Name"`
		State struct {
			Running bool   `json:"Running"`
			Status  string `json:"Status"`
		} `json:"State"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil || data.ID == "" {
		return ContainerPresence{
			State: PresenceUnknown,
			Err:   fmt.Errorf("failed to decode inspect response for %s: %v", target, err),
		}
	}
	return ContainerPresence{
		State:   PresencePresent,
		Running: data.State.Running,
		ID:      data.ID,
		Name:    strings.TrimPrefix(data.Name, "/"),
		Status:  data.State.Status,
	}
}

// ReconcileOperation performs a safe 3-way reconciliation on an ambiguous record.
// Strictly adheres to the fail-closed boundaries:
// 1. Inspect Candidate, Backup, and Named container using Tri-State Presence (PRESENT, ABSENT, UNKNOWN).
// 2. If any target is UNKNOWN, fail-closed and escalate to MANUAL_APPROVAL without mutating any container.
// 3. If Candidate is PRESENT and running, and Backup is stopped/ABSENT:
//    - Perform live business health check probe via e.healthCheckFn (if configured).
//    - If health probe fails: escalate to MANUAL_APPROVAL and retain backup container.
//    - If health probe passes (or none configured): safely delete stopped backup container and mark RESOLVED.
// 4. Dual-active, broken candidate, or identity collision escalates to MANUAL_APPROVAL.
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

	discState := fmt.Sprintf("cand(id:%s,state:%s,run:%v); backup(id:%s,state:%s,run:%v); named(id:%s,state:%s,run:%v)",
		candPres.ID, candPres.State, candPres.Running,
		backupPres.ID, backupPres.State, backupPres.Running,
		namedPres.ID, namedPres.State, namedPres.Running)
	rec.DiscoveredState = discState

	// Strict Fail-Closed: If any inspect returned UNKNOWN, never touch any container and escalate to MANUAL_APPROVAL
	if candPres.State == PresenceUnknown || backupPres.State == PresenceUnknown || namedPres.State == PresenceUnknown {
		rec.State = ReconciliationStateManualApproval
		rec.ResolutionPlan = fmt.Sprintf("ambiguous inspect discovery (cand: %s, backup: %s, named: %s); fail-closed to manual approval",
			candPres.State, backupPres.State, namedPres.State)
		rec.UpdatedAtMs = time.Now().UTC().UnixMilli()
		_ = RecordReconciliation(e.db, rec)
		return &ReconcileResult{
			RecordID:        rec.ID,
			ActionTaken:     "ESCALATED_MANUAL_APPROVAL",
			FinalState:      ReconciliationStateManualApproval,
			DiscoveredState: discState,
			Error:           "ambiguous container presence state detected during inspect",
		}, nil
	}

	// Case A: Safe Candidate Verification (Candidate PRESENT and running, Backup is ABSENT or stopped)
	// Requires named container to either be ABSENT or match the Candidate ID.
	if candPres.State == PresencePresent && candPres.Running &&
		(backupPres.State == PresenceAbsent || (backupPres.State == PresencePresent && !backupPres.Running)) &&
		(namedPres.State == PresenceAbsent || (namedPres.State == PresencePresent && namedPres.ID == candPres.ID)) {

		// Requirement 5: Live business health check before purging backup
		if e.healthCheckFn != nil {
			if hcErr := e.healthCheckFn(ctx, rec.Component); hcErr != nil {
				rec.State = ReconciliationStateManualApproval
				rec.ResolutionPlan = fmt.Sprintf("candidate container running but business health check failed: %v; retaining backup for manual recovery", hcErr)
				rec.UpdatedAtMs = time.Now().UTC().UnixMilli()
				_ = RecordReconciliation(e.db, rec)
				return &ReconcileResult{
					RecordID:        rec.ID,
					ActionTaken:     "HEALTH_CHECK_FAILED_ESCALATED",
					FinalState:      ReconciliationStateManualApproval,
					DiscoveredState: discState,
					Error:           fmt.Sprintf("candidate business health check failed: %v", hcErr),
				}, nil
			}
		}

		// Clean up the stopped backup if it is PRESENT
		if backupPres.State == PresencePresent && isValidDockerContainerID(backupPres.ID) && backupPres.ID != candPres.ID {
			delURL := fmt.Sprintf("http://localhost/containers/%s?v=false&force=true", url.PathEscape(backupPres.ID))
			delReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, delURL, nil)
			if err != nil {
				rec.ResolutionPlan = fmt.Sprintf("candidate verified running; failed to prepare backup purge request: %v", err)
				rec.UpdatedAtMs = time.Now().UTC().UnixMilli()
				_ = RecordReconciliation(e.db, rec)
				return &ReconcileResult{
					RecordID:        rec.ID,
					ActionTaken:     "PURGE_FAILED_PENDING",
					FinalState:      rec.State,
					DiscoveredState: discState,
					Error:           err.Error(),
				}, fmt.Errorf("prepare delete backup request failed: %w", err)
			}
			dResp, dErr := client.Do(delReq)
			if dErr != nil {
				rec.ResolutionPlan = fmt.Sprintf("candidate verified running; backup purge network error: %v", dErr)
				rec.UpdatedAtMs = time.Now().UTC().UnixMilli()
				_ = RecordReconciliation(e.db, rec)
				return &ReconcileResult{
					RecordID:        rec.ID,
					ActionTaken:     "PURGE_FAILED_PENDING",
					FinalState:      rec.State,
					DiscoveredState: discState,
					Error:           dErr.Error(),
				}, fmt.Errorf("delete backup container failed: %w", dErr)
			}
			defer dResp.Body.Close()
			if dResp.StatusCode != http.StatusNoContent && dResp.StatusCode != http.StatusOK && dResp.StatusCode != http.StatusNotFound {
				b, _ := io.ReadAll(dResp.Body)
				rec.ResolutionPlan = fmt.Sprintf("candidate verified running; backup purge returned status %d: %s", dResp.StatusCode, string(b))
				rec.UpdatedAtMs = time.Now().UTC().UnixMilli()
				_ = RecordReconciliation(e.db, rec)
				return &ReconcileResult{
					RecordID:        rec.ID,
					ActionTaken:     "PURGE_FAILED_PENDING",
					FinalState:      rec.State,
					DiscoveredState: discState,
					Error:           fmt.Sprintf("status %d: %s", dResp.StatusCode, string(b)),
				}, fmt.Errorf("delete backup returned unexpected status %d", dResp.StatusCode)
			}
		}

		rec.State = ReconciliationStateResolved
		rec.ResolutionPlan = "candidate verified running and business healthy; orphan backup safely purged"
		rec.UpdatedAtMs = time.Now().UTC().UnixMilli()
		_ = RecordReconciliation(e.db, rec)
		return &ReconcileResult{
			RecordID:        rec.ID,
			ActionTaken:     "PURGED_BACKUP_AND_RESOLVED",
			FinalState:      ReconciliationStateResolved,
			DiscoveredState: discState,
		}, nil
	}

	// Case B: Escalation to Manual Approval (Dual-Running, Candidate not running, or conflicting identity)
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
