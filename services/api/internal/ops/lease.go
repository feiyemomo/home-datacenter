package ops

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

var (
	// ErrLeaseHeld is returned when a recovery lease is actively held by another executor.
	ErrLeaseHeld = errors.New("recovery lease is held by another executor")
	// ErrInvalidLeaseToken is returned when attempting to release a lease with an invalid or expired token.
	ErrInvalidLeaseToken = errors.New("invalid or expired recovery lease token")
	// ErrLeaseDBRequired is returned when a database handle is required for persistent leasing.
	ErrLeaseDBRequired = errors.New("database connection required for persistent lease")
)

// RecoveryLease models the persistent mutual exclusion record in ops_recovery_leases (R13).
type RecoveryLease struct {
	Component  string    `gorm:"primaryKey;column:component;size:64" json:"component"`
	Holder     string    `gorm:"column:holder;size:64;not null" json:"holder"`
	LeaseToken string    `gorm:"column:lease_token;size:64;not null" json:"lease_token"`
	AcquiredAt time.Time `gorm:"column:acquired_at;not null" json:"acquired_at"`
	ExpiresAt  time.Time `gorm:"column:expires_at;not null;index" json:"expires_at"`
}

// TableName returns the table name in SQLite.
func (RecoveryLease) TableName() string {
	return "ops_recovery_leases"
}

// RecoveryLeaseManager coordinates persistent recovery leases between Go Core and Python Ops Agent.
type RecoveryLeaseManager struct {
	db *gorm.DB
	mu sync.RWMutex
}

// NewRecoveryLeaseManager constructs a RecoveryLeaseManager with automatic table migration.
func NewRecoveryLeaseManager(db *gorm.DB) *RecoveryLeaseManager {
	mgr := &RecoveryLeaseManager{db: db}
	if db != nil {
		_ = mgr.AutoMigrate()
	}
	return mgr
}

// AutoMigrate creates or updates the ops_recovery_leases table schema.
func (m *RecoveryLeaseManager) AutoMigrate() error {
	if m == nil || m.db == nil {
		return ErrLeaseDBRequired
	}
	// Direct DDL ensures exact column definitions and indices across all SQLite drivers
	ddl := `
	CREATE TABLE IF NOT EXISTS ops_recovery_leases (
		component TEXT PRIMARY KEY,
		holder TEXT NOT NULL,
		lease_token TEXT NOT NULL,
		acquired_at DATETIME NOT NULL,
		expires_at DATETIME NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_ops_recovery_leases_expires ON ops_recovery_leases (expires_at);
	`
	if err := m.db.Exec(ddl).Error; err != nil {
		return err
	}
	return m.db.AutoMigrate(&RecoveryLease{})
}

func generateLeaseToken(holder string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("lease-%s-%s", holder, hex.EncodeToString(b))
}

// AcquireLease atomically acquires or renews an exclusive persistent recovery lease for a component.
// It succeeds only if:
// 1. No lease currently exists for the canonical component, OR
// 2. The existing lease has expired (expires_at < now), OR
// 3. The existing lease is currently held by the same holder.
// If another active holder holds the lease, ErrLeaseHeld is returned.
func (m *RecoveryLeaseManager) AcquireLease(component string, holder string, ttl time.Duration) (string, error) {
	if m == nil || m.db == nil {
		return "", ErrLeaseDBRequired
	}
	canonical := ResolveTopologyComponent(component)
	if strings.TrimSpace(canonical) == "" {
		return "", errors.New("invalid or empty component identifier")
	}
	if strings.TrimSpace(holder) == "" {
		holder = "go-core"
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}

	token := generateLeaseToken(holder)
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)

	var acquiredToken string
	err := m.db.Transaction(func(tx *gorm.DB) error {
		// Attempt atomic update if expired or owned by the same holder
		updateRes := tx.Exec(
			"UPDATE ops_recovery_leases SET holder = ?, lease_token = ?, acquired_at = ?, expires_at = ? WHERE component = ? AND (expires_at < ? OR holder = ?)",
			holder, token, now, expiresAt, canonical, now, holder,
		)
		if updateRes.Error != nil {
			return updateRes.Error
		}
		if updateRes.RowsAffected > 0 {
			acquiredToken = token
			return nil
		}

		// Check if row exists and is currently held by someone else
		var count int64
		if err := tx.Raw("SELECT COUNT(*) FROM ops_recovery_leases WHERE component = ?", canonical).Scan(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			// Row exists, meaning an active lease is held by another holder
			return ErrLeaseHeld
		}

		// Row does not exist; attempt insert
		insertRes := tx.Exec(
			"INSERT INTO ops_recovery_leases (component, holder, lease_token, acquired_at, expires_at) VALUES (?, ?, ?, ?, ?)",
			canonical, holder, token, now, expiresAt,
		)
		if insertRes.Error != nil {
			// Constraint violation if another process inserted concurrently
			return ErrLeaseHeld
		}
		if insertRes.RowsAffected > 0 {
			acquiredToken = token
			return nil
		}

		return ErrLeaseHeld
	})

	if err != nil {
		return "", err
	}
	return acquiredToken, nil
}

// ReleaseLease releases a recovery lease held by this holder and token.
func (m *RecoveryLeaseManager) ReleaseLease(component string, leaseToken string) error {
	if m == nil || m.db == nil {
		return ErrLeaseDBRequired
	}
	canonical := ResolveTopologyComponent(component)
	if strings.TrimSpace(canonical) == "" || strings.TrimSpace(leaseToken) == "" {
		return ErrInvalidLeaseToken
	}

	res := m.db.Exec("DELETE FROM ops_recovery_leases WHERE component = ? AND lease_token = ?", canonical, leaseToken)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrInvalidLeaseToken
	}
	return nil
}

// GetLease retrieves the current lease state for a component.
func (m *RecoveryLeaseManager) GetLease(component string) (*RecoveryLease, error) {
	if m == nil || m.db == nil {
		return nil, ErrLeaseDBRequired
	}
	canonical := ResolveTopologyComponent(component)
	var lease RecoveryLease
	res := m.db.Raw("SELECT component, holder, lease_token, acquired_at, expires_at FROM ops_recovery_leases WHERE component = ?", canonical).Scan(&lease)
	if res.Error != nil {
		return nil, res.Error
	}
	if lease.Component == "" {
		return nil, nil
	}
	return &lease, nil
}

// IsLeaseActive returns true if an unexpired lease is currently held for component.
func (m *RecoveryLeaseManager) IsLeaseActive(component string) (bool, string, error) {
	lease, err := m.GetLease(component)
	if err != nil {
		return false, "", err
	}
	if lease == nil {
		return false, "", nil
	}
	if time.Now().UTC().Before(lease.ExpiresAt) {
		return true, lease.Holder, nil
	}
	return false, lease.Holder, nil
}
