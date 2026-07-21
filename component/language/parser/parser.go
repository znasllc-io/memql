package parser

import (
	"fmt"
	"math"
	"sort"
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

	// Doc-comment side channel (memql#2633; see doc_comments.go).
	docBlocks         []DocCommentBlock
	docConsumed       []bool
	transparentLines  map[int]bool
	argsBlockOpenLine int

	// forEachOrdinal counts the forEach loops parsed within the CURRENT
	// top-level construct, and is the discriminator in a synthetic
	// forEach step id (#2659). It replaces the loop's character offset +
	// line, which churned on any edit above the loop -- a comment reflow
	// renamed the step, and step ids are persisted in automation
	// checkpoints (component/automations/checkpoint.go keys step results
	// by id), so unrelated edits silently broke resume. Reset per
	// construct in parseDefinition.
	forEachOrdinal int

	// src is the original source the tokens were lexed from, stored as
	// runes because Token.Pos / Token.EndPos are rune indices (the lexer
	// scans over a []rune). It is populated by ParseFile and by the
	// function loader so a collection-chain step RHS (#2317) can be sliced
	// back to its EXACT source span. Nil for callers that don't set it
	// (e.g. ad-hoc NewParser(tokens) expression parses) -- the step-RHS
	// branch falls back to the existing error in that case.
	src []rune

	// pendingArgs holds a file-top `args { ... }` block parsed
	// immediately before the next definition. parseFile attaches it
	// to the resulting FunctionDef and clears the field.
	pendingArgs *ArgsSchema

	// deferredErrors collects errors that surface during attribute
	// processing or other post-parse passes where the call chain has
	// no error-return path. parseFile drains the slice after parsing
	// completes and surfaces the first entry.
	deferredErrors []error

	// currentFuncType is the receiver kind of the construct whose body
	// is being parsed (set in parseGoStyleFunction around the body
	// switch, restored after). It lets construct-scoped grammar rules
	// reject a clause used in the wrong construct -- specifically the
	// query-only `asOf` time-travel clause (core-builtins ADR §2.3),
	// which parseAsOfFunction rejects when this is an explicit
	// non-query kind. The zero value ("" -- standalone ParseExpression /
	// runtime query strings) stays permissive so handwritten query
	// expressions keep working.
	currentFuncType FunctionType

	// suppressCommaOr disables the legacy `,`-as-OR separator at the
	// logical-OR precedence level. It is set while parsing the arguments
	// of a Story 4 collection method (#2302 / ADR §2.2) so a comma
	// terminates one argument instead of folding the next into an OR
	// expression (e.g. `reduce(0, (acc, n) => ...)` -- the `0` arg must
	// stop at the comma). `||` still parses as OR.
	suppressCommaOr bool

	// suppressComparisonFold disables parseIdentifierExpression's early
	// identifier-led comparison fold while a `??` chain's continuation
	// operands parse (#2611): without it, `stage ?? def == "active"` would
	// swallow the comparison into the coalesce arm (the JS-loose shape) --
	// precedence must not depend on the operand's token type.
	suppressComparisonFold bool
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

// SetSource records the original source string the tokens were lexed from
// so byte-exact source spans can be sliced during parsing (#2317). Callers
// that parse logic bodies (the function loader, ParseFile) set this; it must
// be the EXACT string handed to NewLexer, since Token.Pos / Token.EndPos are
// rune indices into it.
func (p *Parser) SetSource(source string) {
	p.src = []rune(source)
}

// sliceSource returns the verbatim source span from rune index start to the
// end of the most-recently-consumed token, trimmed of surrounding
// whitespace. Returns "" when no source was recorded (SetSource not called)
// or the span is degenerate -- callers treat "" as "source unavailable".
func (p *Parser) sliceSource(start int) string {
	if p.src == nil || p.pos == 0 {
		return ""
	}
	end := p.tokens[p.pos-1].EndPos
	if start < 0 || end > len(p.src) || end <= start {
		return ""
	}
	return strings.TrimSpace(string(p.src[start:end]))
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
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}

	// Story S3 (#2358): require EOF after the top-level expression parse so
	// trailing garbage (`active == true bogus trailing tokens`) surfaces as an
	// error instead of being silently dropped. This mirrors the tail check the
	// ParseExpression() package function has always had, and is scoped to this
	// method's expression fall-through: every Parse() caller that feeds a full
	// .memql file takes one of the parseFile() branches above and never reaches
	// here. The #2358 caller survey confirmed no caller relies on the dropped
	// tail (the file-oriented callers already reject any non-*File result).
	for p.check(TokenSemicolon) {
		p.advance()
	}
	if !p.check(TokenEOF) {
		return nil, newParseErrorf(&p.current, "unexpected token %q after expression -- a single expression must consume all of its input", p.current.Literal)
	}
	return expr, nil
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
	parser.SetSource(source)
	parser.SetDocComments(lexer.DocComments())
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

// ParseActionDecl tokenises the given source and parses it as a
// single struct-form authored action declaration (memql#2218).
// Convenience wrapper around ParseFile for the action loader's
// per-slice flow. Mirrors ParsePromptDecl on the error surface.
func ParseActionDecl(source string) (*ActionDecl, error) {
	file, err := ParseFile(source)
	if err != nil {
		return nil, err
	}
	if len(file.Definitions) == 0 {
		return nil, fmt.Errorf("no action declaration found")
	}
	if len(file.Definitions) > 1 {
		return nil, fmt.Errorf("expected one action declaration, got %d definitions", len(file.Definitions))
	}
	action, ok := file.Definitions[0].(*ActionDecl)
	if !ok {
		return nil, fmt.Errorf("expected action declaration, got %T", file.Definitions[0])
	}
	return action, nil
}

// ParseCapabilityDecl tokenises the given source and parses it as a
// single top-level `capability` declaration (construct-invocation ADR
// Decision 4, Story 5 / memql#2325). Convenience wrapper around
// ParseFile for the capability loader's per-slice flow. Mirrors
// ParseActionDecl on the error surface.
func ParseCapabilityDecl(source string) (*CapabilityDecl, error) {
	file, err := ParseFile(source)
	if err != nil {
		return nil, err
	}
	if len(file.Definitions) == 0 {
		return nil, fmt.Errorf("no capability declaration found")
	}
	if len(file.Definitions) > 1 {
		return nil, fmt.Errorf("expected one capability declaration, got %d definitions", len(file.Definitions))
	}
	capDecl, ok := file.Definitions[0].(*CapabilityDecl)
	if !ok {
		return nil, fmt.Errorf("expected capability declaration, got %T", file.Definitions[0])
	}
	return capDecl, nil
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
		// parser so the next definition picks it up. Its line span is
		// transparent for doc-comment attachment: the rewriter hoists
		// args{} between a /// block (or its annotations) and the func
		// line (memql#2633).
		if p.check(TokenIdentifier) && p.current.Literal == "args" {
			argsFirstLine := p.current.Line
			argsDef, err := p.parseFileTopArgsBlock()
			if err != nil {
				return nil, err
			}
			p.addTransparentSpan(argsFirstLine, p.current.Line)
			p.pendingArgs = argsDef
			continue
		}
		defFirstLine := p.current.Line
		def, err := p.parseDefinition()
		if err != nil {
			return nil, err
		}
		if def != nil {
			attachDocComment(def, p.takeDocFor(defFirstLine))
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
//	  use common.traits.{ isActiveRecord, isNotDeleted }
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

// topLevelDeclParsers is the authoritative dispatch table for the
// parser's top-level contextual construct keywords: it maps each
// recognised keyword to the per-construct decl parser parseDefinition
// invokes. It is the SINGLE source of "which contextual keyword starts
// a top-level declaration" -- the parseDefinition switch keys off it,
// the unexpected-token error lists its keys, and TopLevelDeclKeywords
// (consumed by the #2124 drift test) is derived from it. Adding a new
// top-level construct means adding exactly one entry here.
//
// `concept` is the schema declaration (annotations.ByReceiver[""]).
// `spec` and `trait` share parseSpecDecl (trait=true); both are listed
// so the keyword set is complete.
var topLevelDeclParsers = map[string]func(p *Parser, attributes []*Attribute) (Node, error){
	"concept":    func(p *Parser, a []*Attribute) (Node, error) { return p.parseConceptDecl(a) },
	"shape":      func(p *Parser, a []*Attribute) (Node, error) { return p.parseShapeDecl(a) },
	"provider":   func(p *Parser, a []*Attribute) (Node, error) { return p.parseProviderDecl(a) },
	"builtin":    func(p *Parser, a []*Attribute) (Node, error) { return p.parseBuiltinDecl(a) },
	"tool":       func(p *Parser, a []*Attribute) (Node, error) { return p.parseToolDecl(a) },
	"prompt":     func(p *Parser, a []*Attribute) (Node, error) { return p.parsePromptDecl(a) },
	"policy":     func(p *Parser, a []*Attribute) (Node, error) { return p.parsePolicyDecl(a) },
	"spec":       func(p *Parser, a []*Attribute) (Node, error) { return p.parseSpecDecl(a, false) },
	"trait":      func(p *Parser, a []*Attribute) (Node, error) { return p.parseSpecDecl(a, true) },
	"seed":       func(p *Parser, a []*Attribute) (Node, error) { return p.parseSeedDecl(a) },
	"action":     func(p *Parser, a []*Attribute) (Node, error) { return p.parseActionDecl(a) },
	"capability": func(p *Parser, a []*Attribute) (Node, error) { return p.parseCapabilityDecl(a) },
}

// TopLevelDeclKeywords is the sorted set of contextual keywords that
// introduce a top-level declaration in parseDefinition (concept /
// shape / provider / builtin / tool / prompt / policy / spec / trait /
// seed). It is derived from topLevelDeclParsers so it cannot drift
// from the actual dispatch, and is the authoritative list the #2124
// drift test compares dslspec's non-function, non-import constructs
// against. It does NOT include `func` (the retired internal procedural
// form, not an author-facing construct) nor `use` (the file-top import
// statement, handled before parseDefinition).
var TopLevelDeclKeywords = func() []string {
	out := make([]string, 0, len(topLevelDeclParsers))
	for kw := range topLevelDeclParsers {
		out = append(out, kw)
	}
	sort.Strings(out)
	return out
}()

// parseDefinition parses a single definition (function).
// Supports @attribute Python-style decorators before func declarations.
func (p *Parser) parseDefinition() (Node, error) {
	// Each top-level construct numbers its forEach loops from zero, so a
	// loop's id depends only on its ORDER within its own construct --
	// not on anything above it in the file (#2659).
	p.forEachOrdinal = 0

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
		argsFirstLine := p.current.Line
		argsDef, err := p.parseFileTopArgsBlock()
		if err != nil {
			return nil, err
		}
		p.addTransparentSpan(argsFirstLine, p.current.Line)
		p.pendingArgs = argsDef
	}

	switch {
	case p.check(TokenKeywordFunc):
		// Go-style: func (Type) name(args) (returns) { }
		def, err = p.parseGoStyleFunction()
	case p.check(TokenIdentifier) && topLevelDeclParsers[p.current.Literal] != nil:
		// Contextual top-level construct keyword (concept / shape /
		// provider / builtin / tool / prompt / policy / spec / trait /
		// seed). The dispatch table topLevelDeclParsers is the single
		// authoritative set of recognised top-level keywords; the
		// #2124 drift test asserts dslspec stays in lockstep with the
		// derived TopLevelDeclKeywords. Each of these stays a plain
		// identifier elsewhere (query bodies, insert payloads,
		// `<expr> with shape(...)`) -- no lexer keyword promotion --
		// so those sites keep working.
		def, err = topLevelDeclParsers[p.current.Literal](p, attributes)
		if err == nil {
			attributes = nil
		}
	default:
		// Story S3 (#2358): the expected-keyword hint now lists the FULL
		// author-facing set -- `func` + every contextual declaration keyword +
		// the rewriter-handled query/mutate/logic/automation family (previously
		// omitted, so a typo'd `query` got a hint list that didn't contain
		// `query`). A Levenshtein did-you-mean points at the nearest keyword
		// (`quer` -> `query`, `conept` -> `concept`).
		hint := topLevelKeywordHintKeywords()
		return nil, newParseErrorf(&p.current, "unexpected token %q, expected a top-level declaration keyword -- one of %s%s",
			p.current.Literal, renderKeywordList(hint), didYouMean(p.current.Literal, hint))
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

	// Parse body based on receiver type. Stash the receiver kind so
	// construct-scoped grammar rules (the query-only `asOf` clause --
	// core-builtins ADR §2.3) can reject misuse while parsing the body.
	prevFuncType := p.currentFuncType
	p.currentFuncType = funcType
	defer func() { p.currentFuncType = prevFuncType }()

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
		// without @enabled -- including chat-reply (generateResponse),
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
		Name:  name,
		Steps: []StepDef{},
		// Enabled-by-default (#2604): absent = enabled, @enabled is an
		// accepted no-op, @disabled is the only off-switch -- the same
		// lifecycle contract FunctionDef adopted in #360. Autonomous
		// execution safety rests on @disabled plus the authored-automation
		// governance stack (kill switch, owner gates, breakers), not on a
		// divergent default.
		Enabled: true,
	}

	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		// ADR Decision 5 (body rule): `body { }` is the procedural marker
		// reserved for `logic`. A struct-form logic has its `body { }`
		// wrapper inlined by the rewriter (emitLogic) before it reaches
		// this shared parser, so a literal `body {` opener arriving here
		// is always a violation -- a non-logic construct (an automation)
		// wrongly wrapping its steps, or a logic with a stray nested body.
		if p.check(TokenIdentifier) && p.current.Literal == "body" && p.peekAhead(1).Type == TokenBraceOpen {
			if p.currentFuncType == FunctionTypeLogic {
				// Defensive: a rewritten logic should never carry a `body {`
				// token here (emitLogic already unwrapped + inlined it). A
				// stray one means a duplicated/nested `body { }` block.
				return nil, newParseErrorf(&p.current, "logic %q has a stray nested `body { }` block -- a logic wraps its procedural code in exactly one `body { }` (ADR Decision 5)", name)
			}
			return nil, newParseErrorf(&p.current, "%s", bodyRuleForbiddenMessage("automation", name))
		}

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
	// A function-call RHS is `foo(...)` OR the kind-prefixed invocation form
	// `query foo(...)` / `mutation foo(...)` / `builtin foo(...)` (Story 3 /
	// #2326): a kind keyword followed by an identifier and `(`. Both parse via
	// parseExpression -> parseIdentifierExpression into a *FunctionCallExpr; the
	// kind keyword is NOT a step type here (an inline `query { ... }` step is
	// `query` followed by `{`, not by an identifier).
	bareCallRHS := p.check(TokenIdentifier) && p.peekAhead(1).Type == TokenParenOpen
	kindPrefixedCallRHS := p.check(TokenIdentifier) && isInvocationKindKeyword(p.current.Literal) &&
		p.peekAhead(1).Type == TokenIdentifier && p.peekAhead(2).Type == TokenParenOpen
	// A `??` chain RHS (#2611 review finding: the assignment position is a
	// THIRD grammar surface) rides the same branch: the cascade folds the
	// chain to a CoalesceExpr and expressionToFunctionCall converts it to
	// the coalesce call step -- byte-identical to the coalesce() spelling.
	coalesceRHS := p.check(TokenIdentifier) && p.peekAhead(1).Type == TokenQuestionQuestion
	if bareCallRHS || kindPrefixedCallRHS || coalesceRHS {
		rhsStart := p.current.Pos
		expr, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		call, ok := expressionToFunctionCall(expr)
		if !ok {
			// Collection-method / lambda chain RHS (#2317):
			//   active := args.members.where(m => m.active)
			// is not a function-call step. Capture the chain's VERBATIM
			// source span and emit a query step carrying it; the
			// LogicRunner's collection-chain branch
			// (tryEvaluateCollectionChainLocally) re-parses that source and
			// evaluates the chain in-memory. We slice the original source
			// rather than reconstruct via expressionToString because the
			// latter emits engine-IR (`arg("members").where(...)`), which
			// does not round-trip an arg-receiver chain. Only a genuine
			// *MethodCallExpr is rescued here; any OTHER non-call expr (a
			// bare literal `x := 5`, a comparison, ...) keeps erroring.
			if chain, isChain := expr.(*MethodCallExpr); isChain {
				if raw := p.sliceSource(rhsStart); raw != "" {
					chain.Raw = raw
					return &StepDef{
						ID:         names[0],
						Type:       StepTypeQuery,
						RetryCount: retryCount,
						Config:     &QueryStepConfig{Query: chain},
					}, nil
				}
			}
			// Arithmetic-expression step RHS whose FIRST operand is a call
			// (#2542 GAP 2): `weeks := daysBetween(a, b) / 7`,
			// `doubled := count() * 2`. The whole RHS parsed to an
			// ArithmeticExpr, not a call, so expressionToFunctionCall declined
			// it -- emit an arithmetic step (same shape as a leading-operand
			// arithmetic RHS below).
			if arith, isArith := expr.(*ArithmeticExpr); isArith {
				return arithmeticStepDef(names[0], retryCount, arith), nil
			}
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

	// Arithmetic-expression step RHS (#2542 GAP 2): an intermediate step whose
	// RHS is an in-memory arithmetic expression -- `delta := a * 2`,
	// `net := gross - fee`, `weeks := span / 7`. This is neither a function
	// call (the bareCallRHS branch above missed it) nor an inline step block
	// (the step-type switch below). Speculatively parse the RHS as an
	// expression and keep it ONLY when the top node is arithmetic; on a parse
	// error or a non-arithmetic node, rewind so the step-type switch reports
	// its usual error for a genuinely unknown RHS. The gate fires only on an
	// arithmetic-leading token, so a step-type keyword (`query {` /
	// `mutation {`) is never speculatively parsed. The emitted step carries
	// the parsed ArithmeticExpr; the compiler serializes the operator form and
	// the LogicRunner re-parses + evaluates it against the local Evaluator
	// (tryEvaluateArithmeticLocally), the exact route a terminal-return
	// arithmetic takes (#2542 item 1) -- so the operand vocabulary is at parity
	// by construction.
	if p.rhsLooksArithmetic() {
		savePos, saveCur := p.pos, p.current
		saveDeferred := len(p.deferredErrors)
		expr, err := p.parseExpression()
		if err == nil {
			if arith, ok := expr.(*ArithmeticExpr); ok {
				return arithmeticStepDef(names[0], retryCount, arith), nil
			}
			// A paren- or number-led `??` chain (#2611): converts to the
			// coalesce call step, same as the identifier-led route above.
			if co, ok := expr.(*CoalesceExpr); ok {
				if call, okc := expressionToFunctionCall(co); okc {
					return &StepDef{
						ID:         names[0],
						Type:       StepTypeFunction,
						RetryCount: retryCount,
						Config:     &FunctionStepConfig{Name: call.Name, Args: call.Args},
					}, nil
				}
			}
		}
		// Not arithmetic (or a parse error): rewind fully so the step-type
		// switch below sees the exact same tokens it would have without the
		// speculative attempt.
		p.pos, p.current = savePos, saveCur
		p.deferredErrors = p.deferredErrors[:saveDeferred]
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

// arithmeticStepDef wraps an arithmetic expression parsed at step-RHS position
// (#2542 GAP 2) as a query-typed step. The compiler serializes the operator
// form (expressionToString's ArithmeticExpr case) and the LogicRunner
// re-parses + evaluates it against the local Evaluator
// (tryEvaluateArithmeticLocally) -- the same node, serializer, and runtime
// evaluator a terminal-return arithmetic uses (#2542 item 1), so the operand
// vocabulary is at parity by construction.
func arithmeticStepDef(name string, retryCount int, arith *ArithmeticExpr) *StepDef {
	return &StepDef{
		ID:         name,
		Type:       StepTypeQuery,
		RetryCount: retryCount,
		Config:     &QueryStepConfig{Query: arith},
	}
}

// rhsLooksArithmetic reports whether the current position begins an arithmetic
// expression at step-RHS position (#2542 GAP 2): a number, a parenthesised
// group, a unary minus, or an identifier immediately followed by a binary
// arithmetic operator. It deliberately excludes an identifier followed by `(`
// (a function call, handled by the bareCallRHS branch) and a step-type keyword
// followed by `{` / `if` (an inline block, handled by the step-type switch), so
// the speculative parse never intercepts a non-arithmetic RHS shape.
func (p *Parser) rhsLooksArithmetic() bool {
	switch {
	case p.check(TokenNumber):
		return true
	case p.check(TokenParenOpen):
		return true
	case p.check(TokenOperator) && p.current.Literal == "-":
		return true
	case p.check(TokenIdentifier):
		next := p.peekAhead(1)
		return next.Type == TokenOperator &&
			(isAdditiveOperatorLiteral(next.Literal) || isMultiplicativeOperatorLiteral(next.Literal))
	default:
		return false
	}
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

	if err := p.validateDeclAnnotations("Shape", "shape", decl.Name, attrs); err != nil {
		return nil, err
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

	if err := p.validateDeclAnnotations("Builtin", "builtin", decl.Name, attrs); err != nil {
		return nil, err
	}

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

	// First-class enum type + required sigil (#2618), mirroring the
	// args-block field grammar: `status enum("a","b")!`.
	if field.Type == "enum" && p.check(TokenParenOpen) {
		values, err := p.parseParenStringList("builtin field " + field.Name)
		if err != nil {
			return nil, err
		}
		field.Type = "string"
		field.Attributes = append(field.Attributes, enumAttributeFromValues(values))
	}
	if p.check(TokenBang) {
		p.advance()
		field.Required = true
	}

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

	if err := p.validateDeclAnnotations("Prompt", "prompt", decl.Name, attrs); err != nil {
		return nil, err
	}

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

	// First-class enum type + required sigil (#2618), mirroring
	// parseBuiltinField.
	if field.Type == "enum" && p.check(TokenParenOpen) {
		values, err := p.parseParenStringList("prompt field " + field.Name)
		if err != nil {
			return nil, err
		}
		field.Type = "string"
		field.Attributes = append(field.Attributes, enumAttributeFromValues(values))
	}
	if p.check(TokenBang) {
		p.advance()
		field.Required = true
	}

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

// parseActionDecl parses the SIMPLIFIED struct-form authored action declaration
// (construct-invocation ADR Decision 3, Story 4 / memql#2328):
//
//	use capabilities.shell.{ script }
//	@description("...")
//	action <name> {
//	  args { <field> <type> [@required] ... }
//	  capability <verb>(<key>: <expr>, ...)
//	}
//
// The body is an `args` block (reusing the file-top args-block grammar) plus a
// SINGLE `capability <verb>(...)` call -- no `body { }`, no `return`. The bare
// verb resolves through the file's `use capabilities.<path>.{ verb }` import to
// a full dotted capability name at load time (component/actions). The legacy
// action surfaces are REJECTED with migration-pointing errors: the annotations
// `@kind` / `@sideEffect` / `@reliability` (captured into attrs by
// parseDefinition) and the body keys `capability "string"` / `intent` /
// `params` / `argTemplate`.
func (p *Parser) parseActionDecl(attrs []*Attribute) (*ActionDecl, error) {
	if !p.check(TokenIdentifier) || p.current.Literal != "action" {
		return nil, newParseErrorf(&p.current, "expected 'action' keyword, got %q", p.current.Literal)
	}
	p.advance()

	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected action name after 'action', got %q", p.current.Literal)
	}
	decl := &ActionDecl{Name: p.current.Literal, Attributes: attrs}
	p.advance()

	// Reject the retired action-level annotations (ADR Decision 3 table).
	for _, attr := range attrs {
		if attr == nil {
			continue
		}
		switch attr.Name {
		case "kind":
			return nil, newParseErrorf(&p.current,
				"action %q: @kind is retired (construct-invocation ADR Decision 3) -- composites are automations now and primitives need no marker; remove it", decl.Name)
		case "sideEffect":
			return nil, newParseErrorf(&p.current,
				"action %q: @sideEffect is retired on actions (ADR Decision 3) -- the authoritative side-effect class lives on the CAPABILITY declaration now (Story 5); remove it from the action", decl.Name)
		case "reliability":
			return nil, newParseErrorf(&p.current,
				"action %q: @reliability is retired (ADR Decision 3) -- reliability is machine-managed runtime state, not source; remove it", decl.Name)
		}
	}

	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}

	sawArgs := false
	sawCapability := false

	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		if !p.check(TokenIdentifier) {
			return nil, newParseErrorf(&p.current,
				"unexpected token %q in action %q body -- expected an `args { ... }` block and a single `capability <verb>(...)` call", p.current.Literal, decl.Name)
		}
		key := p.current.Literal

		switch key {
		case "args":
			if sawArgs {
				return nil, newParseErrorf(&p.current, "action %q declares 'args' more than once", decl.Name)
			}
			// parseFileTopArgsBlock consumes the `args` keyword + block.
			argsDef, err := p.parseFileTopArgsBlock()
			if err != nil {
				return nil, err
			}
			decl.Args = argsDef
			sawArgs = true
		case "capability":
			if sawCapability {
				return nil, newParseErrorf(&p.current,
					"action %q declares more than one capability -- an action performs exactly one external capability (ADR Decision 3)", decl.Name)
			}
			// Legacy form `capability "<verb>"` (a string after the keyword) is retired.
			if p.peekAhead(1).Type == TokenString {
				return nil, newParseErrorf(&p.current,
					"action %q: the `capability \"<verb>\"` + `argTemplate` form is retired (ADR Decision 3) -- write the body as a single typed call `capability <verb>(<arg>: args.<x>, ...)` and import the verb via `use capabilities.<path>.{ <verb> }`", decl.Name)
			}
			// New form: parse `capability <verb>(<args>)` as a kind-prefixed call expression.
			expr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			fc, ok := expr.(*FunctionCallExpr)
			if !ok || fc.Kind != "capability" {
				return nil, newParseErrorf(&p.current,
					"action %q: the body must be a single `capability <verb>(...)` call", decl.Name)
			}
			decl.Capability = fc.Name
			decl.CallArgs = lowerActionCallArgs(fc.Args)
			sawCapability = true
		case "intent":
			return nil, newParseErrorf(&p.current,
				"action %q: `intent` is retired (ADR Decision 3) -- its content merges into `@description(...)`, the planner's retrieval embedding source", decl.Name)
		case "params":
			return nil, newParseErrorf(&p.current,
				"action %q: `params` is renamed to `args` (ADR Decision 3) -- use an `args { ... }` block", decl.Name)
		case "argTemplate":
			return nil, newParseErrorf(&p.current,
				"action %q: `argTemplate` (and `$params.X` templating) is retired (ADR Decision 3) -- pass the capability's arguments directly in the `capability <verb>(<arg>: args.<x>, ...)` call", decl.Name)
		case "body":
			return nil, newParseErrorf(&p.current,
				"action %q: an action has NO `body { }` (ADR Decision 5) -- its body is exactly the single `capability <verb>(...)` call", decl.Name)
		default:
			return nil, newParseErrorf(&p.current,
				"unknown key %q in action %q body -- expected an `args { ... }` block and a single `capability <verb>(...)` call", key, decl.Name)
		}
	}

	if err := p.expect(TokenBraceClose); err != nil {
		return nil, err
	}

	if !sawCapability {
		return nil, newParseErrorf(&p.current,
			"action %q is missing its `capability <verb>(...)` call -- an action body is exactly one external capability call (ADR Decision 3)", decl.Name)
	}
	return decl, nil
}

// lowerActionCallArgs converts a parsed capability-call argument map (key ->
// value-expression) into the ordered ActionCallArg slice the AST carries. The
// FunctionCallExpr arg map is unordered, so the slice is sorted by key for a
// deterministic result (the runtime renders into a map, so order is cosmetic).
func lowerActionCallArgs(args map[string]any) []*ActionCallArg {
	if len(args) == 0 {
		return nil
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*ActionCallArg, 0, len(keys))
	for _, k := range keys {
		ca := &ActionCallArg{Key: k}
		// parseValue yields AST nodes for references (args.X -> *ArgRefExpr) but
		// RAW Go values for literals (a string / int64 / bool). Wrap the latter
		// in a LiteralExpr so the AST carries a uniform ExpressionNode.
		if expr, ok := args[k].(ExpressionNode); ok {
			ca.Value = expr
		} else {
			ca.Value = &LiteralExpr{Value: args[k]}
		}
		out = append(out, ca)
	}
	return out
}

// parseCapabilityDecl parses a top-level `capability` declaration
// (construct-invocation ADR Decision 4, Story 5 / memql#2325):
//
//	@sideEffect("write")
//	@description("Create a git tag + GitHub release for a version.")
//	capability integration.github.tagRelease {
//	  args { repo string @required; tag string @required }
//	}
//
// A capability is declared like a typed, side-effect-classified builtin
// with NO body (surface-backed). Its name is namespaced/dotted (the
// lexer emits the dotted path as one identifier token). The optional
// `args { ... }` block reuses the file-top args-block grammar. A `body`
// block (or any non-`args` key) is rejected: a capability is surface-
// backed and never procedural. Namespace-vocabulary validation
// (fs/shell/http/integration/mcp) is a loader concern, not the parser's
// -- the parser stays purely syntactic and only requires a dotted name.
//
// Additive only (this story): the invocation grammar (`capability
// NAME(args)` call sites) and the action rewrite land in later stories.
func (p *Parser) parseCapabilityDecl(attrs []*Attribute) (*CapabilityDecl, error) {
	if !p.check(TokenIdentifier) || p.current.Literal != "capability" {
		return nil, newParseErrorf(&p.current, "expected 'capability' keyword, got %q", p.current.Literal)
	}
	p.advance()

	if !p.check(TokenIdentifier) {
		return nil, newParseErrorf(&p.current, "expected capability name after 'capability', got %q", p.current.Literal)
	}
	name := p.current.Literal
	if !strings.Contains(name, ".") {
		return nil, newParseErrorf(&p.current,
			"capability name %q must be namespaced/dotted (fs.* / shell.* / http.* / integration.* / mcp.*), e.g. `integration.github.tagRelease`", name)
	}
	decl := &CapabilityDecl{Name: name, Attributes: attrs}
	p.advance()

	if err := p.expect(TokenBraceOpen); err != nil {
		return nil, err
	}

	sawArgs := false
	for !p.check(TokenBraceClose) && !p.check(TokenEOF) {
		if !p.check(TokenIdentifier) {
			return nil, newParseErrorf(&p.current,
				"unexpected token %q in capability %q body -- only an `args { ... }` block is allowed (a capability is surface-backed and has NO body)", p.current.Literal, decl.Name)
		}
		switch p.current.Literal {
		case "args":
			if sawArgs {
				return nil, newParseErrorf(&p.current, "capability %q declares 'args' more than once", decl.Name)
			}
			// parseFileTopArgsBlock consumes the `args` keyword + block.
			argsDef, err := p.parseFileTopArgsBlock()
			if err != nil {
				return nil, err
			}
			decl.Args = argsDef
			sawArgs = true
		case "body":
			return nil, newParseErrorf(&p.current,
				"capability %q must not declare a `body` block -- a capability is surface-backed (the body is supplied by the runtime surface, not the DSL)", decl.Name)
		default:
			return nil, newParseErrorf(&p.current,
				"unknown key %q in capability %q body -- only an `args { ... }` block is allowed", p.current.Literal, decl.Name)
		}
	}

	if err := p.expect(TokenBraceClose); err != nil {
		return nil, err
	}
	return decl, nil
}

// parseActionParamsBlock parses the `params { <field> <type> [@ann ...] ... }`
// block of an action. Per-field grammar mirrors parsePromptField /
// parseBuiltinField (the shared struct-form field surface).
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

	// Required sigil (#2618): `name string!` means @required. The
	// synthesized attribute is identical to a parsed @required, and a
	// sigil beside an explicit @required stays idempotent (single
	// attribute, deduped after the trailing-annotation loop).
	sigilRequired := false
	if p.check(TokenBang) {
		p.advance()
		sigilRequired = true
	}

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
			if attr.Name == "required" && sigilRequired {
				continue // sigil already carries it (#2618)
			}
			prop.Attributes = append(prop.Attributes, attr)
		}
	}

	if sigilRequired {
		prop.Attributes = append(prop.Attributes, &Attribute{Name: "required", Args: map[string]any{}})
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
	// Date/duration builtins (#2541) -- admitted at step-RHS position so a
	// logic body can bind one as a step value (`delta := daysBetween(args.a,
	// args.b)`). The logic runner evaluates the reconstructed positional
	// call locally (tryEvaluateBuiltinLocally), the same route coalesce
	// takes.
	case *AddDurationExpr:
		return &FunctionCallExpr{Name: "addDuration", Args: map[string]any{
			"0": t.Timestamp, "1": t.Duration,
		}}, true
	case *DaysBetweenExpr:
		return &FunctionCallExpr{Name: "daysBetween", Args: map[string]any{
			"0": t.Date1, "1": t.Date2,
		}}, true
	case *SubtractTimestampsExpr:
		return &FunctionCallExpr{Name: "subtractTimestamps", Args: map[string]any{
			"0": t.T1, "1": t.T2,
		}}, true
	case *YearExpr:
		return &FunctionCallExpr{Name: "year", Args: map[string]any{"0": t.Target}}, true
	case *QuarterExpr:
		return &FunctionCallExpr{Name: "quarter", Args: map[string]any{"0": t.Target}}, true
	case *MonthExpr:
		return &FunctionCallExpr{Name: "month", Args: map[string]any{"0": t.Target}}, true
	case *DayOfMonthExpr:
		return &FunctionCallExpr{Name: "dayOfMonth", Args: map[string]any{"0": t.Target}}, true
	case *IsAnniversaryExpr:
		return &FunctionCallExpr{Name: "isAnniversary", Args: map[string]any{
			"0": t.StartDate, "1": t.CheckDate,
		}}, true
	case *IsFirstDayOfQuarterExpr:
		return &FunctionCallExpr{Name: "isFirstDayOfQuarter", Args: map[string]any{"0": t.Target}}, true
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
			lit := p.current.Literal
			// Token literals are UNQUOTED content; re-quote strings so the
			// reconstructed filter keeps `x == "development"` intact --
			// otherwise the literal leaks as a bare identifier (flagged by
			// the G2 checker and ambiguous for the evaluator). #2367.
			if p.check(TokenString) {
				lit = strconv.Quote(lit)
			}
			filterParts = append(filterParts, lit)
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

	// Synthetic forEach step id (#2659): the loop's VALUE VARIABLE plus
	// its ordinal within the enclosing construct. Both inputs are stable
	// under edits elsewhere in the file, which the previous
	// `forEach_<var>_<charOffset>_L<line>` form was not: any edit that
	// changed the character count above a loop renamed its step, and
	// automation checkpoints key persisted step results by id, so a
	// comment reflow could silently break resume for an in-flight run.
	// The ordinal also keeps two same-named loops in one construct
	// distinct, which the offset previously provided.
	stepId := fmt.Sprintf("forEach_%s_%d", valueVar, p.forEachOrdinal)
	p.forEachOrdinal++
	_, _ = loopStartPos, loopStartLine

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
//	  for item := range collection.nodes() { ... }
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
//   - `for item := range collection.nodes() { ... }` for-range steps
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
// attrNumericString renders a positional numeric attribute value
// (@cache(300)) as its decimal string, or "" when the value is not
// numeric.
func attrNumericString(attr *Attribute) string {
	switch v := attr.Value.(type) {
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

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
			} else if v := getAttrString(attr); v != "" {
				d.CacheTTL = v
			} else if v := attrNumericString(attr); v != "" {
				// Positional numeric form (#2618): @cache(300). The
				// registry's single ttl arg makes position unambiguous.
				d.CacheTTL = v
			}
		case AttrNocache:
			// @nocache is the clearer opt-out alias for @cache(ttl="0")
			// (5.6 / memql#1970): force "never cache", overriding default-on.
			d.CacheTTL = "0"
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
	left, err := p.parseLogicalAnd()
	if err != nil {
		return nil, err
	}
	if left == nil {
		return nil, nil
	}

	for (!p.suppressCommaOr && p.check(TokenComma)) || p.check(TokenPipePipe) {
		p.advance()
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

// parseNullCoalesce parses the resurrected `??` null-coalescing level
// (#2611): `a ?? b ?? c` folds n-ary into ONE CoalesceExpr -- identical
// downstream to the coalesce(a, b, c) spelling, preserving the memql#1614
// final-arg-fallback semantics and matching the object-literal fold. The
// precedence is deliberately Swift-TIGHT -- tighter than comparison,
// looser than additive -- so the dominant fallback-then-compare idiom
// `args.stage ?? "" == "active"` means `(args.stage ?? "") == "active"`.
// (The retired Phase-4 slot sat between || and &&, the JS/C# binding,
// under which a non-nil LHS silently short-circuits the comparison: the
// silent-constant-gate class wave-3 #2542 eliminated. Zero source carried
// `??` at resurrection time, so the precedence change migrates nothing.)
func (p *Parser) parseNullCoalesce() (ExpressionNode, error) {
	left, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	if left == nil {
		return nil, nil
	}
	if !p.check(TokenQuestionQuestion) {
		return left, nil
	}
	args := []ExpressionNode{left}
	for p.check(TokenQuestionQuestion) {
		p.advance()
		// Continuation operands parse with the identifier-led comparison
		// fold suppressed: `stage ?? def == "active"` must leave the
		// comparison for the level above, not swallow it into the arm.
		prevFold := p.suppressComparisonFold
		p.suppressComparisonFold = true
		next, err := p.parseAdditive()
		p.suppressComparisonFold = prevFold
		if err != nil {
			return nil, err
		}
		if next == nil {
			return nil, newParseErrorf(&p.current, "expected an expression after '??'")
		}
		args = append(args, next)
	}
	return &CoalesceExpr{Args: args}, nil
}

// parseLogicalAnd parses semicolon-separated or &&-separated (AND) expressions.
func (p *Parser) parseLogicalAnd() (ExpressionNode, error) {
	left, err := p.parseComparisonLevel()
	if err != nil {
		return nil, err
	}
	if left == nil {
		return nil, nil
	}

	for p.check(TokenSemicolon) || p.check(TokenAmpAmp) {
		p.advance()
		right, err := p.parseComparisonLevel()
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

// parseComparisonLevel parses an EXPRESSION-LED comparison (#2542 item 5
// residual): a comparison whose left side is a full arithmetic / primary
// expression rather than a bare identifier. It sits between the logical
// (&&/||) levels and the additive arithmetic level, so `r.count - 10 > 0`
// parses as `(r.count - 10) > 0` and `0 < r.count` parses literal-led.
//
// Identifier-led comparisons (`m.count > 10`) are still consumed one level
// down, in parseIdentifierExpression, which produces the Field-led
// ComparisonExpr and leaves NO trailing operator; this level is therefore a
// no-op passthrough for them and their AST shape is byte-identical to before.
// Only a comparison whose LHS is NON-identifier-led -- arithmetic, a literal,
// a parenthesised group, or a call result -- reaches here with the operator
// still pending, and becomes a BinaryComparisonExpr. Every such form
// previously failed to parse (the operator was left unconsumed and the
// enclosing arg list / statement rejected it), so this is purely additive.
//
// Non-associative: at most one comparison operator is consumed (`a < b < c`
// is not a chained comparison), matching Go and the additive/multiplicative
// levels' left operand.
func (p *Parser) parseComparisonLevel() (ExpressionNode, error) {
	left, err := p.parseNullCoalesce()
	if err != nil {
		return nil, err
	}
	if left == nil {
		return nil, nil
	}
	if p.check(TokenOperator) && isComparisonOperatorLiteral(p.current.Literal) {
		op := ComparisonOperator(p.current.Literal)
		p.advance()
		right, err := p.parseNullCoalesce()
		if err != nil {
			return nil, err
		}
		if right == nil {
			return nil, newParseErrorf(&p.current, "expected an expression after %q", string(op))
		}
		return &BinaryComparisonExpr{Left: left, Operator: op, Right: right}, nil
	}
	return left, nil
}

// isComparisonOperatorLiteral reports whether a TokenOperator literal is one
// of the six comparison operators. Used to keep the arithmetic operators
// (`+ - * / %`, #2316) -- which the lexer now also emits as TokenOperator --
// from being misread as comparisons inside parseIdentifierExpression.
func isComparisonOperatorLiteral(lit string) bool {
	switch lit {
	case "==", "!=", "<", "<=", ">", ">=":
		return true
	}
	return false
}

// isAdditiveOperatorLiteral reports the `+` / `-` additive operators (#2316).
func isAdditiveOperatorLiteral(lit string) bool {
	return lit == "+" || lit == "-"
}

// isMultiplicativeOperatorLiteral reports the `* / %` operators (#2316).
func isMultiplicativeOperatorLiteral(lit string) bool {
	return lit == "*" || lit == "/" || lit == "%"
}

// checkArgTerminatingOperator reports whether a pending operator should
// terminate an args./ctx. reference's ArgRef early-return. Under the
// ??-operand fold suppression (#2611 round 3, finding C) a pending
// COMPARISON operator belongs to the level above -- the reference is a bare
// coalesce arm and must still resolve as an ArgRef, or the final arm of
// `args.a ?? args.def == "x"` silently degrades to a payload path.
func (p *Parser) checkArgTerminatingOperator() bool {
	if !p.check(TokenOperator) {
		return false
	}
	if p.suppressComparisonFold && isComparisonOperatorLiteral(p.current.Literal) {
		return false
	}
	return true
}

// atArithmeticOperator reports whether the current token is one of the
// binary arithmetic operators (#2316).
func (p *Parser) atArithmeticOperator() bool {
	if !p.check(TokenOperator) {
		return false
	}
	return isAdditiveOperatorLiteral(p.current.Literal) || isMultiplicativeOperatorLiteral(p.current.Literal)
}

// parseAdditive parses `+` / `-` binary arithmetic (#2316). It sits just
// below the &&/|| logical levels and above multiplication, so `* / %` bind
// tighter. Left-associative: `a - b - c` parses as `(a - b) - c`.
func (p *Parser) parseAdditive() (ExpressionNode, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}
	if left == nil {
		return nil, nil
	}
	for p.check(TokenOperator) && isAdditiveOperatorLiteral(p.current.Literal) {
		op := p.current.Literal
		p.advance()
		right, err := p.parseMultiplicative()
		if err != nil {
			return nil, err
		}
		if right == nil {
			return nil, newParseErrorf(&p.current, "expected an expression after %q", op)
		}
		left = &ArithmeticExpr{Op: op, Left: left, Right: right}
	}
	return left, nil
}

// parseMultiplicative parses `* / %` binary arithmetic (#2316). Tighter than
// additive, looser than unary minus / primaries. Left-associative.
func (p *Parser) parseMultiplicative() (ExpressionNode, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	if left == nil {
		return nil, nil
	}
	for p.check(TokenOperator) && isMultiplicativeOperatorLiteral(p.current.Literal) {
		op := p.current.Literal
		p.advance()
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		if right == nil {
			return nil, newParseErrorf(&p.current, "expected an expression after %q", op)
		}
		left = &ArithmeticExpr{Op: op, Left: left, Right: right}
	}
	return left, nil
}

// parseUnary folds a leading unary minus on a primary into `0 - <primary>`
// (#2316), reusing the binary ArithmeticExpr machinery so no separate unary
// node is needed. A `-` glued to a digit (`-5`) is already a numeric literal
// from the lexer and never reaches here. Recurses to allow `- - x`.
func (p *Parser) parseUnary() (ExpressionNode, error) {
	if p.check(TokenOperator) && p.current.Literal == "-" {
		p.advance()
		operand, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		if operand == nil {
			return nil, newParseErrorf(&p.current, "expected an expression after unary '-'")
		}
		return &ArithmeticExpr{Op: "-", Left: &LiteralExpr{Value: int64(0)}, Right: operand}, nil
	}
	return p.parsePrimary()
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
	// A parenthesized group is a fresh expression context: the ??-operand
	// fold suppression (#2611) must not leak in, or `a ?? (b == "x")`
	// mis-shapes the inner comparison (review round 2, finding A).
	prevFold := p.suppressComparisonFold
	p.suppressComparisonFold = false
	defer func() { p.suppressComparisonFold = prevFold }()

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
// when the collection is a row collection field -- either the legacy explicit
// `payload.<field>` form or, since the bare-payload rewrite (#2294), a bare
// row field (e.g. `args.tag in tags`). It consumes the token and returns the
// field path. A list-literal RHS (`kind in ["a", "b"]`) is not a
// TokenIdentifier, so it falls through to parseValue (the OpIn scalar-in-list
// form). A dotted scalar accessor RHS (`args.`/`ctx.`/`actor.`/`config.`) is a
// scalar, never a collection, so it also falls through. #976 / #2342.
func membershipCollectionField(p *Parser) (string, bool) {
	if !p.check(TokenIdentifier) {
		return "", false
	}
	lit := p.current.Literal
	for _, scalarPrefix := range []string{"args.", "ctx.", "actor.", "config."} {
		if strings.HasPrefix(lit, scalarPrefix) {
			return "", false
		}
	}
	p.advance()
	return lit, true
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

// invocationKindKeywords is the set of construct-kind prefixes recognised in
// the kind-prefixed invocation form `<kind> <name>(args)` (Story 2 / #2324).
// These all take parens + args; the predicate kinds (spec / trait) are handled
// separately. Language primitives (coalesce / where / select / if / for /
// return / concat / addDuration / date extractors / ...) are deliberately
// absent — they stay bare and never carry a kind prefix.
var invocationKindKeywords = map[string]bool{
	"logic":      true,
	"query":      true,
	"mutation":   true,
	"action":     true,
	"capability": true,
	"builtin":    true,
	"automation": true,
}

// isInvocationKindKeyword reports whether name is a construct-kind prefix that
// introduces a kind-prefixed call (`mutation createNode(...)`).
func isInvocationKindKeyword(name string) bool {
	return invocationKindKeywords[name]
}

// InvocationKindKeywords returns the invocation-kind prefix set (sorted) for
// consumers outside the parser -- notably Sense tokenize, which colors these
// prefixes as keywords (E6, memql#2392). Deriving from the same map keeps the
// two surfaces drift-proof by construction.
func InvocationKindKeywords() []string {
	out := make([]string, 0, len(invocationKindKeywords))
	for k := range invocationKindKeywords {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseIdentifierExpression parses function calls, field references, or comparisons.
func (p *Parser) parseIdentifierExpression() (ExpressionNode, error) {
	name := p.current.Literal
	nameTok := p.current // retained for a precise error position on the leading word
	p.advance()

	// Story 2 (#2324): kind-prefixed construct invocation `<kind> <name>(args)`.
	// When a kind keyword is immediately followed by `identifier (` we consume
	// the prefix and parse the inner call exactly as before, tagging the
	// resulting FunctionCallExpr with the kind. This is ADDITIVE and
	// backward-compatible: a bare `createNode(...)` (no prefix) never matches
	// (the kind word itself would have to be followed by another identifier and
	// a paren), and language primitives are not in the kind set, so they stay
	// bare. The old-form rejection lands later with the tree migration
	// (Story 3 / #2326); here both forms parse.
	if isInvocationKindKeyword(name) && p.check(TokenIdentifier) && p.peekAhead(1).Type == TokenParenOpen {
		innerName := p.current.Literal
		p.advance() // consume the construct name; p.current is now '('
		call, err := p.parseFunctionCallWithKind(innerName, name)
		if err != nil {
			return nil, err
		}
		if fc, ok := call.(*FunctionCallExpr); ok {
			fc.Kind = name
		}
		// Mirror the bare-call path: accept post-call dotted access.
		return p.consumePostCallDotAccess(call)
	}

	// Story 2 (#2324): spec/trait predicate form `spec <name>` / `trait <name>`
	// — the kind-prefixed replacement for the stringly `spec("name")` call.
	// Produces a SpecReferenceExpr (Trait flag set for the trait form). The
	// trigger requires `spec`/`trait` immediately followed by an identifier that
	// is NOT itself a call (`spec foo(` is not this form). Bare predicate refs
	// in filters (`filter active && isActiveRecord`) already produce a
	// SpecReferenceExpr and are unaffected; the legacy `spec("name")` call form
	// still routes through the paren path below (rejection deferred to Story 3).
	if (name == "spec" || name == "trait") && p.check(TokenIdentifier) && p.peekAhead(1).Type != TokenParenOpen {
		predName := p.current.Literal
		p.advance() // consume the predicate name
		return &SpecReferenceExpr{Name: predName, Trait: name == "trait"}, nil
	}

	// Story S3 (#2358): a bare `<ident> <ident>(` whose LEADING identifier is
	// NOT a known invocation kind is a mistyped kind-prefixed call -- the
	// classic `mutate` (mutation *declaration* verb) written in *call*
	// position where the *invocation* noun `mutation` belongs. Historically
	// the leading word lowered to a bare SpecReferenceExpr and the entire
	// `<ident>(...)` call was silently DROPPED (`mutate createNode(id:"x")`
	// parsed as SpecReferenceExpr{Name:"mutate"}, err=nil). Reject it with a
	// Levenshtein nearest-kind hint so a typo can no longer become a silent
	// semantic change.
	//
	// Delimitation -- this fires ONLY on the exact `identifier identifier (`
	// shape and cannot swallow any valid form:
	//   - a real invocation kind (`mutation createNode(`) already returned above;
	//   - the `spec <name>` / `trait <name>` predicate form has NO following
	//     paren (peekAhead(1) != '('), so it returned above / falls through;
	//   - top-level declaration headers use braces, not parens
	//     (`query participant qFoo {` -- peekAhead(1) is an identifier, not '(');
	//   - a bare call (`createNode(`) has '(' immediately after the first word,
	//     so p.current is '(' here, not an identifier;
	//   - collection lambdas / dotted calls (`args.members.where(`) arrive as a
	//     single fused identifier followed by '(', never ident-ident.
	if p.check(TokenIdentifier) && p.peekAhead(1).Type == TokenParenOpen {
		return nil, newParseErrorf(&nameTok,
			"%q is not a construct-invocation kind, so the call %s(...) would be silently dropped -- a kind-prefixed call must lead with one of %s%s",
			name, p.current.Literal, renderKeywordList(invocationKindKeywordList()), didYouMean(name, kindSuggestionCandidates()))
	}

	// Check for function call
	if p.check(TokenParenOpen) {
		call, err := p.parseFunctionCall(name)
		if err != nil {
			return nil, err
		}
		// Phase 6: accept post-call dotted access like `.first().payload.id`.
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
		if argPath != "" && !p.checkArgTerminatingOperator() && !p.check(TokenKeywordIn) && !p.check(TokenKeywordHas) && !p.check(TokenKeywordNot) {
			return &ArgRefExpr{Path: argPath}, nil
		}
		// `args.X` immediately followed by an arithmetic operator (#2316) is
		// an arithmetic operand, not a comparison LHS -- return the ArgRefExpr
		// and let parseAdditive / parseMultiplicative consume the operator.
		if argPath != "" && p.check(TokenOperator) && p.atArithmeticOperator() {
			return &ArgRefExpr{Path: argPath}, nil
		}
	}
	if strings.HasPrefix(name, "ctx.") {
		argPath := strings.TrimPrefix(name, "ctx.")
		if argPath != "" && p.check(TokenOperator) && p.atArithmeticOperator() {
			switch argPath {
			case "input", "output", "actor", "partition", "now", "config", "error", "trace":
			default:
				return &ArgRefExpr{Path: argPath}, nil
			}
		}
		if argPath != "" && !p.checkArgTerminatingOperator() && !p.check(TokenKeywordIn) && !p.check(TokenKeywordHas) && !p.check(TokenKeywordNot) {
			switch argPath {
			case "input", "output", "actor", "partition", "now", "config", "error", "trace":
				// Reserved envelope fields -- leave for downstream
				// resolution paths instead of treating as caller args.
			default:
				return &ArgRefExpr{Path: argPath}, nil
			}
		}
	}

	// Check for comparison operator (symbol-based). Only the six comparison
	// operators are consumed here; an arithmetic operator (`+ - * / %`, #2316)
	// is left for the additive/multiplicative levels above parsePrimary, so a
	// bare reference like `m.a` followed by `+` falls through to the
	// SpecReferenceExpr return below.
	if !p.suppressComparisonFold && p.check(TokenOperator) && isComparisonOperatorLiteral(p.current.Literal) {
		op := ComparisonOperator(p.current.Literal)
		p.advance()
		valuePos, valueCur := p.pos, p.current

		var value any
		var err error
		value, err = p.parseValue()
		if err != nil {
			return nil, err
		}

		// Swift-tight `??` (#2611): a pending coalesce binds tighter than
		// the comparison, so `a == b ?? c` compares a against
		// COALESCE(b, c). Re-parse the VALUE-led chain through the same
		// operand parser the coalesce() call form's args use, and store
		// the fold as the comparison value -- the identical node the
		// `a == coalesce(b, c)` baseline produces (review round 2,
		// finding B).
		if p.check(TokenQuestionQuestion) {
			p.pos, p.current = valuePos, valueCur
			chain, cerr := p.parseCoalesceArgOperand()
			if cerr != nil {
				return nil, cerr
			}
			return &ComparisonExpr{
				Field: FieldReference{
					Raw:   name,
					Parts: strings.Split(name, "."),
				},
				Operator: op,
				Value:    chain,
			}, nil
		} else {
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

	// Bare reserved `now` is the canonical current-timestamp primitive
	// (epic #2298 / #2301): emit the TimestampExprFunc node so it resolves
	// to the clock in every expression position (logic / automation operands,
	// function arguments like `addDuration(now, ...)`), identically to the
	// retired now() / timestamp() call-forms.
	if name == "now" {
		return &TimestampExprFunc{}, nil
	}

	// Just an identifier (field reference or spec reference)
	return &SpecReferenceExpr{Name: name}, nil
}

// parseFunctionCall parses a function call: name(args).
func (p *Parser) parseFunctionCall(name string) (ExpressionNode, error) {
	return p.parseFunctionCallWithKind(name, "")
}

// parseFunctionCallWithKind is parseFunctionCall with the invocation kind
// prefix threaded through (empty for bare calls). On KIND-PREFIXED construct
// calls two G3/#2365 (+ S8 hole 2, #2395) rules apply inside the arg loop:
//
//   - PUNNING (ADR event-payload-binding Decision 5): a bare simple
//     identifier in argument position is sugar for `name: name` --
//     `action f(environment)` == `action f(environment: environment)`.
//     The punned value is produced by the same parseValue path as the
//     explicit named form, so downstream resolution is identical.
//   - POSITIONAL REJECTION: any other positional argument (literal, dotted
//     path, expression) on a construct call is an error -- construct calls
//     are named-args-only (#2322); positionals were previously accepted
//     silently as keys "0"/"1". Primitive builtins (bare calls, kind == "")
//     legitimately take positional args and are untouched. A lone
//     object-literal still routes to the Story 9 wrapper-specific error.
func (p *Parser) parseFunctionCallWithKind(name, kind string) (ExpressionNode, error) {
	p.advance() // consume '('

	// Call arguments are a fresh expression context: the ??-operand fold
	// suppression (#2611) must not leak in, or `x ?? cond(a == "b", ...)`
	// parses its predicate expression-led instead of the Field-led
	// ComparisonExpr the identifier fold produces (review round 2,
	// finding A).
	prevFold := p.suppressComparisonFold
	p.suppressComparisonFold = false
	defer func() { p.suppressComparisonFold = prevFold }()

	lname := strings.ToLower(name)

	// Story 4 (#2302 / ADR §2.2): a fused dotted path whose final segment
	// is a collection operator (`args.members.where`) is a collection-method
	// call, not a generic function call. Intercept before the table-driven
	// dispatch so the receiver path + lambda args parse correctly. The
	// engine converter enforces scope (logic/forEach only).
	if recvPath, method, ok := isCollectionMethodPath(name); ok {
		return p.parseCollectionMethodCall(recvPath, method)
	}

	// index() with no args = forEach index accessor; index(arr, i) = array element (general parsing)
	if lname == "index" && p.check(TokenParenClose) {
		return p.parseIndexAccessor()
	}

	// shape() has two forms: shape(expr, template) and shape({ source,
	// template }). When the first token is '{' it is the object form, which
	// falls through to the generic function-call path below. Otherwise it is
	// the expression-first form parsed by parseShapeFunction. This fork is
	// kept inline (rather than in the callableParsers dispatch) because the
	// object form must NOT dispatch through the table.
	if lname == "shape" {
		if p.check(TokenBraceOpen) {
			// Object form -- fall through to the generic arg parser below.
		} else {
			return p.parseShapeFunction()
		}
	} else if entry, ok := callableParsers()[lname]; ok {
		// Table-driven dispatch (callable.go) is the single source for every
		// recognised bare-function name: accessors (item / event / step /
		// input / field / actor / timestamp / now / error / var), the retired
		// caller(), the editor-callable expression builtins (concat / coalesce
		// / hash / year / contains / ...), the keyword-functions (case /
		// default), and the query directives (paginate / sort / select / asOf
		// / withDepth / count). The exported parser.CallableBuiltins is derived
		// from the CallableBuiltin-kind entries; dslspec models their metadata
		// and a drift test pins the two together.
		return entry.parse(p)
	}

	// Relationship wrapper functions take a single inner expression and
	// produce a *RelationshipExpr (createWrapper dispatches by name).
	// The dedicated directive parse functions (paginate / sort / select /
	// asOf / withDepth / shape) live in callableParsers and produce their
	// specialised AST types directly; they are NOT in this map.
	// Modern single-paren `contains(filter)` is handled inside
	// parseContainsFunction (arg-count discriminates relationship vs
	// 2-arg string search), so contains is a callableParsers builtin.
	wrapperFunctions := map[string]bool{
		"parentof": true, "childof": true, "aliasof": true,
		"equals": true, "interactswith": true,
		"owns": true, "createdby": true, "ids": true,
	}

	if wrapperFunctions[lname] {
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
		} else if p.check(TokenIdentifier) && p.peekAhead(1).Type == TokenColon {
			// Story 2 (#2324): colon-named argument directly in the parens
			// (`mutation createNode(id: args.node.id, nodeType: args.node.type)`)
			// — the kind-prefixed replacement for the object-literal wrapper.
			// Additive: applies uniformly to kind-prefixed and bare calls, and
			// the `=` named form + object-literal positional form above/below
			// are untouched. Nested object values still parse via parseValue
			// (`payload: { ... }`).
			argName := p.current.Literal
			p.advance() // consume name
			p.advance() // consume ':'
			val, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			args[argName] = val
		} else if kind != "" && p.check(TokenIdentifier) && !strings.Contains(p.current.Literal, ".") &&
			(p.peekAhead(1).Type == TokenComma || p.peekAhead(1).Type == TokenParenClose) {
			// G3 (#2365): punning -- bare simple identifier == `name: name`.
			// Dotted paths (args.workdir) do not pun (no single name); they
			// fall through to the positional rejection below with a hint.
			argName := p.current.Literal
			val, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			args[argName] = val
		} else {
			if kind != "" && !p.check(TokenBraceOpen) {
				// S8 hole 2 (#2395): positional args on a construct call were
				// silently accepted as keys "0"/"1". Construct calls are
				// named-args-only. (A lone `{...}` falls through so the
				// Story 9 object-literal error keeps its specific hint.)
				return nil, newParseErrorf(&p.current,
					"positional args are removed on construct calls; name the argument: %s %s(k: v, ...) -- a bare identifier puns to its own name (%s(x) == %s(x: x))",
					kind, name, name, name)
			}
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

	// Story 9 (#2335): reject the legacy object-literal argument wrapper
	// `name({...})` / `name({})`. The kind-prefixed invocation form passes
	// named args directly in the parens (`name(k: v, ...)`, empty = `name()`).
	// A single positional argument that is an object literal IS the removed
	// wrapper; colon-named / `=`-named args, bare positional primitive args,
	// and nested object VALUES (`payload: { ... }`) all stay valid because
	// they never produce a lone positional object here.
	if len(args) == 0 && len(argList) == 1 {
		if _, isObjectLiteral := argList[0].(map[string]any); isObjectLiteral {
			return nil, newParseErrorf(&p.current,
				"object-literal call args are removed; pass named args directly: %s(k: v, ...), empty = %s() (was: %s({...}))",
				name, name, name)
		}
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
// `paginate(target, LIMIT)` and produces a *PaginateExpr matching what
// the memql runtime parser emits. Limit must be a positive integer
// (the page size). The opening `(` is already consumed by the caller
// (parseFunctionCall). Offset pagination was removed (epic 5, 5.13 /
// memql#1993) -- keyset cursors are the continuation primitive.
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

	if err := p.expect(TokenParenClose); err != nil {
		return nil, err
	}

	return &PaginateExpr{
		Target: target,
		Limit:  &limit,
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
	// `asOf` is a query-only clause: it compiles to a time-travel read
	// against the graph and is rejected in logic / automation / mutation
	// / spec bodies (core-builtins ADR §2.3). The temporal dependency is
	// declared THROUGH the query a body imports, never inline. A zero
	// currentFuncType ("" -- standalone expression / runtime query
	// string) stays permissive; an explicit non-query kind is rejected.
	if p.currentFuncType != "" && p.currentFuncType != FunctionTypeQuery {
		return nil, newParseErrorf(&p.current, "`asOf` is a query-only clause and cannot appear in a %s body; time-travel reads belong in a query the body imports (core-builtins ADR §2.3)", p.currentFuncType)
	}
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
		// for the equivalence rationale. The directive-arg call
		// sites (paginate limit, withDepth,
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
			// Bare reserved `now` is the canonical current-timestamp
			// primitive (epic #2298 / #2301): emit the TimestampExprFunc
			// node so a comparison RHS like `expiresAt < now` resolves to
			// the clock identically to the retired `now()` / `timestamp()`
			// call-forms (executor_filter handles TimestampExprFunc).
			// Without this, bare `now` fell through as the literal string
			// "now" and the predicate never matched.
			if val == "now" {
				return &TimestampExprFunc{}, nil
			}
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

			// Shorthand A: a bare dotted path like `event.payload.partitionId`
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
		} else if p.check(TokenComma) || p.check(TokenBraceClose) {
			// G3 (#2365) punning in object-literal position: a bare simple
			// identifier is `key: key`. This is the path automation STEP
			// calls lower through (the rewriter turns `logic X(k: v, j)`
			// into an object form), so punning must parse here exactly as
			// in parseFunctionCallWithKind. The value is the identifier's
			// own literal -- identical to what parseValue returns for the
			// named form `j: j` -- and resolves at runtime via the G2 bare
			// rules. Dotted keys never reach here (they fail the key check).
			obj[key] = key
			if p.check(TokenComma) {
				p.advance()
			}
			continue
		} else {
			return nil, newParseErrorf(&p.current, "expected ':' or '=' after object key, got %q", p.current.Literal)
		}

		// Collection-method projection context (#2542 item 3): when this
		// object literal is a `select(g => {...})` lambda body, parse each
		// value as a FULL expression so a per-group ratio
		// (`g.good.count() / g.items.count()`) and a bare scope path
		// (`g.key`) become evaluable ExpressionNodes resolved against the
		// lambda scope -- the JSON-value grammar (parseValue) stops at the
		// first arithmetic operator (`expected '}', got "/"`) and stores a
		// path as an opaque raw string. Gated on the collection-arg flag
		// (suppressCommaOr, set only while parsing a Story 4 collection
		// method's arguments) so arg-time / spec / shape object literals keep
		// the literal JSON-value grammar -- infix arithmetic stays out of
		// those positions, preserving the #2316 in-memory-only containment.
		if p.suppressCommaOr {
			expr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			obj[key] = expr
			if p.check(TokenComma) {
				p.advance()
			} else {
				break
			}
			continue
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

// parseExpressionArg parses a single expression argument (stopping at comma or
// paren). Beyond a bare primary it accepts ONE infix comparison (S9, #2407) --
// `cond(role == "admin", ...)` / `cond(x != y, ...)` -- which previously
// failed with "requires three arguments" the first time a comparison-predicate
// cond EXECUTED (load-time lowering never runs this path, so the deploy-pack
// lifecycle logics carried the latent break). Connectives (&& / || / !) stay
// rejected here: nest cond() or put the compound condition on an `if` step.
//
// An IDENTIFIER-led comparison (`x > 10`, `x == "a"`) is already folded into a
// Field-led ComparisonExpr one level down, in parsePrimary ->
// parseIdentifierExpression, and returns here with NO pending operator. Only a
// comparison whose LHS is NON-identifier-led -- the headline being a
// collection-chain aggregate (`cond(args.rows.count(r => r.active) > 0, ...)`,
// #2542 item 2), or a literal / call result -- reaches the switch below with
// the operator still pending. The relational cases emit the expression-led
// BinaryComparisonExpr that the compiler serializer, the cond-predicate
// condition evaluator, and the engine single-return evalCollScalar already
// handle (#2559/#2560); they are purely additive (these operators previously
// always errored here -- cond three-arg / arg-list reject). The `==`/`!=` cases
// emit the SAME BinaryComparisonExpr shape (memql#2654): the earlier
// EqExpr/NotExpr(EqExpr) dual shapes forced every lowering to handle both or
// silently diverge (the #2653 wrong-branch class), so equality is normalized
// at the source and those shapes are no longer produced here.
func (p *Parser) parseExpressionArg() (ExpressionNode, error) {
	left, err := p.parseCoalesceArgOperand()
	if err != nil {
		return nil, err
	}
	if p.check(TokenOperator) {
		switch p.current.Literal {
		case "==", "!=", "<", "<=", ">", ">=":
			op := ComparisonOperator(p.current.Literal)
			p.advance()
			right, rerr := p.parseCoalesceArgOperand()
			if rerr != nil {
				return nil, rerr
			}
			return &BinaryComparisonExpr{Left: left, Operator: op, Right: right}, nil
		case "&&", "||":
			return nil, newParseErrorf(&p.current, "expression args accept a single comparison, not connectives -- nest cond() or move the compound condition to an `if` step (#2407)")
		}
	}
	return left, nil
}

// parseCoalesceArgOperand parses one expression-arg operand: a primary with
// any `??` chain folded into a single CoalesceExpr (#2611). Expression args
// bypass the main cascade (parsePrimary plus one comparison, #2407), so the
// coalesce level is mirrored here in lockstep -- without it `??` would work
// everywhere except cond args, worse than its absence. The fold produces
// exactly the AST the coalesce(a, b) spelling produces in this position.
// Operands are parsePrimary, DELIBERATELY narrower than the cascade's
// parseAdditive: the #2407 arg grammar admits one comparison and no
// arithmetic, and the `??` fold inherits that restriction rather than
// widening the arg surface.
func (p *Parser) parseCoalesceArgOperand() (ExpressionNode, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	if left == nil || !p.check(TokenQuestionQuestion) {
		return left, nil
	}
	args := []ExpressionNode{left}
	for p.check(TokenQuestionQuestion) {
		p.advance()
		prevFold := p.suppressComparisonFold
		p.suppressComparisonFold = true
		next, nerr := p.parsePrimary()
		p.suppressComparisonFold = prevFold
		if nerr != nil {
			return nil, nerr
		}
		if next == nil {
			return nil, newParseErrorf(&p.current, "expected an expression after '??'")
		}
		args = append(args, next)
	}
	return &CoalesceExpr{Args: args}, nil
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
			// Story 4: a chained `.method(...)` collection call where the
			// `.` is a standalone operator token followed by `method(`.
			if next := p.peekAhead(1); next.Type == TokenIdentifier && collectionMethods[next.Literal] && p.peekAhead(2).Type == TokenParenOpen {
				method := next.Literal
				p.advance() // consume '.'
				p.advance() // consume method ident
				p.advance() // consume '('
				args, err := p.parseMethodArgList()
				if err != nil {
					return nil, err
				}
				if err := p.expect(TokenParenClose); err != nil {
					return nil, err
				}
				call = &MethodCallExpr{Receiver: call, Method: method, Args: args}
				continue
			}
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
			// Story 4: a chained `.method(...)` collection call where the
			// lexer fused the leading `.` into the identifier (`.count`)
			// and the next token is `(`. The method must be a single
			// segment (no further dots) for the call form.
			seg := strings.TrimPrefix(p.current.Literal, ".")
			if !strings.Contains(seg, ".") && collectionMethods[seg] && p.peekAhead(1).Type == TokenParenOpen {
				method := seg
				p.advance() // consume '.method'
				p.advance() // consume '('
				args, err := p.parseMethodArgList()
				if err != nil {
					return nil, err
				}
				if err := p.expect(TokenParenClose); err != nil {
					return nil, err
				}
				call = &MethodCallExpr{Receiver: call, Method: method, Args: args}
				continue
			}
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

// parseParenStringList consumes `("a", "b", ...)` and returns the
// values -- the enum-type value list shared by the field parsers
// (#2618). The opening paren must be the current token.
func (p *Parser) parseParenStringList(where string) ([]string, error) {
	p.advance() // consume `(`
	var values []string
	for !p.check(TokenParenClose) && !p.check(TokenEOF) {
		if !p.check(TokenString) {
			return nil, newParseErrorf(&p.current, "enum values must be quoted strings on %s, got %q", where, p.current.Literal)
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
	if len(values) == 0 {
		return nil, newParseErrorf(&p.current, "enum type on %s needs at least one value", where)
	}
	return values, nil
}

// enumAttributeFromValues synthesizes the attribute the @enum
// annotation would have produced (a single string stores bare;
// multiple store as []string -- parseAttribute's shape), so the enum
// TYPE form is indistinguishable downstream (#2618).
func enumAttributeFromValues(values []string) *Attribute {
	attr := &Attribute{Name: "enum", Args: map[string]any{}}
	if len(values) == 1 {
		attr.Value = values[0]
	} else {
		attr.Value = values
	}
	return attr
}
