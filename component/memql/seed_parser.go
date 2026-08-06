package memql

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/core/baseparser"
)

// parseSeedMemQL parses a `.memql` seed declaration and returns a
// seedDecl. Seeds use a struct form similar to mutations -- a
// file-top `use <namespace>.<concept>` clause binds the target
// concept; the body declares the row's field values:
//
//	use platform.partition
//
//	@scope("global")
//	seed defaultPartition {
//	  id:            "default"
//	  partitionType: "standard"
//	  displayName:   "Default"
//	}
//
// For @scope("perUser") the loader iterates v1:identity:user rows
// and materializes one seed row per user; the seed body may then
// reference per-user context bindings (Phase 2; not yet wired):
//
//	use agents.agent
//
//	@scope("perUser")
//	seed assistant {
//	  name:        "Assistant"
//	  role:        "assistant"
//	  providerConfig {
//	    llm {
//	      policyName: "balancedChat"
//	    }
//	  }
//	}
//
// This parser is INTENTIONALLY schema-agnostic: it produces a generic
// tree of field assignments + nested blocks, NOT a type-checked
// projection of any particular concept's schema. The loader is what
// validates body field names + types against the bound concept's
// declared schema. The same parser handles seeds for any concept.
//
// The grammar this parser accepts:
//
//	file        := use-clause? annotation* seed-decl
//	use-clause  := 'use' identifier '.' identifier
//	annotation  := '@' identifier '(' ... ')'
//	seed-decl   := 'seed' identifier '{' body '}'
//	body        := (field-assign | nested-block)*
//	field-assign := identifier ':' value
//	nested-block := identifier '{' body '}'
//	value       := string | int | float | bool | string-array
func parseSeedMemQL(origin string, raw []byte) (*seedDecl, error) {
	p := &seedMemQLParser{}
	p.Init(string(raw), origin)
	return p.parse(origin)
}

type seedMemQLParser struct {
	baseparser.Base
}

// seedDecl is the parsed top-level seed declaration. The body is
// represented as a generic seedBlock tree -- the loader validates
// it against the bound concept's schema.
type seedDecl struct {
	// Annotation values
	name         string // declaration name (the `seed XXX` identifier)
	description  string // @description value
	namespace    string // @namespace value
	version      string // @version value
	scope        string // @scope value: "global" | "perUser" (empty => default policy applied by loader)
	templateFile string // @templateFile path (optional; e.g. systemPrompt source for agent seeds)

	// Use clause -- binds the file's target concept by namespace.concept name.
	// Loader resolves this to the canonical concept id (e.g. v1:agents:agent).
	// Populated by either the legacy file-top `use <ns>.<concept>` form OR
	// by the canonical `seed <Concept> <name>` signature shape (in which
	// case signatureConcept is also set and useNamespace stays empty --
	// the loader resolves the concept via the file's Form B imports).
	useNamespace     string
	useConcept       string
	signatureConcept string       // set when concept binding came from signature
	imports          []seedImport // Form B file-top imports

	// Body -- generic field/block tree. Field validation happens at
	// load time against the bound concept's schema.
	body seedBlock
}

// seedImport records a single Form B file-top `use module.{ names }`
// import. The loader uses these to resolve a signature-bound concept
// name to its source module.
type seedImport struct {
	module string
	names  []string
}

// seedBlock is one `{ ... }` worth of field assignments + nested
// blocks. Maps preserve declaration-order via the keys slice so we
// can render deterministic error messages.
type seedBlock struct {
	keys   []string             // insertion order
	fields map[string]seedValue // key -> value
	nested map[string]seedBlock // key -> nested block
}

// seedValue is a typed scalar or scalar-array from a body field
// assignment. The loader downcasts to the concept's declared type
// for that field.
type seedValue struct {
	kind     seedValueKind
	str      string   // when kind == seedString
	intV     int64    // when kind == seedInt
	floatV   float64  // when kind == seedFloat
	boolV    bool     // when kind == seedBool
	stringsV []string // when kind == seedStringArray
}

type seedValueKind int

const (
	seedNone seedValueKind = iota
	seedString
	seedInt
	seedFloat
	seedBool
	seedStringArray
)

// -----------------------------------------------------------------
// Parse driver
// -----------------------------------------------------------------

func (p *seedMemQLParser) parse(origin string) (*seedDecl, error) {
	decl := &seedDecl{
		body: newSeedBlock(),
	}

	for !p.EOF() {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			break
		}

		ch := p.Peek()

		if p.MatchWord("use") {
			if err := p.parseUseClause(decl); err != nil {
				return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, err)
			}
			continue
		}

		if ch == '@' {
			p.Advance() // consume '@'
			if err := p.parseDecorator(decl); err != nil {
				return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, err)
			}
			continue
		}

		if p.MatchWord("seed") {
			p.SkipWhitespaceAndComments()
			// Read the first identifier with ReadIdentWithHyphens so
			// kebab-case seed names like `graphic-designer` are
			// accepted (memql#180). When the signature is the canonical
			// `seed <Concept> <name>` two-identifier shape, the binder
			// concept (first) is by convention Go-style camelCase --
			// the hyphen-tolerant reader handles both shapes uniformly.
			first := p.ReadIdentWithHyphens()
			if first == "" {
				return nil, fmt.Errorf("%s:%d:%d: expected seed name after 'seed'", origin, p.Line, p.Col)
			}
			p.SkipWhitespaceAndComments()
			// Two-identifier signature `seed <Concept> <name> { ... }`
			// is the canonical post-migration shape; the single-
			// identifier form `seed <name> { ... }` is the legacy
			// shape that still requires a file-top `use ns.concept`
			// clause. We disambiguate by peeking past the next word.
			if !p.EOF() && p.Peek() != '{' {
				second := p.ReadIdentWithHyphens()
				if second != "" {
					// `seed Concept name { ... }` -- concept binding
					// lives in the signature. Reject a duplicate
					// Form A `use` clause.
					if decl.useConcept != "" && decl.useConcept != first {
						return nil, fmt.Errorf("%s:%d:%d: seed %q binds concept %q in its signature but file-top `use %s.%s` already bound %q", origin, p.Line, p.Col, second, first, decl.useNamespace, decl.useConcept, decl.useConcept)
					}
					decl.useConcept = first
					decl.signatureConcept = first
					decl.name = second
				} else {
					decl.name = first
				}
			} else {
				decl.name = first
			}
			if err := p.parseBlockBody(&decl.body); err != nil {
				return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, err)
			}
			break
		}

		// Legacy/typo guard -- never accept func-receiver form.
		if p.MatchWord("func") {
			return nil, fmt.Errorf("%s:%d:%d: no `func (Seed)` form exists -- use canonical struct form `seed name { ... }`", origin, p.Line, p.Col)
		}

		p.Advance()
	}

	if decl.name == "" {
		return nil, fmt.Errorf("%s: no seed declaration found", origin)
	}
	if decl.useConcept == "" {
		return nil, fmt.Errorf("%s: seed %q must bind a concept -- either via the canonical signature `seed <Concept> %s { ... }` or via the legacy file-top `use <namespace>.<concept>` clause", origin, decl.name, decl.name)
	}
	return decl, nil
}

// -----------------------------------------------------------------
// use-clause
// -----------------------------------------------------------------

// parseUseClause consumes a file-top `use` statement in either of the
// two shapes the migration recognises:
//
//	Form A (legacy): use <namespace>.<concept>
//	Form B (canonical): use <module.path>.{ name1, name2, ... }
//
// Form A binds the seed file's concept directly. Form B names a
// module and lists imported constructs; the seed's concept binding
// comes from the signature `seed <Concept> <name>` (validated by the
// loader against this file's import list).
func (p *seedMemQLParser) parseUseClause(decl *seedDecl) error {
	p.SkipWhitespaceInline()
	// Read all dotted path segments. We accumulate segments while
	// the next char is `.` followed by an alphanumeric; we stop
	// (and treat as Form B) when next is `.{`. Form A is a 2-segment
	// path (`<ns>.<concept>`) with no trailing `.{`.
	parts := []string{}
	seg := p.ReadWord()
	if seg == "" {
		return fmt.Errorf("expected module path after `use`")
	}
	parts = append(parts, seg)
	for {
		p.SkipWhitespaceInline()
		if p.EOF() || p.Peek() != '.' {
			break
		}
		// Lookahead past the dot. If next is `{`, this is the Form B
		// brace body marker -- bail out of the segment-accumulation
		// loop with the dot still queued.
		if p.HasNext() && p.PeekAt(1) == '{' {
			break
		}
		p.Advance() // consume '.'
		p.SkipWhitespaceInline()
		nextSeg := p.ReadWord()
		if nextSeg == "" {
			return fmt.Errorf("expected path segment after `.` in `use` clause")
		}
		parts = append(parts, nextSeg)
	}

	// Form B: `.{ name, name, ... }` follows the path.
	if !p.EOF() && p.Peek() == '.' && p.HasNext() && p.PeekAt(1) == '{' {
		modulePath := strings.Join(parts, ".")
		p.Advance() // consume '.'
		p.Advance() // consume '{'
		var names []string
		for {
			p.SkipWhitespaceAndComments()
			if p.EOF() {
				return fmt.Errorf("unexpected EOF inside `use %s.{ ... }`", modulePath)
			}
			if p.Peek() == '}' {
				p.Advance()
				break
			}
			n := p.ReadWord()
			if n == "" {
				return fmt.Errorf("expected imported name in `use %s.{ ... }`", modulePath)
			}
			names = append(names, n)
			p.SkipWhitespaceAndComments()
			if !p.EOF() && p.Peek() == ',' {
				p.Advance()
			}
		}
		if len(names) == 0 {
			return fmt.Errorf("`use %s.{ ... }` must list at least one name", modulePath)
		}
		decl.imports = append(decl.imports, seedImport{module: modulePath, names: names})
		return nil
	}

	// Form A: exactly `<ns>.<concept>` (2 segments).
	if len(parts) != 2 {
		return fmt.Errorf("expected `use <namespace>.<concept>` or `use <path>.{ names }`, got %d-segment path %q", len(parts), strings.Join(parts, "."))
	}
	if decl.useConcept != "" {
		return fmt.Errorf("multiple `use` clauses in one seed file (had %s.%s, now %s.%s)", decl.useNamespace, decl.useConcept, parts[0], parts[1])
	}
	decl.useNamespace = parts[0]
	decl.useConcept = parts[1]
	return nil
}

// -----------------------------------------------------------------
// Annotations
// -----------------------------------------------------------------

func (p *seedMemQLParser) parseDecorator(decl *seedDecl) error {
	name := p.ReadWord()
	if name == "" {
		return fmt.Errorf("expected decorator name after @")
	}

	switch name {
	case "enabled", "disabled":
		// Legacy path (test-only; production seeds load via
		// LoadUnifiedSeeds, where @disabled skips registration -- #2606).
		return nil

	case "description":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.description = val
		return nil

	case "version":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.version = val
		return nil

	case "namespace":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.namespace = val
		return nil

	case "scope":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		switch val {
		case "global", "perUser":
			decl.scope = val
		default:
			return fmt.Errorf("@scope must be \"global\" or \"perUser\", got %q", val)
		}
		return nil

	case "templateFile":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.templateFile = val
		return nil

	default:
		return fmt.Errorf("unknown seed annotation @%s (allowed: @version, @namespace, @scope, @description, @templateFile, @enabled, @disabled)", name)
	}
}

func (p *seedMemQLParser) parseParenStringList() ([]string, error) {
	p.SkipWhitespaceInline()
	if p.EOF() || p.Peek() != '(' {
		return nil, fmt.Errorf("expected '(' after annotation")
	}
	p.Advance() // consume '('
	var out []string
	for {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			return nil, fmt.Errorf("unexpected EOF inside annotation arg list")
		}
		if p.Peek() == ')' {
			p.Advance()
			return out, nil
		}
		if p.Peek() != '"' {
			return nil, fmt.Errorf("expected quoted string in annotation arg list")
		}
		s, err := p.readQuotedString()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		p.SkipWhitespaceAndComments()
		if !p.EOF() && p.Peek() == ',' {
			p.Advance()
			continue
		}
	}
}

// -----------------------------------------------------------------
// Body parsing (generic block tree)
// -----------------------------------------------------------------

func newSeedBlock() seedBlock {
	return seedBlock{
		fields: make(map[string]seedValue),
		nested: make(map[string]seedBlock),
	}
}

// parseBlockBody consumes `{ ... }` containing field assignments and
// nested blocks. Recursive: nested blocks parse with the same rules
// as the top-level body. No field-name validation here -- the loader
// validates against the target concept's schema.
func (p *seedMemQLParser) parseBlockBody(block *seedBlock) error {
	p.SkipWhitespaceAndComments()
	if p.EOF() || p.Peek() != '{' {
		return fmt.Errorf("expected '{' to start block body")
	}
	p.Advance() // consume '{'

	for {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			return fmt.Errorf("unexpected EOF inside block body")
		}
		if p.Peek() == '}' {
			p.Advance()
			return nil
		}

		key := p.ReadWord()
		if key == "" {
			return fmt.Errorf("expected field name or block name in body")
		}
		p.SkipWhitespaceInline()

		if !p.EOF() && p.Peek() == '{' {
			// Nested block.
			child := newSeedBlock()
			if err := p.parseBlockBody(&child); err != nil {
				return err
			}
			if _, dup := block.nested[key]; dup {
				return fmt.Errorf("duplicate nested block %q", key)
			}
			if _, dup := block.fields[key]; dup {
				return fmt.Errorf("name %q used as both field and nested block", key)
			}
			block.keys = append(block.keys, key)
			block.nested[key] = child
			continue
		}

		if p.EOF() || p.Peek() != ':' {
			return fmt.Errorf("expected ':' after field name %q", key)
		}
		p.Advance() // consume ':'
		p.SkipWhitespaceInline()

		val, err := p.parseValue()
		if err != nil {
			return fmt.Errorf("parsing field %q: %w", key, err)
		}
		if _, dup := block.fields[key]; dup {
			return fmt.Errorf("duplicate field %q", key)
		}
		if _, dup := block.nested[key]; dup {
			return fmt.Errorf("name %q used as both field and nested block", key)
		}
		block.keys = append(block.keys, key)
		block.fields[key] = val
	}
}

// parseValue reads one RHS scalar or array. Accepts string, int,
// float, bool, or `[...]` of quoted strings (the only array form
// the body needs today -- ref-typed arrays like
// `[tool("respondToUser")]` from the agent primitive are out of
// scope for v1 seeds; loaders interpreting agent seeds will fall
// back to plain string-array form for tool / knowledge bindings).
func (p *seedMemQLParser) parseValue() (seedValue, error) {
	if p.EOF() {
		return seedValue{}, fmt.Errorf("unexpected EOF where value expected")
	}

	ch := p.Peek()
	switch {
	case ch == '"':
		s, err := p.readQuotedString()
		if err != nil {
			return seedValue{}, err
		}
		return seedValue{kind: seedString, str: s}, nil
	case ch == '[':
		arr, err := p.parseStringArray()
		if err != nil {
			return seedValue{}, err
		}
		return seedValue{kind: seedStringArray, stringsV: arr}, nil
	case p.MatchWord("true"):
		return seedValue{kind: seedBool, boolV: true}, nil
	case p.MatchWord("false"):
		return seedValue{kind: seedBool, boolV: false}, nil
	}

	// Numeric: read until whitespace/newline/comma/}/].
	raw := p.readUntilValueTerminator()
	if raw == "" {
		return seedValue{}, fmt.Errorf("expected value, got %q", string(ch))
	}
	if i, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return seedValue{kind: seedInt, intV: i}, nil
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return seedValue{kind: seedFloat, floatV: f}, nil
	}
	return seedValue{}, fmt.Errorf("unrecognized value token %q (expected quoted string, number, true/false, or [\"...\"] array)", raw)
}

func (p *seedMemQLParser) parseStringArray() ([]string, error) {
	if p.EOF() || p.Peek() != '[' {
		return nil, fmt.Errorf("expected '['")
	}
	p.Advance() // consume '['
	var out []string
	for {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			return nil, fmt.Errorf("unexpected EOF inside string array")
		}
		if p.Peek() == ']' {
			p.Advance()
			return out, nil
		}
		if p.Peek() != '"' {
			return nil, fmt.Errorf("string arrays only contain quoted strings")
		}
		s, err := p.readQuotedString()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		p.SkipWhitespaceAndComments()
		if !p.EOF() && p.Peek() == ',' {
			p.Advance()
			continue
		}
	}
}

// readUntilValueTerminator consumes bytes until a value-boundary
// rune. Used for numeric literals where we don't know the type
// without reading the whole token.
func (p *seedMemQLParser) readUntilValueTerminator() string {
	var b []byte
	for !p.EOF() {
		ch := p.Peek()
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' || ch == '}' || ch == ']' {
			break
		}
		b = append(b, byte(ch))
		p.Advance()
	}
	return string(b)
}

// -----------------------------------------------------------------
// String reader -- mirrors the agent parser's helper. Kept local so
// seed parsing doesn't depend on agent_parser internals (the agent
// primitive is on the deletion list once seeds replace it).
// -----------------------------------------------------------------

func (p *seedMemQLParser) readQuotedString() (string, error) {
	if p.EOF() || p.Peek() != '"' {
		return "", fmt.Errorf("expected '\"' to start string")
	}
	p.Advance() // consume opening quote
	var b []byte
	for !p.EOF() {
		ch := p.Peek()
		if ch == '"' {
			p.Advance()
			return string(b), nil
		}
		if ch == '\\' {
			p.Advance()
			if p.EOF() {
				return "", fmt.Errorf("unterminated string escape")
			}
			esc := p.Peek()
			switch esc {
			case 'n':
				b = append(b, '\n')
			case 't':
				b = append(b, '\t')
			case 'r':
				b = append(b, '\r')
			case '\\':
				b = append(b, '\\')
			case '"':
				b = append(b, '"')
			default:
				b = append(b, byte(esc))
			}
			p.Advance()
			continue
		}
		b = append(b, byte(ch))
		p.Advance()
	}
	return "", fmt.Errorf("unterminated string literal")
}
