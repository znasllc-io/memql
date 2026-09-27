package parser

import (
	"errors"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// memql#2796: a top-level unparenthesized `||` in a query filter did not parse.
//
// The struct form `query NAME { filter ... }` is rewritten to the internal
// procedural shape `func (Receiver) NAME(ctx any) (any, error) { return
// <filterExpr>, nil }`, so the filter is parsed by parseGoStyleQueryBody --
// which entered the precedence chain at parseLogicalAnd and never reached
// parseLogicalOr, the only level that consumes `||`. `&&` was consumed, the
// `||` token was left sitting, and the parser reported whatever it wanted
// next, never naming the operator.
//
// The grammar documents `&&` and `||` with Go precedence, which only means
// something if both can appear at the same level without parens. The corpus
// happens to contain exactly one `||` filter and it is parenthesized, which is
// why the divergence between the documented grammar and the implementation
// went unnoticed.

// queryFilterExpr parses the internal procedural form a struct query lowers to
// and returns the filter expression.
func queryFilterExpr(t *testing.T, filter string) ast.ExpressionNode {
	t.Helper()
	src := "func (Query) q(ctx any) (any, error) {\n  return " + filter + ", nil\n}\n"
	file, err := ParseFile(src)
	if err != nil {
		t.Fatalf("parse %q: %v", filter, err)
	}
	if len(file.Definitions) != 1 {
		t.Fatalf("parse %q: got %d definitions, want 1", filter, len(file.Definitions))
	}
	fn, ok := file.Definitions[0].(*ast.FunctionDef)
	if !ok {
		t.Fatalf("parse %q: definition is %T, want *ast.FunctionDef", filter, file.Definitions[0])
	}
	expr, ok := fn.Body.(ast.ExpressionNode)
	if !ok {
		t.Fatalf("parse %q: body is %T, want an expression", filter, fn.Body)
	}
	return expr
}

func TestQueryFilterParsesTopLevelOr(t *testing.T) {
	for _, filter := range []string{
		`a == args.a || b == args.b`,
		`a == args.a || b == args.b || c == args.c`,
		`a == args.a && b == args.b || c == args.c`,
		`a == args.a || b == args.b && c == args.c`,
		`!a || b == args.b`,
	} {
		if got := queryFilterExpr(t, filter); got == nil {
			t.Errorf("filter %q parsed to a nil expression", filter)
		}
	}
}

// Go precedence: `&&` binds tighter than `||`, so `a && b || c` is
// `(a && b) || c` -- an OR at the root with an AND on its left. Parsing it at
// all is not enough; parsing it with the wrong shape would silently change
// which rows a filter matches.
func TestQueryFilterOrPrecedence(t *testing.T) {
	root, ok := queryFilterExpr(t, `a == args.a && b == args.b || c == args.c`).(*ast.LogicalExpr)
	if !ok {
		t.Fatalf("root is not a LogicalExpr")
	}
	if root.Op != ast.LogicalOr {
		t.Fatalf("root op = %v, want LogicalOr (&& binds tighter than ||)", root.Op)
	}
	left, ok := root.Left.(*ast.LogicalExpr)
	if !ok {
		t.Fatalf("root.Left is %T, want *ast.LogicalExpr", root.Left)
	}
	if left.Op != ast.LogicalAnd {
		t.Fatalf("root.Left op = %v, want LogicalAnd", left.Op)
	}

	// The mirror: `a || b && c` is `a || (b && c)`.
	root2 := queryFilterExpr(t, `a == args.a || b == args.b && c == args.c`).(*ast.LogicalExpr)
	if root2.Op != ast.LogicalOr {
		t.Fatalf("root2 op = %v, want LogicalOr", root2.Op)
	}
	right, ok := root2.Right.(*ast.LogicalExpr)
	if !ok {
		t.Fatalf("root2.Right is %T, want *ast.LogicalExpr", root2.Right)
	}
	if right.Op != ast.LogicalAnd {
		t.Fatalf("root2.Right op = %v, want LogicalAnd", right.Op)
	}
}

// The comma in `return <value>, <error>` is the return separator, NOT the
// retired `,`-as-OR connective. parseLogicalOr refuses a comma it reaches
// (memql#5439) -- and before that it folded one into an OR -- so reaching the
// OR level here through parseLogicalOr would refuse or swallow the error
// expression rather than leave it for the separator. This is the trap that
// made entering the chain one level too low look correct.
func TestQueryReturnCommaIsNotOr(t *testing.T) {
	// `nil` parses to *ast.NilExpr, so a swallowed return separator shows up as
	// a NilExpr arm inside the filter -- not as a literal.
	var hasNil func(ast.ExpressionNode) bool
	hasNil = func(n ast.ExpressionNode) bool {
		switch v := n.(type) {
		case *ast.NilExpr:
			return true
		case *ast.LogicalExpr:
			return hasNil(v.Left) || hasNil(v.Right)
		}
		return false
	}
	if hasNil(queryFilterExpr(t, `a == args.a || b == args.b`)) {
		t.Error("the return separator was folded into the filter as a `,`-OR arm; the comma must stay structural")
	}

	// A body with no error expression must still be rejected -- reaching OR
	// precedence must not loosen the `return <value>, <error>` contract.
	src := "func (Query) q(ctx any) (any, error) {\n  return a == args.a || b == args.b\n}\n"
	if _, err := ParseFile(src); err == nil {
		t.Error("a return with no error expression must still be rejected")
	}
}

// `||` is left-associative, matching Go: `a || b || c` is `(a || b) || c`.
func TestQueryFilterOrIsLeftAssociative(t *testing.T) {
	root, ok := queryFilterExpr(t, `a == args.a || b == args.b || c == args.c`).(*ast.LogicalExpr)
	if !ok {
		t.Fatal("root is not a LogicalExpr")
	}
	if root.Op != ast.LogicalOr {
		t.Fatalf("root op = %v, want LogicalOr", root.Op)
	}
	if _, ok := root.Left.(*ast.LogicalExpr); !ok {
		t.Errorf("root.Left is %T, want *ast.LogicalExpr -- `||` must be left-associative", root.Left)
	}
	if _, ok := root.Right.(*ast.LogicalExpr); ok {
		t.Error("root.Right is a LogicalExpr -- `||` parsed right-associatively")
	}
}

// The `,`-as-OR form used to parse inside a group and a directive target, and
// this test used to pin that it still did. memql#5439 retired it, so the pin
// is now the other way round: every one of these is refused, naming `||`, and
// the same filter written with `||` parses (comma_connective_5439_test.go holds
// the two readings to one tree).
func TestNestedCommaOrIsRefused(t *testing.T) {
	for _, tc := range []struct{ comma, pipe string }{
		{`(a == args.a, b == args.b)`, `(a == args.a || b == args.b)`},
		{`(a == args.a, b == args.b) && c == args.c`, `(a == args.a || b == args.b) && c == args.c`},
		{`sort((a == args.a, b == args.b), "createdAt")`, `sort((a == args.a || b == args.b), "createdAt")`},
	} {
		src := "func (Query) q(ctx any) (any, error) {\n  return " + tc.comma + ", nil\n}\n"
		_, err := ParseFile(src)
		var rf *RetiredFormError
		if !errors.As(err, &rf) || rf.Form.Rule != ruleCommaConnective {
			t.Errorf("filter %q: got %v, want the retired `,` connective refused (%s)", tc.comma, err, ruleCommaConnective)
		}
		if got := queryFilterExpr(t, tc.pipe); got == nil {
			t.Errorf("filter %q parsed to a nil expression", tc.pipe)
		}
	}
}

// The flag that makes parseLogicalOr stop at a comma, inCollectionArgs, also
// selects the object-literal value grammar, so setting it would have changed
// how `{a: 1}` parses inside a query body or directive target: values become
// wrapped expression nodes instead of plain scalars. Pin that they stay
// scalars.
func TestObjectLiteralGrammarUnchangedInQueryBody(t *testing.T) {
	for _, filter := range []string{
		`meta == {a: 1, b: "x"}`,
		`sort(meta == {a: 1}, "createdAt")`,
	} {
		var cmp *ast.ComparisonExpr
		var find func(ast.Node)
		find = func(n ast.Node) {
			switch v := n.(type) {
			case *ast.ComparisonExpr:
				if cmp == nil {
					cmp = v
				}
			case *ast.LogicalExpr:
				find(v.Left)
				find(v.Right)
			case *ast.SortExpr:
				find(v.Target)
			}
		}
		find(queryFilterExpr(t, filter))
		if cmp == nil {
			t.Errorf("filter %q: found no comparison", filter)
			continue
		}
		obj, ok := cmp.Value.(map[string]any)
		if !ok {
			t.Errorf("filter %q: comparison value is %T, want map[string]any", filter, cmp.Value)
			continue
		}
		for k, v := range obj {
			if _, wrapped := v.(ast.ExpressionNode); wrapped {
				t.Errorf("filter %q: object key %q parsed to %T, want a plain scalar; the literal value grammar changed (inCollectionArgs leaked into parseObject)", filter, k, v)
			}
		}
	}
}

// contains() had the same too-low entry point and the same operator-blind
// error. Its comma separates the relationship target from the substring
// argument of the two-arg string-search form, so it needs the same treatment:
// reach OR precedence, leave the comma alone.
func TestContainsTargetParsesTopLevelOr(t *testing.T) {
	for _, filter := range []string{
		`contains(a == args.a || b == args.b)`,
		`contains(a == args.a && b == args.b || c == args.c)`,
	} {
		if got := queryFilterExpr(t, filter); got == nil {
			t.Errorf("filter %q parsed to a nil expression", filter)
		}
	}
	// The two-arg string-search form still works -- the comma still separates.
	if got := queryFilterExpr(t, `contains(name, "abc")`); got == nil {
		t.Error("the two-arg contains(str, substr) form must keep parsing")
	}
}
