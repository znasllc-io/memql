package http

import (
	"net/http"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/recoverykey"
)

// webauthn_recovery.go -- the `Authorization: Recovery` arm (memql#3968).
//
// NO NEW CEREMONY CODE, AND THAT IS THE DESIGN RATHER THAN AN ECONOMY. What a
// recovery key authorizes is EXACTLY what an enrolment token authorizes --
// register a passkey as one named user -- so it resolves to the same
// passkeyEnroller, runs through the same begin/finish handlers, the same
// challenge store, the same per-IP limiter and the same attestation
// verification. A second ceremony would be a second place for the WebAuthn
// rules to drift, on the surface where drift is least visible.
//
// What differs is only WHICH USER the ceremony is for, and WHETHER THE SERVER
// IS WILLING -- the two things below.
//
// THE OWNER COMES FROM THE ROW, NEVER FROM A CALLER ARGUMENT. A leaked key can
// therefore only ever add a credential to the one account it was minted for.
// The same property the enrolment arm has, and for the same reason.

// AuthSchemeRecovery is the Authorization scheme an owner recovery key
// presents itself under: `Authorization: Recovery mql_rec_<...>`.
//
// Deliberately the same shape as AuthSchemeEnrolment and pair.go's
// `Authorization: Pair <code>` -- the credential IS the authorization, so it
// travels where a bearer travels rather than in a body field or a query
// parameter a proxy would log. Admitted on the two passkey-registration routes
// ONLY; every other surface authenticates with a Bearer JWT and does not know
// this scheme exists.
const AuthSchemeRecovery = "Recovery"

// requireRecoveryKey validates a presented recovery key and applies the
// break-glass gate.
//
// Every rejection is audited with its own reason and answered with its own
// error code, matching requireEnrolmentToken: the codes are what the /recover
// page branches on to tell the holder which thing went wrong, and a burst of
// one code means something quite different from a burst of another.
func (s *Server) requireRecoveryKey(w http.ResponseWriter, r *http.Request, deniedAction, plain string) (passkeyEnroller, bool) {
	if s.Store == nil || s.Store.Engine == nil {
		writeJSON(w, http.StatusInternalServerError, WebAuthnRegisterBeginResponse{
			ErrorCode: "server", Error: "identity engine not wired"})
		return passkeyEnroller{}, false
	}
	store := &recoverykey.Store{Engine: s.Store.Engine, Logger: s.Logger}
	row, state, err := store.Resolve(r.Context(), plain)
	if err != nil {
		s.logErr("recovery: key lookup failed", err)
		s.auditPasskey(r, deniedAction, "", "", identity.AuditOutcomeFailure, "recovery_lookup_failed", nil)
		writeJSON(w, http.StatusInternalServerError, WebAuthnRegisterBeginResponse{
			ErrorCode: "lookup_failed", Error: "recovery key lookup failed"})
		return passkeyEnroller{}, false
	}

	actorUserId := ""
	if row != nil {
		actorUserId = row.BoundOwnerUserId
		if actorUserId == "" {
			actorUserId = row.UserId
		}
	}
	if state != recoverykey.StateValid {
		status, code := recoveryRejectionCode(state)
		s.auditPasskey(r, deniedAction, actorUserId, recoveryTargetId(row), identity.AuditOutcomeBlocked,
			"recovery_"+strings.ReplaceAll(string(state), "-", "_"), nil)
		writeJSON(w, status, WebAuthnRegisterBeginResponse{
			ErrorCode: code, Error: recoveryRejectionMessage(state)})
		return passkeyEnroller{}, false
	}

	owner := strings.TrimSpace(row.BoundOwnerUserId)
	if owner == "" {
		// Fall back to the enclosing row's userId, which is the same value for
		// every row that exists. Both empty is a malformed row: it cannot
		// authorize anything, and is refused rather than defaulted, because
		// every default here is somebody else's account.
		owner = strings.TrimSpace(row.UserId)
	}
	if owner == "" {
		s.auditPasskey(r, deniedAction, "", recoveryTargetId(row), identity.AuditOutcomeFailure,
			"recovery_row_has_no_owner", nil)
		writeJSON(w, http.StatusInternalServerError, WebAuthnRegisterBeginResponse{
			ErrorCode: "recovery_malformed", Error: "recovery key names no owner"})
		return passkeyEnroller{}, false
	}

	// THE BREAK-GLASS GATE (memql#3967). Redemption is refused whenever the
	// bound owner still holds a usable sign-in route, because a key redeemable
	// while an ordinary login works is not a break-glass credential -- it is a
	// second password for the account, and a permanent one.
	//
	// FAIL CLOSED on an error. HasSignInRoute returns (bool, error) precisely
	// so this branch exists: an unknown answer must refuse, since allowing on
	// "I could not tell" is exactly the state an attacker who can disrupt a
	// database read would want.
	hasRoute, err := s.Store.HasSignInRoute(r.Context(), owner)
	if err != nil {
		s.logErr("recovery: sign-in route check failed", err)
		s.auditPasskey(r, deniedAction, owner, recoveryTargetId(row), identity.AuditOutcomeFailure,
			"recovery_route_check_failed", nil)
		writeJSON(w, http.StatusInternalServerError, WebAuthnRegisterBeginResponse{
			ErrorCode: "lookup_failed",
			Error:     "could not determine whether this account still has a sign-in route; refusing"})
		return passkeyEnroller{}, false
	}
	if hasRoute {
		s.auditPasskey(r, deniedAction, owner, recoveryTargetId(row), identity.AuditOutcomeBlocked,
			"recovery_owner_still_has_sign_in_route", nil)
		writeJSON(w, http.StatusForbidden, WebAuthnRegisterBeginResponse{
			ErrorCode: "recovery_not_needed",
			Error: "this account can still sign in normally, so the recovery key is refused. " +
				"Sign in and add a passkey from your device settings; the recovery key stays " +
				"unspent for when it is genuinely needed."})
		return passkeyEnroller{}, false
	}

	return passkeyEnroller{UserId: owner, Recovery: row}, true
}

// recoveryRejectionCode maps a rejection state onto an HTTP status and the
// stable code the /recover page branches on.
//
// NOTE THERE IS NO `expired`. A recovery key does not expire (see the
// recoverykey package comment): it is minted when the cluster is claimed and
// used, if ever, on the worst day of the operator's year, and one that had
// quietly expired in the interim would be indistinguishable from one that
// never worked.
func recoveryRejectionCode(state recoverykey.State) (int, string) {
	switch state {
	case recoverykey.StateAlreadyRedeemed:
		return http.StatusConflict, "recovery_already_redeemed"
	case recoverykey.StateDeactivated:
		return http.StatusForbidden, "recovery_deactivated"
	default:
		return http.StatusUnauthorized, "recovery_invalid"
	}
}

// recoveryRejectionMessage is the API-side sentence for each state. The page's
// own copy lives in the templ component; this is what a non-browser caller (or
// a fetch that lost its page context) reads.
func recoveryRejectionMessage(state recoverykey.State) string {
	switch state {
	case recoverykey.StateAlreadyRedeemed:
		return "this recovery key has already been used; a replacement was minted when it was spent"
	case recoverykey.StateDeactivated:
		return "this recovery key was replaced by a newer one"
	default:
		return "this recovery key is not valid"
	}
}

// recoveryTargetId returns the row's id for an audit event, or "" when there
// is no row. Never returns anything derived from the plaintext key.
func recoveryTargetId(row *recoverykey.Row) string {
	if row == nil {
		return ""
	}
	return recoverykey.CanonicalId(row.ID)
}

// spendRecoveryKey stamps the redemption and deactivates the row, then mints
// the successor.
//
// SPEND BEFORE PERSIST, exactly as the enrolment arm consumes before it
// persists, and for the same reason: a spend that fails after the credential
// landed leaves a LIVE recovery key that has already produced a passkey, which
// is a replay window on the highest-value credential in the cluster.
// Inverting it means a persist error after the stamp burns the key and
// registers nothing -- recoverable, in the direction that does not leave a
// spent credential usable.
//
// THE SUCCESSOR IS BEST-EFFORT AND THE REDEMPTION IS NOT. If minting the
// replacement fails, the redemption still stands: the operator is getting
// their passkey, which is what they came for. The invariant mints the
// successor on the identity node's next start anyway (memql#3965), so the
// failure costs a window rather than the route. Refusing the whole redemption
// over it would be strictly worse -- it would deny recovery to somebody who is
// locked out, in order to preserve a future recovery they cannot use yet.
func (s *Server) spendRecoveryKey(r *http.Request, row *recoverykey.Row, sourceIP string) error {
	// The system CREDENTIAL actor, not the service actor. `recovery_key` is a
	// machineCredentialIdentityType, so the memql#2513 guard admits these
	// writes only from role=="system" or a "system:"-prefixed actor.
	// ContextWithSystemActor is neither -- it stamps role="owner" and an
	// email, and ActorFromToken prefers email over subject, so the actor comes
	// out as "system@identity.memql.local". Under it BOTH writes below were
	// refused: the redemption never stamped, so a spent key stayed usable, and
	// no successor was ever minted. Every sibling credential writer here
	// (webauthn_register.go, pair.go, node_bootstrap.go, badge_grant.go) uses
	// the credential actor for the same reason.
	//
	// It also overrides rather than deferring, which matters here: r.Context()
	// carries no authenticated user (this endpoint IS the authentication), but
	// ContextWithSystemActor would return the context UNCHANGED if it ever
	// did, silently attributing a credential write to whoever was signed in.
	ctx := identity.ContextWithSystemCredentialActor(r.Context())
	store := &recoverykey.Store{Engine: s.Store.Engine, Logger: s.Logger}
	if err := store.Redeem(ctx, row.ID, sourceIP, time.Now().UTC()); err != nil {
		return err
	}

	// The successor's PLAINTEXT is discarded on the next line and that is the
	// point: it goes to nobody, reaches no log, and is revealed only when an
	// operator runs `memql recovery-key claim`.
	_, hash, mintErr := recoverykey.Mint()
	if mintErr != nil {
		s.logErr("recovery: successor mint failed (entropy); the invariant will mint on next boot", mintErr)
		return nil
	}
	successorId, idErr := recoverykey.NewId()
	if idErr != nil {
		s.logErr("recovery: successor id failed; the invariant will mint on next boot", idErr)
		return nil
	}
	owner := strings.TrimSpace(row.BoundOwnerUserId)
	if owner == "" {
		owner = strings.TrimSpace(row.UserId)
	}
	if err := store.Create(ctx, successorId, owner, hash, "system:identity-svc",
		recoverykey.CanonicalId(row.ID), recoverykey.DefaultLabel); err != nil {
		s.logErr("recovery: successor create failed; the invariant will mint on next boot", err)
	}
	return nil
}
