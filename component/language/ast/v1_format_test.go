package ast

import "testing"

func id(n string) *IdentExpr { return &IdentExpr{Name: n} }
func mem(o ExpressionNode, f string) *MemberExpr {
	return &MemberExpr{Object: o, Field: f}
}
func opt(o ExpressionNode, f string) *MemberExpr {
	return &MemberExpr{Object: o, Field: f, Optional: true}
}
func bin(op string, l, r ExpressionNode) *BinaryExpr {
	return &BinaryExpr{Op: op, Left: l, Right: r}
}
func lit(v any) *LiteralExpr { return &LiteralExpr{Value: v} }
func call(name string, args ...ExpressionNode) *CallExpr {
	return &CallExpr{Name: name, Args: args}
}
func path(root string, fields ...string) ExpressionNode {
	var n ExpressionNode = id(root)
	for _, f := range fields {
		n = mem(n, f)
	}
	return n
}

// FormatExpr is the printer the codemod and the compiled automation JSON rest
// on, so each case is a tree and the ONE text it must print to.
func TestFormatExpr(t *testing.T) {
	cases := []struct {
		name string
		n    ExpressionNode
		want string
	}{
		{"filter conjunct with a predicate",
			bin("&&", bin("==", path("row", "status"), path("args", "status")), call("isActiveRecord", id("row"))),
			`row.status == args.status && isActiveRecord(row)`},
		{"optional-argument guard keeps its parens",
			&ParenExpr{Inner: bin("||", bin("==", path("args", "x"), &NilExpr{}), bin("==", path("row", "f"), path("args", "x")))},
			`(args.x == nil || row.f == args.x)`},
		{"or under and gets parens without a ParenExpr",
			bin("&&", bin("||", id("a"), id("b")), id("c")),
			`(a || b) && c`},
		{"and under or needs none",
			bin("||", id("a"), bin("&&", id("b"), id("c"))),
			`a || b && c`},
		{"ternary chains right-associatively in the else branch",
			&TernaryExpr{Condition: id("p"), Then: id("a"), Else: &TernaryExpr{Condition: id("q"), Then: id("b"), Else: id("c")}},
			`p ? a : q ? b : c`},
		{"a ternary in the then branch is parenthesised",
			&TernaryExpr{Condition: id("p"), Then: &TernaryExpr{Condition: id("q"), Then: id("a"), Else: id("b")}, Else: id("c")},
			`p ? (q ? a : b) : c`},
		{"a ternary as a comparison operand is parenthesised",
			bin("==", &TernaryExpr{Condition: id("p"), Then: lit("a"), Else: lit("b")}, id("v")),
			`(p ? "a" : "b") == v`},
		{"coalesce under plus is parenthesised",
			bin("+", lit("a"), bin("??", path("args", "x"), lit("b"))),
			`"a" + (args.x ?? "b")`},
		{"plus under coalesce is not",
			bin("??", bin("+", id("a"), id("b")), lit("c")),
			`a + b ?? "c"`},
		{"coalesce chain folds left",
			bin("??", bin("??", id("a"), id("b")), id("c")),
			`a ?? b ?? c`},
		{"a right-nested coalesce keeps its grouping",
			bin("??", id("a"), bin("??", id("b"), id("c"))),
			`a ?? (b ?? c)`},
		{"not over a comparison",
			&UnaryExpr{Op: "!", Operand: bin("==", path("row", "a"), lit(int64(1)))},
			`!(row.a == 1)`},
		{"not over a member",
			&UnaryExpr{Op: "!", Operand: path("row", "active")},
			`!row.active`},
		{"optional member chain",
			mem(opt(id("row"), "lineage"), "planId"),
			`row.?lineage.planId`},
		{"method call with a lambda",
			&CallExpr{Receiver: path("row", "tags"), Name: "any", Args: []ExpressionNode{&LambdaExpr{Params: []string{"t"}, Body: bin("==", id("t"), lit("x"))}}},
			`row.tags.any(t => t == "x")`},
		{"a lambda with two parameters",
			&LambdaExpr{Params: []string{"acc", "x"}, Body: bin("+", id("acc"), id("x"))},
			`(acc, x) => acc + x`},
		{"a lambda as an operand is parenthesised",
			bin("&&", bin("==", id("concept"), lit("v1:a:b")), &LambdaExpr{Params: []string{"row"}, Body: path("row", "active")}),
			`concept == "v1:a:b" && (row => row.active)`},
		{"construct call with named arguments",
			&CallExpr{Kind: "query", Name: "activeUsers", Named: []NamedArg{{Name: "status", Value: lit("active")}}},
			`query activeUsers(status: "active")`},
		{"map and list literals",
			&MapExpr{Entries: []MapEntry{{Key: "a", Value: lit(int64(1))}, {Key: "b", Value: &ListExpr{Elems: []ExpressionNode{id("x"), id("y")}}}}},
			`{a: 1, b: [x, y]}`},
		{"unary minus binds tighter than times",
			bin("*", &UnaryExpr{Op: "-", Operand: id("x")}, lit(int64(2))),
			`-x * 2`},
		{"minus under minus on the right keeps its grouping",
			bin("-", id("a"), bin("-", id("b"), id("c"))),
			`a - (b - c)`},
		{"a negative literal on the right of minus",
			bin("-", id("a"), lit(int64(-5))),
			`a - -5`},
		{"comparisons never chain bare",
			bin("==", bin("<", id("a"), id("b")), lit(true)),
			`(a < b) == true`},
		{"in and startsWith are comparisons",
			bin("&&", bin("in", path("row", "status"), &ListExpr{Elems: []ExpressionNode{lit("a"), lit("b")}}), bin("startsWith", path("row", "code"), path("args", "p"))),
			`row.status in ["a", "b"] && row.code startsWith args.p`},
		{"an integral float keeps its point",
			bin("==", path("row", "score"), lit(5.0)),
			`row.score == 5.0`},
		{"a member of a parenthesised sum",
			&CallExpr{Receiver: bin("+", id("a"), id("b")), Name: "count"},
			`(a + b).count()`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FormatExpr(c.n); got != c.want {
				t.Fatalf("FormatExpr:\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// QuoteString must produce only escapes the MemQL lexer decodes; the lexer's
// decode of each output is checked where the lexer lives
// (component/language/parser TestV1QuoteStringRoundTrips).
func TestQuoteString(t *testing.T) {
	cases := map[string]string{
		"plain":        `"plain"`,
		`say "hi"`:     `"say \"hi\""`,
		`back\slash`:   `"back\\slash"`,
		"tab\tnl\n":    `"tab\tnl\n"`,
		"bell\a":       "\"bell\\" + "u0007\"",
		"é and 😀":      `"é and 😀"`,
		"\x7f":         "\"\\" + "u007f\"",
		"":             `""`,
		"cr\rff\fbs\b": `"cr\rff\fbs\b"`,
	}
	for in, want := range cases {
		if got := QuoteString(in); got != want {
			t.Errorf("QuoteString(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestKindOfCoversEveryV1Node(t *testing.T) {
	cases := map[NodeKind]ExpressionNode{
		KindIdent:          id("x"),
		KindMember:         mem(id("x"), "f"),
		KindOptionalMember: opt(id("x"), "f"),
		KindCall:           call("lower", id("x")),
		KindMethodCall:     &CallExpr{Receiver: id("xs"), Name: "count"},
		KindConstructCall:  &CallExpr{Kind: "query", Name: "q"},
		KindNot:            &UnaryExpr{Op: "!", Operand: id("x")},
		KindNegate:         &UnaryExpr{Op: "-", Operand: id("x")},
		KindArithmetic:     bin("%", id("a"), id("b")),
		KindCoalesce:       bin("??", id("a"), id("b")),
		KindComparison:     bin(">=", id("a"), id("b")),
		KindIn:             bin("in", id("a"), id("b")),
		KindStartsWith:     bin("startsWith", id("a"), id("b")),
		KindAnd:            bin("&&", id("a"), id("b")),
		KindOr:             bin("||", id("a"), id("b")),
		KindTernary:        &TernaryExpr{Condition: id("p"), Then: id("a"), Else: id("b")},
		KindLambda:         &LambdaExpr{Params: []string{"x"}, Body: id("x")},
		KindList:           &ListExpr{},
		KindMap:            &MapExpr{},
		KindLiteral:        lit("s"),
		KindNil:            &NilExpr{},
		KindParen:          &ParenExpr{Inner: id("x")},
	}
	seen := map[NodeKind]bool{}
	for want, n := range cases {
		if got := KindOf(n); got != want {
			t.Errorf("KindOf(%s) = %q, want %q", FormatExpr(n), got, want)
		}
		seen[want] = true
	}
	for _, k := range AllNodeKinds() {
		if !seen[k] {
			t.Errorf("AllNodeKinds lists %q but no case builds it", k)
		}
	}
	if len(AllNodeKinds()) != len(cases) {
		t.Errorf("AllNodeKinds has %d kinds, the cases cover %d", len(AllNodeKinds()), len(cases))
	}
	if KindOf(&ComparisonExpr{}) != KindUnknown {
		t.Errorf("a node of the internal query form must be KindUnknown")
	}
}

func TestWalkV1VisitsEveryChild(t *testing.T) {
	tree := &TernaryExpr{
		Condition: bin("&&", call("isX", id("row")), &UnaryExpr{Op: "!", Operand: mem(id("row"), "a")}),
		Then:      &ListExpr{Elems: []ExpressionNode{lit(int64(1)), &ParenExpr{Inner: id("y")}}},
		Else: &CallExpr{Receiver: id("xs"), Name: "where", Args: []ExpressionNode{
			&LambdaExpr{Params: []string{"m"}, Body: &MapExpr{Entries: []MapEntry{{Key: "k", Value: id("m")}}}},
		}},
	}
	var idents []string
	WalkV1(tree, func(n ExpressionNode) bool {
		if e, ok := n.(*IdentExpr); ok {
			idents = append(idents, e.Name)
		}
		return true
	})
	want := []string{"row", "row", "y", "xs", "m"}
	if len(idents) != len(want) {
		t.Fatalf("visited idents %v, want %v", idents, want)
	}
	for i := range want {
		if idents[i] != want[i] {
			t.Fatalf("visited idents %v, want %v", idents, want)
		}
	}
}
