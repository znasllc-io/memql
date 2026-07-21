package parser

import (
	"fmt"
	"strconv"
	"strings"
)

// parseFileTopArgsBlock parses a file-level `args { ... }` declaration.
// Each non-blank line declares one field:
//
//	<name> <type> [@required] [@enum("a", "b", ...)]
//	               [@maxLength(N)] [@pattern("re")]
//
// (`@default` is NOT a valid args-field annotation -- it was never applied
// and is rejected; apply defaults in the body via `coalesce(args.X, <v>)`.
// `@description` on an args field is accepted for compatibility but
// DISCARDED -- no AST slot, #2615; per-field docs arrive with /// doc
// comments, #2601. Do not write it.)
//
// Returns an *ArgsSchema populated with the field list. Caller stores it
// on the parser and attaches to the next definition.
//
// `args` is a contextual keyword; the lexer emits it as TokenIdentifier
// with literal "args". parseDefinition / parseFile checks for that
// before falling through to the regular switch.
func (p *Parser) parseFileTopArgsBlock() (*ArgsSchema, error) {
	p.argsBlockOpenLine = p.current.Line
	if !(p.check(TokenIdentifier) && p.current.Literal == "args") {
		return nil, newParseErrorf(&p.current, "expected `args` keyword")
	}
	p.advance() // consume `args`
	if !p.check(TokenBraceOpen) {
		return nil, newParseErrorf(&p.current, "expected `{` after `args`")
	}
	p.advance() // consume `{`

	def := &ArgsSchema{Target: "args"}

	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		// Each field is on its own line; tokens are
		// IDENT IDENT (@IDENT (paren-group)?)* . The lexer collapses
		// whitespace, so we read one field declaration per loop
		// iteration, terminating when we hit a `}`.
		field, err := p.parseArgsBlockField()
		if err != nil {
			return nil, err
		}
		def.Fields = append(def.Fields, field)
	}

	if !p.check(TokenBraceClose) {
		return nil, newParseErrorf(&p.current, "expected `}` to close args block")
	}
	p.advance() // consume `}`
	return def, nil
}

// parseArgsBlockField parses a single `<name> <type> [@annotations]`
// line from the args block.
func (p *Parser) parseArgsBlockField() (*ArgsField, error) {
	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected field name in args block, got %q", p.current.Literal)
	}
	// A /// block immediately above the field is the field's doc comment
	// (memql#2633) -- the ONLY channel for arg descriptions; the args-field
	// @description spelling stays retired (#2615).
	fieldDoc := p.takeFieldDocFor(p.current.Line)
	name := p.current.Literal
	// Reserved engine names (S6, memql#2361 -- implementing the documented
	// rejection): `now`, `actor`, `partition`, `config`, and `trace` are
	// ambient top-level identifiers every body may read; an args field of
	// the same name would make resolution ambiguous. Rejected at parse
	// time. (`event` is additionally reserved for AUTOMATION args by the
	// G2 load-time shadow check.)
	if reservedArgsNames[name] {
		return nil, newParseErrorf(&p.current, "args field %q collides with a reserved engine name (now / actor / partition / config / trace) -- rename the field", name)
	}
	p.advance()

	// Accept either a bare type (`object`, `string`, ...) or an array
	// shorthand (`[]object`, `[]string`, ...). The shorthand is
	// translated to {Type: "array", Items: {Type: <inner>}} so the
	// existing validator path -- which already understands Type=array
	// + Items -- handles it without further changes.
	var (
		typ      string
		itemType string
	)
	if p.check(TokenBracketOpen) {
		p.advance() // consume `[`
		if !p.check(TokenBracketClose) {
			return nil, newParseErrorf(&p.current, "expected `]` after `[` in args field %q array shorthand", name)
		}
		p.advance() // consume `]`
		if !p.check(TokenIdentifier) {
			return nil, newParseErrorf(&p.current, "expected element type after `[]` in args field %q", name)
		}
		itemType = p.current.Literal
		p.advance()
		typ = "array"
	} else if p.check(TokenIdentifier) {
		typ = p.current.Literal
		p.advance()
	} else {
		return nil, newParseErrorf(&p.current, "expected type after field name %q in args block", name)
	}

	field := &ArgsField{Name: name, Type: typ, Optional: true, DocComment: fieldDoc}
	if itemType != "" {
		field.Items = &ArgsField{Type: itemType}
	}

	// First-class enum type (#2618): `status enum("a", "b")` is the
	// self-contained spelling of `status string @enum("a", "b")` --
	// same representation (string type + Enum values), one statement.
	// A bare `enum` with no parens keeps its legacy meaning (the
	// lexicon's "pair with @enum(...)" form).
	if typ == "enum" && p.check(TokenParenOpen) {
		p.advance()
		var values []any
		for !p.check(TokenParenClose) {
			if !p.check(TokenString) {
				return nil, newParseErrorf(&p.current, "enum values must be quoted strings on args field %q", name)
			}
			values = append(values, p.current.Literal)
			p.advance()
			if p.check(TokenComma) {
				p.advance()
			}
		}
		p.advance() // consume `)`
		if len(values) == 0 {
			return nil, newParseErrorf(&p.current, "enum type on args field %q needs at least one value", name)
		}
		field.Type = "string"
		field.Enum = values
	}

	// Required sigil (#2618): `name string!` sets required exactly as
	// @required does. Sigil plus an explicit @required is idempotent.
	if p.check(TokenBang) {
		p.advance()
		field.Optional = false
	}

	for p.check(TokenAt) {
		p.advance() // consume `@`
		if !p.check(TokenIdentifier) {
			// `default` lexes as a keyword token, not an identifier, so an
			// `@default` on an args field lands here. It is retired (#991):
			// it was never applied at arg resolution (a silent footgun).
			// Apply the default explicitly in the body via
			// `coalesce(args.X, <default>)`, or use a concept-field @default
			// (those ARE honored on insert). @default thus means one thing.
			if p.current.Literal == "default" {
				return nil, newParseErrorf(&p.current, "@default on an args field %q is retired -- it is never applied; use `coalesce(args.%s, <default>)` in the body, or a concept-field @default", name, name)
			}
			return nil, newParseErrorf(&p.current, "expected annotation name after `@` on args field %q", name)
		}
		ann := p.current.Literal
		p.advance()
		switch ann {
		case "required":
			field.Optional = false
		case "enum":
			// @enum("a", "b", "c")
			if err := p.expect(TokenParenOpen); err != nil {
				return nil, err
			}
			var values []any
			for !p.check(TokenParenClose) {
				if !p.check(TokenString) {
					return nil, newParseErrorf(&p.current, "expected string literal inside @enum(...) on args field %q", name)
				}
				values = append(values, p.current.Literal)
				p.advance()
				if p.check(TokenComma) {
					p.advance()
				}
			}
			p.advance() // consume `)`
			field.Enum = values
		case "description":
			// @description("...") -- silently accepted (no AST slot)
			if err := p.expect(TokenParenOpen); err != nil {
				return nil, err
			}
			if !p.check(TokenString) {
				return nil, newParseErrorf(&p.current, "expected string literal inside @description(...) on args field %q", name)
			}
			p.advance()
			if !p.check(TokenParenClose) {
				return nil, newParseErrorf(&p.current, "expected `)` after @description on args field %q", name)
			}
			p.advance()
		case "maxLength":
			// @maxLength(N) -- rune-count cap for string args. Other
			// arg types accept the annotation at parse time but the
			// validator only enforces it on strings (the natural
			// semantics for length).
			if err := p.expect(TokenParenOpen); err != nil {
				return nil, err
			}
			if !p.check(TokenNumber) {
				return nil, newParseErrorf(&p.current, "expected number literal inside @maxLength(...) on args field %q", name)
			}
			n, convErr := strconv.Atoi(p.current.Literal)
			if convErr != nil {
				return nil, newParseErrorf(&p.current, "invalid @maxLength value %q on args field %q: %v", p.current.Literal, name, convErr)
			}
			if n < 0 {
				return nil, newParseErrorf(&p.current, "@maxLength on args field %q must be non-negative, got %d", name, n)
			}
			field.MaxLength = n
			p.advance()
			if !p.check(TokenParenClose) {
				return nil, newParseErrorf(&p.current, "expected `)` after @maxLength value on args field %q", name)
			}
			p.advance()
		case "pattern":
			// @pattern("regex") -- string-only regex match. The
			// pattern is stored verbatim here; compilation + caching
			// happens once at function-loader time so invalid patterns
			// fail loud during DSL parse rather than per-call.
			if err := p.expect(TokenParenOpen); err != nil {
				return nil, err
			}
			if !p.check(TokenString) {
				return nil, newParseErrorf(&p.current, "expected string literal inside @pattern(...) on args field %q", name)
			}
			field.Pattern = p.current.Literal
			p.advance()
			if !p.check(TokenParenClose) {
				return nil, newParseErrorf(&p.current, "expected `)` after @pattern value on args field %q", name)
			}
			p.advance()
		default:
			return nil, newParseErrorf(&p.current, "unknown annotation @%s on args field %q (supported: @required, @enum, @maxLength, @pattern; @description is accepted but discarded -- #2615)", ann, name)
		}
	}
	return field, nil
}

// formatArgsBlock renders an *ArgsSchema back into source-form
// `args { <field>... }` text. Used by the struct rewriters when they
// move the args block from inside a struct body to file-top.
func formatArgsBlock(def *ArgsSchema) string {
	if def == nil || len(def.Fields) == 0 {
		return "args { }"
	}
	var b strings.Builder
	b.WriteString("args {\n")
	for _, f := range def.Fields {
		b.WriteString(fmt.Sprintf("  %s %s", f.Name, f.Type))
		if !f.Optional {
			b.WriteString(" @required")
		}
		if len(f.Enum) > 0 {
			b.WriteString(" @enum(")
			for i, v := range f.Enum {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(fmt.Sprintf("%q", fmt.Sprint(v)))
			}
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// reservedArgsNames are the ambient top-level identifiers an args field may
// not shadow (the argument-resolution contract, docs + CLAUDE.md).
var reservedArgsNames = map[string]bool{
	"now": true, "actor": true, "partition": true, "config": true, "trace": true,
}
