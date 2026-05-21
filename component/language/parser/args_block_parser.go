package parser

import (
	"fmt"
	"strconv"
	"strings"
)

// parseFileTopArgsBlock parses a file-level `args { ... }` declaration.
// Each non-blank line declares one field:
//
//	<name> <type> [@required] [@enum("a", "b", ...)] [@default(<value>)]
//
// Returns an *ArgsSchema populated with the field list. Caller stores it
// on the parser and attaches to the next definition.
//
// `args` is a contextual keyword; the lexer emits it as TokenIdentifier
// with literal "args". parseDefinition / parseFile checks for that
// before falling through to the regular switch.
func (p *Parser) parseFileTopArgsBlock() (*ArgsSchema, error) {
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
	name := p.current.Literal
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

	field := &ArgsField{Name: name, Type: typ, Optional: true}
	if itemType != "" {
		field.Items = &ArgsField{Type: itemType}
	}

	for p.check(TokenAt) {
		p.advance() // consume `@`
		if !p.check(TokenIdentifier) {
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
		case "default":
			// @default(<literal>) -- accepted but no AST slot today;
			// caller can read via the field's default-via-coalesce in
			// the body. Silently consume the value.
			if err := p.expect(TokenParenOpen); err != nil {
				return nil, err
			}
			p.advance()
			if !p.check(TokenParenClose) {
				return nil, newParseErrorf(&p.current, "expected `)` after @default value on args field %q", name)
			}
			p.advance()
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
			return nil, newParseErrorf(&p.current, "unknown annotation @%s on args field %q (supported: @required, @enum, @default, @description, @maxLength, @pattern)", ann, name)
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
