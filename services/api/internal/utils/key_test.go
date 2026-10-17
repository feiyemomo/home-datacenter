package utils

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestHashAccessKeyIsSaltedAndSelfDescribing(t *testing.T) {
	key := "correct horse battery staple"

	first := HashAccessKey(key)
	second := HashAccessKey(key)

	if !strings.HasPrefix(first, accessKeyAlgorithm+"$") {
		t.Fatalf("hash %q is not in the expected PHC-ish format", first)
	}
	if strings.Count(first, "$") != 3 {
		t.Fatalf("hash %q should have exactly 4 $-separated fields", first)
	}
	// A fresh salt per call means two hashes of the same key differ.
	if first == second {
		t.Fatal("HashAccessKey reused a salt: identical output for the same key")
	}
}

func TestVerifyAccessKeyRoundTrip(t *testing.T) {
	key := "wm10050817"
	stored := HashAccessKey(key)

	if !VerifyAccessKey(key, stored) {
		t.Fatal("VerifyAccessKey rejected the key it hashed")
	}
	if VerifyAccessKey("wrong-key", stored) {
		t.Fatal("VerifyAccessKey accepted a wrong key")
	}
	// Whitespace / case must not be normalised away.
	if VerifyAccessKey("WM10050817", stored) {
		t.Fatal("VerifyAccessKey must be case sensitive")
	}
}

func TestVerifyAccessKeyAcceptsLegacyDigest(t *testing.T) {
	key := "legacy-device-key"
	sum := sha256.Sum256([]byte(key))
	legacy := hex.EncodeToString(sum[:])

	if !VerifyAccessKey(key, legacy) {
		t.Fatal("legacy sha256 digest should still verify")
	}
	if VerifyAccessKey("nope", legacy) {
		t.Fatal("legacy path accepted a wrong key")
	}
}

func TestVerifyAccessKeyRejectsMalformedHashes(t *testing.T) {
	cases := []string{
		"",
		"not-a-hash",
		"pbkdf2-sha256$0$c2FsdA$a2V5",           // zero iterations
		"pbkdf2-sha256$abc$c2FsdA$a2V5",         // non-numeric iterations
		"pbkdf2-sha256$1000$!!!$a2V5",           // bad base64 salt
		"pbkdf2-sha256$1000$c2FsdA$",            // empty derived key
		"pbkdf2-sha256$99999999$c2FsdA$a2V5",    // absurd cost
		strings.Repeat("a", legacyAccessKeyDigestLen-1),
	}
	for _, c := range cases {
		if VerifyAccessKey("anything", c) {
			t.Fatalf("VerifyAccessKey accepted malformed hash %q", c)
		}
	}
}

func TestAccessKeyNeedsRehash(t *testing.T) {
	sum := sha256.Sum256([]byte("k"))
	legacy := hex.EncodeToString(sum[:])

	if !AccessKeyNeedsRehash(legacy) {
		t.Fatal("legacy digest should be flagged for rehash")
	}
	if AccessKeyNeedsRehash(HashAccessKey("k")) {
		t.Fatal("current hash should not be flagged for rehash")
	}
	if AccessKeyNeedsRehash("") {
		t.Fatal("empty hash must not be flagged (nothing to upgrade)")
	}
	if !AccessKeyNeedsRehash("pbkdf2-sha256$1000$c2FsdA$a2V5") {
		t.Fatal("a lower iteration count should be flagged for rehash")
	}
}

func TestGenerateAccessKeyShape(t *testing.T) {
	seen := make(map[string]bool, 16)
	for i := 0; i < 16; i++ {
		k, err := GenerateAccessKey()
		if err != nil {
			t.Fatalf("GenerateAccessKey: %v", err)
		}
		if len(k) != 64 {
			t.Fatalf("expected 64 hex chars, got %d", len(k))
		}
		if seen[k] {
			t.Fatal("GenerateAccessKey produced a duplicate")
		}
		seen[k] = true
	}
}
