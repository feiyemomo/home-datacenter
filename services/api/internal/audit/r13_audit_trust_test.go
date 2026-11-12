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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"home-datacenter-api/internal/model"
)

func boolPtrR13(v bool) *bool { return &v }

type r13MockRemoteServer struct {
	mu           sync.Mutex
	failRequests atomic.Bool
	expectedTok  string
	events       map[uint64]model.AgentAuditEvent
	checkpoints  map[string]model.AgentAuditCheckpoint
	tailSeq      uint64
	tailHash     string
	lastCpID     string
	lastCpSeq    uint64
	chainDigest  string
}

func newR13MockRemoteServer(t *testing.T, expectedTok string) (*r13MockRemoteServer, *httptest.Server) {
	t.Helper()
	state := &r13MockRemoteServer{
		expectedTok: expectedTok,
		events:      make(map[uint64]model.AgentAuditEvent),
		checkpoints: make(map[string]model.AgentAuditCheckpoint),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if state.failRequests.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "remote replica unavailable token="+r.Header.Get("Authorization"))
			return
		}
		if state.expectedTok != "" && r.Header.Get("Authorization") != "Bearer "+state.expectedTok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "unauthorized")
			return
		}
		state.mu.Lock()
		defer state.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tail"):
			_ = json.NewEncoder(w).Encode(RemoteTailState{
				StreamID:          model.AuditPrimaryStreamID,
				TailSeq:           state.tailSeq,
				TailEntryHash:     state.tailHash,
				EventCount:        int64(len(state.events)),
				LastCheckpointID:  state.lastCpID,
				LastCheckpointSeq: state.lastCpSeq,
				LastChainDigest:   state.chainDigest,
				UpdatedAt:         time.Now().UTC().Format(time.RFC3339),
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			fromSeq, _ := strconv.ParseUint(r.URL.Query().Get("from_seq"), 10, 64)
			toSeq, _ := strconv.ParseUint(r.URL.Query().Get("to_seq"), 10, 64)
			var out []model.AgentAuditEvent
			for seq, ev := range state.events {
				if seq >= fromSeq && (toSeq == 0 || seq <= toSeq) {
					out = append(out, ev)
				}
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/checkpoints"):
			var out []model.AgentAuditCheckpoint
			for _, cp := range state.checkpoints {
				out = append(out, cp)
			}
			sort.Slice(out, func(i, j int) bool { return out[i].ToSeq < out[j].ToSeq })
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events"):
			var payload struct {
				Events []model.AgentAuditEvent `json:"events"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for _, ev := range payload.Events {
				if prev, ok := state.events[ev.Seq]; ok {
					if prev.EntryHash != ev.EntryHash {
						w.WriteHeader(http.StatusConflict)
						_, _ = io.WriteString(w, "sequence fork rejected")
						return
					}
					continue
				}
				state.events[ev.Seq] = ev
				if ev.Seq > state.tailSeq {
					state.tailSeq = ev.Seq
					state.tailHash = ev.EntryHash
				}
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":true}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/checkpoints"):
			var cp model.AgentAuditCheckpoint
			if err := json.NewDecoder(r.Body).Decode(&cp); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			state.checkpoints[cp.CheckpointID] = cp
			if cp.ToSeq >= state.lastCpSeq {
				state.lastCpID = cp.CheckpointID
				state.lastCpSeq = cp.ToSeq
				state.chainDigest = cp.ChainDigest
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return state, srv
}

func TestR13_StrictLocalOnly_OverridesStaleRemoteSyncState(t *testing.T) {
	cleanupRing := SetAuditHMACKeyringForTest("k1", map[string]string{
		"k1": "r13-local-only-hmac-key-at-least-32-bytes-01",
	})
	defer cleanupRing()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "audit.db")
	wormDir := filepath.Join(tmpDir, "worm")
	archiveDir := filepath.Join(tmpDir, "archive")

	// Phase A: Run with FileWORM remote store and sync so SQLite holds an IN_SYNC row.
	db := openAuditTestDBAtPath(t, dbPath)
	worm, err := NewFileWORMRemoteStore(wormDir)
	if err != nil {
		t.Fatalf("NewFileWORMRemoteStore: %v", err)
	}
	engRemote, err := NewStrictAgentAuditEngine(
		db, nil,
		WithArchiveDir(archiveDir),
		WithCheckpointInterval(100),
		WithRemoteStore(worm),
		WithStrictRemote(false),
	)
	if err != nil {
		t.Fatalf("NewStrictAgentAuditEngine(remote): %v", err)
	}
	resRun, _, err := engRemote.StartRun(context.Background(), StartRunInput{
		RunID:         "run-r13-local-only",
		CorrelationID: "corr-r13-1",
		HouseholdID:   1,
		TriggerSource: "api_manual",
		ActorType:     "user",
		ActorID:       "admin-1",
		Action:        "test.emit",
		Target:        "svc-a",
		Component:     "test",
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := engRemote.EmitEvent(context.Background(), EmitEventInput{
		RunID:          resRun,
		HouseholdID:    1,
		EventType:      model.AuditEventExecutionCompleted,
		Action:         "test.emit",
		Target:         "svc-a",
		Component:      "test",
		Result:         "SUCCESS",
		FinalRunStatus: model.AuditRunStatusCompleted,
	}); err != nil {
		t.Fatalf("EmitEvent: %v", err)
	}
	syncRep := engRemote.SyncRemote(context.Background(), model.AuditPrimaryStreamID)
	if syncRep.Status != RemoteStatusInSync {
		t.Fatalf("expected SyncRemote IN_SYNC, got %+v", syncRep)
	}
	syncStateSynced, err := engRemote.GetRemoteSyncState(model.AuditPrimaryStreamID)
	if err != nil {
		t.Fatalf("GetRemoteSyncState(synced): %v", err)
	}
	if syncStateSynced.SyncStatus != RemoteStatusInSync {
		t.Fatalf("expected initial sync_status IN_SYNC, got %q", syncStateSynced.SyncStatus)
	}

	// Phase B: Re-open the same DB WITHOUT any remote store configured.
	// Stale SQLite sync_state must NEVER masquerade as IN_SYNC.
	engLocal, err := NewStrictAgentAuditEngine(
		db, nil,
		WithArchiveDir(archiveDir),
		WithCheckpointInterval(100),
		WithRemoteStore(nil),
		WithStrictRemote(false),
	)
	if err != nil {
		t.Fatalf("NewStrictAgentAuditEngine(local-only): %v", err)
	}

	syncStateLocal, err := engLocal.GetRemoteSyncState(model.AuditPrimaryStreamID)
	if err != nil {
		t.Fatalf("GetRemoteSyncState(local-only): %v", err)
	}
	if syncStateLocal.SyncStatus != RemoteStatusLocalOnly || syncStateLocal.RemoteStoreType != "none" || syncStateLocal.RemoteEndpoint != "" {
		t.Fatalf("stale sync state leaked through local-only mode: %+v", syncStateLocal)
	}

	diff := engLocal.CompareWithRemote(context.Background(), model.AuditPrimaryStreamID)
	if !diff.LocalOnly || diff.Configured || diff.Status != RemoteStatusLocalOnly || diff.StoreType != "none" {
		t.Fatalf("CompareWithRemote did not enforce LOCAL_ONLY: %+v", diff)
	}
	if !diff.TrustBoundaries.LocalOnly || diff.TrustBoundaries.WORMGuarantee != WORMGuaranteeNoneLocalOnly || diff.TrustBoundaries.LocalChain != LocalChainBoundaryIntact {
		t.Fatalf("unexpected trust boundaries on local-only diff: %+v", diff.TrustBoundaries)
	}

	vRes := engLocal.VerifyChain(VerifyRequest{
		VerifyCheckpoints: true,
		VerifyTriggers:    true,
		VerifyArchive:     true,
		VerifyRemote:      boolPtrR13(true),
	})
	if !vRes.Valid || vRes.IntegrityStatus != IntegrityChainIntactLocalOnly || !vRes.LocalOnly || vRes.RemoteStoreConfigured || vRes.RemoteStoreType != "none" || vRes.RemoteSyncStatus != RemoteStatusLocalOnly {
		t.Fatalf("VerifyChain did not strictly mark LOCAL_ONLY: %+v", vRes)
	}
	if !vRes.TrustBoundaries.LocalOnly || vRes.TrustBoundaries.RemoteStore != RemoteStatusLocalOnly {
		t.Fatalf("VerifyChain trust boundaries mismatch: %+v", vRes.TrustBoundaries)
	}

	runRes := engLocal.VerifyRunChain(resRun, 1)
	if !runRes.Valid || !runRes.LocalOnly || runRes.RemoteStoreConfigured || runRes.RemoteSyncStatus != RemoteStatusLocalOnly || !runRes.TrustBoundaries.LocalOnly {
		t.Fatalf("VerifyRunChain did not strictly mark LOCAL_ONLY: %+v", runRes)
	}

	ks := engLocal.GetKeyStatus()
	if !ks.LocalOnly || ks.RemoteStoreConfigured || !ks.TrustBoundaries.LocalOnly {
		t.Fatalf("GetKeyStatus did not strictly mark LOCAL_ONLY: %+v", ks)
	}
}

func TestR13_TrustBoundaries_4AxisClassification_And_NoFalseClaims(t *testing.T) {
	cleanupRing := SetAuditHMACKeyringForTest("k1", map[string]string{
		"k1": "r13-boundary-hmac-secret-at-least-32-bytes-01",
	})
	defer cleanupRing()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	signer, err := NewEd25519CheckpointSigner("ed-sign-1", priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	verifierOnly, err := NewEd25519CheckpointVerifier("ed-verify-1", pub)
	if err != nil {
		t.Fatalf("verifierOnly: %v", err)
	}

	// Case 1: LOCAL_ONLY + co-resident HMAC only
	db1 := openAuditTestDBAtPath(t, filepath.Join(t.TempDir(), "db1.db"))
	eng1, err := NewStrictAgentAuditEngine(db1, nil, WithArchiveDir(t.TempDir()), WithStrictRemote(false))
	if err != nil {
		t.Fatalf("eng1: %v", err)
	}
	ks1 := eng1.GetKeyStatus()
	if !ks1.TrustBoundaries.LocalOnly ||
		ks1.TrustBoundaries.LocalChain != LocalChainBoundaryIntact ||
		ks1.TrustBoundaries.RemoteStore != RemoteStatusLocalOnly ||
		ks1.TrustBoundaries.WORMGuarantee != WORMGuaranteeNoneLocalOnly ||
		ks1.TrustBoundaries.PrivateKeySeparation != KeySepHMACCoResidentEnvOnly ||
		ks1.TrustBoundaries.HMACKeyScope != HMACKeyScopeCoResident ||
		ks1.TrustBoundaries.SigningPrivKeyPresent {
		t.Fatalf("Case 1 trust boundaries mismatch: %+v", ks1.TrustBoundaries)
	}

	// Case 2: file-worm + co-resident Ed25519 private signing key
	db2 := openAuditTestDBAtPath(t, filepath.Join(t.TempDir(), "db2.db"))
	worm, err := NewFileWORMRemoteStore(filepath.Join(t.TempDir(), "worm"))
	if err != nil {
		t.Fatalf("worm: %v", err)
	}
	eng2, err := NewStrictAgentAuditEngine(
		db2, nil,
		WithArchiveDir(t.TempDir()),
		WithRemoteStore(worm),
		WithExternalSigner(signer),
		WithStrictRemote(false),
	)
	if err != nil {
		t.Fatalf("eng2: %v", err)
	}
	ks2 := eng2.GetKeyStatus()
	if ks2.TrustBoundaries.LocalOnly ||
		ks2.TrustBoundaries.LocalChain != LocalChainBoundaryIntact ||
		ks2.TrustBoundaries.RemoteStore != "FILE_WORM" ||
		ks2.TrustBoundaries.WORMGuarantee != WORMGuaranteeHostPathAppendOnly ||
		ks2.TrustBoundaries.PrivateKeySeparation != KeySepEd25519PrivCoResident ||
		!ks2.TrustBoundaries.SigningPrivKeyPresent {
		t.Fatalf("Case 2 trust boundaries mismatch: %+v", ks2.TrustBoundaries)
	}

	// Case 3: http-append + verify-only separated Ed25519 public key (private key never on API host)
	db3 := openAuditTestDBAtPath(t, filepath.Join(t.TempDir(), "db3.db"))
	_, srv := newR13MockRemoteServer(t, "tok-r13-boundary")
	httpStore, err := NewHTTPRemoteAuditStore(srv.URL+"/api/v1/audit-replica", "tok-r13-boundary", 3*time.Second)
	if err != nil {
		t.Fatalf("httpStore: %v", err)
	}
	eng3, err := NewStrictAgentAuditEngine(
		db3, nil,
		WithArchiveDir(t.TempDir()),
		WithRemoteStore(httpStore),
		WithExternalVerifier(verifierOnly),
		WithStrictRemote(false),
	)
	if err != nil {
		t.Fatalf("eng3: %v", err)
	}
	ks3 := eng3.GetKeyStatus()
	if ks3.TrustBoundaries.LocalOnly ||
		ks3.TrustBoundaries.RemoteStore != "HTTP_APPEND" ||
		ks3.TrustBoundaries.WORMGuarantee != WORMGuaranteeRemoteAppendProxy ||
		ks3.TrustBoundaries.PrivateKeySeparation != KeySepEd25519VerifyOnlySeparated ||
		ks3.TrustBoundaries.SigningPrivKeyPresent {
		t.Fatalf("Case 3 trust boundaries mismatch: %+v", ks3.TrustBoundaries)
	}
}

func TestR13_RemoteDisconnect_TailTruncation_DBRollback_VerifyRunChain(t *testing.T) {
	cleanupRing := SetAuditHMACKeyringForTest("k1", map[string]string{
		"k1": "r13-fault-injection-hmac-key-at-least-32-bytes",
	})
	defer cleanupRing()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	signer, err := NewEd25519CheckpointSigner("ed-fi-1", priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "audit.db")
	snapPath := filepath.Join(tmpDir, "audit-snap-seq1.db")
	archiveDir := filepath.Join(tmpDir, "archive")

	mockRemote, srv := newR13MockRemoteServer(t, "tok-fi-1")
	httpStore, err := NewHTTPRemoteAuditStore(srv.URL+"/api/v1/audit-replica", "tok-fi-1", 3*time.Second)
	if err != nil {
		t.Fatalf("NewHTTPRemoteAuditStore: %v", err)
	}

	db := openAuditTestDBAtPath(t, dbPath)
	eng, err := NewStrictAgentAuditEngine(
		db, nil,
		WithArchiveDir(archiveDir),
		WithCheckpointInterval(100),
		WithRemoteStore(httpStore),
		WithExternalSigner(signer),
		WithStrictRemote(false),
	)
	if err != nil {
		t.Fatalf("NewStrictAgentAuditEngine: %v", err)
	}

	run, _, err := eng.StartRun(context.Background(), StartRunInput{
		RunID:         "run-r13-fi",
		CorrelationID: "corr-r13-fi",
		HouseholdID:   1,
		TriggerSource: "api_manual",
		ActorType:     "user",
		ActorID:       "admin-1",
		Action:        "svc.restart",
		Target:        "immich",
		Component:     "docker",
		InitialEvent: &EmitEventInput{
			EventType: model.AuditEventProposalCreated,
			Action:    "svc.restart",
			Decision:  "APPROVED",
			Result:    "QUEUED",
		},
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if rep := eng.SyncRemote(context.Background(), model.AuditPrimaryStreamID); rep.Status != RemoteStatusInSync {
		t.Fatalf("SyncRemote #1 expected IN_SYNC, got: %+v", rep)
	}

	// Snapshot SQLite at seq 1 while remote is synced at seq 1.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB: %v", err)
	}
	if _, err := sqlDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatalf("wal checkpoint: %v", err)
	}
	copyFileForRollbackTest(t, dbPath, snapPath)

	// Emit event #2 and #3 and sync + checkpoint to remote.
	if _, err := eng.EmitEvent(context.Background(), EmitEventInput{
		RunID:       run,
		HouseholdID: 1,
		EventType:   model.AuditEventExecutionStarted,
		Action:      "svc.restart",
		Result:      "RUNNING",
	}); err != nil {
		t.Fatalf("EmitEvent #2: %v", err)
	}
	if _, err := eng.EmitEvent(context.Background(), EmitEventInput{
		RunID:          run,
		HouseholdID:    1,
		EventType:      model.AuditEventExecutionCompleted,
		Action:         "svc.restart",
		Result:         "SUCCESS",
		FinalRunStatus: model.AuditRunStatusCompleted,
	}); err != nil {
		t.Fatalf("EmitEvent #3: %v", err)
	}
	if _, err := eng.CreateCheckpoint(model.AuditPrimaryStreamID); err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	if rep := eng.SyncRemote(context.Background(), model.AuditPrimaryStreamID); rep.Status != RemoteStatusInSync {
		t.Fatalf("SyncRemote #2 expected IN_SYNC, got: %+v", rep)
	}

	// Fault 1: Remote outage / disconnect.
	mockRemote.failRequests.Store(true)
	vOutage := eng.VerifyChain(VerifyRequest{
		VerifyCheckpoints: true,
		VerifyTriggers:    true,
		VerifyRemote:      boolPtrR13(true),
	})
	if vOutage.RemoteSyncStatus != RemoteStatusUnreachable || vOutage.TrustBoundaries.RemoteReachable || vOutage.IntegrityStatus == IntegrityVerifiedAgainstRemote {
		t.Fatalf("VerifyChain during outage must expose REMOTE_UNREACHABLE and not claim remote verification: %+v", vOutage)
	}
	runOutage := eng.VerifyRunChain(run, 1)
	if runOutage.RemoteSyncStatus != RemoteStatusUnreachable || runOutage.TrustBoundaries.RemoteReachable || runOutage.IntegrityStatus == IntegrityVerifiedAgainstRemote {
		t.Fatalf("VerifyRunChain during outage must expose REMOTE_UNREACHABLE: %+v", runOutage)
	}
	if _, strictErr := eng.SyncRemoteStrict(context.Background(), model.AuditPrimaryStreamID); !errors.Is(strictErr, ErrAuditSyncFailed) {
		t.Fatalf("SyncRemoteStrict during outage must fail with ErrAuditSyncFailed, got: %v", strictErr)
	}
	mockRemote.failRequests.Store(false)

	// Fault 2: Tail truncation (drop append-only delete trigger and delete seq 3 from local DB).
	if errDrop := db.Exec("DROP TRIGGER IF EXISTS trg_agent_audit_events_no_delete").Error; errDrop != nil {
		t.Fatalf("drop trigger: %v", errDrop)
	}
	if errDel := db.Exec("DELETE FROM agent_audit_events WHERE seq = 3").Error; errDel != nil {
		t.Fatalf("delete seq 3: %v", errDel)
	}
	diffTrunc := eng.CompareWithRemote(context.Background(), model.AuditPrimaryStreamID)
	if diffTrunc.Status != RemoteStatusTailTruncated {
		t.Fatalf("expected TAIL_TRUNCATED after tail deletion relative to remote tail #3, got: %+v", diffTrunc)
	}
	runTrunc := eng.VerifyRunChain(run, 1)
	if runTrunc.Valid {
		t.Fatalf("VerifyRunChain MUST fail after tail truncation, got Valid=true: %+v", runTrunc)
	}

	// Fault 3: Whole-DB snapshot rollback + forked rewrite (restore snapPath at seq 1, write a competing seq 2 while remote holds original seq 2..3).
	snapDB := openAuditTestDBAtPath(t, snapPath)
	engSnap, err := NewStrictAgentAuditEngine(
		snapDB, nil,
		WithArchiveDir(t.TempDir()),
		WithRemoteStore(httpStore),
		WithExternalSigner(signer),
		WithStrictRemote(false),
	)
	if err != nil {
		t.Fatalf("engSnap: %v", err)
	}
	vSnap := engSnap.VerifyChain(VerifyRequest{
		VerifyCheckpoints: true,
		VerifyTriggers:    true,
		VerifyRemote:      boolPtrR13(true),
	})
	if vSnap.Valid || vSnap.IntegrityStatus != IntegrityTruncatedTail {
		t.Fatalf("expected TAIL_TRUNCATED on rolled-back prefix snapshot DB, got: %+v", vSnap)
	}
	if _, err := engSnap.EmitEvent(context.Background(), EmitEventInput{
		RunID:       run,
		HouseholdID: 1,
		EventType:   model.AuditEventExecutionFailed,
		Action:      "svc.restart",
		Result:      "FORKED",
	}); err != nil {
		t.Fatalf("EmitEvent forked seq 2 on rolled-back DB: %v", err)
	}
	runSnapFork := engSnap.VerifyRunChain(run, 1)
	if runSnapFork.Valid || runSnapFork.IntegrityStatus != RemoteStatusDBRollback {
		t.Fatalf("VerifyRunChain expected DB_ROLLBACK_DETECTED after snapshot rollback + rewrite, got: %+v", runSnapFork)
	}
}

func TestR13_PrivateKeyAndTokenLeakagePrevention(t *testing.T) {
	hmacSecret := "r13-canary-hmac-secret-must-never-leak-987654"
	bearerSecret := "r13-canary-remote-bearer-token-must-never-leak-555"

	cleanupRing := SetAuditHMACKeyringForTest("k-canary", map[string]string{
		"k-canary": hmacSecret,
	})
	defer cleanupRing()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	privHex := hex.EncodeToString(priv.Seed())
	signer, err := NewEd25519CheckpointSigner("ed-canary", priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	mockRemote, srv := newR13MockRemoteServer(t, bearerSecret)
	mockRemote.failRequests.Store(true)
	httpStore, err := NewHTTPRemoteAuditStore(srv.URL+"/api/v1/audit-replica", bearerSecret, 3*time.Second)
	if err != nil {
		t.Fatalf("NewHTTPRemoteAuditStore: %v", err)
	}

	db := openAuditTestDBAtPath(t, filepath.Join(t.TempDir(), "leak.db"))
	eng, err := NewStrictAgentAuditEngine(
		db, nil,
		WithArchiveDir(t.TempDir()),
		WithRemoteStore(httpStore),
		WithExternalSigner(signer),
		WithStrictRemote(false),
	)
	if err != nil {
		t.Fatalf("NewStrictAgentAuditEngine: %v", err)
	}
	_, _, err = eng.StartRun(context.Background(), StartRunInput{
		RunID:         "run-leak-check",
		CorrelationID: "corr-leak",
		HouseholdID:   1,
		TriggerSource: "api_manual",
		ActorType:     "user",
		ActorID:       "admin-1",
		Action:        "test.leak",
		Target:        "svc-a",
		Component:     "test",
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	syncRep := eng.SyncRemote(context.Background(), model.AuditPrimaryStreamID)
	_, syncErr := eng.SyncRemoteStrict(context.Background(), model.AuditPrimaryStreamID)
	ks := eng.GetKeyStatus()
	diff := eng.CompareWithRemote(context.Background(), model.AuditPrimaryStreamID)
	vRes := eng.VerifyChain(VerifyRequest{
		VerifyCheckpoints: true,
		VerifyTriggers:    true,
		VerifyRemote:      boolPtrR13(true),
	})
	syncState, ssErr := eng.GetRemoteSyncState(model.AuditPrimaryStreamID)
	if ssErr != nil {
		t.Fatalf("GetRemoteSyncState: %v", ssErr)
	}

	syncRepJSON, _ := json.Marshal(syncRep)
	ksJSON, _ := json.Marshal(ks)
	diffJSON, _ := json.Marshal(diff)
	vResJSON, _ := json.Marshal(vRes)
	ssJSON, _ := json.Marshal(syncState)

	haystack := strings.Join([]string{
		func() string {
			if syncErr != nil {
				return syncErr.Error()
			}
			return ""
		}(),
		string(syncRepJSON),
		string(ksJSON),
		string(diffJSON),
		string(vResJSON),
		string(ssJSON),
	}, "\n")

	for _, secret := range []string{hmacSecret, privHex, bearerSecret} {
		if strings.Contains(haystack, secret) {
			t.Fatalf("secret leaked in audit diagnostics output: matched %q", secret[:12]+"...")
		}
	}
	if ks.ExternalPublicKey == "" || strings.Contains(ks.ExternalPublicKey, privHex[:16]) {
		t.Fatalf("unexpected ExternalPublicKey value: %q", ks.ExternalPublicKey)
	}
}

func TestR13_DecisionBaselineAndRollbackLifecycleCapture(t *testing.T) {
	cleanupRing := SetAuditHMACKeyringForTest("k1", map[string]string{
		"k1": "r13-lifecycle-hmac-secret-at-least-32-bytes",
	})
	defer cleanupRing()

	db := openAuditTestDBAtPath(t, filepath.Join(t.TempDir(), "lifecycle.db"))
	eng, err := NewStrictAgentAuditEngine(
		db, nil,
		WithArchiveDir(t.TempDir()),
		WithCheckpointInterval(100),
		WithStrictRemote(false),
	)
	if err != nil {
		t.Fatalf("NewStrictAgentAuditEngine: %v", err)
	}

	run, _, err := eng.StartRun(context.Background(), StartRunInput{
		RunID:             "run-r13-lifecycle",
		CorrelationID:     "corr-r13-lifecycle",
		HouseholdID:       1,
		TriggerSource:     "safety_fsm",
		ActorType:         "fsm",
		ActorID:           "safety_fsm",
		Action:            "hostsec.apply",
		Target:            "sshd_config",
		Component:         "hostsec",
		Decision:          model.AuditDecisionAllow,
		RiskLevel:         model.AuditRiskHigh,
		ProposalID:        "prop-13",
		ApprovalID:        "appr-13",
		OperationID:       "op-13",
		FencingGeneration: 7,
		InitialEvent: &EmitEventInput{
			EventType:          model.AuditEventPolicyEvaluated,
			Action:             "hostsec.decision",
			Decision:           model.AuditDecisionAllow,
			DecisionReason:     "security baseline drift remediation approved",
			BaselineBeforeHash: "sha256:before-aaa111",
		},
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	if _, err := eng.EmitEvent(context.Background(), EmitEventInput{
		RunID:       run,
		HouseholdID: 1,
		EventType:   model.AuditEventRollbackStarted,
		Action:      "rollback.hostsec",
		Target:      "sshd_config",
		Component:   "hostsec",
		Result:      "ROLLBACK_STARTED",
	}); err != nil {
		t.Fatalf("EmitEvent RollbackStarted: %v", err)
	}

	rbFailedEv, err := eng.EmitEvent(context.Background(), EmitEventInput{
		RunID:             run,
		HouseholdID:       1,
		EventType:         model.AuditEventRollbackFailed,
		Action:            "rollback.hostsec",
		Target:            "sshd_config",
		Component:         "hostsec",
		DecisionReason:    "health check failed after apply",
		BaselineAfterHash: "sha256:after-bbb222",
		RollbackResult: map[string]any{
			"restored": false,
			"reason":   "sshd_healthcheck_timeout",
			"token":    "should-be-redacted-secret",
		},
		Result:       "ROLLBACK_FAILED",
		ErrorCode:    "ROLLBACK_HEALTH_TIMEOUT",
		MirrorLegacy: true,
	})
	if err != nil {
		t.Fatalf("EmitEvent RollbackFailed: %v", err)
	}

	detail, err := eng.GetRunDetail(run, 1)
	if err != nil {
		t.Fatalf("GetRunDetail: %v", err)
	}
	if detail.Run.Status != model.AuditRunStatusRollbackFailed {
		t.Fatalf("expected run status %q, got %q", model.AuditRunStatusRollbackFailed, detail.Run.Status)
	}
	if len(detail.Events) != 3 {
		t.Fatalf("expected 3 lifecycle events, got %d", len(detail.Events))
	}

	ev1 := detail.Events[0]
	if !strings.Contains(ev1.ParametersRedacted, "security baseline drift remediation approved") {
		t.Fatalf("expected decision_reason in event 1 ParametersRedacted, got: %s", ev1.ParametersRedacted)
	}
	if !strings.Contains(ev1.BeforeState, "sha256:before-aaa111") {
		t.Fatalf("expected baseline_before_hash in event 1 BeforeState, got: %s", ev1.BeforeState)
	}

	if !strings.Contains(rbFailedEv.AfterState, "sha256:after-bbb222") {
		t.Fatalf("expected baseline_after_hash in rollback event AfterState, got: %s", rbFailedEv.AfterState)
	}
	if !strings.Contains(rbFailedEv.Evidence, "sshd_healthcheck_timeout") || strings.Contains(rbFailedEv.Evidence, "should-be-redacted-secret") {
		t.Fatalf("unexpected rollback Evidence redaction/capture: %s", rbFailedEv.Evidence)
	}

	var legacyRow model.StructuredOpsAuditLog
	if err := db.Where("event_uuid = ?", rbFailedEv.AuditID).First(&legacyRow).Error; err != nil {
		t.Fatalf("query legacy mirror: %v", err)
	}
	if legacyRow.RollbackResult == "" || !strings.Contains(legacyRow.RollbackResult, "sshd_healthcheck_timeout") || strings.Contains(legacyRow.RollbackResult, "should-be-redacted-secret") {
		t.Fatalf("expected legacy structured_ops_audit_logs.rollback_result populated and redacted, got: %q", legacyRow.RollbackResult)
	}

	vRes := eng.VerifyChain(VerifyRequest{
		VerifyCheckpoints: true,
		VerifyTriggers:    true,
		VerifyArchive:     true,
	})
	if !vRes.Valid || !detail.Integrity.Valid {
		t.Fatalf("expected valid chain/run integrity, got vRes=%+v runIntegrity=%+v", vRes, detail.Integrity)
	}
}
