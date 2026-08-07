package memql

// Row-authz OWNER STAMPING on the raw `insert(` path (memql#3175,
// carrying memql#3059). The third file of the write-side set, and the
// one that covers CREATES.
//
// THE PROBLEM. A query whose text begins `insert(` short-circuits the
// planner (isInsertFunction, component/memql/parser.go) and goes
// straight to executeMutation. It never touches a MutationTemplate, so
// the `args { }` schema, `accept { }` and `stamp { }` are all bypassed
// and the payload is written AS SUPPLIED. For any concept declaring
// `@rowAuthz(owner="F")`, `insert("<concept>", payload={"F": "<someone
// else>"})` therefore writes an arbitrary owner -- the same forgeability
// memql#2982 / #2988 / #2989 spent three PRs removing from the
// named-mutation surface, still open one door over.
//
// WHY A STAMP RATHER THAN A ROLE GATE. The raw surface is DOCUMENTED
// public language: docs/public/language/memql.md (audience: public,
// status: stable) says "There are two write surfaces: the runtime
// `insert()` literal and DSL-defined named mutations", with six purely
// mechanical rules and no named audience. Role-gating it would be a
// breaking change to a published contract rather than a bug fix. The
// repo also HAS the gating machinery (`allowInline`, MCP Tier-3 #1535)
// and deliberately did not apply it here -- the `insert(` short-circuit
// fires before `origin` is consulted at all. So the surface stays
// reachable and the ENGINE supplies the field the bypassed template
// would have stamped.
//
// WHY NOT ANOTHER TABLE ENTRY. executeWrite already carries nine
// per-concept Go guards, each citing a different incident (#403, #2070,
// #2072, #2140, #2143, #2513 ...). That table is reactive: it covers the
// concepts that had an incident, and states no rule for which concepts a
// general fix should reach. `@rowAuthz(owner="F")` already IS that rule,
// machine-readable, naming exactly the field to stamp -- so this reads
// the declaration and covers every declared concept, including the ones
// declared tomorrow. The nine guards stay as defence in depth: they
// enforce state machines, role ranks and credential kinds, none of which
// is "who owns this row".
//
// WHAT IT REACHES, precisely.
//
//   - Concepts: those declaring the `owned` tier with a payload FIELD.
//     `public` has no owner, `clusterOwner` and `granted` have no owner
//     field, and the self-owned form (`owner="id"`) has no field to
//     stamp -- the row's identity IS the owner and rewriting an id
//     changes which row is written. Zero concepts declare self-owned
//     today; TestSelfOwnedConceptsHaveNoFieldToStamp fails the day one
//     does.
//   - Writes: those that did NOT come from a rendered mutation template
//     (MutationNode.FromTemplate). A named mutation states its own
//     answer in `stamp { }`, and overruling it here would silently
//     rewrite the legitimate cases where a handler runs the write under
//     another user's actor. The raw surface is the one with no body to
//     state an answer.
//
// ANTI-DRIFT. "Who may write an owner they did not supply" is
// rowAuthzWriteEscape, imported from #3174 rather than restated, and the
// caller identity comes from rowAuthzActorUserId, the same envelope
// reader #3172 uses. Two spellings of one rule is how a reader and a
// writer drift into disagreeing about the very thing they both exist to
// describe.
//
// ID SPELLING, and the ORDERING it rests on (memql#3172). This stamps
// the caller id exactly as the actor envelope carries it -- normally the
// BARE `u1` -- and does not canonicalize it. It does not have to:
// executeWrite runs canonicalizeRelationshipFields immediately AFTER
// this call (executor_mutation.go), and an owner field is an outgoing
// `@relationship`, so the stamped value is rewritten to
// `v1:identity:user:u1` before the payload reaches the validators or the
// store. That is the spelling every other row of the concept carries and
// the one the read path compares against.
//
// Two consequences, stated rather than left to be rediscovered:
//
//   - The ordering is LOAD-BEARING. Move the stamp below the
//     canonicalizer and the row lands a bare owner in a column every
//     other row spells canonically.
//     TestTheOwnerStampRunsBeforeTheRelationshipCanonicaliser pins it.
//   - There is no double-canonicalization: canonicalizeIdValue returns
//     an already-`<target>:`-prefixed value unchanged, so a caller
//     identified canonically and one identified barely converge on the
//     same stored value (TestStampedOwnerConvergesFromEitherSpelling).

import (
	"context"
	"fmt"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// stampRowAuthzOwner server-stamps the declared owner field on a write
// that bypassed the mutation template.
//
// It MUTATES payload in place, overwriting any caller-supplied value for
// the declared field -- overwriting, not merging: a merge would let the
// caller's value survive in some shape, and the whole point is that the
// caller has no say in who owns the row they are creating.
//
// fromTemplate carries MutationNode.FromTemplate. It is false in the
// zero value on purpose, so a MutationNode built by a future producer is
// treated as raw and stamped rather than silently exempted.
//
// Returns an error only when the write cannot be authorized at all --
// today, a caller with no resolved identity. Everything else either
// stamps or is out of scope.
func stampRowAuthzOwner(ctx context.Context, conceptName string, payload map[string]any, fromTemplate bool) error {
	if fromTemplate || payload == nil {
		return nil
	}
	decl := rowAuthzDeclFor(conceptName)
	if decl == nil || decl.Tier != langparser.RowAuthzOwned {
		// Undeclared, or a tier with no owner field to stamp. Widening the
		// declared population is task #3173.
		return nil
	}
	field := strings.TrimSpace(decl.Owner)
	if field == "" || field == langparser.RowAuthzSelfOwnedField {
		// A tier that names no field, or the self-owned form whose "field"
		// is the row's own id. See the file header.
		return nil
	}

	// The SAME escape set the write guard declares (memql#3174): trusted
	// server-side Go stamping internal origin for this one write, and the
	// cluster owner. Both write the owner they supply, because both are
	// already trusted to write a row that is not theirs -- and refusing
	// them here would break every server-side provisioning path that
	// creates a row FOR a user. `admin` is deliberately not among them.
	if _, escaped := rowAuthzWriteEscape(ctx); escaped {
		return nil
	}

	caller := strings.TrimSpace(rowAuthzActorUserId(ctx))
	if caller == "" {
		// memql#3172 finding 4, on the create path. Stamping the empty
		// string is worse than doing nothing: `F == ""` is a legal
		// predicate that MATCHES, so the row would be minted already
		// wearing the value an unauthenticated read compares against. The
		// read path refuses, #3174's write guard refuses, and a create
		// that manufactures the row has to refuse too or the three
		// disagree.
		return fmt.Errorf(
			"row-authz: %s declares %s and this create carries no caller identity, so there "+
				"is no actor to stamp %q from. The raw insert() surface bypasses the mutation "+
				"template, so nothing else was going to set it, and stamping the empty string "+
				"would mint a row whose owner is a value the owned-tier predicate matches. "+
				"Refused (memql#3175). Authenticate the call, or write through a context "+
				"carrying an AccessContext",
			conceptName, rowAuthzTierDescription(decl), field)
	}

	payload[field] = caller
	return nil
}
