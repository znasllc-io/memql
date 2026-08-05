package memql

import (
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// rowauthz_self_owned_3029_test.go -- memql#3029, the analyzer half.
//
// The self-owned form asks a different question of a mutation. For
// `owner="ownerUserId"` it is "who writes that payload field"; for
// `owner="id"` it is **"can a caller write the ROW'S ID"** -- because the row's
// identity IS the owner.
//
// This file also pins the clause's own history. rowauthz_owner_provenance.go
// used to carry a note explaining why an `id` clause was deliberately ABSENT:
// validateRowAuthz required a declared property, `id` is an intrinsic no
// concept can declare, so the form could not load and "a clause that cannot
// fire is not a safeguard, it is a claim in the shape of one". That was
// correct. memql#3029 is what invalidates its premise, so the clause is
// restored rather than re-invented -- and these tests are what stop it drifting
// back to unreachable.

// TestOwnerProvenance_SelfOwned_CallerSuppliedIdIsWritable is the live
// measurement, taken against the real tree rather than a synthetic template.
//
// v1:identity:user is the concept the self-owned form exists for. Nine
// CLIENT-CALLABLE mutations take its row id from caller args, so the analyzer
// must report it writable -- which is why the tier is not declared on it yet.
func TestOwnerProvenance_SelfOwned_CallerSuppliedIdIsWritable(t *testing.T) {
	reg := loadTreeRegistry(t)
	got := provenanceOf(t, reg, "v1:identity:user", langparser.RowAuthzSelfOwnedField)

	if len(got.WritableBy) == 0 {
		t.Fatalf("the analyzer found NO caller path to v1:identity:user's row id. Nine "+
			"client-callable mutations take it from caller args (createUser, deleteUserHard, "+
			"scheduleAccountDeletion, ...), so a clean verdict here means the self-owned clause "+
			"is not firing and memql#3029 has silently returned to the unreachable state the "+
			"deleted NOTE described.\n  verdict: %+v", got)
	}
	if got.Reason == "" {
		t.Error("a writable verdict must carry a reason; a bare list is not actionable")
	}
	if !strings.Contains(got.Reason, "row id") {
		t.Errorf("the reason should say the ROW ID is caller-chosen, not name a payload field -- "+
			"that distinction is the whole point of the self-owned form. got: %q", got.Reason)
	}
}

// TestOwnerProvenance_ServerOnlyIsStillACallerPath pins the direction a
// landing-review pass got backwards.
//
// That pass skipped @serverOnly mutations in the provenance walk, reasoning
// that the annotation means "args cannot come from a caller by construction".
// The annotation does not mean that. engine.go gates on
// auth.OriginFromContext -- the origin of the CALL -- and never inspects where
// a trusted Go call site got its args.
//
// updateUser is the live counter-example: component/identity/admin/handlers.go
// stamps internal origin on a REQUEST-DERIVED context and passes a userId read
// straight off an HTTP form. call_origin.go warns against exactly that shape,
// and call_origin_conformance_test.go allowlists that package as a KNOWN
// EXCEPTION whose precondition is asserted by a separate per-package test --
// so arg provenance comes from that discipline, not from @serverOnly.
//
// The gate must therefore keep counting it. Failing closed on a mutation whose
// caller-supplied id it can see is the whole product of memql#2982.
func TestOwnerProvenance_ServerOnlyIsStillACallerPath(t *testing.T) {
	reg := loadTreeRegistry(t)
	got := provenanceOf(t, reg, "v1:identity:user", langparser.RowAuthzSelfOwnedField)

	var sawUpdateUser bool
	for _, name := range got.WritableBy {
		if name == "updateUser" {
			sawUpdateUser = true
		}
	}
	if !sawUpdateUser {
		t.Errorf("updateUser takes the row id from caller args (`update { id: args.userId }`) and "+
			"must be counted as a caller path. @serverOnly bars a client CALL; it does not "+
			"constrain where the admin handler got the id -- and that handler reads it from an "+
			"HTTP form under an internal-origin stamp on a request-derived context. Skipping it "+
			"here would let a concept satisfy the owner gate by annotating its mutation, which is "+
			"not on the gate's remedy menu.\n  writable by: %v", got.WritableBy)
	}
}

// TestInjectedPredicate_SelfOwnedUsesRowNamespace is acceptance criterion 6.
//
// Shadow mode renders `decl.Owner + "==actor.userId"`. Concatenating blindly
// for the self-owned form emits a bare `id==actor.userId` -- and a filter
// spells payload properties bare while row intrinsics take the `row.`
// namespace, so that reads as a payload property named `id` and compiles to an
// entirely different SQL path. The bare spelling is retired outright
// (memql#2779, TestFilterIntrinsicsUseRowNamespace).
//
// The criterion allows shadow mode to DECLINE for this form. What it forbids
// is silently emitting a wrong predicate, which is exactly what the
// concatenation would do.
func TestInjectedPredicate_SelfOwnedUsesRowNamespace(t *testing.T) {
	got := InjectedPredicate(&langparser.RowAuthzDecl{
		Tier:  langparser.RowAuthzOwned,
		Owner: langparser.RowAuthzSelfOwnedField,
	})
	if got == "" {
		t.Skip("shadow mode declines to render the self-owned form, which the criterion permits")
	}
	if !strings.Contains(got, "row.id") {
		t.Errorf("the self-owned predicate must name the row intrinsic as `row.id`. A bare `id` "+
			"reads as a payload property and compiles to a different SQL path -- a silently wrong "+
			"predicate, which is the one outcome this renderer must not produce (memql#2779, "+
			"memql#3029).\n  got: %q", got)
	}
	if !strings.Contains(got, "actor.userId") {
		t.Errorf("the predicate must still compare against the actor. got: %q", got)
	}

	// The ordinary payload-field form is untouched.
	if plain := InjectedPredicate(&langparser.RowAuthzDecl{
		Tier: langparser.RowAuthzOwned, Owner: "ownerUserId",
	}); plain != "ownerUserId==actor.userId" {
		t.Errorf("the payload-field form must be unchanged, got %q", plain)
	}
}

// TestAnalyzeShadow_SelfOwnedPredicateRoundTrips is the assertion the
// criterion-6 test above could not make on its own.
//
// Asserting the rendered STRING proves the renderer; it says nothing about
// whether the matcher in the same file can recognise what was rendered. A
// landing-review pass found they disagreed: InjectedPredicate emitted
// `row.id==actor.userId` while isOwnerScopeLeaf resolved conjuncts through
// topLevelPayloadField, which returns "" for `["row","id"]` -- so a filter
// spelling the tier's own predicate VERBATIM was reported would-narrow.
//
// That is the overstatement rowauthz_shadow.go's own doctrine forbids as
// loudly as an understatement, and shadow mode's entire product is the
// evidence the Phase 3 enforcement ruling is taken against. A round trip is
// the only assertion that catches a renderer and a matcher drifting apart.
func TestAnalyzeShadow_SelfOwnedPredicateRoundTrips(t *testing.T) {
	decl := &langparser.RowAuthzDecl{
		Tier:  langparser.RowAuthzOwned,
		Owner: langparser.RowAuthzSelfOwnedField,
	}

	// The predicate the renderer emits, parsed back into a filter.
	verdict, reason := AnalyzeShadow(ownerScoped("row.id"), decl)
	if verdict != ShadowAlreadyImplied {
		t.Errorf("a filter that IS the rendered predicate must be already-implied.\n"+
			"  rendered: %q\n  verdict:  %v\n  reason:   %s\n"+
			"The renderer and the matcher are describing the same predicate; if they disagree, "+
			"the blast-radius number the enforcement ruling rests on is wrong in the direction "+
			"that overstates it.", InjectedPredicate(decl), verdict, reason)
	}

	// The RUNTIME shape too. rewriteFilterFieldRefs normalises `row.id` to the
	// bare intrinsic before the executor's shadow hook sees it, so the matcher
	// receives {Raw:"id", Parts:["id"]} there and {Raw:"row.id",
	// Parts:["row","id"]} on the static report path. Asserting one shape
	// leaves the other unguarded, and they take different branches.
	if v, reason := AnalyzeShadow(ownerScoped("id"), decl); v != ShadowAlreadyImplied {
		t.Errorf("the post-normalisation spelling must also be recognised -- the executor's "+
			"shadow hook only ever sees this shape.\n  verdict: %v\n  reason:  %s", v, reason)
	}

	// The near-miss must NOT satisfy it. createdBy means "who wrote the row",
	// not "whose row it is" -- validateRowAuthz refuses it as an owner, so a
	// matcher that credited it would be looser than the validator.
	if v, _ := AnalyzeShadow(ownerScoped("row.createdBy"), decl); v == ShadowAlreadyImplied {
		t.Error("`row.createdBy==actor.userId` must not satisfy the self-owned predicate: it names " +
			"who WROTE the row, not whose row it is, and admitting it here would credit a scope " +
			"the tier can never have declared")
	}

	// An unrelated payload field must not satisfy it either.
	if v, _ := AnalyzeShadow(ownerScoped("ownerUserId"), decl); v == ShadowAlreadyImplied {
		t.Error("an unrelated payload field must not satisfy the self-owned predicate")
	}
}
