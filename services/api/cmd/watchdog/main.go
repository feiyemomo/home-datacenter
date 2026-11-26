package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"home-datacenter-api/internal/hostsec"
)

// Watchdog tracks health probes and requests container restart through Host Helper.
type Watchdog struct {
	targetURL          string
	checkInterval      time.Duration
	failThreshold      int
	cooldownDuration   time.Duration
	maxPerHour         int
	bridgeEndpoint     string
	bridgeKeyID        string
	bridgeHMACKey      []byte
	consecutiveFails   int
	recentRestarts     []time.Time
	mu                 sync.Mutex
	client             *http.Client
	bridgeClient       hostsec.RestrictedHostExecutionBridge
}

func main() {
	var (
		targetURL      = flag.String("target-url", "http://127.0.0.1:8080/healthz", "Health check URL for home-api")
		checkInterval  = flag.Duration("interval", 10*time.Second, "Interval between health checks")
		failThreshold  = flag.Int("threshold", 3, "Consecutive failed checks before restart")
		cooldown       = flag.Duration("cooldown", 60*time.Second, "Cooldown after requesting restart")
		maxPerHour     = flag.Int("max-restarts-per-hour", 2, "Maximum restarts permitted per hour")
		bridgeSocket   = flag.String("bridge-socket", "/run/home-datacenter/host-bridge.sock", "Path to Host Helper domain socket")
		bridgeKeyID    = flag.String("key-id", "bridge-default-key", "HMAC key ID for Host Helper bridge")
		bridgeKey      = flag.String("hmac-key", "", "HMAC key string for Host Helper bridge")
	)
	flag.Parse()

	if *bridgeKey == "" {
		if envKey := strings.TrimSpace(os.Getenv("OPS_HOST_BRIDGE_HMAC_KEY")); envKey != "" {
			*bridgeKey = envKey
		} else {
			*bridgeKey = "default-bridge-secret-key-min-32-bytes!!"
		}
	}

	w := NewWatchdog(
		*targetURL,
		*checkInterval,
		*failThreshold,
		*cooldown,
		*maxPerHour,
		"unix://"+*bridgeSocket,
		*bridgeKeyID,
		[]byte(*bridgeKey),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("[Watchdog] starting standalone watchdog daemon targeting %s (interval: %v, threshold: %d)", *targetURL, *checkInterval, *failThreshold)
	w.Run(ctx)
	log.Printf("[Watchdog] watchdog daemon stopped gracefully")
}

// NewWatchdog creates a new configured Watchdog instance.
func NewWatchdog(
	targetURL string,
	interval time.Duration,
	threshold int,
	cooldown time.Duration,
	maxPerHour int,
	bridgeEndpoint, keyID string,
	hmacKey []byte,
) *Watchdog {
	return &Watchdog{
		targetURL:        targetURL,
		checkInterval:    interval,
		failThreshold:    threshold,
		cooldownDuration: cooldown,
		maxPerHour:       maxPerHour,
		bridgeEndpoint:   bridgeEndpoint,
		bridgeKeyID:      keyID,
		bridgeHMACKey:    hmacKey,
		client: &http.Client{
			Timeout: 4 * time.Second,
		},
	}
}

// Run starts the watchdog monitoring loop.
func (w *Watchdog) Run(ctx context.Context) {
	ticker := time.NewTicker(w.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.CheckOnce(ctx)
		}
	}
}

// CheckOnce executes a single health check probe and initiates bounded recovery if failed.
func (w *Watchdog) CheckOnce(ctx context.Context) bool {
	healthy := w.probeHealth(ctx)
	w.mu.Lock()
	defer w.mu.Unlock()

	if healthy {
		if w.consecutiveFails > 0 {
			log.Printf("[Watchdog] target %s recovered to HEALTHY state", w.targetURL)
		}
		w.consecutiveFails = 0
		return true
	}

	w.consecutiveFails++
	log.Printf("[Watchdog] target %s UNHEALTHY (failure %d/%d)", w.targetURL, w.consecutiveFails, w.failThreshold)

	if w.consecutiveFails >= w.failThreshold {
		w.handleThresholdExceeded(ctx)
	}
	return false
}

func (w *Watchdog) probeHealth(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.targetURL, nil)
	if err != nil {
		return false
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (w *Watchdog) handleThresholdExceeded(ctx context.Context) {
	now := time.Now()
	// Filter restarts in the last hour
	var recent []time.Time
	for _, t := range w.recentRestarts {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	w.recentRestarts = recent

	if len(w.recentRestarts) >= w.maxPerHour {
		log.Printf("[Watchdog] ALERT: restart limit reached (%d/%d in last hour); REFUSING automated restart to prevent restart storm", len(w.recentRestarts), w.maxPerHour)
		return
	}

	log.Printf("[Watchdog] threshold exceeded: requesting Host Helper to restart home-api...")
	if err := w.triggerRestartViaBridge(ctx); err != nil {
		log.Printf("[Watchdog] ERROR triggering restart via Host Helper: %v", err)
	} else {
		log.Printf("[Watchdog] restart request sent successfully; entering cooldown (%v)", w.cooldownDuration)
		w.recentRestarts = append(w.recentRestarts, now)
		w.consecutiveFails = 0
		time.Sleep(w.cooldownDuration)
	}
}

func (w *Watchdog) triggerRestartViaBridge(ctx context.Context) error {
	client := w.bridgeClient
	if client == nil {
		var err error
		client, err = hostsec.NewHMACHostExecutionBridge(hostsec.HostBridgeConfig{
			Endpoint:       w.bridgeEndpoint,
			KeyID:          w.bridgeKeyID,
			HMACKey:        w.bridgeHMACKey,
			RequireFencing: false, // Watchdog uses grant/scoped bypass
			Timeout:        15 * time.Second,
		})
		if err != nil {
			return fmt.Errorf("create bridge client: %w", err)
		}
		w.bridgeClient = client
	}

	req := hostsec.HostActionRequest{
		Action:         hostsec.HostActionContainerRestart,
		ServiceName:    "home-api",
		TimeoutSeconds: 15,
		Binding: hostsec.HostExecutionBinding{
			ProposalID:        "watchdog-auto-restart",
			ApprovalID:        "watchdog-authorized",
			OperationID:       fmt.Sprintf("watchdog-op-%d", time.Now().UnixNano()),
			BaselineHash:      "host-v1:watchdog-baseline",
			FencingGeneration: 1,
		},
	}

	res, err := client.ExecuteTypedAction(ctx, req)
	if err != nil {
		return fmt.Errorf("execute bridge restart: %w (stderr: %s)", err, res.Stderr)
	}
	return nil
}
