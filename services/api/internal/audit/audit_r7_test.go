package audit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/redact"
)

func setupAuditUnitDB(t *testing.T, opts ...EngineOption) (*gorm.DB, *AgentAuditEngine, string) {
	t.Helper()
	cleanup := SetAuditHMACKeyringForTest("k1", map[string]string{
		"k1": "unit-test-audit-signing-key-32bytes-primary-v1",
		"k2": "unit-test-audit-signing-key-32bytes-rotated-v2",
	})
	t.Cleanup(cleanup)

	tmpDir := t.TempDir()
	db, err := gorm.Open(sqlite.Open(filepath.Join(tmpDir, "audit_unit.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	if sqlDB, sErr := db.DB(); sErr == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	allOpts := append([]EngineOption{
		WithArchiveDir(filepath.Join(tmpDir, "archive")),
		WithCheckpointInterval(3),
	}, opts...)
	ae := NewAgentAuditEngine(db, eventbus.New(), allOpts...)
	return db, ae, tmpDir
}

func TestCanonicalJSONBytes_TrailingGarbageNotDecodedAsJSON(t *testing.T) {
	for _, s := range []string{"123abc", `{"a":1} trailing`} {
		b, err := CanonicalJSONBytes(s)
		if err != nil {
			t.Fatalf("CanonicalJSONBytes(%q) failed: %v", s, err)
		}
		var out string
		if uErr := json.Unmarshal(b, &out); uErr != nil || out != s {
			t.Fatalf("expected string %q preserved verbatim as JSON string, got %s (err=%v)", s, string(b), uErr)
		}
	}
}

func TestResolveAuditHMACKeyring_ValidationAndRedaction(t *testing.T) {
	auditKeyOverrideMu.Lock()
	wasOverride := auditKeyOverrideSet
	auditKeyOverrideSet = false
	auditKeyOverrideMu.Unlock()
	defer func() {
		auditKeyOverrideMu.Lock()
		auditKeyOverrideSet = wasOverride
		auditKeyOverrideMu.Unlock()
	}()

	sec1 := "env-primary-secret-key-minimum-32-bytes-001"
	sec2 := "env-rotated-secret-key-minimum-32-bytes-002"
	t.Setenv("OPS_AUDIT_HMAC_KEY_ID", "k2")
	t.Setenv("OPS_AUDIT_HMAC_KEYS", "k1:"+sec1+",k2:"+sec2)
	t.Setenv("OPS_AUDIT_HMAC_KEY", sec1) // Conflicts with k2 in OPS_AUDIT_HMAC_KEYS
	if _, _, err := ResolveAuditHMACKeyring(); !errors.Is(err, ErrAuditKeyWeak) {
		t.Fatalf("expected ErrAuditKeyWeak on key conflict, got %v", err)
	}

	t.Setenv("OPS_AUDIT_HMAC_KEY", sec2)
	kid, ring, err := ResolveAuditHMACKeyring()
	if err != nil || kid != "k2" || len(ring) != 2 {
		t.Fatalf("expected valid k2 keyring, got kid=%s len=%d err=%v", kid, len(ring), err)
	}
	if got := redact.SensitiveString("leak " + sec1 + " and " + sec2); strings.Contains(got, sec1) || strings.Contains(got, sec2) {
		t.Fatalf("expected registered keyring secrets scrubbed, got %q", got)
	}

	t.Setenv("OPS_AUDIT_HMAC_KEYS", "k1:too-short")
	if _, _, err := ResolveAuditHMACKeyring(); !errors.Is(err, ErrAuditKeyWeak) {
		t.Fatalf("expected ErrAuditKeyWeak for short key in OPS_AUDIT_HMAC_KEYS, got %v", err)
	}
}

func TestEmitEvent_SessionPropagation_Idempotency_AndDurableEmergencyQuota(t *testing.T) {
	db, ae, tmpDir := setupAuditUnitDB(t)

	_, ctx, err := ae.StartRun(context.Background(), StartRunInput{
		RunID:         "run-prop-01",
		HouseholdID:   1,
		TriggerSource: "proposal_exec",
		ActorType:     "user",
		ActorID:       "admin-1",
		Action:        "docker.restart",
		Target:        "home-web",
		Component:     "home-web",
		ProposalID:    "prop-123",
		ApprovalID:    "apv-digest-abc",
		OperationID:   "op-999",
	})
	if err != nil {
		t.Fatalf("StartRun failed: %v", err)
	}

	ev1, err := ae.EmitEvent(ctx, EmitEventInput{
		AuditID:   "aud-fixed-idemp-1",
		EventType: model.AuditEventExecutionStarted,
		Result:    "STARTED",
	})
	if err != nil {
		t.Fatalf("EmitEvent #1 failed: %v", err)
	}
	if ev1.ProposalID != "prop-123" || ev1.ApprovalID != "apv-digest-abc" || ev1.OperationID != "op-999" || ev1.KeyID != "k1" {
		t.Fatalf("expected session fields & key_id propagated, got %+v", ev1)
	}

	// Idempotent retry with same AuditID must return existing row without UNIQUE error
	evRetry, rErr := ae.EmitEvent(ctx, EmitEventInput{
		AuditID:   "aud-fixed-idemp-1",
		EventType: model.AuditEventExecutionStarted,
		Result:    "STARTED",
	})
	if rErr != nil || evRetry.Seq != ev1.Seq || evRetry.EntryHash != ev1.EntryHash {
		t.Fatalf("expected idempotent EmitEvent retry, got seq=%d err=%v", evRetry.Seq, rErr)
	}

	// Query by OperationID / ApprovalID
	runs, total, qErr := ae.QueryRuns(RunQuery{HouseholdID: 1, OperationID: "op-999"})
	if qErr != nil || total != 1 || runs[0].RunID != "run-prop-01" {
		t.Fatalf("QueryRuns by OperationID failed: total=%d err=%v", total, qErr)
	}
	runsSearch, totalSearch, sErr := ae.QueryRuns(RunQuery{HouseholdID: 1, Search: "op-999"})
	if sErr != nil || totalSearch != 1 || runsSearch[0].RunID != "run-prop-01" {
		t.Fatalf("QueryRuns Search by OperationID failed: total=%d err=%v", totalSearch, sErr)
	}

	// Durable bounded emergency quota across engine restarts + signed emergency stub
	archiveDir := filepath.Join(tmpDir, "archive")
	ae.SetFaultHooks(func(in EmitEventInput) error { return errors.New("db down") }, nil)
	emIn := EmitEventInput{
		HouseholdID:   1,
		Action:        "container.emergency_restart",
		Component:     "frigate",
		Target:        "frigate",
		FailurePolicy: PolicyBoundedEmergency,
	}
	if _, err1 := ae.RecordPreMutation(context.Background(), emIn); err1 != nil {
		t.Fatalf("expected emergency #1 allowed, got %v", err1)
	}
	// Simulate process restart with a fresh AgentAuditEngine pointing to same archiveDir
	aeRestarted := NewAgentAuditEngine(db, nil, WithArchiveDir(archiveDir))
	aeRestarted.SetFaultHooks(func(in EmitEventInput) error { return errors.New("db down") }, nil)
	if _, err2 := aeRestarted.RecordPreMutation(context.Background(), emIn); !errors.Is(err2, ErrAuditDegradedQuotaExceeded) {
		t.Fatalf("expected durable emergency quota enforced after restart, got %v", err2)
	}
	rawStub, readErr := os.ReadFile(filepath.Join(archiveDir, "emergency-audit.jsonl"))
	if readErr != nil || !strings.Contains(string(rawStub), "hmac-sha256:k1:") {
		t.Fatalf("expected HMAC-signed emergency stub, err=%v content=%s", readErr, string(rawStub))
	}
}

func makeSignedOfflineEvent(
	t *testing.T,
	kid, secret, originStream string,
	seq uint64,
	uuid, prevHash string,
	tsMs int64,
	evidence string,
) OfflineSyncEvent {
	t.Helper()
	ev := model.AgentAuditEvent{
		StreamID:           originStream,
		Seq:                seq,
		AuditID:            uuid,
		RunID:              "run-" + uuid,
		CorrelationID:      "corr-" + uuid,
		HouseholdID:        1,
		TriggerSource:      "python_offline",
		ActorType:          "offline_agent",
		ActorID:            "home-ops-agent",
		EventType:          model.AuditEventExecutionCompleted,
		Action:             "container.emergency_restart",
		Target:             "frigate",
		Component:          "frigate",
		Decision:           model.AuditDecisionAllow,
		RiskLevel:          model.AuditRiskHigh,
		PolicyVersion:      PolicyVersion,
		ProposalID:         "prop-off-1",
		ApprovalID:         "apv-off-1",
		OperationID:        "op-" + uuid,
		FencingGeneration:  4,
		ParametersRedacted: "{}",
		Result:             "SUCCESS",
		DurationMs:         250,
		Evidence:           evidence,
		CreatedAt:          time.UnixMilli(tsMs).UTC(),
		CreatedAtMs:        tsMs,
		IsOffline:          true,
		PreviousHash:       prevHash,
	}
	entryHash, err := ComputeEventEntryHash(&ev)
	if err != nil {
		t.Fatalf("ComputeEventEntryHash failed: %v", err)
	}
	hmacSig := ComputeEventHMAC(kid, secret, originStream, seq, entryHash, tsMs)
	return OfflineSyncEvent{
		EventUUID:          uuid,
		AuditID:            uuid,
		RunID:              ev.RunID,
		CorrelationID:      ev.CorrelationID,
		HouseholdID:        1,
		OperationID:        ev.OperationID,
		ProposalID:         ev.ProposalID,
		ApprovalID:         ev.ApprovalID,
		PolicyVersion:      PolicyVersion,
		TriggerSource:      ev.TriggerSource,
		ActorType:          ev.ActorType,
		ActorID:            ev.ActorID,
		EventType:          ev.EventType,
		ContainerName:      "frigate",
		Component:          "frigate",
		Target:             "frigate",
		Action:             ev.Action,
		Decision:           ev.Decision,
		RiskLevel:          ev.RiskLevel,
		Holder:             "home-ops-agent",
		FencingGeneration:  4,
		Success:            1,
		ParametersRedacted: "{}",
		Evidence:           evidence,
		DurationMs:         250,
		ExecutedAt:         FormatRFC3339Ms(ev.CreatedAt),
		ExecutedAtMs:       tsMs,
		OriginStreamID:     originStream,
		OfflineSeq:         seq,
		LocalPrevHash:      prevHash,
		EntryHash:          entryHash,
		KeyID:              kid,
		HMACSignature:      hmacSig,
	}
}

func TestIngestOfflineEvents_StrictProof_Gap_Fork_AndCrossDevice(t *testing.T) {
	db, ae, _ := setupAuditUnitDB(t)
	kid := "k1"
	secret := "unit-test-audit-signing-key-32bytes-primary-v1"
	baseMs := time.Now().UTC().UnixMilli()

	// 1. Unsigned event MUST be rejected fail-closed
	unsignedRes, err := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{
		{EventUUID: "off-unsigned", ContainerName: "frigate", Action: "docker.restart", Success: 1},
	}, "req-1", "127.0.0.1", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(unsignedRes.AcceptedUUIDs) != 0 || len(unsignedRes.Rejected) != 1 || unsignedRes.Rejected[0].ErrorCode != "MISSING_CRYPTO_PROOF" {
		t.Fatalf("expected MISSING_CRYPTO_PROOF rejection, got %+v", unsignedRes)
	}

	// 2. Valid seq=1 on stream A + missing seq=2 (seq=3 submitted) -> seq=1 accepted, seq=3 rejected with SEQ_GAP
	evA1 := makeSignedOfflineEvent(t, kid, secret, "edge-node-a", 1, "off-a-1", model.AuditGenesisHash, baseMs, `{"ok":true}`)
	evA3 := makeSignedOfflineEvent(t, kid, secret, "edge-node-a", 3, "off-a-3", evA1.EntryHash, baseMs+20, `{"ok":true}`)
	gapRes, err := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{evA1, evA3}, "req-2", "127.0.0.1", 1)
	if err != nil {
		t.Fatalf("gap batch error: %v", err)
	}
	if len(gapRes.AcceptedUUIDs) != 1 || gapRes.AcceptedUUIDs[0] != "off-a-1" {
		t.Fatalf("expected only off-a-1 accepted, got %+v", gapRes)
	}
	if len(gapRes.Rejected) != 1 || gapRes.Rejected[0].ErrorCode != "SEQ_GAP" || gapRes.ChainStatus != IntegritySeqGap {
		t.Fatalf("expected SEQ_GAP rejection & ChainStatus, got %+v", gapRes)
	}

	// 3. Cross-device duplicate seq=1 on distinct stream B succeeds, while conflicting seq=1 on stream A is rejected
	evB1 := makeSignedOfflineEvent(t, kid, secret, "edge-node-b", 1, "off-b-1", model.AuditGenesisHash, baseMs+30, `{"node":"b"}`)
	evA1Fork := makeSignedOfflineEvent(t, kid, secret, "edge-node-a", 1, "off-a-1-fork", model.AuditGenesisHash, baseMs+40, `{"fork":true}`)
	forkRes, err := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{evB1, evA1Fork}, "req-3", "127.0.0.1", 1)
	if err != nil {
		t.Fatalf("fork batch error: %v", err)
	}
	if len(forkRes.AcceptedUUIDs) != 1 || forkRes.AcceptedUUIDs[0] != "off-b-1" {
		t.Fatalf("expected off-b-1 accepted, got %+v", forkRes)
	}
	if len(forkRes.Rejected) != 1 || forkRes.Rejected[0].ErrorCode != "DUPLICATE_SEQ_CONFLICT" || forkRes.ChainStatus != IntegrityChainFork {
		t.Fatalf("expected DUPLICATE_SEQ_CONFLICT, got %+v", forkRes)
	}

	// 4. Tampered replay with same EventUUID MUST fail cryptographic entry_hash check BEFORE idempotency!
	tamperedReplay := evA1
	tamperedReplay.EntryHash = strings.Repeat("f", 64)
	repRes, err := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{tamperedReplay}, "req-4", "127.0.0.1", 1)
	if err != nil || len(repRes.Rejected) != 1 || repRes.Rejected[0].ErrorCode != "HASH_MISMATCH" {
		t.Fatalf("expected HASH_MISMATCH on tampered replay before idempotency, got %+v err=%v", repRes, err)
	}

	// 4b. Unsigned / seq=0 replay sharing an existing EventUUID MUST fail MISSING_CRYPTO_PROOF (never blindly idempotent!)
	unsignedExisting := OfflineSyncEvent{
		EventUUID:     evA1.EventUUID,
		AuditID:       evA1.AuditID,
		ContainerName: "frigate",
		Action:        "container.emergency_restart",
		Success:       1,
	}
	unRes, err := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{unsignedExisting}, "req-4b", "127.0.0.1", 1)
	if err != nil || len(unRes.Rejected) != 1 || unRes.Rejected[0].ErrorCode != "MISSING_CRYPTO_PROOF" {
		t.Fatalf("expected MISSING_CRYPTO_PROOF on unsigned existing-UUID replay, got %+v err=%v", unRes, err)
	}

	// 4c. Validly re-signed different event reusing existing EventUUID on another stream -> passes hash+HMAC+chain, then rejected by DUPLICATE_ID_CONFLICT
	resignedConflict := makeSignedOfflineEvent(t, kid, secret, "edge-node-c", 1, evA1.EventUUID, model.AuditGenesisHash, baseMs+50, `{"conflicting":"payload"}`)
	confRes, err := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{resignedConflict}, "req-4c", "127.0.0.1", 1)
	if err != nil || len(confRes.Rejected) != 1 || confRes.Rejected[0].ErrorCode != "DUPLICATE_ID_CONFLICT" {
		t.Fatalf("expected DUPLICATE_ID_CONFLICT on validly signed conflicting existing UUID, got %+v err=%v", confRes, err)
	}

	// 4d. Exact duplicate retry with valid hash+HMAC+chain and matching (origin_stream_id, origin_seq, origin_entry_hash) -> accepted idempotently
	exactRetry, err := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{evA1}, "req-4d", "127.0.0.1", 1)
	if err != nil || len(exactRetry.Rejected) != 0 || len(exactRetry.AcceptedUUIDs) != 1 || exactRetry.AcceptedUUIDs[0] != evA1.EventUUID || exactRetry.SyncedCount != 0 {
		t.Fatalf("expected idempotent acceptance on exact signed retry, got %+v err=%v", exactRetry, err)
	}

	// 4e. Invalid origin_stream_id -> rejected with INVALID_ORIGIN_STREAM before crypto hashing
	badStreamEv := evA1
	badStreamEv.OriginStreamID = "edge/bad stream"
	badStreamRes, err := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{badStreamEv}, "req-4e", "127.0.0.1", 1)
	if err != nil || len(badStreamRes.Rejected) != 1 || badStreamRes.Rejected[0].ErrorCode != "INVALID_ORIGIN_STREAM" {
		t.Fatalf("expected INVALID_ORIGIN_STREAM rejection, got %+v err=%v", badStreamRes, err)
	}

	// 4f. Legacy record without origin_entry_hash (e.g. only in structured_ops_audit_logs) MUST NOT be blindly accepted
	if execErr := db.Exec(`
		INSERT INTO structured_ops_audit_logs (
			event_uuid, household_id, request_id, actor_type, actor_id, actor_name,
			client_ip, target_type, target_id, action, execution_result, is_offline_action, created_at
		) VALUES ('off-legacy-no-hash', 1, 'req-leg', 'offline_agent', 0, 'legacy', '127.0.0.1', 'container', 'frigate', 'restart', '{}', 1, CURRENT_TIMESTAMP)`).Error; execErr != nil {
		t.Fatalf("insert legacy structured log failed: %v", execErr)
	}
	legacyDup := makeSignedOfflineEvent(t, kid, secret, "edge-node-legacy", 1, "off-legacy-no-hash", model.AuditGenesisHash, baseMs+60, `{"legacy":true}`)
	legRes, err := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{legacyDup}, "req-4f", "127.0.0.1", 1)
	if err != nil || len(legRes.Rejected) != 1 || legRes.Rejected[0].ErrorCode != "DUPLICATE_ID_CONFLICT" {
		t.Fatalf("expected DUPLICATE_ID_CONFLICT (no blind acceptance of legacy row), got %+v err=%v", legRes, err)
	}

	// 5. Verify full Core chain & persisted metadata (ProposalID, ApprovalID, OriginStreamID, KeyID)
	ver := ae.VerifyChain(VerifyRequest{VerifyCheckpoints: true, VerifyTriggers: true})
	if !ver.Valid {
		t.Fatalf("expected valid Core chain after offline sync, got %+v", ver)
	}
	var stored model.AgentAuditEvent
	if err := db.Where("audit_id = ?", "off-a-1").First(&stored).Error; err != nil {
		t.Fatalf("load stored event failed: %v", err)
	}
	if stored.ProposalID != "prop-off-1" || stored.ApprovalID != "apv-off-1" || stored.DurationMs != 250 || stored.OriginStreamID != "edge-node-a" || stored.KeyID != "k1" {
		t.Fatalf("expected full metadata preserved on Core event, got %+v", stored)
	}
}

func TestVerifyChain_ExternalCheckpointDivergenceAndRunVerification(t *testing.T) {
	db, ae, tmpDir := setupAuditUnitDB(t)
	for i := 0; i < 4; i++ {
		_, err := ae.EmitEvent(context.Background(), EmitEventInput{
			RunID:       "run-chk-1",
			HouseholdID: 1,
			EventType:   model.AuditEventAnalysisCompleted,
			Action:      "probe",
			Target:      "home-api",
			Component:   "home-api",
			Result:      "OK",
		})
		if err != nil {
			t.Fatalf("EmitEvent[%d] failed: %v", i, err)
		}
	}

	// VerifyRunChain passes cleanly
	detail, err := ae.GetRunDetail("run-chk-1", 1)
	if err != nil || !detail.Integrity.Valid || detail.Integrity.CheckedEvents != 4 {
		t.Fatalf("expected GetRunDetail valid with 4 events, got err=%v integrity=%+v", err, detail.Integrity)
	}

	// ExportEvents sorts chronologically (seq ASC)
	raw, _, _, exErr := ae.ExportEvents(EventQuery{HouseholdID: 1, RunID: "run-chk-1"}, "json")
	if exErr != nil {
		t.Fatalf("ExportEvents failed: %v", exErr)
	}
	var exported struct {
		Events []model.AgentAuditEvent `json:"events"`
	}
	if err := json.Unmarshal(raw, &exported); err != nil || len(exported.Events) != 4 {
		t.Fatalf("parse exported JSON failed: %v", err)
	}
	if exported.Events[0].Seq != 1 || exported.Events[3].Seq != 4 {
		t.Fatalf("expected chronological seq ASC export, got first=%d last=%d", exported.Events[0].Seq, exported.Events[3].Seq)
	}

	// Delete DB checkpoint while checkpoints.jsonl still holds seq=3 -> VerifyChain detects missing DB checkpoint even with ToSeq=2
	_ = db.Exec("DROP TRIGGER IF EXISTS trg_agent_audit_checkpoints_no_delete").Error
	_ = db.Exec("DELETE FROM agent_audit_checkpoints").Error
	_ = db.Exec(`
		CREATE TRIGGER IF NOT EXISTS trg_agent_audit_checkpoints_no_delete
		BEFORE DELETE ON agent_audit_checkpoints
		BEGIN
			SELECT RAISE(ABORT, 'agent_audit_checkpoints is append-only');
		END;
	`).Error

	// Also verify VerifyChain catches external checkpoint tail mismatch when DB rows are fewer than checkpoint
	_ = db.Exec("DROP TRIGGER IF EXISTS trg_agent_audit_events_no_delete").Error
	_ = db.Exec("DELETE FROM agent_audit_events WHERE seq >= 3").Error
	_ = db.Exec(`
		CREATE TRIGGER IF NOT EXISTS trg_agent_audit_events_no_delete
		BEFORE DELETE ON agent_audit_events
		BEGIN
			SELECT RAISE(ABORT, 'agent_audit_events is append-only');
		END;
	`).Error

	rangedRes := ae.VerifyChain(VerifyRequest{FromSeq: 1, ToSeq: 2, VerifyCheckpoints: true, VerifyTriggers: true})
	if rangedRes.Valid || rangedRes.IntegrityStatus != IntegrityTruncatedTail {
		t.Fatalf("expected TRUNCATED_TAIL even on ranged VerifyChain(ToSeq=2), got %+v", rangedRes)
	}
	_ = tmpDir
}
