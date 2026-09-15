package parser

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/language/annotations"
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

	// Which annotations a provider takes is the registry's answer
	// (memql#5359); the switch below translates the legal ones into typed
	// ProviderDecl fields.
	if err := p.checkAnnotations(annotations.Provider, fmt.Sprintf("provider %q", decl.Name), attrs); err != nil {
		return nil, err
	}
	for _, attr := range attrs {
		if attr == nil {
			continue
		}
		switch attr.Name {
		case "description":
			decl.Description = attrStringValue(attr)
		case "vendor":
			// Renamed off @type in memql#5375. A concept's @type is its row
			// kind; the two annotations shared a spelling and nothing else,
			// so neither name meant one thing.
			decl.Vendor = attrStringValue(attr)
		case "type":
			return nil, newParseErrorf(&p.current, "provider %q: @type is retired -- write @vendor(%q) instead. A concept's @type is its row kind, and the two shared a spelling for no reason beyond history (memql#5375). %s",
				decl.Name, attrStringValue(attr), annotations.AttributeRewriteHint)
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
		// @enabled was the explicit-on no-op here until memql#5375 retired
		// it; it falls through to the retired-ledger check below.
		case ast.AttrDisabled:
			// @disabled skips the provider at load (loader does not
			// register it or resolve auth); on a @base it propagates
			// to every @extends child.
			decl.Disabled = true
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
		case p.check(TokenIdentifier) && (p.current.Literal == "true" || p.current.Literal == "false"):
			// BOOLEANS, added for `streaming` (epic memql#5137, D3). Every
			// param was a size or a price until a vendor model needed to say
			// what it CAN do rather than how big it is -- streaming used to be
			// a second provider record per model, and collapsing those two rows
			// into one capability flag is what needs a bool here.
			//
			// Stored as a Go bool rather than the string "true", so a reader
			// asking `Params["streaming"].(bool)` gets an answer and a typo
			// like `streaming "ture"` is a parse error rather than a silent
			// false. The alternative -- reusing the string form the way
			// @default("true") does -- would have made every boolean param a
			// place where a misspelling reads as "off".
			decl.Params[key] = p.current.Literal == "true"
			p.advance()
		default:
			return newParseErrorf(&p.current, "provider %q params %q: expected string, number or boolean literal, got %q", decl.Name, key, p.current.Literal)
		}
	}
	return p.expect(TokenBraceClose)
}

// attrStringValue pulls a single string value off an annotation.
// Returns "" for flag attributes or attributes whose value isn't a
// string. Used for `@vendor("OpenAI")` / `@model("gpt-5-mini")` /
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
