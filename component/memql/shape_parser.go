package memql

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/znasllc-io/memql/component/memql/baseparser"
)

// parseShapeMemQL parses a .memql shape definition file and returns a
// ShapeDefinition. The canonical struct form mirrors the concept
// definition syntax:
//
//	@description("Standard participant card")
//	@useConcept(participant)
//	@row
//	shape participantCard {
//	  id
//	  payload.displayName
//	  payload.status
//	  createdAt
//	}
//
// Each body path becomes a `node("path")` template entry keyed by the
// path's terminal segment (`payload.displayName` -> `displayName`).
func parseShapeMemQL(origin string, raw []byte) (*ShapeDefinition, error) {
	p := &shapeMemQLParser{}
	p.Init(string(raw), origin)
	return p.parse(origin)
}

// shapeMemQLParser embeds baseparser.Base for the shared scanning
// primitives. Only construct-specific state (the parsed decl, etc.)
// lives on the wrapper.
type shapeMemQLParser struct {
	baseparser.Base
}

// shapeDecl represents the parsed top-level shape declaration.
type shapeDecl struct {
	name        string
	description string
	useConcepts []string       // `@useConcept(name, ...)` -- bare concept names
	template    map[string]any // body paths translated to `node("...")` entries
	kindRow     bool
	kindCaller  bool
}

func (p *shapeMemQLParser) parse(origin string) (*ShapeDefinition, error) {
	decl := &shapeDecl{}

	for !p.EOF() {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			break
		}

		ch := p.Peek()

		// use statement - skip
		if p.MatchWord("use") {
			p.SkipUseClauseBody()
			continue
		}

		// @ decorator
		if ch == '@' {
			p.Advance() // consume @
			if err := p.parseDecorator(decl); err != nil {
				return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, err)
			}
			continue
		}

		// shape keyword — canonical struct form. The legacy receiver-
		// function form (`func (Shape) name { @template({...}) }`)
		// is retired; every shape under dsl/v1 was migrated to the
		// new shape in the same change that introduced this parser
		// path. The legacy helpers below (parseFuncHeader /
		// parseFuncBody / parseTemplateBlock) are kept inside this
		// file for any out-of-tree caller — they remain reachable
		// but the public entry point (parse) only dispatches the
		// new form.
		if p.MatchWord("shape") {
			if err := p.parseShapeStructDecl(decl); err != nil {
				return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, err)
			}
			break // only one shape per file
		}

		// `func (Shape)` is retired. Fail loudly so stale files in
		// any out-of-tree consumer get caught at load time with a
		// concrete migration hint.
		if p.MatchWord("func") {
			return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, fmt.Errorf("`func (Shape) name { @template(...) }` is retired -- use the canonical struct form: `shape name { id; payload.X; ... }`"))
		}

		// Skip anything else
		p.Advance()
	}

	if decl.name == "" {
		return nil, fmt.Errorf("%s: no shape definition found", origin)
	}

	return decl.toShapeDefinition(origin)
}

func (p *shapeMemQLParser) parseDecorator(decl *shapeDecl) error {
	name := p.ReadWord()
	if name == "" {
		return fmt.Errorf("expected decorator name after @")
	}

	switch name {
	case "description":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.description = val
		return nil

	case "concepts":
		// `@concepts("v1:X:Y")` is retired (Phase G.3.g). Bind the
		// shape via `@useConcept(<bareName>)` instead -- the loader
		// resolves the bare name against the concept registry by
		// trailing-segment match. Drain the parens for a clean error.
		_, _ = p.ParseParenStringList()
		return fmt.Errorf("`@concepts(\"v1:...\")` is retired -- bind the shape via `@useConcept(<bareName>)` (e.g. `@useConcept(space)`); the loader resolves bare names against the registry")

	case "useConcept":
		// `@useConcept(name)` -- bare concept name(s), no quotes.
		// Translates to fully-qualified `v1:<ns>:<name>` ids by
		// looking up the concept's filesystem location at load
		// time. For now, we accept the bare names and stash them
		// alongside the legacy `concepts` list; the loader's
		// post-pass resolves them. Until G.3.g enforces "no more
		// @concepts(...)", both annotation forms coexist.
		names, err := p.ParseParenIdentList()
		if err != nil {
			return err
		}
		decl.useConcepts = append(decl.useConcepts, names...)
		return nil

	case "row":
		// Flag annotation: no parens, no value.
		decl.kindRow = true
		return nil

	case "caller":
		// Flag annotation: no parens, no value.
		decl.kindCaller = true
		return nil

	default:
		// Unknown decorator -- hard reject. Drain any arguments so
		// the parser stays in a consistent state, then surface a
		// structured error. The full surface for shapes is:
		// @description, @row, @caller, @useConcept.
		p.SkipWhitespaceInline()
		if !p.EOF() && p.Peek() == '(' {
			p.SkipBalancedParens()
		}
		return fmt.Errorf("unknown shape annotation @%s -- the supported surface is @description, @row, @caller, @useConcept", name)
	}
}

// parseShapeStructDecl parses the canonical struct form:
//
//	shape <name> {
//	  <path>
//	  <path>
//	  ...
//	}
//
// Each path becomes a `node("<path>")` template entry keyed by the
// path's terminal segment. Paths may be separated by newlines and/or
// commas. Inline `//` comments are tolerated. The path-terminal-key
// rule mirrors the shorthand the legacy @template parser already
// supports (see tryParseNodeShorthand above), so a struct-form
// shape and its hypothetical legacy-form equivalent produce the
// same ShapeDefinition.
func (p *shapeMemQLParser) parseShapeStructDecl(decl *shapeDecl) error {
	p.SkipWhitespaceAndComments()

	// Name: bare identifier (no receiver type).
	decl.name = p.ReadWord()
	if decl.name == "" {
		return fmt.Errorf("expected shape name after 'shape'")
	}

	p.SkipWhitespaceAndComments()
	if p.EOF() || p.Peek() != '{' {
		return fmt.Errorf("expected '{' to start shape body")
	}
	p.Advance() // consume {

	template := make(map[string]any)
	// usedConcepts tracks which `@useConcept(name1, name2, ...)` entries
	// were exercised by at least one body path. Each `<name>.X` field
	// flips the corresponding entry to true; the post-body validator
	// rejects shapes whose annotation declares a concept the body
	// never references (mirrors the declared-must-be-used rule the
	// function loader enforces on queries / mutations).
	usedConcepts := make(map[string]bool, len(decl.useConcepts))

	for !p.EOF() {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			return fmt.Errorf("unexpected end of file in shape body")
		}
		if p.Peek() == '}' {
			p.Advance() // consume }
			decl.template = template
			for _, name := range decl.useConcepts {
				if !usedConcepts[name] {
					return fmt.Errorf("shape %q: @useConcept(%s) declared but %s is never referenced in the body", decl.name, name, name)
				}
			}
			return nil
		}
		// Skip stray commas.
		if p.Peek() == ',' {
			p.Advance()
			continue
		}

		path := p.readShapePath()
		if path == "" {
			return fmt.Errorf("expected field path, got %q", string(p.Peek()))
		}
		// Translate the kind/namespace prefix to the storage form the
		// engine evaluator already understands:
		//   - `row.payload.X` -> `payload.X`
		//   - `row.X` (other intrinsics) -> `X`  (id / createdAt / etc.)
		//   - `<conceptBareName>.X` -> `payload.X` (concept-namespace
		//     form; bareName must appear in this shape's
		//     @useConcept(...) list)
		//   - `caller.X` -> `caller.X` (unchanged; caller paths route
		//     through a different evaluator)
		storedPath := translateShapeBodyPath(path, decl.useConcepts)
		// Mark the concept reference as used when the body path starts
		// with `<name>.` and `<name>` appears in the shape's
		// @useConcept(...) list. The translation result for those
		// paths is `payload.X`; storing the lookup separately keeps
		// the usedConcepts map authoritative.
		if idx := strings.Index(path, "."); idx > 0 {
			head := path[:idx]
			for _, c := range decl.useConcepts {
				if head == c {
					usedConcepts[c] = true
					break
				}
			}
		}
		key := pathTerminalKey(storedPath)
		if key == "" {
			return fmt.Errorf("shape field path %q has no usable terminal key (must end in a simple identifier)", path)
		}
		template[key] = `node(\"` + storedPath + `\")`
	}
	return fmt.Errorf("unexpected end of file in shape body")
}

// readShapePath reads a single field path token of the form
// `id` / `payload.X.Y` / `createdAt`. Stops at whitespace, comma,
// or `}`.
func (p *shapeMemQLParser) readShapePath() string {
	var b strings.Builder
	for !p.EOF() {
		ch := p.Peek()
		if unicode.IsLetter(rune(ch)) || unicode.IsDigit(rune(ch)) || ch == '_' || ch == '.' {
			b.WriteByte(ch)
			p.Advance()
			continue
		}
		break
	}
	return b.String()
}

// translateShapeBodyPath maps the author-facing path forms onto the
// internal storage form the shape template evaluator understands.
//
// Storage form:
//   - `payload.X` -- a concept-payload field.
//   - `id` / `createdAt` / etc. -- a row intrinsic (bare).
//   - `caller.X` -- the engine envelope.
//
// Inputs the author can write:
//   - `row.payload.X` (legacy) -> `payload.X`
//   - `row.X` -> `X` (intrinsic; trims the `row.` prefix)
//   - `<conceptBareName>.X` -> `payload.X` (canonical; the bare name
//     must appear in the shape's @useConcept(...) target list)
//   - `caller.X` -> `caller.X` (unchanged)
//
// Paths that don't match any of the above are returned verbatim --
// the downstream evaluator surfaces the error if the form is
// unsupported.
func translateShapeBodyPath(path string, useConcepts []string) string {
	if strings.HasPrefix(path, "row.") {
		return strings.TrimPrefix(path, "row.")
	}
	if strings.HasPrefix(path, "caller.") {
		return path
	}
	if idx := strings.Index(path, "."); idx > 0 {
		head := path[:idx]
		for _, c := range useConcepts {
			if head == c {
				return "payload." + path[idx+1:]
			}
		}
	}
	return path
}

// pathTerminalKey returns the terminal identifier segment of a
// dotted path. `payload.displayName` -> `displayName`, `id` -> `id`,
// `payload.address.city` -> `city`. Returns "" when the terminal
// segment isn't a simple identifier.
func pathTerminalKey(path string) string {
	terminal := path
	if idx := strings.LastIndex(path, "."); idx >= 0 {
		terminal = path[idx+1:]
	}
	if !isSimpleShapeIdent(terminal) {
		return ""
	}
	return terminal
}

func isSimpleShapeIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, ch := range s {
		switch {
		case i == 0 && (unicode.IsLetter(ch) || ch == '_'):
			continue
		case i > 0 && (unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == '_'):
			continue
		default:
			return false
		}
	}
	return true
}
// toShapeDefinition converts a parsed shape declaration to a ShapeDefinition.
func (d *shapeDecl) toShapeDefinition(origin string) (*ShapeDefinition, error) {
	if d.template == nil {
		return nil, fmt.Errorf("shape %q has no body fields", d.name)
	}

	return &ShapeDefinition{
		Name:        d.name,
		Description: d.description,
		Template:    d.template,
		Origin:      origin,
		KindRow:     d.kindRow,
		KindCaller:  d.kindCaller,
		UseConcepts: d.useConcepts,
	}, nil
}

