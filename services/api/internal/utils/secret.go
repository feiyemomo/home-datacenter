// Package utils: AES-GCM secret box for at-rest credentials.
//
// The 32-byte key is derived from the JWT secret via SHA-256. We
// deliberately avoid adding a new key-management surface (Vault, KMS)
// because the platform runs on a single home node and the JWT secret
// is already the single root secret — re-using it keeps the threat
// model flat (one secret to rotate, not two).
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

// SecretBox is a thin AES-256-GCM wrapper. It is safe for concurrent
// use because cipher.AEAD itself is.
type SecretBox struct{ gcm cipher.AEAD }

// NewSecretBox derives a 32-byte key from the supplied JWT secret and
// returns a ready-to-use SecretBox. Returns an error if the secret is
// obviously too weak (< 16 chars) so a misconfigured deploy fails fast.
func NewSecretBox(jwtSecret string) (*SecretBox, error) {
	if len(jwtSecret) < 16 {
		return nil, errors.New("secret: jwt secret too short (< 16 chars)")
	}
	sum := sha256.Sum256([]byte(jwtSecret))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &SecretBox{gcm: g}, nil
}

// Encrypt seals plaintext with a fresh random nonce. Returns
// base64(nonce || ciphertext) — ready to be stored in a TEXT column.
func (s *SecretBox) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := s.gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

// Decrypt opens a value produced by Encrypt. Returns "" for "" so
// callers can treat empty ciphertext and empty plaintext identically.
func (s *SecretBox) Decrypt(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	ns := s.gcm.NonceSize()
	if len(raw) < ns+s.gcm.Overhead() {
		return "", errors.New("secret: ciphertext too short")
	}
	pt, err := s.gcm.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
