package server

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// errorEnvelope is the canonical shape memQL writes for HTTP error responses.
// Mirrors the gRPC QueryError envelope: a short stable code, a safe message,
// and an error_id that operators can grep server-side logs for. The handler
// logs the full error against the error_id; the response surfaces only the
// id, never the underlying err.Error() string.
type errorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	ErrorId string `json:"error_id"`
}

// newErrorId returns a 6-hex-char tag prefixed `ERR-`, matching the gRPC
// generateErrorId() helper. The two surfaces share the format so operators
// can grep one log shipper for either.
func newErrorId() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("ERR-%x", b)
}

// WriteSafeError emits a structured error envelope to the response writer.
// The full err is logged server-side against the generated error id; only
// the safe code + message + id reach the client. Callers should not pass
// err.Error() into `message` — that's the whole point of the envelope.
//
// If logger is nil the helper still works (drops the server-side log entry);
// the client surface is unchanged.
func WriteSafeError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, statusCode int, code, message string, err error) {
	eid := newErrorId()
	if logger != nil {
		logger.Error("http error",
			"errorId", eid,
			"status", statusCode,
			"code", code,
			"message", message,
			"path", r.URL.Path,
			"method", r.Method,
			"error", err,
		)
	}
	body := errorEnvelope{Code: code, Message: message, ErrorId: eid}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(&body)
}

// PanicRecoveryMiddleware wraps `next` so that any panic from a downstream
// handler is recovered, logged with its stack trace, and surfaced to the
// client as a safe 500 envelope. The panic value never reaches the wire.
//
// The recovery layer is the outermost concern in the chain. Place it before
// auth + CORS so a panic in any inner layer is still caught.
func PanicRecoveryMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					eid := newErrorId()
					if logger != nil {
						logger.Error("http handler panic recovered",
							"errorId", eid,
							"panic", fmt.Sprintf("%v", rec),
							"path", r.URL.Path,
							"method", r.Method,
							"stack", string(debug.Stack()),
						)
					}
					// If the handler already wrote response headers we
					// can't surface a clean envelope; the best we can do
					// is fail closed.
					body := errorEnvelope{
						Code:    "internal",
						Message: "internal server error",
						ErrorId: eid,
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_ = json.NewEncoder(w).Encode(&body)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
