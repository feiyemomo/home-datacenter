package ops

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"home-datacenter-api/internal/hostsec"
)

var (
	ErrBulkheadCapacityExceeded = errors.New("ops bulkhead worker queue full; operation rejected")
	ErrHealthVerificationFailed = errors.New("service health check failed after recovery operation")
	ErrExecutionPanic           = errors.New("ops execution recovered from unexpected panic")
)

// PlaybookEngineConfig configures the resilient playbook execution engine.
type PlaybookEngineConfig struct {
	BridgeClient       hostsec.RestrictedHostExecutionBridge
	StateManager       *ResilientStateManager
	HealthProbeTimeout time.Duration
	MaxConcurrentOps   int
	HealthCheckURLs    map[string]string // service -> health URL
}

// PlaybookEngine orchestrates the four atomic recovery playbooks across the restricted
// Host Helper bridge, enforcing resource mutexes, rate limiting, and panic bulkhead isolation.
type PlaybookEngine struct {
	bridge          hostsec.RestrictedHostExecutionBridge
	stateMgr        *ResilientStateManager
	healthClient    *http.Client
	healthTimeout   time.Duration
	healthCheckURLs map[string]string
	workerSem       chan struct{}
	mu              sync.Mutex
}

// NewPlaybookEngine initializes the engine with isolated bulkhead workers and HTTP transport.
func NewPlaybookEngine(cfg PlaybookEngineConfig) (*PlaybookEngine, error) {
	if cfg.StateManager == nil {
		return nil, errors.New("state manager is required for PlaybookEngine")
	}
	if cfg.MaxConcurrentOps <= 0 {
		cfg.MaxConcurrentOps = 2
	}
	if cfg.HealthProbeTimeout <= 0 {
		cfg.HealthProbeTimeout = 3 * time.Second
	}

	// Bulkhead isolated HTTP transport strictly for ops health checking
	isolatedTransport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 10 * time.Second,
		}).DialContext,
		MaxIdleConns:        5,
		IdleConnTimeout:     15 * time.Second,
		DisableCompression: true,
	}

	return &PlaybookEngine{
		bridge:          cfg.BridgeClient,
		stateMgr:        cfg.StateManager,
		healthTimeout:   cfg.HealthProbeTimeout,
		healthCheckURLs: cfg.HealthCheckURLs,
		workerSem:       make(chan struct{}, cfg.MaxConcurrentOps),
		healthClient: &http.Client{
			Transport: isolatedTransport,
			Timeout:   cfg.HealthProbeTimeout,
		},
	}, nil
}

// ExecuteSafe runs any recovery playbook inside the panic bulkhead, checking rate limits,
// locking per-resource mutexes, and recording atomic state transitions.
func (e *PlaybookEngine) ExecuteSafe(
	ctx context.Context,
	service string,
	action string,
	fn func(ctx context.Context) (string, error),
) (out string, retErr error) {
	if service == "" {
		return "", ErrInvalidServiceName
	}

	// 1. Bulkhead worker admission
	select {
	case e.workerSem <- struct{}{}:
		defer func() { <-e.workerSem }()
	default:
		return "", ErrBulkheadCapacityExceeded
	}

	// 2. Panic containment wrapper
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[OPS_BULKHEAD_PANIC] recovered panic executing %s on %s: %v", action, service, r)
			retErr = fmt.Errorf("%w: %v", ErrExecutionPanic, r)
			_ = e.stateMgr.RecordFailure(service, action, retErr)
		}
	}()

	// 3. Acquire resource-level mutex (blocks concurrent runs on same service only)
	unlock, err := e.stateMgr.AcquireResource(service)
	if err != nil {
		return "", err
	}
	defer unlock()

	// 4. Rate-limit & latching gate
	can, err := e.stateMgr.CanExecute(service)
	if !can || err != nil {
		return "", err
	}

	// 5. Record attempt
	if err := e.stateMgr.RecordAttempt(service, action); err != nil {
		log.Printf("[PlaybookEngine] record attempt warning: %v", err)
	}

	// 6. Execute atomic operation
	out, err = fn(ctx)
	if err != nil {
		_ = e.stateMgr.RecordFailure(service, action, err)
		return out, err
	}

	// 7. Verify health if health check URL is configured
	if healthURL, ok := e.healthCheckURLs[service]; ok && healthURL != "" {
		if hErr := e.verifyHealth(ctx, healthURL); hErr != nil {
			verifyErr := fmt.Errorf("%w: %v", ErrHealthVerificationFailed, hErr)
			_ = e.stateMgr.RecordFailure(service, action, verifyErr)
			return out, verifyErr
		}
	}

	// 8. Record success
	_ = e.stateMgr.RecordSuccess(service, action)
	return out, nil
}

// RestartService (PB-01) restarts a target container via Host Helper.
func (e *PlaybookEngine) RestartService(ctx context.Context, service string, timeoutSec int) (string, error) {
	return e.ExecuteSafe(ctx, service, "PB-01:RestartService", func(runCtx context.Context) (string, error) {
		if e.bridge == nil {
			return "", errors.New("bridge client not configured")
		}
		req := hostsec.HostActionRequest{
			Action:         hostsec.HostActionContainerRestart,
			ServiceName:    service,
			TimeoutSeconds: timeoutSec,
			Binding: hostsec.HostExecutionBinding{
				ProposalID:        "auto-pb01-" + service,
				ApprovalID:        "auto-approved",
				OperationID:       fmt.Sprintf("op-restart-%d", time.Now().UnixNano()),
				BaselineHash:      "host-v1:auto-remediation-baseline",
				FencingGeneration: 1,
			},
		}
		res, err := e.bridge.ExecuteTypedAction(runCtx, req)
		if err != nil {
			return res.Stdout, err
		}
		return res.Stdout, nil
	})
}

// ReapZombies (PB-02) terminates runaway or zombie processes belonging to the target service.
func (e *PlaybookEngine) ReapZombies(ctx context.Context, service string, targetPID int) (string, error) {
	return e.ExecuteSafe(ctx, service, "PB-02:ReapZombies", func(runCtx context.Context) (string, error) {
		if e.bridge == nil {
			return "", errors.New("bridge client not configured")
		}
		if targetPID <= 1 {
			return "", errors.New("target PID must be > 1")
		}
		req := hostsec.HostActionRequest{
			Action:      hostsec.HostActionZombieReap,
			ServiceName: service,
			TargetPID:   targetPID,
			Binding: hostsec.HostExecutionBinding{
				ProposalID:        "auto-pb02-" + service,
				ApprovalID:        "auto-approved",
				OperationID:       fmt.Sprintf("op-zombie-%d", time.Now().UnixNano()),
				BaselineHash:      "host-v1:auto-remediation-baseline",
				FencingGeneration: 1,
			},
		}
		res, err := e.bridge.ExecuteTypedAction(runCtx, req)
		if err != nil {
			return res.Stdout, err
		}
		return res.Stdout, nil
	})
}

// NetworkReset (PB-03) resets specific local network endpoints following diagnostic evaluation.
func (e *PlaybookEngine) NetworkReset(ctx context.Context, scope string) (string, error) {
	serviceName := "network-" + scope
	return e.ExecuteSafe(ctx, serviceName, "PB-03:NetworkReset", func(runCtx context.Context) (string, error) {
		if e.bridge == nil {
			return "", errors.New("bridge client not configured")
		}
		req := hostsec.HostActionRequest{
			Action:     hostsec.HostActionNetworkReset,
			ResetScope: scope,
			Binding: hostsec.HostExecutionBinding{
				ProposalID:        "auto-pb03-" + scope,
				ApprovalID:        "auto-approved",
				OperationID:       fmt.Sprintf("op-net-%d", time.Now().UnixNano()),
				BaselineHash:      "host-v1:auto-remediation-baseline",
				FencingGeneration: 1,
			},
		}
		res, err := e.bridge.ExecuteTypedAction(runCtx, req)
		if err != nil {
			return res.Stdout, err
		}
		return res.Stdout, nil
	})
}

// PruneOrphans (PB-04) safely removes dead temporary test containers matching strict whitelist filter.
func (e *PlaybookEngine) PruneOrphans(ctx context.Context, filter string) (string, error) {
	serviceName := "docker-prune"
	return e.ExecuteSafe(ctx, serviceName, "PB-04:PruneOrphans", func(runCtx context.Context) (string, error) {
		if e.bridge == nil {
			return "", errors.New("bridge client not configured")
		}
		if !strings.HasPrefix(filter, "hdc-tmp-") && !strings.HasPrefix(filter, "hdc-test-") {
			return "", fmt.Errorf("filter %q rejected: only hdc-tmp-* or hdc-test-* permitted", filter)
		}
		req := hostsec.HostActionRequest{
			Action:      hostsec.HostActionOrphanPrune,
			PruneFilter: filter,
			Binding: hostsec.HostExecutionBinding{
				ProposalID:        "auto-pb04-prune",
				ApprovalID:        "auto-approved",
				OperationID:       fmt.Sprintf("op-prune-%d", time.Now().UnixNano()),
				BaselineHash:      "host-v1:auto-remediation-baseline",
				FencingGeneration: 1,
			},
		}
		res, err := e.bridge.ExecuteTypedAction(runCtx, req)
		if err != nil {
			return res.Stdout, err
		}
		return res.Stdout, nil
	})
}

func (e *PlaybookEngine) verifyHealth(ctx context.Context, healthURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return err
	}
	resp, err := e.healthClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("health endpoint %s returned status %d", healthURL, resp.StatusCode)
	}
	return nil
}
