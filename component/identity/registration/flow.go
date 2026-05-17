// Package registration owns the policy decisions that fire when a
// previously-unknown email shows up at /auth/magic-link. Different
// IDENTITY_REGISTRATION_MODE settings produce different downstream
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

// Decide evaluates cfg + email + invitation context and returns the
// downstream Action plus the user-shape metadata for that action.
//
// hasInvitation flips invite_only / waitlist modes from "reject" /
// "create access request" to "issue magic link" — an admin-approved
// invitation overrides the gating policy.
func Decide(cfg identity.Config, email string, hasInvitation bool) (Decision, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return Decision{}, ErrEmailNotAllowed
	}

	internal := cfg.IsInternalEmail(email)
	role := ""
	if internal {
		role = cfg.InternalDefaultRole
	}

	// Invitation overrides everything else.
	if hasInvitation {
		return Decision{
			Action:   ActionIssueMagicLink,
			Internal: internal,
			Role:     role,
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
	}

	// Defensive: unknown mode falls through to reject.
	return Decision{
		Action: ActionReject,
		Reason: "unknown_registration_mode",
	}, ErrEmailNotAllowed
}
