package ops

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

func seedTestSignedProposal(t *testing.T, db *gorm.DB, p *model.OpsProposal) {
	t.Helper()
	if strings.TrimSpace(p.BaselineHash) == "" && strings.TrimSpace(p.TargetComponent) != "" {
		p.BaselineHash = model.ComputeDefaultBaseline(p.TargetComponent)
	}
	if p.Version <= 0 {
		p.Version = 1
	}
	if p.Revision <= 0 {
		p.Revision = 1
	}
	if p.HouseholdID == 0 {
		p.HouseholdID = 1
	}
	if p.Status == model.ProposalStatusApproved || p.Status == model.ProposalStatusScheduled {
		if strings.TrimSpace(p.ApprovedBy) == "" {
			p.ApprovedBy = "admin-fixture"
		}
		if !p.ApprovedAt.Valid {
			p.ApprovedAt = utils.NullTime{Time: time.Now().UTC().Truncate(time.Second), Valid: true}
		} else {
			p.ApprovedAt.Time = p.ApprovedAt.Time.UTC().Truncate(time.Second)
		}
		if strings.TrimSpace(p.ApprovalDigest) == "" {
			digest, err := ComputeProposalApprovalDigest(p)
			if err != nil {
				t.Fatalf("seedTestSignedProposal failed to sign proposal %s: %v", p.ProposalID, err)
			}
			p.ApprovalDigest = digest
		}
	}
	if err := db.Create(p).Error; err != nil {
		t.Fatalf("seedTestSignedProposal failed to persist proposal %s: %v", p.ProposalID, err)
	}
}

// setupMockOnePanelServer simulates a real 1Panel v2 server.
func setupMockOnePanelServer(t *testing.T) (*httptest.Server, *rsa.PrivateKey) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	pubASN1, err := x509.MarshalPKIXPublicKey(&privKey.PublicKey)
	if err != nil {
		t.Fatalf("failed to marshal public key: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubASN1,
	})
	pubB64Cookie := base64.StdEncoding.EncodeToString(pubPEM)

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entrance := r.Header.Get("EntranceCode")
		if entrance == "" {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("500 Internal Server Error (nginx)"))
			return
		}

		// Set panel_public_key cookie on all responses
		http.SetCookie(w, &http.Cookie{
			Name:  "panel_public_key",
			Value: url.QueryEscape(pubB64Cookie),
			Path:  "/",
		})

		switch r.URL.Path {
		case "/api/v2/core/auth/login":
			var req map[string]string
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if req["name"] == "" {
				// Probe request
				json.NewEncoder(w).Encode(onePanelResponse{Code: 400, Message: "name required"})
				return
			}

			// Validate hybrid encrypted password
			parts := strings.Split(req["password"], ":")
			if len(parts) != 3 {
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(onePanelResponse{Code: 401, Message: "ErrAuth"})
				return
			}

			// Decrypt RSA
			rsaCipher, _ := base64.StdEncoding.DecodeString(parts[0])
			aesKeyHexBytes, err := rsa.DecryptPKCS1v15(rand.Reader, privKey, rsaCipher)
			if err != nil {
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(onePanelResponse{Code: 401, Message: "ErrAuth"})
				return
			}

			// Decrypt AES-CBC
			iv, _ := base64.StdEncoding.DecodeString(parts[1])
			aesCipher, _ := base64.StdEncoding.DecodeString(parts[2])

			aesBlock, _ := aes.NewCipher(aesKeyHexBytes)
			decryptedPadded := make([]byte, len(aesCipher))
			mode := cipher.NewCBCDecrypter(aesBlock, iv)
			mode.CryptBlocks(decryptedPadded, aesCipher)

			// Remove PKCS7 padding
			padLen := int(decryptedPadded[len(decryptedPadded)-1])
			decryptedPassword := string(decryptedPadded[:len(decryptedPadded)-padLen])

			if req["name"] == "feiyemomo" && decryptedPassword == "@Momo324" {
				http.SetCookie(w, &http.Cookie{Name: "psession", Value: "mock_session_123", Path: "/"})
				http.SetCookie(w, &http.Cookie{Name: "pcsrftoken", Value: "mock_csrf_456", Path: "/"})
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(onePanelResponse{Code: 200, Message: "success"})
				return
			}

			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(onePanelResponse{Code: 401, Message: "ErrAuth"})

		case "/api/v2/dashboard/current/node":
			cookie, err := r.Cookie("psession")
			if err != nil || cookie.Value != "mock_session_123" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"code":    200,
				"message": "success",
				"data": onePanelNodeMetrics{
					Load1:             0.5,
					Load5:             0.7,
					Load15:            0.6,
					CPUUsedPercent:    25.5,
					CPUTotal:          4,
					CPUModel:          "Intel Celeron J4125",
					MemoryTotal:       8 * 1024 * 1024 * 1024,
					MemoryUsed:        2 * 1024 * 1024 * 1024,
					MemoryAvailable:   6 * 1024 * 1024 * 1024,
					MemoryUsedPercent: 25.0,
					Uptime:            86400,
				},
			})

		case "/api/v2/dashboard/base/os":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"code":    200,
				"message": "success",
				"data": onePanelOSInfo{
					OS:            "linux",
					Platform:      "debian",
					PrettyDistro:  "Debian GNU/Linux 12",
					KernelVersion: "6.18.18",
					KernelArch:    "amd64",
					DiskSize:      500 * 1024 * 1024 * 1024,
					Hostname:      "nas-core",
				},
			})

		case "/api/v2/containers/search":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"code":    200,
				"message": "success",
				"data": onePanelContainerPage{
					Total: 2,
					Items: []onePanelContainerItem{
						{
							ContainerID: "cid_1",
							Name:        "/home-web",
							State:       "running",
							Status:      "Up 3 days",
							Image:       "home-web:latest",
							CPUPerc:     1.2,
							MemUsage:    50 * 1024 * 1024,
							MemLimit:    128 * 1024 * 1024,
						},
						{
							ContainerID: "cid_2",
							Name:        "/home-api",
							State:       "running",
							Status:      "Up 3 days",
							Image:       "home-api:latest",
							CPUPerc:     2.5,
							MemUsage:    65 * 1024 * 1024,
							MemLimit:    256 * 1024 * 1024,
						},
					},
				},
			})

		case "/api/v2/containers/operate":
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			op, _ := req["operation"].(string)
			if op != "restart" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(onePanelResponse{Code: 200, Message: "success"})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	return server, privKey
}

func setupTestDB(t *testing.T) *gorm.DB {
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite test DB: %v", err)
	}
	if err := db.AutoMigrate(&model.SystemLog{}); err != nil {
		t.Fatalf("failed to automigrate system_logs: %v", err)
	}
	return db
}

func TestOnePanelClientLoginAndAPIs(t *testing.T) {
	server, _ := setupMockOnePanelServer(t)
	defer server.Close()

	client := NewOnePanelClient(server.URL, "btpanel", "feiyemomo", "@Momo324")

	ctx := context.Background()
	// Test Login
	if err := client.Login(ctx); err != nil {
		t.Fatalf("client.Login failed: %v", err)
	}

	// Test GetNodeMetrics
	nodeMetrics, err := client.GetNodeMetrics(ctx)
	if err != nil {
		t.Fatalf("client.GetNodeMetrics failed: %v", err)
	}
	if nodeMetrics.CPUTotal != 4 || nodeMetrics.CPUUsedPercent != 25.5 {
		t.Errorf("unexpected nodeMetrics: %+v", nodeMetrics)
	}

	// Test GetOSInfo
	osInfo, err := client.GetOSInfo(ctx)
	if err != nil {
		t.Fatalf("client.GetOSInfo failed: %v", err)
	}
	if osInfo.Hostname != "nas-core" || osInfo.DiskSize == 0 {
		t.Errorf("unexpected osInfo: %+v", osInfo)
	}

	// Test ListContainers
	containers, err := client.ListContainers(ctx)
	if err != nil {
		t.Fatalf("client.ListContainers failed: %v", err)
	}
	if len(containers) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(containers))
	}
	if containers[0].Name != "home-web" || !containers[0].IsCore {
		t.Errorf("unexpected container: %+v", containers[0])
	}

	// Test RestartContainer
	if err := client.RestartContainer(ctx, "home-web"); err != nil {
		t.Fatalf("client.RestartContainer failed: %v", err)
	}
}

func TestHostFallbackProvider(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "last.json")
	stateData := map[string]any{
		"ts":           time.Now().Unix(),
		"ok":           true,
		"remote_files": 42,
		"remote_bytes": 104857600,
		"duration_sec": 3.5,
	}
	b, _ := json.Marshal(stateData)
	os.WriteFile(stateFile, b, 0644)

	provider := NewHostFallbackProvider(tmpDir, stateFile)

	// Test Status
	status := provider.GetHostStatus()
	if status.Source != "host_fallback" || status.Reachable {
		t.Errorf("expected host_fallback and unreachable, got %s, reachable=%v", status.Source, status.Reachable)
	}
	if status.CPU.Cores <= 0 {
		t.Errorf("expected positive cores, got %d", status.CPU.Cores)
	}
	if status.DiskPool.Watermark == "" {
		t.Errorf("watermark should not be empty")
	}

	// Test Containers catalog
	containers := provider.GetHostContainers(context.Background())
	if len(containers) == 0 {
		t.Fatalf("expected non-empty container catalog")
	}
	foundWeb := false
	for _, c := range containers {
		if c.Name == "home-web" && c.IsCore {
			foundWeb = true
		}
	}
	if !foundWeb {
		t.Errorf("home-web not found in fallback container catalog")
	}

	// Test S3 Backup status
	backup := provider.GetS3BackupStatus()
	if !backup.OK || backup.RemoteFiles != 42 || backup.Bucket == "" {
		t.Errorf("unexpected backup status: %+v", backup)
	}
}

func TestOpsHandlerEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)

	server, _ := setupMockOnePanelServer(t)
	defer server.Close()

	client := NewOnePanelClient(server.URL, "btpanel", "feiyemomo", "@Momo324")
	fallback := NewHostFallbackProvider("", "")
	db := setupTestDB(t)
	bus := eventbus.New()

	handler := NewOpsHandler(client, fallback, db, bus, nil)
	// Mock health probe in unit test harness since no live home-web listener exists on :8086
	handler.SetExecutor(NewOperationExecutor(
		db, bus, client, fallback,
		WithBaselineExtractor(handler.ComputeFingerprint),
		WithHealthChecker(func(context.Context, string) error { return nil }),
	))

	r := gin.New()
	rg := r.Group("/api/v1/ops")
	// simulate JWTAuth middleware injecting claims
	rg.Use(func(c *gin.Context) {
		c.Set("user_id", uint(1))
		c.Set("user_name", "admin_tester")
		c.Set("is_admin", c.GetHeader("X-Admin") == "true")
		c.Next()
	})
	handler.RegisterRoutes(rg)

	// 1. GET /api/v1/ops/nas/status
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/ops/nas/status", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var res map[string]any
	json.Unmarshal(w.Body.Bytes(), &res)
	if res["code"] != float64(0) {
		t.Errorf("expected response code 0, got %v", res["code"])
	}
	data, _ := res["data"].(map[string]any)
	if data["source"] != "1panel" {
		t.Errorf("expected source 1panel, got %v", data["source"])
	}

	// 2. GET /api/v1/ops/nas/containers
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/ops/nas/containers", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// 3. POST /api/v1/ops/nas/containers/home-web/restart without admin -> 403 Forbidden
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/nas/containers/home-web/restart", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-admin, got %d: %s", w.Code, w.Body.String())
	}

	// 4. POST /api/v1/ops/nas/containers/forbidden_container/restart with admin -> 400 Bad Request
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/nas/containers/forbidden_container/restart", nil)
	req.Header.Set("X-Admin", "true")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for non-whitelisted container, got %d: %s", w.Code, w.Body.String())
	}

	// 5. POST /api/v1/ops/nas/containers/home-web/restart with admin -> 200 OK & audit log created
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/ops/nas/containers/home-web/restart", nil)
	req.Header.Set("X-Admin", "true")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid restart, got %d: %s", w.Code, w.Body.String())
	}

	// Verify audit log persisted in DB
	var logEntry model.SystemLog
	if err := db.Where("event_type = ?", "ops.container.restart").First(&logEntry).Error; err != nil {
		t.Errorf("expected audit log entry in db: %v", err)
	}
	if logEntry.Context != "home-web" || !strings.Contains(logEntry.Message, "admin_tester") {
		t.Errorf("unexpected audit log entry: %+v", logEntry)
	}

	// 6. GET /api/v1/ops/nas/security-summary
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/ops/nas/security-summary", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOpsHandlerOfflineFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Client pointing to non-existent server to test fallback
	deadClient := NewOnePanelClient("http://127.0.0.1:59999", "btpanel", "feiyemomo", "@Momo324")
	fallback := NewHostFallbackProvider("", "")
	db := setupTestDB(t)
	bus := eventbus.New()

	handler := NewOpsHandler(deadClient, fallback, db, bus, nil)

	r := gin.New()
	rg := r.Group("/api/v1/ops")
	handler.RegisterRoutes(rg)

	// Status endpoint should smoothly fall back to host metrics with 200 OK
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/ops/nas/status", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on fallback, got %d: %s", w.Code, w.Body.String())
	}
	var res map[string]any
	json.Unmarshal(w.Body.Bytes(), &res)
	data, _ := res["data"].(map[string]any)
	if data["source"] != "host_fallback" || data["reachable"] != false {
		t.Errorf("expected source host_fallback, got %v", data)
	}

	// Containers endpoint should fall back to container catalog with 200 OK
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/ops/nas/containers", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on container fallback, got %d", w.Code)
	}
}
