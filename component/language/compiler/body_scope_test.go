package compiler

import (
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// The builders below stamp a line on everything they make, so a problem's
// position can be asserted. A read sits at column 10 of its line; a bound name
// at column 3. The bodies are built by hand because the scope checker is
// tested apart from the statement parser.

func sp(l, c int) ast.Span { return ast.Span{Line: l, Col: c} }

func rd(name string, l int) *ast.IdentExpr { return &ast.IdentExpr{Name: name, Span: sp(l, 10)} }

func fld(obj ast.ExpressionNode, f string) ast.ExpressionNode {
	return &ast.MemberExpr{Object: obj, Field: f}
}

func lit(v any) ast.ExpressionNode { return &ast.LiteralExpr{Value: v} }

func bin(op string, a, b ast.ExpressionNode) ast.ExpressionNode {
	return &ast.BinaryExpr{Op: op, Left: a, Right: b}
}

func lam(params []string, body ast.ExpressionNode) ast.ExpressionNode {
	return &ast.LambdaExpr{Params: params, Body: body}
}

func meth(recv ast.ExpressionNode, name string, args ...ast.ExpressionNode) ast.ExpressionNode {
	return &ast.CallExpr{Receiver: recv, Name: name, Args: args}
}

func named(n string, v ast.ExpressionNode) ast.NamedArg { return ast.NamedArg{Name: n, Value: v} }

func cc(l int, kind, name string, args ...ast.NamedArg) *ast.ConstructCall {
	return &ast.ConstructCall{Kind: kind, Name: name, Args: args, Span: sp(l, 8)}
}

func set(l int, name string, v ast.ExpressionNode) *ast.AssignStatement {
	return &ast.AssignStatement{Name: name, NameSpan: sp(l, 3), Value: v, Span: sp(l, 3)}
}

func setCall(l int, name, kind, callee string, args ...ast.NamedArg) *ast.AssignStatement {
	return &ast.AssignStatement{Name: name, NameSpan: sp(l, 3), Call: cc(l, kind, callee, args...), Span: sp(l, 3)}
}

func do(l int, kind, callee string, args ...ast.NamedArg) *ast.CallStatement {
	return &ast.CallStatement{Call: cc(l, kind, callee, args...), Span: sp(l, 3)}
}

func ifs(l int, branches ...ast.IfBranch) *ast.IfStatement {
	return &ast.IfStatement{Branches: branches, Span: sp(l, 3)}
}

func br(cond ast.ExpressionNode, body ...ast.BodyStatement) ast.IfBranch {
	return ast.IfBranch{Cond: cond, Body: body}
}

func loop(l int, v string, src, filter ast.ExpressionNode, body ...ast.BodyStatement) *ast.ForStatement {
	return &ast.ForStatement{Var: v, VarSpan: sp(l, 7), Source: src, Filter: filter, Body: body, Span: sp(l, 3)}
}

func sw(l int, subject ast.ExpressionNode, arms ...ast.CaseArm) *ast.SwitchStatement {
	return &ast.SwitchStatement{Subject: subject, Cases: arms, Span: sp(l, 3)}
}

func arm(label any, body ...ast.BodyStatement) ast.CaseArm {
	if label == nil {
		return ast.CaseArm{Default: true, Body: body}
	}
	return ast.CaseArm{Labels: []ast.ExpressionNode{lit(label)}, Body: body}
}

func par(l int, branches ...ast.ParallelBranch) *ast.ParallelStatement {
	return &ast.ParallelStatement{Branches: branches, Span: sp(l, 3)}
}

func pbr(l int, label string, body ...ast.BodyStatement) ast.ParallelBranch {
	return ast.ParallelBranch{Label: label, Body: body, Span: sp(l, 5)}
}

func pub(l int, topic string, entries ...ast.MapEntry) *ast.PublishStatement {
	return &ast.PublishStatement{Topic: topic, Payload: &ast.MapExpr{Entries: entries}, Span: sp(l, 3)}
}

func ret(l int, v ast.ExpressionNode) *ast.ReturnStatement {
	return &ast.ReturnStatement{Value: v, Span: sp(l, 3)}
}

func retCall(l int, kind, callee string, args ...ast.NamedArg) *ast.ReturnStatement {
	return &ast.ReturnStatement{Call: cc(l, kind, callee, args...), Span: sp(l, 3)}
}

func body(stmts ...ast.BodyStatement) *ast.Body { return &ast.Body{Statements: stmts, Span: sp(1, 1)} }

// got renders problems as "code@line:col" for comparison.
func got(ps []BodyProblem) []string {
	var out []string
	for _, p := range ps {
		out = append(out, fmt.Sprintf("%s@%d:%d", p.Code, p.Line, p.Col))
	}
	return out
}

type scopeCase struct {
	name string
	kind string
	args []string
	body *ast.Body
	want []string // "code@line:col", in source order; empty for a clean body
	msg  string   // a substring the first problem's message must hold
}

func runScopeCases(t *testing.T, cases []scopeCase) {
	t.Helper()
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			kind := c.kind
			if kind == "" {
				kind = "automation"
			}
			ps := CheckBody(kind, "probe", c.args, c.body)
			if g := strings.Join(got(ps), " "); g != strings.Join(c.want, " ") {
				t.Fatalf("problems = [%s], want [%s]\n%v", g, strings.Join(c.want, " "), ps)
			}
			if c.msg != "" && !strings.Contains(ps[0].Message, c.msg) {
				t.Fatalf("message %q does not contain %q", ps[0].Message, c.msg)
			}
		})
	}
}

func TestCheckBodyNamesResolveInSourceOrder(t *testing.T) {
	runScopeCases(t, []scopeCase{
		{
			name: "a name is readable after the statement that binds it",
			body: body(
				setCall(2, "rows", "query", "activeUsers", named("status", lit("active"))),
				set(3, "n", meth(rd("rows", 3), "count")),
				do(4, "mutation", "record", named("n", rd("n", 4))),
			),
		},
		{
			name: "a read of a later name is a forward reference naming both lines",
			body: body(
				set(2, "a", rd("b", 2)),
				set(3, "b", lit(1)),
			),
			want: []string{"body_forward_reference@2:10"},
			msg:  "`a` reads `b`, which is bound on line 3, after it: move line 3 above line 2",
		},
		{
			name: "the move names the statements in the block that holds both",
			body: body(
				ifs(2, br(rd("c", 2), set(3, "y", rd("x", 3)))),
				set(5, "c", lit(true)),
				set(6, "x", lit(1)),
			),
			want: []string{"body_forward_reference@2:10", "body_forward_reference@3:10"},
			msg:  "`if` reads `c`, which is bound on line 5, after it: move line 5 above line 2",
		},
		{
			name: "a statement cannot read the name it binds",
			body: body(set(2, "x", bin("+", rd("x", 2), lit(1)))),
			want: []string{"body_unknown_name@2:10"},
			msg:  "a statement cannot read the name it binds",
		},
		{
			name: "an unknown name that is an args field says to write args.x",
			args: []string{"id"},
			body: body(do(2, "mutation", "m", named("id", rd("id", 2)))),
			want: []string{"body_unknown_name@2:10"},
			msg:  "write args.id",
		},
		{
			name: "args.x reads the args root in an automation",
			args: []string{"id"},
			body: body(do(2, "mutation", "m", named("id", fld(rd("args", 2), "id")))),
		},
		{
			name: "args.x reads the args root in a logic",
			kind: "logic",
			args: []string{"id"},
			body: body(ret(2, fld(rd("args", 2), "id"))),
		},
		{
			name: "event is a root in an automation",
			body: body(do(2, "logic", "handle", named("event", rd("event", 2)))),
		},
		{
			name: "a logic has no event of its own",
			kind: "logic",
			body: body(ret(2, fld(rd("event", 2), "payload"))),
			want: []string{"body_unknown_name@2:10"},
			msg:  "read args.event",
		},
		{
			name: "the other roots read in both keywords",
			kind: "logic",
			body: body(
				set(2, "who", fld(rd("actor", 2), "userId")),
				set(3, "at", rd("now", 3)),
				set(4, "c", fld(rd("config", 4), "x")),
				ret(5, rd("who", 5)),
			),
		},
		{
			name: "a lambda parameter is a name inside its lambda and nowhere else",
			body: body(
				setCall(2, "rows", "query", "q"),
				set(3, "active", meth(rd("rows", 3), "where", lam([]string{"r"}, fld(rd("r", 3), "active")))),
				set(4, "leak", rd("r", 4)),
			),
			want: []string{"body_unknown_name@4:10"},
		},
		{
			name: "a lambda parameter may shadow a statement name inside its lambda",
			body: body(
				set(2, "r", lit(1)),
				setCall(3, "rows", "query", "q"),
				set(4, "ids", meth(rd("rows", 4), "map", lam([]string{"r"}, fld(rd("r", 4), "id")))),
			),
		},
	})
}

func TestCheckBodyOnceBlocksShareTheirScope(t *testing.T) {
	runScopeCases(t, []scopeCase{
		{
			name: "a name bound in an if is readable after it",
			body: body(
				ifs(2, br(rd("args", 2), setCall(3, "x", "query", "q"))),
				do(5, "mutation", "m", named("x", rd("x", 5))),
			),
		},
		{
			name: "the branches of one if/else chain may bind the same name",
			body: body(
				ifs(2,
					br(rd("args", 2), set(3, "x", lit(1))),
					br(rd("args", 4), set(5, "x", lit(2))),
					br(nil, set(7, "x", lit(3))),
				),
				ret(9, rd("x", 9)),
			),
		},
		{
			name: "the cases of one switch may bind the same name, readable after",
			body: body(
				sw(2, fld(rd("args", 2), "kind"),
					arm("a", set(3, "x", lit(1))),
					arm(nil, set(5, "x", lit(2))),
				),
				ret(7, rd("x", 7)),
			),
		},
		{
			name: "reading a name bound in another branch is refused",
			body: body(
				ifs(2,
					br(rd("args", 2), set(3, "x", lit(1))),
					br(nil, set(5, "y", rd("x", 5))),
				),
			),
			want: []string{"body_unknown_name@5:10"},
			msg:  "x is bound in another branch of this if on line 3, which cannot have run",
		},
		{
			name: "an else-if condition cannot read what an earlier branch bound",
			body: body(
				ifs(2,
					br(rd("args", 2), set(3, "x", lit(1))),
					br(rd("x", 4), set(5, "y", lit(2))),
				),
			),
			want: []string{"body_unknown_name@4:10"},
			msg:  "in another branch of this if on line 3",
		},
		{
			name: "reading across switch cases is refused and says switch",
			body: body(
				sw(2, rd("args", 2),
					arm("a", set(3, "x", lit(1))),
					arm("b", set(5, "y", rd("x", 5))),
				),
			),
			want: []string{"body_unknown_name@5:10"},
			msg:  "another branch of this switch on line 3",
		},
		{
			name: "a name bound before an if may not be rebound inside it",
			body: body(
				set(2, "x", lit(1)),
				ifs(3, br(rd("args", 3), set(4, "x", lit(2)))),
			),
			want: []string{"body_duplicate_name@4:3"},
			msg:  "`x` is already bound on line 2",
		},
		{
			name: "a name bound in an if may not be rebound after it",
			body: body(
				ifs(2, br(rd("args", 2), set(3, "x", lit(1)))),
				set(5, "x", lit(2)),
			),
			want: []string{"body_duplicate_name@5:3"},
		},
		{
			name: "two separate ifs may not bind the same name",
			body: body(
				ifs(2, br(rd("args", 2), set(3, "x", lit(1)))),
				ifs(5, br(rd("args", 5), set(6, "x", lit(2)))),
			),
			want: []string{"body_duplicate_name@6:3"},
		},
		{
			name: "a nested chain inside one branch is a sibling of the outer else",
			body: body(
				ifs(2,
					br(rd("args", 2), ifs(3,
						br(rd("args", 3), set(4, "x", lit(1))),
						br(nil, set(6, "x", lit(2))),
					)),
					br(nil, set(9, "x", lit(3))),
				),
				ret(11, rd("x", 11)),
			),
		},
	})
}

func TestCheckBodyLoopsAndParallelBranchesHaveTheirOwnScope(t *testing.T) {
	runScopeCases(t, []scopeCase{
		{
			name: "a loop variable is read in its body and its filter",
			body: body(
				setCall(2, "rows", "query", "q"),
				loop(3, "item", rd("rows", 3), fld(rd("item", 3), "active"),
					do(4, "mutation", "m", named("id", fld(rd("item", 4), "id"))),
				),
			),
		},
		{
			name: "a loop variable is gone after the loop",
			body: body(
				setCall(2, "rows", "query", "q"),
				loop(3, "item", rd("rows", 3), nil, do(4, "mutation", "m", named("id", fld(rd("item", 4), "id")))),
				ret(6, rd("item", 6)),
			),
			want: []string{"body_unknown_name@6:10"},
			msg:  "item is bound inside the loop on line 3 and exists only inside it",
		},
		{
			name: "a name bound in a loop body is gone after the loop",
			body: body(
				setCall(2, "rows", "query", "q"),
				loop(3, "item", rd("rows", 3), nil, set(4, "last", rd("item", 4))),
				ret(6, rd("last", 6)),
			),
			want: []string{"body_unknown_name@6:10"},
			msg:  "last is bound inside the loop on line 3",
		},
		{
			name: "a loop variable may not shadow an enclosing name",
			body: body(
				setCall(2, "item", "query", "q"),
				loop(3, "item", rd("item", 3), nil, do(4, "mutation", "m", named("v", rd("item", 4)))),
			),
			want: []string{"body_shadowed_name@3:7"},
			msg:  "`item` on line 3 shadows `item` bound on line 2",
		},
		{
			name: "a loop-body name may not shadow an enclosing name",
			body: body(
				set(2, "x", lit(1)),
				setCall(3, "rows", "query", "q"),
				loop(4, "item", rd("rows", 4), nil, set(5, "x", rd("item", 5))),
			),
			want: []string{"body_shadowed_name@5:3"},
		},
		{
			name: "a loop-body name may reuse a name bound after the loop",
			body: body(
				setCall(2, "rows", "query", "q"),
				loop(3, "item", rd("rows", 3), nil, set(4, "x", rd("item", 4))),
				set(6, "x", lit(1)),
			),
		},
		{
			name: "a loop body may not rebind its own variable",
			body: body(
				setCall(2, "rows", "query", "q"),
				loop(3, "item", rd("rows", 3), nil, set(4, "item", lit(1))),
			),
			want: []string{"body_duplicate_name@4:3"},
		},
		{
			name: "a parallel-branch name is gone after the parallel",
			body: body(
				par(2,
					pbr(3, "a", setCall(4, "x", "builtin", "one")),
					pbr(6, "b", setCall(7, "y", "builtin", "two")),
				),
				ret(9, rd("x", 9)),
			),
			want: []string{"body_unknown_name@9:10"},
			msg:  "x is bound inside the parallel branch on line 3",
		},
		{
			name: "two parallel branches may bind the same name, and cannot see each other's",
			body: body(
				par(2,
					pbr(3, "a", setCall(4, "x", "builtin", "one")),
					pbr(6, "b", setCall(7, "x", "builtin", "two"), do(8, "mutation", "m", named("x", rd("x", 8)))),
				),
			),
		},
		{
			name: "a parallel branch reads the enclosing scope",
			body: body(
				setCall(2, "rows", "query", "q"),
				par(3, pbr(4, "a", do(5, "mutation", "m", named("n", meth(rd("rows", 5), "count"))))),
			),
		},
		{
			name: "a read inside a loop of a name bound after it is a forward reference",
			body: body(
				setCall(2, "rows", "query", "q"),
				loop(3, "item", rd("rows", 3), nil, do(4, "mutation", "m", named("v", rd("later", 4)))),
				set(6, "later", lit(1)),
			),
			want: []string{"body_forward_reference@4:10"},
			msg:  "move line 6 above line 3",
		},
	})
}

func TestCheckBodyReservedNames(t *testing.T) {
	var cases []scopeCase
	for _, n := range []string{"args", "actor", "event", "now", "config", "partition", "trace", "steps"} {
		cases = append(cases,
			scopeCase{
				name: n + " as a statement name",
				body: body(set(2, n, lit(1))),
				want: []string{"body_reserved_name@2:3"},
				msg:  "`" + n + "` is a reserved root",
			},
			scopeCase{
				name: n + " as a loop variable",
				body: body(loop(2, n, fld(rd("args", 2), "list"), nil, do(3, "builtin", "b"))),
				want: []string{"body_reserved_name@2:7"},
			},
		)
	}
	runScopeCases(t, cases)
}

func TestCheckBodyConstructRules(t *testing.T) {
	runScopeCases(t, []scopeCase{
		{
			name: "a logic may not publish",
			kind: "logic",
			body: body(pub(2, "x.happened", ast.MapEntry{Key: "a", Value: lit(1)}), ret(3, nil)),
			want: []string{"body_publish_in_logic@2:3"},
			msg:  "a logic may not publish: move `publish \"x.happened\"` into the automation that calls probe, or declare probe as an automation",
		},
		{
			name: "an automation may publish",
			body: body(pub(2, "x.happened", ast.MapEntry{Key: "a", Value: fld(rd("args", 2), "id")})),
		},
		{
			name: "a logic may not call an automation",
			kind: "logic",
			body: body(do(2, "automation", "sweep"), ret(3, nil)),
			want: []string{"body_call_not_in_logic@2:8"},
			msg:  "`automation sweep(...)` belongs in an automation",
		},
		{
			name: "a logic may not call an action, even as its return",
			kind: "logic",
			body: body(retCall(2, "action", "deploy")),
			want: []string{"body_call_not_in_logic@2:8"},
		},
		{
			name: "a logic calls queries, mutations, builtins and logic",
			kind: "logic",
			body: body(
				setCall(2, "rows", "query", "q"),
				setCall(3, "w", "mutation", "m", named("n", meth(rd("rows", 3), "count"))),
				setCall(4, "b", "builtin", "b", named("w", rd("w", 4))),
				retCall(5, "logic", "other", named("b", rd("b", 5))),
			),
		},
		{
			name: "a logic ends with a return",
			kind: "logic",
			body: body(setCall(2, "x", "query", "q")),
			want: []string{"body_logic_return@2:3"},
			msg:  "a logic ends with `return <value>`",
		},
		{
			name: "a return inside an if does not end a logic",
			kind: "logic",
			body: body(ifs(2, br(rd("args", 2), ret(3, lit(1))), br(nil, ret(5, lit(2))))),
			want: []string{"body_logic_return@2:3"},
		},
		{
			name: "an early return inside an if, then a final return",
			kind: "logic",
			body: body(ifs(2, br(rd("args", 2), ret(3, lit(1)))), ret(5, lit(2))),
		},
		{
			name: "an empty logic is refused",
			kind: "logic",
			body: body(),
			want: []string{"body_logic_return@1:1"},
		},
		{
			name: "a parallel branch cannot return, however deep",
			body: body(par(2, pbr(3, "a", ifs(4, br(rd("args", 4), ret(5, nil)))))),
			want: []string{"body_return_in_parallel@5:3"},
		},
		{
			name: "an automation may return anywhere else",
			body: body(
				setCall(2, "rows", "query", "q"),
				loop(3, "r", rd("rows", 3), nil, ifs(4, br(fld(rd("r", 4), "done"), ret(5, fld(rd("r", 5), "id"))))),
				ret(7, nil),
			),
		},
	})
}

func TestCheckBodyReportsEveryProblemInSourceOrder(t *testing.T) {
	ps := CheckBody("logic", "probe", nil, body(
		set(2, "a", rd("b", 2)),
		set(3, "b", lit(1)),
		set(4, "a", lit(2)),
		pub(5, "t"),
	))
	want := "body_forward_reference@2:10 body_duplicate_name@4:3 body_publish_in_logic@5:3 body_logic_return@5:3"
	if g := strings.Join(got(ps), " "); g != want {
		t.Fatalf("problems = [%s], want [%s]", g, want)
	}
	for _, p := range ps {
		if !strings.HasPrefix(p.Error(), "logic probe, line ") || !strings.HasSuffix(p.Error(), " ["+p.Code+"]") {
			t.Errorf("Error() = %q: it names the construct and position first and ends with the code in brackets", p.Error())
		}
	}
}

// TestCheckBodyEveryCodeIsReachable holds the code list to the cases above:
// every code BodyProblemCodes lists is produced by at least one of them, so a
// code added without a case, or a case that stopped producing its code, fails.
func TestCheckBodyEveryCodeIsReachable(t *testing.T) {
	produced := map[string]bool{}
	for _, b := range []struct {
		kind string
		body *ast.Body
	}{
		{"automation", body(set(2, "a", rd("b", 2)), set(3, "b", lit(1)))},
		{"automation", body(set(2, "a", rd("zzz", 2)))},
		{"automation", body(set(2, "a", lit(1)), set(3, "a", lit(2)))},
		{"automation", body(set(2, "a", lit(1)), loop(3, "a", rd("a", 3), nil))},
		{"automation", body(set(2, "now", lit(1)))},
		{"logic", body(pub(2, "t"), ret(3, nil))},
		{"logic", body(do(2, "action", "x"), ret(3, nil))},
		{"logic", body(set(2, "a", lit(1)))},
		{"automation", body(par(2, pbr(3, "a", ret(4, nil))))},
	} {
		for _, p := range CheckBody(b.kind, "probe", nil, b.body) {
			produced[p.Code] = true
		}
	}
	for _, c := range BodyProblemCodes() {
		if !produced[c] {
			t.Errorf("%s is listed by BodyProblemCodes and produced by no body here", c)
		}
	}
	if len(produced) != len(BodyProblemCodes()) {
		t.Errorf("the bodies produced %d codes, BodyProblemCodes lists %d: %v", len(produced), len(BodyProblemCodes()), produced)
	}
}

func TestBodyNamesListsEveryBindingWithItsLines(t *testing.T) {
	names := BodyNames(body(
		setCall(2, "rows", "query", "q"),
		ifs(3, br(rd("args", 3), set(4, "x", lit(1))), br(nil, set(6, "x", lit(2)))),
		loop(8, "item", rd("rows", 8), nil, set(9, "seen", rd("item", 9))),
	))
	want := map[string]string{"rows": "[2]", "x": "[4 6]", "item": "[8]", "seen": "[9]"}
	if len(names) != len(want) {
		t.Fatalf("BodyNames = %v, want %v", names, want)
	}
	for n, lines := range want {
		if g := fmt.Sprint(names[n]); g != lines {
			t.Errorf("%s: lines %s, want %s", n, g, lines)
		}
	}
}
