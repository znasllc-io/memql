package parser

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql/baseparser"
)

// Parser converts tokens into an AST.
type Parser struct {
	tokens  []Token
	pos     int
	current Token
	uses    []*UseDeclaration // populated during parseFile for implicit concept resolution

	// pendingArgs holds a file-top `args { ... }` block parsed
	// immediately before the next definition. parseFile attaches it
	// to the resulting FunctionDef and clears the field.
	pendingArgs *ArgsSchema

	// deferredErrors collects errors that surface during attribute
	// processing or other post-parse passes where the call chain has
	// no error-return path. parseFile drains the slice after parsing
	// completes and surfaces the first entry.
	deferredErrors []error
}

// recordDeferredError stashes a parse error that surfaces from inside
// a void-returning helper (e.g. attribute processing). parseFile
// surfaces the first one after the per-definition parse completes.
func (p *Parser) recordDeferredError(err error) {
	if err == nil {
		return
	}
	p.deferredErrors = append(p.deferredErrors, err)
}

// NewParser creates a new parser for the given tokens.
func NewParser(tokens []Token) *Parser {
	p := &Parser{
		tokens: tokens,
		pos:    0,
	}
	if len(tokens) > 0 {
		p.current = tokens[0]
	}
	return p
}

// Parse parses the token stream and returns the root AST node.
// For .memql files, this returns a *File containing definitions.
// For single expressions, this returns an ExpressionNode.
func (p *Parser) Parse() (Node, error) {
	if p.check(TokenEOF) {
		return nil, ErrEmptyInput
	}

	// Check for use declarations - indicates a file with imports
	if p.check(TokenKeywordUse) {
		return p.parseFile()
	}

	// Check for an `import (...)` block at file top - the new
	// file-import surface, additive to `use`.
	if p.check(TokenKeywordImport) {
		return p.parseFile()
	}

	// Check for @ attributes - indicates a definition follows
	if p.check(TokenAt) {
		return p.parseFile()
	}

	// Check for Go-style function: func (Type) name(args) (returns) { }
	if p.check(TokenKeywordFunc) {
		return p.parseFile()
	}

	// File-top `args { ... }` block introduces a function definition;
	// dispatch to parseFile.
	if p.check(TokenIdentifier) && p.current.Literal == "args" {
		return p.parseFile()
	}

	// Otherwise, parse as an expression (for direct query execution)
	return p.parseExpression()
}

// ParseFile tokenises the given source and parses it as a full .memql
// file (use-declarations + one or more top-level definitions). It is
// the shared entry point that future Phase 2 consumers (concept loader,
// query parser, automation compiler) will call instead of maintaining
// their own lexer/parser pairs.
//
// For single-expression parses (e.g. a raw query string evaluated via
// engine.Execute) keep using NewParser(tokens).Parse() -- that path
// returns an ExpressionNode when the input lacks a top-level
// definition.
func ParseFile(source string) (*File, error) {
	lexer := NewLexer(source)
	tokens, err := lexer.Tokenize()
	if err != nil {
		return nil, err
	}
	parser := NewParser(tokens)
	return parser.parseFile()
}

// ParseShapeDecl tokenises the given source and parses it as a
// single struct-form shape declaration. Convenience wrapper around
// ParseFile for callers that handle one shape per slice (the
// unified-kinds loader's LoadUnifiedShapes flow). Returns an error
// when the source contains zero or more than one definition, or
// when the single definition isn't a shape.
//
// memql#315 (sub-epic #309 / #306 child C).
func ParseShapeDecl(source string) (*ShapeDecl, error) {
	file, err := ParseFile(source)
	if err != nil {
		return nil, err
	}
	if len(file.Definitions) == 0 {
		return nil, fmt.Errorf("no shape declaration found")
	}
	if len(file.Definitions) > 1 {
		return nil, fmt.Errorf("expected one shape declaration, got %d definitions", len(file.Definitions))
	}
	shape, ok := file.Definitions[0].(*ShapeDecl)
	if !ok {
		return nil, fmt.Errorf("expected shape declaration, got %T", file.Definitions[0])
	}
	return shape, nil
}

// ParseBuiltinDecl tokenises the given source and parses it as a
// single struct-form builtin declaration. Convenience wrapper around
// ParseFile for the unified-kinds loader's LoadUnifiedBuiltins flow.
// Mirrors ParseShapeDecl on the error surface.
//
// memql#318 (sub-epic #309 / #306 child C).
func ParseBuiltinDecl(source string) (*BuiltinDecl, error) {
	file, err := ParseFile(source)
	if err != nil {
		return nil, err
	}
	if len(file.Definitions) == 0 {
		return nil, fmt.Errorf("no builtin declaration found")
	}
	if len(file.Definitions) > 1 {
		return nil, fmt.Errorf("expected one builtin declaration, got %d definitions", len(file.Definitions))
	}
	builtin, ok := file.Definitions[0].(*BuiltinDecl)
	if !ok {
		return nil, fmt.Errorf("expected builtin declaration, got %T", file.Definitions[0])
	}
	return builtin, nil
}

// ParsePromptDecl tokenises the given source and parses it as a
// single struct-form prompt declaration. Convenience wrapper around
// ParseFile for the unified-kinds loader's LoadUnifiedPrompts flow.
// Mirrors ParseBuiltinDecl on the error surface.
//
// memql#319 (sub-epic #309 / #306 child C).
func ParsePromptDecl(source string) (*PromptDecl, error) {
	file, err := ParseFile(source)
	if err != nil {
		return nil, err
	}
	if len(file.Definitions) == 0 {
		return nil, fmt.Errorf("no prompt declaration found")
	}
	if len(file.Definitions) > 1 {
		return nil, fmt.Errorf("expected one prompt declaration, got %d definitions", len(file.Definitions))
	}
	prompt, ok := file.Definitions[0].(*PromptDecl)
	if !ok {
		return nil, fmt.Errorf("expected prompt declaration, got %T", file.Definitions[0])
	}
	return prompt, nil
}

// ParseFile parses a file containing multiple definitions.
func (p *Parser) parseFile() (*File, error) {
	file := &File{
		Definitions: []Node{},
	}

	// Parse `import (...)` blocks at the top of the file. The new
	// import surface lives alongside the legacy `use` directive
	// during the transitional Commit 1 state; the two can coexist
	// in the same file. Commit 3 deletes the `use` path.
	for p.check(TokenKeywordImport) {
		entries, err := p.parseImportBlock()
		if err != nil {
			return nil, err
		}
		file.Imports = append(file.Imports, entries...)
	}

	// Parse use declarations at the top of the file
	for p.check(TokenKeywordUse) {
		decl, err := p.parseUseDeclaration()
		if err != nil {
			return nil, err
		}
		file.Uses = append(file.Uses, decl)
	}

	// A second pass for imports after `use` is intentional: some
	// authors interleave the two during migration. Accept either
	// order at parse time; the loader treats the merged list as a
	// single set.
	for p.check(TokenKeywordImport) {
		entries, err := p.parseImportBlock()
		if err != nil {
			return nil, err
		}
		file.Imports = append(file.Imports, entries...)
	}

	// Store uses on the parser so parseMutationBody can resolve implicit concepts
	p.uses = file.Uses

	for !p.check(TokenEOF) {
		// File-top `args { ... }` block? Parse it and stash on the
		// parser so the next definition picks it up.
		if p.check(TokenIdentifier) && p.current.Literal == "args" {
			argsDef, err := p.parseFileTopArgsBlock()
			if err != nil {
				return nil, err
			}
			p.pendingArgs = argsDef
			continue
		}
		def, err := p.parseDefinition()
		if err != nil {
			return nil, err
		}
		if def != nil {
			file.Definitions = append(file.Definitions, def)
		}
	}

	// Surface any deferred parse errors collected during attribute
	// processing (where the void-returning helpers can't return an
	// error directly). The first deferred error wins; the rest are
	// dropped to keep the error surface focused on the most useful
	// hint.
	if len(p.deferredErrors) > 0 {
		return nil, p.deferredErrors[0]
	}

	return file, nil
}

// parseImportBlock parses an `import (...)` block:
//
//	import (
//	    "./cognition/participant"
//	    "./common/space" as cogSpace
//	    "../other/participant" as other
//	)
//
// The single-line shorthand `import "./foo" [as alias]` (no parens)
// is also accepted for one-off entries. Multiple blocks in one file
// are allowed; the parser appends each block's entries to the
// file's combined import list. Path-validation, root-cap, cycle
// detection, and default-alias derivation all happen later in the
// loader -- this function only parses syntax.
func (p *Parser) parseImportBlock() ([]*ImportDecl, error) {
	if err := p.expect(TokenKeywordImport); err != nil {
		return nil, err
	}

	// Single-line form: import "./path" [as alias]
	if p.check(TokenString) {
		decl, err := p.parseImportEntry()
		if err != nil {
			return nil, err
		}
		return []*ImportDecl{decl}, nil
	}

	// Block form: import ( ... )
	if err := p.expect(TokenParenOpen); err != nil {
		return nil, err
	}

	var entries []*ImportDecl
	for !p.check(TokenParenClose) && !p.check(TokenEOF) {
		// Allow stray commas + semicolons between entries; the lexer
		// strips newlines so the only legal separators are whitespace
		// (already consumed) or comma/semicolon.
		if p.check(TokenComma) || p.check(TokenSemicolon) {
			p.advance()
			continue
		}
		decl, err := p.parseImportEntry()
		if err != nil {
			return nil, err
		}
		entries = append(entries, decl)
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}
	return entries, nil
}

// parseImportEntry parses a single `"./path" [as alias]` entry.
func (p *Parser) parseImportEntry() (*ImportDecl, error) {
	if !p.check(TokenString) {
		return nil, newParseErrorf(&p.current, "expected import path string, got %q", p.current.Literal)
	}
	path := p.current.Literal
	p.advance()

	decl := &ImportDecl{Path: path}

	if p.check(TokenKeywordAs) {
		p.advance()
		if !p.check(TokenIdentifier) {
			return nil, newParseErrorf(&p.current, "expected alias identifier after 'as', got %q", p.current.Literal)
		}
		decl.Alias = p.current.Literal
		p.advance()
	}

	return decl, nil
}

// parseUseDeclaration parses one file-top `use` statement in either
// of the two recognised shapes:
//
//	Form A (legacy):
//	  use <dotted.path> [as <alias>]
//	  use cognition.participant
//	  use cognition.session as cognitionSession
//
//	Form B (canonical post-migration):
//	  use <dotted.path>.{ name1, name2, ... }
//	  use cognition.concepts.{ participant, space }
//	  use common.traits.{ traitIsActiveRecord, traitIsNotDeleted }
//
// Form B names a module file and lists the constructs to pull into
// the importing file's local scope. The lexer breaks `path.{` into
// IDENT + DOT + BRACE, so we look ahead one token after consuming
// the path to decide which shape we're in.
func (p *Parser) parseUseDeclaration() (*UseDeclaration, error) {
	if err := p.expect(TokenKeywordUse); err != nil {
		return nil, err
	}

	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected module path after 'use', got %q", p.current.Literal)
	}

	path := p.current.Literal
	p.advance()

	decl := &UseDeclaration{
		Path:  path,
		Parts: strings.Split(path, "."),
	}

	// Form B: `.{ name, name, ... }` follows the path.
	if p.check(TokenDot) {
		p.advance() // consume '.'
		if err := p.expect(TokenBraceOpen); err != nil {
			return nil, newParseErrorf(&p.current, "expected '{' after '.' in `use %s.{ ... }`, got %q", path, p.current.Literal)
		}
		for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
			if p.check(TokenComma) || p.check(TokenSemicolon) {
				p.advance()
				continue
			}
			if !p.check(TokenIdentifier) {
				return nil, newParseErrorf(&p.current, "expected imported name in `use %s.{ ... }`, got %q", path, p.current.Literal)
			}
			decl.Names = append(decl.Names, p.current.Literal)
			p.advance()
		}
		if err := p.expect(TokenBraceClose); err != nil {
			return nil, err
		}
		if len(decl.Names) == 0 {
			return nil, newParseErrorf(&p.current, "`use %s.{ ... }` must list at least one imported name", path)
		}
		return decl, nil
	}

	// Reject Form A (`use <ns>.<concept> [as <alias>]`). The legacy
	// single-binding shape was retired in the import-model pivot --
	// every `use` clause must now be Form B `use <path>.{ names }`.
	if p.check(TokenKeywordAs) {
		return nil, newParseErrorf(&p.current, "`use %s as <alias>` is retired -- use Form B `use <module>.{ <name> }` instead (alias support removed; rename the construct at source if names collide)", path)
	}
	// A bare `use <ns>.<concept>` (no `.{` body, no `as`) is also Form
	// A and similarly rejected.
	return nil, newParseErrorf(&p.current, "`use %s` is the retired Form A shape -- declare the dependency as Form B `use <module>.{ %s }` instead (file-top import block names the source module + lists the constructs pulled into local scope)", path, path)
}

// parseDefinition parses a single definition (function).
// Supports @attribute Python-style decorators before func declarations.
func (p *Parser) parseDefinition() (Node, error) {
	// Parse any leading attributes (@name, @name(value), @name(key=value), @name({...}))
	var attributes []*Attribute
	for p.check(TokenAt) {
		attr, err := p.parseAttribute()
		if err != nil {
			return nil, err
		}
		if attr != nil {
			attributes = append(attributes, attr)
		}
	}

	var def Node
	var err error

	// If the author wrote attributes BEFORE the args block (typical
	// shape: `@enabled @description("...") args { ... } func (...)`),
	// consume the args block here so it lands on the same definition.
	if p.check(TokenIdentifier) && p.current.Literal == "args" {
		argsDef, err := p.parseFileTopArgsBlock()
		if err != nil {
			return nil, err
		}
		p.pendingArgs = argsDef
	}

	switch {
	case p.check(TokenKeywordFunc):
		// Go-style: func (Type) name(args) (returns) { }
		def, err = p.parseGoStyleFunction()
	case p.check(TokenIdentifier) && p.current.Literal == "concept":
		// Contextual keyword: `concept` at top-of-file introduces a
		// concept declaration. It stays a plain identifier inside
		// query bodies (concept==v1:foo:bar) and inside insert()
		// payloads ({concept: "v1:..."}) -- no keyword promotion in
		// the lexer so those sites keep working.
		def, err = p.parseConceptDecl(attributes)
		if err == nil {
			attributes = nil
		}
	case p.check(TokenIdentifier) && p.current.Literal == "shape":
		// Contextual keyword: `shape` at top-of-file introduces a
		// struct-form shape declaration. Stays a plain identifier
		// elsewhere (e.g. ShapeExpr's `<expr> with shape(...)`).
		// memql#315 (sub-epic #309 / #306 child C).
		def, err = p.parseShapeDecl(attributes)
		if err == nil {
			attributes = nil
		}
	case p.check(TokenIdentifier) && p.current.Literal == "provider":
		// Contextual keyword: `provider` at top-of-file introduces an
		// SI provider declaration. Same rationale as `concept` /
		// `shape` -- kept as a plain identifier so the lexer doesn't
		// disturb other uses, and the symmetry matches every other
		// struct-form construct. memql#316 (sub-epic #309 sibling of
		// #315).
		def, err = p.parseProviderDecl(attributes)
		if err == nil {
			attributes = nil
		}
	case p.check(TokenIdentifier) && p.current.Literal == "builtin":
		// Contextual keyword: `builtin` at top-of-file introduces a
		// struct-form builtin declaration. memql#318
		// (sub-epic #309 / #306 child C).
		def, err = p.parseBuiltinDecl(attributes)
		if err == nil {
			attributes = nil
		}
	case p.check(TokenIdentifier) && p.current.Literal == "tool":
		// Contextual keyword: `tool` at top-of-file introduces a
		// struct-form SI tool declaration. memql#317 (sub-epic #309
		// sibling of #315 / #316 / #318).
		def, err = p.parseToolDecl(attributes)
		if err == nil {
			attributes = nil
		}
	case p.check(TokenIdentifier) && p.current.Literal == "prompt":
		// Contextual keyword: `prompt` at top-of-file introduces a
		// struct-form prompt declaration. memql#319
		// (sub-epic #309 / #306 child C).
		def, err = p.parsePromptDecl(attributes)
		if err == nil {
			attributes = nil
		}
	case p.check(TokenIdentifier) && p.current.Literal == "policy":
		// Contextual keyword: `policy` at top-of-file introduces an
		// SI Router routing-policy declaration. memql#333 (sub-epic
		// #329 / Stage 1C of #310).
		def, err = p.parsePolicyDecl(attributes)
		if err == nil {
			attributes = nil
		}
	case p.check(TokenIdentifier) && p.current.Literal == "spec":
		// Contextual keyword: `spec` at top-of-file introduces a
		// struct-form spec declaration. memql#334 (sub-epic #329 /
		// #310 Stage 1C). Stays a plain identifier inside query
		// bodies (the SpecRefExpression path uses bare names).
		def, err = p.parseSpecDecl(attributes, false)
		if err == nil {
			attributes = nil
		}
	case p.check(TokenIdentifier) && p.current.Literal == "trait":
		// Contextual keyword: `trait` at top-of-file introduces a
		// struct-form trait declaration (same runtime contract as
		// spec; concept-agnostic predicate). memql#334.
		def, err = p.parseSpecDecl(attributes, true)
		if err == nil {
			attributes = nil
		}
	case p.check(TokenIdentifier) && p.current.Literal == "seed":
		// Contextual keyword: `seed` at top-of-file introduces a seed
		// declaration. memql#335 (sub-epic #329 / Stage 1C of #310).
		def, err = p.parseSeedDecl(attributes)
		if err == nil {
			attributes = nil
		}
	default:
		return nil, newParseErrorf(&p.current, "unexpected token %q, expected 'func', 'concept', 'shape', 'provider', 'builtin', 'tool', 'prompt', 'policy', 'spec', 'trait', or 'seed'", p.current.Literal)
	}

	if err != nil {
		return nil, err
	}

	// Attach attributes to the definition
	if len(attributes) > 0 {
		def = p.attachAttributes(def, attributes)
	}

	return def, nil
}

// parseAttribute parses a Python-style @attribute decorator.
// Formats:
//   - @enabled                           (flag, no value)
//   - @description("some text")          (single value)
//   - @trigger(event="session.opened")   (named arguments)
//   - @args({ "userId": {...} })         (object value)
func (p *Parser) parseAttribute() (*Attribute, error) {
	if err := p.expect(TokenAt); err != nil {
		return nil, err
	}

	// Attribute name accepts identifiers and a specific set of
	// keyword-tokens that overlap with legal annotation names
	// (`@default`, `@return`, `@case`, etc.). The lexer promotes
	// those words to keyword tokens for their control-flow role but
	// they're perfectly valid annotation names.
	if !p.check(TokenIdentifier) && !isKeywordTokenForAttribute(p.current.Type) {
		return nil, newParseErrorf(&p.current, "expected attribute name after @, got %q", p.current.Literal)
	}
	name := p.current.Literal
	p.advance()

	attr := &Attribute{
		Name: name,
		Args: make(map[string]any),
	}

	// Check for optional arguments: (...)
	if p.check(TokenParenOpen) {
		p.advance()

		// Empty parens: @name()
		if p.check(TokenParenClose) {
			p.advance()
			return attr, nil
		}

		// @filter(...) captures raw expression as a string
		if name == "filter" && !p.check(TokenString) && !p.check(TokenBraceOpen) {
			// Collect all tokens until matching close paren as raw expression
			depth := 1
			var parts []string
			for depth > 0 && !p.check(TokenEOF) {
				if p.check(TokenParenOpen) {
					depth++
				} else if p.check(TokenParenClose) {
					depth--
					if depth == 0 {
						break
					}
				}
				parts = append(parts, p.current.Literal)
				p.advance()
			}
			attr.Value = strings.Join(parts, "")
			if err := p.expect(TokenParenClose); err != nil {
				return nil, err
			}
			return attr, nil
		}

		// Check for object literal: @args({...})
		if p.check(TokenBraceOpen) {
			obj, err := p.parseObject()
			if err != nil {
				return nil, err
			}
			attr.Value = obj
			if err := p.expect(TokenParenClose); err != nil {
				return nil, err
			}
			return attr, nil
		}

		// String value(s): @description("text") or @visibility("cognition", "bff")
		// A single string stores as string; multiple comma-separated strings
		// store as []string.
		if p.check(TokenString) {
			first := p.current.Literal
			p.advance()
			if p.check(TokenComma) {
				// Multi-value string list.
				values := []string{first}
				for p.check(TokenComma) {
					p.advance()
					if !p.check(TokenString) {
						return nil, newParseErrorf(&p.current, "expected string value after ',' in @%s, got %q", name, p.current.Literal)
					}
					values = append(values, p.current.Literal)
					p.advance()
				}
				attr.Value = values
			} else {
				attr.Value = first
			}
			if err := p.expect(TokenParenClose); err != nil {
				return nil, err
			}
			return attr, nil
		}

		// Numeric value: @version(1). When the entire parenthesised
		// body is a single numeric literal, store it as int64 (bare
		// integer) or float64 (decimal / scientific) so attribute-
		// specific validators can type-check without re-parsing the
		// string. Non-bare numeric forms (@cache(ttl=0)) fall through
		// to the named-args path because they start with an identifier.
		// Uses the shared baseparser.ParseNumericLiteral helper so bare
		// attribute values, runtime expression literals (parsePrimary),
		// and named-arg / array / object element values (parseValue) all
		// produce the same Go type for the same source -- see #255 /
		// #265 (consolidated into baseparser).
		if p.check(TokenNumber) {
			val, numErr := baseparser.ParseNumericLiteral(p.current.Literal)
			if numErr == nil {
				p.advance()
				if p.check(TokenParenClose) {
					attr.Value = val
					p.advance()
					return attr, nil
				}
				return nil, newParseErrorf(&p.current, "expected ')' after numeric attribute value in @%s, got %q", name, p.current.Literal)
			}
			// Token is a TokenNumber but doesn't parse as int or float
			// (shouldn't happen given the lexer's emission rules).
			// Fall through to the rest of the dispatch.
		}

		// Bang-prefixed string for exclude syntax: @visibility(!"planner")
		if p.check(TokenBang) {
			p.advance()
			if !p.check(TokenString) {
				return nil, newParseErrorf(&p.current, "expected string after '!' in @%s, got %q", name, p.current.Literal)
			}
			first := "!" + p.current.Literal
			p.advance()
			if p.check(TokenComma) {
				values := []string{first}
				for p.check(TokenComma) {
					p.advance()
					if p.check(TokenBang) {
						p.advance()
					}
					if !p.check(TokenString) {
						return nil, newParseErrorf(&p.current, "expected string after '!' in @%s exclude list", name)
					}
					values = append(values, "!"+p.current.Literal)
					p.advance()
				}
				attr.Value = values
			} else {
				attr.Value = first
			}
			if err := p.expect(TokenParenClose); err != nil {
				return nil, err
			}
			return attr, nil
		}

		// Named arguments: @trigger(event="value", filter="...")
		for !p.check(TokenParenClose) && !p.check(TokenEOF) {
			if !p.check(TokenIdentifier) {
				return nil, newParseErrorf(&p.current, "expected argument name in attribute, got %q", p.current.Literal)
			}
			argName := p.current.Literal
			p.advance()

			// Expect = or :
			if !p.check(TokenOperator) || (p.current.Literal != "=" && p.current.Literal != ":") {
				// Just a name without value (flag)
				attr.Args[argName] = true
			} else {
				p.advance() // consume = or :
				val, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				attr.Args[argName] = val
			}

			if p.check(TokenComma) {
				p.advance()
			} else {
				break
			}
		}

		if err := p.expect(TokenParenClose); err != nil {
			return nil, err
		}
	}

	return attr, nil
}

// parseGoStyleFunction parses Go-style function syntax:
// func (Type) name(args any) (any, error) { ... }
// func (r Type) name(args any) (any, error) { ... }
func (p *Parser) parseGoStyleFunction() (*FunctionDef, error) {
	if err := p.expect(TokenKeywordFunc); err != nil {
		return nil, err
	}

	// Parse receiver: (Type) or (name Type)
	if err := p.expect(TokenParenOpen); err != nil {
		return nil, err
	}

	receiver := &FunctionReceiver{}

	// Check if we have a named receiver: (a Automation) vs (Automation)
	if p.check(TokenIdentifier) {
		first := p.current.Literal
		p.advance()

		if p.check(TokenKeywordQuery) || p.check(TokenKeywordMutation) || p.check(TokenKeywordAutomation) || p.check(TokenKeywordSpec) || p.check(TokenKeywordTool) || p.check(TokenKeywordBuiltin) {
			// Named receiver: (a Automation)
			receiver.Name = first
			receiver.Type = p.tokenToReceiverType(p.current.Type)
			p.advance()
		} else if p.check(TokenParenClose) {
			// Unnamed receiver with just identifier: treat first as type
			receiver.Type = p.identifierToReceiverType(first)
		} else {
			return nil, newParseErrorf(&p.current, "expected receiver type, got %q", p.current.Literal)
		}
	} else if p.check(TokenKeywordQuery) || p.check(TokenKeywordMutation) || p.check(TokenKeywordAutomation) || p.check(TokenKeywordSpec) || p.check(TokenKeywordTool) || p.check(TokenKeywordBuiltin) {
		// Unnamed receiver: (Automation)
		receiver.Type = p.tokenToReceiverType(p.current.Type)
		p.advance()
	} else {
		return nil, newParseErrorf(&p.current, "expected receiver type (Query, Mutation, Automation, Spec, Tool, or Builtin), got %q", p.current.Literal)
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	// Parse function name
	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected function name, got %q", p.current.Literal)
	}
	name := p.current.Literal
	p.advance()

	// Parse arguments: (args any) or ()
	args, err := p.parseGoStyleArgList()
	if err != nil {
		return nil, err
	}

	// Parse return types: bool or (any, error)
	returns, err := p.parseGoStyleReturns()
	if err != nil {
		return nil, err
	}

	funcType := p.receiverToFunctionType(receiver.Type)
	if err := p.validateGoStyleFunctionSignature(funcType, name, args, returns); err != nil {
		return nil, err
	}

	// Parse function body
	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}

	// Parse body based on receiver type.
	var body Node

	switch funcType {
	case FunctionTypeAutomation:
		body, err = p.parseGoStyleAutomationBody(name)
	case FunctionTypeLogic:
		// Logic functions are procedural blocks called from automation
		// steps. Same body shape as an automation: zero or more
		// `name := <expr>` statements, optional control flow, and a
		// final `return <expr>` terminator. We reuse the automation
		// body parser; the `_return` synthetic step its `return`
		// branch emits is what the executor evaluates as the logic's
		// returned value.
		body, err = p.parseGoStyleAutomationBody(name)
	case FunctionTypeMutation:
		body, err = p.parseGoStyleMutationBodyOrLegacy()
	case FunctionTypeQuery:
		body, err = p.parseGoStyleQueryBodyOrLegacy()
	case FunctionTypeSpec:
		// Specs require an explicit return statement with a boolean expression.
		body, err = p.parseGoStyleSpecBody()
	case FunctionTypePolicy:
		// Policies are pure decision functions. Body shape: `return <expr>`.
		body, err = p.parseGoStyleSpecBody()
	case FunctionTypeTool, FunctionTypeBuiltin:
		// Tool and Builtin definitions have declarative bodies parsed externally.
		// Skip the body content until the closing brace.
		body, err = p.parseGoStyleEmptyOrCommentBody()
	}
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenBraceClose); err != nil {
		return nil, err
	}

	def := &FunctionDef{
		Receiver: receiver,
		Name:     name,
		Type:     funcType,
		Args:     args,
		Returns:  returns,
		Body:     body,
		// Functions default to enabled. @disabled flips this off;
		// @enabled is a no-op kept for backward compatibility.
		// Pre-#360 the default was false, which silently disabled
		// every logic function in the tree that authors had written
		// without @enabled -- including chat-reply (logicGenerateResponse),
		// greet-on-join's auto-join (logicAutoJoinAI), and the
		// retention sweeps. Queries / mutations / automations all
		// carried @enabled explicitly so the flip changes behaviour
		// only for the under-annotated logic surface.
		Enabled: true,
	}
	// If a file-top `args { ... }` block was parsed just before this
	// definition, attach it. The args block is the canonical author
	// surface for the function's input schema; the rewriters drop
	// any block they encountered into the file-top position so this
	// fires for both procedural and (post-rewrite) struct forms.
	if p.pendingArgs != nil {
		def.ArgsSchema = p.pendingArgs
		p.pendingArgs = nil
	}
	return def, nil
}

func (p *Parser) validateGoStyleFunctionSignature(funcType FunctionType, name string, args []FunctionArg, returns []string) error {
	switch funcType {
	case FunctionTypeSpec:
		// Specs may declare zero arguments (implicit row payload) or
		// exactly one (ctx any) / (_ any) parameter under the unified
		// DSL convention. ctx.X inside the body resolves to the row's
		// payload.X (the spec validator normalises ctx → payload at
		// field-reference time).
		if len(args) > 1 {
			return newParseErrorf(&p.current, "spec functions accept at most one argument: (ctx any) or (_ any)")
		}
		if len(args) == 1 && !strings.EqualFold(strings.TrimSpace(args[0].Type), "any") {
			return newParseErrorf(&p.current, "spec %q single argument must be of type any", name)
		}
		if len(returns) != 1 || strings.TrimSpace(strings.ToLower(returns[0])) != "bool" {
			return newParseErrorf(&p.current, "spec functions must declare exactly one return type: bool")
		}
	case FunctionTypeQuery:
		if len(args) > 1 || (len(args) == 1 && !strings.EqualFold(strings.TrimSpace(args[0].Type), "any")) {
			return newParseErrorf(&p.current, "query %q arguments must be empty or exactly one argument of type any", name)
		}
		if len(returns) > 0 {
			if len(returns) != 2 ||
				!strings.EqualFold(strings.TrimSpace(returns[0]), "any") ||
				!strings.EqualFold(strings.TrimSpace(returns[1]), "error") {
				return newParseErrorf(&p.current, "query %q return types must be (any, error) when specified", name)
			}
		}
	case FunctionTypeMutation:
		if len(args) > 1 || (len(args) == 1 && !strings.EqualFold(strings.TrimSpace(args[0].Type), "any")) {
			return newParseErrorf(&p.current, "mutation %q arguments must be empty or exactly one argument of type any", name)
		}
		if len(returns) > 0 {
			if len(returns) != 1 || !strings.EqualFold(strings.TrimSpace(returns[0]), "error") {
				return newParseErrorf(&p.current, "mutation %q return type must be error when specified", name)
			}
		}
	case FunctionTypeAutomation:
		// Automations declare exactly one parameter of type `any`,
		// named either `ctx` (when the body reads it) or `_` (when
		// it doesn't). The trigger event's payload is reachable on
		// ctx for handlers that need it; cron / startup-triggered
		// automations don't, and use `_`.
		if len(args) != 1 {
			return newParseErrorf(&p.current, "automation %q must declare exactly one parameter: (ctx any) or (_ any)", name)
		}
		if !strings.EqualFold(strings.TrimSpace(args[0].Type), "any") {
			return newParseErrorf(&p.current, "automation %q single argument must be of type any", name)
		}
		argName := strings.TrimSpace(args[0].Name)
		if argName != "ctx" && argName != "_" {
			return newParseErrorf(&p.current, "automation %q parameter must be named `ctx` or `_`, got %q", name, argName)
		}
	case FunctionTypeTool:
		// Tools have no args (input schema defined via field declarations) and no return types
		if len(args) > 1 || (len(args) == 1 && !strings.EqualFold(strings.TrimSpace(args[0].Type), "any")) {
			return newParseErrorf(&p.current, "tool %q arguments must be empty or exactly one argument of type any", name)
		}
	case FunctionTypeBuiltin:
		// Builtins follow the same signature rules as queries
		if len(args) > 1 || (len(args) == 1 && !strings.EqualFold(strings.TrimSpace(args[0].Type), "any")) {
			return newParseErrorf(&p.current, "builtin %q arguments must be empty or exactly one argument of type any", name)
		}
	}
	return nil
}

// parseGoStyleArgList parses Go-style function arguments: (args any) or ()
func (p *Parser) parseGoStyleArgList() ([]FunctionArg, error) {
	args := []FunctionArg{}

	if err := p.expect(TokenParenOpen); err != nil {
		return nil, err
	}

	if p.check(TokenParenClose) {
		p.advance()
		return args, nil
	}

	// Parse: name type, name type, ...
	for {
		if !p.check(TokenIdentifier) {
			return nil, newParseErrorf(&p.current, "expected argument name, got %q", p.current.Literal)
		}
		argName := p.current.Literal
		p.advance()

		// Type is required in Go-style syntax
		if !p.check(TokenIdentifier) {
			return nil, newParseErrorf(&p.current, "expected argument type after %q", argName)
		}
		argType := p.current.Literal
		p.advance()

		args = append(args, FunctionArg{
			Name:     argName,
			Type:     argType,
			Required: true,
		})

		if p.check(TokenComma) {
			p.advance()
		} else {
			break
		}
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return args, nil
}

// parseGoStyleReturns parses Go-style return types: bool, (any), or (any, error)
func (p *Parser) parseGoStyleReturns() ([]string, error) {
	returns := []string{}

	// Single return type without parentheses: bool
	if p.check(TokenIdentifier) {
		single := p.current.Literal
		p.advance()
		return []string{single}, nil
	}

	if !p.check(TokenParenOpen) {
		return returns, nil // No return type specified
	}
	p.advance()

	for !p.check(TokenParenClose) && !p.check(TokenEOF) {
		if !p.check(TokenIdentifier) {
			return nil, newParseErrorf(&p.current, "expected return type, got %q", p.current.Literal)
		}
		returns = append(returns, p.current.Literal)
		p.advance()

		if p.check(TokenComma) {
			p.advance()
		} else {
			break
		}
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return returns, nil
}

// parseGoStyleSpecBody parses a spec body and enforces explicit return syntax:
//
//	return <boolean-expression>
//
// Spec bodies are exempt from the ctx-envelope output form because
// they compile into SQL filter predicates and the SQL-pushdown path
// can't tolerate per-row ctx wrapping.
func (p *Parser) parseGoStyleSpecBody() (Node, error) {
	if !p.check(TokenKeywordReturn) {
		return nil, newParseErrorf(&p.current, "spec body must start with 'return'")
	}
	p.advance() // consume return

	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	return expr, nil
}

// parseGoStyleEmptyOrCommentBody parses a body that contains only comments.
// Used for Tool and Builtin definitions where the body is declarative metadata
// parsed externally (not executable MemQL code).
func (p *Parser) parseGoStyleEmptyOrCommentBody() (Node, error) {
	// The lexer already skips comments, so we just need to verify
	// the next token is the closing brace. The body is intentionally empty.
	return nil, nil
}

// parseGoStyleQueryBody enforces:
//
//	return <expr>, <errExpr>
//
// and returns only the query value expression as the function body.
func (p *Parser) parseGoStyleQueryBody() (Node, error) {
	if !p.check(TokenKeywordReturn) {
		return nil, newParseErrorf(&p.current, "query body must start with 'return'")
	}
	p.advance() // consume return

	valueExpr, err := p.parseLogicalAnd()
	if err != nil {
		return nil, err
	}
	if valueExpr == nil {
		return nil, newParseErrorf(&p.current, "query return must include a value expression")
	}

	if !p.check(TokenComma) {
		return nil, newParseErrorf(&p.current, "query return must include error value: return <value>, <error>")
	}
	p.advance() // consume comma

	errExpr, err := p.parseLogicalAnd()
	if err != nil {
		return nil, err
	}
	if errExpr == nil {
		return nil, newParseErrorf(&p.current, "query return must include an error expression")
	}
	if !isNilOrErrorExpr(errExpr) {
		return nil, newParseErrorf(&p.current, "query second return value must be nil or error(...)")
	}

	return valueExpr, nil
}

func (p *Parser) parseGoStyleQueryBodyOrLegacy() (Node, error) {
	// Canonical form: `return <expr>, nil`. Non-return bodies fall
	// through to a bare-expression parse for the internal IR path the
	// rewriter feeds (struct-form query rewrite emits `return <expr>`
	// but the legacy compiler fixtures still hand the parser raw
	// expressions in places).
	if p.check(TokenKeywordReturn) {
		return p.parseGoStyleQueryBody()
	}
	return p.parseExpression()
}

// parseGoStyleMutationBody enforces:
//
//	return insert(...)
//	return nil
//	return error(...)
//
// and returns the mutation statement when insert(...) is returned.
func (p *Parser) parseGoStyleMutationBody() (Node, error) {
	if !p.check(TokenKeywordReturn) {
		return nil, newParseErrorf(&p.current, "mutation body must start with 'return'")
	}
	p.advance() // consume return

	if p.check(TokenIdentifier) {
		// Recognise insert() and update() call forms. Both share the
		// same body parser; the only difference is the kind tag set on
		// the resulting MutationStmt -- the executor branches on Kind
		// to either append a fresh full-payload row (insert) or read
		// the latest, splat the partial payload on top, validate, and
		// append the merged row (update).
		lit := strings.TrimSpace(p.current.Literal)
		if strings.EqualFold(lit, "insert") || strings.EqualFold(lit, "update") {
			return p.parseMutationBody()
		}
	}

	errExpr, err := p.parseLogicalAnd()
	if err != nil {
		return nil, err
	}
	if errExpr == nil {
		return nil, newParseErrorf(&p.current, "mutation return must include an error expression")
	}
	if !isNilOrErrorExpr(errExpr) {
		return nil, newParseErrorf(&p.current, "mutation return must be nil, error(...), or return insert(...) / update(...)")
	}

	// Mutations that return nil/error directly are treated as no-op mutation bodies.
	return &MutationStmt{}, nil
}

func (p *Parser) parseGoStyleMutationBodyOrLegacy() (Node, error) {
	// Canonical form: `return insert(...)` / `return update(...)`.
	// Non-return bodies fall through to the bare insert/update parse
	// for the internal IR path the rewriter feeds.
	if p.check(TokenKeywordReturn) {
		return p.parseGoStyleMutationBody()
	}
	return p.parseMutationBody()
}

func isNilOrErrorExpr(expr ExpressionNode) bool {
	switch expr.(type) {
	case *NilExpr, *ErrorExpr:
		return true
	default:
		return false
	}
}

// parseGoStyleAutomationBody parses Go-style automation body with := assignments
func (p *Parser) parseGoStyleAutomationBody(name string) (*AutomationDef, error) {
	automation := &AutomationDef{
		Name:    name,
		Steps:   []StepDef{},
		Enabled: false,
	}

	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		// Check for return statement
		if p.check(TokenKeywordReturn) {
			p.advance()
			expr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			// Handle optional ", nil" or ", err" after return value
			if p.check(TokenComma) {
				p.advance()
				// Skip the second return value (nil or identifier)
				if p.check(TokenKeywordNil) || p.check(TokenIdentifier) {
					p.advance()
				}
			}
			automation.Steps = append(automation.Steps, StepDef{
				ID:   "_return",
				Type: StepTypeQuery,
				Config: &QueryStepConfig{
					Query: expr,
				},
			})
			continue
		}

		// Check for for-range loop
		if p.check(TokenKeywordFor) {
			step, err := p.parseForRangeStep()
			if err != nil {
				return nil, err
			}
			if step != nil {
				automation.Steps = append(automation.Steps, *step)
			}
			continue
		}

		// Check for if statement
		if p.check(TokenKeywordIf) {
			stmt, err := p.parseIfStatement()
			if err != nil {
				return nil, err
			}
			// Convert if statement to step(s)
			steps := p.ifStatementToSteps(stmt)
			automation.Steps = append(automation.Steps, steps...)
			continue
		}

		// Check for switch statement
		if p.check(TokenKeywordSwitch) {
			step, err := p.parseSwitchStep()
			if err != nil {
				return nil, err
			}
			if step != nil {
				automation.Steps = append(automation.Steps, *step)
			}
			continue
		}

		// Parse step assignment: name := type { ... } or name, err := type { ... }
		if p.check(TokenIdentifier) {
			step, err := p.parseGoStyleStep()
			if err != nil {
				return nil, err
			}
			if step != nil {
				automation.Steps = append(automation.Steps, *step)
			}
			continue
		}

		// Unknown token
		return nil, newParseErrorf(&p.current, "unexpected token in automation body: %q", p.current.Literal)
	}

	return automation, nil
}

// parseGoStyleStep parses a Go-style step: name := type { ... } or name, err := type { ... }
func (p *Parser) parseGoStyleStep() (*StepDef, error) {
	// Parse name(s)
	names := []string{p.current.Literal}
	p.advance()

	// Check for ", err" pattern
	if p.check(TokenComma) {
		p.advance()
		if p.check(TokenIdentifier) {
			names = append(names, p.current.Literal)
			p.advance()
		}
	}

	// Expect :=
	if !p.check(TokenDefine) {
		return nil, newParseErrorf(&p.current, "expected ':=' after step name, got %q", p.current.Literal)
	}
	p.advance()

	// Check for retry(n) wrapper
	retryCount := 0
	if p.check(TokenKeywordRetry) {
		p.advance()
		if err := p.expect(TokenParenOpen); err != nil {
			return nil, err
		}
		if p.check(TokenNumber) {
			retryCount = p.parseIntLiteral()
			p.advance()
		}
		if err := p.expect(TokenParenClose); err != nil {
			return nil, err
		}
	}

	// Function-call step with conditional wrapper:
	//   step := if condition { queryFoo({ ... }) }
	//
	// The single-call form remains the canonical shape -- one
	// StepDef whose ID is the LHS name and whose Condition is the
	// if's predicate.
	//
	// When the if body contains multiple statements (workspaces :=
	// queryFoo(...); for item := range ... { ... }; teardown :=
	// queryBar(...)), parseGoStyleStep can only return one StepDef.
	// Multi-statement bodies are flagged here with a clear error and
	// the author is pointed at the top-level form -- which accepts
	// the same `if cond { multi-stmt }` shape today, via
	// parseIfStatement / ifStatementToSteps in the body parser, and
	// where the assignment LHS is just structural noise anyway (the
	// flattener doesn't bind a name to a multi-step block; the
	// inner steps keep their own IDs).
	if p.check(TokenKeywordIf) {
		p.advance()
		cond, err := p.parseConditionExpression()
		if err != nil {
			return nil, err
		}
		if err := p.expect(TokenBraceOpen); err != nil {
			return nil, err
		}
		// Gated parallel step (memql#1368):
		//   step := if cond { parallel { ... } }
		// The struct-form rewriter emits this for a fan-out layer gated on
		// a prior layer's result. The parallel config is parsed in place
		// and the condition is stamped on the StepDef as usual.
		if p.check(TokenIdentifier) && strings.EqualFold(p.current.Literal, "parallel") && p.peekAhead(1).Type == TokenBraceOpen {
			p.advance() // consume `parallel`
			p.advance() // consume `{`
			cfg, err := p.parseParallelStepConfig()
			if err != nil {
				return nil, err
			}
			if err := p.expect(TokenBraceClose); err != nil { // close the parallel block
				return nil, err
			}
			if err := p.expect(TokenBraceClose); err != nil { // close the if body
				return nil, err
			}
			return &StepDef{
				ID:         names[0],
				Type:       StepTypeParallel,
				Condition:  cond,
				RetryCount: retryCount,
				Config:     cfg,
			}, nil
		}
		// Lookahead: is this body a single bare function-call
		// expression (the legacy single-call form) or multiple
		// statements (the multi-stmt form)?
		//
		// Single-call iff the next non-whitespace token is an
		// identifier followed by `(` AND the matching `)` is the
		// last token before `}`. Easier heuristic: try parsing as
		// an expression and see whether the next token is `}`. If
		// not, the author meant a multi-stmt body and we should
		// reject with a clear migration message.
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		if !p.check(TokenBraceClose) {
			// Multi-statement body. Surface a targeted error rather
			// than the generic "expected '}', got X" that the next
			// expect() would emit -- the migration story is unique
			// to the assignment form so the user shouldn't have to
			// guess.
			return nil, newParseErrorf(&p.current,
				"multi-statement bodies are not supported in the `%s := if cond { ... }` assignment form (the LHS only binds a single result). Drop the `%s := ` prefix and write the if statement at the top level of the body -- the inner statements keep their own names and the if's condition is stamped on each.",
				names[0], names[0])
		}
		if err := p.expect(TokenBraceClose); err != nil {
			return nil, err
		}
		call, ok := expressionToFunctionCall(expr)
		if !ok {
			return nil, newParseErrorf(&p.current,
				"conditional step body must be a function call or builtin; got %T", expr)
		}
		return &StepDef{
			ID:         names[0],
			Type:       StepTypeFunction,
			Condition:  cond,
			RetryCount: retryCount,
			Config: &FunctionStepConfig{
				Name: call.Name,
				Args: call.Args,
			},
		}, nil
	}

	// Function-call step OR expression-builtin step:
	//   step := queryFoo({ ... })
	//   step := coalesce(a, b)
	//   step := cond(pred, a, b)
	//
	// The parser resolves known expression builtins (coalesce, cond,
	// concat, hash, first, last, timestamp, lower, upper, trim) into
	// typed AST nodes rather than generic FunctionCallExpr. At step-
	// assignment position we normalise those to FunctionCallExpr so
	// the compiler pipeline treats them uniformly.
	if p.check(TokenIdentifier) && p.peekAhead(1).Type == TokenParenOpen {
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		call, ok := expressionToFunctionCall(expr)
		if !ok {
			return nil, newParseErrorf(&p.current,
				"step RHS must be a function call or builtin; got %T. "+
					"Wrap raw values in a helper or use 'query { }' / 'mutation { }' blocks.",
				expr)
		}
		return &StepDef{
			ID:         names[0],
			Type:       StepTypeFunction,
			RetryCount: retryCount,
			Config: &FunctionStepConfig{
				Name: call.Name,
				Args: call.Args,
			},
		}, nil
	}

	// Check for step type (inline blocks are rejected in favor of function-call syntax)
	var stepType StepType

	switch {
	case p.check(TokenKeywordQuery):
		stepType = StepTypeQuery
		p.advance()
	case p.check(TokenKeywordMutation):
		stepType = StepTypeMutation
		p.advance()
	case p.check(TokenIdentifier):
		switch strings.ToLower(p.current.Literal) {
		case "query":
			stepType = StepTypeQuery
		case "mutation":
			stepType = StepTypeMutation
		case "parallel":
			// Concurrent fan-out step (memql#1368): the body is the
			// config block parsed by parseParallelStepConfig.
			stepType = StepTypeParallel
		case "action":
			// Action-library replay step (#1758): the body is the config
			// block parsed by parseActionStepConfig.
			stepType = StepTypeAction
		case "shape", "webhook", "event", "publishEvent", "publishevent":
			return nil, newParseErrorf(&p.current, "inline %s blocks are no longer supported; use named function calls instead", p.current.Literal)
		default:
			return nil, newParseErrorf(&p.current, "unknown step type %q", p.current.Literal)
		}
		p.advance()
	default:
		return nil, newParseErrorf(&p.current, "expected step type (query, mutation, webhook, etc.), got %q", p.current.Literal)
	}

	// Check for "if condition" after type
	var condition string
	if p.check(TokenKeywordIf) {
		p.advance()
		cond, err := p.parseConditionExpression()
		if err != nil {
			return nil, err
		}
		condition = cond
	}

	// Parse step body
	if !p.check(TokenBraceOpen) {
		return nil, newParseErrorf(&p.current, "expected '{' after step type, got %q", p.current.Literal)
	}
	p.advance()

	config, err := p.parseStepConfig(stepType)
	if err != nil {
		return nil, err
	}

	if !p.check(TokenBraceClose) {
		return nil, newParseErrorf(&p.current, "expected '}' after step body, got %q", p.current.Literal)
	}
	p.advance()

	return &StepDef{
		ID:         names[0],
		Type:       stepType,
		Condition:  condition,
		RetryCount: retryCount,
		Config:     config,
	}, nil
}

// parseConceptDecl parses a concept declaration:
//
//	concept Name {
//	  field1 string @required @description("...")
//	  field2 object {
//	    nested string
//	  }
//	  @relationship(type="parent", field="x", target="v1:foo:bar", direction="outgoing")
//	}
//
// Concept-level attributes (@description, @scope, @cache, ...) are
// passed in by the caller -- they precede the `concept` keyword in
// source and are collected by parseDefinition before dispatching here.
func (p *Parser) parseConceptDecl(attrs []*Attribute) (*ConceptDecl, error) {
	if !p.check(TokenIdentifier) || p.current.Literal != "concept" {
		return nil, newParseErrorf(&p.current, "expected 'concept' keyword, got %q", p.current.Literal)
	}
	p.advance()

	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected concept name after 'concept', got %q", p.current.Literal)
	}
	decl := &ConceptDecl{
		Name:       p.current.Literal,
		Attributes: attrs,
	}
	p.advance()

	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}

	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		// @relationship(...) and other standalone body-level
		// annotations live inside the concept body. @relationship is
		// typed as RelationshipDecl; everything else would be a
		// property-level annotation, which must precede an actual
		// property declaration.
		if p.check(TokenAt) {
			// Collect a run of annotations. If the run ends on @-
			// annotation that happens to be @relationship, treat that
			// one as body-level; the rest become the next property's
			// prefix attributes.
			var prefix []*Attribute
			for p.check(TokenAt) {
				attr, err := p.parseAttribute()
				if err != nil {
					return nil, err
				}
				if attr == nil {
					continue
				}
				if attr.Name == "relationship" {
					rel, err := attributeToRelationshipDecl(attr)
					if err != nil {
						return nil, err
					}
					decl.Relationships = append(decl.Relationships, rel)
					continue
				}
				prefix = append(prefix, attr)
			}
			if p.check(TokenBraceClose) || p.check(TokenEOF) {
				if len(prefix) > 0 {
					return nil, newParseErrorf(&p.current,
						"dangling @%s annotation at end of concept body; must precede a property", prefix[0].Name)
				}
				break
			}
			prop, err := p.parsePropertyDecl()
			if err != nil {
				return nil, err
			}
			if len(prefix) > 0 {
				prop.Attributes = append(prefix, prop.Attributes...)
			}
			decl.Properties = append(decl.Properties, prop)
			continue
		}

		prop, err := p.parsePropertyDecl()
		if err != nil {
			return nil, err
		}
		decl.Properties = append(decl.Properties, prop)
	}

	if err := p.expect(TokenBraceClose); err != nil {
		return nil, err
	}
	return decl, nil
}

// parseShapeDecl parses a struct-form shape declaration. Two
// signature shapes are accepted, mirroring the hand-rolled
// shape_parser.go this is migrating off of:
//
//	shape <name> { <path>; <path>; ... }              -- bare form
//	shape <Concept> <name> { <path>; <path>; ... }    -- concept-bound
//
// Body grammar: each entry is a dotted-path identifier (the lexer
// consumes `payload.X.Y` as one TokenIdentifier). Entries may be
// separated by `;` or `,` or just whitespace; the closing `}`
// terminates the body. No attributes inside the body (annotations all
// live in the leading attribute cluster captured by parseDefinition).
//
// memql#315 (sub-epic #309 / #306 child C). The kind/namespace
// translation (`row.X` -> `X`, `<concept>.X` -> `payload.X`,
// `actor.X` -> unchanged) is intentionally NOT done here -- the
// memql-side converter (shapeDeclToShapeDefinition) handles it. This
// AST node carries the author-facing paths verbatim so future
// language tooling (Sense, diagnostics) can echo back the user's
// source intact.
func (p *Parser) parseShapeDecl(attrs []*Attribute) (*ShapeDecl, error) {
	if !p.check(TokenIdentifier) || p.current.Literal != "shape" {
		return nil, newParseErrorf(&p.current, "expected 'shape' keyword, got %q", p.current.Literal)
	}
	p.advance()

	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected shape name after 'shape', got %q", p.current.Literal)
	}
	first := p.current.Literal
	p.advance()

	decl := &ShapeDecl{Attributes: attrs}

	// Two-identifier form: `shape <Concept> <name> { ... }`. If the
	// next token is another identifier (not `{`), promote the first
	// identifier to SignatureConcept and use the second as the name.
	if p.check(TokenIdentifier) {
		decl.SignatureConcept = first
		decl.Name = p.current.Literal
		p.advance()
	} else {
		decl.Name = first
	}

	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}

	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		// Skip explicit separators (the author surface tolerates `;`
		// `,` or just whitespace between paths).
		for p.check(TokenSemicolon) || p.check(TokenComma) {
			p.advance()
		}
		if p.check(TokenBraceClose) || p.check(TokenEOF) {
			break
		}
		if !p.check(TokenIdentifier) {
			return nil, newParseErrorf(&p.current,
				"expected field path identifier in shape body, got %q", p.current.Literal)
		}
		// The lexer's scanIdentifier consumes dotted paths
		// (`payload.X.Y`) as a single token, so the path is just the
		// literal of the current TokenIdentifier.
		decl.Paths = append(decl.Paths, p.current.Literal)
		p.advance()
	}

	if err := p.expect(TokenBraceClose); err != nil {
		return nil, err
	}
	return decl, nil
}

// parseBuiltinDecl parses a struct-form builtin declaration:
//
//	builtin <name> {
//	  <field> <type> [@required]
//	  ...
//	}
//
// Body grammar: each field is `name type` followed by zero or more
// `@annotation` attributes. The langparser's lexer already strips
// newlines, so the field boundary is detected structurally -- a
// TokenIdentifier that ISN'T preceded by `@` starts a new field;
// `}` ends the body.
//
// Builtin-level annotations (the cluster captured by parseDefinition)
// carry the operational semantics (`@executor`, `@args`, `@alias`,
// `@description`, `@enabled`, `@sdk`). The converter
// (builtinDeclToFunction in the memql package) interprets them.
//
// memql#318 (sub-epic #309 / #306 child C).
func (p *Parser) parseBuiltinDecl(attrs []*Attribute) (*BuiltinDecl, error) {
	if !p.check(TokenIdentifier) || p.current.Literal != "builtin" {
		return nil, newParseErrorf(&p.current, "expected 'builtin' keyword, got %q", p.current.Literal)
	}
	p.advance()

	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected builtin name after 'builtin', got %q", p.current.Literal)
	}
	decl := &BuiltinDecl{
		Name:       p.current.Literal,
		Attributes: attrs,
	}
	p.advance()

	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}

	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		field, err := p.parseBuiltinField()
		if err != nil {
			return nil, err
		}
		decl.Fields = append(decl.Fields, field)
	}

	if err := p.expect(TokenBraceClose); err != nil {
		return nil, err
	}
	return decl, nil
}

// parseBuiltinField parses one `<name> <type> [@annotation ...]` row
// inside a builtin body. Type accepts primitives and the array-of-
// primitive shorthand `[]primitive`. Field-level annotations are
// captured as Attribute list; only `@required` is acted on today
// (parses semantics-bearing flag), but the slice carries any future
// annotations forward verbatim.
func (p *Parser) parseBuiltinField() (*BuiltinField, error) {
	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected builtin field name, got %q", p.current.Literal)
	}
	field := &BuiltinField{Name: p.current.Literal}
	p.advance()

	// Type: optional `[]` prefix + ident.
	if p.check(TokenBracketOpen) {
		p.advance()
		if err := p.expect(TokenBracketClose); err != nil {
			return nil, err
		}
		field.Type = "[]"
	}
	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current,
			"expected type for builtin field %q, got %q", field.Name, p.current.Literal)
	}
	field.Type += p.current.Literal
	p.advance()

	// Field-level annotations: consume `@attribute(args)` while next
	// token is `@`. The annotation cluster ends as soon as we see an
	// ident (= next field's name) or `}` (= end of body).
	for p.check(TokenAt) {
		attr, err := p.parseAttribute()
		if err != nil {
			return nil, err
		}
		if attr == nil {
			continue
		}
		if attr.Name == "required" {
			field.Required = true
		}
		field.Attributes = append(field.Attributes, attr)
	}

	return field, nil
}

// parsePromptDecl parses a struct-form prompt declaration:
//
//	prompt <name> {
//	  <field> <type> [@required] [@description("...")]
//	  ...
//	}
//
// Body grammar mirrors parseBuiltinDecl: each field is `name type`
// followed by zero or more `@annotation` attributes; the next ident
// (no leading `@`) starts a new field, `}` ends the body.
//
// Prompt-level annotations (the cluster captured by parseDefinition)
// carry the operational semantics (`@description`,
// `@defaultProvider`, `@templateFile`, `@enabled` / `@disabled`).
// The converter (promptDeclToPromptDecl in the memql package)
// interprets them.
//
// Two retired forms are rejected at parse time with a migration
// hint:
//   - `func (Prompt) name { ... }` -- the receiver-function wrapper
//     (caught upstream in parseDefinition via the `func` branch +
//     parseReceiver's known-receiver list).
//   - `@input { ... }` -- the body-level wrapper around the field
//     list. The langparser doesn't expose a corresponding body
//     grammar; an attempt to parse it fails on the `{` after the
//     `input` attribute name.
//
// Inline `@template("""...""")` (a rarely-used alternative to
// `@templateFile`) is NOT supported on this path: the langparser's
// lexer doesn't tokenise triple-quoted strings, and zero shipped
// prompts use the inline form. parsePromptDecl rejects body-level
// `@template` with a migration-pointing error.
//
// memql#319 (sub-epic #309 / #306 child C).
func (p *Parser) parsePromptDecl(attrs []*Attribute) (*PromptDecl, error) {
	if !p.check(TokenIdentifier) || p.current.Literal != "prompt" {
		return nil, newParseErrorf(&p.current, "expected 'prompt' keyword, got %q", p.current.Literal)
	}
	p.advance()

	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected prompt name after 'prompt', got %q", p.current.Literal)
	}
	decl := &PromptDecl{
		Name:       p.current.Literal,
		Attributes: attrs,
	}
	p.advance()

	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}

	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		// Body-level `@template` / `@input` are retired authoring forms.
		// Reject them with a clear migration hint before falling through
		// to field parsing (which would otherwise mis-classify them as
		// "expected field name, got '@'").
		if p.check(TokenAt) {
			if next := p.peekNextIdent(); next == "input" {
				return nil, newParseErrorf(&p.current,
					"`@input { ... }` wrapper is retired -- declare prompt fields directly inside `prompt name { ... }`")
			} else if next == "template" {
				return nil, newParseErrorf(&p.current,
					"inline `@template(...)` body annotation is not supported on the langparser path -- use `@templateFile(\"...\")` with a sidecar .tmpl file")
			}
			return nil, newParseErrorf(&p.current,
				"unexpected '@' in prompt body -- annotations attach to field declarations, not the body itself")
		}
		field, err := p.parsePromptField()
		if err != nil {
			return nil, err
		}
		decl.Fields = append(decl.Fields, field)
	}

	if err := p.expect(TokenBraceClose); err != nil {
		return nil, err
	}
	return decl, nil
}

// parsePromptField parses one `<name> <type> [@annotation ...]` row
// inside a prompt body. Mirrors parseBuiltinField -- prompts and
// builtins share the same per-field grammar; the converter handles
// the (small) semantic differences in how the field surface lowers
// to each construct's internal type.
func (p *Parser) parsePromptField() (*PromptField, error) {
	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected prompt field name, got %q", p.current.Literal)
	}
	field := &PromptField{Name: p.current.Literal}
	p.advance()

	// Type: optional `[]` prefix + ident.
	if p.check(TokenBracketOpen) {
		p.advance()
		if err := p.expect(TokenBracketClose); err != nil {
			return nil, err
		}
		field.Type = "[]"
	}
	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current,
			"expected type for prompt field %q, got %q", field.Name, p.current.Literal)
	}
	field.Type += p.current.Literal
	p.advance()

	// Field-level annotations: consume `@attribute(args)` while next
	// token is `@`. Mirrors parseBuiltinField.
	for p.check(TokenAt) {
		attr, err := p.parseAttribute()
		if err != nil {
			return nil, err
		}
		if attr == nil {
			continue
		}
		if attr.Name == "required" {
			field.Required = true
		}
		field.Attributes = append(field.Attributes, attr)
	}

	return field, nil
}

// peekNextIdent returns the literal of the token immediately
// following p.current, or "" if it's not an identifier. Used by
// parsePromptDecl to look one step ahead at the name following an
// `@` token (`input`, `template`) so we can emit a migration-
// pointing error before falling through to field parsing.
func (p *Parser) peekNextIdent() string {
	t := p.peekAhead(1)
	if t.Type != TokenIdentifier {
		return ""
	}
	return t.Literal
}

// parsePropertyDecl parses a single property declaration inside a
// concept body. Accepts both primitive and nested-block forms:
//
//	fieldA string @required
//	fieldB { inner string @required }
//	fieldC enum("a", "b") @default("a")
//	fieldD array(string)
func (p *Parser) parsePropertyDecl() (*PropertyDecl, error) {
	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected property name, got %q", p.current.Literal)
	}
	prop := &PropertyDecl{Name: p.current.Literal}
	p.advance()

	// Nested-object block: `name { ... }` (no explicit type keyword).
	if p.check(TokenBraceOpen) {
		prop.Type = &TypeRef{Kind: "object"}
		p.advance()
		for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
			nested, err := p.parsePropertyDecl()
			if err != nil {
				return nil, err
			}
			prop.Nested = append(prop.Nested, nested)
		}
		if err := p.expect(TokenBraceClose); err != nil {
			return nil, err
		}
		return prop, nil
	}

	// Typed form: `name type @attrs`.
	typ, err := p.parseTypeRef()
	if err != nil {
		return nil, fmt.Errorf("property %q: %w", prop.Name, err)
	}
	prop.Type = typ

	// Trailing property annotations. Stop as soon as we see
	// `@relationship` -- that one belongs to the concept body, not
	// the property, and the caller (parseConceptDecl) is waiting for
	// it.
	for p.check(TokenAt) {
		if p.peekAhead(1).Type == TokenIdentifier && p.peekAhead(1).Literal == "relationship" {
			break
		}
		attr, err := p.parseAttribute()
		if err != nil {
			return nil, err
		}
		if attr != nil {
			prop.Attributes = append(prop.Attributes, attr)
		}
	}

	// Variant-block body:
	//
	//   credentials object @variant(discriminator="identityType") {
	//     oauth   { provider string @required; ... }
	//     api_key { keyHash string @required }
	//   }
	//
	// Only applies when the annotation set included @variant.
	if hasVariantAttribute(prop.Attributes) && p.check(TokenBraceOpen) {
		p.advance()
		for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
			variant, err := p.parseVariantBranch()
			if err != nil {
				return nil, err
			}
			prop.Variants = append(prop.Variants, variant)
		}
		if err := p.expect(TokenBraceClose); err != nil {
			return nil, err
		}
	}
	return prop, nil
}

// parseVariantBranch parses one `<variantName> { field type ... }`
// block inside a @variant body.
func (p *Parser) parseVariantBranch() (*PropertyVariant, error) {
	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected variant name, got %q", p.current.Literal)
	}
	variant := &PropertyVariant{Name: p.current.Literal}
	p.advance()

	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}
	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		nested, err := p.parsePropertyDecl()
		if err != nil {
			return nil, err
		}
		variant.Properties = append(variant.Properties, nested)
	}
	if err := p.expect(TokenBraceClose); err != nil {
		return nil, err
	}
	return variant, nil
}

// hasVariantAttribute reports whether the attribute list contains an
// @variant annotation -- used to decide whether to expect a variant
// body block after the property's primary declaration.
func hasVariantAttribute(attrs []*Attribute) bool {
	for _, a := range attrs {
		if a != nil && a.Name == "variant" {
			return true
		}
	}
	return false
}

// parseTypeRef parses the type expression of a property. Supported
// forms:
//
//	string | bool | int | float | datetime | any | object    primitives
//	enum("a", "b", ...)                                      inline enum
//	array(T)                                                 legacy slice
//	[]T                                                      Go-style slice
//	map[string]T                                             Go-style map
//
// The Go-style forms (`[]T`, `map[K]V`) land in Phase 6; they parse
// alongside the legacy `array(T)` form so existing `.memql` files
// keep working.
func (p *Parser) parseTypeRef() (*TypeRef, error) {
	// Go-style slice: []T
	if p.check(TokenBracketOpen) {
		p.advance()
		if !p.check(TokenBracketClose) {
			return nil, newParseErrorf(&p.current, "expected ']' in slice type, got %q", p.current.Literal)
		}
		p.advance()
		inner, err := p.parseTypeRef()
		if err != nil {
			return nil, err
		}
		return &TypeRef{Kind: "array", ArrayItem: inner}, nil
	}

	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected type, got %q", p.current.Literal)
	}
	kind := p.current.Literal
	p.advance()

	switch kind {
	case "enum":
		if !p.check(TokenParenOpen) {
			return nil, newParseErrorf(&p.current, "expected '(' after enum, got %q", p.current.Literal)
		}
		p.advance()
		values := []string{}
		for !p.check(TokenParenClose) && !p.check(TokenEOF) {
			if !p.check(TokenString) {
				return nil, newParseErrorf(&p.current, "enum values must be quoted strings, got %q", p.current.Literal)
			}
			values = append(values, p.current.Literal)
			p.advance()
			if p.check(TokenComma) {
				p.advance()
			}
		}
		if err := p.expect(TokenParenClose); err != nil {
			return nil, err
		}
		return &TypeRef{Kind: "enum", EnumValues: values}, nil

	case "array":
		item := &TypeRef{Kind: "string"} // default
		if p.check(TokenParenOpen) {
			p.advance()
			inner, err := p.parseTypeRef()
			if err != nil {
				return nil, err
			}
			item = inner
			if err := p.expect(TokenParenClose); err != nil {
				return nil, err
			}
		}
		return &TypeRef{Kind: "array", ArrayItem: item}, nil

	case "map":
		// Go-style map[K]V. The engine's JSON-Schema builder lowers
		// this to `{"type":"object", "additionalProperties": {type: V}}`
		// -- string keys are the only kind JSON objects support, so
		// the key type must be string (or map alias for string).
		if !p.check(TokenBracketOpen) {
			return nil, newParseErrorf(&p.current, "expected '[' after 'map', got %q", p.current.Literal)
		}
		p.advance()
		keyType, err := p.parseTypeRef()
		if err != nil {
			return nil, err
		}
		if err := p.expect(TokenBracketClose); err != nil {
			return nil, err
		}
		valueType, err := p.parseTypeRef()
		if err != nil {
			return nil, err
		}
		if keyType.Kind != "string" {
			return nil, newParseErrorf(&p.current, "map key type must be string, got %q", keyType.Kind)
		}
		return &TypeRef{Kind: "map", ArrayItem: valueType}, nil

	default:
		// Primitive or concept reference. Format hints for datetime.
		ref := &TypeRef{Kind: kind}
		if kind == "datetime" {
			ref.Format = "date-time"
		}
		return ref, nil
	}
}

// isKeywordTokenForAttribute reports whether a token type that the
// lexer promoted to a keyword is still a valid annotation name. Covers
// the annotations that clash with control-flow keywords in practice:
// @default (vs `default` in switch), @return (documentation
// convention), @case, @for, @if.
func isKeywordTokenForAttribute(t TokenType) bool {
	switch t {
	case TokenKeywordDefault, TokenKeywordReturn, TokenKeywordCase,
		TokenKeywordFor, TokenKeywordIf, TokenKeywordElse,
		TokenKeywordContinue, TokenKeywordBreak, TokenKeywordSwitch,
		TokenKeywordIn, TokenKeywordHas, TokenKeywordNot,
		TokenKeywordAs, TokenKeywordWhere, TokenKeywordWhen:
		return true
	}
	return false
}

// attributeToRelationshipDecl converts a parsed @relationship(...) Attribute
// into the typed RelationshipDecl form. Mirrors the validation the
// legacy concept parser does at annotationToRelationship.
func attributeToRelationshipDecl(attr *Attribute) (*RelationshipDecl, error) {
	if attr == nil {
		return nil, fmt.Errorf("relationship annotation missing")
	}
	get := func(key string) string {
		if attr.Args == nil {
			return ""
		}
		if v, ok := attr.Args[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
			return fmt.Sprintf("%v", v)
		}
		return ""
	}
	return &RelationshipDecl{
		Type:        get("type"),
		Field:       get("field"),
		FieldSource: get("fieldSource"),
		Target:      get("target"),
		Direction:   get("direction"),
	}, nil
}

// expressionToFunctionCall normalises an expression at step-RHS position
// into a FunctionCallExpr. Accepts the generic FunctionCallExpr as-is
// and also every typed expression builtin produced by the parser for
// well-known helpers (coalesce, cond, concat, hash, first, last,
// timestamp, lower, upper, trim). Anything else -- literals, ternaries,
// arithmetic, raw identifiers -- returns false so the caller can emit
// a specific error.
//
// The conversion uses positional argument keys ("0", "1", ...) because
// every builtin at this layer takes positional arguments; the compiler
// pipeline already treats both positional-indexed and named arg maps
// uniformly.
func expressionToFunctionCall(e ExpressionNode) (*FunctionCallExpr, bool) {
	switch t := e.(type) {
	case *FunctionCallExpr:
		return t, true
	case *CoalesceExpr:
		args := make(map[string]any, len(t.Args))
		for i, a := range t.Args {
			args[strconv.Itoa(i)] = a
		}
		return &FunctionCallExpr{Name: "coalesce", Args: args}, true
	case *CondExpr:
		return &FunctionCallExpr{Name: "cond", Args: map[string]any{
			"0": t.Condition, "1": t.Then, "2": t.Else,
		}}, true
	case *ConcatExpr:
		args := make(map[string]any, len(t.Args))
		for i, a := range t.Args {
			args[strconv.Itoa(i)] = a
		}
		return &FunctionCallExpr{Name: "concat", Args: args}, true
	case *HashExpr:
		return &FunctionCallExpr{Name: "hash", Args: map[string]any{"0": t.Target}}, true
	case *ShortIdExpr:
		return &FunctionCallExpr{Name: "shortId", Args: map[string]any{"0": t.Target}}, true
	case *CanonicalIdExpr:
		return &FunctionCallExpr{Name: "canonicalId", Args: map[string]any{
			"0": t.Value, "1": t.Concept,
		}}, true
	case *FirstExpr:
		return &FunctionCallExpr{Name: "first", Args: map[string]any{"0": t.Target}}, true
	case *LastExpr:
		return &FunctionCallExpr{Name: "last", Args: map[string]any{"0": t.Target}}, true
	case *LowerExpr:
		return &FunctionCallExpr{Name: "lower", Args: map[string]any{"0": t.Target}}, true
	case *UpperExpr:
		return &FunctionCallExpr{Name: "upper", Args: map[string]any{"0": t.Target}}, true
	case *TrimExpr:
		return &FunctionCallExpr{Name: "trim", Args: map[string]any{"0": t.Target}}, true
	case *TimestampExpr:
		return &FunctionCallExpr{Name: "timestamp", Args: map[string]any{}}, true
	default:
		return nil, false
	}
}

// parseForRangeStep parses: for item := range collection [if filter] { ... }
func (p *Parser) parseForRangeStep() (*StepDef, error) {
	loopStartPos := p.current.Pos
	loopStartLine := p.current.Line

	if err := p.expect(TokenKeywordFor); err != nil {
		return nil, err
	}

	// Parse: item := range collection or i, item := range collection
	var indexVar, valueVar string

	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected variable name after 'for', got %q", p.current.Literal)
	}
	first := p.current.Literal
	p.advance()

	if p.check(TokenComma) {
		// i, item pattern
		p.advance()
		if !p.check(TokenIdentifier) {
			return nil, newParseErrorf(&p.current, "expected value variable name, got %q", p.current.Literal)
		}
		indexVar = first
		valueVar = p.current.Literal
		p.advance()
	} else {
		// item only
		valueVar = first
	}

	// Enforce strict .memql semantics: for-range item variable must be named "item".
	// This keeps runtime resolution unambiguous and prevents accidental interpretation
	// of arbitrary dotted identifiers as references.
	if valueVar != "item" {
		return nil, newParseErrorf(&p.current, "invalid for-range variable %q (must be %q)", valueVar, "item")
	}

	// Expect :=
	if !p.check(TokenDefine) {
		return nil, newParseErrorf(&p.current, "expected ':=' in for-range, got %q", p.current.Literal)
	}
	p.advance()

	// Expect range
	if !p.check(TokenKeywordRange) {
		return nil, newParseErrorf(&p.current, "expected 'range' keyword, got %q", p.current.Literal)
	}
	p.advance()

	// Parse collection expression (until 'if' or '{')
	collectionParts := []string{}
	for !p.check(TokenEOF) && !p.check(TokenKeywordIf) && !p.check(TokenBraceOpen) {
		collectionParts = append(collectionParts, p.current.Literal)
		p.advance()
	}
	source := strings.Join(collectionParts, "")

	// Optional filter: if condition
	var filter string
	if p.check(TokenKeywordIf) {
		p.advance()
		filterParts := []string{}
		for !p.check(TokenEOF) && !p.check(TokenBraceOpen) {
			filterParts = append(filterParts, p.current.Literal)
			p.advance()
		}
		filter = strings.Join(filterParts, " ")
	}

	// Parse body
	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}

	// Parse nested steps
	var doSteps []StepDef
	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		// Check for continue/break
		if p.check(TokenKeywordContinue) {
			p.advance()
			continue
		}
		if p.check(TokenKeywordBreak) {
			p.advance()
			continue
		}

		// Parse nested step
		if p.check(TokenIdentifier) {
			step, err := p.parseGoStyleStep()
			if err != nil {
				return nil, err
			}
			if step != nil {
				doSteps = append(doSteps, *step)
			}
		} else if p.check(TokenKeywordIf) {
			// Handle if err != nil patterns
			stmt, err := p.parseIfStatement()
			if err != nil {
				return nil, err
			}
			steps := p.ifStatementToSteps(stmt)
			doSteps = append(doSteps, steps...)
		} else {
			p.advance() // Skip unknown tokens in loop body
		}
	}

	if err := p.expect(TokenBraceClose); err != nil {
		return nil, err
	}

	// Generate a unique ID for the forEach step
	// Note: variable name is fixed to "item", so include the loop start position for uniqueness.
	stepId := fmt.Sprintf("forEach_%s_%d_L%d", valueVar, loopStartPos, loopStartLine)

	return &StepDef{
		ID:   stepId,
		Type: StepTypeForEach,
		Config: &ForEachStepConfig{
			Source: source,
			Filter: filter,
			As:     valueVar,
			Index:  indexVar,
			Do:     doSteps,
		},
	}, nil
}

// parseIfStatement parses a Go-style if statement. The body shape
// the parser cares about for Logic / Automation use is:
//
//	if <cond> {
//	  name := funcCall(...)
//	  for item := range collection.Nodes() { ... }
//	  if <nestedCond> { ... }
//	  funcCall(...)            // bare call -- emits an anonymous step
//	}
//	else if <cond2> { ... }
//	else { ... }
//
// All statements inside the then-block are parsed into `ThenSteps`
// (an []StepDef) so the body-flattener (ifStatementToSteps) can stamp
// the if's condition on each step. The legacy continue / break /
// return shape is preserved into `Then` for callers that still walk
// that field, but Logic bodies don't use those terminators inside
// an if block today.
func (p *Parser) parseIfStatement() (*IfStmt, error) {
	if err := p.expect(TokenKeywordIf); err != nil {
		return nil, err
	}

	stmt := &IfStmt{}

	// Parse condition (until '{') -- canonicalised by parseConditionExpression
	// so the runtime evaluator sees the same shape that step.Condition
	// strings carry elsewhere.
	condStr, err := p.parseConditionExpression()
	if err != nil {
		return nil, err
	}
	stmt.Condition = &LiteralExpr{Value: condStr}

	// Parse then block
	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}
	thenSteps, err := p.parseIfBodyStatements()
	if err != nil {
		return nil, err
	}
	stmt.ThenSteps = thenSteps
	if err := p.expect(TokenBraceClose); err != nil {
		return nil, err
	}

	// Check for else / else-if
	if p.check(TokenKeywordElse) {
		p.advance()
		if p.check(TokenKeywordIf) {
			// else if -- recurse. The nested IfStmt's ThenSteps will
			// carry its own condition when ifStatementToSteps walks
			// them; the parent's ElseIf pointer is what signals to the
			// flattener that the negated parent condition should be
			// layered too.
			elseIf, err := p.parseIfStatement()
			if err != nil {
				return nil, err
			}
			stmt.ElseIf = elseIf
		} else {
			if err := p.expect(TokenBraceOpen); err != nil {
				return nil, err
			}
			elseSteps, err := p.parseIfBodyStatements()
			if err != nil {
				return nil, err
			}
			stmt.ElseSteps = elseSteps
			if err := p.expect(TokenBraceClose); err != nil {
				return nil, err
			}
		}
	}

	return stmt, nil
}

// parseIfBodyStatements reads the statements inside an if/else body.
// The opening '{' must have been consumed by the caller; this routine
// reads up to but does NOT consume the matching '}'. Accepts:
//
//   - `name := <funcCall>` step assignments
//   - `for item := range collection.Nodes() { ... }` for-range steps
//   - nested `if cond { ... }` statements
//   - bare function-call expressions (emit an anonymous function step)
//
// Returns the flat ordered list of step defs. The if-statement's
// own condition is NOT stamped here -- that lives in
// ifStatementToSteps so the flattener can combine nested condition
// stacks correctly.
func (p *Parser) parseIfBodyStatements() ([]StepDef, error) {
	var steps []StepDef
	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		switch {
		case p.check(TokenKeywordFor):
			step, err := p.parseForRangeStep()
			if err != nil {
				return nil, err
			}
			if step != nil {
				steps = append(steps, *step)
			}
		case p.check(TokenKeywordIf):
			nested, err := p.parseIfStatement()
			if err != nil {
				return nil, err
			}
			steps = append(steps, p.ifStatementToSteps(nested)...)
		case p.check(TokenIdentifier):
			// Distinguish `name :=` assignment from a bare function call.
			next := p.peekAhead(1)
			if next.Type == TokenDefine || next.Type == TokenComma {
				step, err := p.parseGoStyleStep()
				if err != nil {
					return nil, err
				}
				if step != nil {
					steps = append(steps, *step)
				}
				continue
			}
			if next.Type == TokenParenOpen {
				// Bare function call: emit an anonymous function step.
				// The auto-generated ID keys off the parser position so
				// downstream uniqueness checks (validateSteps' duplicate
				// id guard) hold even when multiple anonymous steps share
				// the same callee name.
				pos := p.current.Pos
				line := p.current.Line
				expr, err := p.parseExpression()
				if err != nil {
					return nil, err
				}
				call, ok := expressionToFunctionCall(expr)
				if !ok {
					return nil, newParseErrorf(&p.current,
						"if-body statement must be an assignment, for-range, nested if, or function call; got %T", expr)
				}
				steps = append(steps, StepDef{
					ID:   fmt.Sprintf("anon_%d_L%d", pos, line),
					Type: StepTypeFunction,
					Config: &FunctionStepConfig{
						Name: call.Name,
						Args: call.Args,
					},
				})
				continue
			}
			return nil, newParseErrorf(&p.current,
				"unexpected token after identifier %q in if-body (expected ':=' or '(')", p.current.Literal)
		case p.check(TokenKeywordContinue):
			// Inherited shape -- continue / break terminators only
			// matter inside a for-range body; an if-body inside a
			// for-range body uses them as inner loop control.
			stmt := &ContinueStmt{}
			p.advance()
			// No corresponding StepDef -- the legacy Then slot
			// captured these, but the flattener only walks ThenSteps,
			// so this is effectively a no-op for the runtime today.
			_ = stmt
		case p.check(TokenKeywordBreak):
			p.advance()
		default:
			// Anything else gets reported with a clear error rather
			// than silently skipped (the legacy behaviour was the
			// silent-drop that made every multi-stmt if body invisible
			// at runtime).
			return nil, newParseErrorf(&p.current,
				"unexpected token in if-body: %q", p.current.Literal)
		}
	}
	return steps, nil
}

// parseSwitchStep parses a Go-style switch statement as a step
func (p *Parser) parseSwitchStep() (*StepDef, error) {
	if err := p.expect(TokenKeywordSwitch); err != nil {
		return nil, err
	}

	// Parse expression (until '{')
	exprParts := []string{}
	for !p.check(TokenEOF) && !p.check(TokenBraceOpen) {
		exprParts = append(exprParts, p.current.Literal)
		p.advance()
	}
	expression := strings.Join(exprParts, "")

	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}

	cases := make(map[string]*SwitchCase)
	var defaultCase *SwitchCase

	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		if p.check(TokenKeywordCase) {
			p.advance()
			// Parse case value
			if !p.check(TokenString) && !p.check(TokenIdentifier) {
				return nil, newParseErrorf(&p.current, "expected case value, got %q", p.current.Literal)
			}
			caseValue := p.current.Literal
			p.advance()

			if err := p.expect(TokenColon); err != nil {
				return nil, err
			}

			// Parse case body (steps until next case/default/})
			var caseSteps []StepDef
			for !p.check(TokenKeywordCase) && !p.check(TokenKeywordDefault) && !p.check(TokenBraceClose) && !p.check(TokenEOF) {
				if p.check(TokenIdentifier) {
					step, err := p.parseGoStyleStep()
					if err != nil {
						return nil, err
					}
					if step != nil {
						caseSteps = append(caseSteps, *step)
					}
				} else {
					p.advance()
				}
			}
			cases[caseValue] = &SwitchCase{Steps: caseSteps}

		} else if p.check(TokenKeywordDefault) {
			p.advance()
			if err := p.expect(TokenColon); err != nil {
				return nil, err
			}

			var defaultSteps []StepDef
			for !p.check(TokenKeywordCase) && !p.check(TokenBraceClose) && !p.check(TokenEOF) {
				if p.check(TokenIdentifier) {
					step, err := p.parseGoStyleStep()
					if err != nil {
						return nil, err
					}
					if step != nil {
						defaultSteps = append(defaultSteps, *step)
					}
				} else {
					p.advance()
				}
			}
			defaultCase = &SwitchCase{Steps: defaultSteps}
		} else {
			p.advance()
		}
	}

	if err := p.expect(TokenBraceClose); err != nil {
		return nil, err
	}

	return &StepDef{
		ID:   "switch_" + expression,
		Type: StepTypeSwitch,
		Config: &SwitchStepConfig{
			Expression: expression,
			Cases:      cases,
			Default:    defaultCase,
		},
	}, nil
}

// ifStatementToSteps flattens a parsed IfStmt into a list of
// conditional StepDefs. Each step in stmt.ThenSteps gets the if's
// condition stamped on top of its own (combined with `and` when the
// step already carries an inner condition from a nested `name := if`
// or a deeper if branch). ElseSteps + ElseIf chains are layered with
// the negated parent condition so the runtime evaluator sees a flat
// list of always-gated steps rather than a nested if structure.
//
// Returns an empty slice for an empty if body (legacy behaviour: a
// vacuous if at the top level is a no-op).
func (p *Parser) ifStatementToSteps(stmt *IfStmt) []StepDef {
	if stmt == nil {
		return nil
	}
	cond := conditionString(stmt.Condition)
	var out []StepDef
	for _, step := range stmt.ThenSteps {
		out = append(out, stampStepCondition(step, cond))
	}
	// Negate the parent condition for the else branch. ElseIf takes
	// priority over ElseSteps (matches the parser's wiring: an else-if
	// chain doesn't carry plain ElseSteps).
	negated := negateCondition(cond)
	if stmt.ElseIf != nil {
		for _, step := range p.ifStatementToSteps(stmt.ElseIf) {
			out = append(out, stampStepCondition(step, negated))
		}
	} else if len(stmt.ElseSteps) > 0 {
		for _, step := range stmt.ElseSteps {
			out = append(out, stampStepCondition(step, negated))
		}
	}
	return out
}

// stampStepCondition combines the outer (if-statement) condition with
// the step's existing condition. When the step has no inner condition
// the outer wins as-is. When both are present they get ANDed in
// parenthesised form so operator-precedence quirks in either source
// don't bite the runtime evaluator.
func stampStepCondition(step StepDef, outer string) StepDef {
	if outer == "" {
		return step
	}
	if step.Condition == "" {
		step.Condition = outer
	} else {
		step.Condition = "(" + outer + ") and (" + step.Condition + ")"
	}
	// For-range steps own a list of inner Do steps; the inner steps
	// don't inherit the outer Condition automatically, so we walk
	// them and stamp the same outer string. Without this an
	// `if cond { for item := range x { body } }` body would run the
	// inner mutation regardless of the outer cond when the for-range
	// gate fires.
	if cfg, ok := step.Config.(*ForEachStepConfig); ok && cfg != nil {
		for i := range cfg.Do {
			cfg.Do[i] = stampStepCondition(cfg.Do[i], outer)
		}
	}
	return step
}

// conditionString unwraps the LiteralExpr the parser produces for
// if-condition tokens. Empty when the expression is anything else
// (defensive -- the parser today always emits LiteralExpr from
// parseConditionExpression).
func conditionString(expr ExpressionNode) string {
	if expr == nil {
		return ""
	}
	if lit, ok := expr.(*LiteralExpr); ok {
		if s, ok := lit.Value.(string); ok {
			return s
		}
	}
	return ""
}

// negateCondition wraps a condition string in `not (...)` so the
// else-branch flattener can re-use the same evaluator path as the
// then-branch. Returns the empty string when the input is empty
// (means: no else gating beyond the inner step's own condition).
func negateCondition(cond string) string {
	if cond == "" {
		return ""
	}
	return "not (" + cond + ")"
}

// tokenToReceiverType converts a token type to ReceiverType
func (p *Parser) tokenToReceiverType(tt TokenType) ReceiverType {
	switch tt {
	case TokenKeywordQuery:
		return ReceiverQuery
	case TokenKeywordMutation:
		return ReceiverMutation
	case TokenKeywordAutomation:
		return ReceiverAutomation
	case TokenKeywordSpec:
		return ReceiverSpec
	case TokenKeywordTool:
		return ReceiverTool
	case TokenKeywordBuiltin:
		return ReceiverBuiltin
	default:
		return ReceiverQuery
	}
}

// identifierToReceiverType converts an identifier string to ReceiverType
func (p *Parser) identifierToReceiverType(name string) ReceiverType {
	switch strings.ToLower(name) {
	case "query":
		return ReceiverQuery
	case "mutation":
		return ReceiverMutation
	case "automation":
		return ReceiverAutomation
	case "logic":
		return ReceiverLogic
	case "spec":
		return ReceiverSpec
	case "tool":
		return ReceiverTool
	case "builtin":
		return ReceiverBuiltin
	case "prompt":
		return ReceiverPrompt
	case "provider":
		return ReceiverProvider
	case "shape":
		return ReceiverShape
	case "policy":
		return ReceiverPolicy
	default:
		return ReceiverQuery
	}
}

// receiverToFunctionType converts ReceiverType to FunctionType
func (p *Parser) receiverToFunctionType(rt ReceiverType) FunctionType {
	switch rt {
	case ReceiverQuery:
		return FunctionTypeQuery
	case ReceiverMutation:
		return FunctionTypeMutation
	case ReceiverAutomation:
		return FunctionTypeAutomation
	case ReceiverLogic:
		return FunctionTypeLogic
	case ReceiverSpec:
		return FunctionTypeSpec
	case ReceiverTool:
		return FunctionTypeTool
	case ReceiverBuiltin:
		return FunctionTypeBuiltin
	case ReceiverPolicy:
		return FunctionTypePolicy
	default:
		return FunctionTypeQuery
	}
}

// parseIntLiteral parses the current token as an integer
func (p *Parser) parseIntLiteral() int {
	val, _ := strconv.Atoi(p.current.Literal)
	return val
}

// attachAttributes attaches attributes to a definition
func (p *Parser) attachAttributes(def Node, attributes []*Attribute) Node {
	switch d := def.(type) {
	case *FunctionDef:
		d.Attributes = attributes
		p.processFunctionAttributes(d, attributes)
		if automation, ok := d.Body.(*AutomationDef); ok {
			automation.Attributes = attributes
			p.processAutomationAttributes(automation, attributes)
		}
		return d
	case *AutomationDef:
		d.Attributes = attributes
		p.processAutomationAttributes(d, attributes)
		return d
	}
	return def
}

// getAttrString extracts a string value from an attribute (from Value or Args[""])
func getAttrString(attr *Attribute) string {
	if s, ok := attr.Value.(string); ok {
		return s
	}
	if s, ok := attr.Args[""].(string); ok {
		return s
	}
	return ""
}

// getAttrArgString extracts a string from attribute Args by key
func getAttrArgString(attr *Attribute, key string) string {
	if v, ok := attr.Args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// processFunctionAttributes processes attributes for a function definition
func (p *Parser) processFunctionAttributes(d *FunctionDef, attributes []*Attribute) {
	for _, attr := range attributes {
		switch attr.Name {
		case AttrEnabled:
			d.Enabled = true
		case AttrDisabled:
			d.Enabled = false
		case AttrDeprecated:
			d.Deprecated = "deprecated"
			if v := getAttrString(attr); v != "" {
				d.Deprecated = v
			}
		case AttrVersion:
			d.Version = getAttrString(attr)
		case AttrDescription:
			d.Description = getAttrString(attr)
		case AttrInternal:
			d.Internal = true
		case AttrRole:
			d.Role = getAttrString(attr)
		case AttrPermission:
			d.Permission = getAttrString(attr)
		case AttrTimeout:
			d.Timeout = getAttrString(attr)
		case AttrCache:
			if v := getAttrArgString(attr, "ttl"); v != "" {
				d.CacheTTL = v
			} else {
				d.CacheTTL = getAttrString(attr)
			}
		case AttrRetry:
			if v := getAttrArgString(attr, "count"); v != "" {
				d.Retry, _ = strconv.Atoi(v)
			} else if v := getAttrString(attr); v != "" {
				d.Retry, _ = strconv.Atoi(v)
			}
		case AttrIdempotent:
			d.Idempotent = true
		case AttrAudit:
			d.Audit = true
		}
	}
}

// processAutomationAttributes processes attributes for an automation
func (p *Parser) processAutomationAttributes(d *AutomationDef, attributes []*Attribute) {
	for _, attr := range attributes {
		switch attr.Name {
		case AttrEnabled:
			d.Enabled = true
		case AttrDisabled:
			d.Enabled = false
		case AttrDeprecated:
			d.Deprecated = "deprecated"
			if v := getAttrString(attr); v != "" {
				d.Deprecated = v
			}
		case AttrVersion:
			d.Version = getAttrString(attr)
		case AttrDescription:
			d.Description = getAttrString(attr)
		case AttrInternal:
			d.Internal = true
		case AttrRole:
			d.Role = getAttrString(attr)
		case AttrTimeout:
			d.Timeout = getAttrString(attr)
		case AttrRetry:
			if v := getAttrArgString(attr, "count"); v != "" {
				d.Retry, _ = strconv.Atoi(v)
			} else if v := getAttrString(attr); v != "" {
				d.Retry, _ = strconv.Atoi(v)
			}
		case AttrAudit:
			d.Audit = true
		case AttrAsync:
			d.Async = true
		case AttrSchedule:
			if v := getAttrArgString(attr, "cron"); v != "" {
				d.Schedule = v
			} else {
				d.Schedule = getAttrString(attr)
			}
		case AttrTrigger:
			if d.Trigger == nil {
				d.Trigger = &TriggerDef{}
			}
			if v := getAttrArgString(attr, "event"); v != "" {
				d.Trigger.Event = v
			}
			if v := getAttrArgString(attr, "filter"); v != "" {
				d.Trigger.Filter = v
			}
			// @trigger(schedule="0 0 0 * * *") -- accepted as a synonym for
			// @schedule(cron="..."). Historically the scheduler silently
			// dropped this form; any automation using it never ran. Kept
			// working because event-triggered and schedule-triggered
			// automations coexist on the same AutomationDef.
			if v := getAttrArgString(attr, "schedule"); v != "" {
				d.Schedule = v
			}
		case AttrFilter:
			// @filter(...) as standalone annotation — sets trigger filter
			if d.Trigger == nil {
				d.Trigger = &TriggerDef{}
			}
			// Accept @filter("expression") or @filter(expression)
			if v := getAttrString(attr); v != "" {
				d.Trigger.Filter = v
			} else if v := getAttrArgString(attr, ""); v != "" {
				d.Trigger.Filter = v
			}
		}
	}
}

// parseArgsFields converts a map of field definitions to ArgsField slice
// Supports:
//   - "fieldName": "type" (required field)
//   - "fieldName": "type?" (optional field, trailing ?)
//   - "fieldName": {...} (nested object with its own field assertions)
func (p *Parser) parseArgsFields(obj map[string]any) []*ArgsField {
	return p.parseArgsFieldsWithRequired(obj, nil)
}

func (p *Parser) parseArgsFieldsWithRequired(obj map[string]any, required map[string]bool) []*ArgsField {
	fields := []*ArgsField{}
	for name, value := range obj {
		field := p.parseNamedArgsField(name, value)
		if field == nil {
			continue
		}
		// In schema-style object definitions, non-required fields are optional.
		// If required map is provided (even if empty), use it to determine optionality.
		if required != nil {
			field.Optional = !required[name]
		}
		fields = append(fields, field)
	}
	return fields
}

func (p *Parser) parseNamedArgsField(name string, value any) *ArgsField {
	field := &ArgsField{
		Name:     name,
		Optional: false,
		Type:     "any",
	}

	switch v := value.(type) {
	case string:
		// Parse type with optional marker and enum: "string?|active,idle,left"
		// Format: <type>[?][|<enum1>,<enum2>,...]
		typePart := v
		if idx := strings.Index(v, "|"); idx >= 0 {
			typePart = v[:idx]
			enumPart := v[idx+1:]
			for _, e := range strings.Split(enumPart, ",") {
				e = strings.TrimSpace(e)
				if e != "" {
					field.Enum = append(field.Enum, e)
				}
			}
		}
		if strings.HasSuffix(typePart, "?") {
			field.Type = strings.TrimSuffix(typePart, "?")
			field.Optional = true
		} else {
			field.Type = typePart
		}
		return field
	case map[string]any:
		// Supports both shorthand nested objects and schema-style object declarations.
		if !hasSchemaKeys(v) {
			field.Type = "object"
			field.Nested = p.parseArgsFields(v)
			return field
		}
		return p.parseSchemaArgsField(name, v)
	default:
		return field
	}
}

func (p *Parser) parseSchemaArgsField(name string, schema map[string]any) *ArgsField {
	field := &ArgsField{
		Name: name,
		Type: "any",
	}

	if typeRaw, ok := schema["type"].(string); ok && strings.TrimSpace(typeRaw) != "" {
		if strings.HasSuffix(typeRaw, "?") {
			field.Type = strings.TrimSuffix(typeRaw, "?")
			field.Optional = true
		} else {
			field.Type = typeRaw
		}
	}
	if optional, ok := toBool(schema["optional"]); ok {
		field.Optional = optional
	}
	if field.Type == "any" {
		if _, ok := schema["properties"]; ok {
			field.Type = "object"
		}
	}

	if enumVals, ok := schema["enum"].([]any); ok {
		field.Enum = append([]any(nil), enumVals...)
	}
	if minimum, ok := toFloat64(schema["minimum"]); ok {
		field.Minimum = &minimum
	}
	if maximum, ok := toFloat64(schema["maximum"]); ok {
		field.Maximum = &maximum
	}
	if format, ok := schema["format"].(string); ok {
		field.Format = strings.TrimSpace(format)
	}
	if additionalProps, ok := toBool(schema["additionalProperties"]); ok {
		field.AdditionalProperties = &additionalProps
	}

	if propsRaw, ok := schema["properties"]; ok {
		if props, ok := propsRaw.(map[string]any); ok {
			field.Type = "object"
			field.Nested = p.parseArgsFieldsWithRequired(props, parseRequiredSet(schema["required"]))
		}
	}
	if itemsRaw, ok := schema["items"]; ok {
		field.Type = "array"
		field.Items = p.parseUnnamedArgsField(itemsRaw)
	}

	return field
}

func (p *Parser) parseUnnamedArgsField(value any) *ArgsField {
	field := &ArgsField{Name: "", Type: "any"}

	switch v := value.(type) {
	case string:
		if strings.HasSuffix(v, "?") {
			field.Type = strings.TrimSuffix(v, "?")
			field.Optional = true
		} else {
			field.Type = v
		}
	case map[string]any:
		if !hasSchemaKeys(v) {
			field.Type = "object"
			field.Nested = p.parseArgsFields(v)
			return field
		}
		return p.parseSchemaArgsField("", v)
	default:
		return field
	}

	return field
}

func hasSchemaKeys(obj map[string]any) bool {
	if obj == nil {
		return false
	}
	for _, key := range []string{
		"type", "enum", "minimum", "maximum", "format",
		"properties", "required", "additionalProperties", "items", "optional",
	} {
		if _, ok := obj[key]; ok {
			return true
		}
	}
	return false
}

func parseRequiredSet(raw any) map[string]bool {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	required := make(map[string]bool, len(list))
	for _, item := range list {
		if name, ok := item.(string); ok && strings.TrimSpace(name) != "" {
			required[strings.TrimSpace(name)] = true
		}
	}
	if len(required) == 0 {
		return nil
	}
	return required
}

func toBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}

// parseStep parses a single step in an automation (used by step config parsers).
func (p *Parser) parseStep() (*StepDef, error) {
	// Step format: stepName: stepType [when condition] { ... }
	if !p.check(TokenIdentifier) {
		return nil, nil
	}

	stepId := p.current.Literal
	p.advance()

	if !p.check(TokenColon) {
		// Not a step definition, might be an expression
		// Backtrack not possible in simple parser, return error
		return nil, newParseErrorf(&p.current, "expected ':' after step name %q", stepId)
	}
	p.advance() // consume ':'

	// Step type
	var stepType StepType
	switch {
	case p.check(TokenKeywordQuery):
		stepType = StepTypeQuery
		p.advance()
	case p.check(TokenKeywordMutation):
		stepType = StepTypeMutation
		p.advance()
	case p.check(TokenIdentifier):
		switch strings.ToLower(p.current.Literal) {
		case "shape", "webhook", "event", "publishevent":
			return nil, newParseErrorf(&p.current, "inline %s blocks are no longer supported; use named function calls instead", p.current.Literal)
		case "foreach":
			stepType = StepTypeForEach
		case "parallel":
			stepType = StepTypeParallel
		case "switch":
			stepType = StepTypeSwitch
		case "function":
			stepType = StepTypeFunction
		case "action":
			stepType = StepTypeAction
		default:
			stepType = StepTypeQuery // default to query
		}
		p.advance()
	default:
		return nil, newParseErrorf(&p.current, "expected step type after ':', got %q", p.current.Literal)
	}

	// Optional "when" condition
	var condition string
	if p.check(TokenKeywordWhen) {
		p.advance()
		cond, err := p.parseConditionExpression()
		if err != nil {
			return nil, err
		}
		condition = cond
	}

	// Step body in braces
	if !p.check(TokenBraceOpen) {
		return nil, newParseErrorf(&p.current, "expected '{' after step type, got %q", p.current.Literal)
	}
	p.advance()

	// Parse step content based on type
	config, err := p.parseStepConfig(stepType)
	if err != nil {
		return nil, err
	}

	if !p.check(TokenBraceClose) {
		return nil, newParseErrorf(&p.current, "expected '}' after step body, got %q", p.current.Literal)
	}
	p.advance()

	return &StepDef{
		ID:        stepId,
		Type:      stepType,
		Condition: condition,
		Config:    config,
	}, nil
}

// parseStepConfig parses the configuration for a specific step type.
func (p *Parser) parseStepConfig(stepType StepType) (any, error) {
	switch stepType {
	case StepTypeQuery:
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		return &QueryStepConfig{Query: expr}, nil

	case StepTypeMutation:
		mutation, err := p.parseMutationBody()
		if err != nil {
			return nil, err
		}
		return &MutationStepConfig{Mutation: mutation}, nil

	case StepTypeForEach:
		return p.parseForEachStepConfig()

	case StepTypeParallel:
		return p.parseParallelStepConfig()

	case StepTypeSwitch:
		return p.parseSwitchStepConfig()

	case StepTypeFunction:
		return p.parseFunctionStepConfig()

	case StepTypeAction:
		return p.parseActionStepConfig()

	default:
		// For other step types, skip until closing brace
		return nil, nil
	}
}

// parseActionStepConfig parses an action-library replay step body:
//
//	action { ref: "act_x@3", args: { ... }, surface: "workbench" }
//
// ref (the pinned-by-default action reference) is required; args + surface
// are optional. (#1758, epic #1734.)
func (p *Parser) parseActionStepConfig() (any, error) {
	cfg := &ActionStepConfig{
		Args: map[string]any{},
	}

	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		if !p.check(TokenIdentifier) {
			p.advance()
			continue
		}

		key := strings.ToLower(p.current.Literal)
		p.advance()

		// Support both "key: value" and "key=value" forms.
		if p.check(TokenColon) || (p.check(TokenOperator) && p.current.Literal == "=") {
			p.advance()
		} else {
			continue
		}

		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}

		switch key {
		case "ref", "action":
			if str, ok := val.(string); ok {
				cfg.Ref = str
			}
		case "surface":
			if str, ok := val.(string); ok {
				cfg.Surface = str
			}
		case "args":
			if m, ok := val.(map[string]any); ok {
				cfg.Args = m
			}
		}

		if p.check(TokenComma) {
			p.advance()
		}
	}

	if cfg.Ref == "" {
		return nil, newParseErrorf(&p.current, "action step requires a non-empty ref (e.g. ref: \"act_x@3\")")
	}
	return cfg, nil
}

func (p *Parser) parseFunctionStepConfig() (any, error) {
	cfg := &FunctionStepConfig{
		Args: map[string]any{},
	}

	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		if !p.check(TokenIdentifier) {
			p.advance()
			continue
		}

		key := strings.ToLower(p.current.Literal)
		p.advance()

		// Support both "name: value" and "name=value" forms
		if p.check(TokenColon) || (p.check(TokenOperator) && p.current.Literal == "=") {
			p.advance()
		} else {
			continue
		}

		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}

		switch key {
		case "name":
			if str, ok := val.(string); ok {
				cfg.Name = str
			}
		case "args":
			if m, ok := val.(map[string]any); ok {
				cfg.Args = m
			}
		}

		if p.check(TokenComma) {
			p.advance()
		}
	}

	if cfg.Name == "" {
		return nil, newParseErrorf(&p.current, "function step requires a non-empty name")
	}
	return cfg, nil
}

// parseMutationBody parses an insert() or update() call.
//
// insert() appends a new full-payload row in the time-series; the
// payload must satisfy every @required field on the concept on its
// own. update() does a read-merge-validate-write against the latest
// existing row by id, so the partial payload only has to declare
// the fields the caller wants to change.
//
// Supports the same four syntaxes for both forms:
//   - <op>("concept", id=..., payload={...})       - explicit concept, named arguments
//   - <op>("concept", { id: ..., payload: {...} })  - explicit concept, object literal
//   - <op>(id=..., payload={...})                   - implicit concept (from use declaration), named arguments
//   - <op>({ id: ..., payload: {...} })              - implicit concept (from use declaration), object literal
func (p *Parser) parseMutationBody() (*MutationStmt, error) {
	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected 'insert' or 'update', got %q", p.current.Literal)
	}
	op := strings.ToLower(strings.TrimSpace(p.current.Literal))
	var kind MutationKind
	switch op {
	case "insert":
		kind = MutationKindInsert
	case "update":
		kind = MutationKindUpdate
	default:
		return nil, newParseErrorf(&p.current, "expected 'insert' or 'update', got %q", p.current.Literal)
	}
	p.advance()

	if err := p.expect(TokenParenOpen); err != nil {
		return nil, err
	}

	// Determine whether the concept name is present or implicit.
	// Implicit concept: first token after '(' is '{' (object literal) or
	// an identifier followed by '=' (named argument like id=, payload=).
	// Explicit concept: first token is a string literal or identifier NOT followed by '='.
	var concept string
	implicit := false

	switch {
	case p.check(TokenBraceOpen):
		// Object literal immediately after '(' -- implicit concept
		implicit = true
	case p.check(TokenIdentifier):
		// Lookahead: if next token is '=', this is a named argument (implicit concept).
		// If next token is ',' or ')', this is a concept name (explicit).
		if p.pos+1 < len(p.tokens) && p.tokens[p.pos+1].Type == TokenOperator && p.tokens[p.pos+1].Literal == "=" {
			implicit = true
		} else {
			// Symbolic concept reference from a use declaration (e.g., participant, utterance)
			// Stored as-is; the concept resolver will replace it with the canonical ID
			concept = p.current.Literal
			p.advance()
		}
	case p.check(TokenString):
		concept = p.current.Literal
		p.advance()
	default:
		return nil, newParseErrorf(&p.current, "expected concept name, named argument, or '{', got %q", p.current.Literal)
	}

	if implicit {
		// Leave concept empty for implicit binding. The function loader
		// fills it from BoundConcept (derived from the file's use
		// declaration) after concept resolution runs.
	}

	mutation := &MutationStmt{
		Kind:    kind,
		Concept: concept,
	}

	// For implicit concept, we're already positioned at the first argument
	// (either '{' or an identifier like 'id'). Jump straight to argument parsing.
	if implicit {
		// Check if first argument is an object literal { ... }
		if p.check(TokenBraceOpen) {
			start := p.pos
			depth := 0
			for !p.check(TokenEOF) {
				if p.check(TokenBraceOpen) {
					depth++
				} else if p.check(TokenBraceClose) {
					depth--
					if depth == 0 {
						p.advance()
						break
					}
				}
				p.advance()
			}
			mutation.PayloadRaw = p.reconstructTokens(start, p.pos)
		} else {
			// Named arguments syntax: insert(id=..., payload={...})
			for {
				if p.check(TokenIdentifier) {
					argName := p.current.Literal
					p.advance()

					if !p.check(TokenOperator) || p.current.Literal != "=" {
						continue
					}
					p.advance()

					switch argName {
					case "id":
						idTmpl, err := p.parseValue()
						if err != nil {
							return nil, err
						}
						mutation.IDTemplate = idTmpl
					case "createdAt":
						createdAtTmpl, err := p.parseValue()
						if err != nil {
							return nil, err
						}
						mutation.CreatedAtTemplate = createdAtTmpl
					case "payload":
						start := p.pos
						depth := 0
						for !p.check(TokenEOF) {
							if p.check(TokenBraceOpen) {
								depth++
							} else if p.check(TokenBraceClose) {
								if depth == 0 {
									break
								}
								depth--
								if depth == 0 {
									p.advance()
									break
								}
							}
							p.advance()
						}
						mutation.PayloadRaw = p.reconstructTokens(start, p.pos)
					case "parent":
						valTmpl, err := p.parseValue()
						if err != nil {
							return nil, err
						}
						mutation.ParentTemplate = valTmpl
					case "aliasOf":
						valTmpl, err := p.parseValue()
						if err != nil {
							return nil, err
						}
						mutation.AliasOfTemplate = valTmpl
					}
				}

				if p.check(TokenComma) {
					p.advance()
				} else {
					break
				}
			}
		}
	} else if p.check(TokenComma) {
		// Explicit concept with additional arguments after the comma
		p.advance()

		// Check if second argument is an object literal { ... }
		if p.check(TokenBraceOpen) {
			// Object literal syntax: insert("concept", { id: ..., payload: {...} })
			start := p.pos
			depth := 0
			for !p.check(TokenEOF) {
				if p.check(TokenBraceOpen) {
					depth++
				} else if p.check(TokenBraceClose) {
					depth--
					if depth == 0 {
						p.advance()
						break
					}
				}
				p.advance()
			}
			// Store the entire object as PayloadRaw for processing later
			mutation.PayloadRaw = p.reconstructTokens(start, p.pos)
		} else {
			// Named arguments syntax: insert("concept", id=..., payload={...})
			// Backtrack: we already consumed comma, now process first argument
			for {
				if p.check(TokenIdentifier) {
					argName := p.current.Literal
					p.advance()

					if !p.check(TokenOperator) || p.current.Literal != "=" {
						continue
					}
					p.advance()

					switch argName {
					case "id":
						idTmpl, err := p.parseValue()
						if err != nil {
							return nil, err
						}
						mutation.IDTemplate = idTmpl
					case "createdAt":
						createdAtTmpl, err := p.parseValue()
						if err != nil {
							return nil, err
						}
						mutation.CreatedAtTemplate = createdAtTmpl
					case "payload":
						// Collect payload as raw string
						start := p.pos
						depth := 0
						for !p.check(TokenEOF) {
							if p.check(TokenBraceOpen) {
								depth++
							} else if p.check(TokenBraceClose) {
								if depth == 0 {
									break
								}
								depth--
								if depth == 0 {
									p.advance()
									break
								}
							}
							p.advance()
						}
						mutation.PayloadRaw = p.reconstructTokens(start, p.pos)
					case "parent":
						valTmpl, err := p.parseValue()
						if err != nil {
							return nil, err
						}
						mutation.ParentTemplate = valTmpl
					case "aliasOf":
						valTmpl, err := p.parseValue()
						if err != nil {
							return nil, err
						}
						mutation.AliasOfTemplate = valTmpl
					}
				}

				// Check for more arguments
				if p.check(TokenComma) {
					p.advance()
				} else {
					break
				}
			}
		}
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return mutation, nil
}

// ----------------------------------------------------------------------------
// Step Config Parsers
// ----------------------------------------------------------------------------

// parseForEachStepConfig parses: forEach source [where filter] as varName { ... }
func (p *Parser) parseForEachStepConfig() (*ForEachStepConfig, error) {
	config := &ForEachStepConfig{
		Concurrency: 1,
	}

	// Parse: source [where filter] as varName
	// or: { source: ..., as: ..., do: [...] }

	// Check if using object syntax
	if p.check(TokenIdentifier) && strings.ToLower(p.current.Literal) == "source" {
		// Object syntax
		for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
			if !p.check(TokenIdentifier) {
				break
			}
			key := strings.ToLower(p.current.Literal)
			p.advance()

			if !p.check(TokenColon) {
				continue
			}
			p.advance()

			switch key {
			case "source":
				val, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				if s, ok := val.(string); ok {
					config.Source = s
				}
			case "filter", "where":
				val, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				if s, ok := val.(string); ok {
					config.Filter = s
				}
			case "as":
				val, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				if s, ok := val.(string); ok {
					config.As = s
				}
			case "concurrency":
				val, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				if n, ok := numericAsInt(val); ok {
					config.Concurrency = n
				}
			case "do":
				// Parse array of steps
				if p.check(TokenBracketOpen) {
					p.advance()
					for !p.check(TokenBracketClose) && !p.check(TokenEOF) {
						step, err := p.parseStep()
						if err != nil {
							return nil, err
						}
						if step != nil {
							config.Do = append(config.Do, *step)
						}
						if p.check(TokenComma) {
							p.advance()
						}
					}
					p.expect(TokenBracketClose)
				}
			}

			if p.check(TokenComma) {
				p.advance()
			}
		}
	} else {
		// Inline syntax: forEach step("items") as item { ... }
		// Collect source expression until 'as' or 'where'
		var sourceParts []string
		for !p.check(TokenEOF) {
			if p.check(TokenKeywordAs) || p.check(TokenKeywordWhere) || p.check(TokenBraceOpen) {
				break
			}
			sourceParts = append(sourceParts, p.current.Literal)
			p.advance()
		}
		config.Source = strings.Join(sourceParts, " ")

		// Check for 'where'
		if p.check(TokenKeywordWhere) {
			p.advance()
			var filterParts []string
			for !p.check(TokenEOF) && !p.check(TokenKeywordAs) && !p.check(TokenBraceOpen) {
				filterParts = append(filterParts, p.current.Literal)
				p.advance()
			}
			config.Filter = strings.Join(filterParts, " ")
		}

		// Expect 'as'
		if p.check(TokenKeywordAs) {
			p.advance()
			if p.check(TokenIdentifier) {
				config.As = p.current.Literal
				p.advance()
			}
		}
	}

	// Enforce strict .memql semantics: forEach item variable must be named "item".
	// If omitted, default to "item". Any other name is invalid.
	if strings.TrimSpace(config.As) == "" {
		config.As = "item"
	} else if config.As != "item" {
		return nil, newParseErrorf(&p.current, "invalid forEach as=%q (must be %q)", config.As, "item")
	}

	return config, nil
}

// parseParallelStepConfig parses: parallel { branches: [...], wait: "all", failFast: true }
//
// Keys may appear in any order. `branches` is required and non-empty; each
// entry is either a Go-style step assignment (`name := call`, what the
// struct-form rewriter emits -- including `name := if cond { call }` gating
// and nested `name := parallel { ... }` blocks) or a legacy colon-form step
// (`name: type { ... }`). `wait` must be "all", "any", or "none"; branch ids
// must be unique (the executor surfaces them as `<parent>.<branch>`).
// Unknown keys are rejected. (memql#1368)
func (p *Parser) parseParallelStepConfig() (*ParallelStepConfig, error) {
	config := &ParallelStepConfig{
		Wait: "all",
	}

	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		if !p.check(TokenIdentifier) {
			break
		}
		key := strings.ToLower(p.current.Literal)
		keyTok := p.current
		p.advance()

		if !p.check(TokenColon) {
			continue
		}
		p.advance()

		switch key {
		case "branches":
			if !p.check(TokenBracketOpen) {
				return nil, newParseErrorf(&p.current, "parallel step: expected '[' after branches:, got %q", p.current.Literal)
			}
			p.advance()
			for !p.check(TokenBracketClose) && !p.check(TokenEOF) {
				if !p.check(TokenIdentifier) {
					return nil, newParseErrorf(&p.current, "parallel step: expected a branch step, got %q", p.current.Literal)
				}
				var step *StepDef
				var err error
				if p.peekAhead(1).Type == TokenDefine {
					step, err = p.parseGoStyleStep()
				} else {
					step, err = p.parseStep()
				}
				if err != nil {
					return nil, err
				}
				if step != nil {
					config.Branches = append(config.Branches, *step)
				}
				if p.check(TokenComma) {
					p.advance()
				}
			}
			if err := p.expect(TokenBracketClose); err != nil {
				return nil, err
			}
		case "wait":
			val, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			s, ok := val.(string)
			if !ok {
				return nil, newParseErrorf(&p.current, "parallel step: wait must be a string (\"all\", \"any\", or \"none\")")
			}
			switch s {
			case "all", "any", "none":
			default:
				return nil, newParseErrorf(&p.current, "parallel step: invalid wait value %q (must be \"all\", \"any\", or \"none\")", s)
			}
			config.Wait = s
		case "failfast":
			val, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			b, ok := val.(bool)
			if !ok {
				return nil, newParseErrorf(&p.current, "parallel step: failFast must be true or false")
			}
			config.FailFast = b
		default:
			return nil, newParseErrorf(&keyTok, "parallel step: unknown key %q (expected branches, wait, failFast)", keyTok.Literal)
		}

		if p.check(TokenComma) {
			p.advance()
		}
	}

	if len(config.Branches) == 0 {
		return nil, newParseErrorf(&p.current, "parallel step requires a non-empty branches list")
	}
	seen := make(map[string]struct{}, len(config.Branches))
	for _, b := range config.Branches {
		if _, dup := seen[b.ID]; dup {
			return nil, newParseErrorf(&p.current, "parallel step: duplicate branch id %q", b.ID)
		}
		seen[b.ID] = struct{}{}
	}

	return config, nil
}

// parseSwitchStepConfig parses: switch expr { case "x": { ... }, default: { ... } }
func (p *Parser) parseSwitchStepConfig() (*SwitchStepConfig, error) {
	config := &SwitchStepConfig{
		Cases: make(map[string]*SwitchCase),
	}

	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		if !p.check(TokenIdentifier) {
			break
		}
		key := strings.ToLower(p.current.Literal)
		p.advance()

		if !p.check(TokenColon) {
			continue
		}
		p.advance()

		switch key {
		case "expression", "expr":
			val, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			if s, ok := val.(string); ok {
				config.Expression = s
			}
		case "cases":
			if p.check(TokenBraceOpen) {
				p.advance()
				for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
					// Parse case key
					var caseKey string
					if p.check(TokenString) {
						caseKey = p.current.Literal
						p.advance()
					} else if p.check(TokenIdentifier) {
						caseKey = p.current.Literal
						p.advance()
					} else {
						break
					}

					if !p.check(TokenColon) {
						continue
					}
					p.advance()

					// Parse case body
					switchCase := &SwitchCase{}
					if p.check(TokenBraceOpen) {
						p.advance()
						for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
							step, err := p.parseStep()
							if err != nil {
								return nil, err
							}
							if step != nil {
								switchCase.Steps = append(switchCase.Steps, *step)
							}
						}
						p.expect(TokenBraceClose)
					}
					config.Cases[caseKey] = switchCase

					if p.check(TokenComma) {
						p.advance()
					}
				}
				p.expect(TokenBraceClose)
			}
		case "default":
			switchCase := &SwitchCase{}
			if p.check(TokenBraceOpen) {
				p.advance()
				for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
					step, err := p.parseStep()
					if err != nil {
						return nil, err
					}
					if step != nil {
						switchCase.Steps = append(switchCase.Steps, *step)
					}
				}
				p.expect(TokenBraceClose)
			}
			config.Default = switchCase
		}

		if p.check(TokenComma) {
			p.advance()
		}
	}

	return config, nil
}

// parseConditionExpression parses a condition until '{' is reached.
// Returns the condition as a properly formatted string with:
// - String literals wrapped in quotes
// - No unnecessary whitespace around dots and parentheses
func (p *Parser) parseConditionExpression() (string, error) {
	var result strings.Builder
	depth := 0
	lastWasOperator := true // Start true to avoid leading space

	for !p.check(TokenEOF) {
		if p.check(TokenBraceOpen) && depth == 0 {
			break
		}

		tok := p.current
		literal := tok.Literal

		// Handle token type-specific behavior
		switch tok.Type {
		case TokenParenOpen:
			depth++
		case TokenParenClose:
			depth--
		case TokenString:
			// Wrap string literals in quotes
			literal = `"` + literal + `"`
		}

		// Determine if we need a space before this token
		needsSpace := !lastWasOperator
		if tok.Type == TokenParenOpen || tok.Type == TokenParenClose {
			needsSpace = false // No space around parentheses
		}
		if tok.Literal == "." {
			needsSpace = false // No space before dot
		}

		// Check if this is a token that shouldn't have space after previous
		if result.Len() > 0 {
			lastChar := result.String()[result.Len()-1]
			if lastChar == '(' || lastChar == '.' {
				needsSpace = false
			}
		}

		if needsSpace && result.Len() > 0 {
			result.WriteString(" ")
		}
		result.WriteString(literal)

		// Track if this token is an "operator-like" token (no space after)
		lastWasOperator = tok.Literal == "." || tok.Type == TokenParenOpen

		p.advance()
	}

	return result.String(), nil
}

// parseExpression parses a MemQL expression.
func (p *Parser) parseExpression() (ExpressionNode, error) {
	return p.parseTernary()
}

// parseTernary parses ternary expressions: cond ? then : else
func (p *Parser) parseTernary() (ExpressionNode, error) {
	cond, err := p.parseLogicalOr()
	if err != nil {
		return nil, err
	}
	if cond == nil {
		return nil, nil
	}

	// Check for ? (ternary operator)
	if !p.check(TokenQuestion) {
		return cond, nil
	}
	p.advance() // consume '?'

	// Parse "then" expression
	thenExpr, err := p.parseTernary()
	if err != nil {
		return nil, err
	}

	// Expect ':'
	if !p.check(TokenColon) {
		return nil, newParseErrorf(&p.current, "expected ':' in ternary expression, got %q", p.current.Literal)
	}
	p.advance() // consume ':'

	// Parse "else" expression
	elseExpr, err := p.parseTernary()
	if err != nil {
		return nil, err
	}

	return &TernaryExpr{
		Condition: cond,
		Then:      thenExpr,
		Else:      elseExpr,
	}, nil
}

// parseLogicalOr parses OR expressions. The Go-style `||` operator is the
// canonical form; the legacy `,`-as-OR separator is still accepted (its
// tree-wide retirement is #977). Both sit at the OR precedence level --
// looser than `&&` (parseLogicalAnd) -- so `a && b || c` parses as
// `(a && b) || c`, matching Go.
func (p *Parser) parseLogicalOr() (ExpressionNode, error) {
	left, err := p.parseNullCoalesce()
	if err != nil {
		return nil, err
	}
	if left == nil {
		return nil, nil
	}

	for p.check(TokenComma) || p.check(TokenPipePipe) {
		p.advance()
		right, err := p.parseNullCoalesce()
		if err != nil {
			return nil, err
		}
		if right == nil {
			break
		}
		left = &LogicalExpr{
			Op:    LogicalOr,
			Left:  left,
			Right: right,
		}
	}

	return left, nil
}

// parseNullCoalesce used to collect ??-separated expressions into a
// CoalesceExpr. `??` was retired in Phase 4 -- authors use the
// explicit `coalesce(a, b, c)` form instead. This method remains only
// to preserve the precedence chain; it returns the result of the next
// level unchanged and emits a clear error if it sees `??` anywhere.
func (p *Parser) parseNullCoalesce() (ExpressionNode, error) {
	left, err := p.parseLogicalAnd()
	if err != nil {
		return nil, err
	}
	if left == nil {
		return nil, nil
	}

	if p.check(TokenQuestionQuestion) {
		return nil, newParseErrorf(&p.current,
			"the '??' null-coalescing operator was retired; use coalesce(a, b, ...) instead")
	}
	return left, nil
}

// parseLogicalAnd parses semicolon-separated or &&-separated (AND) expressions.
func (p *Parser) parseLogicalAnd() (ExpressionNode, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	if left == nil {
		return nil, nil
	}

	for p.check(TokenSemicolon) || p.check(TokenAmpAmp) {
		p.advance()
		right, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		if right == nil {
			break
		}
		left = &LogicalExpr{
			Op:    LogicalAnd,
			Left:  left,
			Right: right,
		}
	}

	return left, nil
}

// parsePrimary parses primary expressions.
func (p *Parser) parsePrimary() (ExpressionNode, error) {
	switch {
	case p.check(TokenParenOpen):
		return p.parseGrouped()
	case p.check(TokenBang):
		p.advance() // consume '!'
		operand, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		return &NotExpr{Target: operand}, nil
	case p.check(TokenQuestionDot):
		return p.parseConditionalFilter()
	case p.check(TokenKeywordWhen):
		return p.parseWhenGuard()
	case p.check(TokenKeywordNil):
		p.advance()
		return &NilExpr{}, nil
	// Note: `if` as an expression-level builtin was renamed to `cond(...)`.
	// The `if` keyword is still used for control-flow (`if cond { step }`
	// at the automation statement level); it no longer parses as a
	// value-returning function here.
	case p.check(TokenIdentifier):
		return p.parseIdentifierExpression()
	case p.check(TokenString):
		val := p.current.Literal
		p.advance()
		return &LiteralExpr{Value: val}, nil
	case p.check(TokenNumber):
		// Bare integers store as int64; decimals / scientific store as
		// float64. Matches the memql parser (parseNumberLiteral) and
		// parseAttribute's bare-numeric path so reflect.DeepEqual
		// equivalence holds across the two parser paths for literal
		// types in runtime expressions (issue #255 / #265). Downstream
		// normalisation in executor_filter.normalizeScalarValue is
		// type-agnostic across the int/float family, so this choice
		// is invisible to filter evaluation.
		val, err := baseparser.ParseNumericLiteral(p.current.Literal)
		if err != nil {
			return nil, newParseErrorf(&p.current, "invalid number %q", p.current.Literal)
		}
		p.advance()
		return &LiteralExpr{Value: val}, nil
	case p.check(TokenBracketOpen):
		arr, err := p.parseArray()
		if err != nil {
			return nil, err
		}
		return &LiteralExpr{Value: arr}, nil
	case p.check(TokenBraceOpen):
		obj, err := p.parseObject()
		if err != nil {
			return nil, err
		}
		return &LiteralExpr{Value: obj}, nil
	case p.check(TokenBraceClose), p.check(TokenEOF):
		// End of expression context
		return nil, nil
	default:
		return nil, newParseErrorf(&p.current, "unexpected token %q", p.current.Literal)
	}
}

// parseGrouped parses parenthesized expressions.
func (p *Parser) parseGrouped() (ExpressionNode, error) {
	p.advance() // consume '('
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}
	return expr, nil
}

// parseConditionalFilter parses ?. conditional filters.
// The ArgPath is extracted from the comparison's value if it's an ArgRefExpr.
func (p *Parser) parseConditionalFilter() (ExpressionNode, error) {
	p.advance() // consume '?.'

	// Parse the comparison that follows
	comp, err := p.parseComparison()
	if err != nil {
		return nil, err
	}

	// Extract ArgPath from the comparison value if it's an ArgRefExpr
	var argPath string
	if compExpr, ok := comp.(*ComparisonExpr); ok {
		if argRef, ok := compExpr.Value.(*ArgRefExpr); ok {
			argPath = argRef.Path
		}
	}

	return &ConditionalFilterExpr{
		ArgPath: argPath,
		Filter:  comp,
	}, nil
}

// membershipCollectionField recognises the RHS of `<scalar> in <collection>`
// when the collection is a `payload.<field>` row collection. It consumes the
// token and returns the dotted field path. A list-literal or any other RHS is
// left for parseValue (the OpIn scalar-in-list form). #976.
func membershipCollectionField(p *Parser) (string, bool) {
	if p.check(TokenIdentifier) && strings.HasPrefix(p.current.Literal, "payload.") {
		collection := p.current.Literal
		p.advance()
		return collection, true
	}
	return "", false
}

// scalarMembershipValue converts the bare LHS scalar of `<scalar> in
// <collection>` into the comparison-value representation the membership (has)
// codegen expects -- mirroring parseValue's accessor classification so the
// desugared `in` produces an AST identical to the equivalent `has`.
func scalarMembershipValue(name string) any {
	if rest, ok := strings.CutPrefix(name, "args."); ok {
		return &ArgRefExpr{Path: rest}
	}
	if rest, ok := strings.CutPrefix(name, "ctx."); ok {
		return &ArgRefExpr{Path: rest}
	}
	if strings.HasPrefix(name, "actor.") {
		return &ArgRefExpr{Path: name}
	}
	return name
}

// parseWhenGuard parses the `when(args.<field>) { <expr> }` arg-conditional
// guard (#975). Semantics: a syntactic drop -- if the guard arg is absent at
// query time the guarded block AND its connective are removed as if never
// written. It reuses the `?.` machinery: a ConditionalFilterExpr carries the
// guard arg path + the guarded expression, and the engine's arg-expansion pass
// removes the node (and collapses the surrounding `&&` / `||`) when the arg is
// missing. Unlike `?.`, the block can hold any boolean expression, which makes
// the drop unambiguous inside `||`.
func (p *Parser) parseWhenGuard() (ExpressionNode, error) {
	p.advance() // consume 'when'
	if err := p.expect(TokenParenOpen); err != nil {
		return nil, err
	}
	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "when() requires an `args.<field>` guard, got %q", p.current.Literal)
	}
	guard := p.current.Literal
	p.advance()
	argPath, ok := strings.CutPrefix(guard, "args.")
	if !ok || argPath == "" {
		return nil, newParseErrorf(&p.current, "when() guard must be an `args.<field>` reference, got %q", guard)
	}
	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}
	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}
	inner, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if inner == nil {
		return nil, newParseErrorf(&p.current, "when(%s) { ... } block must contain an expression", guard)
	}
	if err := p.expect(TokenBraceClose); err != nil {
		return nil, err
	}
	return &ConditionalFilterExpr{ArgPath: argPath, Filter: inner}, nil
}

// isNullComparisonLiteral reports whether a parsed comparison RHS is the
// null/nil literal in any of its surface forms: the `nil` keyword
// (*NilExpr), the `null` identifier (which parseValue lowers to a plain
// Go nil, NOT the string "null"), or a bare "null" string. All three
// mean "compare against absence", so `field == <null>` / `field != <null>`
// must lower to OpMissing / OpNotMissing (SQL IS NULL / IS NOT NULL)
// rather than binding nil as a scalar literal -- the latter reaches the
// executor's literal normalizer and fails the whole query with
// "unsupported literal type <nil>" (e.g. queryDueRefreshDomains's
// `payload.refreshCadenceDays != null`). #1631.
func isNullComparisonLiteral(value any) bool {
	if value == nil {
		return true
	}
	if _, isNil := value.(*NilExpr); isNil {
		return true
	}
	if strVal, ok := value.(string); ok && strVal == "null" {
		return true
	}
	return false
}

// parseComparison parses field comparison expressions.
func (p *Parser) parseComparison() (ExpressionNode, error) {
	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected field name, got %q", p.current.Literal)
	}

	field := p.current.Literal
	p.advance()

	// Handle keyword-based operators: in, has, not in
	if p.check(TokenKeywordIn) {
		p.advance()
		// `<scalar> in payload.<collectionField>` (e.g.
		// `args.groupId in payload.groupIds`) is the canonical membership
		// form (#976). It desugars to `payload.<collectionField> has <scalar>`
		// so it reuses the existing array-contains codegen -- `in` becomes
		// the single membership operator, `has` (its reverse) is retired.
		// A list-literal RHS (`payload.kind in ["a", "b"]`) keeps the OpIn
		// scalar-in-list form.
		if collection, ok := membershipCollectionField(p); ok {
			return &ComparisonExpr{
				Field:    FieldReference{Raw: collection, Parts: strings.Split(collection, ".")},
				Operator: OpHas,
				Value:    scalarMembershipValue(field),
			}, nil
		}
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return &ComparisonExpr{
			Field:    FieldReference{Raw: field, Parts: strings.Split(field, ".")},
			Operator: OpIn,
			Value:    value,
		}, nil
	}
	if p.check(TokenKeywordHas) {
		p.advance()
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return &ComparisonExpr{
			Field:    FieldReference{Raw: field, Parts: strings.Split(field, ".")},
			Operator: OpHas,
			Value:    value,
		}, nil
	}
	if p.check(TokenKeywordNot) {
		p.advance()
		if !p.check(TokenKeywordIn) {
			return nil, newParseErrorf(&p.current, "expected 'in' after 'not'")
		}
		p.advance()
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return &ComparisonExpr{
			Field:    FieldReference{Raw: field, Parts: strings.Split(field, ".")},
			Operator: OpOut,
			Value:    value,
		}, nil
	}

	// Symbol-based operators
	if !p.check(TokenOperator) {
		return nil, newParseErrorf(&p.current, "expected operator after field %q, got %q", field, p.current.Literal)
	}

	op := ComparisonOperator(p.current.Literal)
	p.advance()

	var value any
	var err error
	value, err = p.parseValue()
	if err != nil {
		return nil, err
	}

	// Handle == nil/null and != nil/null → OpMissing/OpNotMissing
	if op == OpEq || op == OpNe {
		if isNullComparisonLiteral(value) {
			if op == OpEq {
				op = OpMissing
			} else {
				op = OpNotMissing
			}
			value = nil
		}
	}

	return &ComparisonExpr{
		Field: FieldReference{
			Raw:   field,
			Parts: strings.Split(field, "."),
		},
		Operator: op,
		Value:    value,
	}, nil
}

// parseIdentifierExpression parses function calls, field references, or comparisons.
func (p *Parser) parseIdentifierExpression() (ExpressionNode, error) {
	name := p.current.Literal
	p.advance()

	// Check for function call
	if p.check(TokenParenOpen) {
		call, err := p.parseFunctionCall(name)
		if err != nil {
			return nil, err
		}
		// Phase 6: accept post-call dotted access like `.First().payload.id`.
		// The shared lexer may emit the leading `.` as part of the
		// following identifier (TokenIdentifier starting with ".") or
		// as a standalone TokenOperator ".". Wrap each segment in a
		// DotAccessExpr so the runtime can navigate the result.
		return p.consumePostCallDotAccess(call)
	}

	// `args.<path>` is the canonical caller-argument reference. Convert
	// to an ArgRefExpr so every context (top-level expression,
	// function-call argument, comparison value, ...) sees the same
	// AST node. Without this, `concat("user-", args.id)` and
	// `?.field==args.id` produced different shapes for the same
	// reference -- which broke spec / executor logic that switched
	// on the value type.
	//
	// `ctx.<path>` is the post-rewrite form of `args.<path>` (the
	// struct-form rewriter translates one to the other in filter /
	// insert / update bodies). Both must land at the same AST node
	// so a `ctx.X` reference outside an immediate value position
	// (e.g. as a function-call argument) still resolves to the
	// caller arg instead of leaking through as a bare identifier.
	if strings.HasPrefix(name, "args.") {
		argPath := strings.TrimPrefix(name, "args.")
		if argPath != "" && !p.check(TokenOperator) && !p.check(TokenKeywordIn) && !p.check(TokenKeywordHas) && !p.check(TokenKeywordNot) {
			return &ArgRefExpr{Path: argPath}, nil
		}
	}
	if strings.HasPrefix(name, "ctx.") {
		argPath := strings.TrimPrefix(name, "ctx.")
		if argPath != "" && !p.check(TokenOperator) && !p.check(TokenKeywordIn) && !p.check(TokenKeywordHas) && !p.check(TokenKeywordNot) {
			switch argPath {
			case "input", "output", "actor", "partition", "now", "config", "error", "trace":
				// Reserved envelope fields -- leave for downstream
				// resolution paths instead of treating as caller args.
			default:
				return &ArgRefExpr{Path: argPath}, nil
			}
		}
	}

	// Check for comparison operator (symbol-based)
	if p.check(TokenOperator) {
		op := ComparisonOperator(p.current.Literal)
		p.advance()

		var value any
		var err error
		value, err = p.parseValue()
		if err != nil {
			return nil, err
		}

		// Handle == nil/null and != nil/null → OpMissing/OpNotMissing
		if op == OpEq || op == OpNe {
			if isNullComparisonLiteral(value) {
				if op == OpEq {
					op = OpMissing
				} else {
					op = OpNotMissing
				}
				value = nil
			}
		}

		return &ComparisonExpr{
			Field: FieldReference{
				Raw:   name,
				Parts: strings.Split(name, "."),
			},
			Operator: op,
			Value:    value,
		}, nil
	}

	// Check for keyword-based operators: in, has, not in
	if p.check(TokenKeywordIn) {
		p.advance()
		// `<scalar> in payload.<collectionField>` desugars to
		// `payload.<collectionField> has <scalar>` (#976) -- see the
		// parseComparison path for the rationale.
		if collection, ok := membershipCollectionField(p); ok {
			return &ComparisonExpr{
				Field:    FieldReference{Raw: collection, Parts: strings.Split(collection, ".")},
				Operator: OpHas,
				Value:    scalarMembershipValue(name),
			}, nil
		}
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return &ComparisonExpr{
			Field:    FieldReference{Raw: name, Parts: strings.Split(name, ".")},
			Operator: OpIn,
			Value:    value,
		}, nil
	}
	if p.check(TokenKeywordHas) {
		p.advance()
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return &ComparisonExpr{
			Field:    FieldReference{Raw: name, Parts: strings.Split(name, ".")},
			Operator: OpHas,
			Value:    value,
		}, nil
	}
	if p.check(TokenKeywordNot) {
		p.advance()
		if !p.check(TokenKeywordIn) {
			return nil, newParseErrorf(&p.current, "expected 'in' after 'not' (did you mean 'not in'?)")
		}
		p.advance()
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return &ComparisonExpr{
			Field:    FieldReference{Raw: name, Parts: strings.Split(name, ".")},
			Operator: OpOut,
			Value:    value,
		}, nil
	}

	// Just an identifier (field reference or spec reference)
	return &SpecReferenceExpr{Name: name}, nil
}

// parseFunctionCall parses a function call: name(args).
func (p *Parser) parseFunctionCall(name string) (ExpressionNode, error) {
	p.advance() // consume '('

	// index() with no args = forEach index accessor; index(arr, i) = array element (general parsing)
	if strings.ToLower(name) == "index" && p.check(TokenParenClose) {
		return p.parseIndexAccessor()
	}

	// Check for accessor functions first
	switch strings.ToLower(name) {
	case "var":
		return p.parseVarAccessor()
	case "step":
		return p.parseStepAccessor()
	case "field":
		return p.parseFieldAccessor()
	case "input":
		return p.parseInputAccessor()
	case "item":
		return p.parseItemAccessor()
	case "event":
		return p.parseEventAccessor()
	case "actor":
		// Auth-context accessor; canonical name (#221). The
		// underlying parser function keeps the historical name
		// (parseActorAccessor) -- the produced AST node is a
		// ActorReference -- both are internal-only and unrelated
		// to the DSL author surface.
		return p.parseActorAccessor()
	case "caller":
		// Retired in #221 in favour of actor. for one vocabulary
		// across the DSL. baseparser.ErrCallerRetired is the single
		// source of the migration-hint string; both parsers emit it
		// identically so a cross-parser equivalence test can assert
		// equality (#244 / epic #218).
		return nil, newParseErrorf(&p.current, "%s", baseparser.ErrCallerRetired.Error())
	case "error":
		return p.parseErrorAccessor()
	case "timestamp", "now":
		return p.parseTimestampAccessor()
	case "memqlversion":
		return p.parseMemqlVersionFunction()
	case "concat":
		return p.parseConcatFunction()
	case "coalesce":
		return p.parseCoalesceFunction()
	case "cond":
		return p.parseCondFunction()
	case "first":
		return p.parseFirstFunction()
	case "last":
		return p.parseLastFunction()
	case "lower":
		return p.parseLowerFunction()
	case "upper":
		return p.parseUpperFunction()
	case "trim":
		return p.parseTrimFunction()
	case "hash":
		return p.parseHashFunction()
	case "shortid":
		// Lower-cased dispatch; the DSL spells it `shortId(...)`.
		return p.parseShortIdFunction()
	case "canonicalid":
		// The dispatch above lower-cases the function name (`strings.ToLower(name)`)
		// so the case label MUST be lowercase too. The DSL still spells the call
		// `canonicalId(...)` in source -- this just normalises the lookup.
		return p.parseCanonicalIdFunction()
	case "tostring":
		return p.parseToStringFunction()
	case "addduration":
		return p.parseAddDurationFunction()
	case "daysbetween":
		return p.parseDaysBetweenFunction()
	case "subtracttimestamps":
		return p.parseSubtractTimestampsFunction()
	case "year":
		return p.parseYearFunction()
	case "quarter":
		return p.parseQuarterFunction()
	case "month":
		return p.parseMonthFunction()
	case "dayofmonth":
		return p.parseDayOfMonthFunction()
	case "isanniversary":
		return p.parseIsAnniversaryFunction()
	case "isfirstdayofquarter":
		return p.parseIsFirstDayOfQuarterFunction()
	case "contains":
		return p.parseContainsFunction()
	case "case":
		return p.parseCaseFunction()
	case "default":
		return p.parseDefaultFunction()
	case "paginate":
		return p.parsePaginateFunction()
	case "sort":
		return p.parseSortFunction()
	case "select":
		return p.parseSelectFunction()
	case "asof":
		return p.parseAsOfFunction()
	case "withdepth":
		return p.parseWithDepthFunction()
	case "count":
		return p.parseCountFunction()
	}

	// Handle shape() specially - two forms: shape(expr, template) or shape({ source, template })
	// When first token is '{', use object form (helper function-call style)
	if strings.ToLower(name) == "shape" && !p.check(TokenBraceOpen) {
		return p.parseShapeFunction()
	}

	// Relationship wrapper functions take a single inner expression and
	// produce a *RelationshipExpr (createWrapper dispatches by name).
	// The dedicated directive parse functions (paginate / sort / select /
	// asOf / withDepth / shape) live in the switch above and produce
	// their specialised AST types directly; they are NOT in this map.
	// Modern single-paren `contains(filter)` is handled inside
	// parseContainsFunction (arg-count discriminates relationship vs
	// 2-arg string search).
	wrapperFunctions := map[string]bool{
		"parentof": true, "childof": true, "aliasof": true,
		"equals": true, "interactswith": true,
		"owns": true, "createdby": true, "ids": true,
	}

	if wrapperFunctions[strings.ToLower(name)] {
		// Parse inner expression
		inner, err := p.parseExpression()
		if err != nil {
			return nil, err
		}

		if err := p.expect(TokenParenClose); err != nil {
			return nil, err
		}

		// Return appropriate wrapper
		return p.createWrapper(name, inner)
	}

	// For regular function calls, parse as arguments
	args := make(map[string]any)
	argList := []any{}

	for !p.check(TokenParenClose) && !p.check(TokenEOF) {
		// Check for named argument (identifier followed by single =)
		if p.check(TokenIdentifier) && p.peekAhead(1).Type == TokenOperator && p.peekAhead(1).Literal == "=" {
			argName := p.current.Literal
			p.advance() // consume name
			p.advance() // consume '='
			val, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			args[argName] = val
		} else {
			// Positional argument
			val, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			argList = append(argList, val)
		}

		if p.check(TokenComma) {
			p.advance()
		} else {
			break
		}
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	// If we have positional args, add them to the map
	for i, arg := range argList {
		args[strconv.Itoa(i)] = arg
	}

	// Check for function body (transforms this into a wrapping expression)
	if p.check(TokenParenOpen) {
		// Nested function call - this is a directive like sort(...)(innerExpr)
		inner, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		return p.wrapDirective(name, args, inner)
	}

	return &FunctionCallExpr{
		Name: name,
		Args: args,
	}, nil
}

// parseShapeFunction parses shape(query, template) with two arguments.
// The first argument is a query expression, the second is a template object.
func (p *Parser) parseShapeFunction() (ExpressionNode, error) {
	// Parse the query expression (first argument)
	// We need to stop at the comma, not at the template's {
	target, err := p.parseShapeQueryArg()
	if err != nil {
		return nil, err
	}

	// Expect comma between query and template
	if !p.check(TokenComma) {
		return nil, newParseErrorf(&p.current, "shape() requires two arguments: query expression and template object, got %q", p.current.Literal)
	}
	p.advance() // consume comma

	// Parse the template (second argument).
	// Accepts either:
	//   - An inline template object: shape(query, {"id": node("id"), ...})
	//   - A named shape reference:   shape(query, "participantFull")
	if p.check(TokenString) {
		// Named shape reference -- resolve at runtime via the shape registry.
		templateName := p.current.Literal
		p.advance() // consume string token

		if err := p.expect(TokenParenClose); err != nil {
			return nil, err
		}

		return &ShapeExpr{
			Target:       target,
			TemplateName: templateName,
		}, nil
	}

	if !p.check(TokenBraceOpen) {
		return nil, newParseErrorf(&p.current, "shape() second argument must be a template object {...} or a named shape \"name\", got %q", p.current.Literal)
	}
	template, err := p.parseObject()
	if err != nil {
		return nil, fmt.Errorf("parse shape template: %w", err)
	}

	// Expect closing paren
	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &ShapeExpr{
		Target:   target,
		Template: template,
	}, nil
}

// parseShapeQueryArg parses the query expression argument of shape().
// This is similar to parseExpression but stops at comma when we're at depth 0.
func (p *Parser) parseShapeQueryArg() (ExpressionNode, error) {
	return p.parseShapeLogicalOr()
}

// parseShapeLogicalOr parses OR expressions for shape query, stopping at comma.
func (p *Parser) parseShapeLogicalOr() (ExpressionNode, error) {
	left, err := p.parseLogicalAnd()
	if err != nil {
		return nil, err
	}
	if left == nil {
		return nil, nil
	}

	// In shape context, comma at depth 0 means end of query argument, so a
	// comma followed by `{` (inline template) or "name" (named shape) ends
	// the expression. The `||` operator is unambiguous OR and never a
	// template separator, so it always continues the expression.
	for p.check(TokenComma) || p.check(TokenPipePipe) {
		if p.check(TokenComma) {
			// Peek ahead to see if this comma separates query from template
			nextType := p.peekAhead(1).Type
			if nextType == TokenBraceOpen || nextType == TokenString {
				// This comma precedes the template (inline or named), stop here
				break
			}
		}
		p.advance() // consume the OR operator (`,` or `||`)
		right, err := p.parseLogicalAnd()
		if err != nil {
			return nil, err
		}
		if right == nil {
			break
		}
		left = &LogicalExpr{
			Op:    LogicalOr,
			Left:  left,
			Right: right,
		}
	}

	return left, nil
}

// parseDirectiveTarget parses the leading expression argument of a
// modern single-paren directive call (paginate / sort / select / asOf
// / withDepth). It accepts `;` and `&&` AND-joined comparisons via
// parseLogicalAnd but STOPS at the bare comma that separates the
// target from the directive's tail args. The OR-via-comma precedence
// level (parseLogicalOr) is intentionally NOT used here -- the memql
// runtime parser likewise treats the directive boundary as a hard
// stop.
func (p *Parser) parseDirectiveTarget() (ExpressionNode, error) {
	return p.parseLogicalAnd()
}

// parsePaginateFunction parses the modern single-paren form
// `paginate(target, LIMIT [, OFFSET])` and produces a *PaginateExpr
// matching what the memql runtime parser (parsePaginate, parser.go
// ~line 1223) emits. Limit must be a positive integer; offset, when
// present, must be a non-negative integer. The opening `(` is already
// consumed by the caller (parseFunctionCall).
func (p *Parser) parsePaginateFunction() (ExpressionNode, error) {
	if p.check(TokenParenClose) {
		return nil, newParseErrorf(&p.current, "paginate() requires an expression argument")
	}
	target, err := p.parseDirectiveTarget()
	if err != nil {
		return nil, err
	}
	if !p.check(TokenComma) {
		return nil, newParseErrorf(&p.current, "expected ',' before paginate limit, got %q", p.current.Literal)
	}
	p.advance()

	limit, err := p.expectPositiveIntegerLiteral("paginate limit")
	if err != nil {
		return nil, err
	}

	var offsetPtr *int
	if p.check(TokenComma) {
		p.advance()
		offset, err := p.expectNonNegativeIntegerLiteral("paginate offset")
		if err != nil {
			return nil, err
		}
		offsetPtr = &offset
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &PaginateExpr{
		Target: target,
		Limit:  &limit,
		Offset: offsetPtr,
	}, nil
}

// parseSortFunction parses the modern single-paren form
// `sort(target, "field" [, "asc"|"desc"] ...)` and produces a
// *SortExpr matching parseSort + parseSortFields in the memql runtime
// parser (parser.go ~line 1086). Default direction is SortDesc (the
// memql parser's behaviour; a bare `sort(target, "createdAt")` sorts
// descending). A trailing "asc"/"desc" literal binds to the
// preceding field; otherwise the next string starts a new field with
// the default direction.
func (p *Parser) parseSortFunction() (ExpressionNode, error) {
	if p.check(TokenParenClose) {
		return nil, newParseErrorf(&p.current, "sort() requires an expression argument")
	}
	target, err := p.parseDirectiveTarget()
	if err != nil {
		return nil, err
	}

	fields := []SortField{}
	for {
		if !p.check(TokenComma) {
			return nil, newParseErrorf(&p.current, "expected ',' before sort field, got %q", p.current.Literal)
		}
		p.advance()

		if !p.check(TokenString) {
			return nil, newParseErrorf(&p.current, "sort field must be a string literal (e.g. \"createdAt\" or \"payload.title\")")
		}
		field := strings.TrimSpace(p.current.Literal)
		if field == "" {
			return nil, newParseErrorf(&p.current, "sort field must not be empty")
		}
		p.advance()

		direction := SortDesc
		if p.check(TokenComma) && p.peekAhead(1).Type == TokenString && isSortDirectionLiteral(p.peekAhead(1).Literal) {
			p.advance() // consume comma
			direction = parseSortDirection(p.current.Literal)
			p.advance() // consume direction literal
		}

		fields = append(fields, SortField{Field: field, Direction: direction})

		if p.check(TokenParenClose) {
			break
		}
	}

	if len(fields) == 0 {
		return nil, newParseErrorf(&p.current, "sort() requires at least one field")
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &SortExpr{Target: target, Fields: fields}, nil
}

// parseSelectFunction parses the modern single-paren form
// `select(target, "field1", "field2", ...)` and produces a
// *SelectExpr matching parseSelect (memql parser.go ~line 1168). At
// least one field is required; field references are string literals
// validated by parseFieldReferenceLiteralLang.
func (p *Parser) parseSelectFunction() (ExpressionNode, error) {
	if p.check(TokenParenClose) {
		return nil, newParseErrorf(&p.current, "select() requires an expression argument")
	}
	target, err := p.parseDirectiveTarget()
	if err != nil {
		return nil, err
	}

	if !p.check(TokenComma) {
		return nil, newParseErrorf(&p.current, "select() requires at least one field")
	}

	fields := []FieldReference{}
	for {
		if !p.check(TokenComma) {
			return nil, newParseErrorf(&p.current, "expected ',' between select fields, got %q", p.current.Literal)
		}
		p.advance()

		if !p.check(TokenString) {
			return nil, newParseErrorf(&p.current, "select field must be a string literal (e.g. \"payload.title\")")
		}
		ref, err := parseFieldReferenceLiteralLang(p.current.Literal)
		if err != nil {
			return nil, newParseErrorf(&p.current, "%s", err.Error())
		}
		p.advance()
		fields = append(fields, ref)

		if p.check(TokenParenClose) {
			break
		}
	}

	if len(fields) == 0 {
		return nil, newParseErrorf(&p.current, "select() requires at least one field")
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &SelectExpr{Target: target, Fields: fields}, nil
}

// parseAsOfFunction parses the modern single-paren form
// `asOf(target, "RFC3339" | latest)` and produces a *TimestampExpr
// matching parseAsOf (memql parser.go ~line 1278). The second arg is
// either a string literal (parsed as RFC3339Nano) or the bare
// identifier `latest`.
func (p *Parser) parseAsOfFunction() (ExpressionNode, error) {
	if p.check(TokenParenClose) {
		return nil, newParseErrorf(&p.current, "asOf() requires an expression argument")
	}
	target, err := p.parseDirectiveTarget()
	if err != nil {
		return nil, err
	}
	if !p.check(TokenComma) {
		return nil, newParseErrorf(&p.current, "expected ',' before asOf timestamp, got %q", p.current.Literal)
	}
	p.advance()

	var (
		timestamp *time.Time
		useLatest bool
	)
	switch {
	case p.check(TokenString):
		value := strings.TrimSpace(p.current.Literal)
		if value == "" {
			return nil, newParseErrorf(&p.current, "asOf timestamp must not be empty")
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, newParseErrorf(&p.current, "invalid RFC3339 timestamp %q", value)
		}
		timestamp = &parsed
		p.advance()
	case p.check(TokenIdentifier):
		if !strings.EqualFold(strings.TrimSpace(p.current.Literal), "latest") {
			return nil, newParseErrorf(&p.current, "asOf second argument must be an RFC3339 string or latest")
		}
		useLatest = true
		p.advance()
	default:
		return nil, newParseErrorf(&p.current, "asOf second argument must be an RFC3339 string or latest")
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &TimestampExpr{
		Target:    target,
		Timestamp: timestamp,
		UseLatest: useLatest,
	}, nil
}

// parseWithDepthFunction parses the modern single-paren form
// `withDepth(target, INT)` and produces a *DepthExpr matching
// parseWithDepth (memql parser.go ~line 1337). Depth must be a
// positive integer literal.
func (p *Parser) parseWithDepthFunction() (ExpressionNode, error) {
	if p.check(TokenParenClose) {
		return nil, newParseErrorf(&p.current, "withDepth() requires an expression argument")
	}
	target, err := p.parseDirectiveTarget()
	if err != nil {
		return nil, err
	}
	if !p.check(TokenComma) {
		return nil, newParseErrorf(&p.current, "expected ',' before withDepth value, got %q", p.current.Literal)
	}
	p.advance()

	depth, err := p.expectPositiveIntegerLiteral("withDepth value")
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &DepthExpr{Target: target, Depth: depth}, nil
}

// parseCountFunction parses the single-paren form `count(target)` and
// produces a *CountExpr. Unlike the other directives it takes no tail
// arguments -- it aggregates the matching set to a numeric count, so
// the only argument is the target filter expression. The opening `(`
// is already consumed by the caller (parseFunctionCall).
func (p *Parser) parseCountFunction() (ExpressionNode, error) {
	if p.check(TokenParenClose) {
		return nil, newParseErrorf(&p.current, "count() requires an expression argument")
	}
	target, err := p.parseDirectiveTarget()
	if err != nil {
		return nil, err
	}
	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}
	return &CountExpr{Target: target}, nil
}

// expectPositiveIntegerLiteral consumes the current TokenNumber and
// asserts it parses as a strictly positive integer. Mirrors the
// memql parser's parsePositiveInt validation surface so error text
// stays close enough for the cross-parser equivalence work to focus
// on AST structure rather than message wording.
func (p *Parser) expectPositiveIntegerLiteral(field string) (int, error) {
	if !p.check(TokenNumber) {
		return 0, newParseErrorf(&p.current, "%s must be an integer literal", field)
	}
	value, err := strconv.Atoi(strings.TrimSpace(p.current.Literal))
	if err != nil {
		return 0, newParseErrorf(&p.current, "%s must be an integer", field)
	}
	if value <= 0 {
		return 0, newParseErrorf(&p.current, "%s must be greater than zero", field)
	}
	p.advance()
	return value, nil
}

// expectNonNegativeIntegerLiteral is the offset-style counterpart to
// expectPositiveIntegerLiteral (zero is allowed).
func (p *Parser) expectNonNegativeIntegerLiteral(field string) (int, error) {
	if !p.check(TokenNumber) {
		return 0, newParseErrorf(&p.current, "%s must be an integer literal", field)
	}
	value, err := strconv.Atoi(strings.TrimSpace(p.current.Literal))
	if err != nil {
		return 0, newParseErrorf(&p.current, "%s must be an integer", field)
	}
	if value < 0 {
		return 0, newParseErrorf(&p.current, "%s must be zero or greater", field)
	}
	p.advance()
	return value, nil
}

// isSortDirectionLiteral returns true if value (case-insensitive)
// names a sort direction (asc / desc). Mirrors the memql parser
// helper of the same name -- the langparser cannot import that one
// directly because of the package boundary.
func isSortDirectionLiteral(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(SortAsc), string(SortDesc):
		return true
	}
	return false
}

// parseSortDirection coerces an "asc" / "desc" literal to the
// langparser SortDirection enum. Unknown inputs default to
// SortDesc to match the memql parser's defaulting behaviour.
func parseSortDirection(value string) SortDirection {
	if strings.EqualFold(strings.TrimSpace(value), string(SortAsc)) {
		return SortAsc
	}
	return SortDesc
}

// parseFieldReferenceLiteralLang mirrors the memql parser's
// parseFieldReferenceLiteral / splitFieldParts so a select() field
// reference produced by either parser carries the same Raw + Parts.
// Lives in this package (with the `Lang` suffix) because the memql
// parser's helper is unexported and can't be imported across the
// package boundary.
func parseFieldReferenceLiteralLang(value string) (FieldReference, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return FieldReference{}, fmt.Errorf("field reference must not be empty")
	}
	parts := strings.Split(trimmed, ".")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	if len(parts) == 0 {
		return FieldReference{}, fmt.Errorf("field reference must not be empty")
	}
	return FieldReference{Raw: trimmed, Parts: parts}, nil
}

// createWrapper creates the appropriate expression wrapper for wrapper functions.
func (p *Parser) createWrapper(name string, target ExpressionNode) (ExpressionNode, error) {
	switch strings.ToLower(name) {
	case "parentof":
		return &RelationshipExpr{Function: RelParentOf, Target: target}, nil
	case "childof":
		return &RelationshipExpr{Function: RelChildOf, Target: target}, nil
	case "aliasof":
		return &RelationshipExpr{Function: RelAliasOf, Target: target}, nil
	case "equals":
		return &RelationshipExpr{Function: RelEquals, Target: target}, nil
	case "interactswith":
		return &RelationshipExpr{Function: RelInteractsWith, Target: target}, nil
	case "contains":
		return &RelationshipExpr{Function: RelContains, Target: target}, nil
	case "owns":
		return &RelationshipExpr{Function: RelOwns, Target: target}, nil
	case "createdby":
		return &RelationshipExpr{Function: RelCreatedBy, Target: target}, nil
	case "ids":
		return &RelationshipExpr{Function: RelIds, Target: target}, nil
	default:
		// Generic function call with target as argument
		return &FunctionCallExpr{
			Name: name,
			Args: map[string]any{"0": target},
		}, nil
	}
}

// wrapDirective creates the appropriate expression wrapper for directive functions.
func (p *Parser) wrapDirective(name string, args map[string]any, target ExpressionNode) (ExpressionNode, error) {
	switch strings.ToLower(name) {
	case "sort":
		fields := []SortField{}
		// Parse sort arguments
		for k, v := range args {
			if k == "0" || k == "1" || k == "2" || k == "3" {
				if s, ok := v.(string); ok {
					dir := SortAsc
					field := s
					if strings.HasPrefix(s, "-") {
						dir = SortDesc
						field = s[1:]
					}
					fields = append(fields, SortField{Field: field, Direction: dir})
				}
			}
		}
		return &SortExpr{Target: target, Fields: fields}, nil

	case "paginate":
		expr := &PaginateExpr{Target: target}
		if limit, ok := numericAsInt(args["limit"]); ok {
			expr.Limit = &limit
		}
		if offset, ok := numericAsInt(args["offset"]); ok {
			expr.Offset = &offset
		}
		return expr, nil

	case "depth":
		expr := &DepthExpr{Target: target}
		if d, ok := numericAsInt(args["0"]); ok {
			expr.Depth = d
		}
		return expr, nil

	default:
		// Generic function call with target
		return &FunctionCallExpr{
			Name: name,
			Args: map[string]any{"target": target, "args": args},
		}, nil
	}
}

// valueToExprNode converts a parseValue result to an ExpressionNode.
func (p *Parser) valueToExprNode(val any) ExpressionNode {
	switch v := val.(type) {
	case ExpressionNode:
		return v
	case *FunctionCallExpr:
		return v
	case *ArgRefExpr:
		return v
	case *NilExpr:
		return v
	case string:
		return &LiteralExpr{Value: v}
	case float64:
		return &LiteralExpr{Value: v}
	case bool:
		return &LiteralExpr{Value: v}
	default:
		return &LiteralExpr{Value: v}
	}
}

// parseValue parses a literal value (string, number, array, object).
func (p *Parser) parseValue() (any, error) {
	startPos := p.pos
	switch {
	case p.check(TokenKeywordNil):
		p.advance()
		return &NilExpr{}, nil
	case p.check(TokenString):
		val := p.current.Literal
		p.advance()
		return val, nil
	case p.check(TokenNumber):
		// Bare integers store as int64; decimals / scientific store as
		// float64. Same int-first dance as parsePrimary above and
		// parseAttribute's bare-numeric path; see issues #255 / #265
		// for the equivalence rationale. The four directive-arg call
		// sites (paginate limit/offset, withDepth,
		// concurrent.concurrency) that read these values use
		// numericAsInt to accept both int64 and float64.
		val, err := baseparser.ParseNumericLiteral(p.current.Literal)
		if err != nil {
			return nil, newParseErrorf(&p.current, "invalid number %q", p.current.Literal)
		}
		p.advance()
		return val, nil
	case p.check(TokenIdentifier), p.check(TokenKeywordIf), p.check(TokenKeywordCase), p.check(TokenKeywordDefault):
		// Could be true, false, null, args.fieldName, function call (if/case/default), or variable reference
		val := p.current.Literal
		p.advance()
		switch val {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			// Check if it's a function call
			if p.check(TokenParenOpen) {
				// Parse as function call and return as value reference
				call, err := p.parseFunctionCall(val)
				if err != nil {
					return nil, err
				}
				// After a call expression we accept a dotted suffix in two
				// shapes, because the shared lexer may or may not absorb
				// the leading `.` into the following identifier:
				//   1. TokenOperator "." + TokenIdentifier ("payload")
				//   2. TokenIdentifier starting with "." (".payload.name")
				//      -- the common case when scanIdentifier sees `.`
				//      immediately after `)` and runs the
				//      isIdentifierCharNoColon loop from there.
				// Either form round-trips through reconstructTokens.
				if (p.check(TokenOperator) && p.current.Literal == ".") ||
					(p.check(TokenIdentifier) && strings.HasPrefix(p.current.Literal, ".")) {
					for {
						if p.check(TokenOperator) && p.current.Literal == "." {
							p.advance()
							if !p.check(TokenIdentifier) {
								return nil, newParseErrorf(&p.current, "expected identifier after '.', got %q", p.current.Literal)
							}
							p.advance()
							continue
						}
						if p.check(TokenIdentifier) && strings.HasPrefix(p.current.Literal, ".") {
							p.advance()
							continue
						}
						break
					}
					return p.reconstructTokens(startPos, p.pos), nil
				}
				return call, nil
			}

			// Dotted identifier path (event.payload.id) parsed as a
			// single identifier when '.' is in the ident chars. Both
			// `args.X` (canonical) and `ctx.X` (shorthand kept for
			// backward compatibility per F.3 of the ctx-envelope purge)
			// lower to ArgRefExpr -- otherwise `ctx.X` in value
			// position would fall through as the literal string and
			// compile to a no-op SQL predicate that never matches.
			//
			// memql#302 retired the legacy envelope longhand
			// (`ctx.input.X` / `ctx.output = ...`). The `ctx.actor` /
			// `ctx.now` / `ctx.partition` / `ctx.config` reserved
			// fields likewise never appear in comparison-value position
			// (the canonical struct form is bare `actor.X` / `now` /
			// etc.); the guard below stays minimal -- only `ctx.input`
			// is explicitly rejected, since stripping the longhand
			// without a migration message would silently produce a
			// no-match predicate.
			if strings.HasPrefix(val, "args.") {
				argPath := strings.TrimPrefix(val, "args.")
				return &ArgRefExpr{Path: argPath}, nil
			}
			if strings.HasPrefix(val, "ctx.") {
				argPath := strings.TrimPrefix(val, "ctx.")
				if argPath == "input" || strings.HasPrefix(argPath, "input.") {
					return nil, newParseErrorf(&p.current,
						"`ctx.input.X` is retired -- use `args.X` for caller-passed arguments")
				}
				return &ArgRefExpr{Path: argPath}, nil
			}
			// actor.X is the auth-context accessor (the actor's
			// AccessContext), NOT a caller-passed arg. Emit an
			// ArgRefExpr carrying the prefix; the AST converter routes
			// it to the engine ActorReference so it resolves from the
			// AccessContext at filter time (resolveActorReferences).
			// Without this a bare `actor.userId` in a comparison falls
			// through as the literal string and the predicate
			// (= 'actor.userId') never matches a real row. See
			// memql#216. caller.X retired by #221; baseparser.ClassifyAccessor
			// is the single source for both the actor/args/ctx dispatch
			// AND the caller-rejection migration hint that BOTH parsers
			// emit identically (#244 / epic #218).
			kind, _, accErr := baseparser.ClassifyAccessor(val)
			if accErr != nil {
				return nil, accErr
			}
			if kind == baseparser.KindActor {
				return &ArgRefExpr{Path: val}, nil
			}
			return val, nil
		}
	case p.check(TokenOperator) && p.current.Literal == "$":
		// Handle $variable references in query blocks
		p.advance() // consume $
		if !p.check(TokenIdentifier) {
			return nil, newParseErrorf(&p.current, "expected identifier after $")
		}
		name := p.current.Literal
		p.advance()
		return "$" + name, nil
	case p.check(TokenBracketOpen):
		return p.parseArray()
	case p.check(TokenBraceOpen):
		return p.parseObject()
	default:
		return nil, newParseErrorf(&p.current, "unexpected token %q, expected value", p.current.Literal)
	}
}

// parseArray parses a JSON-like array.
func (p *Parser) parseArray() ([]any, error) {
	p.advance() // consume '['
	arr := []any{}

	for !p.check(TokenBracketClose) && !p.check(TokenEOF) {
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		arr = append(arr, val)

		if p.check(TokenComma) {
			p.advance()
		} else {
			break
		}
	}

	if err := p.expect(TokenBracketClose); err != nil {
		return nil, err
	}

	return arr, nil
}

// parseObject parses a JSON-like object.
func (p *Parser) parseObject() (map[string]any, error) {
	p.advance() // consume '{'
	obj := make(map[string]any)

	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		// Key
		var key string
		if p.check(TokenString) {
			key = p.current.Literal
			p.advance()
		} else if p.check(TokenIdentifier) {
			ident := p.current.Literal
			p.advance()

			// Shorthand A: a bare dotted path like `event.payload.spaceId`
			// or `registerNode.result.node.id` with no `key:` prefix
			// infers the key from the path's terminal segment. The
			// lexer delivers the full dotted path as a single
			// TokenIdentifier, so we detect the shorthand by: (a) the
			// identifier contains a dot and (b) the next token is NOT
			// `:` or `=`. Anything else (single identifier like
			// `allAgents`, keys that happen to contain dots but are
			// followed by `:`) falls through to the normal parser.
			if strings.Contains(ident, ".") && !p.check(TokenColon) && !(p.check(TokenOperator) && p.current.Literal == "=") {
				segments := strings.Split(ident, ".")
				terminal := segments[len(segments)-1]
				if len(segments) >= 2 && isSimpleIdentifierSegment(terminal) {
					obj[terminal] = ident
					if p.check(TokenComma) {
						p.advance()
					} else {
						break
					}
					continue
				}
			}

			// Shorthand B: a `node("ident")` or `node("path.ident")` call
			// inside an inline shape template, with no `key:` prefix.
			// Trigger when an identifier is followed by `(` rather than
			// `:` / `=`. We parse the call as a value, then infer the
			// key from the terminal segment of its single quoted-string
			// argument. Only the accessor functions `node`, `arg`, and
			// `var` are eligible; other calls stay as errors so the
			// caller has to spell out the key. A call whose argument
			// isn't a single quoted identifier path (e.g. `node(a, b)`
			// or `concat(...)`) also falls back to an error.
			if p.check(TokenParenOpen) && canonicalKeyAccessor(ident) {
				callStart := p.pos - 1 // identifier token position
				val, err := p.parseFunctionCall(ident)
				if err == nil {
					if inferred, ok := inferAccessorKey(val); ok && !p.check(TokenColon) && !(p.check(TokenOperator) && p.current.Literal == "=") {
						obj[inferred] = val
						if p.check(TokenComma) {
							p.advance()
						} else {
							break
						}
						continue
					}
					// Not eligible -- rewind to just after the identifier
					// so downstream code can report a clean error.
					p.pos = callStart + 1
					p.current = p.tokens[p.pos]
				}
			}
			key = ident
		} else {
			return nil, newParseErrorf(&p.current, "expected object key, got %q", p.current.Literal)
		}

		// Colon or = (key-value separator)
		if p.check(TokenColon) {
			p.advance()
		} else if p.check(TokenOperator) && p.current.Literal == "=" {
			p.advance()
		} else {
			return nil, newParseErrorf(&p.current, "expected ':' or '=' after object key, got %q", p.current.Literal)
		}

		// Value -- support ?? coalescing inside object literals
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		// Handle ?? (null coalescing) after a value
		if p.check(TokenQuestionQuestion) {
			// Build a CoalesceExpr wrapping the left value and right default
			args := []ExpressionNode{p.valueToExprNode(val)}
			for p.check(TokenQuestionQuestion) {
				p.advance()
				rVal, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				args = append(args, p.valueToExprNode(rVal))
			}
			obj[key] = &CoalesceExpr{Args: args}
		} else {
			obj[key] = val
		}

		if p.check(TokenComma) {
			p.advance()
		} else {
			break
		}
	}

	if err := p.expect(TokenBraceClose); err != nil {
		return nil, err
	}

	return obj, nil
}

// ----------------------------------------------------------------------------
// Accessor Function Parsers
// ----------------------------------------------------------------------------

// parseVarAccessor parses var("NAME") - config variable reference.
func (p *Parser) parseVarAccessor() (ExpressionNode, error) {
	if !p.check(TokenString) {
		return nil, newParseErrorf(&p.current, "var() requires a string argument, got %q", p.current.Literal)
	}
	varName := p.current.Literal
	p.advance()

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &VarRefExpr{Name: varName}, nil
}

// parseStepAccessor parses step("id") - step result reference.
func (p *Parser) parseStepAccessor() (ExpressionNode, error) {
	if !p.check(TokenString) {
		return nil, newParseErrorf(&p.current, "step() requires a string argument, got %q", p.current.Literal)
	}
	stepId := p.current.Literal
	p.advance()

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &StepRefExpr{StepId: stepId}, nil
}

// parseFieldAccessor parses field(obj, "key") - field access on object.
func (p *Parser) parseFieldAccessor() (ExpressionNode, error) {
	// First argument: expression
	obj, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if !p.check(TokenComma) {
		return nil, newParseErrorf(&p.current, "field() requires two arguments")
	}
	p.advance()

	// Second argument: string key
	if !p.check(TokenString) {
		return nil, newParseErrorf(&p.current, "field() second argument must be a string key")
	}
	key := p.current.Literal
	p.advance()

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &FieldRefExpr{Object: obj, Key: key}, nil
}

// parseInputAccessor parses input() - automation input reference.
func (p *Parser) parseInputAccessor() (ExpressionNode, error) {
	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}
	return &InputRefExpr{}, nil
}

// parseItemAccessor parses item() - forEach item reference.
func (p *Parser) parseItemAccessor() (ExpressionNode, error) {
	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}
	return &ItemRefExpr{}, nil
}

// parseIndexAccessor parses index() - forEach index reference.
func (p *Parser) parseIndexAccessor() (ExpressionNode, error) {
	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}
	return &IndexRefExpr{}, nil
}

// parseEventAccessor parses event() - trigger event reference.
func (p *Parser) parseEventAccessor() (ExpressionNode, error) {
	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}
	return &EventRefExpr{}, nil
}

// parseActorAccessor parses caller() -- the authenticated user's
// AccessContext. Typed as CallerRefExpr; runtime resolves dotted
// paths like caller.userId, caller.role.
func (p *Parser) parseActorAccessor() (ExpressionNode, error) {
	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}
	return &CallerRefExpr{}, nil
}

// parseErrorAccessor parses error() or error("message").
// - error() returns ErrorRefExpr - references current error in onError context
// - error("message") returns ErrorExpr - creates an error with a message
func (p *Parser) parseErrorAccessor() (ExpressionNode, error) {
	// Check for no-arg accessor: error()
	if p.check(TokenParenClose) {
		p.advance()
		return &ErrorRefExpr{}, nil
	}

	// Has argument - parse as error creation: error("message")
	message, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}
	return &ErrorExpr{Message: message}, nil
}

// parseTimestampAccessor parses timestamp() or now() - current time.
func (p *Parser) parseTimestampAccessor() (ExpressionNode, error) {
	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}
	return &TimestampExprFunc{}, nil
}

// parseMemqlVersionFunction parses memqlVersion() - current service version.
func (p *Parser) parseMemqlVersionFunction() (ExpressionNode, error) {
	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}
	return &BuiltinFunctionExpr{
		Name:     "memqlVersion",
		Executor: "serviceVersion",
	}, nil
}

// parseConcatFunction parses concat(a, b, ...) - string concatenation.
func (p *Parser) parseConcatFunction() (ExpressionNode, error) {
	args, err := p.parseExpressionArgList()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &ConcatExpr{Args: args}, nil
}

// parseCoalesceFunction parses coalesce(a, b, ...) - first non-null.
func (p *Parser) parseCoalesceFunction() (ExpressionNode, error) {
	args, err := p.parseExpressionArgList()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &CoalesceExpr{Args: args}, nil
}

// parseCondFunction parses cond(predicate, thenValue, elseValue) -- the
// canonical conditional-value builtin. Deliberately not named `if()` to
// avoid visual confusion with the `if` control-flow statement used in
// automation step bodies.
func (p *Parser) parseCondFunction() (ExpressionNode, error) {
	// Predicate
	cond, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if !p.check(TokenComma) {
		return nil, newParseErrorf(&p.current, "cond() requires three arguments: cond(predicate, thenValue, elseValue)")
	}
	p.advance()

	// Then value
	thenVal, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if !p.check(TokenComma) {
		return nil, newParseErrorf(&p.current, "cond() requires three arguments: cond(predicate, thenValue, elseValue)")
	}
	p.advance()

	// Else value
	elseVal, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &CondExpr{Condition: cond, Then: thenVal, Else: elseVal}, nil
}

// parseFirstFunction parses first(expr) - first item.
func (p *Parser) parseFirstFunction() (ExpressionNode, error) {
	target, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &FirstExpr{Target: target}, nil
}

// parseLastFunction parses last(expr) - last item.
func (p *Parser) parseLastFunction() (ExpressionNode, error) {
	target, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &LastExpr{Target: target}, nil
}

// parseLowerFunction parses lower(str) - lowercase.
func (p *Parser) parseLowerFunction() (ExpressionNode, error) {
	target, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &LowerExpr{Target: target}, nil
}

// parseUpperFunction parses upper(str) - uppercase.
func (p *Parser) parseUpperFunction() (ExpressionNode, error) {
	target, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &UpperExpr{Target: target}, nil
}

// parseTrimFunction parses trim(str) - remove whitespace.
func (p *Parser) parseTrimFunction() (ExpressionNode, error) {
	target, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &TrimExpr{Target: target}, nil
}

// parseHashFunction parses hash(str) - SHA256 hash.
func (p *Parser) parseHashFunction() (ExpressionNode, error) {
	target, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &HashExpr{Target: target}, nil
}

// parseShortIdFunction parses shortId(value) - strip the canonical
// concept prefix and return the bare short id. Single-arg, mirrors
// lower/upper/trim/hash.
func (p *Parser) parseShortIdFunction() (ExpressionNode, error) {
	target, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &ShortIdExpr{Target: target}, nil
}

// parseCanonicalIdFunction parses canonicalId(value, "<conceptType>")
// - normalize an id-shaped value to canonical form.
//
// The first argument is any expression. The second argument MUST be
// a literal quoted string (the concept name) so the engine can resolve
// the concept's @scope at execution time without re-parsing dynamic
// values. Letting the concept name be a runtime expression invites
// silently-wrong-canonicalization bugs.
func (p *Parser) parseCanonicalIdFunction() (ExpressionNode, error) {
	value, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}
	if err := p.expect(TokenComma); err != nil {
		return nil, err
	}
	// The second argument names the concept. Two forms (#987):
	//   - typed short-name (canonical): `canonicalId(x, space)` -- an imported
	//     concept short-name. The component/memql loader resolves it to the
	//     canonical-id string before parse; registry-less structural parse
	//     paths (e.g. dslimports.Load) see the bare identifier here and accept
	//     it verbatim.
	//   - quoted canonical-id string (retired): `canonicalId(x, "v1:ns:name")`.
	if !p.check(TokenString) && !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "canonicalId(): second argument must be an imported concept short-name (e.g. `user`) or a quoted canonical-id string")
	}
	concept := strings.TrimSpace(p.current.Literal)
	p.advance()
	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}
	if concept == "" {
		return nil, newParseErrorf(&p.current, "canonicalId(): concept name is empty")
	}
	return &CanonicalIdExpr{Value: value, Concept: concept}, nil
}

// parseCaseFunction parses case(condition, value) - for match() branches.
func (p *Parser) parseCaseFunction() (ExpressionNode, error) {
	cond, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}
	if !p.check(TokenComma) {
		return nil, newParseErrorf(&p.current, "case() requires two arguments: condition and value")
	}
	p.advance()
	val, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}
	return &FunctionCallExpr{Name: "case", Args: map[string]any{"0": cond, "1": val}}, nil
}

// parseDefaultFunction parses default(value) - fallback for match().
func (p *Parser) parseDefaultFunction() (ExpressionNode, error) {
	val, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}
	return &FunctionCallExpr{Name: "default", Args: map[string]any{"0": val}}, nil
}

// parseToStringFunction parses toString(expr) - string conversion.
func (p *Parser) parseToStringFunction() (ExpressionNode, error) {
	target, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &ToStringExpr{Target: target}, nil
}

// parseAddDurationFunction parses addDuration(timestamp, duration).
func (p *Parser) parseAddDurationFunction() (ExpressionNode, error) {
	timestamp, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if !p.check(TokenComma) {
		return nil, newParseErrorf(&p.current, "addDuration() requires two arguments")
	}
	p.advance()

	duration, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &AddDurationExpr{Timestamp: timestamp, Duration: duration}, nil
}

// parseDaysBetweenFunction parses daysBetween(date1, date2).
func (p *Parser) parseDaysBetweenFunction() (ExpressionNode, error) {
	date1, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if !p.check(TokenComma) {
		return nil, newParseErrorf(&p.current, "daysBetween() requires two arguments")
	}
	p.advance()

	date2, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &DaysBetweenExpr{Date1: date1, Date2: date2}, nil
}

// parseSubtractTimestampsFunction parses subtractTimestamps(t1, t2).
func (p *Parser) parseSubtractTimestampsFunction() (ExpressionNode, error) {
	t1, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if !p.check(TokenComma) {
		return nil, newParseErrorf(&p.current, "subtractTimestamps() requires two arguments")
	}
	p.advance()

	t2, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &SubtractTimestampsExpr{T1: t1, T2: t2}, nil
}

// parseYearFunction parses year(timestamp).
func (p *Parser) parseYearFunction() (ExpressionNode, error) {
	target, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &YearExpr{Target: target}, nil
}

// parseQuarterFunction parses quarter(timestamp).
func (p *Parser) parseQuarterFunction() (ExpressionNode, error) {
	target, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &QuarterExpr{Target: target}, nil
}

// parseMonthFunction parses month(timestamp).
func (p *Parser) parseMonthFunction() (ExpressionNode, error) {
	target, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &MonthExpr{Target: target}, nil
}

// parseDayOfMonthFunction parses dayOfMonth(timestamp).
func (p *Parser) parseDayOfMonthFunction() (ExpressionNode, error) {
	target, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &DayOfMonthExpr{Target: target}, nil
}

// parseIsAnniversaryFunction parses isAnniversary(startDate, checkDate).
func (p *Parser) parseIsAnniversaryFunction() (ExpressionNode, error) {
	startDate, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if !p.check(TokenComma) {
		return nil, newParseErrorf(&p.current, "isAnniversary() requires two arguments")
	}
	p.advance()

	checkDate, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &IsAnniversaryExpr{StartDate: startDate, CheckDate: checkDate}, nil
}

// parseIsFirstDayOfQuarterFunction parses isFirstDayOfQuarter(timestamp).
func (p *Parser) parseIsFirstDayOfQuarterFunction() (ExpressionNode, error) {
	target, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &IsFirstDayOfQuarterExpr{Target: target}, nil
}

// parseContainsFunction parses contains(str, substr).
func (p *Parser) parseContainsFunction() (ExpressionNode, error) {
	if p.check(TokenParenClose) {
		return nil, newParseErrorf(&p.current, "contains() requires at least one argument")
	}
	// First argument: parse with the AND-level helper so a runtime
	// `contains(concept==X; payload.y=="z")` relationship invocation
	// picks up the full `;`-joined filter expression. parseLogicalAnd
	// reduces to parsePrimary when the input is a single value
	// reference (the existing string-search 2-arg form), so the
	// legacy DSL surface continues to work unchanged.
	target, err := p.parseLogicalAnd()
	if err != nil {
		return nil, err
	}

	// Single-arg form: modern runtime relationship wrapper. Mirrors
	// what the memql runtime parser (parseRelationship, parser.go
	// ~line 535) emits for `contains(filter)`. The two-arg string-
	// search form (DSL load-time `contains(text, substr)`) keeps its
	// existing *ContainsExpr shape and falls through below.
	if p.check(TokenParenClose) {
		p.advance()
		return &RelationshipExpr{Function: RelContains, Target: target}, nil
	}

	if !p.check(TokenComma) {
		return nil, newParseErrorf(&p.current, "contains() expects ')' after relationship target or ',' before substring argument, got %q", p.current.Literal)
	}
	p.advance()

	substr, err := p.parseExpressionArg()
	if err != nil {
		return nil, err
	}

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &ContainsExpr{Target: target, Substring: substr}, nil
}

// parseExpressionArg parses a single expression argument (stopping at comma or paren).
func (p *Parser) parseExpressionArg() (ExpressionNode, error) {
	// For simple cases, just use parsePrimary
	// This handles identifiers, strings, numbers, function calls
	return p.parsePrimary()
}

// consumePostCallDotAccess wraps call with DotAccessExpr nodes if the
// next tokens are a dotted suffix. Accepts both lexer shapes:
//
//  1. TokenOperator "." followed by TokenIdentifier   (e.g. `foo() . bar`)
//  2. TokenIdentifier whose literal starts with "."   (e.g. `foo().bar.baz`)
//     — this is the common case because isIdentifierCharNoColon treats
//     `.` as part of an identifier when it immediately follows `)`.
//
// Each dotted segment becomes one DotAccessExpr level; deeper paths
// like `.payload.name` nest as DotAccess{DotAccess{call, "payload"}, "name"}.
// Returns call unchanged when no suffix is present.
func (p *Parser) consumePostCallDotAccess(call ExpressionNode) (ExpressionNode, error) {
	for {
		switch {
		case p.check(TokenOperator) && p.current.Literal == ".":
			p.advance() // consume '.'
			if !p.check(TokenIdentifier) {
				return nil, newParseErrorf(&p.current, "expected identifier after '.', got %q", p.current.Literal)
			}
			literal := p.current.Literal
			p.advance()
			// The identifier literal itself may contain further dotted
			// segments (e.g. `payload.name`). Split and wrap each.
			for _, segment := range strings.Split(literal, ".") {
				if segment == "" {
					continue
				}
				call = &DotAccessExpr{Object: call, Field: segment}
			}
		case p.check(TokenIdentifier) && strings.HasPrefix(p.current.Literal, "."):
			literal := strings.TrimPrefix(p.current.Literal, ".")
			p.advance()
			for _, segment := range strings.Split(literal, ".") {
				if segment == "" {
					continue
				}
				call = &DotAccessExpr{Object: call, Field: segment}
			}
		default:
			return call, nil
		}
	}
}

// parseExpressionArgList parses comma-separated expression arguments.
func (p *Parser) parseExpressionArgList() ([]ExpressionNode, error) {
	var args []ExpressionNode

	for !p.check(TokenParenClose) && !p.check(TokenEOF) {
		arg, err := p.parseExpressionArg()
		if err != nil {
			return nil, err
		}
		args = append(args, arg)

		if p.check(TokenComma) {
			p.advance()
		} else {
			break
		}
	}

	return args, nil
}

// ----------------------------------------------------------------------------
// Helper methods
// ----------------------------------------------------------------------------

func (p *Parser) check(typ TokenType) bool {
	return p.current.Type == typ
}

func (p *Parser) advance() {
	p.pos++
	if p.pos < len(p.tokens) {
		p.current = p.tokens[p.pos]
	} else {
		p.current = Token{Type: TokenEOF}
	}
}

func (p *Parser) expect(typ TokenType) error {
	if !p.check(typ) {
		return newParseErrorf(&p.current, "expected %v, got %q", typ, p.current.Literal)
	}
	p.advance()
	return nil
}

func (p *Parser) peekAhead(n int) Token {
	idx := p.pos + n
	if idx < len(p.tokens) {
		return p.tokens[idx]
	}
	return Token{Type: TokenEOF}
}

// isSimpleIdentifierSegment is preserved as a local alias for
// readability at the call sites. The implementation lives in
// component/memql/baseparser as the canonical predicate.
func isSimpleIdentifierSegment(s string) bool {
	return baseparser.IsSimpleIdentifier(s)
}

// canonicalKeyAccessor reports whether the given function name is an
// accessor whose single-string argument is suitable for deriving an
// object-literal key (see Shorthand B in parseObject).
func canonicalKeyAccessor(name string) bool {
	switch name {
	case "node", "var":
		return true
	}
	return false
}

// inferAccessorKey extracts the terminal-segment key from a parsed
// function-call value that matches one of the canonical accessor
// shorthands. It accepts both the raw FunctionCallExpression AST node
// and the reconstructed string form (produced when the parser absorbs
// a trailing dotted suffix). Returns ("", false) if the call's
// argument isn't a single quoted-string simple-identifier path.
func inferAccessorKey(val any) (string, bool) {
	var arg string
	switch v := val.(type) {
	case *FunctionCallExpr:
		if len(v.Args) != 1 {
			return "", false
		}
		// Args is a map keyed by param name or positional index. Take
		// the first string value we see; accessor calls only have one
		// argument by construction.
		for _, any := range v.Args {
			if s, ok := any.(string); ok {
				arg = s
				break
			}
		}
	case string:
		// `func("path.name").segment` was reconstructed to a string --
		// not a clean accessor call. Refuse to infer.
		return "", false
	default:
		return "", false
	}
	if arg == "" {
		return "", false
	}
	key := arg
	if idx := strings.LastIndex(arg, "."); idx >= 0 {
		key = arg[idx+1:]
	}
	if !isSimpleIdentifierSegment(key) {
		return "", false
	}
	return key, true
}

func (p *Parser) reconstructTokens(start, end int) string {
	var parts []string
	for i := start; i < end && i < len(p.tokens); i++ {
		tok := p.tokens[i]
		// Re-add quotes around string tokens since the lexer strips them
		if tok.Type == TokenString {
			parts = append(parts, fmt.Sprintf("%q", tok.Literal))
		} else {
			parts = append(parts, tok.Literal)
		}
	}
	return strings.Join(parts, "")
}

// numericAsInt coerces a parser-emitted numeric value to an int.
// Accepts both int64 (the canonical type for integer literals emitted
// by baseparser.ParseNumericLiteral) and float64 (decimals and
// pre-#255 emissions) so directive-arg consumers that need an int
// (paginate, withDepth, forEach concurrency) don't have to re-
// implement the type dispatch at every call site.
//
// Out-of-range values reject (ok=false) rather than wrap. The bound
// is math.MaxInt32 / math.MinInt32 rather than math.MaxInt /
// math.MinInt: (1) `int` is platform-sized (32 bits on 32-bit
// builds, 64 on 64-bit), so silently truncating int64 to int on
// 32-bit is a latent bug; (2) the four directive consumers all
// expect small positive integers and never approach 2B, so an
// int32 sub-range is operationally fine; (3) chaining the
// narrowed `int32(n)` through `int(...)` gives CodeQL's
// go/incorrect-integer-conversion rule an explicit narrow-then-
// widen pattern it recognises, where the previous direct
// `int(n)` after a range check tripped a false positive against
// the int64-source taint flow even with the bound predicate in
// place.
func numericAsInt(v any) (int, bool) {
	switch n := v.(type) {
	case int64:
		if n < math.MinInt32 || n > math.MaxInt32 {
			return 0, false
		}
		return int(int32(n)), true
	case float64:
		if n < math.MinInt32 || n > math.MaxInt32 {
			return 0, false
		}
		return int(int32(n)), true
	}
	return 0, false
}
