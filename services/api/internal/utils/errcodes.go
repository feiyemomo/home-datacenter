package utils

// Stable, machine-readable auth error codes returned in the
// `error_code` field of the response envelope (see FailWithCode).
//
// Clients (Android app, web SPA) MUST branch on these values rather
// than on the free-text `message`, which may be reworded at any time.
// Values are part of the public API contract: never renumber or reuse.
//
// Semantics for clients:
//   - ErrAuthTokenInvalid / ErrAuthTokenVersionMismatch:
//     recoverable — silently re-bind with the stored access_key.
//   - ErrAuthMissing / ErrAuthDeviceNotFound / ErrAuthDeviceRevoked /
//     ErrAuthUserNotFound / ErrAuthInvalidCredentials:
//     fatal — clear local auth and send the user to the login screen.
//   - ErrAuthDeviceLookupFailed: server-side DB error — transient,
//     must NOT log the user out.
const (
	ErrAuthMissing              = 40101 // no Authorization header / cookie / token
	ErrAuthTokenInvalid         = 40102 // JWT failed to parse / verify / expired
	ErrAuthDeviceNotFound       = 40103 // device row deleted
	ErrAuthDeviceRevoked        = 40104 // device revoked by admin
	ErrAuthTokenVersionMismatch = 40105 // admin rotated token_version
	ErrAuthDeviceLookupFailed   = 40106 // transient DB error during device lookup
	ErrAuthInvalidCredentials   = 40107 // bind rejected (user_id / access_key)
	ErrAuthUserNotFound         = 40108 // user row missing / lookup failed
)
