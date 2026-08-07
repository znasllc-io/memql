package memql

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// agentauthz_rowauthz_3177_test.go -- memql#3177, carrying memql#3129 and
// memql#3138.
//
// # WHAT WAS OPEN
//
// `v1:agents:agentAuthorization` is the standing-grant row: it carries
// `computerUseScope` (the ceiling on what an agent may do on the user's own
// machine) and `skillTierAllowlist` (which skill tiers the planner may mint at
// unattended). Its own field doc has always said "Only the granting user can
// revoke". Nothing enforced that. Two mutations took a caller-supplied `authId`
// with nothing relating it to the caller:
//
//	revokeAgentAuthorization  update { id: args.authId; active: false }
//	updateAgentAuthorization  update { id: args.authId; args.payload }
//
// so any authenticated caller who knew an authId could revoke a stranger's
// grant or write arbitrary fields into it. Worse, `args.payload` splatted
// VERBATIM -- memql#401's overlay-wins protection is populated only from
// EXPLICIT block fields -- so `userId`, the field the ownership claim is keyed
// on, was caller-writable. The escalation was two calls:
//
//  1. create a grant for YOURSELF with computerUseScope:"full" (correctly
//     stamped to you by memql#3081);
//  2. update it with {userId:"<victim>"}; the read-merge keeps the scope and the
//     active flag, and the victim now has a standing `full` ceiling they never
//     approved.
//
// # WHAT CLOSES IT, and why all three parts had to land together
//
//	the DECLARATION  @rowAuthz(owner="userId") on the concept, which is what
//	                 arms memql#3174's write guard on every update/revoke;
//	the STAMP        updateAgentAuthorization re-stamps userId from actor.userId
//	                 over its splat, so the attacker cannot make step 2's row
//	                 somebody else's -- the guard alone does not cover this,
//	                 because in step 2 the attacker is writing a row they
//	                 legitimately own;
//	the READ         agentAuthorizationsForSelf is caller-scoped, so the grants
//	                 cannot be enumerated for a stranger either -- and under a
//	                 declared tier the old `userId==args.userId` shape is not
//	                 even expressible (memql#3172's land gate refuses it).
//
// # WHAT THIS FILE ASSERTS, AND HOW
//
// Through the ENGINE's own machinery, never by asserting that a declaration
// exists. Every concept name, owner field and bound concept below is resolved
// from the LOADED registries, so a construct re-bound to a different concept or
// a declaration that fails to parse fails these tests rather than sliding past
// them (the memql#2875 lesson).
//
//   - the write side calls `guardRowAuthzWrite`, the exact function
//     executeWrite calls at executor_mutation.go, with the prior row's stored
//     payload -- which is what executeWrite has in hand at that moment, BEFORE
//     the read-merge overwrites it with the caller's delta;
//   - the read side boots a real engine over the real embedded DSL and runs the
//     engine's own post-filter pipeline over the query string the Go stores
//     send, in the idiom memql#3178 landed next door. A row that does not match
//     there is a row the executor drops.
//
// The end-to-end claim -- that the write does not LAND -- needs a store and
// lives in agentauthz_rowauthz_3177_db_test.go, postgres-gated like its
// neighbours.

const agentAuthzConcept = "v1:agents:agentAuthorization"

// agentAuthzDecl returns the concept's LOADED row-authz declaration and fails
// when it declares nothing. Everything else in this file is downstream of the
// declaration existing, so it is asserted once, here, rather than assumed.
func agentAuthzDecl(t *testing.T) *langparser.RowAuthzDecl {
	t.Helper()
	decl := declFor(t, agentAuthzConcept)
	if decl == nil {
		t.Fatalf("%s declares no @rowAuthz tier. Without it memql#3174's write guard is inert "+
			"on this concept and any caller who knows an authId can revoke or rewrite a "+
			"stranger's standing grant (memql#3129).", agentAuthzConcept)
	}
	return decl
}

// TestAgentAuthorizationDeclaresTheOwnedTierOnUserId pins the declaration
// itself -- the tier AND the field it names.
//
// The field matters as much as the tier: `@rowAuthz(owner="agentId")` would
// load, would arm the guard, and would compare the caller against an agent id
// that is never a user id, denying everybody including the owner. A tier
// pointing at the wrong column is a broken authorization statement that reads
// as a working one.
func TestAgentAuthorizationDeclaresTheOwnedTierOnUserId(t *testing.T) {
	decl := agentAuthzDecl(t)

	if decl.Tier != langparser.RowAuthzOwned {
		t.Fatalf("%s declares the %q tier, want %q. These rows belong to the user who granted "+
			"them; any other tier is a different authorization statement (memql#3177).",
			agentAuthzConcept, decl.Tier, langparser.RowAuthzOwned)
	}
	if decl.Owner != "userId" {
		t.Fatalf("%s declares owner=%q, want \"userId\" -- the field carrying the granting "+
			"user. The engine compares the caller against THIS field, so naming any other "+
			"one denies the real owner and enforces nothing (memql#3177).",
			agentAuthzConcept, decl.Owner)
	}
}

// TestAgentAuthorizationOwnerFieldIsServerStamped is the memql#3138 half,
// measured by the repo's own analyzer rather than restated.
//
// `OwnerFieldProvenance` reads the LOADED MutationTemplates, which is the only
// place the answer lives: `update { id; args.payload }` and
// `update { id; userId: actor.userId; args.payload }` differ by whether the
// loader's hoist-and-delete pass populates PayloadOverlayTemplate, and only the
// second makes the server's value win. The sibling gate
// (TestDeclaredOwnerFieldsAreServerStamped) sweeps every declared concept; this
// one names THIS concept and the mutations either side of the verdict, so a
// regression here says which mutation opened the hole.
func TestAgentAuthorizationOwnerFieldIsServerStamped(t *testing.T) {
	decl := agentAuthzDecl(t)
	registry := loadedTreeRegistry(t)

	results := OwnerFieldProvenance(registry, map[string]string{agentAuthzConcept: decl.Owner})
	if len(results) != 1 {
		t.Fatalf("got %d provenance verdicts for one concept", len(results))
	}
	got := results[0]

	if !got.ServerStamped {
		t.Fatalf(`%s.%s is NOT server-stamped.

  %s
  stamped by:  %v
  writable by: %v

A caller who can write this field can reassign a grant they legitimately own to
somebody else -- which is the memql#3138 escalation, and the reason the write
guard alone does not close it: in that write the attacker IS the row's owner
(memql#3177).`, got.Concept, got.Field, got.Reason, got.StampedBy, got.WritableBy)
	}

	// The verdict is only meaningful if the analyzer actually SAW the two
	// mutations that write the field. A concept whose mutations all failed to
	// load would report ServerStamped=false, but one whose mutations were
	// simply not found would report a vacuous pass on an empty set.
	stamped := append([]string(nil), got.StampedBy...)
	sort.Strings(stamped)
	for _, want := range []string{"createAgentAuthorization", "updateAgentAuthorization"} {
		if !containsString(stamped, want) {
			t.Errorf("%s is not among the mutations stamping %s.%s (%v).\n"+
				"Both write the owner field and both must stamp it from actor.userId: the "+
				"create decides the row's owner, and the update must not let a payload "+
				"reassign it (memql#3177).", want, got.Concept, got.Field, stamped)
		}
	}
}

// mutationBoundConcept resolves the concept a mutation writes FROM THE LOADED
// REGISTRY, and fails when the mutation is missing or bound elsewhere.
//
// The tests below are about "a write onto an agentAuthorization row", so the
// binding is a precondition rather than a detail: a mutation silently re-bound
// to another concept would keep passing a hard-coded-concept version of these
// tests while enforcing nothing on the rows they are about.
func mutationBoundConcept(t *testing.T, registry *FunctionRegistry, name string) string {
	t.Helper()
	for _, fn := range registry.List() {
		if fn == nil || fn.Name != name {
			continue
		}
		if fn.FunctionKind != "mutation" {
			t.Fatalf("%s is a %q, not a mutation", name, fn.FunctionKind)
		}
		bound := strings.TrimSpace(fn.BoundConcept)
		if bound == "" && fn.MutationTemplate != nil {
			bound = strings.TrimSpace(fn.MutationTemplate.Concept)
		}
		if bound == "" {
			t.Fatalf("%s resolves to no bound concept, so the engine cannot look up a tier "+
				"for its writes", name)
		}
		return bound
	}
	t.Fatalf("mutation %q is not in the loaded registry -- this gate would pass by never "+
		"running", name)
	return ""
}

// agentAuthzRow is a STORED grant payload owned by owner, carrying the two
// fields that make the row worth stealing.
func agentAuthzRow(t *testing.T, owner string) map[string]any {
	t.Helper()
	decl := agentAuthzDecl(t)
	return map[string]any{
		decl.Owner:           owner,
		"agentId":            "v1:agents:agent:some-agent",
		"planKind":           "*",
		"spaceScope":         "*",
		"computerUseScope":   "full",
		"skillTierAllowlist": []any{"A", "B", "C"},
		"active":             true,
	}
}

// TestNonOwnerIsRefusedOnEveryTargetedWriteToAGrant is the memql#3129 half.
//
// It drives the ENGINE's guard -- the same call executeWrite makes, with the
// same arguments -- for every mutation in the tree that writes an existing
// agentAuthorization row by caller-supplied id. Enumerated by name on purpose:
// the day-one coverage table lists them, and a NEW mutation on this concept
// should be added here deliberately rather than inherited silently.
func TestNonOwnerIsRefusedOnEveryTargetedWriteToAGrant(t *testing.T) {
	registry := loadedTreeRegistry(t)

	const (
		owner    = "v1:identity:user:grant-owner"
		attacker = "v1:identity:user:attacker"
	)
	rowId := agentAuthzConcept + ":the-grant"
	prior := agentAuthzRow(t, owner)

	for _, name := range []string{
		"revokeAgentAuthorization",
		"updateAgentAuthorization",
		// dsl/worker/mutations.memql -- the second half of the escalation:
		// sets computerUseScope by row id with no owner predicate.
		"updateAgentAuthScope",
	} {
		t.Run(name, func(t *testing.T) {
			concept := mutationBoundConcept(t, registry, name)
			if concept != agentAuthzConcept {
				t.Fatalf("%s writes %s, not %s -- this test is measuring the wrong construct",
					name, concept, agentAuthzConcept)
			}

			// The attacker names the owner's row. requirePrior=true is the
			// update contract every one of these mutations runs under.
			err := guardRowAuthzWrite(rowAuthzCallerCtx(attacker), concept, rowId, prior, true, true)
			if err == nil {
				t.Fatalf("%s let %s write %s's grant. That row carries computerUseScope=%q and "+
					"skillTierAllowlist=%v, so an unguarded write is a revocation or an "+
					"elevation on somebody else's standing authorization (memql#3129).",
					name, attacker, owner, prior["computerUseScope"], prior["skillTierAllowlist"])
			}
			if !strings.Contains(err.Error(), "row-authz") {
				t.Fatalf("%s was refused for the wrong reason: %v", name, err)
			}

			// The other half: the legitimate owner still gets through. Without
			// this the guard could be a blanket refusal and the assertion above
			// would still be green -- and the feature (a user revoking their
			// own grant from the agent settings page) would be dead.
			if err := guardRowAuthzWrite(rowAuthzCallerCtx(owner), concept, rowId, prior, true, true); err != nil {
				t.Fatalf("%s refused the grant's OWN owner: %v", name, err)
			}
		})
	}
}

// TestAGrantCannotBeReassignedByRewritingItsOwner is memql#3138 stated as the
// attack rather than as a provenance verdict.
//
// The guard cannot catch this one and is not meant to: at step 2 the attacker
// is writing a row they legitimately own, so the owner comparison PASSES. What
// stops it is the stamp -- and the point of asserting it here, next to the
// guard tests, is that the two defences cover disjoint halves and neither is
// redundant.
func TestAGrantCannotBeReassignedByRewritingItsOwner(t *testing.T) {
	decl := agentAuthzDecl(t)
	registry := loadedTreeRegistry(t)

	const attacker = "v1:identity:user:attacker"
	rowId := agentAuthzConcept + ":attackers-own-grant"
	prior := agentAuthzRow(t, attacker)

	// Step 2 of the escalation: the attacker updates their OWN row. The guard
	// admits it, correctly -- they own it.
	if err := guardRowAuthzWrite(rowAuthzCallerCtx(attacker), agentAuthzConcept, rowId, prior, true, true); err != nil {
		t.Fatalf("the guard refused a caller writing their own row: %v.\n"+
			"That is not the defence against this attack, and if it starts refusing here "+
			"the whole self-service surface is broken", err)
	}

	// So the defence has to be the template: whatever the caller puts in
	// `payload.userId`, the overlay re-stamps it. Read from the LOADED
	// template, because that is where memql#401's overlay either exists or
	// does not.
	var found bool
	for _, fn := range registry.List() {
		if fn == nil || fn.Name != "updateAgentAuthorization" || fn.MutationTemplate == nil {
			continue
		}
		found = true
		overlay, ok := fn.MutationTemplate.PayloadOverlayTemplate[decl.Owner]
		if !ok {
			t.Fatalf("updateAgentAuthorization has no overlay entry for %q, so its "+
				"`args.payload` splat lands verbatim and a caller can reassign the grant "+
				"(memql#3138). Overlay keys present: %v",
				decl.Owner, overlayKeys(fn.MutationTemplate.PayloadOverlayTemplate))
		}
		if classifyTemplateValue(overlay) != provStamp {
			t.Fatalf("updateAgentAuthorization's overlay writes %q from something other than "+
				"the actor (%#v). Anything mentioning caller args is caller-controllable, "+
				"which is the hole rather than the fix.", decl.Owner, overlay)
		}
	}
	if !found {
		t.Fatal("updateAgentAuthorization is not in the loaded registry; this gate measured nothing")
	}
}

func overlayKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// grantRowNode builds a stored v1:agents:agentAuthorization row for the read
// tests. Separate from agentAuthzRow because the read path evaluates a
// MemoryNode (concept + id + JSON payload), not a Go map.
func grantRowNode(t *testing.T, id, owner string) memorynodes.MemoryNode {
	t.Helper()
	raw, err := json.Marshal(agentAuthzRow(t, owner))
	require.NoError(t, err)
	return memorynodes.MemoryNode{ID: id, Concept: agentAuthzConcept, Payload: raw}
}

// TestAgentAuthorizationsForSelfReturnsOnlyTheCallersGrants is the read half
// (memql#3129), proven at row level.
//
// The construct used to be `agentAuthorizationsForUser`, `@public`, filtering
// `userId==args.userId` -- a caller-supplied id with no check that the caller IS
// that user, on the concept that carries the computer-use ceiling. It is
// `agentAuthorizationsForSelf` now, with NO userId argument at all, so a caller
// cannot name whose grants get listed.
//
// Evaluated through the engine's own post-filter (the memql#3178 idiom): the
// query string is parsed against the loaded DSL, specs are expanded, actor
// comparisons are folded to constants, and rows are matched with
// nodeMatchesExpression -- the same predicate the executor applies per row. A
// row that does not match here is a row the executor drops, and no database is
// needed to show it.
func TestAgentAuthorizationsForSelfReturnsOnlyTheCallersGrants(t *testing.T) {
	eng := selfScopeEngine(t)

	const (
		caller   = "v1:identity:user:caller"
		stranger = "v1:identity:user:stranger"
	)
	ctx := selfScopeUserCtx(caller)
	filter := evaluableFilter(t, eng, ctx, `query agentAuthorizationsForSelf()`)

	mine := grantRowNode(t, agentAuthzConcept+":mine", caller)
	theirs := grantRowNode(t, agentAuthzConcept+":theirs", stranger)

	if !matchesFilter(t, mine, filter) {
		t.Error("agentAuthorizationsForSelf does not return the CALLER's own grant. " +
			"Self-scoping that returns nothing is not a fix -- it silently disables the " +
			"standing computer-use envelope for every user (memql#3177).")
	}
	if matchesFilter(t, theirs, filter) {
		t.Errorf("agentAuthorizationsForSelf returned a grant owned by %s to caller %s.\n"+
			"That row carries computerUseScope and skillTierAllowlist, and its id is what a "+
			"caller needs to aim revokeAgentAuthorization / updateAgentAuthScope at "+
			"(memql#3129).", stranger, caller)
	}
}

// The old shape must not come back. Deleting `agentAuthorizationsForUser` is
// what makes the caller-supplied id unavailable; a re-added arg would be
// invisible to the test above, which asks only what the CURRENT construct
// returns.
//
// The land gate (TestRowAuthzEnforcementLandGate) would also fail on a
// re-added `userId==args.userId` read of this concept. That is the general
// gate; this is the specific one, and it names what went wrong.
func TestTheCallerSuppliedGrantListIsGone(t *testing.T) {
	registry := loadedTreeRegistry(t)
	for _, fn := range registry.List() {
		if fn == nil || fn.FunctionKind != "query" {
			continue
		}
		if strings.TrimSpace(fn.BoundConcept) != agentAuthzConcept {
			continue
		}
		if fn.ArgsSchema == nil {
			continue
		}
		for _, f := range fn.ArgsSchema.Fields {
			if f != nil && f.Name == "userId" {
				t.Errorf("query %q over %s takes a caller-supplied `userId` argument again.\n"+
					"Reads of this concept derive their row set from actor.userId; an argument "+
					"lets a caller enumerate a stranger's standing grants, which is what "+
					"memql#3129 closed. If an operator view is genuinely needed it lands as "+
					"its own construct with requiresOwnerOrAdmin as a TOP-LEVEL conjunct.",
					fn.Name, agentAuthzConcept)
			}
		}
	}
}

// Filter injection is the FIRST of the read path's two mechanisms, and it is
// the one that pushes down into SQL. The test above measures the authored
// filter; this one measures what the engine ANDs onto it, resolved from
// plan.BoundConcept exactly as enforcement does at runtime.
//
// Both are asserted because they fail independently: an authored filter can be
// correct while the plan carries no injected term (the tier silently not
// resolving), and vice versa.
func TestReadsOfAGrantCarryTheInjectedOwnerTerm(t *testing.T) {
	eng := selfScopeEngine(t)
	ctx := selfScopeUserCtx("v1:identity:user:caller")

	ambient := buildAmbientEnvelope(ctx, eng)
	plan, err := eng.parseWithFunctionsAmbient(`query agentAuthorizationsForSelf()`,
		eng.functions, nil, false, auth.OriginFromContext(ctx), ambient)
	require.NoError(t, err)

	if !plan.RowAuthzInjected {
		t.Fatalf("a read of %s carries no injected row-authz term. The concept declares a "+
			"tier, so enforcement should have ANDed %q into the plan root (memql#3172); "+
			"without it the only remaining defence is the per-row gate.",
			agentAuthzConcept, InjectedPredicate(agentAuthzDecl(t)))
	}
	if plan.RowAuthzConcept != agentAuthzConcept {
		t.Errorf("the injected term resolved against %q, not %q", plan.RowAuthzConcept, agentAuthzConcept)
	}
}

// The SECOND mechanism: row admission, resolved from THE ROW'S OWN concept and
// never from the filter. It is what covers a raw client-supplied query string
// (which has no declared binding) and graph expansion (which has no filter at
// all) -- so a caller who bypasses the named query entirely still cannot read a
// stranger's grant.
func TestAGrantRowIsAdmittedOnlyToItsOwner(t *testing.T) {
	agentAuthzDecl(t)

	const (
		owner    = "v1:identity:user:grant-owner"
		stranger = "v1:identity:user:stranger"
	)
	row := grantRowNode(t, agentAuthzConcept+":the-grant", owner)

	if !admitRowAuthzNode(rowAuthzCallerCtx(owner), row) {
		t.Error("the row gate denied a grant to its own owner")
	}
	if admitRowAuthzNode(rowAuthzCallerCtx(stranger), row) {
		t.Errorf("the row gate admitted %s's grant to %s. This is the mechanism that covers a "+
			"raw query string, which has no declared binding to resolve a tier from "+
			"(memql#3172).", owner, stranger)
	}
	if admitRowAuthzTraversal(rowAuthzCallerCtx(stranger), row) {
		t.Error("graph expansion reached a stranger's grant; traversal has no filter to defer to " +
			"and must fail closed")
	}
}
