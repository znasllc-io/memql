package parser

import (
	"fmt"
	"strconv"

	"github.com/znasllc-io/memql/component/language/annotations"
)

// parseFileTopArgsBlock parses a file-level `args { ... }` declaration.
// Each non-blank line declares one field:
//
//	<name> <type> [@required] [@enum("a", "b", ...)]
//	               [@maxLength(N)] [@pattern("re")]
//	               [@minimum(N)] [@maximum(N)]
//
// (`@default` is NOT a valid args-field annotation -- it was never applied
// and is rejected; apply defaults in the body via `args.X ?? <v>`.
// `@description` is NOT one either -- it was accepted and then discarded
// (no AST slot) and is now rejected outright, memql#3336; an arg
// description is carried by the `///` doc comment above the field, #2601.)
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

	// Which construct the block belongs to, for an args-field refusal.
	owner := p.argsOwner
	if owner == "" {
		owner = p.hoistedArgsOwner()
	}

	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		// Each field is on its own line; tokens are
		// IDENT IDENT (@IDENT (paren-group)?)* . The lexer collapses
		// whitespace, so we read one field declaration per loop
		// iteration, terminating when we hit a `}`.
		field, err := p.parseArgsBlockField(owner)
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

// hoistedArgsOwner names the function construct a hoisted args block belongs
// to -- `query "probe"` -- or "" when none follows. The rewriter emits a
// function construct's `args { ... }` above its `func (Receiver) name(...)`
// header, so the owner is the next such header.
func (p *Parser) hoistedArgsOwner() string {
	for i := 0; ; i++ {
		switch p.peekAhead(i).Type {
		case TokenEOF:
			return ""
		case TokenKeywordFunc:
			if p.peekAhead(i+1).Type != TokenParenOpen || p.peekAhead(i+3).Type != TokenParenClose {
				return ""
			}
			fr, ok := functionReceivers[ReceiverType(p.peekAhead(i+2).Literal)]
			if !ok {
				return ""
			}
			return fmt.Sprintf("%s %q", fr.keyword, p.peekAhead(i+4).Literal)
		}
	}
}

// parseArgsBlockField parses a single `<name> <type> [@annotations]`
// line from the args block. owner names the construct the block belongs to,
// for a refusal's subject ("" when unknown).
func (p *Parser) parseArgsBlockField(owner string) (*ArgsField, error) {
	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected field name in args block, got %q", p.current.Literal)
	}
	// A /// block immediately above the field is the field's doc comment
	// (memql#2633) -- the ONLY channel for arg descriptions; the args-field
	// @description spelling is rejected below (memql#3336).
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

	// The annotations are held to the ArgsField receiver (memql#5359) as each
	// one is reached, before its arguments are read: the per-annotation
	// parsing below keeps only what a legal value MEANS. The retired
	// @default (#991: never applied; `args.X ?? <default>` in the body is the
	// mechanism, and a concept-field @default is no substitute, memql#2960)
	// and @description (memql#3336: never retained; the `///` doc comment is
	// the channel) are refused there with their hints.
	subject := fmt.Sprintf("args field %q", name)
	if owner != "" {
		subject = owner + ", " + subject
	}
	var uses []annotations.Use
	for p.check(TokenAt) {
		at := p.current
		p.advance() // consume `@`
		// `default` lexes as a keyword token, not an identifier.
		if !p.check(TokenIdentifier) && !isKeywordToken(p.current.Type) {
			return nil, newParseErrorf(&p.current, "expected annotation name after `@` on args field %q", name)
		}
		ann := p.current.Literal
		p.advance()
		uses = append(uses, p.peekAnnotationUse(ann))
		if ref := annotations.CheckAll(annotations.ArgsField, uses); ref != nil {
			return nil, annotationRefusalError(at, subject, ref)
		}
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
		case "minimum", "maximum":
			// @minimum(N) / @maximum(N) -- INCLUSIVE numeric bounds.
			//
			// Every layer below this one already carried them: ast.ArgsField
			// has the fields, function_loader copies them onto
			// FunctionArgsField, validateArgsField enforces them, and both the
			// tool JSON-schema emitter and the MCP promoter publish them. The
			// legacy schema-style args form set them from a `minimum:` key.
			// The modern `args { }` block was the ONE surface that could not,
			// so an author who wanted a bound had to write a Go seam or go
			// without -- which is how memql#4522 arrived needing cursorTweenMs
			// held to 250-2500 with no way to say so.
			//
			// A discrete numeric set deliberately does NOT get the same
			// treatment through @enum: valueInEnum compares with
			// reflect.DeepEqual, so an enum member parsed here would be
			// compared against the float64 a JSON caller sends and never match
			// -- the field would refuse EVERY value, fail-closed and silent
			// about why. The bounds path normalises through numericValue
			// instead, which is what makes it safe to open up and @enum not.
			bound, boundErr := p.parseNumericArgsAnnotation(ann, name)
			if boundErr != nil {
				return nil, boundErr
			}
			if ann == "minimum" {
				field.Minimum = &bound
			} else {
				field.Maximum = &bound
			}
		}
	}
	return field, nil
}

// peekAnnotationUse classifies the argument list after an annotation's name
// without consuming it, into the Use AnnotationUse would make of the same
// annotation parsed by parseAttribute -- the same forms, and for keyword
// arguments the keys in written order with their shapes. The args block reads
// its annotations' arguments itself, so the registry check needs this to see
// the form BEFORE the per-annotation parsing reports a narrower error about
// it. The two readings are pinned to each other by
// TestArgsBlockAndAttributeClassifiersAgree, over every form parseAttribute
// produces; the one spelling only this one reads is a negative number whose
// minus is separated from its digits (`@minimum(- 1)`), which parseAttribute
// refuses.
func (p *Parser) peekAnnotationUse(name string) annotations.Use {
	u := annotations.Use{Name: name}
	if !p.check(TokenParenOpen) {
		u.Form = annotations.FormFlag
		return u
	}
	first := p.peekAhead(1)
	switch {
	case first.Type == TokenParenClose:
		u.Form = annotations.FormEmpty
	case name == "filter" && first.Type != TokenString && first.Type != TokenBraceOpen:
		u.Form = annotations.FormExpression
	case first.Type == TokenBraceOpen:
		u.Form = annotations.FormObject
	case first.Type == TokenBang:
		u.Form = annotations.FormExclude
	case first.Type == TokenString:
		u.Form = annotations.FormString
		if p.peekAhead(2).Type == TokenComma {
			u.Form = annotations.FormStrings
		}
	case first.Type == TokenNumber:
		u.Form = annotations.FormNumber
	case first.Literal == "-" && p.peekAhead(2).Type == TokenNumber:
		u.Form = annotations.FormNumber
	case (first.Literal == "true" || first.Literal == "false") && p.peekAhead(2).Type == TokenParenClose:
		u.Form = annotations.FormBool
	default:
		u.Form = annotations.FormKeywords
		u.Keys = p.peekKeywordKeys()
	}
	return u
}

// peekKeywordKeys reads the keys of the keyword argument list that opens at
// the current `(`, without consuming it: every identifier that opens an
// argument (after the `(` or a `,` at the top level of the list), bare unless
// an `=` or `:` follows it.
func (p *Parser) peekKeywordKeys() []annotations.WrittenKey {
	var keys []annotations.WrittenKey
	depth := 0
	opensArg := true
	for i := 1; ; i++ {
		tok := p.peekAhead(i)
		switch tok.Type {
		case TokenEOF:
			return keys
		case TokenParenOpen, TokenBraceOpen, TokenBracketOpen:
			depth++
		case TokenParenClose, TokenBraceClose, TokenBracketClose:
			if depth == 0 {
				return keys
			}
			depth--
		case TokenComma:
			if depth == 0 {
				opensArg = true
				continue
			}
		default:
			if opensArg && depth == 0 && (tok.Type == TokenIdentifier || isKeywordTokenForAttribute(tok.Type)) {
				next := p.peekAhead(i + 1)
				valued := next.Type == TokenOperator && (next.Literal == "=" || next.Literal == ":")
				keys = append(keys, annotations.WrittenKey{Name: tok.Literal, Bare: !valued})
			}
		}
		opensArg = false
	}
}

// reservedArgsNames are the ambient top-level identifiers an args field may
// not shadow (the argument-resolution contract, docs + CLAUDE.md).
var reservedArgsNames = map[string]bool{
	"now": true, "actor": true, "partition": true, "config": true, "trace": true,
}

// parseNumericArgsAnnotation reads the `( <number> )` group shared by
// @minimum and @maximum and returns the bound.
//
// A leading `-` is consumed separately: the lexer folds a minus written
// against the digits into the number (`-1` is one TokenNumber), but a minus
// separated from them (`- 1`) arrives as the operator token followed by the
// magnitude, and reading only TokenNumber would reject that with an error
// naming the wrong thing.
func (p *Parser) parseNumericArgsAnnotation(ann, name string) (float64, error) {
	if err := p.expect(TokenParenOpen); err != nil {
		return 0, err
	}
	negative := false
	if p.current.Literal == "-" {
		negative = true
		p.advance()
	}
	if !p.check(TokenNumber) {
		return 0, newParseErrorf(&p.current, "expected number literal inside @%s(...) on args field %q", ann, name)
	}
	value, convErr := strconv.ParseFloat(p.current.Literal, 64)
	if convErr != nil {
		return 0, newParseErrorf(&p.current, "invalid @%s value %q on args field %q: %v", ann, p.current.Literal, name, convErr)
	}
	if negative {
		value = -value
	}
	p.advance()
	if !p.check(TokenParenClose) {
		return 0, newParseErrorf(&p.current, "expected `)` after @%s value on args field %q", ann, name)
	}
	p.advance()
	return value, nil
}
