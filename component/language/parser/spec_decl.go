package parser

import (
	"fmt"
	"strconv"

	"github.com/znasllc-io/memql/component/language/annotations"
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
//	spec <BoundName> <Name> = row => <bool-expr>     -- edition 2026 (memql#5364)
//	trait <Name> = row => <bool-expr>                -- edition 2026
//
// The `=` form's body is a lambda of one parameter, parsed by the v1
// expression grammar into SpecDecl.Lambda (Body stays nil): the parameter IS
// the bound row -- or, over an @actor shape, the actor envelope, spelled
// `actor`. With Options.ExpressionsV1 on, the `{ return ... }` form is refused
// naming memqlmigrate --rewrite=expressions; with it off both load.
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
			return nil, newParseErrorf(&p.current, "spec %q must bind a shape or concept in its signature: `spec <BoundName> %s = row => <predicate>` (the bound name resolves via the file-top `use` import). A spec with no binding is no longer valid", first, first)
		}
	}

	// Traits validate against the "Spec" receiver set -- both kinds share
	// this parser and accept the same lifecycle/description annotations.
	if err := p.checkAnnotations(annotations.Spec, fmt.Sprintf("%s %q", keyword, decl.Name), attrs); err != nil {
		return nil, err
	}

	// Edition 2026: `= <lambda>`.
	if p.check(TokenOperator) && p.current.Literal == "=" {
		p.advance()
		// The body is a predicate over one row, never a time-travel read: an
		// `asOf(...)` in it is refused as the query-only clause it is, at the
		// author's `asOf`, as it is in a logic or automation body
		// (asOfOutsideQuery). Unrefused, the engine read it as a predicate
		// applied to the wrong number of arguments.
		prev := p.currentFuncType
		p.currentFuncType = FunctionType(keyword)
		lam, err := p.parseOneParamLambda(keyword + " " + strconv.Quote(decl.Name))
		p.currentFuncType = prev
		if err != nil {
			return nil, err
		}
		if err := p.refuseCommaAfterLambda(); err != nil {
			return nil, err
		}
		if err := p.refuseAfterPredicate("the predicate of " + keyword + " " + strconv.Quote(decl.Name)); err != nil {
			return nil, err
		}
		decl.Lambda = lam
		return decl, nil
	}

	// The braced `{ return ... }` body is retired; anything else after the
	// signature is not a spec.
	if p.check(TokenBraceOpen) {
		rule := ruleSpecReturnBody
		if isTrait {
			rule = ruleTraitReturnBody
		}
		return nil, v1Retired(p.current, rule)
	}
	return nil, newParseErrorf(&p.current, "expected `=` and the %s's predicate after %q, as in %s = row => <predicate>; got %q", keyword, decl.Name, keyword, p.current.Literal)
}
