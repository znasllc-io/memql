package dsl

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
//     filter. That is memql#2803 Phase 5, not a thing this issue could fix.
//   - `role` is a plain `enum(...) @default("reader")`; it carries no
//     `@internal` and no `@serverSet`.
//   - `validateMutationCallerArgs` walks the payload's NAMED keys, and a SPLAT
//     names none -- so the sensitive-field gate is structurally blind here and
//     would be even if `role` were annotated.
//   - Row-authz enforcement is inert by construction (`TestRowAuthzIsInert`,
//     memql#2920).
//
// The fix is `@serverOnly`, and it is the right gate rather than a stop-gap:
// this mutation has exactly one production caller, the identity admin server,
// which already authorizes through `admin/auth.go`. The boundary now sits where
// the authorization already lived.
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
			"cannot express an owner predicate, the payload splat is invisible to "+
			"validateMutationCallerArgs, and row-authz is inert by construction. If this "+
			"annotation is being removed, the owner-predicate mechanism (memql#2803 Phase 5) "+
			"has to be in place first (memql#2991).", want)
	}
}
