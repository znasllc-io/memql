// Package registration owns the policy decisions that fire when a
// previously-unknown email shows up at /auth/magic-link. Different
// MEMQL_IDENTITY_REGISTRATION_MODE settings produce different downstream
// actions (issue a magic link, enqueue an access request, reject); the
// magiclink.Issuer calls Decide() to find out which path applies and
// what user-shape to provision after a successful consume.
package registration

import (
	"errors"
	"strings"

	"github.com/znasllc-io/memql/component/identity"
)

// Action enumerates what the issuer should do for a given email under
// the current registration mode.
type Action string

const (
	// ActionIssueMagicLink — proceed with the normal magic-link path.
	ActionIssueMagicLink Action = "issue_magic_link"
	// ActionCreateAccessRequest — enqueue a v1:identity:accessRequest
	// row instead. Used by waitlist mode for emails without an
	// approved invitation.
	ActionCreateAccessRequest Action = "create_access_request"
	// ActionReject — refuse outright. Used by invite_only mode when
	// no invitation token is supplied.
	ActionReject Action = "reject"
)

// Decision is the per-email policy verdict returned by Decide.
type Decision struct {
	// Action tells the caller which path to take.
	Action Action
	// Internal is true when the email matched an internal-domains
	// allowlist. The user row should be stamped internal=true at
	// creation time.
	Internal bool
	// Role is the cluster-wide role to grant on user creation.
	// Empty for external users (no cluster-wide role).
	Role string
	// Reason is a human-readable label used for audit + the rejection
	// error message. Free-form.
	Reason string
}

// ErrEmailNotAllowed is returned when the email's domain doesn't pass
// the configured registration policy.
var ErrEmailNotAllowed = errors.New("registration: email domain not allowed")

// Invitation is a RESOLVED invitation -- a row that was found by the hash of
// the token somebody presented, and checked (memql#4282).
//
// It replaces the `hasInvitation bool` this function used to take, and the
// change of type IS the fix. A boolean could only ever mean "the caller sent a
// non-empty string", and that is exactly what it meant: the presented value was
// never hashed, never looked up, never checked for expiry, acceptance,
// revocation or kind, and never checked against the address being registered.
// Under invite_only -- the mode an operator selects precisely to close
// registration -- typing any characters into a field labelled "invitation" let
// anybody in, and domain_restricted was bypassed the same way, because the
// invitation branch returns before the allowlist is consulted.
//
// A nil *Invitation means no usable invitation was presented. Callers resolve
// it; this package decides what it MEANS.
type Invitation struct {
	// Id is the v1:identity:invitation row id, for the audit trail.
	Id string
	// Email is the address the invitation was issued FOR. Decide refuses an
	// invitation whose address is not the one registering: without that check
	// one leaked link is a general-purpose bypass rather than a credential for
	// one person.
	Email string
	// Role is the cluster role the issuer chose for the recipient. Empty means
	// the cluster default.
	Role string
}

// Decide evaluates cfg + email + a RESOLVED invitation and returns the
// downstream Action plus the user-shape metadata for that action.
//
// A valid invitation flips invite_only / waitlist modes from "reject" /
// "create access request" to "issue magic link" -- an admin-approved
// invitation overrides the gating policy. An invitation for a DIFFERENT
// address does not, and is treated as though none were presented: the modes
// then decide on their own terms, which is the honest outcome (under `open`
// the person can register anyway; under invite_only they cannot).
func Decide(cfg identity.Config, email string, invite *Invitation) (Decision, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return Decision{}, ErrEmailNotAllowed
	}

	internal := cfg.IsInternalEmail(email)
	role := ""
	if internal {
		role = cfg.InternalDefaultRole
	}

	// A VALID invitation, for THIS address, overrides everything else.
	//
	// The address check is not a formality. An invitation names one person; if
	// any holder of any live token could register any address, the credential
	// would authorize the wrong thing entirely -- and a single link forwarded
	// once would reopen a cluster its operator had closed.
	if invite != nil && strings.EqualFold(strings.TrimSpace(invite.Email), email) {
		invitedRole := role
		if r := strings.TrimSpace(invite.Role); r != "" {
			// The issuer's choice wins over the internal-domain default: an
			// admin who deliberately invited somebody as a reader meant it.
			invitedRole = r
		}
		return Decision{
			Action:   ActionIssueMagicLink,
			Internal: internal,
			Role:     invitedRole,
			Reason:   "invitation",
		}, nil
	}

	switch cfg.RegistrationMode {
	case identity.RegistrationModeOpen:
		return Decision{
			Action:   ActionIssueMagicLink,
			Internal: internal,
			Role:     role,
			Reason:   "open_registration",
		}, nil

	case identity.RegistrationModeDomainRestricted:
		if !cfg.IsAllowedRegistrationDomain(email) {
			return Decision{
				Action: ActionReject,
				Reason: "domain_not_allowed",
			}, ErrEmailNotAllowed
		}
		return Decision{
			Action:   ActionIssueMagicLink,
			Internal: internal,
			Role:     role,
			Reason:   "domain_allowlist",
		}, nil

	case identity.RegistrationModeInviteOnly:
		return Decision{
			Action: ActionReject,
			Reason: "invite_only_no_invitation",
		}, ErrEmailNotAllowed

	case identity.RegistrationModeWaitlist:
		return Decision{
			Action: ActionCreateAccessRequest,
			Reason: "waitlist",
		}, nil

	case identity.RegistrationModeDirectory:
		// DIRECTORY MEMBERSHIP IS THE INVITATION (memql#4611), and this
		// function is reached from the EMAIL path -- /auth/magic-link -- so
		// arriving here means somebody typed an address rather than
		// authenticating through the provider.
		//
		// That is a refusal, and the reason it is a distinct mode rather than
		// a flag on invite_only is what the refusal has to SAY: under
		// invite_only the answer is "ask an admin for an invitation", and here
		// it is "sign in with your organization account", which is a different
		// instruction to a person who has one and does not know it applies.
		//
		// The IdP path does not come through Decide at all: a verified
		// upstream identity is admitted by the callback handler, which is the
		// whole point -- the gate is somebody else's directory, and it cannot
		// be evaluated from an email address here.
		return Decision{
			Action: ActionReject,
			Reason: "directory_sign_in_required",
		}, ErrEmailNotAllowed
	}

	// Defensive: unknown mode falls through to reject.
	return Decision{
		Action: ActionReject,
		Reason: "unknown_registration_mode",
	}, ErrEmailNotAllowed
}
