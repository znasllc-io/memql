package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/magiclink"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// magiclink.go -- the device-bound, approve-on-click magic-link flow
// (memql#4302; design sections 3 D2/D3/D4/D8 and 4).
//
// # What changed and why
//
// A magic link used to be spent by whoever opened it, on a GET, before any
// human had interacted with the page. On a group alias (`team@example.com`)
// that made the credential first-come-first-served among everyone who can
// read the mailbox: A asks for a link, B clicks it first, and B gets the
// session -- on the identity path, a first-party cookie with no PKCE and no
// device check, from which B could enrol their own passkey and hold
// permanent, mailbox-independent access. A got "already used".
//
// Four routes replace that with one property: A SESSION CAN ONLY EVER LAND
// ON THE DEVICE THAT ASKED FOR IT.
//
//	GET  /auth/complete?ml=<token>   renders. Writes nothing, ever.
//	POST /auth/landing               cookie matches -> finish here;
//	                                 otherwise -> approve only.
//	GET  /auth/magic-link/status     the requesting tab's poll.
//	POST /auth/magic-link/finish     the requesting tab completes itself.
//
// If B clicks, B approves and A signs in. B is handed nothing: no cookie, no
// code, no session. The row records B's IP under approvedFromIP, so the
// click leaves a trace an operator can read.
//
// # Why the GET writes nothing
//
// Outlook SafeLinks, Gmail's proxy and every mail-security appliance fetch
// the URLs in a message. Against a consuming GET each of those burns the
// link. Rendering a page and moving the state change to a POST makes
// prefetchers harmless for free -- it is not an extra defence, it is the
// absence of the defect. The cost is one click in every case, including the
// same-device one; the design took that trade explicitly (D3) and it is
// reversible in isolation.
//
// # Why the cookie is the only credential on the no-cookie branch
//
// There is deliberately no "continue here anyway" affordance. That button is
// exactly B's click. A person who genuinely opened the link on a second
// device is told to go back to the first one, and the poll there finishes
// the job.

// magicLinkCookieName is the device-binding cookie. Host-only, HttpOnly, and
// scoped to /auth so it rides exactly the four routes above and nothing else.
const magicLinkCookieName = "memql_ml"

// magicLinkCookiePath scopes the binding cookie. /auth covers /auth/complete,
// /auth/landing, /auth/magic-link/status and /auth/magic-link/finish.
const magicLinkCookiePath = "/auth"

// BrowserSessionInput names a sign-in that has already been proved, for the
// seam that mints the first-party browser session.
type BrowserSessionInput struct {
	UserId string
	Email  string
	// Action is the audit action to record ("admin_session_started" or
	// "admin_bootstrap_session_started").
	Action string
}

// BrowserSessionFunc stamps the first-party session cookie for an
// authenticated user.
//
// A func field rather than a direct call because session minting lives in
// component/identity/http (it owns the JWT issuer, the session row and the
// cookie policy) and this package must not import it -- the same seam shape
// IssueMagicLink already uses. Nil leaves the four routes unmounted: a
// magic-link flow that cannot finish is worse than one that is absent,
// because the first looks like it works.
type BrowserSessionFunc func(w http.ResponseWriter, r *http.Request, in BrowserSessionInput) error

// MagicLinkVerifier is the read/finish half of magiclink.Verifier this
// package needs. An interface so a test can drive the four routes without an
// engine.
type MagicLinkVerifier interface {
	// Inspect resolves a plaintext token to its row, writing nothing.
	Inspect(ctx context.Context, plainToken, sourceIP, userAgent string) (*identity.MagicLinkRow, error)
	// Finish consumes the request exactly once and mints what the row's
	// OAuth context calls for.
	Finish(ctx context.Context, in magiclink.FinishInput) (*magiclink.VerifyResult, error)
}

// SetMagicLinkFlow wires the four device-bound routes. All three arguments
// must be non-nil or the routes stay unmounted.
func (s *Server) SetMagicLinkFlow(v MagicLinkVerifier, session BrowserSessionFunc, audit identity.AuditLogger) {
	if s == nil || v == nil || session == nil || audit == nil {
		return
	}
	s.mlVerifier = v
	s.mlSession = session
	s.mlAudit = audit
}

// magicLinkMounted reports whether the flow has everything it needs. The
// Store is required too: approve, poll and the id lookup all read rows.
func (s *Server) magicLinkMounted() bool {
	return s != nil && s.mlVerifier != nil && s.mlSession != nil && s.mlAudit != nil && s.Store != nil
}

// -----------------------------------------------------------------------
// Cookie
// -----------------------------------------------------------------------

// setMagicLinkCookie writes the binding nonce on the requesting browser.
//
// SameSite=Lax is load-bearing rather than a default: a mail client opens
// the link as a TOP-LEVEL navigation, and Lax is precisely the mode that
// sends the cookie on one. Strict would drop it and turn every same-device
// click into a cross-device approval.
func (s *Server) setMagicLinkCookie(w http.ResponseWriter, nonce string, ttl time.Duration) {
	if w == nil || strings.TrimSpace(nonce) == "" {
		return
	}
	maxAge := int(ttl / time.Second)
	if maxAge <= 0 {
		maxAge = int((10 * time.Minute) / time.Second)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     magicLinkCookieName,
		Value:    nonce,
		Path:     magicLinkCookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.cookieSecure(),
		SameSite: http.SameSiteLaxMode,
	})
}

// clearMagicLinkCookie retires the binding once it has been spent. Not a
// security control -- the row's consumedAt is -- but leaving a dead nonce in
// the jar for the rest of the TTL is untidy and confuses the next request.
func (s *Server) clearMagicLinkCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     magicLinkCookieName,
		Value:    "",
		Path:     magicLinkCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cookieSecure(),
		SameSite: http.SameSiteLaxMode,
	})
}

// requestHoldsBinding reports whether this request carries the cookie that
// was minted for this row.
//
// A row with an EMPTY bindingHash returns false for every caller, which is
// the safe direction: such a link can be approved but never completed.
// Rows predating this flow, and the API issue path where no browser is on
// the other end, are both in that position.
func requestHoldsBinding(r *http.Request, row *identity.MagicLinkRow) bool {
	if r == nil || row == nil || !row.HasBinding() {
		return false
	}
	c, err := r.Cookie(magicLinkCookieName)
	if err != nil || c == nil || strings.TrimSpace(c.Value) == "" {
		return false
	}
	return magiclink.HashBindingNonce(c.Value) == strings.TrimSpace(row.BindingHash)
}

// -----------------------------------------------------------------------
// GET /auth/complete -- the click
// -----------------------------------------------------------------------

// handleAuthComplete renders the confirmation page. It writes nothing.
func (s *Server) handleAuthComplete(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.URL.Query().Get("ml"))
	if token == "" {
		s.renderLandingProblem(w, r, "Missing sign-in token", "That link is missing its token. Use the link from your email, or request a new one.")
		return
	}
	row, err := s.mlVerifier.Inspect(r.Context(), token, clientIP(r), r.Header.Get("User-Agent"))
	if err != nil {
		s.renderInspectError(w, r, err)
		return
	}

	// ALREADY APPROVED is its own state, not a failure. Somebody -- possibly
	// this same person on this same device a moment ago -- has already said
	// yes, and the requesting tab is finishing. Saying "invalid" here would
	// be a lie that sends them to request a fresh link they do not need.
	if !row.ApprovedAt.IsZero() && !requestHoldsBinding(r, row) {
		s.renderLandingProblem(w, r, "Already confirmed",
			"This sign-in has already been confirmed. Go back to the device where you asked for the link -- it is finishing signing you in. If you closed that page, request a new link from the device you want to use.")
		return
	}

	data := webtempl.LandingData{
		Layout:      s.LayoutData(r, "Confirm sign-in", false, nil, nil),
		MagicToken:  token,
		MaskedEmail: maskEmail(row.Email),
		SameDevice:  requestHoldsBinding(r, row),
	}
	s.render(w, r, "landing", webtempl.Landing(data))
}

// -----------------------------------------------------------------------
// POST /auth/landing -- Continue
// -----------------------------------------------------------------------

// handleAuthLanding is the one state change a human's click can cause.
//
// CSRF-exempt, and the exemption is reasoned rather than inherited: the
// emailed token in the form body IS the proof of possession, and an attacker
// who can supply it does not need a forged cross-site POST to use it. The
// path is listed in the CSRF middleware's exempt set in server.go.
func (s *Server) handleAuthLanding(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderLandingProblem(w, r, "Sign-in error", "We couldn't read your form submission. Please try again.")
		return
	}
	token := strings.TrimSpace(r.PostForm.Get("ml"))
	if token == "" {
		s.renderLandingProblem(w, r, "Missing sign-in token", "That link is missing its token. Use the link from your email, or request a new one.")
		return
	}
	row, err := s.mlVerifier.Inspect(r.Context(), token, clientIP(r), r.Header.Get("User-Agent"))
	if err != nil {
		s.renderInspectError(w, r, err)
		return
	}

	// SAME DEVICE. The browser holding the binding cookie opened its own
	// link -- the ordinary case where a mail client opens a new tab of the
	// same profile. Nothing to approve; finish here.
	//
	// AN UNBOUND ROW ALSO FINISHES HERE, and that is not a hole in the
	// binding -- it is the absence of one, for a link that never had a device
	// to bind to. Exactly one issuer produces such a row: the boot-time env
	// auto-bootstrap, emailing the configured owner a claim link from a
	// goroutine with nobody at a keyboard (magiclink.IssueInput.Unbound).
	// Binding that link would make it approvable from anywhere and completable
	// nowhere, i.e. an env-bootstrapped cluster nobody can claim.
	//
	// Every other issue path hands a browser the nonce, so every other row
	// has a binding and reaches the branch below. If a row ever arrives here
	// unbound for some OTHER reason, it completes for whoever opened it --
	// which is why Unbound is server-stamped and no handler propagates it.
	if !row.HasBinding() || requestHoldsBinding(r, row) {
		s.finishSignIn(w, r, row, false)
		return
	}

	// CROSS DEVICE. Record the approval and tell this person to go back to
	// where they started. They are handed nothing.
	won, err := s.Store.ApproveMagicLinkRequest(r.Context(), row.ID, clientIP(r), r.Header.Get("User-Agent"))
	if err != nil {
		s.Logger.Warn("identity-web: magic-link approve failed", "error", err, "requestId", row.ID)
		s.renderLandingProblem(w, r, "Sign-in error", "We couldn't confirm that sign-in. Please try the link again.")
		return
	}
	if !won {
		s.auditMagicLink(r, "magic_link_approval_denied", identity.AuditOutcomeBlocked, row, map[string]any{}, "already_approved")
		s.renderLandingProblem(w, r, "Already confirmed",
			"This sign-in has already been confirmed. Go back to the device where you asked for the link.")
		return
	}
	s.auditMagicLink(r, "magic_link_approved", identity.AuditOutcomeSuccess, row, map[string]any{"crossDevice": true}, "")
	s.render(w, r, "landing", webtempl.Landing(webtempl.LandingData{
		Layout:      s.LayoutData(r, "Sign-in confirmed", false, nil, nil),
		Approved:    true,
		MaskedEmail: maskEmail(row.Email),
	}))
}

// -----------------------------------------------------------------------
// GET /auth/magic-link/status -- the poll
// -----------------------------------------------------------------------

// magicLinkStatus is the poll's reply. One field, because one field is all
// the page branches on.
type magicLinkStatus struct {
	State string `json:"state"`
}

// handleMagicLinkStatus answers the requesting tab's poll.
//
// 404 for an unknown id AND for a cookie that does not match, deliberately
// indistinguishable: the id is rendered on a page, so anybody who can read
// over a shoulder has one, and a 403 would confirm that a given id names a
// real pending request.
func (s *Server) handleMagicLinkStatus(w http.ResponseWriter, r *http.Request) {
	requestId := strings.TrimSpace(r.URL.Query().Get("request"))
	if requestId == "" {
		http.NotFound(w, r)
		return
	}
	row, err := s.Store.LookupMagicLinkById(r.Context(), requestId)
	if err != nil {
		s.Logger.Warn("identity-web: magic-link status lookup failed", "error", err, "requestId", requestId)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if row == nil || !requestHoldsBinding(r, row) {
		http.NotFound(w, r)
		return
	}

	state := "pending"
	switch {
	case !row.ConsumedAt.IsZero():
		state = "consumed"
	case !row.ExpiresAt.IsZero() && time.Now().UTC().After(row.ExpiresAt):
		state = "expired"
	case !row.ApprovedAt.IsZero():
		state = "approved"
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(magicLinkStatus{State: state})
}

// -----------------------------------------------------------------------
// POST /auth/magic-link/finish -- the requesting tab completes itself
// -----------------------------------------------------------------------

// handleMagicLinkFinish is submitted by the /check-email page once its poll
// reports `approved`. A real form POST rather than a fetch, so the 303 that
// follows is an ordinary navigation in the tab that holds the client's PKCE
// state -- a fetch would follow the redirect itself and strand the code.
func (s *Server) handleMagicLinkFinish(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderLandingProblem(w, r, "Sign-in error", "We couldn't read your form submission. Please try again.")
		return
	}
	requestId := strings.TrimSpace(r.PostForm.Get("request"))
	if requestId == "" {
		s.renderLandingProblem(w, r, "Sign-in error", "That sign-in has expired. Please request a new link.")
		return
	}
	row, err := s.Store.LookupMagicLinkById(r.Context(), requestId)
	if err != nil {
		s.Logger.Warn("identity-web: magic-link finish lookup failed", "error", err, "requestId", requestId)
		s.renderLandingProblem(w, r, "Sign-in error", "We couldn't complete that sign-in. Please request a new link.")
		return
	}
	if row == nil {
		s.renderLandingProblem(w, r, "Sign-in error", "That sign-in has expired. Please request a new link.")
		return
	}
	if !requestHoldsBinding(r, row) {
		s.auditMagicLink(r, "magic_link_finish_blocked", identity.AuditOutcomeBlocked, row, nil, "cookie_mismatch")
		s.renderLandingProblem(w, r, "Wrong device",
			"This sign-in has to be finished in the browser that asked for it. Request a new link from the device you want to use.")
		return
	}
	if !row.ConsumedAt.IsZero() {
		s.auditMagicLink(r, "magic_link_finish_blocked", identity.AuditOutcomeBlocked, row, nil, "consumed")
		s.renderLandingProblem(w, r, "Already signed in", "This sign-in link has already been used. Please request a new one.")
		return
	}
	if !row.ExpiresAt.IsZero() && time.Now().UTC().After(row.ExpiresAt) {
		s.auditMagicLink(r, "magic_link_finish_blocked", identity.AuditOutcomeBlocked, row, nil, "expired")
		s.renderLandingProblem(w, r, "Link expired", "This sign-in link has expired. Please request a new one.")
		return
	}
	// THE APPROVAL IS REQUIRED HERE and is not required on the landing POST,
	// and the asymmetry is the whole point. The landing POST carries the
	// emailed token; this one carries only a request id anybody looking at
	// the page can read. The approval is what says a human holding the token
	// said yes.
	if row.ApprovedAt.IsZero() {
		s.auditMagicLink(r, "magic_link_finish_blocked", identity.AuditOutcomeBlocked, row, nil, "not_approved")
		s.renderLandingProblem(w, r, "Not confirmed yet", "Open the link in your email first, then come back to this page.")
		return
	}
	s.finishSignIn(w, r, row, true)
}

// -----------------------------------------------------------------------
// Shared completion
// -----------------------------------------------------------------------

// finishSignIn consumes the request exactly once and lands the caller
// wherever the row's OAuth context says.
func (s *Server) finishSignIn(w http.ResponseWriter, r *http.Request, row *identity.MagicLinkRow, crossDevice bool) {
	res, err := s.mlVerifier.Finish(r.Context(), magiclink.FinishInput{
		RequestId:   row.ID,
		SourceIP:    clientIP(r),
		UserAgent:   r.Header.Get("User-Agent"),
		CrossDevice: crossDevice,
	})
	if err != nil {
		s.renderInspectError(w, r, err)
		return
	}
	s.clearMagicLinkCookie(w)

	// Admin path: no relying party, so no OAuth callback to bounce through.
	// A bootstrap link CAN still carry a client + redirect (the cockpit
	// reaches /setup through /login?return_to=<loopback>), and a non-empty
	// RedirectURI is the discriminator -- exactly as it was on the old
	// consuming GET.
	if (res.Bootstrap || res.AdminSession) && res.RedirectURI == "" {
		if err := s.mlSession(w, r, BrowserSessionInput{
			UserId: res.UserId,
			Email:  res.Email,
			Action: browserSessionAction(res.Bootstrap),
		}); err != nil {
			s.Logger.Error("identity-web: magic-link browser session failed", "error", err, "userId", res.UserId)
			s.renderLandingProblem(w, r, "Sign-in error", "We couldn't start your session. Please request a new link.")
			return
		}
		if dest := identity.TakePostLoginRedirect(w, r); dest != "" {
			http.Redirect(w, r, dest, http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, s.postLoginLanding(r), http.StatusSeeOther)
		return
	}

	target, err := buildClientCallback(res.RedirectURI, res.AuthCode, res.State)
	if err != nil {
		s.Logger.Error("identity-web: magic-link callback build failed", "error", err, "redirectURI", res.RedirectURI)
		s.renderLandingProblem(w, r, "Sign-in error", "We couldn't return you to the app that started this sign-in.")
		return
	}
	// 303, not 302: this is a POST, and 303 is the code that says "GET the
	// thing at this other URL". A 302 leaves method conversion to the client.
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func browserSessionAction(bootstrap bool) string {
	if bootstrap {
		return "admin_bootstrap_session_started"
	}
	return "admin_session_started"
}

// postLoginLanding is the first-party destination after a bare admin-session
// click. Mirrors the http package's own -- the cluster domain drives it, with
// the configured base URL as the fallback.
func (s *Server) postLoginLanding(r *http.Request) string {
	domain := ""
	if s != nil && s.Store != nil && r != nil {
		if row, err := s.Store.ReadClusterSettings(r.Context()); err == nil && row != nil {
			domain = row.ClusterDomain
		}
	}
	base := ""
	if s != nil {
		base = s.Cfg.BaseURL
	}
	return identity.DefaultPostLoginLanding(domain, base)
}

// buildClientCallback returns the OAuth-style redirect target.
func buildClientCallback(redirectURI, code, state string) (string, error) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// -----------------------------------------------------------------------
// Rendering + audit helpers
// -----------------------------------------------------------------------

// renderInspectError maps the verifier's sentinels onto the four messages a
// person can act on. Each says what to do next; none of them says "invalid"
// for a state that is merely finished.
func (s *Server) renderInspectError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, magiclink.ErrInvalidToken):
		s.renderLandingProblem(w, r, "Link not recognised", "This sign-in link is invalid. Please request a new one.")
	case errors.Is(err, magiclink.ErrTokenExpired):
		s.renderLandingProblem(w, r, "Link expired", "This sign-in link has expired. Please request a new one.")
	case errors.Is(err, magiclink.ErrTokenAlreadyUsed):
		s.renderLandingProblem(w, r, "Already used", "This sign-in link has already been used. Please request a new one.")
	case errors.Is(err, magiclink.ErrOAuthCtxCorrupted):
		s.renderLandingProblem(w, r, "Sign-in error", "We could not complete sign-in. Please request a new link.")
	default:
		if s.Logger != nil {
			s.Logger.Error("identity-web: magic-link failed", "error", err)
		}
		s.renderLandingProblem(w, r, "Sign-in error", "Something went wrong completing sign-in. Please request a new link.")
	}
}

// renderLandingProblem renders one of the terminal states of the flow.
func (s *Server) renderLandingProblem(w http.ResponseWriter, r *http.Request, title, message string) {
	s.render(w, r, "landing", webtempl.Landing(webtempl.LandingData{
		Layout:  s.LayoutData(r, title, false, nil, nil),
		Problem: title,
		Message: message,
	}))
}

// auditMagicLink emits one row for a landing/finish outcome.
func (s *Server) auditMagicLink(r *http.Request, action string, outcome identity.AuditOutcome, row *identity.MagicLinkRow, detail map[string]any, failureReason string) {
	if s == nil || s.mlAudit == nil || row == nil {
		return
	}
	s.mlAudit.Log(r.Context(), identity.AuditEvent{
		Category:      identity.AuditCategoryAuth,
		Action:        action,
		TargetType:    "magicLinkRequest",
		TargetId:      row.ID,
		TargetEmail:   row.Email,
		SourceIP:      clientIP(r),
		UserAgent:     r.Header.Get("User-Agent"),
		Outcome:       outcome,
		FailureReason: failureReason,
		Detail:        detail,
	})
}

// maskEmail renders an address recognisable to its owner and useless to a
// shoulder-surfer: first character, then the domain.
//
// The landing page has to name the account -- "confirm sign-in" for an
// unnamed account is a phishing shape -- but it is reachable by anybody
// holding the link, so it must not print the address in full.
func maskEmail(email string) string {
	email = strings.TrimSpace(email)
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return ""
	}
	local, domain := email[:at], email[at+1:]
	if len(local) <= 1 {
		return local + "***@" + domain
	}
	return local[:1] + strings.Repeat("*", 3) + "@" + domain
}

// RenderAbuseRejection is the abuse stack's browser-facing rejection page
// (memql#4303).
//
// The stack's default is a JSON envelope, which is right for the API route
// and wrong for a form post: a person who trips the per-IP limiter would see
// a raw payload where a sentence belongs. The reason is NOT rendered --
// telling an abuser which control fired is telling them which one to work
// around, and it tells a legitimate user nothing they can act on. The audit
// row carries the reason for the operator who can.
func (s *Server) RenderAbuseRejection(w http.ResponseWriter, r *http.Request, status int, reason string) {
	message := "We couldn't process that request. Please try again."
	if status == http.StatusTooManyRequests {
		message = "Too many sign-in attempts from this network. Please wait a few minutes and try again."
	}
	s.renderError(w, r, status, message)
}
