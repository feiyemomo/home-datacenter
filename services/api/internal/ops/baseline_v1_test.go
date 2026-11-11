package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/audit"
	"home-datacenter-api/internal/model"
)

type baselineGoldenVector struct {
	Name                  string                 `json:"name"`
	Component             string                 `json:"component"`
	ExpectedValid         bool                   `json:"expected_valid"`
	Inspect               map[string]interface{} `json:"inspect"`
	ExpectedCanonicalJSON string                 `json:"expected_canonical_json,omitempty"`
	ExpectedBaselineHash  string                 `json:"expected_baseline_hash"`
	ExpectedIdentityHash  string                 `json:"expected_identity_hash,omitempty"`
	ExpectedSecurityHash  string                 `json:"expected_security_hash,omitempty"`
	ExpectedStateHash     string                 `json:"expected_state_hash,omitempty"`
}

type baselineGoldenFile struct {
	ProtocolVersion string                 `json:"protocol_version"`
	Vectors         []baselineGoldenVector `json:"vectors"`
}

func TestBaselineV1GoldenVectors(t *testing.T) {
	vecPath := filepath.Join("..", "..", "..", "..", "testdata", "baseline_golden_vectors.json")
	raw, err := os.ReadFile(vecPath)
	if err != nil {
		t.Fatalf("read %s: %v", vecPath, err)
	}
	var gf baselineGoldenFile
	if err := json.Unmarshal(raw, &gf); err != nil {
		t.Fatalf("unmarshal %s: %v", vecPath, err)
	}
	if gf.ProtocolVersion != BaselineProtocolVersion {
		t.Fatalf("expected protocol_version %s, got %s", BaselineProtocolVersion, gf.ProtocolVersion)
	}
	if len(gf.Vectors) != 11 {
		t.Fatalf("expected 11 vectors, got %d", len(gf.Vectors))
	}

	byName := make(map[string]*BaselineComputation, len(gf.Vectors))
	for _, vec := range gf.Vectors {
		t.Run(vec.Name, func(t *testing.T) {
			comp, cErr := ComputeContainerBaselineFromInspect(vec.Component, vec.Inspect)
			if !vec.ExpectedValid {
				if cErr == nil {
					t.Fatalf("expected error for invalid vector %s, got %s", vec.Name, comp.Hash)
				}
				return
			}
			if cErr != nil {
				t.Fatalf("unexpected error for %s: %v", vec.Name, cErr)
			}
			canonBytes, _ := audit.CanonicalJSONBytes(comp.Document)
			if string(canonBytes) != vec.ExpectedCanonicalJSON {
				t.Fatalf("canonical json mismatch:\ngot:  %s\nwant: %s", string(canonBytes), vec.ExpectedCanonicalJSON)
			}
			if comp.Hash != vec.ExpectedBaselineHash {
				t.Fatalf("baseline_hash mismatch: got %s, want %s", comp.Hash, vec.ExpectedBaselineHash)
			}
			if comp.IdentityHash != vec.ExpectedIdentityHash {
				t.Fatalf("identity_hash mismatch: got %s, want %s", comp.IdentityHash, vec.ExpectedIdentityHash)
			}
			if comp.SecurityHash != vec.ExpectedSecurityHash {
				t.Fatalf("security_hash mismatch: got %s, want %s", comp.SecurityHash, vec.ExpectedSecurityHash)
			}
			if comp.StateHash != vec.ExpectedStateHash {
				t.Fatalf("state_hash mismatch: got %s, want %s", comp.StateHash, vec.ExpectedStateHash)
			}
			if !IsRealBaselineHashV1(comp.Hash, vec.Component) {
				t.Fatalf("expected IsRealBaselineHashV1 true for %s", comp.Hash)
			}
			byName[vec.Name] = comp
		})
	}

	// Cross-vector invariants
	if byName["02_empty_fields"].Hash != byName["03_missing_fields"].Hash {
		t.Fatalf("empty vs missing fields must match: %s vs %s", byName["02_empty_fields"].Hash, byName["03_missing_fields"].Hash)
	}
	if byName["01_normal"].Hash != byName["05_different_array_orders"].Hash {
		t.Fatalf("different array orders must match normal: %s vs %s", byName["01_normal"].Hash, byName["05_different_array_orders"].Hash)
	}
	if byName["01_normal"].Hash == byName["07_image_digest_change"].Hash ||
		byName["01_normal"].SecurityHash != byName["07_image_digest_change"].SecurityHash {
		t.Fatalf("07_image_digest_change must change identity/full hash while keeping security_hash equal")
	}
	for _, secDrift := range []string{"08_mounts_change", "09_privileged_change", "10_capabilities_change", "11_network_config_change"} {
		if byName["01_normal"].Hash == byName[secDrift].Hash ||
			byName["01_normal"].IdentityHash != byName[secDrift].IdentityHash ||
			byName["01_normal"].SecurityHash == byName[secDrift].SecurityHash {
			t.Fatalf("%s must change security_hash and full hash while keeping identity_hash equal", secDrift)
		}
	}
}

func newPhase2TestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "phase2_ops.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test sqlite: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, dbErr := db.DB(); dbErr == nil && sqlDB != nil {
			_ = sqlDB.Close()
		}
	})
	if err := EnsureOpsSchema(db); err != nil {
		t.Fatalf("EnsureOpsSchema: %v", err)
	}
	return db
}

func startMockDockerUnixSocket(t *testing.T, containers []map[string]interface{}, inspectByName map[string]map[string]interface{}) string {
	t.Helper()
	// Windows AF_UNIX sun_path is capped at 108 bytes; avoid embedding long t.Name() via TempDir.
	dir, err := os.MkdirTemp("", "dsock-*")
	if err != nil {
		t.Fatalf("mkdir temp for docker sock: %v", err)
	}
	sockPath := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Skipf("unix socket unsupported in this environment: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(containers)
	})
	mux.HandleFunc("/containers/", func(w http.ResponseWriter, r *http.Request) {
		trimmed := strings.TrimPrefix(r.URL.Path, "/containers/")
		name := strings.TrimSuffix(trimmed, "/json")
		if payload, ok := inspectByName[name]; ok {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(payload)
			return
		}
		http.NotFound(w, r)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
		_ = os.RemoveAll(dir)
	})
	return sockPath
}

func validHomeWebDockerInspect() map[string]interface{} {
	return map[string]interface{}{
		"Id":      "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"Name":    "/home-web",
		"Created": "2026-01-15T10:30:00Z",
		"Image":   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"RepoDigests": []string{
			"ghcr.io/home-datacenter/home-web@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
		"RestartCount": 0,
		"Config": map[string]interface{}{
			"Image":      "home-web:1.0.0",
			"User":       "1000:1000",
			"Entrypoint": []string{"/entrypoint.sh"},
			"Cmd":        []string{"nginx", "-g", "daemon off;"},
			"Env":        []string{"APP_ENV=production"},
			"Labels":     map[string]interface{}{"com.home.service": "home-web"},
		},
		"HostConfig": map[string]interface{}{
			"Privileged":     false,
			"NetworkMode":    "home-net",
			"PidMode":        "",
			"IpcMode":        "",
			"UsernsMode":     "",
			"ReadonlyRootfs": true,
			"CapAdd":         []string{},
			"CapDrop":        []string{"ALL"},
			"SecurityOpt":    []string{"no-new-privileges:true"},
			"Binds":          []string{},
			"PortBindings":   map[string]interface{}{"80/tcp": []map[string]interface{}{{"HostIp": "127.0.0.1", "HostPort": "8081"}}},
			"ExtraHosts":     []string{},
			"Dns":            []string{},
			"RestartPolicy":  map[string]interface{}{"Name": "unless-stopped"},
		},
		"Mounts": []map[string]interface{}{},
		"NetworkSettings": map[string]interface{}{
			"Networks": map[string]interface{}{"home-net": map[string]interface{}{}},
		},
		"State": map[string]interface{}{
			"Status":    "running",
			"StartedAt": "2026-01-15T10:30:05Z",
		},
	}
}

func TestR8_DockerSocketAmbiguityAndNameMismatch(t *testing.T) {
	ctx := context.Background()

	// Case 1: Ambiguous container aliases mapping to canonical "openresty"
	ambigSock := startMockDockerUnixSocket(t,
		[]map[string]interface{}{
			{"Id": "111", "Names": []string{"/openresty"}},
			{"Id": "222", "Names": []string{"/1pwaf"}},
		},
		map[string]map[string]interface{}{},
	)
	if _, err := inspectContainerViaDockerSocket(ctx, ambigSock, "openresty"); !errors.Is(err, ErrBaselineAmbiguousContainer) {
		t.Fatalf("expected ErrBaselineAmbiguousContainer, got %v", err)
	}
	db := newPhase2TestDB(t)
	exec := NewOperationExecutor(db, nil, nil, nil, WithDockerSocket(ambigSock))
	if info := exec.ExtractBaseline("openresty"); info.Error == "" || info.Hash != "" {
		t.Fatalf("expected fail-closed empty hash on ambiguous container, got hash=%q err=%q", info.Hash, info.Error)
	}

	// Case 2: Single container listed, but /containers/{name}/json returns wrong Name
	wrongPayload := validHomeWebDockerInspect()
	wrongPayload["Name"] = "/rogue-sidecar"
	mismatchSock := startMockDockerUnixSocket(t,
		[]map[string]interface{}{
			{"Id": "333", "Names": []string{"/home-web"}},
		},
		map[string]map[string]interface{}{
			"home-web": wrongPayload,
		},
	)
	if _, err := inspectContainerViaDockerSocket(ctx, mismatchSock, "home-web"); err == nil || !strings.Contains(err.Error(), "name mismatch") {
		t.Fatalf("expected container name mismatch error, got %v", err)
	}
}

func TestR8_LeaseSafetyGuard5000msRejection(t *testing.T) {
	db := newPhase2TestDB(t)
	mgr := NewRecoveryLeaseManager(db)
	ctx := context.Background()

	// 4000ms remaining TTL must be rejected against DefaultLeaseSafetyGuardMs (5000ms)
	_, shortGuard, err := AcquireLeaseGuard(ctx, mgr, "home-web", "worker-1", 4*time.Second, db, nil)
	if err != nil {
		t.Fatalf("AcquireLeaseGuard(4s): %v", err)
	}
	if err := shortGuard.VerifyBeforeMutation(time.Duration(DefaultLeaseSafetyGuardMs) * time.Millisecond); !errors.Is(err, ErrFencingTokenStale) {
		_ = shortGuard.Release(db, nil)
		t.Fatalf("expected ErrFencingTokenStale when TTL < %dms, got %v", DefaultLeaseSafetyGuardMs, err)
	}
	if err := shortGuard.Release(db, nil); err != nil {
		t.Fatalf("release shortGuard: %v", err)
	}

	// 30s TTL must pass the 5000ms safety guard
	_, longGuard, err := AcquireLeaseGuard(ctx, mgr, "home-web", "worker-1", 30*time.Second, db, nil)
	if err != nil {
		t.Fatalf("AcquireLeaseGuard(30s): %v", err)
	}
	defer func() { _ = longGuard.Release(db, nil) }()
	if err := longGuard.VerifyBeforeMutation(time.Duration(DefaultLeaseSafetyGuardMs) * time.Millisecond); err != nil {
		t.Fatalf("expected 30s lease to satisfy %dms guard, got %v", DefaultLeaseSafetyGuardMs, err)
	}
}

func TestR5_HandleContainerEventReturnsBaselineHashAndProtocolVersion(t *testing.T) {
	prevFSM := GetGlobalRecoveryFSM()
	SetGlobalRecoveryFSM(nil)
	t.Cleanup(func() { SetGlobalRecoveryFSM(prevFSM) })

	gin.SetMode(gin.TestMode)
	db := newPhase2TestDB(t)
	sockPath := startMockDockerUnixSocket(t,
		[]map[string]interface{}{
			{"Id": "abc", "Names": []string{"/home-web"}},
		},
		map[string]map[string]interface{}{
			"home-web": validHomeWebDockerInspect(),
		},
	)

	h := NewOpsHandler(nil, nil, db, nil, nil)
	ae := audit.NewAgentAuditEngine(db, nil, audit.WithArchiveDir(t.TempDir()))
	exec := NewOperationExecutor(
		db, nil, nil, nil,
		WithDockerSocket(sockPath),
		WithAuditEngine(ae),
		WithDockerRunner(func(ctx context.Context, action string, container string, params map[string]interface{}) (string, error) {
			return "home-web", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
	)
	h.SetExecutor(exec)
	h.SetAuditEngine(ae)
	defer func() { _ = h.GetRecoveryFSM().GetCircuitBreaker() }()

	router := gin.New()
	router.POST("/api/v1/ops/fsm/event", func(c *gin.Context) {
		c.Set("is_admin", true)
		h.HandleContainerEvent(c)
	})

	body := `{"container_name":"home-web","action":"die","exit_code":1}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ops/fsm/event", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	bh, _ := resp["baseline_hash"].(string)
	proto, _ := resp["baseline_protocol_version"].(string)
	if !IsRealBaselineHashV1(bh, "home-web") {
		t.Fatalf("expected valid v1 baseline_hash in /fsm/event response, got %q (body=%s)", bh, rec.Body.String())
	}
	if proto != BaselineProtocolVersion {
		t.Fatalf("expected baseline_protocol_version=%q, got %q", BaselineProtocolVersion, proto)
	}
}

func TestR7_SafetyFSM_MidExecHealthCheckAuditFailureEscalatesExecutionUnknown(t *testing.T) {
	db := newPhase2TestDB(t)
	probeCalled := false
	probeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer probeSrv.Close()

	ae := audit.NewAgentAuditEngine(
		db, nil,
		audit.WithArchiveDir(t.TempDir()),
		audit.WithFaultHooks(func(in audit.EmitEventInput) error {
			if in.EventType == model.AuditEventHealthCheckStarted {
				return errors.New("injected audit fault on health check")
			}
			return nil
		}, nil),
	)
	sfsm := NewSafetyFSM(db, nil, nil, t.TempDir(), probeSrv.URL)
	sfsm.SetAuditEngine(ae)

	applied := false
	err := sfsm.ExecuteSafeUpdate(context.Background(), "home-api", "1.0.0", "1.0.1",
		func() error {
			applied = true
			return nil
		},
		nil,
	)
	if !applied {
		t.Fatalf("expected applyFn to have run prior to health check")
	}
	if !errors.Is(err, ErrExecutionAmbiguous) {
		t.Fatalf("expected ErrExecutionAmbiguous when HEALTH_CHECK_STARTED audit fails, got %v", err)
	}
	if probeCalled {
		t.Fatalf("expected health probe to halt immediately when HEALTH_CHECK_STARTED audit failed")
	}

	var runs []model.AgentAuditRun
	if qErr := db.Where("trigger_source = ?", "safety_fsm").Find(&runs).Error; qErr != nil || len(runs) != 1 {
		t.Fatalf("expected 1 safety_fsm audit run, got %d err=%v", len(runs), qErr)
	}
	if runs[0].Status != model.AuditRunStatusExecutionUnknown {
		t.Fatalf("expected audit run status %s, got %s", model.AuditRunStatusExecutionUnknown, runs[0].Status)
	}
}

func TestR7_ProposalFSM_AuditFailureRevertAndEscalate(t *testing.T) {
	db := newPhase2TestDB(t)

	// Subtest 1: Pre-execution transition failure (PENDING -> APPROVED) must revert
	aePre := audit.NewAgentAuditEngine(
		db, nil,
		audit.WithArchiveDir(t.TempDir()),
		audit.WithFaultHooks(func(in audit.EmitEventInput) error {
			if in.EventType == model.AuditEventApprovalGranted {
				return errors.New("injected audit fault on approval")
			}
			return nil
		}, nil),
	)
	pfsmPre := NewProposalFSM(db)
	pfsmPre.SetAuditEngine(aePre)

	seedTestSignedProposal(t, db, &model.OpsProposal{
		ProposalID:      "prop-r7-pre",
		Title:           "Restart home-web",
		TargetComponent: "home-web",
		ExecutionPlan:   "restart home-web",
		RollbackPlan:    "none",
		Status:          model.ProposalStatusPending,
	})
	won, err := pfsmPre.TransitionStrict(ProposalTransitionParams{
		ProposalID:       "prop-r7-pre",
		ExpectedStatuses: []string{model.ProposalStatusPending},
		TargetStatus:     model.ProposalStatusApproved,
		Updates:          map[string]interface{}{"approved_by": "admin"},
	})
	if won || err == nil {
		t.Fatalf("expected transition revert on pre-exec audit failure, got won=%v err=%v", won, err)
	}
	var gotPre model.OpsProposal
	if qErr := db.Where("proposal_id = ?", "prop-r7-pre").First(&gotPre).Error; qErr != nil {
		t.Fatalf("query pre proposal: %v", qErr)
	}
	if gotPre.Status != model.ProposalStatusPending {
		t.Fatalf("expected reverted status PENDING, got %s", gotPre.Status)
	}

	// Subtest 2: Post-execution transition failure (EXECUTING -> EXECUTED) must escalate to EXECUTION_UNKNOWN
	aePost := audit.NewAgentAuditEngine(
		db, nil,
		audit.WithArchiveDir(t.TempDir()),
		audit.WithFaultHooks(func(in audit.EmitEventInput) error {
			if in.EventType == model.AuditEventExecutionCompleted {
				return errors.New("injected audit fault post-exec")
			}
			return nil
		}, nil),
	)
	pfsmPost := NewProposalFSM(db)
	pfsmPost.SetAuditEngine(aePost)

	seedTestSignedProposal(t, db, &model.OpsProposal{
		ProposalID:      "prop-r7-post",
		Title:           "Restart home-web post",
		TargetComponent: "home-web",
		ExecutionPlan:   "restart home-web",
		RollbackPlan:    "none",
		Status:          model.ProposalStatusExecuting,
		ExecutionToken:  "tok-1",
	})
	wonPost, errPost := pfsmPost.TransitionStrict(ProposalTransitionParams{
		ProposalID:            "prop-r7-post",
		ExpectedStatuses:      []string{model.ProposalStatusExecuting},
		TargetStatus:          model.ProposalStatusExecuted,
		RequireExecutionToken: "tok-1",
	})
	if wonPost || !errors.Is(errPost, ErrExecutionAmbiguous) {
		t.Fatalf("expected ErrExecutionAmbiguous on post-exec audit failure, got won=%v err=%v", wonPost, errPost)
	}
	var gotPost model.OpsProposal
	if qErr := db.Where("proposal_id = ?", "prop-r7-post").First(&gotPost).Error; qErr != nil {
		t.Fatalf("query post proposal: %v", qErr)
	}
	if gotPost.Status != model.ProposalStatusExecutionUnknown {
		t.Fatalf("expected escalated status EXECUTION_UNKNOWN, got %s", gotPost.Status)
	}
}

func TestR7_RoutesPreExecAuditBlocksMutation(t *testing.T) {
	prevFSM := GetGlobalRecoveryFSM()
	SetGlobalRecoveryFSM(nil)
	t.Cleanup(func() { SetGlobalRecoveryFSM(prevFSM) })

	gin.SetMode(gin.TestMode)
	db := newPhase2TestDB(t)
	ae := audit.NewAgentAuditEngine(
		db, nil,
		audit.WithArchiveDir(t.TempDir()),
		audit.WithFaultHooks(func(in audit.EmitEventInput) error {
			return errors.New("audit subsystem offline")
		}, nil),
	)
	h := NewOpsHandler(nil, nil, db, nil, nil)
	h.SetAuditEngine(ae)

	router := gin.New()
	router.POST("/api/v1/ops/maintenance/lock", func(c *gin.Context) {
		c.Set("is_admin", true)
		c.Set("username", "admin")
		h.AcquireMaintenanceLock(c)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/ops/maintenance/lock", bytes.NewBufferString(`{"reason":"test","ttl_minutes":10}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 fail-closed when pre-exec audit fails, got %d: %s", rec.Code, rec.Body.String())
	}
	if h.GetRecoveryFSM().GetMaintenanceLock().IsLocked() {
		t.Fatalf("maintenance lock must NOT be acquired when pre-exec audit fails")
	}
}

func TestR5_ProductionRejectsLegacySHA256Baseline(t *testing.T) {
	legacyHash := "sha256:" + strings.Repeat("a", 64)
	v1Hash := "v1:" + strings.Repeat("b", 64)
	hostV1Hash := "host-v1:" + strings.Repeat("c", 64)

	// Non-prod retains backward compatibility for legacy sha256 container fixtures
	if !IsRealBaselineHash(legacyHash, "home-web") {
		t.Fatalf("expected non-prod IsRealBaselineHash to remain backward-compatible for legacy sha256")
	}
	if IsRealBaselineHash(legacyHash, ComponentHostOS) {
		t.Fatalf("expected host-os to reject sha256 even in non-prod")
	}
	if !IsRealBaselineHashV1(hostV1Hash, ComponentHostOS) {
		t.Fatalf("expected host-v1 to satisfy IsRealBaselineHashV1 for host-os")
	}

	// Production strictly requires v1:<64hex> (containers) or host-v1:<64hex> (host-os)
	t.Setenv("OPS_STRICT_BASELINE", "1")
	if IsRealBaselineHash(legacyHash, "home-web") {
		t.Fatalf("production runtime must reject legacy sha256 baseline format")
	}
	if !IsRealBaselineHash(v1Hash, "home-web") {
		t.Fatalf("production runtime must accept valid v1 container baseline")
	}
	if !IsRealBaselineHash(hostV1Hash, ComponentHostOS) {
		t.Fatalf("production runtime must accept valid host-v1 baseline for host-os")
	}

	db := newPhase2TestDB(t)
	exec := NewOperationExecutor(
		db, nil, nil, nil,
		WithBaselineExtractor(func(target string) string { return legacyHash }),
		WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
			t.Fatalf("dockerRunner must not execute with legacy sha256 baseline in production")
			return "", nil
		}),
		WithHealthChecker(func(ctx context.Context, target string) error { return nil }),
	)
	if _, err := exec.Execute(context.Background(), OperationSpec{
		Action:       ActionDockerRestart,
		Target:       "home-web",
		BaselineHash: legacyHash,
	}); !errors.Is(err, ErrBaselineMismatch) {
		t.Fatalf("expected ErrBaselineMismatch on Execute with legacy sha256 in production, got %v", err)
	}

	prop := &model.OpsProposal{
		ProposalID:      "prop-r11-legacy-sha256",
		Title:           "Legacy sha256 proposal",
		TargetComponent: "home-web",
		ExecutionPlan:   "docker restart home-web",
		RollbackPlan:    "none",
		Status:          model.ProposalStatusApproved,
		BaselineHash:    legacyHash,
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
	}
	seedTestSignedProposal(t, db, prop)
	if _, err := exec.ExecuteProposal(context.Background(), prop); !errors.Is(err, ErrBaselineMismatch) {
		t.Fatalf("expected ErrBaselineMismatch on ExecuteProposal with legacy sha256 in production, got %v", err)
	}

	gin.SetMode(gin.TestMode)
	pendingProp := &model.OpsProposal{
		ProposalID:      "prop-r11-pending-legacy",
		Title:           "Pending legacy sha256 proposal",
		TargetComponent: "home-web",
		ExecutionPlan:   "docker restart home-web",
		RollbackPlan:    "none",
		Status:          model.ProposalStatusPending,
		BaselineHash:    legacyHash,
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
	}
	seedTestSignedProposal(t, db, pendingProp)
	h := NewOpsHandler(nil, nil, db, nil, nil)
	h.SetExecutor(exec)
	router := gin.New()
	router.POST("/api/v1/ops/proposals/:id/approve", func(c *gin.Context) {
		c.Set("is_admin", true)
		c.Set("username", "admin")
		h.ApproveProposal(c)
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-r11-pending-legacy/approve", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected HTTP 409 rejecting legacy sha256 approval in production, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestR10_NewStrictOpsHandler_FailClosedOnInvalidAuditConfig(t *testing.T) {
	prevFSM := GetGlobalRecoveryFSM()
	SetGlobalRecoveryFSM(nil)
	t.Cleanup(func() { SetGlobalRecoveryFSM(prevFSM) })

	db := newPhase2TestDB(t)

	// 1. Invalid remote store type must fail closed
	t.Setenv("OPS_AUDIT_REMOTE_TYPE", "bogus-remote")
	if _, err := NewStrictOpsHandler(nil, nil, db, nil, nil, audit.WithArchiveDir(t.TempDir())); !errors.Is(err, audit.ErrAuditConfigInvalid) {
		t.Fatalf("expected ErrAuditConfigInvalid for bogus remote type, got %v", err)
	}
	t.Setenv("OPS_AUDIT_REMOTE_TYPE", "")

	// 2. Invalid checkpoint period must fail closed
	t.Setenv("OPS_AUDIT_CHECKPOINT_PERIOD_SEC", "0")
	if _, err := NewStrictOpsHandler(nil, nil, db, nil, nil, audit.WithArchiveDir(t.TempDir())); !errors.Is(err, audit.ErrAuditConfigInvalid) {
		t.Fatalf("expected ErrAuditConfigInvalid for zero checkpoint period, got %v", err)
	}
	t.Setenv("OPS_AUDIT_CHECKPOINT_PERIOD_SEC", "")

	// 3. Strict remote enabled without configured remote store must fail closed
	t.Setenv("OPS_AUDIT_STRICT_REMOTE", "1")
	if _, err := NewStrictOpsHandler(nil, nil, db, nil, nil, audit.WithArchiveDir(t.TempDir())); !errors.Is(err, audit.ErrAuditRemoteRequired) {
		t.Fatalf("expected ErrAuditRemoteRequired when strict remote enabled without store, got %v", err)
	}
	t.Setenv("OPS_AUDIT_STRICT_REMOTE", "")

	// 4. Valid local-only strict construction binds a single shared AgentAuditEngine cleanly
	h, err := NewStrictOpsHandler(nil, nil, db, nil, nil, audit.WithArchiveDir(t.TempDir()))
	if err != nil {
		t.Fatalf("expected valid local-only NewStrictOpsHandler, got %v", err)
	}
	t.Cleanup(func() { h.getAuditEngine().Close() })

	execAE := h.GetExecutor().(*DefaultOperationExecutor).GetAuditEngine()
	if execAE == nil || execAE != h.getAuditEngine() {
		t.Fatalf("expected NewStrictOpsHandler to bind the exact single AgentAuditEngine to executor")
	}
}
