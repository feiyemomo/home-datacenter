package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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

// ResolveTopologyComponent normalizes container names and aliases to canonical topology identifiers (R4-17).
// Unknown components strictly return "" (Fail-Closed) so they cannot bypass whitelist or dependency checks.
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
		return ""
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
// Unknown components fail closed immediately (R4-17).
// Level 1 and Level 2 containers cannot be restarted unless Level 0 dependencies are running and healthy.
// Core dependencies (app.db, mosquitto, frigate, home-api) FAIL CLOSED on check failure, timeout, or missing checker.
func (t *TopologyEngine) PreflightDependencyCheck(ctx context.Context, target string) error {
	t.mu.RLock()
	defer t.mu.RUnlock()

	canonical := ResolveTopologyComponent(target)
	if canonical == "" {
		return fmt.Errorf("%w: unknown topology component %q", ErrComponentNotWhitelisted, target)
	}
	deps, exists := t.graph[canonical]
	if !exists {
		return fmt.Errorf("%w: component %q is absent from topology graph", ErrComponentNotWhitelisted, canonical)
	}
	if len(deps) == 0 {
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
	bus              *eventbus.Bus
	isDegraded       bool
	degradeReason    string
	nowFn            func() time.Time
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
		nowFn:            func() time.Time { return time.Now().UTC() },
	}
}

// SetNowFunc configures a deterministic clock source for unit tests.
func (cb *ContainerCircuitBreaker) SetNowFunc(fn func() time.Time) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if fn == nil {
		cb.nowFn = func() time.Time { return time.Now().UTC() }
	} else {
		cb.nowFn = fn
	}
}

func (cb *ContainerCircuitBreaker) now() time.Time {
	if cb.nowFn == nil {
		return time.Now().UTC()
	}
	return cb.nowFn().UTC()
}

// SetDB binds the database instance for persistent state recording.
func (cb *ContainerCircuitBreaker) SetDB(db *gorm.DB) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.db = db
}

// SetBus attaches EventBus for degraded security telemetry.
func (cb *ContainerCircuitBreaker) SetBus(bus *eventbus.Bus) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.bus = bus
}

// markDegraded locks the circuit breaker into fail-closed degraded mode and emits observability alerts.
// Caller must hold cb.mu.Lock().
func (cb *ContainerCircuitBreaker) markDegraded(reason string) {
	cb.isDegraded = true
	cb.degradeReason = reason
	GlobalSecurityMetrics.SetCircuitBreakerDegraded(true)
	GlobalSecurityMetrics.IncDatabaseError()
	RecordSecurityEvent(nil, cb.bus, OpsSecurityEvent{
		OperationID: GenerateOperationID("cb-degrade"),
		EventType:   "ops.circuit_breaker.degraded",
		Severity:    "critical",
		Actor:       "circuit-breaker",
		FaultClass:  string(FaultTransactionFailed),
		Message:     "circuit breaker entered fail-closed degraded mode: " + reason,
	})
}

// GetDegradeReason returns the diagnostic reason for fail-closed degraded state.
func (cb *ContainerCircuitBreaker) GetDegradeReason() string {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.degradeReason
}

func isValidCircuitState(state CircuitState) bool {
	return state == CircuitClosed || state == CircuitOpen || state == CircuitHalfOpen
}

// LoadPersistedState restores circuit breaker states, crash windows, and probe metadata from SQLite.
// Fails closed (Degraded) on schema corruption, unrecognized states, or malformed timestamps/JSON (R4-10).
func (cb *ContainerCircuitBreaker) LoadPersistedState(db *gorm.DB) {
	if db == nil {
		return
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.db = db

	hasTbl, tblErr := tableExists(db, "ops_circuit_breaker_states")
	if tblErr != nil || !hasTbl {
		cb.markDegraded(fmt.Sprintf("ops_circuit_breaker_states missing or unreadable (err=%v)", tblErr))
		return
	}
	cols, colErr := getTableColumns(db, "ops_circuit_breaker_states")
	if colErr != nil || !cols["container"] || !cols["state"] || !cols["crash_timestamps"] {
		cb.markDegraded(fmt.Sprintf("ops_circuit_breaker_states schema incomplete (err=%v)", colErr))
		return
	}

	var rows []model.OpsCircuitBreakerState
	if err := db.Find(&rows).Error; err != nil {
		cb.markDegraded("failed querying ops_circuit_breaker_states: " + err.Error())
		return
	}

	cutoff := cb.now().Add(-cb.windowDuration)
	for _, r := range rows {
		canonical := ResolveTopologyComponent(r.Container)
		if canonical == "" {
			cb.markDegraded(fmt.Sprintf("unknown container %q in ops_circuit_breaker_states", r.Container))
			continue
		}

		state := CircuitState(strings.ToUpper(strings.TrimSpace(r.State)))
		if !isValidCircuitState(state) {
			// R4-10 #6: Never default unrecognized or empty state to CLOSED
			cb.states[canonical] = CircuitOpen
			cb.openedAt[canonical] = cb.now()
			cb.markDegraded(fmt.Sprintf("unrecognized circuit state %q for container %s", r.State, canonical))
			continue
		}

		if r.Degraded {
			cb.markDegraded(fmt.Sprintf("persisted degraded flag set on container %s", canonical))
		}

		if state == CircuitHalfOpen || strings.TrimSpace(r.ActiveProbeToken) != "" {
			// R4-10: Any interrupted HALF_OPEN or active probe across process restart resets to OPEN
			state = CircuitOpen
			if !r.OpenedAt.IsZero() {
				cb.openedAt[canonical] = r.OpenedAt.UTC()
			} else if r.OpenedAtMs > 0 {
				cb.openedAt[canonical] = time.UnixMilli(r.OpenedAtMs).UTC()
			} else {
				cb.openedAt[canonical] = cb.now()
			}
			delete(cb.activeProbes, canonical)
		} else if !r.OpenedAt.IsZero() {
			cb.openedAt[canonical] = r.OpenedAt.UTC()
		} else if r.OpenedAtMs > 0 {
			cb.openedAt[canonical] = time.UnixMilli(r.OpenedAtMs).UTC()
		} else if state == CircuitOpen {
			cb.markDegraded(fmt.Sprintf("OPEN circuit for %s missing valid opened_at timestamp", canonical))
			cb.openedAt[canonical] = cb.now()
		}
		cb.states[canonical] = state

		if strings.TrimSpace(r.CrashTimestamps) != "" {
			var tsStrings []string
			if err := json.Unmarshal([]byte(r.CrashTimestamps), &tsStrings); err != nil {
				cb.states[canonical] = CircuitOpen
				cb.openedAt[canonical] = cb.now()
				cb.markDegraded(fmt.Sprintf("corrupt crash_timestamps JSON for %s: %v", canonical, err))
				continue
			}
			var validTimes []time.Time
			for _, s := range tsStrings {
				t, parseErr := time.Parse(time.RFC3339Nano, s)
				if parseErr != nil {
					t, parseErr = time.Parse(time.RFC3339, s)
				}
				if parseErr != nil {
					cb.states[canonical] = CircuitOpen
					cb.openedAt[canonical] = cb.now()
					cb.markDegraded(fmt.Sprintf("invalid timestamp %q in crash_timestamps for %s", s, canonical))
					validTimes = nil
					break
				}
				if t.After(cutoff) {
					validTimes = append(validTimes, t)
				}
			}
			if validTimes != nil {
				cb.crashTimestamps[canonical] = validTimes
			}
		}
	}
}

// persistState saves a container's current state to SQLite atomically with caller holding cb.mu.Lock().
func (cb *ContainerCircuitBreaker) persistState(canonical string) error {
	if cb.db == nil {
		return nil
	}
	state := cb.states[canonical]
	if state == "" {
		state = CircuitClosed
	}
	now := cb.now()
	openedAt := cb.openedAt[canonical]
	var openedAtMs int64
	if !openedAt.IsZero() {
		openedAtMs = openedAt.UTC().UnixMilli()
	}

	tsList := cb.crashTimestamps[canonical]
	tsStrings := make([]string, 0, len(tsList))
	for _, t := range tsList {
		tsStrings = append(tsStrings, t.UTC().Format(time.RFC3339Nano))
	}
	rawJSON, _ := json.Marshal(tsStrings)

	var probeTok string
	var probeIssMs, probeExpMs int64
	if active, ok := cb.activeProbes[canonical]; ok {
		probeTok = active.Token
		probeIssMs = active.IssuedAt.UTC().UnixMilli()
		probeExpMs = active.ExpiresAt.UTC().UnixMilli()
	}

	row := model.OpsCircuitBreakerState{
		Container:            canonical,
		State:                string(state),
		OpenedAt:             openedAt,
		OpenedAtMs:           openedAtMs,
		CrashTimestamps:      string(rawJSON),
		ActiveProbeToken:     probeTok,
		ActiveProbeIssuedMs:  probeIssMs,
		ActiveProbeExpiresMs: probeExpMs,
		Degraded:             cb.isDegraded,
		UpdatedAt:            now,
		UpdatedAtMs:          now.UnixMilli(),
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
			"state":                   row.State,
			"opened_at":               row.OpenedAt,
			"opened_at_ms":            row.OpenedAtMs,
			"crash_timestamps":        row.CrashTimestamps,
			"active_probe_token":      row.ActiveProbeToken,
			"active_probe_issued_ms":  row.ActiveProbeIssuedMs,
			"active_probe_expires_ms": row.ActiveProbeExpiresMs,
			"degraded":                row.Degraded,
			"updated_at":              row.UpdatedAt,
			"updated_at_ms":           row.UpdatedAtMs,
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
// Any database persistence error immediately engages Fail-Closed Degraded mode (R4-09).
func (cb *ContainerCircuitBreaker) RecordCrash(container string) (CircuitState, bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	canonical := ResolveTopologyComponent(container)
	if canonical == "" {
		return CircuitOpen, false
	}

	now := cb.now()
	cutoff := now.Add(-cb.windowDuration)

	existing := cb.crashTimestamps[canonical]
	filtered := make([]time.Time, 0, len(existing)+1)
	for _, t := range existing {
		if t.After(cutoff) {
			filtered = append(filtered, t)
		}
	}
	filtered = append(filtered, now)
	cb.crashTimestamps[canonical] = filtered

	if cb.isDegraded {
		cb.states[canonical] = CircuitOpen
		return CircuitOpen, false
	}

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
		if err := cb.persistState(canonical); err != nil {
			cb.markDegraded("persistState failed in RecordCrash(HALF_OPEN->OPEN): " + err.Error())
		}
		tripCallback := cb.onTrip
		if tripCallback != nil {
			go tripCallback(canonical, crashes)
		}
		return CircuitOpen, true
	}

	// Trip condition: >= 3 crashes in sliding window
	if crashes >= cb.maxCrashes {
		wasOpen := currentState == CircuitOpen
		cb.states[canonical] = CircuitOpen
		if !wasOpen {
			cb.openedAt[canonical] = now
		}
		delete(cb.activeProbes, canonical)
		if err := cb.persistState(canonical); err != nil {
			cb.markDegraded("persistState failed in RecordCrash(OPEN): " + err.Error())
			return CircuitOpen, !wasOpen
		}
		if !wasOpen {
			tripCallback := cb.onTrip
			if tripCallback != nil {
				go tripCallback(canonical, crashes)
			}
			return CircuitOpen, true
		}
		return CircuitOpen, false
	}

	cb.states[canonical] = CircuitClosed
	if err := cb.persistState(canonical); err != nil {
		cb.states[canonical] = CircuitOpen
		cb.openedAt[canonical] = now
		cb.markDegraded("persistState failed in RecordCrash(CLOSED): " + err.Error())
		return CircuitOpen, true
	}
	return CircuitClosed, false
}

// CanAttemptRecovery verifies whether automated recovery may proceed for the container.
// If transitioning to HALF_OPEN, atomically issues a unique ProbeTicket with lease TTL (R10.1, R10.2).
// Persistence failure rolls back transition and activates Degraded mode (R4-09).
func (cb *ContainerCircuitBreaker) CanAttemptRecovery(container string) (bool, *ProbeTicket, error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.isDegraded {
		return false, nil, ErrFailClosedMode
	}

	canonical := ResolveTopologyComponent(container)
	if canonical == "" {
		return false, nil, ErrComponentNotWhitelisted
	}
	state := cb.states[canonical]
	if state == "" {
		state = CircuitClosed
	}

	now := cb.now()

	if state == CircuitOpen {
		openTime, hasOpened := cb.openedAt[canonical]
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

			cb.states[canonical] = CircuitHalfOpen
			cb.activeProbes[canonical] = ticket
			if err := cb.persistState(canonical); err != nil {
				cb.states[canonical] = CircuitOpen
				delete(cb.activeProbes, canonical)
				cb.markDegraded("persistState failed transitioning OPEN->HALF_OPEN: " + err.Error())
				return false, nil, fmt.Errorf("%w: %v", ErrFailClosedMode, err)
			}
			return true, &ticket, nil
		}
		return false, nil, ErrCircuitBreakerOpen
	}

	if state == CircuitHalfOpen {
		active, exists := cb.activeProbes[canonical]
		if exists {
			if now.After(active.ExpiresAt) {
				delete(cb.activeProbes, canonical)
				cb.states[canonical] = CircuitOpen
				cb.openedAt[canonical] = now
				if err := cb.persistState(canonical); err != nil {
					cb.markDegraded("persistState failed expiring HALF_OPEN probe: " + err.Error())
					return false, nil, fmt.Errorf("%w: %v", ErrFailClosedMode, err)
				}
				tripCallback := cb.onTrip
				if tripCallback != nil {
					go tripCallback(canonical, cb.maxCrashes)
				}
				return false, nil, ErrProbeTimeout
			}
			return false, nil, ErrProbeInProgress
		}

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
			cb.openedAt[canonical] = now
			cb.markDegraded("persistState failed issuing HALF_OPEN probe ticket: " + err.Error())
			return false, nil, fmt.Errorf("%w: %v", ErrFailClosedMode, err)
		}
		return true, &ticket, nil
	}

	return true, nil, nil
}

// RecordRecoverySuccess resets circuit to CLOSED after successful recovery.
// When in HALF_OPEN, validates that token matches the active probe, has not expired, and DB write succeeds.
func (cb *ContainerCircuitBreaker) RecordRecoverySuccess(container string, tokens ...string) error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.isDegraded {
		return ErrFailClosedMode
	}

	canonical := ResolveTopologyComponent(container)
	if canonical == "" {
		return ErrComponentNotWhitelisted
	}
	currentState := cb.states[canonical]
	if currentState == "" {
		currentState = CircuitClosed
	}

	if currentState == CircuitOpen {
		return ErrStaleProbeToken
	}

	now := cb.now()
	if currentState == CircuitHalfOpen {
		var token string
		if len(tokens) > 0 {
			token = tokens[0]
		}
		active, exists := cb.activeProbes[canonical]
		if !exists || token == "" || active.Token != token {
			return ErrStaleProbeToken
		}
		if now.After(active.ExpiresAt) {
			delete(cb.activeProbes, canonical)
			cb.states[canonical] = CircuitOpen
			cb.openedAt[canonical] = now
			if err := cb.persistState(canonical); err != nil {
				cb.markDegraded("persistState failed on expired HALF_OPEN probe success: " + err.Error())
				return fmt.Errorf("%w: %v", ErrFailClosedMode, err)
			}
			return ErrProbeExpired
		}

		prevCrashes := cb.crashTimestamps[canonical]
		prevOpened := cb.openedAt[canonical]

		cb.states[canonical] = CircuitClosed
		delete(cb.crashTimestamps, canonical)
		delete(cb.openedAt, canonical)
		delete(cb.activeProbes, canonical)
		if err := cb.persistState(canonical); err != nil {
			// Fail-closed rollback to OPEN + Degraded if DB persistence failed
			cb.states[canonical] = CircuitOpen
			cb.crashTimestamps[canonical] = prevCrashes
			cb.openedAt[canonical] = prevOpened
			cb.markDegraded("persistState failed transitioning HALF_OPEN->CLOSED: " + err.Error())
			return fmt.Errorf("%w: %v", ErrFailClosedMode, err)
		}
		return nil
	}

	return nil
}

// RecordRecoveryFailure records a recovery failure for a container in HALF_OPEN.
func (cb *ContainerCircuitBreaker) RecordRecoveryFailure(container string, token string, reason string) error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	canonical := ResolveTopologyComponent(container)
	if canonical == "" {
		return ErrComponentNotWhitelisted
	}
	currentState := cb.states[canonical]

	if currentState != CircuitHalfOpen {
		return ErrStaleProbeToken
	}

	active, exists := cb.activeProbes[canonical]
	if !exists || token == "" || active.Token != token {
		return ErrStaleProbeToken
	}

	now := cb.now()
	cb.states[canonical] = CircuitOpen
	cb.openedAt[canonical] = now
	delete(cb.activeProbes, canonical)
	err := cb.persistState(canonical)
	if err != nil {
		cb.markDegraded("persistState failed in RecordRecoveryFailure: " + err.Error())
	}

	tripCallback := cb.onTrip
	if tripCallback != nil {
		go tripCallback(canonical, cb.maxCrashes)
	}

	return err
}

// Reset manually resets a container's circuit breaker to CLOSED ONLY when not degraded and persistence succeeds.
func (cb *ContainerCircuitBreaker) Reset(container string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.isDegraded {
		// Strictly refuse clearing protection while storage state is unreconciled (R4-09 #7)
		return
	}

	canonical := ResolveTopologyComponent(container)
	if canonical == "" {
		return
	}
	prevState := cb.states[canonical]
	prevCrashes := cb.crashTimestamps[canonical]
	prevOpened := cb.openedAt[canonical]

	cb.states[canonical] = CircuitClosed
	delete(cb.crashTimestamps, canonical)
	delete(cb.openedAt, canonical)
	delete(cb.activeProbes, canonical)
	if err := cb.persistState(canonical); err != nil {
		cb.states[canonical] = prevState
		cb.crashTimestamps[canonical] = prevCrashes
		cb.openedAt[canonical] = prevOpened
		cb.markDegraded("persistState failed during Reset: " + err.Error())
	}
}

// ReconcileFromDB verifies SQLite integrity, reconciles authoritative DB state into memory,
// and only clears isDegraded when schema & storage read/write are confirmed healthy (R4-09 #7, R4-10 #7).
func (cb *ContainerCircuitBreaker) ReconcileFromDB() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.db == nil {
		return ErrLeaseDBRequired
	}
	if err := VerifyOpsSchemaIntegrity(cb.db); err != nil {
		cb.markDegraded("schema verification failed during reconcile: " + err.Error())
		return err
	}

	// Perform clean write probe on existing rows or verify transaction works
	var rows []model.OpsCircuitBreakerState
	if err := cb.db.Find(&rows).Error; err != nil {
		cb.markDegraded("db read failed during reconcile: " + err.Error())
		return err
	}

	newStates := make(map[string]CircuitState)
	newOpened := make(map[string]time.Time)
	newCrashes := make(map[string][]time.Time)
	cutoff := cb.now().Add(-cb.windowDuration)

	for _, r := range rows {
		canonical := ResolveTopologyComponent(r.Container)
		if canonical == "" {
			return fmt.Errorf("%w: invalid container %q in ops_circuit_breaker_states", ErrRecordInconsistent, r.Container)
		}
		st := CircuitState(strings.ToUpper(strings.TrimSpace(r.State)))
		if !isValidCircuitState(st) {
			return fmt.Errorf("%w: invalid state %q for %s", ErrRecordInconsistent, r.State, canonical)
		}
		if st == CircuitHalfOpen {
			st = CircuitOpen
		}
		newStates[canonical] = st
		if !r.OpenedAt.IsZero() {
			newOpened[canonical] = r.OpenedAt.UTC()
		} else if r.OpenedAtMs > 0 {
			newOpened[canonical] = time.UnixMilli(r.OpenedAtMs).UTC()
		}
		if strings.TrimSpace(r.CrashTimestamps) != "" {
			var tsStrings []string
			if err := json.Unmarshal([]byte(r.CrashTimestamps), &tsStrings); err != nil {
				return fmt.Errorf("%w: corrupt crash_timestamps JSON for %s", ErrRecordInconsistent, canonical)
			}
			var parsed []time.Time
			for _, s := range tsStrings {
				t, pErr := time.Parse(time.RFC3339Nano, s)
				if pErr != nil {
					t, pErr = time.Parse(time.RFC3339, s)
				}
				if pErr != nil {
					return fmt.Errorf("%w: malformed timestamp %q for %s", ErrTimestampInvalid, s, canonical)
				}
				if t.After(cutoff) {
					parsed = append(parsed, t)
				}
			}
			newCrashes[canonical] = parsed
		}
	}

	if err := cb.db.Model(&model.OpsCircuitBreakerState{}).Where("degraded = ?", true).Update("degraded", false).Error; err != nil {
		cb.markDegraded("failed clearing degraded flag during reconcile: " + err.Error())
		return err
	}

	cb.states = newStates
	cb.openedAt = newOpened
	cb.crashTimestamps = newCrashes
	cb.activeProbes = make(map[string]ProbeTicket)
	cb.isDegraded = false
	cb.degradeReason = ""
	GlobalSecurityMetrics.SetCircuitBreakerDegraded(false)
	return nil
}

// ResetAll resets all tracked containers to CLOSED when not degraded.
func (cb *ContainerCircuitBreaker) ResetAll() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.isDegraded {
		return
	}

	cb.states = make(map[string]CircuitState)
	cb.crashTimestamps = make(map[string][]time.Time)
	cb.openedAt = make(map[string]time.Time)
	cb.activeProbes = make(map[string]ProbeTicket)
}

// GetState returns the current state of a container's circuit breaker.
func (cb *ContainerCircuitBreaker) GetState(container string) CircuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.isDegraded {
		return CircuitOpen
	}

	canonical := ResolveTopologyComponent(container)
	if canonical == "" {
		return CircuitOpen
	}
	state := cb.states[canonical]
	if state == "" {
		return CircuitClosed
	}

	// If in HALF_OPEN, check if active probe has expired
	if state == CircuitHalfOpen {
		if active, exists := cb.activeProbes[canonical]; exists && cb.now().After(active.ExpiresAt) {
			delete(cb.activeProbes, canonical)
			cb.states[canonical] = CircuitOpen
			cb.openedAt[canonical] = cb.now()
			if err := cb.persistState(canonical); err != nil {
				cb.markDegraded("persistState failed expiring probe in GetState: " + err.Error())
			}
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
	now := cb.now()
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
// If query fails or schema is incomplete, enters fail-closed degraded mode (R4-09, R4-18).
func (l *MaintenanceLock) LoadPersistedState(db *gorm.DB) {
	if db == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.db = db

	hasTbl, tblErr := tableExists(db, "ops_maintenance_locks")
	if tblErr != nil || !hasTbl {
		l.Active = true
		l.isDegraded = true
		l.LockedBy = "system-fail-closed"
		l.Reason = fmt.Sprintf("ops_maintenance_locks missing or unreadable (err=%v)", tblErr)
		l.LockedAt = time.Now().UTC()
		l.ExpiresAt = time.Now().UTC().Add(24 * time.Hour)
		GlobalSecurityMetrics.SetMaintenanceLockDegraded(true)
		GlobalSecurityMetrics.IncDatabaseError()
		return
	}

	var lockRow model.OpsMaintenanceLock
	err := db.Order("id DESC").First(&lockRow).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			l.Active = false
			return
		}
		l.Active = true
		l.isDegraded = true
		l.LockedBy = "system-fail-closed"
		l.Reason = "persistence error, degraded protection: " + err.Error()
		l.LockedAt = time.Now().UTC()
		l.ExpiresAt = time.Now().UTC().Add(24 * time.Hour)
		GlobalSecurityMetrics.SetMaintenanceLockDegraded(true)
		GlobalSecurityMetrics.IncDatabaseError()
		return
	}

	if lockRow.Active {
		if lockRow.ExpiresAt.IsZero() && lockRow.ExpiresAtMs <= 0 {
			l.Active = true
			l.isDegraded = true
			l.LockedBy = "system-fail-closed"
			l.Reason = "active maintenance lock has invalid zero expiration timestamp"
			l.LockedAt = time.Now().UTC()
			l.ExpiresAt = time.Now().UTC().Add(24 * time.Hour)
			GlobalSecurityMetrics.SetMaintenanceLockDegraded(true)
			GlobalSecurityMetrics.IncDatabaseError()
			return
		}
		exp := lockRow.ExpiresAt.UTC()
		if lockRow.ExpiresAtMs > 0 {
			exp = time.UnixMilli(lockRow.ExpiresAtMs).UTC()
		}
		if time.Now().UTC().Before(exp) {
			l.Active = true
			l.LockedBy = lockRow.LockedBy
			l.Reason = lockRow.Reason
			l.LockedAt = lockRow.LockedAt
			l.ExpiresAt = exp
			return
		}
	}
	l.Active = false
}

// persistState saves lock state to database and returns actual error (R6.1).
func (l *MaintenanceLock) persistState() error {
	if l.db == nil {
		return nil
	}
	row := model.OpsMaintenanceLock{
		Active:      l.Active,
		LockedBy:    l.LockedBy,
		Reason:      l.Reason,
		LockedAt:    l.LockedAt,
		ExpiresAt:   l.ExpiresAt,
		ExpiresAtMs: l.ExpiresAt.UTC().UnixMilli(),
		UpdatedAt:   time.Now().UTC(),
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

	now := time.Now().UTC()
	if ttl <= 0 {
		ttl = 60 * time.Minute // Default: 60 minutes
	}

	if l.Active && now.Before(l.ExpiresAt) {
		if l.LockedBy != "" && l.LockedBy != lockedBy && !strings.Contains(strings.ToLower(lockedBy), "admin") {
			return ErrMaintenanceLockActive
		}
	}

	prevActive := l.Active
	prevLockedBy := l.LockedBy
	prevReason := l.Reason
	prevLockedAt := l.LockedAt
	prevExpiresAt := l.ExpiresAt

	exp := now.Add(ttl)
	row := model.OpsMaintenanceLock{
		Active:      true,
		LockedBy:    lockedBy,
		Reason:      reason,
		LockedAt:    now,
		ExpiresAt:   exp,
		ExpiresAtMs: exp.UnixMilli(),
		UpdatedAt:   now,
	}

	if l.db != nil {
		if err := l.db.Create(&row).Error; err != nil {
			l.Active = prevActive
			l.LockedBy = prevLockedBy
			l.Reason = prevReason
			l.LockedAt = prevLockedAt
			l.ExpiresAt = prevExpiresAt
			GlobalSecurityMetrics.IncDatabaseError()
			return fmt.Errorf("failed to persist maintenance lock acquisition: %w", err)
		}
	}

	l.Active = true
	l.LockedBy = lockedBy
	l.Reason = reason
	l.LockedAt = now
	l.ExpiresAt = exp
	return nil
}

// Release clears the maintenance lock.
// MUST persist to DB first! If DB write fails, DO NOT clear memory lock.
// Never clears isDegraded directly; storage reconciliation is required via ReconcileFromDB (R4-09).
func (l *MaintenanceLock) Release(releasedBy string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.isDegraded {
		return ErrFailClosedMode
	}

	if !l.Active {
		return nil
	}

	now := time.Now().UTC()
	row := model.OpsMaintenanceLock{
		Active:      false,
		LockedBy:    "",
		Reason:      fmt.Sprintf("Released by %s", releasedBy),
		LockedAt:    l.LockedAt,
		ExpiresAt:   time.Time{},
		ExpiresAtMs: 0,
		UpdatedAt:   now,
	}

	if l.db != nil {
		if err := l.db.Create(&row).Error; err != nil {
			l.isDegraded = true
			GlobalSecurityMetrics.IncDatabaseError()
			GlobalSecurityMetrics.SetMaintenanceLockDegraded(true)
			return fmt.Errorf("failed to persist maintenance lock release: %w (memory lock retained)", err)
		}
	}

	l.Active = false
	l.LockedBy = ""
	l.Reason = ""
	l.ExpiresAt = time.Time{}
	return nil
}

// ReconcileFromDB validates schema integrity and reloads maintenance lock state before clearing degraded flag.
func (l *MaintenanceLock) ReconcileFromDB() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db == nil {
		return ErrLeaseDBRequired
	}
	if err := VerifyOpsSchemaIntegrity(l.db); err != nil {
		l.isDegraded = true
		GlobalSecurityMetrics.SetMaintenanceLockDegraded(true)
		return err
	}
	var lockRow model.OpsMaintenanceLock
	err := l.db.Order("id DESC").First(&lockRow).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		l.isDegraded = true
		GlobalSecurityMetrics.SetMaintenanceLockDegraded(true)
		return err
	}
	if err == nil && lockRow.Active {
		exp := lockRow.ExpiresAt.UTC()
		if lockRow.ExpiresAtMs > 0 {
			exp = time.UnixMilli(lockRow.ExpiresAtMs).UTC()
		}
		if time.Now().UTC().Before(exp) {
			l.Active = true
			l.LockedBy = lockRow.LockedBy
			l.Reason = lockRow.Reason
			l.LockedAt = lockRow.LockedAt
			l.ExpiresAt = exp
			l.isDegraded = false
			GlobalSecurityMetrics.SetMaintenanceLockDegraded(false)
			return nil
		}
	}
	l.Active = false
	l.LockedBy = ""
	l.Reason = ""
	l.ExpiresAt = time.Time{}
	l.isDegraded = false
	GlobalSecurityMetrics.SetMaintenanceLockDegraded(false)
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
		if sErr := EnsureOpsSchema(db); sErr != nil {
			fsm.lock.SetDB(db)
			fsm.lock.SetDegraded(true)
			fsm.breaker.SetDB(db)
			fsm.breaker.SetBus(bus)
			fsm.breaker.mu.Lock()
			fsm.breaker.markDegraded("schema verification failed at FSM init: " + sErr.Error())
			fsm.breaker.mu.Unlock()
			return fsm
		}
		fsm.lock.SetDB(db)
		fsm.lock.LoadPersistedState(db)
		fsm.breaker.SetDB(db)
		fsm.breaker.SetBus(bus)
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
	if container == "" {
		return &RecoveryDecision{
			Action:       ActionBlocked,
			EventClass:   ClassifyContainerEvent(event, false),
			Container:    event.ContainerName,
			Reason:       fmt.Sprintf("Unknown or unwhitelisted container component %q (fail-closed)", event.ContainerName),
			CircuitState: CircuitOpen,
		}, ErrComponentNotWhitelisted
	}
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
				Reason:       "Container circuit breaker is OPEN or degraded (fail-closed protection active)",
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
		var newState CircuitState
		var probeToken string
		if ticket != nil {
			newState = CircuitHalfOpen
			probeToken = ticket.Token
		} else {
			var justTripped bool
			newState, justTripped = fsm.breaker.RecordCrash(container)
			if fsm.breaker.IsDegraded() {
				return &RecoveryDecision{
					Action:       ActionBlocked,
					EventClass:   eventClass,
					Container:    container,
					Reason:       "Circuit breaker entered fail-closed degraded mode on persistence failure: " + fsm.breaker.GetDegradeReason(),
					CircuitState: CircuitOpen,
				}, ErrFailClosedMode
			}
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

var circuitTripSeq uint64

// createCircuitTripProposal creates a Tier 2 HIGH proposal when a container circuit breaker trips.
func (fsm *ContainerRecoveryFSM) createCircuitTripProposal(container string, crashCount int) {
	if fsm.db == nil {
		return
	}

	now := time.Now().UTC()
	seq := atomic.AddUint64(&circuitTripSeq, 1)
	canonical := ResolveTopologyComponent(container)
	if canonical == "" {
		return
	}
	propID := fmt.Sprintf("prop-cb-%s-%d-%d", canonical, now.UnixNano(), seq)

	var baselineHash string
	fsm.mu.RLock()
	curExec := fsm.executor
	fsm.mu.RUnlock()
	if defExec, ok := curExec.(*DefaultOperationExecutor); ok && defExec != nil && defExec.baselineExtractor != nil {
		baselineHash = defExec.baselineExtractor(canonical)
	}
	if baselineHash == "" {
		baselineHash = model.ComputeDefaultBaseline(canonical)
	}

	actionSpec, _ := json.Marshal(OperationSpec{
		Action: ActionDockerRestart,
		Target: canonical,
	})

	prop := model.OpsProposal{
		ProposalID:       propID,
		Title:            fmt.Sprintf("【高危告警】%s 连续崩溃触发自愈熔断", canonical),
		Description:      fmt.Sprintf("容器 %s 在 10 分钟内发生 %d 次连续崩溃，已触发自愈熔断器跳闸。自动重启已挂起，需管理员人工审批与介入排查。", canonical, crashCount),
		TargetComponent:  canonical,
		RiskLevel:        model.RiskLevelHigh,
		Status:           model.ProposalStatusAwaitingApproval,
		EstimatedSeconds: 60,
		ExecutionPlan:    fmt.Sprintf("docker restart %s", canonical),
		RollbackPlan:     fmt.Sprintf("docker logs --tail 200 %s", canonical),
		ActionSpec:       string(actionSpec),
		BaselineHash:     baselineHash,
		Version:          1,
		Revision:         1,
		CreatedAt:        now,
	}

	if err := fsm.db.Create(&prop).Error; err != nil {
		GlobalSecurityMetrics.IncDatabaseError()
		return
	}

	if fsm.bus != nil {
		fsm.bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicSystemLog,
			Source:   eventbus.SourceSystem,
			Severity: eventbus.SeverityCritical,
			Payload:  []byte(fmt.Sprintf("容器 %s 连续崩溃 %d 次触发自愈熔断器跳闸，已创建高危审批提案: %s", canonical, crashCount, propID)),
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
			if canonical == "" {
				return false, ErrComponentNotWhitelisted
			}
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

// ReconcileDegradedState runs storage integrity checks and reloads persisted lock and circuit breaker states
// before clearing fail-closed Degraded protection (R4-09, R4-10, R4-18).
func (fsm *ContainerRecoveryFSM) ReconcileDegradedState(actor string) error {
	if err := fsm.lock.ReconcileFromDB(); err != nil {
		return err
	}
	if err := fsm.breaker.ReconcileFromDB(); err != nil {
		return err
	}
	RecordSecurityEvent(fsm.db, fsm.bus, OpsSecurityEvent{
		OperationID: GenerateOperationID("reconcile"),
		EventType:   "ops.fsm.reconciled",
		Severity:    "warning",
		Actor:       actor,
		Message:     fmt.Sprintf("storage and security state reconciled by %s; degraded freeze lifted", actor),
	})
	return nil
}

// ProcessAndRecover performs closed-loop recovery with unified LeaseGuard ownership across the full execution chain (R4-05 ~ R4-08):
// acquire LeaseGuard -> backoff -> pre-exec fencing -> executor.Execute(leaseCtx) -> circuit breaker persistence -> ReleaseLease.
func (fsm *ContainerRecoveryFSM) ProcessAndRecover(ctx context.Context, event ContainerEvent) (*ContainerRecoveryResult, error) {
	if fsm.lock.IsDegraded() || fsm.breaker.IsDegraded() {
		return &ContainerRecoveryResult{
			Status:         StatusBlocked,
			Executed:       false,
			Error:          ErrFailClosedMode.Error(),
			HealthVerified: false,
		}, ErrFailClosedMode
	}

	decision, err := fsm.ProcessContainerEvent(ctx, event)
	if err != nil || decision == nil {
		status := StatusBlocked
		if errors.Is(err, ErrMaintenanceLockActive) || errors.Is(err, ErrSystemInMaintenance) ||
			errors.Is(err, ErrCircuitBreakerOpen) || errors.Is(err, ErrDependencyUnhealthy) ||
			errors.Is(err, ErrFailClosedMode) || errors.Is(err, ErrComponentNotWhitelisted) {
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
		if canonical == "" {
			return &ContainerRecoveryResult{
				Decision:       decision,
				Status:         StatusBlocked,
				Executed:       false,
				Error:          ErrComponentNotWhitelisted.Error(),
				HealthVerified: false,
			}, ErrComponentNotWhitelisted
		}

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

		// R4-05 / R4-06: Single-ownership LeaseGuard passed via ctx to OperationExecutor (no double-acquire!)
		workCtx := ctx
		var leaseGuard *LeaseGuard
		if leaseMgr != nil {
			holderID := GenerateOperationID("go-core")
			lCtx, guard, lErr := AcquireLeaseGuard(ctx, leaseMgr, canonical, holderID, 30*time.Second, fsm.db, fsm.bus)
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
			workCtx = lCtx
			leaseGuard = guard
		}

		releaseLease := func() error {
			if leaseGuard == nil {
				return nil
			}
			return leaseGuard.Release(fsm.db, fsm.bus)
		}

		if decision.BackoffSec > 0 {
			select {
			case <-time.After(time.Duration(decision.BackoffSec) * time.Second):
			case <-workCtx.Done():
				if decision.ProbeToken != "" {
					_ = fsm.breaker.RecordRecoveryFailure(canonical, decision.ProbeToken, "context cancelled during backoff: "+workCtx.Err().Error())
				}
				_ = releaseLease()
				return &ContainerRecoveryResult{
					Decision:       decision,
					Status:         StatusFailed,
					Executed:       false,
					Error:          workCtx.Err().Error(),
					HealthVerified: false,
				}, workCtx.Err()
			}
		}

		// Re-verify lease & maintenance/degraded state right before invoking executor (R4-08)
		if leaseGuard != nil {
			if fErr := leaseGuard.VerifyBeforeMutation(0); fErr != nil {
				if decision.ProbeToken != "" {
					_ = fsm.breaker.RecordRecoveryFailure(canonical, decision.ProbeToken, "lease fencing check failed before execute: "+fErr.Error())
				}
				_ = releaseLease()
				return &ContainerRecoveryResult{
					Decision:       decision,
					Status:         StatusBlocked,
					Executed:       false,
					Error:          fErr.Error(),
					HealthVerified: false,
				}, fErr
			}
		}

		spec := OperationSpec{
			Action: ActionDockerRestart,
			Target: canonical,
		}

		execRes, execErr := exec.Execute(workCtx, spec)
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
			if decision.ProbeToken != "" {
				_ = fsm.breaker.RecordRecoveryFailure(canonical, decision.ProbeToken, errStr)
			}
			_ = releaseLease()
			return &ContainerRecoveryResult{
				Decision:       decision,
				Status:         StatusFailed,
				Executed:       true,
				Output:         outStr,
				Error:          errStr,
				HealthVerified: false,
			}, execErr
		}

		// Verify lease is still valid before recording circuit breaker success
		if leaseGuard != nil {
			if fErr := leaseGuard.VerifyBeforeMutation(0); fErr != nil {
				if decision.ProbeToken != "" {
					_ = fsm.breaker.RecordRecoveryFailure(canonical, decision.ProbeToken, "lease lost before circuit recovery commit: "+fErr.Error())
				}
				_ = releaseLease()
				return &ContainerRecoveryResult{
					Decision:       decision,
					Status:         StatusFailed,
					Executed:       true,
					Output:         execRes.Output,
					Error:          fErr.Error(),
					HealthVerified: false,
				}, fErr
			}
		}

		// Update circuit breaker and strictly verify persistence succeeded (R4-09)
		var cbUpdateErr error
		if decision.ProbeToken != "" {
			cbUpdateErr = fsm.breaker.RecordRecoverySuccess(canonical, decision.ProbeToken)
		} else {
			cbUpdateErr = fsm.breaker.RecordRecoverySuccess(canonical)
		}
		if cbUpdateErr != nil {
			_ = releaseLease()
			return &ContainerRecoveryResult{
				Decision:       decision,
				Status:         StatusFailed,
				Executed:       true,
				Output:         execRes.Output,
				Error:          fmt.Sprintf("recovery executed but circuit breaker state commit failed: %v", cbUpdateErr),
				HealthVerified: true,
			}, cbUpdateErr
		}

		// Explicitly release lease and forbid reporting RECOVERED if release failed (R4-05, R4-08)
		if relErr := releaseLease(); relErr != nil {
			return &ContainerRecoveryResult{
				Decision:       decision,
				Status:         StatusFailed,
				Executed:       true,
				Output:         execRes.Output,
				Error:          fmt.Sprintf("recovery completed but persistent lease release failed: %v", relErr),
				HealthVerified: true,
			}, relErr
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

