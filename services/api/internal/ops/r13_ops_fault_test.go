package ops

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/audit"
	"home-datacenter-api/internal/hostsec"
	"home-datacenter-api/internal/model"
)

func TestR13_FI_11A_HostUpdateGrantEnforcementAndRouteMapping(t *testing.T) {
	resetTestRecoveryFSM(t)
	gin.SetMode(gin.TestMode)

	t.Setenv("OPS_APPROVAL_HMAC_KEY", "r13-ops-approval-hmac-key-at-least-32-bytes-01")
	t.Setenv("OPS_AUDIT_HMAC_KEY", "r13-ops-audit-hmac-key-at-least-32-bytes-0001")

	db := newPhase2TestDB(t)
	ae, err := audit.NewStrictAgentAuditEngine(
		db, nil,
		audit.WithArchiveDir(t.TempDir()),
		audit.WithStrictRemote(false),
	)
	if err != nil {
		t.Fatalf("NewStrictAgentAuditEngine: %v", err)
	}
	defer ae.Close()

	debRoot := writeOpsHostFixture(t, false)
	snapRoot := t.TempDir()
	bridgeKey := []byte("r13-ops-bridge-hmac-key-at-least-32-bytes-01")

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	grantSigner, err := hostsec.NewHostGrantAuthority(hostsec.HostGrantAuthorityConfig{
		KeyID:      "ops-ed-k1",
		PrivateKey: priv,
	})
	if err != nil {
		t.Fatal(err)
	}

	var failPostSSH atomic.Bool
	var failDowngrade atomic.Bool
	var upgradeCalls atomic.Int32
	runner := &stubHostExecRunner{
		handlers: map[string]func(args []string) (string, string, error){
			"apt-cache": func(_ []string) (string, string, error) {
				return "openssh-server:\n  Installed: 1:9.2p1-2+deb12u2\n  Candidate: 1:9.2p1-2+deb12u4\n", "", nil
			},
			"apt-get": func(args []string) (string, string, error) {
				joined := strings.Join(args, " ")
				if strings.Contains(joined, "--allow-downgrades") && failDowngrade.Load() {
					return "", "simulated apt downgrade failure", errors.New("downgrade exit 100")
				}
				upgradeCalls.Add(1)
				return "upgraded openssh-server", "", nil
			},
			"dpkg-query": func(_ []string) (string, string, error) {
				return "install ok installed\t1:9.2p1-2+deb12u4\n", "", nil
			},
			"sshd": func(_ []string) (string, string, error) {
				return "", "", nil
			},
		},
	}

	bh, err := hostsec.NewHostBridgeHTTPHandler(hostsec.HostBridgeServerConfig{
		HostRoot:       debRoot,
		SnapshotRoot:   snapRoot,
		KeyID:          "bridge-ops-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		RequireGrant:   true,
		GrantKeyID:     "ops-ed-k1",
		GrantPublicKey: pub,
		Runner:         runner,
	})
	if err != nil {
		t.Fatalf("NewHostBridgeHTTPHandler: %v", err)
	}
	bsrv := httptest.NewServer(bh)
	defer bsrv.Close()

	bridgeClient, err := hostsec.NewHMACHostExecutionBridge(hostsec.HostBridgeConfig{
		Endpoint:       bsrv.URL,
		KeyID:          "bridge-ops-k1",
		HMACKey:        bridgeKey,
		RequireFencing: true,
		RequireGrant:   true,
	})
	if err != nil {
		t.Fatalf("NewHMACHostExecutionBridge: %v", err)
	}

	hue := hostsec.NewHostUpdateExecutor(debRoot, snapRoot, ae)
	hue.Detector.InDocker = true
	hue.RequireFencing = true
	hue.RequireExecutionGrant = true
	hue.SetBridge(bridgeClient)
	hue.SSHRecoveryWindow = 5 * time.Second
	hue.SSHProbeFn = func(_ context.Context, _ string) error {
		if failPostSSH.Load() {
			return errors.New("simulated ssh probe refusal")
		}
		return nil
	}

	hostV1, _, err := hue.ComputeBaselineHash(context.Background(), []string{"openssh-server"})
	if err != nil {
		t.Fatalf("ComputeBaselineHash: %v", err)
	}

	exec := NewOperationExecutor(
		db, nil, nil, nil,
		WithBaselineExtractor(func(target string) string {
			if target == ComponentHostOS {
				return hostV1
			}
			return ""
		}),
	)
	exec.SetAuditEngine(ae)

	h := NewOpsHandler(nil, nil, db, nil, nil)
	h.SetExecutor(exec)
	h.SetHostUpdateExecutor(hue)

	// 1. Route POST /api/v1/ops/host/update without ExecutionGrant fails closed with HTTP 403 Forbidden.
	routeProp := &model.OpsProposal{
		ProposalID:      "prop-r13-route-nogrant",
		Title:           "Host update without grant",
		TargetComponent: ComponentHostOS,
		ExecutionPlan:   "upgrade openssh-server",
		RollbackPlan:    "restore sshd",
		Status:          model.ProposalStatusApproved,
		BaselineHash:    hostV1,
		RiskLevel:       model.RiskLevelCritical,
		ActionSpec:      `{"action":"host.pkg_upgrade","target":"host-os","params":{"packages":["openssh-server"]}}`,
	}
	seedTestSignedProposal(t, db, routeProp)

	router := gin.New()
	router.POST("/api/v1/ops/host/update", func(c *gin.Context) {
		c.Set("is_admin", true)
		c.Set("username", "admin")
		c.Set("household_id", uint(1))
		h.ExecuteHostUpdate(c)
	})
	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/ops/host/update", bytes.NewBufferString(`{"proposal_id":"prop-r13-route-nogrant"}`))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httpReq)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected HTTP 403 when HostExecutionGrant is missing on RequireExecutionGrant bridge, got %d (%s)", rec.Code, rec.Body.String())
	}
	if upgradeCalls.Load() != 0 {
		t.Fatalf("expected 0 apt-get executions when grant is missing, got %d", upgradeCalls.Load())
	}

	// 2. Full end-to-end HostUpdateExecutor execution with valid Ed25519 HostExecutionGrant + DetachedHostWatchdog + Audit VerifyRunChain.
	propOK := &model.OpsProposal{
		ProposalID:      "prop-r13-ed25519-ok",
		Title:           "Host update with Ed25519 grant",
		TargetComponent: ComponentHostOS,
		ExecutionPlan:   "upgrade openssh-server",
		RollbackPlan:    "restore sshd",
		Status:          model.ProposalStatusApproved,
		BaselineHash:    hostV1,
		RiskLevel:       model.RiskLevelCritical,
		ActionSpec:      `{"action":"host.pkg_upgrade","target":"host-os","params":{"packages":["openssh-server"]}}`,
	}
	seedTestSignedProposal(t, db, propOK)
	opOK := "op-r13-ed-ok"
	grantOK, err := grantSigner.IssueGrant(hostsec.HostExecutionGrantIssueRequest{
		ProposalID:        propOK.ProposalID,
		ApprovalID:        propOK.ApprovalDigest,
		OperationID:       opOK,
		ResourceScope:     "host-os",
		BaselineHash:      hostV1,
		FencingGeneration: 1,
		Action:            hostsec.ActionHostPackageUpgrade,
		Packages:          []string{"openssh-server"},
		TargetVersions:    map[string]string{"openssh-server": "1:9.2p1-2+deb12u4"},
	})
	if err != nil {
		t.Fatalf("IssueGrant(OK): %v", err)
	}
	resOK, err := hue.Execute(context.Background(), hostsec.HostUpdateRequest{
		Proposal:             propOK,
		ApprovalID:           propOK.ApprovalDigest,
		OperationID:          opOK,
		ResourceScope:        "host-os",
		FencingGeneration:    1,
		ExecutionGrant:       grantOK,
		Packages:             []string{"openssh-server"},
		TargetVersions:       map[string]string{"openssh-server": "1:9.2p1-2+deb12u4"},
		ExpectedBaselineHash: hostV1,
	})
	if err != nil || !resOK.Verified {
		t.Fatalf("expected verified host update with Ed25519 grant, got err=%v res=%+v", err, resOK)
	}
	var runOK model.AgentAuditRun
	if qErr := db.Where("proposal_id = ?", propOK.ProposalID).First(&runOK).Error; qErr != nil {
		t.Fatalf("query AgentAuditRun(OK): %v", qErr)
	}
	vOK := ae.VerifyRunChain(runOK.RunID, 1)
	if !vOK.Valid || !vOK.LocalOnly || !vOK.TrustBoundaries.LocalOnly {
		t.Fatalf("expected valid LOCAL_ONLY audit run integrity after host update, got %+v", vOK)
	}

	// 3. End-to-end Rollback failure emits AuditRunStatusRollbackFailed (ROLLBACK_FAILED) cleanly.
	failPostSSH.Store(true)
	failDowngrade.Store(true)
	propRB := &model.OpsProposal{
		ProposalID:      "prop-r13-rb-fail",
		Title:           "Host update rollback failure audit test",
		TargetComponent: ComponentHostOS,
		ExecutionPlan:   "upgrade openssh-server",
		RollbackPlan:    "restore sshd",
		Status:          model.ProposalStatusApproved,
		BaselineHash:    hostV1,
		RiskLevel:       model.RiskLevelCritical,
		ActionSpec:      `{"action":"host.pkg_upgrade","target":"host-os","params":{"packages":["openssh-server"]}}`,
	}
	seedTestSignedProposal(t, db, propRB)
	opRB := "op-r13-rb-fail"
	grantRB, err := grantSigner.IssueGrant(hostsec.HostExecutionGrantIssueRequest{
		ProposalID:        propRB.ProposalID,
		ApprovalID:        propRB.ApprovalDigest,
		OperationID:       opRB,
		ResourceScope:     "host-os",
		BaselineHash:      hostV1,
		FencingGeneration: 2,
		Action:            hostsec.ActionHostPackageUpgrade,
		Packages:          []string{"openssh-server"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resRB, errRB := hue.Execute(context.Background(), hostsec.HostUpdateRequest{
		Proposal:             propRB,
		ApprovalID:           propRB.ApprovalDigest,
		OperationID:          opRB,
		ResourceScope:        "host-os",
		FencingGeneration:    2,
		ExecutionGrant:       grantRB,
		Packages:             []string{"openssh-server"},
		ExpectedBaselineHash: hostV1,
	})
	if errRB == nil || resRB == nil {
		t.Fatalf("expected rollback attempted error, got err=%v res=%+v", errRB, resRB)
	}
	var runRB model.AgentAuditRun
	if qErr := db.Where("proposal_id = ?", propRB.ProposalID).First(&runRB).Error; qErr != nil {
		t.Fatalf("query AgentAuditRun(RB): %v", qErr)
	}
	detailRB, dErr := ae.GetRunDetail(runRB.RunID, 1)
	if dErr != nil {
		t.Fatalf("GetRunDetail(RB): %v", dErr)
	}
	if detailRB.Run.Status != model.AuditRunStatusRollbackFailed || !detailRB.Integrity.Valid || !detailRB.Integrity.LocalOnly {
		t.Fatalf("expected valid LOCAL_ONLY run with status ROLLBACK_FAILED, got status=%q integrity=%+v", detailRB.Run.Status, detailRB.Integrity)
	}
}

func TestR13_FI_11B_PythonOfflineSyncAndLeaseParity(t *testing.T) {
	resetTestRecoveryFSM(t)
	t.Setenv("OPS_AUDIT_HMAC_KEY", "r13-offline-parity-hmac-key-at-least-32-bytes")

	db := newPhase2TestDB(t)
	ae, err := audit.NewStrictAgentAuditEngine(
		db, nil,
		audit.WithArchiveDir(t.TempDir()),
		audit.WithStrictRemote(false),
	)
	if err != nil {
		t.Fatalf("NewStrictAgentAuditEngine: %v", err)
	}
	defer ae.Close()

	// 1. RecoveryLeaseManager enforces monotonic fencing & fails closed on wrong token or stale fencing.
	lm := NewRecoveryLeaseManager(db)
	g1, err := lm.AcquireLeaseGrant("home-web", "go-core", 10*time.Second)
	if err != nil || g1.FencingGeneration < 1 {
		t.Fatalf("AcquireLeaseGrant #1 failed: g=%+v err=%v", g1, err)
	}
	if _, err := lm.AcquireLeaseGrant("home-web", "python-offline", 10*time.Second); err == nil {
		t.Fatalf("expected python-offline competing lease acquire to fail while go-core holds active lease")
	}
	if err := lm.VerifyLeaseFencing("home-web", "wrong-token", g1.FencingGeneration, time.Second); err == nil {
		t.Fatalf("expected VerifyLeaseFencing to fail on wrong token")
	}
	if err := lm.ReleaseLease("home-web", g1.LeaseToken); err != nil {
		t.Fatalf("ReleaseLease failed: %v", err)
	}

	// 2. Python offline audit event with ROLLBACK_FAILED status syncs cleanly into Go Core LOCAL_ONLY ledger.
	now := time.Now().UTC().Truncate(time.Millisecond)
	ev := &model.AgentAuditEvent{
		StreamID:           "python-offline-r13",
		Seq:                1,
		AuditID:            "offline-r13-rb-1",
		RunID:              "run-offline-r13-rb-1",
		CorrelationID:      "corr-offline-r13-1",
		HouseholdID:        1,
		TriggerSource:      "python_offline_agent",
		ActorType:          "python_agent",
		ActorID:            "edge-daemon",
		EventType:          model.AuditEventRollbackFailed,
		Action:             "docker.rollback",
		Target:             "home-web",
		Component:          "home-web",
		Decision:           model.AuditDecisionAllow,
		RiskLevel:          model.AuditRiskHigh,
		PolicyVersion:      audit.PolicyVersion,
		OperationID:        "pyop-r13-1",
		FencingGeneration:  g1.FencingGeneration,
		BeforeState:        `{"baseline_before_hash":"v1:abc"}`,
		AfterState:         `{"baseline_after_hash":"v1:def"}`,
		ParametersDigest:   strings.Repeat("a", 64),
		ParametersRedacted: `{"decision_reason":"offline container health failed"}`,
		Result:             "FAILED",
		ErrorCode:          "ROLLBACK_ERR",
		Evidence:           `{"rollback_result":"failed"}`,
		CreatedAt:          now,
		CreatedAtMs:        now.UnixMilli(),
		IsOffline:          true,
		PreviousHash:       model.AuditGenesisHash,
	}
	entryHash, err := audit.ComputeEventEntryHash(ev)
	if err != nil {
		t.Fatalf("ComputeEventEntryHash: %v", err)
	}
	kid, ring, err := audit.ResolveAuditHMACKeyring()
	if err != nil {
		t.Fatalf("ResolveAuditHMACKeyring: %v", err)
	}
	sig := audit.ComputeEventHMAC(kid, ring[kid], ev.StreamID, ev.Seq, entryHash, ev.CreatedAtMs)

	syncRes, err := ae.IngestOfflineEvents(context.Background(), []audit.OfflineSyncEvent{
		{
			EventUUID:          ev.AuditID,
			AuditID:            ev.AuditID,
			RunID:              ev.RunID,
			CorrelationID:      ev.CorrelationID,
			HouseholdID:        ev.HouseholdID,
			OperationID:        ev.OperationID,
			PolicyVersion:      ev.PolicyVersion,
			TriggerSource:      ev.TriggerSource,
			ActorType:          ev.ActorType,
			ActorID:            ev.ActorID,
			EventType:          ev.EventType,
			ContainerName:      "home-web",
			Component:          ev.Component,
			Target:             ev.Target,
			Action:             ev.Action,
			Decision:           ev.Decision,
			RiskLevel:          ev.RiskLevel,
			Holder:             "python-offline",
			FencingGeneration:  ev.FencingGeneration,
			AuditStatus:        ev.Result,
			BeforeState:        ev.BeforeState,
			AfterState:         ev.AfterState,
			ParametersDigest:   ev.ParametersDigest,
			ParametersRedacted: ev.ParametersRedacted,
			Evidence:           ev.Evidence,
			ErrorMessage:       ev.ErrorCode,
			ExecutedAt:         audit.FormatRFC3339Ms(now),
			ExecutedAtMs:       ev.CreatedAtMs,
			OriginStreamID:     ev.StreamID,
			OfflineSeq:         ev.Seq,
			LocalPrevHash:      ev.PreviousHash,
			EntryHash:          entryHash,
			KeyID:              kid,
			HMACSignature:      sig,
		},
	}, "req-r13-sync-1", "127.0.0.1", 1)
	if err != nil || len(syncRes.AcceptedUUIDs) != 1 || len(syncRes.Rejected) != 0 {
		t.Fatalf("expected 1 accepted offline event and 0 rejections, got err=%v res=%+v", err, syncRes)
	}

	detail, err := ae.GetRunDetail(ev.RunID, 1)
	if err != nil {
		t.Fatalf("GetRunDetail(offline): %v", err)
	}
	if detail.Run.Status != model.AuditRunStatusRollbackFailed || !detail.Integrity.Valid || !detail.Integrity.LocalOnly {
		t.Fatalf("expected valid LOCAL_ONLY offline run with ROLLBACK_FAILED status, got status=%q integrity=%+v", detail.Run.Status, detail.Integrity)
	}
}
