package parser

// chain_bound_test.go -- the chain bound is a BOUND: a chain at
// MaxExpressionChain parses and walks, and the link past it is refused.
//
// The pair that matters most is the last two. A bound on what the parser
// builds is only worth anything if BOTH halves hold: a chain far past it is
// refused without the tree ever being walked, and a chain at it is handed back
// and walks. Either half alone is satisfiable by a bound that is wrong.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/language/ast"
)

// chainKinds are the shapes a loop builds without recursing, each written as a
// function of the number of LINKS the expression should hold. A chain kind
// that stopped being counted would parse at n+1 here and fail the test.
var chainKinds = []struct {
	name string
	// src is an edition-2026 expression holding exactly links links.
	src func(links int) string
}{
	{"member", func(n int) string { return "args" + strings.Repeat(".a", n) }},
	{"and", func(n int) string { return "a" + strings.Repeat(" && a", n) }},
	{"or", func(n int) string { return "a" + strings.Repeat(" || a", n) }},
	{"additive", func(n int) string { return "1" + strings.Repeat(" + 1", n) }},
	{"multiplicative", func(n int) string { return "1" + strings.Repeat(" * 1", n) }},
	{"coalesce", func(n int) string { return "a" + strings.Repeat(" ?? a", n) }},
	{"method", func(n int) string { return "xs" + strings.Repeat(".first()", n) }},
	// A comparison is one link over its two operands, so a comparison
	// between two member chains is 1 + the longer of them.
	{"comparison", func(n int) string { return "args" + strings.Repeat(".a", n-1) + " == b" }},
}

func TestEveryChainKindParsesAtTheBoundAndIsRefusedPastIt(t *testing.T) {
	for _, kind := range chainKinds {
		t.Run(kind.name, func(t *testing.T) {
			if _, err := ParseV1Expression(kind.src(MaxExpressionChain)); err != nil {
				t.Fatalf("a chain of exactly %d links must parse -- the bound is a bound, not a wall: %v", MaxExpressionChain, err)
			}
			_, err := ParseV1Expression(kind.src(MaxExpressionChain + 1))
			requireChainRefusal(t, err)
		})
	}
}

// requireChainRefusal holds a refusal to its whole contract: the typed error,
// the stable code through RuleCode, the code last in brackets, the wording,
// and a position an editor can put a squiggle on.
func requireChainRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("a chain of %d links must be refused, not built", MaxExpressionChain+1)
	}
	var refusal *ChainTooLongError
	if !errors.As(err, &refusal) {
		t.Fatalf("refusal must be a *ChainTooLongError, got %T: %v", err, err)
	}
	if refusal.RuleCode() != RuleExpressionChainTooLong {
		t.Errorf("RuleCode() = %q, want %q", refusal.RuleCode(), RuleExpressionChainTooLong)
	}
	msg := err.Error()
	for _, want := range []string{
		"the expression chain is too long (more than 4096 links)",
		"bind its parts to names and combine them in separate statements",
		"[" + RuleExpressionChainTooLong + "]",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal is missing %q\n  full: %s", want, msg)
		}
	}
	if !strings.HasSuffix(msg, "["+RuleExpressionChainTooLong+"]") {
		t.Errorf("the code is printed last, in brackets: %s", msg)
	}
	if line, col := refusal.Parse.Position(); line < 1 || col < 1 {
		t.Errorf("refusal has no position (line %d, column %d)", line, col)
	}
	if !errors.Is(err, ErrInvalidSyntax) {
		t.Error("a chain refusal must unwrap to ErrInvalidSyntax, as every parse refusal does")
	}
}

// A chain continued past a group, a call argument, a list or a lambda body is
// ONE chain. Without the count travelling through those node kinds, 256 groups
// of 4096 links each would be a tree a million nodes tall -- inside both
// bounds and still fatal to walk.
func TestAChainContinuedPastANonLinkNodeIsOneChain(t *testing.T) {
	half := MaxExpressionChain / 2
	inner := "args" + strings.Repeat(".a", half)
	for _, tc := range []struct{ name, src string }{
		{"group", "(" + inner + ")" + strings.Repeat(".a", half+1)},
		{"call argument", "f(" + inner + ")" + strings.Repeat(".a", half+1)},
		{"list element", "[" + inner + "].first()" + strings.Repeat(".a", half)},
		{"lambda body", "xs.any(x => " + inner + strings.Repeat(".a", half+1) + ")"},
		{"prefix", "!" + inner + strings.Repeat(".a", half+1)},
		{"ternary branch", "p ? " + inner + strings.Repeat(".a", half+1) + " : b"},
		{"map value", "{k: " + inner + strings.Repeat(".a", half+1) + "}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseV1Expression(tc.src)
			requireChainRefusal(t, err)
		})
	}
}

// The bound stops a chain; it does not stop what an author writes. The
// longest chain in the 1,151 .memql files of this tree is 8 links, measured by
// running memqllint over dsl/ with the bound lowered: 8 passes and 6 refuses.
// These are the shapes that measurement is made of.
func TestOrdinaryExpressionsAreUntouchedByTheBound(t *testing.T) {
	for _, src := range []string{
		`row => row.status == args.status && isActiveRecord(row) && row.ownerUserId == actor.userId`,
		`row => (args.x == nil || row.f == args.x) && row.kind in ["a", "b"] && row.name startsWith "x"`,
		`args.lineage.originatingPlanId ?? args.planId ?? ""`,
		`xs.filter(x => x.n > 0).map(x => x.n).reduce(0, (acc, n) => acc + n)`,
		`p ? a.b.c : (q ? d.e : f)`,
		`{k: [1, 2, args.n + 1], j: row.a.b.c.count()}`,
	} {
		if _, err := ParseV1Expression(src); err != nil {
			t.Errorf("an ordinary expression must still parse: %q\n  %v", src, err)
		}
	}
}

// The walk is what the chain bound exists to protect, so the proof is to run
// the parse that would reach it. The parser's own check of a statement body
// walks the expression it has just read (checkV1BodyExpression -> ast.WalkV1),
// recursing once per link, so a chain far past the bound must be refused while
// the tree is still being built rather than after.
func TestTheWalkIsUnreachableAtCrashDepthThroughTheParser(t *testing.T) {
	// Far past the bound, and far past any depth a walk could recurse to.
	const links = 12_000_000
	src := "automation crashProbe { x := mutation m(v: args" + strings.Repeat(".a", links) + ") }"

	start := time.Now()
	_, err := ParseFile(src)
	elapsed := time.Since(start)

	requireChainRefusal(t, err)
	if !strings.Contains(err.Error(), "automation crashProbe") {
		t.Errorf("the refusal must name the construct it is in: %s", err.Error())
	}
	if elapsed > 30*time.Second {
		t.Errorf("the refusal took %v; it should cost little more than lexing", elapsed)
	}
	t.Logf("refused %d bytes in %v", len(src), elapsed.Round(time.Millisecond))
}

// The other half of the same claim: a tree the parser DOES hand back walks.
// A bound that refused everything would pass the test above and be useless.
func TestATreeAtTheBoundStillWalks(t *testing.T) {
	e, err := ParseV1Expression("args" + strings.Repeat(".a", MaxExpressionChain))
	if err != nil {
		t.Fatalf("a chain at the bound must parse: %v", err)
	}
	visited := 0
	ast.WalkV1(e, func(ast.ExpressionNode) bool {
		visited++
		return true
	})
	if visited != MaxExpressionChain+1 {
		t.Errorf("walked %d nodes, want %d (the chain plus its root name)", visited, MaxExpressionChain+1)
	}
	// FormatExpr recurses too, and it is what a refusal quotes.
	if printed := ast.FormatExpr(e); !strings.HasPrefix(printed, "args.a") {
		t.Errorf("printed form starts %q", printed[:min(20, len(printed))])
	}
}

// The procedural grammar -- the internal query form an SDK sends to Execute,
// and what the struct-form rewriter lowers a query into -- has its own count,
// because its nodes carry no height of their own. Its trees are walked by the
// executor's filter evaluation and by forty other switch statements in
// component/memql.
func TestTheProceduralGrammarIsBoundedToo(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  func(links int) string
	}{
		{"and", func(n int) string { return "a==1" + strings.Repeat(" && a==1", n) }},
		{"or", func(n int) string { return "a==1" + strings.Repeat(" || a==1", n) }},
		{"comma-or", func(n int) string { return "a==1" + strings.Repeat(", a==1", n) }},
		{"additive", func(n int) string { return "a == 1" + strings.Repeat(" + 1", n) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseExpression(tc.src(MaxExpressionChain)); err != nil {
				t.Fatalf("a chain of exactly %d links must parse: %v", MaxExpressionChain, err)
			}
			requireChainRefusal(t, mustFail(ParseExpression(tc.src(MaxExpressionChain+1))))
		})
	}
}

// The procedural count is CUMULATIVE over a declaration -- that grammar's
// nodes carry no height of their own -- so each declaration must get the whole
// budget back, or a file would eventually be refused for being LONG rather
// than for holding a chain. The state is set here as a previous declaration
// having spent the whole budget, because reaching that through source alone
// takes thousands of declarations and would say nothing clearer.
func TestTheProceduralCountStartsAgainWithEachDeclaration(t *testing.T) {
	src := mustNormalise(t, `use chainbound.concepts.{ ticket }

@description("an ordinary query after a long declaration")
query ticket chainQueryAfter {
  args {
    a string @required
  }
  filter row => row.a == args.a
  sort "row.createdAt", "desc"
  paginate 10
}
`)
	tokens, err := NewLexer(src).Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	p := NewParser(tokens)
	p.chainLinks = MaxExpressionChain // the previous declaration spent it all
	if _, err := p.Parse(); err != nil {
		t.Fatalf("an ordinary declaration after one that spent the whole budget must parse: %v", err)
	}
}

func mustFail(_ ExpressionNode, err error) error { return err }

func mustNormalise(t *testing.T, src string) string {
	t.Helper()
	out, err := NormaliseAll(src)
	if err != nil {
		t.Fatalf("NormaliseAll: %v", err)
	}
	return out
}
