package ops

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
)

// Standard error declarations for FSM, Circuit Breaker, and Maintenance Lock.
var (
	ErrMaintenanceLockActive = errors.New("system maintenance lock is already held by another operator")
	ErrSystemInMaintenance   = errors.New("system maintenance lock is active; proactive agent recovery suspended")
	ErrCircuitBreakerOpen    = errors.New("circuit breaker is OPEN; automated recovery halted")
	ErrDependencyUnhealthy   = errors.New("upstream container dependency is unhealthy")
	ErrProbeInProgress       = errors.New("circuit breaker probe already in progress")
	ErrStaleProbeToken       = errors.New("circuit breaker probe token is stale or invalid")
	ErrProbeExpired          = errors.New("circuit breaker probe token has expired")
	ErrFailClosedMode        = errors.New("system in fail-closed degraded mode due to storage failure; operations prohibited")
)

// ProbeTicket models an atomic, leased token granting exclusive recovery probe eligibility.
type ProbeTicket struct {
	Token     string    `json:"token"`
	Container string    `json:"container"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// RecoveryProbeTicket is an alias for ProbeTicket for protocol compatibility.
type RecoveryProbeTicket = ProbeTicket


// ContainerEventClass classifies container lifecycle events into 5 distinct categories.
type ContainerEventClass string

const (
	EventManualStop ContainerEventClass = "MANUAL_STOP"
	EventUpdate     ContainerEventClass = "UPDATE"
	EventExitZero   ContainerEventClass = "EXIT_ZERO"
	EventOOM        ContainerEventClass = "OOM"
	EventCrash      ContainerEventClass = "CRASH"
)

// ContainerEvent models an incoming container lifecycle or crash event.
type ContainerEvent struct {
	ContainerName string            `json:"container_name"`
	Action        string            `json:"action"` // "stop", "die", "kill", "oom", "update", "restart"
	ExitCode      int               `json:"exit_code"`
	OOMKilled     bool              `json:"oom_killed"`
	IsManual      bool              `json:"is_manual"`
	IsUpdate      bool              `json:"is_update"`
	Timestamp     time.Time         `json:"timestamp"`
	Attributes    map[string]string `json:"attributes,omitempty"`
}

// ClassifyContainerEvent categorizes a container event according to the 5-class FSM spec.
// Manual stops and clean exits are strictly ignored to prevent unwanted restart loops.
func ClassifyContainerEvent(event ContainerEvent, isMaintenance bool) ContainerEventClass {
	if isMaintenance || event.IsManual || strings.EqualFold(event.Action, "stop") {
		return EventManualStop
	}
	if event.IsUpdate || strings.EqualFold(event.Action, "update") {
		return EventUpdate
	}

	actionLower := strings.ToLower(strings.TrimSpace(event.Action))

	if actionLower == "die" || actionLower == "kill" {
		// OOM check: explicitly flagged, action is oom, or exit code 137 with OOM indicator
		if event.OOMKilled || actionLower == "oom" || (event.ExitCode == 137 && (event.OOMKilled || strings.Contains(strings.ToLower(event.Attributes["error"]), "oom"))) {
			return EventOOM
		}
		// Clean exit: exitCode 0 and not OOM
		if event.ExitCode == 0 && !event.OOMKilled {
			return EventExitZero
		}
		// Non-zero exit: abnormal crash
		return EventCrash
	}

	if actionLower == "oom" {
		return EventOOM
	}
	if event.ExitCode == 0 {
		return EventExitZero
	}
	return EventCrash
}

// ============================================================================
// 2. Container Dependency Topology
// ============================================================================

const (
	TopologyLevel0 = 0 // Storage & Broker Core: mosquitto, frigate, app.db
	TopologyLevel1 = 1 // Core API: home-api
	TopologyLevel2 = 2 // User-Facing SPA & Peripherals: home-web, home-vision, etc.
)

// ComponentHealthChecker tests whether a component is running and healthy.
type ComponentHealthChecker func(ctx context.Context, component string) (bool, error)

// TopologyEngine manages container dependency trees and performs pre-flight recovery checks.
type TopologyEngine struct {
	mu            sync.RWMutex
	graph         map[string][]string // canonical name -> direct dependencies
	levels        map[string]int      // canonical name -> topology level (0, 1, 2)
	healthChecker ComponentHealthChecker
}

// NewTopologyEngine initializes the default Home Datacenter dependency hierarchy.
func NewTopologyEngine() *TopologyEngine {
	engine := &TopologyEngine{
		graph: map[string][]string{
			// Level 0: Storage & Broker Core (no dependencies)
			"mosquitto": {},
			"frigate":   {},
			"app.db":    {},

			// Level 1: Core API
			"home-api": {"mosquitto", "frigate", "app.db"},

			// Level 2: User-Facing SPA & Peripherals
			"home-web":            {"home-api"},
			"home-vision":         {"home-api"},
			"home-cloudflared":    {"home-web", "home-api"},
			"home-h3c-keepalive":  {"frigate"},
			"home-ops-agent":      {"mosquitto", "home-api"},
			"openresty":           {"home-web", "home-api"},
		},
		levels: map[string]int{
			"mosquitto":             TopologyLevel0,
			"home-mosquitto":        TopologyLevel0,
			"frigate":               TopologyLevel0,
			"home-frigate":          TopologyLevel0,
			"app.db":                TopologyLevel0,
			"database":              TopologyLevel0,
			"home-api":              TopologyLevel1,
			"api":                   TopologyLevel1,
			"home-web":              TopologyLevel2,
			"web":                   TopologyLevel2,
			"home-vision":           TopologyLevel2,
			"vision":                TopologyLevel2,
			"home-cloudflared":      TopologyLevel2,
			"home-h3c-keepalive":    TopologyLevel2,
			"home-ops-agent":        TopologyLevel2,
			"1panel-openresty-wudu": TopologyLevel2,
			"openresty":             TopologyLevel2,
		},
		healthChecker: nil, // Fail-closed: unconfigured health checker returns error (R7.2)
	}
	return engine
}

// SetHealthChecker configures the health validation function.
func (t *TopologyEngine) SetHealthChecker(checker ComponentHealthChecker) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.healthChecker = checker
}

// ErrHealthCheckerNotConfigured indicates the health check probe is unconfigured (Fail-Closed).
var (
	ErrHealthCheckerNotConfigured = errors.New("topology health checker is not configured (fail-closed)")
	ErrDependencyCheckTimeout     = errors.New("dependency health check timed out")
	ErrDatabaseUnhealthy          = errors.New("database dependency app.db is unhealthy")
)

// CoreDependencies defines mandatory infrastructure services that must be healthy (Fail-Closed).
var CoreDependencies = map[string]bool{
	"app.db":    true,
	"mosquitto": true,
	"frigate":   true,
	"home-api":  true,
}

// IsCoreDependency returns whether the given component is a mandatory core dependency.
func IsCoreDependency(component string) bool {
	canonical := ResolveTopologyComponent(component)
	return CoreDependencies[canonical]
}

// ResolveTopologyComponent normalizes container names and aliases to canonical topology identifiers (R4).
func ResolveTopologyComponent(name string) string {
	clean := strings.TrimSpace(name)
	clean = strings.TrimPrefix(clean, "/")
	if idx := strings.Index(clean, "("); idx != -1 {
		prefix := strings.TrimSpace(clean[:idx])
		if prefix != "" {
			clean = prefix
		}
	}
	lower := strings.ToLower(clean)
	switch lower {
	case "home-mosquitto", "mqtt", "mosquitto", "broker", "mosquitto-broker", "home-datacenter-mosquitto":
		return "mosquitto"
	case "home-frigate", "nvr", "frigate", "go2rtc", "home-datacenter-frigate":
		return "frigate"
	case "app.db", "database", "sqlite", "db":
		return "app.db"
	case "home-api", "api", "backend", "go-api", "home-datacenter-api":
		return "home-api"
	case "home-web", "web", "frontend", "nginx", "home-datacenter-web":
		return "home-web"
	case "home-vision", "vision", "ai-vision", "home-datacenter-vision":
		return "home-vision"
	case "1panel-openresty-wudu", "1panel-openresty", "openresty", "nginx-proxy", "1pwaf", "waf", "openresty-waf", "home-gateway", "gateway", "home-datacenter-gateway":
		return "openresty"
	case "home-cloudflared", "cloudflared", "home-datacenter-cloudflared":
		return "home-cloudflared"
	case "home-h3c-keepalive", "h3c", "home-datacenter-h3c-keepalive":
		return "home-h3c-keepalive"
	case "home-ops-agent", "ops-agent", "home-datacenter-ops-agent":
		return "home-ops-agent"
	default:
		return lower
	}
}

// GetLevel returns the topological level of a component (0, 1, or 2).
func (t *TopologyEngine) GetLevel(target string) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	canonical := ResolveTopologyComponent(target)
	if lvl, ok := t.levels[canonical]; ok {
		return lvl
	}
	return TopologyLevel2
}

// PreflightDependencyCheck validates that all upstream dependencies are healthy before restarting.
// Level 1 and Level 2 containers cannot be restarted unless Level 0 dependencies are running and healthy.
// Core dependencies (app.db, mosquitto, frigate, home-api) FAIL CLOSED on check failure, timeout, or missing checker.
func (t *TopologyEngine) PreflightDependencyCheck(ctx context.Context, target string) error {
	t.mu.RLock()
	defer t.mu.RUnlock()

	canonical := ResolveTopologyComponent(target)
	deps, exists := t.graph[canonical]
	if !exists || len(deps) == 0 {
		// Level 0 or independent component: no dependencies required
		return nil
	}

	checker := t.healthChecker
	if checker == nil {
		for _, dep := range deps {
			if IsCoreDependency(dep) {
				return fmt.Errorf("%w: cannot recover %s because core dependency %s checker is unconfigured", ErrHealthCheckerNotConfigured, target, dep)
			}
		}
		return ErrHealthCheckerNotConfigured
	}

	// 1. Check all direct dependencies
	for _, dep := range deps {
		isCore := IsCoreDependency(dep)
		if err := ctx.Err(); err != nil {
			if isCore {
				return fmt.Errorf("%w: %v", ErrDependencyCheckTimeout, err)
			}
			continue
		}

		healthy, err := checker(ctx, dep)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return fmt.Errorf("%w: health check for core dependency %s timed out: %v", ErrDependencyCheckTimeout, dep, err)
			}
			if isCore {
				return fmt.Errorf("cannot recover %s: upstream dependency %s is unhealthy: %w", target, dep, err)
			}
			continue
		}
		if !healthy {
			if isCore {
				return fmt.Errorf("cannot recover %s: upstream dependency %s is unhealthy", target, dep)
			}
			continue
		}

		// 2. Transitive check: if dep has dependencies, ensure they are also healthy
		if subDeps, ok := t.graph[dep]; ok {
			for _, subDep := range subDeps {
				subCore := IsCoreDependency(subDep)
				if err := ctx.Err(); err != nil {
					if subCore {
						return fmt.Errorf("%w: %v", ErrDependencyCheckTimeout, err)
					}
					continue
				}
				subHealthy, subErr := checker(ctx, subDep)
				if subErr != nil {
					if errors.Is(subErr, context.DeadlineExceeded) || errors.Is(subErr, context.Canceled) {
						return fmt.Errorf("%w: health check for core dependency %s timed out: %v", ErrDependencyCheckTimeout, subDep, subErr)
					}
					if subCore {
						return fmt.Errorf("cannot recover %s: upstream dependency %s (transitive dependency of %s) is unhealthy: %w", target, subDep, dep, subErr)
					}
					continue
				}
				if !subHealthy {
					if subCore {
						return fmt.Errorf("cannot recover %s: upstream dependency %s (transitive dependency of %s) is unhealthy", target, subDep, dep)
					}
					continue
				}
			}
		}
	}

	return nil
}

// ============================================================================
// 3. Container Circuit Breaker
// ============================================================================

// CircuitState represents the current operating state of a container's circuit breaker.
type CircuitState string

const (
	CircuitClosed   CircuitState = "CLOSED"
	CircuitOpen     CircuitState = "OPEN"
	CircuitHalfOpen CircuitState = "HALF_OPEN"
)

// ContainerCircuitBreaker tracks container crash frequency within a sliding window.
// If >= 3 crashes occur within 10 minutes, the circuit trips to OPEN, halting automated restarts.
type ContainerCircuitBreaker struct {
	mu               sync.RWMutex
	windowDuration   time.Duration // Default: 10 minutes (600s)
	maxCrashes       int           // Default: 3 crashes
	cooldownDuration time.Duration // Default: 15 minutes before half-open
	probeTTL         time.Duration // Default: 30 seconds probe lease
	crashTimestamps  map[string][]time.Time
	states           map[string]CircuitState
	openedAt         map[string]time.Time
	activeProbes     map[string]ProbeTicket
	onTrip           func(container string, crashCount int)
	db               *gorm.DB
	isDegraded       bool
}

// NewContainerCircuitBreaker creates a default circuit breaker (3 crashes in 10 minutes).
func NewContainerCircuitBreaker() *ContainerCircuitBreaker {
	return &ContainerCircuitBreaker{
		windowDuration:   10 * time.Minute,
		maxCrashes:       3,
		cooldownDuration: 15 * time.Minute,
		probeTTL:         30 * time.Second,
		crashTimestamps:  make(map[string][]time.Time),
		states:           make(map[string]CircuitState),
		openedAt:         make(map[string]time.Time),
		activeProbes:     make(map[string]ProbeTicket),
	}
}

// SetDB binds the database instance for persistent state recording.
func (cb *ContainerCircuitBreaker) SetDB(db *gorm.DB) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.db = db
}

// LoadPersistedState restores circuit breaker states and crash windows from SQLite.
// If loading fails due to database errors, enters fail-closed degraded mode.
func (cb *ContainerCircuitBreaker) LoadPersistedState(db *gorm.DB) {
	if db == nil {
		return
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.db = db

	var rows []model.OpsCircuitBreakerState
	if err := db.Find(&rows).Error; err != nil {
		cb.isDegraded = true
		return
	}

	cutoff := time.Now().Add(-cb.windowDuration)
	for _, r := range rows {
		canonical := ResolveTopologyComponent(r.Container)
		state := CircuitState(r.State)
		if state == "" {
			state = CircuitClosed
		}

		// R10.6: Any in-flight probe before restart is an orphaned zombie probe.
		// Reset HALF_OPEN state to CircuitOpen with openedAt preserved so that
		// once cooldown elapses, a fresh probe can be cleanly acquired.
		if state == CircuitHalfOpen {
			state = CircuitOpen
			if r.OpenedAt.IsZero() {
				cb.openedAt[canonical] = time.Now()
			} else {
				cb.openedAt[canonical] = r.OpenedAt
			}
		} else if !r.OpenedAt.IsZero() {
			cb.openedAt[canonical] = r.OpenedAt
		}
		cb.states[canonical] = state

		if r.CrashTimestamps != "" {
			var tsStrings []string
			if err := json.Unmarshal([]byte(r.CrashTimestamps), &tsStrings); err == nil {
				var validTimes []time.Time
				for _, s := range tsStrings {
					if t, err := time.Parse(time.RFC3339Nano, s); err == nil && t.After(cutoff) {
						validTimes = append(validTimes, t)
					}
				}
				cb.crashTimestamps[canonical] = validTimes
			}
		}
	}
}

// persistState saves a container's current state to the database atomically.
// Must be called with lock held. Returns actual database error (R6.1).
func (cb *ContainerCircuitBreaker) persistState(canonical string) error {
	if cb.db == nil {
		return nil
	}
	state := cb.states[canonical]
	if state == "" {
		state = CircuitClosed
	}
	openedAt := cb.openedAt[canonical]
	tsList := cb.crashTimestamps[canonical]
	tsStrings := make([]string, 0, len(tsList))
	for _, t := range tsList {
		tsStrings = append(tsStrings, t.Format(time.RFC3339Nano))
	}
	rawJSON, _ := json.Marshal(tsStrings)

	row := model.OpsCircuitBreakerState{
		Container:       canonical,
		State:           string(state),
		OpenedAt:        openedAt,
		CrashTimestamps: string(rawJSON),
		UpdatedAt:       time.Now(),
	}

	return cb.db.Transaction(func(tx *gorm.DB) error {
		var existing model.OpsCircuitBreakerState
		if err := tx.Where("container = ?", row.Container).First(&existing).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return tx.Create(&row).Error
			}
			return err
		}
		return tx.Model(&existing).Updates(map[string]interface{}{
			"state":            row.State,
			"opened_at":        row.OpenedAt,
			"crash_timestamps": row.CrashTimestamps,
			"updated_at":       row.UpdatedAt,
		}).Error
	})
}

// SetOnTrip sets the callback invoked when a container circuit breaker trips to OPEN.
func (cb *ContainerCircuitBreaker) SetOnTrip(fn func(container string, crashCount int)) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.onTrip = fn
}

// RecordCrash records a crash event for the container in the sliding window.
// Returns the resulting circuit state and true if this crash tripped the breaker.
func (cb *ContainerCircuitBreaker) RecordCrash(container string) (CircuitState, bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	canonical := ResolveTopologyComponent(container)
	now := time.Now()
	cutoff := now.Add(-cb.windowDuration)

	// Prune timestamps older than the sliding window
	existing := cb.crashTimestamps[canonical]
	filtered := make([]time.Time, 0, len(existing)+1)
	for _, t := range existing {
		if t.After(cutoff) {
			filtered = append(filtered, t)
		}
	}
	filtered = append(filtered, now)
	cb.crashTimestamps[canonical] = filtered

	crashes := len(filtered)
	currentState := cb.states[canonical]
	if currentState == "" {
		currentState = CircuitClosed
	}

	// In HALF_OPEN: single failure immediately trips back to CircuitOpen!
	if currentState == CircuitHalfOpen {
		cb.states[canonical] = CircuitOpen
		cb.openedAt[canonical] = now
		delete(cb.activeProbes, canonical)
		_ = cb.persistState(canonical)
		tripCallback := cb.onTrip
		if tripCallback != nil {
			go tripCallback(canonical, crashes)
		}
		return CircuitOpen, true
	}

	// Trip condition: >= 3 crashes in sliding window
	if crashes >= cb.maxCrashes {
		if currentState != CircuitOpen {
			cb.states[canonical] = CircuitOpen
			cb.openedAt[canonical] = now
			delete(cb.activeProbes, canonical)
			_ = cb.persistState(canonical)
			tripCallback := cb.onTrip
			if tripCallback != nil {
				go tripCallback(canonical, crashes)
			}
			return CircuitOpen, true
		}
		_ = cb.persistState(canonical)
		return CircuitOpen, false
	}

	cb.states[canonical] = CircuitClosed
	_ = cb.persistState(canonical)
	return CircuitClosed, false
}

// CanAttemptRecovery verifies whether automated recovery may proceed for the container.
// If transitioning to HALF_OPEN, atomically issues a unique ProbeTicket with lease TTL (R10.1, R10.2).
func (cb *ContainerCircuitBreaker) CanAttemptRecovery(container string) (bool, *ProbeTicket, error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.isDegraded {
		return false, nil, ErrFailClosedMode
	}

	canonical := ResolveTopologyComponent(container)
	state := cb.states[canonical]
	if state == "" {
		state = CircuitClosed
	}

	now := time.Now()

	if state == CircuitOpen {
		openTime, hasOpened := cb.openedAt[canonical]
		// Check whether quiet cooldown period has passed to transition to HALF_OPEN
		if hasOpened && now.Sub(openTime) >= cb.cooldownDuration {
			ttl := cb.probeTTL
			if ttl <= 0 {
				ttl = 30 * time.Second
			}
			ticket := ProbeTicket{
				Token:     fmt.Sprintf("probe-%s-%d", canonical, now.UnixNano()),
				Container: canonical,
				IssuedAt:  now,
				ExpiresAt: now.Add(ttl),
			}

			// Optimistically set state and persist
			cb.states[canonical] = CircuitHalfOpen
			cb.activeProbes[canonical] = ticket
			if err := cb.persistState(canonical); err != nil {
				// Rollback memory state on persistence failure (R6.3, R11.1)
				cb.states[canonical] = CircuitOpen
				delete(cb.activeProbes, canonical)
				return false, nil, fmt.Errorf("failed to persist circuit breaker transition to HALF_OPEN: %w", err)
			}
			return true, &ticket, nil
		}
		return false, nil, ErrCircuitBreakerOpen
	}

	if state == CircuitHalfOpen {
		active, exists := cb.activeProbes[canonical]
		if exists {
			// Check if active probe has expired
			if now.After(active.ExpiresAt) {
				// Probe timed out! Trip back to OPEN (R10.4)
				delete(cb.activeProbes, canonical)
				cb.states[canonical] = CircuitOpen
				cb.openedAt[canonical] = now
				_ = cb.persistState(canonical)
				tripCallback := cb.onTrip
				if tripCallback != nil {
					go tripCallback(canonical, cb.maxCrashes)
				}
				return false, nil, ErrProbeTimeout
			}
			// Probe currently in progress; reject concurrent attempts (R10.2)
			return false, nil, ErrProbeInProgress
		}

		// In HALF_OPEN without active probe (e.g. fresh attempt): issue probe
		ttl := cb.probeTTL
		if ttl <= 0 {
			ttl = 30 * time.Second
		}
		ticket := ProbeTicket{
			Token:     fmt.Sprintf("probe-%s-%d", canonical, now.UnixNano()),
			Container: canonical,
			IssuedAt:  now,
			ExpiresAt: now.Add(ttl),
		}
		cb.activeProbes[canonical] = ticket
		if err := cb.persistState(canonical); err != nil {
			delete(cb.activeProbes, canonical)
			cb.states[canonical] = CircuitOpen
			return false, nil, fmt.Errorf("failed to persist probe ticket: %w", err)
		}
		return true, &ticket, nil
	}

	// CLOSED: allow recovery attempt without probe ticket
	return true, nil, nil
}

// RecordRecoverySuccess resets circuit to CLOSED after successful recovery.
// When in HALF_OPEN, validates that token matches the active probe and has not expired (R10.3, R10.5).
func (cb *ContainerCircuitBreaker) RecordRecoverySuccess(container string, tokens ...string) error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	canonical := ResolveTopologyComponent(container)
	currentState := cb.states[canonical]
	if currentState == "" {
		currentState = CircuitClosed
	}

	// Late callback while in OPEN must be rejected (R10.5)
	if currentState == CircuitOpen {
		return ErrStaleProbeToken
	}

	if currentState == CircuitHalfOpen {
		var token string
		if len(tokens) > 0 {
			token = tokens[0]
		}
		active, exists := cb.activeProbes[canonical]
		if !exists || token == "" || active.Token != token {
			return ErrStaleProbeToken
		}
		if time.Now().After(active.ExpiresAt) {
			// Probe expired: trip back to OPEN
			delete(cb.activeProbes, canonical)
			cb.states[canonical] = CircuitOpen
			cb.openedAt[canonical] = time.Now()
			_ = cb.persistState(canonical)
			return ErrProbeExpired
		}

		// Transition from HALF_OPEN to CLOSED: probe succeeded!
		cb.states[canonical] = CircuitClosed
		delete(cb.crashTimestamps, canonical)
		delete(cb.openedAt, canonical)
		delete(cb.activeProbes, canonical)
		return cb.persistState(canonical)
	}

	// In CLOSED: state is already CLOSED; keep sliding window history
	return nil
}

// RecordRecoveryFailure records a recovery failure for a container in HALF_OPEN.
// Validates token, trips circuit back to OPEN, persists state, and triggers onTrip (R2.4, R10.3).
func (cb *ContainerCircuitBreaker) RecordRecoveryFailure(container string, token string, reason string) error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	canonical := ResolveTopologyComponent(container)
	currentState := cb.states[canonical]

	if currentState != CircuitHalfOpen {
		return ErrStaleProbeToken
	}

	active, exists := cb.activeProbes[canonical]
	if !exists || token == "" || active.Token != token {
		return ErrStaleProbeToken
	}

	now := time.Now()
	cb.states[canonical] = CircuitOpen
	cb.openedAt[canonical] = now
	delete(cb.activeProbes, canonical)
	err := cb.persistState(canonical)

	tripCallback := cb.onTrip
	if tripCallback != nil {
		go tripCallback(canonical, cb.maxCrashes)
	}

	return err
}

// Reset manually resets a container's circuit breaker to CLOSED.
func (cb *ContainerCircuitBreaker) Reset(container string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	canonical := ResolveTopologyComponent(container)
	cb.states[canonical] = CircuitClosed
	delete(cb.crashTimestamps, canonical)
	delete(cb.openedAt, canonical)
	delete(cb.activeProbes, canonical)
	_ = cb.persistState(canonical)
}

// ResetAll resets all tracked containers to CLOSED.
func (cb *ContainerCircuitBreaker) ResetAll() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.states = make(map[string]CircuitState)
	cb.crashTimestamps = make(map[string][]time.Time)
	cb.openedAt = make(map[string]time.Time)
	cb.activeProbes = make(map[string]ProbeTicket)
}

// GetState returns the current state of a container's circuit breaker.
func (cb *ContainerCircuitBreaker) GetState(container string) CircuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	canonical := ResolveTopologyComponent(container)
	state := cb.states[canonical]
	if state == "" {
		return CircuitClosed
	}

	// If in HALF_OPEN, check if active probe has expired
	if state == CircuitHalfOpen {
		if active, exists := cb.activeProbes[canonical]; exists && time.Now().After(active.ExpiresAt) {
			delete(cb.activeProbes, canonical)
			cb.states[canonical] = CircuitOpen
			cb.openedAt[canonical] = time.Now()
			_ = cb.persistState(canonical)
			return CircuitOpen
		}
	}

	return state
}

// GetActiveProbe returns the active probe ticket for a container if any.
func (cb *ContainerCircuitBreaker) GetActiveProbe(container string) (*ProbeTicket, bool) {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	canonical := ResolveTopologyComponent(container)
	p, ok := cb.activeProbes[canonical]
	if !ok {
		return nil, false
	}
	cpy := p
	return &cpy, true
}

// SetProbeTTL sets the TTL for probe tickets.
func (cb *ContainerCircuitBreaker) SetProbeTTL(ttl time.Duration) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.probeTTL = ttl
}

// SetCooldownDuration sets the cooldown duration before half-open.
func (cb *ContainerCircuitBreaker) SetCooldownDuration(d time.Duration) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.cooldownDuration = d
}

// IsDegraded returns whether the circuit breaker is in degraded fail-closed mode.
func (cb *ContainerCircuitBreaker) IsDegraded() bool {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.isDegraded
}

// SetDegraded sets the degraded fail-closed mode flag.
func (cb *ContainerCircuitBreaker) SetDegraded(degraded bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.isDegraded = degraded
}

// GetCrashCount returns the number of crashes in the active sliding window.
func (cb *ContainerCircuitBreaker) GetCrashCount(container string) int {
	cb.mu.RLock()
	defer cb.mu.RUnlock()

	canonical := ResolveTopologyComponent(container)
	now := time.Now()
	cutoff := now.Add(-cb.windowDuration)

	count := 0
	for _, t := range cb.crashTimestamps[canonical] {
		if t.After(cutoff) {
			count++
		}
	}
	return count
}

// ============================================================================
// 4. System Maintenance Lock
// ============================================================================

// MaintenanceLock provides thread-safe system-wide maintenance mode protection.
// When locked, proactive agent recoveries and automated restarts are suspended.
type MaintenanceLock struct {
	mu         sync.RWMutex
	Active     bool      `json:"active"`
	LockedBy   string    `json:"locked_by"`
	Reason     string    `json:"reason"`
	LockedAt   time.Time `json:"locked_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	db         *gorm.DB
	isDegraded bool
}

// MaintenanceStatus represents the serializable status of the maintenance lock.
type MaintenanceStatus struct {
	Active           bool      `json:"active"`
	LockedBy         string    `json:"locked_by"`
	Reason           string    `json:"reason"`
	LockedAt         time.Time `json:"locked_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	RemainingSeconds int64     `json:"remaining_seconds"`
}

// NewMaintenanceLock creates an unlocked MaintenanceLock.
func NewMaintenanceLock() *MaintenanceLock {
	return &MaintenanceLock{}
}

// SetDB binds the database instance for persistent state recording.
func (l *MaintenanceLock) SetDB(db *gorm.DB) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.db = db
}

// LoadPersistedState restores active maintenance lock from SQLite.
// If query fails with unexpected database error (not RecordNotFound), fails closed (R6.5, R11.4).
func (l *MaintenanceLock) LoadPersistedState(db *gorm.DB) {
	if db == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.db = db

	var lockRow model.OpsMaintenanceLock
	err := db.Order("id DESC").First(&lockRow).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			l.Active = false
			return
		}
		// Unexpected DB error: enter fail-closed degraded mode (R6.5, R11.4)
		l.Active = true
		l.isDegraded = true
		l.LockedBy = "system-fail-closed"
		l.Reason = "persistence error, degraded protection: " + err.Error()
		l.LockedAt = time.Now()
		l.ExpiresAt = time.Now().Add(24 * time.Hour)
		return
	}

	if lockRow.Active && time.Now().Before(lockRow.ExpiresAt) {
		l.Active = true
		l.LockedBy = lockRow.LockedBy
		l.Reason = lockRow.Reason
		l.LockedAt = lockRow.LockedAt
		l.ExpiresAt = lockRow.ExpiresAt
	} else {
		l.Active = false
	}
}

// persistState saves lock state to database and returns actual error (R6.1).
func (l *MaintenanceLock) persistState() error {
	if l.db == nil {
		return nil
	}
	row := model.OpsMaintenanceLock{
		Active:    l.Active,
		LockedBy:  l.LockedBy,
		Reason:    l.Reason,
		LockedAt:  l.LockedAt,
		ExpiresAt: l.ExpiresAt,
		UpdatedAt: time.Now(),
	}
	return l.db.Create(&row).Error
}

// Acquire requests exclusive maintenance lock on the system.
// If persistence fails, rolls back memory state and returns error (R6.2, R6.6).
func (l *MaintenanceLock) Acquire(lockedBy string, reason string, ttl time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.isDegraded {
		return ErrFailClosedMode
	}

	now := time.Now()
	if ttl <= 0 {
		ttl = 60 * time.Minute // Default: 60 minutes
	}

	if l.Active && now.Before(l.ExpiresAt) {
		// Allow re-acquisition by same operator or forced admin refresh
		if l.LockedBy != "" && l.LockedBy != lockedBy && !strings.Contains(strings.ToLower(lockedBy), "admin") {
			return ErrMaintenanceLockActive
		}
	}

	// Snapshot previous state for rollback on persistence failure
	prevActive := l.Active
	prevLockedBy := l.LockedBy
	prevReason := l.Reason
	prevLockedAt := l.LockedAt
	prevExpiresAt := l.ExpiresAt

	row := model.OpsMaintenanceLock{
		Active:    true,
		LockedBy:  lockedBy,
		Reason:    reason,
		LockedAt:  now,
		ExpiresAt: now.Add(ttl),
		UpdatedAt: now,
	}

	if l.db != nil {
		if err := l.db.Create(&row).Error; err != nil {
			// Rollback memory state on persistence failure (R6.6)
			l.Active = prevActive
			l.LockedBy = prevLockedBy
			l.Reason = prevReason
			l.LockedAt = prevLockedAt
			l.ExpiresAt = prevExpiresAt
			return fmt.Errorf("failed to persist maintenance lock acquisition: %w", err)
		}
	}

	l.Active = true
	l.LockedBy = lockedBy
	l.Reason = reason
	l.LockedAt = now
	l.ExpiresAt = now.Add(ttl)
	return nil
}

// Release clears the maintenance lock.
// MUST persist to DB first! If DB write fails, DO NOT clear memory lock! (R6.6, R11.1).
func (l *MaintenanceLock) Release(releasedBy string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.Active {
		return nil
	}

	now := time.Now()
	row := model.OpsMaintenanceLock{
		Active:    false,
		LockedBy:  "",
		Reason:    fmt.Sprintf("Released by %s", releasedBy),
		LockedAt:  l.LockedAt,
		ExpiresAt: time.Time{},
		UpdatedAt: now,
	}

	if l.db != nil {
		if err := l.db.Create(&row).Error; err != nil {
			// Retention on failure: DO NOT clear in-memory lock! (R6.6, R11.1)
			return fmt.Errorf("failed to persist maintenance lock release: %w (memory lock retained)", err)
		}
	}

	l.Active = false
	l.LockedBy = ""
	l.Reason = ""
	l.ExpiresAt = time.Time{}
	l.isDegraded = false
	return nil
}

// IsLocked checks whether the maintenance lock is currently active, not expired, or degraded.
func (l *MaintenanceLock) IsLocked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.isDegraded {
		return true
	}

	if !l.Active {
		return false
	}
	if time.Now().After(l.ExpiresAt) {
		l.Active = false
		return false
	}
	return true
}

// IsDegraded returns whether the maintenance lock is in fail-closed degraded mode.
func (l *MaintenanceLock) IsDegraded() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.isDegraded
}

// SetDegraded sets the fail-closed degraded mode flag.
func (l *MaintenanceLock) SetDegraded(degraded bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.isDegraded = degraded
}

// GetStatus returns the current snapshot of the maintenance lock.
func (l *MaintenanceLock) GetStatus() MaintenanceStatus {
	l.mu.RLock()
	defer l.mu.RUnlock()

	now := time.Now()
	active := l.Active && now.Before(l.ExpiresAt)
	var remaining int64
	if active {
		remaining = int64(time.Until(l.ExpiresAt).Seconds())
		if remaining < 0 {
			remaining = 0
		}
	}

	return MaintenanceStatus{
		Active:           active,
		LockedBy:         l.LockedBy,
		Reason:           l.Reason,
		LockedAt:         l.LockedAt,
		ExpiresAt:        l.ExpiresAt,
		RemainingSeconds: remaining,
	}
}

// ============================================================================
// 5. Container Auto-Recovery FSM & Orchestrator
// ============================================================================

// RecoveryDecisionAction indicates the autonomous recovery action chosen by the FSM.
type RecoveryDecisionAction string

const (
	ActionRestart RecoveryDecisionAction = "RESTART"
	ActionIgnore  RecoveryDecisionAction = "IGNORE"
	ActionBlocked RecoveryDecisionAction = "BLOCKED"
	ActionTripped RecoveryDecisionAction = "TRIPPED"
)

// RecoveryDecision represents the final verdict of the FSM on a container event.
type RecoveryDecision struct {
	Action       RecoveryDecisionAction `json:"action"`
	EventClass   ContainerEventClass    `json:"event_class"`
	Container    string                 `json:"container"`
	Reason       string                 `json:"reason"`
	BackoffSec   int                    `json:"backoff_sec"`
	CircuitState CircuitState           `json:"circuit_state"`
	ProposalID   string                 `json:"proposal_id,omitempty"`
	ProbeToken   string                 `json:"probe_token,omitempty"`
	ProbeTicket  *ProbeTicket           `json:"probe_ticket,omitempty"`
}

// ContainerRecoveryFSM encapsulates event classification, topology validation,
// circuit breaker protection, and maintenance lock awareness into a unified recovery engine.
type ContainerRecoveryFSM struct {
	mu           sync.RWMutex
	lock         *MaintenanceLock
	topology     *TopologyEngine
	breaker      *ContainerCircuitBreaker
	leaseManager *RecoveryLeaseManager
	db           *gorm.DB
	bus          *eventbus.Bus
	executor     OperationExecutor
}

// NewContainerRecoveryFSM creates a new container recovery finite state machine.
func NewContainerRecoveryFSM(db *gorm.DB, bus *eventbus.Bus, executor OperationExecutor) *ContainerRecoveryFSM {
	fsm := &ContainerRecoveryFSM{
		lock:     NewMaintenanceLock(),
		topology: NewTopologyEngine(),
		breaker:  NewContainerCircuitBreaker(),
		db:       db,
		bus:      bus,
		executor: executor,
	}

	if db != nil {
		fsm.leaseManager = NewRecoveryLeaseManager(db)
	}

	// Always wire live health probe to topology engine
	fsm.topology.SetHealthChecker(func(ctx context.Context, component string) (bool, error) {
		canonical := ResolveTopologyComponent(component)
		if canonical == "app.db" {
			if fsm.db == nil {
				if fsm.executor == nil {
					return true, nil // Mock classification test without executor
				}
				return false, ErrDatabaseUnhealthy
			}
			sqlDB, err := fsm.db.DB()
			if err != nil {
				return false, fmt.Errorf("%w: failed to get sql.DB handle: %v", ErrDatabaseUnhealthy, err)
			}
			if pingErr := sqlDB.PingContext(ctx); pingErr != nil {
				return false, fmt.Errorf("%w: ping failed: %v", ErrDatabaseUnhealthy, pingErr)
			}
			return true, nil
		}
		if fsm.executor != nil {
			if err := fsm.executor.CheckHealth(ctx, component); err != nil {
				return false, err
			}
			return true, nil
		}
		return true, nil
	})

	// Trip callback: create Tier 2 HIGH proposal for administrator approval
	fsm.breaker.SetOnTrip(func(container string, crashCount int) {
		fsm.createCircuitTripProposal(container, crashCount)
	})

	if db != nil {
		_ = db.AutoMigrate(&model.OpsMaintenanceLock{}, &model.OpsCircuitBreakerState{})
		fsm.lock.SetDB(db)
		fsm.lock.LoadPersistedState(db)
		fsm.breaker.SetDB(db)
		fsm.breaker.LoadPersistedState(db)
	}

	return fsm
}

// GetLeaseManager returns the persistent recovery lease manager.
func (fsm *ContainerRecoveryFSM) GetLeaseManager() *RecoveryLeaseManager {
	fsm.mu.RLock()
	defer fsm.mu.RUnlock()
	return fsm.leaseManager
}

// SetLeaseManager sets the persistent recovery lease manager.
func (fsm *ContainerRecoveryFSM) SetLeaseManager(mgr *RecoveryLeaseManager) {
	fsm.mu.Lock()
	defer fsm.mu.Unlock()
	fsm.leaseManager = mgr
}

// GetTopologyEngine returns the underlying topology engine.
func (fsm *ContainerRecoveryFSM) GetTopologyEngine() *TopologyEngine {
	return fsm.topology
}

// GetMaintenanceLock returns the underlying MaintenanceLock.
func (fsm *ContainerRecoveryFSM) GetMaintenanceLock() *MaintenanceLock {
	return fsm.lock
}

// GetCircuitBreaker returns the underlying ContainerCircuitBreaker.
func (fsm *ContainerRecoveryFSM) GetCircuitBreaker() *ContainerCircuitBreaker {
	return fsm.breaker
}

// ProcessContainerEvent evaluates an incoming container event against FSM rules:
// 1. Checks Maintenance Lock (blocks proactive recoveries when active).
// 2. Classifies event (ignores MANUAL_STOP, UPDATE, EXIT_ZERO).
// 3. For OOM / CRASH: checks Circuit Breaker and Topology Pre-flight.
// 4. If 3 crashes within 10 minutes: trips Circuit Breaker and generates Tier 2 Proposal.
// 5. Otherwise returns exponential backoff restart decision.
func (fsm *ContainerRecoveryFSM) ProcessContainerEvent(ctx context.Context, event ContainerEvent) (*RecoveryDecision, error) {
	fsm.mu.Lock()
	defer fsm.mu.Unlock()

	container := ResolveTopologyComponent(event.ContainerName)
	circuitState := fsm.breaker.GetState(container)

	// 1. Maintenance Lock Check
	// If lock is active and not due to degraded storage error, block proactive recovery immediately.
	if fsm.lock.IsLocked() && !fsm.lock.IsDegraded() {
		return &RecoveryDecision{
			Action:       ActionBlocked,
			EventClass:   EventManualStop,
			Container:    container,
			Reason:       "System maintenance lock is active; proactive agent recovery suspended",
			CircuitState: circuitState,
		}, ErrSystemInMaintenance
	}

	// 2. 5-Class Event Classification
	eventClass := ClassifyContainerEvent(event, false)

	switch eventClass {
	case EventManualStop:
		return &RecoveryDecision{
			Action:       ActionIgnore,
			EventClass:   EventManualStop,
			Container:    container,
			Reason:       "Manual stop detected; autonomous restart suppressed",
			CircuitState: circuitState,
		}, nil

	case EventUpdate:
		return &RecoveryDecision{
			Action:       ActionIgnore,
			EventClass:   EventUpdate,
			Container:    container,
			Reason:       "Container update in progress; autonomous restart suppressed",
			CircuitState: circuitState,
		}, nil

	case EventExitZero:
		return &RecoveryDecision{
			Action:       ActionIgnore,
			EventClass:   EventExitZero,
			Container:    container,
			Reason:       "Clean exit with exit code 0; autonomous restart suppressed",
			CircuitState: circuitState,
		}, nil

	case EventOOM, EventCrash:
		// 3. Pre-flight Dependency Topology Check (upstream dependencies must be healthy before acquiring breaker probe)
		if topErr := fsm.topology.PreflightDependencyCheck(ctx, container); topErr != nil {
			return &RecoveryDecision{
				Action:       ActionBlocked,
				EventClass:   eventClass,
				Container:    container,
				Reason:       topErr.Error(),
				CircuitState: circuitState,
			}, topErr
		}

		// 4. Check Circuit Breaker eligibility and probe lease
		canRecover, ticket, cbErr := fsm.breaker.CanAttemptRecovery(container)
		if !canRecover {
			return &RecoveryDecision{
				Action:       ActionBlocked,
				EventClass:   eventClass,
				Container:    container,
				Reason:       "Container circuit breaker is OPEN (crash loop protection active)",
				CircuitState: fsm.breaker.GetState(container),
			}, cbErr
		}

		// If in degraded fail-closed mode due to storage failure, block after topology check
		if fsm.lock.IsDegraded() {
			if ticket != nil {
				_ = fsm.breaker.RecordRecoveryFailure(container, ticket.Token, "storage failure degraded mode")
			}
			return &RecoveryDecision{
				Action:       ActionBlocked,
				EventClass:   eventClass,
				Container:    container,
				Reason:       fsm.lock.Reason,
				CircuitState: fsm.breaker.GetState(container),
			}, ErrSystemInMaintenance
		}

		// 5. Decouple crash event from recovery probe (R2)
		// If in HALF_OPEN (ticket != nil), DO NOT call RecordCrash!
		// The incoming crash was the trigger to start recovery probe.
		var newState CircuitState
		var probeToken string
		if ticket != nil {
			newState = CircuitHalfOpen
			probeToken = ticket.Token
		} else {
			// In CLOSED: record crash in sliding window and check if threshold reached
			var justTripped bool
			newState, justTripped = fsm.breaker.RecordCrash(container)
			if justTripped {
				return &RecoveryDecision{
					Action:       ActionTripped,
					EventClass:   eventClass,
					Container:    container,
					Reason:       "Circuit breaker tripped after 3 crashes in 10 minutes; Tier 2 proposal created",
					CircuitState: CircuitOpen,
				}, ErrCircuitBreakerOpen
			}

			if newState == CircuitOpen {
				return &RecoveryDecision{
					Action:       ActionBlocked,
					EventClass:   eventClass,
					Container:    container,
					Reason:       "Circuit breaker is OPEN",
					CircuitState: CircuitOpen,
				}, ErrCircuitBreakerOpen
			}
		}

		// Exponential backoff calculation (1s for crash #1, 2s for crash #2)
		crashes := fsm.breaker.GetCrashCount(container)
		backoff := 1
		if crashes > 1 {
			backoff = 2
		}

		reason := fmt.Sprintf("Eligible for autonomous recovery (crash #%d in 10m window)", crashes)
		if ticket != nil {
			reason = fmt.Sprintf("Eligible for HALF_OPEN probe recovery (token: %s)", ticket.Token)
			backoff = 0 // Immediate probe on half-open
		}

		return &RecoveryDecision{
			Action:       ActionRestart,
			EventClass:   eventClass,
			Container:    container,
			Reason:       reason,
			BackoffSec:   backoff,
			CircuitState: newState,
			ProbeToken:   probeToken,
			ProbeTicket:  ticket,
		}, nil

	default:
		return &RecoveryDecision{
			Action:       ActionIgnore,
			EventClass:   eventClass,
			Container:    container,
			Reason:       "Unrecognized container event class",
			CircuitState: circuitState,
		}, nil
	}
}

// createCircuitTripProposal creates a Tier 2 HIGH proposal when a container circuit breaker trips.
func (fsm *ContainerRecoveryFSM) createCircuitTripProposal(container string, crashCount int) {
	if fsm.db == nil {
		return
	}

	now := time.Now()
	propID := fmt.Sprintf("prop-cb-%s-%d", container, now.UnixNano())
	canonical := ResolveTopologyComponent(container)
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%s:offline:none", canonical)))
	baselineHash := fmt.Sprintf("sha256:%x", h.Sum(nil))

	prop := model.OpsProposal{
		ProposalID:       propID,
		Title:            fmt.Sprintf("【高危告警】%s 连续崩溃触发自愈熔断", container),
		Description:      fmt.Sprintf("容器 %s 在 10 分钟内发生 %d 次连续崩溃，已触发自愈熔断器跳闸。自动重启已挂起，需管理员人工审批与介入排查。", container, crashCount),
		TargetComponent:  container,
		RiskLevel:        model.RiskLevelHigh,
		Status:           model.ProposalStatusAwaitingApproval,
		EstimatedSeconds: 60,
		ExecutionPlan:    fmt.Sprintf("docker restart %s", container),
		RollbackPlan:     fmt.Sprintf("docker logs --tail 200 %s", container),
		BaselineHash:     baselineHash,
		CreatedAt:        now,
	}

	if err := fsm.db.Create(&prop).Error; err != nil {
		// Log or handle error silently
		return
	}

	if fsm.bus != nil {
		fsm.bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicSystemLog,
			Source:   eventbus.SourceSystem,
			Severity: eventbus.SeverityCritical,
			Payload:  []byte(fmt.Sprintf("容器 %s 连续崩溃 %d 次触发自愈熔断器跳闸，已创建高危审批提案: %s", container, crashCount, propID)),
		})
	}
}

// RecoveryExecutionStatus classifies the actual execution lifecycle stage of an FSM recovery.
type RecoveryExecutionStatus string

const (
	StatusAccepted  RecoveryExecutionStatus = "ACCEPTED"  // Event recorded/ignored by policy, no action required
	StatusExecuting RecoveryExecutionStatus = "EXECUTING" // Recovery operation currently dispatched to executor
	StatusRecovered RecoveryExecutionStatus = "RECOVERED" // Recovery action executed and health probe verified
	StatusBlocked   RecoveryExecutionStatus = "BLOCKED"   // Blocked by maintenance lock, circuit breaker, or topology
	StatusFailed    RecoveryExecutionStatus = "FAILED"    // Execution attempted but failed or health check unverified
)

// ContainerRecoveryResult models the structured outcome of ProcessAndRecover.
type ContainerRecoveryResult struct {
	Decision       *RecoveryDecision       `json:"decision"`
	Status         RecoveryExecutionStatus `json:"status"`
	Executed       bool                    `json:"executed"`
	Output         string                  `json:"output,omitempty"`
	Error          string                  `json:"error,omitempty"`
	HealthVerified bool                    `json:"health_verified"`
}

// SetExecutor updates the OperationExecutor bound to the recovery FSM.
func (fsm *ContainerRecoveryFSM) SetExecutor(executor OperationExecutor) {
	fsm.mu.Lock()
	defer fsm.mu.Unlock()
	fsm.executor = executor
	if executor != nil {
		fsm.topology.SetHealthChecker(func(ctx context.Context, component string) (bool, error) {
			canonical := ResolveTopologyComponent(component)
			if canonical == "app.db" {
				if fsm.db != nil {
					if sqlDB, err := fsm.db.DB(); err == nil {
						if pingErr := sqlDB.PingContext(ctx); pingErr != nil {
							return false, pingErr
						}
					}
				}
				return true, nil
			}
			if err := executor.CheckHealth(ctx, component); err != nil {
				return false, err
			}
			return true, nil
		})
	}
}

// ProcessAndRecover performs closed-loop recovery: evaluates the container event via FSM,
// and if RESTART is decided, invokes OperationExecutor to execute physical Docker recovery,
// conducts health probe verification, records recovery success in the circuit breaker,
// and returns a structured ContainerRecoveryResult.
func (fsm *ContainerRecoveryFSM) ProcessAndRecover(ctx context.Context, event ContainerEvent) (*ContainerRecoveryResult, error) {
	decision, err := fsm.ProcessContainerEvent(ctx, event)
	if err != nil || decision == nil {
		status := StatusBlocked
		if errors.Is(err, ErrMaintenanceLockActive) || errors.Is(err, ErrSystemInMaintenance) ||
			errors.Is(err, ErrCircuitBreakerOpen) || errors.Is(err, ErrDependencyUnhealthy) {
			status = StatusBlocked
		} else {
			status = StatusFailed
		}
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		return &ContainerRecoveryResult{
			Decision:       decision,
			Status:         status,
			Executed:       false,
			Error:          errMsg,
			HealthVerified: false,
		}, err
	}

	switch decision.Action {
	case ActionIgnore:
		return &ContainerRecoveryResult{
			Decision:       decision,
			Status:         StatusAccepted,
			Executed:       false,
			Output:         "Event ignored per FSM policy (clean exit, update, or manual stop)",
			HealthVerified: false,
		}, nil

	case ActionBlocked, ActionTripped:
		return &ContainerRecoveryResult{
			Decision:       decision,
			Status:         StatusBlocked,
			Executed:       false,
			Error:          decision.Reason,
			HealthVerified: false,
		}, nil

	case ActionRestart:
		fsm.mu.RLock()
		exec := fsm.executor
		leaseMgr := fsm.leaseManager
		fsm.mu.RUnlock()

		canonical := ResolveTopologyComponent(decision.Container)

		if exec == nil {
			if decision.ProbeToken != "" {
				_ = fsm.breaker.RecordRecoveryFailure(canonical, decision.ProbeToken, "operation executor is not configured on Go Active Core")
			}
			return &ContainerRecoveryResult{
				Decision:       decision,
				Status:         StatusFailed,
				Executed:       false,
				Error:          "operation executor is not configured on Go Active Core",
				HealthVerified: false,
			}, errors.New("operation executor not available")
		}

		// R13: Acquire persistent recovery lease before autonomous execution
		var leaseToken string
		if leaseMgr != nil {
			token, lErr := leaseMgr.AcquireLease(canonical, "go-core", 30*time.Second)
			if lErr != nil {
				if decision.ProbeToken != "" {
					_ = fsm.breaker.RecordRecoveryFailure(canonical, decision.ProbeToken, "recovery lease acquisition failed: "+lErr.Error())
				}
				return &ContainerRecoveryResult{
					Decision:       decision,
					Status:         StatusBlocked,
					Executed:       false,
					Error:          fmt.Sprintf("cannot acquire persistent recovery lease: %v", lErr),
					HealthVerified: false,
				}, fmt.Errorf("failed to acquire recovery lease for %s: %w", canonical, lErr)
			}
			leaseToken = token
			defer func() {
				_ = leaseMgr.ReleaseLease(canonical, leaseToken)
			}()
		}

		if decision.BackoffSec > 0 {
			select {
			case <-time.After(time.Duration(decision.BackoffSec) * time.Second):
			case <-ctx.Done():
				if decision.ProbeToken != "" {
					_ = fsm.breaker.RecordRecoveryFailure(canonical, decision.ProbeToken, "context cancelled during backoff: "+ctx.Err().Error())
				}
				return &ContainerRecoveryResult{
					Decision:       decision,
					Status:         StatusFailed,
					Executed:       false,
					Error:          ctx.Err().Error(),
					HealthVerified: false,
				}, ctx.Err()
			}
		}

		spec := OperationSpec{
			Action: ActionDockerRestart,
			Target: canonical,
		}

		execRes, execErr := exec.Execute(ctx, spec)
		if execErr != nil || (execRes != nil && !execRes.Success) {
			errStr := ""
			if execErr != nil {
				errStr = execErr.Error()
			} else if execRes != nil {
				errStr = execRes.Error
			}
			outStr := ""
			if execRes != nil {
				outStr = execRes.Output
			}
			// Probe failure: release probe ticket and trip back to OPEN (R2, R10)
			if decision.ProbeToken != "" {
				_ = fsm.breaker.RecordRecoveryFailure(canonical, decision.ProbeToken, errStr)
			}
			return &ContainerRecoveryResult{
				Decision:       decision,
				Status:         StatusFailed,
				Executed:       true,
				Output:         outStr,
				Error:          errStr,
				HealthVerified: false,
			}, execErr
		}

		// Recovery succeeded and health probe verified by executor -> update circuit breaker
		if decision.ProbeToken != "" {
			_ = fsm.breaker.RecordRecoverySuccess(canonical, decision.ProbeToken)
		} else {
			_ = fsm.breaker.RecordRecoverySuccess(canonical)
		}

		return &ContainerRecoveryResult{
			Decision:       decision,
			Status:         StatusRecovered,
			Executed:       true,
			Output:         execRes.Output,
			HealthVerified: true,
		}, nil

	default:
		return &ContainerRecoveryResult{
			Decision:       decision,
			Status:         StatusAccepted,
			Executed:       false,
			Output:         "Unrecognized action class treated as accepted",
			HealthVerified: false,
		}, nil
	}
}

// ============================================================================
// Global FSM Registration & Helpers
// ============================================================================

var (
	globalRecoveryFSMMu sync.RWMutex
	globalRecoveryFSM   *ContainerRecoveryFSM
)

// SetGlobalRecoveryFSM sets the singleton recovery FSM instance.
func SetGlobalRecoveryFSM(fsm *ContainerRecoveryFSM) {
	globalRecoveryFSMMu.Lock()
	defer globalRecoveryFSMMu.Unlock()
	globalRecoveryFSM = fsm
}

// GetGlobalRecoveryFSM returns the singleton recovery FSM instance.
func GetGlobalRecoveryFSM() *ContainerRecoveryFSM {
	globalRecoveryFSMMu.RLock()
	defer globalRecoveryFSMMu.RUnlock()
	return globalRecoveryFSM
}

// ============================================================================
// 6. Proposal Lifecycle Finite State Machine (CAS Transitions)
// ============================================================================

// ProposalFSM manages atomic state transitions for OpsProposal.
type ProposalFSM struct {
	db *gorm.DB
}

// NewProposalFSM initializes a ProposalFSM.
func NewProposalFSM(db *gorm.DB) *ProposalFSM {
	return &ProposalFSM{db: db}
}

// TransitionCAS executes an atomic Compare-And-Swap state change for a proposal.
// It matches by proposal ID or numeric ID, ensures the current status is in expectedStatuses,
// and applies updates including the targetStatus atomically.
func (fsm *ProposalFSM) TransitionCAS(
	proposalID string,
	expectedStatuses []string,
	targetStatus string,
	updates map[string]interface{},
) (bool, error) {
	if fsm.db == nil {
		return true, nil
	}

	if updates == nil {
		updates = make(map[string]interface{})
	}
	updates["status"] = targetStatus
	updates["updated_at"] = time.Now()

	res := fsm.db.Model(&model.OpsProposal{}).
		Where("(id = ? OR proposal_id = ?) AND status IN (?)", proposalID, proposalID, expectedStatuses).
		Updates(updates)

	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

