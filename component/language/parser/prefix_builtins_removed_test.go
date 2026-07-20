package parser

import (
	"testing"
)

// TestPrefixCallBuiltinsRemoved locks in the removal of the dead prefix-call
// logic/comparison builtins (#978). The boolean grammar is `&&` / `||` / `!`
// and the comparison operators are `==` / `!=` / `<` / `<=` / `>` / `>=`; the
// old prefix-call spellings (`and(...)`, `or(...)`, `not(...)`, `eq(...)`,
// `lt(...)`, `gt(...)`, `lte(...)`, `gte(...)`) no longer parse into their
// dedicated AST nodes. They fall through to a generic, unresolved
// FunctionCallExpr instead -- proof the builtins are gone from the parser.
func TestPrefixCallBuiltinsRemoved(t *testing.T) {
	// These names are plain identifiers; the removed builtin now falls through
	// to a generic (unresolved) FunctionCallExpr, exactly like any unknown name.
	genericCases := []string{
		"and(true, false)",
		"or(true, false)",
		"eq(payload.a, payload.b)",
		"lt(payload.a, payload.b)",
		"gt(payload.a, payload.b)",
		"lte(payload.a, payload.b)",
		"gte(payload.a, payload.b)",
	}

	for _, src := range genericCases {
		t.Run(src, func(t *testing.T) {
			tokens, err := NewLexer(src).Tokenize()
			if err != nil {
				t.Fatalf("tokenize %q: %v", src, err)
			}
			expr, err := NewParser(tokens).parseExpression()
			if err != nil {
				t.Fatalf("parse %q: %v", src, err)
			}

			switch expr.(type) {
			case *AndExpr, *OrExpr, *NotExpr, *LtExpr, *GtExpr, *LteExpr, *GteExpr:
				t.Fatalf("expected %q to no longer parse as a dedicated builtin AST node, got %T", src, expr)
			case *FunctionCallExpr:
				// Expected.
			default:
				t.Fatalf("expected %q to parse as a generic *FunctionCallExpr, got %T", src, expr)
			}
		})
	}

	// `not` is a reserved word (the `not in` membership operator), so the
	// removed `not(...)` prefix builtin no longer parses at all -- it errors
	// rather than producing a NotExpr.
	t.Run("not(true)", func(t *testing.T) {
		tokens, err := NewLexer("not(true)").Tokenize()
		if err != nil {
			t.Fatalf("tokenize: %v", err)
		}
		if _, err := NewParser(tokens).parseExpression(); err == nil {
			t.Fatalf("expected not(...) prefix builtin to be rejected, but it parsed")
		}
	})
}

// TestBooleanGrammarStillProducesLogicNodes guards against an over-removal: the
// infix `!` unary operator must still produce a NotExpr (it shares the AST node
// the deleted `not(...)` builtin used). Removing the prefix builtins must not
// disturb the operator grammar.
func TestInfixBangStillProducesNotExpr(t *testing.T) {
	tokens, err := NewLexer("!payload.active").Tokenize()
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	expr, err := NewParser(tokens).parseExpression()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := expr.(*NotExpr); !ok {
		t.Fatalf("expected infix !expr to parse as *NotExpr, got %T", expr)
	}
}
