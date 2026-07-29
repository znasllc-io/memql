package memql

import (
	"os"
	"sort"

	"fmt"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// ---- expression builders, in the pre-resolution shape the hook sees ----

func fieldCmp(field string, op ComparisonOperator, value any) *ComparisonExpression {
	return &ComparisonExpression{
		Field:    FieldReference{Raw: field, Parts: strings.Split(field, ".")},
		Operator: op,
		Value:    value,
	}
}

// ownerScoped builds `<field> == actor.userId` the way the parser leaves
// it BEFORE resolveActorReferences: an unresolved *ActorReference.
func ownerScoped(field string) *ComparisonExpression {
	return fieldCmp(field, OpEq, &ActorReference{Path: "userId"})
}

func adminGate() *ComparisonExpression {
	return fieldCmp("actor.isClusterOwner", OpEq, true)
}

func and(l, r ExpressionNode) *LogicalExpression {
	return &LogicalExpression{Op: LogicalAnd, Left: l, Right: r}
}

func or(l, r ExpressionNode) *LogicalExpression {
	return &LogicalExpression{Op: LogicalOr, Left: l, Right: r}
}

func ownedTier(field string) *langparser.RowAuthzDecl {
	return &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: field}
}

// ---- the three verdicts the DoD names ----

// A filter that already hand-writes the tier's term is unaffected by
// enforcement. This is the common case and the one that keeps the
// blast-radius number honest.
func TestShadowAlreadyImplied(t *testing.T) {
	cases := []struct {
		name string
		expr ExpressionNode
		decl *langparser.RowAuthzDecl
	}{
		{"bare owner field", ownerScoped("ownerUserId"), ownedTier("ownerUserId")},
		{"payload-prefixed owner field", ownerScoped("payload.ownerUserId"), ownedTier("ownerUserId")},
		{"owner term as one conjunct", and(fieldCmp("status", OpEq, "active"), ownerScoped("ownerUserId")), ownedTier("ownerUserId")},
		{"owner term first", and(ownerScoped("ownerUserId"), fieldCmp("status", OpEq, "active")), ownedTier("ownerUserId")},
		{"admin gate", adminGate(), &langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner}},
		{"admin gate as one conjunct", and(fieldCmp("kind", OpEq, "x"), adminGate()), &langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner}},
		{"public injects nothing", fieldCmp("status", OpEq, "active"), &langparser.RowAuthzDecl{Tier: langparser.RowAuthzPublic}},
		{"granted, spec named as a conjunct", and(&SpecReferenceExpression{Name: "spaceMember"}, fieldCmp("k", OpEq, "v")),
			&langparser.RowAuthzDecl{Tier: langparser.RowAuthzGranted, Spec: "spaceMember"}},
		// Every arm of the disjunction carries the term, so the whole
		// expression guarantees it even with no top-level conjunct.
		{"every disjunct carries the term",
			or(and(ownerScoped("ownerUserId"), fieldCmp("kind", OpEq, "doc")),
				and(ownerScoped("ownerUserId"), fieldCmp("kind", OpEq, "sheet"))),
			ownedTier("ownerUserId")},
		{"admin gate spelled != false", fieldCmp("actor.isClusterOwner", OpNe, false),
			&langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := AnalyzeShadow(tc.expr, tc.decl)
			if got != ShadowAlreadyImplied {
				t.Fatalf("verdict = %q (%s), want %q", got, reason, ShadowAlreadyImplied)
			}
		})
	}
}

// The blast radius: enforcement would remove rows these return today.
func TestShadowWouldNarrow(t *testing.T) {
	cases := []struct {
		name string
		expr ExpressionNode
		decl *langparser.RowAuthzDecl
	}{
		{"no caller term at all", fieldCmp("spaceId", OpEq, "s1"), ownedTier("ownerUserId")},
		// A nested path whose LEAF happens to match is not the concept's
		// top-level column. Reading it as the owner term reported
		// already-implied for filters enforcement WOULD narrow -- the
		// dangerous direction for a measurement feeding a ruling.
		{"nested path with a matching leaf", ownerScoped("input.ownerUserId"), ownedTier("ownerUserId")},
		{"deeply nested path", ownerScoped("a.b.c.ownerUserId"), ownedTier("ownerUserId")},
		{"row intrinsic, not a payload field", ownerScoped("row.ownerUserId"), ownedTier("ownerUserId")},
		{"different owner field", ownerScoped("requestedBy"), ownedTier("ownerUserId")},
		{"no filter at all", nil, ownedTier("ownerUserId")},
		{"admin tier, unrelated filter", fieldCmp("status", OpEq, "open"), &langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner}},
		// memql#2832: a term under a top-level `||` guarantees nothing --
		// the other arm still returns rows the caller does not own.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := AnalyzeShadow(tc.expr, tc.decl)
			if got != ShadowWouldNarrow {
				t.Fatalf("verdict = %q (%s), want %q", got, reason, ShadowWouldNarrow)
			}
			if reason == "" {
				t.Fatal("a would-narrow verdict must carry a reason -- the list is what a ruling is taken against")
			}
		})
	}
}

// Undecidable is never guessed into one of the other buckets. An
// overstated blast radius misleads a ruling exactly as badly as an
// understated one.
func TestShadowUndecidable(t *testing.T) {
	cases := []struct {
		name string
		expr ExpressionNode
		decl *langparser.RowAuthzDecl
	}{
		{"unexpanded spec could itself carry the term",
			and(&SpecReferenceExpression{Name: "isActiveRecord"}, fieldCmp("k", OpEq, "v")), ownedTier("ownerUserId")},
		{"relationship traversal",
			and(&RelationshipExpression{}, fieldCmp("k", OpEq, "v")), ownedTier("ownerUserId")},
		{"builtin expression",
			and(&BuiltinFunctionExpression{Name: "concepts"}, fieldCmp("k", OpEq, "v")), ownedTier("ownerUserId")},
		{"granted tier needs relationship semantics",
			fieldCmp("spaceId", OpEq, "s1"), &langparser.RowAuthzDecl{Tier: langparser.RowAuthzGranted, Spec: "spaceMember"}},
		// memql#2832 in the honest direction: one arm carries the term
		// and the other does not, so the disjunction guarantees nothing
		// -- but it is not a CONFIDENT narrowing either.
		{"owner term is only one arm of a disjunction",
			or(ownerScoped("ownerUserId"), fieldCmp("visibility", OpEq, "public")), ownedTier("ownerUserId")},
		{"owner term under a disjunction, && binding tighter",
			or(and(ownerScoped("ownerUserId"), fieldCmp("kind", OpEq, "doc")), fieldCmp("shared", OpEq, true)),
			ownedTier("ownerUserId")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := AnalyzeShadow(tc.expr, tc.decl)
			if got != ShadowUndecidable {
				t.Fatalf("verdict = %q (%s), want %q", got, reason, ShadowUndecidable)
			}
			if reason == "" {
				t.Fatal("an undecidable verdict must say what could not be decided")
			}
		})
	}
}

// THE POINT OF MEASURING BEFORE RESOLUTION.
//
// resolveActorReferences turns `ownerUserId==actor.userId` into
// `ownerUserId=="u_123"`. Analysing that form reports would-narrow for
// every construct that already hand-writes the term -- which is most of
// the already-correct ones -- and the measurement inverts.
func TestShadowMustSeeUnresolvedActorReferences(t *testing.T) {
	decl := ownedTier("ownerUserId")

	unresolved := ownerScoped("ownerUserId")
	if got, _ := AnalyzeShadow(unresolved, decl); got != ShadowAlreadyImplied {
		t.Fatalf("unresolved form: verdict = %q, want %q", got, ShadowAlreadyImplied)
	}

	// The same construct after resolution: a plain string literal.
	resolved := fieldCmp("ownerUserId", OpEq, "u_123")
	if got, _ := AnalyzeShadow(resolved, decl); got != ShadowWouldNarrow {
		t.Fatalf("resolved form: verdict = %q, want %q -- if this ever reports already-implied "+
			"the hook could be moved after resolution without the test noticing", got, ShadowWouldNarrow)
	}
}

// Graph expansion has no filter, so it cannot imply a narrowing tier.
// The measurement has to say so rather than skip the path.
func TestShadowGraphExpansionHasNoFilter(t *testing.T) {
	got, reason := AnalyzeShadow(nil, ownedTier("ownerUserId"))
	if got != ShadowWouldNarrow {
		t.Fatalf("verdict = %q, want %q for a filterless traversal", got, ShadowWouldNarrow)
	}
	if !strings.Contains(reason, "no filter") {
		t.Fatalf("reason = %q, want it to say the access has no filter", reason)
	}
	// A public concept is still reachable by traversal without change.
	if got, _ := AnalyzeShadow(nil, &langparser.RowAuthzDecl{Tier: langparser.RowAuthzPublic}); got != ShadowAlreadyImplied {
		t.Fatalf("public tier under traversal: verdict = %q, want %q", got, ShadowAlreadyImplied)
	}
}

// An undeclared concept is not measurable -- there is no predicate to
// compute. It must not be silently counted as already-implied in the
// records, which would inflate the "nothing changes" bucket.
func TestShadowSkipsUndeclaredConcepts(t *testing.T) {
	if got, _ := AnalyzeShadow(fieldCmp("k", OpEq, "v"), nil); got != ShadowAlreadyImplied {
		t.Fatalf("a nil decl should be inert, got %q", got)
	}
	// recordShadow is the guard that keeps them OUT of the record set.
	ResetShadowRecords()
	recordShadow("someQuery", "v1:nope:missing", ShadowPathFilter, fieldCmp("k", OpEq, "v"))
	if n := len(ShadowRecords()); n != 0 {
		t.Fatalf("recorded %d entries for an unknown concept, want 0", n)
	}
}

func TestInjectedPredicateRendering(t *testing.T) {
	cases := []struct {
		decl *langparser.RowAuthzDecl
		want string
	}{
		{ownedTier("ownerUserId"), "ownerUserId==actor.userId"},
		{&langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner}, "actor.isClusterOwner==true"},
		{&langparser.RowAuthzDecl{Tier: langparser.RowAuthzPublic}, ""},
		{&langparser.RowAuthzDecl{Tier: langparser.RowAuthzGranted, Spec: "spaceMember"}, "spaceMember"},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := InjectedPredicate(tc.decl); got != tc.want {
			t.Fatalf("InjectedPredicate(%+v) = %q, want %q", tc.decl, got, tc.want)
		}
	}
}

// The gate is off by default. Shadow mode is a measurement tool, not a
// production code path.
func TestShadowOffByDefault(t *testing.T) {
	if _, set := lookupShadowEnv(); set {
		t.Skip("MEMQL_ROWAUTHZ_SHADOW is set in this environment")
	}
	if ShadowEnabled() {
		t.Fatal("shadow mode is on with no env var set; it must be opt-in")
	}
	ResetShadowRecords()
	// With the gate off, the hook records nothing even for a declared
	// concept and a would-narrow filter.
	recordShadow("q", "v1:notes:note", ShadowPathFilter, fieldCmp("k", OpEq, "v"))
	if n := len(ShadowRecords()); n != 0 {
		t.Fatalf("recorded %d entries with the gate off, want 0", n)
	}
}

// forceShadow turns the gate on for one test without touching the
// sync.Once. Doing `shadowOnce.Do(func(){})` instead PERMANENTLY
// poisons the gate: if the Once had not already fired, the no-op
// leaves shadowEnabled false for the rest of the process, and every
// later gate-on assertion silently measures nothing.
func forceShadow(t *testing.T) {
	t.Helper()
	prev := shadowEnabled
	shadowEnabled = true
	t.Cleanup(func() { shadowEnabled = prev })
}

// The gate must not change what a read returns, and the hook must not
// touch the expression it is handed.
//
// It uses a REAL declared concept. Passing an unknown one made this
// vacuous: recordShadow returns before reaching the analyzer for an
// undeclared concept (which TestShadowSkipsUndeclaredConcepts proves),
// so it asserted that a code path doing nothing changed nothing.
func TestShadowHookDoesNotTouchTheExpression(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	declared := aDeclaredConcept(t)

	forceShadow(t)
	ResetShadowRecords()

	expr := and(ownerScoped("ownerUserId"), fieldCmp("status", OpEq, "active"))
	before := fmt.Sprintf("%#v %#v", expr.Left, expr.Right)

	recordShadow("probe", declared, ShadowPathFilter, expr)

	if after := fmt.Sprintf("%#v %#v", expr.Left, expr.Right); after != before {
		t.Fatalf("the hook mutated the expression:\n before %s\n after  %s", before, after)
	}
	// And it must actually have measured something, or the assertion
	// above is worthless.
	if n := len(ShadowRecords()); n != 1 {
		t.Fatalf("recorded %d entries for a DECLARED concept, want 1 -- the test is vacuous otherwise", n)
	}
}

// aDeclaredConcept returns the name of a concept that carries a tier,
// so a test can exercise the path that actually runs the analyzer.
func aDeclaredConcept(t *testing.T) string {
	t.Helper()
	names := make([]string, 0)
	for name, c := range memorynodes.All() {
		if c != nil && c.RowAuthz != nil {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		t.Skip("no concept in the tree declares a tier")
	}
	sort.Strings(names)
	return names[0]
}

// THE GUARD THE DOCS PROMISED.
//
// The analyzer must see the expression BEFORE resolveActorReferences.
// TestShadowMustSeeUnresolvedActorReferences proves the ANALYZER
// distinguishes the two forms, but it builds its own expressions and is
// completely uncoupled from the call site -- moving the hook after
// resolution left the whole package green.
//
// This reads the executor's source and asserts the call ordering
// directly. Structural rather than behavioural, because the behavioural
// version needs a live database; and the failure it guards against is
// silent and total (every construct that hand-writes the term flips to
// would-narrow, inverting the measurement).
func TestShadowHookRunsBeforeActorResolution(t *testing.T) {
	src, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatalf("read executor.go: %v", err)
	}
	text := string(src)

	fnStart := strings.Index(text, "func (e *MemQLEngine) evaluateExpressionSet(")
	if fnStart < 0 {
		t.Fatal("evaluateExpressionSet not found -- this guard needs re-aiming")
	}
	// Bound the search at the next top-level func so a later call
	// cannot satisfy it.
	fnEnd := strings.Index(text[fnStart+1:], "\nfunc ")
	if fnEnd < 0 {
		fnEnd = len(text) - fnStart - 1
	}
	body := text[fnStart : fnStart+1+fnEnd]

	hook := strings.Index(body, "recordShadow(")
	resolve := strings.Index(body, "resolveActorReferences(")
	if hook < 0 {
		t.Fatal("evaluateExpressionSet no longer calls recordShadow -- the filter read path is unmeasured")
	}
	if resolve < 0 {
		t.Fatal("evaluateExpressionSet no longer calls resolveActorReferences -- this guard needs re-aiming")
	}
	if hook > resolve {
		t.Fatal("the shadow hook now runs AFTER resolveActorReferences. That function substitutes " +
			"actor.userId with the caller's concrete id, so the analyzer would see " +
			"`ownerUserId==\"u_123\"` where the author wrote `ownerUserId==actor.userId` and report " +
			"would-narrow for every construct that already carries the term -- inverting the whole " +
			"measurement, silently. Move the hook back above resolveActorReferences.")
	}
}

// The graph-expansion hook must run BEFORE the depth==0 early return.
// The default expansion depth is 1, so a child is visited with depth 0;
// a hook below that return only ever sees the root, which the filter
// path already measured, and never a row traversal walked INTO.
func TestGraphShadowHookRunsBeforeTheDepthGuard(t *testing.T) {
	src, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatalf("read executor.go: %v", err)
	}
	text := string(src)

	fnStart := strings.Index(text, "func (e *MemQLEngine) expandGraph(")
	if fnStart < 0 {
		t.Fatal("expandGraph not found -- this guard needs re-aiming")
	}
	fnEnd := strings.Index(text[fnStart+1:], "\nfunc ")
	if fnEnd < 0 {
		fnEnd = len(text) - fnStart - 1
	}
	body := text[fnStart : fnStart+1+fnEnd]

	hook := strings.Index(body, "ShadowPathGraphExpansion")
	depthGuard := strings.Index(body, "if depth == 0 {")
	if hook < 0 {
		t.Fatal("expandGraph no longer records a graph-expansion verdict -- the traversal path is unmeasured")
	}
	if depthGuard < 0 {
		t.Fatal("expandGraph's depth guard not found -- this guard needs re-aiming")
	}
	if hook > depthGuard {
		t.Fatal("the graph-expansion hook now runs after the `depth == 0` return. The default depth " +
			"is 1, so children are visited with depth 0 and return before reaching it -- the hook " +
			"would only ever see the root row, which the filter path already measured, and never a " +
			"row traversal walked into. That is the only thing this hook exists to observe.")
	}
}
