package parser

import (
	"fmt"
	"strings"
	"testing"
)

// renderExpr renders a boolean expression tree to a fully-parenthesized
// string so precedence/associativity assertions read clearly.
func renderExpr(e ExpressionNode) string {
	switch n := e.(type) {
	case *LogicalExpr:
		op := "&&"
		if n.Op == LogicalOr {
			op = "||"
		}
		return "(" + renderExpr(n.Left) + " " + op + " " + renderExpr(n.Right) + ")"
	case *NotExpr:
		return "!" + renderExpr(n.Target)
	case *ComparisonExpr:
		return n.Field.Raw
	default:
		return fmt.Sprintf("%T", e)
	}
}

func parseExprStr(t *testing.T, src string) ExpressionNode {
	t.Helper()
	tokens, err := NewLexer(src).Tokenize()
	if err != nil {
		t.Fatalf("tokenize %q: %v", src, err)
	}
	expr, err := NewParser(tokens).parseExpression()
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return expr
}

// TestLexerTokenizesPipePipe locks in the new `||` token (#972). The lexer
// had no `|` handling at all, which is why specs using `||` silently failed
// to load.
func TestLexerTokenizesPipePipe(t *testing.T) {
	tokens, err := NewLexer(`payload.a == 1 || payload.b == 2`).Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	found := false
	for _, tok := range tokens {
		if tok.Type == TokenPipePipe {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a TokenPipePipe in the token stream, got none")
	}
}

func TestLexerRejectsSinglePipe(t *testing.T) {
	if _, err := NewLexer(`payload.a == 1 | payload.b == 2`).Tokenize(); err == nil {
		t.Fatalf("expected a single '|' to be rejected by the lexer")
	}
}

// TestOrPrecedenceAndParens covers Go precedence: `!` > comparisons > `&&` > `||`,
// with parens overriding. `,` keeps its legacy OR meaning at the same level as `||`.
func TestOrPrecedenceAndParens(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		// && binds tighter than ||
		{`payload.a == 1 && payload.b == 2 || payload.c == 3`, "((payload.a && payload.b) || payload.c)"},
		{`payload.a == 1 || payload.b == 2 && payload.c == 3`, "(payload.a || (payload.b && payload.c))"},
		// left-associative within a level
		{`payload.a == 1 || payload.b == 2 || payload.c == 3`, "((payload.a || payload.b) || payload.c)"},
		{`payload.a == 1 && payload.b == 2 && payload.c == 3`, "((payload.a && payload.b) && payload.c)"},
		// parens override
		{`(payload.a == 1 || payload.b == 2) && payload.c == 3`, "((payload.a || payload.b) && payload.c)"},
		{`payload.a == 1 && (payload.b == 2 || payload.c == 3)`, "(payload.a && (payload.b || payload.c))"},
		// ! binds tighter than &&
		{`!payload.a == 1 && payload.b == 2`, "(!payload.a && payload.b)"},
		// legacy `,`-OR sits at the same level as `||`
		{`payload.a == 1 && payload.b == 2, payload.c == 3`, "((payload.a && payload.b) || payload.c)"},
		// `||` and `,` interchangeable at the OR level
		{`payload.a == 1 || payload.b == 2, payload.c == 3`, "((payload.a || payload.b) || payload.c)"},
	}

	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			got := renderExpr(parseExprStr(t, tc.src))
			if got != tc.want {
				t.Fatalf("precedence mismatch\n src:  %s\n got:  %s\n want: %s", tc.src, got, tc.want)
			}
		})
	}
}

// TestSpecWithOrLoads is the regression for the live bug (#964): the
// `requiresOwnerOrAdmin` spec in dsl/deployment/specs.memql uses `||` and
// silently failed to load because `|` did not tokenize. It must now parse
// to an OR over the two role comparisons.
func TestSpecWithOrLoads(t *testing.T) {
	src := `@enabled
@description("Caller must hold owner or admin role to use the Deployment Console.")
spec actorEnvelope requiresOwnerOrAdmin {
  return role == "admin" || role == "owner"
}`

	decl, err := ParseSpecDecl(src)
	if err != nil {
		t.Fatalf("ParseSpecDecl: %v", err)
	}
	if decl.BoundName != "actorEnvelope" {
		t.Fatalf("expected signature binding actorEnvelope, got %q", decl.BoundName)
	}
	got := renderExpr(decl.Body)
	if got != "(role || role)" {
		t.Fatalf("spec body did not parse as an OR over the role comparisons, got: %s", got)
	}
	if !strings.Contains(got, "||") {
		t.Fatalf("expected the spec body to contain an OR, got: %s", got)
	}
}
