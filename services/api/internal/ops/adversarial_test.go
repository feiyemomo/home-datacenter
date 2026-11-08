package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// =========================================================================
// SECTION 1: 1Panel & Docker Stats Stress Tests
// =========================================================================

func TestParseDockerStats_AdversarialStress(t *testing.T) {
	t.Run("ZeroSystemDelta_DivideByZeroProtection", func(t *testing.T) {
		// Both precpu and cpu have same system_cpu_usage -> systemDelta = 0
		rawStats := map[string]any{
			"cpu_stats": map[string]any{
				"cpu_usage": map[string]any{
					"total_usage":  1500000000,
					"percpu_usage": []uint64{750000000, 750000000},
				},
				"system_cpu_usage": 5000000000,
				"online_cpus":      2,
			},
			"precpu_stats": map[string]any{
				"cpu_usage": map[string]any{
					"total_usage": 1000000000,
				},
				"system_cpu_usage": 5000000000, // Identical to cpu_stats -> systemDelta = 0!
				"online_cpus":      2,
			},
			"memory_stats": map[string]any{
				"usage": 104857600, // 100 MB
				"limit": 1073741824,
			},
		}

		b, _ := json.Marshal(rawStats)
		cpu, memMB, memLimitMB := parseDockerStats(b)

		// Must NOT be NaN or Inf, must safely result in 0.0% CPU
		if math.IsNaN(cpu) || math.IsInf(cpu, 0) {
			t.Fatalf("divide-by-zero produced NaN or Inf for CPU percent: %v", cpu)
		}
		if cpu != 0.0 {
			t.Errorf("expected 0.0%% CPU when system delta is 0, got %.2f%%", cpu)
		}
		if memMB != 100.0 {
			t.Errorf("expected 100.0 MB memory, got %.2f", memMB)
		}
		if memLimitMB != 1024.0 {
			t.Errorf("expected 1024.0 MB limit, got %.2f", memLimitMB)
		}
	})

	t.Run("NegativeDeltaProtection", func(t *testing.T) {
		// CPU or system delta negative
		rawStats := map[string]any{
			"cpu_stats": map[string]any{
				"cpu_usage": map[string]any{
					"total_usage": 500000000,
				},
				"system_cpu_usage": 4000000000,
				"online_cpus":      1,
			},
			"precpu_stats": map[string]any{
				"cpu_usage": map[string]any{
					"total_usage": 1000000000, // PreCPU > CPU -> negative cpuDelta
				},
				"system_cpu_usage": 5000000000, // PreCPU > CPU -> negative systemDelta
				"online_cpus":      1,
			},
			"memory_stats": map[string]any{
				"usage": 0,
				"limit": 0,
			},
		}

		b, _ := json.Marshal(rawStats)
		cpu, memMB, memLimitMB := parseDockerStats(b)

		if math.IsNaN(cpu) || math.IsInf(cpu, 0) || cpu < 0 {
			t.Fatalf("unexpected negative or invalid CPU: %v", cpu)
		}
		if cpu != 0.0 {
			t.Errorf("expected 0.0%% CPU on negative delta, got %.2f", cpu)
		}
		if memMB != 0.0 || memLimitMB != 0.0 {
			t.Errorf("expected 0.0 memory, got memMB=%.2f, limit=%.2f", memMB, memLimitMB)
		}
	})

	t.Run("ZeroOnlineCPUs_FallbackToPercpuOrOne", func(t *testing.T) {
		// OnlineCPUs = 0 and percpu_usage empty
		rawStats := map[string]any{
			"cpu_stats": map[string]any{
				"cpu_usage": map[string]any{
					"total_usage":  200000000,
					"percpu_usage": []uint64{},
				},
				"system_cpu_usage": 2000000000,
				"online_cpus":      0,
			},
			"precpu_stats": map[string]any{
				"cpu_usage": map[string]any{
					"total_usage": 100000000,
				},
				"system_cpu_usage": 1000000000,
				"online_cpus":      0,
			},
			"memory_stats": map[string]any{
				"usage": 52428800, // 50 MB
				"limit": 1073741824,
			},
		}

		b, _ := json.Marshal(rawStats)
		cpu, memMB, _ := parseDockerStats(b)

		// (100,000,000 / 1,000,000,000) * 1 (fallback core) * 100 = 10.0%
		if cpu != 10.0 {
			t.Errorf("expected 10.0%% with single fallback core, got %.2f%%", cpu)
		}
		if memMB != 50.0 {
			t.Errorf("expected 50.0 MB memory, got %.2f", memMB)
		}
	})

	t.Run("MalformedAndEmptyPayloads", func(t *testing.T) {
		badPayloads := [][]byte{
			[]byte(""),
			[]byte("{"),
			[]byte("null"),
			[]byte("[]"),
			[]byte("<html><head><title>502 Bad Gateway</title></head></html>"),
			[]byte(`{"cpu_stats": "not_an_object"}`),
			[]byte(`{"memory_stats": {"usage": "invalid_number"}}`),
		}

		for _, p := range badPayloads {
			cpu, mem, limit := parseDockerStats(p)
			if cpu != 0.0 || mem != 0.0 || limit != 0.0 {
				t.Errorf("malformed payload should return 0, 0, 0, got cpu=%.2f mem=%.2f limit=%.2f", cpu, mem, limit)
			}
		}
	})

	t.Run("MemoryMBFormattingAndPrecision", func(t *testing.T) {
		// Exact 1 byte, 1024 bytes, 1048576 bytes
		rawStats := map[string]any{
			"memory_stats": map[string]any{
				"usage": 1048576,    // 1.0 MB
				"limit": 2147483648, // 2048.0 MB
			},
		}
		b, _ := json.Marshal(rawStats)
		_, memMB, limitMB := parseDockerStats(b)

		if memMB != 1.0 {
			t.Errorf("expected exactly 1.0 MB, got %.2f", memMB)
		}
		if limitMB != 2048.0 {
			t.Errorf("expected exactly 2048.0 MB, got %.2f", limitMB)
		}
	})
}

func Test1PanelClient_DegradedStatsEndpoint(t *testing.T) {
	// Simulate 1Panel returning valid container list, but stats endpoint returning 500 error / invalid JSON
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/containers/search":
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{
				"code": 200,
				"data": onePanelContainerPage{
					Total: 1,
					Items: []onePanelContainerItem{
						{
							ContainerID: "cid-001",
							Name:        "/home-api",
							State:       "running",
							Status:      "Up 10 hours",
							Image:       "home-api:v1",
							CPUPerc:     2.5,
							MemUsage:    134217728, // 128 MB
							MemLimit:    1073741824,
							Created:     "2026-11-08T00:00:00Z",
						},
					},
				},
			})

		case "/api/v2/containers/list/stats":
			// Simulate 500 internal server error or invalid JSON
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("500 Internal Server Error: Stats engine unavailable"))

		default:
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"code": 200})
		}
	}))
	defer server.Close()

	client := NewOnePanelClient(server.URL, "btpanel", "admin", "pass")
	client.psession = "mock-session"
	client.pcsrftoken = "mock-token"
	client.lastLoginAt = time.Now()

	// Should NOT fail or panic: gracefully falls back to item.CPUPerc and item.MemUsage
	summaries, err := client.ListContainers(context.Background())
	if err != nil {
		t.Fatalf("expected successful ListContainers despite stats endpoint failure, got err: %v", err)
	}

	if len(summaries) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(summaries))
	}

	s := summaries[0]
	if s.Name != "home-api" {
		t.Errorf("expected home-api, got %s", s.Name)
	}
	if s.CPUPercent != 2.5 {
		t.Errorf("expected fallback CPU 2.5%%, got %.2f%%", s.CPUPercent)
	}
	if s.MemoryUsageMB != 128.0 || s.MemoryMB != 128.0 {
		t.Errorf("expected fallback memory 128.0 MB, got UsageMB=%.2f, MB=%.2f", s.MemoryUsageMB, s.MemoryMB)
	}
}

// =========================================================================
// SECTION 2: Copilot Intent & Input Stress Tests
// =========================================================================

func TestCopilotIntent_EmptyAndWhitespaceInputs(t *testing.T) {
	// Execute sync.Once first, then override with mock LLM client
	_ = getOpsLLMClient()
	testLLM := NewLLMClient()
	testLLM.circuitBreakerCooldown = time.Now().Add(10 * time.Minute)
	setOpsLLMClient(testLLM)

	_, r := setupMockChatHandler(t, nil, nil)

	testCases := []struct {
		name       string
		payload    map[string]any
		expectCode int
		expectErr  string
	}{
		{
			name:       "EmptyContent",
			payload:    map[string]any{"content": ""},
			expectCode: http.StatusBadRequest,
			expectErr:  "Message content cannot be empty",
		},
		{
			name:       "WhitespaceOnlyContent",
			payload:    map[string]any{"content": "    \t \r \n   "},
			expectCode: http.StatusBadRequest,
			expectErr:  "Message content cannot be empty",
		},
		{
			name:       "EmptyBothContentAndMessage",
			payload:    map[string]any{"content": "", "message": "   "},
			expectCode: http.StatusBadRequest,
			expectErr:  "Message content cannot be empty",
		},
		{
			name:       "FallbackToLegacyMessageField",
			payload:    map[string]any{"content": "", "message": "你是谁"},
			expectCode: http.StatusOK,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(tc.payload)
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/chat/messages", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)

			if w.Code != tc.expectCode {
				t.Fatalf("expected status %d, got %d: %s", tc.expectCode, w.Code, w.Body.String())
			}
			if tc.expectErr != "" && !strings.Contains(w.Body.String(), tc.expectErr) {
				t.Errorf("expected error message %q in response: %s", tc.expectErr, w.Body.String())
			}
		})
	}
}

func TestCopilotIntent_GreetingKeywordVariations(t *testing.T) {
	_ = getOpsLLMClient()
	testLLM := NewLLMClient()
	testLLM.circuitBreakerCooldown = time.Now().Add(10 * time.Minute)
	setOpsLLMClient(testLLM)

	_, r := setupMockChatHandler(t, nil, nil)

	greetings := []string{
		"你是谁",
		"介绍下自己",
		"介绍一下自己",
		"介绍下你自己",
		"做个自我介绍",
		"自我介绍",
		"你是做什么的",
		"你有什么功能",
		"你能帮我做什么",
		"你能做什么",
		"你是哪位",
		"谁是copilot",
		"你好",
		"您好",
		"在吗",
		"hello",
		"hi",
		"who are you",
		"what can you do",
		"help",
		"功能介绍",
	}

	for _, g := range greetings {
		t.Run(g, func(t *testing.T) {
			b, _ := json.Marshal(map[string]string{"content": g})
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/chat/messages", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("greeting %q returned %d: %s", g, w.Code, w.Body.String())
			}

			var resp ChatMessageResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to decode response for %q: %v", g, err)
			}

			// Must contain assistant identity
			if !strings.Contains(resp.Content, "Ops Copilot") {
				t.Errorf("greeting %q failed to return Ops Copilot self-identity, got: %s", g, resp.Content)
			}
			// Must NOT contain static host metrics template
			if strings.Contains(resp.Content, "宿主机 CPU:") && strings.Contains(resp.Content, "7 个微服务网格正常连通") {
				t.Errorf("greeting %q returned static host metrics template!", g)
			}
		})
	}
}

func TestCopilotIntent_CombinedDomainVsGreeting(t *testing.T) {
	testLLM := NewLLMClient()
	testLLM.circuitBreakerCooldown = time.Now().Add(10 * time.Minute)
	setOpsLLMClient(testLLM)

	_, r := setupMockChatHandler(t, nil, nil)

	// When a query contains both a greeting and a domain keyword (e.g. "你好，帮我查询容器资源消耗排行")
	// The domain keyword should take precedence and return the container ranking board!
	query := "你好，请问你是谁？顺便帮我查询容器资源消耗排行"
	b, _ := json.Marshal(map[string]string{"content": query})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/chat/messages", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("query returned %d: %s", w.Code, w.Body.String())
	}

	var resp ChatMessageResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if !strings.Contains(resp.Content, "【Docker 核心容器资源消耗大盘 (CPU/内存排行)】") {
		t.Errorf("domain keyword precedence failed, got: %s", resp.Content)
	}
}

func TestCopilotIntent_ComplexAndUltraLongQuery(t *testing.T) {
	testLLM := NewLLMClient()
	testLLM.circuitBreakerCooldown = time.Now().Add(10 * time.Minute)
	setOpsLLMClient(testLLM)

	_, r := setupMockChatHandler(t, nil, nil)

	// Ultra-long input (10,000 characters)
	longQuery := "你好 " + strings.Repeat("系统运行是否正常？", 1000)
	b, _ := json.Marshal(map[string]string{"content": longQuery})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/ops/chat/messages", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("ultra long query failed with %d: %s", w.Code, w.Body.String())
	}

	var resp ChatMessageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if strings.TrimSpace(resp.Content) == "" {
		t.Errorf("ultra long query returned empty content")
	}
}

// =========================================================================
// SECTION 3: LLM Client Circuit Breaker Stress Tests
// =========================================================================

func TestLLMClient_CircuitBreakerBehavior(t *testing.T) {
	t.Run("NewAPIFailoverAndCooldown", func(t *testing.T) {
		failCount := 0
		var mu sync.Mutex

		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			if strings.Contains(r.URL.Path, "chat/completions") && !strings.Contains(r.URL.Host, "groq") {
				failCount++
				w.WriteHeader(http.StatusBadGateway)
				w.Write([]byte("502 Bad Gateway"))
				return
			}
			if strings.Contains(r.URL.Path, "generateContent") {
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]any{
					"candidates": []map[string]any{
						{
							"content": map[string]any{
								"parts": []map[string]any{
									{"text": "Gemini fallback reply"},
								},
							},
						},
					},
				})
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer mockServer.Close()

		client := NewLLMClient()
		client.geminiKeys = []string{"test-gemini-key"}
		client.groqKeys = nil
		client.SetHTTPClient(&http.Client{
			Transport: testRoundTripper(func(req *http.Request) (*http.Response, error) {
				rec := httptest.NewRecorder()
				mockServer.Config.Handler.ServeHTTP(rec, req)
				return rec.Result(), nil
			}),
			Timeout: 2 * time.Second,
		})

		// Set custom NEW_API_URL pointing to mock server
		t.Setenv("NEW_API_URL", "http://new-api.local")

		// First call: New API fails once, falls back to Gemini
		content1, model1, err1 := client.Call(context.Background(), "test1", "ctx", "agent-fast")
		if err1 != nil || model1 != "gemini-3.8-flash" || !strings.Contains(content1, "Gemini fallback") {
			t.Fatalf("expected Gemini fallback success, got model=%s, err=%v", model1, err1)
		}

		// Second call: New API fails twice -> triggers newAPICooldownUntil (1 minute)
		content2, model2, err2 := client.Call(context.Background(), "test2", "ctx", "agent-fast")
		if err2 != nil || model2 != "gemini-3.8-flash" {
			t.Fatalf("expected Gemini fallback on second fail, got model=%s, err=%v", model2, err2)
		}

		client.mu.RLock()
		inCooldown := time.Now().Before(client.newAPICooldownUntil)
		client.mu.RUnlock()

		if !inCooldown {
			t.Errorf("expected New API to be in cooldown after 2 consecutive failures")
		}

		// Third call: New API should be bypassed immediately without hitting mockServer
		currentFails := failCount
		content3, model3, err3 := client.Call(context.Background(), "test3", "ctx", "agent-fast")
		if err3 != nil || model3 != "gemini-3.8-flash" {
			t.Fatalf("expected direct Gemini call during cooldown, got %v", err3)
		}
		_ = content2
		_ = content3

		mu.Lock()
		if failCount > currentFails {
			t.Errorf("New API was invoked during cooldown period! failCount=%d, expected=%d", failCount, currentFails)
		}
		mu.Unlock()
	})

	t.Run("FullCircuitBreakerFastFail", func(t *testing.T) {
		// Mock server where all endpoints return 500
		mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"upstream down"}`))
		}))
		defer mockServer.Close()

		client := NewLLMClient()
		client.geminiKeys = []string{"k1"}
		client.groqKeys = []string{"k2"}
		client.SetHTTPClient(mockServer.Client())

		// First call triggers Gemini fail and Groq fail -> sets circuitBreakerCooldown for 30s
		start := time.Now()
		_, _, err := client.Call(context.Background(), "test", "ctx", "agent-fast")
		if !errors.Is(err, ErrNoLLMConfigured) {
			t.Fatalf("expected ErrNoLLMConfigured, got %v", err)
		}

		client.mu.RLock()
		inCB := time.Now().Before(client.circuitBreakerCooldown)
		client.mu.RUnlock()
		if !inCB {
			t.Errorf("expected client to be in circuit breaker cooldown")
		}

		// Second call during circuit break MUST return immediately (< 5ms) without doing HTTP roundtrips
		fastStart := time.Now()
		_, _, errCB := client.Call(context.Background(), "test-fast", "ctx", "agent-fast")
		elapsed := time.Since(fastStart)

		if !errors.Is(errCB, ErrNoLLMConfigured) {
			t.Errorf("expected ErrNoLLMConfigured on circuit break, got %v", errCB)
		}
		if elapsed > 10*time.Millisecond {
			t.Errorf("circuit breaker did not fail fast! took %v", elapsed)
		}
		_ = start
	})
}
