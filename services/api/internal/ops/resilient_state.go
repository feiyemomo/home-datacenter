package ops

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	ErrResourceLatched     = errors.New("resource is latched due to excessive failures; manual unlatch or health decay required")
	ErrRateLimitExceeded   = errors.New("rate limit exceeded for resource within sliding window")
	ErrResourceBusy        = errors.New("resource operation is currently in progress")
	ErrInvalidServiceName  = errors.New("invalid or empty service name")
)

// ResourceState tracks runtime resilience metrics and circuit breaker status for a service.
type ResourceState struct {
	ServiceName             string    `json:"service_name"`
	AttemptCount            int       `json:"attempt_count"`
	WindowStart             time.Time `json:"window_start"`
	LastAttempt             time.Time `json:"last_attempt"`
	LastSuccess             time.Time `json:"last_success,omitempty"`
	Latched                 bool      `json:"latched"`
	LatchedReason           string    `json:"latched_reason,omitempty"`
	LatchedAt               time.Time `json:"latched_at,omitempty"`
	ConsecutiveHealthyTicks int       `json:"consecutive_healthy_ticks"`
}

// DailyOpsDigest summarizes resilience events, recovery attempts, and latched services.
type DailyOpsDigest struct {
	GeneratedAt     time.Time       `json:"generated_at"`
	TotalServices   int             `json:"total_services"`
	ActiveLatches   int             `json:"active_latches"`
	TotalAttempts   int             `json:"total_attempts"`
	LatchedServices []string        `json:"latched_services"`
	ServiceStates   []ResourceState `json:"service_states"`
	SummaryReport   string          `json:"summary_report"`
}

// ResilientStateSnapshot represents the minimal persistent state saved to disk.
type ResilientStateSnapshot struct {
	Version   int                       `json:"version"`
	UpdatedAt time.Time                 `json:"updated_at"`
	States    map[string]*ResourceState `json:"states"`
}

// ResilientStateManager manages fine-grained per-resource locking, sliding window rate-limiting,
// anti-oscillation latching, and decoupled append-only audit logging without touching app.db.
type ResilientStateManager struct {
	mu                         sync.RWMutex
	resourceLocks              sync.Map // string -> *sync.Mutex
	states                     map[string]*ResourceState
	snapshotPath               string
	logPath                    string
	windowDuration             time.Duration
	maxAttemptsPerWindow       int
	consecutiveHealthyForDecay int
	logWriter                  io.Writer
	logMu                      sync.Mutex
}

// ResilientStateConfig configures the ResilientStateManager.
type ResilientStateConfig struct {
	SnapshotPath               string
	LogPath                    string
	WindowDuration             time.Duration
	MaxAttemptsPerWindow       int
	ConsecutiveHealthyForDecay int
	LogWriterOverride          io.Writer
}

// NewResilientStateManager initializes the state manager and restores any existing snapshot from disk.
func NewResilientStateManager(cfg ResilientStateConfig) (*ResilientStateManager, error) {
	if cfg.WindowDuration <= 0 {
		cfg.WindowDuration = 15 * time.Minute
	}
	if cfg.MaxAttemptsPerWindow <= 0 {
		cfg.MaxAttemptsPerWindow = 3
	}
	if cfg.ConsecutiveHealthyForDecay <= 0 {
		cfg.ConsecutiveHealthyForDecay = 60
	}
	if cfg.SnapshotPath == "" {
		cfg.SnapshotPath = "data/ops_snapshot.json"
	}
	if cfg.LogPath == "" {
		cfg.LogPath = "data/ops.log"
	}

	m := &ResilientStateManager{
		states:                     make(map[string]*ResourceState),
		snapshotPath:               filepath.Clean(cfg.SnapshotPath),
		logPath:                    filepath.Clean(cfg.LogPath),
		windowDuration:             cfg.WindowDuration,
		maxAttemptsPerWindow:       cfg.MaxAttemptsPerWindow,
		consecutiveHealthyForDecay: cfg.ConsecutiveHealthyForDecay,
		logWriter:                  cfg.LogWriterOverride,
	}

	// Attempt snapshot restoration
	if err := m.LoadSnapshot(); err != nil && !os.IsNotExist(err) {
		log.Printf("[ResilientStateManager] warning loading snapshot %s: %v (continuing with fresh state)", m.snapshotPath, err)
	}

	return m, nil
}

// GetResourceMutex retrieves or creates the per-resource mutex for fine-grained concurrency control.
func (m *ResilientStateManager) GetResourceMutex(service string) *sync.Mutex {
	actual, _ := m.resourceLocks.LoadOrStore(service, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

// AcquireResource locks the per-resource mutex and returns a release function.
func (m *ResilientStateManager) AcquireResource(service string) (func(), error) {
	if service == "" {
		return nil, ErrInvalidServiceName
	}
	mtx := m.GetResourceMutex(service)
	mtx.Lock()
	return func() {
		mtx.Unlock()
	}, nil
}

// CanExecute checks if an operation on the target service is permitted under rate limit and latching rules.
func (m *ResilientStateManager) CanExecute(service string) (bool, error) {
	if service == "" {
		return false, ErrInvalidServiceName
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	st := m.getOrCreateStateLocked(service)
	now := time.Now()

	// Check latch
	if st.Latched {
		return false, fmt.Errorf("%w: service %q latched at %s (reason: %s)", ErrResourceLatched, service, st.LatchedAt.Format(time.RFC3339), st.LatchedReason)
	}

	// Sliding window renewal
	if now.Sub(st.WindowStart) > m.windowDuration {
		st.WindowStart = now
		st.AttemptCount = 0
	}

	// Rate limit check
	if st.AttemptCount >= m.maxAttemptsPerWindow {
		return false, fmt.Errorf("%w: service %q attempted %d times in current window", ErrRateLimitExceeded, service, st.AttemptCount)
	}

	return true, nil
}

// RecordAttempt records an execution attempt on the target service and persists snapshot.
func (m *ResilientStateManager) RecordAttempt(service, action string) error {
	m.mu.Lock()
	st := m.getOrCreateStateLocked(service)
	now := time.Now()

	if now.Sub(st.WindowStart) > m.windowDuration {
		st.WindowStart = now
		st.AttemptCount = 0
	}

	st.AttemptCount++
	st.LastAttempt = now
	st.ConsecutiveHealthyTicks = 0

	m.mu.Unlock()

	m.LogAudit(service, action, "ATTEMPT", fmt.Sprintf("attempt %d/%d", st.AttemptCount, m.maxAttemptsPerWindow))
	return m.SaveSnapshot()
}

// RecordSuccess records a successful execution and resets consecutive failure counters.
func (m *ResilientStateManager) RecordSuccess(service, action string) error {
	m.mu.Lock()
	st := m.getOrCreateStateLocked(service)
	now := time.Now()

	st.LastSuccess = now
	st.ConsecutiveHealthyTicks = 1
	m.mu.Unlock()

	m.LogAudit(service, action, "SUCCESS", "operation completed successfully")
	return m.SaveSnapshot()
}

// RecordFailure records an operation failure, updating counters and triggering latching if threshold exceeded.
func (m *ResilientStateManager) RecordFailure(service, action string, opErr error) error {
	m.mu.Lock()
	st := m.getOrCreateStateLocked(service)
	now := time.Now()

	st.ConsecutiveHealthyTicks = 0
	errMsg := "unknown error"
	if opErr != nil {
		errMsg = opErr.Error()
	}

	if st.AttemptCount >= m.maxAttemptsPerWindow {
		st.Latched = true
		st.LatchedAt = now
		st.LatchedReason = fmt.Sprintf("exceeded %d attempts in %v; last error: %s", m.maxAttemptsPerWindow, m.windowDuration, errMsg)
	}
	m.mu.Unlock()

	status := "FAILURE"
	if st.Latched {
		status = "LATCHED"
	}
	m.LogAudit(service, action, status, errMsg)
	return m.SaveSnapshot()
}

// Unlatch manually releases the circuit breaker latch for a service.
func (m *ResilientStateManager) Unlatch(service, operator string) error {
	m.mu.Lock()
	st := m.getOrCreateStateLocked(service)
	st.Latched = false
	st.LatchedReason = ""
	st.AttemptCount = 0
	st.WindowStart = time.Now()
	st.ConsecutiveHealthyTicks = 0
	m.mu.Unlock()

	m.LogAudit(service, "UNLATCH", "MANUAL_RESET", fmt.Sprintf("unlatched by operator: %s", operator))
	return m.SaveSnapshot()
}

// TickHealth evaluates periodic health probes, automatically unlatching after prolonged stability.
func (m *ResilientStateManager) TickHealth(service string, healthy bool) {
	m.mu.Lock()
	st := m.getOrCreateStateLocked(service)
	if healthy {
		st.ConsecutiveHealthyTicks++
		if st.Latched && st.ConsecutiveHealthyTicks >= m.consecutiveHealthyForDecay {
			st.Latched = false
			st.LatchedReason = ""
			st.AttemptCount = 0
			st.WindowStart = time.Now()
			m.mu.Unlock()
			m.LogAudit(service, "HEALTH_TICK", "AUTO_UNLATCH_DECAY", fmt.Sprintf("unlatched after %d consecutive healthy ticks", st.ConsecutiveHealthyTicks))
			_ = m.SaveSnapshot()
			return
		}
	} else {
		st.ConsecutiveHealthyTicks = 0
	}
	m.mu.Unlock()
}

// GetState returns a clone of the current state for a service.
func (m *ResilientStateManager) GetState(service string) (ResourceState, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, exists := m.states[service]
	if !exists {
		return ResourceState{}, false
	}
	return *st, true
}

// GetDailyDigest produces a high-level operational resilience summary for administrator review.
func (m *ResilientStateManager) GetDailyDigest() DailyOpsDigest {
	m.mu.RLock()
	defer m.mu.RUnlock()

	digest := DailyOpsDigest{
		GeneratedAt:     time.Now().UTC(),
		TotalServices:   len(m.states),
		LatchedServices: make([]string, 0),
		ServiceStates:   make([]ResourceState, 0, len(m.states)),
	}

	for svc, st := range m.states {
		copied := *st
		digest.TotalAttempts += copied.AttemptCount
		digest.ServiceStates = append(digest.ServiceStates, copied)
		if copied.Latched {
			digest.ActiveLatches++
			digest.LatchedServices = append(digest.LatchedServices, svc)
		}
	}

	if digest.ActiveLatches > 0 {
		digest.SummaryReport = fmt.Sprintf("⚠️ 告警：当前系统有 %d 个服务进入自愈熔断锁存状态 (%s)，需管理员关注排查。",
			digest.ActiveLatches, strings.Join(digest.LatchedServices, ", "))
	} else if digest.TotalAttempts > 0 {
		digest.SummaryReport = fmt.Sprintf("ℹ️ 正常：过去运维周期内发生 %d 次自愈处置，当前所有核心服务运行正常，无熔断锁存。",
			digest.TotalAttempts)
	} else {
		digest.SummaryReport = "✅ 健康：过去运维周期内系统稳定运行，无自愈重启触发，服务健康度 100%。"
	}

	return digest
}

func (m *ResilientStateManager) getOrCreateStateLocked(service string) *ResourceState {
	st, ok := m.states[service]
	if !ok {
		st = &ResourceState{
			ServiceName:  service,
			WindowStart:  time.Now(),
			AttemptCount: 0,
		}
		m.states[service] = st
	}
	return st
}

// SaveSnapshot performs an atomic Write-Temp-Rename to persist state without partial corruption.
func (m *ResilientStateManager) SaveSnapshot() error {
	m.mu.RLock()
	snap := ResilientStateSnapshot{
		Version:   1,
		UpdatedAt: time.Now(),
		States:    make(map[string]*ResourceState, len(m.states)),
	}
	for k, v := range m.states {
		copied := *v
		snap.States[k] = &copied
	}
	m.mu.RUnlock()

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal ops snapshot: %w", err)
	}

	dir := filepath.Dir(m.snapshotPath)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("mkdir for ops snapshot %s: %w", dir, err)
	}

	tmpFile := fmt.Sprintf("%s.tmp.%d", m.snapshotPath, time.Now().UnixNano())
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		return fmt.Errorf("write temp snapshot %s: %w", tmpFile, err)
	}

	if err := os.Rename(tmpFile, m.snapshotPath); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("rename snapshot %s -> %s: %w", tmpFile, m.snapshotPath, err)
	}

	return nil
}

// LoadSnapshot loads persisted state from disk.
func (m *ResilientStateManager) LoadSnapshot() error {
	data, err := os.ReadFile(m.snapshotPath)
	if err != nil {
		return err
	}

	var snap ResilientStateSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("unmarshal ops snapshot: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if snap.States != nil {
		m.states = snap.States
	}
	return nil
}

// LogAudit writes an append-only structured audit record to ops.log without touching app.db.
func (m *ResilientStateManager) LogAudit(service, action, outcome, details string) {
	m.logMu.Lock()
	defer m.logMu.Unlock()

	line := fmt.Sprintf("[%s] service=%s action=%s outcome=%s details=%q\n",
		time.Now().UTC().Format(time.RFC3339Nano),
		service, action, outcome, details,
	)

	if m.logWriter != nil {
		_, _ = io.WriteString(m.logWriter, line)
		return
	}

	dir := filepath.Dir(m.logPath)
	_ = os.MkdirAll(dir, 0750)

	f, err := os.OpenFile(m.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		log.Printf("[OpsLogAudit] failed to open %s: %v", m.logPath, err)
		return
	}
	defer f.Close()

	_, _ = f.WriteString(line)
}
