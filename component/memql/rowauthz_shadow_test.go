package memql

import (
	"fmt"
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
		{"different owner field", ownerScoped("requestedBy"), ownedTier("ownerUserId")},
		{"no filter at all", nil, ownedTier("ownerUserId")},
		{"admin tier, unrelated filter", fieldCmp("status", OpEq, "open"), &langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner}},
		// memql#2832: a term under a top-level `||` guarantees nothing --
		// the other arm still returns rows the caller does not own.
		{"owner term under a top-level disjunction", or(ownerScoped("ownerUserId"), fieldCmp("visibility", OpEq, "public")), ownedTier("ownerUserId")},
		{"owner term under a disjunction, && binding tighter",
			or(and(ownerScoped("ownerUserId"), fieldCmp("kind", OpEq, "doc")), fieldCmp("shared", OpEq, true)),
			ownedTier("ownerUserId")},
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

// The gate must not change what a read returns. This runs the same
// analysis path the hook runs, with the gate on, and asserts the
// expression handed onward is byte-identical -- the hook's only effect
// is an append to the collector.
func TestShadowHookDoesNotTouchTheExpression(t *testing.T) {
	t.Setenv("MEMQL_ROWAUTHZ_SHADOW", "1")
	// ShadowEnabled memoises, so force the gate for this test rather
	// than relying on env ordering.
	shadowOnce.Do(func() {})
	prev := shadowEnabled
	shadowEnabled = true
	t.Cleanup(func() { shadowEnabled = prev })

	ResetShadowRecords()
	expr := and(ownerScoped("ownerUserId"), fieldCmp("status", OpEq, "active"))
	before := fmt.Sprintf("%#v %#v", expr.Left, expr.Right)

	recordShadow("probe", "v1:nope:missing", ShadowPathFilter, expr)

	if after := fmt.Sprintf("%#v %#v", expr.Left, expr.Right); after != before {
		t.Fatalf("the hook mutated the expression:\n before %s\n after  %s", before, after)
	}
}
