package web

// GET /enroll?code=mql_enr_...
//
// The redeem end of the enrolment token (memql#3408): the page a single-use
// enrolment link lands on. It validates the token, then renders the passkey
// registration page that static/enroll.js drives against
// /auth/webauthn/register/{begin,finish}.
//
// WHY THIS PAGE EXISTS AT ALL. /setup cannot host enrolment on the install
// path: handleSetupGet 404s the moment any user exists, and the install
// wizard's unattended bootstrap (MEMQL_IDENTITY_BOOTSTRAP_*) completes setup
// from env before /setup would ever render. So on the one path where somebody
// most needs a first credential, the page that would have offered it is
// already gone. This one is reachable by a link instead of by a state.
//
// WHAT IT DOES NOT DO: consume the token. The single-use stamp lands on the
// registration FINISH call, server-side, so a reload or an abandoned tab does
// not burn a link that has produced nothing. See requirePasskeyEnroller in
// component/identity/http/webauthn_register.go.
//
// It follows pair.go's discipline throughout: HTTPS required (a plaintext hop
// would put the bearer in a proxy log), per-IP rate limiting through the same
// abuse package, and an audit event with SourceIP on every outcome including
// each refusal.

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/abuse"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// envAllowInsecureEnroll is the dev-only escape hatch that bypasses the
// HTTPS-required check on /enroll.
//
// Named separately from pair.go's MEMQL_IDENTITY_ALLOW_INSECURE_PAIR on
// purpose: an operator debugging worker pairing on a laptop should not thereby
// be admitting plaintext enrolment links. Production must leave it unset; the
// runtime logs a WARN every time a request slips through it.
const envAllowInsecureEnroll = "MEMQL_IDENTITY_ALLOW_INSECURE_ENROLL"

// envEnrollPerHour tunes the per-IP redeem rate limit.
//
// The page itself is cheap, but the request behind it is a credential lookup
// keyed on a guessable-in-principle string. 120/h leaves a real person with
// far more retries than they will ever need and leaves a script nowhere near
// a 256-bit search.
const envEnrollPerHour = "MEMQL_IDENTITY_ENROLL_PER_HOUR"

const defaultEnrollPerHour = 120

// EnrolmentState mirrors enrolment.State without importing the engine-facing
// package into web. The web package stays engine-free (see
// PersistClusterSettingsFunc); the wiring layer maps one onto the other.
type EnrolmentState string

const (
	EnrolmentValid       EnrolmentState = "valid"
	EnrolmentInvalid     EnrolmentState = "invalid"
	EnrolmentExpired     EnrolmentState = "expired"
	EnrolmentAlreadyUsed EnrolmentState = "already-used"
	EnrolmentRevoked     EnrolmentState = "revoked"
)

// EnrolmentResolution is what the wiring layer reports back about a presented
// token.
//
// It carries NO token material -- not the plaintext, not the hash. The page
// has the plaintext already (it is in the address bar) and has no use for the
// hash, so neither belongs in a struct that crosses a package boundary and
// might one day be logged.
type EnrolmentResolution struct {
	State EnrolmentState
	// UserId is the account being enrolled. Empty unless State is valid.
	UserId string
	// AccountLabel is what to show the holder: an email when the user row has
	// one, the user id otherwise.
	AccountLabel string
	// ExpiresAt bounds the live token. Zero unless State is valid.
	ExpiresAt time.Time
	// EnrolmentId is the row id, for the audit trail. Empty when nothing
	// matched.
	EnrolmentId string
}

// ResolveEnrolmentFunc validates a presented enrolment token. Supplied by the
// wiring layer, which owns the store; nil leaves /enroll unmounted.
type ResolveEnrolmentFunc func(ctx context.Context, plainToken string) (EnrolmentResolution, error)

// SetResolveEnrolment wires the validator and the audit sink the /enroll route
// needs. Both are required: an unaudited credential-redeem surface is not
// something this package will mount, for the same reason adminops refuses a
// nil Audit.
func (s *Server) SetResolveEnrolment(resolve ResolveEnrolmentFunc, audit identity.AuditLogger) {
	if s == nil || resolve == nil || audit == nil {
		return
	}
	s.resolveEnrolment = resolve
	s.enrolAudit = audit
}

// enrolLimiter returns this Server's per-IP redeem limiter, built once on
// first use. Server-scoped rather than a package global so each Server (and
// each test) gets its own buckets -- the same pattern as the badge-grant and
// passkey limiters in the sibling http package.
func (s *Server) enrolLimiter() *abuse.IPRateLimiter {
	s.enrolLimiterOnce.Do(func() {
		perHour := defaultEnrollPerHour
		if v := strings.TrimSpace(os.Getenv(envEnrollPerHour)); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				perHour = n
			}
		}
		s.enrolLimiterValue = abuse.NewIPRateLimiter(perHour, s.Logger)
	})
	return s.enrolLimiterValue
}

// handleEnroll renders the enrolment page for a presented link.
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	sourceIP := clientIP(r)

	if !s.requireSecureEnrolment(w, r, sourceIP) {
		return
	}
	if allowed, retryAfter := s.enrolLimiter().Allow(sourceIP); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		s.auditEnrol(r, "enrolment_redeem_denied", "", "", identity.AuditOutcomeBlocked, "rate_limited")
		w.WriteHeader(http.StatusTooManyRequests)
		s.renderEnrolRejection(w, r, EnrolmentInvalid,
			"Too many attempts",
			"Too many enrolment attempts have come from this network in the last hour.",
			"Wait a few minutes and open the link again.")
		return
	}

	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		s.auditEnrol(r, "enrolment_redeem_denied", "", "", identity.AuditOutcomeBlocked, "missing_code")
		heading, message, nextStep := enrolCopy(EnrolmentInvalid)
		w.WriteHeader(http.StatusBadRequest)
		s.renderEnrolRejection(w, r, EnrolmentInvalid, heading, message, nextStep)
		return
	}

	res, err := s.resolveEnrolment(r.Context(), code)
	if err != nil {
		s.Logger.Warn("identity-web: enrolment resolve failed", "error", err.Error())
		s.auditEnrol(r, "enrolment_redeem_denied", "", "", identity.AuditOutcomeFailure, "resolve_failed")
		w.WriteHeader(http.StatusInternalServerError)
		s.renderEnrolRejection(w, r, EnrolmentInvalid,
			"Something went wrong",
			"We could not check this enrolment link just now.",
			"Try opening it again in a moment.")
		return
	}

	if res.State != EnrolmentValid {
		// EVERY refusal is audited, with its own reason. A burst of
		// already-used is a replay attempt; a burst of invalid is a scanner.
		// One collapsed "enrolment_denied" would make those indistinguishable.
		s.auditEnrol(r, "enrolment_redeem_denied", res.UserId, res.EnrolmentId,
			identity.AuditOutcomeBlocked, "enrolment_"+strings.ReplaceAll(string(res.State), "-", "_"))
		heading, message, nextStep := enrolCopy(res.State)
		w.WriteHeader(enrolRejectionStatus(res.State))
		s.renderEnrolRejection(w, r, res.State, heading, message, nextStep)
		return
	}

	s.auditEnrol(r, "enrolment_page_served", res.UserId, res.EnrolmentId, identity.AuditOutcomeSuccess, "")

	data := webtempl.EnrollData{
		Layout:       s.LayoutData(r, "Set up your passkey", false, nil, []string{s.assetURL("/static/enroll.js")}),
		AccountLabel: res.AccountLabel,
		ExpiresIn:    humanizeUntil(res.ExpiresAt, time.Now().UTC()),
	}
	s.render(w, r, "enroll", webtempl.Enroll(data))
}

// requireSecureEnrolment refuses a plaintext hop. The enrolment code is a
// bearer credential sitting in a URL, which is the one place a credential is
// most likely to be written down by something else -- a proxy access log, a
// browser history sync, a referrer header. Shares its transport predicate
// (including the X-Forwarded-Proto arm) with pair.go via
// identity.RequestIsSecure.
func (s *Server) requireSecureEnrolment(w http.ResponseWriter, r *http.Request, sourceIP string) bool {
	if identity.RequestIsSecure(r) {
		return true
	}
	if identity.InsecureTransportEscapeEnabled(envAllowInsecureEnroll) {
		if s.Logger != nil {
			s.Logger.Warn("/enroll admitting plaintext request via "+envAllowInsecureEnroll+"=1; production must leave this unset",
				"remote", sourceIP)
		}
		return true
	}
	s.auditEnrol(r, "enrolment_redeem_denied", "", "", identity.AuditOutcomeBlocked, "insecure_transport")
	w.WriteHeader(http.StatusForbidden)
	s.renderEnrolRejection(w, r, EnrolmentInvalid,
		"This link needs a secure connection",
		"Enrolment links carry a credential, so they are only accepted over https.",
		"Open the same link with https:// at the front.")
	return false
}

// enrolCopy is the human text for each rejection state.
//
// FOUR MESSAGES, NOT ONE. Each of these tells the reader a different thing to
// do next, and the difference is the whole acceptance criterion: "expired"
// means ask again, "already used" means check whether that was you, "revoked"
// means talk to whoever issued it, and only "invalid" leaves someone with
// nothing but the link to re-check.
func enrolCopy(state EnrolmentState) (heading, message, nextStep string) {
	switch state {
	case EnrolmentExpired:
		return "This enrolment link has expired",
			"Enrolment links are short-lived on purpose, and this one ran out before it was used.",
			"Ask whoever sent it for a fresh link."
	case EnrolmentAlreadyUsed:
		return "This enrolment link has already been used",
			"Each link sets up exactly one passkey, and this one has already done that.",
			"If you already have your passkey, just sign in. If you did not set one up, tell whoever issued this link."
	case EnrolmentRevoked:
		return "This enrolment link was revoked",
			"Whoever issued this link cancelled it before it was used.",
			"Ask them for a new one."
	default:
		return "This enrolment link is not valid",
			"We do not recognise this link. It may have been mistyped, cut short by an email client, or meant for a different cluster.",
			"Check you copied the whole link, or ask for a new one."
	}
}

// enrolRejectionStatus maps a state to an HTTP status. The page is the product
// here, but the status still has to be honest for anything reading the
// response rather than looking at it.
func enrolRejectionStatus(state EnrolmentState) int {
	switch state {
	case EnrolmentExpired:
		return http.StatusGone
	case EnrolmentAlreadyUsed:
		return http.StatusConflict
	case EnrolmentRevoked:
		return http.StatusForbidden
	default:
		return http.StatusNotFound
	}
}

func (s *Server) renderEnrolRejection(w http.ResponseWriter, r *http.Request, state EnrolmentState, heading, message, nextStep string) {
	data := webtempl.EnrollData{
		Layout:    s.LayoutData(r, "Enrolment link", false, nil, nil),
		Rejection: string(state),
		Heading:   heading,
		Message:   message,
		NextStep:  nextStep,
	}
	s.render(w, r, "enroll", webtempl.Enroll(data))
}

// auditEnrol emits one v1:identity:auditEvent per outcome, always with
// SourceIP -- the address is the only thing a redeem carries that identifies
// the party holding the link.
func (s *Server) auditEnrol(r *http.Request, action, actorUserId, enrolmentId string, outcome identity.AuditOutcome, failureReason string) {
	if s == nil || s.enrolAudit == nil || r == nil {
		return
	}
	s.enrolAudit.Log(r.Context(), identity.AuditEvent{
		OccurredAt:    time.Now().UTC(),
		Category:      identity.AuditCategoryAuth,
		Action:        action,
		ActorUserId:   actorUserId,
		TargetType:    "enrolmentToken",
		TargetId:      enrolmentId,
		SourceIP:      clientIP(r),
		UserAgent:     r.Header.Get("User-Agent"),
		Outcome:       outcome,
		FailureReason: failureReason,
	})
}

// humanizeUntil renders a coarse "14 minutes" for the page's expiry line.
// Deliberately coarse: a countdown to the second would imply a precision the
// holder cannot act on and would make the page look stale on a reload.
func humanizeUntil(at, now time.Time) string {
	if at.IsZero() {
		return ""
	}
	d := at.Sub(now)
	if d <= 0 {
		return ""
	}
	if d < time.Minute {
		return "less than a minute"
	}
	mins := int(d.Round(time.Minute) / time.Minute)
	if mins < 60 {
		return strconv.Itoa(mins) + " minute" + plural(mins)
	}
	hours := int(d.Round(time.Hour) / time.Hour)
	return strconv.Itoa(hours) + " hour" + plural(hours)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
