package parser

// v1_expr_test.go -- the edition-2026 expression parser (epic memql#5363,
// task memql#5364): what each spelling parses TO, what the canonical printer
// prints it back as, and where the mid-stream entry stops.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// mustParseV1 parses src as one v1 expression or fails the test.
func mustParseV1(t *testing.T, src string) ast.ExpressionNode {
	t.Helper()
	n, err := ParseV1Expression(src)
	if err != nil {
		t.Fatalf("ParseV1Expression(%q): %v", src, err)
	}
	return n
}

// fullyParen prints a tree with every operator application parenthesised, so
// a test states the GROUPING the parser chose rather than the canonical text.
// A ParenExpr is transparent here: the node inside it is parenthesised by its
// own rule, and printing the source parentheses as well would only double them.
func fullyParen(n ast.ExpressionNode) string {
	switch e := n.(type) {
	case *ast.IdentExpr:
		return e.Name
	case *ast.LiteralExpr:
		return ast.FormatLiteral(e.Value)
	case *ast.NilExpr:
		return "nil"
	case *ast.ParenExpr:
		return fullyParen(e.Inner)
	case *ast.MemberExpr:
		sep := "."
		if e.Optional {
			sep = ".?"
		}
		return "(" + fullyParen(e.Object) + sep + e.Field + ")"
	case *ast.UnaryExpr:
		return "(" + e.Op + fullyParen(e.Operand) + ")"
	case *ast.BinaryExpr:
		return "(" + fullyParen(e.Left) + " " + e.Op + " " + fullyParen(e.Right) + ")"
	case *ast.TernaryExpr:
		return "(" + fullyParen(e.Condition) + " ? " + fullyParen(e.Then) + " : " + fullyParen(e.Else) + ")"
	case *ast.LambdaExpr:
		return "(" + strings.Join(e.Params, ", ") + " => " + fullyParen(e.Body) + ")"
	case *ast.ListExpr:
		parts := make([]string, len(e.Elems))
		for i, el := range e.Elems {
			parts[i] = fullyParen(el)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *ast.MapExpr:
		parts := make([]string, len(e.Entries))
		for i, en := range e.Entries {
			parts[i] = en.Key + ": " + fullyParen(en.Value)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case *ast.CallExpr:
		var args []string
		for _, a := range e.Args {
			args = append(args, fullyParen(a))
		}
		for _, a := range e.Named {
			args = append(args, a.Name+": "+fullyParen(a.Value))
		}
		call := e.Name + "(" + strings.Join(args, ", ") + ")"
		switch {
		case e.Kind != "":
			return e.Kind + " " + call
		case e.Receiver != nil:
			return "(" + fullyParen(e.Receiver) + "." + call + ")"
		}
		return call
	}
	return fmt.Sprintf("<?%T>", n)
}

// v1Dump renders a tree as an S-expression that ignores spans, so two trees
// compare structurally by comparing strings (and a failure prints both). With
// keepParens false a ParenExpr is dropped and its inner node stands in its
// place: the comparison then asks "same grouping", not "same parentheses".
func v1Dump(n ast.ExpressionNode, keepParens bool) string {
	var b strings.Builder
	v1DumpInto(&b, n, keepParens)
	return b.String()
}

func v1DumpInto(b *strings.Builder, n ast.ExpressionNode, keepParens bool) {
	w := func(ns ...ast.ExpressionNode) {
		for _, c := range ns {
			b.WriteByte(' ')
			v1DumpInto(b, c, keepParens)
		}
	}
	switch e := n.(type) {
	case nil:
		b.WriteString("<nil>")
	case *ast.IdentExpr:
		b.WriteString(e.Name)
	case *ast.LiteralExpr:
		fmt.Fprintf(b, "lit:%T:%s", e.Value, ast.FormatLiteral(e.Value))
	case *ast.NilExpr:
		b.WriteString("nil")
	case *ast.ParenExpr:
		if !keepParens {
			v1DumpInto(b, e.Inner, keepParens)
			return
		}
		b.WriteString("(paren")
		w(e.Inner)
		b.WriteByte(')')
	case *ast.MemberExpr:
		if e.Optional {
			b.WriteString("(.?")
		} else {
			b.WriteString("(.")
		}
		w(e.Object)
		b.WriteString(" " + e.Field + ")")
	case *ast.UnaryExpr:
		b.WriteString("(" + e.Op)
		w(e.Operand)
		b.WriteByte(')')
	case *ast.BinaryExpr:
		b.WriteString("(" + e.Op)
		w(e.Left, e.Right)
		b.WriteByte(')')
	case *ast.TernaryExpr:
		b.WriteString("(?")
		w(e.Condition, e.Then, e.Else)
		b.WriteByte(')')
	case *ast.LambdaExpr:
		b.WriteString("(=> [" + strings.Join(e.Params, " ") + "]")
		w(e.Body)
		b.WriteByte(')')
	case *ast.ListExpr:
		b.WriteString("[list")
		w(e.Elems...)
		b.WriteByte(']')
	case *ast.MapExpr:
		b.WriteString("{map")
		for _, en := range e.Entries {
			b.WriteString(" " + en.Key + ":")
			w(en.Value)
		}
		b.WriteByte('}')
	case *ast.CallExpr:
		switch {
		case e.Kind != "":
			b.WriteString("(" + e.Kind + " " + e.Name)
		case e.Receiver != nil:
			b.WriteString("(method " + e.Name)
			w(e.Receiver)
		default:
			b.WriteString("(call " + e.Name)
		}
		w(e.Args...)
		for _, a := range e.Named {
			b.WriteString(" " + a.Name + ":")
			w(a.Value)
		}
		b.WriteByte(')')
	default:
		fmt.Fprintf(b, "<?%T>", n)
	}
}

// TestV1ParsePrecedence pins the grouping the parser chooses, one row per
// precedence relation the plan's table states (D9).
func TestV1ParsePrecedence(t *testing.T) {
	cases := []struct{ src, want string }{
		{`a || b && c`, `(a || (b && c))`},
		{`a && b || c && d`, `((a && b) || (c && d))`},
		{`a ?? b == c`, `((a ?? b) == c)`},
		{`a == b ?? c`, `(a == (b ?? c))`},
		{`!a == b`, `((!a) == b)`},
		{`a + b * c`, `(a + (b * c))`},
		{`a * b + c`, `((a * b) + c)`},
		{`a - b - c`, `((a - b) - c)`},
		{`a ?? b ?? c`, `((a ?? b) ?? c)`},
		{`a ?? b + c`, `(a ?? (b + c))`},
		{`p ? a : q ? b : c`, `(p ? a : (q ? b : c))`},
		{`p || q ? a : b`, `((p || q) ? a : b)`},
		{`-a.b`, `(-(a.b))`},
		{`-a * b`, `((-a) * b)`},
		{`!!a`, `(!(!a))`},
		{`- - a`, `(-(-a))`},
		{`!a.b.c`, `(!((a.b).c))`},
		{`a - -5`, `(a - -5)`},
		{`a.b(c).d`, `((a.b(c)).d)`},
		{`x => x == 1`, `(x => (x == 1))`},
		{`a == b && c != d`, `((a == b) && (c != d))`},
		{`x in [1, 2]`, `(x in [1, 2])`},
		{`(a || b) && c`, `((a || b) && c)`},
		{`a % b * c / d`, `(((a % b) * c) / d)`},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			if got := fullyParen(mustParseV1(t, c.src)); got != c.want {
				t.Fatalf("grouping:\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// TestV1ParseShapes pins the node each spelling becomes, for the forms whose
// SHAPE (not only their grouping) later tasks switch on.
func TestV1ParseShapes(t *testing.T) {
	cases := []struct{ src, want string }{
		// `.?` makes only the member it precedes optional.
		{`row.?a.b`, `(. (.? row a) b)`},
		{`row.?a.?b`, `(.? (.? row a) b)`},
		{`row.?lineage.planId.x`, `(. (. (.? row lineage) planId) x)`},
		// A fused dotted identifier splits into a root and members.
		{`args.items.0`, `(. (. args items) 0)`},
		{`row => row.x == 1`, `(=> [row] (== (. row x) lit:int64:1))`},
		{`(a, b) => a + b`, `(=> [a b] (+ a b))`},
		{`() => now`, `(=> [] now)`},
		{`x in [1, 2]`, `(in x [list lit:int64:1 lit:int64:2])`},
		{`row.s startsWith args.p`, `(startsWith (. row s) (. args p))`},
		{`query activeUsers(status: "active")`, `(query activeUsers status: lit:string:"active")`},
		{`row.tags.any(t => t == "x")`, `(method any (. row tags) (=> [t] (== t lit:string:"x")))`},
		{`publishEvent(topic: "x", payload: {a: 1})`, `(call publishEvent topic: lit:string:"x" payload: {map a: lit:int64:1})`},
		{`lower(args.email)`, `(call lower (. args email))`},
		{`isActiveRecord(row) && !requiresOwner(actor)`, `(&& (call isActiveRecord row) (! (call requiresOwner actor)))`},
		{`rows.first().email`, `(. (method first rows) email)`},
		{`(a + b).count()`, `(method count (paren (+ a b)))`},
		{`"abc".count()`, `(method count lit:string:"abc")`},
		{`[1, 2].count()`, `(method count [list lit:int64:1 lit:int64:2])`},
		{`f(x).a.b`, `(. (. (call f x) a) b)`},
		{`f(x).?a.b`, `(. (.? (call f x) a) b)`},
		{`row.?tags.any(t => t)`, `(method any (.? row tags) (=> [t] t))`},
		{`query activeUsers(status: "a").count()`, `(method count (query activeUsers status: lit:string:"a"))`},
		// A construct call's bare name is a named argument that puns, and is
		// recorded as one, in source order, mixed with named ones or not.
		{`logic runIsTerminal(status)`, `(logic runIsTerminal status: status)`},
		{`logic f(event, mode: "x")`, `(logic f event: event mode: lit:string:"x")`},
		{`action cloneRepoAtVersion(workdir, ref: ref, dryRun)`, `(action cloneRepoAtVersion workdir: workdir ref: ref dryRun: dryRun)`},
		{`capability script(dry-run: args.dryRun)`, `(capability script dry-run: (. args dryRun))`},
		// Only a bare or construct call is held to the retired object-literal
		// wrapper; a method's map argument is data, as is a map beside others.
		{`xs.merge({k: 1})`, `(method merge xs {map k: lit:int64:1})`},
		{`f({k: 1}, 2)`, `(call f {map k: lit:int64:1} lit:int64:2)`},
		{`f(({k: 1}))`, `(call f (paren {map k: lit:int64:1}))`},
		{`{Content-Type: "x"}`, `{map Content-Type: lit:string:"x"}`},
		{`{in: 1, default: 2}`, `{map in: lit:int64:1 default: lit:int64:2}`},
		{`true && false`, `(&& lit:bool:true lit:bool:false)`},
		{`nil`, `nil`},
		{`now`, `now`},
		{`true.x`, `(. lit:bool:true x)`},
		{`-5`, `lit:int64:-5`},
		{`- 5`, `lit:int64:-5`},
		{`- 5.5`, `lit:float64:-5.5`},
		{`-5.x`, `(- (. lit:int64:5 x))`},
		{`--5`, `(- lit:int64:-5)`},
		{`1e3`, `lit:float64:1000.0`},
		{`p ? x => x : y`, `(? p (=> [x] x) y)`},
		{`a && x => x || b`, `(&& a (=> [x] (|| x b)))`},
		{`x => y => x + y`, `(=> [x] (=> [y] (+ x y)))`},
		{`contains(p => p.active)`, `(call contains (=> [p] (. p active)))`},
		{`contains("label", p => p.active)`, `(call contains lit:string:"label" (=> [p] (. p active)))`},
		{`references("assignedTo", r => r.active)`, `(call references lit:string:"assignedTo" (=> [r] (. r active)))`},
		{`xs.reduce(0, (acc, x) => acc + x)`, `(method reduce xs lit:int64:0 (=> [acc x] (+ acc x)))`},
		{`f(a, b,)`, `(call f a b)`},
		{`[1, 2,]`, `[list lit:int64:1 lit:int64:2]`},
		{`{a: 1,}`, `{map a: lit:int64:1}`},
		{`(a, b,) => a`, `(=> [a b] a)`},
		{`f()`, `(call f)`},
		{`[]`, `[list]`},
		{`{}`, `{map}`},
		{`row.in`, `(. row in)`},
		{`row.?default`, `(.? row default)`},
		{`x.y // a comment` + "\n", `(. x y)`},
		{`a /* inline */ == b`, `(== a b)`},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			if got := v1Dump(mustParseV1(t, c.src), true); got != c.want {
				t.Fatalf("tree:\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// TestV1ParseMultiLine: newlines between tokens are insignificant, so a long
// predicate or a method chain may be split across lines.
func TestV1ParseMultiLine(t *testing.T) {
	cases := []struct{ src, want string }{
		{"row.a == 1\n  && row.b == 2\n  || isX(row)", `((((row.a) == 1) && ((row.b) == 2)) || isX(row))`},
		{"row.tags\n  .where(t => t != \"\")\n  .count() > 0", `((((row.tags).where((t => (t != "")))).count()) > 0)`},
		{"p\n  ? a\n  : b", `(p ? a : b)`},
		{"publishEvent(\n  topic: \"x\",\n  payload: {\n    a: 1,\n  },\n)", `publishEvent(topic: "x", payload: {a: 1})`},
		{"row =>\n  row.a == 1", `(row => ((row.a) == 1))`},
	}
	for _, c := range cases {
		t.Run(strings.ReplaceAll(c.src, "\n", "|"), func(t *testing.T) {
			if got := fullyParen(mustParseV1(t, c.src)); got != c.want {
				t.Fatalf("grouping:\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// TestV1ParseSpans: every node with a Span carries its own source extent,
// 1-indexed with a half-open end, across lines.
func TestV1ParseSpans(t *testing.T) {
	src := "row.status ==\n  args.status"
	n := mustParseV1(t, src)
	bin, ok := n.(*ast.BinaryExpr)
	if !ok {
		t.Fatalf("want a BinaryExpr, got %T", n)
	}
	if want := (ast.Span{Line: 1, Col: 1, EndLine: 2, EndCol: 14}); bin.Span != want {
		t.Errorf("binary span %+v, want %+v", bin.Span, want)
	}
	right, ok := bin.Right.(*ast.MemberExpr)
	if !ok {
		t.Fatalf("want a MemberExpr on the right, got %T", bin.Right)
	}
	if want := (ast.Span{Line: 2, Col: 3, EndLine: 2, EndCol: 14}); right.Span != want {
		t.Errorf("member span %+v, want %+v", right.Span, want)
	}
	root, ok := right.Object.(*ast.IdentExpr)
	if !ok {
		t.Fatalf("want an IdentExpr root, got %T", right.Object)
	}
	if want := (ast.Span{Line: 2, Col: 3, EndLine: 2, EndCol: 7}); root.Span != want {
		t.Errorf("root span %+v, want %+v", root.Span, want)
	}
	left := bin.Left.(*ast.MemberExpr)
	if want := (ast.Span{Line: 1, Col: 1, EndLine: 1, EndCol: 11}); left.Span != want {
		t.Errorf("left member span %+v, want %+v", left.Span, want)
	}

	// A call spans its name through its closing parenthesis; a method call
	// spans its receiver too; a ParenExpr spans its parentheses.
	call := mustParseV1(t, `  f(a,
 b)`).(*ast.CallExpr)
	if want := (ast.Span{Line: 1, Col: 3, EndLine: 2, EndCol: 4}); call.Span != want {
		t.Errorf("call span %+v, want %+v", call.Span, want)
	}
	method := mustParseV1(t, `xs.where(x => x).count()`).(*ast.CallExpr)
	if want := (ast.Span{Line: 1, Col: 1, EndLine: 1, EndCol: 25}); method.Span != want {
		t.Errorf("method span %+v, want %+v", method.Span, want)
	}
	paren := mustParseV1(t, `(a || b)`).(*ast.ParenExpr)
	if want := (ast.Span{Line: 1, Col: 1, EndLine: 1, EndCol: 9}); paren.Span != want {
		t.Errorf("paren span %+v, want %+v", paren.Span, want)
	}
	// A folded negative literal's parent covers the '-': the span of the
	// comparison starts at `x` and ends after `5`.
	cmp := mustParseV1(t, `x == - 5`).(*ast.BinaryExpr)
	if want := (ast.Span{Line: 1, Col: 1, EndLine: 1, EndCol: 9}); cmp.Span != want {
		t.Errorf("comparison span %+v, want %+v", cmp.Span, want)
	}
	// A string token's end includes its closing quote.
	eq := mustParseV1(t, `a == "é"`).(*ast.BinaryExpr)
	if want := (ast.Span{Line: 1, Col: 1, EndLine: 1, EndCol: 9}); eq.Span != want {
		t.Errorf("string comparison span %+v, want %+v", eq.Span, want)
	}
}

// v1CanonicalCorpus is text FormatExpr prints: parse then print is the identity.
var v1CanonicalCorpus = []string{
	`row.status == args.status && isActiveRecord(row)`,
	`(args.x == nil || row.f == args.x)`,
	`row => row.status == args.status`,
	`actor => actor.role == "admin"`,
	`row.tags.any(t => t == "urgent")`,
	`args.members.where(m => m.active).count()`,
	`p ? a : b`,
	`p ? a : q ? b : c`,
	`p ? (q ? a : b) : c`,
	`(p ? "a" : "b") == v`,
	`"a" + (args.x ?? "b")`,
	`a + b ?? "c"`,
	`a ?? b ?? c`,
	`a ?? (b ?? c)`,
	`!(row.a == 1)`,
	`!row.active`,
	`row.?lineage.planId`,
	`row.?lineage.?planId`,
	`(acc, x) => acc + x`,
	`() => now`,
	`query activeUsers(status: "active")`,
	`mutation createNode(id: args.id, payload: {name: args.name, tags: ["a", "b"]})`,
	`{a: 1, b: [x, y]}`,
	`-x * 2`,
	`a - (b - c)`,
	`a - -5`,
	`(a < b) == true`,
	`row.status in ["a", "b"] && row.code startsWith args.p`,
	`row.score == 5.0`,
	`(a + b).count()`,
	`publishEvent(topic: "x", payload: {a: 1})`,
	`lower(args.email)`,
	`isActiveRecord(row) && !requiresOwner(actor)`,
	`row.expiresAt < addDuration(now, "P1D")`,
	`x != nil && x != ""`,
	`args.items.0`,
	`a % b * c / d`,
	`row.count >= 10 || row.count <= -10`,
	`childOf(p => p.concept == "v1:crm:lead")`,
	`references("assignedTo", r => r.active)`,
	`rows.first().email`,
	`logic runIsTerminal(status: status)`,
	`capability script(dry-run: args.dryRun)`,
	`{Content-Type: "application/json"}`,
	`"say \"hi\"" + "tab\t"`,
	`row.n > 1.5e+21`,
	`true && false`,
	`nil`,
	`x => y => x + y`,
	`concept == "v1:a:b" && (row => row.active)`,
	`a || b && c`,
	`(a || b) && c`,
	`f()`,
	`[]`,
	`{}`,
	`xs.reduce(0, (acc, x) => acc + x)`,
	`-5.x`,
	`--5`,
	`!-5`,
	`row.in == row.?default`,
}

// TestV1RoundTripCanonical: canonical text is a fixed point of parse+print.
func TestV1RoundTripCanonical(t *testing.T) {
	if len(v1CanonicalCorpus) < 40 {
		t.Fatalf("the canonical corpus has %d entries; the task asks for about 40", len(v1CanonicalCorpus))
	}
	for _, src := range v1CanonicalCorpus {
		t.Run(src, func(t *testing.T) {
			if got := ast.FormatExpr(mustParseV1(t, src)); got != src {
				t.Fatalf("FormatExpr(parse(s)) != s:\n got %s\nwant %s", got, src)
			}
		})
	}
}

// TestV1RoundTripNonCanonical: a non-canonical spelling prints to canonical
// text that parses to the SAME tree, parentheses included.
func TestV1RoundTripNonCanonical(t *testing.T) {
	cases := []string{
		`a&&b`,
		`a||b&&c`,
		`((a))`,
		`( a == b )`,
		`row.status==args.status&&isActiveRecord(row)`,
		"row.a == 1\n  && row.b == 2",
		"row.tags\n  .any(t => t == \"x\")\n  .count() > 0",
		`f( a , b , )`,
		`[1,2,3,]`,
		`{a:1,b:2,}`,
		`x=>x+1`,
		`(a,b)=>a*b`,
		`-  5`,
		`!  !a`,
		`p ?a :b`,
		`a ?? b??c`,
		`row .?a`,
		"/* c */ a /* d */ == // e\n b",
		`query  activeUsers ( status : "active" )`,
		`1.50`,
		`1e3`,
		`"a"+"b"`,
		`x in[1,2]`,
		`a<b`,
		`- 5.x`,
		`logic runIsTerminal(status)`,
		`action cloneRepoAtVersion(workdir, ref: ref, dryRun)`,
		`(x)=>x`,
		"f(\n  a: 1,\n  b: 2,\n)",
	}
	for _, src := range cases {
		t.Run(strings.ReplaceAll(src, "\n", "|"), func(t *testing.T) {
			first := mustParseV1(t, src)
			printed := ast.FormatExpr(first)
			second, err := ParseV1Expression(printed)
			if err != nil {
				t.Fatalf("the printed form %q does not parse: %v", printed, err)
			}
			if a, b := v1Dump(first, true), v1Dump(second, true); a != b {
				t.Fatalf("re-parse of the printed form differs:\n source  %s\n printed %s\n first   %s\n second  %s", src, printed, a, b)
			}
		})
	}
}

// TestV1QuoteStringRoundTrips: the printer's string quoting uses only escapes
// the MemQL lexer decodes, so a printed literal lexes back to the same value.
// The \u-escaped expectations are built by concatenation, never typed.
func TestV1QuoteStringRoundTrips(t *testing.T) {
	cases := []string{
		"",
		"plain",
		`say "hi"`,
		`back\slash`,
		"tab\there",
		"line\nbreak",
		"cr\rff\fbs\b",
		string(rune(0)),
		string(rune(1)) + string(rune(0x1f)),
		string(rune(0x7f)),
		"é and ü",
		string(rune(0x1F600)),
		string(rune(0x2028)) + string(rune(0x2029)),
		"slash / and \\ backslash",
		"mixed " + string(rune(7)) + " bell",
	}
	for _, s := range cases {
		quoted := ast.QuoteString(s)
		tokens, err := NewLexer(quoted).Tokenize()
		if err != nil {
			t.Errorf("lexing QuoteString(%q) = %s: %v", s, quoted, err)
			continue
		}
		if len(tokens) != 2 || tokens[0].Type != TokenString {
			t.Errorf("QuoteString(%q) = %s lexed to %v, want one string token", s, quoted, tokens)
			continue
		}
		if tokens[0].Literal != s {
			t.Errorf("QuoteString(%q) = %s lexed back to %q", s, quoted, tokens[0].Literal)
		}
	}
	// The bell character's escape is the four-hex form, not Go's `\a`.
	if got, want := ast.QuoteString(string(rune(7))), "\"\\"+"u0007\""; got != want {
		t.Errorf("QuoteString(bell) = %s, want %s", got, want)
	}
}

// TestV1LexerDotQuestion: `.?` is one token, and `?.` keeps its own.
func TestV1LexerDotQuestion(t *testing.T) {
	tokens, err := NewLexer("row.?a.b").Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	want := []struct {
		typ TokenType
		lit string
	}{{TokenIdentifier, "row"}, {TokenDotQuestion, ".?"}, {TokenIdentifier, "a.b"}, {TokenEOF, ""}}
	if len(tokens) != len(want) {
		t.Fatalf("got %d tokens %v, want %d", len(tokens), tokens, len(want))
	}
	for i, w := range want {
		if tokens[i].Type != w.typ || tokens[i].Literal != w.lit {
			t.Errorf("token %d = %v %q, want %v %q", i, tokens[i].Type, tokens[i].Literal, w.typ, w.lit)
		}
	}
	if dq := tokens[1]; dq.Column != 4 || dq.EndCol != 6 {
		t.Errorf("`.?` spans columns %d..%d, want 4..6", dq.Column, dq.EndCol)
	}
	if got := TokenDotQuestion.String(); got != "'.?'" {
		t.Errorf("TokenDotQuestion.String() = %s", got)
	}

	legacy, err := NewLexer("a ?.b").Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	if legacy[1].Type != TokenQuestionDot {
		t.Errorf("`?.` lexed as %v, want TokenQuestionDot", legacy[1].Type)
	}
	// A `.` that does not start `.?`, a field or a number is still TokenDot.
	dot, err := NewLexer("use a.{ b }").Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	if dot[2].Type != TokenDot {
		t.Errorf("`.` before `{` lexed as %v, want TokenDot", dot[2].Type)
	}
}

// TestV1MidStreamStopsAtTheFirstNonContinuation: the construct parsers call
// parseV1Expression mid-stream, so it must stop at the first token that cannot
// extend the expression and leave that token unconsumed.
func TestV1MidStreamStopsAtTheFirstNonContinuation(t *testing.T) {
	cases := []struct {
		src      string
		want     string // FormatExpr of the parsed expression
		stopType TokenType
		stopLit  string
	}{
		{`a == b { x }`, `a == b`, TokenBraceOpen, "{"},
		{`row.status == "x" }`, `row.status == "x"`, TokenBraceClose, "}"},
		{`f(a) ) rest`, `f(a)`, TokenParenClose, ")"},
		{`a, b`, `a`, TokenComma, ","},
		{`a ] b`, `a`, TokenBracketClose, "]"},
		{`x := 1`, `x`, TokenDefine, ":="},
		{"a == 1\nreturn b", `a == 1`, TokenKeywordReturn, "return"},
		{"a\nb := 1", `a`, TokenIdentifier, "b"},
		{`p ? a : b if`, `p ? a : b`, TokenKeywordIf, "if"},
		{`"a": 1`, `"a"`, TokenColon, ":"},
		{`row => row.a == 1 @x`, `row => row.a == 1`, TokenAt, "@"},
		{`xs.count() else`, `xs.count()`, TokenKeywordElse, "else"},
		{`a`, `a`, TokenEOF, ""},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			tokens, err := NewLexer(c.src).Tokenize()
			if err != nil {
				t.Fatalf("tokenize: %v", err)
			}
			p := NewParser(tokens)
			n, err := p.parseV1Expression()
			if err != nil {
				t.Fatalf("parseV1Expression: %v", err)
			}
			if got := ast.FormatExpr(n); got != c.want {
				t.Errorf("parsed %s, want %s", got, c.want)
			}
			if p.current.Type != c.stopType || p.current.Literal != c.stopLit {
				t.Errorf("stopped at %v %q, want %v %q", p.current.Type, p.current.Literal, c.stopType, c.stopLit)
			}
		})
	}
}

// TestParseV1Lambda: the whole input must be one lambda.
func TestParseV1Lambda(t *testing.T) {
	lam, err := ParseV1Lambda(`row => row.a == args.a && isX(row)`)
	if err != nil {
		t.Fatalf("ParseV1Lambda: %v", err)
	}
	if len(lam.Params) != 1 || lam.Params[0] != "row" {
		t.Errorf("params %v, want [row]", lam.Params)
	}
	if got := ast.FormatExpr(lam.Body); got != `row.a == args.a && isX(row)` {
		t.Errorf("body %s", got)
	}
	if _, err := ParseV1Lambda(`(a, b) => a`); err != nil {
		t.Errorf("a two-parameter lambda: %v", err)
	}
	for _, src := range []string{`row.a == 1`, `(row => row.a)`, `isX(row => row.a)`, ``} {
		if _, err := ParseV1Lambda(src); err == nil {
			t.Errorf("ParseV1Lambda(%q) accepted a non-lambda", src)
		}
	}
	if _, err := ParseV1Lambda(`row.a == 1`); err == nil || !strings.Contains(err.Error(), "expected a lambda") {
		t.Errorf("the refusal should say a lambda was expected, got %v", err)
	}
}
