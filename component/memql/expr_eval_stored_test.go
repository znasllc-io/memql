package memql

import (
	"context"
	"errors"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// expr_eval_stored_test.go -- a value read out of a stored row that is not a
// list where one is expected answers as the pushdown does: not a member, no
// element passes any() or all(), no element counted (expr_stored.go,
// memql#5369). These are the mistyped rows on which the differential
// comparison found EvalExpr refusing, or reading an object as a one-element
// list, where the SQL answered: irTags a non-array scalar, irTags an object,
// irNums a non-array scalar (expr_lower_agreement_db_test.go).

func storedCase(t *testing.T, src string, scope ExprScope, opts EvalOptions) (bool, error) {
	t.Helper()
	lam, err := languageParser.ParseV1Lambda(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	if lam != nil && len(lam.Params) == 1 {
		if row, ok := scope.(MapScope)["row"]; ok {
			s := MapScope{}
			for k, v := range scope.(MapScope) {
				s[k] = v
			}
			s[lam.Params[0]] = row
			scope = s
		}
		return EvalCondition(context.Background(), lam.Body, scope, opts)
	}
	return false, nil
}

func TestEvalExprStoredMismatchIsNoMatch(t *testing.T) {
	rows := map[string]ExprRow{
		"a scalar string": {ID: "r1", Payload: map[string]any{"tags": "a", "items": []any{map[string]any{"tags": "x"}}}},
		"an object":       {ID: "r2", Payload: map[string]any{"tags": map[string]any{"k": "a"}}},
		"a number":        {ID: "r3", Payload: map[string]any{"tags": float64(7)}},
		"a boolean":       {ID: "r4", Payload: map[string]any{"tags": true}},
	}
	cases := []struct {
		src  string
		want bool
	}{
		{`row => "a" in row.tags`, false},
		{`row => !("a" in row.tags)`, true},
		{`row => "" in row.tags`, false},
		{`row => nil in row.tags`, false},
		{`row => row.tags.any(t => t == "a")`, false},
		{`row => !(row.tags.any(t => t == "a"))`, true},
		{`row => row.tags.all(t => t != "")`, false},
		{`row => !(row.tags.all(t => t != ""))`, true},
		// An element of a stored list is stored too.
		{`row => row.items.any(i => "x" in i.tags)`, false},
	}
	for name, row := range rows {
		for _, c := range cases {
			got, err := storedCase(t, c.src, MapScope{"row": row}, EvalOptions{})
			if err != nil || got != c.want {
				t.Errorf("%s over a stored %s = %v, %v; want %v (a stored mismatch satisfies nothing)", c.src, name, got, err, c.want)
			}
		}
	}

	// count() of a stored value that is not a list is zero -- an object is no
	// longer a one-element list -- while a string keeps its character count
	// (the open question named in irEvalDivergentRows).
	for name, want := range map[string]bool{"an object": true, "a number": true, "a boolean": true, "a scalar string": false} {
		got, err := storedCase(t, `row => row.tags.count() == 0`, MapScope{"row": rows[name]}, EvalOptions{})
		if err != nil || got != want {
			t.Errorf("row.tags.count() == 0 over a stored %s = %v, %v; want %v", name, got, err, want)
		}
	}

	// A predicate applied to a stored row reads its argument as stored.
	hasA := func(name string) (string, ast.ExpressionNode, bool) {
		if name != "hasTagA" {
			return "", nil, false
		}
		body, err := languageParser.ParseV1Expression(`"a" in r.tags`)
		if err != nil {
			return "", nil, false
		}
		return "r", body, true
	}
	got, err := storedCase(t, `row => hasTagA(row)`, MapScope{"row": rows["a scalar string"]}, EvalOptions{Predicates: hasA})
	if err != nil || got {
		t.Errorf("hasTagA(row) over a stored scalar = %v, %v; want false", got, err)
	}
}

// A value computed in process keeps its refusals: `in` over a number an
// author passed, or a list method on one, is a mistake to name, and has no SQL
// twin to agree with (in a pushdown position it is a plan constant).
func TestEvalExprComputedMismatchStillRefuses(t *testing.T) {
	scope := MapScope{"args": map[string]any{"tags": "a", "obj": map[string]any{"k": "a"}}}
	for src, code := range map[string]string{
		`"a" in args.tags`:             "in_requires_list",
		`"a" in args.obj`:              "in_requires_list",
		`args.tags.any(t => t == "a")`: "operand_type",
		`args.tags.all(t => t == "a")`: "operand_type",
	} {
		n, err := languageParser.ParseV1Expression(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		_, err = EvalCondition(context.Background(), n, scope, EvalOptions{})
		var ee *ExprError
		if !errors.As(err, &ee) || ee.Code != code {
			t.Errorf("%s = %v; want the refusal %s", src, err, code)
		}
	}
	// And a stored LIST is read as a list.
	row := ExprRow{ID: "r", Payload: map[string]any{"tags": []any{"a", "b"}}}
	got, err := storedCase(t, `row => "a" in row.tags && row.tags.count() == 2 && row.tags.all(t => t != "")`, MapScope{"row": row}, EvalOptions{})
	if err != nil || !got {
		t.Errorf("a stored list = %v, %v; want true", got, err)
	}
}
