package memql

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/znasllc-io/memql/component/memql/baseparser"
)

// parsePromptMemQL parses a .memql prompt definition file and returns
// a promptDecl. Prompts use the canonical struct form — no receiver
// function wrapping, no body-level `@input { ... }` wrapper. The
// body is a bare input-schema field list, identical in shape to
// tool / builtin bodies:
//
//	@description("Route utterance to best agent")
//	@defaultProvider("chat54Mini")
//	@templateFile("cognitionRouting.tmpl")
//	prompt cognitionRouting {
//	  agents       array(object)  @required
//	  utterance    string         @required
//	  speakerName  string         @required
//	  transcript   array(object)
//	}
//
// Inline-template form (rare — every shipped prompt uses
// `@templateFile` today). The optional `@template("""...""")`
// annotation can appear at the body level alongside the fields:
//
//	prompt summarise {
//	  content  string  @required
//	  @template("""
//	    Summarize this document.
//	    Content: {{.content}}
//	  """)
//	}
//
// Two legacy forms are retired:
//   - `func (Prompt) name(ctx any) { ... }` — the receiver-function
//     wrapping. Rejected at parse time with a migration hint.
//   - `@input { ... }` — the body-level wrapper around the field
//     list. Rejected at parse time with a migration hint.
func parsePromptMemQL(origin string, raw []byte) (*promptDecl, error) {
	p := &promptMemQLParser{}
	p.Init(string(raw), origin)
	return p.parse(origin)
}

// promptMemQLParser embeds baseparser.Base for scanning primitives.
// Prompt-specific helpers (parseTemplateBlock, readTripleQuotedString,
// parseField) stay on the wrapper.
type promptMemQLParser struct {
	baseparser.Base
}

// promptDecl represents the parsed top-level prompt declaration.
type promptDecl struct {
	name            string
	description     string
	defaultProvider string
	templateSource  string // inline template from @template("""...""")
	templateFile    string // external template file from @templateFile("...")

	// Fields (inputSchema)
	fields []toolField
}

func (p *promptMemQLParser) parse(origin string) (*promptDecl, error) {
	decl := &promptDecl{}

	for !p.EOF() {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			break
		}

		ch := p.Peek()

		// use statement — skipped here; imports surfaced through the
		// loader-side import index.
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

		// prompt keyword — canonical struct form, mirrors concept /
		// shape / tool / provider / builtin. Body keeps the
		// `@input { ... }` and `@template(...)` blocks unchanged —
		// only the outer wrapper changes.
		if p.MatchWord("prompt") {
			p.SkipWhitespaceAndComments()
			decl.name = p.ReadWord()
			if decl.name == "" {
				return nil, fmt.Errorf("%s:%d:%d: expected prompt name after 'prompt'", origin, p.Line, p.Col)
			}
			if err := p.parseFuncBody(decl); err != nil {
				return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, err)
			}
			break
		}

		// `func (Prompt)` is retired. Hard-fail with a migration
		// hint so any stale file fails loud.
		if p.MatchWord("func") {
			return nil, fmt.Errorf("%s:%d:%d: %w", origin, p.Line, p.Col, fmt.Errorf("`func (Prompt) name(ctx any) { ... }` is retired -- use the canonical struct form: `prompt name { @input { ... } }`"))
		}

		// Skip anything else
		p.Advance()
	}

	if decl.name == "" {
		return nil, fmt.Errorf("%s: no prompt definition found", origin)
	}

	return decl, nil
}

func (p *promptMemQLParser) parseDecorator(decl *promptDecl) error {
	name := p.ReadWord()
	if name == "" {
		return fmt.Errorf("expected decorator name after @")
	}

	switch name {
	case "enabled":
		return nil

	case "disabled":
		return nil

	case "description":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.description = val
		return nil

	case "defaultProvider":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.defaultProvider = val
		return nil

	case "templateFile":
		val, err := p.ParseParenString()
		if err != nil {
			return err
		}
		decl.templateFile = val
		return nil

	default:
		// Unknown decorator — skip any arguments
		p.SkipWhitespaceInline()
		if !p.EOF() && p.Peek() == '(' {
			p.SkipBalancedParens()
		}
		return nil
	}
}

func (p *promptMemQLParser) parseFuncBody(decl *promptDecl) error {
	p.SkipWhitespaceAndComments()

	if p.EOF() || p.Peek() != '{' {
		return fmt.Errorf("expected '{' to start prompt body")
	}
	p.Advance() // consume {

	for !p.EOF() {
		p.SkipWhitespaceAndComments()
		if p.EOF() {
			return fmt.Errorf("unexpected end of file in prompt body")
		}

		if p.Peek() == '}' {
			p.Advance() // consume }
			return nil
		}

		// Look for @ blocks
		if p.Peek() == '@' {
			p.Advance() // consume @
			blockName := p.ReadWord()

			switch blockName {
			case "input":
				// Legacy `@input { ... }` wrapper. Every prompt body
				// is just input-schema fields and an optional inline
				// `@template(...)`; the wrapper was the only thing
				// distinguishing the two, and nothing used the
				// inline template form. The struct refactor drops
				// it. If a stale file still wraps fields in @input,
				// fail loud with a migration hint.
				return fmt.Errorf("`@input { ... }` wrapper is retired -- declare prompt fields directly inside `prompt name { ... }`, same as tools and builtins")
			case "template":
				if err := p.parseTemplateBlock(decl); err != nil {
					return err
				}
			default:
				// Unknown block — skip
				p.SkipWhitespaceInline()
				if !p.EOF() && p.Peek() == '(' {
					p.SkipBalancedParens()
				}
				if !p.EOF() && p.Peek() == '{' {
					p.SkipBalancedBraces()
				}
			}
			continue
		}

		// Bare field declaration at the body level: prompt bodies
		// are input-schema field lists, just like tool / builtin
		// bodies. parseField reads `name type @annotations`.
		prevPos := p.Pos
		field, err := p.parseField()
		if err != nil {
			return err
		}
		if field != nil {
			decl.fields = append(decl.fields, *field)
			continue
		}
		// Safety: prevent infinite loops on unrecognised input.
		if p.Pos == prevPos {
			p.Advance()
		}
	}

	return fmt.Errorf("unexpected end of file, missing '}'")
}

func (p *promptMemQLParser) parseField() (*toolField, error) {
	name := p.ReadWord()
	if name == "" {
		return nil, nil
	}

	p.SkipWhitespaceInline()

	// Handle slice prefix `[]` before the element type. The resulting
	// typeName is recorded as "array" for backward compatibility with the
	// downstream shape/template layer; the element type is stored on
	// `elementType`.
	sliceElem := ""
	if !p.EOF() && p.Peek() == '[' {
		p.Advance()
		if p.EOF() || p.Peek() != ']' {
			return nil, fmt.Errorf("field %q: expected ']' after '[' in slice type", name)
		}
		p.Advance()
		elem := p.ReadWord()
		if elem == "" {
			return nil, fmt.Errorf("field %q: expected element type after '[]'", name)
		}
		sliceElem = elem
	}

	typeName := sliceElem
	if typeName == "" {
		typeName = p.ReadWord()
	}
	if typeName == "" {
		return nil, fmt.Errorf("expected type for field %q", name)
	}

	// Handle parameterized types like array(object)
	if !p.EOF() && p.Peek() == '(' {
		p.SkipBalancedParens()
	}

	// If we parsed `[]T`, surface the field as an array whose element
	// type is T. Callers that inspect `typeName` see "array"; the
	// element type is preserved for prompt-template rendering.
	effectiveType := typeName
	elementType := ""
	if sliceElem != "" {
		effectiveType = "array"
		elementType = sliceElem
	}

	field := &toolField{
		name:        name,
		typeName:    effectiveType,
		elementType: elementType,
	}

	// Parse field annotations until end of line or next field
	for !p.EOF() {
		p.SkipWhitespaceInline()
		if p.Peek() == '\n' || p.Peek() == '\r' || p.Peek() == '}' {
			break
		}

		if p.Peek() == '@' {
			p.Advance() // consume @
			ann := p.ReadWord()

			switch ann {
			case "required":
				field.required = true
			case "description":
				val, err := p.ParseParenString()
				if err != nil {
					return nil, err
				}
				field.description = val
			case "enum":
				vals, err := p.ParseParenStringList()
				if err != nil {
					return nil, err
				}
				field.enumValues = vals
			case "default":
				val, err := p.ParseParenString()
				if err != nil {
					return nil, err
				}
				field.defaultVal = val
			default:
				// Unknown annotation — skip args
				if !p.EOF() && p.Peek() == '(' {
					p.SkipBalancedParens()
				}
			}
			continue
		}

		// Handle line comments
		if p.Peek() == '/' && p.HasNext() && p.PeekAt(1) == '/' {
			p.SkipToEndOfLine()
			continue
		}

		// Skip other characters
		p.Advance()
	}

	return field, nil
}

func (p *promptMemQLParser) parseTemplateBlock(decl *promptDecl) error {
	p.SkipWhitespaceInline()
	if p.EOF() || p.Peek() != '(' {
		return fmt.Errorf("expected '(' after @template")
	}
	p.Advance() // consume (
	p.SkipWhitespaceAndComments()

	// Check for triple-quoted string
	if p.Pos+2 < len(p.Input) && p.Input[p.Pos:p.Pos+3] == `"""` {
		val, err := p.readTripleQuotedString()
		if err != nil {
			return err
		}
		decl.templateSource = val

		p.SkipWhitespaceAndComments()
		if p.EOF() || p.Peek() != ')' {
			return fmt.Errorf("expected ')' after @template(\"\"\"...\"\"\")")
		}
		p.Advance() // consume )
		return nil
	}

	// Single-quoted string
	val, err := p.ReadQuotedString()
	if err != nil {
		return err
	}
	decl.templateSource = val

	p.SkipWhitespaceAndComments()
	if p.EOF() || p.Peek() != ')' {
		return fmt.Errorf("expected ')' after @template string")
	}
	p.Advance() // consume )
	return nil
}

// readTripleQuotedString reads a triple-quoted string """...""" and strips
// common leading whitespace (like Python's textwrap.dedent).
func (p *promptMemQLParser) readTripleQuotedString() (string, error) {
	// Consume opening """
	if p.Pos+2 >= len(p.Input) || p.Input[p.Pos:p.Pos+3] != `"""` {
		return "", fmt.Errorf("expected '\"\"\"' to start triple-quoted string")
	}
	for i := 0; i < 3; i++ {
		p.Advance()
	}

	var b strings.Builder
	for !p.EOF() {
		// Check for closing """
		if p.Pos+2 < len(p.Input) && p.Input[p.Pos:p.Pos+3] == `"""` {
			for i := 0; i < 3; i++ {
				p.Advance()
			}
			return dedent(b.String()), nil
		}
		b.WriteByte(p.Peek())
		p.Advance()
	}
	return "", fmt.Errorf("unterminated triple-quoted string")
}

// dedent strips common leading whitespace from all non-empty lines,
// similar to Python's textwrap.dedent.
func dedent(s string) string {
	lines := strings.Split(s, "\n")

	// Find minimum indentation across non-empty lines
	minIndent := math.MaxInt
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" {
			continue
		}
		indent := len(line) - len(trimmed)
		if indent < minIndent {
			minIndent = indent
		}
	}

	if minIndent == math.MaxInt || minIndent == 0 {
		return strings.TrimRight(strings.TrimLeft(s, "\n"), "\n \t")
	}

	// Strip common indent
	for i, line := range lines {
		if len(line) >= minIndent {
			lines[i] = line[minIndent:]
		} else {
			lines[i] = strings.TrimLeft(line, " \t")
		}
	}

	result := strings.Join(lines, "\n")
	return strings.TrimRight(strings.TrimLeft(result, "\n"), "\n \t")
}

// toInputSchema converts fields to a JSON Schema map suitable for jsonschema compilation.
func (d *promptDecl) toInputSchema() (map[string]any, error) {
	if len(d.fields) == 0 {
		return nil, nil
	}

	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
	}

	properties := make(map[string]any)
	var required []string

	for _, f := range d.fields {
		prop := map[string]any{}

		switch strings.ToLower(f.typeName) {
		case "string":
			prop["type"] = "string"
		case "number", "float":
			prop["type"] = "number"
		case "integer", "int":
			prop["type"] = "integer"
		case "bool", "boolean":
			prop["type"] = "boolean"
		case "object":
			prop["type"] = "object"
		case "array":
			prop["type"] = "array"
			prop["items"] = map[string]any{"type": "object"}
		default:
			prop["type"] = "string"
		}

		if f.description != "" {
			prop["description"] = f.description
		}
		if len(f.enumValues) > 0 {
			enumAny := make([]any, len(f.enumValues))
			for i, v := range f.enumValues {
				enumAny[i] = v
			}
			prop["enum"] = enumAny
		}
		if f.defaultVal != "" {
			prop["default"] = f.defaultVal
		}

		properties[f.name] = prop
		if f.required {
			required = append(required, f.name)
		}
	}

	schema["properties"] = properties
	if len(required) > 0 {
		schema["required"] = required
	}

	return schema, nil
}

// toInputSchemaJSON returns the input schema as JSON bytes.
func (d *promptDecl) toInputSchemaJSON() (json.RawMessage, error) {
	schema, err := d.toInputSchema()
	if err != nil {
		return nil, err
	}
	if schema == nil {
		return nil, nil
	}
	return json.Marshal(schema)
}
