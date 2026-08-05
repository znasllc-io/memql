package memql

import (
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// rowauthz_enforce_test.go -- memql#3076.

// The three renderings of the predicate must agree, or the gate lies: the
// report says one thing, the matcher accepts another, and the enforcer injects
// a third.
//
// Specifically -- the node the enforcer builds must be the node the matcher
// accepts. If it were not, AnalyzeShadow would report would-narrow for a filter
// that already spells the predicate verbatim (overstating the blast radius), or
// already-implied for one that does not (understating it, which is the
// dangerous direction).
func TestEnforcedNodeIsAcceptedByTheAnalyzersMatcher(t *testing.T) {
	for _, tc := range []struct {
		name string
		decl *langparser.RowAuthzDecl
	}{
		{"owned by a payload field", &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"}},
		{"self-owned", &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: langparser.RowAuthzSelfOwnedField}},
		{"clusterOwner", &langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, err := enforcedPredicateNode(tc.decl)
			if err != nil {
				t.Fatalf("enforcedPredicateNode: %v", err)
			}
			if node == nil {
				t.Fatal("this tier must inject a predicate")
			}
			// The analyzer must call a filter that is EXACTLY the injected
			// predicate already-implied. Anything else means the two disagree
			// about the one predicate they both describe.
			verdict, reason := AnalyzeShadow(node, tc.decl)
			if verdict != ShadowAlreadyImplied {
				t.Errorf("the analyzer does not recognise the enforcer's own predicate: "+
					"verdict=%v reason=%q.\nInjectedPredicate renders %q -- the renderer, the "+
					"matcher and the enforcer must agree or the gate reports one thing and does "+
					"another (memql#3076).", verdict, reason, InjectedPredicate(tc.decl))
			}
		})
	}
}

// public injects nothing, explicitly. It is the one tier where "no predicate"
// is the declaration rather than a gap, so it is asserted rather than left to
// fall out of a nil check.
func TestPublicTierInjectsNothing(t *testing.T) {
	node, err := enforcedPredicateNode(&langparser.RowAuthzDecl{Tier: langparser.RowAuthzPublic})
	if err != nil {
		t.Fatalf("public must not error: %v", err)
	}
	if node != nil {
		t.Errorf("public injected %#v; it must inject nothing", node)
	}
}

// granted injects its spec. Implication is undecidable without relationship
// semantics (Phase 4), but INJECTION needs no such analysis -- ANDing a
// predicate is sound whether or not the filter already carries it.
func TestGrantedTierInjectsItsSpec(t *testing.T) {
	node, err := enforcedPredicateNode(&langparser.RowAuthzDecl{
		Tier: langparser.RowAuthzGranted, Spec: "isSpaceMember",
	})
	if err != nil {
		t.Fatalf("granted must not error: %v", err)
	}
	ref, ok := node.(*SpecReferenceExpression)
	if !ok || ref.Name != "isSpaceMember" {
		t.Fatalf("granted must inject its spec reference, got %#v", node)
	}
}

// An unrecognised tier must FAIL the read rather than serve it unfiltered.
// This is the fail-closed arm, and it is the one behaviour that must never
// regress to a silent pass: a tier added to the language without being added
// to the enforcer would otherwise mean "declared, and quietly not enforced".
func TestUnknownTierRefusesTheRead(t *testing.T) {
	_, err := enforcedPredicateNode(&langparser.RowAuthzDecl{Tier: langparser.RowAuthzTier("newTierNobodyWired")})
	if err == nil {
		t.Fatal("an unrecognised tier must refuse the read. Returning nil predicate would serve " +
			"the rows unfiltered while the concept declares a restriction (memql#3076)")
	}
	if !strings.Contains(err.Error(), "unrecognised tier") {
		t.Errorf("the error must name the cause; got %v", err)
	}
	// Owned with no owner field, and granted with no spec, are the same class.
	if _, err := enforcedPredicateNode(&langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned}); err == nil {
		t.Error("owned with no owner field must refuse the read")
	}
	if _, err := enforcedPredicateNode(&langparser.RowAuthzDecl{Tier: langparser.RowAuthzGranted}); err == nil {
		t.Error("granted with no spec must refuse the read")
	}
}

// ENFORCEMENT BITES. A filter over a declared concept that lacks the ownership
// conjunct must come back with it ANDed in.
//
// This is the regression test the DoD asks for: without it, every other test
// here would pass against an enforcer that returns expr unchanged.
func TestEnforcementAndsThePredicateIntoAnUnscopedFilter(t *testing.T) {
	decl := &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"}
	predicate, err := enforcedPredicateNode(decl)
	if err != nil {
		t.Fatalf("enforcedPredicateNode: %v", err)
	}

	// A filter with no ownership term at all.
	unscoped := &ComparisonExpression{
		Field:    FieldReference{Raw: "status", Parts: []string{"status"}},
		Operator: OpEq,
		Value:    "active",
	}
	if v, _ := AnalyzeShadow(unscoped, decl); v == ShadowAlreadyImplied {
		t.Fatal("control broken: the unscoped filter must NOT already imply the predicate")
	}

	got, err := andIntoFilter(unscoped, predicate)
	if err != nil {
		t.Fatalf("andIntoFilter: %v", err)
	}
	if v, reason := AnalyzeShadow(got, decl); v != ShadowAlreadyImplied {
		t.Errorf("after enforcement the filter must imply the tier's predicate; got %v (%s)", v, reason)
	}
}

// The predicate must land in the FILTER, not in a wrapper.
//
// A loaded query's expression is the whole pipeline -- shape(paginate(sort(f)))
// -- so ANDing the outermost node would put an authorization term into a
// projection or an ordering: at best a compile error, at worst a filter that
// does not filter. Asserted through the analyzer, which unwraps to the filter
// before judging, so a term parked in a wrapper reads as not-implied.
func TestEnforcementReachesThroughReadPipelineWrappers(t *testing.T) {
	decl := &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"}
	predicate, err := enforcedPredicateNode(decl)
	if err != nil {
		t.Fatalf("enforcedPredicateNode: %v", err)
	}
	inner := &ComparisonExpression{
		Field: FieldReference{Raw: "status", Parts: []string{"status"}}, Operator: OpEq, Value: "active",
	}

	for name, wrapped := range map[string]ExpressionNode{
		"shape":     &ShapeExpression{Target: inner},
		"paginate":  &PaginateExpression{Target: inner},
		"sort":      &SortExpression{Target: inner},
		"select":    &SelectExpression{Target: inner},
		"timestamp": &TimestampExpression{Target: inner},
		"depth":     &DepthExpression{Target: inner},
		"nested":    &ShapeExpression{Target: &PaginateExpression{Target: &SortExpression{Target: inner}}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := andIntoFilter(wrapped, predicate)
			if err != nil {
				t.Fatalf("andIntoFilter: %v", err)
			}
			if v, reason := AnalyzeShadow(got, decl); v != ShadowAlreadyImplied {
				t.Errorf("the predicate did not reach the filter through a %s wrapper: %v (%s).\n"+
					"andIntoFilter must know every wrapper unwrapToFilter looks through, or the "+
					"term lands in a projection instead of a selection (memql#3076).",
					name, v, reason)
			}
			// And the wrapper must SURVIVE -- enforcement must not strip the
			// shape/pagination the caller asked for.
			if _, stillWrapped := got.(*ComparisonExpression); stillWrapped {
				t.Errorf("enforcement collapsed the %s wrapper; the read pipeline must survive", name)
			}
		})
	}
}

// A concept that declares nothing is untouched -- byte-identical expression
// back. 84% of the tree is undeclared, so this is the common case.
func TestUndeclaredConceptIsUntouched(t *testing.T) {
	expr := &ComparisonExpression{
		Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
		Operator: OpEq, Value: "v1:nothing:declared",
	}
	got, err := enforceRowAuthzFilter(expr)
	if err != nil {
		t.Fatalf("enforceRowAuthzFilter: %v", err)
	}
	if got != ExpressionNode(expr) {
		t.Errorf("an undeclared concept must be returned unchanged, got %#v", got)
	}
}

// TestEnforcedPredicateUsesTheLoweredFieldSpelling is the test that would have
// caught the defect the Postgres lane found.
//
// Injection happens in the executor, AFTER ast_converter lowers author
// spellings, so the node must be in the form the FILTER COMPILER accepts --
// `payload.<prop>`, a bare intrinsic, or `actor.<field>`. The first draft of
// the enforcer emitted the author spelling (`ownerUserId`, `row.id`), which
// compiles to "field %q is not supported" at query time.
//
// Every non-database test passed against that draft, because the analyzer's
// topLevelPayloadField deliberately accepts BOTH spellings -- it reads filters
// from both sides of the lowering. So asserting "the analyzer recognises the
// enforcer's node" cannot catch it. This drives the real compiler instead.
func TestEnforcedPredicateUsesTheLoweredFieldSpelling(t *testing.T) {
	engine := &MemQLEngine{}
	// Every real read is concept-bound, so the compiler is given the concept
	// context it would have in production. Without it compileIdComparison
	// refuses a short id, which is a property of id comparison generally and
	// not of the injected predicate.
	const conceptContext = "v1:identity:user"
	for _, tc := range []struct {
		name string
		decl *langparser.RowAuthzDecl
	}{
		{"owned by a payload field", &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"}},
		{"self-owned", &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: langparser.RowAuthzSelfOwnedField}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, err := enforcedPredicateNode(tc.decl)
			if err != nil {
				t.Fatalf("enforcedPredicateNode: %v", err)
			}
			cmp, ok := node.(*ComparisonExpression)
			if !ok {
				t.Fatalf("expected a comparison, got %#v", node)
			}
			// The actor value is resolved by resolveActorReferences before
			// compilation, so substitute a concrete one here -- this test is
			// about the FIELD spelling, not about actor resolution.
			probe := *cmp
			probe.Value = "u-probe"
			if _, err := engine.compileComparisonExpressionWithContext(&probe, conceptContext); err != nil {
				t.Errorf("the injected predicate does not compile: %v\n"+
					"Field was %q (parts %v). The executor injects AFTER ast_converter lowers "+
					"author spellings, so the node must use the lowered form -- "+
					"`payload.<prop>` for a payload field, a bare intrinsic for row.id. The "+
					"analyzer accepts both spellings, so it cannot catch this (memql#3076).",
					err, cmp.Field.Raw, cmp.Field.Parts)
			}
		})
	}
}

// The cluster-owner predicate takes a different compile path
// (compileActorFieldComparison, reached before the payload branch), so it is
// asserted separately rather than folded into the loop above.
func TestClusterOwnerPredicateIsAnActorFieldComparison(t *testing.T) {
	node, err := enforcedPredicateNode(&langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner})
	if err != nil {
		t.Fatalf("enforcedPredicateNode: %v", err)
	}
	cmp, ok := node.(*ComparisonExpression)
	if !ok {
		t.Fatalf("expected a comparison, got %#v", node)
	}
	if !isActorFieldComparison(cmp) {
		t.Errorf("the clusterOwner predicate must be an actor-field comparison so it reaches "+
			"compileActorFieldComparison; got field %q (parts %v)", cmp.Field.Raw, cmp.Field.Parts)
	}
}
