package web

// Tests for the SSO-aware /authorize path (memql#1556).
//
// The real staging failure: a signed-in cluster owner connecting the
// claude.ai MCP connector hit GET /authorize, which (before the fix) was
// the ONLY pre-auth GET surface NOT wrapped with preAuth
// (redirectIfAuthenticated). So instead of the SSO short-circuit it
// rendered the email/login form; the owner then went through the
// magic-link path which dropped the OAuth context and landed on /admin/
// (magic_link_issued {"clientId":"","redirectURI":""}).
//
// Fix part 1 (server.go): wrap GET /authorize with preAuth so a signed-in
// user with OAuth context on the URL hits the SSO short-circuit.
//
// Fix part 2 (redirect_authenticated.go + the mint adapter): thread the
// PKCE code_challenge/code_challenge_method through the SSO mint so the
// minted auth code is PKCE-bound -- a PKCE-required connector's
// /oauth/token exchange (which sends code_verifier) validates against it.
//
// These tests drive preAuth = redirectIfAuthenticated("/admin/", ...)
// directly (that's exactly what server.go's `preAuth` closure produces)
// so they exercise the wrapped /authorize without standing up the full mux.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity"
)

// RFC 7636 Appendix B example challenge (for verifier
// dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk). The matching verifier is
// exercised end-to-end in the http package's token_sso_pkce_test.go.
const ssoCodeChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"

// signedInServer returns a *Server wired with a real JWTIssuer + a DCR
// client in the Store, plus a helper that stamps a valid memql_admin
// cookie (a freshly signed access token) onto a request so
// hasValidSession / sessionClaims treat it as authenticated.
func signedInServer(t *testing.T) (*Server, func(r *http.Request, userId string)) {
	t.Helper()

	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	if err != nil {
		t.Fatalf("NewKeyManager: %v", err)
	}
	if err := km.Load(); err != nil {
		t.Fatalf("KeyManager.Load: %v", err)
	}
	cfg := identity.Config{
		Enabled:     true,
		BaseURL:     "https://identity.test",
		JWTAudience: "memql",
		KeyDir:      dir,
	}
	iss, err := identity.NewJWTIssuer(km, cfg)
	if err != nil {
		t.Fatalf("NewJWTIssuer: %v", err)
	}

	store := &identity.Store{
		Engine: &dcrFakeEngine{
			clientId:     dcrClientID,
			redirectURIs: `["` + dcrRedirectURI + `"]`,
		},
	}

	s := &Server{
		Cfg:    cfg,
		Store:  store,
		Logger: slog.Default(),
	}
	s.SetMeTokens(&MeTokens{Issuer: iss})

	signIn := func(r *http.Request, userId string) {
		tok, _, err := iss.IssueAccessToken(identity.IssueInput{
			UserId: userId,
			Email:  "owner@example.com",
			Role:   "owner",
		}, time.Now().UTC())
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}
		r.AddCookie(&http.Cookie{Name: "memql_admin", Value: tok})
	}
	return s, signIn
}

// TestAuthorize_SignedInUser_SSOShortCircuit is the core acceptance test.
// A signed-in user hits the (preAuth-wrapped) /authorize with a DCR
// client + PKCE; the response must 303 straight to the connector's
// redirect_uri?code=...&state=..., NOT render the login form, NOT /admin.
func TestAuthorize_SignedInUser_SSOShortCircuit(t *testing.T) {
	s, signIn := signedInServer(t)

	var captured MintSSOAuthCodeInput
	s.SetMintSSOAuthCode(func(_ context.Context, in MintSSOAuthCodeInput) (MintSSOAuthCodeResult, error) {
		captured = in
		return MintSSOAuthCodeResult{Code: "SSO-CODE-123"}, nil
	})

	// preAuth is exactly redirectIfAuthenticated("/admin/", handler).
	wrapped := s.redirectIfAuthenticated("/admin/", s.handleAuthorize)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, authorizeURL(map[string]string{
		"response_type":         "code",
		"client_id":             dcrClientID,
		"redirect_uri":          dcrRedirectURI,
		"state":                 dcrOAuthState,
		"code_challenge":        ssoCodeChallenge,
		"code_challenge_method": "S256",
	}), nil)
	signIn(req, "v1:identity:user:owner1")

	wrapped(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (SSO short-circuit); body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location not parseable: %v", err)
	}
	if !strings.HasPrefix(loc, dcrRedirectURI) {
		t.Errorf("redirect target = %q, want prefix %q (NOT the login form, NOT /admin)", loc, dcrRedirectURI)
	}
	if loc == "/admin/" {
		t.Errorf("signed-in OAuth /authorize must NOT land on /admin/")
	}
	if got := u.Query().Get("code"); got != "SSO-CODE-123" {
		t.Errorf("code = %q, want SSO-CODE-123", got)
	}
	if got := u.Query().Get("state"); got != dcrOAuthState {
		t.Errorf("state = %q, want %q", got, dcrOAuthState)
	}

	// The minted code MUST be PKCE-bound for a PKCE-required connector.
	if captured.CodeChallenge != ssoCodeChallenge {
		t.Errorf("MintSSOAuthCodeInput.CodeChallenge = %q, want %q (PKCE not bound -> /oauth/token would not be PKCE-protected)", captured.CodeChallenge, ssoCodeChallenge)
	}
	if captured.CodeChallengeMethod != "S256" {
		t.Errorf("MintSSOAuthCodeInput.CodeChallengeMethod = %q, want S256", captured.CodeChallengeMethod)
	}
	if captured.ClientId != dcrClientID {
		t.Errorf("MintSSOAuthCodeInput.ClientId = %q, want %q", captured.ClientId, dcrClientID)
	}
	if captured.RedirectURI != dcrRedirectURI {
		t.Errorf("MintSSOAuthCodeInput.RedirectURI = %q, want %q", captured.RedirectURI, dcrRedirectURI)
	}
}

// TestAuthorize_UnauthenticatedUser_FallsThroughToForm confirms the
// preAuth wrapper does NOT break the unauthenticated path: with no
// session, the wrapped handler (handleAuthorize) runs and renders the
// consent/login form with the hidden OAuth fields.
func TestAuthorize_UnauthenticatedUser_FallsThroughToForm(t *testing.T) {
	s, _ := signedInServer(t) // built with an Issuer, but we DON'T sign in

	wrapped := s.redirectIfAuthenticated("/admin/", s.handleAuthorize)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, authorizeURL(map[string]string{
		"response_type":         "code",
		"client_id":             dcrClientID,
		"redirect_uri":          dcrRedirectURI,
		"state":                 dcrOAuthState,
		"code_challenge":        ssoCodeChallenge,
		"code_challenge_method": "S256",
	}), nil)
	// no signIn() -> no cookie -> unauthenticated

	wrapped(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (consent form); body=%.300s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`name="client_id"`, `name="redirect_uri"`, dcrClientID, dcrRedirectURI} {
		if !strings.Contains(body, want) {
			t.Errorf("consent form missing %q", want)
		}
	}
}

// TestAuthorize_SignedInUser_NoMintAdapterFallsThrough verifies that when
// the SSO mint adapter is unwired, a signed-in /authorize falls through
// to the consent form rather than erroring -- the magic-link path then
// preserves OAuth context (the #1586 fix) as the backstop.
func TestAuthorize_SignedInUser_NoMintAdapterFallsThrough(t *testing.T) {
	s, signIn := signedInServer(t)
	// Do NOT wire mintSSOAuthCode.

	wrapped := s.redirectIfAuthenticated("/admin/", s.handleAuthorize)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, authorizeURL(map[string]string{
		"response_type":         "code",
		"client_id":             dcrClientID,
		"redirect_uri":          dcrRedirectURI,
		"state":                 dcrOAuthState,
		"code_challenge":        ssoCodeChallenge,
		"code_challenge_method": "S256",
	}), nil)
	signIn(req, "v1:identity:user:owner1")

	wrapped(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fall through to consent form when no mint adapter); body=%.300s", rec.Code, rec.Body.String())
	}
}

// TestSSO_CoPresentPathNoPKCEUnchanged guards the legacy CoPresent SPA SSO
// path: a signed-in /login?client_id=...&redirect_uri=... WITHOUT a
// code_challenge must still mint a (non-PKCE) code -- CodeChallenge stays
// empty, behaviour unchanged. (CoPresent registered as a static client.)
func TestSSO_CoPresentPathNoPKCEUnchanged(t *testing.T) {
	dir := t.TempDir()
	km, err := identity.NewKeyManager(dir, "")
	if err != nil {
		t.Fatalf("NewKeyManager: %v", err)
	}
	if err := km.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg := identity.Config{
		Enabled:     true,
		BaseURL:     "https://identity.test",
		JWTAudience: "memql",
		KeyDir:      dir,
		RegisteredClients: []identity.RegisteredClient{
			{ClientId: "copresent", RedirectURIs: []string{"https://app.copresent.ai/auth/callback"}},
		},
	}
	iss, err := identity.NewJWTIssuer(km, cfg)
	if err != nil {
		t.Fatalf("NewJWTIssuer: %v", err)
	}
	s := &Server{Cfg: cfg, Logger: slog.Default()}
	s.SetMeTokens(&MeTokens{Issuer: iss})

	var captured MintSSOAuthCodeInput
	s.SetMintSSOAuthCode(func(_ context.Context, in MintSSOAuthCodeInput) (MintSSOAuthCodeResult, error) {
		captured = in
		return MintSSOAuthCodeResult{Code: "CP-CODE"}, nil
	})

	wrapped := s.redirectIfAuthenticated("/admin/", s.handleLoginGet)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/login?"+url.Values{
		"client_id":    {"copresent"},
		"redirect_uri": {"https://app.copresent.ai/auth/callback"},
		"state":        {"cp-state"},
	}.Encode(), nil)
	tok, _, err := iss.IssueAccessToken(identity.IssueInput{UserId: "v1:identity:user:cp1", Email: "u@copresent.ai", Role: "writer"}, time.Now().UTC())
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: "memql_admin", Value: tok})

	wrapped(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (CoPresent SSO); body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://app.copresent.ai/auth/callback") {
		t.Errorf("CoPresent SSO redirect = %q, want the registered callback", loc)
	}
	// No code_challenge on the URL -> the minted code stays non-PKCE,
	// exactly as before this change.
	if captured.CodeChallenge != "" {
		t.Errorf("CoPresent SSO must not bind a PKCE challenge; got %q", captured.CodeChallenge)
	}
}
