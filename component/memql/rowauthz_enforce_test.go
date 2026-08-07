package memql

import (
	"os"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// Row-authz ENFORCEMENT, read path (memql#3172, Phase 3 of the #2803
// ruling).
//
// Phase 2 computed the predicate a declared tier WOULD inject and threw
// it away. This phase ANDs it in. The tests here are organised around
// the four findings that got the first attempt refused at review, and
// each one names its finding.

// declaredOwnedConcept is a concept in the tree that declares
// `@rowAuthz(owner="ownerUserId")`. Used as the fixture for the owned
// tier so the tests exercise a REAL declaration rather than a
// hand-built one that could drift from what the loader produces.
const declaredOwnedConcept = "v1:notes:note"

// declaredClusterOwnerConcept declares `@rowAuthz(clusterOwner)`.
const declaredClusterOwnerConcept = "v1:telephony:call"

// probeEngine builds a DB-free engine whose function registry contains
// exactly the named probes, each a query construct BOUND to
// boundConcept with the given filter as its body.
//
// The probes are the point. Every query in the tree over a declared
// concept already carries the tier's term as a top-level conjunct
// (that is why Phase 1 declared the concept at all), so a test built
// only from tree constructs cannot tell enforcement from the author's
// own filter. A probe is a construct over a declared concept that does
// NOT carry the term -- exactly the shape a new query would arrive in.
func probeEngine(t *testing.T, boundConcept string, probes map[string]string) *MemQLEngine {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	reg := newFunctionRegistry()
	for name, filter := range probes {
		parsed, err := parseViaLangparser(filter, false)
		if err != nil {
			t.Fatalf("probe %s: parse %q: %v", name, filter, err)
		}
		fn := &Function{
			Name:         name,
			FunctionKind: "query",
			BoundConcept: boundConcept,
			Expr:         parsed.Root,
			ExprSource:   filter,
			Enabled:      true,
		}
		if err := reg.Upsert(fn); err != nil {
			t.Fatalf("probe %s: register: %v", name, err)
		}
	}
	return &MemQLEngine{initialized: true, functions: reg}
}

// declFor returns a concept's declared tier, failing the test when the
// concept does not declare one (the fixture would be vacuous).
func declFor(t *testing.T, conceptName string) *langparser.RowAuthzDecl {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	c, err := memorynodes.Get(conceptName)
	if err != nil || c == nil {
		t.Fatalf("concept %q is not loaded: %v", conceptName, err)
	}
	if c.RowAuthz == nil {
		t.Fatalf("concept %q declares no @rowAuthz -- this fixture measures nothing", conceptName)
	}
	return c.RowAuthz
}

// FINDING 1: the gate was bypassable by choice of filter spelling.
//
// The refused implementation decided WHETHER TO ENFORCE from
// extractConceptFromExpression, which returns "" for anything that is
// not a top-level `concept==<id>` equality. Three of the four spellings
// below therefore sailed past it, and `handleExecuteQuery` hands the
// engine a raw client-supplied query string.
//
// The fix is to resolve the tier from the construct's DECLARED BINDING
// (fn.BoundConcept), which does not know what the filter says. So all
// four spellings must come out enforced, and the assertion is made with
// the SHADOW ANALYZER: after enforcement the analyzer -- the same one
// the measurement runs -- must report already-implied, because the term
// is now a top-level conjunct.
func TestEnforcementIsIndependentOfFilterSpelling(t *testing.T) {
	spellings := map[string]string{
		// The control: the one spelling the refused implementation
		// enforced.
		"probeByConceptEquality": `concept=="` + declaredOwnedConcept + `"`,
		// Names a row directly. No concept equality anywhere.
		"probeById": `row.id=="` + declaredOwnedConcept + `:victim-shortid"`,
		// A top-level disjunction. An authored `filter a || b` lowers to
		// exactly this, so it is reachable from the DSL, not only from a
		// raw query string.
		"probeTopLevelOr": `concept=="` + declaredOwnedConcept + `" || status=="published"`,
		// A negated concept: matches rows of every OTHER concept, which
		// is a wider read than the control, not a narrower one.
		"probeNegatedConcept": `concept!="v1:cognition:space"`,
	}

	e := probeEngine(t, declaredOwnedConcept, spellings)
	decl := declFor(t, declaredOwnedConcept)

	for name := range spellings {
		t.Run(name, func(t *testing.T) {
			plan, err := e.parseWithFunctions(name+"()", e.functions, nil, false)
			if err != nil {
				t.Fatalf("parse %s(): %v", name, err)
			}
			verdict, reason := AnalyzeShadow(plan.Root, decl)
			if verdict != ShadowAlreadyImplied {
				t.Fatalf("the plan the engine would execute for %s() does not carry %q as a "+
					"top-level conjunct: verdict %q (%s)\n"+
					"plan root: %s\n\n"+
					"The tier must be resolved from the construct's DECLARED BINDING "+
					"(%s), never from what the filter happens to say. Deciding from "+
					"filter text is memql#3172 finding 1: id-naming, top-level-OR and "+
					"negated-concept spellings all bypassed the gate while "+
					"`concept==<id>` enforced.",
					name, InjectedPredicate(decl), verdict, reason,
					canonicalExpression(plan.Root), declaredOwnedConcept)
			}
		})
	}
}

// The clusterOwner tier behaves as declared: the plan carries the
// admin gate, not an ownership term.
func TestClusterOwnerTierInjectsTheAdminGate(t *testing.T) {
	e := probeEngine(t, declaredClusterOwnerConcept, map[string]string{
		"probeAdminRows": `concept=="` + declaredClusterOwnerConcept + `"`,
	})
	decl := declFor(t, declaredClusterOwnerConcept)
	if decl.Tier != langparser.RowAuthzClusterOwner {
		t.Fatalf("%s declares %q, wanted the clusterOwner tier for this fixture",
			declaredClusterOwnerConcept, decl.Tier)
	}

	plan, err := e.parseWithFunctions("probeAdminRows()", e.functions, nil, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if verdict, reason := AnalyzeShadow(plan.Root, decl); verdict != ShadowAlreadyImplied {
		t.Fatalf("clusterOwner plan does not carry %q: %q (%s)\nplan root: %s",
			InjectedPredicate(decl), verdict, reason, canonicalExpression(plan.Root))
	}
	if got := canonicalExpression(plan.Root); !strings.Contains(strings.ToLower(got), "isclusterowner") {
		t.Fatalf("clusterOwner plan root has no admin gate in it: %s", got)
	}
}

// An UNDECLARED concept is untouched. Enforcement covers the declared
// population only; #3173 is the task that grows that population, and
// enforcing a tier nobody declared would be inventing an authorization
// decision rather than applying one.
func TestUndeclaredConceptIsNotEnforced(t *testing.T) {
	const undeclared = "v1:identity:user"
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	c, err := memorynodes.Get(undeclared)
	if err != nil || c == nil {
		t.Fatalf("concept %q is not loaded: %v", undeclared, err)
	}
	if c.RowAuthz != nil {
		t.Skipf("%s now declares a tier; pick another undeclared fixture", undeclared)
	}

	filter := `concept=="` + undeclared + `"`
	e := probeEngine(t, undeclared, map[string]string{"probeUndeclared": filter})
	plan, err := e.parseWithFunctions("probeUndeclared()", e.functions, nil, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e.planReferencesActor(plan.Root) {
		t.Fatalf("an undeclared concept's plan grew an actor term: %s",
			canonicalExpression(plan.Root))
	}
}

// FINDING 2: the result cache served caller A's rows to caller B.
//
// `planReferencesActor(plan.Root)` decides whether the cache key folds
// caller identity. The refused implementation injected into a local and
// never mutated plan.Root, so an enforced query kept one shared key
// across callers -- with caching default-on, a 60s TTL and a
// `v1:identity:`-only denylist. On main the same query returns every row
// to everyone, so the cache was consistent: enforcement is what created
// the divergence.
//
// This asserts the precondition at the plan level: an enforced plan
// depends on the actor, so the key must fold. The two-caller end-to-end
// proof is in rowauthz_enforce_db_test.go.
func TestEnforcedPlanMakesTheCacheKeyActorDependent(t *testing.T) {
	e := probeEngine(t, declaredOwnedConcept, map[string]string{
		"probeUnscoped": `concept=="` + declaredOwnedConcept + `"`,
	})
	plan, err := e.parseWithFunctions("probeUnscoped()", e.functions, nil, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !e.planReferencesActor(plan.Root) {
		t.Fatalf("the enforced plan does not read as actor-dependent, so the result cache "+
			"keys it once for every caller and serves one caller's rows to another "+
			"(memql#3172 finding 2).\nplan root: %s", canonicalExpression(plan.Root))
	}
}

// THE WIRING GUARDS.
//
// The gates above are unit-testable in isolation, and isolation is
// exactly how a gate ends up correct and uncalled. These read the
// executor's source and assert the two read paths still invoke them --
// the same technique, and for the same reason, as
// TestShadowHookRunsBeforeActorResolution next door: the failure being
// guarded against is silent and total.

func executorFuncBody(t *testing.T, signature string) string {
	t.Helper()
	src, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatalf("read executor.go: %v", err)
	}
	text := string(src)
	start := strings.Index(text, signature)
	if start < 0 {
		t.Fatalf("%q not found in executor.go -- this guard needs re-aiming", signature)
	}
	end := strings.Index(text[start+1:], "\nfunc ")
	if end < 0 {
		end = len(text) - start - 1
	}
	return text[start : start+1+end]
}

// The FILTERED read path applies the per-row gate. This is what covers
// a raw client-supplied query string, which has no declared binding for
// the plan-level injection to resolve a tier from.
func TestFilteredReadPathAppliesTheRowGate(t *testing.T) {
	body := executorFuncBody(t, "func (e *MemQLEngine) evaluateExpressionSet(")
	if !strings.Contains(body, "filterRowAuthzSet(") {
		t.Fatal("evaluateExpressionSet no longer applies filterRowAuthzSet. That is the seam every " +
			"filter-bearing read passes through, and the only enforcement a read with no declared " +
			"binding gets -- a raw query string reaches rows of a declared concept without it " +
			"(memql#3172 finding 1).")
	}
}

// GRAPH EXPANSION applies the traversal gate, BEFORE the row is added
// to the bundle and BEFORE the depth guard.
//
// Placement is the whole assertion. After addNode the row is already in
// the caller's result; after the depth guard the gate only ever sees
// the root row, which the filter path already covered, and never a row
// traversal walked INTO -- the only rows this gate exists for.
func TestGraphExpansionAppliesTheTraversalGateBeforeItEmitsTheRow(t *testing.T) {
	body := executorFuncBody(t, "func (e *MemQLEngine) expandGraph(")

	gate := strings.Index(body, "admitRowAuthzTraversal(")
	add := strings.Index(body, "builder.addNode(")
	depthGuard := strings.Index(body, "if depth == 0 {")

	if gate < 0 {
		t.Fatal("expandGraph no longer applies admitRowAuthzTraversal -- graph expansion reaches " +
			"rows through relationship definitions with no filter at all, so nothing else " +
			"enforces there (memql#3172, the graph-expansion half of the DoD; #2803 design " +
			"decision 3 names this as the path that gets missed).")
	}
	if add < 0 || depthGuard < 0 {
		t.Fatal("expandGraph's addNode / depth guard not found -- this guard needs re-aiming")
	}
	if gate > add {
		t.Fatal("the traversal gate now runs AFTER builder.addNode: the row is already in the " +
			"caller's bundle by the time it is checked.")
	}
	if gate > depthGuard {
		t.Fatal("the traversal gate now runs after the `depth == 0` return. The default depth is " +
			"1, so children are visited with depth 0 and return before reaching it -- the gate " +
			"would only ever see the root row.")
	}
}

// The plan path enforces BEFORE the bare-payload rewrite, or the
// injected term keeps a bare field reference no compile stage resolves.
func TestPlanEnforcementRunsBeforeTheBarePayloadRewrite(t *testing.T) {
	src, err := os.ReadFile("parser.go")
	if err != nil {
		t.Fatalf("read parser.go: %v", err)
	}
	text := string(src)
	enforce := strings.Index(text, "enforceRowAuthzOnPlan(plan)")
	rewrite := strings.Index(text, "rewriteFilterFieldRefs(plan.Root)")
	if enforce < 0 {
		t.Fatal("the plan path no longer calls enforceRowAuthzOnPlan -- no declared tier is ANDed " +
			"into any read")
	}
	if rewrite < 0 {
		t.Fatal("rewriteFilterFieldRefs not found in parser.go -- this guard needs re-aiming")
	}
	if enforce > rewrite {
		t.Fatal("enforcement now runs AFTER the bare-payload rewrite. The tier renders " +
			"`ownerUserId==actor.userId`; it is that rewrite which turns the bare property into " +
			"the `payload.ownerUserId` the SQL pushdown consumes, so an injected term that " +
			"misses it resolves to nothing.")
	}
}
