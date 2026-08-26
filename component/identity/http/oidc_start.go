package http

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/oidc"
)

// UPSTREAM SIGN-IN: THE TWO BROWSER ROUTES (memql#4611).
//
// -----------------------------------------------------------------------------
// WHY HTTP, AND WHY THESE TWO
// -----------------------------------------------------------------------------
//
// Both are documented HTTP exceptions of exactly the kind CLAUDE.md's table
// already lists for the identity service: "the other party dictates the wire".
// Here the other party is a BROWSER performing an OAuth redirect, and there is
// no gRPC form of "the user was sent to Microsoft and came back".
//
//   GET /auth/oidc/start     -> 302 to the provider's authorize endpoint
//   GET /auth/oidc/callback  <- the provider's redirect, carrying code + state
//
// -----------------------------------------------------------------------------
// WHAT IS IN THE COOKIE, AND WHY IT IS NOT IN THE URL
// -----------------------------------------------------------------------------
//
// `state`, `nonce` and the PKCE `code_verifier` are minted at /start and have to
// survive the round trip through the provider. They live in ONE short-lived,
// HttpOnly, SameSite=Lax cookie -- never in the URL, because the URL is handed
// to a third party and appears in its logs, and never in a server-side table,
// because a sign-in that has not started needs no row and a table of them is a
// table somebody has to expire.
//
// SameSite=Lax rather than Strict is load-bearing: the callback arrives as a
// TOP-LEVEL NAVIGATION from the provider's origin, and a Strict cookie is not
// sent on a cross-site navigation at all -- the flow would fail every time, for
// everybody, with a state mismatch that looks like an attack.
//
// The cookie is CLEARED on the callback whatever the outcome. A verifier that
// outlives its use is a second chance at a code that should have exactly one.

const (
	// oidcStateCookie holds state|nonce|verifier for one in-flight sign-in.
	oidcStateCookie = "memql_oidc"
	// oidcStateTTL bounds how long a started sign-in stays completable. Five
	// minutes is longer than any human takes to approve and shorter than any
	// window worth stealing.
	oidcStateTTL = 5 * time.Minute
)

// handleOIDCStart begins an upstream sign-in.
func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireSecureRequest(w, r) {
		return
	}
	cfg := s.Cfg.OIDC
	if !cfg.Enabled {
		// 404 rather than 400: on a cluster with no provider this route does
		// not exist, and saying so is both honest and quieter than describing
		// a feature that is off.
		http.NotFound(w, r)
		return
	}

	meta, err := s.oidcDiscoverer().Discover(r.Context(), cfg.ResolvedIssuer())
	if err != nil {
		s.writeJSONError(w, http.StatusBadGateway, "temporarily_unavailable",
			"the identity provider could not be reached: "+err.Error())
		return
	}
	if !meta.SupportsS256() {
		s.writeJSONError(w, http.StatusBadGateway, "temporarily_unavailable",
			"the identity provider does not advertise S256 PKCE")
		return
	}

	state, nonce, verifier := randToken(), randToken(), randToken()
	if state == "" || nonce == "" || verifier == "" {
		s.writeJSONError(w, http.StatusInternalServerError, "server_error", "could not mint sign-in state")
		return
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	authorizeURL, err := oidc.AuthorizeURL(meta, oidc.AuthorizeParams{
		ClientID:            cfg.ClientID,
		RedirectURI:         s.oidcRedirectURI(),
		Scopes:              cfg.Scopes,
		State:               state,
		Nonce:               nonce,
		CodeChallenge:       challenge,
		CodeChallengeMethod: "S256",
		DomainHint:          cfg.DomainHint,
		// The requesting page's own destination rides the cookie, not this.
		LoginHint: strings.TrimSpace(r.URL.Query().Get("login_hint")),
	})
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     oidcStateCookie,
		Value:    strings.Join([]string{state, nonce, verifier}, "|"),
		Path:     "/auth",
		HttpOnly: true,
		Secure:   s.cookieSecure(),
		// Lax, NOT Strict -- see the header. The callback is a top-level
		// navigation from the provider's origin, and Strict withholds the
		// cookie on exactly that, which would fail every sign-in.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(oidcStateTTL / time.Second),
	})
	http.Redirect(w, r, authorizeURL, http.StatusSeeOther)
}

// oidcRedirectURI is where the provider sends the browser back. Derived from
// the identity service's own base URL rather than from the request Host, for
// the reason the WebAuthn RP id is: a value taken from the request is a value
// an attacker chooses.
func (s *Server) oidcRedirectURI() string {
	base := strings.TrimRight(s.Cfg.BaseURL, "/")
	return base + "/auth/oidc/callback"
}

func (s *Server) oidcDiscoverer() *oidc.Discoverer {
	s.oidcOnce.Do(func() {
		s.oidcDisc = &oidc.Discoverer{}
	})
	return s.oidcDisc
}

// randToken mints a 32-byte URL-safe random value. "" on failure, which every
// caller treats as fatal rather than falling back to something guessable.
func randToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// readOIDCStateCookie returns (state, nonce, verifier) from the request.
func readOIDCStateCookie(r *http.Request) (string, string, string, bool) {
	c, err := r.Cookie(oidcStateCookie)
	if err != nil || c.Value == "" {
		return "", "", "", false
	}
	parts := strings.Split(c.Value, "|")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// clearOIDCStateCookie expires the in-flight cookie. Called on EVERY callback
// outcome: a verifier that outlives its use is a second chance at a code that
// should have exactly one.
func (s *Server) clearOIDCStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     oidcStateCookie,
		Value:    "",
		Path:     "/auth",
		HttpOnly: true,
		Secure:   s.cookieSecure(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// oidcRedirectToLogin sends the browser back to the sign-in page with a reason
// it can render. The reason is a STABLE TOKEN, not prose: the page owns the
// wording, and a token is what an operator greps for in the audit trail.
func (s *Server) oidcRedirectToLogin(w http.ResponseWriter, r *http.Request, reason string) {
	dest := "/login?oidc_error=" + url.QueryEscape(reason)
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// oidcEnabled reports whether an upstream provider is configured AND valid.
// Validate has already run at boot; this is the per-request read.
func (s *Server) oidcEnabled() bool {
	return s.Cfg.OIDC.Enabled && s.Cfg.OIDC.ResolvedIssuer() != "" && s.Cfg.OIDC.ClientID != ""
}

var _ = identity.ComponentName

// cookieSecure mirrors the web package's rule: the cookie is Secure iff this
// service is served over https. Stated from the CONFIGURED base URL rather than
// from the request, for the reason oidcRedirectURI gives.
func (s *Server) cookieSecure() bool {
	if s == nil {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s.Cfg.BaseURL)), "https://")
}
