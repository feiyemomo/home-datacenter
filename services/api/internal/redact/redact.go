package redact

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

const (
	sensitiveKeyPattern   = `password|passwd|secret|secret_key|s3_secret|jwt|token|apiKey|api_key|access_token|refresh_token|authorization|pcsrftoken|psession|credential|auth_token|cookie|set-cookie|x-internal-secret|internal_ops_token|emergency_override_token|session_id|hmac|encryption_key|bitiful|totp|otp|private_key`
	kvSensitiveKeyPattern = `password|passwd|secret|secret_key|s3_secret|jwt|token|apiKey|api_key|access_token|refresh_token|pcsrftoken|psession|credential|auth_token|cookie|set-cookie|x-internal-secret|internal_ops_token|emergency_override_token|session_id|hmac|encryption_key|bitiful|totp|otp|private_key`
)

var (
	// rePEMKey matches PEM-encoded private keys
	rePEMKey = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9_\- ]+PRIVATE KEY-----.*?-----END [A-Z0-9_\- ]+PRIVATE KEY-----`)

	// reURLCreds matches credentials embedded in URLs: http(s)://user:password@host
	reURLCreds = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)([^:/\s]+):(.+?)@([a-zA-Z0-9.\-_]+(?::[0-9]+)?(?:/|\?|#|\s|$))`)

	// reURLQuerySecrets matches sensitive URL query parameters (?token=..., &sig=..., &key=...)
	reURLQuerySecrets = regexp.MustCompile(`(?i)([?&](?:` + sensitiveKeyPattern + `|sig|signature|key|x-amz-signature|x-amz-security-token)=)([^&\s#"']+)`)

	// reBearer matches HTTP Authorization Bearer tokens
	reBearer = regexp.MustCompile(`(?i)\b(Bearer\s+)[A-Za-z0-9\-._~+/]+=*`)

	// reBasic matches HTTP Authorization Basic tokens
	reBasic = regexp.MustCompile(`(?i)\b(Basic\s+)[A-Za-z0-9+/]+=*`)

	// reJWT matches raw standalone JWT tokens (3 dot-separated base64url segments starting with eyJ)
	reJWT = regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]+`)

	// reJSONString matches sensitive fields in JSON with string values, including escaped quotes (\")
	reJSONString = regexp.MustCompile(`(?i)("(?:` + sensitiveKeyPattern + `)"\s*:\s*)"((?:\\.|[^"\\])*)"`)

	// reJSONUnquoted matches sensitive fields in JSON with unquoted values (numbers, booleans, literals)
	reJSONUnquoted = regexp.MustCompile(`(?i)("(?:` + sensitiveKeyPattern + `)"\s*:\s*)([^"\s,}\]][^\s,}\]]*)`)

	// reKVQuoted matches quoted key-value pairs (e.g. password="secret" or cookie='secret')
	reKVQuoted = regexp.MustCompile(`(?i)\b(` + kvSensitiveKeyPattern + `)\s*([=:])\s*(?:"((?:\\.|[^"\\])*)"|'([^']*)')`)

	// reKVUnquoted matches unquoted key-value pairs (e.g. password=secret or token: secret)
	reKVUnquoted = regexp.MustCompile(`(?i)\b(` + kvSensitiveKeyPattern + `)\s*([=:])\s*([^"'\[\s,;}&][^\s,;}&]*)`)

	sensitiveKeyExactOrSub = []string{
		"password", "passwd", "secret", "jwt", "token", "apikey", "api_key",
		"access_token", "refresh_token", "authorization", "pcsrftoken", "psession",
		"credential", "auth_token", "cookie", "set-cookie", "x-internal-secret",
		"internal_ops_token", "emergency_override_token", "session_id", "hmac",
		"encryption_key", "bitiful", "s3_secret", "totp", "otp", "private_key",
	}
)

// SensitiveString sanitizes sensitive information (passwords, tokens, cookies, API keys, JWTs, URL credentials)
// from arbitrary log text, JSON payloads, URLs, and stack traces.
func SensitiveString(input string) string {
	if input == "" {
		return ""
	}

	result := input
	result = rePEMKey.ReplaceAllString(result, "[REDACTED_PRIVATE_KEY]")
	result = reURLCreds.ReplaceAllString(result, "${1}${2}:[REDACTED]@${4}")
	result = reURLQuerySecrets.ReplaceAllString(result, "${1}[REDACTED]")
	result = reBearer.ReplaceAllString(result, "${1}[REDACTED]")
	result = reBasic.ReplaceAllString(result, "${1}[REDACTED]")
	result = reJWT.ReplaceAllString(result, "[REDACTED_JWT]")
	result = reJSONString.ReplaceAllString(result, `${1}"[REDACTED]"`)
	result = reJSONUnquoted.ReplaceAllString(result, `${1}"[REDACTED]"`)
	result = reKVQuoted.ReplaceAllString(result, `${1}${2}"[REDACTED]"`)
	result = reKVUnquoted.ReplaceAllString(result, `${1}${2}[REDACTED]`)
	return result
}

// SensitiveBytes applies SensitiveString on a byte slice.
func SensitiveBytes(input []byte) []byte {
	if len(input) == 0 {
		return input
	}
	return []byte(SensitiveString(string(input)))
}

// IsSensitiveKey returns true if a structured parameter key looks sensitive.
func IsSensitiveKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	if lower == "" {
		return false
	}
	for _, k := range sensitiveKeyExactOrSub {
		if lower == k || strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

// Value recursively redacts structured maps/slices and scrubs all nested string values.
func Value(v interface{}) interface{} {
	if v == nil {
		return nil
	}
	switch typed := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(typed))
		for k, val := range typed {
			if IsSensitiveKey(k) {
				out[k] = "[REDACTED]"
			} else {
				out[k] = Value(val)
			}
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(typed))
		for k, val := range typed {
			if IsSensitiveKey(k) {
				out[k] = "[REDACTED]"
			} else {
				out[k] = SensitiveString(val)
			}
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(typed))
		for i, item := range typed {
			out[i] = Value(item)
		}
		return out
	case string:
		return SensitiveString(typed)
	default:
		return typed
	}
}

// StructuredJSON attempts to parse raw JSON, redact all sensitive keys & nested strings cleanly,
// preserving UTF-8 symbols (<, >, &) and numeric precision without HTML escaping.
func StructuredJSON(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var parsed interface{}
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		if err := dec.Decode(&parsed); err == nil {
			redacted := Value(parsed)
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(redacted); err == nil {
				encoded := strings.TrimSuffix(buf.String(), "\n")
				return SensitiveString(encoded)
			}
		}
	}
	return SensitiveString(raw)
}

// EscapeCSVCell neutralizes spreadsheet formula injection (CWE-1236) on untrusted audit fields.
func EscapeCSVCell(cell string) string {
	sanitized := SensitiveString(cell)
	if sanitized != "" {
		switch sanitized[0] {
		case '=', '+', '-', '@', '\t', '\r', '\n':
			return "'" + sanitized
		}
		trimmedLeft := strings.TrimLeft(sanitized, " \t\r\n")
		if trimmedLeft != "" {
			switch trimmedLeft[0] {
			case '=', '+', '-', '@':
				return "'" + sanitized
			}
		}
	}
	return sanitized
}
