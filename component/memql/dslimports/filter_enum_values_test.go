package dslimports

import (
	"strings"
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
)

func enumSet() map[string][]string {
	return map[string][]string{
		"utteranceType": {"speech", "text", "action", "system"},
	}
}

func cmp(field string, op languageAst.ComparisonOperator, value any) *languageAst.ComparisonExpr {
	return &languageAst.ComparisonExpr{
		Field:    languageAst.FieldReference{Parts: []string{field}},
		Operator: op,
		Value:    value,
	}
}

// TestFilterEnumViolationsCatchesTheShippedDefect is the regression evidence
// for memql#2827. `agentInteractionCount` filtered
// `utteranceType in ["speech", "agentGreeting"]` where the enum is
// ("speech","text","action","system").
//
// "agentGreeting" is a `source.kind` value, not an utterance type, so that arm
// matched no row at all -- and because every writer that stamps
// `source.agentId` writes utteranceType "text", the "speech" arm excluded
// exactly the rows the agentId conjunct selected. The query returned ~0 rows
// for every agent, which left agentIsKnownToUser permanently false and agents
// re-introducing themselves in every new space.
//
// An out-of-set literal on an equality operator is statically always-false, so
// it is detectable without running anything -- this is the sibling of #2794's
// undeclared-FIELD check, for undeclared VALUES.
func TestFilterEnumViolationsCatchesTheShippedDefect(t *testing.T) {
	got := filterEnumViolations(cmp("utteranceType", languageAst.OpIn,
		[]any{"speech", "agentGreeting"}), enumSet())

	if len(got) != 1 {
		t.Fatalf("got %d violations, want 1: %+v", len(got), got)
	}
	if got[0].field != "utteranceType" || got[0].value != "agentGreeting" {
		t.Errorf("got %+v, want the agentGreeting member flagged", got[0])
	}
}

// TestEnumViolationStatesTheRightFailureMode pins the diagnostic's polarity.
// An out-of-set literal does opposite things depending on the operator:
//
//	utteranceType == "bogus"      matches NOTHING
//	utteranceType != "bogus"      matches EVERYTHING
//
// Both are defects worth failing on, but reporting the second as "always
// false" sends an author debugging an over-matching query in exactly the wrong
// direction. The first revision said "always false" for all four operators.
func TestEnumViolationStatesTheRightFailureMode(t *testing.T) {
	for _, tc := range []struct {
		op   languageAst.ComparisonOperator
		want string
	}{
		{languageAst.OpEq, "always false"},
		{languageAst.OpIn, "always false"},
		{languageAst.OpNe, "always true"},
		{languageAst.OpOut, "always true"},
	} {
		got := filterEnumViolations(cmp("utteranceType", tc.op, "bogus"), enumSet())
		if len(got) != 1 {
			t.Fatalf("operator %q: got %d violations, want 1", tc.op, len(got))
		}
		if !strings.Contains(got[0].consequence, tc.want) {
			t.Errorf("operator %q reports %q, want it to say %q -- the failure mode flips with polarity",
				tc.op, got[0].consequence, tc.want)
		}
	}
}

// TestFilterEnumViolationsAcceptsValidComparisons is the direction that would
// break the corpus: every legitimate comparison must stay silent.
func TestFilterEnumViolationsAcceptsValidComparisons(t *testing.T) {
	cases := []struct {
		name string
		node languageAst.Node
	}{
		{"equality on a member", cmp("utteranceType", languageAst.OpEq, "text")},
		{"inequality on a member", cmp("utteranceType", languageAst.OpNe, "system")},
		{"in-list of members", cmp("utteranceType", languageAst.OpIn, []any{"speech", "text"})},
		// Unknowable at lint time -- the lane reports only what it can prove.
		{"compared to an arg", cmp("utteranceType", languageAst.OpEq,
			languageAst.FieldReference{Parts: []string{"args", "kind"}})},
		// Not an enum property.
		{"non-enum property", cmp("text", languageAst.OpEq, "anything")},
		// A nested path is not the enum property itself.
		{"nested path", &languageAst.ComparisonExpr{
			Field:    languageAst.FieldReference{Parts: []string{"source", "kind"}},
			Operator: languageAst.OpEq,
			Value:    "agentGreeting",
		}},
		// Ordering on an enum is not this lane's business.
		{"ordering operator", cmp("utteranceType", languageAst.OpGt, "text")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := filterEnumViolations(tc.node, enumSet()); len(got) != 0 {
				t.Errorf("got %+v, want none -- a false positive here fails the build on legitimate DSL", got)
			}
		})
	}
}

// TestFilterEnumViolationsWalksBooleanStructure pins that a violation cannot
// hide behind a connective, a negation or a directive wrapper -- the same
// reach the sibling field-head walker has.
func TestFilterEnumViolationsWalksBooleanStructure(t *testing.T) {
	bad := cmp("utteranceType", languageAst.OpEq, "agentGreeting")
	ok := cmp("utteranceType", languageAst.OpEq, "text")

	for _, tc := range []struct {
		name string
		node languageAst.Node
	}{
		{"left of &&", &languageAst.LogicalExpr{Op: languageAst.LogicalAnd, Left: bad, Right: ok}},
		{"right of ||", &languageAst.LogicalExpr{Op: languageAst.LogicalOr, Left: ok, Right: bad}},
		{"under a negation", &languageAst.NotExpr{Target: bad}},
		{"under a shape wrapper", &languageAst.ShapeExpr{Target: bad}},
		{"under sort + paginate", &languageAst.SortExpr{Target: &languageAst.PaginateExpr{Target: bad}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := filterEnumViolations(tc.node, enumSet()); len(got) != 1 {
				t.Errorf("got %d violations, want 1 -- boolean shape must not hide an always-false predicate", len(got))
			}
		})
	}
}

// TestConceptEnumValuesReadsOnlyEnumProperties keeps the map narrow: a
// non-enum property must not acquire a member set, or every literal compared
// against it would be reported.
func TestConceptEnumValuesReadsOnlyEnumProperties(t *testing.T) {
	decl := &languageAst.ConceptDecl{
		Properties: []*languageAst.PropertyDecl{
			{Name: "utteranceType", Type: &languageAst.TypeRef{Kind: "enum", EnumValues: []string{"speech", "text"}}},
			{Name: "text", Type: &languageAst.TypeRef{Kind: "string"}},
			{Name: "empty", Type: &languageAst.TypeRef{Kind: "enum"}},
			{Name: "untyped"},
		},
	}
	got := conceptEnumValues(decl)
	if len(got) != 1 {
		t.Fatalf("got %d enum properties, want 1: %+v", len(got), got)
	}
	if len(got["utteranceType"]) != 2 {
		t.Errorf("utteranceType members = %v, want the two declared", got["utteranceType"])
	}
}
