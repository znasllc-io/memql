package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
)

// newAuthorizeTestServer builds a minimal *Server with one registered
// OAuth client whose only allowed redirect_uri is the loopback callback
// the cockpit / a desktop OAuth client would use.
func newAuthorizeTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := identity.Config{
		BaseURL: "http://localhost:8080",
		RegisteredClients: []identity.RegisteredClient{
			{
				ClientId:     "claude-desktop",
				RedirectURIs: []string{"http://127.0.0.1:7777/callback"},
			},
		},
	}
	return &Server{
		Cfg:    cfg,
		Logger: slog.Default(),
	}
}

const (
	validChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	clientID       = "claude-desktop"
	redirectURI    = "http://127.0.0.1:7777/callback"
)

func authorizeURL(params map[string]string) string {
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	return "/authorize?" + q.Encode()
}

func TestAuthorize_ValidParams_RendersConsentPage(t *testing.T) {
	s := newAuthorizeTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, authorizeURL(map[string]string{
		"response_type":         "code",
		"client_id":             clientID,
		"redirect_uri":          redirectURI,
		"state":                 "xyz-state",
		"code_challenge":        validChallenge,
		"code_challenge_method": "S256",
		"scope":                 "openid",
		"resource":              "https://mcp.example.com",
	}), nil)

	s.handleAuthorize(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("expected no Location header on success, got %q", loc)
	}
	body := rec.Body.String()
	if !strings.Contains(body, clientID) {
		t.Errorf("consent page should name the client_id %q; body=%s", clientID, body)
	}
	// The hidden form field carrying the PKCE challenge must round-trip.
	if !strings.Contains(body, `name="code_challenge"`) || !strings.Contains(body, validChallenge) {
		t.Errorf("consent page must carry the code_challenge hidden field with value %q; body=%s", validChallenge, body)
	}
	if !strings.Contains(body, `action="/login"`) {
		t.Errorf("consent page should post to /login; body=%s", body)
	}
}

func TestAuthorize_UnknownClient_400NoRedirect(t *testing.T) {
	s := newAuthorizeTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, authorizeURL(map[string]string{
		"response_type":         "code",
		"client_id":             "not-registered",
		"redirect_uri":          redirectURI,
		"code_challenge":        validChallenge,
		"code_challenge_method": "S256",
	}), nil)

	s.handleAuthorize(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("unknown client must NOT redirect; got Location=%q", loc)
	}
}

func TestAuthorize_DisallowedRedirectURI_400NoRedirect(t *testing.T) {
	s := newAuthorizeTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, authorizeURL(map[string]string{
		"response_type":         "code",
		"client_id":             clientID,
		"redirect_uri":          "https://evil.example.com/callback",
		"code_challenge":        validChallenge,
		"code_challenge_method": "S256",
	}), nil)

	s.handleAuthorize(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("disallowed redirect_uri must NOT redirect; got Location=%q", loc)
	}
}

func TestAuthorize_ResponseTypeToken_RedirectsWithError(t *testing.T) {
	s := newAuthorizeTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, authorizeURL(map[string]string{
		"response_type":         "token",
		"client_id":             clientID,
		"redirect_uri":          redirectURI,
		"state":                 "st8",
		"code_challenge":        validChallenge,
		"code_challenge_method": "S256",
	}), nil)

	s.handleAuthorize(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location not parseable: %v", err)
	}
	if !strings.HasPrefix(loc, redirectURI) {
		t.Errorf("redirect target = %q, want prefix %q", loc, redirectURI)
	}
	if got := u.Query().Get("error"); got != "unsupported_response_type" {
		t.Errorf("error = %q, want unsupported_response_type", got)
	}
	if got := u.Query().Get("state"); got != "st8" {
		t.Errorf("state = %q, want st8", got)
	}
}

func TestAuthorize_MissingCodeChallenge_RedirectsWithInvalidRequest(t *testing.T) {
	s := newAuthorizeTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, authorizeURL(map[string]string{
		"response_type": "code",
		"client_id":     clientID,
		"redirect_uri":  redirectURI,
		"state":         "st9",
	}), nil)

	s.handleAuthorize(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	u, _ := url.Parse(rec.Header().Get("Location"))
	if got := u.Query().Get("error"); got != "invalid_request" {
		t.Errorf("error = %q, want invalid_request", got)
	}
}

func TestAuthorize_PlainMethod_RedirectsWithInvalidRequest(t *testing.T) {
	s := newAuthorizeTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, authorizeURL(map[string]string{
		"response_type":         "code",
		"client_id":             clientID,
		"redirect_uri":          redirectURI,
		"code_challenge":        validChallenge,
		"code_challenge_method": "plain",
	}), nil)

	s.handleAuthorize(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	u, _ := url.Parse(rec.Header().Get("Location"))
	if got := u.Query().Get("error"); got != "invalid_request" {
		t.Errorf("error = %q, want invalid_request", got)
	}
}

// Guard against a future refactor dropping the LayoutData closure: a nil
// Live snapshot must still render (the consent page does not require the
// live-settings backend).
func TestAuthorize_NilLiveSettingsStillRenders(t *testing.T) {
	s := newAuthorizeTestServer(t)
	if s.Live != nil {
		t.Fatalf("expected nil Live in test server")
	}
	// Exercise snapshotSettings indirectly through the success path.
	_ = context.Background()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, authorizeURL(map[string]string{
		"response_type":         "code",
		"client_id":             clientID,
		"redirect_uri":          redirectURI,
		"code_challenge":        validChallenge,
		"code_challenge_method": "S256",
	}), nil)
	s.handleAuthorize(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
