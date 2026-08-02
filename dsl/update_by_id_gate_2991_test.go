package dsl

import (
	"os"
	"path/filepath"
	"strings"
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
// this mutation has exactly one legitimate caller, the identity admin server,
// which already authorizes through `admin/auth.go`. The boundary now sits where
// the authorization already lived.

// TestUpdateUserIsServerOnly is the regression guard.
//
// Asserted against the PARSED tree rather than by grepping for the annotation,
// because a `@serverOnly` that the parser does not read as an annotation is not
// a gate -- memql#2875's lesson, and the reason
// TestServerOnlyConstructsAreDocumented switched to the same source.
func TestUpdateUserIsServerOnly(t *testing.T) {
	parsed := serverOnlyConstructs(t)

	var found bool
	for k := range parsed {
		if k.Name == "updateUser" {
			found = true
			break
		}
	}
	if !found {
		t.Error("`updateUser` is not @serverOnly in the parsed tree.\n" +
			"It takes a caller-supplied user id AND a caller-supplied payload splat, and " +
			"v1:identity:user.role is a plain settable enum -- so without this gate a client " +
			"can name any user and any role. Nothing else covers it: the mutation grammar " +
			"cannot express an owner predicate, the payload splat is invisible to " +
			"validateMutationCallerArgs, and row-authz is inert by construction. If this " +
			"annotation is being removed, the owner-predicate mechanism (memql#2803 Phase 5) " +
			"has to be in place first (memql#2991).")
	}
}

// TestAgentAuthorizationDeclaresItsOwner pins the tier declaration.
//
// `agentAuthorization` carries `computerUseScope` and its own doc states the
// contract: "the user who granted the authorization. Only the granting user can
// revoke." Declaring `@rowAuthz(owner="userId")` is what makes that contract
// checkable -- it brings the concept into memql#2982's owner-field provenance
// gate, which verifies the field a tier names is genuinely server-stamped
// rather than caller-writable.
//
// This does NOT enforce anything at runtime. Phase 1 is inert by design; the
// value here is that the assertion is now declared and machine-checked instead
// of living in a doc comment.
func TestAgentAuthorizationDeclaresItsOwner(t *testing.T) {
	src := readTreeFile(t, "agents/concepts.memql")

	idx := strings.Index(src, "concept agentAuthorization {")
	if idx < 0 {
		t.Fatal("concept agentAuthorization not found -- it was renamed and this gate needs " +
			"renaming with it")
	}
	// The annotation block is the run of lines immediately above the header.
	before := src[:idx]
	lines := strings.Split(before, "\n")
	var annots []string
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "@") || strings.HasPrefix(l, "///") || l == "" {
			annots = append(annots, l)
			continue
		}
		break
	}
	blob := strings.Join(annots, " ")

	if !strings.Contains(blob, `@rowAuthz(owner="userId")`) {
		t.Errorf("agentAuthorization does not declare @rowAuthz(owner=\"userId\").\n"+
			"The concept carries computerUseScope and its own field doc says only the granting "+
			"user may revoke -- an assertion nothing checked. Declaring the tier is what brings "+
			"it into memql#2982's provenance gate (memql#2991).\n  annotations found: %s", blob)
	}
}

// TestAdminUpdateUserStampsInternalOrigin is the other half of the fix, and the
// one that fails silently if it regresses.
//
// `@serverOnly` is enforced as `fn.ServerOnly && !auth.OriginFromContext(ctx).IsInternal()`.
// The admin server's updateUser call site was the ONLY one in the identity
// package not stamping an internal origin -- its sibling in the same file, the
// PAT store and the identity store all did. Adding the annotation without the
// stamp would have broken every admin user edit, and the failure would surface
// as a runtime error in the admin UI rather than at build or test time.
//
// A source scan rather than a behavioural test: exercising it needs a live
// engine and database, and the thing worth protecting is one token at one call
// site.
func TestAdminUpdateUserStampsInternalOrigin(t *testing.T) {
	path := filepath.Join("..", "component", "identity", "admin", "handlers.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("cannot read %s: %v", path, err)
	}
	src := string(raw)

	idx := strings.Index(src, "mutation updateUser(userId:")
	if idx < 0 {
		t.Skip("the admin server no longer builds a `mutation updateUser` call -- if it moved, " +
			"this gate should move with it")
	}
	// The Execute call is within a few lines of the query construction.
	window := src[idx:min(idx+600, len(src))]
	if !strings.Contains(window, "auth.ContextWithInternalOrigin") {
		t.Error("the admin server calls the @serverOnly `updateUser` mutation without stamping " +
			"an internal origin.\nThat call will be REFUSED at runtime -- @serverOnly is " +
			"enforced as `fn.ServerOnly && !auth.OriginFromContext(ctx).IsInternal()` -- and it " +
			"fails in the admin UI rather than in any test. Wrap the context the way the " +
			"sibling call in this same file, the PAT store and the identity store all do " +
			"(memql#2991).")
	}
}
