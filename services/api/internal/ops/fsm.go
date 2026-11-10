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
)

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
		},
		levels: map[string]int{
			"mosquitto":          TopologyLevel0,
			"home-mosquitto":     TopologyLevel0,
			"frigate":            TopologyLevel0,
			"home-frigate":       TopologyLevel0,
			"app.db":             TopologyLevel0,
			"database":           TopologyLevel0,
			"home-api":           TopologyLevel1,
			"api":                TopologyLevel1,
			"home-web":           TopologyLevel2,
			"web":                TopologyLevel2,
			"home-vision":        TopologyLevel2,
			"vision":             TopologyLevel2,
			"home-cloudflared":   TopologyLevel2,
			"home-h3c-keepalive": TopologyLevel2,
			"home-ops-agent":     TopologyLevel2,
		},
		healthChecker: func(ctx context.Context, component string) (bool, error) {
			// Default optimistic check in absence of live Docker daemon probe
			return true, nil
		},
	}
	return engine
}

// SetHealthChecker configures the health validation function.
func (t *TopologyEngine) SetHealthChecker(checker ComponentHealthChecker) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.healthChecker = checker
}

// ResolveTopologyComponent normalizes container names and aliases to canonical topology identifiers.
func ResolveTopologyComponent(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch lower {
	case "home-mosquitto", "mqtt", "mosquitto":
		return "mosquitto"
	case "home-frigate", "nvr", "frigate":
		return "frigate"
	case "app.db", "database", "sqlite", "db":
		return "app.db"
	case "home-api", "api", "backend", "go-api":
		return "home-api"
	case "home-web", "web", "frontend", "nginx":
		return "home-web"
	case "home-vision", "vision", "ai-vision":
		return "home-vision"
	case "home-cloudflared", "cloudflared":
		return "home-cloudflared"
	case "home-h3c-keepalive", "h3c":
		return "home-h3c-keepalive"
	case "home-ops-agent", "ops-agent":
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
		return nil
	}

	// 1. Check all direct dependencies
	for _, dep := range deps {
		healthy, err := checker(ctx, dep)
		if err != nil || !healthy {
			return fmt.Errorf("cannot recover %s: upstream dependency %s is unhealthy", target, dep)
		}

		// 2. Transitive check: if dep has Level 0 dependencies, ensure they are also healthy
		if subDeps, ok := t.graph[dep]; ok {
			for _, subDep := range subDeps {
				subHealthy, subErr := checker(ctx, subDep)
				if subErr != nil || !subHealthy {
					return fmt.Errorf("cannot recover %s: upstream dependency %s (transitive dependency of %s) is unhealthy", target, subDep, dep)
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
	crashTimestamps  map[string][]time.Time
	states           map[string]CircuitState
	openedAt         map[string]time.Time
	halfOpenProbing  map[string]bool
	onTrip           func(container string, crashCount int)
	db               *gorm.DB
}

// NewContainerCircuitBreaker creates a default circuit breaker (3 crashes in 10 minutes).
func NewContainerCircuitBreaker() *ContainerCircuitBreaker {
	return &ContainerCircuitBreaker{
		windowDuration:   10 * time.Minute,
		maxCrashes:       3,
		cooldownDuration: 15 * time.Minute,
		crashTimestamps:  make(map[string][]time.Time),
		states:           make(map[string]CircuitState),
		openedAt:         make(map[string]time.Time),
		halfOpenProbing:  make(map[string]bool),
	}
}

// SetDB binds the database instance for persistent state recording.
func (cb *ContainerCircuitBreaker) SetDB(db *gorm.DB) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.db = db
}

// LoadPersistedState restores circuit breaker states and crash windows from SQLite.
func (cb *ContainerCircuitBreaker) LoadPersistedState(db *gorm.DB) {
	if db == nil {
		return
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.db = db

	var rows []model.OpsCircuitBreakerState
	if err := db.Find(&rows).Error; err != nil {
		return
	}

	cutoff := time.Now().Add(-cb.windowDuration)
	for _, r := range rows {
		canonical := ResolveTopologyComponent(r.Container)
		state := CircuitState(r.State)
		if state == "" {
			state = CircuitClosed
		}
		cb.states[canonical] = state
		if !r.OpenedAt.IsZero() {
			cb.openedAt[canonical] = r.OpenedAt
		}

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

// persistState saves a container's current state to the database. Must be called with lock held or copied.
func (cb *ContainerCircuitBreaker) persistState(canonical string) {
	if cb.db == nil {
		return
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

	var existing model.OpsCircuitBreakerState
	if err := cb.db.Where("container = ?", row.Container).First(&existing).Error; err != nil {
		_ = cb.db.Create(&row).Error
	} else {
		_ = cb.db.Model(&existing).Updates(map[string]interface{}{
			"state":            row.State,
			"opened_at":        row.OpenedAt,
			"crash_timestamps": row.CrashTimestamps,
			"updated_at":       row.UpdatedAt,
		}).Error
	}
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
		cb.halfOpenProbing[canonical] = false
		cb.persistState(canonical)
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
			cb.persistState(canonical)
			tripCallback := cb.onTrip
			if tripCallback != nil {
				go tripCallback(canonical, crashes)
			}
			return CircuitOpen, true
		}
		cb.persistState(canonical)
		return CircuitOpen, false
	}

	cb.states[canonical] = CircuitClosed
	cb.persistState(canonical)
	return CircuitClosed, false
}

// CanAttemptRecovery verifies whether automated recovery may proceed for the container.
func (cb *ContainerCircuitBreaker) CanAttemptRecovery(container string) (bool, error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	canonical := ResolveTopologyComponent(container)
	state := cb.states[canonical]
	if state == "" {
		state = CircuitClosed
	}

	if state == CircuitOpen {
		openTime, hasOpened := cb.openedAt[canonical]
		// Check whether quiet cooldown period has passed to transition to HALF_OPEN
		if hasOpened && time.Since(openTime) >= cb.cooldownDuration {
			cb.states[canonical] = CircuitHalfOpen
			cb.halfOpenProbing[canonical] = true
			cb.persistState(canonical)
			return true, nil
		}
		return false, ErrCircuitBreakerOpen
	}

	if state == CircuitHalfOpen {
		// Only allow a single in-flight probe while in HALF_OPEN
		if cb.halfOpenProbing[canonical] {
			return false, ErrCircuitBreakerOpen
		}
		cb.halfOpenProbing[canonical] = true
		return true, nil
	}

	// CLOSED: allow attempt
	return true, nil
}

// RecordRecoverySuccess resets circuit to CLOSED after successful recovery.
func (cb *ContainerCircuitBreaker) RecordRecoverySuccess(container string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	canonical := ResolveTopologyComponent(container)
	cb.states[canonical] = CircuitClosed
	delete(cb.crashTimestamps, canonical)
	delete(cb.openedAt, canonical)
	delete(cb.halfOpenProbing, canonical)
	cb.persistState(canonical)
}

// Reset manually resets a container's circuit breaker to CLOSED.
func (cb *ContainerCircuitBreaker) Reset(container string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	canonical := ResolveTopologyComponent(container)
	cb.states[canonical] = CircuitClosed
	delete(cb.crashTimestamps, canonical)
	delete(cb.openedAt, canonical)
	delete(cb.halfOpenProbing, canonical)
	cb.persistState(canonical)
}

// ResetAll resets all tracked containers to CLOSED.
func (cb *ContainerCircuitBreaker) ResetAll() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.states = make(map[string]CircuitState)
	cb.crashTimestamps = make(map[string][]time.Time)
	cb.openedAt = make(map[string]time.Time)
	cb.halfOpenProbing = make(map[string]bool)
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
	if state == CircuitOpen {
		openTime, hasOpened := cb.openedAt[canonical]
		if hasOpened && time.Since(openTime) >= cb.cooldownDuration {
			cb.states[canonical] = CircuitHalfOpen
			return CircuitHalfOpen
		}
	}
	return state
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
	mu        sync.RWMutex
	Active    bool      `json:"active"`
	LockedBy  string    `json:"locked_by"`
	Reason    string    `json:"reason"`
	LockedAt  time.Time `json:"locked_at"`
	ExpiresAt time.Time `json:"expires_at"`
	db        *gorm.DB
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
func (l *MaintenanceLock) LoadPersistedState(db *gorm.DB) {
	if db == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.db = db

	var lockRow model.OpsMaintenanceLock
	if err := db.Order("id DESC").First(&lockRow).Error; err == nil {
		if lockRow.Active && time.Now().Before(lockRow.ExpiresAt) {
			l.Active = true
			l.LockedBy = lockRow.LockedBy
			l.Reason = lockRow.Reason
			l.LockedAt = lockRow.LockedAt
			l.ExpiresAt = lockRow.ExpiresAt
		}
	}
}

func (l *MaintenanceLock) persistState() {
	if l.db == nil {
		return
	}
	row := model.OpsMaintenanceLock{
		Active:    l.Active,
		LockedBy:  l.LockedBy,
		Reason:    l.Reason,
		LockedAt:  l.LockedAt,
		ExpiresAt: l.ExpiresAt,
		UpdatedAt: time.Now(),
	}
	_ = l.db.Create(&row).Error
}

// Acquire requests exclusive maintenance lock on the system.
// If already locked by another operator and not expired, returns ErrMaintenanceLockActive.
func (l *MaintenanceLock) Acquire(lockedBy string, reason string, ttl time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()

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

	l.Active = true
	l.LockedBy = lockedBy
	l.Reason = reason
	l.LockedAt = now
	l.ExpiresAt = now.Add(ttl)
	l.persistState()
	return nil
}

// Release clears the maintenance lock.
func (l *MaintenanceLock) Release(releasedBy string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.Active = false
	l.LockedBy = ""
	l.Reason = ""
	l.ExpiresAt = time.Time{}
	l.persistState()
	return nil
}

// IsLocked checks whether the maintenance lock is currently active and not expired.
func (l *MaintenanceLock) IsLocked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.Active {
		return false
	}
	if time.Now().After(l.ExpiresAt) {
		l.Active = false
		return false
	}
	return true
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
}

// ContainerRecoveryFSM encapsulates event classification, topology validation,
// circuit breaker protection, and maintenance lock awareness into a unified recovery engine.
type ContainerRecoveryFSM struct {
	mu       sync.RWMutex
	lock     *MaintenanceLock
	topology *TopologyEngine
	breaker  *ContainerCircuitBreaker
	db       *gorm.DB
	bus      *eventbus.Bus
	executor OperationExecutor
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

	// Wire live health probe to topology engine if executor is available
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

	// Trip callback: create Tier 2 HIGH proposal for administrator approval
	fsm.breaker.SetOnTrip(func(container string, crashCount int) {
		fsm.createCircuitTripProposal(container, crashCount)
	})

	if db != nil {
		fsm.lock.SetDB(db)
		fsm.lock.LoadPersistedState(db)
		fsm.breaker.SetDB(db)
		fsm.breaker.LoadPersistedState(db)
	}

	return fsm
}

// GetMaintenanceLock returns the underlying MaintenanceLock.
func (fsm *ContainerRecoveryFSM) GetMaintenanceLock() *MaintenanceLock {
	return fsm.lock
}

// GetCircuitBreaker returns the underlying ContainerCircuitBreaker.
func (fsm *ContainerRecoveryFSM) GetCircuitBreaker() *ContainerCircuitBreaker {
	return fsm.breaker
}

// GetTopologyEngine returns the underlying TopologyEngine.
func (fsm *ContainerRecoveryFSM) GetTopologyEngine() *TopologyEngine {
	return fsm.topology
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
	if fsm.lock.IsLocked() {
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
		// 3. Check Circuit Breaker eligibility before restart
		canRecover, cbErr := fsm.breaker.CanAttemptRecovery(container)
		if !canRecover {
			return &RecoveryDecision{
				Action:       ActionBlocked,
				EventClass:   eventClass,
				Container:    container,
				Reason:       "Container circuit breaker is OPEN (crash loop protection active)",
				CircuitState: CircuitOpen,
			}, cbErr
		}

		// 4. Pre-flight Dependency Topology Check
		if topErr := fsm.topology.PreflightDependencyCheck(ctx, container); topErr != nil {
			return &RecoveryDecision{
				Action:       ActionBlocked,
				EventClass:   eventClass,
				Container:    container,
				Reason:       topErr.Error(),
				CircuitState: circuitState,
			}, topErr
		}

		// 5. Record crash in sliding window and check if threshold reached
		newState, justTripped := fsm.breaker.RecordCrash(container)
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

		// Exponential backoff calculation (1s for crash #1, 2s for crash #2)
		crashes := fsm.breaker.GetCrashCount(container)
		backoff := 1
		if crashes > 1 {
			backoff = 2
		}

		return &RecoveryDecision{
			Action:       ActionRestart,
			EventClass:   eventClass,
			Container:    container,
			Reason:       fmt.Sprintf("Eligible for autonomous recovery (crash #%d in 10m window)", crashes),
			BackoffSec:   backoff,
			CircuitState: newState,
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
	propID := fmt.Sprintf("prop-cb-%s-%d", container, now.Unix())
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
		fsm.mu.RUnlock()

		if exec == nil {
			return &ContainerRecoveryResult{
				Decision:       decision,
				Status:         StatusFailed,
				Executed:       false,
				Error:          "operation executor is not configured on Go Active Core",
				HealthVerified: false,
			}, errors.New("operation executor not available")
		}

		if decision.BackoffSec > 0 {
			select {
			case <-time.After(time.Duration(decision.BackoffSec) * time.Second):
			case <-ctx.Done():
				return &ContainerRecoveryResult{
					Decision:       decision,
					Status:         StatusFailed,
					Executed:       false,
					Error:          ctx.Err().Error(),
					HealthVerified: false,
				}, ctx.Err()
			}
		}

		canonical := ResolveTopologyComponent(decision.Container)
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
		fsm.breaker.RecordRecoverySuccess(canonical)

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

