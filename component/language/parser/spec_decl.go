package parser

import (
	"github.com/znasllc-io/memql/component/language/ast"
)

// parseSpecDecl parses a struct-form spec or trait declaration. The
// leading attribute set has already been consumed by parseDefinition
// and is passed in as attrs. The keyword arm (`spec` vs `trait`) is
// encoded in the isTrait parameter.
//
// Grammar (spec/shape binding redesign, epic #2281):
//
//	spec <BoundName> <Name> { return <bool-expr> }   -- signature-bound
//	trait <Name> { return <bool-expr> }              -- deliberately unbound
//
// A spec binds exactly one shape XOR concept in its signature; the
// bound name resolves through the file-top `use` import (shapes vs
// concepts disambiguated by the import path). A trait carries no
// binding -- it is the one deliberately-unbound row predicate (bare
// payload fields, validated against the concrete concept at the call
// site). The body is a single `return <boolean expression>`; bare field
// names read the bound surface (no payload./shapeName./conceptName.
// prefix). The old bare-expression body (no `return`) is rejected with
// a migration-pointing error.
//
// The body expression is parsed in one shot via p.parseExpression() and
// the resulting typed ExpressionNode stored on the AST node. The
// memql-side converter (specDeclToSpec) runs the engine's ASTConverter
// on it directly -- no string-roundtrip re-parse.
//
// memql#334 (sub-epic #329 / #310 Stage 1C); signature binding + return
// added by epic #2281 (Story 2 / #2282).
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
	first := p.current.Literal
	p.advance()

	decl := &ast.SpecDecl{
		IsTrait:    isTrait,
		Attributes: attrs,
	}

	// Two-identifier signature `spec <BoundName> <Name> { ... }`. If the
	// next token is another identifier (not `{`), the first identifier is
	// the binding (shape XOR concept) and the second is the spec name.
	// Traits are deliberately unbound: a second identifier on a trait is
	// rejected.
	if p.check(TokenIdentifier) {
		if isTrait {
			return nil, newParseErrorf(&p.current, "trait %q must not carry a signature binding -- a trait is the one deliberately unbound row predicate; drop the bound name (or declare a `spec <BoundName> <name>` if you need a binding)", first)
		}
		decl.BoundName = first
		decl.Name = p.current.Literal
		p.advance()
	} else {
		decl.Name = first
		if !isTrait {
			return nil, newParseErrorf(&p.current, "spec %q must bind a shape or concept in its signature: `spec <BoundName> %s { return <bool> }` (the bound name resolves via the file-top `use` import). A spec with no binding is no longer valid", first, first)
		}
	}

	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}

	// The body MUST be a single `return <boolean expression>`. The old
	// bare-expression form is rejected with a migration-pointing error.
	if !p.check(TokenKeywordReturn) {
		return nil, newParseErrorf(&p.current, "%s %q body must be a `return <boolean expression>` -- the old bare-expression form is retired (epic #2281). Wrap the predicate in `return ...` and read bound fields by bare name", keyword, decl.Name)
	}
	p.advance()

	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if expr == nil {
		return nil, newParseErrorf(&p.current, "%s %q: body is empty (expected `return <boolean expression>`)", keyword, decl.Name)
	}
	decl.Body = expr

	if err := p.expect(TokenBraceClose); err != nil {
		return nil, err
	}
	return decl, nil
}
