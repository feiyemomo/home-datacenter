package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// setupMockChatHandler creates an isolated OpsHandler for chat & intent tests.
func setupMockChatHandler(t *testing.T, client *OnePanelClient, fallback *HostFallbackProvider) (*OpsHandler, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	_ = db.AutoMigrate(&model.OpsChatSession{}, &model.OpsChatMessage{}, &model.OpsProposal{})

	bus := eventbus.New()
	if fallback == nil {
		fallback = NewHostFallbackProvider("", "")
	}
	if client == nil {
		client = NewOnePanelClient("http://127.0.0.1:9", "", "", "")
		client.httpClient.Timeout = 20 * time.Millisecond
	}

	handler := NewOpsHandler(client, fallback, db, bus, nil)

	r := gin.New()
	rg := r.Group("/api/v1/ops")
	rg.Use(func(c *gin.Context) {
		c.Set("user_id", uint(1))
		c.Set("user_name", "tester")
		c.Set("is_admin", true)
		c.Next()
	})
	handler.RegisterChatRoutes(rg)

	return handler, r
}

// TestOpsChat_GenericIntent verifies that general identity queries ("你是谁", "介绍一下自己")
// return the natural assistant self-introduction instead of the static host metrics template.
func TestOpsChat_GenericIntent(t *testing.T) {
	// Ensure circuit breaker is in cooldown so it goes directly to domainReasoningFallback
	testLLM := NewLLMClient()
	testLLM.circuitBreakerCooldown = time.Now().Add(10 * time.Minute)
	setOpsLLMClient(testLLM)

	_, r := setupMockChatHandler(t, nil, nil)

	testPrompts := []string{
		"你是谁",
		"介绍下自己",
		"介绍一下自己",
		"介绍下你自己",
		"能介绍下自己吗",
		"做个自我介绍",
		"你能帮我做什么",
		"你好",
	}

	for _, prompt := range testPrompts {
		t.Run(prompt, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{
				"session_id": "session-test-intent",
				"content":    prompt,
			})
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/chat/messages", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
			}

			var resp ChatMessageResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}

			// Must contain natural assistant introduction
			if !strings.Contains(resp.Content, "家庭数据中心") || !strings.Contains(resp.Content, "Ops Copilot") {
				t.Errorf("expected Ops Copilot introduction, got: %s", resp.Content)
			}

			// MUST NOT dump the old static host metrics template
			if strings.Contains(resp.Content, "当前系统状态平稳：") && strings.Contains(resp.Content, "宿主机 CPU:") {
				t.Errorf("detected regression: generic prompt '%s' returned static host metrics template! Content: %s", prompt, resp.Content)
			}
			if strings.Contains(resp.Content, "7 个微服务网格正常连通") {
				t.Errorf("detected static template leak in '%s' response: %s", prompt, resp.Content)
			}
		})
	}
}

// TestOpsChat_ContainerResourceRanking verifies that queries asking for container rankings
// return the container ranking board with valid CPU and Memory formatting.
func TestOpsChat_ContainerResourceRanking(t *testing.T) {
	testLLM := NewLLMClient()
	testLLM.circuitBreakerCooldown = time.Now().Add(10 * time.Minute)
	setOpsLLMClient(testLLM)

	_, r := setupMockChatHandler(t, nil, nil)

	body, _ := json.Marshal(map[string]string{
		"session_id": "session-test-ranking",
		"content":    "查询容器资源消耗排行",
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/chat/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp ChatMessageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !strings.Contains(resp.Content, "【Docker 核心容器资源消耗大盘 (CPU/内存排行)】") {
		t.Errorf("expected container ranking header, got: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "home-api") {
		t.Errorf("expected core container home-api in ranking, got: %s", resp.Content)
	}
}

// TestListContainersStats verifies that OnePanelClient merges real-time CPU% and Memory MB
// from /api/v2/containers/list/stats into ContainerSummary, and populates MemoryMB for Android.
func TestListContainersStats(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]any{}})

		case "/api/v2/containers/search":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"code": 200,
				"data": onePanelContainerPage{
					Total: 2,
					Items: []onePanelContainerItem{
						{
							ContainerID: "cid-api-01",
							Name:        "/home-api",
							State:       "running",
							Status:      "Up 5 hours",
							Image:       "channels-tool/home-api:latest",
							CPUPerc:     0.0,
							MemUsage:    0,
							MemLimit:    1073741824,
							Created:     "2026-11-08T00:00:00Z",
						},
						{
							ContainerID: "cid-frigate-02",
							Name:        "/home-frigate",
							State:       "running",
							Status:      "Up 5 hours",
							Image:       "ghcr.io/blakeblackshear/frigate:0.17",
							CPUPerc:     0.0,
							MemUsage:    0,
							MemLimit:    2147483648,
							Created:     "2026-11-08T00:00:00Z",
						},
					},
				},
			})

		case "/api/v2/containers/list/stats":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"code": 200,
				"data": []onePanelStatsItem{
					{
						ContainerID:   "cid-api-01",
						CPUPercent:    7.8,
						MemoryUsage:   268435456, // 256 MB
						MemoryLimit:   1073741824,
						MemoryPercent: 25.0,
					},
					{
						ContainerID:   "cid-frigate-02",
						CPUPercent:    18.4,
						MemoryUsage:   838860800, // 800 MB
						MemoryLimit:   2147483648,
						MemoryPercent: 39.0,
					},
				},
			})

		default:
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"code": 200})
		}
	}))
	defer server.Close()

	client := NewOnePanelClient(server.URL, "btpanel", "admin", "pass")
	// simulate pre-authenticated session
	client.psession = "mock-session"
	client.pcsrftoken = "mock-token"
	client.lastLoginAt = time.Now()

	summaries, err := client.ListContainers(context.Background())
	if err != nil {
		t.Fatalf("unexpected ListContainers error: %v", err)
	}

	if len(summaries) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(summaries))
	}

	summaryMap := make(map[string]ContainerSummary)
	for _, s := range summaries {
		summaryMap[s.Name] = s
	}

	apiSum, ok := summaryMap["home-api"]
	if !ok {
		t.Fatalf("home-api not found in summaries")
	}
	if apiSum.CPUPercent != 7.8 {
		t.Errorf("expected home-api CPU 7.8%%, got %.2f%%", apiSum.CPUPercent)
	}
	if apiSum.MemoryUsageMB != 256.0 {
		t.Errorf("expected home-api MemoryUsageMB 256.0 MB, got %.2f MB", apiSum.MemoryUsageMB)
	}
	// Verify Android compatibility field
	if apiSum.MemoryMB != 256.0 {
		t.Errorf("expected home-api MemoryMB 256.0 MB, got %.2f MB", apiSum.MemoryMB)
	}

	frigateSum, ok := summaryMap["home-frigate"]
	if !ok {
		t.Fatalf("home-frigate not found in summaries")
	}
	if frigateSum.CPUPercent != 18.4 {
		t.Errorf("expected home-frigate CPU 18.4%%, got %.2f%%", frigateSum.CPUPercent)
	}
	if frigateSum.MemoryUsageMB != 800.0 {
		t.Errorf("expected home-frigate MemoryUsageMB 800.0 MB, got %.2f MB", frigateSum.MemoryUsageMB)
	}
	if frigateSum.MemoryMB != 800.0 {
		t.Errorf("expected home-frigate MemoryMB 800.0 MB, got %.2f MB", frigateSum.MemoryMB)
	}
}

// TestDockerStatsCalculation verifies genuine cgroup stats calculation in parseDockerStats.
func TestDockerStatsCalculation(t *testing.T) {
	// Construct genuine Docker cgroups stats payload:
	// CPU total usage increased by 100,000,000 ns out of 1,000,000,000 ns system delta across 4 cores -> 40.0%
	// Memory usage 524,288,000 bytes -> 500.0 MB
	rawStats := map[string]any{
		"cpu_stats": map[string]any{
			"cpu_usage": map[string]any{
				"total_usage":  1100000000,
				"percpu_usage": []uint64{300000000, 300000000, 250000000, 250000000},
			},
			"system_cpu_usage": 10000000000,
			"online_cpus":      4,
		},
		"precpu_stats": map[string]any{
			"cpu_usage": map[string]any{
				"total_usage": 1000000000,
			},
			"system_cpu_usage": 9000000000,
			"online_cpus":      4,
		},
		"memory_stats": map[string]any{
			"usage": 524288000,
			"limit": 17179869184,
		},
	}

	payload, err := json.Marshal(rawStats)
	if err != nil {
		t.Fatalf("failed to marshal raw stats: %v", err)
	}

	cpu, memMB, memLimitMB := parseDockerStats(payload)

	// (100,000,000 / 1,000,000,000) * 4 * 100 = 40.0%
	if cpu != 40.0 {
		t.Errorf("expected CPU 40.0%%, got %.2f%%", cpu)
	}
	// 524288000 / 1024 / 1024 = 500.0 MB
	if memMB != 500.0 {
		t.Errorf("expected MemMB 500.0 MB, got %.2f MB", memMB)
	}
	// 17179869184 / 1024 / 1024 = 16384.0 MB
	if memLimitMB != 16384.0 {
		t.Errorf("expected MemLimitMB 16384.0 MB, got %.2f MB", memLimitMB)
	}
}

type testRoundTripper func(*http.Request) (*http.Response, error)

func (f testRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestLLMClient_InvocationWithMockServer tests the multi-tier LLM invocation:
// Gemini -> Groq fallback -> Domain engine.
func TestLLMClient_InvocationWithMockServer(t *testing.T) {
	var geminiFails bool
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "generateContent") {
			if geminiFails {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"error":{"code":500,"message":"gemini internal error"}}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"candidates": []map[string]any{
					{
						"content": map[string]any{
							"parts": []map[string]any{
								{"text": "我是由 Google Gemini 驱动的家庭数据中心运维 Copilot。"},
							},
						},
					},
				},
			})
			return
		}

		if strings.Contains(r.URL.Path, "chat/completions") {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{
						"message": map[string]any{
							"role":    "assistant",
							"content": "我是由 Groq LLaMA 3.3 驱动的家庭数据中心运维 Copilot。",
						},
					},
				},
			})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockServer.Close()

	// 1. Test Gemini success
	geminiFails = false
	client := NewLLMClient()
	client.geminiKeys = []string{"test-key"}
	client.groqKeys = []string{"test-groq-key"}

	// Route traffic through mock server transport
	customClient := &http.Client{
		Transport: testRoundTripper(func(req *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			mockServer.Config.Handler.ServeHTTP(rec, req)
			return rec.Result(), nil
		}),
		Timeout: 2 * time.Second,
	}
	client.SetHTTPClient(customClient)

	content, modelName, err := client.Call(context.Background(), "你是谁", "sysContext", "agent-fast")
	if err != nil {
		t.Fatalf("expected call success, got: %v", err)
	}
	if !strings.Contains(content, "Google Gemini") {
		t.Errorf("expected Gemini response, got: %s", content)
	}
	if modelName != "gemini-3.8-flash" {
		t.Errorf("expected model gemini-3.8-flash, got: %s", modelName)
	}

	// 2. Test Gemini failure -> Groq fallback
	geminiFails = true
	client2 := NewLLMClient()
	client2.geminiKeys = []string{"test-key"}
	client2.groqKeys = []string{"test-groq-key"}
	client2.SetHTTPClient(customClient)

	content2, modelName2, err2 := client2.Call(context.Background(), "你是谁", "sysContext", "agent-fast")
	if err2 != nil {
		t.Fatalf("expected fallback to Groq success, got: %v", err2)
	}
	if !strings.Contains(content2, "Groq LLaMA") {
		t.Errorf("expected Groq response, got: %s", content2)
	}
	if modelName2 != "llama-3.3-70b-versatile" {
		t.Errorf("expected model llama-3.3-70b-versatile, got: %s", modelName2)
	}
}

// TestOpsLLMClient_LifecycleAndInjectionNoClobber verifies that injecting a mock client via setOpsLLMClient
// before getOpsLLMClient() is called is never clobbered by lazy initialization, and that concurrent access is thread-safe.
func TestOpsLLMClient_LifecycleAndInjectionNoClobber(t *testing.T) {
	// Clean reset
	setOpsLLMClient(nil)
	defer setOpsLLMClient(nil)

	// Step 1: Inject mock client before getOpsLLMClient() is ever called
	mockClient := NewLLMClient()
	mockClient.circuitBreakerCooldown = time.Now().Add(10 * time.Minute)
	setOpsLLMClient(mockClient)

	// Step 2: Multiple concurrent calls to getOpsLLMClient() should all return mockClient
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := getOpsLLMClient()
			if client == nil {
				t.Errorf("getOpsLLMClient returned nil")
				return
			}
			if client != mockClient {
				t.Errorf("mock client was clobbered by lazy initialization! expected %p, got %p", mockClient, client)
			}
			if !client.circuitBreakerCooldown.After(time.Now()) {
				t.Errorf("mock client state was mutated or lost")
			}
		}()
	}
	wg.Wait()

	// Step 3: Verify reset and re-initialization
	setOpsLLMClient(nil)
	newClient := getOpsLLMClient()
	if newClient == nil {
		t.Fatalf("expected getOpsLLMClient to initialize default client when nil")
	}
	if newClient == mockClient {
		t.Errorf("expected new client instance, got old mockClient")
	}

	// Step 4: Verify setOpsLLMClient overwrites cleanly
	mockClient2 := NewLLMClient()
	setOpsLLMClient(mockClient2)
	if got := getOpsLLMClient(); got != mockClient2 {
		t.Errorf("expected mockClient2 (%p), got %p", mockClient2, got)
	}
}

