package automations

import (
	"reflect"
	"testing"
)

func TestSplitConditionParts_ParenAware(t *testing.T) {
	// `(a , b , c) ; d` must split on `;` into the paren group and `d`,
	// keeping the inner commas grouped.
	got := splitConditionParts("(a , b , c) ; d", ';')
	want := []string{"(a , b , c)", "d"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paren-aware ; split: got %v want %v", got, want)
	}
	// At the top level there is no depth-0 comma.
	if parts := splitConditionParts("(a , b , c) ; d", ','); len(parts) != 1 {
		t.Fatalf("expected no depth-0 comma split, got %v", parts)
	}
}

func TestStripRedundantOuterParens(t *testing.T) {
	cases := map[string]string{
		"(a || b)":      "a || b",
		"((a || b))":    "a || b",
		"(a) && (b)":    "(a) && (b)", // outer parens don't wrap the whole expr
		"a || b":        "a || b",
		"(a || b) && c": "(a || b) && c",
	}
	for in, want := range cases {
		if got := stripRedundantOuterParens(in); got != want {
			t.Errorf("stripRedundantOuterParens(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEvaluateCondition_ParenGroupedOr exercises the exact migrated workbench
// condition shape `(status == ... || ... || ...) && planId != ""` -- the
// previously-broken word form `(... or ... or ...) and ...` now evaluates
// correctly via && / ||, including the parenthesised OR group.
func TestEvaluateCondition_ParenGroupedOr(t *testing.T) {
	cond := `($status == "succeeded" || $status == "failed" || $status == "cancelled") && $planId != ""`
	cases := []struct {
		status string
		planId string
		want   bool
	}{
		{"succeeded", "p-1", true}, // (T||F||F)&&T
		{"failed", "p-1", true},    // (F||T||F)&&T
		{"cancelled", "p-1", true}, // (F||F||T)&&T
		{"running", "p-1", false},  // (F||F||F)&&T -- the paren OR group gates the AND
	}
	for _, tc := range cases {
		e := NewEvaluator()
		e.SetCustom("status", tc.status)
		e.SetCustom("planId", tc.planId)
		got, err := e.EvaluateCondition(cond)
		if err != nil {
			t.Fatalf("EvaluateCondition(status=%q, planId=%q): %v", tc.status, tc.planId, err)
		}
		if got != tc.want {
			t.Errorf("EvaluateCondition(status=%q, planId=%q) = %v, want %v", tc.status, tc.planId, got, tc.want)
		}
	}
}
