package parser

import (
	"fmt"

	"github.com/znasllc-io/memql/core/baseparser"
)

// ParseExpression parses a single MemQL expression from source text
// and returns the parsed AST node. Public entry point exposed for
// dedicated per-construct parsers (e.g. component/memql/spec_parser.go)
// that build their own grammar around the canonical expression
// parser without going through a synthetic function wrapper.
//
// The source must contain exactly one expression. Trailing tokens
// after the expression (other than EOF / ; / a closing brace that
// the caller consumed prior to invocation) produce a parse error.
func ParseExpression(source string) (ExpressionNode, error) {
	lex := NewLexer(source)
	tokens, err := lex.Tokenize()
	if err != nil {
		return nil, fmt.Errorf("tokenize: %w", err)
	}
	p := NewParser(tokens)
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	// Permit trailing semicolons + EOF; reject anything else so a
	// stray identifier or operator surfaces as an error rather than
	// being silently dropped.
	for p.check(TokenSemicolon) {
		p.advance()
	}
	if !p.check(TokenEOF) {
		// `?.` is retired (memql#5375, D17). It already failed here, but as
		// "unexpected token", which names neither the form nor the fix --
		// and `?.` is a form an author could reasonably believe in, since
		// `??` is live one character away.
		if p.current.Literal == "?." {
			return nil, fmt.Errorf("`?.` is retired -- guard the nil explicitly (`owner != nil && owner.id == ...`) or coalesce with `??` (memql#5375). %s", baseparser.AttributeRewriteHint)
		}
		return nil, fmt.Errorf("unexpected token after expression: %q", p.current.Literal)
	}
	return expr, nil
}
