package security

import (
	"os"
	"strings"
	"testing"
)

func TestRedactSensitiveInfo_JSON(t *testing.T) {
	input := `{"username":"admin","password":"SuperSecret123!","token":"abc-token-xyz","pcsrftoken":"csrf_val","nested":{"apiKey":"secret-api-key","count":5}}`
	redacted := RedactSensitiveInfo(input)

	if strings.Contains(redacted, "SuperSecret123!") {
		t.Errorf("Password was not redacted: %s", redacted)
	}
	if strings.Contains(redacted, "abc-token-xyz") {
		t.Errorf("Token was not redacted: %s", redacted)
	}
	if strings.Contains(redacted, "csrf_val") {
		t.Errorf("CSRF token was not redacted: %s", redacted)
	}
	if strings.Contains(redacted, "secret-api-key") {
		t.Errorf("API key was not redacted: %s", redacted)
	}
	if !strings.Contains(redacted, `"password":"[REDACTED]"`) {
		t.Errorf("Expected [REDACTED] for password: %s", redacted)
	}
	if !strings.Contains(redacted, `"count":5`) {
		t.Errorf("Non-sensitive field count was unexpectedly corrupted: %s", redacted)
	}
}

func TestRedactSensitiveInfo_URLCredentials(t *testing.T) {
	input := "Connecting to http://feiyemomo:@Momo324@192.168.31.234:10000/btpanel and https://admin:p%40ssw0rd@remote.host:8443/api"
	redacted := RedactSensitiveInfo(input)

	if strings.Contains(redacted, "@Momo324") {
		t.Errorf("URL password was not redacted: %s", redacted)
	}
	if strings.Contains(redacted, "p%40ssw0rd") {
		t.Errorf("URL password was not redacted: %s", redacted)
	}
	if !strings.Contains(redacted, "http://feiyemomo:[REDACTED]@192.168.31.234:10000/btpanel") {
		t.Errorf("Unexpected URL redaction format: %s", redacted)
	}
}

func TestRedactSensitiveInfo_BearerTokens(t *testing.T) {
	input := "Request headers: Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.e30.t-ID brackets"
	redacted := RedactSensitiveInfo(input)

	if strings.Contains(redacted, "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9") {
		t.Errorf("Bearer token payload was not redacted: %s", redacted)
	}
	if !strings.Contains(redacted, "Bearer [REDACTED]") {
		t.Errorf("Expected 'Bearer [REDACTED]': %s", redacted)
	}
}

func TestRedactSensitiveInfo_RawJWT(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	input := "User authenticated with JWT: " + jwt + " timestamp=12345"
	redacted := RedactSensitiveInfo(input)

	if strings.Contains(redacted, jwt) {
		t.Errorf("Raw JWT was not redacted: %s", redacted)
	}
	if !strings.Contains(redacted, "[REDACTED_JWT]") {
		t.Errorf("Expected [REDACTED_JWT]: %s", redacted)
	}
}

func TestRedactSensitiveInfo_PEMKey(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAz/FakeKeyContent/ABCD...\n-----END RSA PRIVATE KEY-----"
	input := "Loaded server key:\n" + pem + "\nReady to serve."
	redacted := RedactSensitiveInfo(input)

	if strings.Contains(redacted, "MIIEowIBAAKCAQEAz") {
		t.Errorf("Private key contents were not redacted: %s", redacted)
	}
	if !strings.Contains(redacted, "[REDACTED_PRIVATE_KEY]") {
		t.Errorf("Expected [REDACTED_PRIVATE_KEY]: %s", redacted)
	}
}

func TestRedactSensitiveInfo_KeyValueAndQuery(t *testing.T) {
	tests := []struct {
		input       string
		mustContain string
		mustNot     string
	}{
		{"Query /api?user=admin&password=mySecretPass&other=1", "password=[REDACTED]", "mySecretPass"},
		{`env: token="super_token_999"`, `token="[REDACTED]"`, "super_token_999"},
		{"header apiKey: k3y_abc_123;", "apiKey:[REDACTED]", "k3y_abc_123"},
	}

	for _, tt := range tests {
		res := RedactSensitiveInfo(tt.input)
		if strings.Contains(res, tt.mustNot) {
			t.Errorf("Input %q leaked sensitive string %q: %s", tt.input, tt.mustNot, res)
		}
		if !strings.Contains(res, tt.mustContain) {
			t.Errorf("Input %q did not produce expected redaction %q: %s", tt.input, tt.mustContain, res)
		}
	}
}

func TestRedactSensitiveBytes_AndAliases(t *testing.T) {
	inputStr := `password="top_secret"`
	inputBytes := []byte(inputStr)

	redactedBytes := RedactSensitiveBytes(inputBytes)
	if strings.Contains(string(redactedBytes), "top_secret") {
		t.Errorf("RedactSensitiveBytes failed to redact: %s", string(redactedBytes))
	}

	aliasStr := MaskSensitiveString(inputStr)
	if strings.Contains(aliasStr, "top_secret") {
		t.Errorf("MaskSensitiveString failed to redact: %s", aliasStr)
	}

	shorthandStr := RedactString(inputStr)
	if strings.Contains(shorthandStr, "top_secret") {
		t.Errorf("RedactString failed to redact: %s", shorthandStr)
	}

	shorthandBytes := RedactBytes(inputBytes)
	if strings.Contains(string(shorthandBytes), "top_secret") {
		t.Errorf("RedactBytes failed to redact: %s", string(shorthandBytes))
	}
}

func TestValidateBootGuard_NonProduction(t *testing.T) {
	// In non-production, weak or empty credentials do not fail
	if err := ValidateBootGuard(false, "", ""); err != nil {
		t.Errorf("Non-production mode should never error on empty credentials: %v", err)
	}
	if err := ValidateBootGuard(false, "your-secret-key", "@Momo324"); err != nil {
		t.Errorf("Non-production mode should not block default credentials: %v", err)
	}
}

func TestValidateBootGuard_Production(t *testing.T) {
	validJWT := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	validOnePanel := "SecureOnePanelPass2026!"

	// 1. Success case
	if err := ValidateBootGuard(true, validJWT, validOnePanel); err != nil {
		t.Errorf("Valid credentials failed boot guard in production: %v", err)
	}

	// 2. Empty JWT Secret
	if err := ValidateBootGuard(true, "", validOnePanel); err == nil {
		t.Error("Expected error for empty JWT_SECRET in production, got nil")
	}

	// 3. Weak / Blacklisted JWT Secret
	weakJWTs := []string{
		"your-secret-key",
		"change-me",
		"please_change_to_a_long_random_secret",
		"@Momo324",
		"123456",
		"password",
		"short-secret",
	}
	for _, w := range weakJWTs {
		if err := ValidateBootGuard(true, w, validOnePanel); err == nil {
			t.Errorf("Expected error for weak JWT_SECRET %q, got nil", w)
		}
	}

	// 4. Empty OnePanel Password should pass (1Panel is auxiliary, falls back to host telemetry)
	if err := ValidateBootGuard(true, validJWT, ""); err != nil {
		t.Errorf("Empty ONEPANEL_PASSWORD in production should not block core API startup: %v", err)
	}

	// 5. Weak / Blacklisted OnePanel Password
	weakPasswords := []string{
		"123456",
		"12345678",
		"password",
		"admin",
		"root",
		"short",
	}
	for _, p := range weakPasswords {
		if err := ValidateBootGuard(true, validJWT, p); err == nil {
			t.Errorf("Expected error for weak ONEPANEL_PASSWORD %q, got nil", p)
		}
	}
}

func TestIsProductionEnvironment(t *testing.T) {
	origAppEnv := os.Getenv("APP_ENV")
	origEnv := os.Getenv("ENV")
	defer func() {
		os.Setenv("APP_ENV", origAppEnv)
		os.Setenv("ENV", origEnv)
	}()

	os.Setenv("APP_ENV", "production")
	if !IsProductionEnvironment() {
		t.Error("Expected IsProductionEnvironment to be true when APP_ENV=production")
	}

	os.Setenv("APP_ENV", "development")
	if IsProductionEnvironment() {
		t.Error("Expected IsProductionEnvironment to be false when APP_ENV=development")
	}

	os.Setenv("APP_ENV", "test")
	if IsProductionEnvironment() {
		t.Error("Expected IsProductionEnvironment to be false when APP_ENV=test")
	}
}
