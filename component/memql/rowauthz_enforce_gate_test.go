package memql

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslgate"
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
	// rankTier records that the concept declares a rank branch whose
	// injected term has no author spelling (epic memql#4832).
	rankTier bool
	// callerScoped records that the filter carries a top-level conjunct
	// that scopes the read to the caller -- the property this gate keeps
	// checking when implication stops being decidable.
	callerScoped bool
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
			construct:    fn.Name,
			concept:      bound,
			tier:         string(concept.RowAuthz.Tier),
			predicate:    InjectedPredicate(concept.RowAuthz),
			verdict:      verdict,
			reason:       reason,
			rankTier:     concept.RowAuthz.RankVisible,
			callerScoped: filterIsCallerScoped(fn.Expr, concept.RowAuthz),
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

// tierDecidesTheRead names the constructs whose filter deliberately carries NO
// caller-scope conjunct because the concept's tier is the whole answer.
//
// AN ADJUDICATION, WHICH IS WHAT THIS GATE ASKS FOR ON A FAILURE ("make the
// broader read deliberate"), and it is not a widening: the entries below are
// checked for a positive property no other construct in the tree has, and a
// stale entry is an error.
//
// THE PROPERTY. Both constructs declare `@requiresRank("admin")`, so their
// CALLER SET is admin and above. Their filter used to carry
// `(ownerUserId==actor.userId || actor.isClusterOwner==true ||
// requiresDeveloperOrAbove)`, and for every caller that annotation admits the
// third arm was TRUE -- so the disjunction was a pass-through and the tier
// (`owner="ownerUserId", rankVisible, unowned="admin", clusterOwner`) decided
// the row set by itself. Epic memql#5166 deleted the spec that third arm named,
// and keeping the other two arms would have been a REGRESSION rather than a
// conservative choice: `own || clusterOwner` is false for every row an admin
// does not own, so an admin would have stopped seeing the client list they
// manage.
//
// WHAT THE GATE CANNOT SEE, and why the entry is here rather than in the
// analyzer: `@requiresRank` bounds who may CALL, which is not a filter term, so
// no amount of reading the filter recovers the argument above. The gate's
// structural complaint is accurate and its behavioural conclusion -- "this
// construct's result set CHANGES for its callers" -- is false for these two.
//
// A FURTHER ENTRY IS A DESIGN DECISION, not a fix. The right question for a new
// candidate is whether every caller the construct admits is one for whom the
// tier ALREADY decides the whole row set; if the answer needs a paragraph about
// what a particular caller sees, the answer is no.
var tierDecidesTheRead = map[string]string{
	"clientAccountsAll": "epic memql#5166. @requiresRank(\"admin\") bounds the callers to admin " +
		"and above, for whom the deleted requiresDeveloperOrAbove arm was always true -- so the " +
		"disjunction was a pass-through and the concept's tier already decided the row set. " +
		"Keeping `own || clusterOwner` alone would have hidden every client an admin does not " +
		"personally own.",
	"clientAccountById": "epic memql#5166, as clientAccountsAll -- the same filter, the same " +
		"annotation, the same tier.",

	// The five group reads (epic memql#5165), and the argument is STRONGER
	// here than for the two above rather than merely analogous.
	//
	// v1:identity:group and v1:identity:groupMembership carry an ownerUserId
	// that is ALWAYS EMPTY -- they are the deployment's rows, not a
	// principal's (D2). So the tier's owner arm matches nobody at all, and
	// `unowned="admin"` is the entire branch: it admits exactly admin and
	// above, which is exactly the caller set `@requiresRank("admin")` already
	// bounds. The tier does not merely decide the row set for these callers,
	// it decides it identically for all of them.
	//
	// WHAT A CONJUNCT WOULD COST. `own || clusterOwner` is false for every
	// admin these reads exist to serve, because nobody owns the rows -- so
	// adding one is a REGRESSION, not a tightening, and it would empty the
	// Users app's Groups section for every caller but a cluster owner. The
	// slug specs that could have spelled a third arm are deleted by
	// memql#5166, for reasons that apply here too.
	//
	// THIS IS THE THIRD ENTRY the note above calls a design decision, and the
	// test it asks for is the one that would fail if the reasoning were wrong:
	// TestGroupQueriesAnswerForTheSystemActorAndRefuseBelowTheFloor drives all
	// five against a real database and asserts BOTH halves -- rows for the
	// caller set the annotation admits, and none for a writer.
	"groupsAll":        "epic memql#5165. ownerUserId is always empty on this concept, so the tier's owner arm matches nobody and unowned=\"admin\" admits exactly the caller set @requiresRank(\"admin\") already bounds. A conjunct would be false for every admin the read serves.",
	"groupById":        "epic memql#5165, as groupsAll -- the same tier, the same annotation, the same always-empty owner.",
	"groupsForAccount": "epic memql#5165, as groupsAll.",
	"membersOfGroup":   "epic memql#5165, as groupsAll, over v1:identity:groupMembership -- whose ownerUserId is empty for the sharper reason that the natural owner field would be `userId`, and an owned row admits its owner's inserts.",
	"groupsForUser":    "epic memql#5165, as membersOfGroup.",

	// The four benchmark reads (memql#5216), and the property holds in its
	// STRONGEST form here -- the tier decides the row set IDENTICALLY for
	// every caller the annotation admits, with no owner arm to reason about
	// at all.
	//
	// v1:bench:run and v1:bench:sample have NO owner field: "a benchmark is a
	// fact about the DEPLOYMENT rather than about a person", in the concept's
	// own words. The tier is `clusterOwner, rankFloor="admin"`, so its two
	// arms are "is a cluster owner" and "ranks admin or above" -- and
	// `@requiresRank("admin")` bounds the caller set to exactly the second.
	// Every admitted caller therefore clears the tier for every row.
	//
	// WHAT A CONJUNCT WOULD COST, which is what makes this a design decision
	// rather than a formality: these four USED to carry
	// `actor.isClusterOwner==true`, and that is the bug memql#5216 is filed
	// for. It is false for every admin the OS's Benchmarks section admits, so
	// the screen served them a full set of figures rendered as UNMEASURED --
	// a refusal wearing the costume of a measurement, on the surface built to
	// make an absence legible. Re-adding the conjunct restores that.
	//
	// The test the note above asks for -- the one that fails if this reasoning
	// is wrong -- is TestBenchReadsAnswerForAdminAndRefuseBelowTheFloor, which
	// asserts BOTH halves against the real tier.
	"benchRuns": "memql#5216. The concept has no owner field, so the tier's arms are " +
		"clusterOwner OR rankFloor=\"admin\" -- and @requiresRank(\"admin\") bounds the callers " +
		"to exactly the second, so the tier decides every row for every admitted caller. The " +
		"actor.isClusterOwner conjunct this used to carry is the defect the issue names: false " +
		"for every admin the Benchmarks section admits.",
	"benchRunById":          "memql#5216, as benchRuns -- the same concept, tier and annotation.",
	"benchSamplesForRun":    "memql#5216, as benchRuns, over v1:bench:sample, which is ownerless for the same reason.",
	"benchSamplesForMetric": "memql#5216, as benchSamplesForRun.",

	// The two grant reads (epic memql#5294), the bench argument exactly:
	// v1:rbac:grant has no owner field, its tier's arms are clusterOwner OR
	// rankFloor="admin", and @requiresRank("admin") bounds the callers to
	// exactly the second -- so the tier decides every row for every admitted
	// caller. The test that fails if this reasoning is wrong is
	// TestGrantReadsAnswerForAdminAndRefuseBelowTheFloor
	// (rbac_grant_enforcement_db_test.go), which asserts BOTH halves against
	// the real tier.
	"grantsForSubject":  "memql#5294, as benchRuns: an ownerless clusterOwner-tier concept read at exactly its rankFloor.",
	"grantsForResource": "memql#5294, as grantsForSubject -- the same concept, tier and annotation.",
	"grantById":         "memql#5297, as grantsForSubject -- the same concept, tier and annotation; what grantRevoke reads before it acts.",
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

	adjudicated := map[string]bool{}
	for _, r := range measured {
		if r.verdict == ShadowAlreadyImplied {
			continue
		}
		if _, ok := tierDecidesTheRead[r.construct]; ok {
			adjudicated[r.construct] = true
			continue
		}
		// A RANK-DECLARING TIER IS NOT DECIDABLE BY INSPECTION, and this is
		// the same shape the composite arm in AnalyzeShadow records one level
		// down (memql#4312): without an arm for it, the tier could be
		// DECLARED and then no construct over it could be AUTHORED.
		//
		// The injected predicate for `rankVisible` is
		// `owner || clusterOwner || <rank membership>`, and the rank arm has
		// NO AUTHOR SPELLING -- it is a symbolic node resolved per request
		// against the principal table, deliberately, because a resolved list
		// of user ids in a cached plan would be one caller's answer served to
		// the next. So "does this filter imply the injected term" is a
		// question with no static answer, and this file's doctrine is to
		// report undecidable rather than a confident wrong one.
		//
		// THE EXEMPTION IS NOT A HOLE, because it is replaced by a stricter
		// positive requirement: the construct must still carry a top-level
		// CALLER-SCOPE conjunct. What the gate stops being able to check is
		// whether injection changes the result set -- which for these tiers
		// it certainly does, that being the entire feature. What it keeps
		// checking is that the read is scoped to the caller at all, which is
		// the property that actually protects rows.
		if r.rankTier && r.verdict == ShadowUndecidable {
			if !r.callerScoped {
				t.Errorf("%s over %s declares a rank tier and its filter carries no top-level "+
					"caller-scope conjunct.\n"+
					"A rank tier's injected term cannot be decided by "+
					"inspection, so this gate checks the property it still can: the read must be "+
					"scoped to the caller. Add the tier's own composite term as a conjunct.",
					r.construct, r.concept)
			}
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

	// A STALE ADJUDICATION IS WORSE THAN A MISSING ONE: it reports that a
	// broader read was considered and accepted, for a construct that has since
	// been scoped, renamed or deleted -- so the next reader trusts a line that
	// measures nothing. The same rule callerArgSelectionExemptions follows.
	for construct, reason := range tierDecidesTheRead {
		if !adjudicated[construct] {
			t.Errorf("tierDecidesTheRead names %q (%s), but the gate did not flag it.\n"+
				"Either the construct now carries a caller-scope conjunct -- in which case remove "+
				"the entry -- or it no longer exists under that name.", construct, reason)
		}
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

// filterIsCallerScoped reports whether a filter carries a TOP-LEVEL conjunct
// that scopes the read to the caller.
//
// It is deliberately weaker than AnalyzeShadow's implication check and answers
// a different question: not "does injecting change the result set" but "is
// this read scoped to whoever is asking at all". That is the property a rank
// tier still lets this gate verify, and it is the one that protects rows.
//
// A conjunct qualifies when it is an owner-scope leaf, a cluster-owner leaf, a
// recognised actor gate, or a DISJUNCTION whose every arm is one of those --
// which is the shape the composite tier's own predicate takes, and therefore
// the shape every construct over such a concept is already written in.
func filterIsCallerScoped(expr ExpressionNode, decl *langparser.RowAuthzDecl) bool {
	conjuncts, ok := topLevelConjunctsOf(unwrapToFilter(expr))
	if !ok {
		return false
	}
	for _, c := range conjuncts {
		if callerScopeNode(c, decl) {
			return true
		}
	}
	return false
}

func callerScopeNode(node ExpressionNode, decl *langparser.RowAuthzDecl) bool {
	switch n := node.(type) {
	case *LogicalExpression:
		if n.Op != LogicalOr {
			// A conjunction inside a conjunct: either half scoping the read
			// scopes it, the same way a top-level conjunct does.
			return callerScopeNode(n.Left, decl) || callerScopeNode(n.Right, decl)
		}
		// A DISJUNCTION scopes the read only when EVERY arm does -- one
		// unscoped arm returns rows the others would have excluded, which is
		// the fail-open memql#2839 closed.
		return callerScopeNode(n.Left, decl) && callerScopeNode(n.Right, decl)
	case *SpecReferenceExpression:
		// An actor gate, recognised by the SAME regex dslgate uses, so this
		// gate and the composition rule cannot disagree about what counts as
		// one. A gate neither knows is not a gate to either.
		return dslgate.AdminGateRe.MatchString(" " + n.Name + " ")
	default:
		return isOwnerScopeLeaf(node, decl.Owner) || isClusterOwnerLeaf(node)
	}
}
