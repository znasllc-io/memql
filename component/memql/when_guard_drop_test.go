package memql

import (
	"testing"
)

func cmpExpr(field string, value any) *ComparisonExpression {
	return &ComparisonExpression{
		Field:    FieldReference{Raw: field, Parts: splitDots(field)},
		Operator: OpEq,
		Value:    value,
	}
}

func splitDots(s string) []string {
	var parts []string
	cur := ""
	for _, r := range s {
		if r == '.' {
			parts = append(parts, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(parts, cur)
}

// TestWhenGuardDrop covers the #975 semantics end-to-end on the engine AST: a
// `when(args.flag) { ... }` guard (a ConditionalFilterExpression) is dropped --
// along with its surrounding `&&` / `||` connective -- when the guard arg is
// absent, and unwrapped to its inner expression when present.
func TestWhenGuardDrop(t *testing.T) {
	v := &functionValidator{}

	build := func(op LogicalOp) *LogicalExpression {
		return &LogicalExpression{
			Op:    op,
			Left:  &ConditionalFilterExpression{ArgPath: "flag", Filter: cmpExpr("payload.a", "x")},
			Right: cmpExpr("payload.b", "y"),
		}
	}

	for _, op := range []LogicalOp{LogicalAnd, LogicalOr} {
		// Arg absent -> the guarded term and its connective are removed,
		// leaving just the other side (payload.b == "y").
		got, err := v.expandExpressionWithArgs(build(op), map[string]any{})
		if err != nil {
			t.Fatalf("op=%v absent: %v", op, err)
		}
		cmp, ok := got.(*ComparisonExpression)
		if !ok || cmp.Field.Raw != "payload.b" {
			t.Fatalf("op=%v absent: expected the guard dropped to leave payload.b, got %#v", op, got)
		}

		// Arg present-but-nil -> treated as absent and dropped. #1631: tool
		// handlers materialize an omitted optional arg as an explicit `null`
		// (todosList's `todos({done: $args.done})` collapses the
		// unfilled placeholder to `null`), so the args map carries the key
		// with a nil value. Before the fix this slipped past the existence
		// check, leaked nil into `payload.done == args.done`, and failed the
		// query with "unsupported literal type <nil>". It must drop exactly
		// like the absent case.
		gotNil, err := v.expandExpressionWithArgs(build(op), map[string]any{"flag": nil})
		if err != nil {
			t.Fatalf("op=%v present-nil: %v", op, err)
		}
		cmpNil, ok := gotNil.(*ComparisonExpression)
		if !ok || cmpNil.Field.Raw != "payload.b" {
			t.Fatalf("op=%v present-nil: expected the guard dropped to leave payload.b, got %#v", op, gotNil)
		}

		// Arg present -> the guard unwraps to its inner expression, and the
		// connective is preserved over both terms.
		got2, err := v.expandExpressionWithArgs(build(op), map[string]any{"flag": "on"})
		if err != nil {
			t.Fatalf("op=%v present: %v", op, err)
		}
		logical, ok := got2.(*LogicalExpression)
		if !ok || logical.Op != op {
			t.Fatalf("op=%v present: expected the connective preserved, got %#v", op, got2)
		}
		left, ok := logical.Left.(*ComparisonExpression)
		if !ok || left.Field.Raw != "payload.a" {
			t.Fatalf("op=%v present: expected the guard unwrapped to payload.a, got %#v", op, logical.Left)
		}
	}
}
