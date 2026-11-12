package hostsec

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	GrantAlgorithmHMACSHA256 = "hmac-sha256"
	GrantAlgorithmEd25519    = "ed25519"

	defaultHostGrantKeyID   = "grant-k1"
	defaultHostGrantTTL     = 5 * time.Minute
	maxHostGrantTTL         = 30 * time.Minute
	minHostGrantHMACKeySize = 32
	grantClockSkewTolerance = 30 * time.Second
)

var (
	ErrExecutionGrantRequired    = errors.New("independent host execution grant is required")
	ErrExecutionGrantInvalid     = errors.New("host execution grant signature or format invalid")
	ErrExecutionGrantExpired     = errors.New("host execution grant expired")
	ErrExecutionGrantRevoked     = errors.New("host execution grant has been revoked")
	ErrExecutionGrantConsumed    = errors.New("host execution grant already consumed")
	ErrExecutionGrantMismatch    = errors.New("host execution grant binding or scope mismatch")
	ErrExecutionGrantKeyConflict = errors.New("host execution grant key must not match bridge transport key")
)

// HostExecutionGrant is a short-lived, single-operation/single-use cryptographic capability
// minted by an approval grant authority whose signing key is independent from OPS_HOST_BRIDGE_HMAC_KEY.
type HostExecutionGrant struct {
	GrantID           string            `json:"grant_id"`
	KeyID             string            `json:"key_id"`
	Algorithm         string            `json:"algorithm"`
	ProposalID        string            `json:"proposal_id"`
	ApprovalID        string            `json:"approval_id"`
	OperationID       string            `json:"operation_id"`
	ResourceScope     string            `json:"resource_scope"`
	BaselineHash      string            `json:"baseline_hash"`
	FencingGeneration int64             `json:"fencing_generation"`
	Action            string            `json:"action"`
	Packages          []string          `json:"packages,omitempty"`
	TargetVersions    map[string]string `json:"target_versions,omitempty"`
	IssuedAtMs        int64             `json:"issued_at_ms"`
	ExpiresAtMs       int64             `json:"expires_at_ms"`
	Signature         string            `json:"signature"`
}

// HostExecutionGrantIssueRequest describes parameters for issuing a signed HostExecutionGrant.
type HostExecutionGrantIssueRequest struct {
	GrantID           string
	ProposalID        string
	ApprovalID        string
	OperationID       string
	ResourceScope     string
	BaselineHash      string
	FencingGeneration int64
	Action            string
	Packages          []string
	TargetVersions    map[string]string
	TTL               time.Duration
}

// HostGrantAuthority issues and/or verifies HostExecutionGrant credentials using an
// authority key strictly independent from the API container's bridge HMAC transport key.
type HostGrantAuthority struct {
	KeyID         string
	HMACKey       []byte
	PrivateKey    ed25519.PrivateKey
	PublicKey     ed25519.PublicKey
	BridgeHMACKey []byte
}

// HostGrantAuthorityConfig configures HostGrantAuthority.
type HostGrantAuthorityConfig struct {
	KeyID         string
	HMACKey       []byte
	PrivateKey    ed25519.PrivateKey
	PublicKey     ed25519.PublicKey
	BridgeHMACKey []byte
}

// NewHostGrantAuthority validates key material and enforces strict separation from BridgeHMACKey.
func NewHostGrantAuthority(cfg HostGrantAuthorityConfig) (*HostGrantAuthority, error) {
	hasHMAC := len(cfg.HMACKey) > 0
	hasPriv := len(cfg.PrivateKey) > 0
	hasPub := len(cfg.PublicKey) > 0
	if !hasHMAC && !hasPriv && !hasPub {
		return nil, fmt.Errorf("%w: grant HMAC or Ed25519 key material required", ErrExecutionGrantInvalid)
	}
	if hasHMAC {
		if len(cfg.HMACKey) < minHostGrantHMACKeySize {
			return nil, fmt.Errorf("%w: grant HMAC key must be at least %d bytes", ErrExecutionGrantInvalid, minHostGrantHMACKeySize)
		}
		if len(cfg.BridgeHMACKey) > 0 && hmac.Equal(cfg.HMACKey, cfg.BridgeHMACKey) {
			return nil, ErrExecutionGrantKeyConflict
		}
	}
	var pub ed25519.PublicKey
	if hasPriv {
		if len(cfg.PrivateKey) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("%w: invalid ed25519 private key length", ErrExecutionGrantInvalid)
		}
		derived, ok := cfg.PrivateKey.Public().(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("%w: failed to derive ed25519 public key", ErrExecutionGrantInvalid)
		}
		pub = derived
	}
	if hasPub {
		if len(cfg.PublicKey) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%w: invalid ed25519 public key length", ErrExecutionGrantInvalid)
		}
		if len(pub) > 0 && !hmac.Equal(pub, cfg.PublicKey) {
			return nil, fmt.Errorf("%w: ed25519 public/private key mismatch", ErrExecutionGrantInvalid)
		}
		pub = append(ed25519.PublicKey(nil), cfg.PublicKey...)
	}
	keyID := strings.TrimSpace(cfg.KeyID)
	if keyID == "" {
		keyID = defaultHostGrantKeyID
	}
	var hmacCopy []byte
	if hasHMAC {
		hmacCopy = append([]byte(nil), cfg.HMACKey...)
	}
	var privCopy ed25519.PrivateKey
	if hasPriv {
		privCopy = append(ed25519.PrivateKey(nil), cfg.PrivateKey...)
	}
	var bridgeCopy []byte
	if len(cfg.BridgeHMACKey) > 0 {
		bridgeCopy = append([]byte(nil), cfg.BridgeHMACKey...)
	}
	return &HostGrantAuthority{
		KeyID:         keyID,
		HMACKey:       hmacCopy,
		PrivateKey:    privCopy,
		PublicKey:     pub,
		BridgeHMACKey: bridgeCopy,
	}, nil
}

// NewHostGrantAuthorityFromEnv loads an independent grant signing/verification authority from environment.
// Returns nil if neither OPS_HOST_GRANT_HMAC_KEY nor Ed25519 grant key env vars are set.
func NewHostGrantAuthorityFromEnv() (*HostGrantAuthority, error) {
	hmacKey := []byte(strings.TrimSpace(os.Getenv("OPS_HOST_GRANT_HMAC_KEY")))
	pubHex := strings.TrimSpace(os.Getenv("OPS_HOST_GRANT_PUBLIC_KEY"))
	privHex := strings.TrimSpace(os.Getenv("OPS_HOST_GRANT_PRIVATE_KEY"))
	if len(hmacKey) == 0 && pubHex == "" && privHex == "" {
		return nil, nil
	}
	var pub ed25519.PublicKey
	var priv ed25519.PrivateKey
	if pubHex != "" {
		b, err := hex.DecodeString(pubHex)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid OPS_HOST_GRANT_PUBLIC_KEY hex: %v", ErrExecutionGrantInvalid, err)
		}
		pub = ed25519.PublicKey(b)
	}
	if privHex != "" {
		b, err := hex.DecodeString(privHex)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid OPS_HOST_GRANT_PRIVATE_KEY hex: %v", ErrExecutionGrantInvalid, err)
		}
		priv = ed25519.PrivateKey(b)
	}
	return NewHostGrantAuthority(HostGrantAuthorityConfig{
		KeyID:         strings.TrimSpace(os.Getenv("OPS_HOST_GRANT_KEY_ID")),
		HMACKey:       hmacKey,
		PrivateKey:    priv,
		PublicKey:     pub,
		BridgeHMACKey: []byte(strings.TrimSpace(os.Getenv("OPS_HOST_BRIDGE_HMAC_KEY"))),
	})
}

func resolveGrantResourceScope(scope string) string {
	s := strings.TrimSpace(scope)
	if s == "" {
		return defaultBridgeResourceScope
	}
	return s
}

func normalizeGrantPackages(pkgs []string) []string {
	if len(pkgs) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(pkgs))
	out := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		cp := strings.TrimSpace(p)
		if cp == "" || seen[cp] {
			continue
		}
		seen[cp] = true
		out = append(out, cp)
	}
	sort.Strings(out)
	return out
}

func (g *HostExecutionGrant) canonicalSignBytes() ([]byte, error) {
	if g == nil {
		return nil, ErrExecutionGrantRequired
	}
	pkgs := normalizeGrantPackages(g.Packages)
	tvKeys := make([]string, 0, len(g.TargetVersions))
	for k := range g.TargetVersions {
		tvKeys = append(tvKeys, k)
	}
	sort.Strings(tvKeys)
	type tvPair struct {
		Pkg string `json:"pkg"`
		Ver string `json:"ver"`
	}
	tvList := make([]tvPair, 0, len(tvKeys))
	for _, k := range tvKeys {
		tvList = append(tvList, tvPair{Pkg: k, Ver: g.TargetVersions[k]})
	}
	payload := struct {
		Domain            string   `json:"domain"`
		GrantID           string   `json:"grant_id"`
		KeyID             string   `json:"key_id"`
		Algorithm         string   `json:"algorithm"`
		ProposalID        string   `json:"proposal_id"`
		ApprovalID        string   `json:"approval_id"`
		OperationID       string   `json:"operation_id"`
		ResourceScope     string   `json:"resource_scope"`
		BaselineHash      string   `json:"baseline_hash"`
		FencingGeneration int64    `json:"fencing_generation"`
		Action            string   `json:"action"`
		Packages          []string `json:"packages,omitempty"`
		TargetVersions    []tvPair `json:"target_versions,omitempty"`
		IssuedAtMs        int64    `json:"issued_at_ms"`
		ExpiresAtMs       int64    `json:"expires_at_ms"`
	}{
		Domain:            "host-execution-grant-v1",
		GrantID:           strings.TrimSpace(g.GrantID),
		KeyID:             strings.TrimSpace(g.KeyID),
		Algorithm:         strings.TrimSpace(g.Algorithm),
		ProposalID:        strings.TrimSpace(g.ProposalID),
		ApprovalID:        strings.TrimSpace(g.ApprovalID),
		OperationID:       strings.TrimSpace(g.OperationID),
		ResourceScope:     resolveGrantResourceScope(g.ResourceScope),
		BaselineHash:      strings.TrimSpace(g.BaselineHash),
		FencingGeneration: g.FencingGeneration,
		Action:            strings.TrimSpace(g.Action),
		Packages:          pkgs,
		TargetVersions:    tvList,
		IssuedAtMs:        g.IssuedAtMs,
		ExpiresAtMs:       g.ExpiresAtMs,
	}
	return json.Marshal(payload)
}

// ValidateFormat checks structural integrity of all 6+ binding dimensions and timestamps.
func (g *HostExecutionGrant) ValidateFormat(requireFencing bool) error {
	if g == nil {
		return ErrExecutionGrantRequired
	}
	if !reSafeSnapshotID.MatchString(strings.TrimSpace(g.GrantID)) {
		return fmt.Errorf("%w: invalid grant_id %q", ErrExecutionGrantInvalid, g.GrantID)
	}
	if strings.TrimSpace(g.KeyID) == "" {
		return fmt.Errorf("%w: key_id is required", ErrExecutionGrantInvalid)
	}
	if g.Algorithm != GrantAlgorithmHMACSHA256 && g.Algorithm != GrantAlgorithmEd25519 {
		return fmt.Errorf("%w: unsupported algorithm %q", ErrExecutionGrantInvalid, g.Algorithm)
	}
	if strings.TrimSpace(g.ProposalID) == "" || strings.TrimSpace(g.ApprovalID) == "" || strings.TrimSpace(g.OperationID) == "" {
		return fmt.Errorf("%w: proposal_id, approval_id, and operation_id are required", ErrExecutionGrantInvalid)
	}
	base := strings.TrimSpace(g.BaselineHash)
	if !strings.HasPrefix(base, "host-v1:") {
		return fmt.Errorf("%w: valid host-v1 baseline_hash required", ErrExecutionGrantInvalid)
	}
	if g.FencingGeneration < 0 || (requireFencing && g.FencingGeneration <= 0) {
		return fmt.Errorf("%w: valid fencing_generation required", ErrExecutionGrantInvalid)
	}
	if strings.TrimSpace(g.Action) == "" {
		return fmt.Errorf("%w: action is required", ErrExecutionGrantInvalid)
	}
	if g.IssuedAtMs <= 0 || g.ExpiresAtMs <= g.IssuedAtMs {
		return fmt.Errorf("%w: invalid grant validity window", ErrExecutionGrantInvalid)
	}
	if strings.TrimSpace(g.Signature) == "" {
		return fmt.Errorf("%w: missing grant signature", ErrExecutionGrantInvalid)
	}
	return nil
}

// IssueGrant mints and signs a HostExecutionGrant using the authority's independent key.
func (a *HostGrantAuthority) IssueGrant(req HostExecutionGrantIssueRequest) (*HostExecutionGrant, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: grant authority not configured", ErrExecutionGrantRequired)
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = defaultHostGrantTTL
	}
	if ttl > maxHostGrantTTL {
		ttl = maxHostGrantTTL
	}
	grantID := strings.TrimSpace(req.GrantID)
	if grantID == "" {
		var rnd [12]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return nil, err
		}
		grantID = "grant-" + hex.EncodeToString(rnd[:])
	}
	now := time.Now().UTC()
	var tvCopy map[string]string
	if len(req.TargetVersions) > 0 {
		tvCopy = make(map[string]string, len(req.TargetVersions))
		for k, v := range req.TargetVersions {
			tvCopy[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	g := &HostExecutionGrant{
		GrantID:           grantID,
		KeyID:             a.KeyID,
		ProposalID:        strings.TrimSpace(req.ProposalID),
		ApprovalID:        strings.TrimSpace(req.ApprovalID),
		OperationID:       strings.TrimSpace(req.OperationID),
		ResourceScope:     resolveGrantResourceScope(req.ResourceScope),
		BaselineHash:      strings.TrimSpace(req.BaselineHash),
		FencingGeneration: req.FencingGeneration,
		Action:            strings.TrimSpace(req.Action),
		Packages:          normalizeGrantPackages(req.Packages),
		TargetVersions:    tvCopy,
		IssuedAtMs:        now.UnixMilli(),
		ExpiresAtMs:       now.Add(ttl).UnixMilli(),
	}
	switch {
	case len(a.PrivateKey) == ed25519.PrivateKeySize:
		g.Algorithm = GrantAlgorithmEd25519
	case len(a.HMACKey) >= minHostGrantHMACKeySize:
		if len(a.BridgeHMACKey) > 0 && hmac.Equal(a.HMACKey, a.BridgeHMACKey) {
			return nil, ErrExecutionGrantKeyConflict
		}
		g.Algorithm = GrantAlgorithmHMACSHA256
	default:
		return nil, fmt.Errorf("%w: grant authority lacks signing key", ErrExecutionGrantInvalid)
	}

	g.Signature = "pending"
	if err := g.ValidateFormat(req.FencingGeneration > 0); err != nil {
		return nil, err
	}
	canonical, err := g.canonicalSignBytes()
	if err != nil {
		return nil, err
	}
	switch g.Algorithm {
	case GrantAlgorithmEd25519:
		sig := ed25519.Sign(a.PrivateKey, canonical)
		g.Signature = hex.EncodeToString(sig)
	case GrantAlgorithmHMACSHA256:
		mac := hmac.New(sha256.New, a.HMACKey)
		_, _ = mac.Write(canonical)
		g.Signature = hex.EncodeToString(mac.Sum(nil))
	}
	if err := g.ValidateFormat(false); err != nil {
		return nil, err
	}
	return g, nil
}

// VerifyGrant validates grant signature, expiry, key separation, and binding against req.
func (a *HostGrantAuthority) VerifyGrant(
	g *HostExecutionGrant,
	req HostActionRequest,
	bridgeHMACKey []byte,
	requireFencing bool,
	now time.Time,
) error {
	if a == nil {
		return fmt.Errorf("%w: grant verifier not configured", ErrExecutionGrantRequired)
	}
	if g == nil {
		return ErrExecutionGrantRequired
	}
	if err := g.ValidateFormat(requireFencing); err != nil {
		return err
	}
	if len(a.HMACKey) > 0 && len(bridgeHMACKey) > 0 && hmac.Equal(a.HMACKey, bridgeHMACKey) {
		return ErrExecutionGrantKeyConflict
	}
	if a.KeyID != "" && g.KeyID != a.KeyID {
		return fmt.Errorf("%w: unexpected grant key_id %q", ErrExecutionGrantInvalid, g.KeyID)
	}
	sigRaw := strings.TrimPrefix(strings.TrimSpace(g.Signature), g.Algorithm+"=")
	sigBytes, err := hex.DecodeString(sigRaw)
	if err != nil || len(sigBytes) == 0 {
		return fmt.Errorf("%w: malformed grant signature encoding", ErrExecutionGrantInvalid)
	}
	canonical, err := g.canonicalSignBytes()
	if err != nil {
		return err
	}
	switch g.Algorithm {
	case GrantAlgorithmHMACSHA256:
		if len(a.HMACKey) < minHostGrantHMACKeySize {
			return fmt.Errorf("%w: HMAC verification key not configured", ErrExecutionGrantInvalid)
		}
		mac := hmac.New(sha256.New, a.HMACKey)
		_, _ = mac.Write(canonical)
		expected := mac.Sum(nil)
		if !hmac.Equal(sigBytes, expected) {
			return fmt.Errorf("%w: HMAC grant signature mismatch", ErrExecutionGrantInvalid)
		}
	case GrantAlgorithmEd25519:
		if len(a.PublicKey) != ed25519.PublicKeySize {
			return fmt.Errorf("%w: Ed25519 verification key not configured", ErrExecutionGrantInvalid)
		}
		if len(sigBytes) != ed25519.SignatureSize || !ed25519.Verify(a.PublicKey, canonical, sigBytes) {
			return fmt.Errorf("%w: Ed25519 grant signature mismatch", ErrExecutionGrantInvalid)
		}
	default:
		return fmt.Errorf("%w: unsupported algorithm %q", ErrExecutionGrantInvalid, g.Algorithm)
	}

	if now.IsZero() {
		now = time.Now().UTC()
	}
	nowMs := now.UTC().UnixMilli()
	if nowMs > g.ExpiresAtMs {
		return fmt.Errorf("%w: expired at %d (now %d)", ErrExecutionGrantExpired, g.ExpiresAtMs, nowMs)
	}
	if g.IssuedAtMs > nowMs+grantClockSkewTolerance.Milliseconds() {
		return fmt.Errorf("%w: grant issued_at in future", ErrExecutionGrantInvalid)
	}

	// Verify 6-dimension binding against request binding
	if g.ProposalID != strings.TrimSpace(req.Binding.ProposalID) {
		return fmt.Errorf("%w: proposal_id mismatch", ErrExecutionGrantMismatch)
	}
	if g.ApprovalID != strings.TrimSpace(req.Binding.ApprovalID) {
		return fmt.Errorf("%w: approval_id mismatch", ErrExecutionGrantMismatch)
	}
	if g.OperationID != strings.TrimSpace(req.Binding.OperationID) {
		return fmt.Errorf("%w: operation_id mismatch", ErrExecutionGrantMismatch)
	}
	if g.BaselineHash != strings.TrimSpace(req.Binding.BaselineHash) {
		return fmt.Errorf("%w: baseline_hash mismatch", ErrExecutionGrantMismatch)
	}
	if g.FencingGeneration != req.Binding.FencingGeneration {
		return fmt.Errorf("%w: fencing_generation mismatch", ErrExecutionGrantMismatch)
	}
	if resolveGrantResourceScope(g.ResourceScope) != resolveGrantResourceScope(req.Binding.ResourceScope) {
		return fmt.Errorf("%w: resource_scope mismatch", ErrExecutionGrantMismatch)
	}

	// Verify authorized action & packages scope
	if g.Action != ActionHostPackageUpgrade && g.Action != string(req.Action) {
		return fmt.Errorf("%w: grant action %q does not authorize %q", ErrExecutionGrantMismatch, g.Action, req.Action)
	}
	if !IsAllowedHostAction(req.Action) {
		return fmt.Errorf("%w: disallowed action %q", ErrExecutionGrantMismatch, req.Action)
	}
	if len(g.Packages) > 0 || req.Action == HostActionAptUpgrade || req.Action == HostActionAptDowngrade || req.Action == HostActionAptCachePolicy || req.Action == HostActionDpkgQuery {
		allowedSet := make(map[string]bool, len(g.Packages))
		for _, p := range g.Packages {
			allowedSet[strings.TrimSpace(p)] = true
		}
		for _, rp := range req.Packages {
			cp := strings.TrimSpace(rp)
			if !allowedSet[cp] {
				return fmt.Errorf("%w: package %q not in grant packages", ErrExecutionGrantMismatch, cp)
			}
		}
		if req.Action == HostActionAptUpgrade && len(g.TargetVersions) > 0 {
			for _, rp := range req.Packages {
				cp := strings.TrimSpace(rp)
				if pinned := strings.TrimSpace(g.TargetVersions[cp]); pinned != "" {
					if strings.TrimSpace(req.TargetVersions[cp]) != pinned {
						return fmt.Errorf("%w: target_version mismatch or missing pin for package %s", ErrExecutionGrantMismatch, cp)
					}
				}
			}
			for pkg, tv := range req.TargetVersions {
				cp := strings.TrimSpace(pkg)
				if !allowedSet[cp] || strings.TrimSpace(g.TargetVersions[cp]) != strings.TrimSpace(tv) {
					return fmt.Errorf("%w: target_version mismatch for package %s", ErrExecutionGrantMismatch, cp)
				}
			}
		}
	}
	return nil
}

type grantConsumeRecord struct {
	GrantID           string           `json:"grant_id"`
	Action            string           `json:"action"`
	ProposalID        string           `json:"proposal_id"`
	ApprovalID        string           `json:"approval_id"`
	OperationID       string           `json:"operation_id"`
	ResourceScope     string           `json:"resource_scope"`
	BaselineHash      string           `json:"baseline_hash"`
	FencingGeneration int64            `json:"fencing_generation"`
	ConsumedSteps     map[string]int64 `json:"consumed_steps"`
	UpdatedAtMs       int64            `json:"updated_at_ms"`
}

type grantRevocationRecord struct {
	GrantID     string `json:"grant_id"`
	Reason      string `json:"reason,omitempty"`
	RevokedAtMs int64  `json:"revoked_at_ms"`
}

// PersistentGrantStore manages fail-closed, cross-process locked grant consumption
// and revocation state on disk.
type PersistentGrantStore struct {
	GrantDir string
}

// NewPersistentGrantStore creates and verifies accessibility of GrantDir fail-closed.
func NewPersistentGrantStore(grantDir string) (*PersistentGrantStore, error) {
	cleanDir := filepath.Clean(strings.TrimSpace(grantDir))
	if cleanDir == "" || cleanDir == "." {
		return nil, fmt.Errorf("%w: grant_dir must not be empty", ErrExecutionGrantInvalid)
	}
	if err := os.MkdirAll(cleanDir, 0700); err != nil {
		return nil, fmt.Errorf("create grant_dir %q: %w", cleanDir, err)
	}
	unlock, err := acquireCrossProcessFileLock(filepath.Join(cleanDir, ".grant.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock grant_dir %q: %w", cleanDir, err)
	}
	uErr := unlock()
	if uErr != nil {
		return nil, uErr
	}
	return &PersistentGrantStore{GrantDir: cleanDir}, nil
}

func (s *PersistentGrantStore) lockPath() string {
	return filepath.Join(s.GrantDir, ".grant.lock")
}

func grantDiskKey(grantID string) (string, error) {
	id := strings.TrimSpace(grantID)
	if !reSafeSnapshotID.MatchString(id) {
		return "", fmt.Errorf("%w: unsafe grant_id %q", ErrExecutionGrantInvalid, grantID)
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:]), nil
}

// RevokeGrant persists a fail-closed revocation marker for grantID under cross-process lock.
func (s *PersistentGrantStore) RevokeGrant(grantID, reason string) error {
	if s == nil {
		return fmt.Errorf("%w: grant store not initialized", ErrExecutionGrantInvalid)
	}
	key, err := grantDiskKey(grantID)
	if err != nil {
		return err
	}
	unlock, lErr := acquireCrossProcessFileLock(s.lockPath())
	if lErr != nil {
		return lErr
	}
	var unlocked bool
	defer func() {
		if !unlocked {
			_ = unlock()
		}
	}()

	rec := grantRevocationRecord{
		GrantID:     strings.TrimSpace(grantID),
		Reason:      strings.TrimSpace(reason),
		RevokedAtMs: time.Now().UTC().UnixMilli(),
	}
	data, mErr := json.Marshal(rec)
	if mErr != nil {
		return mErr
	}
	if wErr := writeFileAtomicFailClosed(filepath.Join(s.GrantDir, key+".revoked"), data, 0600); wErr != nil {
		return wErr
	}
	unlocked = true
	return unlock()
}

// IsRevoked reports whether grantID has been revoked on disk (failing closed on I/O error).
func (s *PersistentGrantStore) IsRevoked(grantID string) (bool, error) {
	if s == nil {
		return false, nil
	}
	key, err := grantDiskKey(grantID)
	if err != nil {
		return false, err
	}
	unlock, lErr := acquireCrossProcessFileLock(s.lockPath())
	if lErr != nil {
		return false, lErr
	}
	defer func() { _ = unlock() }()

	_, statErr := os.Stat(filepath.Join(s.GrantDir, key+".revoked"))
	if statErr == nil {
		return true, nil
	}
	if os.IsNotExist(statErr) {
		return false, nil
	}
	return false, statErr
}

// CheckGrantUsable validates signature, revocation, expiry, and binding and ensures the
// step has not already been consumed on disk, without mutating consumption state yet.
func (s *PersistentGrantStore) CheckGrantUsable(
	authority *HostGrantAuthority,
	bridgeHMACKey []byte,
	g *HostExecutionGrant,
	req HostActionRequest,
	requireFencing bool,
	now time.Time,
) error {
	if s == nil {
		return fmt.Errorf("%w: persistent grant store unavailable", ErrExecutionGrantInvalid)
	}
	if err := authority.VerifyGrant(g, req, bridgeHMACKey, requireFencing, now); err != nil {
		return err
	}
	key, err := grantDiskKey(g.GrantID)
	if err != nil {
		return err
	}
	unlock, lErr := acquireCrossProcessFileLock(s.lockPath())
	if lErr != nil {
		return fmt.Errorf("acquire grant store lock: %w", lErr)
	}
	var unlocked bool
	defer func() {
		if !unlocked {
			_ = unlock()
		}
	}()

	revPath := filepath.Join(s.GrantDir, key+".revoked")
	if _, statErr := os.Stat(revPath); statErr == nil {
		return fmt.Errorf("%w: grant %s", ErrExecutionGrantRevoked, g.GrantID)
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("check grant revocation %s: %w", g.GrantID, statErr)
	}

	consumePath := filepath.Join(s.GrantDir, key+".consumed")
	raw, rErr := os.ReadFile(consumePath)
	if rErr == nil {
		var rec grantConsumeRecord
		if uErr := json.Unmarshal(raw, &rec); uErr != nil {
			return fmt.Errorf("corrupted grant consumption record for %s: %w", g.GrantID, uErr)
		}
		if g.Action != ActionHostPackageUpgrade {
			return fmt.Errorf("%w: single-use grant %s already consumed", ErrExecutionGrantConsumed, g.GrantID)
		}
		if rec.OperationID != g.OperationID ||
			rec.ProposalID != g.ProposalID ||
			rec.ApprovalID != g.ApprovalID ||
			rec.BaselineHash != g.BaselineHash ||
			rec.FencingGeneration != g.FencingGeneration {
			return fmt.Errorf("%w: grant %s cannot be reused across operations", ErrExecutionGrantConsumed, g.GrantID)
		}
		stepKey := grantStepKey(g.Action, req)
		if _, stepUsed := rec.ConsumedSteps[stepKey]; stepUsed {
			return fmt.Errorf("%w: grant %s step %s already consumed", ErrExecutionGrantConsumed, g.GrantID, stepKey)
		}
	} else if !os.IsNotExist(rErr) {
		return fmt.Errorf("read grant consumption record %s: %w", g.GrantID, rErr)
	}
	unlocked = true
	if uErr := unlock(); uErr != nil {
		return fmt.Errorf("release grant store lock: %w", uErr)
	}
	return nil
}

// VerifyAndConsume verifies grant cryptographically, enforces non-revocation, and atomically
// records single-use consumption on disk under a cross-process file lock.
func (s *PersistentGrantStore) VerifyAndConsume(
	authority *HostGrantAuthority,
	bridgeHMACKey []byte,
	g *HostExecutionGrant,
	req HostActionRequest,
	requireFencing bool,
	now time.Time,
) error {
	if s == nil {
		return fmt.Errorf("%w: persistent grant store unavailable", ErrExecutionGrantInvalid)
	}
	if err := authority.VerifyGrant(g, req, bridgeHMACKey, requireFencing, now); err != nil {
		return err
	}
	key, err := grantDiskKey(g.GrantID)
	if err != nil {
		return err
	}

	unlock, lErr := acquireCrossProcessFileLock(s.lockPath())
	if lErr != nil {
		return fmt.Errorf("acquire grant store lock: %w", lErr)
	}
	var unlocked bool
	defer func() {
		if !unlocked {
			_ = unlock()
		}
	}()

	// 1. Check persistent revocation marker (fail-closed on non-IsNotExist stat error)
	revPath := filepath.Join(s.GrantDir, key+".revoked")
	if _, statErr := os.Stat(revPath); statErr == nil {
		return fmt.Errorf("%w: grant %s", ErrExecutionGrantRevoked, g.GrantID)
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("check grant revocation %s: %w", g.GrantID, statErr)
	}

	// 2. Load existing consumption state (fail-closed on read or JSON parse error)
	consumePath := filepath.Join(s.GrantDir, key+".consumed")
	var rec grantConsumeRecord
	exists := false
	raw, rErr := os.ReadFile(consumePath)
	if rErr == nil {
		exists = true
		if uErr := json.Unmarshal(raw, &rec); uErr != nil {
			return fmt.Errorf("corrupted grant consumption record for %s: %w", g.GrantID, uErr)
		}
	} else if !os.IsNotExist(rErr) {
		return fmt.Errorf("read grant consumption record %s: %w", g.GrantID, rErr)
	}

	if now.IsZero() {
		now = time.Now().UTC()
	}
	nowMs := now.UTC().UnixMilli()
	stepKey := grantStepKey(g.Action, req)

	if exists {
		// Single-action grants are strictly 1-RPC only; replays are rejected immediately.
		if g.Action != ActionHostPackageUpgrade {
			return fmt.Errorf("%w: single-use grant %s already consumed", ErrExecutionGrantConsumed, g.GrantID)
		}
		if rec.OperationID != g.OperationID ||
			rec.ProposalID != g.ProposalID ||
			rec.ApprovalID != g.ApprovalID ||
			rec.BaselineHash != g.BaselineHash ||
			rec.FencingGeneration != g.FencingGeneration {
			return fmt.Errorf("%w: grant %s cannot be reused across operations", ErrExecutionGrantConsumed, g.GrantID)
		}
		if _, stepUsed := rec.ConsumedSteps[stepKey]; stepUsed {
			return fmt.Errorf("%w: grant %s step %s already consumed", ErrExecutionGrantConsumed, g.GrantID, stepKey)
		}
	} else {
		rec = grantConsumeRecord{
			GrantID:           g.GrantID,
			Action:            g.Action,
			ProposalID:        g.ProposalID,
			ApprovalID:        g.ApprovalID,
			OperationID:       g.OperationID,
			ResourceScope:     resolveGrantResourceScope(g.ResourceScope),
			BaselineHash:      g.BaselineHash,
			FencingGeneration: g.FencingGeneration,
			ConsumedSteps:     make(map[string]int64),
		}
	}

	rec.ConsumedSteps[stepKey] = nowMs
	rec.UpdatedAtMs = nowMs
	encoded, mErr := json.Marshal(rec)
	if mErr != nil {
		return mErr
	}
	if wErr := writeFileAtomicFailClosed(consumePath, encoded, 0600); wErr != nil {
		return fmt.Errorf("persist grant consumption %s: %w", g.GrantID, wErr)
	}
	if uErr := unlock(); uErr != nil {
		unlocked = true
		return fmt.Errorf("release grant store lock: %w", uErr)
	}
	unlocked = true
	return nil
}

func grantStepKey(grantAction string, req HostActionRequest) string {
	if grantAction != ActionHostPackageUpgrade {
		return string(req.Action)
	}
	switch req.Action {
	case HostActionAptCachePolicy, HostActionDpkgQuery:
		pkgs := normalizeGrantPackages(req.Packages)
		return string(req.Action) + ":" + strings.Join(pkgs, ",")
	default:
		return string(req.Action)
	}
}
