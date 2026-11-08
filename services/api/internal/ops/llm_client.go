package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrNoLLMConfigured = errors.New("no healthy LLM provider available")
)

// Default Gemini and Groq API key pools (overridden by environment variables)
var defaultGeminiKeys = []string{}
var defaultGroqKeys = []string{}

// LLMClient manages direct connections to LLM providers (New API, Google Gemini, Groq)
// with automatic Mihomo proxy traversal and resilient multi-key fallback.
type LLMClient struct {
	httpClient  *http.Client
	geminiKeys  []string
	groqKeys    []string
	geminiIndex uint32
	groqIndex   uint32

	mu                     sync.RWMutex
	newAPICooldownUntil    time.Time
	newAPIFailCount        int
	circuitBreakerCooldown time.Time
}

// detectProxy checks environment variables and verifies local Mihomo proxy availability.
func detectProxy() string {
	if p := os.Getenv("MIHOMO_PROXY"); p != "" {
		return p
	}
	if p := os.Getenv("HTTP_PROXY"); p != "" {
		return p
	}
	if p := os.Getenv("http_proxy"); p != "" {
		return p
	}

	// Fast probe standard Docker host and localhost Mihomo proxy ports (100ms timeout)
	candidates := []string{"http://172.17.0.1:7890", "http://127.0.0.1:7890"}
	for _, cand := range candidates {
		u, err := url.Parse(cand)
		if err == nil {
			conn, dErr := net.DialTimeout("tcp", u.Host, 100*time.Millisecond)
			if dErr == nil {
				conn.Close()
				return cand
			}
		}
	}
	return ""
}

// buildHTTPClient creates an HTTP client routed through Mihomo proxy if present.
func buildHTTPClient() *http.Client {
	var transport *http.Transport
	proxyStr := detectProxy()
	if proxyStr != "" {
		if proxyURL, err := url.Parse(proxyStr); err == nil {
			transport = &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
				DialContext: (&net.Dialer{
					Timeout:   4 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout: 4 * time.Second,
			}
		}
	}
	if transport == nil {
		transport = &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   4 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout: 4 * time.Second,
		}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   6 * time.Second,
	}
}

// NewLLMClient constructs an initialized LLMClient.
func NewLLMClient() *LLMClient {
	gKeys := make([]string, 0, len(defaultGeminiKeys)+1)
	if envKey := os.Getenv("GEMINI_API_KEY"); envKey != "" {
		gKeys = append(gKeys, envKey)
	}
	gKeys = append(gKeys, defaultGeminiKeys...)

	grKeys := make([]string, 0, len(defaultGroqKeys)+1)
	if envKey := os.Getenv("GROQ_API_KEY"); envKey != "" {
		grKeys = append(grKeys, envKey)
	}
	grKeys = append(grKeys, defaultGroqKeys...)

	return &LLMClient{
		httpClient: buildHTTPClient(),
		geminiKeys: gKeys,
		groqKeys:   grKeys,
	}
}

// SetHTTPClient allows overriding the HTTP client (useful for unit tests and mocks).
func (c *LLMClient) SetHTTPClient(client *http.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.httpClient = client
}

// Call attempts invocation across configured LLM tiers:
// Tier 1: Configured New API Gateway (if explicitly set and not localhost loopback failure)
// Tier 2: Google Gemini 3.8 Flash (gemini-3.8-flash) via Mihomo proxy with key pool rotation
// Tier 3: Groq (llama-3.3-70b-versatile) via Mihomo proxy with key pool rotation
func (c *LLMClient) Call(ctx context.Context, prompt, sysContext, modelAlias string) (string, string, error) {
	c.mu.RLock()
	inCircuitBreak := time.Now().Before(c.circuitBreakerCooldown)
	c.mu.RUnlock()
	if inCircuitBreak {
		return "", "", ErrNoLLMConfigured
	}

	systemPrompt := "你是家庭数据中心的主动协作运维 Copilot (Ops Copilot)。" +
		"你已接入微服务容器总线、WAF安全哨兵、S3备份与提案推演系统。" +
		"请根据以下系统实时指标上下文，专业、清晰、自然地解答管理员的提问与指令。\n\n" +
		sysContext

	// 1. Tier 1: New API Gateway (if custom URL is provided and not in circuit break)
	newAPIURL := os.Getenv("NEW_API_URL")
	if newAPIURL != "" && newAPIURL != "http://127.0.0.1:3000/v1" {
		c.mu.RLock()
		inCooldown := time.Now().Before(c.newAPICooldownUntil)
		c.mu.RUnlock()

		if !inCooldown {
			content, err := c.callNewAPI(ctx, newAPIURL, prompt, systemPrompt, modelAlias)
			if err == nil && strings.TrimSpace(content) != "" {
				c.mu.Lock()
				c.newAPIFailCount = 0
				c.mu.Unlock()
				return content, modelAlias, nil
			}

			c.mu.Lock()
			c.newAPIFailCount++
			if c.newAPIFailCount >= 2 {
				c.newAPICooldownUntil = time.Now().Add(1 * time.Minute)
				log.Println("[llm-client] New API gateway circuit broken for 1m")
			}
			c.mu.Unlock()
		}
	}

	// 2. Tier 2: Google Gemini 3.8 Flash
	content, err := c.callGemini(ctx, prompt, systemPrompt)
	if err == nil && strings.TrimSpace(content) != "" {
		return content, "gemini-3.8-flash", nil
	}
	log.Printf("[llm-client] Gemini invocation failed: %v; falling back to Groq", err)

	// 3. Tier 3: Groq LLaMA 3.3 70B
	content, err = c.callGroq(ctx, prompt, systemPrompt)
	if err == nil && strings.TrimSpace(content) != "" {
		return content, "llama-3.3-70b-versatile", nil
	}
	log.Printf("[llm-client] Groq invocation failed: %v", err)

	c.mu.Lock()
	c.circuitBreakerCooldown = time.Now().Add(30 * time.Second)
	c.mu.Unlock()

	return "", "", ErrNoLLMConfigured
}

func (c *LLMClient) callNewAPI(ctx context.Context, apiURL, prompt, systemPrompt, modelAlias string) (string, error) {
	endpoint := strings.TrimRight(apiURL, "/") + "/chat/completions"
	payload := map[string]any{
		"model": modelAlias,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": prompt},
		},
		"temperature": 0.2,
		"max_tokens":  800,
	}

	reqBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(reqBytes))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if token := os.Getenv("NEW_API_TOKEN"); token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}

	c.mu.RLock()
	client := c.httpClient
	c.mu.RUnlock()

	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("new api HTTP %d: %s", resp.StatusCode, string(b))
	}

	var compResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&compResp); err != nil {
		return "", err
	}
	if len(compResp.Choices) == 0 {
		return "", errors.New("empty choices from New API")
	}
	return compResp.Choices[0].Message.Content, nil
}

type geminiReqPayload struct {
	SystemInstruction struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"systemInstruction"`
	Contents []struct {
		Role  string `json:"role"`
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"contents"`
	GenerationConfig struct {
		Temperature     float64 `json:"temperature"`
		MaxOutputTokens int     `json:"maxOutputTokens"`
	} `json:"generationConfig"`
}

type geminiRespPayload struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error,omitempty"`
}

func (c *LLMClient) callGemini(ctx context.Context, prompt, systemPrompt string) (string, error) {
	if len(c.geminiKeys) == 0 {
		return "", errors.New("no gemini api keys configured")
	}

	payload := geminiReqPayload{}
	payload.SystemInstruction.Parts = []struct {
		Text string `json:"text"`
	}{{Text: systemPrompt}}
	payload.Contents = []struct {
		Role  string `json:"role"`
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}{
		{
			Role: "user",
			Parts: []struct {
				Text string `json:"text"`
			}{{Text: prompt}},
		},
	}
	payload.GenerationConfig.Temperature = 0.2
	payload.GenerationConfig.MaxOutputTokens = 800

	reqBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	startIdx := int(atomic.AddUint32(&c.geminiIndex, 1))
	numKeys := len(c.geminiKeys)

	c.mu.RLock()
	client := c.httpClient
	c.mu.RUnlock()

	var lastErr error
	for i := 0; i < numKeys; i++ {
		apiKey := c.geminiKeys[(startIdx+i)%numKeys]
		endpoint := "https://generativelanguage.googleapis.com/v1beta/models/gemini-3.8-flash:generateContent"

		reqCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
		httpReq, reqErr := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(reqBytes))
		if reqErr != nil {
			cancel()
			lastErr = reqErr
			break
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("x-goog-api-key", apiKey)

		resp, doErr := client.Do(httpReq)
		if doErr != nil {
			cancel()
			lastErr = doErr
			break // Transport failure; do not retry other keys against dead host
		}

		bodyBytes, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()

		if readErr != nil {
			lastErr = readErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("gemini HTTP %d: %s", resp.StatusCode, string(bodyBytes))
			continue
		}

		var gResp geminiRespPayload
		if decErr := json.Unmarshal(bodyBytes, &gResp); decErr != nil {
			lastErr = decErr
			continue
		}
		if gResp.Error != nil {
			lastErr = fmt.Errorf("gemini API error (%d): %s", gResp.Error.Code, gResp.Error.Message)
			continue
		}
		if len(gResp.Candidates) > 0 && len(gResp.Candidates[0].Content.Parts) > 0 {
			text := gResp.Candidates[0].Content.Parts[0].Text
			if strings.TrimSpace(text) != "" {
				return text, nil
			}
		}
		lastErr = errors.New("empty text in gemini candidate")
	}

	return "", lastErr
}

func (c *LLMClient) callGroq(ctx context.Context, prompt, systemPrompt string) (string, error) {
	if len(c.groqKeys) == 0 {
		return "", errors.New("no groq api keys configured")
	}

	payload := map[string]any{
		"model": "llama-3.3-70b-versatile",
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": prompt},
		},
		"temperature": 0.2,
		"max_tokens":  800,
	}

	reqBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	startIdx := int(atomic.AddUint32(&c.groqIndex, 1))
	numKeys := len(c.groqKeys)

	c.mu.RLock()
	client := c.httpClient
	c.mu.RUnlock()

	var lastErr error
	for i := 0; i < numKeys; i++ {
		apiKey := c.groqKeys[(startIdx+i)%numKeys]
		endpoint := "https://api.groq.com/openai/v1/chat/completions"

		reqCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
		httpReq, reqErr := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(reqBytes))
		if reqErr != nil {
			cancel()
			lastErr = reqErr
			break
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)

		resp, doErr := client.Do(httpReq)
		if doErr != nil {
			cancel()
			lastErr = doErr
			break // Transport failure; do not retry other keys against dead host
		}

		bodyBytes, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()

		if readErr != nil {
			lastErr = readErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("groq HTTP %d: %s", resp.StatusCode, string(bodyBytes))
			continue
		}

		var compResp struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if decErr := json.Unmarshal(bodyBytes, &compResp); decErr != nil {
			lastErr = decErr
			continue
		}
		if len(compResp.Choices) > 0 && strings.TrimSpace(compResp.Choices[0].Message.Content) != "" {
			return compResp.Choices[0].Message.Content, nil
		}
		lastErr = errors.New("empty text in groq choice")
	}

	return "", lastErr
}
