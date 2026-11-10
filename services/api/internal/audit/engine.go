package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/redact"
)

// EngineOption customizes AgentAuditEngine construction.
type EngineOption func(*AgentAuditEngine)

// WithArchiveDir configures where append-only JSONL logs, checkpoints, and emergency stubs are stored.
func WithArchiveDir(dir string) EngineOption {
	return func(e *AgentAuditEngine) {
		e.archiveDir = strings.TrimSpace(dir)
	}
}

// WithCheckpointInterval sets how many events trigger an automatic signed checkpoint anchor.
func WithCheckpointInterval(n int) EngineOption {
	return func(e *AgentAuditEngine) {
		if n > 0 {
			e.checkpointInterval = n
		}
	}
}

// WithClock overrides UTC time generation for deterministic tests.
func WithClock(fn func() time.Time) EngineOption {
	return func(e *AgentAuditEngine) {
		if fn != nil {
			e.nowFn = fn
		}
	}
}

// WithFaultHooks injects pre-commit and/or post-commit faults for automated failure testing.
func WithFaultHooks(preHook, postHook func(EmitEventInput) error) EngineOption {
	return func(e *AgentAuditEngine) {
		e.preHook = preHook
		e.postHook = postHook
	}
}

// WithFailureCallback registers a telemetry hook invoked whenever audit commit fails.
func WithFailureCallback(fn func()) EngineOption {
	return func(e *AgentAuditEngine) {
		e.onFailure = fn
	}
}

// AgentAuditEngine is the unified tamper-evident audit pipeline for autonomous agent and system operations.
type AgentAuditEngine struct {
	db                 *gorm.DB
	bus                *eventbus.Bus
	archiveDir         string
	checkpointInterval int
	writeMu            sync.Mutex
	nowFn              func() time.Time
	preHook            func(EmitEventInput) error
	postHook           func(EmitEventInput) error
	onFailure          func()
	degradedMu         sync.Mutex
	degradedWindow     map[string]time.Time
}

// NewAgentAuditEngine creates an AgentAuditEngine bound to db and eventbus.
func NewAgentAuditEngine(db *gorm.DB, bus *eventbus.Bus, opts ...EngineOption) *AgentAuditEngine {
	defaultDir := strings.TrimSpace(os.Getenv("OPS_AUDIT_ARCHIVE_DIR"))
	if defaultDir == "" {
		defaultDir = filepath.Join(os.TempDir(), "home-datacenter-audit")
	}
	e := &AgentAuditEngine{
		db:                 db,
		bus:                bus,
		archiveDir:         defaultDir,
		checkpointInterval: DefaultCheckpointInterval,
		nowFn:              func() time.Time { return time.Now().UTC() },
		degradedWindow:     make(map[string]time.Time),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(e)
		}
	}
	if db != nil {
		_ = e.EnsureAuditSchema()
	}
	return e
}

// SetFaultHooks dynamically updates pre/post fault injection hooks (intended for integration tests).
func (e *AgentAuditEngine) SetFaultHooks(preHook, postHook func(EmitEventInput) error) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	e.preHook = preHook
	e.postHook = postHook
}

func (e *AgentAuditEngine) nowUTC() time.Time {
	if e != nil && e.nowFn != nil {
		return e.nowFn().UTC().Truncate(time.Millisecond)
	}
	return time.Now().UTC().Truncate(time.Millisecond)
}

func (e *AgentAuditEngine) bgCtx() context.Context {
	return context.Background()
}

func (e *AgentAuditEngine) recordFailureMetric() {
	if e != nil && e.onFailure != nil {
		e.onFailure()
	}
}

// EnsureAuditSchema initializes audit tables and append-only triggers on clean databases while failing
// closed if a bootstrapped DB has lost any required audit table.
func (e *AgentAuditEngine) EnsureAuditSchema() error {
	if e == nil || e.db == nil {
		return ErrAuditCommitFailed
	}

	var hasMetaTbl int64
	_ = e.db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = 'ops_schema_meta'").Scan(&hasMetaTbl).Error
	var bootstrappedVal string
	if hasMetaTbl > 0 {
		_ = e.db.Raw("SELECT value FROM ops_schema_meta WHERE key = 'bootstrapped' LIMIT 1").Scan(&bootstrappedVal).Error
	}
	if bootstrappedVal == "1" {
		var ver int
		_ = e.db.Raw("SELECT COALESCE(MAX(version), 0) FROM ops_schema_migrations").Scan(&ver).Error
		if ver >= 5 {
			for _, tbl := range []string{"agent_audit_runs", "agent_audit_events", "agent_audit_checkpoints"} {
				var cnt int64
				if err := e.db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = ?", tbl).Scan(&cnt).Error; err != nil {
					return err
				}
				if cnt == 0 {
					return fmt.Errorf("%w: bootstrapped table %s is missing", ErrAuditCommitFailed, tbl)
				}
			}
			return nil
		}
	}

	if err := e.db.AutoMigrate(
		&model.AgentAuditRun{},
		&model.AgentAuditEvent{},
		&model.AgentAuditCheckpoint{},
		&model.StructuredOpsAuditLog{},
	); err != nil {
		return err
	}

	return e.db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_audit_events_run_seq ON agent_audit_events (run_id, seq);
		CREATE INDEX IF NOT EXISTS idx_audit_events_hh_time ON agent_audit_events (household_id, created_at_ms);
		CREATE INDEX IF NOT EXISTS idx_audit_events_comp_action ON agent_audit_events (component, action);

		CREATE TRIGGER IF NOT EXISTS trg_agent_audit_events_no_update
		BEFORE UPDATE ON agent_audit_events
		BEGIN
			SELECT RAISE(ABORT, 'agent_audit_events is append-only');
		END;

		CREATE TRIGGER IF NOT EXISTS trg_agent_audit_events_no_delete
		BEFORE DELETE ON agent_audit_events
		BEGIN
			SELECT RAISE(ABORT, 'agent_audit_events is append-only');
		END;

		CREATE TRIGGER IF NOT EXISTS trg_agent_audit_checkpoints_no_update
		BEFORE UPDATE ON agent_audit_checkpoints
		BEGIN
			SELECT RAISE(ABORT, 'agent_audit_checkpoints is append-only');
		END;

		CREATE TRIGGER IF NOT EXISTS trg_agent_audit_checkpoints_no_delete
		BEFORE DELETE ON agent_audit_checkpoints
		BEGIN
			SELECT RAISE(ABORT, 'agent_audit_checkpoints is append-only');
		END;
	`).Error
}

func (e *AgentAuditEngine) runImmediate(op func(conn *sql.Conn) error) error {
	if e == nil || e.db == nil {
		return ErrAuditCommitFailed
	}
	sqlDB, err := e.db.DB()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if err := op(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// StartRun creates a new correlated AgentAuditRun and attaches an AuditSession to the returned context.
func (e *AgentAuditEngine) StartRun(ctx context.Context, in StartRunInput) (string, context.Context, error) {
	now := e.nowUTC()
	nowMs := now.UnixMilli()
	runID := strings.TrimSpace(in.RunID)
	if runID == "" {
		runID = fmt.Sprintf("run-%d-%s", nowMs, generateRandomHex(4))
	}
	corrID := strings.TrimSpace(in.CorrelationID)
	if corrID == "" {
		corrID = fmt.Sprintf("corr-%d-%s", nowMs, generateRandomHex(4))
	}
	hhID := in.HouseholdID
	if hhID == 0 {
		hhID = 1
	}
	trig := defaultStr(in.TriggerSource, "api_manual")
	actorType := defaultStr(in.ActorType, "system_daemon")
	actorID := defaultStr(in.ActorID, "home-api")
	synced := true
	if in.Synced != nil {
		synced = *in.Synced
	}

	sess := AuditSession{
		RunID:         runID,
		CorrelationID: corrID,
		HouseholdID:   hhID,
		TriggerSource: trig,
		ActorType:     actorType,
		ActorID:       actorID,
	}
	outCtx := ContextWithSession(ctx, sess)

	if e == nil || e.db == nil {
		e.recordFailureMetric()
		return runID, outCtx, fmt.Errorf("%w: database unavailable", ErrAuditCommitFailed)
	}

	runRow := model.AgentAuditRun{
		RunID:           runID,
		CorrelationID:   corrID,
		HouseholdID:     hhID,
		TriggerSource:   trig,
		ActorType:       actorType,
		ActorID:         actorID,
		Action:          defaultStr(in.Action, "ops.task"),
		Target:          redact.SensitiveString(strings.TrimSpace(in.Target)),
		Component:       strings.TrimSpace(in.Component),
		Decision:        strings.TrimSpace(in.Decision),
		RiskLevel:       defaultStr(in.RiskLevel, model.AuditRiskMedium),
		Status:          model.AuditRunStatusRunning,
		ProposalID:      strings.TrimSpace(in.ProposalID),
		ApprovalID:      strings.TrimSpace(in.ApprovalID),
		OperationID:     strings.TrimSpace(in.OperationID),
		Summary:         CapAndRedactText(in.Summary, 1024),
		IsOffline:       in.IsOffline,
		Synced:          synced,
		IntegrityStatus: IntegrityChainIntactLocalOnly,
		StartedAt:       now,
		StartedAtMs:     nowMs,
	}

	e.writeMu.Lock()
	err := e.runImmediate(func(conn *sql.Conn) error {
		return insertRunIfMissingConn(conn, &runRow)
	})
	e.writeMu.Unlock()
	if err != nil {
		e.recordFailureMetric()
		return runID, outCtx, fmt.Errorf("%w: %v", ErrAuditCommitFailed, err)
	}

	if in.InitialEvent != nil {
		evIn := *in.InitialEvent
		evIn.RunID = runID
		evIn.CorrelationID = corrID
		if evIn.HouseholdID == 0 {
			evIn.HouseholdID = hhID
		}
		if evIn.Action == "" {
			evIn.Action = runRow.Action
		}
		if evIn.Target == "" {
			evIn.Target = redact.SensitiveString(strings.TrimSpace(in.Target))
		}
		if evIn.Component == "" {
			evIn.Component = runRow.Component
		}
		if evIn.Decision == "" {
			evIn.Decision = runRow.Decision
		}
		if evIn.RiskLevel == "" {
			evIn.RiskLevel = runRow.RiskLevel
		}
		if evIn.ProposalID == "" {
			evIn.ProposalID = runRow.ProposalID
		}
		if evIn.ApprovalID == "" {
			evIn.ApprovalID = runRow.ApprovalID
		}
		if evIn.OperationID == "" {
			evIn.OperationID = runRow.OperationID
		}
		ev, emitErr := e.EmitEvent(outCtx, evIn)
		if emitErr != nil {
			return runID, outCtx, emitErr
		}
		if ev != nil {
			sess.ParentEventID = ev.AuditID
			outCtx = ContextWithSession(outCtx, sess)
		}
	}

	return runID, outCtx, nil
}

// EmitEvent atomically appends a signed, hash-chained AgentAuditEvent onto the core stream.
func (e *AgentAuditEngine) EmitEvent(ctx context.Context, in EmitEventInput) (*model.AgentAuditEvent, error) {
	if e == nil {
		return nil, fmt.Errorf("%w: audit engine not initialized", ErrAuditCommitFailed)
	}
	sess, hasSess := SessionFromContext(ctx)
	now := e.nowUTC()
	if in.CreatedAt != nil && !in.CreatedAt.IsZero() {
		now = in.CreatedAt.UTC().Truncate(time.Millisecond)
	}
	nowMs := now.UnixMilli()

	runID := strings.TrimSpace(in.RunID)
	if runID == "" && hasSess {
		runID = sess.RunID
	}
	if runID == "" {
		runID = fmt.Sprintf("run-%d-%s", nowMs, generateRandomHex(4))
	}

	corrID := strings.TrimSpace(in.CorrelationID)
	if corrID == "" && hasSess {
		corrID = sess.CorrelationID
	}
	if corrID == "" {
		corrID = fmt.Sprintf("corr-%d-%s", nowMs, generateRandomHex(4))
	}

	parentID := strings.TrimSpace(in.ParentEventID)
	if parentID == "" && hasSess {
		parentID = sess.ParentEventID
	}

	hhID := in.HouseholdID
	if hhID == 0 && hasSess {
		hhID = sess.HouseholdID
	}
	if hhID == 0 {
		hhID = 1
	}

	trig := defaultStr(in.TriggerSource, sess.TriggerSource)
	trig = defaultStr(trig, "api_manual")
	actorType := defaultStr(in.ActorType, sess.ActorType)
	actorType = defaultStr(actorType, "system_daemon")
	actorID := defaultStr(in.ActorID, sess.ActorID)
	actorID = defaultStr(actorID, "home-api")

	auditID := strings.TrimSpace(in.AuditID)
	if auditID == "" {
		auditID = fmt.Sprintf("aud-%d-%s", nowMs, generateRandomHex(5))
	}

	synced := true
	if in.Synced != nil {
		synced = *in.Synced
	}

	paramDigest, paramRedacted := SerializeParams(in.Parameters)
	ev := &model.AgentAuditEvent{
		StreamID:           model.AuditPrimaryStreamID,
		AuditID:            auditID,
		RunID:              runID,
		ParentEventID:      parentID,
		CorrelationID:      corrID,
		HouseholdID:        hhID,
		TriggerSource:      trig,
		ActorType:          actorType,
		ActorID:            actorID,
		EventType:          defaultStr(in.EventType, model.AuditEventExecutionStepCompleted),
		Action:             defaultStr(in.Action, "ops.unknown"),
		Target:             redact.SensitiveString(strings.TrimSpace(in.Target)),
		Component:          strings.TrimSpace(in.Component),
		Decision:           strings.TrimSpace(in.Decision),
		RiskLevel:          defaultStr(in.RiskLevel, model.AuditRiskMedium),
		PolicyVersion:      defaultStr(in.PolicyVersion, PolicyVersion),
		ProposalID:         strings.TrimSpace(in.ProposalID),
		ApprovalID:         strings.TrimSpace(in.ApprovalID),
		OperationID:        strings.TrimSpace(in.OperationID),
		FencingGeneration:  in.FencingGeneration,
		BeforeState:        SerializeEvidence(in.BeforeState),
		AfterState:         SerializeEvidence(in.AfterState),
		ParametersDigest:   paramDigest,
		ParametersRedacted: paramRedacted,
		Result:             strings.TrimSpace(in.Result),
		ErrorCode:          CapAndRedactText(strings.TrimSpace(in.ErrorCode), 256),
		DurationMs:         in.DurationMs,
		Evidence:           SerializeEvidence(in.Evidence),
		CreatedAt:          now,
		CreatedAtMs:        nowMs,
		IsOffline:          in.IsOffline,
		Synced:             synced,
	}

	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	if e.preHook != nil {
		if hErr := e.preHook(in); hErr != nil {
			return ev, e.handleCommitFailure(in, ev, hErr)
		}
	}
	if e.db == nil {
		return ev, e.handleCommitFailure(in, ev, errors.New("audit database handle is nil"))
	}

	kid, ring, kErr := ResolveAuditHMACKeyring()
	if kErr != nil {
		return ev, e.handleCommitFailure(in, ev, kErr)
	}
	secret := ring[kid]

	var triggerCheckpoint bool
	err := e.runImmediate(func(conn *sql.Conn) error {
		var lastSeq uint64
		var lastHash string
		row := conn.QueryRowContext(e.bgCtx(),
			`SELECT seq, entry_hash FROM agent_audit_events WHERE stream_id = ? ORDER BY seq DESC LIMIT 1`,
			ev.StreamID,
		)
		if sErr := row.Scan(&lastSeq, &lastHash); sErr != nil && sErr != sql.ErrNoRows {
			return sErr
		}
		if lastHash == "" {
			lastHash = model.AuditGenesisHash
		}

		ev.Seq = lastSeq + 1
		ev.PreviousHash = lastHash
		entryHash, cErr := ComputeEventEntryHash(ev)
		if cErr != nil {
			return cErr
		}
		ev.EntryHash = entryHash
		ev.HMACSignature = ComputeEventHMAC(kid, secret, ev.StreamID, ev.Seq, ev.EntryHash, ev.CreatedAtMs)

		if _, execErr := conn.ExecContext(e.bgCtx(), `
			INSERT INTO agent_audit_events (
				stream_id, seq, audit_id, run_id, parent_event_id, correlation_id, household_id,
				trigger_source, actor_type, actor_id, event_type, action, target, component,
				decision, risk_level, policy_version, proposal_id, approval_id, operation_id,
				fencing_generation, before_state, after_state, parameters_digest, parameters_redacted,
				result, error_code, duration_ms, evidence, created_at, created_at_ms, is_offline, synced,
				origin_stream_id, origin_seq, origin_entry_hash, previous_hash, entry_hash, hmac_signature
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ev.StreamID, ev.Seq, ev.AuditID, ev.RunID, ev.ParentEventID, ev.CorrelationID, ev.HouseholdID,
			ev.TriggerSource, ev.ActorType, ev.ActorID, ev.EventType, ev.Action, ev.Target, ev.Component,
			ev.Decision, ev.RiskLevel, ev.PolicyVersion, ev.ProposalID, ev.ApprovalID, ev.OperationID,
			ev.FencingGeneration, ev.BeforeState, ev.AfterState, ev.ParametersDigest, ev.ParametersRedacted,
			ev.Result, ev.ErrorCode, ev.DurationMs, ev.Evidence, ev.CreatedAt.UTC(), ev.CreatedAtMs, boolInt(ev.IsOffline), boolInt(ev.Synced),
			ev.OriginStreamID, ev.OriginSeq, ev.OriginEntryHash, ev.PreviousHash, ev.EntryHash, ev.HMACSignature,
		); execErr != nil {
			return execErr
		}

		if upErr := upsertRunProgressConn(conn, ev, in.FinalRunStatus, in.RunSummary); upErr != nil {
			return upErr
		}

		if in.MirrorLegacy {
			if mErr := insertLegacyAuditMirrorConn(conn, ev, in); mErr != nil {
				return mErr
			}
		}

		if e.checkpointInterval > 0 && ev.Seq%uint64(e.checkpointInterval) == 0 {
			triggerCheckpoint = true
		}
		return nil
	})
	if err != nil {
		return ev, e.handleCommitFailure(in, ev, err)
	}

	if e.postHook != nil {
		if hErr := e.postHook(in); hErr != nil {
			return ev, e.handleCommitFailure(in, ev, hErr)
		}
	}

	if err := e.appendArchiveEvent(ev); err != nil {
		e.recordFailureMetric()
	}

	if triggerCheckpoint {
		if _, cpErr := e.createCheckpointLocked(ev.StreamID); cpErr != nil {
			e.recordFailureMetric()
		}
	}

	if e.bus != nil {
		if rawPayload, mErr := json.Marshal(map[string]interface{}{
			"audit_id":   ev.AuditID,
			"run_id":     ev.RunID,
			"seq":        ev.Seq,
			"event_type": ev.EventType,
			"action":     ev.Action,
			"component":  ev.Component,
			"result":     ev.Result,
			"risk_level": ev.RiskLevel,
			"is_offline": ev.IsOffline,
		}); mErr == nil {
			sev := eventbus.SeverityInfo
			if ev.EventType == model.AuditEventExecutionFailed || ev.EventType == model.AuditEventExecutionUnknown || ev.EventType == model.AuditEventAuditCommitFailed {
				sev = eventbus.SeverityWarn
			}
			e.bus.Publish(eventbus.Event{
				Topic:     eventbus.TopicAuditEventRecorded,
				Source:    eventbus.SourceOps,
				Severity:  sev,
				Payload:   rawPayload,
				Timestamp: ev.CreatedAt,
			})
		}
	}

	return ev, nil
}

// RecordPreMutation guarantees high-risk pre-execution audit commitment before touching side effects.
func (e *AgentAuditEngine) RecordPreMutation(ctx context.Context, in EmitEventInput) (*model.AgentAuditEvent, error) {
	if strings.TrimSpace(in.EventType) == "" {
		in.EventType = model.AuditEventExecutionStarted
	}
	if in.FailurePolicy == "" {
		in.FailurePolicy = PolicyFailClosed
	}
	return e.EmitEvent(ctx, in)
}

// RecordPostMutation commits terminal/post-mutation outcomes and marks EXECUTION_UNKNOWN + emergency stub on failure.
func (e *AgentAuditEngine) RecordPostMutation(ctx context.Context, in EmitEventInput, onUnknown func(error)) (*model.AgentAuditEvent, error) {
	ev, err := e.EmitEvent(ctx, in)
	if err == nil {
		return ev, nil
	}
	if in.RunID == "" {
		if sess, ok := SessionFromContext(ctx); ok {
			in.RunID = sess.RunID
		}
	}
	// Mark Run as EXECUTION_UNKNOWN best-effort if possible without rewriting immutable events.
	if in.RunID != "" && e != nil && e.db != nil {
		nowMs := e.nowUTC().UnixMilli()
		_ = e.db.Exec(
			`UPDATE agent_audit_runs SET status = ?, error_code = ?, ended_at = ?, ended_at_ms = ? WHERE run_id = ?`,
			model.AuditRunStatusExecutionUnknown,
			"AUDIT_COMMIT_FAILED",
			e.nowUTC(),
			nowMs,
			in.RunID,
		).Error
	}
	_ = e.appendEmergencyFallback(map[string]interface{}{
		"stub_type":      "POST_MUTATION_AUDIT_FAILED",
		"run_id":         in.RunID,
		"action":         in.Action,
		"target":         in.Target,
		"component":      in.Component,
		"result":         in.Result,
		"error":          redact.SensitiveString(err.Error()),
		"created_at":     FormatRFC3339Ms(e.nowUTC()),
		"requires_reconcile": true,
	})
	if onUnknown != nil {
		onUnknown(err)
	}
	return ev, err
}

// FinishRun explicitly finalizes a run header (e.g., when execution stopped without a separate terminal event).
func (e *AgentAuditEngine) FinishRun(runID, status, summary, errorCode string) error {
	if e == nil || e.db == nil || strings.TrimSpace(runID) == "" {
		return nil
	}
	now := e.nowUTC()
	nowMs := now.UnixMilli()
	return e.db.Exec(`
		UPDATE agent_audit_runs
		SET status = ?,
		    summary = CASE WHEN ? <> '' THEN ? ELSE summary END,
		    error_code = CASE WHEN ? <> '' THEN ? ELSE error_code END,
		    ended_at = ?,
		    ended_at_ms = ?,
		    duration_ms = CASE WHEN started_at_ms > 0 AND ? >= started_at_ms THEN ? - started_at_ms ELSE duration_ms END
		WHERE run_id = ?`,
		status,
		CapAndRedactText(summary, 1024), CapAndRedactText(summary, 1024),
		CapAndRedactText(errorCode, 256), CapAndRedactText(errorCode, 256),
		now, nowMs, nowMs, nowMs, runID,
	).Error
}

func (e *AgentAuditEngine) handleCommitFailure(in EmitEventInput, ev *model.AgentAuditEvent, cause error) error {
	e.recordFailureMetric()

	if in.FailurePolicy == PolicyBoundedEmergency && in.Action == "container.emergency_restart" {
		targetKey := defaultStr(in.Component, in.Target)
		if targetKey != "" && e.allowBoundedEmergency(targetKey) {
			stubErr := e.appendEmergencyFallback(map[string]interface{}{
				"stub_type":      "BOUNDED_EMERGENCY_DEGRADED",
				"audit_id":       ev.AuditID,
				"run_id":         ev.RunID,
				"component":      targetKey,
				"action":         in.Action,
				"created_at":     FormatRFC3339Ms(ev.CreatedAt),
				"db_error":       redact.SensitiveString(cause.Error()),
			})
			if stubErr == nil {
				return nil
			}
			return fmt.Errorf("%w: DB error (%v) and emergency stub failed (%v)", ErrAuditCommitFailed, cause, stubErr)
		}
		return fmt.Errorf("%w: %v", ErrAuditDegradedQuotaExceeded, cause)
	}

	if in.FailurePolicy == PolicyBestEffortTelemetry {
		return nil
	}

	return fmt.Errorf("%w: %v", ErrAuditCommitFailed, cause)
}

func (e *AgentAuditEngine) allowBoundedEmergency(component string) bool {
	e.degradedMu.Lock()
	defer e.degradedMu.Unlock()
	now := e.nowUTC()
	if last, ok := e.degradedWindow[component]; ok && now.Sub(last) < BoundedEmergencyWindow {
		return false
	}
	e.degradedWindow[component] = now
	return true
}

func insertRunIfMissingConn(conn *sql.Conn, r *model.AgentAuditRun) error {
	_, err := conn.ExecContext(context.Background(), `
		INSERT INTO agent_audit_runs (
			run_id, correlation_id, household_id, trigger_source, actor_type, actor_id,
			action, target, component, decision, risk_level, status,
			proposal_id, approval_id, operation_id, summary, error_code,
			is_offline, synced, event_count, first_seq, last_seq,
			last_audit_id, last_entry_hash, integrity_status,
			started_at, started_at_ms, ended_at_ms, duration_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO NOTHING`,
		r.RunID, r.CorrelationID, r.HouseholdID, r.TriggerSource, r.ActorType, r.ActorID,
		r.Action, r.Target, r.Component, r.Decision, r.RiskLevel, r.Status,
		r.ProposalID, r.ApprovalID, r.OperationID, r.Summary, r.ErrorCode,
		boolInt(r.IsOffline), boolInt(r.Synced), 0, 0, 0,
		"", "", r.IntegrityStatus,
		r.StartedAt.UTC(), r.StartedAtMs, 0, 0,
	)
	return err
}

func upsertRunProgressConn(conn *sql.Conn, ev *model.AgentAuditEvent, finalStatus, runSummary string) error {
	runStatus := model.AuditRunStatusRunning
	endedAtMs := int64(0)
	if strings.TrimSpace(finalStatus) != "" {
		runStatus = finalStatus
		endedAtMs = ev.CreatedAtMs
	} else {
		switch ev.EventType {
		case model.AuditEventExecutionCompleted, model.AuditEventRecoveryVerified:
			runStatus = model.AuditRunStatusCompleted
			endedAtMs = ev.CreatedAtMs
		case model.AuditEventRollbackCompleted:
			runStatus = model.AuditRunStatusRolledBack
			endedAtMs = ev.CreatedAtMs
		case model.AuditEventExecutionFailed, model.AuditEventApprovalRejected, model.AuditEventApprovalExpired:
			runStatus = model.AuditRunStatusFailed
			endedAtMs = ev.CreatedAtMs
		case model.AuditEventExecutionCancelled:
			runStatus = model.AuditRunStatusCancelled
			endedAtMs = ev.CreatedAtMs
		case model.AuditEventExecutionUnknown:
			runStatus = model.AuditRunStatusExecutionUnknown
			endedAtMs = ev.CreatedAtMs
		case model.AuditEventAuditCommitFailed:
			runStatus = model.AuditRunStatusAuditCommitFailed
			endedAtMs = ev.CreatedAtMs
		}
	}

	_, err := conn.ExecContext(context.Background(), `
		INSERT INTO agent_audit_runs (
			run_id, correlation_id, household_id, trigger_source, actor_type, actor_id,
			action, target, component, decision, risk_level, status,
			proposal_id, approval_id, operation_id, summary, error_code,
			is_offline, synced, event_count, first_seq, last_seq,
			last_audit_id, last_entry_hash, integrity_status,
			started_at, started_at_ms, ended_at_ms, duration_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
			event_count = agent_audit_runs.event_count + 1,
			first_seq = CASE WHEN agent_audit_runs.first_seq = 0 THEN excluded.first_seq ELSE agent_audit_runs.first_seq END,
			last_seq = excluded.last_seq,
			last_audit_id = excluded.last_audit_id,
			last_entry_hash = excluded.last_entry_hash,
			decision = CASE WHEN excluded.decision <> '' THEN excluded.decision ELSE agent_audit_runs.decision END,
			proposal_id = CASE WHEN excluded.proposal_id <> '' THEN excluded.proposal_id ELSE agent_audit_runs.proposal_id END,
			approval_id = CASE WHEN excluded.approval_id <> '' THEN excluded.approval_id ELSE agent_audit_runs.approval_id END,
			operation_id = CASE WHEN excluded.operation_id <> '' THEN excluded.operation_id ELSE agent_audit_runs.operation_id END,
			error_code = CASE WHEN excluded.error_code <> '' THEN excluded.error_code ELSE agent_audit_runs.error_code END,
			summary = CASE WHEN excluded.summary <> '' THEN excluded.summary ELSE agent_audit_runs.summary END,
			status = CASE WHEN ? <> 'RUNNING' THEN ? ELSE agent_audit_runs.status END,
			ended_at = CASE WHEN ? > 0 THEN ? ELSE agent_audit_runs.ended_at END,
			ended_at_ms = CASE WHEN ? > 0 THEN ? ELSE agent_audit_runs.ended_at_ms END,
			duration_ms = CASE WHEN ? > 0 AND ? >= agent_audit_runs.started_at_ms THEN ? - agent_audit_runs.started_at_ms ELSE agent_audit_runs.duration_ms END`,
		ev.RunID, ev.CorrelationID, ev.HouseholdID, ev.TriggerSource, ev.ActorType, ev.ActorID,
		ev.Action, ev.Target, ev.Component, ev.Decision, ev.RiskLevel, runStatus,
		ev.ProposalID, ev.ApprovalID, ev.OperationID, CapAndRedactText(runSummary, 1024), ev.ErrorCode,
		boolInt(ev.IsOffline), boolInt(ev.Synced), ev.Seq, ev.Seq,
		ev.AuditID, ev.EntryHash, IntegrityChainIntactLocalOnly,
		ev.CreatedAt.UTC(), ev.CreatedAtMs, endedAtMs, int64(0),
		runStatus, runStatus,
		endedAtMs, ev.CreatedAt.UTC(),
		endedAtMs, endedAtMs,
		endedAtMs, endedAtMs, endedAtMs,
	)
	return err
}

func insertLegacyAuditMirrorConn(conn *sql.Conn, ev *model.AgentAuditEvent, in EmitEventInput) error {
	targetType := defaultStr(in.LegacyTargetType, "component")
	execPayload := ev.AfterState
	if execPayload == "" {
		execPayload = ev.Evidence
	}
	_, err := conn.ExecContext(context.Background(), `
		INSERT OR IGNORE INTO structured_ops_audit_logs (
			event_uuid, household_id, request_id, actor_type, actor_id, actor_name,
			client_ip, target_type, target_id, action, proposal_id, approver,
			execution_result, rollback_result, is_offline_action, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.AuditID, ev.HouseholdID, ev.CorrelationID, ev.ActorType, 0, ev.ActorID,
		in.LegacyClientIP, targetType, defaultStr(ev.Target, ev.Component), ev.Action, ev.ProposalID, in.LegacyApprover,
		execPayload, "", boolInt(ev.IsOffline), ev.CreatedAt.UTC(),
	)
	return err
}

func defaultStr(v, fallback string) string {
	trimmed := strings.TrimSpace(v)
	if trimmed == "" {
		return fallback
	}
	return trimmed
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
