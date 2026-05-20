package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCORS_NoCredentialsWithWildcard asserts the audit-driven fix:
// when the allow-list contains `*`, the middleware must NOT emit
// `Access-Control-Allow-Credentials: true`. Pairing wildcard
// origin + credentials is a CORS-spec violation that browsers
// reject; the older behaviour echoed the caller's Origin alongside
// credentials, which is a credentialed-XHR exfiltration surface.
func TestCORS_NoCredentialsWithWildcard(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := corsMiddleware([]string{"*"}, inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://attacker.example.com")
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://attacker.example.com" {
		t.Errorf("Allow-Origin = %q, want echoed caller origin (wildcard mode)", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("Allow-Credentials must NOT be set when allow-list is wildcard, got %q", got)
	}
}

// TestCORS_CredentialsWithExplicitAllowlist asserts the inverse:
// when the allow-list is explicit, credentials are emitted (the
// legitimate credentialed-XHR use case).
func TestCORS_CredentialsWithExplicitAllowlist(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := corsMiddleware([]string{"https://app.example.com"}, inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("Allow-Origin = %q, want explicit allowed origin", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Allow-Credentials = %q, want true under explicit allow-list", got)
	}
}

// TestCORS_DisallowedOriginGetsNothing — origins NOT on the
// explicit allow-list get no CORS headers at all; the browser
// will block the request itself.
func TestCORS_DisallowedOriginGetsNothing(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := corsMiddleware([]string{"https://app.example.com"}, inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, want absent for disallowed origin", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Allow-Credentials = %q, want absent", got)
	}
}
