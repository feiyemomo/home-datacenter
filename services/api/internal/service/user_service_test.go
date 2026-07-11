package service

import (
	"strings"
	"testing"
)

// Unit tests for the user-management service. We test the pure
// functions (name validation, unique-violation detection) and the
// domain errors here; the GORM-backed paths are covered by the
// integration test (cmd/integration_test) that boots the full
// API against a throwaway SQLite file.

func TestIsValidUserName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"ascii short", "alice", true},
		{"ascii with digits", "alice42", true},
		{"ascii with underscore", "alice_b", true},
		{"ascii with dash", "alice-b", true},
		{"unicode", "小明", true},
		{"unicode bootstrap name", "自己", true},
		{"min length", "a", true},
		{"max length", strings.Repeat("a", 32), true},
		{"empty", "", false},
		{"whitespace only", "   ", false},
		{"too long", strings.Repeat("a", 33), false},
		{"contains space", "ali ce", false},
		{"contains dot", "alice.b", false},
		{"contains at", "alice@home", false},
		{"contains slash", "alice/home", false},
		{"leading space trimmed", " alice", true},  // trim is forgiving
		{"trailing space trimmed", "alice ", true}, // trim is forgiving
		{"tab inside", "ali\tce", false},
		{"new line inside", "ali\nce", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isValidUserName(tc.in); got != tc.want {
				t.Errorf("isValidUserName(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeUserName(t *testing.T) {
	// Leading/trailing whitespace is trimmed silently — this is
	// more forgiving than rejecting "alice " outright, and avoids
	// the "name already in use" surprise when the user pastes a
	// name with a trailing space from another source.
	got, err := normalizeUserName("alice ")
	if err != nil {
		t.Errorf("normalizeUserName(\"alice \") unexpected err: %v", err)
	}
	if got != "alice" {
		t.Errorf("normalizeUserName(\"alice \") = %q, want \"alice\"", got)
	}

	// Internal whitespace is still rejected.
	_, err = normalizeUserName("ali ce")
	if err != ErrInvalidName {
		t.Errorf("normalizeUserName(\"ali ce\") err = %v, want ErrInvalidName", err)
	}

	// Unicode is allowed.
	got, err = normalizeUserName("  小明  ")
	if err != nil {
		t.Errorf("normalizeUserName(\"  小明  \") unexpected err: %v", err)
	}
	if got != "小明" {
		t.Errorf("normalizeUserName(\"  小明  \") = %q, want \"小明\"", got)
	}
}

func TestIsUniqueViolation(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want bool
	}{
		{"nil", nil, false},
		{"sqlite unique", errString("UNIQUE constraint failed: users.name"), true},
		{"postgres unique", errString("duplicate key value violates unique constraint"), true},
		{"unrelated", errString("connection refused"), false},
		{"empty", errString(""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUniqueViolation(tc.in); got != tc.want {
				t.Errorf("isUniqueViolation(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// errString is a tiny test helper to construct an error from a
// string literal without importing errors just for errors.New.
type errString string

func (e errString) Error() string { return string(e) }
