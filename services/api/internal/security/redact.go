package security

import (
	"fmt"
	"os"
	"strings"

	"home-datacenter-api/internal/redact"
)

// RedactSensitiveInfo sanitizes sensitive information (passwords, tokens, API keys, JWTs, credentials)
// from arbitrary log text, JSON payloads, URLs, and stack traces.
func RedactSensitiveInfo(input string) string {
	return redact.SensitiveString(input)
}

// RedactSensitiveBytes applies RedactSensitiveInfo on a byte slice.
func RedactSensitiveBytes(input []byte) []byte {
	return redact.SensitiveBytes(input)
}

// MaskSensitiveString is an alias for RedactSensitiveInfo matching the M1-M2 contract.
func MaskSensitiveString(input string) string {
	return RedactSensitiveInfo(input)
}

// RedactString is a shorthand alias for RedactSensitiveInfo.
func RedactString(input string) string {
	return RedactSensitiveInfo(input)
}

// RedactBytes is a shorthand alias for RedactSensitiveBytes.
func RedactBytes(input []byte) []byte {
	return RedactSensitiveBytes(input)
}

// Weak and placeholder passwords blacklisted from production usage.
var WeakPasswordBlacklist = map[string]struct{}{
	"":          {},
	"123456":    {},
	"12345678":  {},
	"123456789": {},
	"password":  {},
	"admin":     {},
	"root":      {},
	"qwerty":    {},
	"pass123":   {},
	"default":   {},
}

// Insecure JWT secret values blacklisted from production usage.
var InsecureJWTSecretBlacklist = map[string]struct{}{
	"":                                      {},
	"your-secret-key":                       {},
	"change-me":                             {},
	"please_change_to_a_long_random_secret": {},
	"123456":                                {},
	"password":                              {},
	"secret":                                {},
	"admin":                                 {},
	"12345678":                              {},
	"default":                               {},
	"homedatacenter":                        {},
}

const (
	// MinJWTSecretLength defines the minimum length for a production JWT secret (256-bit entropy).
	MinJWTSecretLength = 32
	// MinOnePanelPasswordLen defines the minimum length for OnePanel production password.
	MinOnePanelPasswordLen = 8
)

// ValidateBootGuard checks that critical production credentials are set and sufficiently strong.
// In non-production environments (development/test), this check does not block startup.
func ValidateBootGuard(isProduction bool, jwtSecret, onePanelPassword string) error {
	if !isProduction {
		return nil
	}

	trimmedJWT := strings.TrimSpace(jwtSecret)
	if trimmedJWT == "" {
		return fmt.Errorf("JWT_SECRET must not be empty in production")
	}
	if _, bad := InsecureJWTSecretBlacklist[strings.ToLower(trimmedJWT)]; bad {
		return fmt.Errorf("JWT_SECRET matches known weak/placeholder value (%q)", trimmedJWT)
	}
	if len(trimmedJWT) < MinJWTSecretLength {
		return fmt.Errorf("JWT_SECRET is too short (%d chars, required >= %d)", len(trimmedJWT), MinJWTSecretLength)
	}

	trimmedOnePanel := strings.TrimSpace(onePanelPassword)
	if trimmedOnePanel != "" {
		if _, bad := WeakPasswordBlacklist[strings.ToLower(trimmedOnePanel)]; bad {
			return fmt.Errorf("ONEPANEL_PASSWORD matches known weak/default password (%q)", trimmedOnePanel)
		}
		if len(trimmedOnePanel) < MinOnePanelPasswordLen {
			return fmt.Errorf("ONEPANEL_PASSWORD is too short (%d chars, required >= %d)", len(trimmedOnePanel), MinOnePanelPasswordLen)
		}
	}

	return nil
}

// IsProductionEnvironment returns true if the current runtime environment is considered production.
func IsProductionEnvironment() bool {
	appEnv := strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV")))
	if appEnv == "development" || appEnv == "dev" || appEnv == "test" || appEnv == "testing" {
		return false
	}
	if appEnv == "production" || appEnv == "prod" {
		return true
	}

	env := strings.ToLower(strings.TrimSpace(os.Getenv("ENV")))
	if env == "development" || env == "dev" || env == "test" || env == "testing" {
		return false
	}
	if env == "production" || env == "prod" {
		return true
	}

	// Container checks
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if os.Getenv("APP_CONFIG") == "/configs/config.yaml" {
		return true
	}

	return false
}
