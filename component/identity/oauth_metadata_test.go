package identity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOAuthServerMetadataHandler(t *testing.T) {
	const base = "https://identity.example.com"
	h := OAuthServerMetadataHandler(Config{BaseURL: base})

	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	var doc OAuthServerMetadata
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("body not JSON: %v; body=%s", err, rec.Body.String())
	}

	if doc.Issuer != base {
		t.Errorf("issuer = %q, want %q", doc.Issuer, base)
	}
	if want := base + "/authorize"; doc.AuthorizationEndpoint != want {
		t.Errorf("authorization_endpoint = %q, want %q", doc.AuthorizationEndpoint, want)
	}
	if want := base + "/oauth/token"; doc.TokenEndpoint != want {
		t.Errorf("token_endpoint = %q, want %q", doc.TokenEndpoint, want)
	}
	if want := base + "/register"; doc.RegistrationEndpoint != want {
		t.Errorf("registration_endpoint = %q, want %q", doc.RegistrationEndpoint, want)
	}
	if want := base + "/.well-known/jwks.json"; doc.JWKSURI != want {
		t.Errorf("jwks_uri = %q, want %q", doc.JWKSURI, want)
	}

	if !containsString(doc.CodeChallengeMethodsSupported, "S256") {
		t.Errorf("code_challenge_methods_supported = %v, want it to contain S256", doc.CodeChallengeMethodsSupported)
	}
}

// TestOAuthServerMetadataHandler_TrailingSlash asserts a BaseURL with a
// trailing slash does not produce double slashes in the derived absolute URLs.
func TestOAuthServerMetadataHandler_TrailingSlash(t *testing.T) {
	h := OAuthServerMetadataHandler(Config{BaseURL: "https://identity.example.com/"})

	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var doc OAuthServerMetadata
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}

	const wantBase = "https://identity.example.com"
	for name, got := range map[string]string{
		"authorization_endpoint": doc.AuthorizationEndpoint,
		"token_endpoint":         doc.TokenEndpoint,
		"registration_endpoint":  doc.RegistrationEndpoint,
		"jwks_uri":               doc.JWKSURI,
	} {
		// None of the endpoint URLs should contain a double slash after the scheme.
		rest := got[len("https://"):]
		if hasDoubleSlash(rest) {
			t.Errorf("%s = %q has a double slash", name, got)
		}
	}
	if doc.TokenEndpoint != wantBase+"/oauth/token" {
		t.Errorf("token_endpoint = %q, want %q", doc.TokenEndpoint, wantBase+"/oauth/token")
	}
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func hasDoubleSlash(s string) bool {
	for i := 1; i < len(s); i++ {
		if s[i] == '/' && s[i-1] == '/' {
			return true
		}
	}
	return false
}
