package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/redact"
)

func TestR11_FI_07_OfflineReplayHashMismatchBeforeIdempotency(t *testing.T) {
	db, ae, _ := setupAuditUnitDB(t)
	kid := "k1"
	secret := "unit-test-audit-signing-key-32bytes-primary-v1"
	ts := time.Now().UTC().UnixMilli()

	ev1 := makeSignedOfflineEvent(t, kid, secret, "edge-fi-07", 1, "fi-07-uuid-1", model.AuditGenesisHash, ts, `{"step":"initial"}`)
	res1, err := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{ev1}, "req-07-1", "127.0.0.1", 1)
	if err != nil || len(res1.AcceptedUUIDs) != 1 {
		t.Fatalf("initial ingest failed: res=%+v err=%v", res1, err)
	}

	// Replay same EventUUID with tampered Evidence while keeping stale EntryHash -> HASH_MISMATCH before idempotency.
	tamperedPayload := ev1
	tamperedPayload.Evidence = `{"step":"tampered"}`
	res2, err := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{tamperedPayload}, "req-07-2", "127.0.0.1", 1)
	if err != nil || len(res2.Rejected) != 1 || res2.Rejected[0].ErrorCode != "HASH_MISMATCH" {
		t.Fatalf("expected HASH_MISMATCH on payload tamper replay, got res=%+v err=%v", res2, err)
	}

	// Verify DB row remained untouched.
	var stored model.AgentAuditEvent
	if err := db.Where("audit_id = ?", "fi-07-uuid-1").First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored.Evidence, "initial") {
		t.Fatalf("expected stored evidence untouched, got %q", stored.Evidence)
	}
}

func TestR11_FI_08_OfflineMissingHMACAndUnsignedBypassBlocked(t *testing.T) {
	_, ae, _ := setupAuditUnitDB(t)
	kid := "k1"
	secret := "unit-test-audit-signing-key-32bytes-primary-v1"
	ts := time.Now().UTC().UnixMilli()

	ev := makeSignedOfflineEvent(t, kid, secret, "edge-fi-08", 1, "fi-08-uuid-1", model.AuditGenesisHash, ts, `{"ok":true}`)
	missingHMAC := ev
	missingHMAC.HMACSignature = ""
	res, err := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{missingHMAC}, "req-08-1", "127.0.0.1", 1)
	if err != nil || len(res.Rejected) != 1 || (res.Rejected[0].ErrorCode != "MISSING_CRYPTO_PROOF" && res.Rejected[0].ErrorCode != "HMAC_INVALID") {
		t.Fatalf("expected MISSING_CRYPTO_PROOF or HMAC_INVALID when HMACSignature stripped, got res=%+v err=%v", res, err)
	}

	missingHash := ev
	missingHash.EntryHash = ""
	res2, err := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{missingHash}, "req-08-2", "127.0.0.1", 1)
	if err != nil || len(res2.Rejected) != 1 || res2.Rejected[0].ErrorCode != "MISSING_CRYPTO_PROOF" {
		t.Fatalf("expected MISSING_CRYPTO_PROOF when EntryHash stripped, got res=%+v err=%v", res2, err)
	}

	// Accept ev (seq=1), then send seq=2 with empty LocalPrevHash -> rejected with CHAIN_FORK.
	if resOk, errOk := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{ev}, "req-08-3", "127.0.0.1", 1); errOk != nil || len(resOk.AcceptedUUIDs) != 1 {
		t.Fatalf("expected seq=1 ingest accept, got res=%+v err=%v", resOk, errOk)
	}
	ev2 := makeSignedOfflineEvent(t, kid, secret, "edge-fi-08", 2, "fi-08-uuid-2", model.AuditGenesisHash, ts+1000, `{"ok":true}`)
	ev2.LocalPrevHash = ""
	res3, err := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{ev2}, "req-08-4", "127.0.0.1", 1)
	if err != nil || len(res3.Rejected) != 1 || res3.Rejected[0].ErrorCode != "CHAIN_FORK" {
		t.Fatalf("expected CHAIN_FORK when seq=2 omits LocalPrevHash, got res=%+v err=%v", res3, err)
	}
}

func TestR11_FI_09_OfflineSeqGapAndForkDetection(t *testing.T) {
	_, ae, _ := setupAuditUnitDB(t)
	kid := "k1"
	secret := "unit-test-audit-signing-key-32bytes-primary-v1"
	ts := time.Now().UTC().UnixMilli()

	// Out-of-order seq=2 without seq=1 MUST fail SEQ_GAP.
	ev2First := makeSignedOfflineEvent(t, kid, secret, "edge-fi-09", 2, "fi-09-2-early", model.AuditGenesisHash, ts, `{"n":2}`)
	resGap, err := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{ev2First}, "req-09-1", "127.0.0.1", 1)
	if err != nil || len(resGap.Rejected) != 1 || resGap.Rejected[0].ErrorCode != "SEQ_GAP" {
		t.Fatalf("expected SEQ_GAP for missing seq=1, got res=%+v err=%v", resGap, err)
	}

	// Establish seq=1, then submit seq=2 with wrong LocalPrevHash -> CHAIN_FORK.
	ev1 := makeSignedOfflineEvent(t, kid, secret, "edge-fi-09", 1, "fi-09-1", model.AuditGenesisHash, ts+10, `{"n":1}`)
	ev2Fork := makeSignedOfflineEvent(t, kid, secret, "edge-fi-09", 2, "fi-09-2-fork", strings.Repeat("a", 64), ts+20, `{"n":2}`)
	resFork, err := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{ev1, ev2Fork}, "req-09-2", "127.0.0.1", 1)
	if err != nil || len(resFork.AcceptedUUIDs) != 1 || len(resFork.Rejected) != 1 || resFork.Rejected[0].ErrorCode != "CHAIN_FORK" {
		t.Fatalf("expected seq=1 accepted and seq=2 CHAIN_FORK, got res=%+v err=%v", resFork, err)
	}
}

func TestR11_FI_10_OfflineForgedKeyIDAndWrongSecretRejected(t *testing.T) {
	_, ae, _ := setupAuditUnitDB(t)
	ts := time.Now().UTC().UnixMilli()

	// Forged key_id not present in keyring -> HMAC_INVALID.
	evForgedKID := makeSignedOfflineEvent(t, "k-forged", "unit-test-audit-signing-key-32bytes-primary-v1", "edge-fi-10", 1, "fi-10-forged", model.AuditGenesisHash, ts, `{"forged":true}`)
	res1, err := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{evForgedKID}, "req-10-1", "127.0.0.1", 1)
	if err != nil || len(res1.Rejected) != 1 || res1.Rejected[0].ErrorCode != "HMAC_INVALID" {
		t.Fatalf("expected HMAC_INVALID for forged key_id, got res=%+v err=%v", res1, err)
	}

	// Valid key_id="k1" signed with wrong secret -> HMAC_INVALID.
	evWrongSec := makeSignedOfflineEvent(t, "k1", "attacker-wrong-secret-key-at-least-32-bytes-long", "edge-fi-10", 1, "fi-10-wrongsec", model.AuditGenesisHash, ts+5, `{"wrong":true}`)
	res2, err := ae.IngestOfflineEvents(context.Background(), []OfflineSyncEvent{evWrongSec}, "req-10-2", "127.0.0.1", 1)
	if err != nil || len(res2.Rejected) != 1 || res2.Rejected[0].ErrorCode != "HMAC_INVALID" {
		t.Fatalf("expected HMAC_INVALID for wrong HMAC secret, got res=%+v err=%v", res2, err)
	}
}

func TestR11_FI_11_StrictInitRemoteConfigFaults(t *testing.T) {
	cleanupRing := SetAuditHMACKeyringForTest("k1", map[string]string{
		"k1": "r11-fi-audit-hmac-key-at-least-32-bytes-01",
	})
	defer cleanupRing()
	restoreExt := SetExternalCheckpointKeysForTest(nil, nil)
	restoreExt()

	resetEnv := func() {
		for _, k := range []string{
			"OPS_AUDIT_REMOTE_TYPE", "OPS_AUDIT_REMOTE_URL", "OPS_AUDIT_REMOTE_DIR",
			"OPS_AUDIT_REMOTE_TOKEN", "OPS_AUDIT_REMOTE_TIMEOUT_SEC",
			"OPS_AUDIT_CHECKPOINT_ED25519_PRIV", "OPS_AUDIT_CHECKPOINT_ED25519_PUB",
			"OPS_AUDIT_STRICT_REMOTE", "OPS_AUDIT_CHECKPOINT_PERIOD_SEC",
		} {
			t.Setenv(k, "")
		}
	}
	resetEnv()
	db := openAuditTestDBAtPath(t, filepath.Join(t.TempDir(), "fi-11.db"))

	cases := []struct {
		name    string
		env     map[string]string
		wantErr error
	}{
		{
			name:    "none_with_url_conflict",
			env:     map[string]string{"OPS_AUDIT_REMOTE_TYPE": "none", "OPS_AUDIT_REMOTE_URL": "http://127.0.0.1:9999"},
			wantErr: ErrAuditConfigInvalid,
		},
		{
			name:    "token_with_newline",
			env:     map[string]string{"OPS_AUDIT_REMOTE_TYPE": "http-append", "OPS_AUDIT_REMOTE_URL": "http://127.0.0.1:9999", "OPS_AUDIT_REMOTE_TOKEN": "valid-token-32bytes-minimum-with\nnewline"},
			wantErr: ErrAuditConfigInvalid,
		},
		{
			name:    "negative_checkpoint_period",
			env:     map[string]string{"OPS_AUDIT_CHECKPOINT_PERIOD_SEC": "-5"},
			wantErr: ErrAuditConfigInvalid,
		},
		{
			name:    "strict_remote_without_store",
			env:     map[string]string{"OPS_AUDIT_STRICT_REMOTE": "1"},
			wantErr: ErrAuditRemoteRequired,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetEnv()
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := NewStrictAgentAuditEngine(db, nil, WithArchiveDir(t.TempDir()))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v for %s, got %v", tc.wantErr, tc.name, err)
			}
		})
	}
}

func TestR11_FI_12_StrictInitEd25519FormatAndMismatchFaults(t *testing.T) {
	cleanupRing := SetAuditHMACKeyringForTest("k1", map[string]string{
		"k1": "r11-fi-ed25519-hmac-key-at-least-32-bytes",
	})
	defer cleanupRing()
	restoreExt := SetExternalCheckpointKeysForTest(nil, nil)
	restoreExt()

	db := openAuditTestDBAtPath(t, filepath.Join(t.TempDir(), "fi-12.db"))
	for _, k := range []string{
		"OPS_AUDIT_REMOTE_TYPE", "OPS_AUDIT_REMOTE_URL", "OPS_AUDIT_REMOTE_DIR",
		"OPS_AUDIT_REMOTE_TOKEN", "OPS_AUDIT_STRICT_REMOTE",
	} {
		t.Setenv(k, "")
	}

	t.Run("zero_seed", func(t *testing.T) {
		t.Setenv("OPS_AUDIT_CHECKPOINT_ED25519_PRIV", strings.Repeat("0", 64))
		t.Setenv("OPS_AUDIT_CHECKPOINT_ED25519_PUB", "")
		if _, err := NewStrictAgentAuditEngine(db, nil, WithArchiveDir(t.TempDir())); !errors.Is(err, ErrAuditConfigInvalid) {
			t.Fatalf("expected ErrAuditConfigInvalid for zero Ed25519 seed, got %v", err)
		}
	})

	t.Run("short_pub_key", func(t *testing.T) {
		t.Setenv("OPS_AUDIT_CHECKPOINT_ED25519_PRIV", "")
		t.Setenv("OPS_AUDIT_CHECKPOINT_ED25519_PUB", hex.EncodeToString(make([]byte, 16)))
		if _, err := NewStrictAgentAuditEngine(db, nil, WithArchiveDir(t.TempDir())); !errors.Is(err, ErrAuditConfigInvalid) {
			t.Fatalf("expected ErrAuditConfigInvalid for 16-byte Ed25519 pubkey, got %v", err)
		}
	})

	t.Run("mismatched_keypair", func(t *testing.T) {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
		t.Setenv("OPS_AUDIT_CHECKPOINT_ED25519_PRIV", hex.EncodeToString(priv.Seed()))
		t.Setenv("OPS_AUDIT_CHECKPOINT_ED25519_PUB", hex.EncodeToString(otherPub))
		if _, err := NewStrictAgentAuditEngine(db, nil, WithArchiveDir(t.TempDir())); !errors.Is(err, ErrAuditConfigInvalid) {
			t.Fatalf("expected ErrAuditConfigInvalid for mismatched Ed25519 keypair, got %v", err)
		}
	})
}

func TestR11_FI_13_RemoteSyncDBAndCheckpointErrorPropagation(t *testing.T) {
	cleanupRing := SetAuditHMACKeyringForTest("k1", map[string]string{
		"k1": "r11-fi-sync-db-hmac-key-at-least-32-bytes",
	})
	defer cleanupRing()

	var mu sync.Mutex
	var remoteEvents []model.AgentAuditEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/tail":
			tail := RemoteTailState{StreamID: model.AuditPrimaryStreamID}
			if n := len(remoteEvents); n > 0 {
				tail.TailSeq = remoteEvents[n-1].Seq
				tail.TailEntryHash = remoteEvents[n-1].EntryHash
			}
			_ = json.NewEncoder(w).Encode(tail)
		case r.Method == http.MethodGet && r.URL.Path == "/checkpoints":
			_ = json.NewEncoder(w).Encode([]model.AgentAuditCheckpoint{})
		case r.Method == http.MethodPost && r.URL.Path == "/events":
			var req struct {
				Events []model.AgentAuditEvent `json:"events"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &req)
			remoteEvents = append(remoteEvents, req.Events...)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && r.URL.Path == "/checkpoints":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	store, err := NewHTTPRemoteAuditStore(srv.URL, "r11-fi-13-remote-token-32bytes-min", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	bus := eventbus.New()
	var alertCount int
	var alertMu sync.Mutex
	unsub := bus.Subscribe("ops.audit.remote_sync_failed", func(_ eventbus.Event) {
		alertMu.Lock()
		alertCount++
		alertMu.Unlock()
	})
	defer unsub()

	db := openAuditTestDBAtPath(t, filepath.Join(t.TempDir(), "fi-13.db"))
	ae, err := NewStrictAgentAuditEngine(db, bus, WithArchiveDir(t.TempDir()), WithRemoteStore(store), WithStrictRemote(true), WithCheckpointInterval(100))
	if err != nil {
		t.Fatal(err)
	}
	defer ae.Close()

	if _, err := ae.EmitEvent(context.Background(), EmitEventInput{
		HouseholdID: 1, TriggerSource: "api_manual", ActorType: "user", ActorID: "admin",
		EventType: model.AuditEventExecutionCompleted, Action: "container.restart",
		Target: "home-web", Component: "home-web", Result: "SUCCESS",
	}); err != nil {
		t.Fatalf("EmitEvent failed: %v", err)
	}

	// Drop agent_audit_remote_sync_states -> saveRemoteSyncState DB error MUST surface in SyncRemoteStrict & emit alert.
	if err := db.Exec(`DROP TABLE agent_audit_remote_sync_states`).Error; err != nil {
		t.Fatal(err)
	}
	if _, syncErr := ae.SyncRemoteStrict(context.Background(), model.AuditPrimaryStreamID); syncErr == nil {
		t.Fatalf("expected SyncRemoteStrict to return error when sync state table missing")
	}
	alertMu.Lock()
	alerts := alertCount
	alertMu.Unlock()
	if alerts == 0 {
		t.Fatalf("expected ops.audit.remote_sync_failed alert on DB persistence failure")
	}
}

func TestR11_FI_14_RemoteTailTruncationAndDBRollbackDetection(t *testing.T) {
	cleanupRing := SetAuditHMACKeyringForTest("k1", map[string]string{
		"k1": "r11-fi-worm-hmac-key-at-least-32-bytes",
	})
	defer cleanupRing()

	wormDir := t.TempDir()
	store, err := NewFileWORMRemoteStore(wormDir)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "fi-14.db")
	db1 := openAuditTestDBAtPath(t, dbPath)
	ae1, err := NewStrictAgentAuditEngine(db1, nil, WithArchiveDir(t.TempDir()), WithRemoteStore(store), WithCheckpointInterval(2))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ae1.EmitEvent(context.Background(), EmitEventInput{
		HouseholdID: 1, TriggerSource: "api_manual", ActorType: "user", ActorID: "admin",
		EventType: model.AuditEventExecutionCompleted, Action: "container.restart",
		Target: "home-web", Component: "home-web", Result: "SUCCESS",
	}); err != nil {
		t.Fatal(err)
	}
	ae1.Close()

	rollbackSnap := filepath.Join(t.TempDir(), "snap-seq1.db")
	copyFileForRollbackTest(t, dbPath, rollbackSnap)

	db2 := openAuditTestDBAtPath(t, dbPath)
	ae2, err := NewStrictAgentAuditEngine(db2, nil, WithArchiveDir(t.TempDir()), WithRemoteStore(store), WithCheckpointInterval(2))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := ae2.EmitEvent(context.Background(), EmitEventInput{
			HouseholdID: 1, TriggerSource: "api_manual", ActorType: "user", ActorID: "admin",
			EventType: model.AuditEventExecutionCompleted, Action: "container.restart",
			Target: "home-web", Component: "home-web", Result: "SUCCESS",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, sErr := ae2.SyncRemoteStrict(context.Background(), model.AuditPrimaryStreamID); sErr != nil {
		t.Fatalf("SyncRemoteStrict failed: %v", sErr)
	}
	ae2.Close()

	// 1. Local tail truncation detection when DB rolls back to pre-sync snapshot (seq=1 < remote seq=3 with intact prefix).
	dbRolled := openAuditTestDBAtPath(t, rollbackSnap)
	aeRolled, err := NewStrictAgentAuditEngine(dbRolled, nil, WithArchiveDir(t.TempDir()), WithRemoteStore(store))
	if err != nil {
		t.Fatal(err)
	}
	defer aeRolled.Close()
	verTrunc := aeRolled.VerifyChain(VerifyRequest{VerifyCheckpoints: true, VerifyTriggers: true})
	if verTrunc.Valid || verTrunc.IntegrityStatus != IntegrityTruncatedTail {
		t.Fatalf("expected IntegrityTruncatedTail on seq=1 DB vs seq=3 remote, got %+v", verTrunc)
	}

	// 2. Whole-DB rollback detection when local DB is wiped empty (seq=0 < remote seq=3).
	dbWiped := openAuditTestDBAtPath(t, filepath.Join(t.TempDir(), "wiped.db"))
	aeWiped, err := NewStrictAgentAuditEngine(dbWiped, nil, WithArchiveDir(t.TempDir()), WithRemoteStore(store))
	if err != nil {
		t.Fatal(err)
	}
	defer aeWiped.Close()
	verRoll := aeWiped.VerifyChain(VerifyRequest{VerifyCheckpoints: true, VerifyTriggers: true})
	if verRoll.Valid || verRoll.IntegrityStatus != IntegrityDBRollbackDetected {
		t.Fatalf("expected IntegrityDBRollbackDetected on wiped DB vs remote tail, got %+v", verRoll)
	}
}

func TestR11_FI_15_KeyRotationVerificationAndZeroSecretLeak(t *testing.T) {
	sec1 := "r11-fi-rot-secret-key-v1-32-bytes-minimum-01"
	sec2 := "r11-fi-rot-secret-key-v2-32-bytes-minimum-02"
	redact.RegisterRuntimeSecret(sec1)
	redact.RegisterRuntimeSecret(sec2)
	cleanup1 := SetAuditHMACKeyringForTest("k1", map[string]string{"k1": sec1})
	db := openAuditTestDBAtPath(t, filepath.Join(t.TempDir(), "fi-15.db"))
	archDir := t.TempDir()

	ae1, err := NewStrictAgentAuditEngine(db, nil, WithArchiveDir(archDir), WithCheckpointInterval(10))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ae1.EmitEvent(context.Background(), EmitEventInput{
		HouseholdID: 1, TriggerSource: "api_manual", ActorType: "user", ActorID: "admin",
		EventType: model.AuditEventExecutionCompleted, Action: "container.restart",
		Target: "home-web", Component: "home-web", Result: "SUCCESS",
	}); err != nil {
		t.Fatal(err)
	}
	ae1.Close()
	cleanup1()

	cleanup2 := SetAuditHMACKeyringForTest("k2", map[string]string{"k1": sec1, "k2": sec2})
	defer cleanup2()
	ae2, err := NewStrictAgentAuditEngine(db, nil, WithArchiveDir(archDir), WithCheckpointInterval(10))
	if err != nil {
		t.Fatal(err)
	}
	defer ae2.Close()

	ver := ae2.VerifyChain(VerifyRequest{VerifyTriggers: true})
	if !ver.Valid || ver.CheckedEvents < 2 {
		t.Fatalf("expected valid chain with rotation event + historical k1 event, got %+v", ver)
	}

	ks := ae2.GetKeyStatus()
	ksBytes, _ := json.Marshal(ks)
	ksStr := string(ksBytes)
	if strings.Contains(ksStr, sec1) || strings.Contains(ksStr, sec2) || strings.Contains(redact.SensitiveString(sec1), sec1) {
		t.Fatalf("secret leaked in KeyStatus or redact failed: %s", ksStr)
	}
	if ks.ActiveKeyID != "k2" || ks.RotationCount < 1 || len(ks.ConfiguredKeyIDs) != 2 {
		t.Fatalf("unexpected KeyStatus after rotation: %+v", ks)
	}

	cleanup3 := SetAuditHMACKeyringForTest("k2", map[string]string{"k2": sec2})
	defer cleanup3()
	verRevoked := ae2.VerifyChain(VerifyRequest{VerifyTriggers: false})
	if verRevoked.Valid || verRevoked.IntegrityStatus != IntegrityHMACInvalid {
		t.Fatalf("expected IntegrityHMACInvalid when k1 removed from keyring, got %+v", verRevoked)
	}
}
