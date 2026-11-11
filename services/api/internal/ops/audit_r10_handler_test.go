package ops

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/audit"
	"home-datacenter-api/internal/model"
)

func TestAuditR10_Endpoints_RBAC_RemoteSync_And_MigrationV7(t *testing.T) {
	gin.SetMode(gin.TestMode)

	hmacSecret := "r10-handler-hmac-secret-at-least-32-bytes-long"
	cleanupRing := audit.SetAuditHMACKeyringForTest("k1", map[string]string{
		"k1": hmacSecret,
	})
	defer cleanupRing()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	signer, err := audit.NewEd25519CheckpointSigner("ed-r10", priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	verifier, err := audit.NewEd25519CheckpointVerifier("ed-r10", priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}

	wormDir := filepath.Join(t.TempDir(), "remote-worm")
	wormStore, err := audit.NewFileWORMRemoteStore(wormDir)
	if err != nil {
		t.Fatalf("NewFileWORMRemoteStore: %v", err)
	}

	db, bus, ae, _ := setupAuditR5TestEnv(t,
		audit.WithRemoteStore(wormStore),
		audit.WithExternalSigner(signer),
		audit.WithExternalVerifier(verifier),
	)
	defer ae.Close()

	var maxVer int
	if err := db.Raw("SELECT COALESCE(MAX(version), 0) FROM ops_schema_migrations").Scan(&maxVer).Error; err != nil {
		t.Fatalf("query migration version: %v", err)
	}
	if maxVer < 7 || RequiredOpsSchemaVersion != 7 {
		t.Fatalf("expected migration v7 applied, got maxVer=%d RequiredOpsSchemaVersion=%d", maxVer, RequiredOpsSchemaVersion)
	}

	if _, err := ae.EmitEvent(context.Background(), audit.EmitEventInput{
		HouseholdID:   1,
		TriggerSource: "api_manual",
		ActorType:     "user",
		ActorID:       "admin-1",
		EventType:     model.AuditEventExecutionCompleted,
		Action:        "container.restart",
		Target:        "home-web",
		Component:     "home-web",
		OperationID:   "op-r10-handler-1",
		Result:        "SUCCESS",
	}); err != nil {
		t.Fatalf("emit event: %v", err)
	}

	handler := NewOpsHandler(nil, nil, db, bus, nil)
	handler.SetAuditEngine(ae)

	buildRouter := func(isAdmin, isInternal bool) *gin.Engine {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Set("is_admin", isAdmin)
			c.Set("is_internal_service", isInternal)
			c.Set("household_id", uint(1))
			c.Set("user_id", uint(1))
			c.Next()
		})
		handler.RegisterAuditRoutes(r.Group("/api/v1/ops"))
		return r
	}

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/ops/audit/key-status"},
		{http.MethodGet, "/api/v1/ops/audit/remote-status"},
		{http.MethodPost, "/api/v1/ops/audit/remote/sync"},
		{http.MethodPost, "/api/v1/ops/audit/checkpoints"},
	} {
		for _, role := range []struct {
			name       string
			isAdmin    bool
			isInternal bool
		}{
			{"non_admin", false, false},
			{"internal_service", true, true},
		} {
			w := httptest.NewRecorder()
			buildRouter(role.isAdmin, role.isInternal).ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s %s (%s) expected 403, got %d (%s)", tc.method, tc.path, role.name, w.Code, w.Body.String())
			}
		}
	}

	rAdmin := buildRouter(true, false)

	// 1. Create checkpoint via endpoint (signed by Ed25519 + replicated to WORM)
	wCP := httptest.NewRecorder()
	rAdmin.ServeHTTP(wCP, httptest.NewRequest(http.MethodPost, "/api/v1/ops/audit/checkpoints", nil))
	if wCP.Code != http.StatusOK || !strings.Contains(wCP.Body.String(), `"created":true`) {
		t.Fatalf("POST /audit/checkpoints failed: %d %s", wCP.Code, wCP.Body.String())
	}

	// 2. Check key-status returns key_id + ed25519 pubkey metadata with ZERO secret leakage
	wKS := httptest.NewRecorder()
	rAdmin.ServeHTTP(wKS, httptest.NewRequest(http.MethodGet, "/api/v1/ops/audit/key-status", nil))
	if wKS.Code != http.StatusOK {
		t.Fatalf("GET /audit/key-status failed: %d %s", wKS.Code, wKS.Body.String())
	}
	if strings.Contains(wKS.Body.String(), hmacSecret) {
		t.Fatalf("HMAC secret leaked in /audit/key-status!")
	}
	var ksEnv struct {
		Code int                  `json:"code"`
		Data audit.AuditKeyStatus `json:"data"`
	}
	if err := json.Unmarshal(wKS.Body.Bytes(), &ksEnv); err != nil {
		t.Fatalf("decode key-status: %v", err)
	}
	ks := ksEnv.Data
	if ks.ActiveKeyID != "k1" || ks.ExternalSigAlg != "ed25519" || ks.ExternalKeyID != "ed-r10" || !ks.RemoteStoreConfigured {
		t.Fatalf("unexpected key-status: %+v", ks)
	}

	// 3. Check remote-status & sync endpoints
	wRS := httptest.NewRecorder()
	rAdmin.ServeHTTP(wRS, httptest.NewRequest(http.MethodGet, "/api/v1/ops/audit/remote-status", nil))
	if wRS.Code != http.StatusOK {
		t.Fatalf("GET /audit/remote-status failed: %d %s", wRS.Code, wRS.Body.String())
	}
	var rsResp struct {
		Code int `json:"code"`
		Data struct {
			Diff audit.RemoteDiffReport `json:"diff"`
		} `json:"data"`
	}
	if err := json.Unmarshal(wRS.Body.Bytes(), &rsResp); err != nil {
		t.Fatalf("decode remote-status: %v", err)
	}
	if rsResp.Data.Diff.Status != audit.RemoteStatusInSync || !rsResp.Data.Diff.ExternalSigVerified {
		t.Fatalf("expected IN_SYNC + ExternalSigVerified=true, got %+v", rsResp.Data.Diff)
	}
}
