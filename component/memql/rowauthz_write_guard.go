package memql

// Row-authz ENFORCEMENT on the WRITE path (memql#3174, Phase 5 of the
// #2803 ruling). The counterpart to rowauthz_enforce.go, which is the
// read path (memql#3172).
//
// THE PROBLEM. 120 of the tree's 215 mutations take a caller-supplied
// target id with nothing relating it to actor.userId, and the mutation
// grammar cannot express such a relation: zero mutations carry a filter,
// and an `update` block takes `id:` plus field assignments and nothing
// else. Every layer that looks like it should catch this does not, and
// each reason is load-bearing elsewhere -- validateMutationCallerArgs
// walks the payload's NAMED keys and a splat names none; @serverOnly
// removes a construct from the generated SDK, so applying it to the 120
// would be 120 frontend breaks rather than a policy.
//
// THE RULING. For a write onto an EXISTING row of a concept declaring a
// tier, the ENGINE resolves the target row and refuses when the row's
// declared owner is not the actor -- rather than requiring each mutation
// to say so. The declaration already exists at the concept; asking every
// mutation to restate it is 120 separate authorization judgments and is
// where drift enters.
//
// WHAT IT REACHES, precisely.
//
//   - Concepts: the 13 that declare a tier today (12 `owned`, 1
//     `clusterOwner`). The other 88 are untouched; widening that
//     population is task #3173, not this file.
//   - Writes: those that land on an EXISTING row -- `update`, the
//     soft-delete/status-flip class (memQL has no hard DELETE; a delete
//     is an append onto the same id), and a raw `insert(` aimed at an id
//     that already has a row. All of them converge on executeWrite,
//     which is where this is called from.
//   - NOT creates. A create has no target row to resolve an owner from,
//     so its problem is STAMPING rather than guarding: a raw `insert(`
//     can still forge the owner field on a new row. That is memql#3059 /
//     task #3175.
//   - NOT writes that reach the store without passing executeWrite at
//     all -- the boot seeder's direct concept.Create is task #3176.
//
// ANTI-DRIFT. The owner comparison is NOT reimplemented here. It
// delegates to rowAuthzAdmits, the same function the read path's row
// gate uses, so "who owns this row" has exactly one answer across both
// directions. Two spellings of one rule is how a reader and a writer
// drift into disagreeing about the very thing they both exist to
// describe.
//
// That delegation is what made the canonical-vs-bare defect a one-place
// fix (memql#3172): the stored owner is CANONICAL because the field is
// an outgoing `@relationship`, the caller id is BARE, and the comparison
// underneath was `==`, so this guard refused owners their own writes.
// The normalization now lives in sameRowAuthzOwner and every surface
// picked it up at once. Had this file carried its own copy, the read
// path would have been fixed and this one would still be refusing.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// rowAuthzWriteEscape is THE enumeration of what bypasses this guard.
// Nothing else does, and nothing else should be added without the same
// kind of written decision the two entries below carry.
//
// It returns the reason so a caller can log or explain the bypass; the
// bool is the answer.
//
//  1. INTERNAL ORIGIN -- trusted server-side Go, stamped per-write.
//     auth.CallOrigin's zero value is OriginClient, so an unstamped
//     context is untrusted and a forgotten stamp fails loudly rather
//     than silently granting. Crucially the stamp is SCOPED TO ONE
//     WRITE: the trusted caller stamps a context IT constructs, inline
//     as the argument to the one Execute that needs it (the shape
//     workertoken.Store.ListForUser landed in memql#3072). The
//     alternative -- stamping internal origin onto the request's own
//     context -- was BUILT AND REFUTED in memql#2989, because it opens
//     every @serverOnly construct, and now this guard too, for the rest
//     of that request. Which packages may stamp at all is itself
//     enforced, by TestOnlyAllowlistedPackagesStampInternalOrigin at
//     the repo root; component/grpc and component/node are refused
//     unconditionally because every context there descends from an
//     inbound request.
//
//  2. CLUSTER OWNER -- auth's own IsClusterOwner(), which is
//     `Role == RoleOwner` and nothing else. The cluster owner is the
//     operator of the deployment and is already the declared audience
//     of the clusterOwner tier; refusing them on the owned tier would
//     make cluster administration impossible without a second
//     mechanism.
//
// `admin` is deliberately NOT here (memql#3174 constraint 2). An escape
// is a decision, and no decision was taken to widen this one beyond the
// owner role -- so it is not inferred from the read side, from
// hasElevatedWriteRole, or from the fact that admin sounds privileged.
// Neither is a `system:` actor prefix or a `system` role: call_origin.go
// says in as many words that a role is a database fact about a user row,
// not a statement about which channel the call arrived on, and
// conflating the two makes the gate spoofable by anyone who can obtain
// that role.
func rowAuthzWriteEscape(ctx context.Context) (string, bool) {
	if auth.OriginFromContext(ctx).IsInternal() {
		return "internal origin (trusted server-side Go, stamped for this write)", true
	}
	if rowAuthzIsClusterOwner(ctx) {
		return "cluster owner", true
	}
	return "", false
}

// guardRowAuthzWrite refuses a write onto an existing row the caller
// does not own, per the row's concept declaration.
//
// Parameters mirror what executeWrite has in hand at the moment it calls
// this, which is BEFORE the read-merge:
//
//	prior        the target row's STORED payload (nil when absent)
//	existed      whether a row was found at (concept, id)
//	requirePrior the update() contract -- a missing row is an error
//
// The `prior` payload matters more than it looks. mergePayloadFields
// overwrites it in place with the caller's delta, and every owned-tier
// mutation in the tree re-stamps `ownerUserId: actor.userId` (that is
// what makes them pass the read-side land gate). A guard reading the
// merged payload would therefore compare the attacker against the
// attacker and always agree. Called before the merge, it compares the
// attacker against the row's real owner.
func guardRowAuthzWrite(ctx context.Context, conceptName, id string, prior map[string]any, existed, requirePrior bool) error {
	decl := rowAuthzDeclFor(conceptName)
	if decl == nil {
		// Undeclared concept: nothing to enforce. 88 of 101 concepts sit
		// here today; task #3173 is what makes that population visible
		// and shrinkable.
		return nil
	}
	if decl.Tier == langparser.RowAuthzPublic {
		// Public by declaration. Zero concepts declare it today, but a
		// public tier is a statement that the rows are not owned, so
		// there is no owner to compare a writer against.
		return nil
	}

	if _, escaped := rowAuthzWriteEscape(ctx); escaped {
		return nil
	}

	if !existed {
		// CONSTRAINT 3 -- fail closed on a missing target row. There is
		// no owner to resolve, so the only honest answers are refuse and
		// report-not-found. Falling through is neither, and memql#2982's
		// analyzer had to be fixed once (`e486d0f5`) for exactly that
		// shape.
		//
		// On the update contract this is the not-found refusal, stated
		// here rather than left to executeWrite's later check so the
		// refusal is unambiguously the guard's and a test can name it.
		// On the insert contract this is a genuine CREATE: no target
		// row, so this guard has nothing to say and the owner-forging
		// hole on that path is memql#3059 / task #3175.
		if requirePrior {
			return fmt.Errorf(
				"row-authz: update over %s names id %q, which has no row. A write that "+
					"cannot resolve its target cannot be authorized against it, so this "+
					"refuses rather than falling through (memql#3174)",
				conceptName, id)
		}
		return nil
	}

	payload, err := json.Marshal(prior)
	if err != nil {
		// Unreachable for a payload that came out of the store as JSON,
		// but a marshal we cannot perform is a row we cannot read an
		// owner off. Refuse.
		return fmt.Errorf(
			"row-authz: cannot read the stored payload of %s %q to resolve its declared "+
				"owner (%w); refusing the write rather than proceeding unchecked (memql#3174)",
			conceptName, id, err)
	}

	// ONE rule for "who owns this row", shared with the read path's row
	// gate. See the anti-drift note at the top of this file.
	switch rowAuthzAdmits(ctx, conceptName, id, payload) {
	case rowAuthzAdmit:
		return nil

	case rowAuthzUndecided:
		// The `granted` tier. Its predicate is a relationship spec, and
		// deciding it needs the join a FILTER performs -- which a write
		// does not have. The read path can defer to the filter that
		// already ran; there is nothing here to defer to, so this fails
		// closed.
		//
		// Zero concepts declare `granted` today, so this is inert.
		// TestWriteGuardDayOneCoverage fails the moment one does, so the
		// refusal is adjudicated at merge rather than discovered in
		// production.
		return fmt.Errorf(
			"row-authz: %s declares the granted tier (%s), whose predicate is a "+
				"relationship spec. A write carries no filter, so the join that would "+
				"decide it never ran and this write cannot be authorized against the "+
				"target row. Refused (memql#3174)",
			conceptName, InjectedPredicate(decl))

	default:
		return rowAuthzWriteRefusal(ctx, decl, conceptName, id)
	}
}

// rowAuthzWriteRefusal renders the denial. Split out so every refusal
// path produces the same message shape, and so the message can name the
// declaration the caller failed rather than a generic "forbidden".
//
// It deliberately does not echo the row's actual owner: the caller has
// just been told they may not read this row, and the error is the one
// place that could hand them its contents anyway.
func rowAuthzWriteRefusal(ctx context.Context, decl *langparser.RowAuthzDecl, conceptName, id string) error {
	caller := strings.TrimSpace(rowAuthzActorUserId(ctx))
	if caller == "" {
		caller = "(no caller identity)"
	}
	return fmt.Errorf(
		"row-authz: %s declares %s and row %q is not this caller's (%s) to write. "+
			"The mutation grammar cannot express an owner predicate, so the engine "+
			"resolves the target row and refuses here (memql#3174). A cluster owner, or "+
			"trusted server-side Go stamping internal origin for this one write, may "+
			"proceed; nothing else may",
		conceptName, rowAuthzTierDescription(decl), id, caller)
}

// rowAuthzTierDescription renders a declaration for an error message:
// the predicate for the tiers that have one, the tier name otherwise.
func rowAuthzTierDescription(decl *langparser.RowAuthzDecl) string {
	if decl == nil {
		return "no row-authz tier"
	}
	if p := strings.TrimSpace(InjectedPredicate(decl)); p != "" {
		return fmt.Sprintf("the %s tier (%s)", decl.Tier, p)
	}
	return fmt.Sprintf("the %s tier", decl.Tier)
}
