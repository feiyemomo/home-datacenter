package model

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"home-datacenter-api/internal/utils"
)

// Proposal status constants.
const (
	ProposalStatusPending                = "PENDING"
	ProposalStatusAwaitingApproval       = "AWAITING_APPROVAL" // Canonical / backward compatible
	ProposalStatusApprovalRequired       = "APPROVAL_REQUIRED"
	ProposalStatusApproved               = "APPROVED"
	ProposalStatusScheduled              = "SCHEDULED"
	ProposalStatusExecuting              = "EXECUTING"
	ProposalStatusSuccess                = "SUCCESS"           // Canonical completed state
	ProposalStatusExecuted               = "EXECUTED"          // Backward compatible alias for SUCCESS
	ProposalStatusRejected               = "REJECTED"
	ProposalStatusCancelled              = "CANCELLED"
	ProposalStatusFailed                 = "FAILED"
	ProposalStatusRollingBack            = "ROLLING_BACK"
	ProposalStatusRolledBack             = "ROLLED_BACK"
	ProposalStatusFailedRolledback       = "FAILED_ROLLEDBACK" // Backward compatible alias for ROLLED_BACK
	ProposalStatusRollbackFailed         = "ROLLBACK_FAILED"
	ProposalStatusExecutionUnknown       = "EXECUTION_UNKNOWN"       // Network timeout / unknown response
	ProposalStatusReconciliationRequired = "RECONCILIATION_REQUIRED" // Process crash or ambiguous state
	ProposalStatusExpired                = "EXPIRED"                 // Scheduled window missed
)

// Risk level constants.
const (
	RiskLevelLow      = "LOW"
	RiskLevelMedium   = "MEDIUM"
	RiskLevelHigh     = "HIGH"
	RiskLevelCritical = "CRITICAL"
)

// OpsProposal represents a system improvement or maintenance proposal
// managed by the Ops Agent and pending administrator approval.
type OpsProposal struct {
	ID               uint           `gorm:"primaryKey" json:"id"`
	ProposalID       string         `gorm:"size:64;uniqueIndex" json:"proposal_id"`
	IdempotencyKey   string         `gorm:"size:128;index" json:"idempotency_key,omitempty"`
	HouseholdID      uint           `gorm:"index;default:1" json:"household_id"`
	Title            string         `gorm:"size:255;not null" json:"title"`
	Description      string         `gorm:"type:text" json:"description"`
	TargetComponent  string         `gorm:"size:128" json:"target_component"`
	RiskLevel        string         `gorm:"size:32;not null;default:'LOW'" json:"risk_level"`
	EstimatedSeconds int            `gorm:"default:30" json:"estimated_seconds"`
	DowntimeSeconds  int            `gorm:"default:0" json:"downtime_seconds"`
	ExecutionPlan    string         `gorm:"type:text;not null" json:"execution_plan"`
	RollbackPlan     string         `gorm:"type:text;not null" json:"rollback_plan"`
	Status           string         `gorm:"size:32;not null;index;default:'AWAITING_APPROVAL'" json:"status"`
	ScheduledAt      *string        `gorm:"size:64" json:"scheduled_at"`
	ApprovedAt       utils.NullTime `json:"approved_at"`
	ApprovedBy       string         `gorm:"size:64" json:"approved_by"`
	ExecutedAt       utils.NullTime `json:"executed_at"`
	ReconciledAt     utils.NullTime `json:"reconciled_at,omitempty"`
	ExecutionLog     string         `gorm:"type:text" json:"execution_log"`
	BaselineHash     string         `gorm:"size:128" json:"baseline_hash"`
	ActionSpec       string         `gorm:"type:text" json:"action_spec"`
	ExecutionPolicy  string         `gorm:"column:execution_policy;size:128" json:"execution_policy,omitempty"`
	ApprovalDigest   string         `gorm:"column:approval_digest;type:varchar(160)" json:"approval_digest,omitempty"`
	ExecutionToken   string         `gorm:"column:execution_token;size:128;default:''" json:"execution_token,omitempty"`
	Version          int            `gorm:"column:version;default:1" json:"version"`
	Revision         int            `gorm:"column:revision;default:1" json:"revision"`
	CreatedAt        time.Time      `gorm:"not null" json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
}

func (OpsProposal) TableName() string { return "pending_proposals" }

// Approval cryptographic errors.
var (
	ErrApprovalKeyMissing         = errors.New("approval HMAC signing key is not configured")
	ErrApprovalKeyWeak            = errors.New("approval HMAC signing key must be at least 32 bytes")
	ErrApprovalSignatureDowngrade = errors.New("unsigned legacy sha256 approval digest is rejected in fail-closed mode")
	ErrApprovalUnknownField       = errors.New("operation spec contains unknown or disallowed JSON field")
)

const (
	ApprovalDigestPrefixHMAC = "hmac-sha256:"
	MinApprovalHMACKeyBytes  = 32
)

var (
	approvalKeyOverrideMu       sync.RWMutex
	approvalKeyOverrideSet      bool
	approvalKeyOverrideActiveID string
	approvalKeyOverrideRing     map[string]string
	ephemeralTestKeyOnce        sync.Once
	ephemeralTestKey            string
)

// SetApprovalHMACKeyringForTest overrides the process-level HMAC keyring for unit testing (key rotation, missing key).
// Pass activeKID == "" and keys == nil to restore env-var resolution, or empty keys map to simulate missing keys.
func SetApprovalHMACKeyringForTest(activeKID string, keys map[string]string) {
	approvalKeyOverrideMu.Lock()
	defer approvalKeyOverrideMu.Unlock()
	if activeKID == "" && keys == nil {
		approvalKeyOverrideSet = false
		approvalKeyOverrideActiveID = ""
		approvalKeyOverrideRing = nil
		return
	}
	approvalKeyOverrideSet = true
	approvalKeyOverrideActiveID = strings.TrimSpace(activeKID)
	approvalKeyOverrideRing = make(map[string]string, len(keys))
	for k, v := range keys {
		approvalKeyOverrideRing[strings.TrimSpace(k)] = v
	}
}

func resolveApprovalHMACKeyring() (string, map[string]string, error) {
	approvalKeyOverrideMu.RLock()
	if approvalKeyOverrideSet {
		aid := approvalKeyOverrideActiveID
		ringCopy := make(map[string]string, len(approvalKeyOverrideRing))
		for k, v := range approvalKeyOverrideRing {
			ringCopy[k] = v
		}
		approvalKeyOverrideMu.RUnlock()
		if aid == "" || len(ringCopy) == 0 {
			return "", nil, ErrApprovalKeyMissing
		}
		return aid, ringCopy, nil
	}
	approvalKeyOverrideMu.RUnlock()

	ring := make(map[string]string)
	activeKID := strings.TrimSpace(os.Getenv("OPS_APPROVAL_HMAC_KEY_ID"))
	if activeKID == "" {
		activeKID = "k1"
	}

	// Parse multi-key keyring OPS_APPROVAL_HMAC_KEYS="k2:secret2,k1:secret1"
	if rawKeys, hasKeys := os.LookupEnv("OPS_APPROVAL_HMAC_KEYS"); hasKeys {
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

	rawPrimary, primaryEnvSet := os.LookupEnv("OPS_APPROVAL_HMAC_KEY")
	primary := strings.TrimSpace(rawPrimary)
	if primaryEnvSet {
		if primary == "" {
			return "", nil, ErrApprovalKeyMissing
		}
		if len(primary) < MinApprovalHMACKeyBytes {
			return "", nil, fmt.Errorf("%w: got %d bytes", ErrApprovalKeyWeak, len(primary))
		}
		ring[activeKID] = primary
	}

	if _, ok := ring[activeKID]; ok {
		for kid, sec := range ring {
			if len(sec) < MinApprovalHMACKeyBytes {
				return "", nil, fmt.Errorf("%w: key '%s' has %d bytes", ErrApprovalKeyWeak, kid, len(sec))
			}
		}
		return activeKID, ring, nil
	}

	// Only under explicit `go test` execution AND when OPS_APPROVAL_HMAC_KEY was not explicitly cleared in env,
	// generate a per-process ephemeral CSPRNG test key (never hardcoded) so legacy unit tests remain isolated.
	if testing.Testing() && !primaryEnvSet {
		ephemeralTestKeyOnce.Do(func() {
			buf := make([]byte, 32)
			_, _ = rand.Read(buf)
			ephemeralTestKey = hex.EncodeToString(buf)
		})
		ring[activeKID] = ephemeralTestKey
		return activeKID, ring, nil
	}

	return "", nil, ErrApprovalKeyMissing
}

// ValidateApprovalHMACKeyBootGuard verifies that an external non-DB HMAC signing key is configured in production
// and rejects common weak/placeholder strings.
func ValidateApprovalHMACKeyBootGuard(isProduction bool) error {
	if !isProduction {
		return nil
	}
	activeKID, ring, err := resolveApprovalHMACKeyring()
	if err != nil {
		return err
	}
	for kid, sec := range ring {
		if len(sec) < MinApprovalHMACKeyBytes {
			return fmt.Errorf("%w: key '%s'", ErrApprovalKeyWeak, kid)
		}
		lower := strings.ToLower(strings.TrimSpace(sec))
		for _, weakSub := range []string{"change-me", "placeholder", "your-secret", "example-key", "replace-this"} {
			if strings.Contains(lower, weakSub) {
				return fmt.Errorf("%w: key '%s' uses weak placeholder pattern", ErrApprovalKeyWeak, kid)
			}
		}
	}
	_ = activeKID
	return nil
}

// CanonicalRollbackSpec is the normalized representation of rollback specification fields.
type CanonicalRollbackSpec struct {
	Action          string      `json:"action"`
	Target          string      `json:"target"`
	Params          interface{} `json:"params"`
	TimeoutSec      int         `json:"timeout_sec"`
	ExecutionPolicy string      `json:"execution_policy"`
}

// CanonicalApprovalPayload is the complete normalized representation of proposal attributes
// cryptographically signed via HMAC-SHA256 upon administrator approval (R4-11, R4-12).
type CanonicalApprovalPayload struct {
	ProposalID      string                 `json:"proposal_id"`
	HouseholdID     uint                   `json:"household_id"`
	TargetComponent string                 `json:"target_component"`
	ExecutionAction string                 `json:"execution_action"`
	Parameters      interface{}            `json:"parameters"`
	ExecutionPlan   string                 `json:"execution_plan"`
	RollbackPlan    string                 `json:"rollback_plan"`
	RollbackSpec    *CanonicalRollbackSpec `json:"rollback_spec,omitempty"`
	TimeoutSec      int                    `json:"timeout_sec"`
	ExecutionPolicy string                 `json:"execution_policy"`
	ScheduledAt     string                 `json:"scheduled_at"`
	BaselineHash    string                 `json:"baseline_hash"`
	ApprovedBy      string                 `json:"approved_by"`
	ApprovedAtSec   int64                  `json:"approved_at_sec"`
	Version         int                    `json:"version"`
}

type strictRollbackSpecInput struct {
	Action          string                 `json:"action"`
	Target          string                 `json:"target"`
	Params          map[string]interface{} `json:"params,omitempty"`
	BaselineHash    string                 `json:"baseline_hash,omitempty"`
	RollbackSpec    *strictRollbackSpecInput `json:"rollback_spec,omitempty"`
	TimeoutSec      int                    `json:"timeout_sec,omitempty"`
	ExecutionPolicy string                 `json:"execution_policy,omitempty"`
}

// CanonicalizeJSONValue recursively normalizes maps (sorted keys), slices, and json.Numbers
// to guarantee identical canonical serialization across Go and Python.
func CanonicalizeJSONValue(v interface{}) interface{} {
	if v == nil {
		return nil
	}
	switch typed := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(typed))
		for k := range typed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]interface{}, len(typed))
		for _, k := range keys {
			out[k] = CanonicalizeJSONValue(typed[k])
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(typed))
		for i, elem := range typed {
			out[i] = CanonicalizeJSONValue(elem)
		}
		return out
	case json.Number:
		s := typed.String()
		if !strings.ContainsAny(s, ".eE") {
			if iv, err := strconv.ParseInt(s, 10, 64); err == nil {
				return iv
			}
		}
		if fv, err := strconv.ParseFloat(s, 64); err == nil {
			if float64(int64(fv)) == fv && !strings.Contains(s, ".") {
				return int64(fv)
			}
			return fv
		}
		return s
	case float64:
		if float64(int64(typed)) == typed {
			return int64(typed)
		}
		return typed
	default:
		return typed
	}
}

func decodeStrictJSON(raw string, dst interface{}) error {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return fmt.Errorf("%w: %v", ErrApprovalUnknownField, err)
		}
		return err
	}
	// Reject trailing garbage after JSON object
	if dec.More() {
		return errors.New("unexpected trailing tokens in action_spec JSON")
	}
	var extra interface{}
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("unexpected extra JSON value in action_spec")
	}
	return nil
}

// CanonicalizeComponent normalizes known component names and aliases.
// Unknown components return "" (Fail-Closed) to prevent whitelist bypasses (R4-17).
func CanonicalizeComponent(name string) string {
	clean := strings.TrimSpace(name)
	clean = strings.TrimPrefix(clean, "/")
	if idx := strings.Index(clean, "("); idx != -1 {
		prefix := strings.TrimSpace(clean[:idx])
		if prefix != "" {
			clean = prefix
		}
	}
	clean = strings.ToLower(clean)
	switch clean {
	case "home-frigate", "frigate", "nvr", "go2rtc", "home-datacenter-frigate":
		return "frigate"
	case "home-mosquitto", "mosquitto", "mqtt", "broker", "mosquitto-broker", "home-datacenter-mosquitto":
		return "mosquitto"
	case "home-api", "api", "backend", "go-api", "home-datacenter-api":
		return "home-api"
	case "home-web", "web", "frontend", "nginx", "home-datacenter-web":
		return "home-web"
	case "home-vision", "vision", "ai-vision", "home-datacenter-vision":
		return "home-vision"
	case "openresty", "1panel-openresty", "nginx-proxy", "1panel-openresty-wudu", "home-gateway", "gateway", "home-datacenter-gateway":
		return "openresty"
	case "1pwaf", "waf", "openresty-waf":
		return "1pwaf"
	case "app.db", "database", "sqlite", "db":
		return "app.db"
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

// ComputeApprovalDigest computes a deterministic HMAC-SHA256 digest over canonical JSON
// when called via legacy 5-argument helper (for targeted unit tests).
func ComputeApprovalDigest(target string, action string, params map[string]interface{}, baseline string, version int) string {
	if params == nil {
		params = make(map[string]interface{})
	}
	if version <= 0 {
		version = 1
	}
	activeKID, ring, err := resolveApprovalHMACKeyring()
	if err != nil {
		return ""
	}
	canonicalTarget := CanonicalizeComponent(target)
	if canonicalTarget == "" {
		return ""
	}
	payload := CanonicalApprovalPayload{
		TargetComponent: canonicalTarget,
		ExecutionAction: strings.ToLower(strings.TrimSpace(action)),
		Parameters:      CanonicalizeJSONValue(params),
		BaselineHash:    strings.TrimSpace(baseline),
		Version:         version,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(ring[activeKID]))
	_, _ = mac.Write(data)
	return fmt.Sprintf("%s%s:%x", ApprovalDigestPrefixHMAC, activeKID, mac.Sum(nil))
}

func buildCanonicalPayloadFromProposal(p *OpsProposal) ([]byte, error) {
	if p == nil {
		return nil, errors.New("proposal is nil")
	}
	canonicalPropTarget := CanonicalizeComponent(p.TargetComponent)
	if canonicalPropTarget == "" {
		return nil, fmt.Errorf("unknown or disallowed target component '%s'", p.TargetComponent)
	}
	target := canonicalPropTarget
	action := ""
	params := make(map[string]interface{})
	var timeoutSec int
	execPolicy := strings.TrimSpace(p.ExecutionPolicy)
	var rbSpec *CanonicalRollbackSpec

	if strings.TrimSpace(p.ActionSpec) != "" {
		var spec strictRollbackSpecInput
		if err := decodeStrictJSON(p.ActionSpec, &spec); err != nil {
			return nil, fmt.Errorf("invalid action_spec: %w", err)
		}
		if spec.Action != "" {
			action = spec.Action
		}
		if spec.Target != "" {
			specTarget := CanonicalizeComponent(spec.Target)
			if specTarget == "" || specTarget != canonicalPropTarget {
				return nil, fmt.Errorf("target component mismatch: proposal target '%s' does not match action_spec target '%s'", p.TargetComponent, spec.Target)
			}
			target = specTarget
		}
		if spec.Params != nil {
			params = spec.Params
		}
		timeoutSec = spec.TimeoutSec
		if strings.TrimSpace(spec.ExecutionPolicy) != "" {
			if execPolicy != "" && execPolicy != strings.TrimSpace(spec.ExecutionPolicy) {
				return nil, errors.New("execution_policy mismatch between proposal and action_spec")
			}
			execPolicy = strings.TrimSpace(spec.ExecutionPolicy)
		}
		if spec.RollbackSpec != nil {
			rbTarget := canonicalPropTarget
			if spec.RollbackSpec.Target != "" {
				resolvedRB := CanonicalizeComponent(spec.RollbackSpec.Target)
				if resolvedRB == "" || resolvedRB != canonicalPropTarget {
					return nil, fmt.Errorf("rollback target mismatch: '%s' != '%s'", spec.RollbackSpec.Target, canonicalPropTarget)
				}
				rbTarget = resolvedRB
			}
			rbParams := spec.RollbackSpec.Params
			if rbParams == nil {
				rbParams = make(map[string]interface{})
			}
			rbSpec = &CanonicalRollbackSpec{
				Action:          strings.ToLower(strings.TrimSpace(spec.RollbackSpec.Action)),
				Target:          rbTarget,
				Params:          CanonicalizeJSONValue(rbParams),
				TimeoutSec:      spec.RollbackSpec.TimeoutSec,
				ExecutionPolicy: strings.TrimSpace(spec.RollbackSpec.ExecutionPolicy),
			}
		}
	}

	if action == "" {
		planLower := strings.ToLower(p.ExecutionPlan)
		switch {
		case strings.Contains(planLower, "restart"):
			action = "docker.restart"
		case strings.Contains(planLower, "update") || strings.Contains(planLower, "recreate") || strings.Contains(planLower, "pull"):
			action = "docker.recreate"
		case strings.Contains(planLower, "create index"):
			action = "db.create_index"
		case strings.Contains(planLower, "drop index"):
			action = "db.drop_index"
		case strings.Contains(planLower, "reload"):
			action = "waf.reload"
		case strings.Contains(planLower, "cache") || strings.Contains(planLower, "purge"):
			action = "cache.purge"
		default:
			action = strings.TrimSpace(p.ExecutionPlan)
		}
	}

	version := p.Version
	if version <= 0 {
		version = 1
	}

	var approvedAtSec int64
	if p.ApprovedAt.Valid {
		approvedAtSec = p.ApprovedAt.Time.UTC().Unix()
	}
	schedAt := ""
	if p.ScheduledAt != nil {
		schedAt = strings.TrimSpace(*p.ScheduledAt)
	}

	householdID := p.HouseholdID
	if householdID == 0 {
		householdID = 1
	}

	payload := CanonicalApprovalPayload{
		ProposalID:      strings.TrimSpace(p.ProposalID),
		HouseholdID:     householdID,
		TargetComponent: target,
		ExecutionAction: strings.ToLower(strings.TrimSpace(action)),
		Parameters:      CanonicalizeJSONValue(params),
		ExecutionPlan:   strings.TrimSpace(p.ExecutionPlan),
		RollbackPlan:    strings.TrimSpace(p.RollbackPlan),
		RollbackSpec:    rbSpec,
		TimeoutSec:      timeoutSec,
		ExecutionPolicy: execPolicy,
		ScheduledAt:     schedAt,
		BaselineHash:    strings.TrimSpace(p.BaselineHash),
		ApprovedBy:      strings.TrimSpace(p.ApprovedBy),
		ApprovedAtSec:   approvedAtSec,
		Version:         version,
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

// ComputeProposalApprovalDigest generates an HMAC-SHA256 signature over the complete canonical proposal spec.
func ComputeProposalApprovalDigest(p *OpsProposal) (string, error) {
	activeKID, ring, err := resolveApprovalHMACKeyring()
	if err != nil {
		return "", err
	}
	data, err := buildCanonicalPayloadFromProposal(p)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, []byte(ring[activeKID]))
	_, _ = mac.Write(data)
	return fmt.Sprintf("%s%s:%x", ApprovalDigestPrefixHMAC, activeKID, mac.Sum(nil)), nil
}

// VerifyProposalApprovalDigest validates the HMAC-SHA256 approval signature using the keyring <kid>
// and constant-time comparison, strictly rejecting legacy unkeyed sha256 digests or missing keys (R4-12).
func VerifyProposalApprovalDigest(p *OpsProposal) error {
	if p == nil {
		return errors.New("proposal is nil")
	}
	digest := strings.TrimSpace(p.ApprovalDigest)
	if digest == "" {
		return errors.New("proposal lacks approval digest")
	}
	if strings.HasPrefix(digest, "sha256:") {
		return ErrApprovalSignatureDowngrade
	}
	if !strings.HasPrefix(digest, ApprovalDigestPrefixHMAC) {
		return errors.New("unrecognized approval digest format")
	}
	rest := strings.TrimPrefix(digest, ApprovalDigestPrefixHMAC)
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || len(parts[1]) != 64 {
		return errors.New("malformed hmac approval digest structure")
	}
	kid := strings.TrimSpace(parts[0])
	providedHex := strings.ToLower(strings.TrimSpace(parts[1]))
	providedMAC, err := hex.DecodeString(providedHex)
	if err != nil || len(providedMAC) != sha256.Size {
		return errors.New("invalid hmac digest hex encoding")
	}

	_, ring, err := resolveApprovalHMACKeyring()
	if err != nil {
		return err
	}
	secret, ok := ring[kid]
	if !ok || len(secret) < MinApprovalHMACKeyBytes {
		return fmt.Errorf("unknown or unavailable HMAC key id '%s'", kid)
	}

	data, err := buildCanonicalPayloadFromProposal(p)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(data)
	expectedMAC := mac.Sum(nil)

	if subtle.ConstantTimeCompare(providedMAC, expectedMAC) != 1 {
		return errors.New("approval HMAC signature verification failed")
	}
	return nil
}

// ComputeDefaultBaseline computes fallback baseline matching HostFallbackProvider.
func ComputeDefaultBaseline(component string) string {
	canonical := CanonicalizeComponent(component)
	if canonical == "" {
		return ""
	}
	containerName := canonical
	switch canonical {
	case "frigate":
		containerName = "home-frigate"
	case "mosquitto":
		containerName = "home-mosquitto"
	case "openresty", "1pwaf":
		containerName = "1Panel-openresty-WuDU"
	}
	hasher := sha256.New()
	raw := fmt.Sprintf("%s:channels-tool/%s:latest:running:", canonical, containerName)
	hasher.Write([]byte(raw))
	return fmt.Sprintf("sha256:%x", hasher.Sum(nil))
}

// BeforeCreate initializes monotonic version/revision and blocks unapproved production persistence bypasses.
func (p *OpsProposal) BeforeCreate(tx *gorm.DB) error {
	if p.Version <= 0 {
		p.Version = 1
	}
	if p.Revision <= 0 {
		p.Revision = 1
	}
	if !testing.Testing() && (p.Status == ProposalStatusApproved || p.Status == ProposalStatusScheduled) {
		if strings.TrimSpace(p.ApprovalDigest) == "" {
			return errors.New("security violation: cannot create pre-approved/scheduled proposal without HMAC approval digest")
		}
		if err := VerifyProposalApprovalDigest(p); err != nil {
			return fmt.Errorf("security violation: pre-approved proposal failed HMAC verification: %w", err)
		}
	}
	return nil
}


// MarshalJSON customizes JSON serialization so ID can map to proposal_id for frontend compatibility.
func (p OpsProposal) MarshalJSON() ([]byte, error) {
	idVal := any(p.ID)
	if p.ProposalID != "" {
		idVal = p.ProposalID
	} else if p.ID > 0 {
		idVal = fmt.Sprintf("prop-%d", p.ID)
	}

	type Alias OpsProposal
	return json.Marshal(&struct {
		ID any `json:"id"`
		Alias
	}{
		ID:    idVal,
		Alias: (Alias)(p),
	})
}

// UnmarshalJSON supports deserializing proposals whether ID is numeric or string formatted,
// and parses nullable timestamps for approved_at and executed_at.
func (p *OpsProposal) UnmarshalJSON(data []byte) error {
	type Alias OpsProposal
	aux := struct {
		RawID       any `json:"id"`
		ApprovedAt  any `json:"approved_at"`
		ExecutedAt  any `json:"executed_at"`
		*Alias
	}{
		Alias: (*Alias)(p),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	switch v := aux.RawID.(type) {
	case float64:
		p.ID = uint(v)
	case string:
		if p.ProposalID == "" {
			p.ProposalID = v
		}
		var num uint
		if _, err := fmt.Sscanf(v, "prop-%d", &num); err == nil {
			p.ID = num
		} else if _, err := fmt.Sscanf(v, "%d", &num); err == nil {
			p.ID = num
		}
	}

	if s, ok := aux.ApprovedAt.(string); ok && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			p.ApprovedAt = utils.NullTime{Time: t, Valid: true}
		} else if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
			p.ApprovedAt = utils.NullTime{Time: t, Valid: true}
		}
	}

	if s, ok := aux.ExecutedAt.(string); ok && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			p.ExecutedAt = utils.NullTime{Time: t, Valid: true}
		} else if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
			p.ExecutedAt = utils.NullTime{Time: t, Valid: true}
		}
	}

	return nil
}

const (
	ChatScopePersonal  = "personal"
	ChatScopeHousehold = "household"
)

// OpsChatSession models a multi-turn chat session with Ops Copilot.
type OpsChatSession struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	SessionID string    `gorm:"size:64;uniqueIndex" json:"session_id"`
	UserID    uint      `gorm:"index;not null;default:0" json:"user_id"`
	Scope     string    `gorm:"size:32;not null;default:'personal'" json:"scope"` // "personal" | "household"
	Title     string    `gorm:"size:255" json:"title"`
	CreatedAt time.Time `gorm:"not null" json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (OpsChatSession) TableName() string { return "ops_chat_sessions" }

// OpsChatMessage models an individual message exchanged in an Ops Chat session.
type OpsChatMessage struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	MessageID string    `gorm:"size:64;index" json:"message_id"`
	SessionID string    `gorm:"size:64;index" json:"session_id"`
	UserID    uint      `gorm:"index;not null;default:0" json:"user_id"`
	Scope     string    `gorm:"size:32;not null;default:'personal'" json:"scope"` // "personal" | "household"
	Role      string    `gorm:"size:32;not null" json:"role"`                     // "user", "assistant", "system"
	Content   string    `gorm:"type:text;not null" json:"content"`
	Model     string    `gorm:"size:64" json:"model"`
	Timestamp int64     `json:"timestamp"`
	CreatedAt time.Time `gorm:"not null" json:"created_at"`
}

func (OpsChatMessage) TableName() string { return "ops_chat_messages" }

// OpsExecutionHistory models the historical processing record of autonomous agent operations,
// including component auto-updates, DB snapshot backups, health probe rollbacks, and vulnerability patching.
type OpsExecutionHistory struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	ExecutionType string    `gorm:"size:64;not null;index" json:"execution_type"` // "component_update", "vuln_patch", "auto_rollback", "proposal_exec"
	Component     string    `gorm:"size:128;not null" json:"component"`          // e.g. "home-api", "sqlite", "frigate"
	Action        string    `gorm:"size:255;not null" json:"action"`             // Human readable description
	OldVersion    string    `gorm:"size:64" json:"old_version"`
	NewVersion    string    `gorm:"size:64" json:"new_version"`
	Status        string    `gorm:"size:32;not null;index" json:"status"`         // "success", "rolled_back", "failed", "pending"
	SnapshotID    string    `gorm:"size:128" json:"snapshot_id"`                 // Backup snapshot file path or ID
	HealthOutput  string    `gorm:"type:text" json:"health_output"`
	Detail        string    `gorm:"type:text" json:"detail"`
	CreatedAt     time.Time `gorm:"not null;index" json:"created_at"`
}

func (OpsExecutionHistory) TableName() string { return "ops_execution_histories" }

// OpsMaintenanceLock models the persistent system-wide maintenance lock.
type OpsMaintenanceLock struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Active      bool      `gorm:"not null;default:false" json:"active"`
	LockedBy    string    `gorm:"size:64" json:"locked_by"`
	Reason      string    `gorm:"type:text" json:"reason"`
	LockedAt    time.Time `json:"locked_at"`
	ExpiresAt   time.Time `gorm:"index" json:"expires_at"`
	ExpiresAtMs int64     `gorm:"column:expires_at_ms;not null;default:0" json:"expires_at_ms"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (OpsMaintenanceLock) TableName() string { return "ops_maintenance_locks" }

// OpsCircuitBreakerState models the persistent container circuit breaker status and crash history.
type OpsCircuitBreakerState struct {
	ID                   uint      `gorm:"primaryKey" json:"id"`
	Container            string    `gorm:"size:64;uniqueIndex;not null" json:"container"`
	State                string    `gorm:"size:32;not null;default:'CLOSED'" json:"state"` // CLOSED, OPEN, HALF_OPEN
	OpenedAt             time.Time `json:"opened_at"`
	OpenedAtMs           int64     `gorm:"column:opened_at_ms;not null;default:0" json:"opened_at_ms"`
	CrashTimestamps      string    `gorm:"type:text" json:"crash_timestamps"` // JSON array of RFC3339 timestamps
	ActiveProbeToken     string    `gorm:"column:active_probe_token;size:128;not null;default:''" json:"active_probe_token,omitempty"`
	ActiveProbeIssuedMs  int64     `gorm:"column:active_probe_issued_ms;not null;default:0" json:"active_probe_issued_ms,omitempty"`
	ActiveProbeExpiresMs int64     `gorm:"column:active_probe_expires_ms;not null;default:0" json:"active_probe_expires_ms,omitempty"`
	Degraded             bool      `gorm:"column:degraded;not null;default:false" json:"degraded"`
	UpdatedAt            time.Time `json:"updated_at"`
	UpdatedAtMs          int64     `gorm:"column:updated_at_ms;not null;default:0" json:"updated_at_ms"`
}

func (OpsCircuitBreakerState) TableName() string { return "ops_circuit_breaker_states" }

