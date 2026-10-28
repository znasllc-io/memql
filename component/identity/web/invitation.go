package web

// GET /invitation?code=mql_inv_...  +  POST /invitation/accept
//
// The redeem end of a user invitation (memql#4601). The page an invitation
// email lands on, and the one action that turns it into an account.
//
// # Why this page exists rather than the login form
//
// It used not to. adminops.invitationURL composed `/login?invitation=<token>`
// and its comment claimed the recipient "arrives with it already filled in".
// Nothing on the receiving end read that parameter -- `Query().Get("invitation")`
// appeared nowhere in the tree -- so an invitee landed on a bare email box,
// typed their address, was bounced into stage needs_invite ("paste your
// invitation token"), pasted the token out of their own address bar, and was
// refused, because loginFormInvite rendered no email field and the empty
// address failed resolveInvitation's address check. The message they got told
// them to check their email address. User-invitation redemption had never
// worked on any shipped version.
//
// The deeper problem was the shape, not the missing field. An invitation is a
// credential that already names one person; a form that asks that person who
// they are, and then asks them to hand back the credential they were just
// given, is the wrong interaction. This page instead RESOLVES the token
// server-side and tells the holder what it found: who invited them, what role
// they will land with, which address it is for, and when it stops working.
//
// # GET RENDERS, POST CHANGES STATE, AND THAT IS NOT A STYLE CHOICE
//
// Enterprise mail security (Defender Safe Links, Proofpoint, Mimecast) fetches
// URLs out of email to scan them, from the vendor's infrastructure, before any
// human clicks. A GET that consumed the invitation would therefore be spent by
// a scanner, and the invitee -- the first actual human to open it -- would be
// told the link had already been used. That failure is indistinguishable, from
// the outside, from someone stealing the invitation, which is the worst way to
// be wrong. So this GET is idempotent and writes nothing at all -- no spend, no
// browser binding, no cookie -- and every step that changes state hangs off
// the POST.
//
// /enroll already has exactly this shape and says so; this file follows it, as
// it follows pair.go's discipline throughout: HTTPS required (the token is a
// bearer in a URL), per-IP rate limiting through the same abuse package, and an
// audit event carrying SourceIP on every outcome including each refusal.
//
// # WHY THE BINDING WAITS FOR CONTINUE
//
// An invitation is accepted in one browser only: a nonce in that browser's
// cookie, its digest on the row (bindingHash). That is what stops a forwarded
// email from being accepted by whoever it was forwarded to. The GET used to
// stamp the binding on FIRST TOUCH, which made it exactly the side effect the
// paragraph above rules out: whatever fetched the link first owned the
// invitation. A mail scanner, a link preview, an email app's in-app browser or
// a second device got there first, and the invitee -- reported from Safari on
// an iPhone -- was refused with "Finish in the browser that opened the
// invitation", a browser they had never used.
// TestScannerGetThenHumanContinueAndAccept (invitation_flow_test.go) replays it.
//
// So the GET only READS the binding, and the binding is made by a POST the
// invitee sends by pressing Continue: intent=continue on the existing
// POST /invitation/accept. Not a route of its own, because a new HTTP endpoint
// needs the owner's approval here and the front door's generated path list
// would change with it. Link scanners and previews fetch; they do not submit
// forms. The page shows one step at a time, chosen by the GET:
//
//   - nobody has pressed Continue: Continue;
//   - this browser pressed it: Accept;
//   - another browser pressed it: the refusal, naming where to finish.
//
// Accept still requires the binding, and now also requires that one exists
// (see invitationBindingMatches). The forwarding control is the same in kind --
// the first person to press Continue holds the invitation, as the first to
// open it did -- but automated fetchers are out of that race.
//
// FIRST TOUCH ALSO FAILED WITH NOTHING ELSE INVOLVED. The emailed link opens on
// identity's host (adminops.invitationURL); nativeUI runs this handler and
// then sends the browser to MemQL OS, which reads the page and posts its forms
// through its own origin (component/edge identityUIPaths). The binding cookie
// is host-only, so the one the link's GET set on identity's host never reached
// the OS's Accept. Continue and Accept are posted from the same place as each
// other, whichever host that is. TestTheLinksHostAndTheOSHostShareNoCookies
// pins it.
//
// Rows the old GET bound stay bound; no migration rewrites them. A browser
// presenting that cookie still sees Accept and any other sees the refusal, and
// since the link's GET left its cookie on identity's host, expect the refusal,
// whose next step is a fresh invitation.
//
// # What the accept hands off to
//
// Not a session. The accept exchanges the long-lived invitation for a
// short-lived enrolment token and redirects to /enroll, in the same window.
// The invitation bearer never itself creates an account; all it can do is be
// spent once for the narrow credential whose own package argues that its
// narrowness is what makes a bearer-in-a-URL safe (memql#3408). Reusing that
// boundary is the security property here, and it is deliberately not a new one.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/abuse"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// envAllowInsecureInvitation is the dev-only escape hatch that bypasses the
// HTTPS-required check on the invitation routes.
//
// Named separately from /enroll's MEMQL_IDENTITY_ALLOW_INSECURE_ENROLL for the
// reason that one is named separately from pair.go's: an operator debugging one
// credential surface on a laptop should not thereby be admitting plaintext
// traffic on the others. Production must leave it unset; the runtime logs a
// WARN every time a request slips through it.
const envAllowInsecureInvitation = "MEMQL_IDENTITY_ALLOW_INSECURE_INVITATION"

// envInvitationPerHour tunes the per-IP redeem rate limit.
//
// The page is cheap, but the request behind it is a credential lookup keyed on
// a string that is guessable in principle. 120/h leaves a real person far more
// retries than they will ever need and leaves a script nowhere near a 256-bit
// search -- the same numbers, for the same reasons, as /enroll.
const envInvitationPerHour = "MEMQL_IDENTITY_INVITATION_PER_HOUR"

const defaultInvitationPerHour = 120

// invitationCookieName is the binding cookie, set by Continue.
//
// A SEPARATE COOKIE FROM memql_ml, NOT A SECOND MECHANISM. The magic-link
// binding cookie is path-scoped to /auth, which does not cover these routes, so
// the two cannot literally be the same jar entry. What they DO share is
// writeBindingCookie below, which owns the HttpOnly / Secure / SameSite / MaxAge
// semantics for both: the thing worth having exactly one of is the decision
// about how a binding cookie behaves, not the string it is stored under.
const invitationCookieName = "memql_inv"

// invitationCookiePath scopes the binding cookie to the two routes that use it.
const invitationCookiePath = "/invitation"

// InvitationState mirrors the resolver's verdict without importing the
// engine-facing invitation package into web.
//
// The web package stays engine-free (see PersistClusterSettingsFunc and
// EnrolmentState, which exists for precisely this reason); the wiring layer
// maps one onto the other.
type InvitationState string

const (
	InvitationValid       InvitationState = "valid"
	InvitationInvalid     InvitationState = "invalid"
	InvitationExpired     InvitationState = "expired"
	InvitationAlreadyUsed InvitationState = "already-used"
	InvitationRevoked     InvitationState = "revoked"
	// InvitationWrongKind is a guest invitation presented here. It authorizes
	// joining a space, not registering an account on the cluster: different
	// credential, different flow. Kept distinct from InvitationInvalid because
	// the holder of one is not making a typo and telling them to re-check the
	// link would send them looking for a problem that is not there.
	InvitationWrongKind InvitationState = "wrong-kind"

	// InvitationBoundElsewhere and InvitationNotContinued are the page's own
	// refusals. The resolver never returns them: they are verdicts about THIS
	// browser, reached by comparing its cookie with the row's binding, where
	// the five above are verdicts about the row.
	InvitationBoundElsewhere InvitationState = "bound-elsewhere"
	InvitationNotContinued   InvitationState = "not-continued"
)

// InvitationResolution is what the wiring layer reports back about a presented
// token.
//
// It carries NO token material -- not the plaintext, not the hash. The page has
// the plaintext already (it is in the address bar) and has no use for the hash,
// so neither belongs in a struct that crosses a package boundary and might one
// day be logged. Same rule EnrolmentResolution states.
type InvitationResolution struct {
	State InvitationState
	// InvitationId is the row id, for the audit trail. Empty when nothing
	// matched.
	InvitationId string
	// InviteeEmail is the address this invitation authorizes. Shown to the
	// holder and NOT editable: the whole point of resolving server-side is that
	// nobody has to be asked who they are.
	InviteeEmail string
	// InviterName is who issued it. Empty renders as an unattributed
	// invitation rather than a fabricated one, matching the email body's rule.
	InviterName string
	// Role is the cluster role the recipient lands with. Empty means the
	// cluster's default, and the copy says so rather than naming a role the
	// server never promised.
	Role string
	// ExpiresAt bounds the live invitation. Zero unless State is valid.
	ExpiresAt time.Time
	// StepUp is true when this invitation grants a privileged role and must
	// therefore also prove control of the mailbox before a credential is
	// issued. Decided by the wiring layer, which owns the threshold; the page
	// only renders the consequence.
	StepUp bool
	// BindingHash is the binding digest stamped when somebody pressed
	// Continue, or empty when nobody has. The GET compares it with this
	// browser's cookie to pick the step it shows; the accept requires a cookie
	// hashing to it and refuses an empty one.
	//
	// A DIGEST IS NOT TOKEN MATERIAL. The rule this struct states above is
	// about the invitation credential, whose plaintext and hash both stay out
	// of here. This is the SHA-256 of a nonce the server itself minted for one
	// browser, it is exactly what magicLinkRequest.bindingHash persists in the
	// clear, and the comparison it exists for has to happen somewhere the
	// request's cookie is in scope.
	BindingHash string
}

// InvitationAcceptResult is what the wiring layer reports back after spending
// an invitation.
type InvitationAcceptResult struct {
	// EnrolmentCode is the freshly-minted enrolment token plaintext. The
	// handler redirects to /enroll with it and holds it nowhere else.
	EnrolmentCode string
	// Email is the address the account was provisioned for, used for the
	// step-up branch's magic-link issue.
	Email string
}

// ResolveInvitationFunc validates a presented invitation token. Supplied by the
// wiring layer, which owns the store; nil leaves the routes unmounted.
type ResolveInvitationFunc func(ctx context.Context, plainToken string) (InvitationResolution, error)

// BindInvitationFunc stamps the binding digest on a pending row when its
// invitee presses Continue.
//
// FIRST WRITE ONLY: an implementation must not overwrite a binding already
// present. Overwriting would let a second browser re-bind the row, which is the
// exact move the control exists to stop, so the no-overwrite rule is the
// security property and not a tidiness one.
type BindInvitationFunc func(ctx context.Context, invitationId, bindingHash string) error

// AcceptInvitationFunc spends the invitation: it provisions the user row for
// the invited address and role, marks the invitation accepted, and mints the
// enrolment token that the /enroll page will consume.
//
// One function rather than three seams because the three steps have to agree
// about ordering and failure, and that argument belongs in one place next to
// the store rather than being reassembled here from parts.
type AcceptInvitationFunc func(ctx context.Context, plainToken, sourceIP string) (InvitationAcceptResult, error)

// SetInvitationFlow wires the four things the invitation routes need.
//
// ALL FOUR ARE REQUIRED and nil leaves both routes unmounted, the same
// all-or-nothing judgment SetResolveEnrolment makes: a credential-redeem
// surface that renders but cannot complete is worse than one that is absent,
// because the first looks like it works. The audit sink is its own parameter
// rather than being borrowed from enrolAudit for the reason recorded on the
// Server's recoverAudit field -- a shared field would let wiring one surface
// silently satisfy another's mount condition.
func (s *Server) SetInvitationFlow(
	resolve ResolveInvitationFunc,
	bind BindInvitationFunc,
	accept AcceptInvitationFunc,
	audit identity.AuditLogger,
) {
	if s == nil || resolve == nil || bind == nil || accept == nil || audit == nil {
		return
	}
	s.resolveInvitation = resolve
	s.bindInvitation = bind
	s.acceptInvitation = accept
	s.invitationAudit = audit
}

// invitationLimiter returns this Server's per-IP redeem limiter, built once on
// first use. Server-scoped rather than a package global so each Server (and
// each test) gets its own buckets -- the same pattern as enrolLimiter.
func (s *Server) invitationLimiter() *abuse.IPRateLimiter {
	s.invitationLimiterOnce.Do(func() {
		perHour := defaultInvitationPerHour
		if v := strings.TrimSpace(os.Getenv(envInvitationPerHour)); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				perHour = n
			}
		}
		s.invitationLimiterValue = abuse.NewIPRateLimiter(perHour, s.Logger)
	})
	return s.invitationLimiterValue
}

// handleInvitationGet renders the invitation page for a presented link.
//
// It WRITES NOTHING: no spend, no binding, no cookie (see the header's notes on
// mail scanners and on Continue). It reads the row's binding against this
// browser's cookie to decide which step to show.
func (s *Server) handleInvitationGet(w http.ResponseWriter, r *http.Request) {
	sourceIP := clientIP(r)

	if !s.requireSecureInvitation(w, r, sourceIP) {
		return
	}
	if allowed, retryAfter := s.invitationLimiter().Allow(sourceIP); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		s.auditInvitation(r, "invitation_redeem_denied", "", identity.AuditOutcomeBlocked, "rate_limited")
		w.WriteHeader(http.StatusTooManyRequests)
		s.renderInvitationRejection(w, r, InvitationInvalid,
			"Too many attempts",
			"Too many invitation attempts have come from this network in the last hour.",
			"Wait a few minutes and open the link again.")
		return
	}

	code := invitationCodeFrom(r)
	if code == "" {
		s.auditInvitation(r, "invitation_redeem_denied", "", identity.AuditOutcomeBlocked, "missing_code")
		heading, message, nextStep := invitationCopy(InvitationInvalid)
		w.WriteHeader(http.StatusBadRequest)
		s.renderInvitationRejection(w, r, InvitationInvalid, heading, message, nextStep)
		return
	}

	res, err := s.resolveInvitation(r.Context(), code)
	if err != nil {
		s.Logger.Warn("identity-web: invitation resolve failed", "error", err.Error())
		s.auditInvitation(r, "invitation_redeem_denied", "", identity.AuditOutcomeFailure, "resolve_failed")
		w.WriteHeader(http.StatusInternalServerError)
		s.renderInvitationRejection(w, r, InvitationInvalid,
			"Something went wrong",
			"We could not check this invitation just now.",
			"Try opening it again in a moment.")
		return
	}

	if res.State != InvitationValid {
		// EVERY refusal is audited with its own reason. A burst of
		// already-used is a replay attempt; a burst of invalid is a scanner.
		// One collapsed "invitation_denied" would make those indistinguishable
		// -- the same argument /enroll records.
		s.auditInvitation(r, "invitation_redeem_denied", res.InvitationId,
			identity.AuditOutcomeBlocked, "invitation_"+strings.ReplaceAll(string(res.State), "-", "_"))
		heading, message, nextStep := invitationCopy(res.State)
		w.WriteHeader(invitationRejectionStatus(res.State))
		s.renderInvitationRejection(w, r, res.State, heading, message, nextStep)
		return
	}

	// THE STEP IS READ, NEVER WRITTEN. The row's binding against this
	// browser's cookie picks the page; stamping one here is the first-touch
	// binding the header records the removal of.
	var step string
	switch {
	case strings.TrimSpace(res.BindingHash) == "":
		step = webtempl.InvitationStepContinue
	case s.invitationBindingMatches(r, res):
		step = webtempl.InvitationStepAccept
	default:
		// Somebody pressed Continue in another browser. This one cannot
		// accept, so it gets the refusal and not the invitation's details:
		// once somebody has claimed it, whom it is for and from whom are no
		// longer for every holder of the link to read. Blocked under the same
		// reason the Continue race uses, on the GET's own action.
		s.auditInvitation(r, "invitation_redeem_denied", res.InvitationId,
			identity.AuditOutcomeBlocked, "bound_elsewhere")
		s.refuseInvitation(w, r, InvitationBoundElsewhere)
		return
	}

	s.auditInvitation(r, "invitation_page_served", res.InvitationId, identity.AuditOutcomeSuccess, "")

	data := webtempl.InvitationData{
		Layout:       s.LayoutData(r, "You have been invited", false, nil, nil),
		Code:         code,
		InviteeEmail: res.InviteeEmail,
		InviterName:  res.InviterName,
		Role:         res.Role,
		ExpiresIn:    humanizeUntil(res.ExpiresAt, time.Now().UTC()),
		StepUp:       res.StepUp,
		Step:         step,
	}
	s.render(w, r, "invitation", webtempl.Invitation(data), data)
}

// handleInvitationAccept is the POST half of the pair: Continue, which binds the
// invitation to this browser, and Accept, which spends it and hands off.
// Everything that changes state is here, for the reason the file header gives.
//
// ONE ROUTE FOR BOTH, told apart by the form's intent field. Continue is a
// write, so it needs everything Accept needs first -- HTTPS, the rate limit,
// CSRF, a fresh resolve -- and a route of its own would be a new HTTP endpoint,
// which this repository admits only with the owner's approval.
func (s *Server) handleInvitationAccept(w http.ResponseWriter, r *http.Request) {
	sourceIP := clientIP(r)

	if !s.requireSecureInvitation(w, r, sourceIP) {
		return
	}
	// Before the form is read, so a refused Continue is recorded under the
	// route's action too: the bucket is spent before the intent is known.
	if allowed, retryAfter := s.invitationLimiter().Allow(sourceIP); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		s.auditInvitation(r, "invitation_accept_denied", "", identity.AuditOutcomeBlocked, "rate_limited")
		w.WriteHeader(http.StatusTooManyRequests)
		s.renderInvitationRejection(w, r, InvitationInvalid,
			"Too many attempts",
			"Too many invitation attempts have come from this network in the last hour.",
			"Wait a few minutes and try again.")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "We couldn't read your form submission. Please try again.")
		return
	}

	// EMPTY INTENT IS ACCEPT. Every Accept form posted none before Continue
	// existed, and a MemQL OS bundle one release behind the identity node
	// still posts none. Any other value is refused rather than guessed at: a
	// client that sends one is broken, and reading it as Accept is how a typo
	// would become a spent invitation.
	intent := strings.TrimSpace(r.PostForm.Get("intent"))
	continuing := intent == webtempl.InvitationStepContinue
	if !continuing && intent != "" && intent != webtempl.InvitationStepAccept {
		s.auditInvitation(r, "invitation_accept_denied", "", identity.AuditOutcomeBlocked, "unknown_intent")
		s.renderError(w, r, http.StatusBadRequest, "We couldn't read your form submission. Please try again.")
		return
	}
	// A refused Continue is recorded as one, so the trail does not show an
	// Accept nobody pressed.
	denied := "invitation_accept_denied"
	if continuing {
		denied = "invitation_continue_denied"
	}

	code := strings.TrimSpace(r.PostForm.Get("code"))
	if code == "" {
		s.auditInvitation(r, denied, "", identity.AuditOutcomeBlocked, "missing_code")
		heading, message, nextStep := invitationCopy(InvitationInvalid)
		w.WriteHeader(http.StatusBadRequest)
		s.renderInvitationRejection(w, r, InvitationInvalid, heading, message, nextStep)
		return
	}

	// RESOLVED AGAIN, not trusted from the GET. The two requests are separate,
	// minutes may have passed, and the row can have been revoked or spent in
	// between. Re-resolving is also what makes the binding check meaningful,
	// for Continue and Accept alike: it needs the row's current digest, not
	// one the page remembered.
	res, err := s.resolveInvitation(r.Context(), code)
	if err != nil {
		s.Logger.Warn("identity-web: invitation resolve failed on accept", "error", err.Error(), "intent", intent)
		s.auditInvitation(r, denied, "", identity.AuditOutcomeFailure, "resolve_failed")
		w.WriteHeader(http.StatusInternalServerError)
		s.renderInvitationRejection(w, r, InvitationInvalid,
			"Something went wrong",
			"We could not check this invitation just now.",
			"Try opening the link again in a moment.")
		return
	}
	if res.State != InvitationValid {
		s.auditInvitation(r, denied, res.InvitationId,
			identity.AuditOutcomeBlocked, "invitation_"+strings.ReplaceAll(string(res.State), "-", "_"))
		heading, message, nextStep := invitationCopy(res.State)
		w.WriteHeader(invitationRejectionStatus(res.State))
		s.renderInvitationRejection(w, r, res.State, heading, message, nextStep)
		return
	}

	if continuing {
		s.continueInvitation(w, r, code, res)
		return
	}

	// ONLY THE BROWSER THAT CONTINUED MAY ACCEPT. Two refusals, because they
	// send the reader to different places: nobody has pressed Continue (the
	// page always offers it first, so this is a post that skipped it), or
	// somebody did in another browser.
	switch {
	case strings.TrimSpace(res.BindingHash) == "":
		s.auditInvitation(r, "invitation_accept_denied", res.InvitationId,
			identity.AuditOutcomeBlocked, "not_continued")
		s.refuseInvitation(w, r, InvitationNotContinued)
		return
	case !s.invitationBindingMatches(r, res):
		s.auditInvitation(r, "invitation_accept_denied", res.InvitationId,
			identity.AuditOutcomeBlocked, "binding_mismatch")
		s.refuseInvitation(w, r, InvitationBoundElsewhere)
		return
	}

	// STEP-UP FOR PRIVILEGED ROLES (memql#4601). An invitation granting admin
	// or owner is a credential that can take the cluster, and it arrived by
	// email. Here it buys a magic link rather than an account: the invitee must
	// also demonstrate they can read the mailbox the invitation was sent to
	// before anything is provisioned. Ordinary roles do not pay this -- the
	// friction lands where it is justified instead of being charged to
	// everyone.
	if res.StepUp {
		s.acceptViaStepUp(w, r, res)
		return
	}

	out, err := s.acceptInvitation(r.Context(), code, sourceIP)
	if err != nil {
		s.Logger.Warn("identity-web: invitation accept failed",
			"error", err.Error(), "invitationId", res.InvitationId)
		s.auditInvitation(r, "invitation_accept_denied", res.InvitationId,
			identity.AuditOutcomeFailure, "accept_failed")
		w.WriteHeader(http.StatusInternalServerError)
		s.renderInvitationRejection(w, r, InvitationInvalid,
			"We could not finish setting up your account",
			"The invitation is valid, but something went wrong while creating your account.",
			"Try again in a moment. If it keeps happening, tell whoever invited you.")
		return
	}

	s.auditInvitation(r, "invitation_accepted", res.InvitationId, identity.AuditOutcomeSuccess, "")

	// The binding has done its job; leaving a dead nonce in the jar for the
	// rest of the TTL is untidy and confuses the next request. Same reasoning
	// clearMagicLinkCookie records.
	s.clearBindingCookie(w, invitationCookieName, invitationCookiePath)

	// SAME WINDOW. The whole design is that the invitee never leaves the tab
	// they opened the email in, so this is a redirect and not an instruction to
	// go and do something else.
	http.Redirect(w, r, "/enroll?code="+url.QueryEscape(out.EnrolmentCode), http.StatusSeeOther)
}

// continueInvitation binds a live invitation to the browser that pressed
// Continue and sends it back to the page, where Accept is now offered.
//
// res was resolved for this request, so its binding is the row's current one.
// The write itself is first-write-wins under the store's lock
// (Store.BindUserInvitation): two browsers pressing at once cannot both win,
// and the loser arrives here as ErrInvitationBoundElsewhere.
func (s *Server) continueInvitation(w http.ResponseWriter, r *http.Request, code string, res InvitationResolution) {
	page := "/invitation?code=" + url.QueryEscape(code)

	if strings.TrimSpace(res.BindingHash) != "" {
		if s.invitationBindingMatches(r, res) {
			// This browser already pressed it: a double press, the back
			// button. Nothing to write, and a second cookie would only replace
			// one that already works. Its own action, so the trail does not
			// read as a second binding.
			s.auditInvitation(r, "invitation_continue_repeated", res.InvitationId, identity.AuditOutcomeSuccess, "")
			http.Redirect(w, r, page, http.StatusSeeOther)
			return
		}
		s.auditInvitation(r, "invitation_binding_taken", res.InvitationId,
			identity.AuditOutcomeBlocked, "bound_elsewhere")
		s.refuseInvitation(w, r, InvitationBoundElsewhere)
		return
	}

	nonce := newBindingNonce()
	if nonce == "" {
		s.auditInvitation(r, "invitation_binding_not_stamped", res.InvitationId,
			identity.AuditOutcomeFailure, "nonce_unavailable")
		s.renderInvitationContinueFailed(w, r)
		return
	}
	switch err := s.bindInvitation(r.Context(), res.InvitationId, hashBindingNonce(nonce)); {
	case err == nil:
		// Bound to this browser; its cookie is handed over below.

	case errors.Is(err, identity.ErrInvitationBoundElsewhere):
		// THE CONTROL WORKING, NOT A FAULT -- and the distinction is the whole
		// reason this is its own branch.
		//
		// Somebody else pressed Continue on this invitation between our
		// resolve and our stamp. That is precisely the event the binding exists
		// to catch, so it is recorded as a BLOCKED outcome under its own reason
		// rather than folded into the failure below. A run of these on one
		// invitation is the signature of a forwarded email being continued by
		// more than one person; a run of bind_failed is infrastructure. An
		// operator grepping the trail must be able to tell those apart, and a
		// single collapsed reason would make a forwarding incident look like a
		// database wobble.
		s.auditInvitation(r, "invitation_binding_taken", res.InvitationId,
			identity.AuditOutcomeBlocked, "bound_elsewhere")
		s.refuseInvitation(w, r, InvitationBoundElsewhere)
		return

	default:
		// FATAL TO THIS PRESS, where the same failure on the old first-touch
		// GET was not. The accept refuses an unbound row, so carrying on would
		// only move the refusal one page later. Nothing was written, so asking
		// the person to press again is safe, and it is recorded so the trail
		// shows the binding was never made.
		s.Logger.Warn("identity-web: invitation binding stamp failed",
			"error", err.Error(), "invitationId", res.InvitationId)
		s.auditInvitation(r, "invitation_binding_not_stamped", res.InvitationId,
			identity.AuditOutcomeFailure, "bind_failed")
		s.renderInvitationContinueFailed(w, r)
		return
	}

	s.writeBindingCookie(w, invitationCookieName, invitationCookiePath, nonce, time.Until(res.ExpiresAt))
	s.auditInvitation(r, "invitation_continued", res.InvitationId, identity.AuditOutcomeSuccess, "")

	// Back to the page, which now offers Accept to this browser. A redirect
	// rather than the page itself, so a reload re-reads the row rather than
	// re-posting the form.
	http.Redirect(w, r, page, http.StatusSeeOther)
}

// renderInvitationContinueFailed answers a Continue whose binding could not be
// written. Nothing changed, so the next step is simply to press it again.
func (s *Server) renderInvitationContinueFailed(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusInternalServerError)
	s.renderInvitationRejection(w, r, InvitationInvalid,
		"Something went wrong",
		"We could not continue with this invitation just now.",
		"Open the link again in a moment and press Continue.")
}

// acceptViaStepUp handles the privileged-role branch: issue a magic link to the
// invited address and send the browser to the check-email page.
//
// The invitation is deliberately NOT spent here. It is spent when the magic
// link is consumed and the account actually comes into existence; burning it at
// this point would leave someone who never received the mail holding a dead
// invitation and no account.
func (s *Server) acceptViaStepUp(w http.ResponseWriter, r *http.Request, res InvitationResolution) {
	if s.IssueMagicLink == nil {
		s.auditInvitation(r, "invitation_accept_denied", res.InvitationId,
			identity.AuditOutcomeFailure, "magic_link_unavailable")
		s.renderError(w, r, http.StatusServiceUnavailable,
			"Sign-in is temporarily unavailable. Please try again in a moment.")
		return
	}

	out, err := s.IssueMagicLink(r.Context(), IssueMagicLinkInput{
		Email:        res.InviteeEmail,
		InvitationId: strings.TrimSpace(r.PostForm.Get("code")),
		SourceIP:     clientIP(r),
		UserAgent:    r.Header.Get("User-Agent"),
		AdminSession: true,
	})
	if err != nil {
		s.Logger.Warn("identity-web: step-up magic link failed",
			"error", err.Error(), "invitationId", res.InvitationId)
		s.auditInvitation(r, "invitation_accept_denied", res.InvitationId,
			identity.AuditOutcomeFailure, "step_up_issue_failed")
		s.renderError(w, r, http.StatusBadRequest,
			"We couldn't send your sign-in link. Please try again in a moment.")
		return
	}

	s.auditInvitation(r, "invitation_step_up_issued", res.InvitationId, identity.AuditOutcomeSuccess, "")
	s.setMagicLinkCookie(w, out.BindingNonce, s.bindingLifetime(out))
	http.Redirect(w, r, checkEmailURL(res.InviteeEmail, out.RequestId), http.StatusSeeOther)
}

// invitationCodeFrom reads the presented token from the request.
//
// BOTH SPELLINGS ARE ACCEPTED, and that is a compatibility requirement rather
// than indecision. `code` is this page's own parameter, matching /enroll and
// /recover. `invitation` is what adminops.invitationURL emitted for every
// invitation issued before this page existed, and those links are sitting in
// mailboxes right now. Dropping the old spelling would break exactly the people
// this change is meant to rescue.
func invitationCodeFrom(r *http.Request) string {
	if v := strings.TrimSpace(r.URL.Query().Get("code")); v != "" {
		return v
	}
	return strings.TrimSpace(r.URL.Query().Get("invitation"))
}

// invitationBindingMatches reports whether this browser holds the nonce whose
// digest is bound to the row: whether it is the browser that pressed Continue.
//
// AN UNBOUND ROW MATCHES NO BROWSER. It used to admit the accept, because rows
// issued before binding existed carried no digest and refusing them would have
// invalidated every invitation in flight at deploy time. That reason is gone:
// an unbound row is now offered Continue, which binds it, so refusing it at
// accept strands nobody -- it refuses only a post that skipped the step the
// page always shows first. What refusing buys is one invariant: every accepted
// invitation was continued in the browser that accepted it.
func (s *Server) invitationBindingMatches(r *http.Request, res InvitationResolution) bool {
	want := strings.TrimSpace(res.BindingHash)
	if want == "" {
		return false
	}
	c, err := r.Cookie(invitationCookieName)
	if err != nil || c == nil || strings.TrimSpace(c.Value) == "" {
		return false
	}
	// subtle.ConstantTimeCompare is deliberately NOT used. Both sides are
	// SHA-256 digests of values the attacker does not control and cannot
	// iterate against -- the row's digest is server-minted and the cookie is
	// theirs already -- so there is no secret here to leak a byte at a time,
	// and reaching for it would imply a threat this comparison does not carry.
	return hashBindingNonce(c.Value) == want
}

// newBindingNonce mints the 32-byte CSPRNG value handed to the browser.
//
// Returns the empty string on the (impossible in practice) failure of the
// system entropy source. Continue then refuses rather than leaving the row
// unbound: an unbound row cannot be accepted, so carrying on would only move
// the refusal one page later.
func newBindingNonce() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// hashBindingNonce is the digest form the row stores. The plaintext nonce lives
// in the cookie and nowhere else.
func hashBindingNonce(nonce string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(nonce)))
	return hex.EncodeToString(sum[:])
}

// writeBindingCookie owns the cookie semantics shared by the binding surfaces.
//
// The magic-link path has its own name and path constants and cannot share the
// jar entry, but the DECISION -- HttpOnly, Secure when the transport is,
// SameSite=Lax, and a MaxAge derived from the credential's own remaining life
// rather than from a second constant -- is worth having in exactly one place.
// Two independent notions of how long a binding lives would agree today and
// drift silently later, in the worse direction: a cookie that expires early
// turns every same-device click into a cross-device refusal, which reads to the
// user as "the link stopped working on my own machine". That is the failure
// bindingLifetime was written to avoid, and this shares its shape.
func (s *Server) writeBindingCookie(w http.ResponseWriter, name, path, nonce string, ttl time.Duration) {
	if w == nil || strings.TrimSpace(nonce) == "" {
		return
	}
	maxAge := int(ttl / time.Second)
	if maxAge <= 0 {
		maxAge = int((10 * time.Minute) / time.Second)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    nonce,
		Path:     path,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.cookieSecure(),
		SameSite: http.SameSiteLaxMode,
	})
}

// clearBindingCookie retires a spent binding. Not a security control -- the
// row's accepted status is -- but see clearMagicLinkCookie for why it is worth
// doing anyway.
func (s *Server) clearBindingCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cookieSecure(),
		SameSite: http.SameSiteLaxMode,
	})
}

// requireSecureInvitation refuses a plaintext hop. The invitation code is a
// bearer credential sitting in a URL, which is the one place a credential is
// most likely to be written down by something else -- a proxy access log, a
// browser history sync, a referrer header. Shares its transport predicate with
// pair.go and /enroll via identity.RequestIsSecure.
func (s *Server) requireSecureInvitation(w http.ResponseWriter, r *http.Request, sourceIP string) bool {
	if identity.RequestIsSecure(r) {
		return true
	}
	if identity.InsecureTransportEscapeEnabled(envAllowInsecureInvitation) {
		if s.Logger != nil {
			s.Logger.Warn("/invitation admitting plaintext request via "+envAllowInsecureInvitation+"=1; production must leave this unset",
				"remote", sourceIP)
		}
		return true
	}
	s.auditInvitation(r, "invitation_redeem_denied", "", identity.AuditOutcomeBlocked, "insecure_transport")
	w.WriteHeader(http.StatusForbidden)
	s.renderInvitationRejection(w, r, InvitationInvalid,
		"This link needs a secure connection",
		"Invitation links carry a credential, so they are only accepted over https.",
		"Open the same link with https:// at the front.")
	return false
}

// invitationCopy is the human text for each rejection state.
//
// FIVE MESSAGES, NOT ONE. Each tells the reader a different thing to do next,
// and the difference is the whole point: "expired" means ask for another,
// "already used" means check whether that was you, "revoked" means talk to
// whoever issued it, "wrong kind" means this link is for something else
// entirely, and only "invalid" leaves someone with nothing but the link to
// re-check. Collapsing them would send four of those five people to the wrong
// place. /enroll makes the same argument for its four.
//
// Plus two about this browser rather than the row: "bound elsewhere" sends the
// reader back to the browser where Continue was pressed, and "not continued"
// to the step they skipped.
func invitationCopy(state InvitationState) (heading, message, nextStep string) {
	switch state {
	case InvitationBoundElsewhere:
		return "Finish in the browser you continued with",
			"For safety, an invitation can only be accepted in the same browser it was continued with. " +
				"This looks like a different browser, a different device, or a private window.",
			"Open the original link again in the browser where you pressed Continue. " +
				"If you cannot, ask whoever invited you for a fresh invitation."
	case InvitationNotContinued:
		return "Press Continue first",
			"An invitation is accepted in the browser where Continue was pressed, and it has not been pressed yet.",
			"Open the link in your invitation email again and press Continue."
	case InvitationExpired:
		return "This invitation has expired",
			"Invitations are time-limited on purpose, and this one ran out before it was used.",
			"Ask whoever invited you to send a fresh one."
	case InvitationAlreadyUsed:
		return "This invitation has already been used",
			"Each invitation creates exactly one account, and this one has already done that.",
			"If that was you, just sign in. If it was not, tell whoever invited you straight away."
	case InvitationRevoked:
		return "This invitation was cancelled",
			"Whoever invited you cancelled this invitation before it was used.",
			"Ask them for a new one if you still need access."
	case InvitationWrongKind:
		return "This link is for something else",
			"This is a guest link, which lets you join a shared space rather than create an account on the cluster.",
			"Open it from the message it came in, or ask for an invitation to the cluster itself."
	default:
		return "This invitation is not valid",
			"We do not recognise this invitation. It may have been mistyped, cut short by an email client, or meant for a different cluster.",
			"Check you copied the whole link, or ask whoever invited you for a new one."
	}
}

// invitationRejectionStatus maps a state to an HTTP status. The page is the
// product here, but the status still has to be honest for anything reading the
// response rather than looking at it.
func invitationRejectionStatus(state InvitationState) int {
	switch state {
	case InvitationExpired:
		return http.StatusGone
	case InvitationAlreadyUsed:
		return http.StatusConflict
	case InvitationRevoked, InvitationWrongKind, InvitationBoundElsewhere, InvitationNotContinued:
		return http.StatusForbidden
	default:
		return http.StatusNotFound
	}
}

// refuseInvitation writes the refusal page for state, with its status and copy.
func (s *Server) refuseInvitation(w http.ResponseWriter, r *http.Request, state InvitationState) {
	heading, message, nextStep := invitationCopy(state)
	w.WriteHeader(invitationRejectionStatus(state))
	s.renderInvitationRejection(w, r, state, heading, message, nextStep)
}

func (s *Server) renderInvitationRejection(w http.ResponseWriter, r *http.Request, state InvitationState, heading, message, nextStep string) {
	data := webtempl.InvitationData{
		Layout:    s.LayoutData(r, "Invitation", false, nil, nil),
		Rejection: string(state),
		Heading:   heading,
		Message:   message,
		NextStep:  nextStep,
	}
	s.render(w, r, "invitation", webtempl.Invitation(data), data)
}

// auditInvitation emits one v1:identity:auditEvent per outcome, always with
// SourceIP -- the address is the only thing a redeem carries that identifies
// the party holding the link.
func (s *Server) auditInvitation(r *http.Request, action, invitationId string, outcome identity.AuditOutcome, failureReason string) {
	if s == nil || s.invitationAudit == nil || r == nil {
		return
	}
	s.invitationAudit.Log(r.Context(), identity.AuditEvent{
		OccurredAt:    time.Now().UTC(),
		Category:      identity.AuditCategoryAuth,
		Action:        action,
		TargetType:    "invitation",
		TargetId:      invitationId,
		SourceIP:      clientIP(r),
		UserAgent:     r.Header.Get("User-Agent"),
		Outcome:       outcome,
		FailureReason: failureReason,
	})
}
