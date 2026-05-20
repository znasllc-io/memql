package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteSafeError_ResponseShape(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	req := httptest.NewRequest(http.MethodGet, "/some/path", nil)
	rec := httptest.NewRecorder()

	innerErr := io.ErrUnexpectedEOF
	WriteSafeError(rec, req, logger, http.StatusBadRequest, "bad_request", "invalid request", innerErr)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}

	var body errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if body.Code != "bad_request" {
		t.Errorf("code = %q, want %q", body.Code, "bad_request")
	}
	if body.Message != "invalid request" {
		t.Errorf("message = %q, want %q", body.Message, "invalid request")
	}
	if !strings.HasPrefix(body.ErrorId, "ERR-") {
		t.Errorf("error_id = %q, want ERR-* prefix", body.ErrorId)
	}
	if strings.Contains(body.Message, innerErr.Error()) {
		t.Fatalf("envelope leaks inner err: %q", body.Message)
	}
}

func TestWriteSafeError_NoInnerErrInResponse(t *testing.T) {
	// Reinforce the central invariant: even when the inner err string
	// contains highly distinctive content (a DB column name, a project
	// id, etc.), nothing of that string appears in the wire response.
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()

	innerErr := io.EOF
	WriteSafeError(rec, req, nil, http.StatusInternalServerError, "internal", "internal server error", innerErr)

	if strings.Contains(rec.Body.String(), "EOF") {
		t.Fatalf("response leaks inner err: %q", rec.Body.String())
	}
}

func TestPanicRecoveryMiddleware_CatchesPanic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mw := PanicRecoveryMiddleware(logger)

	// Inner handler panics with a value that, if leaked, would be
	// obviously distinctive in the response body.
	inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("SECRET-CANARY-do-not-leak")
	})

	wrapped := mw(inner)
	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rec := httptest.NewRecorder()

	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	body := rec.Body.String()
	if strings.Contains(body, "SECRET-CANARY") {
		t.Fatalf("response leaks panic value: %q", body)
	}

	var envelope errorEnvelope
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Code != "internal" {
		t.Errorf("code = %q, want internal", envelope.Code)
	}
	if !strings.HasPrefix(envelope.ErrorId, "ERR-") {
		t.Errorf("error_id = %q, want ERR-* prefix", envelope.ErrorId)
	}
}

func TestPanicRecoveryMiddleware_PassesThroughNonPanic(t *testing.T) {
	mw := PanicRecoveryMiddleware(nil)

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	wrapped := mw(inner)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "ok")
	}
}
