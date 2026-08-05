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
