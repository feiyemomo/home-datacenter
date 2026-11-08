package security

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	// rePEMKey matches PEM-encoded private keys
	rePEMKey = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9_\- ]+PRIVATE KEY-----.*?-----END [A-Z0-9_\- ]+PRIVATE KEY-----`)

	// reURLCreds matches credentials embedded in URLs: http(s)://user:password@host (including passwords with symbols)
	reURLCreds = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)([^:/\s]+):(.+?)@([a-zA-Z0-9.\-_]+(?::[0-9]+)?(?:/|\?|#|\s|$))`)

	// reBearer matches HTTP Authorization Bearer tokens
	reBearer = regexp.MustCompile(`(?i)\b(Bearer\s+)[A-Za-z0-9\-._~+/]+=*`)

	// reBasic matches HTTP Authorization Basic tokens
	reBasic = regexp.MustCompile(`(?i)\b(Basic\s+)[A-Za-z0-9+/]+=*`)

	// reJWT matches raw standalone JWT tokens (3 dot-separated base64url segments starting with eyJ)
	reJWT = regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]+`)

	// reJSONString matches sensitive fields in JSON with string values
	reJSONString = regexp.MustCompile(`(?i)("(?:password|passwd|secret|jwt|token|apiKey|api_key|access_token|authorization|pcsrftoken|psession|credential|auth_token)"\s*:\s*)"([^"]*)"`)

	// reJSONUnquoted matches sensitive fields in JSON with unquoted values (numbers, booleans, literals)
	reJSONUnquoted = regexp.MustCompile(`(?i)("(?:password|passwd|secret|jwt|token|apiKey|api_key|access_token|authorization|pcsrftoken|psession|credential|auth_token)"\s*:\s*)([^"\s,}\]][^\s,}\]]*)`)

	// reKVQuoted matches quoted key-value pairs (e.g. password="secret" or token='secret')
	reKVQuoted = regexp.MustCompile(`(?i)\b(password|passwd|secret|jwt|token|apiKey|api_key|access_token|pcsrftoken|psession|credential|auth_token)\s*([=:])\s*["']([^"']*)["']`)

	// reKVUnquoted matches unquoted key-value pairs (e.g. password=secret or token: secret)
	reKVUnquoted = regexp.MustCompile(`(?i)\b(password|passwd|secret|jwt|token|apiKey|api_key|access_token|pcsrftoken|psession|credential|auth_token)\s*([=:])\s*([^"'\[\s,;}&][^\s,;}&]*)`)
)

// RedactSensitiveInfo sanitizes sensitive information (passwords, tokens, API keys, JWTs, credentials)
// from arbitrary log text, JSON payloads, URLs, and stack traces.
func RedactSensitiveInfo(input string) string {
	if input == "" {
		return ""
	}

	result := input

	// 1. Scrub PEM private keys
	result = rePEMKey.ReplaceAllString(result, "[REDACTED_PRIVATE_KEY]")

	// 2. Scrub credentials in URLs: http(s)://user:password@host -> http(s)://user:[REDACTED]@host
	result = reURLCreds.ReplaceAllString(result, "${1}${2}:[REDACTED]@${4}")

	// 3. Scrub Bearer tokens
	result = reBearer.ReplaceAllString(result, "${1}[REDACTED]")

	// 4. Scrub Basic auth tokens
	result = reBasic.ReplaceAllString(result, "${1}[REDACTED]")

	// 5. Scrub standalone JWTs
	result = reJWT.ReplaceAllString(result, "[REDACTED_JWT]")

	// 6. Scrub JSON string fields
	result = reJSONString.ReplaceAllString(result, `${1}"[REDACTED]"`)

	// 7. Scrub JSON unquoted fields
	result = reJSONUnquoted.ReplaceAllString(result, `${1}"[REDACTED]"`)

	// 8. Scrub quoted key-value pairs
	result = reKVQuoted.ReplaceAllString(result, `${1}${2}"[REDACTED]"`)

	// 9. Scrub unquoted key-value pairs
	result = reKVUnquoted.ReplaceAllString(result, `${1}${2}[REDACTED]`)

	return result
}

// RedactSensitiveBytes applies RedactSensitiveInfo on a byte slice.
func RedactSensitiveBytes(input []byte) []byte {
	if len(input) == 0 {
		return input
	}
	return []byte(RedactSensitiveInfo(string(input)))
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
	"":                                      {},
	"123456":                                {},
	"12345678":                              {},
	"123456789":                             {},
	"@momo324":                              {},
	"password":                              {},
	"admin":                                 {},
	"btpanel":                               {},
	"feiyemomo":                             {},
	"root":                                  {},
	"qwerty":                                {},
	"pass123":                               {},
	"default":                               {},
}

// Insecure JWT secret values blacklisted from production usage.
var InsecureJWTSecretBlacklist = map[string]struct{}{
	"":                                      {},
	"your-secret-key":                       {},
	"change-me":                             {},
	"please_change_to_a_long_random_secret": {},
	"@momo324":                              {},
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
	if trimmedOnePanel == "" {
		return fmt.Errorf("ONEPANEL_PASSWORD must not be empty in production")
	}
	if _, bad := WeakPasswordBlacklist[strings.ToLower(trimmedOnePanel)]; bad {
		return fmt.Errorf("ONEPANEL_PASSWORD matches known weak/default password (%q)", trimmedOnePanel)
	}
	if len(trimmedOnePanel) < MinOnePanelPasswordLen {
		return fmt.Errorf("ONEPANEL_PASSWORD is too short (%d chars, required >= %d)", len(trimmedOnePanel), MinOnePanelPasswordLen)
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
