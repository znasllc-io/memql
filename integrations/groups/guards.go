package groups

// guards.go -- who may do what to a group (epic memql#5165, D7).
//
// Every guard here is checked against the CALLER's own AccessContext, before
// store.go writes anything under the engine's identity. That split is the
// whole security argument of this package: the caller's authority decides, the
// engine's identity writes, and nothing in between can widen the first.
//
// # Why a builtin has to do this in Go
//
// A builtin's annotation set carries no `@requiresRank` and no
// `@requiresCapability` -- the floor has to live in the handler. That is the
// same conclusion component/logstore and integrations/work reached, and it is
// why the codes below are constants rather than sentences: they are the
// contract the OS keys its copy on, so a refusal that reworded would still be
// recognised and one that renamed would not.
//
// # The rank rule, and its one asymmetry
//
// A caller may place or remove somebody who ranks STRICTLY BELOW them. Nobody
// adds themselves -- which is not politeness, it is the escalation guard:
// self-add plus `update` on group would let any admin walk into every client's
// group. But a caller may always REMOVE themselves, because the alternative is
// a person who cannot leave a group they were placed in, and leaving grants
// nobody anything.

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// The typed refusal codes.
const (
	CodeNoCaller                = "group_no_caller"
	CodeCapabilityMissing       = "group_capability_missing"
	CodeSelfAddRefused          = "group_self_add_refused"
	CodeRankNotBelowCaller      = "group_member_rank_not_below_caller"
	CodeGroupAccountActive      = "group_account_active"
	CodeGroupNotActive          = "group_not_active"
	CodeGroupNotFound           = "group_not_found"
	CodeAccountNotFound         = "group_account_not_found"
	CodeAccountNotActive        = "group_account_not_active"
	CodeTargetUserNotFound      = "group_target_user_not_found"
	CodeAccountKindNotCreatable = "group_account_kind_not_creatable"
)

// Lifecycle values, written out because the store, the capabilities and the
// tests all compare against them.
const (
	StatusActive   = "active"
	StatusArchived = "archived"
	StatusRemoved  = "removed"

	KindAccount = "account"
	KindCustom  = "custom"

	OriginAdded      = "added"
	OriginInvitation = "invitation"
	OriginDomain     = "domain"
)

// caller is the acting principal, resolved once per call.
type caller struct {
	userID  string
	role    auth.Role
	rank    int
	isOwner bool
	// subject is the caller as the capability resolver sees them: the same
	// role and id, plus their groups (epic memql#5296). Built from the
	// VERIFIED caller by auth.SubjectFromContext and nothing else.
	subject auth.Subject
}

// resolveCaller reads the acting principal and refuses anything that is not a
// signed-in person.
//
// An anonymous or connector actor is refused BY NAME rather than falling
// through to a capability check that would refuse it anyway: a connector holds
// no role, so `Capable` would answer false and the message would say the
// caller lacks a permission rather than that they are not a person.
func resolveCaller(ctx context.Context) (caller, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return caller{}, refusal(CodeNoCaller, "no authenticated caller on this connection")
	}
	if ac.IsAnonymousActor() || ac.IsConnector() || strings.TrimSpace(ac.UserId) == "" {
		return caller{}, refusal(CodeNoCaller, "an anonymous or connector actor may not act on groups")
	}
	// LOWERCASED, the principalOf discipline: AccessContext.Role is stamped
	// straight off the user row without folding case, and an unfolded value
	// ranks 0 and matches no capability set. For a CALLER that fails closed;
	// the same slip on a TARGET would fail open, which is why both sides of
	// the rank rule fold.
	slug := strings.ToLower(strings.TrimSpace(string(ac.Role)))
	subject, _ := auth.SubjectFromContext(ctx)
	return caller{
		userID:  strings.TrimSpace(ac.UserId),
		role:    auth.Role(slug),
		rank:    auth.RoleRank(auth.Role(slug)),
		isOwner: slug == string(auth.RoleOwner),
		subject: subject,
	}, nil
}

// requireCapability refuses a caller who does not hold (verb, group) -- by
// role, or by a group or user grant overlaying it (epic memql#5296).
func (c caller) requireCapability(ctx context.Context, verb string) error {
	if auth.CapableFor(ctx, c.subject, verb, auth.ResourceGroup) {
		return nil
	}
	return refusal(CodeCapabilityMissing,
		fmt.Sprintf("role %q holds no %s capability on groups", c.role, verb))
}

// requireTargetBelow refuses unless `targetRole` ranks strictly below the
// caller.
//
// STRICTLY below, so a peer cannot move a peer. Placing somebody into a
// client's group is granting them reach into that client's work, and letting
// two admins place each other makes the grant self-service between anyone at
// one rung.
//
// An OWNER target is refused for anyone but an owner, the carve-out
// canManagePrincipal makes for the same reason: rank alone would admit a
// custom role authored above owner.
func (c caller) requireTargetBelow(targetUserID, targetRole string) error {
	slug := strings.ToLower(strings.TrimSpace(targetRole))
	targetIsOwner := slug == string(auth.RoleOwner)
	if targetIsOwner && !c.isOwner {
		return refusal(CodeRankNotBelowCaller,
			fmt.Sprintf("%s holds the owner role, and only an owner may place or remove an owner", targetUserID))
	}
	if auth.RoleRank(auth.Role(slug)) < c.rank {
		return nil
	}
	return refusal(CodeRankNotBelowCaller,
		fmt.Sprintf("%s holds %q, which does not rank below your %q -- a person may only be placed in or "+
			"removed from a group by somebody who outranks them",
			targetUserID, slug, c.role))
}

// refusal builds a typed error: the code first, then the sentence.
//
// The code leads because that is what a caller matches on, and putting it
// after the prose would make every match a substring search over a message
// somebody will eventually improve.
func refusal(code, message string) error {
	return fmt.Errorf("%s: %s", code, message)
}

// RefusalCode reads the typed code back out of an error this package built,
// so a caller -- or a test -- matches on the contract rather than the prose.
func RefusalCode(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// The code may be wrapped by the store's own "groups: <verb>: %w", so
	// find the first token that looks like one of ours rather than assuming
	// it leads the string.
	for _, code := range []string{
		CodeNoCaller, CodeCapabilityMissing, CodeSelfAddRefused, CodeRankNotBelowCaller,
		CodeGroupAccountActive, CodeGroupNotActive, CodeGroupNotFound, CodeAccountNotFound,
		CodeAccountNotActive, CodeTargetUserNotFound, CodeAccountKindNotCreatable,
	} {
		if strings.Contains(msg, code) {
			return code
		}
	}
	return ""
}
