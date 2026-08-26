package http

import (
	"net/http"
	"strings"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/oidc"
)

// THE CALLBACK: where an upstream identity becomes a MemQL session (memql#4611).
//
// -----------------------------------------------------------------------------
// THE ORDER OF CHECKS IS THE SECURITY OF THIS FILE
// -----------------------------------------------------------------------------
//
//  1. The cookie must exist. No cookie means this browser never STARTED a
//     sign-in, so whatever it is carrying was minted for somebody else.
//  2. `state` must match the cookie's. This is the CSRF check: it proves the
//     callback belongs to the sign-in this browser began.
//  3. The code is exchanged with the PKCE verifier from the cookie.
//  4. The id token is verified -- signature, issuer, audience, expiry, nonce.
//     Nothing before this point has established WHO anybody is.
//  5. Only then is the claim set turned into a linking decision.
//
// The cookie is cleared on EVERY path out of here, success or not: a verifier
// that outlives its use is a second chance at a code that should have one.
//
// -----------------------------------------------------------------------------
// WHAT THIS HANDLER DOES NOT DO
// -----------------------------------------------------------------------------
//
// It does not decide the registration policy and it does not mint the user row.
// It resolves a VERIFIED identity and a LINK DECISION, and hands both to the
// same provisioning seam the magic-link path uses. Keeping provisioning in one
// place is what stops a federated cluster growing a second, subtly different
// definition of what a new user is.

// handleOIDCCallback completes an upstream sign-in.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if !s.requireSecureRequest(w, r) {
		return
	}
	// Cleared on every outcome, including the early refusals below.
	defer s.clearOIDCStateCookie(w)

	if !s.oidcEnabled() {
		http.NotFound(w, r)
		return
	}

	q := r.URL.Query()
	// THE PROVIDER'S OWN REFUSAL COMES FIRST. A user who declined consent, or a
	// tenant policy that blocked them, arrives here with `error` and no code --
	// and reporting that as "state mismatch" would send an operator hunting a
	// security problem that is not there.
	if providerErr := strings.TrimSpace(q.Get("error")); providerErr != "" {
		s.auditOIDC(r, "oidc_sign_in_refused_by_provider", "", map[string]any{
			"error":       providerErr,
			"description": q.Get("error_description"),
		})
		s.oidcRedirectToLogin(w, r, "provider_refused")
		return
	}

	state, nonce, verifier, ok := readOIDCStateCookie(r)
	if !ok {
		// This browser never started a sign-in here.
		s.auditOIDC(r, "oidc_sign_in_refused", "", map[string]any{"reason": "no_state_cookie"})
		s.oidcRedirectToLogin(w, r, "expired")
		return
	}
	if got := strings.TrimSpace(q.Get("state")); got == "" || subtleEqual(got, state) != 1 {
		s.auditOIDC(r, "oidc_sign_in_refused", "", map[string]any{"reason": "state_mismatch"})
		s.oidcRedirectToLogin(w, r, "state_mismatch")
		return
	}
	code := strings.TrimSpace(q.Get("code"))
	if code == "" {
		s.auditOIDC(r, "oidc_sign_in_refused", "", map[string]any{"reason": "no_code"})
		s.oidcRedirectToLogin(w, r, "no_code")
		return
	}

	cfg := s.Cfg.OIDC
	meta, err := s.oidcDiscoverer().Discover(r.Context(), cfg.ResolvedIssuer())
	if err != nil {
		s.auditOIDC(r, "oidc_sign_in_failed", "", map[string]any{"reason": "discovery", "error": err.Error()})
		s.oidcRedirectToLogin(w, r, "provider_unreachable")
		return
	}

	tokens, err := oidc.Exchange(r.Context(), nil, meta, oidc.ExchangeParams{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURI:  s.oidcRedirectURI(),
		Code:         code,
		CodeVerifier: verifier,
	})
	if err != nil {
		s.auditOIDC(r, "oidc_sign_in_failed", "", map[string]any{"reason": "exchange", "error": err.Error()})
		s.oidcRedirectToLogin(w, r, "exchange_failed")
		return
	}

	keys := &oidc.KeySet{URL: meta.JWKSURI}
	claims, err := keys.VerifyIDToken(r.Context(), tokens.IDToken, oidc.VerifyParams{
		Issuer:      meta.Issuer,
		ClientID:    cfg.ClientID,
		Nonce:       nonce,
		GroupsClaim: cfg.GroupsClaim,
	})
	if err != nil {
		// Everything above this point established only that SOMETHING came
		// back. This is the line where identity is established, and its failure
		// is the one worth auditing loudest.
		s.auditOIDC(r, "oidc_id_token_rejected", "", map[string]any{"error": err.Error()})
		s.oidcRedirectToLogin(w, r, "token_rejected")
		return
	}

	decision := oidc.DecideLink(claims, s.lookupOIDCLink(r, claims))
	s.auditOIDC(r, "oidc_sign_in_decided", decision.UserId, map[string]any{
		"reason":  decision.Reason,
		"action":  string(decision.Action),
		"subject": claims.Subject,
	})

	switch decision.Action {
	case oidc.LinkRefuse:
		s.oidcRedirectToLogin(w, r, decision.Reason)
	case oidc.LinkExisting, oidc.LinkByEmail, oidc.LinkRegister:
		// The provisioning seam. Left to the caller-supplied hook so this
		// handler owns the PROTOCOL and the shared magic-link path keeps
		// owning what a user row is -- a federated cluster must not grow a
		// second definition of that.
		if s.OIDCSignIn == nil {
			s.oidcRedirectToLogin(w, r, "federation_not_wired")
			return
		}
		if err := s.OIDCSignIn(w, r, claims, decision); err != nil {
			s.auditOIDC(r, "oidc_sign_in_failed", decision.UserId,
				map[string]any{"reason": "provision", "error": err.Error()})
			s.oidcRedirectToLogin(w, r, "provision_failed")
		}
	}
}

// OIDCSignInHook provisions or resolves the user and establishes the session.
//
// A HOOK RATHER THAN AN IMPLEMENTATION HERE, so this package stays the protocol
// and the cluster's own definition of a user stays in one place. It is also
// what lets the whole protocol above be tested with no store at all.
type OIDCSignInHook func(w http.ResponseWriter, r *http.Request, claims oidc.Claims, d oidc.LinkDecision) error

// lookupOIDCLink resolves what the cluster already knows about this identity.
//
// Returns an EMPTY lookup when no resolver is wired, which DecideLink reads as
// "nobody matched" -- so an unwired cluster registers rather than silently
// linking somebody to a row it never checked for.
func (s *Server) lookupOIDCLink(r *http.Request, c oidc.Claims) oidc.LinkLookup {
	if s.OIDCLookup == nil {
		return oidc.LinkLookup{}
	}
	return s.OIDCLookup(r.Context(), c)
}

func (s *Server) auditOIDC(r *http.Request, action, userID string, detail map[string]any) {
	if s.Audit == nil {
		return
	}
	outcome := identity.AuditOutcomeSuccess
	if strings.Contains(action, "refused") || strings.Contains(action, "rejected") || strings.Contains(action, "failed") {
		outcome = identity.AuditOutcomeFailure
	}
	s.audit(r, identity.AuditEvent{
		Category:    identity.AuditCategoryAuth,
		Action:      action,
		TargetType:  "upstreamIdentity",
		ActorUserId: userID,
		Outcome:     outcome,
		Detail:      detail,
	})
}

// subtleEqual is a constant-time compare returning 1 on equality. The state
// value is a secret this server minted, and a timing oracle on it would let an
// attacker construct a matching one.
func subtleEqual(a, b string) int {
	if len(a) != len(b) {
		return 0
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	if diff == 0 {
		return 1
	}
	return 0
}
