package redact

import (
	"strings"
	"testing"
)

func TestSensitiveString_MultiKeyringAndRuntimeSecret(t *testing.T) {
	sec1 := "primary-audit-hmac-secret-key-32bytes-test-01"
	sec2 := "rotated-audit-hmac-secret-key-32bytes-test-02"
	RegisterRuntimeSecret(sec1)
	RegisterRuntimeSecret(sec2)

	envLine := "OPS_AUDIT_HMAC_KEYS=k1:" + sec1 + ",k2:" + sec2 + " next=ok"
	redactedEnv := SensitiveString(envLine)
	if strings.Contains(redactedEnv, sec1) || strings.Contains(redactedEnv, sec2) {
		t.Fatalf("expected all keyring secrets redacted, got %q", redactedEnv)
	}
	if !strings.Contains(redactedEnv, "next=ok") {
		t.Fatalf("expected non-secret trailing field preserved, got %q", redactedEnv)
	}

	rawErr := "unexpected signing failure using key " + sec2 + " at line 42"
	redactedErr := SensitiveString(rawErr)
	if strings.Contains(redactedErr, sec2) {
		t.Fatalf("expected raw registered secret redacted, got %q", redactedErr)
	}
	if !strings.Contains(redactedErr, "[REDACTED_SECRET]") {
		t.Fatalf("expected [REDACTED_SECRET] marker, got %q", redactedErr)
	}
}
