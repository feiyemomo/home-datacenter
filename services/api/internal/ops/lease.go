package ops

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
)

var (
	// ErrLeaseHeld is returned when a recovery lease is actively held by another executor (or non-expired lease).
	ErrLeaseHeld = errors.New("recovery lease is actively held")
	// ErrInvalidLeaseToken is returned when attempting to renew or release a lease with an invalid or expired token.
	ErrInvalidLeaseToken = errors.New("invalid or expired recovery lease token")
	// ErrFencingTokenStale is returned when a holder presents an outdated fencing generation or insufficient remaining TTL.
	ErrFencingTokenStale = errors.New("recovery fencing generation is stale or lease expired")
	// ErrLeaseDBRequired is returned when a database handle is required for persistent leasing.
	ErrLeaseDBRequired = errors.New("database connection required for persistent lease")
)

const (
	// DefaultLeaseSafetyGuardMs is the guard band required before external Docker mutations begin.
	DefaultLeaseSafetyGuardMs int64 = 5000
)

// RecoveryLease models the persistent mutual exclusion record in ops_recovery_leases (R4-04 ~ R4-07).
type RecoveryLease struct {
	Component         string    `gorm:"primaryKey;column:component;size:64" json:"component"`
	Holder            string    `gorm:"column:holder;size:64;not null" json:"holder"`
	LeaseToken        string    `gorm:"column:lease_token;size:96;not null" json:"lease_token"`
	FencingGeneration int64     `gorm:"column:fencing_generation;not null;default:1" json:"fencing_generation"`
	AcquiredAt        time.Time `gorm:"column:acquired_at" json:"acquired_at"`
	ExpiresAt         time.Time `gorm:"column:expires_at" json:"expires_at"`
	AcquiredAtMs      int64     `gorm:"column:acquired_at_ms;not null;default:0" json:"acquired_at_ms"`
	ExpiresAtMs       int64     `gorm:"column:expires_at_ms;not null;default:0" json:"expires_at_ms"`
	State             string    `gorm:"column:state;size:32;not null;default:'ACTIVE'" json:"state"`
}

// TableName returns the table name in SQLite.
func (RecoveryLease) TableName() string {
	return "ops_recovery_leases"
}

// LeaseGrant contains all cryptographic and fencing credentials for an acquired lease.
type LeaseGrant struct {
	Component         string `json:"component"`
	Holder            string `json:"holder"`
	LeaseToken        string `json:"lease_token"`
	FencingGeneration int64  `json:"fencing_generation"`
	AcquiredAtMs      int64  `json:"acquired_at_ms"`
	ExpiresAtMs       int64  `json:"expires_at_ms"`
	OperationID       string `json:"operation_id"`
}

// RecoveryLeaseManager coordinates cross-language persistent recovery leases between Go Core and Python Ops Agent.
type RecoveryLeaseManager struct {
	db    *gorm.DB
	mu    sync.RWMutex
	nowFn func() time.Time
}

// NewRecoveryLeaseManager constructs a RecoveryLeaseManager and ensures lease schema exists on fresh DBs.
func NewRecoveryLeaseManager(db *gorm.DB) *RecoveryLeaseManager {
	mgr := &RecoveryLeaseManager{
		db:    db,
		nowFn: func() time.Time { return time.Now().UTC() },
	}
	if db != nil {
		_ = mgr.AutoMigrate()
	}
	return mgr
}

// SetNowFunc injects a deterministic clock for unit and concurrency tests.
func (m *RecoveryLeaseManager) SetNowFunc(fn func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fn == nil {
		m.nowFn = func() time.Time { return time.Now().UTC() }
	} else {
		m.nowFn = fn
	}
}

func (m *RecoveryLeaseManager) nowUTC() time.Time {
	m.mu.RLock()
	fn := m.nowFn
	m.mu.RUnlock()
	if fn == nil {
		return time.Now().UTC()
	}
	return fn().UTC()
}

// AutoMigrate initializes or verifies the unified ops lease schema (never silently recreates dropped tables on bootstrapped DBs).
func (m *RecoveryLeaseManager) AutoMigrate() error {
	if m == nil || m.db == nil {
		return ErrLeaseDBRequired
	}
	return EnsureOpsSchema(m.db)
}

func generateLeaseToken(holder string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("lease-%s-%s", holder, hex.EncodeToString(b))
}

func resolveLeaseCanonical(component string) (string, error) {
	canonical := ResolveTopologyComponent(component)
	if canonical == "" {
		// Fall back to CanonicalizeComponent to allow Component1PWAF if passed from executor
		canonical = strings.TrimSpace(component)
		if c2, err := ResolveComponent(canonical); err == nil {
			canonical = c2
		} else {
			return "", fmt.Errorf("%w: unknown component %q", ErrComponentNotWhitelisted, component)
		}
	}
	return canonical, nil
}

// runImmediate executes an atomic SQLite BEGIN IMMEDIATE callback with a pinned *sql.Conn.
func (m *RecoveryLeaseManager) runImmediate(op func(ctx context.Context, conn *sql.Conn) error) error {
	if m == nil || m.db == nil {
		return ErrLeaseDBRequired
	}
	sqlDB, err := m.db.DB()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		GlobalSecurityMetrics.IncDatabaseError()
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if err := op(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		GlobalSecurityMetrics.IncDatabaseError()
		return err
	}
	committed = true
	return nil
}

// AcquireLeaseGrant atomically acquires an exclusive persistent recovery lease and monotonically increments
// fencing_generation. Same-holder unconditional re-entry while active is strictly FORBIDDEN (R4-04).
func (m *RecoveryLeaseManager) AcquireLeaseGrant(component string, holder string, ttl time.Duration) (*LeaseGrant, error) {
	if m == nil || m.db == nil {
		return nil, ErrLeaseDBRequired
	}
	canonical, err := resolveLeaseCanonical(component)
	if err != nil {
		return nil, err
	}
	cleanHolder := strings.TrimSpace(holder)
	if cleanHolder == "" {
		cleanHolder = "go-core"
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}

	now := m.nowUTC()
	nowMs := now.UnixMilli()
	expiresAt := now.Add(ttl)
	expiresMs := expiresAt.UnixMilli()
	if nowMs <= 0 || expiresMs <= nowMs {
		return nil, fmt.Errorf("%w: invalid clock parameters nowMs=%d expiresMs=%d", ErrTimestampInvalid, nowMs, expiresMs)
	}

	token := generateLeaseToken(cleanHolder)
	var grant *LeaseGrant

	txErr := m.runImmediate(func(ctx context.Context, conn *sql.Conn) error {
		// 1. Inspect existing row if present (fail closed on malformed timestamps or active lease)
		var existHolder, existToken, existState string
		var existGen, existAcqMs, existExpMs int64
		row := conn.QueryRowContext(ctx,
			"SELECT holder, lease_token, fencing_generation, acquired_at_ms, expires_at_ms, state FROM ops_recovery_leases WHERE component = ?",
			canonical,
		)
		scanErr := row.Scan(&existHolder, &existToken, &existGen, &existAcqMs, &existExpMs, &existState)
		if scanErr == nil {
			// Validate timestamp sanity (R4-20 #12: malformed/zero/inverted timestamps NEVER treated as expired)
			if existAcqMs <= 0 || existExpMs <= existAcqMs {
				GlobalSecurityMetrics.IncDatabaseError()
				return fmt.Errorf("%w: corrupt lease timestamps (acq=%d exp=%d) for %s", ErrTimestampInvalid, existAcqMs, existExpMs, canonical)
			}
			// Fail-closed if lease is still active (EVEN IF same holder — R4-04!)
			if existState == "ACTIVE" && existExpMs > nowMs {
				return ErrLeaseHeld
			}
			// Expired lease: record expiration metric
			if existExpMs <= nowMs {
				GlobalSecurityMetrics.IncLeaseExpired()
			}
		} else if !errors.Is(scanErr, sql.ErrNoRows) {
			GlobalSecurityMetrics.IncDatabaseError()
			return scanErr
		}

		// 2. Monotonically advance fencing generation in ops_recovery_lease_generations
		var nextGen int64
		genRow := conn.QueryRowContext(ctx, "SELECT last_generation FROM ops_recovery_lease_generations WHERE component = ?", canonical)
		genErr := genRow.Scan(&nextGen)
		if errors.Is(genErr, sql.ErrNoRows) {
			nextGen = existGen + 1
			if _, err := conn.ExecContext(ctx,
				"INSERT INTO ops_recovery_lease_generations (component, last_generation) VALUES (?, ?)",
				canonical, nextGen,
			); err != nil {
				GlobalSecurityMetrics.IncDatabaseError()
				return err
			}
		} else if genErr != nil {
			GlobalSecurityMetrics.IncDatabaseError()
			return genErr
		} else {
			if existGen > nextGen {
				nextGen = existGen
			}
			nextGen++
			if _, err := conn.ExecContext(ctx,
				"UPDATE ops_recovery_lease_generations SET last_generation = ? WHERE component = ?",
				nextGen, canonical,
			); err != nil {
				GlobalSecurityMetrics.IncDatabaseError()
				return err
			}
		}

		// 3. Upsert lease row with atomic expiration check
		if errors.Is(scanErr, sql.ErrNoRows) {
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO ops_recovery_leases
				 (component, holder, lease_token, fencing_generation, acquired_at, expires_at, acquired_at_ms, expires_at_ms, state)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'ACTIVE')`,
				canonical, cleanHolder, token, nextGen, now, expiresAt, nowMs, expiresMs,
			); err != nil {
				return ErrLeaseHeld
			}
		} else {
			res, err := conn.ExecContext(ctx,
				`UPDATE ops_recovery_leases
				 SET holder = ?, lease_token = ?, fencing_generation = ?,
				     acquired_at = ?, expires_at = ?, acquired_at_ms = ?, expires_at_ms = ?, state = 'ACTIVE'
				 WHERE component = ? AND (state != 'ACTIVE' OR expires_at_ms <= ?)`,
				cleanHolder, token, nextGen, now, expiresAt, nowMs, expiresMs, canonical, nowMs,
			)
			if err != nil {
				GlobalSecurityMetrics.IncDatabaseError()
				return err
			}
			affected, _ := res.RowsAffected()
			if affected == 0 {
				return ErrLeaseHeld
			}
		}

		grant = &LeaseGrant{
			Component:         canonical,
			Holder:            cleanHolder,
			LeaseToken:        token,
			FencingGeneration: nextGen,
			AcquiredAtMs:      nowMs,
			ExpiresAtMs:       expiresMs,
			OperationID:       GenerateOperationID("lease"),
		}
		return nil
	})

	if txErr != nil {
		return nil, txErr
	}
	GlobalSecurityMetrics.AddLeaseActive(1)
	return grant, nil
}

// AcquireLease acquires a persistent exclusive lease and returns its lease token.
// Rejects active leases regardless of holder identity (no unconditional same-holder re-entry).
func (m *RecoveryLeaseManager) AcquireLease(component string, holder string, ttl time.Duration) (string, error) {
	grant, err := m.AcquireLeaseGrant(component, holder, ttl)
	if err != nil {
		return "", err
	}
	return grant.LeaseToken, nil
}

// RenewLease atomically extends an active lease ONLY for the exact holder token while still unexpired.
// Returns updated expires_at_ms on success, or ErrInvalidLeaseToken if expired/stolen/missing.
func (m *RecoveryLeaseManager) RenewLease(component string, leaseToken string, ttl time.Duration) (int64, error) {
	if m == nil || m.db == nil {
		return 0, ErrLeaseDBRequired
	}
	canonical, err := resolveLeaseCanonical(component)
	if err != nil || strings.TrimSpace(leaseToken) == "" {
		return 0, ErrInvalidLeaseToken
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}

	now := m.nowUTC()
	nowMs := now.UnixMilli()
	newExp := now.Add(ttl)
	newExpMs := newExp.UnixMilli()
	if nowMs <= 0 || newExpMs <= nowMs {
		return 0, ErrTimestampInvalid
	}

	var renewed bool
	txErr := m.runImmediate(func(ctx context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx,
			`UPDATE ops_recovery_leases
			 SET expires_at = ?, expires_at_ms = ?
			 WHERE component = ? AND lease_token = ? AND state = 'ACTIVE' AND expires_at_ms > ? AND acquired_at_ms > 0 AND expires_at_ms > acquired_at_ms`,
			newExp, newExpMs, canonical, leaseToken, nowMs,
		)
		if err != nil {
			GlobalSecurityMetrics.IncDatabaseError()
			return err
		}
		affected, _ := res.RowsAffected()
		renewed = affected > 0
		return nil
	})
	if txErr != nil {
		GlobalSecurityMetrics.IncLeaseRenewalFailed()
		return 0, txErr
	}
	if !renewed {
		GlobalSecurityMetrics.IncLeaseRenewalFailed()
		return 0, ErrInvalidLeaseToken
	}
	return newExpMs, nil
}

// VerifyLeaseFencing checks that the caller still holds the active lease at the expected fencing generation
// and has at least minRemaining before expiry. Strictly Fail-Closed on any mismatch or DB error.
func (m *RecoveryLeaseManager) VerifyLeaseFencing(component string, leaseToken string, fencingGen int64, minRemaining time.Duration) error {
	if m == nil || m.db == nil {
		return ErrLeaseDBRequired
	}
	canonical, err := resolveLeaseCanonical(component)
	if err != nil || strings.TrimSpace(leaseToken) == "" || fencingGen <= 0 {
		GlobalSecurityMetrics.IncFencingRejected()
		return ErrFencingTokenStale
	}

	lease, err := m.GetLease(canonical)
	if err != nil {
		GlobalSecurityMetrics.IncDatabaseError()
		GlobalSecurityMetrics.IncFencingRejected()
		return fmt.Errorf("fencing check storage error: %w", err)
	}
	if lease == nil {
		GlobalSecurityMetrics.IncFencingRejected()
		return ErrInvalidLeaseToken
	}
	if lease.AcquiredAtMs <= 0 || lease.ExpiresAtMs <= lease.AcquiredAtMs {
		GlobalSecurityMetrics.IncFencingRejected()
		return ErrTimestampInvalid
	}
	if lease.State != "ACTIVE" || lease.LeaseToken != leaseToken || lease.FencingGeneration != fencingGen {
		GlobalSecurityMetrics.IncFencingRejected()
		return fmt.Errorf("%w: token or generation mismatch (held gen=%d, claimed gen=%d)", ErrFencingTokenStale, lease.FencingGeneration, fencingGen)
	}

	nowMs := m.nowUTC().UnixMilli()
	minRemMs := minRemaining.Milliseconds()
	if lease.ExpiresAtMs-nowMs <= minRemMs {
		GlobalSecurityMetrics.IncFencingRejected()
		GlobalSecurityMetrics.IncLeaseExpired()
		return fmt.Errorf("%w: remaining TTL %dms <= guard %dms", ErrFencingTokenStale, lease.ExpiresAtMs-nowMs, minRemMs)
	}
	return nil
}

// ReleaseLease releases a recovery lease ONLY if the exact lease_token matches.
func (m *RecoveryLeaseManager) ReleaseLease(component string, leaseToken string) error {
	if m == nil || m.db == nil {
		return ErrLeaseDBRequired
	}
	canonical, err := resolveLeaseCanonical(component)
	if err != nil || strings.TrimSpace(leaseToken) == "" {
		return ErrInvalidLeaseToken
	}

	var deleted bool
	txErr := m.runImmediate(func(ctx context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx,
			"DELETE FROM ops_recovery_leases WHERE component = ? AND lease_token = ?",
			canonical, leaseToken,
		)
		if err != nil {
			GlobalSecurityMetrics.IncDatabaseError()
			return err
		}
		affected, _ := res.RowsAffected()
		deleted = affected > 0
		return nil
	})
	if txErr != nil {
		return txErr
	}
	if !deleted {
		return ErrInvalidLeaseToken
	}
	GlobalSecurityMetrics.AddLeaseActive(-1)
	return nil
}

// GetLease retrieves the current lease state for a component.
func (m *RecoveryLeaseManager) GetLease(component string) (*RecoveryLease, error) {
	if m == nil || m.db == nil {
		return nil, ErrLeaseDBRequired
	}
	canonical, err := resolveLeaseCanonical(component)
	if err != nil {
		return nil, err
	}
	var lease RecoveryLease
	res := m.db.Raw(
		"SELECT component, holder, lease_token, fencing_generation, acquired_at, expires_at, acquired_at_ms, expires_at_ms, state FROM ops_recovery_leases WHERE component = ?",
		canonical,
	).Scan(&lease)
	if res.Error != nil {
		GlobalSecurityMetrics.IncDatabaseError()
		return nil, res.Error
	}
	if lease.Component == "" {
		return nil, nil
	}
	return &lease, nil
}

// IsLeaseActive returns true if an unexpired, valid-timestamp lease is currently held for component.
func (m *RecoveryLeaseManager) IsLeaseActive(component string) (bool, string, error) {
	lease, err := m.GetLease(component)
	if err != nil {
		return false, "", err
	}
	if lease == nil {
		return false, "", nil
	}
	if lease.AcquiredAtMs <= 0 || lease.ExpiresAtMs <= lease.AcquiredAtMs {
		return false, lease.Holder, ErrTimestampInvalid
	}
	nowMs := m.nowUTC().UnixMilli()
	if lease.State == "ACTIVE" && lease.ExpiresAtMs > nowMs {
		return true, lease.Holder, nil
	}
	return false, lease.Holder, nil
}

// ============================================================================
// RecoveryExecutionContext / LeaseGuard (Single-Ownership Lifecycle)
// ============================================================================

type leaseGuardContextKey struct{}

// LeaseGuard binds an active lease grant to a cancellable execution context and background renewal loop.
type LeaseGuard struct {
	mgr    *RecoveryLeaseManager
	Grant  LeaseGrant
	mu     sync.RWMutex
	expMs  int64
	lost   atomic.Bool
	cancel context.CancelCauseFunc
	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once
}

// WithRecoveryLease attaches a LeaseGuard onto ctx.
func WithRecoveryLease(ctx context.Context, guard *LeaseGuard) context.Context {
	if guard == nil {
		return ctx
	}
	return context.WithValue(ctx, leaseGuardContextKey{}, guard)
}

// RecoveryLeaseFromContext extracts an active LeaseGuard from ctx if present.
func RecoveryLeaseFromContext(ctx context.Context) *LeaseGuard {
	if ctx == nil {
		return nil
	}
	g, _ := ctx.Value(leaseGuardContextKey{}).(*LeaseGuard)
	return g
}

// AcquireLeaseGuard acquires a lease for component, binds it to a cancellable context, and launches background renewal.
// If renewal fails or TTL elapses, the derived context is immediately cancelled (R4-06).
func AcquireLeaseGuard(
	parent context.Context,
	mgr *RecoveryLeaseManager,
	component string,
	holder string,
	ttl time.Duration,
	db *gorm.DB,
	bus *eventbus.Bus,
) (context.Context, *LeaseGuard, error) {
	if mgr == nil {
		return parent, nil, ErrLeaseDBRequired
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	grant, err := mgr.AcquireLeaseGrant(component, holder, ttl)
	if err != nil {
		return parent, nil, err
	}

	ctx, cancel := context.WithCancelCause(parent)
	guard := &LeaseGuard{
		mgr:    mgr,
		Grant:  *grant,
		expMs:  grant.ExpiresAtMs,
		cancel: cancel,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}

	renewInterval := ttl / 3
	if renewInterval < 10*time.Millisecond {
		renewInterval = 10 * time.Millisecond
	}
	if renewInterval > 10*time.Second {
		renewInterval = 10 * time.Second
	}

	go guard.renewLoop(ctx, ttl, renewInterval, db, bus)
	return WithRecoveryLease(ctx, guard), guard, nil
}

func (g *LeaseGuard) renewLoop(ctx context.Context, ttl, interval time.Duration, db *gorm.DB, bus *eventbus.Bus) {
	defer close(g.doneCh)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-g.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			newExp, err := g.mgr.RenewLease(g.Grant.Component, g.Grant.LeaseToken, ttl)
			if err != nil {
				g.lost.Store(true)
				RecordSecurityEvent(db, bus, OpsSecurityEvent{
					OperationID:       g.Grant.OperationID,
					EventType:         "ops.lease.renewal_failed",
					Severity:          "critical",
					Component:         g.Grant.Component,
					Actor:             g.Grant.Holder,
					FencingGeneration: g.Grant.FencingGeneration,
					Message:           fmt.Sprintf("lease renewal failed for %s: %v; cancelling recovery context", g.Grant.Component, err),
				})
				g.cancel(fmt.Errorf("%w: renewal failed (%v)", ErrFencingTokenStale, err))
				return
			}
			g.mu.Lock()
			g.expMs = newExp
			g.Grant.ExpiresAtMs = newExp
			g.mu.Unlock()
		}
	}
}

// VerifyBeforeMutation confirms this guard still holds the authoritative lease & fencing generation.
func (g *LeaseGuard) VerifyBeforeMutation(minRemaining time.Duration) error {
	if g == nil || g.mgr == nil {
		return ErrInvalidLeaseToken
	}
	if g.lost.Load() {
		GlobalSecurityMetrics.IncFencingRejected()
		return ErrFencingTokenStale
	}
	return g.mgr.VerifyLeaseFencing(g.Grant.Component, g.Grant.LeaseToken, g.Grant.FencingGeneration, minRemaining)
}

// Release stops renewal and releases the persistent lease. If release fails, emits critical audit alert and returns error.
func (g *LeaseGuard) Release(db *gorm.DB, bus *eventbus.Bus) error {
	if g == nil {
		return nil
	}
	g.once.Do(func() {
		close(g.stopCh)
	})
	<-g.doneCh
	if g.cancel != nil {
		defer g.cancel(context.Canceled)
	}

	err := g.mgr.ReleaseLease(g.Grant.Component, g.Grant.LeaseToken)
	if err != nil {
		GlobalSecurityMetrics.IncDatabaseError()
		RecordSecurityEvent(db, bus, OpsSecurityEvent{
			OperationID:       g.Grant.OperationID,
			EventType:         "ops.lease.release_failed",
			Severity:          "critical",
			Component:         g.Grant.Component,
			Actor:             g.Grant.Holder,
			FencingGeneration: g.Grant.FencingGeneration,
			Message:           fmt.Sprintf("failed to cleanly release recovery lease for %s (gen=%d): %v", g.Grant.Component, g.Grant.FencingGeneration, err),
		})
		return err
	}
	return nil
}
