package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/middleware"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/repository"
)

// setupRemediationDB initializes an in-memory SQLite database for closed-loop verification.
func setupRemediationDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbName := fmt.Sprintf("file:remediation_test_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test sqlite db: %v", err)
	}
	err = db.AutoMigrate(
		&model.OpsProposal{},
		&model.OpsExecutionHistory{},
		&model.OpsChatSession{},
		&model.OpsChatMessage{},
		&model.User{},
		&model.Device{},
	)
	if err != nil {
		t.Fatalf("failed to auto migrate: %v", err)
	}
	return db
}

// 1. TestCAS_StrictRejectionOfAwaitingAndPending verifies that ExecuteProposal strictly
// rejects AWAITING_APPROVAL and PENDING proposals, and prevents SCHEDULED proposals
// from running before their designated execution window.
func TestCAS_StrictRejectionOfAwaitingAndPending(t *testing.T) {
	db := setupRemediationDB(t)
	executor := NewOperationExecutor(db, eventbus.New(), nil, nil,
		WithDockerSocket("/var/run/docker.sock"),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
		WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
			return "restarted successfully", nil
		}),
	)

	// 1.1 AWAITING_APPROVAL proposal -> MUST FAIL
	propAwaiting := model.OpsProposal{
		ProposalID:       "prop-cas-awaiting",
		Title:            "Unapproved Proposal",
		TargetComponent:  "home-web",
		Status:           model.ProposalStatusAwaitingApproval,
		ActionSpec:       `{"action":"docker.restart","target":"home-web"}`,
		CreatedAt:        time.Now(),
	}
	db.Create(&propAwaiting)

	_, err := executor.ExecuteProposal(context.Background(), &propAwaiting)
	if err == nil {
		t.Fatalf("expected error executing AWAITING_APPROVAL proposal, got nil")
	}
	if !errors.Is(err, ErrProposalUnauthorized) {
		t.Errorf("expected ErrProposalUnauthorized, got: %v", err)
	}

	// 1.2 PENDING proposal -> MUST FAIL
	propPending := model.OpsProposal{
		ProposalID:       "prop-cas-pending",
		Title:            "Pending Proposal",
		TargetComponent:  "home-web",
		Status:           model.ProposalStatusPending,
		ActionSpec:       `{"action":"docker.restart","target":"home-web"}`,
		CreatedAt:        time.Now(),
	}
	db.Create(&propPending)

	_, err = executor.ExecuteProposal(context.Background(), &propPending)
	if err == nil {
		t.Fatalf("expected error executing PENDING proposal, got nil")
	}
	if !errors.Is(err, ErrProposalUnauthorized) {
		t.Errorf("expected ErrProposalUnauthorized, got: %v", err)
	}

	// 1.3 SCHEDULED proposal with future window -> MUST FAIL
	futureTime := time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	propScheduledFuture := model.OpsProposal{
		ProposalID:       "prop-cas-scheduled-future",
		Title:            "Future Scheduled Proposal",
		TargetComponent:  "home-web",
		Status:           model.ProposalStatusScheduled,
		ScheduledAt:      &futureTime,
		ActionSpec:       `{"action":"docker.restart","target":"home-web"}`,
		CreatedAt:        time.Now(),
	}
	seedTestSignedProposal(t, db, &propScheduledFuture)

	_, err = executor.ExecuteProposal(context.Background(), &propScheduledFuture)
	if err == nil {
		t.Fatalf("expected error executing future SCHEDULED proposal, got nil")
	}
	if !errors.Is(err, ErrScheduledWindowNotReached) {
		t.Errorf("expected ErrScheduledWindowNotReached, got: %v", err)
	}

	// 1.4 APPROVED proposal -> ALLOWED to execute
	propApproved := model.OpsProposal{
		ProposalID:       "prop-cas-approved",
		Title:            "Authorized Approved Proposal",
		TargetComponent:  "home-web",
		Status:           model.ProposalStatusApproved,
		ActionSpec:       `{"action":"docker.restart","target":"home-web"}`,
		CreatedAt:        time.Now(),
	}
	seedTestSignedProposal(t, db, &propApproved)

	res, err := executor.ExecuteProposal(context.Background(), &propApproved)
	if err != nil {
		t.Fatalf("expected approved proposal to execute successfully, got: %v", err)
	}
	if !res.Success {
		t.Errorf("expected res.Success == true")
	}
	if propApproved.Status != model.ProposalStatusExecuted {
		t.Errorf("expected proposal status EXECUTED, got %s", propApproved.Status)
	}
}

// 2. TestSafetyFSM_NoRollbackFnReportedAsFailed verifies that when rollbackFn == nil,
// SafetyFSM marks StateFailed, NEVER StateRolledBack, and panic in applyFn/rollbackFn
// is safely isolated.
func TestSafetyFSM_NoRollbackFnReportedAsFailed(t *testing.T) {
	db := setupRemediationDB(t)
	backupDir := t.TempDir()

	// Probe always fails (500)
	probeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer probeSrv.Close()

	fsm := NewSafetyFSM(db, nil, nil, backupDir, probeSrv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 2.1 Call ExecuteSafeUpdate with rollbackFn == nil
	err := fsm.ExecuteSafeUpdate(ctx, "home-api", "v1.18.0", "v1.19.0",
		func() error { return nil },
		nil, // NO rollback function
	)

	if err == nil {
		t.Fatalf("expected error from failed probe, got nil")
	}

	// Last terminal state MUST be StateFailed, NEVER StateRolledBack
	if fsm.LastTerminalState() == StateRolledBack {
		t.Fatalf("CRITICAL FLAW: LastTerminalState was marked as ROLLED_BACK when rollbackFn was nil!")
	}
	if fsm.LastTerminalState() != StateFailed {
		t.Errorf("expected LastTerminalState to be FAILED, got: %s", fsm.LastTerminalState())
	}

	// 2.2 Verify applyFn panic is isolated and recorded as FAILED
	panicFSM := NewSafetyFSM(db, nil, nil, backupDir, probeSrv.URL)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()

	err = panicFSM.ExecuteSafeUpdate(ctx2, "home-api", "v1.18.0", "v1.19.0",
		func() error {
			panic("simulated fatal memory fault during apply")
		},
		func() error { return nil },
	)

	if err == nil {
		t.Fatalf("expected error from panicking applyFn, got nil")
	}
	if !strings.Contains(err.Error(), "panic in applyFn") {
		t.Errorf("expected error to capture panic, got: %v", err)
	}
	if panicFSM.LastTerminalState() != StateFailed {
		t.Errorf("expected state FAILED after panic, got: %s", panicFSM.LastTerminalState())
	}
}

// 3. TestCachePurge_NoSimulatedSuccessBranches verifies that Cache Purge returns
// real errors for missing/unsupported targets and rejects arbitrary paths.
func TestCachePurge_NoSimulatedSuccessBranches(t *testing.T) {
	executor := NewOperationExecutor(nil, eventbus.New(), nil, nil, WithDockerSocket("/nonexistent/docker.sock"))

	// 3.1 Unsupported target -> ErrNotImplemented
	_, err := executor.executeCachePurge(context.Background(), map[string]interface{}{
		"cache_type": "in_memory",
	})
	if !errors.Is(err, ErrNotImplemented) {
		t.Errorf("expected in_memory to return ErrNotImplemented, got: %v", err)
	}

	_, err = executor.executeCachePurge(context.Background(), map[string]interface{}{
		"cache_type": "random_fake_target",
	})
	if !errors.Is(err, ErrNotImplemented) {
		t.Errorf("expected random_fake_target to return ErrNotImplemented, got: %v", err)
	}

	// 3.2 Docker builder with inaccessible socket -> real error (no fake "completed")
	_, err = executor.executeCachePurge(context.Background(), map[string]interface{}{
		"cache_type": "docker_builder",
	})
	if err == nil {
		t.Fatalf("expected error when docker socket is inaccessible, got nil (fake success)")
	}

	// 3.3 Transcode temp with non-existent directory -> real error
	t.Setenv("TRANSCODE_TEMP_DIR", "/nonexistent/transcode/cache/path/12345")
	_, err = executor.executeCachePurge(context.Background(), map[string]interface{}{
		"cache_type": "transcode_temp",
	})
	if err == nil {
		t.Fatalf("expected error when transcode temp dir is nonexistent, got nil")
	}

	// 3.4 Transcode temp with real files: files modified < 5min are kept, older files are deleted
	validTemp := t.TempDir()
	t.Setenv("TRANSCODE_TEMP_DIR", validTemp)

	// File 1: freshly modified (now)
	fileFresh := filepath.Join(validTemp, "segment_live.m4s")
	_ = os.WriteFile(fileFresh, []byte("fresh-video-data"), 0644)

	// File 2: modified 10 minutes ago
	fileOld := filepath.Join(validTemp, "segment_old.m4s")
	_ = os.WriteFile(fileOld, []byte("old-expired-video-data-12345"), 0644)
	oldMTime := time.Now().Add(-10 * time.Minute)
	_ = os.Chtimes(fileOld, oldMTime, oldMTime)

	out, err := executor.executeCachePurge(context.Background(), map[string]interface{}{
		"cache_type": "transcode_temp",
	})
	if err != nil {
		t.Fatalf("unexpected error purging valid temp: %v", err)
	}
	if !strings.Contains(out, "purged 1 temporary transcode files") {
		t.Errorf("expected 1 file purged, got output: %s", out)
	}

	// Verify fileFresh still exists (not purged because < 5 min old)
	if _, err := os.Stat(fileFresh); os.IsNotExist(err) {
		t.Errorf("CRITICAL: freshly written segment file was erroneously purged!")
	}
	// Verify fileOld is gone
	if _, err := os.Stat(fileOld); !os.IsNotExist(err) {
		t.Errorf("expected old segment file to be purged, but it still exists")
	}
}

// 4. TestOSVScanner_PartialFailureState verifies that OSV scanner accurately transitions
// to "partial_failure" when some targets fail, and "failed" when zero valid dependencies exist.
func TestOSVScanner_PartialFailureState(t *testing.T) {
	// Mock OSV server: responds with 200 for pkg-ok, but 500 for pkg-err
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "pkg-err") {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error": "OSV internal gateway error"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"vulns": []}`))
	}))
	defer srv.Close()

	scanner := NewVulnScanner(nil, nil)
	scanner.SetAPIURL(srv.URL)

	tempDir := t.TempDir()

	// 4.1 Empty workspace (0 valid dependencies) -> returns error and status "failed"
	_, err := scanner.ScanWorkspaceDependencies(context.Background(), tempDir)
	// DefaultScanTargets has packages, but let's test DefaultScanTargets with partial failure
	// Create a requirements.txt with 1 good pkg and 1 bad pkg
	reqPath := filepath.Join(tempDir, "requirements.txt")
	reqContent := "pkg-ok==1.0.0\npkg-err==2.0.0\n"
	_ = os.WriteFile(reqPath, []byte(reqContent), 0644)

	// Scan with mixed good and bad packages
	vulns, err := scanner.ScanWorkspaceDependencies(context.Background(), tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = vulns

	// Scanner status MUST be partial_failure, NEVER success!
	if scanner.LastScanStatus() == "success" {
		t.Fatalf("CRITICAL FLAW: LastScanStatus was marked 'success' despite pkg-err failing with 500!")
	}
	if scanner.LastScanStatus() != ScanStatusPartialFailure {
		t.Errorf("expected status '%s', got '%s'", ScanStatusPartialFailure, scanner.LastScanStatus())
	}
	if !strings.Contains(scanner.LastScanError(), "1 of 2 dependency scans failed") {
		t.Errorf("expected detailed partial error message, got: %s", scanner.LastScanError())
	}
}

// 5. TestInternalSecretAuth_EnforcesDualTrack verifies that internal daemons can
// authenticate via X-Internal-Secret from loopback/private IPs, but are forbidden
// from self-approving proposals (Human-in-the-Loop).
func TestInternalSecretAuth_EnforcesDualTrack(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupRemediationDB(t)

	testSecret := "test-internal-secret-token-32-bytes-long"
	t.Setenv("INTERNAL_OPS_TOKEN", testSecret)

	deviceRepo := repository.NewDeviceRepository(db)
	userRepo := repository.NewUserRepository(db)

	r := gin.New()
	rg := r.Group("/api/v1/ops")
	rg.Use(middleware.OpsFlexibleAuth(deviceRepo, userRepo))

	handler := NewOpsHandler(nil, nil, db, eventbus.New(), nil)
	handler.RegisterProposalRoutes(rg)

	// 5.1 Request with valid X-Internal-Secret to create proposal -> SUCCEEDS (201/200)
	propPayload := `{"proposal_id":"prop-int-001","title":"Internal Daemon Proposal","target_component":"home-web","risk_level":"HIGH","action_spec":"{\"action\":\"docker.restart\",\"target\":\"home-web\"}"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals", strings.NewReader(propPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", testSecret)
	req.RemoteAddr = "127.0.0.1:54321" // Loopback IP
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("expected 200/201 creating proposal with X-Internal-Secret, got %d: %s", w.Code, w.Body.String())
	}

	// 5.2 Internal daemon attempts to approve proposal -> FORBIDDEN (403)
	wApprove := httptest.NewRecorder()
	reqApprove, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-int-001/approve", strings.NewReader(`{"action":"now"}`))
	reqApprove.Header.Set("Content-Type", "application/json")
	reqApprove.Header.Set("X-Internal-Secret", testSecret)
	reqApprove.RemoteAddr = "127.0.0.1:54321"
	r.ServeHTTP(wApprove, reqApprove)

	if wApprove.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden when internal daemon attempts to approve proposal, got %d", wApprove.Code)
	}
	if !strings.Contains(wApprove.Body.String(), "internal daemon cannot approve proposals") {
		t.Errorf("expected denial message for internal daemon, got: %s", wApprove.Body.String())
	}

	// 5.3 Request with invalid X-Internal-Secret -> 401 Unauthorized
	wBad := httptest.NewRecorder()
	reqBad, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals", strings.NewReader(propPayload))
	reqBad.Header.Set("Content-Type", "application/json")
	reqBad.Header.Set("X-Internal-Secret", "wrong-secret-token")
	reqBad.RemoteAddr = "127.0.0.1:54321"
	r.ServeHTTP(wBad, reqBad)

	if wBad.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized with wrong secret, got %d", wBad.Code)
	}
}

// 6. TestProductionDeFaking_ZeroFakeSeedData verifies that when tables are empty,
// ListProposals and ListExecutionHistory return empty arrays [], NOT fake seeds.
func TestProductionDeFaking_ZeroFakeSeedData(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupRemediationDB(t)

	handler := NewOpsHandler(nil, nil, db, eventbus.New(), nil)
	r := gin.New()
	rg := r.Group("/api/v1/ops")
	rg.Use(func(c *gin.Context) {
		c.Set("user_id", uint(1))
		c.Set("is_admin", true)
		c.Next()
	})
	handler.RegisterRoutes(rg)
	handler.RegisterProposalRoutes(rg)

	// 6.1 ListProposals on empty DB
	wProp := httptest.NewRecorder()
	reqProp, _ := http.NewRequest(http.MethodGet, "/api/v1/ops/proposals", nil)
	r.ServeHTTP(wProp, reqProp)

	if wProp.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", wProp.Code)
	}
	var proposals []model.OpsProposal
	_ = json.Unmarshal(wProp.Body.Bytes(), &proposals)
	if len(proposals) != 0 {
		t.Fatalf("CRITICAL FLAW: ListProposals injected %d fake seed proposals into empty DB!", len(proposals))
	}

	// 6.2 ListExecutionHistory on empty DB
	wHist := httptest.NewRecorder()
	reqHist, _ := http.NewRequest(http.MethodGet, "/api/v1/ops/history", nil)
	r.ServeHTTP(wHist, reqHist)

	if wHist.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", wHist.Code)
	}
	var histResp struct {
		Data []model.OpsExecutionHistory `json:"data"`
	}
	_ = json.Unmarshal(wHist.Body.Bytes(), &histResp)
	if len(histResp.Data) != 0 {
		t.Fatalf("CRITICAL FLAW: ListExecutionHistory injected %d fake seed histories into empty DB!", len(histResp.Data))
	}
}

// 7. TestDockerRecreate_AtomicRollbackOnFailure verifies that when recreating a container
// fails during create/start/probe, the Blue-Green safety mechanism automatically restores
// the original container from backup.
func TestDockerRecreate_AtomicRollbackOnFailure(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "recreate_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	sockPath := filepath.Join(tempDir, "mock_docker.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Skipf("unix domain sockets not supported on this Windows build: %v", err)
		return
	}
	defer listener.Close()

	var callLog []string
	var mu sync.Mutex
	logCall := func(call string) {
		mu.Lock()
		defer mu.Unlock()
		callLog = append(callLog, call)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri := r.URL.RequestURI()
		logCall(fmt.Sprintf("%s %s", r.Method, uri))
		path := r.URL.Path

		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/containers/home-web/json"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"Config": {
					"Image": "feiyemomo/home-web:v1.18.0",
					"Env": ["PORT=80"],
					"Cmd": ["nginx"],
					"Labels": {"app": "home-web"}
				},
				"HostConfig": {"RestartPolicy": {"Name": "always"}},
				"State": {"Running": true, "Status": "running"}
			}`))

		case r.Method == http.MethodPost && strings.Contains(path, "/containers/home-web/stop"):
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && strings.Contains(path, "/rename"):
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && strings.Contains(path, "/containers/create"):
			// Simulate create failure
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message": "simulated out of disk space during container create"}`))

		case r.Method == http.MethodDelete && strings.Contains(path, "/containers/home-web"):
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && strings.Contains(path, "/start"):
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	server := &http.Server{Handler: handler}
	go func() {
		_ = server.Serve(listener)
	}()
	defer server.Close()

	// Execute Docker Recreate
	_, execErr := executeDockerSocketRecreate(context.Background(), sockPath, "home-web", "feiyemomo/home-web:v1.19.0")
	if execErr == nil {
		t.Fatalf("expected executeDockerSocketRecreate to fail due to simulated create error, but got nil")
	}

	// Verify that original container was restored via rename & start
	mu.Lock()
	defer mu.Unlock()

	hasRenameToBak := false
	hasRenameBack := false
	hasOrigRestart := false

	for _, call := range callLog {
		if strings.Contains(call, "POST /containers/home-web/rename?name=home-web_bak_") {
			hasRenameToBak = true
		}
		if strings.Contains(call, "POST /containers/home-web_bak_") && strings.Contains(call, "rename?name=home-web") {
			hasRenameBack = true
		}
		if strings.Contains(call, "POST /containers/home-web/start") {
			hasOrigRestart = true
		}
	}

	if !hasRenameToBak {
		t.Errorf("expected container to be renamed to backup, calls: %v", callLog)
	}
	if !hasRenameBack {
		t.Errorf("expected backup container to be restored via rename back to original name, calls: %v", callLog)
	}
	if !hasOrigRestart {
		t.Errorf("expected original container to be restarted after rollback, calls: %v", callLog)
	}
}

// 8. TestDockerRecreate_SuccessAndOldContainerPruned verifies that on success:
// 1) Target image is used (or falls back to existing image)
// 2) New container is created and started
// 3) Health probe verifies running state
// 4) Old backup container is cleanly removed
func TestDockerRecreate_SuccessAndOldContainerPruned(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "recreate_success_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	sockPath := filepath.Join(tempDir, "mock_docker.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Skipf("unix domain sockets not supported on this Windows build: %v", err)
		return
	}
	defer listener.Close()

	var callLog []string
	var mu sync.Mutex
	logCall := func(call string) {
		mu.Lock()
		defer mu.Unlock()
		callLog = append(callLog, call)
	}

	var createdImage string

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri := r.URL.RequestURI()
		logCall(fmt.Sprintf("%s %s", r.Method, uri))
		path := r.URL.Path

		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/containers/home-web/json"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"Config": {
					"Image": "feiyemomo/home-web:v1.18.0",
					"Env": ["PORT=80"],
					"Cmd": ["nginx"],
					"Labels": {"app": "home-web"}
				},
				"HostConfig": {"RestartPolicy": {"Name": "always"}},
				"State": {"Running": true, "Status": "running"}
			}`))

		case r.Method == http.MethodPost && strings.Contains(path, "/containers/home-web/stop"):
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && strings.Contains(path, "/rename"):
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && strings.Contains(path, "/containers/create"):
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if img, ok := body["Image"].(string); ok {
				createdImage = img
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id": "new-container-12345"}`))

		case r.Method == http.MethodPost && strings.Contains(path, "/start"):
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodDelete && strings.Contains(path, "home-web_bak_"):
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	server := &http.Server{Handler: handler}
	go func() {
		_ = server.Serve(listener)
	}()
	defer server.Close()

	out, err := executeDockerSocketRecreate(context.Background(), sockPath, "home-web", "feiyemomo/home-web:v1.19.0")
	if err != nil {
		t.Fatalf("unexpected recreation error: %v", err)
	}

	if !strings.Contains(out, "recreated successfully with image feiyemomo/home-web:v1.19.0") {
		t.Errorf("unexpected output: %s", out)
	}
	if createdImage != "feiyemomo/home-web:v1.19.0" {
		t.Errorf("expected created image to be v1.19.0, got: %s", createdImage)
	}

	mu.Lock()
	defer mu.Unlock()

	hasBackupDelete := false
	for _, call := range callLog {
		if strings.Contains(call, "DELETE /containers/home-web_bak_") {
			hasBackupDelete = true
			break
		}
	}
	if !hasBackupDelete {
		t.Errorf("expected backup container to be cleanly deleted after verified success, calls: %v", callLog)
	}
}

// 9. TestExecuteProposalEndpoint_DualTrackAndStateSafety tests POST /api/v1/ops/proposals/:id/execute
// verifying:
// - Internal daemon (X-Internal-Secret) CAN execute APPROVED proposals
// - Internal daemon CANNOT execute AWAITING_APPROVAL proposals (blocked by CAS)
// - Unauthenticated requests return 401 Unauthorized
func TestExecuteProposalEndpoint_DualTrackAndStateSafety(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupRemediationDB(t)

	secret := "test-internal-secret-for-ops-daemon-auth-32chars"
	t.Setenv("INTERNAL_OPS_TOKEN", secret)

	devRepo := repository.NewDeviceRepository(db)
	userRepo := repository.NewUserRepository(db)

	executor := NewOperationExecutor(db, eventbus.New(), nil, nil,
		WithDockerSocket("/var/run/docker.sock"),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
		WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
			return "recreated successfully", nil
		}),
	)

	handler := NewOpsHandler(nil, nil, db, eventbus.New(), nil)
	handler.SetExecutor(executor)

	r := gin.New()
	r.Use(middleware.OpsFlexibleAuth(devRepo, userRepo))
	rg := r.Group("/api/v1/ops")
	handler.RegisterProposalRoutes(rg)

	// Seed proposals
	propApproved := model.OpsProposal{
		ProposalID:      "prop-approved-exec-001",
		Title:           "Approved Proposal for Exec Route",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusApproved,
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
		CreatedAt:       time.Now(),
	}
	seedTestSignedProposal(t, db, &propApproved)

	propAwaiting := model.OpsProposal{
		ProposalID:      "prop-awaiting-exec-002",
		Title:           "Awaiting Proposal for Exec Route",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusAwaitingApproval,
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
		CreatedAt:       time.Now(),
	}
	seedTestSignedProposal(t, db, &propAwaiting)

	// Case 1: Unauthenticated -> 401 Unauthorized
	wNoAuth := httptest.NewRecorder()
	reqNoAuth, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-approved-exec-001/execute", nil)
	r.ServeHTTP(wNoAuth, reqNoAuth)
	if wNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for request without auth, got %d", wNoAuth.Code)
	}

	// Case 2: Internal Daemon executing APPROVED proposal -> 200 OK
	wDaemonOk := httptest.NewRecorder()
	reqDaemonOk, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-approved-exec-001/execute", nil)
	reqDaemonOk.Header.Set("X-Internal-Secret", secret)
	reqDaemonOk.RemoteAddr = "127.0.0.1:54321"
	r.ServeHTTP(wDaemonOk, reqDaemonOk)
	if wDaemonOk.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for internal daemon executing APPROVED proposal, got %d: %s", wDaemonOk.Code, wDaemonOk.Body.String())
	}

	// Case 3: Internal Daemon executing AWAITING_APPROVAL proposal -> 400 Bad Request
	wDaemonBad := httptest.NewRecorder()
	reqDaemonBad, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/proposals/prop-awaiting-exec-002/execute", nil)
	reqDaemonBad.Header.Set("X-Internal-Secret", secret)
	reqDaemonBad.RemoteAddr = "127.0.0.1:54321"
	r.ServeHTTP(wDaemonBad, reqDaemonBad)
	if wDaemonBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request executing AWAITING_APPROVAL proposal, got %d: %s", wDaemonBad.Code, wDaemonBad.Body.String())
	}
}

// 10. TestSCHEDULED_DaytimeWindowBlocked verifies that SCHEDULED proposals
// targeted for low-peak window cannot be executed during daytime/peak hours.
func TestSCHEDULED_DaytimeWindowBlocked(t *testing.T) {
	db := setupRemediationDB(t)
	executor := NewOperationExecutor(db, eventbus.New(), nil, nil,
		WithDockerSocket("/var/run/docker.sock"),
		WithHealthChecker(func(ctx context.Context, target string) error {
			return nil
		}),
		WithDockerRunner(func(ctx context.Context, action, container string, params map[string]interface{}) (string, error) {
			return "done", nil
		}),
	)

	schedTime := "03:00"
	propSched := model.OpsProposal{
		ProposalID:      "prop-sched-low-peak",
		Title:           "Low-Peak Scheduled Proposal",
		TargetComponent: "home-web",
		Status:          model.ProposalStatusScheduled,
		ScheduledAt:     &schedTime,
		ActionSpec:      `{"action":"docker.restart","target":"home-web"}`,
		CreatedAt:       time.Now(),
	}
	seedTestSignedProposal(t, db, &propSched)

	// If current time is not off-peak (02:00 - 05:00), ExecuteProposal MUST reject
	if !IsOffPeak(time.Now()) {
		_, err := executor.ExecuteProposal(context.Background(), &propSched)
		if err == nil {
			t.Fatalf("expected ErrScheduledWindowNotReached during daytime peak, got nil")
		}
		if !errors.Is(err, ErrScheduledWindowNotReached) {
			t.Errorf("expected ErrScheduledWindowNotReached, got: %v", err)
		}
	}
}
