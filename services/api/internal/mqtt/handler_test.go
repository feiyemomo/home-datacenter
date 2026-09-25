package mqtt

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"home-datacenter-api/internal/camera"
	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/vision"
)

func TestParseStatusPayload(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		wantStat string
		wantTS   int64
		wantOK   bool
	}{
		{"strict quoted", `{"status":"online","ts":1234567890}`, "online", 1234567890, true},
		{"strict reversed", `{"ts":42,"status":"offline"}`, "offline", 42, true},
		{"unquoted keys and values", `{status:online,ts:1234567890}`, "online", 1234567890, true},
		{"unquoted keys, quoted values", `{"status":"heartbeat","ts":99}`, "heartbeat", 99, true},
		{"bareword status only", `status=offline`, "offline", 0, true},
		{"garbage rejected", `not-json-at-all`, "", 0, false},
		{"empty rejected", ``, "", 0, false},
		{"unknown status still accepted at parse layer", `{"status":"weird"}`, "weird", 0, true},
		{"nested arrays tolerated", `{"status":"online","tags":["a","b"]}`, "online", 0, true},
		{"quoted key with embedded comma", `{"a,b":"x","status":"online"}`, "online", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStatus, gotTS, gotOK := parseStatusPayload([]byte(tc.payload))
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v (status=%q ts=%d)", gotOK, tc.wantOK, gotStatus, gotTS)
			}
			if !gotOK {
				return
			}
			if gotStatus != tc.wantStat {
				t.Errorf("status = %q, want %q", gotStatus, tc.wantStat)
			}
			if gotTS != tc.wantTS {
				t.Errorf("ts = %d, want %d", gotTS, tc.wantTS)
			}
		})
	}
}

func TestLenientJSON(t *testing.T) {
	// lenientJSON is the fallback path. When the input is already
	// well-formed JSON, it returns nil so the strict decoder above
	// owns the conversion. When the input is unquoted-keyed, it
	// rewrites it into a strict object. When the input is not an
	// object at all, it returns nil.
	cases := []struct {
		name string
		in   string
		want string // empty == expect nil result
	}{
		{"unquoted status and ts", `{status:online,ts:1234567890}`, `{"status":"online","ts":1234567890}`},
		{"already strict", `{"status":"online","ts":1}`, ""},
		{"value with spaces", `{ status : online , ts : 5 }`, `{"status":"online","ts":5}`},
		{"trailing comma on strict input", `{"status":"online","ts":1,}`, ""},
		{"empty object", `{}`, ""},
		{"non-object rejected", `[]`, ""},
		{"nested braces OK strict", `{"a":{"b":1},"status":"online"}`, ""},
		{"nested braces in unquoted", `{a:{b:1},status:online}`, `{"a":"{b:1}","status":"online"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lenientJSON([]byte(tc.in))
			if tc.want == "" {
				if got != nil {
					t.Fatalf("want nil, got %q", got)
				}
				return
			}
			if string(got) != tc.want {
				t.Errorf("lenientJSON(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPoseRetry_DeduplicationAndCancellation(t *testing.T) {
	h := NewHandler(nil, nil, nil)
	// Wire dummy clients so schedulePoseRetry doesn't short-circuit
	h.visionClient = &vision.Client{}
	h.frigateClient = &camera.FrigateClient{}

	task1 := visionTask{eventID: "ev-1", cameraID: 1, slug: "living_room", ts: 1000}
	task2 := visionTask{eventID: "ev-2", cameraID: 1, slug: "living_room", ts: 1005}

	h.schedulePoseRetry(task1)
	if len(h.pendingRetry) != 1 {
		t.Fatalf("expected 1 pending retry, got %d", len(h.pendingRetry))
	}
	if h.pendingRetry[1].task.eventID != "ev-1" {
		t.Errorf("expected task ev-1, got %s", h.pendingRetry[1].task.eventID)
	}

	// Newer event on same camera should overwrite (deduplication)
	h.schedulePoseRetry(task2)
	if len(h.pendingRetry) != 1 {
		t.Fatalf("expected 1 pending retry after overwrite, got %d", len(h.pendingRetry))
	}
	if h.pendingRetry[1].task.eventID != "ev-2" {
		t.Errorf("expected task ev-2, got %s", h.pendingRetry[1].task.eventID)
	}

	// Cancel retry (e.g. after normal full detection)
	h.cancelPoseRetry(1)
	if len(h.pendingRetry) != 0 {
		t.Errorf("expected 0 pending retries after cancel, got %d", len(h.pendingRetry))
	}
}

func TestPoseRetry_Expiration(t *testing.T) {
	h := NewHandler(nil, nil, nil)
	h.visionClient = &vision.Client{}
	h.frigateClient = &camera.FrigateClient{}

	task := visionTask{eventID: "ev-expired", cameraID: 2, slug: "front_door", ts: 2000}
	h.schedulePoseRetry(task)

	// Simulate expired deadline (past 1 min)
	h.retryMu.Lock()
	h.pendingRetry[2].deadline = time.Now().Add(-1 * time.Second)
	h.retryMu.Unlock()

	h.checkAndRunPendingRetries()

	h.retryMu.Lock()
	defer h.retryMu.Unlock()
	if len(h.pendingRetry) != 0 {
		t.Errorf("expected expired retry to be evicted, but %d remain", len(h.pendingRetry))
	}
}

func TestPoseRetry_CPUGatingAndCompensation(t *testing.T) {
	// Mock Frigate snapshot server
	frigateSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("fake-jpeg-content"))
	}))
	defer frigateSrv.Close()

	// Mock Vision AI server
	visionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var resp vision.AnalyzeResponse
		resp.Code = 0
		resp.Data.HasFall = true
		resp.Data.Poses = []vision.PoseResult{
			{
				Pose:      "fallen",
				IsFall:    true,
				FallScore: 0.95,
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer visionSrv.Close()

	bus := eventbus.New()
	h := NewHandler(bus, nil, nil)
	h.frigateClient = &camera.FrigateClient{FrigateBase: frigateSrv.URL, HC: frigateSrv.Client()}
	h.visionClient = vision.NewClient(visionSrv.URL)

	// Mock CPU: high (85%)
	mockCPU := 85.0
	h.sampleCPU = func() float64 { return mockCPU }

	task := visionTask{eventID: "ev-fall-1", cameraID: 3, slug: "bedroom", ts: 5000}
	h.schedulePoseRetry(task)

	// Check under high CPU: should NOT execute
	h.checkAndRunPendingRetries()
	h.retryMu.Lock()
	if len(h.pendingRetry) != 1 {
		h.retryMu.Unlock()
		t.Fatalf("expected task to stay pending under high CPU, got len %d", len(h.pendingRetry))
	}
	h.retryMu.Unlock()

	// Listen for fall event on bus
	fallDetectedCh := make(chan []byte, 1)
	bus.Subscribe(eventbus.TopicCameraFallDetected, func(e eventbus.Event) {
		fallDetectedCh <- e.Payload
	})

	// Simulate CPU recovery: drop to 40%
	mockCPU = 40.0
	h.checkAndRunPendingRetries()

	select {
	case payload := <-fallDetectedCh:
		var p struct {
			EventID    string `json:"event_id"`
			CameraSlug string `json:"camera_slug"`
			Delayed    bool   `json:"delayed"`
			TS         int64  `json:"ts"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			t.Fatalf("unmarshal fall event payload: %v", err)
		}
		if !p.Delayed {
			t.Errorf("expected delayed == true, got %v", p.Delayed)
		}
		if p.EventID != "ev-fall-1" {
			t.Errorf("expected event ev-fall-1, got %s", p.EventID)
		}
		if p.TS != 5000 {
			t.Errorf("expected ts 5000, got %d", p.TS)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TopicCameraFallDetected event")
	}

	// Verify task was cleared from pendingRetry
	h.retryMu.Lock()
	defer h.retryMu.Unlock()
	if len(h.pendingRetry) != 0 {
		t.Errorf("expected pendingRetry to be empty after execution, got %d", len(h.pendingRetry))
	}
}
