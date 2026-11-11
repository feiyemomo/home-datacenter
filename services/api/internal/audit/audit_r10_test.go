package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/redact"
)

func openAuditTestDBAtPath(t *testing.T, dbPath string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite at %s: %v", dbPath, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func copyFileForRollbackTest(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open src %s: %v", src, err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create dst %s: %v", dst, err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatalf("copy %s -> %s: %v", src, dst, err)
	}
}

func TestRemoteFileWORM_Sync_TailTruncation_And_DBRollback(t *testing.T) {
	cleanupRing := SetAuditHMACKeyringForTest("k1", map[string]string{
		"k1": "r10-worm-hmac-secret-key-at-least-32-bytes-01",
	})
	defer cleanupRing()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	signer, err := NewEd25519CheckpointSigner("ed-k1", priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	verifier, err := NewEd25519CheckpointVerifier("ed-k1", priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "audit.db")
	snapPath := filepath.Join(tmpDir, "audit-snap-seq2.db")
	emptyPath := filepath.Join(tmpDir, "audit-empty.db")
	forkPath := filepath.Join(tmpDir, "audit-fork.db")
	wormDir := filepath.Join(tmpDir, "remote-worm")
	archiveDir := filepath.Join(tmpDir, "archive")

	wormStore, err := NewFileWORMRemoteStore(wormDir)
	if err != nil {
		t.Fatalf("NewFileWORMRemoteStore: %v", err)
	}

	db := openAuditTestDBAtPath(t, dbPath)
	ae := NewAgentAuditEngine(db, nil,
		WithArchiveDir(archiveDir),
		WithRemoteStore(wormStore),
		WithExternalSigner(signer),
		WithExternalVerifier(verifier),
	)
	defer ae.Close()

	// Step 1: emit 2 events and take a snapshot of the DB file before emitting events 3..4.
	for i := 1; i <= 2; i++ {
		if _, err := ae.EmitEvent(context.Background(), EmitEventInput{
			HouseholdID:   1,
			TriggerSource: "proposal_exec",
			ActorType:     "user",
			ActorID:       "admin-1",
			EventType:     model.AuditEventExecutionCompleted,
			Action:        "container.restart",
			Target:        "home-web",
			Component:     "home-web",
			OperationID:   "op-worm-1",
			Result:        "SUCCESS",
		}); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}

	// Close SQLite handles cleanly before copying snapshots on Windows.
	sqlDB, _ := db.DB()
	_ = sqlDB.Close()
	copyFileForRollbackTest(t, dbPath, snapPath)
	copyFileForRollbackTest(t, dbPath, forkPath)

	// Reopen primary DB and emit events 3..4 + checkpoint.
	db = openAuditTestDBAtPath(t, dbPath)
	ae = NewAgentAuditEngine(db, nil,
		WithArchiveDir(archiveDir),
		WithRemoteStore(wormStore),
		WithExternalSigner(signer),
		WithExternalVerifier(verifier),
	)
	defer ae.Close()

	for i := 3; i <= 4; i++ {
		if _, err := ae.EmitEvent(context.Background(), EmitEventInput{
			HouseholdID:   1,
			TriggerSource: "proposal_exec",
			ActorType:     "user",
			ActorID:       "admin-1",
			EventType:     model.AuditEventExecutionCompleted,
			Action:        "container.restart",
			Target:        "home-web",
			Component:     "home-web",
			OperationID:   "op-worm-2",
			Result:        "SUCCESS",
		}); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}
	cp, err := ae.CreateCheckpoint(model.AuditPrimaryStreamID)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	if cp.ExternalSigAlg != "ed25519" || cp.ExternalKeyID != "ed-k1" || cp.ExternalSig == "" {
		t.Fatalf("expected ed25519 external signature on checkpoint, got %+v", cp)
	}

	// Full chain + remote + external Ed25519 verify passes cleanly.
	vr := ae.VerifyChain(VerifyRequest{VerifyCheckpoints: true, VerifyTriggers: true})
	if !vr.Valid || vr.IntegrityStatus != IntegrityVerifiedAgainstRemote || !vr.ExternalSigVerified {
		t.Fatalf("expected VERIFIED_AGAINST_REMOTE with ExternalSigVerified=true, got %+v", vr)
	}
	if vr.RemoteTailSeq != 4 || vr.TailSeq != 4 {
		t.Fatalf("expected tail_seq=4 locally and remotely, got local=%d remote=%d", vr.TailSeq, vr.RemoteTailSeq)
	}

	// Step 2: Simulate fnOS tail truncation (DB restored to snapshot at seq 2 against populated WORM at seq 4).
	snapDB := openAuditTestDBAtPath(t, snapPath)
	snapEngine := NewAgentAuditEngine(snapDB, nil,
		WithArchiveDir(t.TempDir()),
		WithRemoteStore(wormStore),
		WithExternalVerifier(verifier),
	)
	defer snapEngine.Close()

	diffTrunc := snapEngine.CompareWithRemote(context.Background(), model.AuditPrimaryStreamID)
	if diffTrunc.Status != RemoteStatusTailTruncated {
		t.Fatalf("expected TAIL_TRUNCATED on seq-2 DB snapshot, got %+v", diffTrunc)
	}
	vrTrunc := snapEngine.VerifyChain(VerifyRequest{VerifyCheckpoints: true, VerifyTriggers: true})
	if vrTrunc.Valid || vrTrunc.IntegrityStatus != IntegrityTruncatedTail {
		t.Fatalf("expected VerifyChain to detect TRUNCATED_TAIL, got %+v", vrTrunc)
	}

	// Step 3: Simulate whole-DB rollback to an empty DB against populated WORM.
	emptyDB := openAuditTestDBAtPath(t, emptyPath)
	emptyEngine := NewAgentAuditEngine(emptyDB, nil,
		WithArchiveDir(t.TempDir()),
		WithRemoteStore(wormStore),
		WithExternalVerifier(verifier),
	)
	defer emptyEngine.Close()

	vrEmpty := emptyEngine.VerifyChain(VerifyRequest{VerifyCheckpoints: true, VerifyTriggers: true})
	if vrEmpty.Valid || vrEmpty.IntegrityStatus != IntegrityDBRollbackDetected {
		t.Fatalf("expected VerifyChain to detect DB_ROLLBACK_DETECTED on empty DB, got %+v", vrEmpty)
	}

	// Step 4: Simulate rollback + conflicting fork at seq 3 (attacker rewrites seq 3 locally).
	forkDB := openAuditTestDBAtPath(t, forkPath)
	forkEngine := NewAgentAuditEngine(forkDB, nil,
		WithArchiveDir(t.TempDir()),
		WithRemoteStore(wormStore),
		WithExternalVerifier(verifier),
	)
	defer forkEngine.Close()

	// Note: emitting on forked DB triggers SyncRemote which must refuse to overwrite WORM and record conflict.
	if _, err := forkEngine.EmitEvent(context.Background(), EmitEventInput{
		HouseholdID:   1,
		TriggerSource: "api_manual",
		ActorType:     "user",
		ActorID:       "attacker",
		EventType:     model.AuditEventExecutionCompleted,
		Action:        "container.stop",
		Target:        "evil-rewrite",
		Component:     "home-web",
		Result:        "SUCCESS",
	}); err != nil {
		t.Fatalf("fork emit: %v", err)
	}
	vrFork := forkEngine.VerifyChain(VerifyRequest{VerifyCheckpoints: true, VerifyTriggers: true})
	if vrFork.Valid || (vrFork.IntegrityStatus != IntegrityDBRollbackDetected && vrFork.IntegrityStatus != IntegrityChainDiverged) {
		t.Fatalf("expected DB_ROLLBACK_DETECTED or CHAIN_DIVERGED on forked DB, got %+v", vrFork)
	}
}

func TestHTTPRemoteAuditStore_Outage_Catchup_And_SecretRedaction(t *testing.T) {
	cleanupRing := SetAuditHMACKeyringForTest("k1", map[string]string{
		"k1": "r10-http-hmac-secret-key-at-least-32-bytes-01",
	})
	defer cleanupRing()

	bearerToken := "remote-bearer-secret-token-xyz-99887766"

	var mu sync.Mutex
	outage := true
	var remoteEvents []model.AgentAuditEvent
	var remoteCPs []model.AgentAuditCheckpoint

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+bearerToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if outage {
			http.Error(w, "upstream outage with token "+bearerToken, http.StatusServiceUnavailable)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/events":
			var body struct {
				StreamID string                  `json:"stream_id"`
				Events   []model.AgentAuditEvent `json:"events"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, ev := range body.Events {
				found := false
				for _, old := range remoteEvents {
					if old.Seq == ev.Seq {
						if old.EntryHash != ev.EntryHash {
							http.Error(w, "conflict", http.StatusConflict)
							return
						}
						found = true
					}
				}
				if !found {
					remoteEvents = append(remoteEvents, ev)
				}
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/checkpoints":
			var cp model.AgentAuditCheckpoint
			_ = json.NewDecoder(r.Body).Decode(&cp)
			remoteCPs = append(remoteCPs, cp)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/tail":
			tail := RemoteTailState{StreamID: model.AuditPrimaryStreamID, EventCount: int64(len(remoteEvents))}
			if len(remoteEvents) > 0 {
				last := remoteEvents[len(remoteEvents)-1]
				tail.TailSeq = last.Seq
				tail.TailEntryHash = last.EntryHash
			}
			if len(remoteCPs) > 0 {
				lastCP := remoteCPs[len(remoteCPs)-1]
				tail.LastCheckpointID = lastCP.CheckpointID
				tail.LastCheckpointSeq = lastCP.ToSeq
				tail.LastChainDigest = lastCP.ChainDigest
			}
			_ = json.NewEncoder(w).Encode(tail)
		case r.Method == http.MethodGet && r.URL.Path == "/checkpoints":
			_ = json.NewEncoder(w).Encode(remoteCPs)
		case r.Method == http.MethodGet && r.URL.Path == "/events":
			fromQ, _ := strconv.ParseUint(r.URL.Query().Get("from_seq"), 10, 64)
			toQ, _ := strconv.ParseUint(r.URL.Query().Get("to_seq"), 10, 64)
			var filtered []model.AgentAuditEvent
			for _, ev := range remoteEvents {
				if (fromQ == 0 || ev.Seq >= fromQ) && (toQ == 0 || ev.Seq <= toQ) {
					filtered = append(filtered, ev)
				}
			}
			_ = json.NewEncoder(w).Encode(filtered)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	httpStore, err := NewHTTPRemoteAuditStore(srv.URL, bearerToken, 2*time.Second)
	if err != nil {
		t.Fatalf("NewHTTPRemoteAuditStore: %v", err)
	}

	db := openAuditTestDBAtPath(t, filepath.Join(t.TempDir(), "http-audit.db"))
	ae := NewAgentAuditEngine(db, nil, WithRemoteStore(httpStore), WithArchiveDir(t.TempDir()))
	defer ae.Close()

	// Emit 2 events during remote outage: local DB commits, sync state records unreachable + redacted error.
	for i := 1; i <= 2; i++ {
		if _, err := ae.EmitEvent(context.Background(), EmitEventInput{
			HouseholdID:   1,
			TriggerSource: "docker_fsm",
			ActorType:     "system",
			ActorID:       "fsm",
			EventType:     model.AuditEventExecutionCompleted,
			Action:        "container.restart",
			Target:        "home-web",
			Component:     "home-web",
			Result:        "SUCCESS",
		}); err != nil {
			t.Fatalf("emit %d during outage: %v", i, err)
		}
	}

	syncState, err := ae.GetRemoteSyncState(model.AuditPrimaryStreamID)
	if err != nil {
		t.Fatalf("GetRemoteSyncState: %v", err)
	}
	if syncState.SyncStatus != RemoteStatusUnreachable || syncState.ConsecutiveFailures == 0 {
		t.Fatalf("expected REMOTE_UNREACHABLE with consecutive failures, got %+v", syncState)
	}
	if strings.Contains(syncState.LastError, bearerToken) {
		t.Fatalf("remote bearer token leaked in syncState.LastError: %s", syncState.LastError)
	}

	// Recover remote store and sync catch-up.
	mu.Lock()
	outage = false
	mu.Unlock()

	diff := ae.SyncRemote(context.Background(), model.AuditPrimaryStreamID)
	if diff.Status != RemoteStatusInSync || diff.RemoteTailSeq != 2 || diff.PendingEvents != 0 {
		t.Fatalf("expected IN_SYNC with remote_tail_seq=2 after catchup, got %+v", diff)
	}
}

func TestExternalEd25519Checkpoint_TamperDetection(t *testing.T) {
	cleanupRing := SetAuditHMACKeyringForTest("k1", map[string]string{
		"k1": "r10-ed25519-hmac-key-at-least-32-bytes-01",
	})
	defer cleanupRing()

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := NewEd25519CheckpointSigner("ed-k1", priv)
	verifier, _ := NewEd25519CheckpointVerifier("ed-k1", priv.Public().(ed25519.PublicKey))

	wormStore, _ := NewFileWORMRemoteStore(t.TempDir())
	db := openAuditTestDBAtPath(t, filepath.Join(t.TempDir(), "ed-tamper.db"))
	ae := NewAgentAuditEngine(db, nil,
		WithArchiveDir(t.TempDir()),
		WithRemoteStore(wormStore),
		WithExternalSigner(signer),
		WithExternalVerifier(verifier),
	)
	defer ae.Close()

	if _, err := ae.EmitEvent(context.Background(), EmitEventInput{
		HouseholdID:   1,
		TriggerSource: "api_manual",
		ActorType:     "user",
		ActorID:       "admin-1",
		EventType:     model.AuditEventExecutionCompleted,
		Action:        "container.restart",
		Target:        "home-web",
		Component:     "home-web",
		Result:        "SUCCESS",
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	cp, err := ae.CreateCheckpoint(model.AuditPrimaryStreamID)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := verifier.VerifyCheckpointExternal(cp); err != nil {
		t.Fatalf("expected valid external sig, got %v", err)
	}

	tampered := *cp
	tampered.TailEntryHash = strings.Repeat("a", 64)
	if err := verifier.VerifyCheckpointExternal(&tampered); err == nil {
		t.Fatalf("expected tampered checkpoint external signature to fail verification")
	}
}

func TestKeyRotation_AutoAuditEvent_KeyStatus_And_ZeroLeakage(t *testing.T) {
	sec1 := "r10-key-rotation-secret-k1-32bytes-minimum-1"
	sec2 := "r10-key-rotation-secret-k2-32bytes-minimum-2"
	redact.RegisterRuntimeSecret(sec1)
	redact.RegisterRuntimeSecret(sec2)

	cleanup1 := SetAuditHMACKeyringForTest("k1", map[string]string{"k1": sec1})
	db := openAuditTestDBAtPath(t, filepath.Join(t.TempDir(), "keyrot.db"))
	ae := NewAgentAuditEngine(db, nil, WithArchiveDir(t.TempDir()))
	defer ae.Close()

	if _, err := ae.EmitEvent(context.Background(), EmitEventInput{
		HouseholdID:   1,
		TriggerSource: "api_manual",
		ActorType:     "user",
		ActorID:       "admin-1",
		EventType:     model.AuditEventExecutionCompleted,
		Action:        "container.restart",
		Target:        "home-web",
		Component:     "home-web",
		Result:        "SUCCESS",
	}); err != nil {
		t.Fatalf("emit k1: %v", err)
	}
	cleanup1()

	cleanup2 := SetAuditHMACKeyringForTest("k2", map[string]string{"k1": sec1, "k2": sec2})
	defer cleanup2()

	ev2, err := ae.EmitEvent(context.Background(), EmitEventInput{
		HouseholdID:   1,
		TriggerSource: "api_manual",
		ActorType:     "user",
		ActorID:       "admin-1",
		EventType:     model.AuditEventExecutionCompleted,
		Action:        "container.restart",
		Target:        "home-web",
		Component:     "home-web",
		Result:        "SUCCESS",
	})
	if err != nil {
		t.Fatalf("emit k2: %v", err)
	}
	if ev2.Seq != 3 || ev2.KeyID != "k2" {
		t.Fatalf("expected seq=3 (after auto AUDIT_KEY_ROTATED at seq=2) with key_id=k2, got seq=%d key_id=%s", ev2.Seq, ev2.KeyID)
	}

	var rotEv model.AgentAuditEvent
	if err := db.Where("seq = 2").First(&rotEv).Error; err != nil {
		t.Fatalf("load rotation event: %v", err)
	}
	if rotEv.EventType != model.AuditEventAuditKeyRotated || rotEv.KeyID != "k2" {
		t.Fatalf("expected AUDIT_KEY_ROTATED at seq=2 signed by k2, got %+v", rotEv)
	}

	vr := ae.VerifyChain(VerifyRequest{VerifyCheckpoints: true, VerifyTriggers: true})
	if !vr.Valid || len(vr.KeyIDsUsed) != 2 {
		t.Fatalf("expected valid chain spanning 2 keys, got %+v", vr)
	}

	ks := ae.GetKeyStatus()
	rawKS, _ := json.Marshal(ks)
	if strings.Contains(string(rawKS), sec1) || strings.Contains(string(rawKS), sec2) {
		t.Fatalf("HMAC secret leaked in GetKeyStatus output: %s", string(rawKS))
	}
	if ks.ActiveKeyID != "k2" || ks.RotationCount < 1 || !strings.Contains(ks.SecurityBoundaryNote, "fnOS") {
		t.Fatalf("unexpected GetKeyStatus metadata: %+v", ks)
	}
}

func TestPeriodicBackgroundCheckpoint_TriggersAndStopsCleanly(t *testing.T) {
	cleanupRing := SetAuditHMACKeyringForTest("k1", map[string]string{
		"k1": "r10-periodic-checkpoint-hmac-key-32bytes-01",
	})
	defer cleanupRing()

	wormStore, _ := NewFileWORMRemoteStore(t.TempDir())
	db := openAuditTestDBAtPath(t, filepath.Join(t.TempDir(), "periodic.db"))
	ae := NewAgentAuditEngine(db, nil,
		WithArchiveDir(t.TempDir()),
		WithRemoteStore(wormStore),
		WithCheckpointInterval(1000),
		WithCheckpointPeriod(35*time.Millisecond),
	)
	defer ae.Close()

	if _, err := ae.EmitEvent(context.Background(), EmitEventInput{
		HouseholdID:   1,
		TriggerSource: "api_manual",
		ActorType:     "user",
		ActorID:       "admin-1",
		EventType:     model.AuditEventExecutionCompleted,
		Action:        "container.restart",
		Target:        "home-web",
		Component:     "home-web",
		Result:        "SUCCESS",
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var cpCount int64
	for time.Now().Before(deadline) {
		_ = db.Model(&model.AgentAuditCheckpoint{}).Count(&cpCount).Error
		if cpCount > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if cpCount == 0 {
		t.Fatalf("expected periodic background loop to create checkpoint within 2s")
	}
	ae.Close()
}
