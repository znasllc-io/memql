package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
)

// MintSSOAuthCodeFunc mints a one-time OAuth auth code from an
// already-authenticated user session. Used by redirectIfAuthenticated
// when a signed-in user lands on /login?return_to=<relying-party>:
// instead of asking them for their email a second time, the
// middleware bounces straight to the relying party with a fresh code.
//
// The wiring layer fills this in (component/identity/web doesn't
// import the engine). Nil is acceptable -- when nil, the middleware
// falls back to rendering the login form, same UX as before SSO
// landed.
type MintSSOAuthCodeFunc func(ctx context.Context, in MintSSOAuthCodeInput) (MintSSOAuthCodeResult, error)

// MintSSOAuthCodeInput is the payload the wiring layer needs to
// produce an auth-code row keyed off the existing session.
type MintSSOAuthCodeInput struct {
	UserId      string
	ClientId    string
	RedirectURI string
	State       string
	SourceIP    string
	UserAgent   string

	// CodeChallenge / CodeChallengeMethod carry the OAuth 2.1 PKCE
	// challenge (RFC 7636) when the SSO short-circuit is reached via
	// the /authorize endpoint (a PKCE-required client, e.g. the
	// claude.ai MCP connector). The adapter MUST persist these onto the
	// minted auth-code row so the client's /oauth/token exchange --
	// which presents the matching code_verifier -- validates against a
	// PKCE-bound code. Empty for the legacy product SPA SSO path
	// (no PKCE), which keeps minting a non-PKCE code exactly as before.
	CodeChallenge       string
	CodeChallengeMethod string
}

// MintSSOAuthCodeResult carries the plaintext code back to the
// middleware so it can build the redirect URL. The hash + persistence
// happen inside the adapter; only the plaintext leaves.
type MintSSOAuthCodeResult struct {
	Code string
}

// SetMintSSOAuthCode plumbs the SSO mint adapter onto the Server.
// Optional -- when nil, signed-in users hitting /login?return_to=...
// fall through to the login form. Wired by the integration layer
// once the engine + store are ready.
func (s *Server) SetMintSSOAuthCode(fn MintSSOAuthCodeFunc) {
	if s == nil {
		return
	}
	s.mintSSOAuthCode = fn
}

// hasValidSession checks the request for a valid memql_admin cookie
// (or Authorization: Bearer header) and returns true when the JWT
// verifies. Best-effort: returns false on any signal that the
// caller is unauthenticated -- missing token, invalid signature,
// expired, or unwired issuer dependency.
//
// This is the cheap "is the user signed in?" probe; it does NOT
// hit the database. If a future change wants to additionally check
// for revoked sessions, layer that on top -- the cookie check is
// the necessary-but-not-sufficient gate.
func (s *Server) hasValidSession(r *http.Request) bool {
	return s.sessionClaims(r) != nil
}

// sessionClaims is the same probe as hasValidSession but returns
// the verified claims for downstream use (SSO auth-code mint).
// Returns nil on any failure path.
func (s *Server) sessionClaims(r *http.Request) *identity.AccessTokenClaims {
	if s == nil || s.meTokens == nil || s.meTokens.Issuer == nil {
		return nil
	}
	raw := extractUserToken(r)
	if raw == "" {
		return nil
	}
	claims, err := s.meTokens.Issuer.VerifyAccessToken(raw, time.Now().UTC())
	if err != nil {
		return nil
	}
	return claims
}

// redirectIfAuthenticated wraps a pre-auth GET handler. When the
// caller already has a valid session, the wrapped handler is
// skipped and the browser is bounced to a destination chosen by
// the request shape:
//
//  1. No session -> wrapped handler runs (unchanged).
//  2. Session + no return_to -> redirect to `target` (typically
//     /admin/). This is the "I revisited /login while still signed
//     in" UX fix.
//  3. Session + return_to that matches a registered client's
//     redirect URI -> SSO short-circuit. Mint a fresh auth code
//     for THAT client + redirect to <return_to>?code=...&state=...
//     so the relying-party SPA exchanges it via /oauth/token. The
//     user never sees the email-entry form -- one click from
//     "AuthPortal" to "signed in to the product SPA".
//  4. Session + return_to that doesn't match any registered client
//     -> wrapped handler runs (form renders). The relying party
//     isn't trusted for SSO; user must confirm via the email round
//     trip.
//
// Use this wrapper for any GET endpoint that presents an
// "authenticate yourself" affordance (login form, setup wizard,
// landing page). POST handlers and authenticated /me/* surfaces
// are out of scope -- they already handle the signed-in case.
func (s *Server) redirectIfAuthenticated(target string, next http.HandlerFunc) http.HandlerFunc {
	if next == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		claims := s.sessionClaims(r)
		if claims == nil {
			next(w, r)
			return
		}
		// Pull OAuth context off the URL: client_id + redirect_uri
		// are the canonical params SPAs send (the product SPA +
		// any other relying party). return_to is kept for legacy
		// callers that stuffed the redirect URI in there.
		q := r.URL.Query()
		urlClientId := strings.TrimSpace(q.Get("client_id"))
		urlRedirectURI := strings.TrimSpace(q.Get("redirect_uri"))
		urlReturnTo := strings.TrimSpace(q.Get("return_to"))
		urlState := strings.TrimSpace(q.Get("state"))
		// PKCE challenge from an OAuth 2.1 /authorize flow (RFC 7636).
		// When present (PKCE-required client, e.g. the claude.ai MCP
		// connector hitting GET /authorize), it MUST be bound onto the
		// SSO-minted auth code so the /oauth/token exchange validates.
		// Absent on the legacy product SPA SSO path (/login?return_to=).
		urlCodeChallenge := strings.TrimSpace(q.Get("code_challenge"))
		urlCodeChallengeMethod := strings.TrimSpace(q.Get("code_challenge_method"))

		// No OAuth context anywhere -> this is the bare-revisit
		// case. Just bounce to the in-product target (typically
		// /admin/) so the user lands somewhere instead of seeing
		// the form unnecessarily.
		if urlClientId == "" && urlRedirectURI == "" && urlReturnTo == "" {
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		// SSO short-circuit. Try to match the OAuth params (or the
		// legacy return_to) against a registered client. Same
		// predicate the magic-link flow uses; matched=false means
		// no relying party in scope, so we fall through to the form
		// for the user to confirm.
		clientId, redirectURI, state, matched := s.pickOAuthCtx(r.Context(), urlClientId, urlRedirectURI, urlReturnTo, urlState)
		if !matched || s.mintSSOAuthCode == nil {
			next(w, r)
			return
		}
		mint, err := s.mintSSOAuthCode(r.Context(), MintSSOAuthCodeInput{
			UserId:      claims.Subject,
			ClientId:    clientId,
			RedirectURI: redirectURI,
			State:       state,
			SourceIP:    clientIP(r),
			UserAgent:   r.Header.Get("User-Agent"),
			// Bind PKCE when the /authorize flow supplied a challenge.
			// Empty for the product SPA SSO path (unchanged).
			CodeChallenge:       urlCodeChallenge,
			CodeChallengeMethod: urlCodeChallengeMethod,
		})
		if err != nil {
			if s.Logger != nil {
				s.Logger.Warn("identity-web: SSO auth-code mint failed; falling back to form",
					"error", err, "client_id", clientId, "user_id", claims.Subject)
			}
			next(w, r)
			return
		}
		http.Redirect(w, r, buildSSORedirect(redirectURI, mint.Code, state), http.StatusSeeOther)
	}
}

// buildSSORedirect appends ?code=...&state=... to the registered
// redirect URI. Mirrors http.buildClientCallback (same package
// can't be imported because of a cycle; the surface is small).
func buildSSORedirect(redirectURI, code, state string) string {
	u, err := url.Parse(redirectURI)
	if err != nil {
		// Fall back to a clearly-broken URL rather than building a
		// silently wrong one. The caller logs the upstream error
		// and the SPA reports a token_exchange_failed if it ever
		// receives this.
		return fmt.Sprintf("%s?code=%s&state=%s", redirectURI, url.QueryEscape(code), url.QueryEscape(state))
	}
	q := u.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	return u.String()
}
