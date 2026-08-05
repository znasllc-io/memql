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

// TestOwnerProvenance_ServerOnlyIsNotACallerPath pins the narrowing.
//
// A @serverOnly mutation bars client-originated calls at expandFunctionCall
// (#2800), so its args cannot come from a caller BY CONSTRUCTION. Counting it
// as caller-writable reports a guarantee that is provided as one that is not.
//
// memql#2991 rests on exactly this: it gated `updateUser` @serverOnly
// specifically so its caller-supplied id would stop being a forging path. If
// the analyzer ignores the annotation, that work buys nothing here.
func TestOwnerProvenance_ServerOnlyIsNotACallerPath(t *testing.T) {
	reg := loadTreeRegistry(t)
	got := provenanceOf(t, reg, "v1:identity:user", langparser.RowAuthzSelfOwnedField)

	for _, name := range got.WritableBy {
		if name == "updateUser" {
			t.Errorf("updateUser is counted as a caller path, but memql#2991 gated it "+
				"@serverOnly -- a client call is refused at expandFunctionCall, so its "+
				"caller-supplied id is unreachable from a caller. Counting it anyway reports a "+
				"guarantee that IS provided as one that is not.\n  writable by: %v", got.WritableBy)
		}
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
