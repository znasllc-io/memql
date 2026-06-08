package parser

import (
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/language/ast"
)

// parseProviderDecl parses a struct-form `provider NAME { ... }`
// declaration. The leading attribute set has already been consumed by
// parseDefinition and is passed in as attrs; this method consumes the
// `provider` keyword onward and walks the body's `auth { ... }` +
// `params { ... }` sub-blocks.
//
// Returns a typed *ast.ProviderDecl with Type / Model / Modality /
// IsDefault / IsBase / Extends pulled out of the attribute set, and
// Params + Auth populated from the body. The caller (memql package's
// providerDeclToProviderConfig) translates this directly to a
// *ProviderConfig with no further parsing.
//
// Both regular providers (`@type @model provider ...`) and `@base`
// providers (vendor-level metadata with no @model) use this single
// path. The two-pass @base / @extends resolution stays on the loader
// side -- the parser is purely syntactic.
func (p *Parser) parseProviderDecl(attrs []*ast.Attribute) (*ast.ProviderDecl, error) {
	if !p.check(TokenIdentifier) || p.current.Literal != "provider" {
		return nil, newParseErrorf(&p.current, "expected 'provider' keyword, got %q", p.current.Literal)
	}
	p.advance()

	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected provider name after 'provider', got %q", p.current.Literal)
	}
	decl := &ast.ProviderDecl{
		Name:   p.readProviderName(),
		Params: map[string]any{},
		Auth:   map[string]string{},
	}
	if decl.Name == "" {
		return nil, newParseErrorf(&p.current, "expected provider name, got empty token")
	}

	// Translate the leading attribute set into typed ProviderDecl
	// fields. Unknown attributes are tolerated -- a future annotation
	// will be picked up at the loader/registry layer rather than
	// rejected here.
	for _, attr := range attrs {
		if attr == nil {
			continue
		}
		switch attr.Name {
		case "description":
			decl.Description = attrStringValue(attr)
		case "type":
			decl.Type = attrStringValue(attr)
		case "model":
			decl.Model = attrStringValue(attr)
		case "modality":
			decl.Modality = attrStringValue(attr)
		case "default":
			decl.IsDefault = true
		case "base":
			decl.IsBase = true
		case "extends":
			decl.Extends = attrStringValue(attr)
		case ast.AttrEnabled:
			// @enabled is the explicit-on form -- a no-op default,
			// kept for symmetry with functions/builtins/prompts.
		case ast.AttrDisabled:
			// @disabled skips the provider at load (loader does not
			// register it or resolve auth); on a @base it propagates
			// to every @extends child.
			decl.Disabled = true
		default:
			return nil, newParseErrorf(&p.current, "provider %q: unknown annotation @%s -- supported: @base, @default, @description, @disabled, @enabled, @extends, @modality, @model, @type", decl.Name, attr.Name)
		}
	}

	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}

	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		if !p.check(TokenIdentifier) {
			return nil, newParseErrorf(&p.current, "provider %q: expected 'auth' or 'params' block, got %q", decl.Name, p.current.Literal)
		}
		blockName := p.current.Literal
		p.advance()

		switch blockName {
		case "auth":
			if err := p.parseProviderAuthBlock(decl); err != nil {
				return nil, err
			}
		case "params":
			if err := p.parseProviderParamsBlock(decl); err != nil {
				return nil, err
			}
		default:
			return nil, newParseErrorf(&p.current, "provider %q: unknown sub-block %q (supported: auth, params)", decl.Name, blockName)
		}
	}

	if err := p.expect(TokenBraceClose); err != nil {
		return nil, err
	}
	return decl, nil
}

// parseProviderAuthBlock parses an `auth { key env("VAR") | key
// "literal" }` body. Each key takes either an `env(...)` call (the
// common case -- secrets stay in env vars; the loader's
// resolveAuthPlaceholders pass substitutes them at startup) or a
// quoted string literal (rare; mostly tests / fixtures).
//
// env("VAR") values are stored as the literal string `${VAR}` to
// match what the legacy hand-rolled parser produced.
func (p *Parser) parseProviderAuthBlock(decl *ast.ProviderDecl) error {
	if err := p.expect(TokenBraceOpen); err != nil {
		return err
	}
	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		if !p.check(TokenIdentifier) {
			return newParseErrorf(&p.current, "provider %q auth: expected key identifier, got %q", decl.Name, p.current.Literal)
		}
		key := p.current.Literal
		p.advance()

		if p.check(TokenIdentifier) && p.current.Literal == "env" {
			p.advance()
			if err := p.expect(TokenParenOpen); err != nil {
				return err
			}
			if !p.check(TokenString) {
				return newParseErrorf(&p.current, "provider %q auth %q: env(...) takes a quoted string, got %q", decl.Name, key, p.current.Literal)
			}
			envName := p.current.Literal
			p.advance()
			if err := p.expect(TokenParenClose); err != nil {
				return err
			}
			decl.Auth[key] = "${" + envName + "}"
			continue
		}

		if p.check(TokenString) {
			decl.Auth[key] = p.current.Literal
			p.advance()
			continue
		}

		return newParseErrorf(&p.current, "provider %q auth %q: expected env(\"...\") or quoted string, got %q", decl.Name, key, p.current.Literal)
	}
	return p.expect(TokenBraceClose)
}

// parseProviderParamsBlock parses a `params { key value }` body.
// Values are quoted strings, integer literals, or floating-point
// literals. The Go-side types stored in the map mirror what the
// legacy parser produced: int / float64 / string.
func (p *Parser) parseProviderParamsBlock(decl *ast.ProviderDecl) error {
	if err := p.expect(TokenBraceOpen); err != nil {
		return err
	}
	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		if !p.check(TokenIdentifier) {
			return newParseErrorf(&p.current, "provider %q params: expected key identifier, got %q", decl.Name, p.current.Literal)
		}
		key := p.current.Literal
		p.advance()

		switch {
		case p.check(TokenString):
			decl.Params[key] = p.current.Literal
			p.advance()
		case p.check(TokenNumber):
			raw := p.current.Literal
			p.advance()
			if strings.ContainsAny(raw, ".eE") {
				f, err := strconv.ParseFloat(raw, 64)
				if err != nil {
					return newParseErrorf(&p.current, "provider %q params %q: invalid float %q: %v", decl.Name, key, raw, err)
				}
				decl.Params[key] = f
			} else {
				i, err := strconv.Atoi(raw)
				if err != nil {
					return newParseErrorf(&p.current, "provider %q params %q: invalid integer %q: %v", decl.Name, key, raw, err)
				}
				decl.Params[key] = i
			}
		default:
			return newParseErrorf(&p.current, "provider %q params %q: expected string or number literal, got %q", decl.Name, key, p.current.Literal)
		}
	}
	return p.expect(TokenBraceClose)
}

// attrStringValue pulls a single string value off an annotation.
// Returns "" for flag attributes or attributes whose value isn't a
// string. Used for `@type("OpenAI")` / `@model("gpt-5-mini")` /
// `@extends("openai")` / etc.
func attrStringValue(attr *ast.Attribute) string {
	if attr == nil || attr.Value == nil {
		return ""
	}
	if s, ok := attr.Value.(string); ok {
		return s
	}
	return ""
}

// readProviderName returns the current identifier literal and
// advances. The lexer already folds `.` and `-` into TokenIdentifier
// (see isIdentifierCharNoColon + the dot-lookahead branch in
// scanIdentifier), so names like `chat54Mini` and `gpt-4o` and
// `nova.preview` all arrive as a single token.
func (p *Parser) readProviderName() string {
	if !p.check(TokenIdentifier) {
		return ""
	}
	name := p.current.Literal
	p.advance()
	return name
}
