package ops

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/eventbus"
	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/security"
)

// SecurityMetricsSnapshot represents the Round 4 security closed-loop telemetry counters (R4-25).
type SecurityMetricsSnapshot struct {
	RecoveryLeaseActive        int64 `json:"recovery_lease_active"`
	RecoveryLeaseExpired       int64 `json:"recovery_lease_expired"`
	RecoveryLeaseRenewalFailed int64 `json:"recovery_lease_renewal_failed"`
	RecoveryFencingRejected    int64 `json:"recovery_fencing_rejected"`
	RecoveryExecutionUnknown   int64 `json:"recovery_execution_unknown"`
	RecoveryAuditFailed        int64 `json:"recovery_audit_failed"`
	CircuitBreakerDegraded     int64 `json:"circuit_breaker_degraded"`
	MaintenanceLockDegraded    int64 `json:"maintenance_lock_degraded"`
	ProposalCASConflict        int64 `json:"proposal_cas_conflict"`
	ProposalDigestMismatch     int64 `json:"proposal_digest_mismatch"`
	RecoveryHealthUnknown      int64 `json:"recovery_health_unknown"`
	RecoveryDatabaseError      int64 `json:"recovery_database_error"`
}

// SecurityMetricsRegistry tracks process-wide atomic counters for autonomous ops security states.
type SecurityMetricsRegistry struct {
	recoveryLeaseActive        atomic.Int64
	recoveryLeaseExpired       atomic.Int64
	recoveryLeaseRenewalFailed atomic.Int64
	recoveryFencingRejected    atomic.Int64
	recoveryExecutionUnknown   atomic.Int64
	recoveryAuditFailed        atomic.Int64
	circuitBreakerDegraded     atomic.Int64
	maintenanceLockDegraded    atomic.Int64
	proposalCASConflict        atomic.Int64
	proposalDigestMismatch     atomic.Int64
	recoveryHealthUnknown      atomic.Int64
	recoveryDatabaseError      atomic.Int64
}

var GlobalSecurityMetrics = &SecurityMetricsRegistry{}

// GenerateOperationID creates a unique correlation ID for tracing an autonomous or manual recovery chain.
func GenerateOperationID(prefix string) string {
	if prefix == "" {
		prefix = "op"
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UTC().UnixMilli(), hex.EncodeToString(b))
}

// Snapshot returns a point-in-time copy of all Round 4 security metrics.
func (m *SecurityMetricsRegistry) Snapshot() SecurityMetricsSnapshot {
	if m == nil {
		return SecurityMetricsSnapshot{}
	}
	return SecurityMetricsSnapshot{
		RecoveryLeaseActive:        m.recoveryLeaseActive.Load(),
		RecoveryLeaseExpired:       m.recoveryLeaseExpired.Load(),
		RecoveryLeaseRenewalFailed: m.recoveryLeaseRenewalFailed.Load(),
		RecoveryFencingRejected:    m.recoveryFencingRejected.Load(),
		RecoveryExecutionUnknown:   m.recoveryExecutionUnknown.Load(),
		RecoveryAuditFailed:        m.recoveryAuditFailed.Load(),
		CircuitBreakerDegraded:     m.circuitBreakerDegraded.Load(),
		MaintenanceLockDegraded:    m.maintenanceLockDegraded.Load(),
		ProposalCASConflict:        m.proposalCASConflict.Load(),
		ProposalDigestMismatch:     m.proposalDigestMismatch.Load(),
		RecoveryHealthUnknown:      m.recoveryHealthUnknown.Load(),
		RecoveryDatabaseError:      m.recoveryDatabaseError.Load(),
	}
}

func (m *SecurityMetricsRegistry) SetLeaseActive(active int64) {
	if m != nil {
		m.recoveryLeaseActive.Store(active)
	}
}
func (m *SecurityMetricsRegistry) AddLeaseActive(delta int64) {
	if m != nil {
		if m.recoveryLeaseActive.Add(delta) < 0 {
			m.recoveryLeaseActive.Store(0)
		}
	}
}
func (m *SecurityMetricsRegistry) IncLeaseExpired()       { if m != nil { m.recoveryLeaseExpired.Add(1) } }
func (m *SecurityMetricsRegistry) IncLeaseRenewalFailed() { if m != nil { m.recoveryLeaseRenewalFailed.Add(1) } }
func (m *SecurityMetricsRegistry) IncFencingRejected()    { if m != nil { m.recoveryFencingRejected.Add(1) } }
func (m *SecurityMetricsRegistry) IncExecutionUnknown()   { if m != nil { m.recoveryExecutionUnknown.Add(1) } }
func (m *SecurityMetricsRegistry) IncAuditFailed()        { if m != nil { m.recoveryAuditFailed.Add(1) } }
func (m *SecurityMetricsRegistry) SetCircuitBreakerDegraded(degraded bool) {
	if m != nil {
		if degraded {
			m.circuitBreakerDegraded.Store(1)
		} else {
			m.circuitBreakerDegraded.Store(0)
		}
	}
}
func (m *SecurityMetricsRegistry) SetMaintenanceLockDegraded(degraded bool) {
	if m != nil {
		if degraded {
			m.maintenanceLockDegraded.Store(1)
		} else {
			m.maintenanceLockDegraded.Store(0)
		}
	}
}
func (m *SecurityMetricsRegistry) IncProposalCASConflict()    { if m != nil { m.proposalCASConflict.Add(1) } }
func (m *SecurityMetricsRegistry) IncProposalDigestMismatch() { if m != nil { m.proposalDigestMismatch.Add(1) } }
func (m *SecurityMetricsRegistry) IncHealthUnknown()          { if m != nil { m.recoveryHealthUnknown.Add(1) } }
func (m *SecurityMetricsRegistry) IncDatabaseError()          { if m != nil { m.recoveryDatabaseError.Add(1) } }

// OpsSecurityEvent represents a structured, cross-language consistent security event record.
type OpsSecurityEvent struct {
	OperationID       string `json:"operation_id"`
	EventType         string `json:"event_type"`
	Severity          string `json:"severity"` // "info", "warning", "critical"
	Component         string `json:"component"`
	Actor             string `json:"actor"`
	FencingGeneration int64  `json:"fencing_generation,omitempty"`
	ProposalID        string `json:"proposal_id,omitempty"`
	FaultClass        string `json:"fault_class,omitempty"`
	Message           string `json:"message"`
	TimestampMs       int64  `json:"timestamp_ms"`
}

// RecordSecurityEvent emits a structured security rejection/audit event across stderr log,
// EventBus, and SQLite SystemLog/StructuredOpsAuditLog without ever discarding events on DB errors
// or leaking credentials/tokens.
func RecordSecurityEvent(db *gorm.DB, bus *eventbus.Bus, ev OpsSecurityEvent) {
	if ev.TimestampMs <= 0 {
		ev.TimestampMs = time.Now().UTC().UnixMilli()
	}
	if ev.OperationID == "" {
		ev.OperationID = GenerateOperationID("sec")
	}
	ev.Message = security.RedactSensitiveInfo(ev.Message)
	ev.Component = security.RedactSensitiveInfo(ev.Component)
	ev.Actor = security.RedactSensitiveInfo(ev.Actor)

	payloadBytes, _ := json.Marshal(ev)

	// Always log to stderr so critical security events survive DB unavailability.
	log.Printf("[ops-security] op=%s type=%s sev=%s comp=%s gen=%d fault=%s msg=%s",
		ev.OperationID, ev.EventType, ev.Severity, ev.Component, ev.FencingGeneration, ev.FaultClass, ev.Message)

	if bus != nil {
		sev := eventbus.SeverityWarn
		if ev.Severity == "critical" {
			sev = eventbus.SeverityCritical
		} else if ev.Severity == "info" {
			sev = eventbus.SeverityInfo
		}
		bus.Publish(eventbus.Event{
			Topic:    eventbus.TopicSystemLog,
			Source:   eventbus.SourceSystem,
			Severity: sev,
			Payload:  payloadBytes,
		})
	}

	if db != nil {
		lvl := model.LevelWarning
		if ev.Severity == "critical" {
			lvl = model.LevelCritical
		} else if ev.Severity == "info" {
			lvl = model.LevelInfo
		}
		entry := model.SystemLog{
			HouseholdID: 1,
			Ts:          ev.TimestampMs / 1000,
			EventType:   ev.EventType,
			Level:       lvl,
			Source:      "ops_security",
			Context:     ev.Component,
			Message:     fmt.Sprintf("[%s] %s", ev.OperationID, ev.Message),
			Payload:     string(payloadBytes),
		}
		if err := db.Create(&entry).Error; err != nil {
			GlobalSecurityMetrics.IncAuditFailed()
			log.Printf("[ops-security-fallback] CRITICAL: failed to persist security audit event op=%s type=%s: %v",
				ev.OperationID, ev.EventType, err)
		}
	}
}
