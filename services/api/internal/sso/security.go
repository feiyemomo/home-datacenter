package sso

import (
	"net/http"
	"net/url"
	"strings"
)

var AllowedRedirectPrefixes = []string{
	"/btpanel",
	"/btpanel/",
	"/btpanel/#/",
}

// ValidateRedirectTarget prevents Open Redirect attacks by validating destination against whitelist.
func ValidateRedirectTarget(target string) string {
	if target == "" {
		return "/btpanel"
	}

	// Must be relative path and start with allowed prefix
	// Defend against scheme-relative URLs (//attacker.com), Windows-style path separators (/\attacker.com), etc.
	if strings.HasPrefix(target, "http://") ||
		strings.HasPrefix(target, "https://") ||
		strings.HasPrefix(target, "//") ||
		strings.HasPrefix(target, "/\\") ||
		strings.HasPrefix(target, "\\") {
		return "/btpanel"
	}

	// Check against whitelist prefixes
	for _, allowed := range AllowedRedirectPrefixes {
		if strings.HasPrefix(target, allowed) {
			// Additional safety: ensure no embedded protocol before query/fragment
			lower := strings.ToLower(target)
			if strings.Contains(lower, "://") {
				return "/btpanel"
			}
			return target
		}
	}

	return "/btpanel"
}

// ValidateOrigin validates the request Origin or Referer against allowed patterns.
func ValidateOrigin(r *http.Request, allowedOrigins []string) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		if referer := r.Header.Get("Referer"); referer != "" {
			if u, err := url.Parse(referer); err == nil {
				origin = u.Scheme + "://" + u.Host
			}
		}
	}

	if origin == "" {
		// Standard same-origin browser form navigation may omit Origin/Referer in some browsers
		return true
	}

	parsedOrigin, err := url.Parse(origin)
	if err != nil {
		return false
	}

	originHost := parsedOrigin.Hostname()

	for _, allowed := range allowedOrigins {
		if strings.EqualFold(origin, allowed) {
			return true
		}
		if u, err := url.Parse(allowed); err == nil {
			if strings.EqualFold(originHost, u.Hostname()) {
				return true
			}
		}
	}

	// Allowed development, local network, and authorized domain patterns
	if originHost == "localhost" ||
		originHost == "127.0.0.1" ||
		strings.HasPrefix(originHost, "192.168.") ||
		strings.HasPrefix(originHost, "10.") ||
		strings.HasPrefix(originHost, "172.18.") ||
		strings.HasPrefix(originHost, "172.17.") ||
		strings.HasSuffix(originHost, ".feiyemomo.top") ||
		originHost == "feiyemomo.top" {
		return true
	}

	return false
}
