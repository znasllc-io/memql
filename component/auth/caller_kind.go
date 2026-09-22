package auth

import "context"

// caller_kind.go -- WHAT KIND OF CALLER made this call (memql#5581).
//
// A decision record whose `userId` is empty answers two very different
// questions with the same blank: "a person made this call and nobody wrote
// down which one" and "no person made this call at all -- it was a boot seed,
// a retention sweep or an automation running as the deployment". The first is
// a defect in attribution; the second is the correct and complete answer. A
// blank cannot tell them apart, so a reader auditing "who caused this spend"
// has to treat every unattributed row as a possible gap.
//
// THE KIND IS DERIVED, NEVER PASSED. Every source it reads is already on the
// context and none of it is caller-supplied: AccessContext's three synthetic
// flags are set only by the constructors in this package, and the absence of
// an AccessContext is itself an answer. Accepting a kind as an argument would
// make it a claim rather than a fact, which is the mistake call_origin.go
// spells out at length for the origin bit.
//
// IT IS NOT AN AUTHORIZATION INPUT and nothing may branch on it. It rides on
// the router's decision record and on nothing else; row admission, the rank
// rules and the origin gate all answer exactly as before.
const (
	// CallerKindUser is a person: a signed-in caller, or server-side Go
	// acting on one person's behalf through ContextWithUserActor. The row's
	// userId names them.
	CallerKindUser = "user"

	// CallerKindSystem is THE CLUSTER ACTING RATHER THAN A PERSON --
	// MaintenanceActor, the seed materializer, an automation's system actor.
	// It is the honest answer for a call with no user, and it is what makes
	// an empty userId legible rather than suspicious.
	CallerKindSystem = "system"

	// CallerKindConnector is a connector writing its mirror (ConnectorActor).
	// It holds no rung and is nobody's call.
	CallerKindConnector = "connector"

	// CallerKindAnonymous is the identity-less caller a public-reads bridge
	// admits. It reaches @rowAuthz(public) concepts and nothing else.
	CallerKindAnonymous = "anonymous"

	// CallerKindUnattributed is a context that carries no access context at
	// all: a Go call site that never stamped one. It is DISTINCT from
	// `system`, deliberately -- "the cluster did this" is a claim, and a
	// context nobody stamped is not evidence for it. This value says nobody
	// said, which is the only true thing available.
	CallerKindUnattributed = "unattributed"
)

// CallerKinds is the closed set, for validation and for an error message.
func CallerKinds() []string {
	return []string{
		CallerKindUser, CallerKindSystem, CallerKindConnector,
		CallerKindAnonymous, CallerKindUnattributed,
	}
}

// CallerKindFromContext names the kind of caller ctx carries.
//
// THE ORDER IS THE DEFINITION. A connector and an anonymous caller both carry
// a role that a later check would also match, so the two narrow flags are read
// before the general ones; Synthetic is read before UserId because borrowed
// authority (ContextWithUserActor) carries a real person's id and is NOT
// synthetic, while the three cluster actors are synthetic and carry no
// person's id at all.
//
// It never returns "". An unstamped or nil context is CallerKindUnattributed,
// which is the fail-honest direction: a missing answer reads as missing rather
// than as the cluster having acted.
func CallerKindFromContext(ctx context.Context) string {
	access, ok := AccessFromContext(ctx)
	if !ok || access == nil {
		return CallerKindUnattributed
	}
	switch {
	case access.IsAnonymous:
		return CallerKindAnonymous
	case access.ConnectorName != "":
		return CallerKindConnector
	case access.Synthetic:
		return CallerKindSystem
	case access.UserId != "":
		return CallerKindUser
	}
	// An AccessContext with no user, not synthetic, not a connector and not
	// anonymous is a shape no constructor in this package produces. Saying
	// "unattributed" rather than guessing keeps the one property this value
	// has: every non-blank answer is something the context actually says.
	return CallerKindUnattributed
}
