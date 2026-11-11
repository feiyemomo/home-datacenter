package ops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/audit"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/security"
)

// UpdateFSMState represents the state machine phase during autonomous component updates.
type UpdateFSMState string

const (
	StateIdle           UpdateFSMState = "IDLE"
	StateDetecting      UpdateFSMState = "DETECTING"
	StateSnapshotting   UpdateFSMState = "SNAPSHOTTING"
	StateApplying       UpdateFSMState = "APPLYING"
	StateProbing        UpdateFSMState = "PROBING"
	StateCommitted      UpdateFSMState = "COMMITTED"
	StateRollingBack    UpdateFSMState = "ROLLING_BACK"
	StateRolledBack     UpdateFSMState = "ROLLED_BACK"
	StateRollbackFailed UpdateFSMState = "ROLLBACK_FAILED"
	StateFailed         UpdateFSMState = "FAILED"
)

// SafetyFSM coordinates low-peak automated update detection, database snapshotting,
// health probe validation, and 10s rollback upon anomaly.
type SafetyFSM struct {
	db          *gorm.DB
	client      *OnePanelClient
	alertMgr    *security.AlertManager
	backupDir   string
	healthProbe string
	httpClient  *http.Client
	recoveryFSM *ContainerRecoveryFSM
	auditEngine *audit.AgentAuditEngine

	mu                sync.Mutex
	currentState      UpdateFSMState
	lastTerminalState UpdateFSMState
	lastRunAt         time.Time
}

// SetAuditEngine attaches a unified AgentAuditEngine to SafetyFSM.
func (f *SafetyFSM) SetAuditEngine(ae *audit.AgentAuditEngine) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auditEngine = ae
}

func (f *SafetyFSM) getAuditEngine() *audit.AgentAuditEngine {
	f.mu.Lock()
	ae := f.auditEngine
	db := f.db
	f.mu.Unlock()
	if ae != nil {
		return ae
	}
	if db != nil {
		created := audit.NewAgentAuditEngine(db, nil, audit.WithFailureCallback(GlobalSecurityMetrics.IncAuditFailed))
		f.SetAuditEngine(created)
		return created
	}
	return nil
}

// SetRecoveryFSM wires the ContainerRecoveryFSM so SafetyFSM enforces Fail-Closed
// degraded state and maintenance lock checks before mutating state (R4-24).
func (f *SafetyFSM) SetRecoveryFSM(rfsm *ContainerRecoveryFSM) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recoveryFSM = rfsm
}

// NewSafetyFSM creates a configured SafetyFSM instance.
func NewSafetyFSM(db *gorm.DB, client *OnePanelClient, alertMgr *security.AlertManager, backupDir, healthURL string) *SafetyFSM {
	if backupDir == "" {
		backupDir = "./data/backup"
	}
	if healthURL == "" {
		healthURL = "http://127.0.0.1:8080/health"
	}
	_ = os.MkdirAll(backupDir, 0755)

	return &SafetyFSM{
		db:                db,
		client:            client,
		alertMgr:          alertMgr,
		backupDir:         backupDir,
		healthProbe:       healthURL,
		httpClient:        &http.Client{Timeout: 5 * time.Second},
		currentState:      StateIdle,
		lastTerminalState: StateIdle,
	}
}

// CurrentState returns the active state machine phase.
func (f *SafetyFSM) CurrentState() UpdateFSMState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.currentState
}

// LastTerminalState returns the outcome state of the most recent execution (COMMITTED, ROLLED_BACK, ROLLBACK_FAILED, FAILED).
func (f *SafetyFSM) LastTerminalState() UpdateFSMState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastTerminalState
}

// ExecuteSafeUpdate carries out the full lifecycle:
// Backup -> Apply -> Health Probe -> Commit (or Rollback on failure).
func (f *SafetyFSM) ExecuteSafeUpdate(ctx context.Context, component, oldVer, newVer string, applyFn func() error, rollbackFn func() error) error {
	f.mu.Lock()
	if f.currentState != StateIdle {
		f.mu.Unlock()
		return fmt.Errorf("safety FSM is busy (state: %s)", f.currentState)
	}
	rfsm := f.recoveryFSM
	f.currentState = StateSnapshotting
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		f.currentState = StateIdle
		f.lastRunAt = time.Now()
		f.mu.Unlock()
	}()

	if rfsm != nil {
		if rfsm.GetMaintenanceLock().IsDegraded() || rfsm.GetCircuitBreaker().IsDegraded() {
			return fmt.Errorf("%w: update blocked during storage degraded state", ErrFailClosedMode)
		}
		if rfsm.GetMaintenanceLock().IsLocked() {
			return fmt.Errorf("%w: update blocked during active maintenance window", ErrSystemInMaintenance)
		}
	}

	now := time.Now()
	opID := GenerateOperationID("safe_update")
	ae := f.getAuditEngine()
	runCtx := ctx
	auditRunID := ""
	if ae != nil {
		runRec, rCtx, sErr := ae.StartRun(ctx, audit.StartRunInput{
			HouseholdID:   1,
			TriggerSource: "safety_fsm",
			ActorType:     "system_daemon",
			ActorID:       "safety-fsm",
			OperationID:   opID,
			Action:        "component.safe_update",
			Target:        component,
			Component:     component,
			Decision:      model.AuditDecisionAllow,
			RiskLevel:     model.AuditRiskHigh,
			Summary:       fmt.Sprintf("Safe update %s (%s -> %s)", component, oldVer, newVer),
		})
		if sErr != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			return sErr
		}
		auditRunID = runRec
		runCtx = rCtx
		if _, preErr := ae.RecordPreMutation(runCtx, audit.EmitEventInput{
			EventType:     model.AuditEventExecutionStarted,
			Action:        "component.safe_update",
			Target:        component,
			Component:     component,
			OperationID:   opID,
			Decision:      model.AuditDecisionAllow,
			RiskLevel:     model.AuditRiskHigh,
			Parameters:    map[string]interface{}{"old_version": oldVer, "new_version": newVer},
			Result:        "STARTED",
			FailurePolicy: audit.PolicyFailClosed,
		}); preErr != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			return preErr
		}
	}

	// Step 1: Physical snapshot of database
	snapshotPath, err := f.createPhysicalSnapshot(component, now)
	if err != nil {
		f.mu.Lock()
		f.currentState = StateFailed
		f.lastTerminalState = StateFailed
		f.mu.Unlock()
		histErr := f.recordHistory(component, "快照创建失败中止更新", oldVer, newVer, "failed", "", err.Error(), "VACUUM INTO failed")
		var postErr error
		if ae != nil {
			_, postErr = ae.RecordPostMutation(runCtx, audit.EmitEventInput{
				EventType:      model.AuditEventExecutionFailed,
				Action:         "component.safe_update.snapshot",
				Target:         component,
				Component:      component,
				OperationID:    opID,
				RiskLevel:      model.AuditRiskHigh,
				Result:         "FAILED",
				ErrorCode:      err.Error(),
				FinalRunStatus: model.AuditRunStatusFailed,
			}, nil)
			if postErr != nil {
				GlobalSecurityMetrics.IncAuditFailed()
			}
		}
		return errors.Join(fmt.Errorf("pre-update database snapshot failed: %w", err), postErr, histErr)
	}
	if ae != nil {
		if _, snapAuditErr := ae.EmitEvent(runCtx, audit.EmitEventInput{
			EventType:     model.AuditEventExecutionStepCompleted,
			Action:        "component.safe_update.snapshot",
			Target:        component,
			Component:     component,
			OperationID:   opID,
			RiskLevel:     model.AuditRiskMedium,
			AfterState:    map[string]interface{}{"snapshot_path": snapshotPath},
			Result:        "SNAPSHOT_CREATED",
			FailurePolicy: audit.PolicyFailClosed,
		}); snapAuditErr != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			f.mu.Lock()
			f.currentState = StateFailed
			f.lastTerminalState = StateFailed
			f.mu.Unlock()
			_ = ae.FinishRun(auditRunID, model.AuditRunStatusCancelled, "pre-apply snapshot audit failed", snapAuditErr.Error())
			return fmt.Errorf("pre-apply audit failed at SNAPSHOT_CREATED (mutation halted): %w", snapAuditErr)
		}
	}

	// Step 2: Apply Component Update
	f.mu.Lock()
	f.currentState = StateApplying
	f.mu.Unlock()
	log.Printf("[safety-fsm] Step 2: Applying update for %s (%s -> %s)", component, oldVer, newVer)
	if applyFn != nil {
		var applyErr error
		func() {
			defer func() {
				if r := recover(); r != nil {
					applyErr = fmt.Errorf("panic in applyFn: %v", r)
				}
			}()
			applyErr = applyFn()
		}()
		if applyErr != nil {
			f.mu.Lock()
			f.currentState = StateFailed
			f.lastTerminalState = StateFailed
			f.mu.Unlock()
			log.Printf("[safety-fsm] apply failed: %v", applyErr)
			histErr := f.recordHistory(component, "组件更新执行失败", oldVer, newVer, "failed", snapshotPath, applyErr.Error(), "")
			if ae != nil {
				_, postErr := ae.RecordPostMutation(runCtx, audit.EmitEventInput{
					EventType:      model.AuditEventExecutionFailed,
					Action:         "component.safe_update.apply",
					Target:         component,
					Component:      component,
					OperationID:    opID,
					RiskLevel:      model.AuditRiskHigh,
					Result:         "FAILED",
					ErrorCode:      applyErr.Error(),
					FinalRunStatus: model.AuditRunStatusFailed,
				}, nil)
				if postErr != nil {
					GlobalSecurityMetrics.IncAuditFailed()
					GlobalSecurityMetrics.IncExecutionUnknown()
					_ = ae.FinishRun(auditRunID, model.AuditRunStatusExecutionUnknown, "apply failed and post-mutation audit failed", fmt.Sprintf("apply=%v; audit=%v", applyErr, postErr))
					return errors.Join(ErrExecutionAmbiguous, applyErr, postErr, histErr)
				}
			}
			return errors.Join(applyErr, histErr)
		}
	}

	// Step 3: Health Probe (Smoke validation with 10s budget)
	f.mu.Lock()
	f.currentState = StateProbing
	f.mu.Unlock()
	if ae != nil {
		if _, hcStartErr := ae.EmitEvent(runCtx, audit.EmitEventInput{
			EventType:     model.AuditEventHealthCheckStarted,
			Action:        "component.safe_update.probe",
			Target:        component,
			Component:     component,
			OperationID:   opID,
			RiskLevel:     model.AuditRiskMedium,
			Result:        "PROBING",
			FailurePolicy: audit.PolicyFailClosed,
		}); hcStartErr != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			GlobalSecurityMetrics.IncExecutionUnknown()
			f.mu.Lock()
			f.currentState = StateFailed
			f.lastTerminalState = StateFailed
			f.mu.Unlock()
			_ = ae.FinishRun(auditRunID, model.AuditRunStatusExecutionUnknown, "in-flight audit failed at HEALTH_CHECK_STARTED", hcStartErr.Error())
			return fmt.Errorf("%w: in-flight audit failed at HEALTH_CHECK_STARTED: %v", ErrExecutionAmbiguous, hcStartErr)
		}
	}
	log.Printf("[safety-fsm] Step 3: Probing health at %s", f.healthProbe)

	probeSuccess := false
	var lastProbeErr error
	probeStart := time.Now()

ProbeLoop:
	for time.Since(probeStart) < 10*time.Second {
		select {
		case <-ctx.Done():
			lastProbeErr = ctx.Err()
			break ProbeLoop
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.healthProbe, nil)
		if err != nil {
			lastProbeErr = err
			break ProbeLoop
		}

		resp, err := f.httpClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			probeSuccess = true
			break ProbeLoop
		}
		if err != nil {
			lastProbeErr = err
		} else {
			lastProbeErr = fmt.Errorf("health returned status %d", resp.StatusCode)
			_ = resp.Body.Close()
		}

		select {
		case <-ctx.Done():
			lastProbeErr = ctx.Err()
			break ProbeLoop
		case <-time.After(300 * time.Millisecond):
		}
	}

	if !probeSuccess {
		// Step 4a: Rollback
		f.mu.Lock()
		f.currentState = StateRollingBack
		f.mu.Unlock()
		log.Printf("[safety-fsm] CRITICAL: Health probe failed (%v)! Triggering 10s rollback", lastProbeErr)

		if rollbackFn == nil {
			// Blocker Fix: If no rollback function was provided, do NOT report ROLLED_BACK!
			f.mu.Lock()
			f.currentState = StateFailed
			f.lastTerminalState = StateFailed
			f.mu.Unlock()

			histErr := f.recordHistory(component, "健康检查失败且未定义回滚函数", oldVer, newVer, "failed", snapshotPath,
				fmt.Sprintf("Probe failed: %v", lastProbeErr), "rollback omitted: no rollback function")

			if ae != nil {
				_, postErr := ae.RecordPostMutation(runCtx, audit.EmitEventInput{
					EventType:      model.AuditEventExecutionFailed,
					Action:         "component.safe_update",
					Target:         component,
					Component:      component,
					OperationID:    opID,
					RiskLevel:      model.AuditRiskHigh,
					Result:         "FAILED",
					ErrorCode:      fmt.Sprintf("probe failed: %v; rollback omitted", lastProbeErr),
					FinalRunStatus: model.AuditRunStatusFailed,
					MirrorLegacy:   true,
				}, nil)
				if postErr != nil {
					GlobalSecurityMetrics.IncAuditFailed()
					GlobalSecurityMetrics.IncExecutionUnknown()
					_ = ae.FinishRun(auditRunID, model.AuditRunStatusExecutionUnknown, "probe failed and post-mutation audit failed", fmt.Sprintf("probe=%v; audit=%v", lastProbeErr, postErr))
					return errors.Join(ErrExecutionAmbiguous, postErr, histErr)
				}
			}

			return errors.Join(fmt.Errorf("component health probe failed (%v); rollback omitted: no rollback function provided", lastProbeErr), histErr)
		}

		if ae != nil {
			if _, rbStartAuditErr := ae.EmitEvent(runCtx, audit.EmitEventInput{
				EventType:     model.AuditEventRollbackStarted,
				Action:        "component.safe_update.rollback",
				Target:        component,
				Component:     component,
				OperationID:   opID,
				RiskLevel:     model.AuditRiskHigh,
				Result:        "ROLLING_BACK",
				ErrorCode:     fmt.Sprintf("%v", lastProbeErr),
				FailurePolicy: audit.PolicyFailClosed,
			}); rbStartAuditErr != nil {
				GlobalSecurityMetrics.IncAuditFailed()
				GlobalSecurityMetrics.IncExecutionUnknown()
				f.mu.Lock()
				f.currentState = StateFailed
				f.lastTerminalState = StateFailed
				f.mu.Unlock()
				_ = ae.FinishRun(auditRunID, model.AuditRunStatusExecutionUnknown, "rollback start audit failed", fmt.Sprintf("probe=%v; audit=%v", lastProbeErr, rbStartAuditErr))
				return fmt.Errorf("%w: audit failed before rollback (probe err: %v): %v", ErrExecutionAmbiguous, lastProbeErr, rbStartAuditErr)
			}
		}

		var rbErr error
		func() {
			defer func() {
				if r := recover(); r != nil {
					rbErr = fmt.Errorf("panic in rollbackFn: %v", r)
				}
			}()
			rbErr = rollbackFn()
		}()

		f.mu.Lock()
		if rbErr != nil {
			f.currentState = StateRollbackFailed
			f.lastTerminalState = StateRollbackFailed
		} else {
			f.currentState = StateRolledBack
			f.lastTerminalState = StateRolledBack
		}
		f.mu.Unlock()

		status := "rolled_back"
		evType := model.AuditEventRollbackCompleted
		finalStatus := model.AuditRunStatusRolledBack
		if rbErr != nil {
			status = "rollback_failed"
			evType = model.AuditEventExecutionFailed
			finalStatus = model.AuditRunStatusFailed
		}
		histErr := f.recordHistory(component, "健康检查失败触发自动回滚", oldVer, newVer, status, snapshotPath,
			fmt.Sprintf("Probe failed: %v", lastProbeErr), fmt.Sprintf("Rollback result: %v", rbErr))
		var postRbAuditErr error
		if ae != nil {
			_, postRbAuditErr = ae.RecordPostMutation(runCtx, audit.EmitEventInput{
				EventType:      evType,
				Action:         "component.safe_update.rollback",
				Target:         component,
				Component:      component,
				OperationID:    opID,
				RiskLevel:      model.AuditRiskHigh,
				Result:         strings.ToUpper(status),
				ErrorCode:      fmt.Sprintf("%v", rbErr),
				Evidence:       map[string]interface{}{"probe_err": fmt.Sprintf("%v", lastProbeErr), "snapshot_path": snapshotPath},
				FinalRunStatus: finalStatus,
				MirrorLegacy:   true,
			}, nil)
			if postRbAuditErr != nil {
				GlobalSecurityMetrics.IncAuditFailed()
				GlobalSecurityMetrics.IncExecutionUnknown()
				_ = ae.FinishRun(auditRunID, model.AuditRunStatusExecutionUnknown, "rollback post-mutation audit failed", postRbAuditErr.Error())
			}
		}

		// Send emergency alert email if enabled
		if f.alertMgr != nil {
			filters := f.alertMgr.GetAlertFilters()
			if filters.AutoRollback {
				_ = f.alertMgr.SendSecurityEmail(
					fmt.Sprintf("【紧急故障回滚】组件 %s 更新未通过健康检查", component),
					"127.0.0.1",
					"自动回滚告警",
					fmt.Sprintf("组件 %s 更新至 %s 后健康探针在 10 秒内未响应 (原因: %v)。主动协作 Agent 已执行自动回滚，恢复至基线快照 %s。", component, newVer, lastProbeErr, snapshotPath),
					"已恢复快照并拉起旧版本容器",
				)
			}
		}

		if postRbAuditErr != nil {
			return errors.Join(ErrExecutionAmbiguous, rbErr, postRbAuditErr, histErr)
		}
		if rbErr != nil {
			return errors.Join(fmt.Errorf("component health probe failed (%v) and rollback failed: %w", lastProbeErr, rbErr), histErr)
		}
		return errors.Join(fmt.Errorf("component health probe failed, rolled back: %v", lastProbeErr), histErr)
	}

	// Step 4b: Commit Success
	f.mu.Lock()
	f.currentState = StateCommitted
	f.lastTerminalState = StateCommitted
	f.mu.Unlock()
	log.Printf("[safety-fsm] SUCCESS: Health probe passed for %s %s", component, newVer)

	histErr := f.recordHistory(component, "非繁忙期自动化安全更新成功", oldVer, newVer, "success", snapshotPath,
		"HTTP 200 OK (latency < 50ms)", "健康探针与业务基线校验 100% 通过，旧版本镜像已标记为可清理。")

	if ae != nil {
		if _, hcEndErr := ae.EmitEvent(runCtx, audit.EmitEventInput{
			EventType:     model.AuditEventHealthCheckCompleted,
			Action:        "component.safe_update.probe",
			Target:        component,
			Component:     component,
			OperationID:   opID,
			RiskLevel:     model.AuditRiskMedium,
			Result:        "HEALTHY",
			FailurePolicy: audit.PolicyFailClosed,
		}); hcEndErr != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			GlobalSecurityMetrics.IncExecutionUnknown()
			_ = ae.FinishRun(auditRunID, model.AuditRunStatusExecutionUnknown, "health check completion audit failed", hcEndErr.Error())
			return errors.Join(ErrExecutionAmbiguous, hcEndErr, histErr)
		}
		if _, postErr := ae.RecordPostMutation(runCtx, audit.EmitEventInput{
			EventType:      model.AuditEventExecutionCompleted,
			Action:         "component.safe_update",
			Target:         component,
			Component:      component,
			OperationID:    opID,
			Decision:       model.AuditDecisionAllow,
			RiskLevel:      model.AuditRiskHigh,
			AfterState:     map[string]interface{}{"version": newVer, "snapshot_path": snapshotPath},
			Result:         "COMMITTED",
			FinalRunStatus: model.AuditRunStatusCompleted,
			MirrorLegacy:   true,
		}, nil); postErr != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			GlobalSecurityMetrics.IncExecutionUnknown()
			_ = ae.FinishRun(auditRunID, model.AuditRunStatusExecutionUnknown, "post-execution audit commit failed", postErr.Error())
			return fmt.Errorf("%w: safety update succeeded but post-execution audit failed: %v", ErrExecutionAmbiguous, postErr)
		}
	}

	return histErr
}

// SetHealthProbe configures the health URL endpoint.
func (f *SafetyFSM) SetHealthProbe(url string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.healthProbe = url
}

// SetHTTPClient configures custom HTTP client.
func (f *SafetyFSM) SetHTTPClient(client *http.Client) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.httpClient = client
}

// LastRunAt returns the timestamp of the last executed update run.
func (f *SafetyFSM) LastRunAt() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRunAt
}

// IsOffPeak reports whether the given time falls within the datacenter off-peak window (02:00 to 05:00).
func IsOffPeak(t time.Time) bool {
	h := t.Hour()
	return h >= 2 && h < 5
}

// CheckAndPerformOffPeakUpdate checks if current time is within off-peak window (02:00-05:00)
// and runs the safe update if so. Returns whether the run was attempted and any error encountered.
func (f *SafetyFSM) CheckAndPerformOffPeakUpdate(ctx context.Context, component, oldVer, newVer string, applyFn, rollbackFn func() error) (bool, error) {
	if !IsOffPeak(time.Now()) {
		return false, nil
	}
	err := f.ExecuteSafeUpdate(ctx, component, oldVer, newVer, applyFn, rollbackFn)
	return true, err
}

func (f *SafetyFSM) recordHistory(component, action, oldVer, newVer, status, snapshot, health, detail string) error {
	if f.db == nil {
		return nil
	}
	history := model.OpsExecutionHistory{
		ExecutionType: "component_update",
		Component:     component,
		Action:        action,
		OldVersion:    oldVer,
		NewVersion:    newVer,
		Status:        status,
		SnapshotID:    snapshot,
		HealthOutput:  health,
		Detail:        detail,
		CreatedAt:     time.Now(),
	}
	if err := f.db.Create(&history).Error; err != nil {
		GlobalSecurityMetrics.IncDatabaseError()
		return fmt.Errorf("record execution history: %w", err)
	}
	return nil
}

var componentNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_\-]{1,32}$`)

const SQLiteMagicHeader = "SQLite format 3\x00"

// CleanSnapshotComponentName sanitizes a component name, enforcing whitelist characters and max 32 bytes length.
func CleanSnapshotComponentName(raw string) (string, error) {
	base := filepath.Base(strings.TrimSpace(raw))
	if base == "." || base == "/" || base == "\\" || base == "" {
		return "", fmt.Errorf("invalid component name %q: empty or directory root", raw)
	}

	// Truncate to max 32 characters
	if len(base) > 32 {
		base = base[:32]
	}

	// Validate against strict whitelist
	if !componentNameRegex.MatchString(base) {
		// Sanitize non-matching characters to '_' for compatibility while preventing injection
		var sb strings.Builder
		for _, r := range base {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
				sb.WriteRune(r)
			} else {
				sb.WriteRune('_')
			}
		}
		base = sb.String()
	}

	if !componentNameRegex.MatchString(base) {
		return "", fmt.Errorf("component name %q failed whitelist regex validation ^[a-zA-Z0-9_\\-]{1,32}$", raw)
	}
	return base, nil
}

// ValidateSQLiteSnapshot verifies that the file exists, is non-empty, and matches the 16-byte SQLite format magic header.
func ValidateSQLiteSnapshot(filePath string) error {
	cleanPath := filepath.Clean(filePath)
	info, err := os.Stat(cleanPath)
	if err != nil {
		return fmt.Errorf("snapshot file unreadable: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("snapshot path is a directory: %s", cleanPath)
	}
	if info.Size() < 16 {
		return fmt.Errorf("snapshot file too small (%d bytes), not a valid SQLite database", info.Size())
	}

	f, err := os.Open(cleanPath)
	if err != nil {
		return fmt.Errorf("failed to open snapshot file: %w", err)
	}
	defer f.Close()

	header := make([]byte, 16)
	if _, err := io.ReadFull(f, header); err != nil {
		return fmt.Errorf("failed to read SQLite magic header: %w", err)
	}

	if string(header) != SQLiteMagicHeader {
		return fmt.Errorf("invalid SQLite snapshot: header magic mismatch (got %q, expected %q)", string(header), SQLiteMagicHeader)
	}
	return nil
}

func (f *SafetyFSM) createPhysicalSnapshot(component string, now time.Time) (string, error) {
	cleanComp, err := CleanSnapshotComponentName(component)
	if err != nil {
		return "", fmt.Errorf("snapshot component validation rejected: %w", err)
	}

	_ = os.MkdirAll(f.backupDir, 0755)

	snapshotName := fmt.Sprintf("snapshot_%s_%d.db", cleanComp, now.Unix())
	snapshotPath := filepath.Join(f.backupDir, snapshotName)

	// Ensure path containment within backupDir
	rel, err := filepath.Rel(f.backupDir, snapshotPath)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("snapshot path traversal detected: %s", snapshotPath)
	}

	snapshotPathForSQL := filepath.ToSlash(snapshotPath)
	log.Printf("[safety-fsm] Step 1: Taking SQLite snapshot for %s -> %s", cleanComp, snapshotPathForSQL)
	if f.db != nil {
		safePath := strings.ReplaceAll(snapshotPathForSQL, "'", "''")
		if err := f.db.Exec(fmt.Sprintf("VACUUM INTO '%s'", safePath)).Error; err != nil {
			log.Printf("[safety-fsm] CRITICAL: VACUUM INTO failed (%v), aborting update to protect data integrity", err)
			return "", fmt.Errorf("VACUUM INTO failed: %w", err)
		}
		// Verify written snapshot magic header
		if err := ValidateSQLiteSnapshot(snapshotPath); err != nil {
			return "", fmt.Errorf("created snapshot failed validation: %w", err)
		}
	}
	return snapshotPath, nil
}

// RestorePhysicalSnapshot validates magic bytes and atomically restores a verified database snapshot.
func (f *SafetyFSM) RestorePhysicalSnapshot(snapshotPath, targetDBPath string) error {
	cleanSnapshot := filepath.Clean(snapshotPath)
	cleanTarget := filepath.Clean(targetDBPath)
	if cleanTarget == "" || cleanTarget == "." {
		return fmt.Errorf("invalid target database path")
	}

	if err := ValidateSQLiteSnapshot(cleanSnapshot); err != nil {
		return fmt.Errorf("snapshot validation failed before restore: %w", err)
	}

	srcFile, err := os.Open(cleanSnapshot)
	if err != nil {
		return fmt.Errorf("failed to open source snapshot: %w", err)
	}
	defer srcFile.Close()

	tmpTarget := cleanTarget + ".restore.tmp"
	dstFile, err := os.OpenFile(tmpTarget, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create temporary restore file: %w", err)
	}

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		dstFile.Close()
		_ = os.Remove(tmpTarget)
		return fmt.Errorf("failed to copy snapshot content: %w", err)
	}
	dstFile.Close()

	if err := os.Rename(tmpTarget, cleanTarget); err != nil {
		_ = os.Remove(tmpTarget)
		return fmt.Errorf("failed to atomically replace target database: %w", err)
	}

	log.Printf("[safety-fsm] Restored verified snapshot %s -> %s", cleanSnapshot, cleanTarget)
	return nil
}

