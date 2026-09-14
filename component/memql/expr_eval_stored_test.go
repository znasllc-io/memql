package memql

import (
	"context"
	"errors"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// expr_eval_stored_test.go -- a value read out of a stored row that is not
// the type an operation expects answers as the pushdown does: not a member,
// no element passes any() or all(), count() counts what is stored, and as a
// condition it is not true (expr_stored.go, memql#5369). These are the
// mistyped rows on which the differential comparison found EvalExpr refusing,
// or reading an object as a one-element list, where the SQL answered
// (expr_lower_agreement_db_test.go, test/conformance/differential_db_test.go).

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

	// count() counts what is stored: an object is no longer a one-element
	// list, anything that is neither a list nor a string counts 0, and a
	// string counts its characters -- the pushdown's count dispatches on
	// jsonb_typeof the same way.
	for name, want := range map[string]int{"an object": 0, "a number": 0, "a boolean": 0, "a scalar string": 1} {
		got, err := EvalExpr(context.Background(), xmeth(xpath("row", "tags"), "count"), MapScope{"row": rows[name]}, EvalOptions{})
		if err != nil || got != int64(want) {
			t.Errorf("row.tags.count() over a stored %s = %#v, %v; want %d", name, got, err, want)
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

// A stored value that is not a bool, read as a condition, is NOT TRUE -- in
// every condition position, as the pushdown's `x == true` reads it -- and a
// stored bool is itself.
func TestEvalExprStoredNonBooleanIsNotTrue(t *testing.T) {
	flagged := func(name string) (string, ast.ExpressionNode, bool) {
		if name != "isFlagged" {
			return "", nil, false
		}
		body, err := languageParser.ParseV1Expression(`r.flag`)
		if err != nil {
			return "", nil, false
		}
		return "r", body, true
	}
	opts := EvalOptions{Predicates: flagged}
	cases := []struct {
		src  string
		want bool
	}{
		{`row => row.flag`, false},
		{`row => !row.flag`, true},
		{`row => (row.flag)`, false},
		{`row => row.flag && true`, false},
		{`row => true && row.flag`, false},
		{`row => row.flag || true`, true},
		{`row => false || row.flag`, false},
		// A ternary's condition, and its branches when the ternary is itself
		// a condition.
		{`row => row.flag ? false : true`, true},
		{`row => true ? row.flag : true`, false},
		{`row => !(true ? row.flag : true)`, true},
		// A predicate lambda's body over a stored element, and a predicate's
		// body over the row.
		{`row => row.items.any(i => i.flag)`, false},
		{`row => row.items.all(i => !i.flag)`, true},
		{`row => row.names.any(n => n)`, false},
		{`row => row.names.where(n => n).count() == 0`, true},
		{`row => isFlagged(row)`, false},
		{`row => !isFlagged(row)`, true},
	}
	for _, flag := range []any{"true", "maybe", float64(1), map[string]any{"k": true}, []any{true}} {
		row := ExprRow{ID: "r", Payload: map[string]any{
			"flag": flag, "items": []any{map[string]any{"flag": flag}}, "names": []any{"a"},
		}}
		for _, c := range cases {
			got, err := storedCase(t, c.src, MapScope{"row": row}, opts)
			if err != nil || got != c.want {
				t.Errorf("%s over a stored %#v = %v, %v; want %v (a stored non-boolean is not true)", c.src, flag, got, err, c.want)
			}
		}
	}
	for flag, want := range map[bool]bool{true: true, false: false} {
		row := ExprRow{ID: "r", Payload: map[string]any{"flag": flag}}
		got, err := storedCase(t, `row => row.flag`, MapScope{"row": row}, opts)
		if err != nil || got != want {
			t.Errorf("row.flag over a stored %v = %v, %v; want %v", flag, got, err, want)
		}
	}
}

// A value the expression COMPUTED that is not a bool is still refused as a
// condition: an argument, a call's result, arithmetic, an element of a list
// the expression built, a predicate applied to a computed value, and the row
// itself. A ternary hands back the value of its branch when it is not a
// condition.
func TestEvalExprComputedNonBooleanStillRefuses(t *testing.T) {
	isNamed := func(name string) (string, ast.ExpressionNode, bool) {
		if name != "isNamed" {
			return "", nil, false
		}
		body, err := languageParser.ParseV1Expression(`r.name`)
		if err != nil {
			return "", nil, false
		}
		return "r", body, true
	}
	row := ExprRow{ID: "r", Payload: map[string]any{"title": "A", "n": float64(1)}}
	scope := MapScope{"row": row, "args": map[string]any{"s": "true", "names": []any{"a"}, "record": map[string]any{"name": "A"}}}
	for _, src := range []string{
		`args.s`,
		`!args.s`,
		`args.s && true`,
		`true ? args.s : false`,
		`lower(row.title)`,
		`row.n + 1`,
		`args.names.any(n => n)`,
		`isNamed(args.record)`,
		`row`,
	} {
		n, err := languageParser.ParseV1Expression(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		_, err = EvalCondition(context.Background(), n, scope, EvalOptions{Predicates: isNamed})
		var ee *ExprError
		if !errors.As(err, &ee) || ee.Code != "condition_not_boolean" {
			t.Errorf("%s = %v; want condition_not_boolean", src, err)
		}
	}
	n, err := languageParser.ParseV1Expression(`true ? row.title : false`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := EvalExpr(context.Background(), n, scope, EvalOptions{})
	if err != nil || got != "A" {
		t.Errorf("a value ternary = %#v, %v; want the branch's value", got, err)
	}
}
