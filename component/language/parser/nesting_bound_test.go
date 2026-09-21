package parser

// nesting_bound_test.go -- the nesting bound is a BOUND: input at
// MaxNestingDepth parses, input one level past it is refused, and input far
// past it is refused in the time it takes to lex, without the parser
// recursing into it.
//
// The pair matters as much here as it does for the chain bound. A bound on
// recursion is only worth something if BOTH halves hold: the level past it is
// refused, and the level at it is read. Either alone is satisfiable by a bound
// that is wrong.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// proceduralNestingKinds are the shapes the PROCEDURAL expression grammar
// recurses through, each written as a function of how many times the shape
// repeats. cost is how many levels ONE repetition enters -- a chained
// collection method enters its argument list and then its lambda's operand --
// so the deepest input that must still parse is MaxNestingDepth/cost
// repetitions. A kind that stopped being counted would parse well past that.
var proceduralNestingKinds = []struct {
	name string
	cost int
	src  func(repeats int) string
}{
	{"group", 1, func(n int) string { return strings.Repeat("(", n) + "a==1" + strings.Repeat(")", n) }},
	{"array", 1, func(n int) string { return "a == " + strings.Repeat("[", n) + "1" + strings.Repeat("]", n) }},
	{"call", 1, func(n int) string { return strings.Repeat("f(", n) + "1" + strings.Repeat(")", n) }},
	{"prefix bang", 1, func(n int) string { return strings.Repeat("!", n) + "a" }},
	{"prefix minus", 1, func(n int) string { return strings.Repeat("- ", n) + "1" }},
	{"conditional", 1, func(n int) string { return strings.Repeat("p ? ", n) + "a" + strings.Repeat(" : b", n) }},
	{"method args", 2, func(n int) string { return "xs" + strings.Repeat(".where(x => x", n) + strings.Repeat(")", n) }},
}

func TestEveryProceduralNestingKindParsesAtTheBoundAndIsRefusedPastIt(t *testing.T) {
	for _, kind := range proceduralNestingKinds {
		t.Run(kind.name, func(t *testing.T) {
			at := MaxNestingDepth / kind.cost
			if _, err := ParseExpression(kind.src(at)); err != nil {
				t.Fatalf("%d levels must parse -- the bound is a bound, not a wall: %v", MaxNestingDepth, err)
			}
			requireNestingRefusal(t, mustFail(ParseExpression(kind.src(at+1))),
				"the expression", "name its parts as separate statements or constructs")
		})
	}
}

// requireNestingRefusal holds a refusal to its whole contract: the typed
// error, the stable code through RuleCode, the code last in brackets, the
// subject and remedy the site earns, and a position an editor can mark.
func requireNestingRefusal(t *testing.T, err error, subject, remedy string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%d levels must be refused, not recursed into", MaxNestingDepth+1)
	}
	var refusal *NestingTooDeepError
	if !errors.As(err, &refusal) {
		t.Fatalf("refusal must be a *NestingTooDeepError, got %T: %v", err, err)
	}
	if refusal.RuleCode() != RuleNestingTooDeep {
		t.Errorf("RuleCode() = %q, want %q", refusal.RuleCode(), RuleNestingTooDeep)
	}
	msg := err.Error()
	for _, want := range []string{
		subject + " nests too deeply (more than 256 levels)",
		remedy,
		"[" + RuleNestingTooDeep + "]",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal is missing %q\n  full: %s", want, msg)
		}
	}
	if !strings.HasSuffix(msg, "["+RuleNestingTooDeep+"]") {
		t.Errorf("the code is printed last, in brackets: %s", msg)
	}
	if line, col := refusal.Parse.Position(); line < 1 || col < 1 {
		t.Errorf("refusal has no position (line %d, column %d)", line, col)
	}
	if !errors.Is(err, ErrInvalidSyntax) {
		t.Error("a nesting refusal must unwrap to ErrInvalidSyntax, as every parse refusal does")
	}
}

// The internal query form is what an SDK sends to Execute, what the HTTP
// gateway builds from a POST body and what the MCP `query` argument carries,
// and it is read by this grammar. Nesting far past the bound is refused in
// about the time it takes to lex, which is what "refused while reading rather
// than after" looks like from outside.
func TestTheQueryFormIsRefusedAtCrashDepth(t *testing.T) {
	const levels = 1_000_000
	src := `concept=="v1:todos:todo" && ` + strings.Repeat("(", levels) + "row.a==1" + strings.Repeat(")", levels)

	start := time.Now()
	_, err := ParseExpression(src)
	elapsed := time.Since(start)

	requireNestingRefusal(t, err, "the expression", "name its parts as separate statements or constructs")
	if elapsed > 30*time.Second {
		t.Errorf("the refusal took %v; it should cost little more than lexing", elapsed)
	}
	t.Logf("refused %d bytes in %v", len(src), elapsed.Round(time.Millisecond))
}

// A query one level INSIDE the bound still parses, so the door is bounded and
// not closed.
func TestTheQueryFormParsesJustInsideTheBound(t *testing.T) {
	n := MaxNestingDepth - 1 // the leading `&&` operand is the first level
	src := `concept=="v1:todos:todo" && ` + strings.Repeat("(", n) + "row.a==1" + strings.Repeat(")", n)
	if _, err := ParseExpression(src); err != nil {
		t.Fatalf("%d levels inside the bound must parse: %v", n, err)
	}
}

// The declaration parsers recurse too, and a bundle is how a declaration
// arrives over the wire. Each subject gets its own remedy, because "name its
// parts as separate statements" is the wrong instruction for a property tree.
func TestDeclarationNestingIsBounded(t *testing.T) {
	const remedy = "flatten the nesting into separate fields or concepts"

	typeRef := func(n int) string {
		return "concept deepTypeProbe {\n  ownerUserId string @required\n  a " + strings.Repeat("[]", n) + "string\n}\n"
	}
	// The property itself is level 1, so the deepest type that still parses
	// carries one fewer `[]` than the bound.
	if _, err := ParseFile(typeRef(MaxNestingDepth - 1)); err != nil {
		t.Fatalf("a type nested %d deep must parse: %v", MaxNestingDepth-1, err)
	}
	requireNestingRefusal(t, mustFailFile(ParseFile(typeRef(MaxNestingDepth+1))), "the declaration", remedy)

	properties := func(n int) string {
		var b strings.Builder
		b.WriteString("concept deepPropertyProbe {\n  ownerUserId string @required\n")
		for range n {
			b.WriteString("a {\n")
		}
		b.WriteString("leaf string\n")
		for range n {
			b.WriteString("}\n")
		}
		b.WriteString("}\n")
		return b.String()
	}
	// A concept's own properties are at level 1, so the deepest nested block
	// that still parses is one below the bound.
	if _, err := ParseFile(properties(MaxNestingDepth - 1)); err != nil {
		t.Fatalf("properties nested %d deep must parse: %v", MaxNestingDepth-1, err)
	}
	requireNestingRefusal(t, mustFailFile(ParseFile(properties(MaxNestingDepth+1))), "the declaration", remedy)
}

// A statement body's blocks recurse as well, and a logic arrives over the same
// bundle payloads.
func TestStatementBlockNestingIsBounded(t *testing.T) {
	body := func(n int) string {
		return "logic deepBlockProbe {\n  args {\n    a any\n  }\n" +
			strings.Repeat("  if args.a {\n", n) + "  x := 1\n" + strings.Repeat("  }\n", n) +
			"  return 1\n}\n"
	}
	if _, err := ParseFile(body(MaxNestingDepth)); err != nil {
		t.Fatalf("a body nested %d blocks deep must parse: %v", MaxNestingDepth, err)
	}
	requireNestingRefusal(t, mustFailFile(ParseFile(body(MaxNestingDepth+1))),
		"the body", "split the body into smaller logic calls")
}

// The bound stops runaway recursion; it does not stop what anything real
// writes. The deepest nesting measured anywhere -- the shipped DSL tree, the
// conformance corpus, the SDK's generated calls and the lambda serializer --
// is seven levels (nesting_bound.go). These are the shapes that measurement is
// made of.
func TestOrdinaryNestingIsUntouchedByTheBound(t *testing.T) {
	for _, src := range []string{
		`rows.groupBy(g => g.w).select(g => {worker: g.key, pct: g.a.count() * 100 / g.b.count()})`,
		`concept=="v1:todos:todo" && (status=="open" || (owner=="u1" && !(archived==true)))`,
		`cond(a == "x", concat(b, c), d)`,
	} {
		if _, err := ParseExpression(src); err != nil {
			t.Errorf("an ordinary expression must still parse: %q\n  %v", src, err)
		}
	}
}

func mustFailFile(_ Node, err error) error { return err }
