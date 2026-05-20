package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRequireSecureRequest_RejectsPlaintextHTTP asserts that
// /pair/codes and /pair/redeem refuse to admit requests that did not
// arrive over TLS (or behind an HTTPS-terminating proxy).
//
// The pair-code endpoints carry plaintext credentials -- a Bearer JWT
// on /pair/codes and a Pair-scheme code on /pair/redeem, plus the
// minted plainCode in the response body. A plaintext hop here is a
// credential-leak surface; the HTTPS-required check (F5) closes it.
func TestRequireSecureRequest_RejectsPlaintextHTTP(t *testing.T) {
	t.Setenv(envAllowInsecurePair, "")
	s := &Server{}

	req := httptest.NewRequest(http.MethodPost, "http://example.test/pair/codes", strings.NewReader(`{}`))
	// No req.TLS, no X-Forwarded-Proto.
	rec := httptest.NewRecorder()

	if s.requireSecureRequest(rec, req) {
		t.Fatalf("requireSecureRequest admitted plaintext HTTP request")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if !strings.Contains(rec.Body.String(), "insecure_transport") {
		t.Fatalf("response body lacks insecure_transport errorCode: %q", rec.Body.String())
	}
}

// TestRequireSecureRequest_AdmitsForwardedHTTPS confirms an
// HTTPS-terminating reverse proxy (Cloud Run, Cloudflare, nginx, etc.)
// can hand a request through to the pair endpoints by setting
// X-Forwarded-Proto: https.
func TestRequireSecureRequest_AdmitsForwardedHTTPS(t *testing.T) {
	t.Setenv(envAllowInsecurePair, "")
	s := &Server{}

	req := httptest.NewRequest(http.MethodPost, "http://example.test/pair/codes", strings.NewReader(`{}`))
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()

	if !s.requireSecureRequest(rec, req) {
		t.Fatalf("requireSecureRequest rejected request with X-Forwarded-Proto=https; status=%d body=%q", rec.Code, rec.Body.String())
	}
}

// TestRequireSecureRequest_DevEscapeHatch verifies that
// MEMQL_IDENTITY_ALLOW_INSECURE_PAIR=1 admits plaintext for local
// development. Production deployments must leave the env var unset.
func TestRequireSecureRequest_DevEscapeHatch(t *testing.T) {
	t.Setenv(envAllowInsecurePair, "1")
	s := &Server{}

	req := httptest.NewRequest(http.MethodPost, "http://example.test/pair/redeem", strings.NewReader(``))
	rec := httptest.NewRecorder()

	if !s.requireSecureRequest(rec, req) {
		t.Fatalf("dev escape-hatch failed: status=%d body=%q", rec.Code, rec.Body.String())
	}
}
