package network

import "crypto/rand"

// readRandom fills b with cryptographically secure random bytes.
// Small wrapper so callers don't need to import crypto/rand directly.
func readRandom(b []byte) (int, error) {
	return rand.Read(b)
}
