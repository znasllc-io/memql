package memql

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// THE LAND-TIME GATE for row-authz enforcement (memql#3172).
//
// Enforcement ANDs a declared tier's predicate into every read of the
// concept. For the constructs already in the tree that is a no-op ONLY
// while every one of them already carries the term as a top-level
// conjunct -- which is why Phase 1 declared those concepts at all. One
// new query over a declared concept that omits the conjunct turns this
// change from a no-op into a silent result-set change for whoever calls
// it.
//
// So the measurement is re-derived HERE, at build time, and a
// would-narrow or undecidable entry FAILS. The response to a failure is
// to adjudicate the entry -- fix the query, or declare the read's
// broader scope deliberately -- never to widen the gate.
//
// This is the gate that replaces TestRowAuthzIsInert, retired in the
// same commit that landed enforcement. The tree is never left with
// neither.

type gateRow struct {
	construct string
	concept   string
	tier      string
	predicate string
	verdict   ShadowVerdict
	reason    string
}

// measureDeclaredReads runs the analyzer over every query construct in
// the registry whose DECLARED BINDING names a concept carrying a tier.
//
// Resolution is from fn.BoundConcept, the same source enforcement uses
// at runtime. A gate that found its concepts by reading filter text
// would go quiet for exactly the spellings enforcement covers and this
// gate exists to watch (memql#3172 finding 1).
func measureDeclaredReads(registry *FunctionRegistry) (measured []gateRow, undeclared int, queries int) {
	for _, fn := range registry.List() {
		if fn == nil || fn.FunctionKind != "query" {
			continue
		}
		queries++
		bound := strings.TrimSpace(fn.BoundConcept)
		if bound == "" {
			continue
		}
		concept, err := memoryNodes.Get(bound)
		if err != nil || concept == nil {
			continue
		}
		if concept.RowAuthz == nil {
			undeclared++
			continue
		}
		verdict, reason := AnalyzeShadow(fn.Expr, concept.RowAuthz)
		measured = append(measured, gateRow{
			construct: fn.Name,
			concept:   bound,
			tier:      string(concept.RowAuthz.Tier),
			predicate: InjectedPredicate(concept.RowAuthz),
			verdict:   verdict,
			reason:    reason,
		})
	}
	sort.Slice(measured, func(i, j int) bool {
		if measured[i].concept != measured[j].concept {
			return measured[i].concept < measured[j].concept
		}
		return measured[i].construct < measured[j].construct
	})
	return measured, undeclared, queries
}

func loadedTreeRegistry(t *testing.T) *FunctionRegistry {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(nil, registry, memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	return registry
}

func TestRowAuthzEnforcementLandGate(t *testing.T) {
	registry := loadedTreeRegistry(t)
	measured, undeclared, queries := measureDeclaredReads(registry)

	counts := map[ShadowVerdict]int{}
	for _, r := range measured {
		counts[r.verdict]++
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\n=== rowAuthz enforcement, re-derived at this tree (memql#3172) ===\n\n")
	fmt.Fprintf(&b, "query constructs in the tree      %d\n", queries)
	fmt.Fprintf(&b, "  measured (concept declares)     %d\n", len(measured))
	fmt.Fprintf(&b, "  not measurable (undeclared)     %d\n\n", undeclared)
	fmt.Fprintf(&b, "verdicts over the measured set:\n")
	for _, v := range []ShadowVerdict{ShadowAlreadyImplied, ShadowWouldNarrow, ShadowUndecidable} {
		fmt.Fprintf(&b, "  %-16s %d\n", v, counts[v])
	}
	t.Log(b.String())

	if len(measured) == 0 {
		t.Fatal("measured nothing -- either the tree failed to load or the declarations are gone, " +
			"and a gate that measures nothing passes forever")
	}

	for _, r := range measured {
		if r.verdict == ShadowAlreadyImplied {
			continue
		}
		t.Errorf("%s over %s (tier %s) is %s under enforcement: %s\n"+
			"Enforcement ANDs %q into every read of this concept, so this construct's result "+
			"set CHANGES for its callers.\n"+
			"Adjudicate the entry -- add the conjunct if the read should be scoped, or make the "+
			"broader read deliberate (a different tier on the concept, or a construct that does "+
			"not bind it). Do NOT widen this gate: it is the only thing standing between a new "+
			"query and a silent result-set change (memql#3172).",
			r.construct, r.concept, r.tier, r.verdict, r.reason, r.predicate)
	}
}

// REGRESSION (memql#3172 acceptance criterion): a query over a declared
// concept with the ownership conjunct removed narrows under enforcement,
// and the gate above catches it.
//
// Both halves matter. Without the first, enforcement could be a no-op
// and the gate would still be green. Without the second, the gate could
// be blind and enforcement would change result sets with nothing
// watching.
func TestGateCatchesAStrippedOwnershipConjunct(t *testing.T) {
	registry := loadedTreeRegistry(t)
	decl := declFor(t, declaredOwnedConcept)

	// Pick a REAL construct over the declared concept and take the
	// ownership conjunct back out of a copy of its filter. The first
	// candidate whose remainder is renderable back to source wins --
	// re-parsing the remainder is what keeps this test honest, since a
	// hand-assembled AST could carry a shape the parser would never
	// produce.
	var (
		victim   *Function
		stripped ExpressionNode
	)
	candidates := registry.List()
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	for _, fn := range candidates {
		if fn == nil || fn.FunctionKind != "query" || fn.BoundConcept != declaredOwnedConcept {
			continue
		}
		if verdict, _ := AnalyzeShadow(fn.Expr, decl); verdict != ShadowAlreadyImplied {
			continue
		}
		remainder := stripOwnerConjunct(unwrapToFilter(fn.Expr), decl.Owner)
		if remainder == nil || !renderableFilter(remainder) {
			continue
		}
		victim, stripped = fn, remainder
		break
	}
	if victim == nil {
		t.Fatalf("no construct over %s carries a strippable ownership conjunct with a renderable "+
			"remainder", declaredOwnedConcept)
	}

	// HALF ONE: the gate catches it.
	verdict, reason := AnalyzeShadow(stripped, decl)
	if verdict == ShadowAlreadyImplied {
		t.Fatalf("%s still reads as already-implied after its ownership conjunct was removed "+
			"(%s) -- the land-time gate would not catch the very change it exists to catch",
			victim.Name, canonicalExpression(stripped))
	}
	t.Logf("stripped %s: %s (%s)", victim.Name, verdict, reason)

	// HALF TWO: enforcement puts it back, so the read is narrowed again.
	e := probeEngine(t, declaredOwnedConcept, map[string]string{
		"probeStripped": strippedFilterSource(t, stripped),
	})
	plan, err := e.parseWithFunctions("probeStripped()", e.functions, nil, false)
	if err != nil {
		t.Fatalf("parse the stripped construct: %v", err)
	}
	if !plan.RowAuthzInjected {
		t.Fatal("the stripped construct was not enforced")
	}
	if got, _ := AnalyzeShadow(plan.Root, decl); got != ShadowAlreadyImplied {
		t.Fatalf("the stripped construct's PLAN is still %q after enforcement: %s",
			got, canonicalExpression(plan.Root))
	}
}

// stripOwnerConjunct removes every top-level `<owner> == actor.userId`
// conjunct from an expression, which is what a newly-authored query
// that forgot the term looks like.
func stripOwnerConjunct(expr ExpressionNode, ownerField string) ExpressionNode {
	logical, ok := expr.(*LogicalExpression)
	if !ok {
		if isOwnerScopeLeaf(expr, ownerField) {
			return nil
		}
		return expr
	}
	if logical.Op != LogicalAnd {
		return expr
	}
	left := stripOwnerConjunct(logical.Left, ownerField)
	right := stripOwnerConjunct(logical.Right, ownerField)
	switch {
	case left == nil && right == nil:
		return nil
	case left == nil:
		return right
	case right == nil:
		return left
	default:
		return &LogicalExpression{Op: LogicalAnd, Left: left, Right: right}
	}
}

// strippedFilterSource renders a stripped filter back to source so the
// probe engine can re-parse it through the real pipeline rather than
// having a hand-assembled AST smuggled past the parser.
func strippedFilterSource(t *testing.T, expr ExpressionNode) string {
	t.Helper()
	switch node := expr.(type) {
	case *ComparisonExpression:
		return comparisonSource(t, node)
	case *LogicalExpression:
		op := " && "
		if node.Op == LogicalOr {
			op = " || "
		}
		return "(" + strippedFilterSource(t, node.Left) + op + strippedFilterSource(t, node.Right) + ")"
	default:
		t.Fatalf("cannot render %T back to source; pick a simpler victim construct", expr)
		return ""
	}
}

func comparisonSource(t *testing.T, cmp *ComparisonExpression) string {
	t.Helper()
	field := cmp.Field.Raw
	if field == "" {
		field = strings.Join(cmp.Field.Parts, ".")
	}
	switch v := cmp.Value.(type) {
	case string:
		return fmt.Sprintf("%s%s%q", field, cmp.Operator, v)
	case bool:
		return fmt.Sprintf("%s%s%t", field, cmp.Operator, v)
	default:
		t.Fatalf("cannot render comparison value %T back to source", v)
		return ""
	}
}

// renderableFilter reports whether an expression is made only of the
// shapes strippedFilterSource can put back into source: AND/OR chains
// over comparisons against string or bool literals. Anything else (an
// arg reference, a `when(...)` guard, a spec reference) means picking a
// different victim rather than smuggling a hand-built AST past the
// parser.
func renderableFilter(expr ExpressionNode) bool {
	switch node := expr.(type) {
	case *ComparisonExpression:
		switch node.Value.(type) {
		case string, bool:
			return true
		default:
			return false
		}
	case *LogicalExpression:
		return renderableFilter(node.Left) && renderableFilter(node.Right)
	default:
		return false
	}
}

// The gate must be resolvable from the declared binding, not from
// filter text -- otherwise it goes quiet on exactly the spellings
// memql#3172 finding 1 is about.
func TestLandGateResolvesConceptsFromTheDeclaredBinding(t *testing.T) {
	registry := loadedTreeRegistry(t)
	measured, _, _ := measureDeclaredReads(registry)

	byFilterText := 0
	for _, r := range measured {
		fn, err := registry.Get(r.construct)
		if err != nil || fn == nil {
			continue
		}
		if extractConceptFromExpression(fn.Expr) == r.concept {
			byFilterText++
		}
	}
	if len(measured) == 0 {
		t.Fatal("nothing measured")
	}
	t.Logf("%d of %d measured constructs would ALSO have been found by filter text; the "+
		"remainder are found only because the gate reads the declared binding",
		byFilterText, len(measured))

	// The declaration is the source of truth: every measured construct
	// must have a bound concept that declares a tier, whatever its
	// filter says.
	for _, r := range measured {
		fn, err := registry.Get(r.construct)
		if err != nil || fn == nil {
			t.Fatalf("construct %s vanished from the registry", r.construct)
		}
		if strings.TrimSpace(fn.BoundConcept) != r.concept {
			t.Fatalf("%s was measured against %s but declares %s",
				r.construct, r.concept, fn.BoundConcept)
		}
	}
}

// The tier the gate reports is the tier the concept declares -- no
// hypothesising, no inference. Guards against a future edit teaching
// the gate to guess a tier for an undeclared concept, which would make
// its verdicts unactionable.
func TestLandGateReportsOnlyDeclaredTiers(t *testing.T) {
	registry := loadedTreeRegistry(t)
	measured, _, _ := measureDeclaredReads(registry)
	for _, r := range measured {
		c, err := memoryNodes.Get(r.concept)
		if err != nil || c == nil || c.RowAuthz == nil {
			t.Fatalf("%s was measured against %s, which declares nothing", r.construct, r.concept)
		}
		if string(c.RowAuthz.Tier) != r.tier {
			t.Fatalf("%s: measured tier %q, declared tier %q", r.construct, r.tier, c.RowAuthz.Tier)
		}
		if c.RowAuthz.Tier == langparser.RowAuthzPublic && r.predicate != "" {
			t.Fatalf("%s declares public but the gate reports the predicate %q", r.concept, r.predicate)
		}
	}
}
