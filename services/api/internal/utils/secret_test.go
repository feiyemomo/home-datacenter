package utils

import "testing"

const (
	secretA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // 32 chars
	secretB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" // 32 chars, distinct
)

func TestSecretBoxRoundTrip(t *testing.T) {
	box, err := NewSecretBox(secretA)
	if err != nil {
		t.Fatalf("NewSecretBox: %v", err)
	}

	ct, err := box.Encrypt("hello world")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if ct == "" {
		t.Fatal("Encrypt returned empty ciphertext for non-empty input")
	}

	pt, err := box.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if pt != "hello world" {
		t.Fatalf("Decrypt = %q, want %q", pt, "hello world")
	}
}

func TestSecretBoxEmptyIsIdempotent(t *testing.T) {
	box, _ := NewSecretBox(secretA)
	if ct, err := box.Encrypt(""); err != nil || ct != "" {
		t.Fatalf("Encrypt(\"\") = (%q, %v), want (\"\", nil)", ct, err)
	}
	if pt, err := box.Decrypt(""); err != nil || pt != "" {
		t.Fatalf("Decrypt(\"\") = (%q, %v), want (\"\", nil)", pt, err)
	}
}

func TestSecretBoxRejectsShortSecret(t *testing.T) {
	if _, err := NewSecretBox("too-short"); err == nil {
		t.Fatal("expected error for short secret, got nil")
	}
}

// TestKeyRingReadsLegacyCiphertext is the regression guard for the
// production incident where rotating the JWT secret made every stored
// camera password undecryptable: rows sealed with the old key must
// still open when that key is present in the ring.
func TestKeyRingReadsLegacyCiphertext(t *testing.T) {
	legacyBox, err := NewSecretBox(secretB)
	if err != nil {
		t.Fatalf("legacy box: %v", err)
	}
	legacyCT, err := legacyBox.Encrypt("camera-secret")
	if err != nil {
		t.Fatalf("legacy encrypt: %v", err)
	}

	// Primary is A (the new ENCRYPTION_KEY); B is a legacy entry.
	box, err := NewSecretBoxWithKeyRing(secretA, secretB)
	if err != nil {
		t.Fatalf("key ring box: %v", err)
	}

	pt, err := box.Decrypt(legacyCT)
	if err != nil {
		t.Fatalf("Decrypt with legacy key in ring: %v", err)
	}
	if pt != "camera-secret" {
		t.Fatalf("Decrypt = %q, want %q", pt, "camera-secret")
	}

	// And the ring must report that a legacy key was used.
	if _, legacy, err := box.DecryptLegacy(legacyCT); err != nil || !legacy {
		t.Fatalf("DecryptLegacy = (legacy=%v, err=%v), want (true, nil)", legacy, err)
	}

	// New writes use the primary key, so they are NOT "legacy".
	newCT, err := box.Encrypt("fresh")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, legacy, err := box.DecryptLegacy(newCT); err != nil || legacy {
		t.Fatalf("DecryptLegacy(new) = (legacy=%v, err=%v), want (false, nil)", legacy, err)
	}

	// A box without the legacy key must still fail loudly.
	strict, _ := NewSecretBoxWithKeyRing(secretA)
	if _, err := strict.Decrypt(legacyCT); err == nil {
		t.Fatal("expected authentication failure without the legacy key")
	}
}

// TestNewAppSecretBoxFallsBackToJWT covers a deployment with no
// ENCRYPTION_KEY set: the JWT secret becomes primary so behaviour is
// unchanged from the pre-decoupling code.
func TestNewAppSecretBoxFallsBackToJWT(t *testing.T) {
	box, err := NewAppSecretBox("", secretA)
	if err != nil {
		t.Fatalf("NewAppSecretBox: %v", err)
	}
	ct, err := box.Encrypt("x")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, legacy, err := box.DecryptLegacy(ct); err != nil || legacy {
		t.Fatalf("primary should be the JWT secret, got legacy=%v err=%v", legacy, err)
	}

	// With a distinct ENCRYPTION_KEY the JWT secret becomes a legacy
	// (read-only) key: data sealed under it still opens.
	jwtBox, _ := NewSecretBox(secretA)
	oldCT, _ := jwtBox.Encrypt("old")
	appBox, _ := NewAppSecretBox(secretB, secretA)
	pt, legacy, err := appBox.DecryptLegacy(oldCT)
	if err != nil || !legacy || pt != "old" {
		t.Fatalf("expected legacy read via JWT secret, got (%q, %v, %v)", pt, legacy, err)
	}
}
