package parser

import (
	"github.com/znasllc-io/memql/component/language/ast"
)

// parseSpecDecl parses a struct-form `spec NAME { <bool-expr> }` or
// `trait NAME { <bool-expr> }` declaration. The leading attribute
// set has already been consumed by parseDefinition and is passed in
// as attrs. The keyword arm (`spec` vs `trait`) is encoded in the
// isTrait parameter -- both keywords share the same body grammar.
//
// The body is parsed in one shot via p.parseExpression(); the
// resulting typed ExpressionNode is stored on the AST node. The
// memql-side converter (specDeclToSpec) runs the engine's
// ASTConverter on it directly -- no string-roundtrip re-parse.
//
// memql#334 (sub-epic #329 / #310 Stage 1C).
func (p *Parser) parseSpecDecl(attrs []*ast.Attribute, isTrait bool) (*ast.SpecDecl, error) {
	keyword := "spec"
	if isTrait {
		keyword = "trait"
	}
	if !p.check(TokenIdentifier) || p.current.Literal != keyword {
		return nil, newParseErrorf(&p.current, "expected %q keyword, got %q", keyword, p.current.Literal)
	}
	p.advance()

	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected %s name after %q, got %q", keyword, keyword, p.current.Literal)
	}
	decl := &ast.SpecDecl{
		Name:       p.current.Literal,
		IsTrait:    isTrait,
		Attributes: attrs,
	}
	p.advance()

	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}

	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if expr == nil {
		return nil, newParseErrorf(&p.current, "%s %q: body is empty (expected a boolean expression)", keyword, decl.Name)
	}
	decl.Body = expr

	if err := p.expect(TokenBraceClose); err != nil {
		return nil, err
	}
	return decl, nil
}
