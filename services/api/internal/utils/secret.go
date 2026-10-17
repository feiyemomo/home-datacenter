// Package utils: AES-GCM secret box for at-rest credentials.
//
// Key management
// --------------
// Historically the 32-byte key was derived from the JWT signing
// secret via SHA-256. That coupled two unrelated lifecycles: rotating
// the JWT secret (a routine security action) silently made every
// stored camera password undecryptable — the AES-GCM tag check failed
// and the whole camera pipeline (go2rtc + Frigate) came up with zero
// cameras.
//
// The key is now sourced from a dedicated `ENCRYPTION_KEY`
// (config: security.encryption_key). To stay backward compatible the
// box accepts a *key ring*: encryption always uses the primary key,
// while decryption falls back through a list of legacy keys. Existing
// ciphertexts sealed with the old JWT secret therefore keep working
// after the switch, and a rotation of either secret no longer loses
// data.
//
// On-disk format: base64( nonce || ciphertext || gcm_tag )
// Empty input is returned as empty (idempotent).
package utils

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
)

// minSecretLen is the shortest secret accepted. The key is run
// through SHA-256 so any length works, but a trivially short secret
// is rejected to fail fast on a misconfigured deploy.
const minSecretLen = 16

// SecretBox is a thin AES-256-GCM wrapper. It is safe for concurrent
// use because cipher.AEAD itself is.
//
// primary is used for all writes. legacy holds the keys of previous
// deployments and is consulted only when the primary tag check fails,
// so a key rotation can read old rows and lazily re-seal them.
type SecretBox struct {
	primary cipher.AEAD
	legacy  []cipher.AEAD
}

// deriveGCM turns an arbitrary-length secret into an AES-256-GCM AEAD
// keyed by SHA-256(secret).
func deriveGCM(secret string) (cipher.AEAD, error) {
	if len(secret) < minSecretLen {
		return nil, errors.New("secret: key material too short (< 16 chars)")
	}
	sum := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// NewSecretBox derives a 32-byte key from the supplied secret and
// returns a ready-to-use SecretBox. Returns an error if the secret is
// obviously too weak (< 16 chars) so a misconfigured deploy fails fast.
func NewSecretBox(secret string) (*SecretBox, error) {
	return NewSecretBoxWithKeyRing(secret)
}

// NewAppSecretBox builds the box the API (and the credential repair
// tooling) uses: the primary key comes from the dedicated
// ENCRYPTION_KEY, falling back to the JWT secret when unset, and the
// JWT secret is always registered as a legacy key. Centralising this
// keeps the API and the one-shot migration scripts on the exact same
// key ring — a drift here would write rows the API cannot read.
func NewAppSecretBox(encryptionKey, jwtSecret string) (*SecretBox, error) {
	primary := encryptionKey
	if primary == "" {
		primary = jwtSecret
	}
	return NewSecretBoxWithKeyRing(primary, jwtSecret)
}

// NewSecretBoxWithKeyRing builds a box whose writes use primary and
// whose reads also try each distinct, valid legacy secret. Empty or
// too-short legacy entries are skipped so a partially migrated
// deployment still boots.
func NewSecretBoxWithKeyRing(primary string, legacy ...string) (*SecretBox, error) {
	g, err := deriveGCM(primary)
	if err != nil {
		return nil, err
	}
	box := &SecretBox{primary: g}
	for _, l := range legacy {
		if l == "" || l == primary {
			continue
		}
		lg, err := deriveGCM(l)
		if err != nil {
			// A bad legacy entry must not block startup: the primary
			// key is the only one required for writes.
			continue
		}
		box.legacy = append(box.legacy, lg)
	}
	return box, nil
}

// Encrypt seals plaintext with a fresh random nonce using the primary
// key. Returns base64(nonce || ciphertext) — ready to be stored in a
// TEXT column.
func (s *SecretBox) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, s.primary.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := s.primary.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

// decryptWith opens ciphertext with a single AEAD.
func decryptWith(g cipher.AEAD, raw []byte) (string, error) {
	ns := g.NonceSize()
	if len(raw) < ns+g.Overhead() {
		return "", errors.New("secret: ciphertext too short")
	}
	pt, err := g.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// Decrypt opens a value produced by Encrypt. Returns "" for "" so
// callers can treat empty ciphertext and empty plaintext identically.
//
// The primary key is tried first; on a tag mismatch each legacy key is
// tried in turn. The first success wins.
func (s *SecretBox) Decrypt(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	if pt, err := decryptWith(s.primary, raw); err == nil {
		return pt, nil
	}
	var lastErr error
	for _, g := range s.legacy {
		if pt, err := decryptWith(g, raw); err == nil {
			return pt, nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("secret: message authentication failed")
	}
	return "", lastErr
}

// DecryptLegacy reports whether the ciphertext opened with a key other
// than the primary. Callers can use it to re-seal rows opportunistically
// while they are already being rewritten.
func (s *SecretBox) DecryptLegacy(ciphertext string) (string, bool, error) {
	if ciphertext == "" {
		return "", false, nil
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", false, err
	}
	if pt, err := decryptWith(s.primary, raw); err == nil {
		return pt, false, nil
	}
	for _, g := range s.legacy {
		if pt, err := decryptWith(g, raw); err == nil {
			return pt, true, nil
		}
	}
	return "", false, errors.New("secret: message authentication failed")
}
