package audit

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/redact"
)

const (
	// MinAuditHMACKeyBytes enforces 256-bit minimum entropy for external audit HMAC keys.
	MinAuditHMACKeyBytes = 32
)

var (
	auditKeyOverrideMu       sync.RWMutex
	auditKeyOverrideSet      bool
	auditKeyOverrideActiveID string
	auditKeyOverrideRing     map[string]string

	ephemeralAuditKeyOnce sync.Once
	ephemeralAuditKey     string
)

// FormatRFC3339Ms formats time in UTC with mandatory millisecond precision (YYYY-MM-DDTHH:MM:SS.mmmZ)
// matching the cross-language Go/Python audit protocol specification.
func FormatRFC3339Ms(t time.Time) string {
	return t.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z")
}

// CanonicalJSONBytes serializes arbitrary values deterministically without HTML escaping or whitespace,
// producing identical byte output across Go and Python.
func CanonicalJSONBytes(v interface{}) ([]byte, error) {
	if s, ok := v.(string); ok {
		var decoded interface{}
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		if err := dec.Decode(&decoded); err == nil {
			v = decoded
		}
	} else {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		var decoded interface{}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&decoded); err != nil {
			return nil, err
		}
		v = decoded
	}

	normalized := model.CanonicalizeJSONValue(v)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(normalized); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// CapAndRedactText sanitizes sensitive secrets and truncates oversized blobs with a SHA-256 tail tag.
func CapAndRedactText(raw string, maxBytes int) string {
	if raw == "" {
		return ""
	}
	sanitized := redact.StructuredJSON(raw)
	if maxBytes <= 0 {
		maxBytes = MaxEvidenceBytes
	}
	if len(sanitized) <= maxBytes {
		return sanitized
	}
	sum := sha256.Sum256([]byte(sanitized))
	suffix := fmt.Sprintf("...[TRUNCATED:sha256=%s]", hex.EncodeToString(sum[:8]))
	cut := maxBytes - len(suffix)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(sanitized[cut]) {
		cut--
	}
	return sanitized[:cut] + suffix
}

// SerializeParams computes both the canonical SHA-256 digest of original parameters and the
// capped, redacted canonical JSON representation stored in audit logs.
func SerializeParams(params interface{}) (digest string, redactedJSON string) {
	if params == nil {
		return "", ""
	}
	if str, ok := params.(string); ok && strings.TrimSpace(str) == "" {
		return "", ""
	}
	rawBytes, err := CanonicalJSONBytes(params)
	if err == nil && len(rawBytes) > 0 {
		sum := sha256.Sum256(rawBytes)
		digest = hex.EncodeToString(sum[:])
	}

	redactedVal := redact.Value(params)
	if str, ok := params.(string); ok {
		redactedVal = redact.StructuredJSON(str)
	}
	redBytes, err := CanonicalJSONBytes(redactedVal)
	if err != nil {
		redactedJSON = CapAndRedactText(fmt.Sprintf("%v", params), MaxEvidenceBytes)
		return digest, redactedJSON
	}
	redactedJSON = CapAndRedactText(string(redBytes), MaxEvidenceBytes)
	return digest, redactedJSON
}

// SerializeEvidence normalizes structured or string state/evidence payloads with redaction and size limits.
func SerializeEvidence(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return CapAndRedactText(s, MaxEvidenceBytes)
	}
	redacted := redact.Value(v)
	b, err := CanonicalJSONBytes(redacted)
	if err != nil {
		return CapAndRedactText(fmt.Sprintf("%v", v), MaxEvidenceBytes)
	}
	return CapAndRedactText(string(b), MaxEvidenceBytes)
}

// CanonicalEventHashPayload builds the deterministic Go/Python-identical hash input object.
func CanonicalEventHashPayload(ev *model.AgentAuditEvent) map[string]interface{} {
	createdAtMs := ev.CreatedAtMs
	createdAtStr := FormatRFC3339Ms(ev.CreatedAt)
	if createdAtMs == 0 && !ev.CreatedAt.IsZero() {
		createdAtMs = ev.CreatedAt.UTC().UnixMilli()
	}
	return map[string]interface{}{
		"audit_id":            ev.AuditID,
		"run_id":              ev.RunID,
		"parent_event_id":     ev.ParentEventID,
		"correlation_id":      ev.CorrelationID,
		"household_id":        int64(ev.HouseholdID),
		"trigger_source":      ev.TriggerSource,
		"actor_type":          ev.ActorType,
		"actor_id":            ev.ActorID,
		"event_type":          ev.EventType,
		"action":              ev.Action,
		"target":              ev.Target,
		"component":           ev.Component,
		"decision":            ev.Decision,
		"risk_level":          ev.RiskLevel,
		"policy_version":      ev.PolicyVersion,
		"proposal_id":         ev.ProposalID,
		"approval_id":         ev.ApprovalID,
		"operation_id":        ev.OperationID,
		"fencing_generation":  ev.FencingGeneration,
		"before_state":        ev.BeforeState,
		"after_state":         ev.AfterState,
		"parameters_digest":   ev.ParametersDigest,
		"parameters_redacted": ev.ParametersRedacted,
		"result":              ev.Result,
		"error_code":          ev.ErrorCode,
		"duration_ms":         ev.DurationMs,
		"evidence":            ev.Evidence,
		"created_at":          createdAtStr,
		"created_at_ms":       createdAtMs,
		"is_offline":          ev.IsOffline,
		"previous_hash":       ev.PreviousHash,
	}
}

// ComputeEventEntryHash computes the SHA-256 entry_hash for an AgentAuditEvent.
func ComputeEventEntryHash(ev *model.AgentAuditEvent) (string, error) {
	if ev == nil {
		return "", fmt.Errorf("nil audit event")
	}
	canon, err := CanonicalJSONBytes(CanonicalEventHashPayload(ev))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

// ComputeCheckpointChainDigest computes SHA256(prevChainDigest | tailEntryHash | eventCount).
func ComputeCheckpointChainDigest(prevDigest, tailEntryHash string, eventCount int64) string {
	material := fmt.Sprintf("%s|%s|%d", prevDigest, tailEntryHash, eventCount)
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

// SetAuditHMACKeyringForTest overrides the active external audit HMAC keyring inside unit tests.
func SetAuditHMACKeyringForTest(activeKID string, ring map[string]string) func() {
	auditKeyOverrideMu.Lock()
	auditKeyOverrideSet = true
	auditKeyOverrideActiveID = activeKID
	auditKeyOverrideRing = make(map[string]string, len(ring))
	for k, v := range ring {
		auditKeyOverrideRing[k] = v
	}
	auditKeyOverrideMu.Unlock()

	return func() {
		auditKeyOverrideMu.Lock()
		auditKeyOverrideSet = false
		auditKeyOverrideActiveID = ""
		auditKeyOverrideRing = nil
		auditKeyOverrideMu.Unlock()
	}
}

// ResolveAuditHMACKeyring loads external non-DB HMAC keys for audit chain authentication.
// Never falls back to JWT_SECRET or hardcoded secrets.
func ResolveAuditHMACKeyring() (string, map[string]string, error) {
	auditKeyOverrideMu.RLock()
	if auditKeyOverrideSet {
		aid := auditKeyOverrideActiveID
		ringCopy := make(map[string]string, len(auditKeyOverrideRing))
		for k, v := range auditKeyOverrideRing {
			ringCopy[k] = v
		}
		auditKeyOverrideMu.RUnlock()
		if aid == "" || len(ringCopy) == 0 {
			return "", nil, ErrAuditKeyMissing
		}
		return aid, ringCopy, nil
	}
	auditKeyOverrideMu.RUnlock()

	ring := make(map[string]string)
	activeKID := strings.TrimSpace(os.Getenv("OPS_AUDIT_HMAC_KEY_ID"))
	if activeKID == "" {
		activeKID = "k1"
	}

	if rawKeys, hasKeys := os.LookupEnv("OPS_AUDIT_HMAC_KEYS"); hasKeys {
		for _, pair := range strings.Split(rawKeys, ",") {
			trimmed := strings.TrimSpace(pair)
			if trimmed == "" {
				continue
			}
			parts := strings.SplitN(trimmed, ":", 2)
			if len(parts) == 2 {
				kid := strings.TrimSpace(parts[0])
				sec := strings.TrimSpace(parts[1])
				if kid != "" && sec != "" {
					ring[kid] = sec
				}
			}
		}
	}

	rawPrimary, primarySet := os.LookupEnv("OPS_AUDIT_HMAC_KEY")
	primary := strings.TrimSpace(rawPrimary)
	if primarySet {
		if primary == "" {
			return "", nil, ErrAuditKeyMissing
		}
		if len(primary) < MinAuditHMACKeyBytes {
			return "", nil, fmt.Errorf("%w: got %d bytes", ErrAuditKeyWeak, len(primary))
		}
		ring[activeKID] = primary
	}

	if _, ok := ring[activeKID]; ok {
		for kid, sec := range ring {
			if len(sec) < MinAuditHMACKeyBytes {
				return "", nil, fmt.Errorf("%w: key '%s' has %d bytes", ErrAuditKeyWeak, kid, len(sec))
			}
		}
		return activeKID, ring, nil
	}

	// Only under `go test` when env var was not explicitly set to empty, generate an ephemeral CSPRNG key.
	if testing.Testing() && !primarySet {
		ephemeralAuditKeyOnce.Do(func() {
			buf := make([]byte, 32)
			_, _ = rand.Read(buf)
			ephemeralAuditKey = hex.EncodeToString(buf)
		})
		ring[activeKID] = ephemeralAuditKey
		return activeKID, ring, nil
	}

	return "", nil, ErrAuditKeyMissing
}

// ValidateAuditHMACKeyBootGuard enforces strict external HMAC key presence in production environments.
func ValidateAuditHMACKeyBootGuard(isProduction bool) error {
	if !isProduction {
		return nil
	}
	_, ring, err := ResolveAuditHMACKeyring()
	if err != nil {
		return err
	}
	for kid, sec := range ring {
		if len(sec) < MinAuditHMACKeyBytes {
			return fmt.Errorf("%w: key '%s'", ErrAuditKeyWeak, kid)
		}
		lower := strings.ToLower(strings.TrimSpace(sec))
		for _, weakSub := range []string{"change-me", "placeholder", "your-secret", "example-key", "replace-this"} {
			if strings.Contains(lower, weakSub) {
				return fmt.Errorf("%w: key '%s' uses weak placeholder pattern", ErrAuditKeyWeak, kid)
			}
		}
	}
	return nil
}

// ComputeEventHMAC signs (stream_id|seq|entry_hash|created_at_ms) with the active keyring secret.
func ComputeEventHMAC(kid, secret, streamID string, seq uint64, entryHash string, createdAtMs int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%s|%d|%s|%d", streamID, seq, entryHash, createdAtMs)))
	return fmt.Sprintf("hmac-sha256:%s:%s", kid, hex.EncodeToString(mac.Sum(nil)))
}

// VerifyEventHMAC verifies an AgentAuditEvent HMAC signature against the external keyring.
func VerifyEventHMAC(ev *model.AgentAuditEvent, ring map[string]string) (string, error) {
	if ev == nil {
		return "", fmt.Errorf("nil event")
	}
	parts := strings.SplitN(strings.TrimSpace(ev.HMACSignature), ":", 3)
	if len(parts) != 3 || parts[0] != "hmac-sha256" || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf("malformed or downgraded audit HMAC signature format")
	}
	kid := parts[1]
	secret, ok := ring[kid]
	if !ok || len(secret) < MinAuditHMACKeyBytes {
		return kid, fmt.Errorf("unknown or weak audit signing key id %q", kid)
	}
	expected := ComputeEventHMAC(kid, secret, ev.StreamID, ev.Seq, ev.EntryHash, ev.CreatedAtMs)
	if subtle.ConstantTimeCompare([]byte(ev.HMACSignature), []byte(expected)) != 1 {
		return kid, fmt.Errorf("audit event HMAC signature mismatch for seq=%d", ev.Seq)
	}
	return kid, nil
}

// ComputeCheckpointHMAC signs an AgentAuditCheckpoint record using the active HMAC key.
func ComputeCheckpointHMAC(kid, secret string, cp *model.AgentAuditCheckpoint) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%s|%s|%d|%d|%d|%s|%s|%s|%d",
		cp.CheckpointID, cp.StreamID, cp.FromSeq, cp.ToSeq, cp.EventCount,
		cp.HeadEntryHash, cp.TailEntryHash, cp.ChainDigest, cp.CreatedAtMs)))
	return fmt.Sprintf("hmac-sha256:%s:%s", kid, hex.EncodeToString(mac.Sum(nil)))
}

// VerifyCheckpointHMAC validates an AgentAuditCheckpoint HMAC against keyring secrets.
func VerifyCheckpointHMAC(cp *model.AgentAuditCheckpoint, ring map[string]string) error {
	if cp == nil {
		return fmt.Errorf("nil checkpoint")
	}
	parts := strings.SplitN(strings.TrimSpace(cp.HMACSignature), ":", 3)
	if len(parts) != 3 || parts[0] != "hmac-sha256" || parts[1] == "" || parts[2] == "" {
		return fmt.Errorf("malformed checkpoint HMAC signature")
	}
	kid := parts[1]
	secret, ok := ring[kid]
	if !ok || len(secret) < MinAuditHMACKeyBytes {
		return fmt.Errorf("unknown checkpoint signing key id %q", kid)
	}
	expected := ComputeCheckpointHMAC(kid, secret, cp)
	if subtle.ConstantTimeCompare([]byte(cp.HMACSignature), []byte(expected)) != 1 {
		return fmt.Errorf("checkpoint HMAC signature mismatch for checkpoint %s", cp.CheckpointID)
	}
	return nil
}

// VerifyOfflineOriginHMAC validates an offline event HMAC computed by the Python edge agent.
// Python signs (offline_seq|entry_hash|executed_at_ms) or ("offline-agent"|offline_seq|entry_hash|executed_at_ms).
func VerifyOfflineOriginHMAC(ev OfflineSyncEvent, ring map[string]string) error {
	sig := strings.TrimSpace(ev.HMACSignature)
	if sig == "" {
		return fmt.Errorf("missing offline HMAC signature")
	}
	parts := strings.SplitN(sig, ":", 3)
	if len(parts) != 3 || parts[0] != "hmac-sha256" || parts[1] == "" || parts[2] == "" {
		return fmt.Errorf("malformed offline HMAC format")
	}
	kid := parts[1]
	secret, ok := ring[kid]
	if !ok || len(secret) < MinAuditHMACKeyBytes {
		return fmt.Errorf("unknown offline signing key id %q", kid)
	}
	expected := ComputeEventHMAC(kid, secret, "offline-agent", ev.OfflineSeq, ev.EntryHash, ev.ExecutedAtMs)
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) == 1 {
		return nil
	}
	return fmt.Errorf("offline HMAC verification failed for event %s", ev.EventUUID)
}
