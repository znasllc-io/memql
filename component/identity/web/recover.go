package web

// GET /recover?code=mql_rec_...
//
// The redeem end of the owner recovery key (memql#3968): the page a
// break-glass credential lands on. It validates the key, then renders the SAME
// passkey registration card /enroll renders, driven by the SAME
// static/enroll.js against /auth/webauthn/register/{begin,finish}.
//
// WHY THIS PAGE EXISTS. The ceremony needs a browser -- navigator.credentials
// .create() runs in a document, and there is no wire form of "the user touched
// their security key". So a recovery key that could only be presented by a CLI
// could not actually recover anything. This is the entry point that turns the
// key into a passkey.
//
// GENERALISED, NOT FORKED. Everything below is the /enroll handler with a
// different resolver and different copy; the page component, the driver
// script, the ceremony endpoints and the single-use-at-finish semantics are
// shared. What differs is what a refusal means and what the reader should do
// about it -- which is exactly the part that cannot be shared, because the
// reader of this page is locked out rather than being onboarded.
//
// WHAT IT DOES NOT DO: spend the key. The redemption stamp lands on the
// registration FINISH call, server-side, so a reload or an abandoned tab does
// not burn the one credential that gets a locked-out owner back in. See
// requireRecoveryKey and spendRecoveryKey in
// component/identity/http/webauthn_recovery.go.
//
// It follows /enroll's discipline throughout: HTTPS required (a plaintext hop
// would put the bearer in a proxy log), per-IP rate limiting, and an audit
// event with SourceIP on every outcome including each refusal.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	webtempl "github.com/znasllc-io/memql/component/identity/web/templ"
)

// RecoveryState mirrors recoverykey.State without importing the engine-facing
// package into web, exactly as EnrolmentState does.
//
// NOTE THERE IS NO `expired`. A recovery key does not expire: it is minted
// when the cluster is claimed and used, if ever, on the worst day of the
// operator's year, and one that had quietly lapsed in between would be
// indistinguishable from one that never worked.
type RecoveryState string

const (
	RecoveryValid RecoveryState = "valid"
	// RecoveryInvalid means no row matched -- a typo, a truncated key, or one
	// from another cluster.
	RecoveryInvalid RecoveryState = "invalid"
	// RecoveryAlreadyRedeemed means the key was spent. A replacement was
	// minted when it was.
	RecoveryAlreadyRedeemed RecoveryState = "already-redeemed"
	// RecoveryDeactivated means an owner rotated it out without using it.
	RecoveryDeactivated RecoveryState = "deactivated"
	// RecoveryNotNeeded is the BREAK-GLASS REFUSAL (memql#3967): the bound
	// owner can still sign in normally, so the key is not spent.
	//
	// It is a state of the REQUEST rather than of the row -- the key is
	// perfectly valid -- and it is listed here because from the holder's chair
	// it is one of the things that can happen when they open the link, and
	// they need a different sentence for it than "invalid".
	RecoveryNotNeeded RecoveryState = "not-needed"
)

// RecoveryResolution is what the wiring layer reports back about a presented
// key.
//
// It carries NO key material -- not the plaintext, not the hash. The page has
// the plaintext already (it is in the address bar) and has no use for the
// hash, so neither belongs in a struct that crosses a package boundary and
// might one day be logged.
type RecoveryResolution struct {
	State RecoveryState
	// UserId is the bound owner. Empty unless a row matched.
	UserId string
	// AccountLabel is what to show the holder: an email when the user row has
	// one, the user id otherwise.
	AccountLabel string
	// RecoveryKeyId is the row id, for the audit trail.
	RecoveryKeyId string
}

// ResolveRecoveryFunc validates a presented recovery key AND applies the
// break-glass gate. Supplied by the wiring layer, which owns the store; nil
// leaves /recover unmounted.
//
// THE GATE IS THE RESOLVER'S JOB, NOT THIS PACKAGE'S, and that is deliberate:
// answering "does this owner still have a sign-in route" needs the engine, and
// this package stays engine-free. The resolver returns RecoveryNotNeeded when
// the answer is yes.
type ResolveRecoveryFunc func(ctx context.Context, plainKey string) (RecoveryResolution, error)

// SetResolveRecovery wires the validator and the audit sink /recover needs.
// Both are required: an unaudited credential-redeem surface is not something
// this package will mount.
func (s *Server) SetResolveRecovery(resolve ResolveRecoveryFunc, audit identity.AuditLogger) {
	if s == nil || resolve == nil || audit == nil {
		return
	}
	s.resolveRecovery = resolve
	s.recoverAudit = audit
}

// handleRecover renders the recovery page for a presented key.
func (s *Server) handleRecover(w http.ResponseWriter, r *http.Request) {
	sourceIP := clientIP(r)

	if !s.requireSecureRecovery(w, r, sourceIP) {
		return
	}
	// The SAME per-IP limiter /enroll uses. Shared on purpose: both are
	// credential lookups keyed on a guessable-in-principle string reaching the
	// same service from the same address, and a script that has exhausted one
	// should not get a fresh allowance by switching paths.
	if allowed, retryAfter := s.enrolLimiter().Allow(sourceIP); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		s.auditRecover(r, "recovery_redeem_denied", "", "", identity.AuditOutcomeBlocked, "rate_limited")
		w.WriteHeader(http.StatusTooManyRequests)
		s.renderRecoverRejection(w, r, RecoveryInvalid,
			"Too many attempts",
			"Too many recovery attempts have come from this network in the last hour.",
			"Wait a few minutes and open the link again.")
		return
	}

	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		s.auditRecover(r, "recovery_redeem_denied", "", "", identity.AuditOutcomeBlocked, "missing_code")
		heading, message, nextStep := recoverCopy(RecoveryInvalid)
		w.WriteHeader(http.StatusBadRequest)
		s.renderRecoverRejection(w, r, RecoveryInvalid, heading, message, nextStep)
		return
	}

	res, err := s.resolveRecovery(r.Context(), code)
	if err != nil {
		s.Logger.Warn("identity-web: recovery resolve failed", "error", err.Error())
		s.auditRecover(r, "recovery_redeem_denied", "", "", identity.AuditOutcomeFailure, "resolve_failed")
		w.WriteHeader(http.StatusInternalServerError)
		s.renderRecoverRejection(w, r, RecoveryInvalid,
			"Something went wrong",
			"We could not check this recovery key just now.",
			"Try opening the link again in a moment.")
		return
	}

	if res.State != RecoveryValid {
		// EVERY refusal is audited, with its own reason. A burst of
		// already-redeemed is a replay attempt; a burst of invalid is somebody
		// spraying guesses at a break-glass endpoint, which is the single most
		// important thing this trail can surface.
		s.auditRecover(r, "recovery_redeem_denied", res.UserId, res.RecoveryKeyId,
			identity.AuditOutcomeBlocked, "recovery_"+strings.ReplaceAll(string(res.State), "-", "_"))
		heading, message, nextStep := recoverCopy(res.State)
		w.WriteHeader(recoverRejectionStatus(res.State))
		s.renderRecoverRejection(w, r, res.State, heading, message, nextStep)
		return
	}

	s.auditRecover(r, "recovery_page_served", res.UserId, res.RecoveryKeyId, identity.AuditOutcomeSuccess, "")

	data := webtempl.EnrollData{
		Layout:       s.LayoutData(r, "Recover your account", false, nil, []string{s.assetURL("/static/enroll.js")}),
		AuthScheme:   "Recovery",
		LiveHeading:  "Recover your account",
		AccountLabel: res.AccountLabel,
		SingleUseNote: "This recovery key works once. Using it now sets up a passkey and mints a " +
			"replacement key, which you can claim from the cluster when you are back in.",
	}
	s.render(w, r, "enroll", webtempl.Enroll(data))
}

// requireSecureRecovery refuses a plaintext hop, sharing /enroll's transport
// predicate and its escape hatch.
//
// The SAME env var rather than a third one: an operator who has already
// admitted plaintext enrolment links on a laptop has made exactly this
// decision once, and making them make it twice produces two settings that
// drift rather than one that is understood.
func (s *Server) requireSecureRecovery(w http.ResponseWriter, r *http.Request, sourceIP string) bool {
	if identity.RequestIsSecure(r) {
		return true
	}
	if identity.InsecureTransportEscapeEnabled(envAllowInsecureEnroll) {
		if s.Logger != nil {
			s.Logger.Warn("/recover admitting plaintext request via "+envAllowInsecureEnroll+"=1; production must leave this unset",
				"remote", sourceIP)
		}
		return true
	}
	s.auditRecover(r, "recovery_redeem_denied", "", "", identity.AuditOutcomeBlocked, "insecure_transport")
	w.WriteHeader(http.StatusForbidden)
	s.renderRecoverRejection(w, r, RecoveryInvalid,
		"This link needs a secure connection",
		"Recovery keys are credentials, so they are only accepted over https.",
		"Open the same link with https:// at the front.")
	return false
}

// recoverCopy is the human text for each rejection state.
//
// FOUR MESSAGES, NOT ONE, for the reason enrolCopy records -- each tells the
// reader a different thing to do next. The `not-needed` one is the newest and
// the least obvious: the holder has a perfectly good key and is being told not
// to spend it, so the message has to make clear that nothing is broken and
// nothing has been used up.
func recoverCopy(state RecoveryState) (heading, message, nextStep string) {
	switch state {
	case RecoveryAlreadyRedeemed:
		return "This recovery key has already been used",
			"A recovery key works once. This one has already set up a passkey, and a replacement key was minted at the same moment.",
			"If that was you, sign in with the passkey you created. If it was not, treat this as a compromise: sign in, rotate the recovery key, and review the account's passkeys."
	case RecoveryDeactivated:
		return "This recovery key was replaced",
			"Somebody rotated the cluster's recovery key, which retires the previous one. This is the previous one.",
			"Use the key from the most recent rotation."
	case RecoveryNotNeeded:
		return "You can still sign in normally",
			"A recovery key is refused while the account still has a working way in -- that is what makes it a break-glass credential rather than a second password. Nothing has been used up.",
			"Sign in as you normally would, then add a passkey from your device settings. Keep this key for when it is genuinely needed."
	default:
		return "This recovery key is not valid",
			"We do not recognise this key. It may have been mistyped, cut short when it was copied, or meant for a different cluster.",
			"Check you copied the whole key, or claim the cluster's current one from inside the identity pod."
	}
}

// recoverRejectionStatus maps a state to an HTTP status. The page is the
// product here, but the status still has to be honest for anything reading the
// response rather than looking at it.
func recoverRejectionStatus(state RecoveryState) int {
	switch state {
	case RecoveryAlreadyRedeemed:
		return http.StatusConflict
	case RecoveryDeactivated, RecoveryNotNeeded:
		return http.StatusForbidden
	default:
		return http.StatusNotFound
	}
}

func (s *Server) renderRecoverRejection(w http.ResponseWriter, r *http.Request, state RecoveryState, heading, message, nextStep string) {
	data := webtempl.EnrollData{
		Layout:    s.LayoutData(r, "Account recovery", false, nil, nil),
		Rejection: string(state),
		Heading:   heading,
		Message:   message,
		NextStep:  nextStep,
	}
	s.render(w, r, "enroll", webtempl.Enroll(data))
}

// auditRecover emits one v1:identity:auditEvent per outcome, always with
// SourceIP -- the address is the only thing a redeem carries that identifies
// the party holding the key, and for a break-glass credential that is the
// single most valuable field in the trail.
func (s *Server) auditRecover(r *http.Request, action, actorUserId, recoveryKeyId string, outcome identity.AuditOutcome, failureReason string) {
	if s == nil || s.recoverAudit == nil || r == nil {
		return
	}
	s.recoverAudit.Log(r.Context(), identity.AuditEvent{
		OccurredAt:  time.Now().UTC(),
		Category:    identity.AuditCategoryAuth,
		Action:      action,
		ActorUserId: actorUserId,
		// "identity" rather than a new target type: a recovery key IS a
		// v1:identity:identity row of the recovery_key variant, and the
		// targetType enum already contains this value. Choosing the variant
		// over a new concept is what avoided widening that enum (memql#3964).
		TargetType:    "identity",
		TargetId:      recoveryKeyId,
		SourceIP:      clientIP(r),
		UserAgent:     r.Header.Get("User-Agent"),
		Outcome:       outcome,
		FailureReason: failureReason,
	})
}
