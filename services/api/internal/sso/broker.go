package sso

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/utils"
)

var (
	ErrTicketInvalidOrExpired = errors.New("sso ticket is invalid, expired, or already consumed")
	ErrReplayAttackDetected   = errors.New("sso ticket replay attack detected")
	ErrUnauthorizedAdminOnly  = errors.New("sso ticket generation is restricted to administrators")
)

// Broker handles issuance and single-use atomic consumption of SSO tickets.
type Broker struct {
	db        *gorm.DB
	ticketTTL time.Duration
}

// NewBroker creates an SSO broker with default 30-second ticket TTL.
func NewBroker(db *gorm.DB) *Broker {
	return &Broker{
		db:        db,
		ticketTTL: 30 * time.Second, // 30s strict validity
	}
}

// SetTicketTTL allows overriding the ticket TTL (used in testing).
func (b *Broker) SetTicketTTL(ttl time.Duration) {
	b.ticketTTL = ttl
}

// IssueTicket generates a 256-bit cryptographically secure ticket and saves its SHA-256 hash.
// Plaintext ticket is returned to caller and NEVER stored in the database.
func (b *Broker) IssueTicket(ctx context.Context, userID uint, username string, isAdmin bool, clientIP, userAgent string) (string, time.Time, error) {
	if !isAdmin {
		return "", time.Time{}, ErrUnauthorizedAdminOnly
	}

	// 1. Generate 256 bits (32 bytes) of cryptographic entropy
	rawTicketBytes := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, rawTicketBytes); err != nil {
		return "", time.Time{}, fmt.Errorf("failed to generate secure random ticket: %w", err)
	}
	plaintextTicket := hex.EncodeToString(rawTicketBytes)

	// 2. Compute SHA-256 hash for database storage (plaintext ticket never persisted)
	hashBytes := sha256.Sum256([]byte(plaintextTicket))
	ticketHash := hex.EncodeToString(hashBytes[:])

	expiresAt := time.Now().Add(b.ticketTTL)

	record := model.SSOTicket{
		TicketHash: ticketHash,
		UserID:     userID,
		Username:   username,
		ExpiresAt:  expiresAt,
		Consumed:   false,
		ClientIP:   clientIP,
		UserAgent:  userAgent,
		CreatedAt:  time.Now(),
	}

	if err := b.db.WithContext(ctx).Create(&record).Error; err != nil {
		return "", time.Time{}, fmt.Errorf("failed to persist sso ticket: %w", err)
	}

	return plaintextTicket, expiresAt, nil
}

// VerifyAndConsume atomically checks ticket validity and marks it consumed (Atomic CAS).
// Returns ErrReplayAttackDetected if the ticket was already consumed.
func (b *Broker) VerifyAndConsume(ctx context.Context, plaintextTicket, clientIP string) (*model.SSOTicket, error) {
	if len(plaintextTicket) < 32 {
		return nil, ErrTicketInvalidOrExpired
	}

	hashBytes := sha256.Sum256([]byte(plaintextTicket))
	ticketHash := hex.EncodeToString(hashBytes[:])

	now := time.Now()

	// Atomic CAS query: UPDATE WHERE ticket_hash = ? AND consumed = false AND expires_at > now
	result := b.db.WithContext(ctx).
		Model(&model.SSOTicket{}).
		Where("ticket_hash = ? AND consumed = ? AND expires_at > ?", ticketHash, false, now).
		Updates(map[string]any{
			"consumed":    true,
			"consumed_at": utils.NullTime{Time: now, Valid: true},
		})

	if result.Error != nil {
		return nil, fmt.Errorf("database error during ticket consumption: %w", result.Error)
	}

	if result.RowsAffected == 0 {
		// Differentiate between replay, expiration, or non-existent
		var check model.SSOTicket
		if err := b.db.WithContext(ctx).Where("ticket_hash = ?", ticketHash).First(&check).Error; err == nil {
			if check.Consumed {
				return nil, ErrReplayAttackDetected
			}
		}
		return nil, ErrTicketInvalidOrExpired
	}

	var ticket model.SSOTicket
	if err := b.db.WithContext(ctx).Where("ticket_hash = ?", ticketHash).First(&ticket).Error; err != nil {
		return nil, err
	}

	return &ticket, nil
}
