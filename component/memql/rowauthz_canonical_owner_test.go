package memql

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// THE CANONICAL-VS-BARE REGRESSION (memql#3172, found by the DB lane).
//
// An owner field is an outgoing `@relationship`, so executeWrite's
// canonicalizeRelationshipFields rewrites it before the row is stored: a
// note created by caller `u1` stores `ownerUserId:
// "v1:identity:user:u1"`. Row-authz compared that against the actor
// envelope's BARE `u1` with a plain `==`, and against a filter RHS that
// was only canonicalized when the concept could be read off the filter
// text. Both comparisons were false for every row, so all 13 declared
// concepts read back empty -- for their owners too.
//
// WHY THE LOCAL SUITES WERE GREEN. Every DB-free test in this package
// builds its fixture rows by hand, so it seeded the stored owner and the
// caller id with the SAME spelling. The mismatch only exists once
// canonicalizeRelationshipFields has run, which needs a real write
// through executeWrite. That is precisely what the db-tests lane runs
// and a unit test cannot.
//
// So these tests do the one thing a DB-free test CAN do honestly: they
// run the real canonicalizer over the payload, exactly as the write path
// does, and then require every row-authz surface to agree with what it
// produced. No hand-spelled owner values.

// conceptEngine builds a DB-free engine that carries the loaded concept
// REGISTRY, which is what the id canonicalizers need (they resolve a
// relationship's target concept; they never touch the database).
func conceptEngine(t *testing.T, probes map[string]string, boundConcept string) *MemQLEngine {
	t.Helper()
	e := probeEngine(t, boundConcept, probes)
	e.concepts = memorynodes.DefaultRegistry()
	return e
}

// storedOwnerFor returns the value the WRITE path would persist in the
// declared owner field for a caller, by running the same canonicalizer
// executeWrite runs (canonicalizeRelationshipFields, executor_mutation.go).
func storedOwnerFor(t *testing.T, e *MemQLEngine, conceptName, ownerField, caller string) string {
	t.Helper()
	payload := map[string]any{ownerField: caller}
	if err := e.canonicalizeRelationshipFields(context.Background(), conceptName, payload); err != nil {
		t.Fatalf("canonicalizeRelationshipFields: %v", err)
	}
	stored, _ := payload[ownerField].(string)
	if strings.TrimSpace(stored) == "" {
		t.Fatalf("the write path stored no owner for %s.%s", conceptName, ownerField)
	}
	return stored
}

// THE FIXTURE'S OWN PREMISE, asserted first. If the write path stopped
// canonicalizing the owner field, every test below would still pass
// while measuring nothing -- so this states the mismatch exists before
// anything claims to close it.
func TestTheWritePathCanonicalisesTheDeclaredOwnerField(t *testing.T) {
	decl := declFor(t, declaredOwnedConcept)
	e := conceptEngine(t, map[string]string{}, declaredOwnedConcept)

	const caller = "user-canon-3172"
	stored := storedOwnerFor(t, e, declaredOwnedConcept, decl.Owner, caller)

	if stored == caller {
		t.Skipf("%s.%s is no longer canonicalized on write (stored %q unchanged), so the "+
			"canonical-vs-bare mismatch this file pins cannot occur; re-aim the fixture",
			declaredOwnedConcept, decl.Owner, stored)
	}
	if !strings.HasSuffix(stored, ":"+caller) {
		t.Fatalf("the write path stored %q, which does not end in the caller's bare id -- "+
			"this fixture no longer knows what the write path does", stored)
	}
}

// SURFACE 1 -- the per-row gate (the read path's net, and via
// rowAuthzAdmits the #3174 write guard too).
func TestRowGateAdmitsACanonicalStoredOwnerForABareCaller(t *testing.T) {
	decl := declFor(t, declaredOwnedConcept)
	e := conceptEngine(t, map[string]string{}, declaredOwnedConcept)

	const caller = "user-canon-3172-gate"
	stored := storedOwnerFor(t, e, declaredOwnedConcept, decl.Owner, caller)

	raw, err := json.Marshal(map[string]any{decl.Owner: stored, "title": "mine"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	row := memorynodes.MemoryNode{
		ID:      declaredOwnedConcept + ":row-canon-3172",
		Concept: declaredOwnedConcept,
		Payload: raw,
	}

	if !admitRowAuthzNode(callerCtx(caller), row) {
		t.Fatalf("the row gate denied the OWNER their own row. The write path stores %q and "+
			"the actor envelope carries the bare %q, so a raw `==` between them is false for "+
			"every row -- all 13 declared concepts read back empty, for everyone "+
			"(memql#3172, caught by the db-tests lane).", stored, caller)
	}
	if !admitRowAuthzTraversal(callerCtx(caller), row) {
		t.Fatal("the traversal gate denied the owner their own row on the same payload")
	}

	// And the fix must not have widened anything: a stranger is still
	// refused, whichever spelling they arrive in.
	for _, stranger := range []string{"user-canon-3172-other", "v1:identity:user:user-canon-3172-other"} {
		if admitRowAuthzNode(callerCtx(stranger), row) {
			t.Errorf("caller %q was admitted to a row owned by %q", stranger, stored)
		}
	}
}

// SURFACE 2 -- the injected filter predicate, on a filter whose concept
// CANNOT be read off the text.
//
// The generic canonicalize-RHS pass takes its concept from
// extractConceptFromExpression, which answers "" for a top-level
// disjunction. The tier is resolved from the declared binding, so
// enforcement reaches this filter; without the concept carried on the
// injected node, the RHS stays bare and the term matches nothing.
func TestInjectedPredicateIsCanonicalisedWithoutAConceptContext(t *testing.T) {
	decl := declFor(t, declaredOwnedConcept)
	const caller = "user-canon-3172-filter"

	e := conceptEngine(t, map[string]string{
		// A top-level `||`: no derivable concept context. This is one of
		// the four spellings memql#3172 finding 1 is about.
		"probeCanonOr": `concept=="` + declaredOwnedConcept + `" || status=="published"`,
	}, declaredOwnedConcept)

	plan, err := e.parseWithFunctions("probeCanonOr()", e.functions, nil, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !plan.RowAuthzInjected {
		t.Fatal("the probe was not enforced, so this test measures nothing")
	}

	ctx := callerCtx(caller)
	resolved, err := resolveActorReferences(ctx, plan.Root)
	if err != nil {
		t.Fatalf("resolveActorReferences: %v", err)
	}
	// EXACTLY what evaluateExpressionSet does next, concept context and
	// all -- including the "" this filter yields.
	conceptContext := extractConceptFromExpression(resolved)
	if conceptContext != "" {
		t.Fatalf("this filter was supposed to yield no concept context, got %q -- the fixture "+
			"no longer exercises the case the fix is for", conceptContext)
	}
	resolved = e.canonicalizeRelationshipComparisons(ctx, resolved, conceptContext)

	injected := findRowAuthzComparison(resolved)
	if injected == nil {
		t.Fatal("no injected row-authz comparison survived the resolve/canonicalize passes")
	}
	got, _ := injected.Value.(string)
	want := storedOwnerFor(t, e, declaredOwnedConcept, decl.Owner, caller)
	if got != want {
		t.Fatalf("the injected predicate compares against %q, but the write path stores %q.\n"+
			"The two must be the same spelling or the term matches no row -- including the "+
			"owner's own (memql#3172). The RHS is canonicalized from the DECLARATION carried "+
			"on the node, because this filter yields no concept context to canonicalize from.",
			got, want)
	}
}

// findRowAuthzComparison returns the injected owner comparison, or nil.
func findRowAuthzComparison(expr ExpressionNode) *ComparisonExpression {
	switch n := expr.(type) {
	case *ComparisonExpression:
		if n != nil && strings.TrimSpace(n.RowAuthzConcept) != "" {
			return n
		}
		return nil
	case *LogicalExpression:
		if n == nil {
			return nil
		}
		if found := findRowAuthzComparison(n.Left); found != nil {
			return found
		}
		return findRowAuthzComparison(n.Right)
	default:
		return nil
	}
}

// The same claim on a filter that DOES yield a concept context: the
// node-carried concept must not change the answer where the generic
// pass already had one.
func TestInjectedPredicateIsCanonicalisedWithAConceptContextToo(t *testing.T) {
	decl := declFor(t, declaredOwnedConcept)
	const caller = "user-canon-3172-ctx"

	e := conceptEngine(t, map[string]string{
		"probeCanonEq": `concept=="` + declaredOwnedConcept + `"`,
	}, declaredOwnedConcept)

	plan, err := e.parseWithFunctions("probeCanonEq()", e.functions, nil, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ctx := callerCtx(caller)
	resolved, err := resolveActorReferences(ctx, plan.Root)
	if err != nil {
		t.Fatalf("resolveActorReferences: %v", err)
	}
	resolved = e.canonicalizeRelationshipComparisons(ctx, resolved, extractConceptFromExpression(resolved))

	injected := findRowAuthzComparison(resolved)
	if injected == nil {
		t.Fatal("no injected row-authz comparison found")
	}
	want := storedOwnerFor(t, e, declaredOwnedConcept, decl.Owner, caller)
	if got, _ := injected.Value.(string); got != want {
		t.Fatalf("injected RHS is %q, the write path stores %q", got, want)
	}
}

// SURFACE 3 -- the raw-insert owner stamp (memql#3175).
//
// The stamp writes the caller id as the envelope carries it and relies
// on canonicalizeRelationshipFields running AFTERWARDS to bring it to
// the stored spelling. That ordering is load-bearing and nothing else
// pins it: move the stamp below the canonicalize call and it lands a
// bare owner in a canonical column, which is the same mismatch from the
// write side.
func TestTheOwnerStampRunsBeforeTheRelationshipCanonicaliser(t *testing.T) {
	src, err := os.ReadFile("executor_mutation.go")
	if err != nil {
		t.Fatalf("read executor_mutation.go: %v", err)
	}
	text := string(src)
	stamp := strings.Index(text, "stampRowAuthzOwner(")
	canon := strings.Index(text, "e.canonicalizeRelationshipFields(")
	if stamp < 0 {
		t.Fatal("executeWrite no longer stamps the declared owner (memql#3175)")
	}
	if canon < 0 {
		t.Fatal("executeWrite no longer canonicalizes relationship fields -- this guard needs re-aiming")
	}
	if stamp > canon {
		t.Fatal("the row-authz owner stamp now runs AFTER canonicalizeRelationshipFields, so the " +
			"stamped value never gets canonicalized: the row would carry a bare owner in a " +
			"field every other row stores canonically, and the read gate would compare it " +
			"against the wrong spelling (memql#3172 / #3175).")
	}
}

// And the stamp is not double-canonicalizing: a caller id that ALREADY
// arrives canonical survives the canonicalizer unchanged, so stamping
// either spelling converges on one stored value.
func TestStampedOwnerConvergesFromEitherSpelling(t *testing.T) {
	decl := declFor(t, declaredOwnedConcept)
	e := conceptEngine(t, map[string]string{}, declaredOwnedConcept)

	const bare = "user-canon-3172-converge"
	fromBare := storedOwnerFor(t, e, declaredOwnedConcept, decl.Owner, bare)
	fromCanonical := storedOwnerFor(t, e, declaredOwnedConcept, decl.Owner, fromBare)

	if fromBare != fromCanonical {
		t.Fatalf("stamping the bare id stores %q but stamping the canonical id stores %q -- "+
			"the canonicalizer is not idempotent on its own output and the stored spelling "+
			"depends on how the caller happened to be identified", fromBare, fromCanonical)
	}
}

// The shared helper's contract, stated directly: every spelling pair of
// one id matches, and different ids never do.
func TestSameRowAuthzOwnerAcceptsEverySpellingPairing(t *testing.T) {
	const (
		bare      = "u1"
		canonical = "v1:identity:user:u1"
	)
	for _, tc := range []struct {
		stored, caller string
		want           bool
	}{
		{bare, bare, true},
		{canonical, canonical, true},
		{canonical, bare, true},
		{bare, canonical, true},
		{canonical, "u2", false},
		{"v1:identity:user:u2", bare, false},
		{"", bare, false},
		{canonical, "", false},
		{"", "", false},
	} {
		if got := sameRowAuthzOwner(tc.stored, tc.caller); got != tc.want {
			t.Errorf("sameRowAuthzOwner(%q, %q) = %v, want %v", tc.stored, tc.caller, got, tc.want)
		}
	}
}

// The self-owned form (memql#3029) uses the same helper, so it answers
// the same way for the same pairs. Pinned because it had its OWN
// hand-rolled prefix strip before this fix, which is how the two
// spellings of one rule drifted in the first place.
func TestSelfOwnedTierUsesTheSharedOwnerComparison(t *testing.T) {
	name := registerProbeConcept(t, "v1:rowauthzprobe:selfOwnedThing",
		&langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: langparser.RowAuthzSelfOwnedField})

	row := memorynodes.MemoryNode{ID: name + ":u1", Concept: name, Payload: []byte(`{}`)}
	if got := rowAuthzAdmits(callerCtx("u1"), row.Concept, row.ID, row.Payload); got != rowAuthzAdmit {
		t.Errorf("self-owned row denied to the bare caller id it belongs to: %v", got)
	}
	if got := rowAuthzAdmits(callerCtx(name+":u1"), row.Concept, row.ID, row.Payload); got != rowAuthzAdmit {
		t.Errorf("self-owned row denied to its own canonical id: %v", got)
	}
	if got := rowAuthzAdmits(callerCtx("u2"), row.Concept, row.ID, row.Payload); got != rowAuthzDeny {
		t.Errorf("self-owned row admitted to a stranger: %v", got)
	}
}

// SURFACE 2b -- the self-owned form (`@rowAuthz(owner="id")`,
// memql#3029), whose term names the ROW'S OWN identity rather than a
// payload field.
//
// It has the same spelling problem from a different direction: the id
// COLUMN stores canonical ids and actor.userId resolves to the bare
// form. No concept declares the form today, so this declares one
// synthetically rather than waiting for the day #3029 unblocks it on
// v1:identity:user and the trap springs with real rows behind it.
func TestSelfOwnedInjectedPredicateIsCanonicalised(t *testing.T) {
	const caller = "user-canon-3172-self"
	name := registerProbeConcept(t, "v1:rowauthzprobe:selfOwnedFilterThing",
		&langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: langparser.RowAuthzSelfOwnedField})

	e := conceptEngine(t, map[string]string{
		// Again a top-level `||`, so there is no concept context for the
		// generic short-id resolution to work from.
		"probeSelfOwned": `concept=="` + name + `" || status=="published"`,
	}, name)

	plan, err := e.parseWithFunctions("probeSelfOwned()", e.functions, nil, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !plan.RowAuthzInjected {
		t.Fatal("the self-owned probe was not enforced, so this test measures nothing")
	}

	ctx := callerCtx(caller)
	resolved, err := resolveActorReferences(ctx, plan.Root)
	if err != nil {
		t.Fatalf("resolveActorReferences: %v", err)
	}
	conceptContext := extractConceptFromExpression(resolved)
	resolved = e.canonicalizeRelationshipComparisons(ctx, resolved, conceptContext)

	injected := findRowAuthzComparison(resolved)
	if injected == nil {
		t.Fatal("no injected row-authz comparison survived the resolve/canonicalize passes")
	}
	want := name + ":" + caller
	if got, _ := injected.Value.(string); got != want {
		t.Fatalf("the self-owned term compares the id column against %q, but ids are STORED "+
			"canonically as %q -- the term would match no row, the caller's own included "+
			"(memql#3172 / #3029)", got, want)
	}
}
