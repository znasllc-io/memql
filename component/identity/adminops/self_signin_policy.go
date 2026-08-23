package adminops

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
)

// self_signin_policy.go -- the SELF-SERVICE half of the passkey-only control
// (memql#4304 supplies the field; memql#4319 renders the control in the
// portal).
//
// # Why this sits in adminops, which is otherwise the admin gate
//
// Because the other direction already does. ResetSignInPolicy next door is
// the owner/admin rescue -- turn links back ON for somebody who turned them
// off and then lost their passkey -- and the two halves of one policy
// reasoning about the same field from two packages is how they drift. What
// this file does NOT borrow from its neighbours is `authorize`: that is the
// owner/admin role check, and applying it here would refuse the only caller
// this path is for.
//
// # The authorization is the absence of a parameter
//
// setUserSignInPolicy is @serverOnly, and its doc comment explains why an
// actor-scoped filter would be wrong: the admin reset is a legitimate write
// against a row the actor does not own. So the self-service half cannot lean
// on the DSL for scoping and has to carry its own -- which it does by having
// nothing to aim. The row written is the resolved caller's. There is no user
// id on the message, none in this signature, and none reachable from the
// wire; a caller who wants to change somebody else's policy has no way to
// name them.
//
// # The precondition is enforced here, not by the control
//
// Turning on passkey_only with no passkey enrolled locks a person out of
// their own account, as far as they can tell permanently. The portal renders
// the switch disabled in that state and identity's /me/settings does the
// same -- but a disabled control is a suggestion, and this is not a
// suggestion.
//
// It FAILS CLOSED. An unreadable passkey list refuses the change rather than
// assuming there is a passkey, because a transport blip and "no passkeys"
// must not reach the same decision when the difference is a lockout. That is
// the rule component/identity/web/me_signin_policy.go states for its own
// copy of this check, and this is the second enforcement point of ONE rule:
// the two nodes read different passkey adapters (identity has its own; a bff
// has the engine), so the reads cannot be shared, but the DECISION and the
// sentence a person is shown are pinned in component/identity so the two
// cannot come to disagree about what the rule is.

// SetOwnSignInPolicy sets the CALLING user's sign-in policy.
//
// policy is "any" or "passkey_only"; anything else is refused. Re-setting the
// policy already in force is reported as success with no write -- the caller
// asked for a state and the state holds.
func (s *Service) SetOwnSignInPolicy(ctx context.Context, policy string) Result {
	const action = "sign_in_policy_changed"
	policy = strings.TrimSpace(policy)
	detail := map[string]any{"by": "self", "to": policy}

	act, resolved := resolveActor(ctx)
	if !resolved || strings.TrimSpace(act.userID) == "" {
		return fail(CodeUnauthenticated, s.emit(ctx, identity.AuditCategoryIdentity, action,
			actor{}, "", "", detail, identity.AuditOutcomeBlocked, "no_authenticated_actor"),
			"identity: no authenticated caller on this connection")
	}
	userID := strings.TrimSpace(act.userID)

	if policy != identity.SignInPolicyAny && policy != identity.SignInPolicyPasskeyOnly {
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryIdentity, action,
			act, userID, act.email, detail, identity.AuditOutcomeFailure, "unknown_policy"),
			fmt.Sprintf("identity: %q is not a sign-in policy", policy))
	}

	if policy == identity.SignInPolicyPasskeyOnly {
		count, err := s.activePasskeyCount(ctx)
		if err != nil {
			// FAIL CLOSED. Without the count there is no way to tell a safe
			// change from a lockout, and making one blind is precisely what
			// this precondition exists to prevent.
			if s.Logger != nil {
				s.Logger.Warn("identity: passkey count failed; refusing sign-in policy change",
					"error", err, "userId", userID)
			}
			return fail(CodeFailedPrecondition, s.emit(ctx, identity.AuditCategoryIdentity, action,
				act, userID, act.email, detail, identity.AuditOutcomeFailure, "passkey_count_unreadable"),
				identity.SignInPolicyPrecheckFailedMessage)
		}
		detail["activePasskeys"] = count
		if count == 0 {
			return fail(CodeFailedPrecondition, s.emit(ctx, identity.AuditCategoryIdentity, action,
				act, userID, act.email, detail, identity.AuditOutcomeFailure, "no_active_passkey"),
				identity.SignInPolicyNeedsPasskeyMessage)
		}
	}

	user, err := s.userById(ctx, userID)
	if err != nil || user == nil {
		return s.notFound(ctx, action, act, userID, detail, err)
	}
	from := signInPolicyOrDefault(user.SignInPolicy)
	detail["from"] = from
	if from == policy {
		return ok(s.emit(ctx, identity.AuditCategoryIdentity, action, act, userID, user.PrimaryEmail,
			detail, identity.AuditOutcomeSuccess, ""),
			identity.SignInPolicyMessage(policy))
	}

	user.SignInPolicy = policy
	return s.finish(ctx, identity.AuditCategoryIdentity, action, act, userID, user.PrimaryEmail,
		detail, identity.SignInPolicyMessage(policy), s.writeUser(ctx, user))
}

// OwnSignInPolicy reads the policy currently in force on the CALLER'S OWN
// account, normalized (an empty stored value reads as "any").
//
// It exists so a refusal can report the state the caller still has rather
// than leaving a client rendering the value it optimistically flipped to. Same
// no-parameter scoping as the setter: there is no user id to aim.
func (s *Service) OwnSignInPolicy(ctx context.Context) (string, error) {
	act, resolved := resolveActor(ctx)
	if !resolved || strings.TrimSpace(act.userID) == "" {
		return "", fmt.Errorf("adminops: no authenticated caller")
	}
	user, err := s.userById(ctx, strings.TrimSpace(act.userID))
	if err != nil {
		return "", err
	}
	if user == nil {
		return "", fmt.Errorf("adminops: no user row for the calling actor")
	}
	return signInPolicyOrDefault(user.SignInPolicy), nil
}

// activePasskeyCount reports how many ACTIVE passkeys the caller holds.
//
// Read through passkeysForSelf under the CALLER'S OWN actor context -- not
// the internal-origin context the neighbouring admin reads use. That is the
// ownership check as well as the count: the query's filter is
// `userId==actor.userId`, so there is no caller-supplied user id anywhere in
// this path and no way to ask how recoverable somebody else's account is.
//
// Returns an error rather than zero when the list cannot be read, so the
// caller can fail closed instead of reading a transport blip as "no
// passkeys".
func (s *Service) activePasskeyCount(ctx context.Context) (int, error) {
	if s == nil || s.Engine == nil {
		return 0, fmt.Errorf("adminops: engine not configured")
	}
	if _, resolved := auth.AccessFromContext(ctx); !resolved {
		// passkeysForSelf resolves its row set from actor.userId. With no
		// actor the query matches nothing, which would read as "no passkeys"
		// -- the one answer that must never be produced by accident here.
		return 0, fmt.Errorf("adminops: no access context for a self-scoped passkey read")
	}
	res, err := s.Engine.Execute(ctx, `query passkeysForSelf()`)
	if err != nil {
		return 0, fmt.Errorf("adminops: list own passkeys: %w", err)
	}
	if res == nil || res.Bundle == nil {
		return 0, nil
	}
	count := 0
	for _, node := range res.Bundle.Nodes {
		if node == nil || node.Payload == nil {
			continue
		}
		if v, present := node.Payload.GetFields()["active"]; present && v != nil && v.GetBoolValue() {
			count++
		}
	}
	return count, nil
}

