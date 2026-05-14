package integrations

import (
	"errors"
	"fmt"
)

// Sentinel errors for integrations.
var (
	// ErrIntegrationNotConfigured indicates the integration was not properly initialized.
	ErrIntegrationNotConfigured = errors.New("integration not configured")

	// ErrMissingCredentials indicates required credentials are missing.
	ErrMissingCredentials = errors.New("missing required credentials")

	// ErrInvalidConfiguration indicates the configuration is invalid.
	ErrInvalidConfiguration = errors.New("invalid configuration")

	// ErrConnectionFailed indicates a connection attempt failed.
	ErrConnectionFailed = errors.New("connection failed")

	// ErrRequestFailed indicates an API request failed.
	ErrRequestFailed = errors.New("request failed")

	// ErrRateLimited indicates the request was rate limited.
	ErrRateLimited = errors.New("rate limited")

	// ErrSessionClosed indicates the session has been closed.
	ErrSessionClosed = errors.New("session closed")

	// ErrUnsupportedOperation indicates the operation is not supported.
	ErrUnsupportedOperation = errors.New("unsupported operation")
)

// HTTPError represents an HTTP error with status code.
type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("HTTP %d", e.StatusCode)
}

// IsRetryable returns true if the error indicates a retryable condition.
func (e *HTTPError) IsRetryable() bool {
	return e.StatusCode >= 500 || e.StatusCode == 429
}

// APIError represents an error from an external API.
type APIError struct {
	Provider string
	Code     string
	Message  string
	Details  map[string]any
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s API error [%s]: %s", e.Provider, e.Code, e.Message)
	}
	return fmt.Sprintf("%s API error: %s", e.Provider, e.Message)
}

// ValidationError represents a validation failure.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation error on %s: %s", e.Field, e.Message)
}
