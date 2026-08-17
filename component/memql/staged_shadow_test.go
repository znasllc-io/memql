package memql

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// THE MEASUREMENT for staged-data shadow mode (memql#3981, epic #3974).
//
// An ordinary test over the loaded registry -- the same phase-2 shape that
// produced the evidence memql#3172's enforcement ruling was taken against
// (rowauthz_enforce_gate_test.go re-derives that one at build time).
//
// IT MEASURES, IT DOES NOT GATE, and the difference is the phase. A
// would-hide entry is the mechanism WORKING; an undecidable entry is a read
// the predicate cannot be placed on, which is a finding for the enforcement
// ruling rather than a build break to be cleared. Failing the build on
// either today would be phase 3 arriving before the ruling that authorises
// it. What DOES fail here is the measurement being vacuous or blind --
// see TestStagedDataShadowIsNotBlind and the guards at the foot of the
// report -- because a measurement that measures nothing passes forever, and
// so does one that cannot tell two different constructs apart.
//
// Nothing in this file injects anything. It calls a pure analyzer over the
// loaded tree; no engine is started and no plan is rewritten.

// stagedDataTreeRegistry loads the real .memql tree: concepts, then the
// function family, then BUILTINS.
//
// The builtins are loaded deliberately and are not a detail. A builtin is a
// construct whose read runs inside a Go executor, so it binds no concept and
// reaches storage without passing a plan -- precisely the population this
// measurement exists to name. A registry loaded without them would report
// full coverage of a tree it had only looked at half of.
func stagedDataTreeRegistry(t *testing.T) *FunctionRegistry {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(nil, registry, memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	if _, err := LoadUnifiedBuiltins(nil, registry); err != nil {
		t.Fatalf("LoadUnifiedBuiltins: %v", err)
	}
	return registry
}

// measureStagedDataShadow runs the analyzer over every construct in the
// registry, hypothesising that the concept each one BINDS is staged.
//
// "Hypothesise your own binding staged" is the universal form of the
// question. Any single concept would answer for one construct and say
// nothing about the rest; asking it of every construct's own binding gives
// the mechanism's blast radius across the whole tree in one pass, and it is
// the only form that needs no live lifecycle state at all.
func measureStagedDataShadow(registry *FunctionRegistry) []StagedDataShadowRecord {
	var out []StagedDataShadowRecord
	for _, fn := range registry.List() {
		if fn == nil {
			continue
		}
		binding := &StagedDataBinding{Concept: strings.TrimSpace(fn.BoundConcept), Staged: true}
		verdict, reason := AnalyzeStagedDataShadow(fn.FunctionKind, fn.Expr, binding)
		out = append(out, StagedDataShadowRecord{
			Construct: fn.Name,
			Kind:      strings.ToLower(strings.TrimSpace(fn.FunctionKind)),
			Concept:   binding.Concept,
			Predicate: StagedDataPredicate(binding),
			Verdict:   verdict,
			Reason:    reason,
			Origin:    fn.Origin,
			Traverses: StagedDataTraverses(fn.Expr),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Verdict != out[j].Verdict {
			return out[i].Verdict < out[j].Verdict
		}
		if out[i].Concept != out[j].Concept {
			return out[i].Concept < out[j].Concept
		}
		return out[i].Construct < out[j].Construct
	})
	return out
}

var stagedDataVerdictOrder = []StagedDataVerdict{
	StagedDataWouldHide,
	StagedDataNoChange,
	StagedDataNotAReadPath,
	StagedDataUndecidable,
}

func TestStagedDataShadowMeasuresTheTree(t *testing.T) {
	registry := stagedDataTreeRegistry(t)
	measured := measureStagedDataShadow(registry)

	counts := map[StagedDataVerdict]int{}
	byKind := map[string]map[StagedDataVerdict]int{}
	undecidableReasons := map[string]int{}
	traversing := 0
	for _, r := range measured {
		counts[r.verdictKey()]++
		if byKind[r.Kind] == nil {
			byKind[r.Kind] = map[StagedDataVerdict]int{}
		}
		byKind[r.Kind][r.Verdict]++
		if r.Verdict == StagedDataUndecidable {
			undecidableReasons[stagedDataReasonClass(r.Reason)]++
		}
		if r.Traverses {
			traversing++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\n=== staged-data shadow mode, over this tree (memql#3981) ===\n")
	fmt.Fprintf(&b, "hypothesis: for each construct, its OWN declared binding is staged.\n")
	fmt.Fprintf(&b, "nothing is injected; this is a pure analysis of the loaded registry.\n\n")
	fmt.Fprintf(&b, "constructs measured                %d\n\n", len(measured))
	fmt.Fprintf(&b, "verdicts:\n")
	for _, v := range stagedDataVerdictOrder {
		fmt.Fprintf(&b, "  %-16s %4d\n", v, counts[v])
	}
	fmt.Fprintf(&b, "\nby construct kind:\n")
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	fmt.Fprintf(&b, "  %-10s %-16s %-16s %-16s %-16s\n", "kind", string(StagedDataWouldHide),
		string(StagedDataNoChange), string(StagedDataNotAReadPath), string(StagedDataUndecidable))
	for _, k := range kinds {
		fmt.Fprintf(&b, "  %-10s %-16d %-16d %-16d %-16d\n", k,
			byKind[k][StagedDataWouldHide], byKind[k][StagedDataNoChange],
			byKind[k][StagedDataNotAReadPath], byKind[k][StagedDataUndecidable])
	}
	fmt.Fprintf(&b, "\nundecidable, by why the binding is absent:\n")
	reasons := make([]string, 0, len(undecidableReasons))
	for r := range undecidableReasons {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		fmt.Fprintf(&b, "  %-46s %4d\n", r, undecidableReasons[r])
	}
	fmt.Fprintf(&b, "\nalso reach rows by GRAPH EXPANSION (no filter to inject into,\n")
	fmt.Fprintf(&b, "so the row gate is their only mechanism): %d\n", traversing)

	// REPORT-SIDE CLASSIFICATION of the unexpanded calls.
	//
	// The analyzer refuses to resolve a callee, deliberately -- that would
	// make it a reader of loaded state. The REPORT is not the analyzer and
	// already holds the registry, so it can say which hole each unexpanded
	// call actually is, which is the difference between "13 calls" and
	// "3 of them are the memql#3982 shape". Done here rather than in the
	// analyzer so the split stays visible: one of these two things is a
	// pure function and the other is a lookup.
	callClass := map[string]int{}
	var toBuiltin []string
	for _, r := range measured {
		callee, ok := stagedDataCalleeOf(r.Reason)
		if !ok {
			continue
		}
		target, err := registry.Get(callee)
		switch {
		case err != nil || target == nil:
			callClass["names no registry construct (a step reference or a scalar helper)"]++
		case strings.EqualFold(strings.TrimSpace(target.FunctionKind), FunctionTypeBuiltin):
			callClass["resolves to a BUILTIN -> a bare BuiltinFunctionExpression at plan.Root (memql#3982)"]++
			toBuiltin = append(toBuiltin, r.Construct+" -> "+callee)
		default:
			callClass["resolves to a "+strings.ToLower(strings.TrimSpace(target.FunctionKind))+
				", which carries its own binding, so the read it performs IS covered"]++
		}
	}
	if len(callClass) > 0 {
		fmt.Fprintf(&b, "\nof the unexpanded calls, what the callee turns out to be\n")
		fmt.Fprintf(&b, "(report-side registry lookup -- the analyzer does not do this):\n")
		classes := make([]string, 0, len(callClass))
		for c := range callClass {
			classes = append(classes, c)
		}
		sort.Strings(classes)
		for _, c := range classes {
			fmt.Fprintf(&b, "  %4d  %s\n", callClass[c], c)
		}
		sort.Strings(toBuiltin)
		for _, entry := range toBuiltin {
			fmt.Fprintf(&b, "        [3982] %s\n", entry)
		}
	}
	t.Log(b.String())

	// THE DELIVERABLE IS THE LIST, and the number is a summary of it. The
	// undecidable bucket is printed in FULL and first: each entry is a read
	// a statically-placed predicate cannot cover, and which of them matter
	// cannot be read off a count.
	for _, v := range []StagedDataVerdict{StagedDataUndecidable, StagedDataWouldHide, StagedDataNoChange, StagedDataNotAReadPath} {
		var list strings.Builder
		fmt.Fprintf(&list, "\n--- %s (%d) ---\n", v, counts[v])
		for _, r := range measured {
			if r.Verdict != v {
				continue
			}
			flag := ""
			if r.Traverses {
				flag = "  [traverses]"
			}
			switch v {
			case StagedDataUndecidable:
				fmt.Fprintf(&list, "  %-8s %-44s %s%s\n", r.Kind, r.Construct, r.Reason, flag)
			default:
				fmt.Fprintf(&list, "  %-8s %-44s %s%s\n", r.Kind, r.Construct, r.Concept, flag)
			}
		}
		t.Log(list.String())
	}

	// VACUOUS-PASS GUARDS.
	//
	// The first is the one rowauthz_enforce_gate_test.go states: a gate
	// that measures nothing passes forever. The rest are this measurement's
	// own version of the same failure, and they are per-BUCKET because the
	// buckets can empty independently and each emptying is a different lie.
	// An empty would-hide would mean the mechanism reaches nothing; an
	// empty undecidable would claim total coverage, which is the specific
	// false reassurance this whole task exists to refuse.
	if len(measured) == 0 {
		t.Fatal("measured nothing -- the tree failed to load, and a measurement that measures " +
			"nothing passes forever")
	}
	for _, v := range []StagedDataVerdict{StagedDataWouldHide, StagedDataNotAReadPath, StagedDataUndecidable} {
		if counts[v] == 0 {
			t.Fatalf("the %q bucket is empty over the whole tree. Either the loader stopped "+
				"producing that shape of construct or the analyzer stopped recognising it; "+
				"an empty %q makes every other number in this report a claim about a tree "+
				"nobody looked at", v, v)
		}
	}
	// Every verdict must be one of the four. Guards a future fifth constant
	// being added without the report learning to print it -- which would
	// silently drop constructs out of the deliverable.
	for _, r := range measured {
		if r.verdictKey() == "" {
			t.Fatalf("%s produced the unknown verdict %q", r.Construct, r.Verdict)
		}
	}
}

// verdictKey returns the record's verdict when it is one this report knows
// how to print, and "" otherwise.
func (r StagedDataShadowRecord) verdictKey() StagedDataVerdict {
	for _, v := range stagedDataVerdictOrder {
		if r.Verdict == v {
			return v
		}
	}
	return ""
}

// stagedDataCalleeOf recovers the callee name from an unexpanded-call
// reason, or reports false when the reason is not one.
//
// Parsing the analyzer's own prose is a seam worth naming rather than
// hiding: the analyzer refuses to resolve a callee (that would make it a
// reader of loaded state), so it carries the NAME instead, and this is the
// report picking it back up. The alternative -- a structured field on the
// record -- would put the callee on every record including the ones that
// have none, and would tempt a later edit to fill it by lookup inside the
// analyzer, which is the property being protected.
func stagedDataCalleeOf(reason string) (string, bool) {
	const prefix = `the read expression is a call to "`
	idx := strings.Index(reason, prefix)
	if idx < 0 {
		return "", false
	}
	rest := reason[idx+len(prefix):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// stagedDataReasonClass collapses a per-construct reason to the class of
// hole it names, so the report can count classes without losing the
// per-construct detail the full list carries.
func stagedDataReasonClass(reason string) string {
	switch {
	case strings.Contains(reason, "builtin: the read runs inside a Go executor"):
		return "builtin (Go executor issues its own SQL)"
	case strings.Contains(reason, "top-level builtin call"):
		return "top-level builtin at plan.Root (memql#3982)"
	case strings.Contains(reason, "is a call to"):
		return "unexpanded function call"
	case strings.Contains(reason, "is the spec"):
		return "unexpanded spec reference"
	case strings.Contains(reason, "collection method"):
		return "collection method over an unexpanded receiver"
	case strings.Contains(reason, "relationship traversal"):
		return "relationship traversal"
	case strings.Contains(reason, "neither a declared binding nor a read expression"):
		return "no binding and no read expression"
	default:
		return "other expression shape carrying no binding"
	}
}

// TestStagedDataShadowIsNotBlind proves the analyzer distinguishes states
// it must distinguish, by MUTATING REAL CONSTRUCTS out of the loaded tree
// and asserting the verdict moves.
//
// Both halves matter, and they fail in opposite directions:
//
//   - Half one removes a binding. If the verdict did not move, the
//     analyzer would report a read the predicate cannot be placed on as
//     covered -- reporting a leak as safe, which is the failure that
//     matters here.
//   - Half two adds an exclusion the filter did not have. If the verdict
//     did not move, the no-change arm would be dead code that no future
//     reader could tell from working code.
//
// Modelled on TestGateCatchesAStrippedOwnershipConjunct, which mutates a
// real construct for the same reason: a hand-assembled fixture proves the
// analyzer works on shapes the loader may never produce.
func TestStagedDataShadowIsNotBlind(t *testing.T) {
	registry := stagedDataTreeRegistry(t)

	// Pick the first real BOUND query, in name order, so the victim is
	// deterministic and is a construct that actually ships.
	var victim *Function
	candidates := registry.List()
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	for _, fn := range candidates {
		if fn == nil || !strings.EqualFold(fn.FunctionKind, "query") {
			continue
		}
		if strings.TrimSpace(fn.BoundConcept) == "" || fn.Expr == nil {
			continue
		}
		victim = fn
		break
	}
	if victim == nil {
		t.Fatal("no bound query construct in the loaded tree to mutate")
	}
	bound := strings.TrimSpace(victim.BoundConcept)

	// BASELINE: the unmutated construct.
	base, baseReason := AnalyzeStagedDataShadow(victim.FunctionKind, victim.Expr,
		&StagedDataBinding{Concept: bound, Staged: true})
	if base != StagedDataWouldHide {
		t.Fatalf("%s over %s reads as %q before any mutation (%s); this test needs a would-hide baseline",
			victim.Name, bound, base, baseReason)
	}
	t.Logf("baseline: %s over %s -> %s", victim.Name, bound, base)

	// HALF ONE -- take the binding away, the way a logic or a builtin
	// arrives with none.
	unbound, unboundReason := AnalyzeStagedDataShadow(victim.FunctionKind, victim.Expr,
		&StagedDataBinding{Concept: "", Staged: true})
	if unbound != StagedDataUndecidable {
		t.Fatalf("%s still reads as %q with its binding removed -- the analyzer cannot see the "+
			"difference between a read the predicate can be placed on and one it cannot, which "+
			"is the only distinction this measurement makes", victim.Name, unbound)
	}
	t.Logf("binding removed: %s -> %s (%s)", victim.Name, unbound, unboundReason)

	// HALF TWO -- AND the exclusion onto the REAL filter. Built from
	// StagedDataPredicate's own concept so the matcher and the renderer are
	// tested against each other rather than against a hand-typed string:
	// a renderer and a matcher that disagree about the predicate they both
	// exist to describe is the memql#3029 near-miss rowauthz_shadow.go
	// records at length.
	excluded := &LogicalExpression{
		Op:   LogicalAnd,
		Left: victim.Expr,
		Right: &ComparisonExpression{
			Field:    FieldReference{Raw: "row.concept", Parts: []string{"row", "concept"}},
			Operator: OpNe,
			Value:    bound,
		},
	}
	got, reason := AnalyzeStagedDataShadow(victim.FunctionKind, excluded,
		&StagedDataBinding{Concept: bound, Staged: true})
	if got != StagedDataNoChange {
		t.Fatalf("%s reads as %q with `row.concept!=%q` ANDed onto its filter (%s) -- the "+
			"no-change arm never fires, so it is unfalsifiable",
			victim.Name, got, bound, reason)
	}
	t.Logf("exclusion added: %s -> %s (%s)", victim.Name, got, reason)

	// HALF TWO, bare spelling. The loader builds the concept intrinsic as a
	// single bare part; an authored filter spells it `row.concept`. Both
	// must match or the analyzer measures half the tree it walks.
	bareExcluded := &LogicalExpression{
		Op:   LogicalAnd,
		Left: victim.Expr,
		Right: &ComparisonExpression{
			Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
			Operator: OpNe,
			Value:    bound,
		},
	}
	if got, _ := AnalyzeStagedDataShadow(victim.FunctionKind, bareExcluded,
		&StagedDataBinding{Concept: bound, Staged: true}); got != StagedDataNoChange {
		t.Fatalf("the bare `concept!=` spelling reads as %q; the loader builds exactly that "+
			"shape (ensureBoundConceptFilter), so the analyzer would miss it in the tree", got)
	}

	// AND THE HYPOTHESIS ITSELF MOVES THE VERDICT. Without this the whole
	// measurement could be a constant that never consulted its input.
	if got, _ := AnalyzeStagedDataShadow(victim.FunctionKind, victim.Expr,
		&StagedDataBinding{Concept: bound, Staged: false}); got != StagedDataNoChange {
		t.Fatalf("%s reads as %q with the concept NOT staged -- the analyzer ignores its own "+
			"hypothesis, so every verdict it reports is a constant", victim.Name, got)
	}
}

// The measurement must resolve concepts from the DECLARED BINDING, never
// from filter text -- memql#3172 finding 1, which the row-authz land gate
// carries its own version of. A measurement reading filter text would go
// quiet on exactly the spellings the injection seam covers
// (extractConceptFromExpression answers "" for a top-level `||`, a
// negated concept, and a read named by id).
func TestStagedDataShadowResolvesConceptsFromTheDeclaredBinding(t *testing.T) {
	registry := stagedDataTreeRegistry(t)
	measured := measureStagedDataShadow(registry)
	if len(measured) == 0 {
		t.Fatal("measured nothing")
	}

	byFilterText := 0
	bound := 0
	for _, r := range measured {
		fn, err := registry.Get(r.Construct)
		if err != nil || fn == nil {
			t.Fatalf("construct %s vanished from the registry", r.Construct)
		}
		if strings.TrimSpace(fn.BoundConcept) != r.Concept {
			t.Fatalf("%s was measured against %q but declares %q",
				r.Construct, r.Concept, fn.BoundConcept)
		}
		if r.Concept == "" {
			continue
		}
		bound++
		if extractConceptFromExpression(fn.Expr) == r.Concept {
			byFilterText++
		}
	}
	t.Logf("%d of %d bound constructs would ALSO have been found by filter text; the remaining "+
		"%d are measured only because the binding is the source of truth",
		byFilterText, bound, bound-byFilterText)
}

// The predicate is a CONSTANT. This is the #3976 audience ruling made
// checkable: a staged row is visible to nobody on the ordinary read path,
// so the term names no caller -- which is what lets the injection seam stay
// context-free and keeps enforceRowAuthzOnPlan's three recorded
// justifications standing (rowauthz_enforce.go:134-143,
// rowauthz_pii_unbound.go:37 and :89).
//
// If this test ever fails, the fix is NOT to widen it. An identity in this
// predicate means threading an actor through every read seam including the
// ~55 hand-rolled SQL sites of memql#3984, each of which then gets its own
// chance to spell the term wrong -- which is the class of mistake
// refuseRowAuthzWithoutActor exists because somebody made once, on the one
// path that IS centralised.
func TestStagedDataPredicateIsCallerIndependent(t *testing.T) {
	rendered := StagedDataPredicate(&StagedDataBinding{Concept: "v1:probe:widget", Staged: true})
	if rendered == "" {
		t.Fatal("a staged binding rendered no predicate")
	}
	for _, forbidden := range []string{"actor", "caller", "userId", "isClusterOwner", "identityId"} {
		if strings.Contains(strings.ToLower(rendered), strings.ToLower(forbidden)) {
			t.Fatalf("the staged predicate %q names %q. Staged visibility is caller-independent "+
				"by ruling (memql#3976); an identity here reverses that ruling and requires an "+
				"actor at every read seam", rendered, forbidden)
		}
	}

	// The two ways there is no predicate, stated explicitly so a future
	// edit cannot quietly make an unstaged concept inject something.
	if got := StagedDataPredicate(&StagedDataBinding{Concept: "v1:probe:widget", Staged: false}); got != "" {
		t.Fatalf("an unstaged concept rendered %q; it must render nothing", got)
	}
	if got := StagedDataPredicate(nil); got != "" {
		t.Fatalf("a nil binding rendered %q", got)
	}
}

// The rendered predicate must be a REAL filter, not a string nobody
// checked. rowauthz_enforce.go's rowAuthzPredicateExpr parses what
// InjectedPredicate renders rather than hand-building the AST, precisely so
// the term the engine enforces is the term shadow mode reports; whatever
// enforces this one will want the same property, and a renderer that emits
// something the parser rejects would not discover that until then.
func TestStagedDataPredicateParsesToTheConceptIntrinsic(t *testing.T) {
	const concept = "v1:probe:widget"
	rendered := StagedDataPredicate(&StagedDataBinding{Concept: concept, Staged: true})
	parsed, err := parseViaLangparser(rendered, false)
	if err != nil {
		t.Fatalf("the rendered predicate %q does not parse: %v", rendered, err)
	}
	if parsed.Root == nil {
		t.Fatalf("the rendered predicate %q parsed to nothing", rendered)
	}
	cmp, ok := parsed.Root.(*ComparisonExpression)
	if !ok {
		t.Fatalf("the rendered predicate %q parsed to a %T, not a comparison", rendered, parsed.Root)
	}
	if !stagedDataIsConceptField(cmp.Field) {
		t.Fatalf("the rendered predicate %q parsed to a comparison on %v, not the concept intrinsic",
			rendered, cmp.Field)
	}
	if cmp.Operator != OpNe {
		t.Fatalf("the rendered predicate %q parsed with operator %q; the exclusion must be %q",
			rendered, cmp.Operator, OpNe)
	}
	if got, _ := cmp.Value.(string); strings.TrimSpace(got) != concept {
		t.Fatalf("the rendered predicate %q parsed with value %v, not %q", rendered, cmp.Value, concept)
	}

	// The round trip: the matcher accepts what the renderer emitted, once
	// the parser has been through it. Renderer and matcher agreeing on
	// hand-built ASTs is not the same claim.
	if !stagedDataExcludesConcept(parsed.Root, concept) {
		t.Fatalf("the matcher does not recognise the parsed form of its own renderer's output (%s)",
			canonicalExpression(parsed.Root))
	}
}

// The `not in` spelling of the exclusion, and the near misses that must NOT
// be credited. A matcher looser than the exclusion it is looking for would
// report a construct as unchanged that enforcement changes -- the
// understatement this file's doctrine forbids as loudly as overstatement.
func TestStagedDataExclusionMatcherIsExact(t *testing.T) {
	const concept = "v1:probe:widget"
	conceptField := FieldReference{Raw: "row.concept", Parts: []string{"row", "concept"}}

	if !stagedDataExcludesConcept(&ComparisonExpression{
		Field: conceptField, Operator: OpOut, Value: []any{"v1:other:thing", concept},
	}, concept) {
		t.Fatal("`row.concept not in [..., <concept>]` was not credited as an exclusion")
	}

	for label, node := range map[string]ExpressionNode{
		"equality, not exclusion": &ComparisonExpression{
			Field: conceptField, Operator: OpEq, Value: concept},
		"excludes a DIFFERENT concept": &ComparisonExpression{
			Field: conceptField, Operator: OpNe, Value: "v1:other:thing"},
		"not-in naming other concepts only": &ComparisonExpression{
			Field: conceptField, Operator: OpOut, Value: []any{"v1:other:thing"}},
		"a payload property that happens to be called concept": &ComparisonExpression{
			Field:    FieldReference{Raw: "payload.concept", Parts: []string{"payload", "concept"}},
			Operator: OpNe, Value: concept},
		"a different intrinsic": &ComparisonExpression{
			Field:    FieldReference{Raw: "row.type", Parts: []string{"row", "type"}},
			Operator: OpNe, Value: concept},
		"not a comparison at all": &SpecReferenceExpression{Name: "someSpec"},
	} {
		if stagedDataExcludesConcept(node, concept) {
			t.Errorf("%s was credited as excluding %s", label, concept)
		}
	}
}

// A BARE BUILTIN AT plan.Root -- the memql#3982 shape -- must be named as
// that shape, not folded into a generic "some expression" answer.
//
// The arm cannot be reached from the loaded registry: the loader stores a
// logic's `return <builtin>({...})` as a FunctionCallExpression and only
// the validator rewrites it to a BuiltinFunctionExpression, at plan time.
// So over the tree this arm reports zero, and an arm that reports zero and
// is never exercised is indistinguishable from an arm that is broken. This
// test is the difference.
func TestStagedDataShadowNamesTheTopLevelBuiltinSeam(t *testing.T) {
	root := &BuiltinFunctionExpression{Name: "recall", Executor: "integration.knowledge.recall"}
	verdict, reason := AnalyzeStagedDataShadow("logic", root, &StagedDataBinding{Concept: "", Staged: true})
	if verdict != StagedDataUndecidable {
		t.Fatalf("a bare builtin at plan.Root reads as %q; it binds no concept, so it is undecidable", verdict)
	}
	if !strings.Contains(reason, "3982") || !strings.Contains(reason, `"recall"`) {
		t.Fatalf("the reason %q neither names the builtin nor the seam gap; the undecidable bucket "+
			"is only useful if each entry says which hole it is", reason)
	}
	// And it survives the read-pipeline wrappers, since a plan root can
	// carry them.
	wrapped := &ShapeExpression{Target: root}
	if _, wrappedReason := AnalyzeStagedDataShadow("logic", wrapped,
		&StagedDataBinding{Concept: "", Staged: true}); !strings.Contains(wrappedReason, "3982") {
		t.Fatalf("a wrapped bare builtin reads as %q; unwrapToFilter must be applied before the "+
			"shape is classified", wrappedReason)
	}
}

// The traversal detector reports ZERO over this tree, because the tree
// contains no `withDepth` directive and no relationship-traversal spelling.
// That is a real measurement only if the detector can fire at all -- so
// prove it can, on each shape it must recognise and through the wrappers
// that would otherwise hide one.
func TestStagedDataTraversesDetectsGraphExpansion(t *testing.T) {
	leaf := &ComparisonExpression{
		Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
		Operator: OpEq, Value: "v1:probe:widget",
	}
	if StagedDataTraverses(leaf) {
		t.Fatal("a plain comparison was reported as traversing")
	}
	if StagedDataTraverses(nil) {
		t.Fatal("a nil expression was reported as traversing")
	}
	for label, expr := range map[string]ExpressionNode{
		"a bare relationship":      &RelationshipExpression{Target: leaf},
		"a bare depth directive":   &DepthExpression{Target: leaf},
		"a wrapped relationship":   &ShapeExpression{Target: &PaginateExpression{Target: &RelationshipExpression{Target: leaf}}},
		"a relationship in an AND": &LogicalExpression{Op: LogicalAnd, Left: leaf, Right: &RelationshipExpression{Target: leaf}},
		"a relationship in an OR":  &LogicalExpression{Op: LogicalOr, Left: leaf, Right: &RelationshipExpression{Target: leaf}},
	} {
		if !StagedDataTraverses(expr) {
			t.Errorf("%s was not detected; the zero this reports over the tree would be a broken "+
				"detector rather than a finding", label)
		}
	}
}

// A builtin must land in `undecidable`, never in `not-a-read-path`.
//
// The trap is real and cheap to fall into: a builtin carries no read
// EXPRESSION, so a classifier keying on "expr == nil" files it beside the
// mutations as a construct that does not read. It does read -- its Go
// executor runs its own SQL, reaching storage without a plan -- so filing
// it there would delete the largest single population of uncoverable reads
// from the finding while the totals still added up.
func TestStagedDataShadowFilesBuiltinsAsUndecidable(t *testing.T) {
	registry := stagedDataTreeRegistry(t)
	seen := 0
	for _, fn := range registry.List() {
		if fn == nil || !strings.EqualFold(strings.TrimSpace(fn.FunctionKind), FunctionTypeBuiltin) {
			continue
		}
		seen++
		verdict, reason := AnalyzeStagedDataShadow(fn.FunctionKind, fn.Expr,
			&StagedDataBinding{Concept: strings.TrimSpace(fn.BoundConcept), Staged: true})
		if verdict != StagedDataUndecidable {
			t.Fatalf("builtin %s reads as %q (%s); a builtin's read runs inside a Go executor and "+
				"never reaches the injection seam, so it is undecidable", fn.Name, verdict, reason)
		}
	}
	if seen == 0 {
		t.Fatal("no builtins in the loaded registry -- LoadUnifiedBuiltins is not being called, " +
			"and the measurement is blind to the population it most needs to see")
	}
	t.Logf("%d builtins, all undecidable", seen)
}
