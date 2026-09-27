package memql

// call_owner.go -- whose behalf an integration capability acts on.
//
// Some capabilities take an `ownerUserId` argument and act FOR that user:
// open a goal owned by them, park their run, resolve their catalog, run a
// command on their machine, write onto their step. The value arrives as an
// argument, so each such handler has to decide whether this caller may name
// it. CallOwner is that decision, stated once for every one of them.
//
// THE RULE: the owner is the actor on the context. A call naming a DIFFERENT
// user is accepted only on a context the row write guard would also let write
// a row that is not the actor's -- RowAuthzWriteEscape, the one enumeration of
// that set: internal origin (trusted server-side Go, which is how a shipped
// automation's body reaches a builtin) and a cluster owner. Anything else
// naming another user is refused, before the handler reads or writes anything.
//
// Ids compare by the user they name, because the same user arrives spelled
// both ways: the actor envelope normally carries the bare id and a stamped
// owner field the canonical one. An id spelled as a node of some OTHER
// concept names no user and matches nobody.
//
// AN ABSENT ownerUserId RESOLVES TO THE ACTOR when the actor is a person (not
// the cluster acting as itself, not anonymous, not a connector). On any other
// context it stays absent, and each handler keeps its own answer to an absent
// owner.
//
// An agent's tool loop never takes the value from the model: the tool declares
// it @autoInjected and the runtime stamps the turn's owner, which is the actor
// the run restored on the same context -- so there the rule holds without
// reaching for the escape. It is every OTHER way in that the rule exists for:
// a builtin statement, an action, a direct call.

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/id"
)

// OwnerNotCallerCode prefixes CallOwner's refusal so a caller can match on it.
const OwnerNotCallerCode = "owner_not_caller"

// CallOwner resolves the owner one call acts for, or refuses the call.
//
// op names the capability in the refusal. requested is the raw argument.
func CallOwner(ctx context.Context, op, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	actor := callerPersonId(ctx)
	if requested == "" {
		return actor, nil
	}
	if key := userKey(requested); key != "" && key == userKey(actor) {
		return requested, nil
	}
	if _, escaped := RowAuthzWriteEscape(ctx); escaped {
		return requested, nil
	}
	return "", fmt.Errorf("%s: %s: not authorized to act for ownerUserId %q -- this call acts for its caller, "+
		"and only trusted server-side code or a cluster owner may name another user", op, OwnerNotCallerCode, requested)
}

// callerPersonId is the actor's user id when the actor is a person, else "".
//
// A synthetic actor is the cluster acting as itself (an automation's system
// actor), so its id names no user anything could be done for. Anonymous and
// connector actors are nobody's behalf either.
func callerPersonId(ctx context.Context) string {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || ac.Synthetic || ac.IsAnonymousActor() || ac.IsConnector() {
		return ""
	}
	return strings.TrimSpace(ac.UserId)
}

// userKey is the user an id names, in its bare form: the id itself when it is
// bare, its short id when it is a v1:identity:user node id, and "" otherwise.
func userKey(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	concept, short, err := id.ParseNodeId(v)
	switch {
	case err != nil || short == "":
		return ""
	case concept == "":
		return short
	case concept == memorynodes.ConceptIdentityUser:
		return short
	}
	return ""
}
