package agents

// owner_scope.go -- whose behalf an agents builtin acts on.
//
// Five capabilities here take an `ownerUserId` and act FOR that user: `invoke`
// and `produceArtifact` open a goal and its run owned by them,
// `requestUserFeedback` parks their run on a question, `askSpecialist`
// resolves their specialists, and `ensureForGoal` reads and writes their agent
// catalog. The value arrives as an argument, so each handler has to decide
// whether this caller may name it.
//
// THE RULE: the owner is the actor on the context. A call naming a DIFFERENT
// user is accepted only on a context the row write guard would also let write
// a row that is not the actor's -- memql.RowAuthzWriteEscape, the one
// enumeration of that set: internal origin (trusted server-side Go: a shipped
// automation's body, and the agent tool loop inside a run it started) and a
// cluster owner. Anything else naming another user is refused before the
// handler reads or writes anything.
//
// Ids compare by the user they name, because the same user arrives spelled
// both ways: the actor envelope normally carries the bare id and a stamped
// owner field the canonical one. An id spelled as a node of some OTHER
// concept names no user and matches nobody.
//
// AN ABSENT ownerUserId RESOLVES TO THE ACTOR when the actor is a person (not
// the cluster acting as itself, not anonymous, not a connector). On any other
// context it stays absent and each handler keeps its own answer to that:
// `invoke` resolves the shared catalog, the rest refuse.
//
// The tool surface stamps the owner from the turn (@autoInjected plus
// agentContextStamps), so a model never supplies it there. This rule is what
// holds for every OTHER way in: a builtin statement, an action, a direct call.

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// ownerNotCallerCode prefixes the refusal so a caller can match on it.
const ownerNotCallerCode = "owner_not_caller"

// callOwner resolves the owner one call acts for, or refuses the call.
//
// op names the capability in the refusal. requested is the raw argument.
func callOwner(ctx context.Context, op, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	actor := callerPersonId(ctx)
	if requested == "" {
		return actor, nil
	}
	if key := userKey(requested); key != "" && key == userKey(actor) {
		return requested, nil
	}
	if _, escaped := memql.RowAuthzWriteEscape(ctx); escaped {
		return requested, nil
	}
	return "", fmt.Errorf("%s: %s: not authorized to act for ownerUserId %q -- this call acts for its caller, "+
		"and only trusted server-side code or a cluster owner may name another user", op, ownerNotCallerCode, requested)
}

// callerPersonId is the actor's user id when the actor is a person, else "".
//
// A synthetic actor is the cluster acting as itself (an automation's system
// actor), so its id names no user a goal could be opened for. Anonymous and
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
