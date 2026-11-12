package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

// WithRemoteStore configures an off-DB append-only/WORM remote audit replica sink and verifier.
func WithRemoteStore(store RemoteAuditStore) EngineOption {
	return func(e *AgentAuditEngine) {
		e.remoteStore = store
	}
}

// WithExternalSigner configures an asymmetric external signer (e.g. Ed25519) for checkpoint anchoring.
func WithExternalSigner(signer ExternalCheckpointSigner) EngineOption {
	return func(e *AgentAuditEngine) {
		e.externalSigner = signer
		if v, ok := signer.(ExternalCheckpointVerifier); ok && e.externalVerifier == nil {
			e.externalVerifier = v
		}
	}
}

// WithExternalVerifier configures an external public-key verifier for asymmetric checkpoint verification.
func WithExternalVerifier(verifier ExternalCheckpointVerifier) EngineOption {
	return func(e *AgentAuditEngine) {
		e.externalVerifier = verifier
	}
}

// WithCheckpointPeriod enables periodic background checkpoint creation and remote catch-up sync.
func WithCheckpointPeriod(period time.Duration) EngineOption {
	return func(e *AgentAuditEngine) {
		if period > 0 {
			e.checkpointPeriod = period
		}
	}
}

// WithStrictRemote explicitly enforces or relaxes strict remote replication mode.
// In strict mode, missing or unreachable remote audit stores fail closed instead of silently degrading.
func WithStrictRemote(strict bool) EngineOption {
	return func(e *AgentAuditEngine) {
		e.strictRemote = strict
		e.strictRemoteSet = true
	}
}

// AgentAuditEngine is the unified tamper-evident audit pipeline for autonomous agent and system operations.
type AgentAuditEngine struct {
	db                 *gorm.DB
	bus                *eventbus.Bus
	archiveDir         string
	checkpointInterval int
	checkpointPeriod   time.Duration
	strictRemote       bool
	strictRemoteSet    bool
	initErr            error
	writeMu            sync.Mutex
	remoteMu           sync.Mutex
	remoteStore        RemoteAuditStore
	externalSigner     ExternalCheckpointSigner
	externalVerifier   ExternalCheckpointVerifier
	nowFn              func() time.Time
	preHook            func(EmitEventInput) error
	postHook           func(EmitEventInput) error
	onFailure          func()
	degradedMu         sync.Mutex
	degradedWindow     map[string]time.Time
	stopOnce           sync.Once
	stopCh             chan struct{}
	loopWG             sync.WaitGroup
}

func isAuditProductionRuntime() bool {
	appEnv := strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV")))
	if appEnv == "development" || appEnv == "dev" || appEnv == "test" || appEnv == "testing" {
		return false
	}
	if appEnv == "production" || appEnv == "prod" {
		return true
	}
	env := strings.ToLower(strings.TrimSpace(os.Getenv("ENV")))
	if env == "development" || env == "dev" || env == "test" || env == "testing" {
		return false
	}
	if env == "production" || env == "prod" {
		return true
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if os.Getenv("APP_CONFIG") == "/configs/config.yaml" {
		return true
	}
	return false
}

func resolveStrictRemoteEnv() (val bool, set bool, err error) {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("OPS_AUDIT_STRICT_REMOTE")))
	switch raw {
	case "":
		return false, false, nil
	case "1", "true", "yes", "on":
		return true, true, nil
	case "0", "false", "no", "off":
		return false, true, nil
	default:
		return false, false, fmt.Errorf("%w: invalid OPS_AUDIT_STRICT_REMOTE %q", ErrAuditConfigInvalid, raw)
	}
}

// NewStrictAgentAuditEngine creates an AgentAuditEngine and fails closed on invalid remote/signer configuration,
// schema initialization failures, key rotation recording failures, or unavailable required remote stores.
func NewStrictAgentAuditEngine(db *gorm.DB, bus *eventbus.Bus, opts ...EngineOption) (*AgentAuditEngine, error) {
	defaultDir := strings.TrimSpace(os.Getenv("OPS_AUDIT_ARCHIVE_DIR"))
	if defaultDir == "" {
		defaultDir = filepath.Join(os.TempDir(), "home-datacenter-audit")
	}
	var defaultPeriod time.Duration
	if rawSec := strings.TrimSpace(os.Getenv("OPS_AUDIT_CHECKPOINT_PERIOD_SEC")); rawSec != "" {
		sec, err := strconv.Atoi(rawSec)
		if err != nil || sec <= 0 {
			return nil, fmt.Errorf("%w: invalid OPS_AUDIT_CHECKPOINT_PERIOD_SEC %q", ErrAuditConfigInvalid, rawSec)
		}
		defaultPeriod = time.Duration(sec) * time.Second
	}
	e := &AgentAuditEngine{
		db:                 db,
		bus:                bus,
		archiveDir:         defaultDir,
		checkpointInterval: DefaultCheckpointInterval,
		checkpointPeriod:   defaultPeriod,
		nowFn:              func() time.Time { return time.Now().UTC() },
		degradedWindow:     make(map[string]time.Time),
		stopCh:             make(chan struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(e)
		}
	}
	if e.remoteStore == nil {
		envStore, err := ResolveRemoteAuditStoreFromEnv()
		if err != nil {
			e.initErr = err
			return nil, err
		}
		e.remoteStore = envStore
	}
	if e.externalSigner == nil && e.externalVerifier == nil {
		envSigner, envVerifier, err := ResolveExternalCheckpointKeys()
		if err != nil {
			e.initErr = err
			return nil, err
		}
		e.externalSigner = envSigner
		e.externalVerifier = envVerifier
	}
	envStrict, envStrictSet, err := resolveStrictRemoteEnv()
	if err != nil {
		e.initErr = err
		return nil, err
	}
	if !e.strictRemoteSet {
		if envStrictSet {
			e.strictRemote = envStrict
		} else if isAuditProductionRuntime() && e.remoteStore != nil {
			e.strictRemote = true
		}
	}
	if e.strictRemote {
		if e.remoteStore == nil {
			err := fmt.Errorf("%w: remote audit store must be configured when strict remote mode is enabled", ErrAuditRemoteRequired)
			e.initErr = err
			return nil, err
		}
		probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, probeErr := e.remoteStore.FetchTail(probeCtx, model.AuditPrimaryStreamID)
		cancel()
		if probeErr != nil {
			err := fmt.Errorf("%w: remote audit store unreachable at startup: %s", ErrAuditRemoteRequired, redact.SensitiveString(probeErr.Error()))
			e.initErr = err
			return nil, err
		}
	}
	if db != nil {
		if err := e.EnsureAuditSchema(); err != nil {
			e.initErr = err
			return nil, err
		}
		if _, err := e.RecordKeyRotationIfNeeded(context.Background()); err != nil {
			e.initErr = err
			return nil, err
		}
	}
	if e.checkpointPeriod > 0 {
		e.loopWG.Add(1)
		go e.runPeriodicLoop()
	}
	return e, nil
}

// NewAgentAuditEngine creates an AgentAuditEngine bound to db and eventbus.
// Legacy callers without error handling receive a non-nil engine with InitError() populated if initialization failed.
func NewAgentAuditEngine(db *gorm.DB, bus *eventbus.Bus, opts ...EngineOption) *AgentAuditEngine {
	e, err := NewStrictAgentAuditEngine(db, bus, opts...)
	if err == nil && e != nil {
		return e
	}
	// Preserve non-nil engine handle for non-strict legacy callers while refusing background loops
	// and surfacing the exact failure via InitError(), metrics, and eventbus alerts.
	fallback := &AgentAuditEngine{
		db:                 db,
		bus:                bus,
		checkpointInterval: DefaultCheckpointInterval,
		nowFn:              func() time.Time { return time.Now().UTC() },
		degradedWindow:     make(map[string]time.Time),
		stopCh:             make(chan struct{}),
		initErr:            err,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(fallback)
		}
	}
	fallback.recordFailureMetric()
	if err != nil {
		fallback.publishRemoteAlert("ops.audit.init_failed", err.Error())
	}
	return fallback
}

// InitError returns any initialization error captured during non-strict construction.
func (e *AgentAuditEngine) InitError() error {
	if e == nil {
		return ErrAuditCommitFailed
	}
	return e.initErr
}

// IsStrictRemote reports whether strict remote replication enforcement is active.
func (e *AgentAuditEngine) IsStrictRemote() bool {
	if e == nil {
		return false
	}
	return e.strictRemote
}

// Close stops background checkpoint and remote sync loops cleanly.
func (e *AgentAuditEngine) Close() {
	if e == nil {
		return
	}
	e.stopOnce.Do(func() {
		if e.stopCh != nil {
			close(e.stopCh)
		}
	})
	e.loopWG.Wait()
}

func (e *AgentAuditEngine) runPeriodicLoop() {
	defer e.loopWG.Done()
	ticker := time.NewTicker(e.checkpointPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			if e.db != nil {
				cp, cpErr := e.CreateCheckpoint(model.AuditPrimaryStreamID)
				if cpErr != nil {
					e.recordFailureMetric()
					e.publishRemoteAlert("ops.audit.checkpoint_failed", cpErr.Error())
					continue
				}
				if cp == nil && e.remoteStore != nil {
					_, _ = e.SyncRemoteStrict(context.Background(), model.AuditPrimaryStreamID)
				}
			}
		}
	}
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
		if ver >= 7 {
			for _, tbl := range []string{"agent_audit_runs", "agent_audit_events", "agent_audit_checkpoints", "agent_audit_remote_sync_states"} {
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
		&model.AgentAuditRemoteSyncState{},
		&model.StructuredOpsAuditLog{},
	); err != nil {
		return err
	}

	return e.db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_audit_events_run_seq ON agent_audit_events (run_id, seq);
		CREATE INDEX IF NOT EXISTS idx_audit_events_hh_time ON agent_audit_events (household_id, created_at_ms);
		CREATE INDEX IF NOT EXISTS idx_audit_events_comp_action ON agent_audit_events (component, action);
		CREATE INDEX IF NOT EXISTS idx_audit_events_operation_id ON agent_audit_events (operation_id);
		CREATE INDEX IF NOT EXISTS idx_audit_runs_operation_id ON agent_audit_runs (operation_id);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_audit_events_origin_stream_seq ON agent_audit_events (origin_stream_id, origin_seq) WHERE origin_stream_id <> '' AND origin_seq > 0;

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
		ProposalID:    strings.TrimSpace(in.ProposalID),
		ApprovalID:    strings.TrimSpace(in.ApprovalID),
		OperationID:   strings.TrimSpace(in.OperationID),
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
		if evIn.FencingGeneration == 0 && in.FencingGeneration > 0 {
			evIn.FencingGeneration = in.FencingGeneration
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

	auditIDExplicit := strings.TrimSpace(in.AuditID) != ""
	auditID := strings.TrimSpace(in.AuditID)
	if auditID == "" {
		auditID = fmt.Sprintf("aud-%d-%s", nowMs, generateRandomHex(5))
	}

	proposalID := strings.TrimSpace(in.ProposalID)
	if proposalID == "" && hasSess {
		proposalID = sess.ProposalID
	}
	approvalID := strings.TrimSpace(in.ApprovalID)
	if approvalID == "" && hasSess {
		approvalID = sess.ApprovalID
	}
	operationID := strings.TrimSpace(in.OperationID)
	if operationID == "" && hasSess {
		operationID = sess.OperationID
	}

	synced := true
	if in.Synced != nil {
		synced = *in.Synced
	}

	paramsInput := mergeLifecycleFields(in.Parameters, nonEmptyPair("decision_reason", CapAndRedactText(strings.TrimSpace(in.DecisionReason), 1024)))
	beforeInput := mergeLifecycleFields(in.BeforeState, nonEmptyPair("baseline_before_hash", strings.TrimSpace(in.BaselineBeforeHash)))
	afterInput := mergeLifecycleFields(in.AfterState, nonEmptyPair("baseline_after_hash", strings.TrimSpace(in.BaselineAfterHash)))
	evidenceInput := in.Evidence
	if in.RollbackResult != nil {
		evidenceInput = mergeLifecycleFields(in.Evidence, map[string]any{"rollback_result": in.RollbackResult})
	}

	paramDigest, paramRedacted := SerializeParams(paramsInput)
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
		ProposalID:         proposalID,
		ApprovalID:         approvalID,
		OperationID:        operationID,
		FencingGeneration:  in.FencingGeneration,
		BeforeState:        SerializeEvidence(beforeInput),
		AfterState:         SerializeEvidence(afterInput),
		ParametersDigest:   paramDigest,
		ParametersRedacted: paramRedacted,
		Result:             strings.TrimSpace(in.Result),
		ErrorCode:          CapAndRedactText(strings.TrimSpace(in.ErrorCode), 256),
		DurationMs:         in.DurationMs,
		Evidence:           SerializeEvidence(evidenceInput),
		CreatedAt:          now,
		CreatedAtMs:        nowMs,
		IsOffline:          in.IsOffline,
		Synced:             synced,
	}

	e.writeMu.Lock()
	writeLocked := true
	defer func() {
		if writeLocked {
			e.writeMu.Unlock()
		}
	}()

	if e.preHook != nil {
		if hErr := e.preHook(in); hErr != nil {
			return ev, e.handleCommitFailure(in, ev, hErr)
		}
	}
	if e != nil && e.initErr != nil {
		return ev, e.handleCommitFailure(in, ev, e.initErr)
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
	var deduplicated bool
	var rotationEv *model.AgentAuditEvent
	err := e.runImmediate(func(conn *sql.Conn) error {
		if auditIDExplicit {
			var existingSeq uint64
			var existingRunID, existingPrevHash, existingEntryHash, existingHMAC, existingKID string
			var existingMs int64
			existErr := conn.QueryRowContext(e.bgCtx(),
				`SELECT seq, run_id, previous_hash, entry_hash, hmac_signature, key_id, created_at_ms FROM agent_audit_events WHERE audit_id = ? LIMIT 1`,
				ev.AuditID,
			).Scan(&existingSeq, &existingRunID, &existingPrevHash, &existingEntryHash, &existingHMAC, &existingKID, &existingMs)
			if existErr == nil {
				ev.Seq = existingSeq
				ev.RunID = existingRunID
				ev.PreviousHash = existingPrevHash
				ev.EntryHash = existingEntryHash
				ev.HMACSignature = existingHMAC
				ev.KeyID = existingKID
				if existingMs > 0 {
					ev.CreatedAtMs = existingMs
					ev.CreatedAt = time.UnixMilli(existingMs).UTC()
				}
				deduplicated = true
				return nil
			} else if existErr != sql.ErrNoRows {
				return existErr
			}
		}

		var lastSeq uint64
		var lastHash, lastKID string
		row := conn.QueryRowContext(e.bgCtx(),
			`SELECT seq, entry_hash, key_id FROM agent_audit_events WHERE stream_id = ? ORDER BY seq DESC LIMIT 1`,
			ev.StreamID,
		)
		if sErr := row.Scan(&lastSeq, &lastHash, &lastKID); sErr != nil && sErr != sql.ErrNoRows {
			return sErr
		}
		if lastHash == "" {
			lastHash = model.AuditGenesisHash
		}

		// Record an immutable AUDIT_KEY_ROTATED lifecycle event whenever the active HMAC key_id transitions.
		if ev.EventType != model.AuditEventAuditKeyRotated && lastKID != "" && lastKID != kid {
			var ringKIDs []string
			for k := range ring {
				ringKIDs = append(ringKIDs, k)
			}
			sort.Strings(ringKIDs)
			pDig, pRed := SerializeParams(map[string]interface{}{"configured_key_ids": ringKIDs})
			rotNow := e.nowUTC()
			rotMs := rotNow.UnixMilli()
			rotRunID := fmt.Sprintf("run-%d-%s", rotMs, generateRandomHex(4))
			rotCorrID := fmt.Sprintf("corr-%d-%s", rotMs, generateRandomHex(4))
			rotEv := &model.AgentAuditEvent{
				StreamID:           ev.StreamID,
				Seq:                lastSeq + 1,
				AuditID:            fmt.Sprintf("aud-%d-%s", rotMs, generateRandomHex(4)),
				RunID:              rotRunID,
				CorrelationID:      rotCorrID,
				HouseholdID:        1,
				TriggerSource:      "audit_keyring",
				ActorType:          "system",
				ActorID:            "audit-engine",
				EventType:          model.AuditEventAuditKeyRotated,
				Action:             "audit.key_rotated",
				Target:             ev.StreamID,
				Component:          "audit-engine",
				Decision:           model.AuditDecisionAllow,
				RiskLevel:          model.AuditRiskHigh,
				PolicyVersion:      PolicyVersion,
				BeforeState:        SerializeEvidence(map[string]string{"active_key_id": lastKID}),
				AfterState:         SerializeEvidence(map[string]string{"active_key_id": kid}),
				ParametersDigest:   pDig,
				ParametersRedacted: pRed,
				Result:             "SUCCESS",
				CreatedAt:          rotNow,
				CreatedAtMs:        rotMs,
				Synced:             true,
				KeyID:              kid,
				PreviousHash:       lastHash,
			}
			rotHash, rErr := ComputeEventEntryHash(rotEv)
			if rErr != nil {
				return rErr
			}
			rotEv.EntryHash = rotHash
			rotEv.HMACSignature = ComputeEventHMAC(kid, secret, rotEv.StreamID, rotEv.Seq, rotEv.EntryHash, rotEv.CreatedAtMs)
			if _, execErr := conn.ExecContext(e.bgCtx(), `
				INSERT INTO agent_audit_events (
					stream_id, seq, audit_id, run_id, parent_event_id, correlation_id, household_id,
					trigger_source, actor_type, actor_id, event_type, action, target, component,
					decision, risk_level, policy_version, proposal_id, approval_id, operation_id,
					fencing_generation, before_state, after_state, parameters_digest, parameters_redacted,
					result, error_code, duration_ms, evidence, created_at, created_at_ms, is_offline, synced,
					origin_stream_id, origin_seq, origin_entry_hash, key_id, previous_hash, entry_hash, hmac_signature
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				rotEv.StreamID, rotEv.Seq, rotEv.AuditID, rotEv.RunID, rotEv.ParentEventID, rotEv.CorrelationID, rotEv.HouseholdID,
				rotEv.TriggerSource, rotEv.ActorType, rotEv.ActorID, rotEv.EventType, rotEv.Action, rotEv.Target, rotEv.Component,
				rotEv.Decision, rotEv.RiskLevel, rotEv.PolicyVersion, rotEv.ProposalID, rotEv.ApprovalID, rotEv.OperationID,
				rotEv.FencingGeneration, rotEv.BeforeState, rotEv.AfterState, rotEv.ParametersDigest, rotEv.ParametersRedacted,
				rotEv.Result, rotEv.ErrorCode, rotEv.DurationMs, rotEv.Evidence, rotEv.CreatedAt.UTC(), rotEv.CreatedAtMs, 0, 1,
				"", uint64(0), "", rotEv.KeyID, rotEv.PreviousHash, rotEv.EntryHash, rotEv.HMACSignature,
			); execErr != nil {
				return execErr
			}
			if upErr := upsertRunProgressConn(conn, rotEv, model.AuditRunStatusCompleted, fmt.Sprintf("Audit HMAC key rotated from %s to %s", lastKID, kid)); upErr != nil {
				return upErr
			}
			lastSeq = rotEv.Seq
			lastHash = rotEv.EntryHash
			rotationEv = rotEv
		}

		ev.Seq = lastSeq + 1
		ev.PreviousHash = lastHash
		ev.KeyID = kid
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
				origin_stream_id, origin_seq, origin_entry_hash, key_id, previous_hash, entry_hash, hmac_signature
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ev.StreamID, ev.Seq, ev.AuditID, ev.RunID, ev.ParentEventID, ev.CorrelationID, ev.HouseholdID,
			ev.TriggerSource, ev.ActorType, ev.ActorID, ev.EventType, ev.Action, ev.Target, ev.Component,
			ev.Decision, ev.RiskLevel, ev.PolicyVersion, ev.ProposalID, ev.ApprovalID, ev.OperationID,
			ev.FencingGeneration, ev.BeforeState, ev.AfterState, ev.ParametersDigest, ev.ParametersRedacted,
			ev.Result, ev.ErrorCode, ev.DurationMs, ev.Evidence, ev.CreatedAt.UTC(), ev.CreatedAtMs, boolInt(ev.IsOffline), boolInt(ev.Synced),
			ev.OriginStreamID, ev.OriginSeq, ev.OriginEntryHash, ev.KeyID, ev.PreviousHash, ev.EntryHash, ev.HMACSignature,
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
	if deduplicated {
		return ev, nil
	}

	if e.postHook != nil {
		if hErr := e.postHook(in); hErr != nil {
			return ev, e.handleCommitFailure(in, ev, hErr)
		}
	}

	if rotationEv != nil {
		if err := e.appendArchiveEvent(rotationEv); err != nil {
			e.recordFailureMetric()
		}
	}
	if err := e.appendArchiveEvent(ev); err != nil {
		e.recordFailureMetric()
	}

	if triggerCheckpoint {
		if _, cpErr := e.createCheckpointLocked(ev.StreamID); cpErr != nil {
			e.recordFailureMetric()
			e.publishRemoteAlert("ops.audit.checkpoint_failed", cpErr.Error())
		}
	}

	// Release writeMu before network/remote I/O so local writers are never blocked on remote latency.
	writeLocked = false
	e.writeMu.Unlock()

	var syncErr error
	if e.remoteStore != nil {
		if _, sErr := e.SyncRemoteStrict(e.bgCtx(), ev.StreamID); sErr != nil && e.strictRemote {
			syncErr = sErr
		}
	} else if e.strictRemote {
		e.recordFailureMetric()
		syncErr = fmt.Errorf("%w: remote audit store unavailable in strict mode", ErrAuditRemoteRequired)
		e.publishRemoteAlert("ops.audit.remote_sync_failed", syncErr.Error())
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

	if syncErr != nil {
		return ev, syncErr
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
	e.loadDegradedWindowLocked()
	if last, ok := e.degradedWindow[component]; ok && now.Sub(last) < BoundedEmergencyWindow {
		return false
	}
	e.degradedWindow[component] = now
	e.saveDegradedWindowLocked()
	return true
}

func (e *AgentAuditEngine) degradedWindowFile() string {
	if strings.TrimSpace(e.archiveDir) == "" {
		return ""
	}
	return filepath.Join(e.archiveDir, "degraded-window.json")
}

func (e *AgentAuditEngine) loadDegradedWindowLocked() {
	path := e.degradedWindowFile()
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return
	}
	var diskMap map[string]int64
	if err := json.Unmarshal(raw, &diskMap); err != nil {
		return
	}
	for k, ms := range diskMap {
		if ms <= 0 {
			continue
		}
		t := time.UnixMilli(ms).UTC()
		if cur, ok := e.degradedWindow[k]; !ok || t.After(cur) {
			e.degradedWindow[k] = t
		}
	}
}

func (e *AgentAuditEngine) saveDegradedWindowLocked() {
	path := e.degradedWindowFile()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	out := make(map[string]int64, len(e.degradedWindow))
	for k, t := range e.degradedWindow {
		out[k] = t.UTC().UnixMilli()
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err == nil {
		_ = os.Rename(tmp, path)
	}
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
			upperRes := strings.ToUpper(strings.TrimSpace(ev.Result))
			if upperRes == "ROLLBACK_FAILED" || upperRes == "FAILED" {
				runStatus = model.AuditRunStatusRollbackFailed
			} else {
				runStatus = model.AuditRunStatusRolledBack
			}
			endedAtMs = ev.CreatedAtMs
		case model.AuditEventRollbackFailed:
			runStatus = model.AuditRunStatusRollbackFailed
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
	rollbackResultStr := resolveLegacyRollbackResult(ev, in)
	_, err := conn.ExecContext(context.Background(), `
		INSERT OR IGNORE INTO structured_ops_audit_logs (
			event_uuid, household_id, request_id, actor_type, actor_id, actor_name,
			client_ip, target_type, target_id, action, proposal_id, approver,
			execution_result, rollback_result, is_offline_action, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.AuditID, ev.HouseholdID, ev.CorrelationID, ev.ActorType, 0, ev.ActorID,
		in.LegacyClientIP, targetType, defaultStr(ev.Target, ev.Component), ev.Action, ev.ProposalID, in.LegacyApprover,
		execPayload, rollbackResultStr, boolInt(ev.IsOffline), ev.CreatedAt.UTC(),
	)
	return err
}

func resolveLegacyRollbackResult(ev *model.AgentAuditEvent, in EmitEventInput) string {
	if in.RollbackResult != nil {
		return SerializeEvidence(in.RollbackResult)
	}
	upperRes := strings.ToUpper(strings.TrimSpace(ev.Result))
	isRollback := ev.EventType == model.AuditEventRollbackStarted ||
		ev.EventType == model.AuditEventRollbackCompleted ||
		ev.EventType == model.AuditEventRollbackFailed ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(ev.Action)), "rollback.") ||
		strings.Contains(upperRes, "ROLLBACK") ||
		strings.Contains(upperRes, "ROLLED_BACK")
	if !isRollback {
		return ""
	}
	if ev.AfterState != "" {
		return ev.AfterState
	}
	if ev.Evidence != "" {
		return ev.Evidence
	}
	if upperRes != "" {
		return CapAndRedactText(strings.TrimSpace(ev.Result), 512)
	}
	return ev.ErrorCode
}

func nonEmptyPair(key, val string) map[string]any {
	if val == "" {
		return nil
	}
	return map[string]any{key: val}
}

func mergeLifecycleFields(base any, extras map[string]any) any {
	if len(extras) == 0 {
		return base
	}
	if base == nil {
		return extras
	}
	merged := make(map[string]any, len(extras)+4)
	if directMap, ok := base.(map[string]any); ok {
		for k, v := range directMap {
			merged[k] = v
		}
		for k, v := range extras {
			if _, exists := merged[k]; !exists {
				merged[k] = v
			}
		}
		return merged
	}
	raw, err := json.Marshal(base)
	if err == nil && json.Unmarshal(raw, &merged) == nil && len(merged) > 0 {
		for k, v := range extras {
			if _, exists := merged[k]; !exists {
				merged[k] = v
			}
		}
		return merged
	}
	out := map[string]any{"payload": base}
	for k, v := range extras {
		out[k] = v
	}
	return out
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
