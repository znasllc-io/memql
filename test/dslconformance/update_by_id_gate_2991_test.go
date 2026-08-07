package dslconformance

import (
	"testing"
)

// update_by_id_gate_2991_test.go -- memql#2991.
//
// `updateUser` was `update { id: args.userId; args.payload }`: a caller-supplied
// target and a caller-supplied payload, on the row holding that caller's own
// cluster-wide `role`. Nothing gated it.
//
// Every layer that looks like it should have caught this did not, and the
// reasons are worth keeping because each one is load-bearing somewhere else:
//
//   - No predicate related `args.userId` to `actor.userId`, and the mutation
//     grammar has no way to express one -- ZERO mutations in the tree carry a
//     filter. That WAS memql#2803 Phase 5, and Phase 5 has since landed: see
//     below.
//   - `role` is a plain `enum(...) @default("reader")`; it carries no
//     `@internal` and no `@serverSet`.
//   - `validateMutationCallerArgs` walks the payload's NAMED keys, and a SPLAT
//     names none -- so the sensitive-field gate is structurally blind here and
//     would be even if `role` were annotated.
//   - Row-authz enforcement was inert by construction (memql#2920).
//
// TWO OF THOSE FOUR STOPPED BEING TRUE (memql#3172 / memql#3174).
//
// Row-authz is no longer inert. `TestRowAuthzIsInert` was RETIRED by memql#3172
// in the same commit that landed read-path enforcement, replaced by
// `TestRowAuthzEnforcementLandGate`; the tree is never left with neither gate.
// And memql#2803 Phase 5 -- the mechanism this file said "this issue could not
// fix" -- landed as memql#3174: `component/memql/rowauthz_write_guard.go`
// resolves the target row inside `executeWrite` and refuses an update / delete
// whose declared owner is not the actor, WITHOUT the mutation having to express
// the predicate. That is the general mechanism; it is the reason
// `updateCalendarEvent` / `deleteCalendarEvent` (memql#2991 parts 2 and 3) are
// closed.
//
// IT DOES NOT COVER THIS MUTATION, and the annotation below stays. `#3174`
// reaches the 13 concepts that DECLARE a tier. `v1:identity:user` is not one of
// them -- it cannot be, until memql#3029 lands `owner="id"` for the self-owned
// case, because a user row's owner is its own id and there is no separate owner
// field to name. Declaring tiers on the remaining 88 concepts is task #3173, and
// each one is its own authorization judgment.
//
// So `@serverOnly` remains the gate here, and it is the right one rather than a
// stop-gap: this mutation has exactly one production caller, the identity admin
// server, which already authorizes through `admin/auth.go`. The boundary sits
// where the authorization already lived.
//
// THE GATE HAS THREE HALVES, in three packages, and each covers a failure the
// others cannot see:
//
//  1. this file -- the annotation is present in the PARSED tree;
//  2. component/memql/server_only_mutation_resolve_test.go -- the LOADER agrees
//     and the mutation dispatch path actually refuses a client-origin call;
//  3. component/identity/admin/server_only_origin_test.go -- the one production
//     caller stamps an internal origin, so the gate does not break the admin UI.
//
// (2) and (3) are not redundant with (1). An annotation the parser reads but
// the loader drops is not a gate (memql#2875), and a gate nobody can call is an
// outage.

// TestUpdateUserIsServerOnly is the regression guard.
//
// Asserted against the PARSED tree rather than by grepping for the annotation,
// because a `@serverOnly` that the parser does not read as an annotation is not
// a gate -- memql#2875's lesson, and the reason
// TestServerOnlyConstructsAreDocumented switched to the same source.
//
// Matched on the FULL key (path AND name), not the name alone. A name-only
// match is satisfied by any construct called `updateUser` anywhere in the tree
// -- including a different declaration KIND in a different domain -- so it can
// report green while the mutation this issue is about carries no annotation at
// all. That is the same "collapsing the key defeats its purpose" rule
// server_only_parsed_test.go states for its own expected set.
//
// This test and `TestServerOnlyParsedSetMatchesTheTree` are complementary
// rather than duplicates: that one is an author-editable expectation list, so
// deleting the annotation AND its entry leaves it green. This one is a ratchet
// with no escape hatch, and under that edit it is the only failure in the
// package.
func TestUpdateUserIsServerOnly(t *testing.T) {
	parsed := serverOnlyConstructs(t)

	want := serverOnlyKey{Path: "identity/mutations.memql", Name: "updateUser"}
	if !parsed[want] {
		t.Errorf("`updateUser` is not @serverOnly in the parsed tree (looked for %+v).\n"+
			"It takes a caller-supplied user id AND a caller-supplied payload splat, and "+
			"v1:identity:user.role is a plain settable enum -- so without this gate a client "+
			"can name any user and any role. Nothing else covers it: the mutation grammar "+
			"cannot express an owner predicate, and the payload splat is invisible to "+
			"validateMutationCallerArgs. The owner-predicate mechanism DOES exist now "+
			"(memql#2803 Phase 5, landed as memql#3174's write guard), but it engages only "+
			"on a concept that DECLARES a tier -- and v1:identity:user cannot declare one "+
			"until memql#3029 lands the self-owned `owner=\"id\"` spelling. So this "+
			"annotation may only come off once that declaration is in the tree "+
			"(memql#2991).", want)
	}
}
