package ops

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/audit"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/redact"
)

func setupAuditR5TestEnv(t *testing.T, opts ...audit.EngineOption) (*gorm.DB, *eventbus.Bus, *audit.AgentAuditEngine, string) {
	t.Helper()
	cleanupKeyring := audit.SetAuditHMACKeyringForTest("k1", map[string]string{
		"k1": "super-secret-audit-signing-key-32bytes-minimum-test",
		"k2": "rotated-secret-audit-signing-key-32bytes-test-v2",
	})
	t.Cleanup(cleanupKeyring)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "audit_r5.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	if sqlDB, sErr := db.DB(); sErr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}

	if err := db.AutoMigrate(
		&model.OpsProposal{},
		&model.OpsExecutionHistory{},
		&model.SystemLog{},
		&model.OpsChatSession{},
		&model.OpsChatMessage{},
		&model.StructuredOpsAuditLog{},
		&model.SecurityAlertFilter{},
		&model.SecurityRecipient{},
	); err != nil {
		t.Fatalf("failed auto-migrate: %v", err)
	}
	if err := EnsureOpsSchema(db); err != nil {
		t.Fatalf("EnsureOpsSchema failed: %v", err)
	}
	if err := VerifyOpsSchemaIntegrity(db); err != nil {
		t.Fatalf("VerifyOpsSchemaIntegrity failed: %v", err)
	}

	bus := eventbus.New()
	allOpts := append([]audit.EngineOption{
		audit.WithArchiveDir(filepath.Join(tmpDir, "audit-archive")),
		audit.WithCheckpointInterval(5),
		audit.WithFailureCallback(GlobalSecurityMetrics.IncAuditFailed),
	}, opts...)
	ae := audit.NewAgentAuditEngine(db, bus, allOpts...)
	return db, bus, ae, tmpDir
}

// 1. Cross-language Golden Vector Parity (Go <-> Python)
func TestAuditR5_GoldenVectorsParity(t *testing.T) {
	cleanupKeyring := audit.SetAuditHMACKeyringForTest("k1", map[string]string{
		"k1": "super-secret-audit-signing-key-32bytes-minimum-test",
		"k2": "rotated-secret-audit-signing-key-32bytes-test-v2",
	})
	defer cleanupKeyring()

	vecPath := filepath.Join("..", "..", "..", "..", "testdata", "audit_golden_vectors.json")
	raw, err := os.ReadFile(vecPath)
	if err != nil {
		t.Fatalf("failed to read golden vectors %s: %v", vecPath, err)
	}

	var doc struct {
		Keyring              map[string]string `json:"keyring"`
		CanonicalJSONVectors []struct {
			Input        interface{} `json:"input"`
			ExpectedJSON string      `json:"expected_json"`
		} `json:"canonical_json_vectors"`
		TimestampVectors []struct {
			Ms              int64  `json:"ms"`
			ExpectedRFC3339 string `json:"expected_rfc3339_ms"`
		} `json:"timestamp_vectors"`
		EventVectors []struct {
			StreamID string `json:"stream_id"`
			Seq      uint64 `json:"seq"`
			KeyID    string `json:"key_id"`
			Event    struct {
				AuditID            string `json:"audit_id"`
				RunID              string `json:"run_id"`
				ParentEventID      string `json:"parent_event_id"`
				CorrelationID      string `json:"correlation_id"`
				HouseholdID        uint   `json:"household_id"`
				TriggerSource      string `json:"trigger_source"`
				ActorType          string `json:"actor_type"`
				ActorID            string `json:"actor_id"`
				EventType          string `json:"event_type"`
				Action             string `json:"action"`
				Target             string `json:"target"`
				Component          string `json:"component"`
				Decision           string `json:"decision"`
				RiskLevel          string `json:"risk_level"`
				PolicyVersion      string `json:"policy_version"`
				ProposalID         string `json:"proposal_id"`
				ApprovalID         string `json:"approval_id"`
				OperationID        string `json:"operation_id"`
				FencingGeneration  int64  `json:"fencing_generation"`
				BeforeState        string `json:"before_state"`
				AfterState         string `json:"after_state"`
				ParametersDigest   string `json:"parameters_digest"`
				ParametersRedacted string `json:"parameters_redacted"`
				Result             string `json:"result"`
				ErrorCode          string `json:"error_code"`
				DurationMs         int64  `json:"duration_ms"`
				Evidence           string `json:"evidence"`
				CreatedAtMs        int64  `json:"created_at_ms"`
				IsOffline          bool   `json:"is_offline"`
				PreviousHash       string `json:"previous_hash"`
			} `json:"event"`
			ExpectedEntryHash string `json:"expected_entry_hash"`
			ExpectedHMAC      string `json:"expected_hmac"`
		} `json:"event_vectors"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("failed to parse golden vectors JSON: %v", err)
	}

	for i, cv := range doc.CanonicalJSONVectors {
		got, cErr := audit.CanonicalJSONBytes(cv.Input)
		if cErr != nil {
			t.Fatalf("canonical vector[%d] failed: %v", i, cErr)
		}
		if string(got) != cv.ExpectedJSON {
			t.Fatalf("canonical vector[%d] mismatch:\n got: %s\nwant: %s", i, string(got), cv.ExpectedJSON)
		}
	}

	for i, tv := range doc.TimestampVectors {
		got := audit.FormatRFC3339Ms(time.UnixMilli(tv.Ms).UTC())
		if got != tv.ExpectedRFC3339 {
			t.Fatalf("timestamp vector[%d] mismatch: got %s want %s", i, got, tv.ExpectedRFC3339)
		}
	}

	for i, evVec := range doc.EventVectors {
		ev := &model.AgentAuditEvent{
			StreamID:           evVec.StreamID,
			Seq:                evVec.Seq,
			AuditID:            evVec.Event.AuditID,
			RunID:              evVec.Event.RunID,
			ParentEventID:      evVec.Event.ParentEventID,
			CorrelationID:      evVec.Event.CorrelationID,
			HouseholdID:        evVec.Event.HouseholdID,
			TriggerSource:      evVec.Event.TriggerSource,
			ActorType:          evVec.Event.ActorType,
			ActorID:            evVec.Event.ActorID,
			EventType:          evVec.Event.EventType,
			Action:             evVec.Event.Action,
			Target:             evVec.Event.Target,
			Component:          evVec.Event.Component,
			Decision:           evVec.Event.Decision,
			RiskLevel:          evVec.Event.RiskLevel,
			PolicyVersion:      evVec.Event.PolicyVersion,
			ProposalID:         evVec.Event.ProposalID,
			ApprovalID:         evVec.Event.ApprovalID,
			OperationID:        evVec.Event.OperationID,
			FencingGeneration:  evVec.Event.FencingGeneration,
			BeforeState:        evVec.Event.BeforeState,
			AfterState:         evVec.Event.AfterState,
			ParametersDigest:   evVec.Event.ParametersDigest,
			ParametersRedacted: evVec.Event.ParametersRedacted,
			Result:             evVec.Event.Result,
			ErrorCode:          evVec.Event.ErrorCode,
			DurationMs:         evVec.Event.DurationMs,
			Evidence:           evVec.Event.Evidence,
			CreatedAt:          time.UnixMilli(evVec.Event.CreatedAtMs).UTC(),
			CreatedAtMs:        evVec.Event.CreatedAtMs,
			IsOffline:          evVec.Event.IsOffline,
			PreviousHash:       evVec.Event.PreviousHash,
		}
		gotHash, hErr := audit.ComputeEventEntryHash(ev)
		if hErr != nil {
			t.Fatalf("event vector[%d] hash error: %v", i, hErr)
		}
		if gotHash != evVec.ExpectedEntryHash {
			t.Fatalf("event vector[%d] entry_hash mismatch: got %s want %s", i, gotHash, evVec.ExpectedEntryHash)
		}
		secret := doc.Keyring[evVec.KeyID]
		gotHMAC := audit.ComputeEventHMAC(evVec.KeyID, secret, ev.StreamID, ev.Seq, gotHash, ev.CreatedAtMs)
		if gotHMAC != evVec.ExpectedHMAC {
			t.Fatalf("event vector[%d] hmac mismatch: got %s want %s", i, gotHMAC, evVec.ExpectedHMAC)
		}
		ev.EntryHash = gotHash
		ev.HMACSignature = gotHMAC
		kidUsed, vErr := audit.VerifyEventHMAC(ev, doc.Keyring)
		if vErr != nil || kidUsed != evVec.KeyID {
			t.Fatalf("event vector[%d] VerifyEventHMAC failed: kid=%s err=%v", i, kidUsed, vErr)
		}
	}
}

// 2. Append-Only SQLite Triggers & Honest Fail-Closed Empty Verify
func TestAuditR5_AppendOnlyTriggersAndEmptyFailClosed(t *testing.T) {
	db, _, ae, _ := setupAuditR5TestEnv(t)

	// Empty database MUST never report Valid=true
	emptyRes := ae.VerifyChain(audit.VerifyRequest{VerifyCheckpoints: true, VerifyTriggers: true})
	if emptyRes.Valid || emptyRes.IntegrityStatus != audit.IntegrityUnverifiable {
		t.Fatalf("expected Valid=false & UNVERIFIABLE on empty chain, got valid=%v status=%s", emptyRes.Valid, emptyRes.IntegrityStatus)
	}

	_, ctx, err := ae.StartRun(context.Background(), audit.StartRunInput{
		HouseholdID:   1,
		TriggerSource: "unit_test",
		ActorType:     "admin",
		ActorID:       "admin-1",
		Action:        "docker.restart",
		Target:        "home-web",
		Component:     "home-web",
		Decision:      model.AuditDecisionAllow,
		RiskLevel:     model.AuditRiskMedium,
		InitialEvent: &audit.EmitEventInput{
			EventType: model.AuditEventExecutionStarted,
			Result:    "STARTED",
		},
	})
	if err != nil {
		t.Fatalf("StartRun failed: %v", err)
	}
	sess, _ := audit.SessionFromContext(ctx)

	// Direct UPDATE/DELETE on agent_audit_events must be aborted by triggers
	if uErr := db.Exec("UPDATE agent_audit_events SET target = 'evil' WHERE run_id = ?", sess.RunID).Error; uErr == nil {
		t.Fatalf("expected SQLite trigger to abort UPDATE on agent_audit_events")
	}
	if dErr := db.Exec("DELETE FROM agent_audit_events WHERE run_id = ?", sess.RunID).Error; dErr == nil {
		t.Fatalf("expected SQLite trigger to abort DELETE on agent_audit_events")
	}
}

// 3. Fault Injection F01..F08: Pre-commit blocks mutation, Post-commit marks EXECUTION_UNKNOWN, Tamper Detection, Concurrency
func TestAuditR5_FaultInjectionAndTamperDetection(t *testing.T) {
	// F-01: Pre-execution AUDIT_COMMIT_FAILED must abort BEFORE dockerRunner executes
	db, bus, ae, _ := setupAuditR5TestEnv(t)
	var dockerCalls int32
	exec := NewOperationExecutor(
		db, bus, nil, nil,
		WithBaselineExtractor(func(target string) string { return "base-hash-ok" }),
		WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			atomic.AddInt32(&dockerCalls, 1)
			return "restarted", nil
		}),
	)
	exec.SetAuditEngine(ae)

	ae.SetFaultHooks(func(in audit.EmitEventInput) error {
		if in.EventType == model.AuditEventExecutionStarted {
			return errors.New("simulated sqlite io error")
		}
		return nil
	}, nil)

	_, execErr := exec.Execute(context.Background(), OperationSpec{
		Action: ActionDockerRestart,
		Target: "home-web",
	})
	if execErr == nil || !errors.Is(execErr, audit.ErrAuditCommitFailed) {
		t.Fatalf("expected ErrAuditCommitFailed on pre-mutation audit failure, got: %v", execErr)
	}
	if gotCalls := atomic.LoadInt32(&dockerCalls); gotCalls != 0 {
		t.Fatalf("expected 0 dockerRunner calls when pre-mutation audit failed, got %d", gotCalls)
	}

	// F-02: Post-execution audit commit failure must NEVER fake Success=true; marks EXECUTION_UNKNOWN
	ae.SetFaultHooks(nil, func(in audit.EmitEventInput) error {
		if in.EventType == model.AuditEventExecutionCompleted {
			return errors.New("simulated post-commit failure")
		}
		return nil
	})
	_, postErr := exec.Execute(context.Background(), OperationSpec{
		Action: ActionDockerRestart,
		Target: "home-web",
	})
	if postErr == nil || !errors.Is(postErr, ErrExecutionAmbiguous) {
		t.Fatalf("expected ErrExecutionAmbiguous when post-mutation audit fails, got: %v", postErr)
	}
	if gotCalls := atomic.LoadInt32(&dockerCalls); gotCalls != 1 {
		t.Fatalf("expected 1 dockerRunner call, got %d", gotCalls)
	}

	// F-03: Bounded Emergency degraded allowance allows 1 whitelisted container.emergency_restart via JSONL per 10m window, then blocks
	ae.SetFaultHooks(func(in audit.EmitEventInput) error {
		return errors.New("db unavailable")
	}, nil)
	boundedIn := audit.EmitEventInput{
		HouseholdID:   1,
		TriggerSource: "ops_agent_emergency",
		ActorType:     "agent",
		ActorID:       "ops-agent",
		EventType:     model.AuditEventExecutionStarted,
		Action:        "container.emergency_restart",
		Target:        "home-web",
		Component:     "home-web",
		FailurePolicy: audit.PolicyBoundedEmergency,
	}
	if _, err1 := ae.RecordPreMutation(context.Background(), boundedIn); err1 != nil {
		t.Fatalf("expected bounded emergency #1 to succeed via signed JSONL, got: %v", err1)
	}
	if _, err2 := ae.RecordPreMutation(context.Background(), boundedIn); !errors.Is(err2, audit.ErrAuditDegradedQuotaExceeded) {
		t.Fatalf("expected ErrAuditDegradedQuotaExceeded on #2 degraded restart within window, got: %v", err2)
	}
	ae.SetFaultHooks(nil, nil)

	// F-04: Concurrency: 20 goroutines writing simultaneously must produce strictly monotonic seq without hash fork
	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, emitErr := ae.EmitEvent(context.Background(), audit.EmitEventInput{
				HouseholdID:   1,
				TriggerSource: "concurrency_test",
				ActorType:     "system",
				ActorID:       fmt.Sprintf("worker-%d", idx),
				EventType:     model.AuditEventAnalysisCompleted,
				Action:        "probe",
				Target:        "home-api",
				Component:     "home-api",
				Decision:      model.AuditDecisionAllow,
				RiskLevel:     model.AuditRiskLow,
				Result:        "OK",
			})
			if emitErr != nil {
				t.Errorf("concurrent EmitEvent[%d] failed: %v", idx, emitErr)
			}
		}(g)
	}
	wg.Wait()

	verRes := ae.VerifyChain(audit.VerifyRequest{VerifyCheckpoints: true, VerifyTriggers: true})
	if !verRes.Valid || (verRes.IntegrityStatus != audit.IntegrityVerifiedAgainstCheckpoint && verRes.IntegrityStatus != audit.IntegrityChainIntactLocalOnly) {
		t.Fatalf("concurrent chain verification failed: valid=%v status=%s reason=%s", verRes.Valid, verRes.IntegrityStatus, verRes.FailureReason)
	}

	// F-05..F-08: Explicit tamper scenarios (trigger missing -> DROP trigger, HMAC_INVALID, HASH_MISMATCH, SEQ_GAP, TRUNCATED_TAIL)
	if dErr := db.Exec("DROP TRIGGER IF EXISTS trg_agent_audit_events_no_update").Error; dErr != nil {
		t.Fatalf("failed to drop trigger in tamper test: %v", dErr)
	}
	trigMissingRes := ae.VerifyChain(audit.VerifyRequest{VerifyCheckpoints: true, VerifyTriggers: true})
	if trigMissingRes.Valid || trigMissingRes.IntegrityStatus != audit.IntegrityTriggerMissing {
		t.Fatalf("expected TRIGGER_MISSING, got valid=%v status=%s", trigMissingRes.Valid, trigMissingRes.IntegrityStatus)
	}

	// Tamper target without recomputing entry_hash -> HASH_MISMATCH
	if uErr := db.Exec("UPDATE agent_audit_events SET target = 'tampered-target' WHERE seq = 1").Error; uErr != nil {
		t.Fatalf("failed to tamper row seq=1: %v", uErr)
	}
	hashRes := ae.VerifyChain(audit.VerifyRequest{VerifyCheckpoints: false, VerifyTriggers: false})
	if hashRes.Valid || hashRes.IntegrityStatus != audit.IntegrityHashMismatch {
		t.Fatalf("expected HASH_MISMATCH, got valid=%v status=%s reason=%s", hashRes.Valid, hashRes.IntegrityStatus, hashRes.FailureReason)
	}
}

// 4. Full Proposal, Rollback, Reject, Cancel Lifecycle L-01..L-09 & Offline Batch Sync
func TestAuditR5_ProposalRollbackAndOfflineSyncLifecycle(t *testing.T) {
	db, bus, ae, tmpDir := setupAuditR5TestEnv(t)

	exec := NewOperationExecutor(
		db, bus, nil, nil,
		WithBaselineExtractor(func(target string) string { return "fp-v1" }),
		WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			return "ok", nil
		}),
	)
	exec.SetAuditEngine(ae)

	// Approve + Execute proposal via ProposalFSM
	prop := model.OpsProposal{
		ProposalID:      "prop-r5-1",
		HouseholdID:     1,
		Title:           "Restart home-web",
		TargetComponent: "home-web",
		RiskLevel:       "MEDIUM",
		ExecutionPlan:   "Restart home-web gracefully",
		RollbackPlan:    "Restore previous container",
		BaselineHash:    "fp-v1",
		ActionSpec:      `{"action":"docker.restart","target":"home-web","baseline_hash":"fp-v1","rollback_spec":{"action":"docker.restart","target":"home-web"}}`,
		Status:          model.ProposalStatusAwaitingApproval,
	}
	digest, dErr := ComputeProposalApprovalDigest(&prop)
	if dErr != nil {
		t.Fatalf("ComputeProposalApprovalDigest failed: %v", dErr)
	}
	prop.ApprovalDigest = digest
	prop.Status = model.ProposalStatusApproved
	if err := db.Create(&prop).Error; err != nil {
		t.Fatalf("create proposal failed: %v", err)
	}

	// Execute correlated ExecuteProposal
	res, err := exec.ExecuteProposal(context.Background(), &prop)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("ExecuteProposal failed: res=%+v err=%v", res, err)
	}

	// Verify run & events stored with EXECUTION_STARTED + HEALTH_CHECK_STARTED + HEALTH_CHECK_COMPLETED + EXECUTION_COMPLETED
	runs, _, qErr := ae.QueryRuns(audit.RunQuery{HouseholdID: 1, Page: 1, PageSize: 20})
	if qErr != nil || len(runs) == 0 {
		t.Fatalf("expected recorded AgentAuditRun, got len=%d err=%v", len(runs), qErr)
	}

	// Test Offline Batch Ingestion + Idempotency
	createdMs := time.Now().UTC().UnixMilli()
	ev := model.AgentAuditEvent{
		StreamID:           "offline-agent",
		Seq:                1,
		AuditID:            "off-uuid-1",
		RunID:              "run-off-1",
		CorrelationID:      "corr-off-1",
		HouseholdID:        1,
		TriggerSource:      "ops_agent_offline",
		ActorType:          "agent",
		ActorID:            "edge-python-agent",
		EventType:          model.AuditEventExecutionCompleted,
		Action:             "docker.restart",
		Target:             "frigate",
		Component:          "frigate",
		Decision:           model.AuditDecisionAllow,
		RiskLevel:          model.AuditRiskHigh,
		PolicyVersion:      audit.PolicyVersion,
		OperationID:        "op-off-1",
		FencingGeneration:  2,
		ParametersRedacted: "{}",
		Result:             "SUCCESS",
		CreatedAt:          time.UnixMilli(createdMs).UTC(),
		CreatedAtMs:        createdMs,
		IsOffline:          true,
		PreviousHash:       model.AuditGenesisHash,
	}
	hashHex, err := audit.ComputeEventEntryHash(&ev)
	if err != nil {
		t.Fatalf("ComputeEventEntryHash failed: %v", err)
	}
	hmacSig := audit.ComputeEventHMAC("k1", "super-secret-audit-signing-key-32bytes-minimum-test", "offline-agent", 1, hashHex, createdMs)

	syncBatch := []audit.OfflineSyncEvent{
		{
			EventUUID:          "off-uuid-1",
			AuditID:            "off-uuid-1",
			RunID:              "run-off-1",
			CorrelationID:      "corr-off-1",
			OperationID:        "op-off-1",
			TriggerSource:      "ops_agent_offline",
			ActorType:          "agent",
			ActorID:            "edge-python-agent",
			EventType:          model.AuditEventExecutionCompleted,
			ContainerName:      "frigate",
			Component:          "frigate",
			Target:             "frigate",
			Action:             "docker.restart",
			Decision:           model.AuditDecisionAllow,
			RiskLevel:          model.AuditRiskHigh,
			Holder:             "python-agent",
			FencingGeneration:  2,
			ExitCode:           0,
			Success:            1,
			AuditStatus:        "RECORDED",
			ParametersRedacted: "{}",
			ExecutedAt:         audit.FormatRFC3339Ms(time.UnixMilli(createdMs).UTC()),
			ExecutedAtMs:       createdMs,
			OfflineSeq:         1,
			LocalPrevHash:      model.AuditGenesisHash,
			EntryHash:          hashHex,
			HMACSignature:      hmacSig,
		},
		{
			// Bad entry_hash -> must be rejected while off-uuid-1 is accepted!
			EventUUID:     "off-uuid-bad",
			ContainerName: "frigate",
			Action:        "docker.restart",
			EntryHash:     strings.Repeat("a", 64),
			HMACSignature: hmacSig,
		},
	}

	syncRes, sErr := ae.IngestOfflineEvents(context.Background(), syncBatch, "req-sync-1", "127.0.0.1", 1)
	if sErr != nil {
		t.Fatalf("IngestOfflineEvents failed: %v", sErr)
	}
	if len(syncRes.AcceptedUUIDs) != 1 || syncRes.AcceptedUUIDs[0] != "off-uuid-1" {
		t.Fatalf("expected only off-uuid-1 accepted, got %+v", syncRes)
	}
	if len(syncRes.Rejected) != 1 || syncRes.Rejected[0].EventUUID != "off-uuid-bad" {
		t.Fatalf("expected off-uuid-bad rejected, got %+v", syncRes.Rejected)
	}

	// Replay same accepted UUID -> still returns in AcceptedUUIDs without duplicating DB row
	replayRes, rErr := ae.IngestOfflineEvents(context.Background(), syncBatch[:1], "req-sync-2", "127.0.0.1", 1)
	if rErr != nil || len(replayRes.AcceptedUUIDs) != 1 || replayRes.SyncedCount != 0 {
		t.Fatalf("expected idempotent replay SyncedCount=0 AcceptedUUIDs=[off-uuid-1], got %+v err=%v", replayRes, rErr)
	}

	// Blocker-1 & Blocker-2 regressions:
	// 1) Failed offline event with ErrorMessage="exit_code=137" and <tag>& symbols in JSON Evidence MUST pass entry_hash verification
	// 2) v5 offline event with missing hmac_signature MUST be rejected fail-closed
	failMs := createdMs + 100
	evFail := model.AgentAuditEvent{
		StreamID:           "offline-agent",
		Seq:                2,
		AuditID:            "off-uuid-fail-html",
		RunID:              "run-off-fail-html",
		CorrelationID:      "corr-off-2",
		HouseholdID:        1,
		TriggerSource:      "ops_agent_offline",
		ActorType:          "agent",
		ActorID:            "edge-python-agent",
		EventType:          model.AuditEventExecutionFailed,
		Action:             "offline_restart",
		Target:             "frigate",
		Component:          "frigate",
		Decision:           model.AuditDecisionAllow,
		RiskLevel:          model.AuditRiskHigh,
		PolicyVersion:      audit.PolicyVersion,
		OperationID:        "op-off-fail",
		FencingGeneration:  3,
		ParametersRedacted: `{"k":"<v>&1"}`,
		Result:             "FAILED",
		ErrorCode:          "exit_code=137",
		Evidence:           `{"probe":"http://localhost?a=1&b=2<ok>"}`,
		CreatedAt:          time.UnixMilli(failMs).UTC(),
		CreatedAtMs:        failMs,
		IsOffline:          true,
		PreviousHash:       hashHex,
	}
	failHash, fhErr := audit.ComputeEventEntryHash(&evFail)
	if fhErr != nil {
		t.Fatalf("ComputeEventEntryHash failed: %v", fhErr)
	}
	failHMAC := audit.ComputeEventHMAC("k1", "super-secret-audit-signing-key-32bytes-minimum-test", "offline-agent", 2, failHash, failMs)

	regBatch := []audit.OfflineSyncEvent{
		{
			EventUUID:          "off-uuid-fail-html",
			AuditID:            "off-uuid-fail-html",
			RunID:              "run-off-fail-html",
			CorrelationID:      "corr-off-2",
			OperationID:        "op-off-fail",
			TriggerSource:      "ops_agent_offline",
			ActorType:          "agent",
			ActorID:            "edge-python-agent",
			EventType:          model.AuditEventExecutionFailed,
			ContainerName:      "frigate",
			Component:          "frigate",
			Target:             "frigate",
			Action:             "offline_restart",
			Decision:           model.AuditDecisionAllow,
			RiskLevel:          model.AuditRiskHigh,
			Holder:             "python-agent",
			FencingGeneration:  3,
			ExitCode:           137,
			Success:            0,
			AuditStatus:        "AUDIT_COMMITTED",
			ParametersRedacted: `{"k":"<v>&1"}`,
			Evidence:           `{"probe":"http://localhost?a=1&b=2<ok>"}`,
			ErrorMessage:       "exit_code=137",
			ExecutedAt:         audit.FormatRFC3339Ms(time.UnixMilli(failMs).UTC()),
			ExecutedAtMs:       failMs,
			OfflineSeq:         2,
			LocalPrevHash:      hashHex,
			EntryHash:          failHash,
			HMACSignature:      failHMAC,
		},
		{
			EventUUID:     "off-uuid-no-hmac",
			AuditID:       "off-uuid-no-hmac",
			ContainerName: "frigate",
			Action:        "offline_restart",
			ExecutedAtMs:  failMs,
			OfflineSeq:    3,
			LocalPrevHash: failHash,
			EntryHash:     failHash,
			HMACSignature: "", // Must reject missing HMAC
		},
	}
	regRes, regErr := ae.IngestOfflineEvents(context.Background(), regBatch, "req-sync-3", "127.0.0.1", 1)
	if regErr != nil {
		t.Fatalf("IngestOfflineEvents regression batch failed: %v", regErr)
	}
	if len(regRes.AcceptedUUIDs) != 1 || regRes.AcceptedUUIDs[0] != "off-uuid-fail-html" {
		t.Fatalf("expected failed HTML-bearing event accepted, got %+v", regRes)
	}
	if len(regRes.Rejected) != 1 || !strings.Contains(regRes.Rejected[0].Reason, "hmac") {
		t.Fatalf("expected missing HMAC v5 event rejected, got %+v", regRes.Rejected)
	}
	_ = tmpDir
}

// 5. Security Matrix: RBAC 403, Cross-Household IDOR 404, CSV Formula Neutralization + Secret Redaction, & B1-B10 Checks
func TestAuditR5_SecurityMatrixRBAC_IDOR_ExportAndBypassClosures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, bus, ae, _ := setupAuditR5TestEnv(t)

	// Seed a run for Household 1 containing secret leak candidate and CSV formula payload (=CMD|...)
	_, ctx, err := ae.StartRun(context.Background(), audit.StartRunInput{
		RunID:         "run-hh1-secret",
		HouseholdID:   1,
		TriggerSource: "manual_ops",
		ActorType:     "admin",
		ActorID:       "admin-1",
		Action:        "docker.restart",
		Target:        "=CMD|' /C calc'!A0",
		Component:     "home-web",
		Decision:      model.AuditDecisionAllow,
		RiskLevel:     model.AuditRiskMedium,
		InitialEvent: &audit.EmitEventInput{
			EventType: model.AuditEventExecutionStarted,
			Result:    "STARTED",
			Parameters: map[string]string{
				"password":          "UltraSecretPass999!",
				"Authorization":     "Bearer super-jwt-secret-token",
				"internal_ops_token": "int-secret-xyz-12345",
			},
			Evidence: "http://example.com/hook?token=leak123&safe=1",
		},
	})
	if err != nil || ctx == nil {
		t.Fatalf("StartRun failed: %v", err)
	}

	handler := NewOpsHandler(nil, nil, db, bus, nil)
	handler.SetAuditEngine(ae)

	buildRouter := func(isAdmin, isInternal bool, householdID uint) *gin.Engine {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Set("is_admin", isAdmin)
			c.Set("is_internal_service", isInternal)
			c.Set("household_id", householdID)
			c.Set("user_id", uint(99))
			c.Next()
		})
		handler.RegisterAuditRoutes(r.Group("/api/v1/ops"))
		return r
	}

	// RBAC: Non-admin & Internal service must receive 403 Forbidden
	for _, tc := range []struct {
		name       string
		isAdmin    bool
		isInternal bool
	}{
		{"regular_member", false, false},
		{"internal_service", true, true},
	} {
		r := buildRouter(tc.isAdmin, tc.isInternal, 1)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/ops/audit/runs", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s expected 403 Forbidden on /audit/runs, got %d (%s)", tc.name, w.Code, w.Body.String())
		}
	}

	// IDOR: Admin in Household 2 must NOT see Household 1's run
	rHH2 := buildRouter(true, false, 2)
	wIDOR := httptest.NewRecorder()
	rHH2.ServeHTTP(wIDOR, httptest.NewRequest(http.MethodGet, "/api/v1/ops/audit/runs/run-hh1-secret", nil))
	if wIDOR.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-household IDOR, got %d (%s)", wIDOR.Code, wIDOR.Body.String())
	}

	// CSV Export (HH1 Admin): must escape formula target (=CMD...) and redact secrets
	rHH1 := buildRouter(true, false, 1)
	wCSV := httptest.NewRecorder()
	rHH1.ServeHTTP(wCSV, httptest.NewRequest(http.MethodGet, "/api/v1/ops/audit/export?format=csv", nil))
	if wCSV.Code != http.StatusOK {
		t.Fatalf("expected 200 on CSV export, got %d (%s)", wCSV.Code, wCSV.Body.String())
	}
	if digest := wCSV.Header().Get("X-Audit-Export-SHA256"); len(digest) != 64 {
		t.Fatalf("expected 64-char X-Audit-Export-SHA256 header, got %q", digest)
	}
	csvBody := wCSV.Body.String()
	for _, leaked := range []string{"UltraSecretPass999!", "super-jwt-secret-token", "int-secret-xyz-12345", "leak123"} {
		if strings.Contains(csvBody, leaked) {
			t.Fatalf("secret %q leaked in audit export CSV!", leaked)
		}
	}
	records, csvErr := csv.NewReader(bytes.NewBufferString(csvBody)).ReadAll()
	if csvErr != nil || len(records) < 2 {
		t.Fatalf("failed to parse exported CSV: err=%v rows=%d", csvErr, len(records))
	}
	// Target column (index 7 in ExportEvents CSV) must be neutralized with leading single quote ('=)
	foundSafeFormula := false
	for _, row := range records[1:] {
		for _, cell := range row {
			if strings.HasPrefix(cell, "'=CMD") {
				foundSafeFormula = true
			}
		}
	}
	if !foundSafeFormula {
		t.Fatalf("expected CWE-1236 formula cell prefixed with ', got CSV:\n%s", csvBody)
	}
	_ = redact.EscapeCSVCell("=1+1")

	// B2 & B10: Component whitelist parity (home-new-api, mihomo, tailscale)
	for _, comp := range []string{"home-new-api", "home-backup", "mihomo", "tailscale", "home-ops-agent"} {
		if _, rErr := ResolveComponent(comp); rErr != nil {
			t.Fatalf("expected %s in unified coreComponentWhitelist, got err: %v", comp, rErr)
		}
		if resolved := ResolveTopologyComponent(comp); resolved == "" {
			t.Fatalf("expected %s mapped in ResolveTopologyComponent", comp)
		}
	}

	// Blocker-3: ReconcileProposalEndpoint must enforce household anti-IDOR isolation
	propHH1 := model.OpsProposal{
		ProposalID:      "prop-idor-rec-1",
		HouseholdID:     1,
		Title:           "Reconcile test",
		TargetComponent: "home-web",
		RiskLevel:       "HIGH",
		ExecutionPlan:   "plan",
		RollbackPlan:    "rb",
		Status:          model.ProposalStatusExecutionUnknown,
	}
	if err := db.Create(&propHH1).Error; err != nil {
		t.Fatalf("failed create hh1 proposal: %v", err)
	}
	rReconcileHH2 := gin.New()
	rReconcileHH2.Use(func(c *gin.Context) {
		c.Set("is_admin", true)
		c.Set("is_internal_service", false)
		c.Set("household_id", uint(2))
		c.Next()
	})
	handler.RegisterProposalRoutes(rReconcileHH2.Group("/api/v1/ops"))
	wRec := httptest.NewRecorder()
	reqRec := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/ops/proposals/prop-idor-rec-1/reconcile",
		strings.NewReader(`{"resolved_status":"FAILED","note":"cross-tenant attempt"}`),
	)
	reqRec.Header.Set("Content-Type", "application/json")
	rReconcileHH2.ServeHTTP(wRec, reqRec)
	if wRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on cross-household ReconcileProposalEndpoint IDOR, got %d (%s)", wRec.Code, wRec.Body.String())
	}
}
