package audit

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"

	"home-datacenter-api/internal/model"
	"home-datacenter-api/internal/redact"
)

// ExternalCheckpointSigner produces an asymmetric signature over a checkpoint so local DB/HMAC
// compromise on fnOS cannot forge externally anchored checkpoints.
type ExternalCheckpointSigner interface {
	Algorithm() string
	KeyID() string
	PublicKeyHex() string
	SignCheckpoint(cp *model.AgentAuditCheckpoint) (string, error)
}

// ExternalCheckpointVerifier verifies asymmetric checkpoint signatures using only public key material.
type ExternalCheckpointVerifier interface {
	Algorithm() string
	KeyID() string
	PublicKeyHex() string
	VerifyCheckpointExternal(cp *model.AgentAuditCheckpoint) error
}

// CheckpointExternalPayload returns the deterministic canonical byte payload signed by external asymmetric signers.
func CheckpointExternalPayload(cp *model.AgentAuditCheckpoint) []byte {
	if cp == nil {
		return nil
	}
	return []byte(fmt.Sprintf(
		"audit-cp-v1|%s|%s|%d|%d|%d|%s|%s|%s|%s|%d",
		cp.CheckpointID, cp.StreamID, cp.FromSeq, cp.ToSeq, cp.EventCount,
		cp.HeadEntryHash, cp.TailEntryHash, cp.ChainDigest, cp.PrevChainDigest, cp.CreatedAtMs,
	))
}

// Ed25519CheckpointKey implements ExternalCheckpointSigner (when priv is non-nil) and ExternalCheckpointVerifier.
type Ed25519CheckpointKey struct {
	kid  string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

// NewEd25519CheckpointSigner creates a signer+verifier pair from an Ed25519 private key.
func NewEd25519CheckpointSigner(kid string, priv ed25519.PrivateKey) (*Ed25519CheckpointKey, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid ed25519 private key length %d", len(priv))
	}
	if strings.TrimSpace(kid) == "" {
		kid = "ed25519-k1"
	}
	pub := priv.Public().(ed25519.PublicKey)
	return &Ed25519CheckpointKey{
		kid:  strings.TrimSpace(kid),
		pub:  pub,
		priv: append(ed25519.PrivateKey(nil), priv...),
	}, nil
}

// NewEd25519CheckpointVerifier creates a public-key-only verifier (safe to distribute without private key).
func NewEd25519CheckpointVerifier(kid string, pub ed25519.PublicKey) (*Ed25519CheckpointKey, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid ed25519 public key length %d", len(pub))
	}
	if strings.TrimSpace(kid) == "" {
		kid = "ed25519-k1"
	}
	return &Ed25519CheckpointKey{
		kid: strings.TrimSpace(kid),
		pub: append(ed25519.PublicKey(nil), pub...),
	}, nil
}

func (k *Ed25519CheckpointKey) Algorithm() string { return "ed25519" }
func (k *Ed25519CheckpointKey) KeyID() string     { return k.kid }
func (k *Ed25519CheckpointKey) PublicKeyHex() string {
	if len(k.pub) == 0 {
		return ""
	}
	return hex.EncodeToString(k.pub)
}

func (k *Ed25519CheckpointKey) SignCheckpoint(cp *model.AgentAuditCheckpoint) (string, error) {
	if k == nil || len(k.priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("ed25519 private key unavailable")
	}
	if cp == nil {
		return "", fmt.Errorf("nil checkpoint")
	}
	sig := ed25519.Sign(k.priv, CheckpointExternalPayload(cp))
	return fmt.Sprintf("ed25519:%s:%s", k.kid, hex.EncodeToString(sig)), nil
}

func (k *Ed25519CheckpointKey) VerifyCheckpointExternal(cp *model.AgentAuditCheckpoint) error {
	if k == nil || len(k.pub) != ed25519.PublicKeySize {
		return fmt.Errorf("ed25519 public key unavailable")
	}
	if cp == nil {
		return fmt.Errorf("nil checkpoint")
	}
	rawSig := strings.TrimSpace(cp.ExternalSig)
	if rawSig == "" {
		return fmt.Errorf("checkpoint %s missing required external asymmetric signature", cp.CheckpointID)
	}
	parts := strings.SplitN(rawSig, ":", 3)
	if len(parts) != 3 || parts[0] != "ed25519" || parts[1] == "" || parts[2] == "" {
		return fmt.Errorf("malformed external signature on checkpoint %s", cp.CheckpointID)
	}
	if cp.ExternalKeyID != "" && cp.ExternalKeyID != parts[1] {
		return fmt.Errorf("checkpoint %s external_key_id %q mismatches signature kid %q", cp.CheckpointID, cp.ExternalKeyID, parts[1])
	}
	if k.kid != "" && parts[1] != k.kid {
		return fmt.Errorf("checkpoint %s signed by unexpected external key_id %q (expected %q)", cp.CheckpointID, parts[1], k.kid)
	}
	sigBytes, err := hex.DecodeString(parts[2])
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return fmt.Errorf("invalid ed25519 signature bytes on checkpoint %s", cp.CheckpointID)
	}
	if !ed25519.Verify(k.pub, CheckpointExternalPayload(cp), sigBytes) {
		return fmt.Errorf("ed25519 signature mismatch on checkpoint %s", cp.CheckpointID)
	}
	return nil
}

func decodeHexOrBase64Bytes(raw string) ([]byte, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("empty key material")
	}
	if b, err := hex.DecodeString(trimmed); err == nil {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(trimmed); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(trimmed); err == nil {
		return b, nil
	}
	return nil, fmt.Errorf("key material must be valid hex or base64")
}

var (
	extKeyOverrideMu       sync.RWMutex
	extKeyOverrideSet      bool
	extKeyOverrideSigner   ExternalCheckpointSigner
	extKeyOverrideVerifier ExternalCheckpointVerifier
)

// SetExternalCheckpointKeysForTest overrides external asymmetric checkpoint signing/verification keys in tests.
func SetExternalCheckpointKeysForTest(signer ExternalCheckpointSigner, verifier ExternalCheckpointVerifier) func() {
	extKeyOverrideMu.Lock()
	extKeyOverrideSet = true
	extKeyOverrideSigner = signer
	extKeyOverrideVerifier = verifier
	extKeyOverrideMu.Unlock()

	return func() {
		extKeyOverrideMu.Lock()
		extKeyOverrideSet = false
		extKeyOverrideSigner = nil
		extKeyOverrideVerifier = nil
		extKeyOverrideMu.Unlock()
	}
}

// ResolveExternalCheckpointKeys loads optional Ed25519 asymmetric checkpoint keys from environment.
// If only public key is provided, verifier is returned with nil signer (verify-only mode).
func ResolveExternalCheckpointKeys() (ExternalCheckpointSigner, ExternalCheckpointVerifier, error) {
	extKeyOverrideMu.RLock()
	if extKeyOverrideSet {
		s, v := extKeyOverrideSigner, extKeyOverrideVerifier
		extKeyOverrideMu.RUnlock()
		return s, v, nil
	}
	extKeyOverrideMu.RUnlock()

	kid := strings.TrimSpace(os.Getenv("OPS_AUDIT_CHECKPOINT_ED25519_KEY_ID"))
	if kid == "" {
		kid = "ed25519-k1"
	}

	rawPriv := strings.TrimSpace(os.Getenv("OPS_AUDIT_CHECKPOINT_ED25519_PRIV"))
	rawPub := strings.TrimSpace(os.Getenv("OPS_AUDIT_CHECKPOINT_ED25519_PUB"))

	var signer ExternalCheckpointSigner
	var verifier ExternalCheckpointVerifier

	if rawPriv != "" {
		redact.RegisterRuntimeSecret(rawPriv)
		b, err := decodeHexOrBase64Bytes(rawPriv)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid OPS_AUDIT_CHECKPOINT_ED25519_PRIV: %w", err)
		}
		var priv ed25519.PrivateKey
		switch len(b) {
		case ed25519.SeedSize:
			priv = ed25519.NewKeyFromSeed(b)
		case ed25519.PrivateKeySize:
			priv = ed25519.PrivateKey(b)
		default:
			return nil, nil, fmt.Errorf("OPS_AUDIT_CHECKPOINT_ED25519_PRIV must be 32-byte seed or 64-byte private key")
		}
		k, err := NewEd25519CheckpointSigner(kid, priv)
		if err != nil {
			return nil, nil, err
		}
		signer = k
		verifier = k
	}

	if rawPub != "" {
		b, err := decodeHexOrBase64Bytes(rawPub)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid OPS_AUDIT_CHECKPOINT_ED25519_PUB: %w", err)
		}
		pubKey, err := NewEd25519CheckpointVerifier(kid, ed25519.PublicKey(b))
		if err != nil {
			return nil, nil, err
		}
		if signer != nil && signer.PublicKeyHex() != pubKey.PublicKeyHex() {
			return nil, nil, fmt.Errorf("OPS_AUDIT_CHECKPOINT_ED25519_PUB does not match OPS_AUDIT_CHECKPOINT_ED25519_PRIV")
		}
		verifier = pubKey
	}

	return signer, verifier, nil
}
