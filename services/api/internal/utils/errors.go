package utils

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Sentinel errors for common handler failure modes. Handlers can return
// one of these (or wrap it) and let RespondError map it to an HTTP status
// instead of hardcoding a status code at every call site.
var (
	ErrBadRequest   = errors.New("bad request")
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrNotFound     = errors.New("resource not found")
	ErrConflict     = errors.New("resource conflict")
	ErrBadGateway   = errors.New("upstream error")
)

// APIError carries an explicit HTTP status with a client-safe message
// and an optional underlying cause. Use NewAPIError to build one.
type APIError struct {
	Status  int
	Message string
	Cause   error
}

func (e *APIError) Error() string {
	if e.Cause != nil {
		return e.Message + ": " + e.Cause.Error()
	}
	return e.Message
}

// Unwrap exposes the cause so errors.Is / errors.As work through it.
func (e *APIError) Unwrap() error { return e.Cause }

// NewAPIError builds a status-carrying error. Pass a nil cause for a
// self-contained error.
func NewAPIError(status int, message string, cause error) error {
	return &APIError{Status: status, Message: message, Cause: cause}
}

// StatusFor maps an error to an HTTP status. It recognizes APIError and
// the sentinel errors above; anything else defaults to 500.
func StatusFor(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	switch {
	case errors.Is(err, ErrBadRequest):
		return http.StatusBadRequest
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrConflict):
		return http.StatusConflict
	case errors.Is(err, ErrBadGateway):
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// MessageFor extracts a client-safe message from an error. For APIError
// it returns the explicit message; otherwise the error string.
func MessageFor(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Message
	}
	return err.Error()
}

// RespondError writes the unified {code,message,data:null} error envelope
// for err, mapping it to an HTTP status via StatusFor. Use it in handlers
// to replace manual hardcoded status codes.
func RespondError(c *gin.Context, err error) {
	Fail(c, StatusFor(err), MessageFor(err))
}