package utils

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Access-key hashing
//
// Historical builds stored a bare `sha256(access_key)` hex digest in
// devices.access_key_hash. That is unsalted, un-stretched and extremely
// fast, so anyone who obtains a database dump (see the leaked S3 bucket
// finding) can brute-force or rainbow-table the digest back to the
// original key and then call POST /api/v1/auth/bind to mint a
// long-lived admin JWT. Several access keys were in fact recovered
// this way because they were human-chosen passwords.
//
// v1.14.9 stores PBKDF2-HMAC-SHA256 with a fresh per-row random salt and
// a deliberately expensive iteration count, encoded as a self-describing
// string so the cost can be raised later without a schema migration:
//
//	pbkdf2-sha256$<iterations>$<base64 raw salt>$<base64 raw derived key>
//
// Verification still accepts the legacy bare-hex digest (compared in
// constant time) so existing devices keep working, and the bind path
// transparently re-derives legacy rows with the current parameters —
// see AccessKeyNeedsRehash.
const (
	accessKeyAlgorithm = "pbkdf2-sha256"

	// accessKeyIterations is the PBKDF2 work factor. It is a
	// compromise between offline-guessing resistance and login
	// latency: a bind request verifies the key against every device
	// owned by the user, so the cost is multiplied by the device
	// count. ~120k HMAC-SHA256 iterations lands in the tens of
	// milliseconds on the NAS CPU while making a 10k-entry dictionary
	// attack cost ~10^9 hash evaluations instead of the ~10^4 a bare
	// SHA-256 allowed.
	accessKeyIterations = 120_000

	accessKeySaltLen = 16
	accessKeyKeyLen  = 32

	// legacyAccessKeyDigestLen is the length of a sha256 hex digest,
	// used to recognise pre-1.14.9 rows.
	legacyAccessKeyDigestLen = 64
)

// GenerateAccessKey returns a 32-byte random key as a 64-character hex
// string. It is shown to the user exactly once, at device creation.
func GenerateAccessKey() (string, error) {
	b := make([]byte, 32)

	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}

// HashAccessKey derives a salted PBKDF2 hash of key.
//
// The salt is drawn from crypto/rand, which is guaranteed not to fail
// on supported platforms (Go 1.24+), so this function has no error to
// report.
func HashAccessKey(key string) string {
	salt := make([]byte, accessKeySaltLen)
	//nolint:errcheck // crypto/rand.Read never returns an error (Go 1.24+).
	rand.Read(salt)

	dk := deriveAccessKey(key, salt, accessKeyIterations)

	return fmt.Sprintf("%s$%d$%s$%s",
		accessKeyAlgorithm,
		accessKeyIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk),
	)
}

// VerifyAccessKey reports whether accessKey matches storedHash.
//
// storedHash may be either the current PBKDF2 encoding or the legacy
// bare sha256 hex digest. Both comparisons are constant time so the
// result cannot be used as a byte-by-byte oracle.
func VerifyAccessKey(accessKey string, storedHash string) bool {
	parts := strings.Split(storedHash, "$")
	if len(parts) == 4 && parts[0] == accessKeyAlgorithm {
		iterations, err := strconv.Atoi(parts[1])
		if err != nil || iterations <= 0 || iterations > 10_000_000 {
			return false
		}
		salt, err := base64.RawStdEncoding.DecodeString(parts[2])
		if err != nil || len(salt) == 0 {
			return false
		}
		want, err := base64.RawStdEncoding.DecodeString(parts[3])
		if err != nil || len(want) == 0 {
			return false
		}
		got := deriveAccessKey(accessKey, salt, iterations)
		if len(got) != len(want) {
			return false
		}
		return subtle.ConstantTimeCompare(got, want) == 1
	}

	// Legacy: bare hex sha256 digest. Reject anything that is not
	// shaped like one so malformed rows fail closed.
	if len(storedHash) != legacyAccessKeyDigestLen {
		return false
	}
	legacy := legacyHashAccessKey(accessKey)
	return subtle.ConstantTimeCompare([]byte(legacy), []byte(storedHash)) == 1
}

// AccessKeyNeedsRehash reports whether storedHash should be re-derived
// with the current algorithm/parameters. Callers that hold the plaintext
// key (the bind flow) use this to upgrade legacy rows in place.
func AccessKeyNeedsRehash(storedHash string) bool {
	if storedHash == "" {
		return false
	}
	parts := strings.Split(storedHash, "$")
	if len(parts) != 4 || parts[0] != accessKeyAlgorithm {
		// Legacy digest, or an encoding we do not recognise.
		return true
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil {
		return true
	}
	return iterations < accessKeyIterations
}

// deriveAccessKey runs PBKDF2-HMAC-SHA256. The parameters are validated
// by callers, so an error here is not expected; it yields a nil slice,
// which fails verification.
func deriveAccessKey(key string, salt []byte, iterations int) []byte {
	dk, err := pbkdf2.Key(sha256.New, key, salt, iterations, accessKeyKeyLen)
	if err != nil {
		return nil
	}
	return dk
}

// legacyHashAccessKey reproduces the pre-1.14.9 unsalted SHA-256 digest
// so that device rows created by older builds continue to authenticate
// until they are rehashed.
func legacyHashAccessKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
