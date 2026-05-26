package parser

import (
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
)

// parseSeedDecl parses a struct-form `seed NAME { ... }` declaration.
// The leading attribute set is supplied by parseDefinition; this
// method consumes the `seed` keyword onward and walks the body.
//
// Two header shapes are accepted:
//
//   - `seed <Concept> <name> { ... }` -- canonical post-migration
//     form. The first ident is the concept binding (resolved via
//     the file's Form B imports by the memql-side converter); the
//     second is the seed name. SignatureConcept is set.
//   - `seed <name> { ... }` -- legacy single-ident form. Loader-side
//     resolution relies on a file-top `use <ns>.<concept>` clause;
//     SignatureConcept stays empty.
//
// Body grammar: a recursive tree of `<field>: <value>` assignments
// and `<field> { ... }` nested blocks. Values cover string / int /
// float / bool / string-array; no other element types in arrays.
// Field-name validation against the bound concept's schema is the
// loader's job, NOT the parser's -- this matches parseSeedMemQL's
// schema-agnostic stance.
//
// memql#335 (sub-epic #329 / Stage 1C of #310).
func (p *Parser) parseSeedDecl(attrs []*ast.Attribute) (*ast.SeedDecl, error) {
	if !p.check(TokenIdentifier) || p.current.Literal != "seed" {
		return nil, newParseErrorf(&p.current, "expected 'seed' keyword, got %q", p.current.Literal)
	}
	p.advance()

	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected seed name after 'seed', got %q", p.current.Literal)
	}
	first := p.current.Literal
	p.advance()

	decl := &ast.SeedDecl{}

	// Two-identifier signature: `seed <Concept> <name>` -- canonical
	// form. We disambiguate by peeking past the next token: if it's
	// an identifier (not `{`), we have the two-ident shape.
	if p.check(TokenIdentifier) {
		decl.SignatureConcept = first
		decl.Name = p.current.Literal
		p.advance()
	} else {
		decl.Name = first
	}

	// Translate the leading attribute set. Mirror parseSeedMemQL's
	// allow-list -- unknown annotations are rejected, matching the
	// hand-rolled parser's hard-fail default branch (different from
	// the prompt / builtin parsers which tolerate unknowns silently).
	for _, attr := range attrs {
		if attr == nil {
			continue
		}
		switch attr.Name {
		case "enabled", "disabled":
			// Tolerated for parity with other primitives; no semantic
			// effect on seed decls.
		case "description":
			decl.Description = attrStringValue(attr)
		case "namespace":
			decl.Namespace = attrStringValue(attr)
		case "version":
			decl.Version = attrStringValue(attr)
		case "scope":
			v := attrStringValue(attr)
			switch v {
			case "global", "perUser":
				decl.Scope = v
			default:
				return nil, newParseErrorf(&p.current,
					"@scope must be \"global\" or \"perUser\", got %q", v)
			}
		case "templateFile":
			decl.TemplateFile = attrStringValue(attr)
		default:
			return nil, newParseErrorf(&p.current,
				"unknown seed annotation @%s (allowed: @version, @namespace, @scope, @description, @templateFile, @enabled, @disabled)", attr.Name)
		}
	}

	block, err := p.parseSeedBlock()
	if err != nil {
		return nil, err
	}
	decl.Body = block
	return decl, nil
}

// parseSeedBlock consumes `{ ... }` containing field assignments and
// nested blocks. Recursive: nested blocks parse with the same rules
// as the top-level body. No field-name validation here -- the loader
// validates against the target concept's schema.
func (p *Parser) parseSeedBlock() (*ast.SeedBlock, error) {
	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}
	block := &ast.SeedBlock{
		Fields: map[string]*ast.SeedValue{},
		Nested: map[string]*ast.SeedBlock{},
	}
	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		if !p.check(TokenIdentifier) {
			return nil, newParseErrorf(&p.current,
				"expected field name or block name in seed body, got %q", p.current.Literal)
		}
		key := p.current.Literal
		p.advance()

		if p.check(TokenBraceOpen) {
			// Nested block: `<key> { ... }`.
			if _, dup := block.Nested[key]; dup {
				return nil, newParseErrorf(&p.current, "duplicate nested block %q", key)
			}
			if _, dup := block.Fields[key]; dup {
				return nil, newParseErrorf(&p.current, "name %q used as both field and nested block", key)
			}
			child, err := p.parseSeedBlock()
			if err != nil {
				return nil, err
			}
			block.Keys = append(block.Keys, key)
			block.Nested[key] = child
			continue
		}

		if !p.check(TokenColon) {
			return nil, newParseErrorf(&p.current,
				"expected ':' after seed field name %q, got %q", key, p.current.Literal)
		}
		p.advance() // consume ':'

		val, err := p.parseSeedValue(key)
		if err != nil {
			return nil, err
		}
		if _, dup := block.Fields[key]; dup {
			return nil, newParseErrorf(&p.current, "duplicate field %q", key)
		}
		if _, dup := block.Nested[key]; dup {
			return nil, newParseErrorf(&p.current, "name %q used as both field and nested block", key)
		}
		block.Keys = append(block.Keys, key)
		block.Fields[key] = val
	}
	if err := p.expect(TokenBraceClose); err != nil {
		return nil, err
	}
	return block, nil
}

// parseSeedValue reads one RHS scalar or string-array. Mirrors
// parseSeedMemQL's value surface: string, int, float, true / false,
// or `[ "a", "b" ]`. Ref-typed arrays (e.g. `[tool("respondToUser")]`)
// are out of scope for v1 seeds -- callers that need them fall back
// to plain string-array form for tool / knowledge bindings, same as
// the hand-rolled parser.
func (p *Parser) parseSeedValue(fieldName string) (*ast.SeedValue, error) {
	switch {
	case p.check(TokenString):
		v := &ast.SeedValue{Kind: ast.SeedValueString, String: p.current.Literal}
		p.advance()
		return v, nil
	case p.check(TokenBracketOpen):
		return p.parseSeedStringArray(fieldName)
	case p.check(TokenIdentifier):
		switch p.current.Literal {
		case "true":
			p.advance()
			return &ast.SeedValue{Kind: ast.SeedValueBool, Bool: true}, nil
		case "false":
			p.advance()
			return &ast.SeedValue{Kind: ast.SeedValueBool, Bool: false}, nil
		default:
			return nil, newParseErrorf(&p.current,
				"unexpected identifier %q in seed value for field %q (expected quoted string, number, true/false, or [...] array)",
				p.current.Literal, fieldName)
		}
	case p.check(TokenNumber):
		lit := p.current.Literal
		p.advance()
		if strings.ContainsAny(lit, ".eE") {
			f, err := strconv.ParseFloat(lit, 64)
			if err != nil {
				return nil, newParseErrorf(&p.current,
					"invalid float %q in seed field %q: %v", lit, fieldName, err)
			}
			return &ast.SeedValue{Kind: ast.SeedValueFloat, Float: f}, nil
		}
		i, err := strconv.ParseInt(lit, 10, 64)
		if err != nil {
			return nil, newParseErrorf(&p.current,
				"invalid integer %q in seed field %q: %v", lit, fieldName, err)
		}
		return &ast.SeedValue{Kind: ast.SeedValueInt, Int: i}, nil
	}
	return nil, newParseErrorf(&p.current,
		"expected value for seed field %q (string, number, true/false, or [...] array), got %q",
		fieldName, p.current.Literal)
}

// parseSeedStringArray consumes `[ "a", "b", "c" ]`. The hand-rolled
// parser accepts a trailing comma; mirror that here so existing
// authoring style stays valid.
func (p *Parser) parseSeedStringArray(fieldName string) (*ast.SeedValue, error) {
	if err := p.expect(TokenBracketOpen); err != nil {
		return nil, err
	}
	out := []string{}
	for !p.check(TokenBracketClose) && !p.check(TokenEOF) {
		if !p.check(TokenString) {
			return nil, newParseErrorf(&p.current,
				"expected quoted string in seed field %q array, got %q",
				fieldName, p.current.Literal)
		}
		out = append(out, p.current.Literal)
		p.advance()
		if p.check(TokenComma) {
			p.advance()
			continue
		}
	}
	if err := p.expect(TokenBracketClose); err != nil {
		return nil, err
	}
	return &ast.SeedValue{Kind: ast.SeedValueStringArray, StringArray: out}, nil
}
