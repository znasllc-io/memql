package memql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/functions"
)

// expr_eval_test.go pins EvalExpr, the one in-process evaluator of edition
// 2026 (epic memql#5363, memql#5367). The v1 parser is written in a parallel
// stream, so every input here is a tree built by hand; each failure prints the
// tree through ast.FormatExpr, which is the source text the parser will read
// back to the same tree.

// ---------------------------------------------------------------------------
// tree builders
// ---------------------------------------------------------------------------

func xid(name string) *ast.IdentExpr { return &ast.IdentExpr{Name: name} }
func xmem(o ast.ExpressionNode, f string) *ast.MemberExpr {
	return &ast.MemberExpr{Object: o, Field: f}
}
func xopt(o ast.ExpressionNode, f string) *ast.MemberExpr {
	return &ast.MemberExpr{Object: o, Field: f, Optional: true}
}
func xbin(op string, l, r ast.ExpressionNode) *ast.BinaryExpr {
	return &ast.BinaryExpr{Op: op, Left: l, Right: r}
}
func xlit(v any) *ast.LiteralExpr { return &ast.LiteralExpr{Value: v} }
func xnil() *ast.NilExpr          { return &ast.NilExpr{} }
func xnot(x ast.ExpressionNode) *ast.UnaryExpr {
	return &ast.UnaryExpr{Op: "!", Operand: x}
}
func xneg(x ast.ExpressionNode) *ast.UnaryExpr {
	return &ast.UnaryExpr{Op: "-", Operand: x}
}
func xcall(name string, args ...ast.ExpressionNode) *ast.CallExpr {
	return &ast.CallExpr{Name: name, Args: args}
}
func xmeth(recv ast.ExpressionNode, name string, args ...ast.ExpressionNode) *ast.CallExpr {
	return &ast.CallExpr{Receiver: recv, Name: name, Args: args}
}
func xlam(param string, body ast.ExpressionNode) *ast.LambdaExpr {
	return &ast.LambdaExpr{Params: []string{param}, Body: body}
}
func xlist(elems ...ast.ExpressionNode) *ast.ListExpr { return &ast.ListExpr{Elems: elems} }
func xtern(c, a, b ast.ExpressionNode) *ast.TernaryExpr {
	return &ast.TernaryExpr{Condition: c, Then: a, Else: b}
}
func xparen(x ast.ExpressionNode) *ast.ParenExpr { return &ast.ParenExpr{Inner: x} }

// xmap builds a map literal from key, value pairs.
func xmap(kv ...any) *ast.MapExpr {
	m := &ast.MapExpr{}
	for i := 0; i+1 < len(kv); i += 2 {
		m.Entries = append(m.Entries, ast.MapEntry{Key: kv[i].(string), Value: kv[i+1].(ast.ExpressionNode)})
	}
	return m
}

// xpath builds root.f1.f2...
func xpath(root string, fields ...string) ast.ExpressionNode {
	var n ast.ExpressionNode = xid(root)
	for _, f := range fields {
		n = xmem(n, f)
	}
	return n
}

// argsScope is the common scope: the caller's arguments under `args`.
func argsScope(args map[string]any) MapScope { return MapScope{"args": args} }

// ---------------------------------------------------------------------------
// expectations
// ---------------------------------------------------------------------------

// errCode is an expected ExprError code in a table whose other cells are
// values.
type errCode string

// exprCase is one expression, its scope, and what it must evaluate to.
type exprCase struct {
	name  string
	expr  ast.ExpressionNode
	scope ExprScope
	opts  EvalOptions
	// want is the value, the Absent sentinel, or an errCode.
	want any
}

func runExprCases(t *testing.T, cases []exprCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := EvalExpr(context.Background(), c.expr, c.scope, c.opts)
			checkExprResult(t, ast.FormatExpr(c.expr), got, err, c.want)
		})
	}
}

// checkExprResult compares one evaluation against its expectation, naming the
// source text on failure.
func checkExprResult(t *testing.T, src string, got any, err error, want any) {
	t.Helper()
	if code, isErr := want.(errCode); isErr {
		var ee *ExprError
		if !errors.As(err, &ee) {
			t.Fatalf("%s\n  want ExprError %q\n   got value %#v, err %v", src, code, got, err)
		}
		if ee.Code != string(code) {
			t.Fatalf("%s\n  want ExprError %q\n   got %q: %v", src, code, ee.Code, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("%s\n  want %#v\n   got error %v", src, want, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s\n  want %#v (%T)\n   got %#v (%T)", src, want, want, got, got)
	}
}

// ---------------------------------------------------------------------------
// THE ABSENCE TABLE (D8), row by row
// ---------------------------------------------------------------------------

// absenceSubject is one value of `x` the table is evaluated for. The last one
// is a nested read whose INTERMEDIATE is absent: `row.a.b` on a row with no
// `a`, which must behave exactly like an absent key.
type absenceSubject struct {
	name  string
	expr  ast.ExpressionNode
	scope MapScope
}

func absenceSubjects() []absenceSubject {
	x := xpath("args", "x")
	return []absenceSubject{
		{"absent key", x, argsScope(map[string]any{})},
		{"JSON null", x, argsScope(map[string]any{"x": nil})},
		{`""`, x, argsScope(map[string]any{"x": ""})},
		{`" "`, x, argsScope(map[string]any{"x": " "})},
		// A decoded payload number is a float64; the literal it is compared
		// with is an int64. Numbers compare numerically across the two.
		{"0", x, argsScope(map[string]any{"x": float64(0)})},
		{"false", x, argsScope(map[string]any{"x": false})},
		{`"é"`, x, argsScope(map[string]any{"x": "é"})},
		{"row.a.b with a absent", xpath("row", "a", "b"), MapScope{"row": ExprRow{Payload: map[string]any{}}}},
	}
}

// absenceColumn is one operator applied to x, with the expected answer for
// each subject in absenceSubjects order.
type absenceColumn struct {
	build func(x ast.ExpressionNode) ast.ExpressionNode
	want  []any
}

func TestEvalExprAbsenceTable(t *testing.T) {
	const T, F = true, false
	notBool := errCode("condition_not_boolean")
	notList := errCode("in_requires_list")
	notColl := errCode("operand_type")

	columns := []absenceColumn{
		// `== nil` / `!= nil` are the PRESENCE test: the lowering emits IS NULL
		// / IS NOT NULL, so a blank string is PRESENT. That is why the codemod
		// rewrites `exists(x)` -- which also treated a blank as absent -- to
		// `(x != nil && x != "")` rather than to `x != nil`.
		//              absent null  ""   " "  0    false "é"  nested
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin("==", x, xnil()) },
			[]any{T, T, F, F, F, F, F, T}},
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin("!=", x, xnil()) },
			[]any{F, F, T, T, T, T, T, F}},
		// `== ""`: an absent value EQUALS blank (rule 27, completed by D8:
		// COALESCE(x, '') = ''). " " is not blank to `==` -- strings compare
		// verbatim; only `??` trims.
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin("==", x, xlit("")) },
			[]any{T, T, T, F, F, F, F, T}},
		// `!= ""` is the "is set" idiom (#1708 / #1714): FALSE on absent.
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin("!=", x, xlit("")) },
			[]any{F, F, F, T, T, T, T, F}},
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin("==", x, xlit("é")) },
			[]any{F, F, F, F, F, F, T, F}},
		// `!=` on a concrete value is null-safe (#1685, IS DISTINCT FROM):
		// TRUE on absent.
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin("!=", x, xlit("é")) },
			[]any{T, T, T, T, T, T, F, T}},
		// Unicode is compared by bytes, never folded: "é" is not "e".
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin("==", x, xlit("e")) },
			[]any{F, F, F, F, F, F, F, F}},
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin("==", x, xlit(int64(0))) },
			[]any{F, F, F, F, T, F, F, F}},
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin("!=", x, xlit(int64(0))) },
			[]any{T, T, T, T, F, T, T, T}},
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin("==", x, xlit(false)) },
			[]any{F, F, F, F, F, T, F, F}},
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin("!=", x, xlit(false)) },
			[]any{T, T, T, T, T, F, T, T}},
		// Ordered comparisons: false on absent and on a type mismatch; strings
		// order by BYTE, so "é" (0xC3 0xA9) sorts after "f" and after "e".
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin("<", x, xlit("f")) },
			[]any{F, F, T, T, F, F, F, F}},
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin(">", x, xlit("e")) },
			[]any{F, F, F, F, F, F, T, F}},
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin(">=", x, xlit("")) },
			[]any{F, F, T, T, F, F, T, F}},
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin("<", x, xlit(int64(1))) },
			[]any{F, F, F, F, T, F, F, F}},
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin(">=", x, xlit(int64(0))) },
			[]any{F, F, F, F, T, F, F, F}},
		// `x in list`: an absent LHS is never a member.
		{func(x ast.ExpressionNode) ast.ExpressionNode {
			return xbin("in", x, xlist(xlit(""), xlit(" "), xlit(int64(0)), xlit(false), xlit("é")))
		}, []any{F, F, T, T, T, T, T, F}},
		// `v in x`: an absent list contains nothing; a present non-list is an
		// authoring error, whatever the LHS.
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin("in", xlit("é"), x) },
			[]any{F, F, notList, notList, notList, notList, notList, F}},
		// startsWith: the subject must be a string; a blank prefix is no
		// prefix at all (rule 32).
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin("startsWith", x, xlit("é")) },
			[]any{F, F, F, F, F, F, T, F}},
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin("startsWith", x, xlit("")) },
			[]any{F, F, F, F, F, F, F, F}},
		{func(x ast.ExpressionNode) ast.ExpressionNode {
			return xbin("startsWith", x, xlist(xlit(" "), xlit("")))
		}, []any{F, F, F, F, F, F, F, F}},
		// `!`: absent is false, so its negation is true; a non-boolean is
		// refused, never read for truthiness.
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xnot(x) },
			[]any{T, T, notBool, notBool, notBool, T, notBool, T}},
		// `??` falls through on absent, null and BLANK (rule 30); false and 0
		// are values.
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xbin("??", x, xlit("D")) },
			[]any{"D", "D", "D", "D", float64(0), false, "é", "D"}},
		// `.count()` on an absent receiver is 0; on a string it counts runes
		// (so "é" is 1, not 2 bytes); a number is not a collection.
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xmeth(x, "count") },
			[]any{int64(0), int64(0), int64(0), int64(1), notColl, notColl, int64(1), int64(0)}},
		// `.?` propagates absence exactly as `.` does at run time.
		{func(x ast.ExpressionNode) ast.ExpressionNode { return xopt(x, "f") },
			[]any{Absent, Absent, Absent, Absent, Absent, Absent, Absent, Absent}},
	}

	subjects := absenceSubjects()
	for _, col := range columns {
		if len(col.want) != len(subjects) {
			t.Fatalf("%s: %d expectations for %d subjects", ast.FormatExpr(col.build(xid("x"))), len(col.want), len(subjects))
		}
		for i, s := range subjects {
			expr := col.build(s.expr)
			got, err := EvalExpr(context.Background(), expr, s.scope, EvalOptions{})
			t.Run(ast.FormatExpr(col.build(xid("x")))+"/"+s.name, func(t *testing.T) {
				checkExprResult(t, ast.FormatExpr(expr)+"  [x = "+s.name+"]", got, err, col.want[i])
			})
		}
	}
}

// For every pair from the value set, `x == v` and `x != v` are exact
// negations, and `==` is symmetric. Checked for x and v both read from the
// scope (the value rule) and for v written as a literal (where the nil
// literal is the presence test). A table cell can be wrong; this property
// cannot be satisfied by an evaluator that treats one operator as a
// special case of nothing.
func TestEvalExprEqualityIsNegationAndSymmetric(t *testing.T) {
	type named struct {
		name  string
		value any
		lit   ast.ExpressionNode // the value written as a literal
	}
	absentMarker := &struct{}{}
	values := []named{
		{"absent", absentMarker, xpath("args", "missing")},
		{"nil", nil, xnil()},
		{`""`, "", xlit("")},
		{`" "`, " ", xlit(" ")},
		{`"a"`, "a", xlit("a")},
		{`"é"`, "é", xlit("é")},
		{"0", int64(0), xlit(int64(0))},
		{"1", int64(1), xlit(int64(1))},
		{"1.0", float64(1), xlit(float64(1))},
		{"1.5", 1.5, xlit(1.5)},
		{"true", true, xlit(true)},
		{"false", false, xlit(false)},
		{"[1]", []any{int64(1)}, xlist(xlit(int64(1)))},
		{"{a: 1}", map[string]any{"a": int64(1)}, xmap("a", xlit(int64(1)))},
	}
	// evalBool evaluates a comparison tree through EvalExpr and requires a
	// boolean answer.
	evalBool := func(t *testing.T, n ast.ExpressionNode, scope ExprScope) bool {
		t.Helper()
		v, err := EvalExpr(context.Background(), n, scope, EvalOptions{})
		if err != nil {
			t.Fatalf("%s: %v", ast.FormatExpr(n), err)
		}
		b, ok := v.(bool)
		if !ok {
			t.Fatalf("%s: got %#v, want a bool", ast.FormatExpr(n), v)
		}
		return b
	}
	for _, x := range values {
		for _, v := range values {
			args := map[string]any{}
			if x.value != absentMarker {
				args["x"] = x.value
			}
			if v.value != absentMarker {
				args["v"] = v.value
			}
			scope := argsScope(args)
			ax, av := xpath("args", "x"), xpath("args", "v")

			eq := evalBool(t, xbin("==", ax, av), scope)
			ne := evalBool(t, xbin("!=", ax, av), scope)
			if eq == ne {
				t.Errorf("x=%s v=%s: (x == v) = %v and (x != v) = %v; they must be negations", x.name, v.name, eq, ne)
			}
			if rev := evalBool(t, xbin("==", av, ax), scope); rev != eq {
				t.Errorf("x=%s v=%s: (x == v) = %v but (v == x) = %v; equality must be symmetric", x.name, v.name, eq, rev)
			}

			leq := evalBool(t, xbin("==", ax, v.lit), scope)
			lne := evalBool(t, xbin("!=", ax, v.lit), scope)
			if leq == lne {
				t.Errorf("x=%s, %s: == gives %v and != gives %v; they must be negations",
					x.name, ast.FormatExpr(v.lit), leq, lne)
			}
			if rev := evalBool(t, xbin("==", v.lit, ax), scope); rev != leq {
				t.Errorf("x=%s: (x == %s) = %v but (%s == x) = %v; equality must be symmetric",
					x.name, ast.FormatExpr(v.lit), leq, ast.FormatExpr(v.lit), rev)
			}
		}
	}
}

// The one place the table distinguishes the nil LITERAL from a null VALUE,
// pinned by name so nobody "simplifies" it. `x == nil` lowers to IS NULL, a
// presence test, so a blank is present; a null value on either side of `==`
// is compared by the table, where absent equals blank.
func TestEvalExprNilLiteralIsAPresenceTest(t *testing.T) {
	runExprCases(t, []exprCase{
		{"a blank is present", xbin("==", xpath("args", "x"), xnil()),
			argsScope(map[string]any{"x": ""}), EvalOptions{}, false},
		{"a null value equals a blank", xbin("==", xpath("args", "x"), xpath("args", "v")),
			argsScope(map[string]any{"x": "", "v": nil}), EvalOptions{}, true},
		{"an absent value equals a blank", xbin("==", xpath("args", "x"), xpath("args", "missing")),
			argsScope(map[string]any{"x": ""}), EvalOptions{}, true},
		{"nil equals nil", xbin("==", xnil(), xnil()), nil, EvalOptions{}, true},
		{"a parenthesised nil is still the literal", xbin("==", xlit(""), xparen(xnil())), nil, EvalOptions{}, false},
	})
}

// Typed equality: a number is never equal to its text, and numbers compare
// numerically across every Go number kind.
func TestEvalExprTypedEquality(t *testing.T) {
	scope := argsScope(map[string]any{
		"i":   1,
		"i8":  int8(1),
		"u":   uint64(1),
		"f":   float64(1),
		"n":   json.Number("1"),
		"big": int64(9007199254740993), // 2^53 + 1: not representable as a float64
		"s":   "1",
		"b":   true,
		"xs":  []any{int64(1), "a", []any{int64(2)}},
		"m":   map[string]any{"a": []any{int64(1)}},
	})
	runExprCases(t, []exprCase{
		{"int == int8", xbin("==", xpath("args", "i"), xpath("args", "i8")), scope, EvalOptions{}, true},
		{"int == uint64", xbin("==", xpath("args", "i"), xpath("args", "u")), scope, EvalOptions{}, true},
		{"int == float", xbin("==", xpath("args", "i"), xpath("args", "f")), scope, EvalOptions{}, true},
		{"json.Number == int", xbin("==", xpath("args", "n"), xlit(int64(1))), scope, EvalOptions{}, true},
		{"1 == \"1\" is false", xbin("==", xlit(int64(1)), xlit("1")), scope, EvalOptions{}, false},
		{"a number is not its text", xbin("==", xpath("args", "i"), xpath("args", "s")), scope, EvalOptions{}, false},
		{"true is not 1", xbin("==", xpath("args", "b"), xlit(int64(1))), scope, EvalOptions{}, false},
		{"2^53+1 is not the float 2^53", xbin("==", xpath("args", "big"), xlit(float64(9007199254740992))), scope, EvalOptions{}, false},
		{"2^53+1 orders above the float 2^53", xbin(">", xpath("args", "big"), xlit(float64(9007199254740992))), scope, EvalOptions{}, true},
		{"deep list equality", xbin("==", xpath("args", "xs"), xlist(xlit(float64(1)), xlit("a"), xlist(xlit(int64(2))))), scope, EvalOptions{}, true},
		{"deep list inequality", xbin("==", xpath("args", "xs"), xlist(xlit(int64(1)), xlit("a"))), scope, EvalOptions{}, false},
		{"deep map equality", xbin("==", xpath("args", "m"), xmap("a", xlist(xlit(int64(1))))), scope, EvalOptions{}, true},
		{"ordering across kinds is false", xbin("<", xlit("1"), xlit(int64(2))), scope, EvalOptions{}, false},
		{"bools do not order", xbin("<", xlit(false), xlit(true)), scope, EvalOptions{}, false},
		{"1 in [1.0]", xbin("in", xlit(int64(1)), xlist(xlit(float64(1)))), scope, EvalOptions{}, true},
		{`"1" in [1]`, xbin("in", xlit("1"), xlist(xlit(int64(1)))), scope, EvalOptions{}, false},
		{"a blank is not a member of [nil]", xbin("in", xlit(""), xlist(xnil())), scope, EvalOptions{}, false},
		{"membership in a typed slice", xbin("in", xlit("b"), xpath("args", "tags")),
			argsScope(map[string]any{"tags": []string{"a", "b"}}), EvalOptions{}, true},
		{"in over a map is refused", xbin("in", xlit("a"), xpath("args", "m")), scope, EvalOptions{}, errCode("in_requires_list")},
	})
}

// ---------------------------------------------------------------------------
// `??` -- rule 30, verbatim, and the one selection rule it shares
// ---------------------------------------------------------------------------

// Rule 30's table (docs/public/language/authoring-rules.md): `args.v ??
// "DEFAULT"` for each value the caller may pass. Every row is also checked
// against coalesceSelect, THE selection rule both spellings of the operator
// resolve through (memql#3627), so the new evaluator cannot become a third
// implementation that disagrees.
func TestEvalExprCoalesceIsRule30(t *testing.T) {
	for _, row := range []struct {
		name string
		v    any
		want any
	}{
		{"false -> false kept", false, false},
		{"0 -> 0 kept", int64(0), int64(0)},
		{"[] -> [] kept", []any{}, []any{}},
		{"{} -> {} kept", map[string]any{}, map[string]any{}},
		{`"" -> "DEFAULT" rewritten`, "", "DEFAULT"},
		{`" " -> "DEFAULT" rewritten`, " ", "DEFAULT"},
		{`"\t\n" -> "DEFAULT" rewritten`, "\t\n", "DEFAULT"},
		{`"value" -> "value" kept`, "value", "value"},
	} {
		t.Run(row.name, func(t *testing.T) {
			expr := xbin("??", xpath("args", "v"), xlit("DEFAULT"))
			got, err := EvalExpr(context.Background(), expr, argsScope(map[string]any{"v": row.v}), EvalOptions{})
			checkExprResult(t, ast.FormatExpr(expr), got, err, row.want)

			shared, err := coalesceSelect(2, func(i int) (any, error) {
				if i == 0 {
					return row.v, nil
				}
				return "DEFAULT", nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, shared) {
				t.Fatalf("%s: EvalExpr gives %#v, coalesceSelect gives %#v -- one rule, one answer", ast.FormatExpr(expr), got, shared)
			}
		})
	}
}

// A `??` chain folds left (`a ?? b ?? c` is `(a ?? b) ?? c`), and the fold
// must select exactly what coalesceSelect selects over the n arms -- including
// the two rows memql#3627 fixed: a blank middle arm, and a missing final arm
// normalised to nil rather than leaked.
func TestEvalExprCoalesceChainIsTheOneSelectionRule(t *testing.T) {
	a, b, c := xpath("args", "a"), xpath("args", "b"), xpath("args", "c")
	for _, tc := range []struct {
		name string
		arms []ast.ExpressionNode
		args map[string]any
	}{
		{"blank middle arm, nothing else resolves", []ast.ExpressionNode{a, xlit(""), c}, map[string]any{}},
		{"blank middle arm, later arm wins", []ast.ExpressionNode{a, xlit(""), c}, map[string]any{"c": "C"}},
		{"blank final arm is the default", []ast.ExpressionNode{a, xlit("")}, map[string]any{}},
		{"first non-blank wins", []ast.ExpressionNode{a, xlit("B"), xlit("C")}, map[string]any{}},
		{"present value wins", []ast.ExpressionNode{a, xlit("B")}, map[string]any{"a": "A"}},
		{"blank present value is skipped", []ast.ExpressionNode{a, xlit("B")}, map[string]any{"a": "  "}},
		{"all arms missing", []ast.ExpressionNode{a, b}, map[string]any{}},
		{"all three missing", []ast.ExpressionNode{a, b, c}, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expr := tc.arms[0]
			for _, arm := range tc.arms[1:] {
				expr = xbin("??", expr, arm)
			}
			scope := argsScope(tc.args)
			got, err := EvalExpr(context.Background(), expr, scope, EvalOptions{})
			if err != nil {
				t.Fatalf("%s: %v", ast.FormatExpr(expr), err)
			}
			want, err := coalesceSelect(len(tc.arms), func(i int) (any, error) {
				v, err := EvalExpr(context.Background(), tc.arms[i], scope, EvalOptions{})
				if IsAbsent(v) && v != nil {
					return missingValue{}, err
				}
				return v, err
			})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s with %v: EvalExpr %#v, coalesceSelect %#v", ast.FormatExpr(expr), tc.args, got, want)
			}
			if got == Absent {
				t.Fatalf("%s: the Absent sentinel leaked out of ??", ast.FormatExpr(expr))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// operators: precedence-bearing trees, `+`, arithmetic, ternary
// ---------------------------------------------------------------------------

// Each tree is the shape the parser produces for the text FormatExpr prints,
// so evaluating it pins what the precedence table MEANS.
func TestEvalExprPrecedenceTrees(t *testing.T) {
	one, two, three := xlit(int64(1)), xlit(int64(2)), xlit(int64(3))
	scope := argsScope(map[string]any{"s": "active", "p": true, "q": false})
	runExprCases(t, []exprCase{
		{"times binds tighter than plus", xbin("+", one, xbin("*", two, three)), scope, EvalOptions{}, int64(7)},
		{"parens override", xbin("*", xparen(xbin("+", one, two)), three), scope, EvalOptions{}, int64(9)},
		{"minus is left-associative", xbin("-", xbin("-", xlit(int64(10)), xlit(int64(4))), three), scope, EvalOptions{}, int64(3)},
		{"unary minus binds tighter than times", xbin("*", xneg(two), three), scope, EvalOptions{}, int64(-6)},
		{"and binds tighter than or", xbin("||", xlit(true), xbin("&&", xlit(false), xlit(false))), scope, EvalOptions{}, true},
		{"or under and when parenthesised", xbin("&&", xparen(xbin("||", xlit(true), xlit(false))), xlit(false)), scope, EvalOptions{}, false},
		{"?? binds tighter than ==", xbin("==", xbin("??", xpath("args", "missing"), xlit("active")), xpath("args", "s")), scope, EvalOptions{}, true},
		{"! over a comparison", xnot(xparen(xbin("==", one, two))), scope, EvalOptions{}, true},
		{"ternary chains in the else branch", xtern(xpath("args", "q"), xlit("a"), xtern(xpath("args", "p"), xlit("b"), xlit("c"))), scope, EvalOptions{}, "b"},
		{"a ternary evaluates only the chosen branch", xtern(xlit(true), xlit("ok"), xcall("error", xlit("the other branch ran"))), scope, EvalOptions{}, "ok"},
		{"a ternary on an absent condition takes else", xtern(xpath("args", "missing"), xlit("a"), xlit("b")), scope, EvalOptions{}, "b"},
		{"a ternary on a string condition is refused", xtern(xpath("args", "s"), xlit("a"), xlit("b")), scope, EvalOptions{}, errCode("condition_not_boolean")},
		{"a comparison inside a ternary condition", xtern(xbin(">", xlit(int64(5)), three), xlit("big"), xlit("small")), scope, EvalOptions{}, "big"},
	})
}

// `+` is the old concat() when either side is a string: the other side's
// canonical text, absent contributing "". That is what makes the codemod's
// concat(a, b) -> a + b exact.
func TestEvalExprPlus(t *testing.T) {
	days := xpath("args", "days")
	duration := xbin("+", xbin("+", xlit("PT-"), xparen(xbin("??", days, xlit("90")))), xlit("S"))
	runExprCases(t, []exprCase{
		{"PT-(days ?? 90)S with days absent", duration, argsScope(map[string]any{}), EvalOptions{}, "PT-90S"},
		{"PT-(days ?? 90)S with days 7", duration, argsScope(map[string]any{"days": int64(7)}), EvalOptions{}, "PT-7S"},
		{"PT-(days ?? 90)S with a decoded 7", duration, argsScope(map[string]any{"days": float64(7)}), EvalOptions{}, "PT-7S"},
		{"PT-(days ?? 90)S with days blank", duration, argsScope(map[string]any{"days": " "}), EvalOptions{}, "PT-90S"},
		{"string + string", xbin("+", xlit("a"), xlit("b")), nil, EvalOptions{}, "ab"},
		{"string + int", xbin("+", xlit("n-"), xlit(int64(42))), nil, EvalOptions{}, "n-42"},
		{"float + string prints without an exponent", xbin("+", xlit(1.5e-7), xlit("")), nil, EvalOptions{}, "0.00000015"},
		{"string + bool", xbin("+", xlit("is "), xlit(true)), nil, EvalOptions{}, "is true"},
		{"absent + string", xbin("+", xpath("args", "missing"), xlit("x")), argsScope(map[string]any{}), EvalOptions{}, "x"},
		{"string + nil", xbin("+", xlit("x"), xnil()), nil, EvalOptions{}, "x"},
		{"string + list is its JSON text", xbin("+", xlit("ids="), xlist(xlit(int64(1)), xlit("<a>"))), nil, EvalOptions{}, `ids=[1,"<a>"]`},
		{"int + int is int64", xbin("+", xlit(int64(2)), xlit(int64(3))), nil, EvalOptions{}, int64(5)},
		{"int + float is float64", xbin("+", xlit(int64(2)), xlit(0.5)), nil, EvalOptions{}, 2.5},
		{"list + list", xbin("+", xlist(xlit(int64(1))), xlist(xlit("a"))), nil, EvalOptions{}, []any{int64(1), "a"}},
		{"number + absent is refused", xbin("+", xlit(int64(1)), xpath("args", "missing")), argsScope(map[string]any{}), EvalOptions{}, errCode("operand_type")},
		{"absent + absent is refused", xbin("+", xpath("args", "a"), xpath("args", "b")), argsScope(map[string]any{}), EvalOptions{}, errCode("operand_type")},
		{"list + number is refused", xbin("+", xlist(), xlit(int64(1))), nil, EvalOptions{}, errCode("operand_type")},
		{"map + map is refused", xbin("+", xmap(), xmap()), nil, EvalOptions{}, errCode("operand_type")},
		{"bool + number is refused", xbin("+", xlit(true), xlit(int64(1))), nil, EvalOptions{}, errCode("operand_type")},
	})
}

func TestEvalExprArithmetic(t *testing.T) {
	maxInt := xlit(int64(9223372036854775807))
	runExprCases(t, []exprCase{
		{"integer division truncates", xbin("/", xlit(int64(10)), xlit(int64(4))), nil, EvalOptions{}, int64(2)},
		{"a float operand divides as floats", xbin("/", xlit(int64(10)), xlit(4.0)), nil, EvalOptions{}, 2.5},
		{"a decoded payload number divides as a float", xbin("/", xpath("args", "n"), xlit(int64(4))),
			argsScope(map[string]any{"n": float64(10)}), EvalOptions{}, 2.5},
		{"modulo", xbin("%", xlit(int64(7)), xlit(int64(3))), nil, EvalOptions{}, int64(1)},
		{"modulo on a decoded whole number", xbin("%", xpath("args", "n"), xlit(int64(3))),
			argsScope(map[string]any{"n": float64(7)}), EvalOptions{}, int64(1)},
		{"modulo on a fraction is refused", xbin("%", xlit(7.5), xlit(int64(2))), nil, EvalOptions{}, errCode("operand_type")},
		{"times", xbin("*", xlit(int64(6)), xlit(int64(7))), nil, EvalOptions{}, int64(42)},
		{"minus", xbin("-", xlit(1.5), xlit(int64(1))), nil, EvalOptions{}, 0.5},
		{"division by zero", xbin("/", xlit(int64(1)), xlit(int64(0))), nil, EvalOptions{}, errCode("division_by_zero")},
		{"float division by zero", xbin("/", xlit(1.0), xlit(0.0)), nil, EvalOptions{}, errCode("division_by_zero")},
		{"modulo by zero", xbin("%", xlit(int64(5)), xlit(int64(0))), nil, EvalOptions{}, errCode("division_by_zero")},
		{"minus on a string is refused", xbin("-", xlit("a"), xlit(int64(1))), nil, EvalOptions{}, errCode("operand_type")},
		{"times on absent is refused", xbin("*", xpath("args", "missing"), xlit(int64(2))), argsScope(map[string]any{}), EvalOptions{}, errCode("operand_type")},
		{"an overflowing sum is refused, never wrapped", xbin("+", maxInt, xlit(int64(1))), nil, EvalOptions{}, errCode("arithmetic_overflow")},
		{"an overflowing product is refused", xbin("*", maxInt, xlit(int64(2))), nil, EvalOptions{}, errCode("arithmetic_overflow")},
		{"a float product that overflows to infinity is refused", xbin("*", xlit(1e308), xlit(10.0)), nil, EvalOptions{}, errCode("arithmetic_overflow")},
		{"unary minus on an int", xneg(xlit(int64(5))), nil, EvalOptions{}, int64(-5)},
		{"unary minus on a float", xneg(xlit(1.5)), nil, EvalOptions{}, -1.5},
		{"unary minus on a string is refused", xneg(xlit("a")), nil, EvalOptions{}, errCode("operand_type")},
		{"unary minus on absent is refused", xneg(xpath("args", "missing")), argsScope(map[string]any{}), EvalOptions{}, errCode("operand_type")},
		{"unary minus on the smallest int is refused", xneg(xbin("-", xneg(maxInt), xlit(int64(1)))), nil, EvalOptions{}, errCode("arithmetic_overflow")},
	})
}

// ---------------------------------------------------------------------------
// conditions: booleans only
// ---------------------------------------------------------------------------

func TestEvalConditionRefusesWhatIsNotBoolean(t *testing.T) {
	scope := argsScope(map[string]any{"flag": true, "n": int64(1), "s": "yes"})
	for _, tc := range []struct {
		name string
		expr ast.ExpressionNode
		want any
	}{
		{`the string "yes" is not a condition`, xlit("yes"), errCode("condition_not_boolean")},
		{"a number is not a condition", xlit(int64(1)), errCode("condition_not_boolean")},
		{"a list is not a condition", xlist(xlit(true)), errCode("condition_not_boolean")},
		{"true", xlit(true), true},
		{"a boolean argument", xpath("args", "flag"), true},
		{"an absent value is false", xpath("args", "missing"), false},
		{"nil is false", xnil(), false},
		{"&& refuses a number operand", xbin("&&", xpath("args", "n"), xlit(true)), errCode("condition_not_boolean")},
		{"|| refuses a string operand on the right", xbin("||", xlit(false), xpath("args", "s")), errCode("condition_not_boolean")},
		{"an absent && operand is false", xbin("&&", xlit(true), xpath("args", "missing")), false},
		{"an absent || operand is false", xbin("||", xpath("args", "missing"), xlit(true)), true},
		{"&& short-circuits", xbin("&&", xlit(false), xcall("error", xlit("evaluated"))), false},
		{"|| short-circuits", xbin("||", xlit(true), xcall("error", xlit("evaluated"))), true},
		{"&& does not short-circuit past a refusal", xbin("&&", xpath("args", "s"), xlit(false)), errCode("condition_not_boolean")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalCondition(context.Background(), tc.expr, scope, EvalOptions{})
			checkExprResult(t, ast.FormatExpr(tc.expr), got, err, tc.want)
		})
	}

	// The refusal names the type and the text, so an author can find it.
	_, err := EvalCondition(context.Background(), xbin("&&", xpath("args", "n"), xlit(true)), scope, EvalOptions{})
	if err == nil || !strings.Contains(err.Error(), "args.n") || !strings.Contains(err.Error(), "number") {
		t.Fatalf("the refusal must name the operand's text and type, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// names, rows, members
// ---------------------------------------------------------------------------

func TestEvalExprNames(t *testing.T) {
	clock := time.Date(2026, 9, 13, 12, 0, 0, 123, time.FixedZone("X", 3600))
	runExprCases(t, []exprCase{
		{"now is the clock in UTC, RFC3339Nano", xid("now"), nil, EvalOptions{Now: clock}, "2026-09-13T11:00:00.000000123Z"},
		{"a scope that binds now wins", xid("now"), MapScope{"now": "pinned"}, EvalOptions{Now: clock}, "pinned"},
		{"an unknown name is refused", xid("nope"), MapScope{"args": map[string]any{}}, EvalOptions{}, errCode("unknown_name")},
		{"a nil scope resolves nothing", xid("args"), nil, EvalOptions{}, errCode("unknown_name")},
		{"a missing key is Absent", xpath("args", "missing"), argsScope(map[string]any{}), EvalOptions{}, Absent},
		{"a JSON null is nil", xpath("args", "x"), argsScope(map[string]any{"x": nil}), EvalOptions{}, nil},
		{"a member of absent is absent", xpath("args", "a", "b", "c"), argsScope(map[string]any{}), EvalOptions{}, Absent},
		{"a member of a string is absent", xpath("args", "s", "f"), argsScope(map[string]any{"s": "x"}), EvalOptions{}, Absent},
		{"a list index", xpath("args", "xs", "1"), argsScope(map[string]any{"xs": []any{"a", "b"}}), EvalOptions{}, "b"},
		{"a negative list index counts from the end", xpath("args", "xs", "-1"), argsScope(map[string]any{"xs": []any{"a", "b"}}), EvalOptions{}, "b"},
		{"an index out of range is absent", xpath("args", "xs", "2"), argsScope(map[string]any{"xs": []any{"a", "b"}}), EvalOptions{}, Absent},
		{"a bare lambda is not callable", xlam("x", xid("x")), nil, EvalOptions{}, errCode("lambda_not_callable")},
		{"a node outside edition 2026 is refused", &ast.ComparisonExpr{}, nil, EvalOptions{}, errCode("unsupported_node")},
	})
}

// ExprRow: the six intrinsics read the row's own columns; every other name
// reads the payload -- including a payload key that happens to be spelled
// like an intrinsic, which the intrinsic shadows exactly as the SQL column
// does.
func TestExprRowIntrinsicsAndPayload(t *testing.T) {
	at := time.Date(2026, 9, 13, 10, 30, 0, 5, time.FixedZone("X", -7200))
	row := ExprRow{
		ID:         "v1:identity:user:u-1",
		Concept:    "v1:identity:user",
		Type:       "object",
		CreatedBy:  "v1:identity:user:admin",
		CreatedAt:  at,
		Provenance: map[string]any{"kind": "seed", "name": "bootstrap"},
		Payload:    map[string]any{"id": "payload-id", "status": "active", "profile": map[string]any{"age": float64(30)}},
	}
	scope := MapScope{"row": row, "rowPtr": &row}
	runExprCases(t, []exprCase{
		{"id is the intrinsic, not the payload key", xpath("row", "id"), scope, EvalOptions{}, "v1:identity:user:u-1"},
		{"concept", xpath("row", "concept"), scope, EvalOptions{}, "v1:identity:user"},
		{"type", xpath("row", "type"), scope, EvalOptions{}, "object"},
		{"createdBy", xpath("row", "createdBy"), scope, EvalOptions{}, "v1:identity:user:admin"},
		{"createdAt is RFC3339Nano in UTC", xpath("row", "createdAt"), scope, EvalOptions{}, "2026-09-13T12:30:00.000000005Z"},
		// Case-insensitive like the SQL twin's intrinsic registry
		// (resolveIntrinsicField), so both evaluators map one spelling to
		// one column.
		{"intrinsic names resolve as the SQL side resolves them", xpath("row", "createdat"), scope, EvalOptions{}, "2026-09-13T12:30:00.000000005Z"},
		{"provenance leaf", xpath("row", "provenance", "kind"), scope, EvalOptions{}, "seed"},
		{"payload field", xpath("row", "status"), scope, EvalOptions{}, "active"},
		{"nested payload field", xpath("row", "profile", "age"), scope, EvalOptions{}, float64(30)},
		{"missing payload field", xpath("row", "nope"), scope, EvalOptions{}, Absent},
		{"a pointer row reads the same", xpath("rowPtr", "status"), scope, EvalOptions{}, "active"},
		{"a row with no provenance", xpath("row", "provenance", "kind"), MapScope{"row": ExprRow{}}, EvalOptions{}, Absent},
		{"a row with no createdAt", xpath("row", "createdAt"), MapScope{"row": ExprRow{}}, EvalOptions{}, Absent},
		{"a row compares", xbin("==", xpath("row", "profile", "age"), xlit(int64(30))), scope, EvalOptions{}, true},
	})
}

// NewExprRow builds the row EvalExpr reads from the stored node, so the
// differential lane and `refine` see what the SQL side sees.
func TestNewExprRowFromAStoredNode(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	row, err := NewExprRow(memorynodes.MemoryNode{
		ID:         "v1:a:b:c",
		Concept:    "v1:a:b",
		Type:       "object",
		CreatedBy:  "v1:identity:user:u",
		CreatedAt:  at,
		Payload:    json.RawMessage(`{"status":"active","n":2}`),
		Provenance: json.RawMessage(`{"kind":"mutation"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := ExprRow{
		ID: "v1:a:b:c", Concept: "v1:a:b", Type: "object", CreatedBy: "v1:identity:user:u", CreatedAt: at,
		Provenance: map[string]any{"kind": "mutation"},
		Payload:    map[string]any{"status": "active", "n": float64(2)},
	}
	if !reflect.DeepEqual(row, want) {
		t.Fatalf("NewExprRow:\n got %#v\nwant %#v", row, want)
	}
	empty, err := NewExprRow(memorynodes.MemoryNode{ID: "v1:a:b:d"})
	if err != nil || empty.Payload != nil || empty.Provenance != nil {
		t.Fatalf("an empty payload and provenance read as nil maps, got %#v, %v", empty, err)
	}
	if _, err := NewExprRow(memorynodes.MemoryNode{Payload: json.RawMessage(`{`)}); err == nil {
		t.Fatal("a malformed payload must be an error, not an empty row")
	}
}

// Values the scope carries in Go shapes other than the decoded-JSON ones are
// normalised before they are read: a typed slice of rows (the trap
// MaterializeRows walks into), typed maps, structs (by JSON round trip, as the
// automations resolver reads them) and raw JSON.
func TestEvalExprNormalisesScopeValues(t *testing.T) {
	type person struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	scope := argsScope(map[string]any{
		"rows":   []map[string]any{{"n": float64(1)}, {"n": float64(2)}},
		"tags":   []string{"a", "b"},
		"labels": map[string]string{"team": "core"},
		"counts": map[string]int{"open": 3},
		"person": person{Name: "Ada", Age: 36},
		"ptr":    &person{Name: "Bob"},
		"raw":    json.RawMessage(`{"k":[1,2]}`),
		"when":   time.Date(2026, 9, 13, 0, 0, 0, 0, time.FixedZone("X", 3600)),
		"nilptr": (*person)(nil),
		"nilmap": map[string]any(nil),
	})
	runExprCases(t, []exprCase{
		{"a typed row slice indexes", xpath("args", "rows", "1", "n"), scope, EvalOptions{}, float64(2)},
		{"a typed row slice is a collection", xmeth(xmeth(xpath("args", "rows"), "where", xlam("r", xbin(">", xpath("r", "n"), xlit(int64(1))))), "count"), scope, EvalOptions{}, int64(1)},
		{"a string slice is a collection", xmeth(xpath("args", "tags"), "count"), scope, EvalOptions{}, int64(2)},
		{"a typed string map", xpath("args", "labels", "team"), scope, EvalOptions{}, "core"},
		{"a typed int map", xbin("==", xpath("args", "counts", "open"), xlit(int64(3))), scope, EvalOptions{}, true},
		{"a struct reads by its JSON names", xpath("args", "person", "name"), scope, EvalOptions{}, "Ada"},
		{"a struct pointer", xpath("args", "ptr", "name"), scope, EvalOptions{}, "Bob"},
		{"raw JSON", xpath("args", "raw", "k", "1"), scope, EvalOptions{}, float64(2)},
		{"a time is an RFC3339Nano UTC string", xbin("==", xpath("args", "when"), xlit("2026-09-12T23:00:00Z")), scope, EvalOptions{}, true},
		{"a nil pointer is absent", xbin("==", xpath("args", "nilptr"), xnil()), scope, EvalOptions{}, true},
		{"a nil map is absent", xbin("==", xpath("args", "nilmap"), xnil()), scope, EvalOptions{}, true},
		{"a member of a nil pointer is absent", xpath("args", "nilptr", "name"), scope, EvalOptions{}, Absent},
	})
}

// ---------------------------------------------------------------------------
// containers: a missing value contributes nothing
// ---------------------------------------------------------------------------

func TestEvalExprContainersOmitAbsent(t *testing.T) {
	scope := argsScope(map[string]any{"b": "B"})
	missing := xpath("args", "missing")
	runExprCases(t, []exprCase{
		{"a map drops an absent value and keeps an explicit nil",
			xmap("a", missing, "b", xnil(), "c", xlit(int64(1))), scope, EvalOptions{},
			map[string]any{"b": nil, "c": int64(1)}},
		{"a list drops an absent element", xlist(missing, xlit("x")), scope, EvalOptions{}, []any{"x"}},
		{"a list keeps an explicit nil", xlist(xnil(), xpath("args", "b")), scope, EvalOptions{}, []any{nil, "B"}},
		{"nested containers", xmap("outer", xmap("inner", missing), "xs", xlist(missing)), scope, EvalOptions{},
			map[string]any{"outer": map[string]any{}, "xs": []any{}}},
		{"a member of absent inside a map", xmap("v", xpath("args", "missing", "deeper")), scope, EvalOptions{}, map[string]any{}},
		// `??` never yields the sentinel: its final arm is normalised to nil
		// exactly as coalesceSelect normalises it, so the key is KEPT as null
		// -- what the mutation template writes today.
		{"a ?? whose arms are all absent is a kept nil", xmap("v", xbin("??", missing, xpath("args", "other"))), scope, EvalOptions{},
			map[string]any{"v": nil}},
	})
}

// ---------------------------------------------------------------------------
// methods and lambdas
// ---------------------------------------------------------------------------

func TestEvalExprListMethods(t *testing.T) {
	xs := xpath("args", "xs")
	empty := xpath("args", "empty")
	missing := xpath("args", "missing")
	x := xid("x")
	people := []any{
		map[string]any{"name": "a", "team": "red"},
		map[string]any{"name": "b", "team": "blue"},
		map[string]any{"name": "c", "team": "red"},
	}
	scope := argsScope(map[string]any{
		"xs":       []any{int64(3), int64(1), int64(2)},
		"ys":       []any{int64(2), int64(9)},
		"empty":    []any{},
		"min":      int64(1),
		"people":   people,
		"dups":     []any{int64(1), "1", float64(1), int64(2)},
		"envelope": map[string]any{"nodes": []any{"n1", "n2"}},
		"one":      []any{int64(7)},
		"groups": []any{
			map[string]any{"members": []any{"ann", "bo"}},
			map[string]any{"members": []any{"cy"}},
		},
		"who": "bo",
	})
	identity := xlam("x", x)
	runExprCases(t, []exprCase{
		{"count", xmeth(xs, "count"), scope, EvalOptions{}, int64(3)},
		{"where with an outer argument in the lambda", xmeth(xmeth(xs, "where", xlam("x", xbin(">", x, xpath("args", "min")))), "count"), scope, EvalOptions{}, int64(2)},
		{"where keeps order", xmeth(xs, "where", xlam("x", xbin(">", x, xpath("args", "min")))), scope, EvalOptions{}, []any{int64(3), int64(2)}},
		{"select", xmeth(xs, "select", xlam("x", xbin("*", x, xlit(int64(2))))), scope, EvalOptions{}, []any{int64(6), int64(2), int64(4)}},
		{"select of a missing field is nil, keeping the length", xmeth(xpath("args", "people"), "select", xlam("p", xpath("p", "email"))), scope, EvalOptions{}, []any{nil, nil, nil}},
		{"any", xmeth(xs, "any", xlam("x", xbin("==", x, xlit(int64(2))))), scope, EvalOptions{}, true},
		{"any on an empty list", xmeth(empty, "any", xlam("x", xlit(true))), scope, EvalOptions{}, false},
		{"all", xmeth(xs, "all", xlam("x", xbin(">", x, xlit(int64(0))))), scope, EvalOptions{}, true},
		{"all fails", xmeth(xs, "all", xlam("x", xbin(">", x, xlit(int64(1))))), scope, EvalOptions{}, false},
		{"all on an empty list", xmeth(empty, "all", xlam("x", xlit(false))), scope, EvalOptions{}, true},
		{"first", xmeth(xs, "first"), scope, EvalOptions{}, int64(3)},
		// The predicate forms are retired: filter first.
		{"the first match is where(...).first()", xmeth(xmeth(xs, "where", xlam("x", xbin("<", x, xlit(int64(3))))), "first"), scope, EvalOptions{}, int64(1)},
		{"first of an empty list is nil", xmeth(empty, "first"), scope, EvalOptions{}, nil},
		{"first with no match is nil", xmeth(xmeth(xs, "where", xlam("x", xbin(">", x, xlit(int64(9))))), "first"), scope, EvalOptions{}, nil},
		{"last", xmeth(xs, "last"), scope, EvalOptions{}, int64(2)},
		{"the last match is where(...).last()", xmeth(xmeth(xs, "where", xlam("x", xbin(">", x, xlit(int64(1))))), "last"), scope, EvalOptions{}, int64(2)},
		{"last of an empty list is nil", xmeth(empty, "last"), scope, EvalOptions{}, nil},
		{"empty", xmeth(xs, "empty"), scope, EvalOptions{}, false},
		{"empty on an empty list", xmeth(empty, "empty"), scope, EvalOptions{}, true},
		{"sum", xmeth(xs, "sum", identity), scope, EvalOptions{}, float64(6)},
		{"sum of nothing is 0", xmeth(empty, "sum", identity), scope, EvalOptions{}, float64(0)},
		{"min", xmeth(xs, "min", identity), scope, EvalOptions{}, float64(1)},
		{"max", xmeth(xs, "max", identity), scope, EvalOptions{}, float64(3)},
		{"avg", xmeth(xs, "avg", identity), scope, EvalOptions{}, float64(2)},
		{"min of nothing is nil", xmeth(empty, "min", identity), scope, EvalOptions{}, nil},
		{"sum over strings is refused", xmeth(xpath("args", "people"), "sum", xlam("p", xpath("p", "name"))), scope, EvalOptions{}, errCode("operand_type")},
		{"orderBy", xmeth(xs, "orderBy", identity), scope, EvalOptions{}, []any{int64(1), int64(2), int64(3)}},
		{"orderByDesc", xmeth(xs, "orderByDesc", identity), scope, EvalOptions{}, []any{int64(3), int64(2), int64(1)}},
		{"groupBy keeps first-seen order", xmeth(xpath("args", "people"), "groupBy", xlam("p", xpath("p", "team"))), scope, EvalOptions{},
			[]any{
				map[string]any{"key": "red", "items": []any{people[0], people[2]}},
				map[string]any{"key": "blue", "items": []any{people[1]}},
			}},
		// Typed: 1 and 1.0 are one value, "1" is another.
		{"distinct is typed", xmeth(xpath("args", "dups"), "distinct"), scope, EvalOptions{}, []any{int64(1), "1", int64(2)}},
		{"distinct by a key", xmeth(xpath("args", "people"), "distinct", xlam("p", xpath("p", "team"))), scope, EvalOptions{}, []any{people[0], people[1]}},
		{"take", xmeth(xs, "take", xlit(int64(2))), scope, EvalOptions{}, []any{int64(3), int64(1)}},
		{"take past the end", xmeth(xs, "take", xlit(int64(10))), scope, EvalOptions{}, []any{int64(3), int64(1), int64(2)}},
		{"skip", xmeth(xs, "skip", xlit(int64(1))), scope, EvalOptions{}, []any{int64(1), int64(2)}},
		{"skip a negative count", xmeth(xs, "skip", xlit(int64(-1))), scope, EvalOptions{}, []any{int64(3), int64(1), int64(2)}},
		{"take a non-number is refused", xmeth(xs, "take", xlit("2")), scope, EvalOptions{}, errCode("invalid_argument")},
		{"nodes of a bundle envelope", xmeth(xpath("args", "envelope"), "nodes"), scope, EvalOptions{}, []any{"n1", "n2"}},
		{"nodes of a list", xmeth(xs, "nodes"), scope, EvalOptions{}, []any{int64(3), int64(1), int64(2)}},
		{"a single map is one element", xmeth(xpath("args", "people", "0"), "count"), scope, EvalOptions{}, int64(1)},
		{"reduce", xmeth(xs, "reduce", xlit(int64(0)), &ast.LambdaExpr{Params: []string{"acc", "x"}, Body: xbin("+", xid("acc"), x)}), scope, EvalOptions{}, int64(6)},
		{"reduce of nothing is the seed", xmeth(empty, "reduce", xlit("seed"), &ast.LambdaExpr{Params: []string{"acc", "x"}, Body: xid("x")}), scope, EvalOptions{}, "seed"},
		{"single", xmeth(xpath("args", "one"), "single"), scope, EvalOptions{}, int64(7)},
		{"single over many is refused", xmeth(xs, "single"), scope, EvalOptions{}, errCode("single_requires_one")},
		{"single over none is refused", xmeth(empty, "single"), scope, EvalOptions{}, errCode("single_requires_one")},
		{"the one match is where(...).single()", xmeth(xmeth(xs, "where", xlam("x", xbin("==", x, xlit(int64(1))))), "single"), scope, EvalOptions{}, int64(1)},

		// Nested lambdas: the inner lambda reads the outer one's parameter.
		{"an inner lambda reads the outer parameter", xmeth(xs, "where", xlam("x", xmeth(xpath("args", "ys"), "any", xlam("y", xbin("==", xid("y"), x))))), scope, EvalOptions{}, []any{int64(2)}},
		{"nested collections", xmeth(xmeth(xpath("args", "groups"), "where", xlam("g", xmeth(xpath("g", "members"), "any", xlam("m", xbin("==", xid("m"), xpath("args", "who")))))), "count"), scope, EvalOptions{}, int64(1)},
		{"a lambda parameter does not leak out of its method", xbin("&&", xmeth(xs, "any", xlam("x", xlit(true))), xbin("==", x, xlit(int64(1)))), scope, EvalOptions{}, errCode("unknown_name")},

		// An absent receiver: the brief's answers.
		{"count of absent", xmeth(missing, "count"), scope, EvalOptions{}, int64(0)},
		{"any of absent", xmeth(missing, "any", xlam("x", xlit(true))), scope, EvalOptions{}, false},
		{"all of absent", xmeth(missing, "all", xlam("x", xlit(false))), scope, EvalOptions{}, true},
		{"empty of absent", xmeth(missing, "empty"), scope, EvalOptions{}, true},
		{"first of absent is absent", xmeth(missing, "first"), scope, EvalOptions{}, Absent},
		{"last of absent is absent", xmeth(missing, "last"), scope, EvalOptions{}, Absent},
		{"where of absent is an empty list", xmeth(missing, "where", xlam("x", xlit(true))), scope, EvalOptions{}, []any{}},
		{"select of absent is an empty list", xmeth(missing, "select", identity), scope, EvalOptions{}, []any{}},
		{"nodes of absent is an empty list", xmeth(missing, "nodes"), scope, EvalOptions{}, []any{}},
		{"sum of absent is 0", xmeth(missing, "sum", identity), scope, EvalOptions{}, float64(0)},
		{"avg of absent is nil", xmeth(missing, "avg", identity), scope, EvalOptions{}, nil},
		{"reduce of absent is the seed", xmeth(missing, "reduce", xlit(int64(9)), &ast.LambdaExpr{Params: []string{"acc", "x"}, Body: xid("x")}), scope, EvalOptions{}, int64(9)},
		{"single of absent is refused", xmeth(missing, "single"), scope, EvalOptions{}, errCode("single_requires_one")},
		{"a malformed call is refused on an absent receiver too", xmeth(missing, "where"), scope, EvalOptions{}, errCode("argument_count")},

		// A call of the wrong shape is argument_count, checked against the
		// catalog's signature before the receiver is read.
		{"where needs a lambda", xmeth(xs, "where"), scope, EvalOptions{}, errCode("argument_count")},
		{"where of a non-lambda", xmeth(xs, "where", xlit(true)), scope, EvalOptions{}, errCode("argument_count")},
		{"any needs a predicate", xmeth(xs, "any"), scope, EvalOptions{}, errCode("argument_count")},
		{"a two-parameter lambda where one is expected", xmeth(xs, "where", &ast.LambdaExpr{Params: []string{"a", "b"}, Body: xlit(true)}), scope, EvalOptions{}, errCode("argument_count")},
		{"count(x => p) is retired", xmeth(xs, "count", xlam("x", xlit(true))), scope, EvalOptions{}, errCode("argument_count")},
		{"first(x => p) is retired", xmeth(xs, "first", xlam("x", xlit(true))), scope, EvalOptions{}, errCode("argument_count")},
		{"last(x => p) is retired", xmeth(xs, "last", xlam("x", xlit(true))), scope, EvalOptions{}, errCode("argument_count")},
		{"single(x => p) is retired", xmeth(xs, "single", xlam("x", xlit(true))), scope, EvalOptions{}, errCode("argument_count")},
		{"empty takes nothing", xmeth(xs, "empty", xlit(int64(1))), scope, EvalOptions{}, errCode("argument_count")},
		{"take a lambda is refused", xmeth(xs, "take", xlam("x", xlit(true))), scope, EvalOptions{}, errCode("argument_count")},
		{"reduce needs a two-parameter lambda", xmeth(xs, "reduce", xlit(int64(0)), xlam("x", x)), scope, EvalOptions{}, errCode("argument_count")},
		{"distinct takes at most a key", xmeth(xs, "distinct", identity, identity), scope, EvalOptions{}, errCode("argument_count")},
		{"a named argument is refused", &ast.CallExpr{Receiver: xs, Name: "count", Named: []ast.NamedArg{{Name: "n", Value: xlit(int64(1))}}}, scope, EvalOptions{}, errCode("argument_count")},

		// Other refusals.
		{"a lambda must return a boolean", xmeth(xs, "where", xlam("x", x)), scope, EvalOptions{}, errCode("condition_not_boolean")},
		{"the retired .contains(v) is not a method", xmeth(xs, "contains", xlit(int64(1))), scope, EvalOptions{}, errCode("unknown_method")},
		{"mean() is retired", xmeth(xs, "mean", identity), scope, EvalOptions{}, errCode("unknown_method")},
		{"a number is not a collection", xmeth(xlit(int64(5)), "any", xlam("x", xlit(true))), scope, EvalOptions{}, errCode("operand_type")},
		{"a string is not a list", xmeth(xlit("abc"), "first"), scope, EvalOptions{}, errCode("operand_type")},
	})
}

func TestEvalExprStringMethods(t *testing.T) {
	scope := argsScope(map[string]any{"s": "hello", "u": "héllo", "xs": []any{"a"}, "n": int64(123)})
	s := xpath("args", "s")
	runExprCases(t, []exprCase{
		{"includes", xmeth(s, "includes", xlit("ell")), scope, EvalOptions{}, true},
		{"includes misses", xmeth(s, "includes", xlit("xyz")), scope, EvalOptions{}, false},
		{"includes on an absent receiver", xmeth(xpath("args", "missing"), "includes", xlit("a")), scope, EvalOptions{}, false},
		{"an absent needle is false", xmeth(s, "includes", xpath("args", "missing")), scope, EvalOptions{}, false},
		// The catalog's contract is "whether sub occurs in the string", and
		// "" occurs in every string -- as in SQL's strpos, the twin a pushed-
		// down includes() lowers to.
		{"the empty string occurs in every string", xmeth(s, "includes", xlit("")), scope, EvalOptions{}, true},
		{"whitespace is an ordinary needle", xmeth(s, "includes", xlit(" ")), scope, EvalOptions{}, false},
		{"whitespace found", xmeth(xlit("hello world"), "includes", xlit(" ")), scope, EvalOptions{}, true},
		{"includes on a number is false", xmeth(xpath("args", "n"), "includes", xlit("2")), scope, EvalOptions{}, false},
		{"includes on a list is refused, naming membership", xmeth(xpath("args", "xs"), "includes", xlit("a")), scope, EvalOptions{}, errCode("operand_type")},
		{"a non-string needle is refused", xmeth(s, "includes", xlit(int64(1))), scope, EvalOptions{}, errCode("invalid_argument")},
		{"includes takes one argument", xmeth(s, "includes"), scope, EvalOptions{}, errCode("argument_count")},
		{"count is runes", xmeth(xpath("args", "u"), "count"), scope, EvalOptions{}, int64(5)},
		{"count takes no argument on a string", xmeth(s, "count", xlit(int64(1))), scope, EvalOptions{}, errCode("argument_count")},
		{"a list method on a string is refused", xmeth(s, "where", xlam("c", xlit(true))), scope, EvalOptions{}, errCode("operand_type")},
	})
}

// A predicate (a spec or trait) is applied to its receiver: its v1 body is
// evaluated with its parameter bound to the argument, and nothing else of the
// caller's -- a lambda parameter of the caller is not visible inside it.
func TestEvalExprPredicateApplication(t *testing.T) {
	predicates := func(name string) (string, ast.ExpressionNode, bool) {
		switch name {
		case "isActiveRecord":
			return "r", xbin("==", xpath("r", "active"), xlit(true)), true
		case "readsCallerLambda":
			return "r", xbin("==", xid("x"), xlit(int64(1))), true
		case "notBoolean":
			return "r", xpath("r", "name"), true
		case "selfRecursive":
			return "r", xcall("selfRecursive", xid("r")), true
		}
		return "", nil, false
	}
	opts := EvalOptions{Predicates: predicates}
	active := ExprRow{ID: "a", Payload: map[string]any{"active": true, "name": "A"}}
	inactive := ExprRow{ID: "b", Payload: map[string]any{"active": false}}
	scope := MapScope{"row": active, "other": inactive, "args": map[string]any{"rows": []any{active, inactive, active}}}
	runExprCases(t, []exprCase{
		{"a trait over a row", xcall("isActiveRecord", xid("row")), scope, opts, true},
		{"a trait over another row", xcall("isActiveRecord", xid("other")), scope, opts, false},
		{"a trait in a conjunction", xbin("&&", xbin("==", xpath("row", "name"), xlit("A")), xcall("isActiveRecord", xid("row"))), scope, opts, true},
		{"a trait inside a lambda", xmeth(xmeth(xpath("args", "rows"), "where", xlam("r", xcall("isActiveRecord", xid("r")))), "count"), scope, opts, int64(2)},
		{"a predicate cannot see the caller's lambda parameter", xmeth(xpath("args", "rows"), "any", xlam("x", xcall("readsCallerLambda", xid("x")))), scope, opts, errCode("unknown_name")},
		{"a predicate takes exactly one argument", xcall("isActiveRecord", xid("row"), xid("other")), scope, opts, errCode("argument_count")},
		{"a predicate's body must be boolean", xcall("notBoolean", xid("row")), scope, opts, errCode("condition_not_boolean")},
		{"a recursive predicate stops", xcall("selfRecursive", xid("row")), scope, opts, errCode("predicate_depth_exceeded")},
		{"an unknown function", xcall("isUnknown", xid("row")), scope, opts, errCode("unknown_function")},
		{"no predicate registry", xcall("isActiveRecord", xid("row")), scope, EvalOptions{}, errCode("unknown_function")},
	})
}

// ---------------------------------------------------------------------------
// the catalog functions
// ---------------------------------------------------------------------------

func TestEvalExprCatalogFunctions(t *testing.T) {
	sum := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return hex.EncodeToString(h[:])
	}
	clock := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	canon := func(_ context.Context, v any, concept string) (string, error) {
		return concept + ":" + fmt.Sprint(v), nil
	}
	vars := func(_ context.Context, kind, name string) (string, error) {
		return kind + "/" + name, nil
	}
	scope := argsScope(map[string]any{"id": "abc", "n": float64(5), "blank": "  "})
	missing := xpath("args", "missing")
	runExprCases(t, []exprCase{
		{"lower", xcall("lower", xlit("AbC")), scope, EvalOptions{}, "abc"},
		{"upper of absent is blank", xcall("upper", missing), scope, EvalOptions{}, ""},
		{"trim", xcall("trim", xlit("  x \t")), scope, EvalOptions{}, "x"},
		{"hash", xcall("hash", xlit("abc")), scope, EvalOptions{}, sum("abc")},
		{"hash of absent hashes the empty string", xcall("hash", missing), scope, EvalOptions{}, sum("")},
		{"hash of a decoded number hashes its canonical text", xcall("hash", xpath("args", "n")), scope, EvalOptions{}, sum("5")},
		{"shortId of a canonical id", xcall("shortId", xlit("v1:identity:user:abc")), scope, EvalOptions{}, "abc"},
		{"shortId of a bare id", xcall("shortId", xpath("args", "id")), scope, EvalOptions{}, "abc"},
		{"shortId of absent", xcall("shortId", missing), scope, EvalOptions{}, ""},
		{"canonicalId through the hook", xcall("canonicalId", xpath("args", "id"), xlit("v1:identity:user")), scope, EvalOptions{CanonicalID: canon}, "v1:identity:user:abc"},
		{"canonicalId with no hook is the identity", xcall("canonicalId", xpath("args", "id"), xlit("v1:identity:user")), scope, EvalOptions{}, "abc"},
		{"canonicalId of absent is blank", xcall("canonicalId", missing, xlit("v1:identity:user")), scope, EvalOptions{CanonicalID: canon}, ""},
		{"canonicalId of a blank is blank", xcall("canonicalId", xpath("args", "blank"), xlit("v1:identity:user")), scope, EvalOptions{CanonicalID: canon}, ""},
		{"canonicalId needs a concept", xcall("canonicalId", xpath("args", "id"), xlit("")), scope, EvalOptions{}, errCode("invalid_argument")},
		{"toString of a float", xcall("toString", xlit(1.5)), scope, EvalOptions{}, "1.5"},
		{"toString of a bool", xcall("toString", xlit(true)), scope, EvalOptions{}, "true"},
		{"toString of absent", xcall("toString", missing), scope, EvalOptions{}, ""},
		{"addDuration", xcall("addDuration", xlit("2026-01-01T00:00:00Z"), xlit("P1D")), scope, EvalOptions{}, "2026-01-02T00:00:00Z"},
		{"addDuration of a date", xcall("addDuration", xlit("2026-01-01"), xlit("PT36H")), scope, EvalOptions{}, "2026-01-02T12:00:00Z"},
		{"addDuration answers in UTC", xcall("addDuration", xlit("2026-01-01T00:00:00+02:00"), xlit("P1D")), scope, EvalOptions{}, "2026-01-01T22:00:00Z"},
		{"addDuration keeps fractional seconds", xcall("addDuration", xlit("2026-01-01T00:00:00.5Z"), xlit("PT1S")), scope, EvalOptions{}, "2026-01-01T00:00:01.5Z"},
		{"addDuration subtracts", xcall("addDuration", xid("now"), xlit("-P1D")), scope, EvalOptions{Now: clock}, "2026-09-12T12:00:00Z"},
		{"addDuration of absent is refused", xcall("addDuration", missing, xlit("P1D")), scope, EvalOptions{}, errCode("invalid_argument")},
		{"addDuration of a bad timestamp is refused", xcall("addDuration", xlit("soon"), xlit("P1D")), scope, EvalOptions{}, errCode("invalid_argument")},
		{"daysBetween", xcall("daysBetween", xlit("2026-01-01"), xlit("2026-01-31")), scope, EvalOptions{}, int64(30)},
		{"error raises", xcall("error", xlit("boom")), scope, EvalOptions{}, errCode("raised")},
		{"var", xcall("var", xlit("MODE")), scope, EvalOptions{Vars: vars}, "var/MODE"},
		{"systemSecret", xcall("systemSecret", xlit("KEY")), scope, EvalOptions{Vars: vars}, "systemSecret/KEY"},
		{"a var with no resolver", xcall("secret", xlit("KEY")), scope, EvalOptions{}, errCode("var_not_available")},
		{"a var name must be a string", xcall("var", xlit(int64(1))), scope, EvalOptions{Vars: vars}, errCode("invalid_argument")},
		{"too few arguments", xcall("lower"), scope, EvalOptions{}, errCode("argument_count")},
		{"too many arguments", xcall("lower", xlit("a"), xlit("b")), scope, EvalOptions{}, errCode("argument_count")},
		{"a lambda where a value belongs", xcall("hash", xlam("x", xid("x"))), scope, EvalOptions{}, errCode("argument_count")},
		{"a function name is never a predicate first", xcall("lower", xlit("A")), scope,
			EvalOptions{Predicates: func(string) (string, ast.ExpressionNode, bool) { return "r", xlit(true), true }}, "a"},
		// A relationship traversal selects rows in SQL; it has no value in
		// process.
		{"a traversal does not evaluate in process", xcall("childOf", xlam("c", xbin("==", xpath("c", "active"), xlit(true)))), scope, EvalOptions{}, errCode("unknown_function")},
		{"a retired function names its replacement", xcall("concat", xlit("a"), xlit("b")), scope, EvalOptions{}, errCode("unknown_function")},
	})

	// The retired function's refusal says what to write instead.
	_, retiredErr := EvalExpr(context.Background(), xcall("cond", xlit(true), xlit("a"), xlit("b")), scope, EvalOptions{})
	if retiredErr == nil || !strings.Contains(retiredErr.Error(), "p ? a : b") {
		t.Fatalf("a retired function must name its replacement, got %v", retiredErr)
	}
	// A wrong-shape call names the catalog signature.
	_, shapeErr := EvalExpr(context.Background(), xmeth(xpath("args", "xs"), "any"), argsScope(map[string]any{"xs": []any{}}), EvalOptions{})
	if shapeErr == nil || !strings.Contains(shapeErr.Error(), "list.any(pred lambda) bool") {
		t.Fatalf("an argument_count refusal must name the signature, got %v", shapeErr)
	}

	// error(msg) carries the author's message, not a wrapped one.
	_, err := EvalExpr(context.Background(), xcall("error", xbin("+", xlit("bad: "), xpath("args", "id"))), scope, EvalOptions{})
	var ee *ExprError
	if !errors.As(err, &ee) || ee.Message != "bad: abc" {
		t.Fatalf("error() must carry its message verbatim, got %#v", err)
	}
}

// ---------------------------------------------------------------------------
// construct calls, errors, the budget
// ---------------------------------------------------------------------------

func TestEvalExprConstructCalls(t *testing.T) {
	call := &ast.CallExpr{Kind: "query", Name: "activeUsers", Named: []ast.NamedArg{
		{Name: "status", Value: xlit("active")},
		{Name: "team", Value: xpath("args", "missing")},
		{Name: "limit", Value: xnil()},
	}}
	scope := argsScope(map[string]any{})
	runExprCases(t, []exprCase{
		{"refused with no call hook", call, scope, EvalOptions{}, errCode("construct_call_not_allowed")},
	})

	var gotNamed map[string]any
	var gotCall *ast.CallExpr
	opts := EvalOptions{Calls: func(_ context.Context, c *ast.CallExpr, named map[string]any) (any, error) {
		gotCall, gotNamed = c, named
		return []any{"u1"}, nil
	}}
	v, err := EvalExpr(context.Background(), xmeth(call, "count"), scope, opts)
	if err != nil || v != int64(1) {
		t.Fatalf("a construct call's result is a value like any other: got %#v, %v", v, err)
	}
	if gotCall != call {
		t.Fatalf("the hook must receive the call node itself")
	}
	if want := map[string]any{"status": "active", "limit": nil}; !reflect.DeepEqual(gotNamed, want) {
		t.Fatalf("named arguments: got %#v, want %#v (an absent value contributes nothing)", gotNamed, want)
	}

	// A hook's own error reaches the caller unwrapped, so a typed engine
	// refusal stays typed.
	sentinel := errors.New("capability_not_held: query activeUsers")
	_, err = EvalExpr(context.Background(), call, scope, EvalOptions{Calls: func(context.Context, *ast.CallExpr, map[string]any) (any, error) {
		return nil, sentinel
	}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("a hook error must pass through unchanged, got %v", err)
	}
}

func TestExprErrorNamesItsCodeAndPlace(t *testing.T) {
	plain := &ExprError{Code: "unknown_name", Message: "nope is not defined"}
	if got := plain.Error(); got != "unknown_name: nope is not defined" {
		t.Fatalf("got %q", got)
	}
	placed := &ExprError{Code: "unknown_name", Message: "nope is not defined", Span: ast.Span{Line: 3, Col: 7, EndLine: 3, EndCol: 11}}
	if got := placed.Error(); got != "unknown_name at 3:7: nope is not defined" {
		t.Fatalf("got %q", got)
	}

	// A refusal carries the span of the node it is about.
	n := &ast.IdentExpr{Name: "nope", Span: ast.Span{Line: 2, Col: 5, EndLine: 2, EndCol: 9}}
	_, err := EvalExpr(context.Background(), xbin("==", n, xlit(int64(1))), MapScope{}, EvalOptions{})
	var ee *ExprError
	if !errors.As(err, &ee) || ee.Span.Line != 2 || ee.Span.Col != 5 {
		t.Fatalf("want the ident's span on the refusal, got %#v", err)
	}
}

// The runtime budget: every node evaluation is one step. A nested scan over
// 2,000 elements is ~39,000 evaluations here -- each element finds its match
// within the first ten, so the inner scan stops early -- which a budget of
// 10,000 stops and the default budget lets finish. (Over 2,000 DISTINCT
// values the same tree is ~6,000,000 evaluations and the default budget stops
// that too, which is the budget doing its job.)
func TestEvalExprBudget(t *testing.T) {
	xs := make([]any, 2000)
	for i := range xs {
		xs[i] = int64(i % 10)
	}
	scope := argsScope(map[string]any{"xs": xs})
	expr := xmeth(xmeth(xpath("args", "xs"), "where",
		xlam("x", xmeth(xpath("args", "xs"), "any", xlam("y", xbin("==", xid("y"), xid("x")))))),
		"count")

	_, err := EvalExpr(context.Background(), expr, scope, EvalOptions{Budget: 10_000})
	var ee *ExprError
	if !errors.As(err, &ee) || ee.Code != "expression_budget_exceeded" {
		t.Fatalf("%s with Budget 10,000: want expression_budget_exceeded, got %v", ast.FormatExpr(expr), err)
	}

	got, err := EvalExpr(context.Background(), expr, scope, EvalOptions{})
	if err != nil || got != int64(2000) {
		t.Fatalf("%s with the default budget: want 2000, got %#v, %v", ast.FormatExpr(expr), got, err)
	}
}

// TestCatalogMatchesTheEvaluators holds the function catalog
// (component/language/functions) and EvalExpr's dispatch to each other in
// both directions -- the acceptance test the plan puts beside the evaluator:
//
//   - every entry with an in-process meaning is implemented, so a catalog
//     entry cannot ship with nothing behind it. The one exception is the
//     relationship traversals, whose result is a row set that only SQL
//     holds (Returns == rows): they must NOT be implemented here;
//   - every implementation is an entry, so the evaluator cannot grow a
//     function the catalog -- and so Sense, the docs and the load-time
//     check -- does not know;
//   - no retired spelling is implemented.
//
// Argument counts and kinds need no parity check: exprCheckShape reads them
// from the catalog entry itself.
func TestCatalogMatchesTheEvaluators(t *testing.T) {
	catalog := functions.Catalog()
	inCatalog := map[string]bool{}
	implemented, traversals := 0, 0
	for _, f := range catalog {
		inCatalog[f.Key()] = true
		_, isFunction := exprFunctionImpls[f.Key()]
		_, isMethod := exprMethodImpls[f.Key()]
		if f.Returns == functions.TypeRows {
			traversals++
			if isFunction || isMethod {
				t.Errorf("%s is a relationship traversal (it returns rows, which only SQL holds); EvalExpr must not implement it", f.Signature())
			}
			continue
		}
		if !isFunction && !isMethod {
			t.Errorf("%s is in the catalog, and EvalExpr has no implementation of it", f.Signature())
			continue
		}
		implemented++
	}
	for key := range exprFunctionImpls {
		if !inCatalog[key] {
			t.Errorf("EvalExpr implements the function %q, which the catalog does not list", key)
		}
	}
	for key := range exprMethodImpls {
		if !inCatalog[key] {
			t.Errorf("EvalExpr implements the method %q, which the catalog does not list", key)
		}
	}
	for name := range functions.RetiredFunctions() {
		if _, ok := exprFunctionImpls[name]; ok {
			t.Errorf("EvalExpr implements %s(), which edition 2026 retires", name)
		}
	}
	for key := range functions.RetiredMethods() {
		if _, ok := exprMethodImpls[key]; ok {
			t.Errorf("EvalExpr implements %s, which edition 2026 retires", key)
		}
	}
	// A reachable positive: the loops above examined the whole catalog, and
	// found both kinds of entry.
	if implemented+traversals != len(catalog) || implemented == 0 || traversals == 0 {
		t.Fatalf("examined %d implemented entries and %d traversals of a %d-entry catalog", implemented, traversals, len(catalog))
	}
}

// A cancelled context stops a long evaluation with the context's own error.
func TestEvalExprHonoursCancellation(t *testing.T) {
	xs := make([]any, 5000)
	for i := range xs {
		xs[i] = int64(i)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	expr := xmeth(xpath("args", "xs"), "where", xlam("x", xbin(">", xid("x"), xlit(int64(-1)))))
	_, err := EvalExpr(ctx, expr, argsScope(map[string]any{"xs": xs}), EvalOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
