package ops

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"home-datacenter-api/internal/model"
)

func TestParseDockerImageRef(t *testing.T) {
	cases := []struct {
		in   string
		repo string
		tag  string
	}{
		{"nginx", "nginx", "latest"},
		{"ghcr.io/foo/bar:v1.2.3", "ghcr.io/foo/bar", "v1.2.3"},
		{"localhost:5000/myapp", "localhost:5000/myapp", "latest"},
		{"localhost:5000/myapp:dev", "localhost:5000/myapp", "dev"},
		{"nginx@sha256:abcdef", "nginx@sha256:abcdef", ""},
	}
	for _, tc := range cases {
		r, tag := parseDockerImageRef(tc.in)
		if r != tc.repo || tag != tc.tag {
			t.Fatalf("parseDockerImageRef(%q) = (%q, %q), want (%q, %q)", tc.in, r, tag, tc.repo, tc.tag)
		}
	}
}

func TestDetectActionProposalDockerImageUpdate(t *testing.T) {
	h := &OpsHandler{}
	draft := h.detectActionProposal("请更新 frigate 镜像到 ghcr.io/blakeblackshear/frigate:0.15.0", 1, 1)
	if draft == nil {
		t.Fatal("expected ActionProposalDraft, got nil")
	}
	if draft.Action != ActionDockerUpdateImage {
		t.Fatalf("expected action %s, got %s", ActionDockerUpdateImage, draft.Action)
	}
	if draft.TargetComponent != ComponentFrigate {
		t.Fatalf("expected target %s, got %s", ComponentFrigate, draft.TargetComponent)
	}
	if draft.Params["new_image"] != "ghcr.io/blakeblackshear/frigate:0.15.0" {
		t.Fatalf("expected new_image param, got %v", draft.Params)
	}
}

func TestPullDockerImageStreamErrorAndSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "docker.sock")
	if os.PathSeparator == '\\' {
		// On Windows, unix domain sockets in TempDir work with Go net.Listen("unix", ...),
		// or skip if unsupported on older builds.
	}

	var returnStreamErr bool
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/images/create") {
			w.WriteHeader(http.StatusOK)
			if returnStreamErr {
				_, _ = w.Write([]byte("{\"status\":\"Pulling\"}\n{\"error\":\"manifest unknown\"}\n"))
			} else {
				_, _ = w.Write([]byte("{\"status\":\"Pulling from library/nginx\"}\n{\"status\":\"Status: Downloaded newer image\"}\n"))
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Skipf("unix socket listen unsupported in test env: %v", err)
	}
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	exec := &DefaultOperationExecutor{dockerSocket: sockPath}
	if err := exec.pullDockerImage(context.Background(), sockPath, "home-web", "nginx:latest"); err != nil {
		t.Fatalf("expected pullDockerImage success, got %v", err)
	}

	returnStreamErr = true
	if err := exec.pullDockerImage(context.Background(), sockPath, "home-web", "nginx:bad"); err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatalf("expected manifest unknown error, got %v", err)
	}
}

func TestCheckDockerImageUpdatesCreatesProposal(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.OpsProposal{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "docker.sock")
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/home-frigate/json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Image":  "sha256:old123",
				"Config": map[string]any{"Image": "ghcr.io/blakeblackshear/frigate:stable"},
			})
		case r.URL.Path == "/images/sha256:old123/json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"RepoDigests": []string{"ghcr.io/blakeblackshear/frigate@sha256:olddigest"},
			})
		case strings.HasPrefix(r.URL.Path, "/distribution/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Descriptor": map[string]any{"digest": "sha256:newdigest"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Skipf("unix socket listen unsupported in test env: %v", err)
	}
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	scanner := NewVulnScanner(db, nil)
	created := scanner.CheckDockerImageUpdates(context.Background(), sockPath)
	if created != 1 {
		t.Fatalf("expected 1 proposal created, got %d", created)
	}
	// Second run must be idempotent
	if created2 := scanner.CheckDockerImageUpdates(context.Background(), sockPath); created2 != 0 {
		t.Fatalf("expected 0 duplicate proposals, got %d", created2)
	}
}

func TestFetchRegistryV2DigestWithBearerAuth(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-tok"})
		case "/v2/tailscale/tailscale/manifests/latest":
			if r.Header.Get("Authorization") != "Bearer test-tok" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="registry.docker.io",scope="repository:tailscale/tailscale:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Docker-Content-Digest", "sha256:remote123")
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	hostPort := strings.TrimPrefix(srv.URL, "http://")
	digest, err := fetchRegistryV2Digest(context.Background(), srv.Client(), hostPort+"/tailscale/tailscale:latest")
	if err != nil {
		t.Fatalf("expected digest fetch success, got %v", err)
	}
	if digest != "sha256:remote123" {
		t.Fatalf("expected sha256:remote123, got %q", digest)
	}
}

func TestDiscoverRunningImageTargetsIncludesTailscale(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "ts*")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)
	sockPath := filepath.Join(tmpDir, "d.sock")
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/containers/json":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"Names": []string{"/tailscale"}, "Image": "tailscale/tailscale:latest", "ImageID": "sha256:ts1"},
				{"Names": []string{"/home-api"}, "Image": "home-datacenter-api", "ImageID": "sha256:api1"},
			})
		case "/images/sha256:ts1/json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"RepoDigests": []string{"tailscale/tailscale@sha256:oldts"},
			})
		case "/images/sha256:api1/json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"RepoDigests": []string{},
			})
		case "/distribution/tailscale/tailscale:latest/json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Descriptor": map[string]any{"digest": "sha256:newts"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Skipf("unix socket listen unsupported in test env: %v", err)
	}
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.OpsProposal{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	scanner := NewVulnScanner(db, nil)
	created := scanner.CheckDockerImageUpdates(context.Background(), sockPath)
	if created != 1 {
		t.Fatalf("expected 1 proposal for tailscale via dynamic discovery + distribution fallback, got %d", created)
	}
	var prop model.OpsProposal
	if err := db.First(&prop, "target_component = ?", ComponentTailscale).Error; err != nil {
		t.Fatalf("expected tailscale proposal in DB: %v", err)
	}
}
