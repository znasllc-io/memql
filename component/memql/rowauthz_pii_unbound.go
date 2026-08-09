package memql

// Unbound-read PII admission (memql#3350).
//
// THE HOLE. `browseConceptPage` -- the generic keyset browse any client
// may issue over `MemqlService.Stream` -- sends a RAW query string
// (`sort(paginate(concept==<id>, N), "createdAt", "asc")`) and returns
// `rawNodes()`: the full nested wire row, payload and all, with no shape
// projecting anything away. Point it at `v1:identity:user` and every
// authenticated caller, whatever their role, receives every user row
// including all eight `@pii` fields -- displayName, firstName, lastName,
// primaryEmail, phone, primaryRole, gender, birthdate.
//
// The named surface is gated: `searchUsers` and `userById` both carry
// `requiresOwnerOrAdmin`, and `dsl/common/specs.memql` says of that spec
// in as many words that "identity needs it to gate the full-PII user
// read". The generic browse does not go through them, and that is the
// whole point of a generic browse -- it is concept-name-agnostic by
// construction, which is also why no named primitive can replace it.
//
// # WHY NOT SIMPLY DECLARE A TIER ON THE CONCEPT
//
// memql#3350 proposed that and it is the wrong instrument here, for a
// reason that only shows up once the reads are enumerated rather than
// assumed. A tier engages TWO mechanisms, and the author gets no say in
// which: filter injection for a read with a declared binding, and row
// admission for one without. `v1:identity:user` cannot survive the first:
//
//   - It carries legitimate CROSS-USER reads for ORDINARY callers.
//     `userDisplayById` is `@public` and `usersActiveInSpace` is ungated
//     -- they are how one participant's name renders in another's chat.
//     Any self-scoped predicate ANDed into those breaks the product for
//     everyone, not just for readers.
//   - It carries PRE-ACTOR reads. `userByEmail` is the magic-link lookup
//     and `userByIdSystem` resolves `sub` -> user in order to BUILD the
//     actor (component/auth/identity_resolver.go). `actor.userId` is
//     circular there. `enforceRowAuthzOnPlan` takes no context and has no
//     escape, so no tier can spare them -- the write path's
//     `rowAuthzWriteEscape` has no read-side counterpart.
//   - Its admin reads are an admin ROLL-UP, which `owned` cannot express
//     at all: the owned predicate is ANDed unconditionally, so
//     "every user in the cluster" would narrow to the admin's own row and
//     return a confidently wrong answer.
//
// So the boundary on `v1:identity:user` is not row-level. Users may see
// each other; what they may not see is each other's PII. The named
// queries already draw exactly that line, and they draw it with their
// SHAPES -- `userDisplayById` projects display fields, `searchUsers`
// projects the full row behind an owner/admin gate. The generic browse
// draws no line because it projects no shape.
//
// # WHAT THIS FILE DOES INSTEAD
//
// It closes the surface the shapes do not cover, on the ONE read path
// that has no shape and no binding, keyed off a declaration the tree
// ALREADY carries.
//
// The rule: on an UNBOUND read, a row whose concept declares `@pii`
// fields and declares NO `@rowAuthz` tier is admitted only to the row's
// own subject or to an owner/admin.
//
// Four properties, each load-bearing:
//
//  1. ENGINE-WIDE, not portal-specific. It sits at row admission, which
//     the VS Code concept browser (#3301), the portal People view and any
//     SDK caller all reach identically. memql#3350 rejected a fix in the
//     portal's People view precisely because it would close one window
//     and leave the door open -- and would break the deliberate property
//     that a predefined view and the generic browser read rows through
//     ONE path (`useViewRows` -> `useConceptRows`). Nothing here touches
//     that path; both still read through it and both are now gated.
//
//  2. DECLARATION-DRIVEN, so it cannot rot. It reads `Concept.PIIFields()`
//     -- the same `x-pii` list the hard-delete PII scrub (memql#1711)
//     already consults, whose own comment records the property being
//     reused here: "adding a new PII field to a concept needs only the
//     @pii annotation -- the scrub picks it up automatically and cannot
//     drift out of sync with a hand-maintained list." A concept that
//     grows a `@pii` field tomorrow is covered tomorrow, with no edit
//     here. That is what answers memql#3350's third acceptance box for
//     concepts that do not exist yet.
//
//  3. SCOPED TO UNBOUND READS, deliberately and narrowly. A bound read
//     went through a named construct, which has a filter and a shape and
//     an author who chose them; re-deciding that here would break
//     `userDisplayById` and would be a second authorization opinion
//     competing with the first. The binding is stamped onto the context
//     at the one plan seam every read passes through (engine.go), so
//     "unbound" means what `enforceRowAuthzOnPlan` means by it -- the two
//     cannot disagree about which reads carry a declared binding.
//
//  4. IT DEFERS TO A DECLARED TIER. When a concept DOES declare one, that
//     declaration is a deliberate authorization statement and wins
//     outright; this gate never fires. It exists for the UNMEASURED
//     population and nothing else, which is the distinction
//     rowauthz_undeclared_gate_test.go's header draws: an undeclared
//     concept is "not safe and not unchanged; it is unmeasured", and
//     unmeasured PII on an unshaped read is the one place fail-open costs
//     something concrete.
//
// # WHAT THIS IS NOT
//
// Not a substitute for a tier on `v1:identity:user`. It bounds the
// generic browse; it does not measure the concept, and the concept
// remains on the undeclared list. What it does is make the honest answer
// -- recorded on the concept itself and in
// docs/public/operate/auth/per-row-authz-audit.md -- safe to hold: that
// role-gating for `user` lives at the projection, not at the row.
//
// Not a field-level redactor either. It DENIES the row rather than
// blanking fields, because a half-row on an inspector surface is a
// worse answer than no row: the caller cannot tell a redacted field from
// an empty one. Denial is also what memql#3350's second acceptance box
// asks for in as many words -- a reader "cannot read another user's row"
// through the generic browse.

import (
	"context"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// rowAuthzBindingKey is the context key carrying the executing plan's
// declared concept binding.
//
// An unexported struct type, so no other package can set or read it: the
// value is an engine-internal statement about how THIS read was
// resolved, and a caller able to stamp it could turn the gate off by
// claiming a binding it does not have.
type rowAuthzBindingKey struct{}

// rowAuthzBinding is the stamped value. A struct rather than a bare
// string so "stamped, and the plan had no binding" is distinguishable
// from "never stamped" -- the empty string alone conflates them, and
// they must fail in opposite directions.
type rowAuthzBinding struct {
	// Concept is plan.BoundConcept: the concept the construct DECLARED a
	// binding to, empty for a raw client-supplied query string.
	Concept string
}

// contextWithRowAuthzBinding records how the executing plan resolved its
// concept, for the row gate downstream.
//
// Called from the single plan seam in engine.go, next to
// refuseRowAuthzWithoutActor, so the stamp lands on every read the engine
// executes rather than on the subset some caller remembered to mark.
func contextWithRowAuthzBinding(ctx context.Context, boundConcept string) context.Context {
	return context.WithValue(ctx, rowAuthzBindingKey{}, rowAuthzBinding{
		Concept: strings.TrimSpace(boundConcept),
	})
}

// rowAuthzReadIsUnbound reports whether the executing read resolved no
// declared concept binding -- a raw client-supplied query string.
//
// UNSTAMPED READS ARE NOT UNBOUND. A context with no stamp never passed
// the engine's plan seam, which means it is not a client query at all:
// graph expansion re-enters the row gate through
// admitRowAuthzTraversal with the ambient context, and in-process callers
// that build a node set directly never reach a plan. Treating those as
// unbound would apply a rule about the CLIENT BROWSE to reads that are
// not it, on the strength of a missing value.
//
// That is fail-open for a read the seam does not cover, and it is the
// right direction here only because the seam demonstrably covers the
// surface this gate exists for: every ExecuteQueryMsg lands in
// handleExecuteQuery -> Execute -> the plan path, which stamps. The
// narrower risk -- a future read path that bypasses the seam AND accepts
// a client query string -- is asserted against directly by
// TestGenericBrowseOverUserIsStamped rather than left to this comment.
func rowAuthzReadIsUnbound(ctx context.Context) bool {
	binding, stamped := ctx.Value(rowAuthzBindingKey{}).(rowAuthzBinding)
	if !stamped {
		return false
	}
	return binding.Concept == ""
}

// rowAuthzIsOwnerOrAdmin reports whether the caller holds the role pair
// that the named full-PII user read already admits.
//
// It mirrors `requiresOwnerOrAdmin` (dsl/common/specs.memql:
// `role == "admin" || role == "owner"`) rather than inventing a second
// answer, because the two gate the same data for the same reason: that
// spec's own doc comment says "identity needs it to gate the full-PII
// user read". If the generic browse admitted a DIFFERENT set than
// searchUsers, one of the two would be wrong and nothing would say which.
//
// Read through the actor envelope, the canonical resolver, so this cannot
// drift from the other surfaces that resolve `actor.role`. A missing
// AccessContext yields the empty role and therefore DENIES (memql#2801).
func rowAuthzIsOwnerOrAdmin(ctx context.Context) bool {
	ac, _ := auth.AccessFromContext(ctx)
	v, ok := auth.ActorEnvelopeValue(ac, "role")
	if !ok {
		return false
	}
	role, _ := v.(string)
	switch strings.TrimSpace(role) {
	case string(auth.RoleOwner), string(auth.RoleAdmin):
		return true
	default:
		return false
	}
}

// rowAuthzConceptCarriesPII reports whether a concept declares any `@pii`
// field.
//
// Resolved from the loaded concept registry via the same `memorynodes.Get`
// that rowAuthzDeclFor uses, and read off `PIIFields()` -- the accessor
// the hard-delete scrub already drives from the `x-pii` schema keyword.
// No second list, so a new `@pii` annotation reaches both at once.
func rowAuthzConceptCarriesPII(conceptName string) bool {
	name := strings.TrimSpace(conceptName)
	if name == "" {
		return false
	}
	c, err := memorynodes.Get(name)
	if err != nil || c == nil {
		return false
	}
	return len(c.PIIFields()) > 0
}

// rowAuthzPIIUnboundDenies decides whether to withhold one row from an
// unbound read because the row carries unmeasured PII.
//
// Every clause is a narrowing, and the order is the cheap tests first.
// Returning false means "this gate has no opinion", NOT "admit" -- the
// caller still applies the declared tier when there is one.
func rowAuthzPIIUnboundDenies(ctx context.Context, conceptName, id string) bool {
	// 1. Bound reads are the named surface's business. See property 3.
	if !rowAuthzReadIsUnbound(ctx) {
		return false
	}

	// 2. Trusted server-side Go, stamped for this call. Mirrors clause 1
	//    of rowAuthzWriteEscape, for the same reason and with the same
	//    discipline behind it: auth.CallOrigin's zero value is
	//    OriginClient, so an unstamped context is untrusted, and WHICH
	//    packages may stamp at all is enforced separately by
	//    TestOnlyAllowlistedPackagesStampInternalOrigin. component/grpc
	//    is refused there unconditionally, so a client request cannot
	//    arrive carrying this.
	if auth.OriginFromContext(ctx).IsInternal() {
		return false
	}

	// 3. No PII declared, nothing to withhold.
	if !rowAuthzConceptCarriesPII(conceptName) {
		return false
	}

	// 4. The audience the named full-PII read already admits.
	if rowAuthzIsOwnerOrAdmin(ctx) {
		return false
	}

	// 5. The subject themselves. Compared with sameRowAuthzOwner, the
	//    repo's single answer to "is this row this caller's", so the
	//    canonical-vs-bare id normalization that bit memql#3172 cannot
	//    bite here differently than it does on the read and write gates.
	//
	//    `id` is the subject key because the only PII-bearing concept in
	//    the tree today is v1:identity:user, whose subject IS the row.
	//    For a future PII concept keyed some other way this clause simply
	//    does not match, and the row is withheld from the generic browse
	//    -- fail-CLOSED for a concept nobody has measured, which is the
	//    correct default and the one that makes declaring a tier the way
	//    to get finer access rather than an optional tidy-up.
	if caller := strings.TrimSpace(rowAuthzActorUserId(ctx)); caller != "" {
		if sameRowAuthzOwner(id, caller) {
			return false
		}
	}

	return true
}
